package config

import (
	"os"
	"strings"
	"testing"
)

// mustParseModelsEntry parses one models[] entry (plus the agent_models
// routing that makes GetModelForAgent resolve it) and returns the entry.
func mustParseModelsEntry(t *testing.T, allowRaw string) ModelSetting {
	t.Helper()
	content := `{
  "models": [
    {
      "id": "local-llama",
      "url": "http://127.0.0.1:8080",
      "key": "",
      "model": "llama-3",
      "allow_parallel_execution": ` + allowRaw + `
    }
  ],
  "agent_models": {"coder": "local-llama"}
}`
	cfg, err := parseConfigContent("/x/config.json", []byte(content))
	if err != nil {
		t.Fatalf("parseConfigContent(%s) error = %v", allowRaw, err)
	}
	setting, ok := cfg.GetModelForAgent("coder")
	if !ok {
		t.Fatalf("GetModelForAgent(coder) not found for %s", allowRaw)
	}
	return setting
}

// TestModelSetting_AllowParallelExecutionDefaultsToFalse pins the ABSENT =
// FALSE rule: an entry without the key (and a config with no models at all)
// must never allow parallel execution — parallel is an explicit per-model
// opt-in.
func TestModelSetting_AllowParallelExecutionDefaultsToFalse(t *testing.T) {
	if setting := (ModelSetting{}); setting.AllowsParallelExecution() {
		t.Fatal("zero ModelSetting must not allow parallel execution")
	}

	content := `{
  "models": [{"id": "local-llama", "url": "http://127.0.0.1:8080", "key": "", "model": "llama-3"}],
  "agent_models": {"coder": "local-llama"}
}`
	cfg, err := parseConfigContent("/x/config.json", []byte(content))
	if err != nil {
		t.Fatalf("parseConfigContent() error = %v", err)
	}
	setting, ok := cfg.GetModelForAgent("coder")
	if !ok {
		t.Fatal("GetModelForAgent(coder) not found")
	}
	if setting.AllowsParallelExecution() {
		t.Fatal("absent allow_parallel_execution must not allow parallel execution")
	}
}

// TestModelSetting_AllowParallelExecutionSynonyms pins the strict parser's
// acceptance of the FlexBool synonyms on the new models[] key: the true side
// honors parallel execution, the false side (and a wrong-typed value) does
// not.
func TestModelSetting_AllowParallelExecutionSynonyms(t *testing.T) {
	trueCases := []string{`true`, `"on"`, `"yes"`, `"enabled"`, `1`, `"Y"`}
	for _, raw := range trueCases {
		setting := mustParseModelsEntry(t, raw)
		if !setting.AllowsParallelExecution() {
			t.Errorf("allow_parallel_execution %s: AllowsParallelExecution() = false, want true", raw)
		}
	}
	falseCases := []string{`false`, `"off"`, `"no"`, `"disabled"`, `0`, `"N"`}
	for _, raw := range falseCases {
		setting := mustParseModelsEntry(t, raw)
		if setting.AllowsParallelExecution() {
			t.Errorf("allow_parallel_execution %s: AllowsParallelExecution() = true, want false", raw)
		}
	}
}

// TestModelSetting_AllowParallelExecutionUnknownValueRejected pins the
// strict parser's rejection of a value outside the synonym table.
func TestModelSetting_AllowParallelExecutionUnknownValueRejected(t *testing.T) {
	content := `{
  "models": [{"id": "m", "url": "http://p:8080", "key": "k", "model": "x", "allow_parallel_execution": "maybe"}]
}`
	if _, err := parseConfigContent("/x/config.json", []byte(content)); err == nil {
		t.Fatal("expected a parse error for an unknown allow_parallel_execution value")
	} else if !strings.Contains(err.Error(), "not a valid boolean value") {
		t.Fatalf("error is not the FlexBool synonym-table error: %v", err)
	}
}

// TestModelSetting_AllowParallelExecutionRoundTrip pins the save side: a
// true flag persists as the plain `true` literal, reloads cleanly under the
// strict parser, and keeps honoring parallel execution; a false flag is
// omitted (omitempty, absent = false).
func TestModelSetting_AllowParallelExecutionRoundTrip(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	cfg.Models = []ModelSetting{{
		ID:                     "local-llama",
		URL:                    "http://127.0.0.1:8080",
		Key:                    "",
		Model:                  "llama-3",
		AllowParallelExecution: FlexBool(true),
	}}
	cfg.AgentModels = map[string]string{"coder": "local-llama"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	raw, err := os.ReadFile(lateConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"allow_parallel_execution": true`) {
		t.Fatalf("saved config must contain the plain true literal, got:\n%s", raw)
	}

	reloaded, err := LoadConfig()
	if err != nil {
		t.Fatalf("strict reload failed: %v", err)
	}
	setting, ok := reloaded.GetModelForAgent("coder")
	if !ok {
		t.Fatal("reloaded config lost the agent_models routing")
	}
	if !setting.AllowsParallelExecution() {
		t.Fatal("reloaded flag must still allow parallel execution")
	}

	// The false side is omitted: absent = false, so nothing is written.
	reloaded.Models[0].AllowParallelExecution = FlexBool(false)
	if err := SaveConfig(reloaded); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	raw, err = os.ReadFile(lateConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "allow_parallel_execution") {
		t.Fatalf("false flag must be omitted from the saved config, got:\n%s", raw)
	}
}
