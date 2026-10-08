package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"late/internal/common"
	"late/internal/session"
	"late/internal/tool"
)

// SubagentScheduler owns the background half of subagent execution: the
// parallel/serial children spawned with the spawn_subagent "execution"
// argument. It is a small deterministic state machine with one rule pair
// (deliberately asymmetric):
//
//   - PARALLEL children launch immediately while no serial child is live.
//     A parallel child arriving during a serial run queues, and the whole
//     parallel queue launches as one batch the moment the serial chain
//     (a serial child may itself queue the NEXT serial child) drains.
//   - SERIAL children run only when NOTHING else is running — no parallel,
//     no serial — so a serial run is always alone on the machine.
//
// Sync spawns (the default) never reach the scheduler: they keep the
// historical blocking behavior. The orchestrator is the parent loop and is
// unaffected by both rules.
//
// Per-model gate: every mode reaching this scheduler is already the
// EFFECTIVE mode — the runner downgrades a requested parallel to serial
// when the child's agent_models-routed model does not explicitly allow
// parallel execution (config.json models[] "allow_parallel_execution":
// absent/false, or no model routing at all). The scheduler therefore never
// sees a "requested" mode: its parallel accounting counts only
// effective-parallel children, and every downgraded child is an ordinary
// serial entry (FIFO serial queue, runs alone).
//
// Crash model: queue persistence is NOT implemented this phase. A child
// queued or running at process death is lost — its manifest record stays
// non-terminal, the next resume flips it to "frozen" and synthesizes the
// interruption (see subagent_resume.go), exactly the recovery semantics of
// a sync child killed by an exit.
//
// Concurrency: one mutex guards the whole state machine; transitions are
// decided under the lock and executed (goroutine start, notification, disk)
// outside it, so a completion handler never blocks a spawn behind I/O.
type SubagentScheduler struct {
	mu      sync.Mutex
	running map[string]string // child ID → EFFECTIVE execution mode, live children only

	// liveChildren maps the child ID to its live orchestrator for the
	// running entries (nil for queued entries). It is the ID→orchestrator
	// bridge the freeze policy needs (Cancel on demand); entries are
	// registered in startLocked and deleted in finished, exactly like
	// running.
	liveChildren map[string]common.Orchestrator

	// serialRunning is the ID of the live serial child, "" when none. The
	// map alone cannot answer "is a serial child live" without a scan; the
	// dedicated field keeps the scheduling predicates O(1).
	serialRunning string

	// parallelRunning counts the live children whose EFFECTIVE mode is
	// parallel. It is kept in lockstep with running/serialRunning by
	// startLocked and finished, and exists so the "nothing else may run"
	// predicate counts parallel children explicitly — a count that can only
	// ever contain effective-parallel children, since the per-model gate
	// downgrades everything else to serial before the scheduler sees it.
	parallelRunning int

	serialQueue   []schedulerEntry
	parallelQueue []schedulerEntry

	// sess is the parent session completion notifications are appended to
	// (AddUserMessage — the established mid-loop harness-note shape).
	sess *session.Session
	// statusWriter persists scheduler-driven manifest transitions
	// (queued at enqueue, running at actual launch). Terminal statuses are
	// written by the run closure itself via classifyAndReportSubagentOutcome.
	// May be nil (tests); every error is logged, never fatal.
	statusWriter func(id, status string) error
	// statusWriter, running, queues above; the stall auto-resume path needs
	// no scheduler state: the watchdog cancels the child's run context and
	// the run closure's outcome processing (terminal record, notification
	// with the resume directive) flows through finished() unchanged — so a
	// stalled child drains the queue exactly like any finished child.

	// freezeRequested holds the child IDs whose freeze was requested while
	// their run closure was still executing. A freeze flips the record to
	// frozen and cancels the child's context; the run closure's completion
	// then MUST NOT overwrite frozen with a terminal status — the
	// freeze-aware branch in finished() consults this set, records the
	// frozen outcome, and the entry leaves the set with the slot release.
	freezeRequested map[string]bool
}

