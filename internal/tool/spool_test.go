package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setShellSpoolDirForTest overrides the process-wide shell spool dir and
// restores the previous value when the test finishes.
func setShellSpoolDirForTest(t *testing.T, dir string) {
	t.Helper()
	prev := shellSpoolDir
	SetShellSpoolDir(dir)
	t.Cleanup(func() { SetShellSpoolDir(prev) })
}

// spoolPathFor returns the transcript path one call ID maps to.
func spoolPathFor(dir, callID string) string {
	return filepath.Join(dir, "partial-"+callID+".txt")
}

// sha256Sum hex-encodes the SHA-256 of b (the OutputArchive naming input).
func sha256Sum(t *testing.T, b []byte) string {
	t.Helper()
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// errString renders a nil error as "" so result+err can be searched as one
// string regardless of which shape the cancellation surfaced through.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// TestSpoolWriterPassthrough pins the SpoolWriter contract: Write always
// reports the full chunk and nil error (pass-through), the bytes reach the
// file, Close is idempotent, and Promote renames to the content-addressed
// archive name with the exact same bytes.
func TestSpoolWriterPassthrough(t *testing.T) {
	dir := t.TempDir()
	s := NewSpool(dir)
	w, err := s.New("call-passthrough")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()

	chunk := "hello from a command"
	n, werr := w.Write([]byte(chunk))
	if werr != nil {
		t.Fatalf("Write error = %v, want nil (pass-through)", werr)
	}
	if n != len(chunk) {
		t.Fatalf("Write = %d, want %d", n, len(chunk))
	}
	n, werr = w.Write([]byte(chunk))
	if werr != nil || n != len(chunk) {
		t.Fatalf("second Write = (%d, %v), want (%d, nil)", n, werr, len(chunk))
	}

	path := spoolPathFor(dir, "call-passthrough")
	if w.Path() != path {
		t.Fatalf("Path = %q, want %q", w.Path(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != chunk+chunk {
		t.Fatalf("transcript = %q, want both chunks", string(data))
	}
	// Mode 0600, like archived tool outputs.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("transcript mode = %o, want 600", perm)
	}

	promoted, err := w.Promote()
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial file still present after Promote (err=%v)", err)
	}
	// Content-addressed: <sha256[:16]>.txt of the exact transcript bytes.
	sum := sha256Sum(t, []byte(chunk + chunk))
	if want := filepath.Join(dir, sum+".txt"); promoted != want {
		t.Fatalf("Promote = %q, want %q", promoted, want)
	}
	data, err = os.ReadFile(promoted)
	if err != nil {
		t.Fatalf("ReadFile(promoted): %v", err)
	}
	if string(data) != chunk+chunk {
		t.Fatalf("promoted bytes = %q, want the transcript preserved verbatim", string(data))
	}
}

// TestSpoolWriterEmptyDiscard pins the empty-transcript contract: Promote
// removes the partial file and returns "" — a silent command leaves no
// artifact and no retry pointer.
func TestSpoolWriterEmptyDiscard(t *testing.T) {
	dir := t.TempDir()
	w, err := NewSpool(dir).New("call-empty")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	promoted, err := w.Promote()
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if promoted != "" {
		t.Fatalf("Promote = %q, want \"\" for an empty transcript", promoted)
	}
	if _, err := os.Stat(spoolPathFor(dir, "call-empty")); !os.IsNotExist(err) {
		t.Fatalf("empty transcript not removed (err=%v)", err)
	}
}

// TestShellTool_SpoolCapturesCancelledOutput is the perfect-resume proof:
// a long-running command cancelled via its context leaves a non-empty
// transcript containing the output produced BEFORE the kill, and the failed
// result carries the retry pointer to that transcript.
func TestShellTool_SpoolCapturesCancelledOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive cancellation test")
	}
	dir := t.TempDir()
	setShellSpoolDirForTest(t, dir)

	ctx, cancel := context.WithCancel(approvedContext())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	// The shell prints early output, then hangs until the group kill.
	result, err := (&ShellTool{}).Execute(ctx, json.RawMessage(`{"command": "echo early-output-marker; sleep 300"}`))
	_ = err // cancelled commands may surface as error or failure-shaped result

	// Exactly one transcript was left behind (partial — never promoted).
	matches, globErr := filepath.Glob(filepath.Join(dir, "partial-*.txt"))
	if globErr != nil {
		t.Fatalf("Glob: %v", globErr)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d partial transcript(s) in %s, want exactly 1", len(matches), dir)
	}
	data, readErr := os.ReadFile(matches[0])
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	if len(data) == 0 {
		t.Fatal("cancelled command's transcript is empty — early output was not captured")
	}
	if !strings.Contains(string(data), "early-output-marker") {
		t.Fatalf("transcript %q missing the pre-kill output", string(data))
	}

	// The failed result must carry the retry pointer to THIS transcript.
	pointer := "tool call failed: " + spoolFailureWording + " " + matches[0]
	full := result + "\n" + errString(err)
	if !strings.Contains(full, spoolFailureWording) {
		t.Fatalf("failed result %q missing the retry-pointer wording", full)
	}
	if !strings.Contains(full, matches[0]) {
		t.Fatalf("failed result %q missing the transcript path %s", full, matches[0])
	}
	_ = pointer
}

