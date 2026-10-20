//go:build !windows

package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSubagentPIDWriteAndPath pins the path shape and the atomic write: a
// PID file is temp+rename-written under the same subagents dir as the
// manifest, and overwrites atomically.
func TestSubagentPIDWriteAndPath(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)
	const sessionID, childID = "session-pid", "coder-subagent-1"

	if err := WriteSubagentPID(sessionID, childID, 4242); err != nil {
		t.Fatalf("WriteSubagentPID: %v", err)
	}
	path, err := SubagentPIDPath(sessionID, childID)
	if err != nil {
		t.Fatalf("SubagentPIDPath: %v", err)
	}
	if want := filepath.Join(tmp, sessionID, subagentsDirName, childID+".pid"); path != want {
		t.Fatalf("path %s, want %s", path, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(raw)) != "4242" {
		t.Fatalf("pid file content %q err %v, want 4242", raw, err)
	}

	// Overwrite wins atomically (no temp leftover junk can win the rename).
	if err := WriteSubagentPID(sessionID, childID, 999); err != nil {
		t.Fatalf("WriteSubagentPID (overwrite): %v", err)
	}
	raw, _ = os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != "999" {
		t.Fatalf("pid after overwrite: %q", raw)
	}

	// Invalid IDs and pids are refused, never half-written.
	if err := WriteSubagentPID("../escape", childID, 1); err == nil {
		t.Fatal("invalid session ID accepted")
	}
	if err := WriteSubagentPID(sessionID, childID, 0); err == nil {
		t.Fatal("pid 0 accepted")
	}
}

// TestSubagentPIDLivenessWithRealProcess classifies a real subprocess: its
// pid reads alive while the process runs and dead once reaped. The
// own-identity case (pid == os.Getpid()) reads alive by construction.
func TestSubagentPIDLivenessWithRealProcess(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)
	const sessionID, childID, deadID = "session-pid-live", "coder-subagent-live", "coder-subagent-dead"

	// Own pid: alive.
	if err := WriteSubagentPID(sessionID, childID, os.Getpid()); err != nil {
		t.Fatalf("WriteSubagentPID: %v", err)
	}
	if alive, err := IsSubagentAlive(sessionID, childID); err != nil || !alive {
		t.Fatalf("own pid classified alive=%v err=%v, want true/nil", alive, err)
	}

	// A genuinely dead pid: kill+wait reaps the process, so kill(pid, 0)
	// then misses it (ESRCH).
	dead := exec.Command("sleep", "30")
	if err := dead.Start(); err != nil {
		t.Skipf("cannot spawn sleep subprocess: %v", err)
	}
	if err := WriteSubagentPID(sessionID, deadID, dead.Process.Pid); err != nil {
		t.Fatalf("WriteSubagentPID: %v", err)
	}
	if alive, err := IsSubagentAlive(sessionID, deadID); err != nil || !alive {
		t.Fatalf("running subprocess classified alive=%v err=%v, want true/nil", alive, err)
	}
	_ = dead.Process.Kill()
	if err := dead.Wait(); err != nil {
		t.Logf("wait after kill (expected signal exit): %v", err)
	}
	// The reaped pid is gone; poll briefly in case of close timing.
	seenDead := false
	for i := 0; i < 50; i++ {
		alive, aliveErr := IsSubagentAlive(sessionID, deadID)
		if aliveErr == nil && !alive {
			seenDead = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !seenDead {
		t.Fatal("reaped subprocess still classified alive")
	}
}

// TestSubagentPIDMissingAndCorrupt covers the file-content contracts:
// missing file ⇒ (false, nil), corrupt content ⇒ (false, error).
func TestSubagentPIDMissingAndCorrupt(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)
	const sessionID = "session-pid-missing"

	if alive, err := IsSubagentAlive(sessionID, "never-written"); err != nil || alive {
		t.Fatalf("missing pid file classified alive=%v err=%v, want false/nil", alive, err)
	}

	dir := filepath.Join(tmp, sessionID, subagentsDirName)
	corrupt := filepath.Join(dir, "coder-subagent-x.pid")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corrupt, []byte("not-a-pid"), 0600); err != nil {
		t.Fatal(err)
	}
	if alive, err := IsSubagentAlive(sessionID, "coder-subagent-x"); alive || err == nil {
		t.Fatalf("corrupt pid file classified alive=%v err=%v, want false/error", alive, err)
	}
}

// TestSubagentPIDRemoveToleratesMissing pins the idempotent removal: a
// removed-again removal is success, not ErrNotExist.
func TestSubagentPIDRemoveToleratesMissing(t *testing.T) {
	tmp := t.TempDir()
	stubSessionDir(t, tmp)
	const sessionID, childID = "session-pid-remove", "coder-subagent-1"

	if err := WriteSubagentPID(sessionID, childID, os.Getpid()); err != nil {
		t.Fatalf("WriteSubagentPID: %v", err)
	}
	if err := RemoveSubagentPID(sessionID, childID); err != nil {
		t.Fatalf("RemoveSubagentPID: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, sessionID, subagentsDirName, childID+".pid")); !os.IsNotExist(err) {
		t.Fatalf("pid file still present: %v", err)
	}
	if err := RemoveSubagentPID(sessionID, childID); err != nil {
		t.Fatalf("RemoveSubagentPID (already gone): %v", err)
	}
}
