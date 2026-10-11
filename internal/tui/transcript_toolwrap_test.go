package tui

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"late/internal/client"
)

// toolWrapHistory builds a minimal user → assistant tool call → tool result
// conversation carrying toolContent as the tool result body.
func toolWrapHistory(toolContent string) []client.ChatMessage {
	return []client.ChatMessage{
		{Role: "user", Content: client.TextContent("run it")},
		{
			Role:    "assistant",
			Content: client.TextContent("Calling a tool."),
			ToolCalls: []client.ToolCall{{
				ID:       "call-wrap",
				Type:     "function",
				Function: client.FunctionCall{Name: "bash", Arguments: `{"command":"echo hi"}`},
			}},
		},
		{Role: "tool", ToolCallID: "call-wrap", Content: client.TextContent(toolContent)},
	}
}

// toolWrapTestModel renders a transcript of toolWrapHistory at the given
// viewport width and returns the model.
func toolWrapTestModel(t *testing.T, width int, toolContent string) *Model {
	t.Helper()
	m, _ := newViewportBenchmarkModel(toolWrapHistory(toolContent))
	m.SetSize(width, 40)
	renderTestTranscript(m)
	return m
}

// toolBodyPlain extracts the visible text of gutter-prefixed tool body rows:
// ANSI stripped, gutter and continuation indent removed, background padding
// trimmed. Non-body rows (headers, spacers) are dropped.
func toolBodyPlain(rows []string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		p := ansi.Strip(r)
		if !strings.HasPrefix(p, toolGutter) {
			continue
		}
		p = strings.TrimPrefix(p, toolGutter)
		p = strings.TrimPrefix(p, toolWrapIndent)
		out = append(out, strings.TrimRight(p, " "))
	}
	return out
}

// squash removes all whitespace. ansi.Wrap only drops whitespace at wrap
// break points, so the squashed rendering of a fully shown block contains the
// squashed source text: a lossless-wrap assertion that survives re-wrapping.
func squash(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// TestToolOutputTranscriptSoftWrapsAtViewportWidth is the bug regression: a
// 300-char single-line tool output at width 80 used to be hard-cut by
// padRow's ansi.Truncate, losing everything past the terminal edge. It must
// wrap into multiple rows carrying ALL of the content instead.
func TestToolOutputTranscriptSoftWrapsAtViewportWidth(t *testing.T) {
	line := strings.Repeat("0123456789", 30) // 300 unbroken chars
	m := toolWrapTestModel(t, 80, line)
	rows := m.GetAgentState(m.Focused.ID()).Transcript.rows
	body := toolBodyPlain(rows)
	if len(body) < 4 {
		t.Fatalf("300-char line must wrap into multiple rows at width 80, got %d body rows:\n%q", len(body), body)
	}
	if got, want := squash(strings.Join(body, " ")), squash(line); !strings.Contains(got, want) {
		t.Fatalf("wrapped rows must carry the full input: got %d chars, want %d chars\nrows: %q", len(got), len(want), body)
	}
	for _, row := range body {
		if w := ansi.StringWidth(toolGutter + row); w > 80 {
			t.Fatalf("body row %d cells exceeds viewport width 80: %q", w, row)
		}
	}
}

// TestToolOutputWrapsEachSourceLine checks word-aware wrapping of mixed
// content: short lines stay one row, long lines wrap, empty lines survive,
// and no character of any source line is lost.
func TestToolOutputWrapsEachSourceLine(t *testing.T) {
	content := "short line\n" + strings.Repeat("word ", 40) + "\n\nlast line"
	rows := toolBodyRows(content, 80)
	joined := strings.Join(toolBodyPlain(rows), "\n")
	for _, want := range []string{"short line", "last line"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in wrapped body:\n%s", want, joined)
		}
	}
	if got, want := squash(joined), squash(content); !strings.Contains(got, want) {
		t.Fatalf("squashed body must contain squashed source: got %q want %q", got, want)
	}
	// Leading whitespace of a source line must survive on its first row (the
	// body row carries the raw source indent; only the gutter is stripped).
	rows = toolBodyRows("    indented code line that is long enough to wrap around the block edge", 80)
	first := strings.TrimRight(strings.TrimPrefix(ansi.Strip(rows[0]), toolGutter), " ")
	if !strings.HasPrefix(first, "    indented code") {
		t.Fatalf("source leading indent must be preserved on the first row, got %q", first)
	}
}

// TestToolOutputCapShowsTailHint checks the row cap: at most maxToolBodyRows
// body rows plus the hint, the hint names the unshown line count, and the
// drop is never silent.
func TestToolOutputCapShowsTailHint(t *testing.T) {
	lines := make([]string, 0, 1000)
	for i := 0; i < 1000; i++ {
		lines = append(lines, fmt.Sprintf("TOOLLINE%04d", i))
	}
	content := strings.Join(lines, "\n")
	rows := toolBodyRows(content, 80)
	if len(rows) != maxToolBodyRows+1 {
		t.Fatalf("expected %d rows (cap + one hint row), got %d", maxToolBodyRows+1, len(rows))
	}
	hint := ansi.Strip(rows[len(rows)-1])
	wantHint := "…[output continues — 600 more lines]"
	if !strings.Contains(hint, wantHint) {
		t.Fatalf("hint must report the 600 unshown lines, got %q", hint)
	}
	body := strings.Join(toolBodyPlain(rows), "\n")
	if !strings.Contains(body, "TOOLLINE0000") || !strings.Contains(body, "TOOLLINE0399") {
		t.Fatal("lines within the cap must be fully rendered")
	}
	if strings.Contains(body, "TOOLLINE0400") {
		t.Fatal("lines beyond the cap must not be rendered")
	}
}

