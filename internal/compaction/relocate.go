package compaction

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"late/internal/common"
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
	// ID is the store key ("elide-<n>", a per-store counter shared by
	// tool-output and history compaction).
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
	// Tripwire is non-empty when the gate's safety tripwire fired and the
	// scorer's elision decisions were discarded: TripwireMaxElideFraction
	// means the scorer wanted to elide more than MaxElideFraction of the
	// output's tokens, so it was distrusted and everything was kept. Empty
	// means the decisions stood.
	Tripwire string
}

// Store holds the original text of elided segments, keyed by elide id, so
// the expand tool can retrieve what compaction removed. It is safe for
// concurrent use: the root agent and every subagent share one store.
//
// The store owns the elide-id counter: the session's CompactContext shares
// the pipeline's elide-id space with the expand tool through this one store
// (NextID/Put), so tool-output and history pointers can never collide —
// "elide-<n>" always names exactly one original.
type Store struct {
	mu        sync.Mutex
	originals map[string]string
	next      int
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

// NextID mints the next elide-pointer id ("elide-<n>", 1-based). The
// session's CompactContext shares the pipeline's elide-id space with the
// expand tool through this counter, so history and tool-output pointers
// minted into one store never collide. A nil store returns "".
func (s *Store) NextID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("elide-%d", s.next)
}

// Put stores text under id (minted by NextID) — the exported write side the
// session's CompactContext uses to relocate elided history segments, next to
// the pipeline's own tool-output relocation. A nil store is a no-op.
func (s *Store) Put(id, text string) {
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

// CompactToolOutput scores one tool output and, when relocation is armed,
// relocates every segment scoring strictly below its floor — the
// protected-kind floor when the segment's kind has one (the gate's
// ProtectedKinds), else the keep threshold: the segment's original goes into
// the armed store and its slot in the result is replaced by an [[elided …]]
// pointer line. The returned CompactText is the tool result that should
// enter history.
//
// Two gate guards run before and after the per-segment decisions:
//
//   - MinGateTokens: an output estimated below that many tokens is returned
//     unchanged without scoring it (no backend call) — the round trip costs
//     more than any possible elision saves.
//   - The MaxElideFraction tripwire: when the scorer wants to elide more
//     than that share of the output's tokens, it is distrusted and NOTHING
//     is elided; the result reports Tripwire=TripwireMaxElideFraction and
//     the shadow log records the override.
//
// Fail-open contract, extended to relocation: any scoring error (provider
// outage, bad answer, canceled context) means no reliable elision decision
// exists, so the original output is returned unchanged (Elided empty) along
// with the error. Compaction must never break a tool call.
func (p *Pipeline) CompactToolOutput(ctx context.Context, toolName, output string) (CompactResult, error) {
	out := CompactResult{CompactText: output}

	// Min-gate: below MinGateTokens the scoring round trip costs more than
	// elision can possibly save — skip scoring entirely.
	if min := p.minGateTokens(); min > 0 && common.EstimateTokenCount(output) < min {
		return out, nil
	}

	scores, err := p.ScoreToolOutput(ctx, toolName, output)
	if err != nil {
		// Fail-open: keep the whole output verbatim.
		return out, err
	}

	store, _ := p.relocationArmed()
	if store == nil || len(scores.Segments) == 0 {
		return out, nil
	}

	// Decide per segment first (flags only), so the tripwire can still veto
	// the whole batch before anything is stored or replaced.
	gate := p.resolveGate()
	totalTokens, elidedTokens := 0, 0
	elide := make([]bool, len(scores.Segments))
	for i, seg := range scores.Segments {
		score, ok := scores.Scores[seg.ID]
		if !ok {
			score = keepScore // defensive; ScoreToolOutput fills every id
		}
		totalTokens += seg.Tokens
		if score < gate.floor(seg.Kind) {
			elide[i] = true
			elidedTokens += seg.Tokens
		}
	}

	// TRIPWIRE: a scorer that wants to drop most of the output is wrong
	// more often than not (and one broken score map could gut a tool result
	// wholesale). Distrust it: elide nothing, say so in the result, and log
	// the override.
	if gate.cfg.MaxElideFraction > 0 && totalTokens > 0 &&
		float64(elidedTokens) > gate.cfg.MaxElideFraction*float64(totalTokens) {
		for i := range elide {
			elide[i] = false
		}
		elidedTokens = 0
		out.Tripwire = TripwireMaxElideFraction
		p.logTripwire(scores.TaskHash, totalTokens)
	}

	var kept []Segment
	for i, seg := range scores.Segments {
		if !elide[i] {
			kept = append(kept, seg)
			continue
		}
		id := store.NextID()
		store.Put(id, seg.Text)
		out.Elided = append(out.Elided, ElidedSegment{
			ID:      id,
			Lines:   countLines(seg.Text),
			Tokens:  seg.Tokens,
			Preview: previewOf(seg.Text),
			Text:    seg.Text,
		})
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
