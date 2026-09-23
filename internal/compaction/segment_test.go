package compaction

import (
	"fmt"
	"strings"
	"testing"
)

// assertOffsetsInvariant checks the Segment contract: original[s.StartByte:
// s.EndByte] == s.Text for every segment, plus tokens and ID numbering.
func assertSegmentsInvariant(t *testing.T, original string, segs []Segment) {
	t.Helper()
	for i, s := range segs {
		if got := original[s.StartByte:s.EndByte]; got != s.Text {
			t.Errorf("segment %d (%s): original[%d:%d] = %q, want Text %q", i, s.ID, s.StartByte, s.EndByte, got, s.Text)
		}
		if s.EndByte < s.StartByte {
			t.Errorf("segment %d (%s): EndByte %d < StartByte %d", i, s.ID, s.EndByte, s.StartByte)
		}
		if s.Tokens <= 0 {
			t.Errorf("segment %d (%s): Tokens = %d, want > 0 for non-empty text", i, s.ID, s.Tokens)
		}
		if want := fmt.Sprintf("seg-%d", i+1); s.ID != want {
			t.Errorf("segment %d ID = %q, want %q", i, s.ID, want)
		}
	}
	for i := 1; i < len(segs); i++ {
		if segs[i].StartByte < segs[i-1].EndByte {
			t.Errorf("segment %d starts at %d before segment %d ends at %d (overlapping spans)",
				i, segs[i].StartByte, i-1, segs[i-1].EndByte)
		}
	}
}

func TestSegmentSegments_ParagraphSplitting(t *testing.T) {
	out := "first paragraph\n\nsecond paragraph\n\nthird paragraph"
	segs := SegmentSegments(out, 0)
	if len(segs) != 3 {
		t.Fatalf("got %d segments, want 3: %+v", len(segs), segs)
	}
	for i, want := range []string{"first paragraph\n\n", "second paragraph\n\n", "third paragraph"} {
		if segs[i].Text != want {
			t.Errorf("segment %d Text = %q, want %q (trailing separator included)", i, segs[i].Text, want)
		}
	}
	assertSegmentsInvariant(t, out, segs)

	// The spans tile the input from the first content byte: concatenating
	// every segment reproduces the output.
	var joined strings.Builder
	for _, s := range segs {
		joined.WriteString(s.Text)
	}
	if joined.String() != out {
		t.Errorf("concatenated segments = %q, want original %q", joined.String(), out)
	}
}

func TestSegmentSegments_TinyParagraphsMerged(t *testing.T) {
	// A tiny paragraph between two larger ones is absorbed forward.
	out := strings.Repeat("x", 100) + "\n\ntiny\n\n" + strings.Repeat("y", 100)
	segs := SegmentSegments(out, 0)
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1 (tiny paragraph merged forward)", len(segs))
	}

	// A trailing tiny paragraph is folded backward into the previous one.
	out = strings.Repeat("x", 100) + "\n\ntiny"
	segs = SegmentSegments(out, 0)
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1 (trailing tiny paragraph folded back)", len(segs))
	}
	if segs[0].Text != out {
		t.Errorf("merged Text = %q, want the full output %q", segs[0].Text, out)
	}
	assertSegmentsInvariant(t, out, segs)
}

func TestSegmentSegments_TinyParagraphNotMergedWhenTooBig(t *testing.T) {
	// Merging would blow the cap, so the paragraphs stay separate: 1195
	// content bytes + the 2-byte separator + the 6-byte tiny paragraph
	// exceeds the 1200-byte cap.
	out := strings.Repeat("x", 1195) + "\n\ntiny"
	segs := SegmentSegments(out, 0)
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (merge would exceed the cap)", len(segs))
	}
	assertSegmentsInvariant(t, out, segs)
}

