package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"late/internal/client"
	"late/internal/common"
	"late/internal/compaction"
	"late/internal/tool"
)

// --- fixture helpers ---------------------------------------------------------
//
// All helpers are cmp-prefixed to stay clear of the history_sanitize_test.go
// fixture helpers in this package.

// cmpStamped marks fixture message n. On the source branch it stamped the
// message with a fixed RFC3339 timestamp (timestamps must survive compaction
// untouched); the receive-time Timestamp field itself belongs to the excluded
// timestamps feature, so here it is an identity marker kept so the fixture
// call sites keep their per-message indices.
func cmpStamped(msg client.ChatMessage, _ int) client.ChatMessage {
	return msg
}

func cmpSystem(text string) client.ChatMessage {
	return client.ChatMessage{Role: "system", Content: client.TextContent(text)}
}

func cmpUser(text string) client.ChatMessage {
	return client.ChatMessage{Role: "user", Content: client.TextContent(text)}
}

func cmpAssistant(text string) client.ChatMessage {
	return client.ChatMessage{Role: "assistant", Content: client.TextContent(text)}
}

func cmpTool(text string) client.ChatMessage {
	return client.ChatMessage{Role: "tool", Content: client.TextContent(text)}
}

func cmpToolResult(callID, text string) client.ChatMessage {
	return client.ChatMessage{Role: "tool", ToolCallID: callID, Content: client.TextContent(text)}
}

func cmpAssistantWithCalls(text string, calls []client.ToolCall) client.ChatMessage {
	return client.ChatMessage{Role: "assistant", Content: client.TextContent(text), ToolCalls: calls}
}

func cmpToolCall(id string) client.ToolCall {
	return client.ToolCall{Index: 0, ID: id, Type: "function", Function: client.FunctionCall{Name: "dump", Arguments: `{"path":"."}`}}
}

// cmpLongText builds n distinct ~550-byte paragraphs separated by blank
// lines, so compaction.SegmentSegments yields exactly n single-paragraph
// segments (none tiny, none over the 1200-byte cap) and n >= 3 exceeds
// minCompactChars.
func cmpLongText(tag string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "%s segment %02d: ", tag, i)
		b.WriteString(strings.Repeat(fmt.Sprintf("%s%02dtoken ", tag, i), 42))
	}
	return b.String()
}

// defaultFixture builds a 12-message history whose frozen prefix is
// max(1, 12/4) = 3 messages (the system prompt plus the two earliest
// exchanges) and whose eligible region mixes four compactable candidates
// (indices 4, 5, 7, 8), a long user message (index 9 — never compacted), and
// short filler.
func defaultFixture() []client.ChatMessage {
	return []client.ChatMessage{
		cmpStamped(cmpSystem("You are a helpful coding assistant."), 0),                                         // 0 frozen
		cmpStamped(cmpUser("Explore the repository and summarize it."), 1),                                      // 1 frozen
		cmpStamped(cmpAssistant("It is a Go CLI for LLM chat sessions."), 2),                                    // 2 frozen
		cmpStamped(cmpUser("Now inspect internal/session."), 3),                                                 // 3
		cmpStamped(cmpAssistant(cmpLongText("alpha", 3)), 4),                                                    // 4 candidate
		cmpStamped(cmpTool(cmpLongText("beta", 3)), 5),                                                          // 5 candidate (tool result)
		cmpStamped(cmpUser("What about tool calls?"), 6),                                                        // 6
		cmpStamped(cmpAssistantWithCalls(cmpLongText("gamma", 3), []client.ToolCall{cmpToolCall("call_7")}), 7), // 7 candidate
		cmpStamped(cmpToolResult("call_7", cmpLongText("delta", 3)), 8),                                         // 8 candidate (tool result, call id)
		cmpStamped(cmpUser(cmpLongText("user", 3)), 9),                                                          // 9 long USER — never compacted
		cmpStamped(cmpAssistant("A short closing answer."), 10),                                                 // 10
		cmpStamped(cmpTool(`{"ok":true}`), 11),                                                                  // 11
	}
}