// schedulerEntry is one accepted background child: the identity the
// notifications and manifest need, plus the run closure that executes the
// child to completion and returns its classified outcome. The closure
// captures everything per-spawn (detached context, budget, child
// orchestrator) and is invoked exactly once, from a scheduler-owned
// goroutine. mode is the child's EFFECTIVE execution mode — the per-model
// parallel gate has already run; a requested parallel on a gated model
// arrives here as serial.
type schedulerEntry struct {
	id        string
	agentType string
	mode      string // tool.SubagentExecutionParallel or SubagentExecutionSerial
	run       func() subagentCompletion
	// childSource exposes the entry's live orchestrator once its run closure
	// has it (nil-func tolerated: the bridge stays empty and freeze policy
	// degrades to not-finding the child). The closure captures the child
	// per-spawn, so the accessor is closure-local by construction.
	childSource func() (common.Orchestrator, bool)
}

// subagentCompletion is what a background child reports when its run
// closure returns. Status mirrors the terminal manifest status the closure
// wrote; Preview is the ≤200-char head of the model-facing final text.
type subagentCompletion struct {
	ID        string
	AgentType string
	Status    string // session.SubagentStatusCompleted / Failed / Cancelled / ...
	Cause     string // termination cause for failed/cancelled, "" when completed
	Preview   string
}

// newSubagentScheduler wires the scheduler to the parent session and the
// manifest status writer. Both are per-session-lived, like the scheduler.
func newSubagentScheduler(sess *session.Session, statusWriter func(id, status string) error) *SubagentScheduler {
	return &SubagentScheduler{
		running:         make(map[string]string),
		liveChildren:    make(map[string]common.Orchestrator),
		freezeRequested: make(map[string]bool),
		sess:            sess,
		statusWriter:    statusWriter,
	}
}

// Launch accepts one background child. mode must be the child's EFFECTIVE
// execution mode (tool.SubagentExecutionParallel or SubagentExecutionSerial)
// — the per-model parallel gate has already run in the runner. Launch either
// starts the child immediately on a scheduler-owned goroutine (launched=true)
// or queues it and returns its 1-based queue position among same-mode
// entries. The run closure is invoked exactly once — now or when a drain
// reaches the entry.
func (s *SubagentScheduler) Launch(id, agentType, mode string, run func() subagentCompletion) (launched bool, position int) {
	return s.LaunchEntry(schedulerEntry{id: id, agentType: agentType, mode: mode, run: run})
}

// LaunchEntry is Launch with the full entry surface (the childSource bridge
// included). See Launch for the contract.
func (s *SubagentScheduler) LaunchEntry(entry schedulerEntry) (launched bool, position int) {
	s.mu.Lock()
	if s.canStartLocked(entry.mode) {
		s.startLocked(entry)
		s.mu.Unlock()
		return true, 0
	}
	queue := &s.parallelQueue
	if entry.mode == tool.SubagentExecutionSerial {
		queue = &s.serialQueue
	}
	*queue = append(*queue, entry)
	position = len(*queue)
	s.mu.Unlock()

	s.writeStatus(entry.id, session.SubagentStatusQueued)
	return false, position
}

// canStartLocked applies the scheduling rules. Caller holds s.mu. Both
// predicates reason about EFFECTIVE modes only: parallelRunning counts
// effective-parallel children and serialRunning is the live effective-serial
// child, so a model-gated (downgraded) child is indistinguishable from any
// other serial entry.
func (s *SubagentScheduler) canStartLocked(mode string) bool {
	if mode == tool.SubagentExecutionSerial {
		// Serial runs only when NOTHING runs — no parallel, no serial.
		return s.parallelRunning == 0 && s.serialRunning == ""
	}
	return s.serialRunning == "" // parallel yields only to a live serial child
}

// startLocked registers the entry as live and spawns its goroutine. The
// goroutine start itself happens here under the lock: it is cheap, and
// doing it inside guarantees a drained entry can never be double-counted
// or forgotten between unlock and start. Caller holds s.mu.
func (s *SubagentScheduler) startLocked(entry schedulerEntry) {
	s.running[entry.id] = entry.mode
	if entry.mode == tool.SubagentExecutionSerial {
		s.serialRunning = entry.id
	} else {
		s.parallelRunning++
	}
	if entry.childSource != nil {
		if child, ok := entry.childSource(); ok {
			s.liveChildren[entry.id] = child
		} else {
			delete(s.liveChildren, entry.id)
		}
	}
	go s.execute(entry)
}

