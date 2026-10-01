package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"late/internal/common"
)

// worktreeCtx returns a context carrying the worktree, the way the runner
// wires it for a worktree child.
func worktreeCtx(dir string) context.Context {
	return context.WithValue(context.Background(), common.WorktreeDirKey, dir)
}

// TestShellTool_WorktreeDefaultDir pins the worktree wiring: without an
// explicit cwd, the shell runs inside the context's worktree; an explicit
// cwd still wins; without a worktree the process CWD applies.
func TestShellTool_WorktreeDefaultDir(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real shells")
	}
	setShellTimeoutForTest(t, 0)

	t.Run("worktree becomes the default dir", func(t *testing.T) {
		worktree := t.TempDir()
		probe := filepath.Join(worktree, "marker.txt")
		if err := os.WriteFile(probe, []byte("in worktree"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := ShellTool{}.Execute(worktreeCtx(worktree), json.RawMessage(`{"command":"pwd"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(got, filepath.Base(worktree)) {
			t.Errorf("shell ran outside the worktree: %q", got)
		}
		_ = probe
	})

	t.Run("explicit cwd still wins over the worktree", func(t *testing.T) {
		worktree := t.TempDir()
		sub := filepath.Join(worktree, "subdir")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := ShellTool{}.Execute(worktreeCtx(worktree), json.RawMessage(`{"command":"pwd","cwd":`+quoteJSON(sub)+`}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !strings.Contains(got, "subdir") {
			t.Errorf("explicit cwd ignored: %q", got)
		}

		// An explicit cwd OUTSIDE both the process CWD and the worktree is
		// still refused — the worktree only widens the base, it does not
		// open arbitrary directories.
		elsewhere := t.TempDir()
		_, err = ShellTool{}.Execute(worktreeCtx(worktree), json.RawMessage(`{"command":"pwd","cwd":`+quoteJSON(elsewhere)+`}`))
		if err == nil || !strings.Contains(err.Error(), "outside the allowed directory") {
			t.Errorf("external cwd = err %v, want the outside-allowed error", err)
		}
	})

	t.Run("no worktree keeps the process cwd", func(t *testing.T) {
		got, err := ShellTool{}.Execute(context.Background(), json.RawMessage(`{"command":"pwd"}`))
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		cwd, _ := os.Getwd()
		if !strings.Contains(got, filepath.Base(cwd)) {
			t.Errorf("shell ran outside the process cwd: %q (cwd %q)", got, cwd)
		}
	})
}

// TestSearchContentTool_WorktreeDefaultPath pins the search wiring: with no
// explicit path, the search walks the worktree and finds only its files.
func TestSearchContentTool_WorktreeDefaultPath(t *testing.T) {
	worktree := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "needle.txt"), []byte("findme-in-worktree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "needle.txt"), []byte("findme-elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := (&SearchContentTool{}).Execute(worktreeCtx(worktree), json.RawMessage(`{"pattern":"findme"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(got, "findme-in-worktree") {
		t.Errorf("search missed the worktree file: %q", got)
	}
	if strings.Contains(got, "findme-elsewhere") {
		t.Errorf("search escaped the worktree: %q", got)
	}

	// An explicit path wins over the worktree.
	got, err = (&SearchContentTool{}).Execute(worktreeCtx(worktree), json.RawMessage(`{"pattern":"findme","path":`+quoteJSON(other)+`}`))
	if err != nil {
		t.Fatalf("Execute(explicit path): %v", err)
	}
	if !strings.Contains(got, "findme-elsewhere") || strings.Contains(got, "findme-in-worktree") {
		t.Errorf("explicit path ignored: %q", got)
	}
}

// TestFindFilesTool_WorktreeDefaultPath pins find_files' worktree base:
// without an explicit path it walks the worktree only.
func TestFindFilesTool_WorktreeDefaultPath(t *testing.T) {
	worktree := t.TempDir()
	other := t.TempDir()
	for _, base := range []string{worktree, other} {
		if err := os.WriteFile(filepath.Join(base, "target-file.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := (&FindFilesTool{}).Execute(worktreeCtx(worktree), json.RawMessage(`{"pattern":"target-file"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(got, "target-file.txt") {
		t.Errorf("find missed the worktree file: %q", got)
	}
	// Only ONE result: the worktree's copy, not the other dir's.
	if strings.Count(got, "target-file.txt") != 1 {
		t.Errorf("find escaped the worktree: %q", got)
	}
}

// TestReadFileTool_WorktreeAnchorsRelativePaths pins the read_file anchor:
// a relative path resolves against the worktree; absolute paths are
// untouched; worktree-less runs keep the process-CWD-relative behavior.
func TestReadFileTool_WorktreeAnchorsRelativePaths(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "notes.md"), []byte("1 | worktree notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Relative path anchored at the worktree.
	got, err := (&ReadFileTool{}).Execute(worktreeCtx(worktree), json.RawMessage(`{"path":"notes.md"}`))
	if err != nil {
		t.Fatalf("Execute(relative): %v", err)
	}
	if !strings.Contains(got, "worktree notes") {
		t.Errorf("relative read missed the worktree file: %q", got)
	}

	// Absolute path still honored verbatim.
	got, err = (&ReadFileTool{}).Execute(worktreeCtx(worktree), json.RawMessage(`{"path":`+quoteJSON(filepath.Join(worktree, "notes.md"))+`}`))
	if err != nil {
		t.Fatalf("Execute(absolute): %v", err)
	}
	if !strings.Contains(got, "worktree notes") {
		t.Errorf("absolute read failed: %q", got)
	}

	// No worktree: unchanged behavior (file not found relative to CWD).
	if _, err := os.CreateTemp(t.TempDir(), "x"); err != nil {
		t.Fatal(err)
	}
	_, err = (&ReadFileTool{}).Execute(context.Background(), json.RawMessage(`{"path":"notes.md"}`))
	if err == nil {
		t.Error("relative read without a worktree must resolve against the process CWD (expect not-found)")
	}
}

// TestIsSafePathIn_WorktreeBase pins the path-safety base selection:
// IsSafePathIn measures against the explicit base when given, and the
// process CWD otherwise (IsSafePath's historical behavior unchanged).
func TestIsSafePathIn_WorktreeBase(t *testing.T) {
	worktree := t.TempDir()
	inside := filepath.Join(worktree, "src", "main.go")
	outside := filepath.Join(t.TempDir(), "evil.go")

	if !IsSafePathIn(inside, worktree) {
		t.Errorf("IsSafePathIn(inside, worktree) = false, want true")
	}
	if IsSafePathIn(outside, worktree) {
		t.Errorf("IsSafePathIn(outside, worktree) = true, want false")
	}
	// Escape attempts are refused even inside the base form.
	if IsSafePathIn(filepath.Join(worktree, "..", "evil.go"), worktree) {
		t.Error("parent escape accepted")
	}
}

// TestIsInsideWorktree pins the ctx-driven shell escape hatch: only an
// absolute path inside the ctx worktree passes.
func TestIsInsideWorktree(t *testing.T) {
	worktree := t.TempDir()
	inside := filepath.Join(worktree, "a.txt")
	outside := filepath.Join(t.TempDir(), "b.txt")

	if !isInsideWorktree(worktreeCtx(worktree), inside) {
		t.Error("absolute path inside the worktree must pass")
	}
	if isInsideWorktree(worktreeCtx(worktree), outside) {
		t.Error("absolute path outside the worktree must fail")
	}
	if isInsideWorktree(worktreeCtx(worktree), "relative/a.txt") {
		t.Error("relative paths are not the shell escape hatch's business")
	}
	if isInsideWorktree(context.Background(), inside) {
		t.Error("no worktree in ctx: nothing passes")
	}
}

// quoteJSON encodes s as a JSON string literal for inline argument
// assembly in these tests.
func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
