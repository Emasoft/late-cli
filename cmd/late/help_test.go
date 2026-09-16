package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

// newHelpTestFlagSet mirrors main()'s root flag registrations (names and
// kinds only) on an isolated FlagSet so rendering can be tested without
// running main().
func newHelpTestFlagSet(t *testing.T) *flag.FlagSet {
	t.Helper()
	fs := flag.NewFlagSet("help-test", flag.ContinueOnError)
	bools := []string{
		"help", "version", "continue", "show-cwd", "inject-cwd", "gemma-thinking",
		"suppress-thinking-words", "save-subagent-histories", "enable-sqz",
		"i-promise-i-have-backups-and-will-not-file-issues",
		"force-revaluate-dangerous-commands", "enable-images",
		"use-tools", "enable-bash", "enable-subagents",
	}
	for _, name := range bools {
		def := name == "use-tools" || name == "enable-bash" || name == "enable-subagents"
		fs.Bool(name, def, "usage of "+name)
	}
	strs := []string{"system-prompt", "system-prompt-file", "append-system-prompt", "theme", "prompt", "logit-bias", "subagent-logit-bias"}
	for _, name := range strs {
		fs.String(name, "", "usage of "+name)
	}
	fs.Int("subagent-max-turns", 500, "usage of subagent-max-turns")
	fs.Int("max-stream-retries", 100, "usage of max-stream-retries")
	return fs
}

// countRenderedFlagLines counts output lines that render the flag `name`:
// a line starting with "  -" whose flag-name token (up to the first space
// or tab) equals name exactly. Token-boundary matching keeps "-system-prompt"
// from being miscounted against the "-system-prompt-file" line, which a
// plain substring count ("  -"+name) would wrongly attribute to both.
func countRenderedFlagLines(out, name string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, "  -")
		if !ok {
			continue
		}
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			rest = rest[:i]
		}
		if rest == name {
			n++
		}
	}
	return n
}

func TestWriteGroupedFlagsCoversAllGroupedFlagsOnce(t *testing.T) {
	var buf bytes.Buffer
	writeGroupedFlags(&buf, newHelpTestFlagSet(t))
	out := buf.String()
	for _, g := range flagGroups {
		for _, name := range g.flags {
			if n := countRenderedFlagLines(out, name); n != 1 {
				t.Errorf("flag -%s rendered %d times, want exactly 1", name, n)
			}
		}
	}
	// Headings appear, in the declared order.
	last := -1
	for _, g := range flagGroups {
		idx := strings.Index(out, g.heading+":")
		if idx < 0 {
			t.Fatalf("missing heading %q in output:\n%s", g.heading, out)
		}
		if idx < last {
			t.Errorf("heading %q appears out of order", g.heading)
		}
		last = idx
	}
	if strings.Contains(out, "Other:") {
		t.Errorf("all grouped flags were listed, unexpected Other section:\n%s", out)
	}
	// Boolean flags must not render a value name; string flags render "string".
	if !strings.Contains(out, "  -enable-bash\n") {
		t.Errorf("bool flag should render with no value name:\n%s", out)
	}
	if !strings.Contains(out, "  -system-prompt string") {
		t.Errorf("string flag should render with 'string' value name:\n%s", out)
	}
	if !strings.Contains(out, "(default 500)") {
		t.Errorf("expected '(default 500)' for subagent-max-turns:\n%s", out)
	}
}

func TestWriteGroupedFlagsUncategorizedFallToOther(t *testing.T) {
	fs := newHelpTestFlagSet(t)
	fs.String("zzz-future-flag", "", "a flag added later and not yet grouped")
	var buf bytes.Buffer
	writeGroupedFlags(&buf, fs)
	out := buf.String()
	if !strings.Contains(out, "Other:") || !strings.Contains(out, "  -zzz-future-flag") {
		t.Fatalf("ungrouped flag must still be rendered under Other:\n%s", out)
	}
}

func TestWriteHelpSections(t *testing.T) {
	var buf bytes.Buffer
	writeHelp(&buf, newHelpTestFlagSet(t))
	out := buf.String()
	for _, want := range []string{
		"Usage:", "Commands:", "Flags:",
		"session list [-v]", "session load <id>", "session delete <id>",
		"plugin list | ls", "plugin install | i", "plugin remove | rm | uninstall",
		"plugin update [<name>]", "worktree create <path> [branch]", "worktree active",
		"force-revaluate-dangerous-commands",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("writeHelp output missing %q", want)
		}
	}
}