// TestToolOutputCapHintCarriesArchivePath checks that the hint surfaces the
// executor's archived-copy path when the content carries the archive marker.
func TestToolOutputCapHintCarriesArchivePath(t *testing.T) {
	archived := "/tmp/arch/abc123.txt" // short enough that the hint fits one row
	lines := make([]string, 0, maxToolBodyRows+50)
	for i := 0; i < maxToolBodyRows+50; i++ {
		lines = append(lines, fmt.Sprintf("LINE%04d", i))
	}
	lines = append(lines, "…[full output archived: "+archived+" — read_file it to view everything]")
	rows := toolBodyRows(strings.Join(lines, "\n"), 80)
	want := "…[output continues — " + strconv.Itoa(len(lines)-maxToolBodyRows) +
		" more lines; archived copy: " + archived + "]"
	if hint := ansi.Strip(rows[len(rows)-1]); !strings.Contains(hint, want) {
		t.Fatalf("hint must carry the archived copy path, want %q got %q", want, hint)
	}

	// A long archive path wraps the hint across rows; the path must still be
	// fully present when the wrapped rows are read in order.
	longArchived := "/tmp/sessions/s/tool-outputs/a-very-long-content-addressed-name-0123456789abcdef.txt"
	lines[len(lines)-1] = "…[full output archived: " + longArchived + " — read_file it to view everything]"
	rows = toolBodyRows(strings.Join(lines, "\n"), 80)
	body := squash(strings.Join(toolBodyPlain(rows), " "))
	if !strings.Contains(body, squash("; archived copy: "+longArchived)) {
		t.Fatalf("wrapped hint must still carry the full archive path, tail:\n%s",
			tailRows(strings.Join(toolBodyPlain(rows), "\n"), 3))
	}
}

// TestToolOutputWrapsWideUnicode checks runewidth/grapheme-aware wrapping:
// wide chars count as two cells, emoji and ZWJ sequences never split, and
// every grapheme survives.
func TestToolOutputWrapsWideUnicode(t *testing.T) {
	rocket := strings.Repeat("🚀", 60)     // 120 cells
	cjk := strings.Repeat("中文文本", 10)     // 80 cells
	family := strings.Repeat("👨‍👩‍👧", 10) // ZWJ sequences must stay whole
	line := rocket + " " + cjk + " " + family + " tail"
	limit, _ := toolWrapLimit(80)
	rows := toolBodyRows(line, 80)
	var sb strings.Builder
	for _, r := range rows {
		p := strings.TrimPrefix(ansi.Strip(r), toolGutter)
		p = strings.TrimPrefix(p, toolWrapIndent)
		p = strings.TrimRight(p, " ")
		if w := ansi.StringWidth(p); w > limit {
			t.Fatalf("row width %d exceeds wrap limit %d: %q", w, limit, p)
		}
		sb.WriteString(p)
	}
	out := sb.String()
	if got := strings.Count(out, "🚀"); got != 60 {
		t.Fatalf("emoji graphemes must survive wrapping intact: got %d/60", got)
	}
	if got := strings.Count(out, "👨‍👩‍👧"); got != 10 {
		t.Fatalf("ZWJ sequences must never split: got %d/10", got)
	}
	if !strings.Contains(out, "中文文本") || !strings.Contains(out, "tail") {
		t.Fatal("CJK text or tail lost by wrapping")
	}
}

// TestArchivedToolOutputPath unit-checks the marker parser against both
// executor marker forms and the no-marker case.
func TestArchivedToolOutputPath(t *testing.T) {
	p := "/tmp/s/tool-outputs/x.txt"
	short := "head\n…[full output archived: " + p + " — read_file it to view everything]"
	long := "…[output truncated: 5000 chars total. Head above / tail below. Full output archived: " + p + " — read_file it to view everything]\ntail"
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"no marker":    {in: "plain output", want: ""},
		"short marker": {in: short, want: p},
		"long marker":  {in: long, want: p},
	} {
		t.Run(name, func(t *testing.T) {
			if got := archivedToolOutputPath(tc.in); got != tc.want {
				t.Fatalf("archivedToolOutputPath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestToolOutputCapHintThroughTranscript runs the capped path through the
// full model render so the hint is proven on the integration surface too.
func TestToolOutputCapHintThroughTranscript(t *testing.T) {
	lines := make([]string, 0, maxToolBodyRows+10)
	for i := 0; i < maxToolBodyRows+10; i++ {
		lines = append(lines, fmt.Sprintf("INTEGRINE%04d", i))
	}
	m := toolWrapTestModel(t, 80, strings.Join(lines, "\n"))
	rows := m.GetAgentState(m.Focused.ID()).Transcript.rows
	body := strings.Join(toolBodyPlain(rows), "\n")
	if !strings.Contains(body, "…[output continues — 10 more lines]") {
		t.Fatalf("transcript must show the cap hint, got tail:\n%s", tailRows(body, 3))
	}
	if strings.Contains(body, "INTEGRINE"+strconv.Itoa(maxToolBodyRows)) {
		t.Fatal("rows beyond the cap must not be rendered")
	}
}

// tailRows returns the last n lines of s for compact failure messages.
func tailRows(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
