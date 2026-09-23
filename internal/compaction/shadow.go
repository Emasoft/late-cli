package compaction

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"late/internal/pathutil"
)

// DecisionKeep is the only decision the shadow-only stage records: every
// scored segment is kept; elision comes with the relocation stage.
const DecisionKeep = "keep"

// ShadowEntry is one JSONL line in the shadow log: a single scored segment
// at a single decision point. The raw task text never reaches the log —
// only its TaskHash digest does.
type ShadowEntry struct {
	TS        time.Time `json:"ts"`
	TaskHash  string    `json:"task_hash"`
	SegmentID string    `json:"segment_id"`
	Tokens    int       `json:"tokens"`
	Score     float64   `json:"score"`
	Decision  string    `json:"decision"`
}

// ReplayReport summarizes what WOULD have been elided at a score threshold.
// Miss-risk — whether eliding a segment would actually have lost information
// the agent needed — is not computable offline, so the report is counts only.
type ReplayReport struct {
	Threshold      float64 `json:"threshold"`
	Entries        int     `json:"entries"`         // decision lines read
	UniqueSegments int     `json:"unique_segments"` // distinct segment_ids
	ElidedEntries  int     `json:"elided_entries"`  // decisions scoring below the threshold
	ElidedSegments int     `json:"elided_segments"` // distinct segment_ids elided at least once
	TokensTotal    int     `json:"tokens_total"`    // tokens across all entries
	TokensElided   int     `json:"tokens_elided"`   // tokens on elided entries
	MalformedLines int     `json:"malformed_lines"` // lines that failed to parse
}

// HashTask returns the short SHA-256 hex digest used as task_hash in the
// shadow log (16 hex chars — enough to group decisions by task without
// leaking the task text).
func HashTask(task string) string {
	sum := sha256.Sum256([]byte(task))
	return hex.EncodeToString(sum[:8])
}

// ShadowLog is a JSONL appender for decision records. Appends are
// goroutine-safe (mutex) and crash-atomic per line (one Write call on an
// O_APPEND descriptor, so concurrent late processes interleave whole lines).
type ShadowLog struct {
	path string
	mu   sync.Mutex
}

// DefaultShadowPath returns the shadow log location:
// ~/.local/share/late/compaction-shadow.jsonl (mirroring the session dir's
// platform handling; Windows keeps everything under the config dir).
func DefaultShadowPath() (string, error) {
	if runtime.GOOS == "windows" {
		dir, err := pathutil.LateConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, "compaction-shadow.jsonl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "late", "compaction-shadow.jsonl"), nil
}

// NewShadowLog opens (creating parent directories 0700) the default shadow
// log at DefaultShadowPath.
func NewShadowLog() (*ShadowLog, error) {
	p, err := DefaultShadowPath()
	if err != nil {
		return nil, err
	}
	return NewShadowLogAt(p)
}

// NewShadowLogAt opens the shadow log at path, creating parent directories
// with 0700 (the log file itself is created 0600 on first append).
func NewShadowLogAt(path string) (*ShadowLog, error) {
	if path == "" {
		return nil, fmt.Errorf("compaction: shadow log path is empty")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("compaction: create shadow log dir %s: %w", dir, err)
	}
	return &ShadowLog{path: path}, nil
}

// Path returns the log file path.
func (l *ShadowLog) Path() string { return l.path }

// Append writes one decision line. Zero fields are defaulted: a zero TS
// becomes time.Now() and an empty Decision becomes DecisionKeep. The entry
// is serialized first so a marshal failure cannot leave a torn line behind.
func (l *ShadowLog) Append(e ShadowEntry) error {
	if e.Decision == "" {
		e.Decision = DecisionKeep
	}
	if e.TS.IsZero() {
		e.TS = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("compaction: encode shadow entry: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("compaction: open shadow log %s: %w", l.path, err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("compaction: append shadow log %s: %w", l.path, err)
	}
	return nil
}

// Replay reads the whole log and reports what WOULD be elided at threshold:
// every decision whose score is strictly below threshold counts as elided.
// A missing log file is an empty report, not an error (nothing has been
// scored yet).
func (l *ShadowLog) Replay(threshold float64) (ReplayReport, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReplayReport{Threshold: threshold}, nil
		}
		return ReplayReport{}, fmt.Errorf("compaction: open shadow log %s: %w", l.path, err)
	}
	defer f.Close()

	report := ReplayReport{Threshold: threshold}
	seen := make(map[string]bool)
	elidedSeen := make(map[string]bool)
	sc := bufio.NewScanner(f)
	// Segment IDs are short but the lines carry no payload text; a 4 MiB
	// cap is purely defensive against a corrupted or hostile log.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e ShadowEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			report.MalformedLines++
			continue
		}
		report.Entries++
		report.TokensTotal += e.Tokens
		if !seen[e.SegmentID] {
			seen[e.SegmentID] = true
			report.UniqueSegments++
		}
		if e.Score < threshold {
			report.ElidedEntries++
			report.TokensElided += e.Tokens
			if !elidedSeen[e.SegmentID] {
				elidedSeen[e.SegmentID] = true
				report.ElidedSegments++
			}
		}
	}
	if err := sc.Err(); err != nil {
		return ReplayReport{}, fmt.Errorf("compaction: read shadow log %s: %w", l.path, err)
	}
	return report, nil
}
