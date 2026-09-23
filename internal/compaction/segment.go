package compaction

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"late/internal/common"
)

// DefaultMaxSegChars caps one segment's size in bytes. It mirrors the
// jev-compaction default: large enough to keep a paragraph's context together
// for the scorer, small enough that one giant tool dump does not become one
// giant undifferentiated segment.
const DefaultMaxSegChars = 1200

// defaultMinSegChars is the size under which a paragraph is "tiny": a tiny
// paragraph is folded into an adjacent larger paragraph instead of being
// scored on its own, so a stray one-liner next to substantial output does
// not become its own segment. Runs of small paragraphs stay separate.
const defaultMinSegChars = 80

// Segment is one scored piece of a tool output.
//
// StartByte/EndByte are byte offsets into the ORIGINAL string (EndByte
// exclusive) with the invariant original[StartByte:EndByte] == Text. Each
// span includes the paragraph's trailing blank-line separator when one
// follows it, so the spans tile the input: concatenating the kept segments'
// Text reproduces the original output byte-for-byte minus the elided spans.
// (Blank bytes before the first paragraph belong to no segment.)
type Segment struct {
	ID        string
	Text      string
	StartByte int
	EndByte   int
	Tokens    int
	// Kind is the segment's content classification (classifyKind), computed
	// at segmentation time. The gate consults it for protected-kind floors:
	// stacktrace and diff segments are only elided below their own, much
	// lower, floor.
	Kind SegmentKind
	// LineStart/LineEnd are the 1-based [first, last] line numbers the
	// segment's span occupies in the segmented string (the reference's
	// line_span) — the numbers a pointer for this segment carries. Computed
	// at segmentation time from the byte offsets; 0/0 never occurs for
	// segments from SegmentSegments.
	LineStart int
	LineEnd   int
}

// SegmentKind classifies a segment's content so the gate can treat kinds
// differently (the reference pipeline's protected_kinds). The zero value is
// meaningless — SegmentSegments always sets one of the Kind* constants.
type SegmentKind string

const (
	// KindText is prose: anything that is none of the more specific kinds.
	KindText SegmentKind = "text"
	// KindJSON is a parseable JSON document (object or array).
	KindJSON SegmentKind = "json"
	// KindLog is timestamped/severity-prefixed log output.
	KindLog SegmentKind = "log"
	// KindStacktrace is a panic or exception trace. Protected: traces are
	// dense in signal and cheap in tokens, so they survive almost any score.
	KindStacktrace SegmentKind = "stacktrace"
	// KindTable is pipe-delimited tabular data.
	KindTable SegmentKind = "table"
	// KindCode is a fenced code block.
	KindCode SegmentKind = "code"
	// KindDiff is unified-diff output. Protected: a dropped hunk silently
	// corrupts everything built on top of it.
	KindDiff SegmentKind = "diff"
)

// Classification patterns. Compiled once; all anchored per trimmed line so
// leading indentation (Java's "\tat com...") and CRLF endings never hide a
// match.
var (
	// stackFrameLineRe matches V8/Java-style frames: "at pkg.File(Tool.go:42)"
	// and "at com.example.Foo.bar(Foo.java:99)".
	stackFrameLineRe = regexp.MustCompile(`^at .+\(.+:\d+\)`)
	// goroutineHeaderRe matches Go panic headers: "goroutine 1 [running]:"
	// and "goroutine 17 [signal SIGSEGV: ...]".
	goroutineHeaderRe = regexp.MustCompile(`^goroutine \d+ \[`)
	// iso8601LogLineRe matches a leading ISO-8601-ish timestamp, with a "T"
	// or space separator: "2024-01-02T15:04:05Z", "2024-01-02 15:04:05,123".
	iso8601LogLineRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`)
	// bracketedTimeLogLineRe matches syslog-style prefixes: "[12:34:56]".
	bracketedTimeLogLineRe = regexp.MustCompile(`^\[\d{1,2}:\d{2}:\d{2}\]`)
	// logLevelLineRe matches a leading severity word: "ERROR ", "WARN:",
	// "INFO", "DEBUG ...".
	logLevelLineRe = regexp.MustCompile(`^(WARN|ERROR|INFO|DEBUG)\b`)
)

// classifyKind guesses a segment's content kind with cheap, deterministic
// heuristics, most specific first (ported from the reference pipeline):
//
//  1. a fenced ``` code block → code (the fence wins over the fenced
//     content's own appearance, which may look like logs or diffs),
//  2. `diff --git` / `+++ ` / `@@ -` lines → diff,
//  3. "Traceback (most recent call last)", exception names, "at f(x.go:1)"
//     frames, or "goroutine N [...]" headers → stacktrace,
//  4. trimmed text opening with { or [ that parses as JSON → json (the
//     validity check keeps "[TODO] fix the parser" prose out),
//  5. a majority of lines with timestamp or severity prefixes → log,
//  6. a majority of lines pipe rows with one shared column count → table,
//  7. anything else → text.
func classifyKind(text string) SegmentKind {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return KindText
	}
	lines := strings.Split(trimmed, "\n")

	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			return KindCode
		}
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "diff --git") || strings.HasPrefix(t, "+++ ") || strings.HasPrefix(t, "@@ -") {
			return KindDiff
		}
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.Contains(t, "Traceback (most recent call last)") ||
			strings.Contains(t, "Exception") ||
			stackFrameLineRe.MatchString(t) ||
			goroutineHeaderRe.MatchString(t) {
			return KindStacktrace
		}
	}
	if trimmed[0] == '{' || trimmed[0] == '[' {
		if json.Valid([]byte(trimmed)) {
			return KindJSON
		}
	}
	if lineMajority(lines, looksLikeLogLine) {
		return KindLog
	}
	if looksLikeTable(lines) {
		return KindTable
	}
	return KindText
}

