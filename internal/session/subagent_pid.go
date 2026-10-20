package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"late/internal/common"
)

// Background subagent children are goroutines inside the late process that
// owns the session (see cmd/late's SubagentScheduler), not detached OS
// processes — so every PID file written at launch records that owner's
// process ID (os.Getpid()). The PID file is the crash-model ingredient the
// manifest alone lacks: a record that still says "running" at the next
// process's resume time can no longer be read as either state when the
// previous process might STILL be alive. The PID file disambiguates: a
// record whose owner process is alive keeps its live/running semantics (the
// other process owns the child and its outcome; resume must not steal the
// slot), while a record whose PID file is dead or gone is the familiar
// interrupted case (freeze + relaunch).
//
// Windows: the kill(pid, 0) syscall is not exposed, so liveness degrades to
// "the PID file exists" (the presence/absence lifecycle — written at launch,
// removed on terminal or freeze — is authoritative). The owner-process
// disambiguation is best-effort there; documented behavior, not a bug.

// SubagentPIDPath returns the full path of a subagent's PID file:
// <sessionsDir>/<parentSessionID>/subagents/<childID>.pid.
func SubagentPIDPath(parentSessionID, childID string) (string, error) {
	if !isValidPathElement(parentSessionID) {
		return "", fmt.Errorf("invalid session ID: %q", parentSessionID)
	}
	if !isValidPathElement(childID) {
		return "", fmt.Errorf("invalid child ID: %q", childID)
	}

	dir, err := SubagentHistoryDir(parentSessionID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, childID+".pid"), nil
}

// WriteSubagentPID atomically records a subagent's owner-process PID. The
// write goes through the shared temp+rename primitive (writeAtomic), so a
// reader never observes a partial PID.
func WriteSubagentPID(sessionID, childID string, pid int) error {
	if !isValidPathElement(sessionID) {
		return fmt.Errorf("invalid session ID: %q", sessionID)
	}
	if !isValidPathElement(childID) {
		return fmt.Errorf("invalid child ID: %q", childID)
	}
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d for subagent %s", pid, childID)
	}
	path, err := SubagentPIDPath(sessionID, childID)
	if err != nil {
		return err
	}
	return writeAtomic(path, []byte(strconv.Itoa(pid)+"\n"), historyFileMode, 0700)
}

// RemoveSubagentPID deletes a subagent's PID file. A missing file is
// success (removal races with the owner's own removal on terminal or
// freeze); every other error is surfaced.
func RemoveSubagentPID(sessionID, childID string) error {
	if !isValidPathElement(sessionID) {
		return fmt.Errorf("invalid session ID: %q", sessionID)
	}
	if !isValidPathElement(childID) {
		return fmt.Errorf("invalid child ID: %q", childID)
	}
	path, err := SubagentPIDPath(sessionID, childID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove subagent pid file: %w", err)
	}
	return nil
}

// IsSubagentAlive reports whether the owner process recorded in a
// subagent's PID file is still alive.
//
//   - missing file ⇒ (false, nil): no owner — the interrupted case.
//   - non-integer content ⇒ (false, error with context): a corrupt file is
//     not silently equated with "dead owner"; the caller decides.
//   - pid == os.Getpid() ⇒ alive: the querying process IS the owner.
//   - otherwise kill(pid, 0): nil ⇒ alive; ESRCH ⇒ dead; EPERM ⇒ the
//     process exists under a different owner — conservatively reported as
//     alive-foreign ⇒ (false, nil) with a log, because stealing a slot we
//     cannot verify is worse than the (benign, self-healing next resume)
//     orphan being left alone.
func IsSubagentAlive(sessionID, childID string) (bool, error) {
	if !isValidPathElement(sessionID) {
		return false, fmt.Errorf("invalid session ID: %q", sessionID)
	}
	if !isValidPathElement(childID) {
		return false, fmt.Errorf("invalid child ID: %q", childID)
	}
	path, err := SubagentPIDPath(sessionID, childID)
	if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to read subagent pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return false, fmt.Errorf("subagent %s pid file %q holds non-integer content %q: %w", childID, path, raw, err)
	}
	if runtime.GOOS == "windows" {
		// kill(0) is unavailable: the file's presence IS the liveness signal
		// (see the file-level note).
		return true, nil
	}
	if pid == os.Getpid() {
		return true, nil // this process owns the child
	}
	switch syscall.Kill(pid, 0) {
	case nil:
		return true, nil
	case syscall.ESRCH:
		return false, nil
	case syscall.EPERM:
		common.LogErrorf("subagent-pid", "subagent %s pid %d is alive under a different owner; treating its slot as not ours to steal", childID, pid)
		return false, nil
	default:
		return false, nil // unknown errno: cannot confirm liveness
	}
}