// execute runs one accepted child to completion and hands the outcome back
// to the state machine. It is the only invoker of entry.run. The stall
// auto-resume path needs no special handling here: a stalled child's run
// closure performs its own terminal record (stall cause + resume
// directive) before returning, so finished() — notify, drain — proceeds
// exactly as for any completed child.
func (s *SubagentScheduler) execute(entry schedulerEntry) {
	completion := entry.run()
	s.finished(entry, completion)
}

// finished notifies the parent of the outcome, then records the child as
// done and drains the queues under one lock. The notification precedes the
// state transition deliberately: it appends into the parent session on the
// same goroutine, so once this child leaves the running set its
// notification is already in the history — an observer that sees the
// scheduler drained sees every notification too.
func (s *SubagentScheduler) finished(entry schedulerEntry, completion subagentCompletion) {
	// Freeze-aware outcome: a freeze-cancelled run must be reported as
	// frozen (the resumable state the freeze write landed), never as
	// "cancelled by the user". The marker is consumed exactly once here —
	// the entry is leaving the running set, so no later completion can
	// double-consume.
	if s.FreezeRequested(entry.id) {
		completion.Status = session.SubagentStatusFrozen
		completion.Cause = ""
		completion.Preview = previewText("Subagent paused (freeze) — its conversation is preserved and it can be resumed with spawn_subagent {\"resume\": \""+entry.id+"\"} or unfrozen with spawn_subagent {\"action\": \"unfreeze\", \"id\": \""+entry.id+"\"}.", manifestResultPreviewLimit)
		// Re-assert the frozen status on the manifest: the freeze write and
		// the run's own terminal write race, and MarkSubagentStatus's freeze
		// guard only covers the order where the freeze landed FIRST. Here the
		// freeze marker was still set at completion time — the freeze write
		// may not have landed yet (or lost the race), so the status goes
		// through the preserving write, which can never clobber the identity
		// fields and wins by running last.
		if s.sess != nil {
			if err := s.sess.MarkSubagentStatusPreserving(entry.id, session.SubagentStatusFrozen, "", ""); err != nil {
				common.LogErrorf("subagent-scheduler", "failed to persist frozen status for %s: %v", entry.id, err)
			}
		}
	}
	s.notifyParent(completion)

	var drained []schedulerEntry
	s.mu.Lock()
	delete(s.running, entry.id)
	delete(s.liveChildren, entry.id)
	if entry.mode == tool.SubagentExecutionSerial {
		if s.serialRunning == entry.id {
			s.serialRunning = ""
		}
	} else {
		s.parallelRunning--
	}
	drained = s.drainLocked()
	s.mu.Unlock()

	// writeStatus after unlock: disk I/O never under the state machine.
	// Drained entries re-mark "running" — an immediate launch already says
	// running, so the extra write is a harmless same-value transition.
	for _, e := range drained {
		s.writeStatus(e.id, session.SubagentStatusRunning)
	}
}

// drainLocked pops every entry the rules now allow to start, registering
// each as live before returning. A serial head always wins first (it needs
// an empty machine); the parallel batch launches only once the serial queue
// is empty and no serial child is live. Caller holds s.mu.
func (s *SubagentScheduler) drainLocked() []schedulerEntry {
	var drained []schedulerEntry
	for {
		if len(s.serialQueue) > 0 && s.canStartLocked(tool.SubagentExecutionSerial) {
			next := s.serialQueue[0]
			s.serialQueue = s.serialQueue[1:]
			s.startLocked(next)
			drained = append(drained, next)
			continue
		}
		if len(s.serialQueue) == 0 && len(s.parallelQueue) > 0 && s.serialRunning == "" {
			batch := s.parallelQueue
			s.parallelQueue = nil
			for _, e := range batch {
				s.startLocked(e)
			}
			drained = append(drained, batch...)
		}
		return drained
	}
}

