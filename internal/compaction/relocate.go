package compaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

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

// SummaryMaxChars is the reference summary_max_chars: how many characters of
// an elided run's first non-blank line its pointer carries, so the agent can
// guess whether the run is worth expanding without paying for it.
const SummaryMaxChars = 120

// contentIDPrefix is the reference's prefix for elided-run record ids:
// content-addressed ("r:<8hex>"), so the same run text always maps to the
// same id — and to the same store record.
const contentIDPrefix = "r"

// Pointer id charset, ported from the reference _POINTER_RE. Content ids
// ("r:<8hex>") and legacy counter ids ("elide-<n>") both fit it.
const pointerIDCharset = `[A-Za-z0-9:_.-]+`

// pointerPattern is the reference _POINTER_RE, byte for byte: one
// [[elided …]] pointer with a mandatory id, an optional "lines=a-b" range,
// a mandatory token count, and a double-quoted summary in which backslash
// escapes are allowed. Used both to parse pointers and (plus the optional
// trailing newline) to substitute them back during Reconstruct.
const pointerPattern = `\[\[elided id=(?P<id>` + pointerIDCharset + `)` +
	`(?: lines=(?P<start>\d+)-(?P<end>\d+))?` +
	` tokens=(?P<tokens>\d+)` +
	` "(?P<summary>(?:[^"\\]|\\.)*)"\]\]`

var (
	pointerRe     = regexp.MustCompile(pointerPattern)
	pointerLineRe = regexp.MustCompile(pointerPattern + `\n?`)
	// unescapeRe is the reference's summary unescape: re.sub(r"\\(.)", r"\1").
	unescapeRe = regexp.MustCompile(`\\(.)`)
)

// ContentID derives the stable short id for a piece of content: the first 8
// hex chars of sha256(salt + "\x00" + text), prefixed. Ported from the
// reference content_id (types.py). Same input → same id, always — which is
// what makes elided-run records content-addressed: re-compacting identical
// text cannot mint a second record for it.
func ContentID(text, salt, prefix string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + text))
	return prefix + ":" + hex.EncodeToString(sum[:])[:8]
}

// Pointer is the one-line stand-in left in context for relocated content
// (the reference types.py Pointer). Lines is the 1-based [first, last] line
// range of the elided run in the original output, or nil when unknown; the
// id names the run's record in the Store (content ids "r:<8hex>", or legacy
// counter ids "elide-<n>", both parse).
type Pointer struct {
	ID      string
	Lines   *[2]int
	Tokens  int
	Summary string
}

// FormatPointer renders one pointer line — the exact format ParsePointer
// parses back. The summary is escaped (backslashes first, then quotes) so
// the result is always a single line the regex can recover it from.
func FormatPointer(p Pointer) string {
	lines := ""
	if p.Lines != nil {
		lines = fmt.Sprintf(" lines=%d-%d", p.Lines[0], p.Lines[1])
	}
	summary := strings.ReplaceAll(p.Summary, "\\", "\\\\")
	summary = strings.ReplaceAll(summary, `"`, `\"`)
	return fmt.Sprintf(`[[elided id=%s%s tokens=%d "%s"]]`, p.ID, lines, p.Tokens, summary)
}

// ParsePointer parses one pointer line — the inverse of FormatPointer,
// ported from the reference parse_pointer. The second return is false when
// line carries no pointer at all. Legacy "elide-<n>" ids parse too (the id
// charset allows them), and a missing lines part yields a nil Lines.
func ParsePointer(line string) (Pointer, bool) {
	m := pointerRe.FindStringSubmatch(line)
	if m == nil {
		return Pointer{}, false
	}
	get := func(name string) string { return m[pointerRe.SubexpIndex(name)] }
	p := Pointer{
		ID:      get("id"),
		Summary: unescapeSummary(get("summary")),
	}
	p.Tokens, _ = strconv.Atoi(get("tokens"))
	if start, end := get("start"), get("end"); start != "" && end != "" {
		a, _ := strconv.Atoi(start)
		b, _ := strconv.Atoi(end)
		p.Lines = &[2]int{a, b}
	}
	return p, true
}

// FindPointers returns every pointer in text, in order of appearance
// (the reference find_pointers).
func FindPointers(text string) []Pointer {
	matches := pointerRe.FindAllString(text, -1)
	out := make([]Pointer, 0, len(matches))
	for _, match := range matches {
		if p, ok := ParsePointer(match); ok {
			out = append(out, p)
		}
	}
	return out
}

// unescapeSummary undoes FormatPointer's escaping: every backslash-escaped
// character collapses to the character itself (the reference's
// re.sub(r"\\(.)", r"\1", summary)).
func unescapeSummary(s string) string {
	return unescapeRe.ReplaceAllString(s, "$1")
}

// Summarise builds a pointer summary from run text (the reference
// _summarise): the first non-blank line, whitespace-flattened, cut at the
// last word boundary within limit runes with a "…" suffix. An empty text
// (or one with no non-blank line) summarises to "".
func Summarise(text string, limit int) string {
	for _, rawLine := range strings.Split(text, "\n") {
		line := strings.Join(strings.Fields(rawLine), " ")
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) <= limit {
			return line
		}
		if limit <= 0 {
			return "…"
		}
		cut := string(runes[:limit])
		// Word-boundary cut: drop the trailing partial word (rsplit(" ", 1)[0]),
		// falling back to the raw cut when that leaves nothing.
		if idx := strings.LastIndex(cut, " "); idx >= 0 {
			cut = cut[:idx]
		}
		if cut == "" {
			cut = string(runes[:limit])
		}
		return cut + "…"
	}
	return ""
}

