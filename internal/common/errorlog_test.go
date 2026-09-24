package common

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestErrorLog_AppendsJSONLines pins the durable critical-error log: 0600
// file, 0700 parent dirs, one JSON line per entry shaped
// {"ts":RFC3339,"component":...,"message":...}, appends across calls.
func TestErrorLog_AppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "late-errors.log")
	l, err := OpenErrorLogAt(path)
	if err != nil {
		t.Fatalf("OpenErrorLogAt() error = %v", err)
	}

	l.Log("compaction", "walk aborted after 2 messages")
	l.Logf("diagnostic", "hook %s failed: %d", "pre-tool", 3)

	// File and directory permissions.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) error = %v", path, err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %o, want 600", perm)
	}
	if dirSt, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("Stat(dir) error = %v", err)
	} else if perm := dirSt.Mode().Perm(); perm != 0o700 {
		t.Errorf("log dir mode = %o, want 700", perm)
	}

	// Line shape and count.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines []errorLogLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e errorLogLine
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("log line %q is not valid JSON: %v", line, err)
		}
		if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
			t.Errorf("log line ts %q is not RFC3339: %v", e.TS, err)
		}
		lines = append(lines, e)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d log lines, want 2", len(lines))
	}
	if lines[0].Component != "compaction" || lines[0].Message != "walk aborted after 2 messages" {
		t.Errorf("first line = %+v, want the compaction entry", lines[0])
	}
	if lines[1].Component != "diagnostic" || lines[1].Message != "hook pre-tool failed: 3" {
		t.Errorf("second line = %+v, want the formatted diagnostic entry", lines[1])
	}
}

// TestErrorLog_BestEffort pins the best-effort contract: logging never
// panics and never fails the caller — a nil log, an empty path, and an
// unwritable location are all silent no-ops.
func TestErrorLog_BestEffort(t *testing.T) {
	var nilLog *ErrorLog
	nilLog.Log("compaction", "must not panic") // nil receiver

	if _, err := OpenErrorLogAt(""); err == nil {
		t.Error("OpenErrorLogAt(\"\") = nil error, want an error")
	}

	// A path whose parent is a FILE cannot be created.
	file := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenErrorLogAt(filepath.Join(file, "late-errors.log")); err == nil {
		t.Error("OpenErrorLogAt under a file-parent = nil error, want an error")
	}
}

// TestProcessWideErrorLog pins the global helpers: LogError goes through the
// lazily opened process log; SetErrorLog re-points it (and nil disables).
func TestProcessWideErrorLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late-errors.log")
	l, err := OpenErrorLogAt(path)
	if err != nil {
		t.Fatal(err)
	}

	old := processLog
	t.Cleanup(func() { SetErrorLog(old) })

	SetErrorLog(l)
	LogError("test", "via the process log")
	LogErrorf("test", "formatted %d", 42)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), "\n"); n != 2 {
		t.Fatalf("process log holds %d lines, want 2:\n%s", n, data)
	}
	if !strings.Contains(string(data), `"component":"test"`) {
		t.Errorf("log missing the component field:\n%s", data)
	}

	// A nil install disables logging; a later LogError must not panic or
	// resurrect the previous log.
	SetErrorLog(nil)
	LogError("test", "silently dropped")
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "silently dropped") {
		t.Error("a nil process log must drop entries, not append them")
	}
}

// TestErrorLog_ConcurrentAppends: appends from many goroutines interleave as
// whole lines — the file always parses line-per-line.
func TestErrorLog_ConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late-errors.log")
	l, err := OpenErrorLogAt(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			l.Logf("concurrent", "writer %d says hello hello hello", n)
		}(i)
	}
	wg.Wait()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	count := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e errorLogLine
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("torn or invalid line %q: %v", line, err)
		}
		count++
	}
	if count != 32 {
		t.Errorf("parsed %d lines, want 32", count)
	}
}
