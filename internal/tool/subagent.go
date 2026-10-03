package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"late/internal/assets"
	"late/internal/git"
)

// SubagentRunner executes one subagent run. timeoutOverride carries the
// per-spawn wall-clock budget parsed from the spawn_subagent "timeout"
// argument: nil = no override (the global --subagent-timeout/config value
// applies); a non-positive value = unlimited (no budget for this run);
// a positive value = a per-spawn budget overriding the global one.
//
// request is the parsed spawn request: a normal spawn fills the goal/
// ctx_files/agent_type fields; a resume request fills ResumeID (the runner
// then ignores the rest); a worktree spawn carries the validated worktree
// PATH in Worktree (the tool resolves branch names before the runner runs).
type SubagentRunner func(ctx context.Context, request SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error)

// SubagentSpawnRequest is the parsed spawn_subagent argument surface.
type SubagentSpawnRequest struct {
	Goal      string   `json:"goal"`
	CtxFiles  []string `json:"ctx_files"`
	AgentType string   `json:"agent_type"`
	Timeout   string   `json:"timeout"`
	// ResumeID, when non-empty, asks the runner to restore the child with
	// this ID from the session manifest instead of spawning fresh. All
	// other fields are ignored in that mode.
	ResumeID string `json:"resume"`
	// Worktree, when non-empty on a fresh spawn, is either a path to an
	// already-registered worktree of the current repo or a branch name to
	// create + check out in a new worktree. ValidateWorktree runs before
	// the runner and replaces branch names with the created worktree's
	// path, so the runner always sees a real directory here.
	Worktree string `json:"worktree"`
}

// IsResume reports whether this request restores an existing child instead
// of spawning a fresh one.
func (r SubagentSpawnRequest) IsResume() bool { return r.ResumeID != "" }

type SpawnSubagentTool struct {
	Runner SubagentRunner
}

