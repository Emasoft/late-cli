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

// Subagent execution modes (the spawn_subagent "execution" argument).
// Sync is the default and preserves the historical blocking behavior
// exactly; parallel and serial hand the child to the parent process's
// background scheduler (cmd/late/subagent_scheduler.go) and return to the
// model immediately.
const (
	SubagentExecutionSync     = "sync"     // block the tool call until the child finishes (default)
	SubagentExecutionParallel = "parallel" // run in the background immediately, unless a serial child is running
	SubagentExecutionSerial   = "serial"   // queue; runs alone when no other subagent is running
)

// Subagent lifecycle actions (the spawn_subagent "action" argument). They
// are orthogonal to the execution modes: freeze/unfreeze address an
// EXISTING child (by id), while execution selects how a NEW child is
// scheduled. Unfreeze relaunches in the background — the default for the
// resumed life of a previously frozen child.
const (
	SubagentActionFreeze   = "freeze"   // pause a running child, preserving its history (resumable)
	SubagentActionUnfreeze = "unfreeze" // resume a frozen child in the background
)

// NormalizeSubagentExecution maps an absent/empty "execution" argument to
// the sync default and passes known modes through. The bool result reports
// whether raw is an acceptable value at all.
func NormalizeSubagentExecution(raw string) (string, bool) {
	switch raw {
	case "":
		return SubagentExecutionSync, true
	case SubagentExecutionSync, SubagentExecutionParallel, SubagentExecutionSerial:
		return raw, true
	default:
		return "", false
	}
}

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
	// Action, when non-empty, asks the runner for a lifecycle operation
	// instead of a spawn or resume: "freeze" pauses a running child at its
	// next turn boundary (marking the manifest record frozen, resumable),
	// "unfreeze" resumes a frozen child in the background. Empty = absent.
	Action string `json:"action"`
	// ActionID is the subagent ID the action addresses ("freeze"/"unfreeze"
	// with spawn_subagent's "id" argument). Empty for spawns and resumes.
	ActionID string `json:"id"`
	// Worktree, when non-empty on a fresh spawn, is either a path to an
	// already-registered worktree of the current repo or a branch name to
	// create + check out in a new worktree. ValidateWorktree runs before
	// the runner and replaces branch names with the created worktree's
	// path, so the runner always sees a real directory here.
	Worktree string `json:"worktree"`
	// Execution selects how the child is scheduled: "sync" (the default —
	// the tool call blocks until the child finishes and its result becomes
	// the tool result), "parallel" (the child runs in the background right
	// away unless a serial child is running; the spawn returns
	// immediately), or "serial" (the child is queued and runs alone when no
	// other subagent is running; the spawn returns immediately). A
	// requested "parallel" is downgraded to "serial" by the per-model gate
	// when the agent's model has allow_parallel_execution != true (see the
	// execution schema description). Ignored for resume requests, which
	// always run synchronously.
	Execution string `json:"execution"`
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
			"action": {
				"type": "string",
				"enum": ["freeze", "unfreeze"],
				"description": "Lifecycle action instead of a spawn or resume. \"freeze\": pause a RUNNING subagent at its next turn boundary, marking it frozen (its history is preserved; resume later with spawn_subagent {\"resume\": id}). \"unfreeze\": resume a previously frozen subagent in the background (a running/queued child unfreezes nothing and reports its live state). All other parameters are ignored."
			},
			"id": {
				"type": "string",
				"description": "With \"action\": the subagent ID to freeze or unfreeze, exactly as a launch acknowledgement or [late harness] notification reported it (e.g. \"coder-subagent-2\"). Ignored without \"action\"."
			},
			"worktree": {
				"type": "string",
				"description": "Optional git worktree for this subagent to work in: either the path of an existing worktree of the current repository, or a branch name — a worktree for that branch is created first. The subagent's working directory becomes the worktree."
			},
			"execution": {
				"type": "string",
				"enum": ["sync", "parallel", "serial"],
				"description": "How this subagent is scheduled. \"sync\" (default) blocks this tool call until the subagent finishes and returns its full result. \"parallel\" runs the subagent in the background immediately (unless a serial subagent is running — then it queues and launches as a parallel batch when the serial chain drains) and returns to you at once. \"serial\" queues the subagent to run ALONE when no other subagent is running and returns to you at once. Model gate: a requested \"parallel\" is downgraded to \"serial\" when this agent's model has allow_parallel_execution != true (the config.json models[] entry routed to this agent must explicitly set \"allow_parallel_execution\": true; agents with no model routing are always downgraded) — the downgrade is stated in the spawn acknowledgement. Parallel/serial subagents report their outcome with a [late harness] history notification; fetch the full result with the subagent_results tool using the reported ID. Ignored for resume requests."
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

	// Lifecycle actions short-circuit before every spawn/resume surface:
	// they carry no goal and must never touch the worktree or timeout
	// validation. Unknown actions fall through and are rejected by the
	// runner (the tool stays schema-tolerant the same way it is for goal).
	if params.Action == SubagentActionFreeze || params.Action == SubagentActionUnfreeze {
		return t.Runner(ctx, params, nil)
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
//   - execution: one of the three scheduling modes (or absent for the sync
//     default). A typo would otherwise surface as a scheduler surprise only.
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

	if _, ok := NormalizeSubagentExecution(params.Execution); !ok {
		return fmt.Sprintf("Error: unknown execution %q — valid modes: %q (default), %q, %q",
			params.Execution, SubagentExecutionSync, SubagentExecutionParallel, SubagentExecutionSerial)
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

// capitalize upper-cases the first rune of s. It backs CallString's
// action labels ("freeze" → "Freeze") without pulling strings.Title
// (deprecated) or a unicode package for an ASCII-first two-word set.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + string(r[1:])
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
	if err := json.Unmarshal(args, &params); err == nil && params.Action != "" {
		id := getToolParam(args, "id")
		if id == "" {
			id = "unknown id"
		}
		return fmt.Sprintf("%s subagent: %s", capitalize(params.Action), truncate(id, 50))
	}
	goal := getToolParam(args, "goal")
	if goal == "" {
		goal = "unknown goal"
	}
	suffix := ""
	if mode, ok := NormalizeSubagentExecution(params.Execution); ok && mode != SubagentExecutionSync {
		suffix = " (" + mode + ")"
	}
	return fmt.Sprintf("Spawning subagent for: %s%s", truncate(goal, 50), suffix)
}

// SubagentResultsTool is the read side of background subagent execution:
// a child spawned with execution "parallel" or "serial" returns only a
// launch/queue acknowledgement, and its FULL result reaches the parent as
// a disk-backed artifact (manifest ResultPath → subagents/<id>.result.txt)
// announced by a [late harness] history notification. This tool resolves a
// child ID to that full result — or to the child's live scheduler state
// ("still running", "queued") when it has not finished yet.
//
// Like SpawnSubagentTool it is a thin shell over an injected closure: the
// tool package cannot import internal/session (import cycle), so the
// lookup — scheduler state, manifest, result file — lives in cmd/late and
// is wired in at registration, exactly like the spawn Runner.
type SubagentResultsTool struct {
	// Lookup resolves one subagent ID to its full stored result or live
	// state text. It never returns an empty string; not-found and other
	// degenerate cases are error-RESULT strings (nil Go error) so the
	// model can read the hint and correct the ID.
	Lookup func(ctx context.Context, id string) (string, error)
}

func (t SubagentResultsTool) Name() string { return "subagent_results" }

func (t SubagentResultsTool) Description() string {
	return "Fetch the full stored result of a background subagent — one spawned with the spawn_subagent execution argument set to \"parallel\" or \"serial\". Returns the complete final report for a finished subagent, or its live state (\"still running\", \"queued\") for one that has not finished yet. Background subagents announce completion with a [late harness] message; pass that message's id here. Sync subagents do not need this tool: their result already arrives in the spawn_subagent tool result."
}

func (t SubagentResultsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {
				"type": "string",
				"description": "The subagent ID exactly as the launch acknowledgement or the [late harness] completion notification reported it (e.g. \"coder-subagent-2\")"
			}
		},
		"required": ["id"]
	}`)
}

func (t SubagentResultsTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.Lookup == nil {
		return "", fmt.Errorf("subagent results lookup not configured")
	}
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %v", err)
	}
	if strings.TrimSpace(params.ID) == "" {
		return `Error: empty id — pass the subagent ID exactly as the launch acknowledgement or the [late harness] notification reported it (e.g. "coder-subagent-2")`, nil
	}
	return t.Lookup(ctx, params.ID)
}

func (t SubagentResultsTool) RequiresConfirmation(args json.RawMessage) bool { return false }

func (t SubagentResultsTool) CallString(args json.RawMessage) string {
	id := getToolParam(args, "id")
	if id == "" {
		id = "unknown id"
	}
	return fmt.Sprintf("Fetching subagent result: %s", truncate(id, 50))
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