// fixtureSegments precomputes, for every compactable candidate in fixture,
// the segments CompactContext will see (segmentation is deterministic),
// keyed by history index.
func fixtureSegments(t *testing.T, fixture []client.ChatMessage) map[int][]compaction.Segment {
	t.Helper()
	out := make(map[int][]compaction.Segment)
	for i := range fixture {
		msg := &fixture[i]
		if !compactableContent(msg) {
			continue
		}
		segs := compaction.SegmentSegments(msg.Content.Text, minCompactChars)
		if len(segs) > 0 {
			out[i] = segs
		}
	}
	return out
}

// stubScorer is the map-based HistoryScorer stub: it scores each item by
// exact segment-text lookup (fallback otherwise), can fail the Nth call, and
// records every task it saw. No network.
type stubScorer struct {
	scores    map[string]float64
	fallback  float64
	err       error
	errOnCall int // 1-based ScoreBatch call that fails; 0 = never
	calls     int
	tasks     []string
}

func (s *stubScorer) ScoreBatch(_ context.Context, task string, items map[string]compaction.Item) (map[string]float64, error) {
	s.calls++
	s.tasks = append(s.tasks, task)
	if s.err != nil && (s.errOnCall == 0 || s.calls == s.errOnCall) {
		return nil, s.err
	}
	out := make(map[string]float64, len(items))
	for id, it := range items {
		if score, ok := s.scores[it.Text]; ok {
			out[id] = score
		} else {
			out[id] = s.fallback
		}
	}
	return out, nil
}

// elideFirstScorer scores the FIRST segment of every precomputed candidate
// 0.1 (elide) and everything else 0.9 (keep), so each rewritten message
// loses exactly its opening segment.
func elideFirstScorer(segs map[int][]compaction.Segment) *stubScorer {
	scores := make(map[string]float64)
	for _, msgSegs := range segs {
		scores[msgSegs[0].Text] = 0.1
		for _, seg := range msgSegs[1:] {
			scores[seg.Text] = 0.9
		}
	}
	return &stubScorer{scores: scores, fallback: 0.9}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// newCompactSession wraps the fixture in an in-memory session (no history
// path: nothing is persisted) as a deep copy, so the caller's fixture stays
// pristine for comparisons.
func newCompactSession(history []client.ChatMessage) *Session {
	return New(nil, "", cloneHistory(history), "", false)
}

// assertZeroReport asserts a no-op report with the expected scan count.
func assertZeroReport(t *testing.T, report CompactionReport, wantScanned int) {
	t.Helper()
	if report.MessagesScanned != wantScanned {
		t.Errorf("MessagesScanned = %d, want %d", report.MessagesScanned, wantScanned)
	}
	if report.MessagesCompacted != 0 || report.SegmentsElided != 0 || report.TokensSaved != 0 || report.StoreSize != 0 {
		t.Errorf("expected a no-op report, got %+v", report)
	}
	if report.TokensAfter != report.TokensBefore {
		t.Errorf("TokensAfter = %d, want TokensBefore %d", report.TokensAfter, report.TokensBefore)
	}
}

// --- tests -------------------------------------------------------------------

// (a) The frozen prefix is byte-identical after a mutating run — including
// the system prompt at index 0 — and the walk starts right after it.
func TestCompactContextFrozenPrefixUntouched(t *testing.T) {
	fixture := defaultFixture()
	s := newCompactSession(fixture)
	segs := fixtureSegments(t, fixture)

	report, err := s.CompactContext(context.Background(), elideFirstScorer(segs), NewCompactStore(), CompactionOptions{})
	if err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}

	// 12 messages at the default 25% → frozen = max(1, 3) = 3.
	if frozen := frozenPrefix(len(fixture), defaultFrozenPercent); frozen != 3 {
		t.Fatalf("frozenPrefix(12, 25) = %d, want 3", frozen)
	}
	if got, want := mustJSON(t, s.History[:3]), mustJSON(t, fixture[:3]); got != want {
		t.Fatalf("frozen prefix mutated:\n got %s\nwant %s", got, want)
	}
	if s.History[0].Content.Text != fixture[0].Content.Text {
		t.Error("the system prompt (message 0) must never be compacted")
	}
	// The walk starts at index 3: the first candidate (index 4) was rewritten.
	if !strings.Contains(s.History[4].Content.Text, "[[elided id=") {
		t.Error("expected the first eligible message (index 4) to be compacted")
	}
	if report.MessagesScanned != len(fixture)-3 {
		t.Errorf("MessagesScanned = %d, want %d", report.MessagesScanned, len(fixture)-3)
	}
}

