package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"late/internal/git"
)

// TestSpawnSubagentTool_ParametersDocumentResumeAndWorktree guards the JSON
// schema: the resume and worktree extensions must stay advertised with
// their semantics (resume ignores everything else; worktree takes a
// registered path or a branch name).
func TestSpawnSubagentTool_ParametersDocumentResumeAndWorktree(t *testing.T) {
	schema := string(SpawnSubagentTool{Runner: nil}.Parameters())
	if !strings.Contains(schema, `"resume"`) {
		t.Fatal("parameters schema does not advertise the resume property")
	}
	if !strings.Contains(schema, "All other parameters are ignored") {
		t.Fatal("resume schema description does not document the ignored-parameters semantics")
	}
	if !strings.Contains(schema, `"worktree"`) {
		t.Fatal("parameters schema does not advertise the worktree property")
	}
	if !strings.Contains(schema, "branch name") {
		t.Fatal("worktree schema description does not document the branch-name form")
	}
}

// TestSpawnSubagentTool_ResumePassthrough pins the resume path: the parsed
// resume ID reaches the runner untouched, and no worktree validation runs
// (resume mode ignores the other fields).
func TestSpawnSubagentTool_ResumePassthrough(t *testing.T) {
	runnerCalled := false
	spawnTool := SpawnSubagentTool{
		Runner: func(ctx context.Context, request SubagentSpawnRequest) (string, error) {
			runnerCalled = true
			if !request.IsResume() {
				t.Errorf("request.IsResume() = false, want true")
			}
			if request.ResumeID != "coder-subagent-3" {
				t.Errorf("ResumeID = %q, want coder-subagent-3", request.ResumeID)
			}
			// goal/agent_type may be present but meaningless on resume;
			// the runner decides.
			return "resumed", nil
		},
	}

	result, err := spawnTool.Execute(context.Background(), json.RawMessage(
		`{"resume":"coder-subagent-3","goal":"ignored","agent_type":"ignored"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if !runnerCalled {
		t.Fatal("runner was not invoked for a resume request")
	}
	if result != "resumed" {
		t.Errorf("result = %q, want resumed", result)
	}
}

// TestSpawnSubagentTool_ResumeSkipsWorktreeValidation pins that a resume
// request never runs worktree validation even when a worktree value is
// present: resume mode ignores everything but the ID.
func TestSpawnSubagentTool_ResumeSkipsWorktreeValidation(t *testing.T) {
	restoreCWD(t, t.TempDir()) // no repo anywhere near the CWD

	spawnTool := SpawnSubagentTool{
		Runner: func(ctx context.Context, request SubagentSpawnRequest) (string, error) {
			return "resumed", nil
		},
	}

	// Validation would fail outside a repo for a fresh spawn; the resume
	// path must bypass it entirely.
	result, err := spawnTool.Execute(context.Background(), json.RawMessage(
		`{"resume":"a-1","agent_type":"coder","worktree":"nope"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if !strings.Contains(result, "resumed") {
		t.Errorf("result = %q, want the runner result", result)
	}
}

// TestSpawnSubagentTool_CallStringResume pins the TUI-facing call string for
// a resume request: it says "Resuming", not "Spawning for unknown goal".
func TestSpawnSubagentTool_CallStringResume(t *testing.T) {
	got := SpawnSubagentTool{}.CallString(json.RawMessage(`{"resume":"researcher-subagent-2"}`))
	if !strings.Contains(got, "Resuming") || !strings.Contains(got, "researcher-subagent-2") {
		t.Errorf("CallString(resume) = %q, want the resuming wording with the ID", got)
	}
}

// initTestGitRepo creates a git repo with one commit at dir and configures
// an identity locally so the commit cannot pick up user-level settings.
// Skips the test when git itself is unavailable.
func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", ".")
	run("-c", "user.email=test@example.com", "-c", "user.name=test", "commit", "-q", "--allow-empty", "-m", "init")
}

// repoRootForTest resolves the repo root the same way ValidateWorktree
// does (git.RepoRoot from the process CWD). On macOS the temp dir's /var
// prefix is a symlink to /private/var, so expectations MUST be built from
// the resolved root — comparing against the raw t.TempDir() path fails.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root, ok := git.RepoRoot(cwd)
	if !ok {
		t.Fatalf("RepoRoot(%s): not a git repository", cwd)
	}
	return root
}

// TestValidateWorktree_BranchForm exercises the branch form end to end in a
// real temp repo: an existing unchecked-out branch gets a worktree; a
// branch that does not exist is created at HEAD first; a branch already
// checked out elsewhere reuses that worktree.
func TestValidateWorktree_BranchForm(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, repo)

	// Point the process CWD inside the repo — the repo root is resolved
	// from it.
	restoreCWD(t, repo)
	root := repoRootForTest(t)
	worktreesBase := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-worktrees")

	// A new branch that does not exist yet: created at HEAD, then checked
	// out into the worktree.
	wtPath, err := ValidateWorktree("fresh-branch")
	if err != nil {
		t.Fatalf("ValidateWorktree(fresh-branch): %v", err)
	}
	want := filepath.Join(worktreesBase, "fresh-branch")
	if wtPath != want {
		t.Errorf("worktree path = %q, want %q", wtPath, want)
	}
	if info, err := os.Stat(filepath.Join(wtPath, ".git")); err != nil || info.IsDir() {
		t.Errorf("created worktree has no .git file at %s (err=%v)", wtPath, err)
	}

	// An existing branch that is not checked out anywhere: the worktree is
	// created for it.
	if out, err := validateBranch(t, repo, "side-branch"); err != nil {
		t.Fatalf("create side-branch: %v: %s", err, out)
	}
	wtPath2, err := ValidateWorktree("side-branch")
	if err != nil {
		t.Fatalf("ValidateWorktree(side-branch): %v", err)
	}
	if wtPath2 != filepath.Join(worktreesBase, "side-branch") {
		t.Errorf("side-branch worktree = %q, want %q", wtPath2, filepath.Join(worktreesBase, "side-branch"))
	}

	// A branch already checked out (fresh-branch in the worktree created
	// above) is REUSED, not double-checked-out.
	wtPath3, err := ValidateWorktree("fresh-branch")
	if err != nil {
		t.Fatalf("ValidateWorktree(fresh-branch) reuse: %v", err)
	}
	if wtPath3 != wtPath {
		t.Errorf("checked-out branch resolved to %q, want the existing worktree %q", wtPath3, wtPath)
	}
}