// notifyParent appends the completion notification into the parent session
// history — the same mid-loop user-role harness-note shape the executor
// uses for failed shell commands. The parent may be mid-stream: the append
// is historyMu-guarded (thread-safe) and generation-checked on persist; the
// notification simply appears in the parent's next request. A failed append
// degrades to a lost notification — the child's result is still on disk and
// subagent_results can fetch it — so it is logged, never fatal.
func (s *SubagentScheduler) notifyParent(c subagentCompletion) {
	if s.sess == nil {
		return
	}
	outcome := c.Status
	if c.Cause != "" {
		outcome = fmt.Sprintf("%s (%s)", c.Status, c.Cause)
	}
	note := fmt.Sprintf("[late harness] subagent %s (%s) %s. Result preview: %s. Full result: call subagent_results with {\"id\": %q}.",
		c.ID, c.AgentType, outcome, c.Preview, c.ID)
	if c.Status == session.SubagentStatusFrozen {
		// Freeze (Phase 3): the notification must carry the resume and
		// unfreeze directives — the child is not dead, it is parked, and
		// the parent decides when its work continues.
		note += fmt.Sprintf(" The agent was FROZEN at its own request boundary; its conversation is fully preserved — resume it to continue exactly where it stopped: call spawn_subagent with {\"resume\": %q}, or relaunch it in the background with {\"action\": \"unfreeze\", \"id\": %q}.", c.ID, c.ID)
	}
	if strings.HasPrefix(c.Cause, "stalled:") {
		// Stall auto-resume: the directive must reach the parent IN the
		// notification — the preview above can clip the one inside the
		// full result text, and a stalled child is exactly the child the
		// parent should resume instead of re-spawning.
		note += fmt.Sprintf(" The agent stalled and was cancelled; its conversation is fully preserved — resume it to continue exactly where it stopped: call spawn_subagent with {\"resume\": %q}.", c.ID)
	}
	if err := s.sess.AddUserMessage(note); err != nil {
		common.LogErrorf("subagent-scheduler", "failed to append completion notification for %s: %v", c.ID, err)
	}
}

// writeStatus persists one scheduler-driven manifest transition. Nil
// statusWriter (tests) and a missing record are silently tolerated; other
// failures are logged — a lost "queued" stamp only degrades resume
// wording, never the run.
func (s *SubagentScheduler) writeStatus(id, status string) {
	if s.statusWriter == nil {
		return
	}
	if err := s.statusWriter(id, status); err != nil {
		common.LogErrorf("subagent-scheduler", "failed to record %s status for %s: %v", status, id, err)
	}
}

// RequestFreeze marks one LIVE child freeze-requested and cancels its run
// context so the run loop unwinds at the next boundary. It reports whether
// the child was found among the live running entries — queued children are
// NOT freezable (they have no context to cancel and have written no
// history; a future launch would immediately fight the freeze marker).
//
// The freeze marker is read by the completion path (FreezeRequested) so a
// freeze-cancelled run is reported as frozen — never as "cancelled by the
// user" — and the manifest record keeps the resumable status the freeze
// write landed.
func (s *SubagentScheduler) RequestFreeze(id string) bool {
	s.mu.Lock()
	mode, ok := s.running[id]
	if !ok {
		s.mu.Unlock()
		return false
	}
	s.freezeRequested[id] = true
	s.mu.Unlock()

	if child, live := s.LookupRunning(id); live {
		common.LogErrorf("subagent-freeze", "freeze requested for %s (%s) — cancelling its run context", id, mode)
		child.Cancel()
	}
	return true
}

// LookupRunning returns the live child orchestrator for id, whether it is
// currently running (not queued). The scheduler owns the run closures, so
// it is the only component that can map a child ID back to its
// common.Orchestrator (for Cancel and freeze policy).
func (s *SubagentScheduler) LookupRunning(id string) (common.Orchestrator, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	child, ok := s.liveChildren[id]
	return child, ok
}

