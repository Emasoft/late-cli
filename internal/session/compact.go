package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"late/internal/client"
	"late/internal/common"
	"late/internal/compaction"
	"late/internal/tool"
)

// Full-history context compaction.
//
// CompactContext is the core both the /jev-compact-context command and the
// auto-trigger call: it walks the session history, segments every eligible
// message after the frozen prefix, scores the segments against the ongoing
// task, and relocates low-scoring segments into the elided-original store,
// replacing them in place with [[elided …]] pointer lines that the expand
// tool resolves. It reuses the compaction port's segmentation and scoring
// contract (internal/compaction) but owns the history walk, because the
// pipeline's tool-output path (CompactToolOutput) works one tool result at a
// time and knows nothing about history structure.
//
// Invariants (the upstream jev-compaction design):
//
//   - Frozen prefix: the first max(1, len(history)/4) messages are never
//     compacted, so the prompt-cache anchor — the system prompt at index 0
//     plus the earliest exchanges — stays byte-identical across compactions.
//   - User messages are never compacted; assistant and tool-result contents
//     are. Assistant ToolCalls are structurally required and are never
//     touched: only Content shrinks.
//   - Segments are scored against the ongoing task (the last user message);
//     a segment scoring strictly below the threshold is elided into the
//     store and replaced by one pointer line.
//   - Fail-open: a scorer error that still answers every requested id (the
//     pipeline's contract — unscoreable items come back as keep-scores)
//     does not stop the walk: those scores are used, the scorer's errors
//     accumulate and surface at the end via errors.Join. The walk stops
//     mid-flight only when the scorer returns no usable scores for a
//     message; messages already rewritten stay rewritten (their pointers
//     and stored originals are valid) and the returned error reports how
//     far the walk got. Compaction never breaks a session.
//   - Shadow mode (compaction-mode "shadow") runs the full scoring walk and
//     computes the honest would-save report without mutating history.
//
// The scorer and the store are passed in (dependency injection): the session
// never constructs the compaction pipeline — the TUI/main holds it and hands
// CompactContext its scoring client and original-text store.
const (
	// minCompactChars is the content size above which a post-frozen-prefix
	// assistant or tool-result message becomes a compaction candidate. It is
	// the segment default: a message shorter than one maximum segment offers
	// no elidable granularity.
	minCompactChars = compaction.DefaultMaxSegChars

	// defaultFrozenPercent is the share of history messages (by count) the
	// frozen prefix keeps byte-identical.
	defaultFrozenPercent = 25

	// compactionTaskFallback is the derived task when the history contains
	// no user message to score against.
	compactionTaskFallback = "general context compaction"

	// maxTaskChars caps the derived task so one giant prompt does not become
	// a giant scoring request.
	maxTaskChars = 500

	// elidePreviewChars is how many characters of an elided segment's
	// original text are shown inside its pointer line, so the agent can guess
	// whether the segment is worth expanding without paying for it.
	elidePreviewChars = 60

	// keepScoreFallback is the defensive fail-open score for a segment whose
	// id is missing from the scorer's answer map: fully essential, keep.
	// (compaction.DecisionClient.ScoreBatch fills every id; this only guards
	// against a non-conforming HistoryScorer.)
	keepScoreFallback = 1.0
)

// HistoryScorer scores one batch of segments against an ongoing task. It is
// satisfied by compaction.DecisionClient (the pipeline's scoring client);
// tests inject map-based stubs. Like the pipeline's contract, an error
// return that still carries a score for every requested id is the fail-open
// shape (unscoreable items answered as keep-scores): CompactContext keeps
// walking with those scores and surfaces the errors at the end. An error
// with scores missing means the batch could not be scored — CompactContext
// stops the walk rather than eliding on unusable answers.
type HistoryScorer interface {
	ScoreBatch(ctx context.Context, task string, items map[string]compaction.Item) (map[string]float64, error)
}

