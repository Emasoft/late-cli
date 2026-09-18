package tui

import (
	"regexp"
	"strings"
	"testing"
)

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRe.ReplaceAllString(s, "") }

func TestAgentTypeFromID(t *testing.T) {
	cases := map[string]string{
		"main":                  "orchestrator",
		"researcher-subagent-0": "researcher",
		"coder-subagent-12":     "coder",
		"planner-subagent-3":    "planner", // future category: new JSON config, no code change
		"mock":                  "mock",
		"":                      "",
	}
	for id, want := range cases {
		if got := agentTypeFromID(id); got != want {
			t.Errorf("agentTypeFromID(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestAgentTypeColorsAreDistinctAndStable(t *testing.T) {
	orch := agentTypeStyle("orchestrator").Render("orchestrator")
	res := agentTypeStyle("researcher").Render("researcher")
	code := agentTypeStyle("coder").Render("coder")
	if orch == res || res == code || orch == code {
		t.Fatalf("orchestrator, researcher and coder must use different bright colors")
	}
	if !strings.Contains(stripANSI(orch), "orchestrator") {
		t.Fatalf("label text lost: %q", orch)
	}
	if first, second := agentTypeStyle("future-type").Render("x"), agentTypeStyle("future-type").Render("x"); first != second {
		t.Fatalf("future categories must get a stable color")
	}
}

func TestStatusBarShowsAgentTypeBeforeBranch(t *testing.T) {
	model := NewModel(&mockOrchestrator{}, nil, nil)
	model.Focused = &focusTestOrchestrator{id: "main"}
	model.Width = 120
	model.ShowCWD = true
	model.CWD = "/tmp/repo"
	model.GitBranch = "feat/test"
	bar := stripANSI(model.statusBarView())
	if !strings.Contains(bar, "orchestrator") {
		t.Fatalf("status bar must show the agent category, got: %q", bar)
	}
	if strings.Index(bar, "orchestrator") > strings.Index(bar, "feat/test") {
		t.Fatalf("agent category must appear before the git branch, got: %q", bar)
	}
	if !strings.Contains(bar, "feat/test") {
		t.Fatalf("branch missing entirely: %q", bar)
	}
}

func TestStatusBarShowsAgentTypeWhenShowCWDDisabled(t *testing.T) {
	model := NewModel(&mockOrchestrator{}, nil, nil)
	model.Focused = &focusTestOrchestrator{id: "researcher-subagent-0"}
	model.Width = 120
	model.ShowCWD = false
	model.GitBranch = ""
	bar := stripANSI(model.statusBarView())
	if !strings.Contains(bar, "researcher") {
		t.Fatalf("agent category must be visible even with ShowCWD=false, got: %q", bar)
	}
}