// FreezeRequested reports — and consumes — the freeze marker for id. The
// run closure's completion path calls it exactly once per entry: consuming
// keeps the map from growing with every freeze for the lifetime of the
// process.
func (s *SubagentScheduler) FreezeRequested(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := s.freezeRequested[id]
	delete(s.freezeRequested, id)
	return req
}

// State reports where the scheduler holds a child right now:
// "running" (live, parallel or serial), "queued" (accepted, not started),
// or "" (unknown to this scheduler — the caller falls through to the
// manifest, which is authoritative for terminal and frozen children).
func (s *SubagentScheduler) State(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode, ok := s.running[id]; ok {
		if mode == tool.SubagentExecutionSerial {
			return session.SubagentStatusRunning + " (serial)"
		}
		return session.SubagentStatusRunning + " (parallel)"
	}
	for i := range s.serialQueue {
		if s.serialQueue[i].id == id {
			return session.SubagentStatusQueued + " (serial, position " + fmt.Sprint(i+1) + ")"
		}
	}
	for i := range s.parallelQueue {
		if s.parallelQueue[i].id == id {
			return session.SubagentStatusQueued + " (parallel, position " + fmt.Sprint(i+1) + ")"
		}
	}
	return ""
}

// subagentCompletionStatus classifies a finished run into the notification's
// manifest-shaped status. It mirrors the precedence of
// classifyAndReportSubagentOutcome — idle kill before budget before cancel
// before crash — but answers "which terminal status" instead of building the
// parent-facing text, keeping the battle-tested classifier untouched.
func subagentCompletionStatus(child common.Orchestrator, err error, octx subagentOutcomeContext) (status, cause string) {
	if sa, ok := child.(interface{ IdleKillReason() string }); ok {
		if reason := sa.IdleKillReason(); reason != "" {
			if strings.HasPrefix(reason, "stalled:") {
				// Stall verdict passes through verbatim (see the matching
				// branch in classifyAndReportSubagentOutcome): the
				// subagent_results lookup keys on the prefix.
				return session.SubagentStatusFailed, reason
			}
			return session.SubagentStatusFailed, fmt.Sprintf("idle: killed by the harness idle watchdog (%s)", reason)
		}
	}
	if octx.runBudget > 0 && octx.runCtx.Err() == context.DeadlineExceeded {
		return session.SubagentStatusFailed, fmt.Sprintf("time budget exhausted (%s)", octx.runBudget)
	}
	if errors.Is(err, context.Canceled) || child.IsStopRequested() {
		return session.SubagentStatusCancelled, "cancelled or killed by the user"
	}
	if err != nil {
		return session.SubagentStatusFailed, fmt.Sprintf("crashed: %v", err)
	}
	return session.SubagentStatusCompleted, ""
}

// recordSubagentResult persists a background child's FULL final text to
// <sessionsDir>/<sessionID>/subagents/<id>.result.txt and points the
// manifest record's ResultPath at it. The file is written BEFORE the
// manifest pointer, so the recorded path is always durable. The full text —
// not just the preview — is what subagent_results streams back to the
// parent model on demand. Best-effort: persistence failures are logged and
// never fail the run (the preview notification already went out or is about
// to; the parent still sees the outcome).
//
// The manifest pointer goes through sess.MarkSubagentResultPath, whose
// load-modify-save runs under the session package's manifest mutex — two
// background children finishing concurrently must not drop each other's
// terminal writes.
func recordSubagentResult(sess *session.Session, sessionID, childID, finalText string) {
	if sessionID == "" {
		// In-memory session: no folder, no manifest, no durable result.
		return
	}
	path, err := session.SubagentResultPath(sessionID, childID)
	if err != nil {
		common.LogErrorf("subagent-scheduler", "subagent %s result path unavailable: %v", childID, err)
		return
	}
	// 0700, matching the subagents folder the manifest and histories live
	// in (session.manifestDirMode is private; keep the same value here).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		common.LogErrorf("subagent-scheduler", "subagent %s result dir unavailable: %v", childID, err)
		return
	}
	// 0600, matching the manifest file mode: results can carry
	// repository-derived content and stay private to the user.
	if err := os.WriteFile(path, []byte(finalText), 0o600); err != nil {
		common.LogErrorf("subagent-scheduler", "subagent %s result file not written: %v", childID, err)
		return
	}
	if err := sess.MarkSubagentResultPath(childID, path); err != nil {
		common.LogErrorf("subagent-scheduler", "subagent %s result path not recorded: %v", childID, err)
	}
}