// ElideStore is the write side of the elided-original store CompactContext
// relocates segments into: NextID mints the pointer ids ("elide-<n>") and
// Put stores each elided segment's original under its id so the expand tool
// can retrieve it (tool.ExpandStore reads it back). One store is one id
// space; production wiring backs it with the compaction pipeline's store so
// tool-output and history pointers never collide.
type ElideStore interface {
	NextID() string
	Put(id, text string)
}

// CompactStore is the in-memory original-text store backing history
// compaction: CompactContext Puts every elided segment's original under the
// id minted by NextID, and the expand tool Gets it back — it satisfies
// tool.ExpandStore directly. Safe for concurrent use.
type CompactStore struct {
	mu        sync.Mutex
	originals map[string]string
	next      int
}

// NewCompactStore returns an empty original-text store.
func NewCompactStore() *CompactStore {
	return &CompactStore{originals: make(map[string]string)}
}

// NextID mints the next elide-pointer id ("elide-<n>", 1-based).
func (s *CompactStore) NextID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	return fmt.Sprintf("elide-%d", s.next)
}

// Put stores text under id, minted by NextID.
func (s *CompactStore) Put(id, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.originals == nil {
		s.originals = make(map[string]string)
	}
	s.originals[id] = text
}

// Get returns the original stored for id (the tool.ExpandStore read side).
// An unknown id reports ("", false).
func (s *CompactStore) Get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	text, ok := s.originals[id]
	return text, ok
}

// The expand tool reads history-elided originals straight from this store.
var _ tool.ExpandStore = (*CompactStore)(nil)

// The compaction pipeline's scoring client is the production HistoryScorer.
var _ HistoryScorer = (*compaction.DecisionClient)(nil)

// CompactionOptions tunes CompactContext; zero values are production
// defaults.
type CompactionOptions struct {
	// Threshold is the score strictly below which a segment is elided.
	// Values outside (0, 1] fall back to compaction.DefaultRelocationThreshold
	// (0.35), matching Pipeline.EnableRelocation.
	Threshold float64
	// FrozenPercent is the share of history messages (by count) kept
	// byte-identical in the frozen prefix. Values outside (0, 100] fall back
	// to defaultFrozenPercent (25); 100 freezes everything (a no-op run).
	FrozenPercent int
	// ShadowOnly computes the would-save report without mutating history or
	// storing originals (compaction-mode "shadow").
	ShadowOnly bool
}

// CompactionReport summarizes one CompactContext run. In shadow mode the
// counts are what a mutating run would have done (honest staging: the same
// numbers a real run reports, nothing applied).
type CompactionReport struct {
	// MessagesScanned is the number of history messages the walk examined —
	// everything after the frozen prefix.
	MessagesScanned int
	// MessagesScored is the number of messages whose segments were actually
	// scored, including fail-open-scored ones (a scorer error answered with
	// a complete keep-score map still counts the message as scored).
	MessagesScored int
	// MessagesCompacted is the number of messages actually rewritten (or
	// would-be rewritten in shadow mode).
	MessagesCompacted int
	// SegmentsElided is the number of segments relocated into the store.
	SegmentsElided int
	// TokensBefore is the summed EstimateMessageTokens of the whole history
	// before the run.
	TokensBefore int
	// TokensAfter is the same sum after the run (the would-be sum in shadow
	// mode).
	TokensAfter int
	// TokensSaved is TokensBefore - TokensAfter.
	TokensSaved int
	// StoreSize is the total byte size of the original texts this run
	// relocated into the store (what they would be in shadow mode).
	StoreSize int
	// ShadowOnly reports whether the run mutated anything.
	ShadowOnly bool
	// TaskHash is the compaction.HashTask digest of the scoring task this
	// run scored against (the derived ongoing task, never its text). The
	// shadow log's per-run summary lines group under it, the same way the
	// per-segment decision lines do.
	TaskHash string
}

