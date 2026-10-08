package main

// Freeze/unfreeze engine (Phase 3, test verification surface). Freeze pauses
// a RUNNING background child at its next turn boundary — the same resumable
// state as the exit-interruption freeze (subagent_resume.go) — but reached
// while late is still alive: the manifest record is marked frozen (non-
// terminal, resumable), the child context is cancelled so the run loop
// unwinds cleanly, and the scheduler slot is released by the run closure's
// normal completion path (with the freeze-aware branch that reports frozen
// instead of cancelled). Unfreeze is the inverse: a frozen record is
// relaunched in the background through NewResumedSubagentOrchestrator with
// the SAME history path, so the resumed life APPENDS to the frozen state and
// a later resume keeps the whole picture.
//
// Nothing here is model-facing policy: both operations are exposed through
// spawn_subagent's "action" argument (internal/tool/subagent.go), and the
// runner body in main.go dispatches to them.

import (
	"context"
	"fmt"
	"time"

	"late/internal/agent"
	"late/internal/client"
	"late/internal/common"
	"late/internal/session"
	"late/internal/tool"
	"late/internal/tui"
)

// unfreezeDeps bundles the startup-scope collaborators the unfreeze relaunch
// needs (all resolved once in main, like the runner's own locals): the
// client factory for the child's routed model, the tool surface, the prompt
// switches, and the budget inputs.
type unfreezeDeps struct {
	// clientFor resolves the child's client by agent type (the agent_models
	// routing the fresh-spawn path applies). nil falls back to defaultClient.
	clientFor        func(agentType string) *client.Client
	defaultClient    *client.Client
	enabledTools     map[string]bool
	injectCWD        bool
	gemmaThinking    bool
	subagentMaxTurns int
	// messenger is the TUI messenger (child confirmations, status text),
	// carried as the interface the child constructor accepts.
	messenger tui.Messenger
	// Budget inputs re-derived at actual launch by
	// launchBackgroundSubagent: the per-spawn override (nil = global) and
	// the resolved global budget.
	timeoutOverride *time.Duration
	globalBudget    time.Duration
}

// agentNewResumed is a thin indirection over agent.NewResumedSubagentOrchestrator
// so the unfreeze path stays readable next to the resume closure in main.
func agentNewResumed(
	c *client.Client,
	record session.SubagentRecord,
	agentType string,
	deps unfreezeDeps,
	env *subagentRunEnv,
) (common.Orchestrator, string, error) {
	return agent.NewResumedSubagentOrchestrator(
		c, record, agentType, deps.enabledTools, deps.injectCWD,
		deps.gemmaThinking, deps.subagentMaxTurns, env.root, deps.messenger)
}

// freezeRunningSubagent freezes one live background child. The returned
// text is the model-facing tool result; a nil error with error-shaped text
// means the model should correct its arguments (the spawn tool's error-
// result convention).
//
// Only a child live in THIS process can be frozen — the scheduler is the
// live-children authority. The freeze is expressed in two steps, in this
// order: (1) RequestFreeze marks the child freeze-requested in the
// scheduler (so the run closure's completion branch reports frozen, never
// cancelled) and cancels the child context; (2) the manifest record is
// flipped to frozen through the preserving write, so the freeze can never
// clobber a terminal write the run closure raced in, and the scheduler's
// slot release happens on the closure's own completion.
func freezeRunningSubagent(
	scheduler *SubagentScheduler,
	sess *session.Session,
	sessionID string,
	childID string,
) (string, error) {
	if childID == "" {
		return "", fmt.Errorf("freeze requires a non-empty subagent id")
	}
	if scheduler == nil {
		return "Error: this session has no background subagent scheduler — freeze applies to children spawned with execution \"parallel\"/\"serial\".", nil
	}

	// Live child: freeze-request it (the cancel lands inside) and persist
	// the frozen status. The run closure unwinds, appends the frozen
	// notification, and releases the slot — no scheduler state is touched
	// here, so the state machine stays the single writer of running counts.
	if scheduler.RequestFreeze(childID) {
		if err := sess.MarkSubagentStatusPreserving(childID, session.SubagentStatusFrozen,
			"", ""); err != nil {
			common.LogErrorf("subagent-freeze", "failed to record frozen status for %s: %v", childID, err)
		}
		return fmt.Sprintf("Subagent %s freeze requested: it will pause at its next turn boundary and its manifest record is frozen. Its work state is preserved; resume it later with spawn_subagent {\"resume\": %q} or unfreeze it with spawn_subagent {\"action\": \"unfreeze\", \"id\": %q}.",
			childID, childID, childID), nil
	}

	// Not live here: report what the manifest says so the model can tell
	// "already frozen/interrupted" (nothing to do) from "unknown id"
	// (typo) from "terminal" (done for good).
	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		return "", fmt.Errorf("freeze %s: failed to load the session manifest: %w", childID, err)
	}
	rec, ok := manifest.Get(childID)
	if !ok {
		return fmt.Sprintf("Error: no subagent %q in this session's manifest — freeze needs the id of a RUNNING background subagent (one spawned with execution \"parallel\"/\"serial\").", childID), nil
	}
	if isInterruptedSubagentStatus(rec.Status) {
		return fmt.Sprintf("Subagent %s (%s) is already %s — nothing to freeze. Resume it with spawn_subagent {\"resume\": %q} to continue its work.",
			childID, rec.AgentType, rec.Status, childID), nil
	}
	return fmt.Sprintf("Error: subagent %s is %s — only a RUNNING background subagent can be frozen (sync spawns end with the tool call that spawned them).", childID, rec.Status), nil
}

