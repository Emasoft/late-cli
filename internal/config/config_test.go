package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_MissingFileCreatesDefault(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil config")
	}
	if !cfg.EnabledTools["read_file"] || !cfg.EnabledTools["bash"] {
		t.Fatalf("LoadConfig() missing default enabled tools: %#v", cfg.EnabledTools)
	}

	configPath := lateConfigPath(t)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected config file to be created at %s: %v", configPath, err)
	}
	if cfg.OpenAIBaseURL != "" || cfg.OpenAIAPIKey != "" || cfg.OpenAIModel != "" {
		t.Fatal("expected default OpenAI fields to be empty")
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Dir(configPath))
		if err != nil {
			t.Fatalf("failed to stat config directory: %v", err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("config dir permissions = %o, want %o", got, 0o700)
		}

		fileInfo, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("failed to stat config file: %v", err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("config file permissions = %o, want %o", got, 0o600)
		}
	}
}

func TestLoadConfig_ExistingFileTightensPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not reliably comparable on Windows")
	}

	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)
	configPath := lateConfigPath(t)
	configDir := filepath.Dir(configPath)

	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"enabled_tools":{"bash":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil config")
	}

	dirInfo, err := os.Stat(configDir)
	if err != nil {
		t.Fatalf("failed to stat config directory: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("config dir permissions = %o, want %o", got, 0o700)
	}

	fileInfo, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("failed to stat config file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("config file permissions = %o, want %o", got, 0o600)
	}
}

func TestLoadConfig_ParsesLegacyConfig(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)
	configPath := lateConfigPath(t)

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"enabled_tools":{"bash":false,"read_file":true}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.EnabledTools["bash"] {
		t.Fatal("expected bash to be disabled from legacy config")
	}
	if !cfg.EnabledTools["read_file"] {
		t.Fatal("expected read_file to remain enabled from legacy config")
	}
	if cfg.OpenAIBaseURL != "" || cfg.OpenAIAPIKey != "" || cfg.OpenAIModel != "" {
		t.Fatal("expected legacy config to leave OpenAI fields empty")
	}
}