// subagentScheduling captures how one background child is scheduled: the
// execution mode the parent REQUESTED, the EFFECTIVE mode after the
// per-model parallel gate (see effectiveSubagentExecutionMode), and the
// agent_models reference of the child's routed model ("" = the default
// subagent client, which is never allowed parallel). It feeds the scheduler
// (Effective), the launch/queue acknowledgment (the downgrade note), and
// the manifest record (Effective + ModelRef).
type subagentScheduling struct {
	Requested string
	Effective string
	ModelRef  string
}

// effectiveSubagentExecutionMode applies the per-model parallel gate to a
// (already tool-normalized) requested execution mode:
//
//   - sync stays sync — the historical blocking path, untouched by the gate;
//   - serial stays serial — the parent explicitly asked to run alone;
//   - parallel is honored only when modelAllowsParallel is true; otherwise
//     the child is DOWNGRADED to serial: it joins the serial queue in
//     request order and runs alone once everything else (the live parallel
//     batch and any serials queued ahead of it) has drained.
//
// The caller passes modelAllowsParallel = model.AllowsParallelExecution()
// for the agent's agent_models-routed model entry, and FALSE when the agent
// has no model entry at all: no routing means the default subagent client,
// whose capacity is unknown, so the conservative choice is serial
// (config.ModelSetting docs the motivation — single-instance local models
// and rate-limited providers).
func effectiveSubagentExecutionMode(requested string, modelAllowsParallel bool) string {
	switch requested {
	case tool.SubagentExecutionParallel:
		if modelAllowsParallel {
			return tool.SubagentExecutionParallel
		}
		return tool.SubagentExecutionSerial
	case tool.SubagentExecutionSerial:
		return tool.SubagentExecutionSerial
	default: // sync (the normalized "" default) — the gate never applies
		return tool.SubagentExecutionSync
	}
}

// downgradedToSerial reports whether the gate turned a requested parallel
// into an effective serial run — the one case the acknowledgment must
// explain, so the parent model understands why the child is not running in
// parallel.
func downgradedToSerial(s subagentScheduling) bool {
	return s.Requested == tool.SubagentExecutionParallel && s.Effective == tool.SubagentExecutionSerial
}

// modelDisplayName renders a subagentScheduling.ModelRef for model-facing
// text: the agent_models reference, or "the default subagent model" when the
// child has no model routing (which also means the parallel gate is closed).
func modelDisplayName(modelRef string) string {
	if modelRef == "" {
		return "the default subagent model"
	}
	return modelRef
}