// (b) User messages are never compacted — not even a long one whose segments
// would all score below the threshold.
func TestCompactContextNeverCompactsUserMessages(t *testing.T) {
	fixture := defaultFixture()
	s := newCompactSession(fixture)
	segs := fixtureSegments(t, fixture)
	scorer := elideFirstScorer(segs)

	report, err := s.CompactContext(context.Background(), scorer, NewCompactStore(), CompactionOptions{})
	if err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}

	if got, want := s.History[9].Content.Text, fixture[9].Content.Text; got != want {
		t.Fatalf("the long user message was compacted:\n got %q\nwant %q", truncateRunes(got, 120), truncateRunes(want, 120))
	}
	// It was never even scored: exactly the four assistant/tool candidates
	// reached the scorer.
	if scorer.calls != len(segs) {
		t.Errorf("scorer calls = %d, want %d (one per candidate)", scorer.calls, len(segs))
	}
	if report.MessagesCompacted != len(segs) {
		t.Errorf("MessagesCompacted = %d, want %d", report.MessagesCompacted, len(segs))
	}
}

// (c) A long tool-result message is rewritten to kept segments plus an
// [[elided …]] pointer, and the original round-trips through the store —
// including through the expand tool.
func TestCompactContextElidesToolResultWithStoreRoundTrip(t *testing.T) {
	fixture := defaultFixture()
	s := newCompactSession(fixture)
	segs := fixtureSegments(t, fixture)
	store := NewCompactStore()
	scorer := elideFirstScorer(segs)

	report, err := s.CompactContext(context.Background(), scorer, store, CompactionOptions{})
	if err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}

	// The tool result at index 5 is the second rewritten message (index 4
	// minted elide-1), so its elided opening segment is elide-2.
	segs5 := segs[5]
	want := segs5[1].Text + segs5[2].Text + "\n\n" + elidePointerLine("elide-2", segs5[0])
	if got := s.History[5].Content.Text; got != want {
		t.Fatalf("compacted tool result mismatch:\n got %q\nwant %q", truncateRunes(got, 200), truncateRunes(want, 200))
	}
	if report.SegmentsElided != len(segs) {
		t.Errorf("SegmentsElided = %d, want %d", report.SegmentsElided, len(segs))
	}

	original, ok := store.Get("elide-2")
	if !ok {
		t.Fatal("store.Get(elide-2) miss: the elided original was not stored")
	}
	if original != segs5[0].Text {
		t.Fatalf("store round trip mismatch:\n got %q\nwant %q", truncateRunes(original, 120), truncateRunes(segs5[0].Text, 120))
	}

	// The expand tool reads the same store.
	expanded, xerr := tool.ExpandTool{Store: store}.Execute(context.Background(), json.RawMessage(`{"id":"elide-2"}`))
	if xerr != nil {
		t.Fatalf("expand(elide-2) error = %v", xerr)
	}
	if expanded != segs5[0].Text {
		t.Error("expand tool did not return the stored original")
	}

	// The derived task is the last user message's content, truncated to
	// maxTaskChars runes.
	wantTask := truncateRunes(fixture[9].Content.String(), maxTaskChars)
	for i, task := range scorer.tasks {
		if task != wantTask {
			t.Errorf("scorer task[%d] = %q, want the truncated last user message %q", i, truncateRunes(task, 80), truncateRunes(wantTask, 80))
		}
	}
}