// looksLikeLogLine reports whether one line carries a log timestamp or
// severity prefix.
func looksLikeLogLine(line string) bool {
	t := strings.TrimSpace(line)
	return iso8601LogLineRe.MatchString(t) ||
		bracketedTimeLogLineRe.MatchString(t) ||
		logLevelLineRe.MatchString(t)
}

// lineMajority reports whether strictly more than half of the non-blank
// lines satisfy match. A majority (not a single hit) keeps one timestamp
// mention inside prose from reclassifying the paragraph around it.
func lineMajority(lines []string, match func(string) bool) bool {
	total, hits := 0, 0
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		total++
		if match(t) {
			hits++
		}
	}
	return total > 0 && hits*2 > total
}

// looksLikeTable reports whether a majority of the non-blank lines are table
// rows — lines containing "|" with one shared column count — and at least two
// such rows exist (a single line mentioning a pipe is prose, not a table).
func looksLikeTable(lines []string) bool {
	total, rows, columns := 0, 0, -1
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		total++
		if !strings.Contains(t, "|") {
			continue
		}
		cols := strings.Count(t, "|")
		if columns >= 0 && cols != columns {
			return false // inconsistent columns: not a table
		}
		columns = cols
		rows++
	}
	return rows >= 2 && rows*2 > total
}

// SegmentSegments splits toolOutput into segments at paragraph boundaries
// (blank lines), merging tiny paragraphs and capping each segment at
// maxSegChars bytes (DefaultMaxSegChars when maxSegChars <= 0). It returns
// nil for empty or whitespace-only output.
func SegmentSegments(toolOutput string, maxSegChars int) []Segment {
	if maxSegChars <= 0 {
		maxSegChars = DefaultMaxSegChars
	}
	paras := splitParagraphs(toolOutput)
	if len(paras) == 0 {
		return nil
	}

	var segs []Segment
	n := 0
	// newlines counts the '\n' bytes in toolOutput[:cursor]; pieces arrive
	// in offset order, so line numbers come from one linear pass. (The
	// reference carries the same information as line_span.)
	cursor, newlines := 0, 0
	for _, sp := range mergeParagraphs(paras, toolOutput, maxSegChars) {
		for _, piece := range splitOversized(toolOutput, sp, maxSegChars) {
			n++
			text := toolOutput[piece.start:piece.end]
			newlines += strings.Count(toolOutput[cursor:piece.start], "\n")
			lineStart := newlines + 1
			newlines += strings.Count(toolOutput[piece.start:piece.end], "\n")
			// The last byte of the span terminates its line when it is a
			// newline; either way the span ends on that line.
			lineEnd := newlines + 1
			if toolOutput[piece.end-1] == '\n' {
				lineEnd--
			}
			cursor = piece.end
			segs = append(segs, Segment{
				ID:        fmt.Sprintf("seg-%d", n),
				Text:      text,
				StartByte: piece.start,
				EndByte:   piece.end,
				Tokens:    common.EstimateTokenCount(text),
				Kind:      classifyKind(text),
				LineStart: lineStart,
				LineEnd:   lineEnd,
			})
		}
	}
	return segs
}

