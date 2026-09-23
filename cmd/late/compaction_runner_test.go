package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"late/internal/client"
	"late/internal/compaction"
	"late/internal/session"
)

// fakeHistoryScorer scores every segment with one fixed score, standing in
// for the compaction pipeline's decision client.
type fakeHistoryScorer struct {
	score float64
}

func (f fakeHistoryScorer) ScoreBatch(_ context.Context, _ string, items map[string]compaction.Item) (map[string]float64, error) {
	out := make(map[string]float64, len(items))
	for id := range items {
		out[id] = f.score
	}
	return out, nil
}

func compactionTestSession(t *testing.T, path string) *session.Session {
	t.Helper()
	return session.New(nil, path, []client.ChatMessage{
		{Role: "user", Content: client.TextContent("Please analyze this build log.")},
		{Role: "assistant", Content: client.TextContent(strings.Repeat("verbose analysis ", 200))},
	}, "system prompt", false)
}

// TestHistoryCompactionRunnerPersistsMutatingRun: an enabled-mode run
// compacts the session history in place and persists it to the session's
// history path, mirroring the orchestrator's own SaveHistory call sites.
func TestHistoryCompactionRunnerPersistsMutatingRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	sess := compactionTestSession(t, path)
	store := compaction.NewStore()

	runner := historyCompactionRunner(sess, fakeHistoryScorer{score: 0}, store, false, compaction.DefaultRelocationThreshold)
	report, err := runner(context.Background())
	if err != nil {
		t.Fatalf("runner() error = %v", err)
	}
	if report.ShadowOnly {
		t.Fatal("mutating run must not report ShadowOnly")
	}
	if report.SegmentsElided == 0 || report.TokensSaved <= 0 {
		t.Fatalf("expected a real elision, report = %+v", report)
	}
	if !strings.Contains(sess.History[1].Content.Text, "[[elided") {
		t.Fatal("compaction must rewrite the assistant message in place")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("SaveHistory did not write %s: %v", path, err)
	}
	var saved []client.ChatMessage
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatalf("saved history is not valid JSON: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved history has %d messages, want 2", len(saved))
	}
	if !strings.Contains(saved[1].Content.String(), "[[elided") {
		t.Fatal("the persisted history must contain the compacted message")
	}
}

// TestHistoryCompactionRunnerShadowRunSkipsPersistence: a shadow run computes
// the honest would-save report without mutating history or writing the
// session file.
func TestHistoryCompactionRunnerShadowRunSkipsPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	sess := compactionTestSession(t, path)
	store := compaction.NewStore()
	original := sess.History[1].Content.Text

	runner := historyCompactionRunner(sess, fakeHistoryScorer{score: 0}, store, true, compaction.DefaultRelocationThreshold)
	report, err := runner(context.Background())
	if err != nil {
		t.Fatalf("runner() error = %v", err)
	}
	if !report.ShadowOnly {
		t.Fatal("shadow run must report ShadowOnly")
	}
	if report.SegmentsElided == 0 || report.TokensSaved <= 0 {
		t.Fatalf("shadow report must quantify the would-save, report = %+v", report)
	}
	if sess.History[1].Content.Text != original {
		t.Fatal("shadow run must not mutate history")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("shadow run must not write the history file, stat error = %v", err)
	}
	if _, ok := store.Get("elide-1"); ok {
		t.Fatal("shadow run must not store originals")
	}
}