// (d) An assistant message with tool calls: Content is compacted, ToolCalls
// (and role) stay structurally intact.
func TestCompactContextCompactsAssistantContentKeepsToolCalls(t *testing.T) {
	fixture := defaultFixture()
	s := newCompactSession(fixture)
	segs := fixtureSegments(t, fixture)

	if _, err := s.CompactContext(context.Background(), elideFirstScorer(segs), NewCompactStore(), CompactionOptions{}); err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}

	before, after := fixture[7], s.History[7]
	if !reflect.DeepEqual(before.ToolCalls, after.ToolCalls) {
		t.Fatalf("ToolCalls mutated:\n got %+v\nwant %+v", after.ToolCalls, before.ToolCalls)
	}
	if after.Role != before.Role {
		t.Errorf("Role changed: got %q, want %q", after.Role, before.Role)
	}
	if !strings.Contains(after.Content.Text, "[[elided id=") {
		t.Error("assistant Content was not compacted")
	}
	if len(after.Content.Text) >= len(before.Content.Text) {
		t.Error("assistant Content did not shrink")
	}
	// The tool result answering the call keeps its ToolCallID too.
	if s.History[8].ToolCallID != fixture[8].ToolCallID {
		t.Errorf("ToolCallID changed: got %q, want %q", s.History[8].ToolCallID, fixture[8].ToolCallID)
	}
}

// (e) The token math is exact: before/after are the summed
// EstimateMessageTokens of the history and saved is their difference.
func TestCompactContextTokenMath(t *testing.T) {
	fixture := defaultFixture()
	s := newCompactSession(fixture)
	segs := fixtureSegments(t, fixture)

	report, err := s.CompactContext(context.Background(), elideFirstScorer(segs), NewCompactStore(), CompactionOptions{})
	if err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}

	var wantBefore, wantAfter int
	for i := range fixture {
		wantBefore += common.EstimateMessageTokens(fixture[i])
		wantAfter += common.EstimateMessageTokens(s.History[i])
	}
	if report.TokensBefore != wantBefore {
		t.Errorf("TokensBefore = %d, want %d", report.TokensBefore, wantBefore)
	}
	if report.TokensAfter != wantAfter {
		t.Errorf("TokensAfter = %d, want %d", report.TokensAfter, wantAfter)
	}
	if report.TokensSaved != wantBefore-wantAfter {
		t.Errorf("TokensSaved = %d, want %d", report.TokensSaved, wantBefore-wantAfter)
	}
	if report.TokensSaved <= 0 {
		t.Errorf("TokensSaved = %d, want > 0", report.TokensSaved)
	}
	if report.StoreSize <= 0 {
		t.Errorf("StoreSize = %d, want > 0 (the relocated originals)", report.StoreSize)
	}
}

// (f) Shadow mode: the report carries the honest would-save numbers
// (identical to a mutating run's) and the history stays byte-identical.
func TestCompactContextShadowModeDoesNotMutate(t *testing.T) {
	fixtureJSON := mustJSON(t, defaultFixture())
	segs := fixtureSegments(t, defaultFixture())

	mutating := newCompactSession(defaultFixture())
	shadow := newCompactSession(defaultFixture())
	storeM, storeS := NewCompactStore(), NewCompactStore()

	gotMutating, err := mutating.CompactContext(context.Background(), elideFirstScorer(segs), storeM, CompactionOptions{})
	if err != nil {
		t.Fatalf("mutating run error = %v", err)
	}
	gotShadow, err := shadow.CompactContext(context.Background(), elideFirstScorer(segs), storeS, CompactionOptions{ShadowOnly: true})
	if err != nil {
		t.Fatalf("shadow run error = %v", err)
	}

	if got, want := mustJSON(t, shadow.History), fixtureJSON; got != want {
		t.Fatalf("the shadow run mutated history:\n got %s\nwant %s", truncateRunes(got, 300), truncateRunes(want, 300))
	}
	if !gotShadow.ShadowOnly {
		t.Error("the shadow report must set ShadowOnly")
	}
	if gotMutating.ShadowOnly {
		t.Error("the mutating report must not set ShadowOnly")
	}
	// Honest staging: every number matches the real run.
	if gotShadow.MessagesScanned != gotMutating.MessagesScanned ||
		gotShadow.MessagesCompacted != gotMutating.MessagesCompacted ||
		gotShadow.SegmentsElided != gotMutating.SegmentsElided ||
		gotShadow.TokensBefore != gotMutating.TokensBefore ||
		gotShadow.TokensAfter != gotMutating.TokensAfter ||
		gotShadow.TokensSaved != gotMutating.TokensSaved ||
		gotShadow.StoreSize != gotMutating.StoreSize {
		t.Fatalf("shadow report %+v differs from mutating report %+v", gotShadow, gotMutating)
	}
	if gotShadow.MessagesCompacted == 0 || gotShadow.TokensSaved <= 0 {
		t.Fatalf("shadow report not populated: %+v", gotShadow)
	}
	// Shadow stores nothing; the mutating run does.
	if _, ok := storeS.Get("elide-1"); ok {
		t.Error("the shadow run must not store originals")
	}
	if _, ok := storeM.Get("elide-1"); !ok {
		t.Error("the mutating run must store originals")
	}
	// The mutating run really did rewrite history.
	if got := mustJSON(t, mutating.History); got == fixtureJSON {
		t.Error("the mutating run left history unchanged")
	}
}