// span is a half-open byte range [start, end) into the segmented string.
type span struct {
	start int
	end   int
}

// splitParagraphs cuts s into paragraphs: maximal runs of non-blank lines.
// A blank line is a line that is empty or whitespace-only (\r\n tolerant).
// The span of a paragraph starts at its first content byte and ends after
// the blank-line separator that follows it (or at EOF for trailing
// whitespace), so consecutive spans tile the string from the first content
// byte onward.
func splitParagraphs(s string) []span {
	var out []span
	i, n := 0, len(s)
	for i < n {
		// Skip blank lines (this also skips leading blanks, which belong to
		// no paragraph).
		i = skipBlankLines(s, i)
		if i >= n {
			break
		}
		start := i
		// Consume content lines until a blank line or EOF.
		for i < n {
			lineEnd, next := lineBounds(s, i)
			if strings.TrimSpace(s[i:lineEnd]) == "" {
				break
			}
			i = next
		}
		// Absorb the trailing blank-line separator into this paragraph's
		// span so the spans tile the input.
		end := i
		for i < n {
			lineEnd, next := lineBounds(s, i)
			if strings.TrimSpace(s[i:lineEnd]) != "" {
				break
			}
			i = next
			end = i
		}
		out = append(out, span{start, end})
	}
	return out
}

// lineBounds returns the end (exclusive, before the '\n') and the start of
// the following line for the line beginning at i.
func lineBounds(s string, i int) (lineEnd, next int) {
	if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
		return i + j, i + j + 1
	}
	return len(s), len(s)
}

// skipBlankLines advances i past blank lines and returns the new offset.
func skipBlankLines(s string, i int) int {
	for i < len(s) {
		lineEnd, next := lineBounds(s, i)
		if strings.TrimSpace(s[i:lineEnd]) != "" {
			return i
		}
		i = next
	}
	return i
}

// mergeParagraphs folds tiny paragraphs into their large neighbors: adjacent
// paragraphs merge when exactly one of them is tiny (smaller than
// defaultMinSegChars), so a stray one-liner next to substantial content does
// not become its own scored segment, while runs of small paragraphs — and
// runs of large ones — keep their separate identities. Merges chain greedily
// from each paragraph and are only taken while the combined span still fits
// maxSegChars. Lengths are measured in bytes including the absorbed
// separators.
func mergeParagraphs(paras []span, s string, maxSegChars int) []span {
	if len(paras) <= 1 {
		return paras
	}
	tiny := func(sp span) bool { return sp.end-sp.start < defaultMinSegChars }
	merged := make([]span, 0, len(paras))
	i := 0
	for i < len(paras) {
		// Grow the group starting at i across adjacent paragraphs: each step
		// straddles a tiny/large boundary and must keep the combined span
		// within the cap.
		j := i + 1
		for j < len(paras) &&
			tiny(paras[j-1]) != tiny(paras[j]) &&
			len(s[paras[i].start:paras[j].end]) <= maxSegChars {
			j++
		}
		merged = append(merged, span{paras[i].start, paras[j-1].end})
		i = j
	}
	return merged
}

// splitOversized cuts a merged span longer than maxSegChars into pieces of at
// most maxSegChars bytes, cutting at the last newline inside the window when
// one exists and otherwise at a rune-safe hard boundary.
func splitOversized(s string, sp span, maxSegChars int) []span {
	if sp.end-sp.start <= maxSegChars {
		return []span{sp}
	}
	var out []span
	start := sp.start
	for start < sp.end {
		if sp.end-start <= maxSegChars {
			out = append(out, span{start, sp.end})
			break
		}
		window := start + maxSegChars
		cut := window
		if idx := strings.LastIndexByte(s[start:window], '\n'); idx >= 0 {
			// Keep the newline with the left piece.
			cut = start + idx + 1
		} else {
			// Hard cut that never splits a multi-byte rune.
			for cut > start && !utf8.RuneStart(s[cut]) {
				cut--
			}
		}
		if cut <= start {
			cut = start + maxSegChars // paranoia; cannot happen
		}
		out = append(out, span{start, cut})
		start = cut
	}
	return out
}
