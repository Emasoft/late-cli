package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"late/internal/git"
)

// TestSpawnSubagentTool_TimeoutParsing covers the per-spawn "timeout"
// argument: absent/empty → no override; a valid duration → parsed override;
// "0"/negative → explicit unlimited (pointer to 0); an invalid value → an
// error RESULT (nil Go error) with the runner never invoked, so the model
// can retry with a valid duration.
func TestSpawnSubagentTool_TimeoutParsing(t *testing.T) {
	tests := []struct {
		name         string
		args         string
		wantErrStr   bool           // expect an error-result string; runner not called
		wantOverride *time.Duration // expected override passed to the runner
	}{
		{
			name:         "absent timeout means no override",
			args:         `{"goal":"g","agent_type":"coder"}`,
			wantOverride: nil,
		},
		{
			name:         "empty timeout means no override",
			args:         `{"goal":"g","agent_type":"coder","timeout":""}`,
			wantOverride: nil,
		},
		{
			name:         "valid duration is passed through",
			args:         `{"goal":"g","agent_type":"coder","timeout":"45m"}`,
			wantOverride: subagentTimeoutPtr(45 * time.Minute),
		},
		{
			name:         "two hours is passed through",
			args:         `{"goal":"g","agent_type":"coder","timeout":"2h"}`,
			wantOverride: subagentTimeoutPtr(2 * time.Hour),
		},
		{
			name:         "zero means explicit unlimited",
			args:         `{"goal":"g","agent_type":"coder","timeout":"0"}`,
			wantOverride: subagentTimeoutPtr(0),
		},
		{
			name:         "negative means explicit unlimited normalized to 0",
			args:         `{"goal":"g","agent_type":"coder","timeout":"-5m"}`,
			wantOverride: subagentTimeoutPtr(0),
		},
		{
			name:       "garbage duration is an error result",
			args:       `{"goal":"g","agent_type":"coder","timeout":"banana"}`,
			wantErrStr: true,
		},
		{
			name:       "missing unit is an error result",
			args:       `{"goal":"g","agent_type":"coder","timeout":"5"}`,
			wantErrStr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerCalled := false
			var gotOverride *time.Duration
			spawnTool := SpawnSubagentTool{
				Runner: func(ctx context.Context, request SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
					runnerCalled = true
					gotOverride = timeoutOverride
					if request.Goal != "g" || request.AgentType != "coder" {
						t.Errorf("runner got goal=%q agentType=%q, want goal=%q agentType=%q", request.Goal, request.AgentType, "g", "coder")
					}
					return "ok", nil
				},
			}

			result, err := spawnTool.Execute(context.Background(), json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("Execute() Go error = %v, want nil", err)
			}

			if tt.wantErrStr {
				if runnerCalled {
					t.Fatal("runner must not be invoked for an invalid timeout")
				}
				if !strings.Contains(result, `invalid subagent timeout`) || !strings.Contains(result, "use a duration like 45m, 2h") {
					t.Fatalf("error result = %q, want the invalid-timeout retry hint", result)
				}
				return
			}

			if !runnerCalled {
				t.Fatal("runner was not invoked")
			}
			if tt.wantOverride == nil {
				if gotOverride != nil {
					t.Fatalf("override = %v, want nil", *gotOverride)
				}
				return
			}
			if gotOverride == nil {
				t.Fatalf("override = nil, want %v", *tt.wantOverride)
			}
			if *gotOverride != *tt.wantOverride {
				t.Fatalf("override = %v, want %v", *gotOverride, *tt.wantOverride)
			}
		})
	}
}

// TestSpawnSubagentTool_ParametersDocumentTimeout guards the JSON schema:
// the optional timeout property and its budget semantics must stay advertised
// to the model.
func TestSpawnSubagentTool_ParametersDocumentTimeout(t *testing.T) {
	schema := string(SpawnSubagentTool{Runner: nil}.Parameters())
	if !strings.Contains(schema, `"timeout"`) {
		t.Fatal("parameters schema does not advertise the timeout property")
	}
	if !strings.Contains(schema, "unlimited") {
		t.Fatal("timeout schema description does not document the unlimited semantics")
	}
	if !strings.Contains(schema, "--subagent-timeout/config value") {
		t.Fatal("timeout schema description does not document the omitted-means-global semantics")
	}
}

