//go:build !windows

package main

import (
	"os"
	"strconv"
	"testing"
	"time"

	"late/internal/session"
	"late/internal/tool"
)

// stubSessionDirForSubagentPid sandboxes the real sessions directory for
// the PID-file lifecycle tests (the scheduler persists under the global
// SessionDir, so the override must surround every write or removal). The
// override shape follows compaction_runner_test.go's established stub.
func stubSessionDirForSubagentPid(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	original := session.SetSessionDirOverrideForTest(func() (string, error) { return dir, nil })
	t.Cleanup(func() { session.SetSessionDirOverrideForTest(original) })
	return dir
}

// pidPathFor resolves the PID file via the session-level path helper.
func pidPathFor(t *testing.T, sessionID, childID string) string {
	t.Helper()
	path, err := session.SubagentPIDPath(sessionID, childID)
	if err != nil {
		t.Fatalf("SubagentPIDPath: %v", err)
	}
	return path
}

// mustPIDFileExistsAfter asserts the pid file exists and holds this test
// process's pid (background children are goroutines of this process).
func mustPIDFileExistsAfter(t *testing.T, path string) os.FileInfo {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pid file %s not present at launch: %v", path, err)
	}
	pid, err := strconv.Atoi(string(raw[:len(raw)-1])) // trailing newline
	if err != nil || pid != os.Getpid() {
		t.Fatalf("pid file %s content %q, want own pid %d", path, raw, os.Getpid())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat pid file: %v", err)
	}
	return info
}

// waitPIDFileGone polls the pid file away (the removal happens on the
// scheduler's own goroutine, so it must be observed, not assumed).
func waitPIDFileGone(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid file %s still present after removal", path)
}

// waitSchedulerIdle polls the scheduler's live set empty (a frozen child
// stays registered until its run closure returns through finished(), so a
// follow-up Launch must wait for that unwind, not assume it).
func waitSchedulerIdle(t *testing.T, s *SubagentScheduler) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		idle := len(s.running) == 0 && s.serialRunning == "" && s.parallelRunning == 0
		s.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("scheduler still live after frozen child unwound")
}

// TestSchedulerPIDFileLifecycle pins the PID-file contract of Step 6:
// written at launch (the scheduler runs background children as goroutines
// of THIS process, so the recorded owner pid is os.Getpid()), removed on
// terminal completion, and removed at freeze — frozen is not running.
func TestSchedulerPIDFileLifecycle(t *testing.T) {
	stubSessionDirForSubagentPid(t)
	const sessionID = "session-pid-lifecycle"

	s := newSubagentScheduler(nil, nil)
	s.sessionID = sessionID

	// Launch freezes the child mid-run on a gate so lifecycle states are
	// deterministic.
	release := make(chan struct{})
	if launched, pos := s.Launch("coder-subagent-1", "coder", tool.SubagentExecutionSerial, func() subagentCompletion {
		<-release
		return completionFor("coder-subagent-1", "coder", session.SubagentStatusCompleted)
	}); !launched || pos != 0 {
		t.Fatalf("launch: launched=%v pos=%d, want true/0", launched, pos)
	}

	path := pidPathFor(t, sessionID, "coder-subagent-1")
	// The pid file exists the moment the scheduler actually starts the
	// child (subagentPIDForLaunch runs inside startLocked under the same
	// lock as the goroutine spawn, so completion cannot race past it).
	mustPIDFileExistsAfter(t, path)

	// Freeze removes it even though the run closure still has cleanup to
	// do — frozen is not running.
	if !s.RequestFreeze("coder-subagent-1") {
		t.Fatal("RequestFreeze: child not found among running entries")
	}
	waitPIDFileGone(t, path)
	close(release)
	waitSchedulerIdle(t, s)

	// Terminal completion of a second child also removes its pid file.
	// The second closure is ALSO gated mid-run on a channel: an instant
	// closure can finish and remove its pid file before this test even
	// stats it, so the existence check must observe the child while it is
	// provably still running.
	release2 := make(chan struct{})
	done := make(chan struct{})
	if launched, _ := s.Launch("coder-subagent-2", "coder", tool.SubagentExecutionParallel, func() subagentCompletion {
		<-release2
		defer close(done)
		return completionFor("coder-subagent-2", "coder", session.SubagentStatusCompleted)
	}); !launched {
		t.Fatal("second child did not launch")
	}
	path2 := pidPathFor(t, sessionID, "coder-subagent-2")
	mustPIDFileExistsAfter(t, path2)
	close(release2)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second run did not finish")
	}
	waitPIDFileGone(t, path2)
}