// launchBackgroundSubagent is the parallel/serial branch of the spawn
// runner: the child is already fully constructed (its constructor minted
// the ID and wrote the "running" manifest record), so this function hands
// it to the scheduler together with a run closure that executes it exactly
// like the sync path does — buildAndWireChild wiring, outcome
// classification, terminal manifest write — plus the two background-only
// steps: the full-result artifact (recordSubagentResult) and the completion
// payload the scheduler turns into the parent notification. It returns the
// immediate launch/queue acknowledgement the parent model reads.
//
// Context layering for a background run: the run closure derives its
// context from context.WithoutCancel of the SPAWN-time context — values
// preserved, cancellation detached — because the spawn context belongs to
// the parent's current run and dies with it (BaseOrchestrator.run cancels
// its derived context on return), while a background child outlives that
// run by design. The per-spawn/global budget is re-derived at actual launch
// (not at enqueue), so a queued child's budget is never burned while it
// waits. Process exit still kills everything — the crash model is
// unchanged — and the budget deadline and idle watchdog still bound the
// child exactly as for a sync run.
func launchBackgroundSubagent(
	scheduler *SubagentScheduler,
	env *subagentRunEnv,
	sess *session.Session,
	sessionID string,
	child common.Orchestrator,
	agentType, goal, worktree string,
	spawnCtx context.Context,
	timeoutOverride *time.Duration,
	globalBudget time.Duration,
	scheduling subagentScheduling,
) (string, error) {
	childID := child.ID()
	detached := context.WithoutCancel(spawnCtx)

	run := func() subagentCompletion {
		runBudget := effectiveSubagentBudget(timeoutOverride, globalBudget)
		runCtx := detached
		var runCancel context.CancelFunc = func() {}
		if runBudget > 0 {
			runCtx, runCancel = context.WithTimeout(detached, runBudget)
		}
		// Carry the cancel inside the context for the stall watchdog
		// (withStallCancel) — a wedged background run must be cancellable
		// even when the budget is unlimited.
		runCtx = withStallCancel(runCtx, runCancel)
		defer runCancel()

		res, err := buildAndWireChild(env, child, wireChildConfig{
			runCtx:    runCtx,
			worktree:  worktree,
			runBudget: runBudget,
		})
		octx := subagentOutcomeContext{runCtx: runCtx, runBudget: runBudget}
		final, cerr := classifyAndReportSubagentOutcome(sess, child, agentType, goal, res, err, octx)
		if cerr != nil {
			// Classification itself failed (never the run's outcome —
			// that is err above). Fall back to the raw result so the
			// notification and the result file still carry the outcome.
			common.LogErrorf("subagent-scheduler", "subagent %s outcome classification failed: %v", childID, cerr)
			final = res
		}
		status, cause := subagentCompletionStatus(child, err, octx)
		recordSubagentResult(sess, sessionID, childID, final)
		return subagentCompletion{
			ID:        childID,
			AgentType: agentType,
			Status:    status,
			Cause:     cause,
			Preview:   previewText(final, manifestResultPreviewLimit),
		}
	}

	// Label the manifest record with the EFFECTIVE mode and the routed
	// model BEFORE the scheduler transition: a queued child is then already
	// labeled with why it is queued (a requested parallel downgraded to
	// serial reads back as execution:"serial" + the model that closed the
	// gate). Best-effort — a lost stamp degrades status/debug wording only,
	// exactly like the other manifest refinements.
	if err := sess.MarkSubagentExecution(childID, scheduling.Effective, scheduling.ModelRef); err != nil {
		common.LogErrorf("subagent-scheduler", "subagent %s execution/model not recorded: %v", childID, err)
	}

	launched, position := scheduler.LaunchEntry(schedulerEntry{
		id:        childID,
		agentType: agentType,
		mode:      scheduling.Effective,
		run:       run,
		childSource: func() (common.Orchestrator, bool) {
			return child, child != nil
		},
	})
	// downgradedNote explains a per-model parallel downgrade inline so the
	// parent model never wonders why a requested parallel runs serially.
	downgradedNote := ""
	if downgradedToSerial(scheduling) {
		downgradedNote = fmt.Sprintf(" Requested \"parallel\" was downgraded to \"serial\": the agent's model (%s) does not allow parallel execution — set \"allow_parallel_execution\": true on its models[] entry.",
			modelDisplayName(scheduling.ModelRef))
	}
	if launched {
		return fmt.Sprintf("subagent %s (%s) launched in %s execution in the background%s; you will be notified on completion; fetch the full result with subagent_results (id: %q). Continue with your own work in the meantime.",
			childID, agentType, scheduling.Effective, downgradedNote, childID), nil
	}
	return fmt.Sprintf("subagent %s (%s) queued at position %d for %s execution%s; you will be notified on completion; fetch the full result with subagent_results (id: %q). Continue with your own work in the meantime.",
		childID, agentType, position, scheduling.Effective, downgradedNote, childID), nil
}