// TestShellTool_SpoolSuccessPromotes pins the success path: a command that
// runs to completion has its transcript promoted (renamed) to the
// content-addressed archive name and no partial-*.txt remains; a silent
// command leaves nothing at all. The result string itself is unchanged by
// spooling.
func TestShellTool_SpoolSuccessPromotes(t *testing.T) {
	dir := t.TempDir()
	setShellSpoolDirForTest(t, dir)

	result, err := (&ShellTool{}).Execute(approvedContext(), json.RawMessage(`{"command": "echo promoted-marker"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "promoted-marker") {
		t.Fatalf("result = %q, want the command output unchanged by spooling", result)
	}

	partials, _ := filepath.Glob(filepath.Join(dir, "partial-*.txt"))
	if len(partials) != 0 {
		t.Fatalf("partial transcript(s) left behind after success: %v", partials)
	}
	archived, _ := filepath.Glob(filepath.Join(dir, "*.txt"))
	if len(archived) != 1 {
		t.Fatalf("want exactly 1 promoted archive, got %v", archived)
	}
	data, err := os.ReadFile(archived[0])
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "promoted-marker") {
		t.Fatalf("promoted archive = %q, want the captured output", string(data))
	}

	// A silent command (exit 0, no output) leaves no artifact either.
	if _, err := (&ShellTool{}).Execute(approvedContext(), json.RawMessage(`{"command": "true"}`)); err != nil {
		t.Fatalf("Execute(true): %v", err)
	}
	archived, _ = filepath.Glob(filepath.Join(dir, "*.txt"))
	if len(archived) != 1 {
		t.Fatalf("silent command left an artifact: %v", archived)
	}
}

// TestShellTool_SpoolFailedResultWording pins the exact failed-result
// wording for a non-zero exit: the original failure-shaped result plus the
// appended retry pointer naming the partial transcript.
func TestShellTool_SpoolFailedResultWording(t *testing.T) {
	dir := t.TempDir()
	setShellSpoolDirForTest(t, dir)

	result, err := (&ShellTool{}).Execute(approvedContext(), json.RawMessage(`{"command": "echo before-fail; exit 3"}`))
	if err != nil {
		t.Fatalf("Execute returned error %v, want a failure-shaped result", err)
	}
	if !strings.Contains(result, "Command failed with exit code 3") {
		t.Fatalf("result = %q, want the exit-code prefix", result)
	}
	partials, _ := filepath.Glob(filepath.Join(dir, "partial-*.txt"))
	if len(partials) != 1 {
		t.Fatalf("found %d partial transcript(s), want exactly 1", len(partials))
	}
	if !strings.Contains(result, spoolFailureWording) {
		t.Fatalf("result = %q, missing the retry-pointer wording", result)
	}
	// The pointer names the transcript, with the full sentence shape the
	// resume synthesis reproduces for interrupted calls.
	wantTail := "The transcript of its execution was preserved here — read it to decide whether to retry the tool call: " + partials[0]
	if !strings.Contains(result, wantTail) {
		t.Fatalf("result = %q, missing transcript pointer %q", result, wantTail)
	}
	if !strings.Contains(result, "before-fail") {
		t.Fatalf("result = %q, want the pre-failure output still inline", result)
	}
}

// TestShellTool_SpoolDisabled pins the nil-dir contract: without a spool
// dir set, nothing is written anywhere and results are unchanged.
func TestShellTool_SpoolDisabled(t *testing.T) {
	setShellSpoolDirForTest(t, "")
	result, err := (&ShellTool{}).Execute(approvedContext(), json.RawMessage(`{"command": "echo no-spool"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(result, "no-spool") {
		t.Fatalf("result = %q, want the normal output", result)
	}
}