func (t SpawnSubagentTool) Name() string { return "spawn_subagent" }
func (t SpawnSubagentTool) Description() string {
	return "Spawn a specialist subagent to perform a complex task. Use this when you need to isolate a task, such as researching a topic or writing a specific module. Pass \"resume\" instead of a goal to continue a previously interrupted subagent exactly where it stopped. " +
		"After any interruption notification, prefer {" + `"resume": "<id>"` + "} over re-spawning: the interrupted agent is restored exactly — same id, complete persisted history, full tool surface — and continues its task from where it stopped instead of redoing finished work."
}
func (t SpawnSubagentTool) Parameters() json.RawMessage {
	configs := assets.GetSubagents()
	var enums []string
	var descriptions []string
	for _, c := range configs {
		enums = append(enums, fmt.Sprintf(`"%s"`, c.Name))
		descriptions = append(descriptions, c.Description)
	}

	enumStr := strings.Join(enums, ", ")
	descStr := strings.Join(descriptions, " ")

	paramStr := fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"goal": { "type": "string", "description": "The specific goal or instruction for the subagent" },
			"ctx_files": {
				"type": "array",
				"items": { "type": "string" },
				"description": "List of file paths to provide as context to the subagent"
			},
			"agent_type": {
				"type": "string",
				"enum": [%s],
				"description": "The type of subagent to spawn. %s"
			},
			"timeout": {
				"type": "string",
				"description": "Optional wall-clock budget for this subagent run, e.g. \"45m\", \"2h\"; \"0\" = unlimited; omitted = the global --subagent-timeout/config value"
			},
			"resume": {
				"type": "string",
				"description": "The ID of a previously interrupted subagent to resume exactly where it stopped (e.g. \"coder-subagent-0\"). All other parameters are ignored; the agent restores with its complete history and continues its task."
			},
			"worktree": {
				"type": "string",
				"description": "Optional git worktree for this subagent to work in: either the path of an existing worktree of the current repository, or a branch name — a worktree for that branch is created first. The subagent's working directory becomes the worktree."
			}
		},
		"required": ["goal", "agent_type"]
	}`, enumStr, descStr)

	return json.RawMessage(paramStr)
}

func (t SpawnSubagentTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Runner == nil {
		return "", fmt.Errorf("subagent runner not configured")
	}

	var params SubagentSpawnRequest
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %v", err)
	}

	timeoutOverride, err := parseSubagentTimeout(params.Timeout)
	if err != nil {
		// Surface the failure as an error RESULT (nil Go error) so the model
		// can read the hint and retry with a valid duration.
		return fmt.Sprintf("Error: invalid subagent timeout %q — use a duration like 45m, 2h, or 0 for unlimited", params.Timeout), nil
	}

	if params.IsResume() {
		return t.Runner(ctx, params, timeoutOverride)
	}

	// Fresh-spawn input validation: every failure is an error RESULT (nil
	// Go error) and must land BEFORE the worktree check — worktree creation
	// has side effects (a branch and a directory), so a spawn with an empty
	// goal or unreadable context files must be rejected before any of them
	// happen. The model reads the hint and retries with fixed arguments.
	if hint := validateFreshSpawnArgs(params); hint != "" {
		return hint, nil
	}

	if params.Worktree != "" {
		wtPath, wtErr := ValidateWorktree(params.Worktree)
		if wtErr != nil {
			// Validation failures are error RESULTS (nil Go error): the
			// model reads the hint — registered paths, or a branch name —
			// and retries.
			return fmt.Sprintf("Error: %v", wtErr), nil
		}
		// The resolved worktree path replaces the raw argument so the
		// runner (and the manifest record) always sees the real directory
		// the child will run in.
		params.Worktree = wtPath
	}

	return t.Runner(ctx, params, timeoutOverride)
}

// validateFreshSpawnArgs checks the fresh-spawn argument surface the schema
// cannot guarantee (a provider may not enforce "required", and empty strings
// satisfy JSON types). Returns an error-RESULT string when validation fails —
// the spawn never reaches the runner — or "" when the arguments are usable:
//
//   - goal: non-empty after trimming. An empty goal would spawn a child whose
//     only instruction is "Goal: " — the child has nothing to do, yet the
//     manifest records it as running work.
//   - agent_type: one of the configured subagent types (the same source the
//     schema enum is built from). Without this check an unknown type fails
//     deeper in the runner with a bare Go error instead of a retryable hint.
//   - ctx_files: every entry must exist and be a readable FILE. Missing
//     entries used to be dropped silently when the goal message was built
//     (os.ReadFile error → skipped), so the model believed context was
//     attached when it was not.
func validateFreshSpawnArgs(params SubagentSpawnRequest) string {
	if strings.TrimSpace(params.Goal) == "" {
		return `Error: empty goal — pass the task for the subagent in the "goal" field`
	}

	if strings.TrimSpace(params.AgentType) == "" {
		return fmt.Sprintf("Error: empty agent_type — valid types: %s", configuredAgentTypes())
	}
	if !isConfiguredAgentType(params.AgentType) {
		return fmt.Sprintf("Error: unknown agent_type %q — valid types: %s", params.AgentType, configuredAgentTypes())
	}

	if len(params.CtxFiles) > 0 {
		var problems []string
		for _, f := range params.CtxFiles {
			info, err := os.Stat(f)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%q does not exist (%v)", f, err))
				continue
			}
			if info.IsDir() {
				problems = append(problems, fmt.Sprintf("%q is a directory, not a file", f))
			}
		}
		if len(problems) > 0 {
			return fmt.Sprintf("Error: unusable ctx_files entries — %s. Remove them or pass existing file paths, then spawn again",
				strings.Join(problems, "; "))
		}
	}
	return ""
}

// isConfiguredAgentType reports whether name is a configured subagent type.
// A broken/empty embedded registry disables the check (spawns then fail
// deeper in the runner with the historical "unknown agent type" error
// instead of being bricked at the door).
func isConfiguredAgentType(name string) bool {
	configs := assets.GetSubagents()
	if len(configs) == 0 {
		return true
	}
	for _, c := range configs {
		if c.Name == name {
			return true
		}
	}
	return false
}

// configuredAgentTypes renders the configured subagent type names for
// model-facing hints (falls back to a pointer at the config when the
// embedded registry is unavailable).
func configuredAgentTypes() string {
	configs := assets.GetSubagents()
	names := make([]string, 0, len(configs))
	for _, c := range configs {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return "see the subagent registry"
	}
	return strings.Join(names, ", ")
}

// parseSubagentTimeout parses the optional per-spawn "timeout" argument.
// Empty (absent) → nil override: the global budget applies.
// "0" or a negative duration → pointer to 0 (explicit unlimited).
// A positive duration → pointer to that budget.
// Anything else → a parse error for the caller to surface as an error result.
func parseSubagentTimeout(raw string) (*time.Duration, error) {
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return nil, err
	}
	if parsed <= 0 {
		unlimited := time.Duration(0)
		return &unlimited, nil
	}
	return &parsed, nil
}

func (t SpawnSubagentTool) RequiresConfirmation(args json.RawMessage) bool { return false }

func (t SpawnSubagentTool) CallString(args json.RawMessage) string {
	var params SubagentSpawnRequest
	if err := json.Unmarshal(args, &params); err == nil && params.ResumeID != "" {
		return fmt.Sprintf("Resuming subagent: %s", truncate(params.ResumeID, 50))
	}
	goal := getToolParam(args, "goal")
	if goal == "" {
		goal = "unknown goal"
	}
	return fmt.Sprintf("Spawning subagent for: %s", truncate(goal, 50))
}

// ValidateWorktree resolves the spawn_subagent "worktree" argument to a
// usable worktree directory. The argument is either:
//
//   - a path to an EXISTING directory: it must be one of the registered
//     worktrees of the current repository (git worktree list, compared
//     after symlink resolution) — anything else is rejected, so this
//     parameter can never point the child at an arbitrary directory;
//   - a BRANCH name: a worktree for that branch is created via
//     git.CreateWorktree under <repo-parent>/<repo>-worktrees/<branch>.
//     `git worktree add <path> <branch>` only accepts an existing branch
//     that is not checked out anywhere, so a branch already checked out in
//     a registered worktree reuses that worktree, and a branch that does
//     not exist yet is created at HEAD first (one bounded, fail-fast git
//     call with the same credential guards the git package applies).
//
// The repository root is resolved from the process CWD (git.RepoRoot) —
// the same CWD late runs children in. The returned path is the registered
// worktree path; a failure carries the model-facing hint.
func ValidateWorktree(worktree string) (string, error) {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return "", fmt.Errorf("empty worktree argument")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the current directory: %v", err)
	}
	root, isRepo := git.RepoRoot(cwd)
	if !isRepo {
		return "", fmt.Errorf("worktree %q requires a git repository but the current directory is not one", worktree)
	}

	if info, statErr := os.Stat(worktree); statErr == nil && info.IsDir() {
		return validateExistingWorktree(worktree)
	}
	return createWorktreeForBranch(worktree, root)
}

// validateExistingWorktree checks that path is a registered worktree of the
// repository and returns the registered path.
func validateExistingWorktree(path string) (string, error) {
	worktrees, err := git.ListWorktrees()
	if err != nil {
		return "", fmt.Errorf("failed to list the repository's worktrees (%v) — pass a branch name to create one instead", err)
	}
	// Registered paths and user-supplied paths can disagree through
	// symlinked parents (macOS /tmp → /private/tmp), so both sides are
	// resolved before comparing.
	realPath, realErr := filepath.EvalSymlinks(path)
	for _, wt := range worktrees {
		if wt.Path == path {
			return wt.Path, nil
		}
		if realErr == nil {
			if wtReal, err := filepath.EvalSymlinks(wt.Path); err == nil && wtReal == realPath {
				return wt.Path, nil
			}
		}
	}
	// Not registered: tell the model what IS registered so it can retry.
	registered := make([]string, 0, len(worktrees))
	for _, wt := range worktrees {
		registered = append(registered, wt.Path)
	}
	return "", fmt.Errorf("worktree %q is not a registered worktree of this repository (registered: %s) — pass one of those paths, or a branch name to create a new worktree", path, strings.Join(registered, ", "))
}

// createWorktreeForBranch creates a worktree for branch under the repo's
// sibling worktree directory and returns its path.
func createWorktreeForBranch(branch, root string) (string, error) {
	// A branch already checked out in a registered worktree reuses that
	// worktree — git refuses to check out the same branch twice.
	if worktrees, listErr := git.ListWorktrees(); listErr == nil {
		for _, wt := range worktrees {
			if wt.Branch == branch {
				return wt.Path, nil
			}
		}
	}

	dir := worktreesDirFor(root, branch)
	err := git.CreateWorktree(dir, branch)
	if err != nil {
		// `git worktree add <path> <branch>` also fails when the branch
		// does not exist yet ("invalid reference") — create it at HEAD and
		// retry once. A different failure (path exists, permission, …) is
		// reported as-is.
		if branchExists(root, branch) {
			return "", fmt.Errorf("failed to create a worktree for branch %q at %s (%v) — pass the path of an existing worktree instead", branch, dir, err)
		}
		if createErr := createBranchAtHead(root, branch); createErr != nil {
			return "", fmt.Errorf("failed to create branch %q (%v) — pass the path of an existing worktree instead", branch, createErr)
		}
		if err := git.CreateWorktree(dir, branch); err != nil {
			return "", fmt.Errorf("failed to create a worktree for new branch %q at %s (%v)", branch, dir, err)
		}
	}
	return dir, nil
}

// worktreesDirFor derives the directory a new worktree for branch is
// created in: a "<repo>-worktrees" sibling of the repo root, with branch
// separators sanitized to dashes so the path stays one element deep.
func worktreesDirFor(root, branch string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ', '\t':
			return '-'
		}
		return r
	}, branch)
	return filepath.Join(filepath.Dir(root), filepath.Base(root)+"-worktrees", sanitized)
}

// branchExists reports whether branch exists in the repo at root.
func branchExists(root, branch string) bool {
	cmd, cancel := boundedGitCmd(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	defer cancel()
	_, err := cmd.Output()
	return err == nil
}

// createBranchAtHead creates branch in the repo at root pointing at the
// current HEAD, without checking it out.
func createBranchAtHead(root, branch string) error {
	cmd, cancel := boundedGitCmd(root, "branch", branch)
	defer cancel()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// boundedGitCmd is the tool-package counterpart of internal/git's
// newGitCmd: a git invocation with a bounded context and the same fail-fast
// credential environment, so a wedged or prompting git can never hang a
// subagent spawn. (internal/git keeps no helper for these branch-plumbing
// calls; duplicating the 6-line guard here avoids widening that package's
// API for one caller.)
func boundedGitCmd(dir string, args ...string) (*exec.Cmd, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdBound)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=echo")
	return cmd, cancel
}

// gitCmdBound bounds every boundedGitCmd invocation (mirrors
// internal/git.gitCmdTimeout).
const gitCmdBound = 60 * time.Second