// (g) Fail-open mid-walk: the scorer fails on the third candidate; the first
// two rewritten messages stay rewritten, the rest are untouched, and the
// error reports how far the walk got.
func TestCompactContextFailOpenMidWalk(t *testing.T) {
	sentinel := errors.New("scorer unavailable")
	fixture := []client.ChatMessage{
		cmpStamped(cmpSystem("system prompt"), 0),         // frozen
		cmpStamped(cmpUser("first question"), 1),          // frozen
		cmpStamped(cmpAssistant("first answer"), 2),       // frozen
		cmpStamped(cmpAssistant(cmpLongText("c1", 3)), 3), // candidate 1
		cmpStamped(cmpTool(cmpLongText("c2", 3)), 4),      // candidate 2
		cmpStamped(cmpAssistant(cmpLongText("c3", 3)), 5), // candidate 3 — fails here
		cmpStamped(cmpTool(cmpLongText("c4", 3)), 6),      // candidate 4
		cmpStamped(cmpAssistant(cmpLongText("c5", 3)), 7), // candidate 5
	}
	s := newCompactSession(fixture)
	segs := fixtureSegments(t, fixture)
	if len(segs) != 5 {
		t.Fatalf("fixture candidates = %d, want 5", len(segs))
	}
	scorer := elideFirstScorer(segs)
	scorer.err = sentinel
	scorer.errOnCall = 3

	report, err := s.CompactContext(context.Background(), scorer, NewCompactStore(), CompactionOptions{})
	if err == nil {
		t.Fatal("CompactContext() error = nil, want the scorer failure")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not wrap the scorer failure: %v", err)
	}
	if !strings.Contains(err.Error(), "after 2 messages") {
		t.Errorf("error %q does not report the completed-message count", err)
	}

	// Messages 1-2 of the walk (fixture 3-4) stay rewritten.
	for _, idx := range []int{3, 4} {
		if !strings.Contains(s.History[idx].Content.Text, "[[elided id=") {
			t.Errorf("fixture message %d should have been rewritten before the failure", idx)
		}
	}
	// Messages 3+ of the walk are untouched.
	for _, idx := range []int{5, 6, 7} {
		if got, want := s.History[idx].Content.Text, fixture[idx].Content.Text; got != want {
			t.Errorf("fixture message %d was mutated after the failure:\n got %q", idx, truncateRunes(got, 120))
		}
	}
	if report.MessagesCompacted != 2 {
		t.Errorf("MessagesCompacted = %d, want 2", report.MessagesCompacted)
	}
	if report.SegmentsElided != 2 {
		t.Errorf("SegmentsElided = %d, want 2", report.SegmentsElided)
	}
}

// (h) Timestamps survive compaction untouched — on compacted and frozen
// messages alike. (The receive-time Timestamp field belongs to the excluded
// timestamps feature, so that assertion lives with it; compaction's
// rewrite path preserves everything except the compacted content.)

