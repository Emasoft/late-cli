package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Subagent status values stored in the manifest. "running" is the spawn-time
// placeholder: nothing runs at crash time, so a record that is still running
// when the manifest is next read means the process exited (crash, kill,
// terminal close) before the runner could write a terminal state. Resume
// interprets running-at-exit as "interrupted" (see cmd/late).
const (
	SubagentStatusRunning   = "running"
	SubagentStatusCompleted = "completed"
	SubagentStatusFailed    = "failed"
	SubagentStatusCancelled = "cancelled"
)

// manifestFileName is the manifest file inside <sessionsDir>/<sessionID>/subagents.
const manifestFileName = "manifest.json"

// subagentsDirName is the per-session folder child histories live in — the
// layout marker that distinguishes a subagent session's history path from a
// root session's flat history file.
const subagentsDirName = "subagents"

const (
	manifestFileMode os.FileMode = 0o600
	manifestDirMode  os.FileMode = 0o700
)

// SubagentRecord is one subagent spawn entry in the session manifest. It is
// the resume-critical registry: the child's own history file is written by
// the child session (which never writes metadata), so the manifest — written
// through the PARENT session — is the only sidecar that names every child,
// its status, and where its work was persisted.
type SubagentRecord struct {
	ID        string   `json:"id"`
	AgentType string   `json:"agent_type"`
	Goal      string   `json:"goal"`
	CtxFiles  []string `json:"ctx_files,omitempty"`
	// Status is one of the SubagentStatus* constants: running,
	// completed, failed, or cancelled. "running" doubles as "interrupted by
	// exit" at resume time (see the constants above).
	Status string `json:"status"`
	// SpawnedAt is when the spawn call started. EndedAt is zero while the
	// record is running and stamped by MarkStatus on every terminal write.
	SpawnedAt time.Time `json:"spawned_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	// HistoryPath is the child's own persisted history file
	// (<sessions>/<session>/subagents/<id>.json), empty when subagent
	// history persistence was disabled for the run. Resume surfaces it as
	// the preserved-work pointer for interrupted children.
	HistoryPath string `json:"history_path,omitempty"`
	// TranscriptPath is the pruned Markdown transcript written by the
	// runner for abnormal terminations (idle kill, budget exhaustion,
	// user cancel, error). Completed children leave it empty — their full
	// result lives in the parent history already.
	TranscriptPath string `json:"transcript_path,omitempty"`
	// ResultPreview is the first slice (200 chars) of the completed child's
	// final result. Empty for every other status.
	ResultPreview string `json:"result_preview,omitempty"`
	// Cause is the runner's termination classification ("idle: killed by
	// the harness idle watchdog (...)", "time budget exhausted (2h)",
	// "cancelled or killed by the user", "crashed: ..."). Empty while
	// running and for completed children.
	Cause string `json:"cause,omitempty"`
}

// SubagentManifest is the JSON document persisted at
// <sessionsDir>/<sessionID>/subagents/manifest.json. SessionID is
// informational (it names the folder the file was found in, so a
// hand-moved file cannot silently cross sessions).
type SubagentManifest struct {
	SessionID string           `json:"session_id"`
	Records   []SubagentRecord `json:"records"`
}

// LoadSubagentManifest loads the manifest for sessionID. A missing file (the
// common case: no subagent ever spawned in this session) yields an empty
// manifest and a nil error, so callers never special-case first resume.
func LoadSubagentManifest(sessionID string) (*SubagentManifest, error) {
	path, err := SubagentManifestPath(sessionID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &SubagentManifest{SessionID: sessionID, Records: []SubagentRecord{}}, nil
		}
		return nil, fmt.Errorf("failed to read subagent manifest: %w", err)
	}
	var manifest SubagentManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to unmarshal subagent manifest: %w", err)
	}
	if manifest.Records == nil {
		manifest.Records = []SubagentRecord{}
	}
	return &manifest, nil
}

// Save persists the manifest atomically (temp file + rename in the target
// directory), creating the subagents directory on first use. It is a no-op
// for an in-memory session (empty session ID): a manifest must never be
// written outside a proper session folder.
func (m *SubagentManifest) Save() error {
	if m.SessionID == "" {
		return nil
	}
	path, err := SubagentManifestPath(m.SessionID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal subagent manifest: %w", err)
	}
	if err := writeAtomic(path, data, manifestFileMode, manifestDirMode); err != nil {
		return fmt.Errorf("failed to save subagent manifest: %w", err)
	}
	return nil
}

// Upsert inserts the record or replaces the existing record with the same
// ID (last write wins — the terminal write reuses the spawn-time record's
// ID). It operates on the in-memory copy; call Save to persist.
func (m *SubagentManifest) Upsert(rec SubagentRecord) {
	for i := range m.Records {
		if m.Records[i].ID == rec.ID {
			m.Records[i] = rec
			return
		}
	}
	m.Records = append(m.Records, rec)
}

// Get returns the record with the given ID and whether it was found.
func (m *SubagentManifest) Get(id string) (*SubagentRecord, bool) {
	for i := range m.Records {
		if m.Records[i].ID == id {
			return &m.Records[i], true
		}
	}
	return nil, false
}

// MarkStatus flips one record to a terminal status, stamping EndedAt and
// recording cause/resultPreview/transcriptPath (each replaces the previous
// value; empty strings clear). Unknown IDs are a no-op: a terminal write
// may only refine an existing record. It operates on the in-memory copy;
// call Save to persist.
func (m *SubagentManifest) MarkStatus(id, status, cause, resultPreview, transcriptPath string) {
	m.markStatusAt(id, status, cause, resultPreview, transcriptPath, time.Now())
}

// markStatusAt is MarkStatus with an injectable timestamp for tests.
func (m *SubagentManifest) markStatusAt(id, status, cause, resultPreview, transcriptPath string, endedAt time.Time) {
	rec, ok := m.Get(id)
	if !ok {
		return
	}
	rec.Status = status
	rec.Cause = cause
	rec.ResultPreview = resultPreview
	rec.TranscriptPath = transcriptPath
	rec.EndedAt = endedAt
}

// manifestMu serializes the load-modify-save cycles of SaveSubagentRecord.
// Concurrent subagent runners may both spawn and terminate children; the
// manifest is shared per-session state, so without the lock a reader-modify
// race could drop the other writer's record. The session folder is per
// process (one late session per folder), so a single process-wide mutex is
// sufficient and cheaper than per-manifest bookkeeping.
var manifestMu sync.Mutex

// SaveSubagentRecord durably merges one subagent record into the manifest of
// the session's folder. It is THE manifest write path from both spawn (the
// child orchestrator, whose own session skips metadata) and termination (the
// subagent runner), and deliberately bypasses the subagent-session
// skipMetadata rule: the manifest is keyed by the PARENT session's folder,
// which is the session object this method runs on.
//
// The load-modify-save cycle is mutex-guarded so concurrent runners cannot
// drop each other's records, and it re-loads from disk each time so a
// long-lived session's manifest never carries a stale in-memory copy.
func (s *Session) SaveSubagentRecord(rec SubagentRecord) error {
	sessionID := s.sessionSubagentRecordID()
	if sessionID == "" {
		return nil // in-memory session: no session folder, no manifest
	}
	manifestMu.Lock()
	defer manifestMu.Unlock()

	manifest, err := LoadSubagentManifest(sessionID)
	if err != nil {
		return err
	}
	manifest.SessionID = sessionID
	manifest.Upsert(rec)
	return manifest.Save()
}

// sessionSubagentRecordID returns the session folder the manifest belongs
// to. Two layouts reach this method:
//
//   - a root session, whose history is the flat <sessionsDir>/<id>.json —
//     the record key is that file's base name;
//   - a subagent session, whose history is
//     <sessionsDir>/<id>/subagents/<childID>.json (the child session itself
//     calls this for nested-spawn bookkeeping) — the record key is the
//     session folder ABOVE the subagents directory, so a child's manifest
//     record lands in the same manifest as its siblings.
//
// It is empty for in-memory sessions (no folder, no manifest) and for
// unsafe IDs (a manifest must never be written outside a valid session
// folder).
func (s *Session) sessionSubagentRecordID() string {
	if s.HistoryPath == "" {
		return ""
	}
	dir := filepath.Dir(s.HistoryPath)
	var id string
	if filepath.Base(dir) == subagentsDirName {
		id = filepath.Base(filepath.Dir(dir))
	} else {
		id = strings.TrimSuffix(filepath.Base(s.HistoryPath), ".json")
	}
	if !isValidPathElement(id) {
		return ""
	}
	return id
}

// MarkSubagentStatus flips one record in the session's manifest to a
// terminal status — the runner's write path for completed / failed /
// cancelled outcomes. It records cause, resultPreview, and transcriptPath
// (each replacing the previous value; empty clears) and stamps EndedAt.
// It is a no-op for an in-memory session, when the manifest cannot be
// loaded, or for an unknown ID — a terminal write may only refine an
// existing spawn record, never invent one.
func (s *Session) MarkSubagentStatus(id, status, cause, resultPreview, transcriptPath string) error {
	sessionID := s.sessionSubagentRecordID()
	if sessionID == "" {
		return nil // in-memory session: no session folder, no manifest
	}
	manifestMu.Lock()
	defer manifestMu.Unlock()

	manifest, err := LoadSubagentManifest(sessionID)
	if err != nil {
		return err
	}
	manifest.SessionID = sessionID
	manifest.MarkStatus(id, status, cause, resultPreview, transcriptPath)
	return manifest.Save()
}