// unfreezeFrozenSubagent relaunches a frozen child in the background through
// the resume constructor. It returns the same launch/queue acknowledgement
// shape a fresh background spawn does, so the parent's next steps read
// identically.
//
// The record must be frozen — a running/queued record means the child is
// already live in this process and there is nothing to unfreeze; terminal
// records are done for good. The relaunch reuses the record's identity:
// same ID (with the same "-r1" collision rule as resume), same history path
// (the resumed life APPENDS to the frozen state), same agent type and goal,
// and MarkResumed counts the unfreeze like any live resume.
func unfreezeFrozenSubagent(
	scheduler *SubagentScheduler,
	env *subagentRunEnv,
	deps unfreezeDeps,
	sess *session.Session,
	sessionID string,
	childID string,
	spawnCtx context.Context,
) (string, error) {
	if childID == "" {
		return "", fmt.Errorf("unfreeze requires a non-empty subagent id")
	}
	if scheduler != nil && scheduler.State(childID) != "" {
		return fmt.Sprintf("Subagent %s is already live in this process (%s) — nothing to unfreeze.",
			childID, scheduler.State(childID)), nil
	}

	manifest, err := session.LoadSubagentManifest(sessionID)
	if err != nil {
		return "", fmt.Errorf("unfreeze %s: failed to load the session manifest: %w", childID, err)
	}
	record, ok := manifest.Get(childID)
	if !ok {
		return fmt.Sprintf("Error: no subagent %q in this session's manifest — unfreeze needs the id of a FROZEN subagent (one you froze with spawn_subagent {\"action\": \"freeze\"}).", childID), nil
	}
	if record.Status != session.SubagentStatusFrozen {
		if session.IsTerminalSubagentStatus(record.Status) {
			return fmt.Sprintf("Error: subagent %s already terminated (%s) — spawn a fresh agent instead.", record.ID, record.Status), nil
		}
		return fmt.Sprintf("Error: subagent %s is %s, not frozen — nothing to unfreeze (a running child needs no unfreeze).", record.ID, record.Status), nil
	}
	if err := validateResumeRecord(record); err != nil {
		return "", err
	}

	agentType := record.AgentType
	if agentType == "" {
		// The manifest record is the identity source; an empty agent type is
		// a corrupt record — refuse before the constructor can guess.
		return "", fmt.Errorf("unfreeze %s: the manifest record has no agent_type", record.ID)
	}

	childClient := deps.defaultClient
	if deps.clientFor != nil {
		if routed := deps.clientFor(agentType); routed != nil {
			childClient = routed
		}
	}

	// Relaunch in the background: the frozen record flips back to running
	// (MarkResumed through the constructor — ResumeCount counts the
	// unfreeze like any live resume) and the child appends to the SAME
	// history path its frozen life used. No worktree is re-resolved here:
	// an unfreeze keeps the record's spawn-time environment, exactly like
	// the resume path's recorded-worktree drop when it no longer validates.
	child, _, err := agentNewResumed(childClient, *record, agentType, deps, env)
	if err != nil {
		return "", fmt.Errorf("unfreeze %s failed: %w", record.ID, err)
	}

	scheduling := subagentScheduling{
		Requested: tool.SubagentExecutionParallel,
		Effective: tool.SubagentExecutionParallel,
		ModelRef:  "",
	}
	return launchBackgroundSubagent(scheduler, env, sess, sessionID, child, agentType,
		record.Goal, "", spawnCtx, deps.timeoutOverride, deps.globalBudget, scheduling)
}