// (i) Short histories are no-ops: nothing eligible, nothing rewritten, a
// zero report.
func TestCompactContextShortHistoryNoOp(t *testing.T) {
	t.Run("single system message", func(t *testing.T) {
		fixture := []client.ChatMessage{cmpStamped(cmpSystem("only the prompt"), 0)}
		s := newCompactSession(fixture)
		report, err := s.CompactContext(context.Background(), elideFirstScorer(nil), NewCompactStore(), CompactionOptions{})
		if err != nil {
			t.Fatalf("CompactContext() error = %v", err)
		}
		assertZeroReport(t, report, 0) // the only message is frozen
		if got, want := mustJSON(t, s.History), mustJSON(t, fixture); got != want {
			t.Fatalf("history mutated:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("system plus long user message", func(t *testing.T) {
		fixture := []client.ChatMessage{
			cmpStamped(cmpSystem("system prompt"), 0),
			cmpStamped(cmpUser(cmpLongText("u", 3)), 1), // long, but a user message
		}
		s := newCompactSession(fixture)
		report, err := s.CompactContext(context.Background(), elideFirstScorer(nil), NewCompactStore(), CompactionOptions{})
		if err != nil {
			t.Fatalf("CompactContext() error = %v", err)
		}
		assertZeroReport(t, report, 1) // the user message is scanned but never compacted
		if got, want := mustJSON(t, s.History), mustJSON(t, fixture); got != want {
			t.Fatalf("history mutated:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("system, user and short assistant", func(t *testing.T) {
		fixture := []client.ChatMessage{
			cmpStamped(cmpSystem("system prompt"), 0),
			cmpStamped(cmpUser("question"), 1),
			cmpStamped(cmpAssistant("a short answer"), 2), // under minCompactChars
		}
		s := newCompactSession(fixture)
		report, err := s.CompactContext(context.Background(), elideFirstScorer(nil), NewCompactStore(), CompactionOptions{})
		if err != nil {
			t.Fatalf("CompactContext() error = %v", err)
		}
		assertZeroReport(t, report, 2)
		if got, want := mustJSON(t, s.History), mustJSON(t, fixture); got != want {
			t.Fatalf("history mutated:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("everything frozen via FrozenPercent 100", func(t *testing.T) {
		fixture := defaultFixture()
		s := newCompactSession(fixture)
		report, err := s.CompactContext(context.Background(), elideFirstScorer(fixtureSegments(t, fixture)), NewCompactStore(), CompactionOptions{FrozenPercent: 100})
		if err != nil {
			t.Fatalf("CompactContext() error = %v", err)
		}
		assertZeroReport(t, report, 0) // every message sits inside the frozen prefix
		if got, want := mustJSON(t, s.History), mustJSON(t, fixture); got != want {
			t.Fatalf("history mutated:\n got %s\nwant %s", got, want)
		}
	})
}

// (j) All-high scores: the walk runs (every candidate scored) but nothing is
// elided — a populated scan report with zero rewrites and unchanged history.
func TestCompactContextAllHighScoresNoOp(t *testing.T) {
	fixture := defaultFixture()
	s := newCompactSession(fixture)
	segs := fixtureSegments(t, fixture)
	scorer := &stubScorer{fallback: 0.9} // nothing elidable

	report, err := s.CompactContext(context.Background(), scorer, NewCompactStore(), CompactionOptions{})
	if err != nil {
		t.Fatalf("CompactContext() error = %v", err)
	}
	if got, want := mustJSON(t, s.History), mustJSON(t, fixture); got != want {
		t.Fatalf("history mutated under all-high scores:\n got %s", truncateRunes(got, 300))
	}
	if report.MessagesScanned != len(fixture)-3 {
		t.Errorf("MessagesScanned = %d, want %d", report.MessagesScanned, len(fixture)-3)
	}
	if report.MessagesCompacted != 0 || report.SegmentsElided != 0 || report.TokensSaved != 0 || report.StoreSize != 0 {
		t.Errorf("expected a no-op report, got %+v", report)
	}
	if scorer.calls != len(segs) {
		t.Errorf("scorer calls = %d, want %d", scorer.calls, len(segs))
	}
}

// The threshold is exclusive and defaults to the relocation default: a
// segment scoring exactly compaction.DefaultRelocationThreshold is kept, one
// strictly below is elided.
func TestCompactContextThresholdIsExclusive(t *testing.T) {
	build := func() (*Session, []client.ChatMessage, map[int][]compaction.Segment) {
		fixture := []client.ChatMessage{
			cmpStamped(cmpSystem("system prompt"), 0),
			cmpStamped(cmpUser("question"), 1),
			cmpStamped(cmpAssistant(cmpLongText("x", 4)), 2), // the only candidate (n=4 clears minCompactChars)
		}
		return newCompactSession(fixture), fixture, fixtureSegments(t, fixture)
	}

	t.Run("exactly the default threshold is kept", func(t *testing.T) {
		s, fixture, segs := build()
		scorer := elideFirstScorer(segs)
		scorer.scores[segs[2][0].Text] = compaction.DefaultRelocationThreshold
		report, err := s.CompactContext(context.Background(), scorer, NewCompactStore(), CompactionOptions{})
		if err != nil {
			t.Fatalf("CompactContext() error = %v", err)
		}
		if report.MessagesCompacted != 0 || report.SegmentsElided != 0 {
			t.Errorf("expected a keep at the exact threshold, got %+v", report)
		}
		if s.History[2].Content.Text != fixture[2].Content.Text {
			t.Error("history mutated at the exact threshold")
		}
	})

	t.Run("just below the default threshold is elided", func(t *testing.T) {
		s, _, segs := build()
		scorer := elideFirstScorer(segs)
		scorer.scores[segs[2][0].Text] = compaction.DefaultRelocationThreshold - 0.01
		report, err := s.CompactContext(context.Background(), scorer, NewCompactStore(), CompactionOptions{})
		if err != nil {
			t.Fatalf("CompactContext() error = %v", err)
		}
		if report.MessagesCompacted != 1 || report.SegmentsElided != 1 {
			t.Fatalf("expected one elision just below the threshold, got %+v", report)
		}
		if !strings.Contains(s.History[2].Content.Text, "[[elided id=elide-1 ") {
			t.Error("expected the segment just below the threshold to be elided")
		}
	})
}

// A nil scorer is a wiring bug: CompactContext refuses to run and leaves the
// history untouched. A nil store leaves relocation disarmed: nothing is
// elided (a pointer whose original cannot be stored must never enter
// history), mirroring the pipeline with relocation off.
func TestCompactContextFailSafes(t *testing.T) {
	fixture := defaultFixture()

	t.Run("nil scorer", func(t *testing.T) {
		s := newCompactSession(fixture)
		report, err := s.CompactContext(context.Background(), nil, NewCompactStore(), CompactionOptions{})
		if err == nil {
			t.Fatal("nil scorer: want an error")
		}
		if report != (CompactionReport{}) {
			t.Errorf("nil scorer: want a zero report, got %+v", report)
		}
		if got, want := mustJSON(t, s.History), mustJSON(t, fixture); got != want {
			t.Error("nil scorer: history must stay untouched")
		}
	})

	t.Run("nil store disarms relocation", func(t *testing.T) {
		s := newCompactSession(fixture)
		report, err := s.CompactContext(context.Background(), elideFirstScorer(fixtureSegments(t, fixture)), nil, CompactionOptions{})
		if err != nil {
			t.Fatalf("nil store: error = %v, want nil", err)
		}
		if got, want := mustJSON(t, s.History), mustJSON(t, fixture); got != want {
			t.Error("nil store: history must stay untouched")
		}
		if report.MessagesScanned != len(fixture)-3 {
			t.Errorf("nil store: MessagesScanned = %d, want %d", report.MessagesScanned, len(fixture)-3)
		}
		if report.MessagesCompacted != 0 || report.SegmentsElided != 0 {
			t.Errorf("nil store: expected no elisions, got %+v", report)
		}
	})
}

// CompactStore mints sequential elide ids and round-trips originals; ids
// without a stored original miss (the expand tool's unknown-id path).
func TestCompactStoreRoundTrip(t *testing.T) {
	store := NewCompactStore()
	if got := store.NextID(); got != "elide-1" {
		t.Errorf("NextID() = %q, want elide-1", got)
	}
	if got := store.NextID(); got != "elide-2" {
		t.Errorf("NextID() = %q, want elide-2", got)
	}
	store.Put("elide-1", "original one")
	got, ok := store.Get("elide-1")
	if !ok || got != "original one" {
		t.Fatalf("Get(elide-1) = %q, %v; want the stored original", got, ok)
	}
	if _, ok := store.Get("elide-2"); ok {
		t.Error("Get(elide-2) = hit; want a miss for an id with no stored original")
	}
	if _, ok := store.Get("elide-999"); ok {
		t.Error("Get(elide-999) = hit; want a miss")
	}
}