// subagentResultsLookup builds the closure SubagentResultsTool executes:
// resolve a subagent ID to its full stored result or live state. Precedence:
//
//  1. scheduler live state — running/queued are authoritative while this
//     process holds the child;
//  2. the manifest — terminal statuses read the stored full result file
//     (falling back to the recorded preview when the file is gone);
//     "frozen" explains the interruption and points at resume;
//     "running" in the manifest but absent from the scheduler means the
//     record was written by a child whose launch is still in flight.
func subagentResultsLookup(sess *session.Session, scheduler *SubagentScheduler, sessionID string) func(ctx context.Context, id string) (string, error) {
	return func(ctx context.Context, id string) (string, error) {
		if scheduler != nil {
			if state := scheduler.State(id); state != "" {
				return fmt.Sprintf("Subagent %s is still %s. You will receive a [late harness] notification when it finishes; this tool will then return its full result.", id, state), nil
			}
		}
		if sessionID == "" {
			return fmt.Sprintf("No subagent %q: this session keeps no manifest (in-memory session), so background subagent results cannot be resolved.", id), nil
		}
		manifest, err := session.LoadSubagentManifest(sessionID)
		if err != nil {
			return "", fmt.Errorf("failed to load the session manifest: %w", err)
		}
		rec, ok := manifest.Get(id)
		if !ok {
			return fmt.Sprintf("No subagent %q in this session's manifest — the ID must come from a spawn_subagent result or a [late harness] notification of this session.", id), nil
		}
		switch rec.Status {
		case session.SubagentStatusQueued:
			return fmt.Sprintf("Subagent %s (%s) is still queued and has not started running. You will be notified when it finishes.", id, rec.AgentType), nil
		case session.SubagentStatusRunning:
			return fmt.Sprintf("Subagent %s (%s) is still running. You will be notified when it finishes.", id, rec.AgentType), nil
		case session.SubagentStatusFrozen:
			text := fmt.Sprintf("Subagent %s (%s) is FROZEN (it was paused by a freeze request or by a previous late exit and never finished).", id, rec.AgentType)
			if rec.HistoryPath != "" {
				text += fmt.Sprintf(" Its work state is preserved at %s", rec.HistoryPath)
			}
			text += fmt.Sprintf(". To continue it exactly where it stopped, call spawn_subagent with {\"resume\": %q}.", id)
			return text, nil
		}
		// Terminal: the full result is the stored file, the manifest preview
		// is the fallback when the file was deleted.
		if !session.IsTerminalSubagentStatus(rec.Status) {
			// Unknown future status: report it factually instead of guessing.
			return fmt.Sprintf("Subagent %s (%s) has manifest status %q.", id, rec.AgentType, rec.Status), nil
		}
		if rec.ResultPath != "" {
			data, readErr := os.ReadFile(rec.ResultPath)
			if readErr == nil && len(data) > 0 {
				return fmt.Sprintf("Subagent %s (%s) — %s:\n\n%s", id, rec.AgentType, rec.Status, string(data)), nil
			}
			common.LogErrorf("subagent-scheduler", "subagent %s result file unavailable: %v", id, readErr)
		}
		if rec.ResultPreview != "" {
			return fmt.Sprintf("Subagent %s (%s) — %s (full result file unavailable, recorded preview only):\n\n%s", id, rec.AgentType, rec.Status, rec.ResultPreview), nil
		}
		// Terminal without a preview (failed/cancelled children have none):
		// surface the recorded cause. A stalled child is called out with
		// the resume directive — its preserved history is exactly what the
		// resume restores (the auto-resume loop: stall → cancel → notify →
		// parent resumes).
		if rec.Status == session.SubagentStatusFailed && strings.HasPrefix(rec.Cause, "stalled:") {
			return fmt.Sprintf("Subagent %s (%s) — %s Its conversation is fully preserved; resume it to continue exactly where it stopped: call spawn_subagent with {\"resume\": %q}.",
				id, rec.AgentType, rec.Cause, id), nil
		}
		cause := rec.Cause
		if cause == "" {
			cause = "no recorded cause"
		}
		if rec.TranscriptPath != "" {
			cause += fmt.Sprintf("; pruned transcript: %s", rec.TranscriptPath)
		}
		return fmt.Sprintf("Subagent %s (%s) — %s: %s.", id, rec.AgentType, rec.Status, cause), nil
	}
}
