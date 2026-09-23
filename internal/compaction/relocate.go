package compaction

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// DecisionElide is the shadow-log decision recorded for a segment whose
// score fell strictly below the relocation threshold while relocation is
// armed (compaction-mode "enabled"). Shadow mode never records it: there
// every segment is DecisionKeep.
const DecisionElide = "elide"

// DefaultRelocationThreshold is the score under which a segment is elided
// when compaction-mode is "enabled" and no explicit threshold was set. It
// comes from the upstream repo's own shadow-log replay data.
const DefaultRelocationThreshold = 0.35

// previewChars is how many characters of an elided segment's original text
// are shown inside its pointer line, so the agent can guess whether the
// segment is worth expanding without paying for it.
const previewChars = 60

// ElidedSegment is one segment removed from a tool result by relocation.
// ID/Tokens/Lines/Preview describe the pointer that replaced the segment;
// Text is the original that was stored for the expand tool.
type ElidedSegment struct {
	// ID is the store key ("elide-<n>", a per-pipeline counter).
	ID string
	// Lines is the line count of the stored original text.
	Lines int
	// Tokens is the segment's estimated token count.
	Tokens int
	// Preview is the first previewChars characters of the original with
	// line breaks and tabs flattened to spaces.
	Preview string
	// Text is the original segment text (what the expand tool returns).
	Text string
}

// CompactResult is the outcome of relocating one tool output.
type CompactResult struct {
	// CompactText is the replacement tool result: the kept segments followed
	// by one [[elided …]] pointer line per elided segment. When nothing was
	// elided this is the original output byte-for-byte.
	CompactText string
	// Elided carries the relocated segments (originals included) for the
	// caller's Store. Empty when nothing was elided.
	Elided []ElidedSegment
}

// Store holds the original text of elided segments, keyed by elide id, so
// the expand tool can retrieve what compaction removed. It is safe for
// concurrent use: the root agent and every subagent share one store.
type Store struct {
	mu        sync.Mutex
	originals map[string]string
}

// NewStore returns an empty original-text store.
func NewStore() *Store {
	return &Store{originals: make(map[string]string)}
}

// Get returns the original text stored for id. A nil store — or an unknown
// id — reports ( "", false ); the expand tool turns the miss into an
// "unknown elided id" error result.
func (s *Store) Get(id string) (string, bool) {
	if s == nil {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok := s.originals[id]
	return text, ok
}

// put records id → original. Only the pipeline writes: ids are minted by the
// pipeline's counter so they stay unique across agents.
func (s *Store) put(id, text string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.originals == nil {
		s.originals = make(map[string]string)
	}
	s.originals[id] = text
}

// EnableRelocation arms stage 2 on the pipeline: from now on
// CompactToolOutput elides segments whose score is strictly below threshold
// into store and replaces them with [[elided …]] pointers. threshold outside
// (0, 1] falls back to DefaultRelocationThreshold. Calling it again re-arms
// with the new values; passing a nil store disarms relocation (the pipeline
// degenerates to shadow-only scoring).
func (p *Pipeline) EnableRelocation(store *Store, threshold float64) {
	if p == nil {
		return
	}
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultRelocationThreshold
	}
	p.relocMu.Lock()
	defer p.relocMu.Unlock()
	p.reloc = store
	p.threshold = threshold
}

// relocationArmed reports the armed store (nil when relocation is off) and
// its threshold.
func (p *Pipeline) relocationArmed() (*Store, float64) {
	if p == nil {
		return nil, 0
	}
	p.relocMu.Lock()
	defer p.relocMu.Unlock()
	return p.reloc, p.threshold
}

// nextElideID mints the next per-pipeline elide id ("elide-<n>").
func (p *Pipeline) nextElideID() string {
	p.relocMu.Lock()
	defer p.relocMu.Unlock()
	p.nextElide++
	return fmt.Sprintf("elide-%d", p.nextElide)
}

// CompactToolOutput scores one tool output and, when relocation is armed,
// relocates every segment scoring strictly below the threshold: the
// segment's original goes into the armed store and its slot in the result is
// replaced by an [[elided …]] pointer line. The returned CompactText is the
// tool result that should enter history.
//
// Fail-open contract, extended to relocation: any scoring error (provider
// outage, bad answer, canceled context) means no reliable elision decision
// exists, so the original output is returned unchanged (Elided empty) along
// with the error. Compaction must never break a tool call.
func (p *Pipeline) CompactToolOutput(ctx context.Context, toolName, output string) (CompactResult, error) {
	out := CompactResult{CompactText: output}

	scores, err := p.ScoreToolOutput(ctx, toolName, output)
	if err != nil {
		// Fail-open: keep the whole output verbatim.
		return out, err
	}

	store, threshold := p.relocationArmed()
	if store == nil || len(scores.Segments) == 0 {
		return out, nil
	}

	var kept []Segment
	for _, seg := range scores.Segments {
		score, ok := scores.Scores[seg.ID]
		if !ok {
			score = keepScore // defensive; ScoreToolOutput fills every id
		}
		if score < threshold {
			id := p.nextElideID()
			store.put(id, seg.Text)
			out.Elided = append(out.Elided, ElidedSegment{
				ID:      id,
				Lines:   countLines(seg.Text),
				Tokens:  seg.Tokens,
				Preview: previewOf(seg.Text),
				Text:    seg.Text,
			})
			continue
		}
		kept = append(kept, seg)
	}

	if len(out.Elided) == 0 {
		// Nothing to relocate: return the original byte-for-byte. (The kept
		// segments alone would drop leading blank bytes, which belong to no
		// segment.)
		out.Elided = nil
		out.CompactText = output
		return out, nil
	}

	var b strings.Builder
	for _, seg := range kept {
		b.WriteString(seg.Text)
	}
	b.WriteString("\n\n")
	for i, e := range out.Elided {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(pointerLine(e))
	}
	out.CompactText = b.String()
	return out, nil
}

// CompactToolResult adapts CompactToolOutput to the executor's
// ToolResultCompactor interface: it returns the compacted result with a
// trailer telling the agent how many segments were elided and how to get
// them back, or the original result unchanged when nothing was elided or
// scoring failed.
func (p *Pipeline) CompactToolResult(ctx context.Context, toolName, result string) string {
	compacted, err := p.CompactToolOutput(ctx, toolName, result)
	if err != nil || len(compacted.Elided) == 0 {
		return result
	}
	ids := make([]string, 0, len(compacted.Elided))
	for _, e := range compacted.Elided {
		ids = append(ids, e.ID)
	}
	return fmt.Sprintf("%s\n\n%d segments elided — use the expand tool with the elided ids to retrieve originals (%s).",
		compacted.CompactText, len(compacted.Elided), strings.Join(ids, ", "))
}

// pointerLine renders one elided pointer: the exact format the expand tool's
// description and the tests pin.
func pointerLine(e ElidedSegment) string {
	return fmt.Sprintf(`[[elided id=%s lines=%d tokens=%d %q]]`, e.ID, e.Lines, e.Tokens, e.Preview)
}

// countLines counts the lines in text (a non-empty string always has at
// least one line; a trailing newline does not start a new line).
func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// previewOf returns the first previewChars characters of text with line
// breaks and tabs flattened to spaces, so the pointer stays one line.
func previewOf(text string) string {
	flat := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, text)
	runes := []rune(flat)
	if len(runes) > previewChars {
		runes = runes[:previewChars]
	}
	return string(runes)
}
