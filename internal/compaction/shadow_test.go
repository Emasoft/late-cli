package compaction

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			lines = append(lines, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return lines
}

func TestShadowLog_AppendAndReadBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "shadow.jsonl")
	l, err := NewShadowLogAt(path)
	if err != nil {
		t.Fatalf("NewShadowLogAt() error = %v", err)
	}
	if l.Path() != path {
		t.Errorf("Path() = %q, want %q", l.Path(), path)
	}

	ts := time.Unix(1700000000, 0).UTC()
	entries := []ShadowEntry{
		{TS: ts, TaskHash: "abc123", SegmentID: "seg-1", Tokens: 120, Score: 0.42, Decision: DecisionKeep},
		{SegmentID: "seg-2", Tokens: 30, Score: 0.9}, // defaults: TS and Decision
	}
	for i, e := range entries {
		if err := l.Append(e); err != nil {
			t.Fatalf("Append(%d) error = %v", i, err)
		}
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	var got []ShadowEntry
	for i, line := range lines {
		var e ShadowEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		got = append(got, e)
	}
	if got[0] != entries[0] {
		t.Errorf("entry 0 = %+v, want %+v", got[0], entries[0])
	}
	if got[1].TS.IsZero() {
		t.Error("entry 1 TS was not defaulted to now")
	}
	if got[1].Decision != DecisionKeep {
		t.Errorf("entry 1 Decision = %q, want %q", got[1].Decision, DecisionKeep)
	}

	// The created directory must be private (0700).
	if info, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir perms = %o, want 700", perm)
	}
}

func TestShadowLog_ReplayMath(t *testing.T) {
	l, err := NewShadowLogAt(filepath.Join(t.TempDir(), "shadow.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	entries := []ShadowEntry{
		{SegmentID: "seg-keep", Tokens: 50, Score: 0.9},
		{SegmentID: "seg-drop", Tokens: 100, Score: 0.2},
		{SegmentID: "seg-drop", Tokens: 100, Score: 0.3}, // same segment re-scored
		{SegmentID: "seg-mid", Tokens: 25, Score: 0.5},   // exactly at threshold: kept
	}
	for _, e := range entries {
		if err := l.Append(e); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}

	report, err := l.Replay(0.5)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	want := ReplayReport{
		Threshold:      0.5,
		Entries:        4,
		UniqueSegments: 3,
		ElidedEntries:  2, // both seg-drop decisions (0.2, 0.3 < 0.5)
		ElidedSegments: 1,
		TokensTotal:    275,
		TokensElided:   200,
	}
	if report != want {
		t.Errorf("Replay() = %+v, want %+v", report, want)
	}
}

func TestShadowLog_ReplayMissingFileIsEmpty(t *testing.T) {
	l, err := NewShadowLogAt(filepath.Join(t.TempDir(), "never-written.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := l.Replay(0.5)
	if err != nil {
		t.Fatalf("Replay() error = %v (a missing log is empty, not an error)", err)
	}
	if report.Entries != 0 || report.Threshold != 0.5 {
		t.Errorf("Replay() = %+v, want an empty report at threshold 0.5", report)
	}
}

func TestShadowLog_ReplaySkipsMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow.jsonl")
	l, err := NewShadowLogAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ShadowEntry{SegmentID: "seg-1", Tokens: 10, Score: 0.1}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("this is not json\n\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := l.Append(ShadowEntry{SegmentID: "seg-2", Tokens: 20, Score: 0.9}); err != nil {
		t.Fatal(err)
	}

	report, err := l.Replay(0.5)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if report.MalformedLines != 1 {
		t.Errorf("MalformedLines = %d, want 1", report.MalformedLines)
	}
	if report.Entries != 2 || report.ElidedEntries != 1 {
		t.Errorf("Replay() = %+v, want 2 entries with 1 elided", report)
	}
}

func TestShadowLog_ConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow.jsonl")
	l, err := NewShadowLogAt(path)
	if err != nil {
		t.Fatal(err)
	}

	const (
		goroutines = 20
		perWorker  = 10
	)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				e := ShadowEntry{
					SegmentID: fmt.Sprintf("seg-%d-%d", g, i),
					Tokens:    1,
					Score:     0.5,
				}
				if err := l.Append(e); err != nil {
					t.Errorf("Append() error = %v", err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	lines := readLines(t, path)
	if len(lines) != goroutines*perWorker {
		t.Fatalf("got %d lines, want %d — appends were lost or torn", len(lines), goroutines*perWorker)
	}
	seen := make(map[string]bool, len(lines))
	for i, line := range lines {
		var e ShadowEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d torn or invalid: %v (%q)", i, err, line)
		}
		if seen[e.SegmentID] {
			t.Errorf("duplicate entry for %s — a write was interleaved", e.SegmentID)
		}
		seen[e.SegmentID] = true
	}
}

func TestShadowLog_AppendFailureOnUnwritablePath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewShadowLogAt(filepath.Join(blocker, "shadow.jsonl")); err == nil {
		t.Error("NewShadowLogAt() under a file path error = nil, want a mkdir failure")
	}

	// A log whose path is a directory makes every append fail.
	l, err := NewShadowLogAt(filepath.Join(dir, "sub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(ShadowEntry{SegmentID: "seg-1"}); err == nil {
		t.Error("Append() to a directory error = nil, want a failure")
	}
}

func TestShadowLog_EmptyPathRejected(t *testing.T) {
	if _, err := NewShadowLogAt(""); err == nil {
		t.Error("NewShadowLogAt(\"\") error = nil, want an error")
	}
}

func TestDefaultShadowPath(t *testing.T) {
	p, err := DefaultShadowPath()
	if err != nil {
		t.Skipf("no home dir available: %v", err)
	}
	if !strings.HasSuffix(p, filepath.Join(".local", "share", "late", "compaction-shadow.jsonl")) {
		t.Errorf("DefaultShadowPath() = %q, want it under ~/.local/share/late/compaction-shadow.jsonl", p)
	}
}

func TestHashTask(t *testing.T) {
	a := HashTask("write the parser")
	b := HashTask("write the parser")
	c := HashTask("write the parser!")
	if a != b {
		t.Errorf("HashTask is not deterministic: %q vs %q", a, b)
	}
	if a == c {
		t.Error("HashTask collided on different inputs")
	}
	if len(a) != 16 {
		t.Errorf("HashTask length = %d, want 16 hex chars", len(a))
	}
	// Distinct tasks must hash differently from their raw text (the point
	// of hashing is that the raw text is not logged).
	if strings.Contains(a, "parser") {
		t.Error("HashTask leaked raw task text")
	}
}
