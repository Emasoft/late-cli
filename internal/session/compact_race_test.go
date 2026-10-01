package session

import (
	"context"
	"strings"
	"sync"
	"testing"

	"late/internal/client"
	"late/internal/compaction"
)

// signalingScorer closes its channel on the first ScoreBatch call — the
// moment the walk is mid-flight — so the concurrent appenders below start
// while history is being walked and rewritten in place.
type signalingScorer struct {
	started chan struct{}
	once    sync.Once
}

func (s *signalingScorer) ScoreBatch(ctx context.Context, task string, items map[string]compaction.Item) (map[string]float64, error) {
	s.once.Do(func() { close(s.started) })
	scores := make(map[string]float64, len(items))
	for id := range items {
		scores[id] = 0 // elide everything: maximum in-place mutation
	}
	return scores, nil
}

// TestCompactContextUnderConcurrentAppends is the -race pin for the historyMu
// fix in CompactContext: history appends (the streaming commit path) must
// never race the walk's in-place message rewrites. The appenders fire as
// soon as the walk starts scoring; without the lock they slice-write
// s.History while the walk holds stale pointers into the same array
// (-race flags it), with the lock they serialize behind the walk.
func TestCompactContextUnderConcurrentAppends(t *testing.T) {
	sess := New(nil, "", []client.ChatMessage{
		{Role: "user", Content: client.TextContent("task")},
	}, "system prompt", false)

	// Six compactable assistant messages: one ScoreBatch call each.
	for i := 0; i < 6; i++ {
		sess.History = append(sess.History, client.ChatMessage{
			Role:      "assistant",
			Content:   client.TextContent(strings.Repeat("verbose work output ", 200)),
			ToolCalls: []client.ToolCall{{Index: 0, ID: "call_1", Type: "function", Function: client.FunctionCall{Name: "Bash", Arguments: `{}`}}},
		})
	}

	// Three appends launched mid-walk. They block on historyMu until the
	// walk releases it — that is the serialized-behind-the-walk behavior
	// the fix guarantees. The appends are serialized among THEMSELVES with
	// a test-local mutex: appendMessage's persistence step reads history
	// outside historyMu by design (see session.go), so two overlapping
	// appends race each other independently of this test's target — the
	// walk-vs-append race the CompactContext fix addresses.
	scorer := &signalingScorer{started: make(chan struct{})}
	var appends sync.WaitGroup
	var appendMu sync.Mutex
	appends.Add(3)
	for i := 0; i < 3; i++ {
		go func() {
			defer appends.Done()
			<-scorer.started
			appendMu.Lock()
			defer appendMu.Unlock()
			_ = sess.AddUserMessage("concurrent append during compaction")
		}()
	}

	report, err := sess.CompactContext(context.Background(), scorer, NewCompactStore(), CompactionOptions{})
	if err != nil {
		t.Fatalf("CompactContext error = %v", err)
	}
	appends.Wait()

	// The walk covered exactly the six messages present when it started;
	// the concurrent appends must not have been scanned or rewritten.
	if report.MessagesScanned != 6 {
		t.Errorf("MessagesScanned = %d, want 6 (appends land after the walk, never mid-walk)", report.MessagesScanned)
	}
	if report.MessagesCompacted == 0 || report.TokensSaved <= 0 {
		t.Fatalf("expected a real elision, report = %+v", report)
	}
	// Consistency: 1 seeded user + 6 walked (now pointer-bearing) + 3
	// concurrent appends = 10 messages.
	if len(sess.History) != 10 {
		t.Errorf("history length = %d, want 10 (1 seeded + 6 walked + 3 appends)", len(sess.History))
	}
	for i := 1; i <= 6; i++ {
		if !strings.Contains(sess.History[i].Content.Text, "[[elided") {
			t.Errorf("walked message %d was not rewritten: %.60q", i, sess.History[i].Content.Text)
		}
	}
	for i := 7; i < len(sess.History); i++ {
		if got := sess.History[i].Content.String(); got != "concurrent append during compaction" {
			t.Errorf("appended message %d = %q, want the concurrent append text", i, got)
		}
	}
}
