package main

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"late/internal/common"
	"late/internal/session"
)

// Exit-time interrupted-status flush (Step 1 of the resume-completeness
// delta): a SIGINT/SIGTERM that kills late used to leave every live or
// queued background child marked "running" in the session manifest until
// the next read synthesized the interruption. The process already persists
// eagerly at every commit point, so this is NOT a checkpoint — it is the
// best-effort final flush over the manifest registry, bounded to a couple
// of seconds, reusing the preserving status writer so a terminal or frozen
// stamp can never be overwritten by it.
//
// Everything below is wired in main.go right before the TUI run loop
// starts, next to the unfreeze/live-restore dependency bundles: the same
// startup-scope collaborators (the session and the background scheduler)
// the other bundles hand around.

// signalFlushTimeout bounds the whole flushing step. Whatever is not done
// by the deadline is abandoned: the default handler re-raise below must
// still honor the signal's exit-code semantics, and the manifest failures
// the deadline cuts short degrade exactly like a non-flushed exit did —
// the next read's resume synthesis reproduces the interruption.
const signalFlushTimeout = 2 * time.Second

// signalFlushDeps bundles the exit-time flush's collaborators, built once
// in main() right after the scheduler (nil fields degrade the flush to
// root-history persistence only):
//
//   - sessionID is the manifest namespace ("" disables manifest writes).
//   - sess is the parent session (preserving status writes + history
//     persistence).
//   - scheduler exposes the live/queued children to stamp.
type signalFlushDeps struct {
	sessionID string
	sess      *session.Session
	scheduler *SubagentScheduler
}

// flushInterruptedSubagentStatuses stamps every orphanable background
// child in the session manifest with SubagentStatusInterrupted through
// MarkSubagentStatusPreserving — the preserve rule keeps the ResultPath
// and other stamps, and records whose status is already frozen or
// terminal are skipped outright so a late finished/freeze write the flush
// raced can never regress. It then flushes the root history so a signal
// delivered mid-turn does not lose the tail the commit points had not
// persisted yet. Every error is joined (and logged by the caller's
// counterpart path); nothing here is fatal — the remaining cleanup runs.
func flushInterruptedSubagentStatuses(deps signalFlushDeps) error {
	if deps.sess == nil {
		return nil
	}

	// Records the flush must never touch: a child that already reached a
	// terminal status, or was frozen (the persisted interruption), keeps
	// that stamp — interrupted is strictly a non-terminal→non-terminal
	// refinement of children the exit actually orphaned.
	skip := make(map[string]bool)
	if deps.sessionID != "" {
		if manifest, err := session.LoadSubagentManifest(deps.sessionID); err == nil {
			for i := range manifest.Records {
				status := manifest.Records[i].Status
				switch status {
				case session.SubagentStatusFrozen, session.SubagentStatusCompleted,
					session.SubagentStatusFailed, session.SubagentStatusCancelled:
					skip[manifest.Records[i].ID] = true
				}
			}
		}
	}

	var errs []error
	for _, id := range deps.scheduler.interruptibleChildren() {
		if skip[id] {
			continue
		}
		if err := deps.sess.MarkSubagentStatusPreserving(id, session.SubagentStatusInterrupted, "interrupted by signal", ""); err != nil {
			common.LogErrorf("signals", "failed to record interrupted status for %s: %v", id, err)
			errs = append(errs, err)
		}
	}

	// The root history flush is part of the same best-effort step: a
	// SIGTERM between commit points windows (e.g. mid-turn before the
	// runner's next save) keeps the last committed prefix — never MORE
	// than the commit points gave, never LESS persisted than this exit
	// can afford in its 2s budget.
	if err := deps.sess.PersistHistory(); err != nil {
		common.LogErrorf("signals", "failed to persist history before exit: %v", err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// installSignalTrap catches SIGINT/SIGTERM for the whole run: on the first
// signal it flushes the interrupted statuses and the root history under
// the hard 2s bound, restores the default handling so nothing intercepts
// the re-raise, then re-raises the same signal — the exit status stays the
// user-facing signal semantics (130/143), not a clean late return.
func installSignalTrap(deps signalFlushDeps) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig, ok := <-ch
		if !ok {
			return
		}
		common.LogErrorf("signals", "%v received — flushing interrupted subagent statuses", sig)

		// Best-effort under a deadline: the flush runs in its own
		// goroutine so a slow writer can never hold the process past the
		// bound; whatever it has not written in time is abandoned (the
		// abandoned goroutine's writes either land or do not, and the
		// resume synthesis covers both outcomes).
		done := make(chan struct{})
		go func() {
			_ = flushInterruptedSubagentStatuses(deps)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(signalFlushTimeout):
			common.LogErrorf("signals", "interrupted-status flush did not complete within %s — exiting", signalFlushTimeout)
		}

		signal.Stop(ch)
		// Re-raise with default handling restored so the process dies of
		// the SAME signal it received: the exit status is the shell's
		// 128+n, job-control tools behave, and a second Ctrl-C never lands
		// in a half-flushed state.
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(sig)
		}
	}()
}
