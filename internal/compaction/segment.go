package compaction

import (
	"fmt"
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
	for _, sp := range mergeParagraphs(paras, toolOutput, maxSegChars) {
		for _, piece := range splitOversized(toolOutput, sp, maxSegChars) {
			n++
			text := toolOutput[piece.start:piece.end]
			segs = append(segs, Segment{
				ID:        fmt.Sprintf("seg-%d", n),
				Text:      text,
				StartByte: piece.start,
				EndByte:   piece.end,
				Tokens:    common.EstimateTokenCount(text),
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