// TestSpawnSubagentTool_ParametersDocumentModelGate guards the JSON schema:
// the execution property must state the per-model parallel gate — a
// requested parallel is downgraded to serial when the agent's model does
// not explicitly set allow_parallel_execution: true.
func TestSpawnSubagentTool_ParametersDocumentModelGate(t *testing.T) {
	schema := string(SpawnSubagentTool{Runner: nil}.Parameters())
	if !strings.Contains(schema, "allow_parallel_execution") {
		t.Fatal("execution schema description does not name the allow_parallel_execution gate")
	}
	if !strings.Contains(schema, `is downgraded to`) {
		t.Fatal("execution schema description does not document the parallel-to-serial downgrade")
	}
	if !strings.Contains(schema, "models[] entry") {
		t.Fatal("execution schema description does not point at the models[] entry that closes or opens the gate")
	}
}

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
// resume ID reaches the runner untouched, the timeout override still
// applies, and no worktree validation runs (resume mode ignores the other
// fields).
func TestSpawnSubagentTool_ResumePassthrough(t *testing.T) {
	runnerCalled := false
	spawnTool := SpawnSubagentTool{
		Runner: func(ctx context.Context, request SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
			runnerCalled = true
			if !request.IsResume() {
				t.Errorf("request.IsResume() = false, want true")
			}
			if request.ResumeID != "coder-subagent-3" {
				t.Errorf("ResumeID = %q, want coder-subagent-3", request.ResumeID)
			}
			// goal/agent_type may be present but meaningless on resume;
			// the runner decides. The timeout still applies.
			if timeoutOverride == nil || *timeoutOverride != 30*time.Minute {
				t.Errorf("timeoutOverride = %v, want 30m", timeoutOverride)
			}
			return "resumed", nil
		},
	}

	result, err := spawnTool.Execute(context.Background(), json.RawMessage(
		`{"resume":"coder-subagent-3","goal":"ignored","agent_type":"ignored","timeout":"30m"}`))
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

// TestSpawnSubagentTool_CallStringResume pins the TUI-facing call string for
// a resume request: it says "Resuming", not "Spawning for unknown goal".
func TestSpawnSubagentTool_CallStringResume(t *testing.T) {
	got := SpawnSubagentTool{}.CallString(json.RawMessage(`{"resume":"researcher-subagent-2"}`))
	if !strings.Contains(got, "Resuming") || !strings.Contains(got, "researcher-subagent-2") {
		t.Errorf("CallString(resume) = %q, want the resuming wording with the ID", got)
	}
}

func subagentTimeoutPtr(d time.Duration) *time.Duration { return &d }

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
		Runner: func(ctx context.Context, request SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
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

// TestSpawnSubagentTool_FreshSpawnArgumentValidation pins the tool-level
// validation of the fresh-spawn argument surface (the schema cannot rely on
// provider-side "required" enforcement, and an empty string satisfies every
// JSON type). Every failure is an error RESULT (nil Go error) with the
// runner NEVER invoked — before the worktree check, so no branch or
// directory is created for a spawn that is going to be rejected anyway.
func TestSpawnSubagentTool_FreshSpawnArgumentValidation(t *testing.T) {
	tmp := t.TempDir()
	existing := filepath.Join(tmp, "notes.md")
	if err := os.WriteFile(existing, []byte("context"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		args        string
		wantInError string // substring the error result must carry
	}{
		{
			name:        "empty goal is rejected",
			args:        `{"goal":"","agent_type":"coder"}`,
			wantInError: `empty goal`,
		},
		{
			name:        "whitespace goal is rejected",
			args:        `{"goal":"   ","agent_type":"coder"}`,
			wantInError: `empty goal`,
		},
		{
			name:        "missing goal is rejected",
			args:        `{"agent_type":"coder"}`,
			wantInError: `empty goal`,
		},
		{
			name:        "unknown agent_type is rejected with the valid set",
			args:        `{"goal":"g","agent_type":"astronaut"}`,
			wantInError: `unknown agent_type "astronaut"`,
		},
		{
			name:        "empty agent_type is rejected",
			args:        `{"goal":"g"}`,
			wantInError: `empty agent_type`,
		},
		{
			name:        "missing ctx_file is rejected with the path named",
			args:        `{"goal":"g","agent_type":"coder","ctx_files":["` + filepath.Join(tmp, "gone.md") + `"]}`,
			wantInError: filepath.Join(tmp, "gone.md"),
		},
		{
			name:        "directory ctx_file is rejected",
			args:        `{"goal":"g","agent_type":"coder","ctx_files":["` + tmp + `"]}`,
			wantInError: "is a directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runnerCalled := false
			spawnTool := SpawnSubagentTool{
				Runner: func(ctx context.Context, request SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
					runnerCalled = true
					return "ok", nil
				},
			}

			result, err := spawnTool.Execute(context.Background(), json.RawMessage(tt.args))
			if err != nil {
				t.Fatalf("Execute() Go error = %v, want nil (an error result the model can read)", err)
			}
			if runnerCalled {
				t.Fatal("runner must not be invoked for invalid spawn arguments")
			}
			if !strings.Contains(result, tt.wantInError) {
				t.Fatalf("error result = %q, want it to contain %q", result, tt.wantInError)
			}
			if !strings.Contains(result, "Error:") {
				t.Fatalf("error result = %q, want the Error: prefix the other validation results carry", result)
			}
		})
	}

	t.Run("valid arguments reach the runner", func(t *testing.T) {
		var got SubagentSpawnRequest
		spawnTool := SpawnSubagentTool{
			Runner: func(ctx context.Context, request SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
				got = request
				return "ok", nil
			},
		}
		args := fmt.Sprintf(`{"goal":" port the module ","agent_type":"coder","ctx_files":[%q]}`, existing)
		result, err := spawnTool.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("Execute() Go error = %v, want nil", err)
		}
		if result != "ok" {
			t.Fatalf("result = %q, want the runner's ok", result)
		}
		if got.Goal != " port the module " {
			t.Errorf("goal = %q, want it passed through untrimmed (the trim is only a validation check)", got.Goal)
		}
		if len(got.CtxFiles) != 1 || got.CtxFiles[0] != existing {
			t.Errorf("ctx_files = %v, want the existing file", got.CtxFiles)
		}
	})

	t.Run("resume requests skip fresh-spawn validation", func(t *testing.T) {
		// A resume names the child and ignores every other field — the
		// goal-less request is exactly the shape a resume call takes when
		// the provider does not enforce the schema's required list.
		runnerCalled := false
		spawnTool := SpawnSubagentTool{
			Runner: func(ctx context.Context, request SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
				runnerCalled = true
				return "ok", nil
			},
		}
		result, err := spawnTool.Execute(context.Background(), json.RawMessage(`{"resume":"coder-subagent-0"}`))
		if err != nil {
			t.Fatalf("Execute() Go error = %v, want nil", err)
		}
		if !runnerCalled {
			t.Fatal("runner must be invoked for a resume request")
		}
		if result != "ok" {
			t.Fatalf("result = %q, want the runner's ok", result)
		}
	})
}