// CompactContext compacts the session history in place (unless
// opts.ShadowOnly) per the package doc. scorer scores segments; store
// receives the elided originals. A nil store leaves relocation disarmed —
// like the pipeline with relocation off, nothing is elided, because a
// pointer whose original cannot be stored must never enter history. The
// returned error is non-nil when the scorer reported failures: a walk that
// completed on fail-open scores joins those scorer errors at the end, and a
// walk that had to stop (no usable scores for a message) reports how far it
// got. The report covers everything completed either way, and
// already-rewritten messages stay rewritten; persistence is the caller's job
// (SaveHistory).
func (s *Session) CompactContext(ctx context.Context, scorer HistoryScorer, store ElideStore, opts CompactionOptions) (CompactionReport, error) {
	var report CompactionReport
	if scorer == nil {
		return report, fmt.Errorf("context compaction: no scorer provided")
	}
	// Zero values are production defaults (mirrors Pipeline.EnableRelocation
	// and the frozen-prefix default).
	if opts.Threshold <= 0 || opts.Threshold > 1 {
		opts.Threshold = compaction.DefaultRelocationThreshold
	}
	if opts.FrozenPercent <= 0 || opts.FrozenPercent > 100 {
		opts.FrozenPercent = defaultFrozenPercent
	}
	report.ShadowOnly = opts.ShadowOnly

	frozen := frozenPrefix(len(s.History), opts.FrozenPercent)
	report.MessagesScanned = len(s.History) - frozen

	report.TokensBefore = historyMessageTokens(s.History)
	report.TokensAfter = report.TokensBefore

	// The scoring task is hashed up front so every report carries its group
	// key even when the walk stops early: the run summary the caller appends
	// to the shadow log uses this digest, and the per-segment decisions the
	// scorer logs share it.
	task := compactionTask(s.History)
	report.TaskHash = compaction.HashTask(task)

	if store == nil {
		// Relocation disarmed: nothing can be stored for the expand tool, so
		// nothing is elided. The scan/token counts above still stand.
		return report, nil
	}

	scored := 0
	var walkErrs []error

	for i := frozen; i < len(s.History); i++ {
		msg := &s.History[i]
		if !compactableContent(msg) {
			continue
		}
		segs := compaction.SegmentSegments(msg.Content.Text, minCompactChars)
		if len(segs) == 0 {
			continue
		}
		items := make(map[string]compaction.Item, len(segs))
		for _, seg := range segs {
			items[seg.ID] = compaction.Item{Text: seg.Text, Tokens: seg.Tokens}
		}
		scores, err := scorer.ScoreBatch(ctx, task, items)
		if err != nil {
			if !scoresComplete(items, scores) {
				// Wholesale failure: the scorer returned no usable scores
				// for this message. Fail-open mid-walk: stop the walk.
				// Messages already rewritten stay rewritten — their pointers
				// and stored originals are valid — and the error says how
				// far the walk got.
				return report, fmt.Errorf("context compaction stopped after %d messages: %w", scored, err)
			}
			// Fail-open complete: every requested id came back (the
			// pipeline's contract — unscoreable items are answered with
			// keep-scores), so the scores are usable. Keep the walk going;
			// the scorer's errors surface at the end.
			walkErrs = append(walkErrs, err)
		}
		scored++
		report.MessagesScored++

		var (
			kept     []compaction.Segment
			pointers []string
			stored   int
		)
		for _, seg := range segs {
			score, ok := scores[seg.ID]
			if !ok {
				score = keepScoreFallback
			}
			if score >= opts.Threshold {
				kept = append(kept, seg)
				continue
			}
			id := store.NextID()
			pointers = append(pointers, elidePointerLine(id, seg))
			stored += len(seg.Text)
			if !opts.ShadowOnly {
				store.Put(id, seg.Text)
			}
		}
		if len(pointers) == 0 {
			continue
		}

		// Kept segments keep their exact text (each span includes its
		// trailing blank-line separator, so concatenation reproduces the
		// original minus the elided spans), then one pointer line per elided
		// segment — the same shape CompactToolOutput produces.
		var b strings.Builder
		for _, seg := range kept {
			b.WriteString(seg.Text)
		}
		b.WriteString("\n\n")
		b.WriteString(strings.Join(pointers, "\n"))
		compacted := b.String()

		report.MessagesCompacted++
		report.SegmentsElided += len(pointers)
		report.StoreSize += stored
		// Only Content.Text changes, so the per-message token delta is the
		// text delta — exact for both the mutating and the shadow run.
		report.TokensAfter -= common.EstimateTokenCount(msg.Content.Text) - common.EstimateTokenCount(compacted)
		if !opts.ShadowOnly {
			// In-place content swap: Role, ToolCalls, ToolCallID, Timestamp
			// and ReasoningContent are preserved; only Content shrinks.
			msg.Content = client.TextContent(compacted)
		}
	}

	report.TokensSaved = report.TokensBefore - report.TokensAfter
	// Fail-open scorer errors accumulated along the walk surface here; a
	// clean walk returns a nil error.
	return report, errors.Join(walkErrs...)
}

