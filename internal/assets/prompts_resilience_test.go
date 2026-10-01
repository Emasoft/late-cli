package assets

import (
	"io/fs"
	"strings"
	"testing"
)

// TestEveryDefaultPromptCarriesResilienceRules pins the subagent-resilience
// contract: the root orchestrator prompt AND every subagent prompt reachable
// through GetSubagents() must contain the "Subagent resilience &
// checkpointing" rules block, so every agent — parent or spawned child —
// commits verified milestones and resumes by delta after an interruption.
// A new prompt file or subagent config that forgets the block fails here.
func TestEveryDefaultPromptCarriesResilienceRules(t *testing.T) {
	const marker = "Subagent resilience & checkpointing"

	// The root orchestrator prompt: the parent that spawns subagents and
	// therefore carries the full rule (commit every verified milestone).
	root, err := PromptsFS.ReadFile("prompts/instruction-orchestrator.md")
	if err != nil {
		t.Fatalf("read the root orchestrator prompt: %v", err)
	}
	if !strings.Contains(string(root), marker) {
		t.Error("the root orchestrator prompt (prompts/instruction-orchestrator.md) is missing the subagent resilience & checkpointing rules block")
	}
	if !strings.Contains(string(root), "Commit every verified milestone") {
		t.Error("the root orchestrator prompt carries the block heading but not the milestone-commit rule")
	}

	// Every registered subagent: walk the embedded prompt files each
	// config references, so a future subagent without the block is caught
	// even if its prompt file is new.
	configs := GetSubagents()
	if len(configs) == 0 {
		t.Fatal("GetSubagents() returned no subagent configs; the embedded registry is broken")
	}
	for _, cfg := range configs {
		if cfg.PromptFile == "" {
			t.Errorf("subagent %q has no prompt_file; it cannot carry the resilience rules", cfg.Name)
			continue
		}
		data, err := PromptsFS.ReadFile(cfg.PromptFile)
		if err != nil {
			t.Errorf("subagent %q prompt file %s: %v", cfg.Name, cfg.PromptFile, err)
			continue
		}
		if !strings.Contains(string(data), marker) {
			t.Errorf("subagent %q prompt (%s) is missing the subagent resilience & checkpointing rules block", cfg.Name, cfg.PromptFile)
		}
	}
}

// TestEveryEmbeddedPromptFileCarriesResilienceRules is the belt to the
// registry walk's braces: even a prompt file no subagent config references
// must carry the block, because the default-prompt surface is the three
// embedded instruction-*.md files.
func TestEveryEmbeddedPromptFileCarriesResilienceRules(t *testing.T) {
	const marker = "Subagent resilience & checkpointing"
	entries, err := fs.ReadDir(PromptsFS, "prompts")
	if err != nil {
		t.Fatalf("read the embedded prompts directory: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no prompt files embedded under prompts/")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := PromptsFS.ReadFile("prompts/" + entry.Name())
		if err != nil {
			t.Errorf("read %s: %v", entry.Name(), err)
			continue
		}
		if !strings.Contains(string(data), marker) {
			t.Errorf("prompt file %s is missing the subagent resilience & checkpointing rules block", entry.Name())
		}
	}
}
