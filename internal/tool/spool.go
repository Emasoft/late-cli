package tool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"late/internal/common"
)

const (
	// spoolDirMode keeps the spool directory as private as the session
	// folder it lives in (session output_archive.go uses 0700 too).
	spoolDirMode os.FileMode = 0o700

	// spoolFileMode keeps spool transcripts as private as archived tool
	// outputs and history files (0600).
	spoolFileMode os.FileMode = 0o600

	// spoolPrefix + <callID> + spoolExt names an in-flight transcript:
	// <dir>/partial-<callID>.txt. The cmd/late resume synthesis globs this
	// pattern to find transcripts orphaned by a previous late exit.
	spoolPrefix = "partial-"
	spoolExt    = ".txt"

	// spoolArchiveExt is the content-addressed name a completed transcript
	// promotes to — the OutputArchive naming (<sha256[:16]>.txt) — so a
	// spool dir that IS the session's tool-outputs dir collects both kinds
	// of artifact under one convention.
	spoolArchiveExt = ".txt"
)

// Spool hands out per-tool-call transcript files under one directory. A
// transcript is written WHILE the tool runs (tee), so a kill, timeout, or
// process exit still leaves every byte the tool produced on disk — the
// foundation of "perfect resume": a resumed session can read what an
// interrupted call actually did before retrying it.
type Spool struct {
	dir string
}

// NewSpool returns a spool rooted at dir. The directory is created lazily,
// on the first New, so construction cannot fail.
func NewSpool(dir string) *Spool {
	return &Spool{dir: dir}
}

// New starts a fresh transcript file for one tool call:
// <dir>/partial-<callID>.txt (0600). Any existing file with the same name is
// truncated — call IDs are unique per process (see newShellSpoolCallID).
func (s *Spool) New(callID string) (*SpoolWriter, error) {
	if err := os.MkdirAll(s.dir, spoolDirMode); err != nil {
		return nil, fmt.Errorf("failed to create tool-output spool directory %s: %w", s.dir, err)
	}
	path := filepath.Join(s.dir, spoolPrefix+callID+spoolExt)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, spoolFileMode)
	if err != nil {
		return nil, fmt.Errorf("failed to open spool transcript %s: %w", path, err)
	}
	return &SpoolWriter{path: path, file: f, hash: sha256.New()}, nil
}

// SpoolWriter is the tee sink for one tool call's captured output. Write is
// pass-through: it always reports the full chunk written so the caller's
// capture path can never fail because of spooling — spool errors are
// swallowed by design, because losing transcript bytes must never fail a
// tool call.
type SpoolWriter struct {
	mu   sync.Mutex
	path string
	file *os.File
	hash hash.Hash // sha256 over the bytes that actually reached the file
	n    int64     // bytes that actually reached the file
}

// Write appends p to the transcript file and reports len(p), nil
// unconditionally (pass-through). The running hash and byte count only
// advance for bytes that actually reached the file.
func (w *SpoolWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		if _, err := w.file.Write(p); err == nil {
			_, _ = w.hash.Write(p) // hash.Hash writes never error
			w.n += int64(len(p))
		}
		// A failed file write is swallowed: the tool call keeps its bytes,
		// only the transcript falls behind.
	}
	return len(p), nil
}

// Path returns the transcript file's path.
func (w *SpoolWriter) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

// Empty reports whether nothing reached the file — a command that produced
// no output has no transcript worth referencing.
func (w *SpoolWriter) Empty() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n == 0
}

// Close releases the file handle. Idempotent; safe to call on every path.
func (w *SpoolWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// Discard closes the transcript and removes the partial file. Used when a
// call produced no output (nothing to preserve).
func (w *SpoolWriter) Discard() {
	w.Close()
	w.mu.Lock()
	path := w.path
	w.mu.Unlock()
	_ = os.Remove(path)
}

// Promote renames the partial transcript to its content-addressed archive
// name <sha256[:16]>.txt — the OutputArchive naming — and returns the new
// path. An empty transcript is removed instead and "" returned. Promotion
// is meant for successful calls whose spool dir is the session's
// tool-outputs dir (the production wiring), where the renamed file becomes
// a first-class archive artifact.
func (w *SpoolWriter) Promote() (string, error) {
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("failed to close spool transcript %s: %w", w.Path(), err)
	}
	if w.Empty() {
		w.Discard()
		return "", nil
	}
	w.mu.Lock()
	name := hex.EncodeToString(w.hash.Sum(nil))[:16] + spoolArchiveExt
	dest := filepath.Join(filepath.Dir(w.path), name)
	path := w.path
	w.mu.Unlock()
	if err := os.Rename(path, dest); err != nil {
		return "", fmt.Errorf("failed to promote spool transcript %s: %w", path, err)
	}
	return dest, nil
}