// scoresComplete reports whether scores answers every id in items — the
// pipeline's fail-open contract shape (compaction.DecisionClient.ScoreBatch
// fills every id, unscoreable items with keep-scores, even when it also
// reports errors). A missing id means the scorer had nothing usable for that
// segment.
func scoresComplete(items map[string]compaction.Item, scores map[string]float64) bool {
	for id := range items {
		if _, ok := scores[id]; !ok {
			return false
		}
	}
	return true
}

// frozenPrefix computes how many leading history messages are never
// compacted: max(1, len(history)*percent/100), rounded down, capped at the
// history length. The floor of 1 always keeps message[0] — the system
// prompt — inside the prefix (prompt-cache preservation); for histories of
// twelve or more messages at the default 25% the prefix is a quarter of the
// messages, rounded down.
func frozenPrefix(n, percent int) int {
	if n <= 0 {
		return 0
	}
	frozen := n * percent / 100
	if frozen < 1 {
		frozen = 1
	}
	if frozen > n {
		frozen = n
	}
	return frozen
}

// compactableContent reports whether msg is a compaction candidate: an
// assistant or tool-result message whose text-only content exceeds
// minCompactChars. User messages are never compacted; multimodal messages
// pass through untouched (parts cannot be rebuilt losslessly here); shorter
// messages offer no elidable granularity.
func compactableContent(msg *client.ChatMessage) bool {
	if msg.Role != "assistant" && msg.Role != "tool" {
		return false
	}
	if len(msg.Content.Parts) != 0 {
		return false
	}
	return len(msg.Content.Text) > minCompactChars
}

// compactionTask derives the ongoing-task description the scorer scores
// against: the last user message's content truncated to maxTaskChars runes,
// or compactionTaskFallback when the history has no user message.
func compactionTask(history []client.ChatMessage) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" {
			return truncateRunes(history[i].Content.String(), maxTaskChars)
		}
	}
	return compactionTaskFallback
}

// truncateRunes cuts s to at most max runes, rune-safe and without a suffix.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// historyMessageTokens sums EstimateMessageTokens over the history. The
// system prompt lives outside the history (Session.systemPrompt) and is
// never touched, so it is not part of the before/after math.
func historyMessageTokens(history []client.ChatMessage) int {
	total := 0
	for _, msg := range history {
		total += common.EstimateMessageTokens(msg)
	}
	return total
}

// elidePointerLine renders the [[elided …]] pointer that replaces an elided
// segment — the exact format the expand tool's description and the
// compaction pipeline pin.
func elidePointerLine(id string, seg compaction.Segment) string {
	return fmt.Sprintf(`[[elided id=%s lines=%d tokens=%d %q]]`, id, countLines(seg.Text), seg.Tokens, previewOf(seg.Text))
}

// countLines counts the lines in text (a non-empty string always has at
// least one line; a trailing newline does not start a new line).
func countLines(text string) int {
	if text == "" {
		return 0
	}
	return strings.Count(text, "\n") + 1
}

// previewOf returns the first elidePreviewChars characters of text with line
// breaks and tabs flattened to spaces, so the pointer stays one line.
func previewOf(text string) string {
	flat := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, text)
	runes := []rune(flat)
	if len(runes) > elidePreviewChars {
		runes = runes[:elidePreviewChars]
	}
	return string(runes)
}