func TestSegmentSegments_SizeCapSplitsLargeParagraphs(t *testing.T) {
	// One giant paragraph with newlines inside: splits at newlines within
	// the 1200-byte window.
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, strings.Repeat(fmt.Sprint(i%10), 100))
	}
	out := strings.Join(lines, "\n") // 40*100 + 39 = 4039 bytes
	segs := SegmentSegments(out, 0)
	if len(segs) < 3 {
		t.Fatalf("got %d segments, want ≥3 for a %d-byte paragraph under the %d-byte cap",
			len(segs), len(out), DefaultMaxSegChars)
	}
	for i, s := range segs {
		if len(s.Text) > DefaultMaxSegChars {
			t.Errorf("segment %d is %d bytes, want ≤%d", i, len(s.Text), DefaultMaxSegChars)
		}
	}
	assertSegmentsInvariant(t, out, segs)

	// Rune-safe hard splitting: a paragraph with no newlines at all.
	out = strings.Repeat("é", 3000) // 6000 bytes of 2-byte runes
	segs = SegmentSegments(out, 0)
	if len(segs) != 5 { // 6000 bytes / 1200 = 5 exactly (rune-aligned cap)
		t.Fatalf("got %d segments, want 5", len(segs))
	}
	var joined strings.Builder
	for _, s := range segs {
		if len(s.Text) > DefaultMaxSegChars {
			t.Errorf("segment %s is %d bytes, want ≤%d", s.ID, len(s.Text), DefaultMaxSegChars)
		}
		joined.WriteString(s.Text)
	}
	if joined.String() != out {
		t.Error("hard-split pieces do not reproduce the original output")
	}
	assertSegmentsInvariant(t, out, segs)
}

func TestSegmentSegments_ExplicitMaxSegChars(t *testing.T) {
	out := strings.Repeat("a", 500)
	if segs := SegmentSegments(out, 0); len(segs) != 1 {
		t.Fatalf("got %d segments, want 1 under the default cap", len(segs))
	}
	if segs := SegmentSegments(out, 200); len(segs) != 3 {
		t.Fatalf("got %d segments, want 3 under a 200-byte cap", len(segs))
	}
}

func TestSegmentSegments_DefaultCapMatchesConstant(t *testing.T) {
	out := strings.Repeat("a", 3000)
	a := SegmentSegments(out, 0)
	b := SegmentSegments(out, DefaultMaxSegChars)
	if len(a) != len(b) {
		t.Fatalf("maxSegChars 0 gave %d segments, explicit %d gave %d — 0 must mean the default",
			len(a), DefaultMaxSegChars, len(b))
	}
	for i := range a {
		if a[i].Text != b[i].Text {
			t.Errorf("segment %d differs between 0 and the default cap", i)
		}
	}
}

func TestSegmentSegments_EmptyAndWhitespace(t *testing.T) {
	for _, in := range []string{"", "\n\n\n", "   \n\t\n  \n"} {
		if segs := SegmentSegments(in, 0); segs != nil {
			t.Errorf("SegmentSegments(%q) = %+v, want nil", in, segs)
		}
	}
}

func TestSegmentSegments_LeadingBlanksAndCRLF(t *testing.T) {
	// Leading blank lines belong to no segment; CRLF blank lines still
	// separate paragraphs.
	out := "\n\n\r\nfirst\r\n\n\r\nsecond\n"
	segs := SegmentSegments(out, 0)
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2: %+v", len(segs), segs)
	}
	if segs[0].Text != "first\r\n\n\r\n" {
		t.Errorf("segment 0 Text = %q, want %q", segs[0].Text, "first\r\n\n\r\n")
	}
	if segs[1].Text != "second\n" {
		t.Errorf("segment 1 Text = %q, want %q", segs[1].Text, "second\n")
	}
	assertSegmentsInvariant(t, out, segs)
}

func TestSegmentSegments_WhitespaceOnlyLinesDoNotSplitParagraphs(t *testing.T) {
	// A line holding only spaces is blank (a separator), not content.
	out := "alpha\n   \nbeta"
	segs := SegmentSegments(out, 0)
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2 (whitespace-only line is a separator)", len(segs))
	}
	assertSegmentsInvariant(t, out, segs)
}

func TestSegmentSegments_MultiByteOffsets(t *testing.T) {
	// Byte offsets must stay byte-based (not rune-based) with multibyte
	// content before the split point.
	out := "日本語のテキスト\n\nsecond paragraph with ascii"
	segs := SegmentSegments(out, 0)
	if len(segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(segs))
	}
	assertSegmentsInvariant(t, out, segs)
	if segs[0].StartByte != 0 || segs[0].EndByte != len("日本語のテキスト\n\n") {
		t.Errorf("segment 0 span = [%d,%d), want [0,%d)", segs[0].StartByte, segs[0].EndByte, len("日本語のテキスト\n\n"))
	}
}