// shellSpoolDir is the process-wide directory shell-call transcripts spool
// into; empty (the zero value) disables spooling. It mirrors
// defaultShellTimeout's wiring model: set once at startup by cmd/late —
// with the active session's OutputArchive dir, so the root agent and every
// subagent share it — and read by every ShellTool.Execute.
var shellSpoolDir string

// spoolCallSeq disambiguates call IDs issued within the same nanosecond
// (parallel subagents can start shell calls concurrently).
var spoolCallSeq atomic.Uint64

// SetShellSpoolDir points shell-call spooling at dir; pass "" to disable.
// Not goroutine-safe: call it once at startup (the cmd/late session wiring)
// or from tests before concurrent shell execution begins. Production wiring
// passes the session's OutputArchive dir, so a successful call's transcript
// promotes to a content-addressed archive name in the same directory; the
// spool and the executor's output archive then share one convention.
func SetShellSpoolDir(dir string) { shellSpoolDir = dir }

// newShellSpoolCallID mints a unique-per-process call ID: the call's start
// time (nanoseconds) plus a monotonic sequence number.
func newShellSpoolCallID() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), spoolCallSeq.Add(1))
}

// startShellSpool opens a fresh transcript for one shell call, or returns
// nil when spooling is disabled or the file could not be opened — spooling
// is fail-open end to end: losing the transcript must never fail the call.
func startShellSpool(dir string) *SpoolWriter {
	if dir == "" {
		return nil
	}
	w, err := NewSpool(dir).New(newShellSpoolCallID())
	if err != nil {
		common.LogErrorf("tool-spool", "shell transcript spooling disabled for this call: %v", err)
		return nil
	}
	return w
}

// runShellCapturing runs cmd capturing combined stdout/stderr into a
// buffer, teeing every captured chunk to the spool writer when one is
// attached — the tee sits on the pipe-reading seam, so bytes reach disk as
// the command produces them and survive a group kill. cmd.Stdout and
// cmd.Stderr hold the SAME writer value, so os/exec serializes writes into
// it (identical semantics to CombinedOutput, which shares one buffer).
func runShellCapturing(cmd *exec.Cmd, spool *SpoolWriter) ([]byte, error) {
	var buf bytes.Buffer
	var sink io.Writer = &buf
	if spool != nil {
		sink = io.MultiWriter(&buf, spool)
	}
	cmd.Stdout = sink
	cmd.Stderr = sink
	err := cmd.Run()
	return buf.Bytes(), err
}

// spoolFailureWording is the retry-pointer sentence appended to a failed
// shell result (live failure) and reproduced by cmd/late's resume synthesis
// (interrupted call). Kept in one place so the two surfaces stay identical.
const spoolFailureWording = "The transcript of its execution was preserved here — read it to decide whether to retry the tool call:"

// spoolFailureNote finalizes the spool for a FAILED call (error, cancel, or
// timeout): the transcript stays on disk as partial-<id>.txt and the retry
// pointer is appended to the result being built. Empty or absent
// transcripts yield "" — there is nothing to reference, and the result is
// left untouched. The appended wording is self-contained on purpose: read
// alone (in a resumed session, next to a dangling call) it still says what
// happened and where the bytes are.
func spoolFailureNote(spool *SpoolWriter, errDesc string) string {
	if spool == nil {
		return ""
	}
	spool.Close()
	if spool.Empty() {
		spool.Discard()
		return ""
	}
	return "\n\n" + fmt.Sprintf("tool call failed: %s. %s %s", errDesc, spoolFailureWording, spool.Path())
}

// finishSpoolSuccess finalizes the spool for a successful call: a silent
// command's empty transcript is discarded, a non-empty one promotes to its
// content-addressed archive name. Promotion failure is logged and the
// partial file is left in place (still readable) rather than deleted.
func finishSpoolSuccess(spool *SpoolWriter) {
	if spool == nil {
		return
	}
	if spool.Empty() {
		spool.Discard()
		return
	}
	if _, err := spool.Promote(); err != nil {
		common.LogErrorf("tool-spool", "failed to promote shell transcript %s: %v", spool.Path(), err)
	}
}