func TestLoadConfig_ParsesOpenAIFields(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)
	configPath := lateConfigPath(t)

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatal(err)
	}
	content := `{
		"enabled_tools": {"bash": true},
		"openai_base_url": "https://example.test/v1",
		"openai_api_key": "secret",
		"openai_model": "gpt-test",
		"late_subagent_base_url": "https://subagent.example/v1",
		"late_subagent_api_key": "sub-secret",
		"late_subagent_model": "qwen-sub"
	}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.OpenAIBaseURL != "https://example.test/v1" {
		t.Fatalf("OpenAIBaseURL = %q", cfg.OpenAIBaseURL)
	}
	if cfg.OpenAIAPIKey != "secret" {
		t.Fatalf("OpenAIAPIKey = %q", cfg.OpenAIAPIKey)
	}
	if cfg.OpenAIModel != "gpt-test" {
		t.Fatalf("OpenAIModel = %q", cfg.OpenAIModel)
	}
	if cfg.LateSubagentBaseURL != "https://subagent.example/v1" {
		t.Fatalf("LateSubagentBaseURL = %q", cfg.LateSubagentBaseURL)
	}
	if cfg.LateSubagentAPIKey != "sub-secret" {
		t.Fatalf("LateSubagentAPIKey = %q", cfg.LateSubagentAPIKey)
	}
	if cfg.LateSubagentModel != "qwen-sub" {
		t.Fatalf("LateSubagentModel = %q", cfg.LateSubagentModel)
	}
}

func TestLoadConfig_OpenAIOnlyConfigDefaultsEnabledTools(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)
	configPath := lateConfigPath(t)

	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `{
		"openai_base_url": "https://example.test/v1",
		"openai_api_key": "secret",
		"openai_model": "gpt-test"
	}`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig() returned nil config")
	}

	if cfg.OpenAIBaseURL != "https://example.test/v1" {
		t.Fatalf("OpenAIBaseURL = %q", cfg.OpenAIBaseURL)
	}
	if cfg.OpenAIAPIKey != "secret" {
		t.Fatalf("OpenAIAPIKey = %q", cfg.OpenAIAPIKey)
	}
	if cfg.OpenAIModel != "gpt-test" {
		t.Fatalf("OpenAIModel = %q", cfg.OpenAIModel)
	}

	if cfg.EnabledTools == nil {
		t.Fatal("EnabledTools is nil")
	}

	for toolName, wantEnabled := range defaultConfig().EnabledTools {
		gotEnabled, ok := cfg.EnabledTools[toolName]
		if !ok {
			t.Fatalf("expected default tool %q to be present", toolName)
		}
		if gotEnabled != wantEnabled {
			t.Fatalf("EnabledTools[%q] = %v, want %v", toolName, gotEnabled, wantEnabled)
		}
	}
}

// TestLoadConfig_StrictErrorsAbort pins the strict-config startup rule: any
// config.json content problem — JSON syntax error, unknown top-level entry,
// wrong-typed value, invalid enum value, invalid boolean synonym — is fatal.
// LoadConfig returns the rendered line/column error together with a NIL
// config so main can print it and exit instead of silently starting on
// fallback defaults.
func TestLoadConfig_StrictErrorsAbort(t *testing.T) {
	cases := []struct {
		name           string
		configContent  string
		wantErrParts   []string
		wantNilCfg     bool
		wantPositioned bool
	}{
		{
			name:          "valid config parses without error",
			configContent: `{"enabled_tools":{"bash":true},"openai_model":"gpt-test"}`,
			wantNilCfg:    false,
		},
		{
			name:           "syntax error is fatal and positioned",
			configContent:  `{"enabled_tools":{"bash":true},}`,
			wantErrParts:   []string{"error in ", "at line 1", "column", "invalid character ','"},
			wantNilCfg:     true,
			wantPositioned: true,
		},
		{
			name:           "unknown key names the entry and suggests the closest",
			configContent:  `{"compaction_mode": true}`,
			wantErrParts:   []string{"error in ", `"compaction_mode" is not a valid config.json entry`, `Did you mean "compaction-mode"?`},
			wantNilCfg:     true,
			wantPositioned: true,
		},
		{
			name:           "wrong-typed value is fatal and positioned",
			configContent:  `{"compaction-threshold-percent":"many"}`,
			wantErrParts:   []string{`"compaction-threshold-percent" must be a number, found a string`},
			wantNilCfg:     true,
			wantPositioned: true,
		},
		{
			name:           "invalid enum value is fatal and positioned",
			configContent:  `{"compaction-mode":"shado"}`,
			wantErrParts:   []string{`"shado" is not a valid compaction-mode value`, `Did you mean "shadow"?`},
			wantNilCfg:     true,
			wantPositioned: true,
		},
		{
			name:           "invalid boolean synonym is fatal and positioned",
			configContent:  `{"use-tools":"actve"}`,
			wantErrParts:   []string{`"actve" is not a valid boolean value for "use-tools"`, "Accepted values are:"},
			wantNilCfg:     true,
			wantPositioned: true,
		},
		{
			// A formerly permissive case: an unknown entry is no longer
			// silently ignored.
			name:           "unknown extra field is rejected",
			configContent:  `{"totally-new-option":123}`,
			wantErrParts:   []string{`"totally-new-option" is not a valid config.json entry`},
			wantNilCfg:     true,
			wantPositioned: true,
		},
		{
			// A formerly wrong-typed case: "yes" is now a valid boolean
			// synonym for the FlexBool entries.
			name:           "boolean synonym yes parses",
			configContent:  `{"jev-autocompact":"yes","save_subagent_histories":1}`,
			wantErrParts:   nil,
			wantNilCfg:     false,
			wantPositioned: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configRoot := t.TempDir()
			setUserConfigEnv(t, configRoot)
			configPath := lateConfigPath(t)

			if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, []byte(tc.configContent), 0o644); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadConfig()
			if tc.wantNilCfg {
				if err == nil {
					t.Fatal("LoadConfig() expected a fatal error, got nil")
				}
				if cfg != nil {
					t.Fatalf("LoadConfig() returned a config alongside a fatal error — strict config must not fall back: %#v", cfg)
				}
				if !strings.HasPrefix(err.Error(), "error in ") || !strings.Contains(err.Error(), configPath) {
					t.Fatalf("LoadConfig() error = %q, want it to name the config path %q", err.Error(), configPath)
				}
				for _, part := range tc.wantErrParts {
					if !strings.Contains(err.Error(), part) {
						t.Fatalf("LoadConfig() error = %q, want it to contain %q", err.Error(), part)
					}
				}
				if tc.wantPositioned {
					var parseErr *ConfigParseError
					if !errors.As(err, &parseErr) {
						t.Fatalf("LoadConfig() error = %T, want *ConfigParseError", err)
					}
					if !strings.Contains(err.Error(), "at line ") || !strings.Contains(err.Error(), "column ") {
						t.Fatalf("LoadConfig() error = %q, want a line/column position", err.Error())
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadConfig() error = %v, want nil", err)
			}
			if cfg == nil {
				t.Fatal("LoadConfig() returned nil config")
			}
		})
	}
}

// TestLoadConfig_UnreadableFileIsFatal pins that an existing-but-unreadable
// config.json (here: the config path is a directory) aborts with an error
// naming the path and a nil config — starting on fallback defaults would
// silently ignore the user's real settings.
func TestLoadConfig_UnreadableFileIsFatal(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)
	configPath := lateConfigPath(t)

	if err := os.MkdirAll(configPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error when config path is a directory")
	}
	if cfg != nil {
		t.Fatal("expected nil config on a fatal load error")
	}
	if !strings.Contains(err.Error(), configPath) {
		t.Fatalf("LoadConfig() error = %q, want it to contain the config path %q", err.Error(), configPath)
	}
}

// TestLoadConfig_DefaultCreateFailureIsFatal pins that a fresh install that
// cannot write its default config.json aborts with an error and a nil
// config instead of starting on in-memory defaults.
func TestLoadConfig_DefaultCreateFailureIsFatal(t *testing.T) {
	configRoot := t.TempDir()
	blockingPath := filepath.Join(configRoot, "not-a-dir")
	if err := os.WriteFile(blockingPath, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	setUserConfigEnv(t, blockingPath)

	cfg, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error when config directory cannot be created")
	}
	if cfg != nil {
		t.Fatal("expected nil config on a fatal load error")
	}
}

// TestSaveConfig_RefusesDegradedConfig pins the defense-in-depth guard: the
// Degraded flag is never set by LoadConfig anymore (strict config aborts
// instead), but if a caller ever hands a degraded config to SaveConfig it
// must refuse so defaults cannot clobber the user's hand-edited config.json.
func TestSaveConfig_RefusesDegradedConfig(t *testing.T) {
	degraded := &Config{Degraded: true}
	if err := SaveConfig(degraded); err == nil {
		t.Fatal("SaveConfig() expected a refusal error for a degraded config, got nil")
	} else if !strings.Contains(err.Error(), "refusing to save config") {
		t.Fatalf("SaveConfig() error = %q, want it to mention refusing to save", err.Error())
	}
}

func setUserConfigEnv(t *testing.T, configRoot string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("APPDATA", configRoot)
	if runtime.GOOS != "windows" {
		t.Setenv("HOME", configRoot)
	}
}

func lateConfigPath(t *testing.T) string {
	t.Helper()

	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}

	return filepath.Join(configDir, "late", "config.json")
}

func TestResolveOpenAISettings(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		env     map[string]string
		present map[string]bool
		want    OpenAISettings
	}{
		{
			name: "env only",
			env: map[string]string{
				"OPENAI_BASE_URL": "https://env.example",
				"OPENAI_API_KEY":  "env-key",
				"OPENAI_MODEL":    "env-model",
			},
			present: map[string]bool{
				"OPENAI_BASE_URL": true,
				"OPENAI_API_KEY":  true,
				"OPENAI_MODEL":    true,
			},
			want: OpenAISettings{BaseURL: "https://env.example", APIKey: "env-key", Model: "env-model"},
		},
		{
			name: "config only",
			cfg: &Config{
				OpenAIBaseURL: "https://config.example",
				OpenAIAPIKey:  "config-key",
				OpenAIModel:   "config-model",
			},
			want: OpenAISettings{BaseURL: "https://config.example", APIKey: "config-key", Model: "config-model"},
		},
		{
			name: "env wins over config",
			cfg: &Config{
				OpenAIBaseURL: "https://config.example",
				OpenAIAPIKey:  "config-key",
				OpenAIModel:   "config-model",
			},
			env: map[string]string{
				"OPENAI_BASE_URL": "https://env.example",
				"OPENAI_API_KEY":  "env-key",
				"OPENAI_MODEL":    "env-model",
			},
			present: map[string]bool{
				"OPENAI_BASE_URL": true,
				"OPENAI_API_KEY":  true,
				"OPENAI_MODEL":    true,
			},
			want: OpenAISettings{BaseURL: "https://env.example", APIKey: "env-key", Model: "env-model"},
		},
		{
			name: "none set uses default URL",
			want: OpenAISettings{BaseURL: DefaultOpenAIBaseURL},
		},
		{
			name: "empty env falls back to config",
			cfg: &Config{
				OpenAIBaseURL: "https://config.example",
				OpenAIAPIKey:  "config-key",
				OpenAIModel:   "config-model",
			},
			env: map[string]string{
				"OPENAI_BASE_URL": "",
				"OPENAI_API_KEY":  "",
				"OPENAI_MODEL":    "",
			},
			present: map[string]bool{
				"OPENAI_BASE_URL": true,
				"OPENAI_API_KEY":  true,
				"OPENAI_MODEL":    true,
			},
			want: OpenAISettings{BaseURL: "https://config.example", APIKey: "config-key", Model: "config-model"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveOpenAISettingsWithEnv(tt.cfg, func(key string) (string, bool) {
				value, ok := tt.env[key]
				if tt.present != nil {
					ok = tt.present[key]
				}
				return value, ok
			})

			if got.BaseURL != tt.want.BaseURL {
				t.Fatalf("BaseURL = %q, want %q", got.BaseURL, tt.want.BaseURL)
			}
			if got.APIKey != tt.want.APIKey {
				t.Fatalf("APIKey = %q, want %q", got.APIKey, tt.want.APIKey)
			}
			if got.Model != tt.want.Model {
				t.Fatalf("Model = %q, want %q", got.Model, tt.want.Model)
			}
		})
	}
}

func TestResolveSubagentSettings(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		openAI  OpenAISettings
		env     map[string]string
		present map[string]bool
		want    SubagentSettings
	}{
		{
			name:   "env only",
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			env: map[string]string{
				"LATE_SUBAGENT_BASE_URL": "https://env-sub.example",
				"LATE_SUBAGENT_API_KEY":  "env-sub-key",
				"LATE_SUBAGENT_MODEL":    "env-sub-model",
			},
			present: map[string]bool{
				"LATE_SUBAGENT_BASE_URL": true,
				"LATE_SUBAGENT_API_KEY":  true,
				"LATE_SUBAGENT_MODEL":    true,
			},
			want: SubagentSettings{BaseURL: "https://env-sub.example", APIKey: "env-sub-key", Model: "env-sub-model"},
		},
		{
			name: "config only",
			cfg: &Config{
				LateSubagentBaseURL: "https://config-sub.example",
				LateSubagentAPIKey:  "config-sub-key",
				LateSubagentModel:   "config-sub-model",
			},
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			want:   SubagentSettings{BaseURL: "https://config-sub.example", APIKey: "config-sub-key", Model: "config-sub-model"},
		},
		{
			name: "env wins over config",
			cfg: &Config{
				LateSubagentBaseURL: "https://config-sub.example",
				LateSubagentAPIKey:  "config-sub-key",
				LateSubagentModel:   "config-sub-model",
			},
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			env: map[string]string{
				"LATE_SUBAGENT_BASE_URL": "https://env-sub.example",
				"LATE_SUBAGENT_API_KEY":  "env-sub-key",
				"LATE_SUBAGENT_MODEL":    "env-sub-model",
			},
			present: map[string]bool{
				"LATE_SUBAGENT_BASE_URL": true,
				"LATE_SUBAGENT_API_KEY":  true,
				"LATE_SUBAGENT_MODEL":    true,
			},
			want: SubagentSettings{BaseURL: "https://env-sub.example", APIKey: "env-sub-key", Model: "env-sub-model"},
		},
		{
			name: "empty env falls back to config",
			cfg: &Config{
				LateSubagentBaseURL: "https://config-sub.example",
				LateSubagentAPIKey:  "config-sub-key",
				LateSubagentModel:   "config-sub-model",
			},
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			env: map[string]string{
				"LATE_SUBAGENT_BASE_URL": "",
				"LATE_SUBAGENT_API_KEY":  "",
				"LATE_SUBAGENT_MODEL":    "",
			},
			present: map[string]bool{
				"LATE_SUBAGENT_BASE_URL": true,
				"LATE_SUBAGENT_API_KEY":  true,
				"LATE_SUBAGENT_MODEL":    true,
			},
			want: SubagentSettings{BaseURL: "https://config-sub.example", APIKey: "config-sub-key", Model: "config-sub-model"},
		},
		{
			name: "openai fallback for base and api key",
			cfg: &Config{
				LateSubagentModel: "config-sub-model",
			},
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			want:   SubagentSettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "config-sub-model"},
		},
		{
			name:   "openai fallback for model",
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			want:   SubagentSettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
		},
		{
			name: "legacy config support",
			cfg: &Config{
				SubagentBaseURL: "https://legacy-sub.example",
				SubagentAPIKey:  "legacy-sub-key",
				SubagentModel:   "legacy-sub-model",
			},
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			want:   SubagentSettings{BaseURL: "https://legacy-sub.example", APIKey: "legacy-sub-key", Model: "legacy-sub-model"},
		},
		{
			name: "new config overrides legacy",
			cfg: &Config{
				SubagentBaseURL:     "https://legacy-sub.example",
				LateSubagentBaseURL: "https://new-sub.example",
			},
			openAI: OpenAISettings{BaseURL: "https://openai.example", APIKey: "openai-key", Model: "openai-model"},
			want:   SubagentSettings{BaseURL: "https://new-sub.example", APIKey: "openai-key", Model: "openai-model"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveSubagentSettingsWithEnv(tt.cfg, tt.openAI, func(key string) (string, bool) {
				value, ok := tt.env[key]
				if tt.present != nil {
					ok = tt.present[key]
				}
				return value, ok
			})

			if got.BaseURL != tt.want.BaseURL {
				t.Fatalf("BaseURL = %q, want %q", got.BaseURL, tt.want.BaseURL)
			}
			if got.APIKey != tt.want.APIKey {
				t.Fatalf("APIKey = %q, want %q", got.APIKey, tt.want.APIKey)
			}
			if got.Model != tt.want.Model {
				t.Fatalf("Model = %q, want %q", got.Model, tt.want.Model)
			}
		})
	}
}

func TestResolveSaveSubagentHistories(t *testing.T) {
	enabled := true
	disabled := false
	tests := []struct {
		name            string
		cfg             *Config
		cliExplicit     bool
		cliValue        bool
		savedPreference *bool
		want            bool
	}{
		{
			name:            "explicit flag on wins over config off",
			cfg:             &Config{SaveSubagentHistories: false},
			cliExplicit:     true,
			cliValue:        true,
			savedPreference: &disabled,
			want:            true,
		},
		{
			name:            "explicit flag off wins over config on",
			cfg:             &Config{SaveSubagentHistories: true},
			cliExplicit:     true,
			cliValue:        false,
			savedPreference: &enabled,
			want:            false,
		},
		{
			name:            "saved preference wins over config",
			cfg:             &Config{SaveSubagentHistories: true},
			savedPreference: &disabled,
			want:            false,
		},
		{
			name:            "saved enabled preference wins over config",
			cfg:             &Config{SaveSubagentHistories: false},
			savedPreference: &enabled,
			want:            true,
		},
		{
			name:        "no flag uses config on",
			cfg:         &Config{SaveSubagentHistories: true},
			cliExplicit: false,
			cliValue:    false,
			want:        true,
		},
		{
			name:        "no flag uses config off",
			cfg:         &Config{SaveSubagentHistories: false},
			cliExplicit: false,
			cliValue:    false,
			want:        false,
		},
		{
			name:        "no flag and nil config defaults to off",
			cfg:         nil,
			cliExplicit: false,
			cliValue:    false,
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveSaveSubagentHistories(tt.cfg, tt.cliExplicit, tt.cliValue, tt.savedPreference); got != tt.want {
				t.Fatalf("ResolveSaveSubagentHistories() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveSubagentTimeout(t *testing.T) {
	tests := []struct {
		name        string
		cfg         *Config
		cliExplicit bool
		cliValue    time.Duration
		want        time.Duration
		wantWarning string
	}{
		{
			name:        "explicit flag wins over config",
			cfg:         &Config{SubagentTimeout: "1h"},
			cliExplicit: true,
			cliValue:    2 * time.Minute,
			want:        2 * time.Minute,
		},
		{
			name:        "explicit zero flag wins over config (unlimited)",
			cfg:         &Config{SubagentTimeout: "1h"},
			cliExplicit: true,
			cliValue:    0,
			want:        0,
		},
		{
			name: "config parses to the configured budget",
			cfg:  &Config{SubagentTimeout: "24h"},
			want: 24 * time.Hour,
		},
		{
			name: "config minutes parse",
			cfg:  &Config{SubagentTimeout: "45m"},
			want: 45 * time.Minute,
		},
		{
			name: "config zero passes through as unlimited",
			cfg:  &Config{SubagentTimeout: "0"},
			want: 0,
		},
		{
			name: "config negative passes through as unlimited",
			cfg:  &Config{SubagentTimeout: "-5m"},
			want: -5 * time.Minute,
		},
		{
			name:        "invalid config warns and falls back to default",
			cfg:         &Config{SubagentTimeout: "garbage"},
			want:        DefaultSubagentTimeout,
			wantWarning: `ignoring invalid config.json subagent_timeout "garbage"; using default 24h`,
		},
		{
			name:        "unitless config value warns and falls back to default",
			cfg:         &Config{SubagentTimeout: "5"},
			want:        DefaultSubagentTimeout,
			wantWarning: `ignoring invalid config.json subagent_timeout "5"; using default 24h`,
		},
		{
			name: "empty config entry uses default",
			cfg:  &Config{},
			want: DefaultSubagentTimeout,
		},
		{
			name: "nil config uses default",
			cfg:  nil,
			want: DefaultSubagentTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warning := ResolveSubagentTimeout(tt.cfg, tt.cliExplicit, tt.cliValue)
			if got != tt.want {
				t.Fatalf("ResolveSubagentTimeout() = %v, want %v", got, tt.want)
			}
			if warning != tt.wantWarning {
				t.Fatalf("ResolveSubagentTimeout() warning = %q, want %q", warning, tt.wantWarning)
			}
		})
	}
}

// TestConfig_SubagentTimeoutJSONRoundTrip pins the config.json key spelling
// and round-trip behavior of the subagent_timeout entry: omitting it stays
// omitempty, and a set value survives Marshal/Unmarshal unchanged. The key
// is snake_case, matching the sibling entries in the config schema.
func TestConfig_SubagentTimeoutJSONRoundTrip(t *testing.T) {
	data, err := json.Marshal(&Config{})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "subagent_timeout") {
		t.Fatalf("empty SubagentTimeout must be omitted, got %s", data)
	}

	data, err = json.Marshal(&Config{SubagentTimeout: "45m"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(data), `"subagent_timeout":"45m"`) {
		t.Fatalf("expected subagent_timeout key in JSON, got %s", data)
	}

	var back Config
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if back.SubagentTimeout != "45m" {
		t.Fatalf("round-tripped SubagentTimeout = %q, want %q", back.SubagentTimeout, "45m")
	}
}

func TestConfig_GetModelForAgent(t *testing.T) {
	cfg := &Config{
		Models: []ModelSetting{
			{URL: "http://localhost:8080", Key: "key-1", Model: "model-1"},
			{URL: "http://localhost:9090", Key: "key-2", Model: "model-2"},
		},
		AgentModels: map[string]string{
			"orchestrator": "model-1",
			"coder":        "model-2",
			"unknown":      "model-3",
		},
	}

	tests := []struct {
		agentType string
		wantModel string
		wantOk    bool
	}{
		{"orchestrator", "model-1", true},
		{"coder", "model-2", true},
		{"unknown", "", false},
		{"missing", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.agentType, func(t *testing.T) {
			got, ok := cfg.GetModelForAgent(tt.agentType)
			if ok != tt.wantOk {
				t.Errorf("GetModelForAgent(%q) ok = %v, want %v", tt.agentType, ok, tt.wantOk)
			}
			if ok && got.Model != tt.wantModel {
				t.Errorf("GetModelForAgent(%q) got model = %q, want %q", tt.agentType, got.Model, tt.wantModel)
			}
		})
	}
}

func TestConfig_ResolveShowTodoPane(t *testing.T) {
	closed := false
	open := true

	var absentEntry Config
	if err := json.Unmarshal([]byte(`{"theme":"late"}`), &absentEntry); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}

	tests := []struct {
		name string
		cfg  *Config
		want bool
	}{
		{"nil config defaults to open", nil, true},
		{"absent show-todo-pane entry defaults to open", &absentEntry, true},
		{"zero-value config defaults to open", &Config{}, true},
		{"explicit false starts with the pane closed", &Config{ShowTodoPane: flexPtr(closed)}, false},
		{"explicit true keeps the pane open", &Config{ShowTodoPane: flexPtr(open)}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.ResolveShowTodoPane(); got != tt.want {
				t.Errorf("ResolveShowTodoPane() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfig_GetModelForAgentUsesStableID(t *testing.T) {
	cfg := &Config{
		Models: []ModelSetting{
			{ID: "provider-a", URL: "https://a.example/v1", Model: "shared-model"},
			{ID: "provider-b", URL: "https://b.example/v1", Model: "shared-model"},
		},
		AgentModels: map[string]string{"orchestrator": "provider-b"},
	}

	got, ok := cfg.GetModelForAgent("orchestrator")
	if !ok {
		t.Fatal("expected model setting to resolve")
	}
	if got.URL != "https://b.example/v1" {
		t.Fatalf("resolved URL = %q, want provider B", got.URL)
	}
}

func TestSaveConfigAtomicallyReplacesFile(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)

	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	configPath := lateConfigPath(t)
	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &Config{OpenAIModel: "replacement-model"}
	if err := SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}

	after, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && os.SameFile(before, after) {
		t.Fatal("config file was modified in place instead of atomically replaced")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm() != configFilePerm {
		t.Fatalf("config file permissions = %o, want %o", after.Mode().Perm(), configFilePerm)
	}

	loaded, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OpenAIModel != "replacement-model" {
		t.Fatalf("saved model = %q, want replacement-model", loaded.OpenAIModel)
	}

	entries, err := os.ReadDir(filepath.Dir(configPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Fatalf("temporary config was not cleaned up: %s", entry.Name())
		}
	}
}

func TestResolvePermissionMode(t *testing.T) {
	tests := []struct {
		name               string
		cfg                *Config
		askFlag            bool
		unsupervisedFlag   bool
		forceRevaluateFlag bool
		wantMode           string
		// wantWarning nil: warning must be empty; non-nil: warning must
		// contain each substring.
		wantWarning     []string
		wantErr         bool
		wantErrContains string
	}{
		{
			name:     "no flags, empty config, nil cfg",
			cfg:      nil,
			wantMode: PermissionModeAskForUserApproval,
		},
		{
			name:     "no flags, empty config value",
			cfg:      &Config{PermissionMode: ""},
			wantMode: PermissionModeAskForUserApproval,
		},
		{
			name:     "ask flag alone",
			askFlag:  true,
			wantMode: PermissionModeAskForUserApproval,
		},
		{
			name:             "unsupervised flag alone",
			unsupervisedFlag: true,
			wantMode:         PermissionModeUnsupervised,
		},
		{
			name:               "force-revaluate flag alone",
			forceRevaluateFlag: true,
			wantMode:           PermissionModeForceRevaluate,
		},
		{
			name:     "ask flag overrides unsupervised config",
			cfg:      &Config{PermissionMode: PermissionModeUnsupervised},
			askFlag:  true,
			wantMode: PermissionModeAskForUserApproval,
		},
		{
			name:             "unsupervised flag overrides ask config",
			cfg:              &Config{PermissionMode: PermissionModeAskForUserApproval},
			unsupervisedFlag: true,
			wantMode:         PermissionModeUnsupervised,
		},
		{
			name:     "ask flag overrides force-revaluate config",
			cfg:      &Config{PermissionMode: PermissionModeForceRevaluate},
			askFlag:  true,
			wantMode: PermissionModeAskForUserApproval,
		},
		{
			name:               "force-revaluate flag overrides ask config",
			cfg:                &Config{PermissionMode: PermissionModeAskForUserApproval},
			forceRevaluateFlag: true,
			wantMode:           PermissionModeForceRevaluate,
		},
		{
			name:     "config value: ask-for-user-approval respected",
			cfg:      &Config{PermissionMode: PermissionModeAskForUserApproval},
			wantMode: PermissionModeAskForUserApproval,
		},
		{
			name:     "config value: i-promise-i-have-backups-and-will-not-file-issues respected",
			cfg:      &Config{PermissionMode: PermissionModeUnsupervised},
			wantMode: PermissionModeUnsupervised,
		},
		{
			name:     "config value: force-revaluate-dangerous-commands respected",
			cfg:      &Config{PermissionMode: PermissionModeForceRevaluate},
			wantMode: PermissionModeForceRevaluate,
		},
		{
			name:        "invalid config value falls back to default with warning",
			cfg:         &Config{PermissionMode: "yolo"},
			wantMode:    PermissionModeAskForUserApproval,
			wantWarning: []string{"invalid", "yolo"},
		},
		{
			name:             "two flags set is an error",
			askFlag:          true,
			unsupervisedFlag: true,
			wantErr:          true,
			wantErrContains:  "mutually exclusive",
		},
		{
			name:               "all three flags set is an error",
			askFlag:            true,
			unsupervisedFlag:   true,
			forceRevaluateFlag: true,
			wantErr:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode, warning, err := ResolvePermissionMode(tt.cfg, tt.askFlag, tt.unsupervisedFlag, tt.forceRevaluateFlag)

			if tt.wantErr {
				if err == nil {
					t.Fatal("ResolvePermissionMode() expected an error, got nil")
				}
				if tt.wantErrContains != "" && !strings.Contains(err.Error(), tt.wantErrContains) {
					t.Fatalf("ResolvePermissionMode() error = %q, want it to contain %q", err.Error(), tt.wantErrContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolvePermissionMode() error = %v, want nil", err)
			}
			if mode != tt.wantMode {
				t.Fatalf("ResolvePermissionMode() mode = %q, want %q", mode, tt.wantMode)
			}
			if len(tt.wantWarning) == 0 {
				if warning != "" {
					t.Fatalf("ResolvePermissionMode() warning = %q, want empty", warning)
				}
				return
			}
			if warning == "" {
				t.Fatal("ResolvePermissionMode() warning is empty, want a warning")
			}
			for _, substring := range tt.wantWarning {
				if !strings.Contains(warning, substring) {
					t.Fatalf("ResolvePermissionMode() warning = %q, want it to contain %q", warning, substring)
				}
			}
		})
	}
}

func TestResolveCompactionThreshold(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *Config
		want        int
		wantWarning []string
	}{
		{
			name: "nil config uses default",
			cfg:  nil,
			want: DefaultCompactionThresholdPercent,
		},
		{
			name: "unset uses default",
			cfg:  &Config{},
			want: DefaultCompactionThresholdPercent,
		},
		{
			name: "valid value honored",
			cfg:  &Config{CompactionThresholdPercent: 65},
			want: 65,
		},
		{
			name: "one is valid",
			cfg:  &Config{CompactionThresholdPercent: 1},
			want: 1,
		},
		{
			name: "hundred is valid",
			cfg:  &Config{CompactionThresholdPercent: 100},
			want: 100,
		},
		{
			name:        "negative value invalid",
			cfg:         &Config{CompactionThresholdPercent: -5},
			want:        DefaultCompactionThresholdPercent,
			wantWarning: []string{"invalid", "-5"},
		},
		{
			name:        "over hundred invalid",
			cfg:         &Config{CompactionThresholdPercent: 250},
			want:        DefaultCompactionThresholdPercent,
			wantWarning: []string{"invalid", "250"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := ResolveCompactionThreshold(tc.cfg)
			if got != tc.want {
				t.Fatalf("ResolveCompactionThreshold() = %d, want %d", got, tc.want)
			}
			if len(tc.wantWarning) == 0 {
				if warning != "" {
					t.Fatalf("warning = %q, want empty", warning)
				}
				return
			}
			if warning == "" {
				t.Fatal("warning is empty, want a warning")
			}
			for _, substring := range tc.wantWarning {
				if !strings.Contains(warning, substring) {
					t.Fatalf("warning = %q, want it to contain %q", warning, substring)
				}
			}
		})
	}
}

// TestResolveCompactionMode mirrors TestResolveCompactionThreshold: the
// staged rollout modes validate to off|shadow|enabled, empty means the
// default (shadow), and anything else warns and falls back to the default.
func TestResolveCompactionMode(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *Config
		want        string
		wantWarning []string
	}{
		{
			name: "nil config uses default",
			cfg:  nil,
			want: DefaultCompactionMode,
		},
		{
			name: "unset uses default",
			cfg:  &Config{},
			want: DefaultCompactionMode,
		},
		{
			name: "off honored",
			cfg:  &Config{CompactionMode: CompactionModeOff},
			want: CompactionModeOff,
		},
		{
			name: "shadow honored",
			cfg:  &Config{CompactionMode: CompactionModeShadow},
			want: CompactionModeShadow,
		},
		{
			name: "enabled honored",
			cfg:  &Config{CompactionMode: CompactionModeEnabled},
			want: CompactionModeEnabled,
		},
		{
			name:        "invalid value warns and falls back",
			cfg:         &Config{CompactionMode: "aggressive"},
			want:        DefaultCompactionMode,
			wantWarning: []string{"invalid", "aggressive", DefaultCompactionMode},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := ResolveCompactionMode(tc.cfg)
			if got != tc.want {
				t.Fatalf("ResolveCompactionMode() = %q, want %q", got, tc.want)
			}
			if len(tc.wantWarning) == 0 {
				if warning != "" {
					t.Fatalf("warning = %q, want empty", warning)
				}
				return
			}
			if warning == "" {
				t.Fatal("warning is empty, want a warning")
			}
			for _, substring := range tc.wantWarning {
				if !strings.Contains(warning, substring) {
					t.Fatalf("warning = %q, want it to contain %q", warning, substring)
				}
			}
		})
	}

	for _, valid := range []string{CompactionModeOff, CompactionModeShadow, CompactionModeEnabled} {
		if !IsValidCompactionMode(valid) {
			t.Errorf("IsValidCompactionMode(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"", "Aggressive", "shadow ", "elided"} {
		if IsValidCompactionMode(invalid) {
			t.Errorf("IsValidCompactionMode(%q) = true, want false", invalid)
		}
	}
}

// TestResolveCompactionMaxElidePercent mirrors TestResolveCompactionThreshold
// for the elide-fraction tripwire knob: 0 (unset) means the reference default,
// 1-100 are honored, anything else warns and falls back.
func TestResolveCompactionMaxElidePercent(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *Config
		want        int
		wantWarning []string
	}{
		{
			name: "nil config uses default",
			cfg:  nil,
			want: DefaultCompactionMaxElidePercent,
		},
		{
			name: "unset uses default",
			cfg:  &Config{},
			want: DefaultCompactionMaxElidePercent,
		},
		{
			name: "valid value honored",
			cfg:  &Config{CompactionMaxElidePercent: 50},
			want: 50,
		},
		{
			name: "one is valid",
			cfg:  &Config{CompactionMaxElidePercent: 1},
			want: 1,
		},
		{
			name: "hundred is valid",
			cfg:  &Config{CompactionMaxElidePercent: 100},
			want: 100,
		},
		{
			name:        "negative value invalid",
			cfg:         &Config{CompactionMaxElidePercent: -10},
			want:        DefaultCompactionMaxElidePercent,
			wantWarning: []string{"invalid", "-10"},
		},
		{
			name:        "over hundred invalid",
			cfg:         &Config{CompactionMaxElidePercent: 101},
			want:        DefaultCompactionMaxElidePercent,
			wantWarning: []string{"invalid", "101"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := ResolveCompactionMaxElidePercent(tc.cfg)
			if got != tc.want {
				t.Fatalf("ResolveCompactionMaxElidePercent() = %d, want %d", got, tc.want)
			}
			assertResolverWarning(t, warning, tc.wantWarning)
		})
	}
}

// TestResolveCompactionProtectedFloor mirrors TestResolveCompactionThreshold
// for the protected-kind floor knob: 0 (unset) means the reference default,
// 1-100 are honored, anything else warns and falls back.
func TestResolveCompactionProtectedFloor(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *Config
		want        int
		wantWarning []string
	}{
		{
			name: "nil config uses default",
			cfg:  nil,
			want: DefaultCompactionProtectedFloorPercent,
		},
		{
			name: "unset uses default",
			cfg:  &Config{},
			want: DefaultCompactionProtectedFloorPercent,
		},
		{
			name: "valid value honored",
			cfg:  &Config{CompactionProtectedFloor: 20},
			want: 20,
		},
		{
			name: "one is valid",
			cfg:  &Config{CompactionProtectedFloor: 1},
			want: 1,
		},
		{
			name: "hundred is valid",
			cfg:  &Config{CompactionProtectedFloor: 100},
			want: 100,
		},
		{
			name:        "negative value invalid",
			cfg:         &Config{CompactionProtectedFloor: -3},
			want:        DefaultCompactionProtectedFloorPercent,
			wantWarning: []string{"invalid", "-3"},
		},
		{
			name:        "over hundred invalid",
			cfg:         &Config{CompactionProtectedFloor: 250},
			want:        DefaultCompactionProtectedFloorPercent,
			wantWarning: []string{"invalid", "250"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warning := ResolveCompactionProtectedFloor(tc.cfg)
			if got != tc.want {
				t.Fatalf("ResolveCompactionProtectedFloor() = %d, want %d", got, tc.want)
			}
			assertResolverWarning(t, warning, tc.wantWarning)
		})
	}
}

// assertResolverWarning checks a resolver's warning against the expected
// substrings (empty means the warning must be empty too).
func assertResolverWarning(t *testing.T, warning string, wantSubstrings []string) {
	t.Helper()
	if len(wantSubstrings) == 0 {
		if warning != "" {
			t.Fatalf("warning = %q, want empty", warning)
		}
		return
	}
	if warning == "" {
		t.Fatal("warning is empty, want a warning")
	}
	for _, substring := range wantSubstrings {
		if !strings.Contains(warning, substring) {
			t.Fatalf("warning = %q, want it to contain %q", warning, substring)
		}
	}
}

// TestLoadConfig_CompactionMode covers the config-file path: valid modes
// parse through, invalid ones survive loading so ResolveCompactionMode can
// warn and fall back to the default.
func TestLoadConfig_CompactionMode(t *testing.T) {
	t.Run("valid mode parses", func(t *testing.T) {
		configRoot := t.TempDir()
		setUserConfigEnv(t, configRoot)
		configPath := lateConfigPath(t)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(`{"enabled_tools": {"bash": true}, "compaction-mode": "enabled"}`), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig() error = %v", err)
		}
		if cfg.CompactionMode != CompactionModeEnabled {
			t.Fatalf("CompactionMode = %q, want %q", cfg.CompactionMode, CompactionModeEnabled)
		}
	})

	t.Run("invalid mode is a fatal load error under strict config", func(t *testing.T) {
		configRoot := t.TempDir()
		setUserConfigEnv(t, configRoot)
		configPath := lateConfigPath(t)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(`{"enabled_tools": {"bash": true}, "compaction-mode": "yolo"}`), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadConfig()
		if err == nil {
			t.Fatal("LoadConfig() expected the invalid compaction-mode to be a fatal error")
		}
		if cfg != nil {
			t.Fatal("LoadConfig() returned a config alongside a fatal error")
		}
		if !strings.Contains(err.Error(), `"yolo" is not a valid compaction-mode value`) {
			t.Fatalf("LoadConfig() error = %q, want the R3 enum message", err.Error())
		}
	})

	t.Run("resolver still warns on an invalid in-memory value (defense in depth)", func(t *testing.T) {
		// Strict config rejects an invalid compaction-mode at load time, so
		// the resolver's warning branch is unreachable via LoadConfig; it is
		// kept for callers that construct a Config directly.
		mode, warning := ResolveCompactionMode(&Config{CompactionMode: "yolo"})
		if mode != DefaultCompactionMode {
			t.Fatalf("resolved mode = %q, want %q", mode, DefaultCompactionMode)
		}
		if !strings.Contains(warning, "yolo") || !strings.Contains(warning, DefaultCompactionMode) {
			t.Fatalf("warning = %q, want it to name the invalid value and the fallback", warning)
		}
	})
}

// TestConfig_CompactionModeJSONRoundTrip: the field marshals under its
// config key and stays omitted when unset.
func TestConfig_CompactionModeJSONRoundTrip(t *testing.T) {
	original := Config{CompactionMode: CompactionModeEnabled}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.CompactionMode != CompactionModeEnabled {
		t.Fatalf("CompactionMode after round trip = %q, want %q", decoded.CompactionMode, CompactionModeEnabled)
	}

	emptyData, err := json.Marshal(Config{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(emptyData, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := raw["compaction-mode"]; ok {
		t.Fatalf("empty config should not marshal a compaction-mode key, got %s", emptyData)
	}
}

func TestLoadConfig_ParsesInfoBarAndThresholdFields(t *testing.T) {
	configRoot := t.TempDir()
	setUserConfigEnv(t, configRoot)
	configPath := lateConfigPath(t)

	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatal(err)
	}
	content := `{
		"enabled_tools": {"bash": true},
		"show-info-bar": true,
		"compaction-threshold-percent": 65
	}`
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !cfg.ShowInfoBar.Bool() {
		t.Fatal("ShowInfoBar = false, want true")
	}
	if cfg.CompactionThresholdPercent != 65 {
		t.Fatalf("CompactionThresholdPercent = %d, want 65", cfg.CompactionThresholdPercent)
	}
}

func TestConfig_InfoBarAndThresholdJSONRoundTrip(t *testing.T) {
	original := Config{ShowInfoBar: true, CompactionThresholdPercent: 65}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !decoded.ShowInfoBar.Bool() {
		t.Fatal("ShowInfoBar after round trip = false, want true")
	}
	if decoded.CompactionThresholdPercent != 65 {
		t.Fatalf("CompactionThresholdPercent after round trip = %d, want 65", decoded.CompactionThresholdPercent)
	}

	// Zero values must not emit keys (omitempty), keeping config.json clean
	// for users who never touched the new settings.
	emptyData, err := json.Marshal(Config{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(emptyData, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, key := range []string{"show-info-bar", "compaction-threshold-percent"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("empty config should not marshal a %s key, got %s", key, emptyData)
		}
	}
}

func TestConfig_PermissionModeJSONRoundTrip(t *testing.T) {
	original := Config{PermissionMode: PermissionModeUnsupervised}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if decoded.PermissionMode != PermissionModeUnsupervised {
		t.Fatalf("PermissionMode after round trip = %q, want %q", decoded.PermissionMode, PermissionModeUnsupervised)
	}

	emptyData, err := json.Marshal(Config{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(emptyData, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if _, ok := raw["permission-mode"]; ok {
		t.Fatalf("empty config should not marshal a permission-mode key, got %s", emptyData)
	}
}

// TestResolveAutocompact mirrors TestResolveCompactionThreshold: the switch
// is a plain boolean (absent = disabled) and the percentage validates 1-100,
// with 0 (unset) meaning the default and anything else warning and falling
// back to the default.
func TestResolveAutocompact(t *testing.T) {
	cases := []struct {
		name             string
		cfg              *Config
		wantEnabled      bool
		wantPercent      int
		wantWarningParts []string
	}{
		{
			name:        "nil config uses defaults",
			cfg:         nil,
			wantEnabled: false,
			wantPercent: DefaultJevAutocompactPercent,
		},
		{
			name:        "unset uses defaults",
			cfg:         &Config{},
			wantEnabled: false,
			wantPercent: DefaultJevAutocompactPercent,
		},
		{
			name:        "enabled with default percent",
			cfg:         &Config{JevAutocompact: true},
			wantEnabled: true,
			wantPercent: DefaultJevAutocompactPercent,
		},
		{
			name:        "valid percent honored",
			cfg:         &Config{JevAutocompact: true, JevAutocompactPercent: 90},
			wantEnabled: true,
			wantPercent: 90,
		},
		{
			name:        "one is valid",
			cfg:         &Config{JevAutocompactPercent: 1},
			wantEnabled: false,
			wantPercent: 1,
		},
		{
			name:        "hundred is valid",
			cfg:         &Config{JevAutocompactPercent: 100},
			wantEnabled: false,
			wantPercent: 100,
		},
		{
			name:             "negative percent invalid",
			cfg:              &Config{JevAutocompact: true, JevAutocompactPercent: -5},
			wantEnabled:      true,
			wantPercent:      DefaultJevAutocompactPercent,
			wantWarningParts: []string{"invalid", "-5"},
		},
		{
			name:             "over hundred percent invalid",
			cfg:              &Config{JevAutocompactPercent: 250},
			wantEnabled:      false,
			wantPercent:      DefaultJevAutocompactPercent,
			wantWarningParts: []string{"invalid", "250"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEnabled, gotPercent, warning := ResolveAutocompact(tc.cfg)
			if gotEnabled != tc.wantEnabled {
				t.Fatalf("ResolveAutocompact() enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if gotPercent != tc.wantPercent {
				t.Fatalf("ResolveAutocompact() percent = %d, want %d", gotPercent, tc.wantPercent)
			}
			if len(tc.wantWarningParts) == 0 {
				if warning != "" {
					t.Fatalf("warning = %q, want empty", warning)
				}
				return
			}
			if warning == "" {
				t.Fatal("warning is empty, want a warning")
			}
			for _, substring := range tc.wantWarningParts {
				if !strings.Contains(warning, substring) {
					t.Fatalf("warning = %q, want it to contain %q", warning, substring)
				}
			}
		})
	}
}

func TestConfig_AutocompactJSONRoundTrip(t *testing.T) {
	original := Config{JevAutocompact: true, JevAutocompactPercent: 90}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if !decoded.JevAutocompact.Bool() {
		t.Fatal("JevAutocompact after round trip = false, want true")
	}
	if decoded.JevAutocompactPercent != 90 {
		t.Fatalf("JevAutocompactPercent after round trip = %d, want 90", decoded.JevAutocompactPercent)
	}

	// Zero values must not emit keys (omitempty), keeping config.json clean
	// for users who never touched the new settings.
	emptyData, err := json.Marshal(Config{})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(emptyData, &raw); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	for _, key := range []string{"jev-autocompact", "jev-autocompact-percent"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("empty config should not marshal a %s key, got %s", key, emptyData)
		}
	}
}

// The degradation guard is covered by TestLoadConfig_StrictErrorsAbort
// (every content error is fatal with a nil config) and
// TestSaveConfig_RefusesDegradedConfig (the Degraded defense-in-depth
// refusal).

// TestConfig_DegradedNotSerialized pins that the runtime-only Degraded flag
// never leaks into config.json (json:"-").
func TestConfig_DegradedNotSerialized(t *testing.T) {
	data, err := json.Marshal(&Config{Degraded: true})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if _, ok := raw["Degraded"]; ok {
		t.Fatalf("Degraded must not be serialized, got %s", data)
	}
}