// TestValidateWorktree_ExistingPath exercises the path form: a registered
// worktree path (the main worktree itself) is accepted; a directory outside
// the repo is rejected as unregistered.
func TestValidateWorktree_ExistingPath(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initTestGitRepo(t, repo)
	restoreCWD(t, repo)
	root := repoRootForTest(t)

	// The main worktree is a registered worktree of the repo.
	got, err := ValidateWorktree(repo)
	if err != nil {
		t.Fatalf("ValidateWorktree(repo): %v", err)
	}
	if got != root {
		t.Errorf("got %q, want the registered root %q", got, root)
	}

	// A sibling directory of the repo is NOT a registered worktree.
	stranger := filepath.Join(filepath.Dir(root), "stranger")
	if err := os.MkdirAll(stranger, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = ValidateWorktree(stranger)
	if err == nil || !strings.Contains(err.Error(), "not a registered worktree") {
		t.Fatalf("ValidateWorktree(stranger) = %v, want the not-registered error", err)
	}
}

// TestValidateWorktree_OutsideRepo pins the guard: with the process CWD
// outside any git repository, every worktree argument is rejected (and
// nothing is created).
func TestValidateWorktree_OutsideRepo(t *testing.T) {
	restoreCWD(t, t.TempDir())

	outside := filepath.Join(t.TempDir(), "not-a-repo")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ValidateWorktree("some-branch")
	if err == nil || !strings.Contains(err.Error(), "requires a git repository") {
		t.Fatalf("ValidateWorktree outside a repo = %v, want the not-a-repo error", err)
	}
	// Nothing was created for the branch.
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "not-a-repo-worktrees")); !os.IsNotExist(err) {
		t.Errorf("worktree directory must not be created outside a repo, stat err=%v", err)
	}
}

// TestSpawnSubagentTool_WorktreeValidationResult pins the tool-level
// behavior: a bad worktree argument is an error RESULT naming the problem
// with the runner never invoked; a valid one reaches the runner with the
// resolved path in request.Worktree.
func TestSpawnSubagentTool_WorktreeValidationResult(t *testing.T) {
	// No repo anywhere near the test CWD: validation fails.
	restoreCWD(t, t.TempDir())

	runnerCalled := false
	spawnTool := SpawnSubagentTool{
		Runner: func(ctx context.Context, request SubagentSpawnRequest) (string, error) {
			runnerCalled = true
			return "ok", nil
		},
	}

	result, err := spawnTool.Execute(context.Background(), json.RawMessage(
		`{"goal":"g","agent_type":"coder","worktree":"nope"}`))
	if err != nil {
		t.Fatalf("Execute() Go error = %v, want nil", err)
	}
	if runnerCalled {
		t.Fatal("runner must not run when worktree validation fails")
	}
	if !strings.Contains(result, "requires a git repository") {
		t.Fatalf("error result = %q, want the not-a-repo hint", result)
	}
}

// restoreCWD chdirs the test into dir for its duration and restores the
// original CWD on cleanup. Used by the worktree tests because repo-root
// resolution reads the process CWD.
func restoreCWD(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir(%s): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
}

// validateBranch creates branch in repo (at HEAD) without checking it out,
// returning the combined output for error reporting.
func validateBranch(t *testing.T, repo, branch string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("git", "branch", branch)
	cmd.Dir = repo
	return cmd.CombinedOutput()
}