// Reconstruct substitutes every pointer in text back with the original text
// it stands for — the byte-for-byte inverse of what admit/compaction
// produced, provided the records are still in the store (the reference
// reconstruct). Each match is the pointer line plus its trailing newline, so
// the substituted text lands exactly where the run was; a pointer whose id
// is unknown (record gone, foreign text) is left as-is. A nil store leaves
// every pointer in place.
func Reconstruct(text string, store *Store) string {
	return pointerLineRe.ReplaceAllStringFunc(text, func(match string) string {
		p, ok := ParsePointer(match)
		if !ok {
			return match // unreachable: the regex just matched
		}
		original, found := store.Get(p.ID)
		if !found {
			return match
		}
		return original
	})
}

// ElidedSegment is one RUN of consecutive segments removed from a tool
// result (or history message) by relocation. ID/Lines/Tokens/Summary
// describe the pointer that replaced the run; Text is the concatenated
// original the store keeps for the expand tool.
type ElidedSegment struct {
	// ID is the run's content id ("r:<8hex>" — ContentID of the run text
	// with the tool name as salt), so the same run stored twice collapses
	// onto one record.
	ID string
	// Lines is the 1-based [first, last] line range the run occupied in the
	// original output.
	Lines [2]int
	// Tokens is the run's summed token count.
	Tokens int
	// Segments is the number of source segments the run grouped.
	Segments int
	// Summary is Summarise(Text, SummaryMaxChars) — the pointer's preview.
	Summary string
	// Text is the original run text (what the expand tool returns).
	Text string
}

// CompactResult is the outcome of relocating one tool output.
type CompactResult struct {
	// CompactText is the replacement tool result: kept segments in place,
	// each run of elided segments replaced by one [[elided …]] pointer line.
	// When nothing was elided this is the original output byte-for-byte.
	CompactText string
	// Elided carries the relocated runs (originals included) for the
	// caller's Store, in order of appearance. Empty when nothing was elided.
	Elided []ElidedSegment
	// Tripwire is non-empty when the gate's safety tripwire fired and the
	// scorer's elision decisions were discarded: TripwireMaxElideFraction
	// means the scorer wanted to elide more than MaxElideFraction of the
	// output's tokens, so it was distrusted and everything was kept. Empty
	// means the decisions stood.
	Tripwire string
}

// EnableRelocation arms stage 2 on the pipeline: from now on
// CompactToolOutput elides segments whose score is strictly below threshold
// into store and replaces each run of them with [[elided …]] pointers.
// threshold outside (0, 1] falls back to DefaultRelocationThreshold. Calling
// it again re-arms with the new values; passing a nil store disarms
// relocation (the pipeline degenerates to shadow-only scoring).
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
// Runs: consecutive below-floor segments are grouped into ONE run (the
// reference pipeline's flush_run pattern) sharing a single record and a
// single pointer — the record's text is the concatenated run, its id is
// ContentID(runText, toolName, "r"), and the pointer carries the run's
// [first, last] line range in the original output. Pointer lines stand
// exactly where the runs stood, so Reconstruct(compacted, store) restores
// the original byte for byte.
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
	var b strings.Builder
	var run []Segment

	// flushRun relocates the accumulated run of consecutive elided segments
	// (the reference pipeline's flush_run): the concatenated run text goes
	// into the store under its content id, and one pointer line — run line
	// range, summed tokens, 120-char summary — takes the run's place in the
	// output.
	flushRun := func() {
		if len(run) == 0 {
			return
		}
		var text strings.Builder
		segIDs := make([]string, 0, len(run))
		for _, seg := range run {
			text.WriteString(seg.Text)
			segIDs = append(segIDs, seg.ID)
		}
		runText := text.String()
		id := ContentID(runText, toolName, contentIDPrefix)
		tokens := 0
		for _, seg := range run {
			tokens += seg.Tokens
		}
		summary := Summarise(runText, SummaryMaxChars)
		// The record carries what the reference attaches to every stored
		// run (store.py Record): its origin — "tool:<name>", the only
		// surface this path knows — token count, pointer summary, and the
		// contributing segment ids the outcomes ledger attributes back to.
		// Turn stays 0: turn plumbing does not exist yet.
		store.PutRecord(Record{
			ID:         id,
			Text:       runText,
			Kind:       RecordKindElidedSegment,
			Origin:     Origin{Source: OriginSourceToolPrefix + toolName},
			Tokens:     tokens,
			Summary:    summary,
			SegmentIDs: segIDs,
		})
		e := ElidedSegment{
			ID:       id,
			Lines:    [2]int{run[0].LineStart, run[len(run)-1].LineEnd},
			Tokens:   tokens,
			Segments: len(run),
			Summary:  summary,
			Text:     runText,
		}
		out.Elided = append(out.Elided, e)
		b.WriteString(FormatPointer(Pointer{
			ID:      e.ID,
			Lines:   &[2]int{e.Lines[0], e.Lines[1]},
			Tokens:  e.Tokens,
			Summary: e.Summary,
		}))
		b.WriteString("\n")
		run = run[:0]
	}

	for i, seg := range scores.Segments {
		if !elide[i] {
			flushRun()
			kept = append(kept, seg)
			b.WriteString(seg.Text)
			continue
		}
		run = append(run, seg)
	}
	flushRun()

	if len(out.Elided) == 0 {
		// Nothing to relocate: return the original byte-for-byte. (The kept
		// segments alone would drop leading blank bytes, which belong to no
		// segment.)
		out.Elided = nil
		out.CompactText = output
		return out, nil
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
	elidedSegments := 0
	for _, e := range compacted.Elided {
		elidedSegments += e.Segments
		ids = append(ids, e.ID)
	}
	return fmt.Sprintf("%s\n\n%d segments elided — use the expand tool with the elided ids to retrieve originals (%s).",
		compacted.CompactText, elidedSegments, strings.Join(ids, ", "))
}
