//go:build !windows

package tool

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// setShellTimeoutForTest overrides the global default shell timeout and
// restores the previous value when the test finishes.
func setShellTimeoutForTest(t *testing.T, d time.Duration) {
	t.Helper()
	prev := defaultShellTimeout
	SetShellTimeout(d)
	t.Cleanup(func() { SetShellTimeout(prev) })
}

// TestShellTool_TimeoutKillsHangingCommand verifies that a command which never
// exits is killed by the shell timeout and that Execute surfaces a timeout
// error containing the partial output produced before the kill.
func TestShellTool_TimeoutKillsHangingCommand(t *testing.T) {
	// 2s (not a tight bound like 300ms): under back-to-back -race load, bash
	// startup + echo can exceed a few-hundred-ms timeout before "start" is
	// written, making the partial-output assertion below flaky. 2s keeps the
	// kill proof and the strong assertion deterministic while staying fast.
	setShellTimeoutForTest(t, 2*time.Second)

	start := time.Now()
	out, err := (&ShellTool{}).Execute(approvedContext(), json.RawMessage(`{"command": "echo start; sleep 300"}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got nil (out=%q)", out)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected error to mention the timeout, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "start") {
		t.Fatalf("expected partial output 'start' in error, got %q", err.Error())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Execute took %v, expected < 5s", elapsed)
	}
}

// TestShellTool_CancelReturnsDespiteGrandchildHoldingPipes is THE regression
// test for the subagent-hang bug: `(sleep 300 &)` leaves a grandchild that
// inherits bash's stdout/stderr pipes. Plain CommandContext cancellation kills
// only the direct bash process, so CombinedOutput's Wait used to block forever
// even after the context was cancelled. With the process-group kill in
// newShellCommand the whole group dies, the pipes close, and Execute returns.
func TestShellTool_CancelReturnsDespiteGrandchildHoldingPipes(t *testing.T) {
	setShellTimeoutForTest(t, 0) // disable the timeout; this test exercises explicit cancel

	ctx, cancel := context.WithCancel(approvedContext())
	defer cancel()

	type execResult struct {
		out string
		err error
	}
	results := make(chan execResult, 1)
	go func() {
		out, err := (&ShellTool{}).Execute(ctx, json.RawMessage(`{"command": "echo start; (sleep 300 &) ; sleep 300"}`))
		results <- execResult{out, err}
	}()

	// Let bash start and spawn the detached sleeper before cancelling.
	select {
	case res := <-results:
		t.Fatalf("Execute returned before cancel: out=%q err=%v", res.out, res.err)
	case <-time.After(200 * time.Millisecond):
	}

	cancelledAt := time.Now()
	cancel()

	select {
	case res := <-results:
		if elapsed := time.Since(cancelledAt); elapsed > 9*time.Second {
			t.Errorf("Execute returned %v after cancel, expected < 9s (out=%q err=%v)", elapsed, res.out, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Execute did not return within 10s after cancel — pipe-holding grandchild is still blocking Wait")
	}
}

// TestShellTool_TimeoutLeavesNoProcesses verifies the process-group kill end
// to end: after a timed-out shell command returns, no descendant (bash or its
// sleep child) may still be running.
func TestShellTool_TimeoutLeavesNoProcesses(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}

	setShellTimeoutForTest(t, 300*time.Millisecond)

	_, _ = (&ShellTool{}).Execute(approvedContext(), json.RawMessage(`{"command": "sleep 300"}`))

	deadline := time.Now().Add(2 * time.Second)
	for {
		// Anchored pattern so unrelated processes merely containing "sleep 300"
		// in a longer command line do not produce false positives. pgrep exits
		// non-zero with empty output when nothing matches.
		out, _ := exec.Command("pgrep", "-f", `^sleep 300$`).Output()
		matches := strings.TrimSpace(string(out))
		if matches == "" {
			return // no leftover processes
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still running after shell timeout: %s", matches)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestShellTool_PerCallTimeoutOverridesGlobal verifies the per-call timeout
// parameter beats the (longer) global default.
func TestShellTool_PerCallTimeoutOverridesGlobal(t *testing.T) {
	setShellTimeoutForTest(t, 10*time.Minute)

	tool := ShellTool{}
	args := json.RawMessage(`{"command": "echo start; sleep 300", "timeout": "1s"}`)

	start := time.Now()
	_, err := tool.Execute(approvedContext(), args)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected error to mention the timeout, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "start") {
		t.Fatalf("expected partial output 'start' in the timeout error, got %q", err.Error())
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("command should have been killed after ~1s, took %s", elapsed)
	}
}

// TestShellTool_PerCallTimeoutUnlimited verifies that "0" parses as unlimited
// without hanging a fast command.
func TestShellTool_PerCallTimeoutUnlimited(t *testing.T) {
	setShellTimeoutForTest(t, 10*time.Minute)

	tool := ShellTool{}
	args := json.RawMessage(`{"command": "echo ok", "timeout": "0"}`)

	result, err := tool.Execute(approvedContext(), args)
	if err != nil {
		t.Fatalf("expected success with unlimited timeout, got %v", err)
	}
	if !strings.Contains(result, "ok") {
		t.Fatalf("expected command output 'ok', got %q", result)
	}
}

// TestShellTool_InvalidTimeoutIsErrorResult verifies that an unparsable timeout
// is reported as an error RESULT (nil Go error) and that the command never runs.
func TestShellTool_InvalidTimeoutIsErrorResult(t *testing.T) {
	setShellTimeoutForTest(t, 10*time.Minute)

	tool := ShellTool{}
	args := json.RawMessage(`{"command": "echo should-not-run", "timeout": "banana"}`)

	result, err := tool.Execute(approvedContext(), args)
	if err != nil {
		t.Fatalf("expected an error result with nil Go error, got %v", err)
	}
	if !strings.Contains(result, "invalid timeout") {
		t.Fatalf("expected result to report the invalid timeout, got %q", result)
	}
	if strings.Contains(result, "should-not-run") {
		t.Fatalf("command must not run when the timeout is invalid, got %q", result)
	}
}

// TestResolveShellTimeout covers the pure timeout resolution function directly.
func TestResolveShellTimeout(t *testing.T) {
	tests := []struct {
		name           string
		raw            string
		wantDuration   time.Duration
		wantDeadline   bool
		wantCancelFunc bool
		wantErr        bool
	}{
		{
			name:           "empty uses global default",
			raw:            "",
			wantDuration:   10 * time.Minute,
			wantDeadline:   true,
			wantCancelFunc: true,
		},
		{
			name:         "zero is unlimited",
			raw:          "0",
			wantDuration: 0,
			wantDeadline: false,
		},
		{
			name:         "negative is unlimited",
			raw:          "-5m",
			wantDuration: 0,
			wantDeadline: false,
		},
		{
			name:           "valid duration is honored",
			raw:            "90m",
			wantDuration:   90 * time.Minute,
			wantDeadline:   true,
			wantCancelFunc: true,
		},
		{
			name:    "invalid string errors",
			raw:     "banana",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setShellTimeoutForTest(t, 10*time.Minute)

			ctx, cancel, gotDuration, err := resolveShellTimeout(context.Background(), tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error for raw=%q, got nil", tt.raw)
				}
				if !strings.Contains(err.Error(), "invalid timeout") {
					t.Fatalf("expected error to mention 'invalid timeout', got %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for raw=%q: %v", tt.raw, err)
			}
			if gotDuration != tt.wantDuration {
				t.Fatalf("resolved duration = %s, want %s", gotDuration, tt.wantDuration)
			}
			_, hasDeadline := ctx.Deadline()
			if hasDeadline != tt.wantDeadline {
				t.Fatalf("ctx hasDeadline = %v, want %v", hasDeadline, tt.wantDeadline)
			}
			if (cancel != nil) != tt.wantCancelFunc {
				t.Fatalf("cancel func presence = %v, want %v", cancel != nil, tt.wantCancelFunc)
			}
			if cancel != nil {
				cancel()
			}
		})
	}
}
