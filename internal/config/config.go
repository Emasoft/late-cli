package config

import (
	"encoding/json"
	"fmt"
	"late/internal/pathutil"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const DefaultOpenAIBaseURL = "http://localhost:8080"

// Permission modes for supervising potentially dangerous commands.
// The effective mode is resolved by ResolvePermissionMode:
// explicitly set CLI flag > config.json permission-mode entry >
// PermissionModeAskForUserApproval.
const (
	PermissionModeAskForUserApproval = "ask-for-user-approval"
	PermissionModeUnsupervised       = "i-promise-i-have-backups-and-will-not-file-issues"
	PermissionModeForceRevaluate     = "force-revaluate-dangerous-commands"
)

// DefaultSubagentTimeout is the default wall-clock budget for a single
// subagent run, applied when neither the --subagent-timeout flag nor the
// config.json "subagent_timeout" entry provides a value.
const DefaultSubagentTimeout = 24 * time.Hour

type EnvLookup func(string) (string, bool)

type OpenAISettings struct {
	BaseURL string
	APIKey  string
	Model   string
}

type SubagentSettings struct {
	BaseURL string
	APIKey  string
	Model   string
}

type ModelSetting struct {
	ID    string `json:"id,omitempty"`
	URL   string `json:"url"`
	Key   string `json:"key"`
	Model string `json:"model"`
}

// Reference returns the stable value stored in agent_models. Model is retained
// as a fallback for configurations created before model IDs were introduced.
func (m ModelSetting) Reference() string {
	if m.ID != "" {
		return m.ID
	}
	return m.Model
}

const (
	configDirPerm  os.FileMode = 0o700
	configFilePerm os.FileMode = 0o600
)

// DefaultCompactionThresholdPercent is the context-usage percentage at which
// compaction happens when config.json does not set
// compaction-threshold-percent (or sets an invalid value).
const DefaultCompactionThresholdPercent = 80

// Compaction modes (staged rollout of the jev-compaction port). The
// effective mode is resolved by ResolveCompactionMode: an explicitly set
// --compaction-mode flag (validated in main) > config.json compaction-mode >
// DefaultCompactionMode.
const (
	// CompactionModeOff disables compaction entirely: no scoring, no shadow
	// log, no relocation.
	CompactionModeOff = "off"
	// CompactionModeShadow scores tool outputs and appends to the shadow log
	// without changing any tool result (the stage-1 behavior).
	CompactionModeShadow = "shadow"
	// CompactionModeEnabled additionally relocates low-scoring segments out
	// of tool results (with [[elided …]] pointers plus the expand tool to
	// retrieve the originals).
	CompactionModeEnabled = "enabled"
)

// DefaultCompactionMode is the compaction-mode default: score and shadow-log
// only, never change agent behavior.
const DefaultCompactionMode = CompactionModeShadow

// DefaultJevAutocompactPercent is the context-usage percentage at which the
// JEV auto-compaction trigger fires when config.json does not set
// jev-autocompact-percent (or sets an invalid value).
const DefaultJevAutocompactPercent = 99

// DefaultCompactionMaxElidePercent is the elide-fraction tripwire: when the
// compaction scorer wants to elide more than this percentage of a tool
// output's tokens, it is distrusted and nothing is elided (reference:
// jev-compaction pipeline.py max_elide_fraction=0.7). Applied when
// config.json does not set compaction-max-elide-percent (or sets an
// invalid value).
const DefaultCompactionMaxElidePercent = 70

// DefaultCompactionThreshold mirrors compaction.DefaultRelocationThreshold —
// the score strictly below which segments are elided (re-declared here so
// the config package stays free of a compaction import). It is the fallback
// when neither the -compaction-threshold flag nor config.json
// compaction-threshold provides a valid value.
const DefaultCompactionThreshold = 0.35

// DefaultCompactionProtectedFloorPercent is the score floor (as a
// percentage) under which protected segment kinds (stacktrace, diff) may be
// elided: at any higher score they are kept even below the normal
// threshold (reference: pipeline.py protected_floor=0.05). Applied when
// config.json does not set compaction-protected-floor (or sets an invalid
// value).
const DefaultCompactionProtectedFloorPercent = 5

// CompactionBackendOffline is the compaction-backend value that selects the
// deterministic offline scripted scorer (compaction.ScriptedScorer): the
// whole compaction flow runs locally with no API key and no network. It
// exists for demos and tests — the scripted scores are a content hash, not a
// judgment of essentialness — and must never become a production default.
// It mirrors compaction.OfflineBackendName (re-declared here so the config
// package stays free of a compaction import).
const CompactionBackendOffline = "offline"

// Config represents the application configuration.
type Config struct {
	EnabledTools        map[string]bool `json:"enabled_tools"`
	OpenAIBaseURL       string          `json:"openai_base_url,omitempty"`
	OpenAIAPIKey        string          `json:"openai_api_key,omitempty"`
	OpenAIModel         string          `json:"openai_model,omitempty"`
	LateSubagentBaseURL string          `json:"late_subagent_base_url,omitempty"`
	LateSubagentAPIKey  string          `json:"late_subagent_api_key,omitempty"`
	LateSubagentModel   string          `json:"late_subagent_model,omitempty"`

	// SaveSubagentHistories opts in to persisting subagent conversation
	// histories under <sessions>/<session-id>/subagents/. Default false.
	// Enable via config file or the --save-subagent-histories CLI flag.
	SaveSubagentHistories bool `json:"save_subagent_histories,omitempty"`

	// PermissionMode selects how potentially dangerous commands are
	// supervised. One of the PermissionMode* constants; empty means the
	// default (ask-for-user-approval). Set via config file; the CLI flags
	// of the same names override it.
	PermissionMode string `json:"permission-mode,omitempty"`

	// SubagentTimeout is an optional wall-clock budget for a single subagent
	// run, parsed with time.ParseDuration (e.g. "45m", "2h"). "0" or a
	// negative value means unlimited. Empty means the built-in default
	// (DefaultSubagentTimeout). Precedence: an explicitly passed
	// --subagent-timeout flag wins over this entry.
	SubagentTimeout string `json:"subagent_timeout,omitempty"`

	// Legacy subagent fields for backward compatibility
	SubagentBaseURL string `json:"subagent_base_url,omitempty"`
	SubagentAPIKey  string `json:"subagent_api_key,omitempty"`
	SubagentModel   string `json:"subagent_model,omitempty"`

	SkillsDir string `json:"skills_dir,omitempty"`

	// ShowTodoPane controls whether the todos side pane starts open.
	// The todos panel is open by default; set false to start with it
	// closed. Terminals narrower than 85 columns always start with the
	// pane closed; it can be opened later with /todos.
	ShowTodoPane *bool `json:"show-todo-pane,omitempty"`

	Theme       string            `json:"theme,omitempty"`
	Models      []ModelSetting    `json:"models,omitempty"`
	AgentModels map[string]string `json:"agent_models,omitempty"`

	// ShowInfoBar toggles the single-line info footer rendered below the
	// TUI status bar (version, model, context usage, uptime, ...). Toggled
	// at runtime with the /infobar slash command, which persists the new
	// value back to config.json.
	ShowInfoBar bool `json:"show-info-bar,omitempty"`

	// ShowTimestamps toggles the [HH:MM:SS] prefix rendered at the start
	// of each transcript message block. Toggled at runtime with the
	// /timestamps slash command, which persists the new value back to
	// config.json.
	ShowTimestamps bool `json:"show-timestamps,omitempty"`

	// CompactionThresholdPercent is the context-usage percentage at which
	// compaction should trigger (surfaced by the TUI info bar as the
	// remaining headroom). 0 means DefaultCompactionThresholdPercent;
	// values outside 1-100 are invalid and resolve back to the default
	// with a warning (see ResolveCompactionThreshold).
	CompactionThresholdPercent int `json:"compaction-threshold-percent,omitempty"`

	// CompactionMode selects the staged rollout stage of the tool-output
	// compaction port: CompactionModeOff, CompactionModeShadow (default:
	// score + shadow log only), or CompactionModeEnabled (also relocate
	// low-scoring segments and register the expand tool). Set via config
	// file; the --compaction-mode CLI flag overrides it. Invalid values
	// warn and fall back to DefaultCompactionMode (see
	// ResolveCompactionMode).
	CompactionMode string `json:"compaction-mode,omitempty"`

	// JevAutocompact enables the automatic full-history context compaction
	// (the /jev-compact-context flow): when the focused agent's context
	// usage crosses JevAutocompactPercent of the context window, the TUI
	// runs one compaction pass. Default false.
	JevAutocompact bool `json:"jev-autocompact,omitempty"`

	// JevAutocompactPercent is that threshold percentage. 0 (unset) means
	// DefaultJevAutocompactPercent; values outside 1-100 are invalid and
	// resolve back to the default with a warning (see ResolveAutocompact).
	JevAutocompactPercent int `json:"jev-autocompact-percent,omitempty"`

	// CompactionMaxElidePercent is the compaction gate's elide-fraction
	// tripwire as a percentage: when the scorer wants to elide more than
	// this share of a tool output's tokens, the scorer is distrusted and
	// NOTHING is elided (the tripwire is recorded in the result and the
	// shadow log). 0 (unset) means DefaultCompactionMaxElidePercent;
	// values outside 1-100 are invalid and resolve back to the default with
	// a warning (see ResolveCompactionMaxElidePercent). The tripwire cannot
	// be disabled from config.json (100 still trips on an over-100% claim,
	// i.e. never — set it to 100 for the closest thing to off).
	CompactionMaxElidePercent int `json:"compaction-max-elide-percent,omitempty"`

	// CompactionThreshold is the elision score threshold: the score
	// strictly below which tool-output segments (and full-history
	// segments, for /jev-compact-context) are elided when compaction-mode
	// is "enabled". 0 (unset) means the -compaction-threshold flag, or the
	// DefaultCompactionThreshold (0.35) when the flag is not passed; values
	// outside (0,1] are invalid and resolve back to the default with a
	// warning (see ResolveCompactionScoreThreshold). NOTE: this is the
	// per-segment SCORE cutoff — compaction-threshold-percent above is a
	// different knob (the context-usage level the info bar reports
	// headroom for), and jev-autocompact-percent is a third one (the
	// context-usage level that fires the auto-trigger).
	CompactionThreshold float64 `json:"compaction-threshold,omitempty"`

	// CompactionProtectedFloor is the score floor, as a percentage, under
	// which protected segment kinds (stacktrace, diff) may be elided: at
	// any higher score they are kept even below the normal threshold.
	// 0 (unset) means DefaultCompactionProtectedFloorPercent; values
	// outside 1-100 are invalid and resolve back to the default with a
	// warning (see ResolveCompactionProtectedFloor).
	CompactionProtectedFloor int `json:"compaction-protected-floor,omitempty"`

	// CompactionBackend selects where compaction scores come from. The only
	// value today is CompactionBackendOffline ("offline"): the deterministic
	// scripted scorer — no API key, no network, demos and tests only (its
	// scores are content hashes, not judgments). Empty (unset) keeps the
	// env-based backend resolution (JEV_API / auto-detection) that
	// compaction.ResolveBackendEnv performs. A set value WINS over the env
	// — the config entry is the explicit statement about where scoring
	// happens, so JEV_API is only consulted when this entry is absent.
	// Invalid values warn and fall back to the env-based resolution (see
	// ResolveCompactionBackend).
	CompactionBackend string `json:"compaction-backend,omitempty"`

	// CompactionRetrieval enables the retrieve() read side of the compaction
	// record store (Step 17): before every stream request the store's digest
	// summaries are scored against the current task and the top-k relevant
	// records are appended to the outgoing request's work area (never the
	// frozen prefix, never persisted to history). Default false. It needs a
	// record store to read, which only fills when compaction-mode is
	// "enabled" — ResolveCompactionRetrieval warns about the inert
	// combinations.
	CompactionRetrieval bool `json:"compaction-retrieval,omitempty"`

	// Degraded is set by LoadConfig when config.json exists but could not
	// be read or parsed: the returned config is a fallback default, not
	// the user's real settings. SaveConfig refuses to persist a degraded
	// config so that runtime toggles (/infobar, /timestamps, /model)
	// cannot overwrite the user's hand-edited config.json with defaults.
	// It is never serialized (json:"-") and is NOT set when the file is
	// merely missing — that is a normal fresh install.
	Degraded bool `json:"-"`
}

func defaultConfig() Config {
	return Config{
		EnabledTools: map[string]bool{
			"read_file":      true,
			"write_file":     true,
			"target_edit":    true,
			"spawn_subagent": true,
			"bash":           true,
			"search_content": true,
			"find_files":     true,
			"create_todos":   true,
			"list_todos":     true,
			"finish_todo":    true,
		},
	}
}

func LoadConfig() (*Config, error) {
	lateConfigDir, err := pathutil.LateConfigDir()
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(lateConfigDir, "config.json")

	content, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Pre-populate with a default config that enables everything
			fallback := defaultConfig()
			defaultData, _ := json.MarshalIndent(fallback, "", "  ")

			// Ensure directory exists
			if err := os.MkdirAll(lateConfigDir, configDirPerm); err != nil {
				return &fallback, fmt.Errorf("failed to create config directory: %w", err)
			}

			if err := os.WriteFile(configPath, defaultData, configFilePerm); err != nil {
				return &fallback, fmt.Errorf("failed to write default config: %w", err)
			}

			if err := ensureSecureConfigPermissions(lateConfigDir, configPath); err != nil {
				return &fallback, err
			}

			return &fallback, nil
		}

		fallback := defaultConfig()
		fallback.Degraded = true
		return &fallback, fmt.Errorf("failed to read %s: %w", configPath, err)
	}

	permErr := ensureSecureConfigPermissions(lateConfigDir, configPath)

	var cfg Config
	if err := json.Unmarshal(content, &cfg); err != nil {
		fallback := defaultConfig()
		fallback.Degraded = true
		return &fallback, fmt.Errorf("failed to parse %s: %w", configPath, err)
	}

	if cfg.EnabledTools == nil {
		cfg.EnabledTools = defaultConfig().EnabledTools
	} else {
		// Merge missing defaults for backward compatibility
		// so existing config files get new tools automatically.
		defaults := defaultConfig().EnabledTools
		for k, v := range defaults {
			if _, exists := cfg.EnabledTools[k]; !exists {
				cfg.EnabledTools[k] = v
			}
		}
	}

	if permErr != nil {
		return &cfg, permErr
	}

	return &cfg, nil
}

func ResolveOpenAISettings(cfg *Config) OpenAISettings {
	return ResolveOpenAISettingsWithEnv(cfg, os.LookupEnv)
}

func ResolveOpenAISettingsWithEnv(cfg *Config, lookup EnvLookup) OpenAISettings {
	resolved := OpenAISettings{BaseURL: DefaultOpenAIBaseURL}

	if cfg != nil {
		if cfg.OpenAIBaseURL != "" {
			resolved.BaseURL = cfg.OpenAIBaseURL
		}
		resolved.APIKey = cfg.OpenAIAPIKey
		resolved.Model = cfg.OpenAIModel
	}

	if value, ok := nonEmptyEnv(lookup, "OPENAI_BASE_URL"); ok {
		resolved.BaseURL = value
	}
	if value, ok := nonEmptyEnv(lookup, "OPENAI_API_KEY"); ok {
		resolved.APIKey = value
	}
	if value, ok := nonEmptyEnv(lookup, "OPENAI_MODEL"); ok {
		resolved.Model = value
	}

	return resolved
}

func ResolveSubagentSettings(cfg *Config, openAI OpenAISettings) SubagentSettings {
	return ResolveSubagentSettingsWithEnv(cfg, openAI, os.LookupEnv)
}

func ResolveSubagentSettingsWithEnv(cfg *Config, openAI OpenAISettings, lookup EnvLookup) SubagentSettings {
	resolved := SubagentSettings(openAI)

	if cfg != nil {
		// Check legacy fields first
		if cfg.SubagentBaseURL != "" {
			resolved.BaseURL = cfg.SubagentBaseURL
		}
		if cfg.SubagentAPIKey != "" {
			resolved.APIKey = cfg.SubagentAPIKey
		}
		if cfg.SubagentModel != "" {
			resolved.Model = cfg.SubagentModel
		}

		// New fields override legacy fields
		if cfg.LateSubagentBaseURL != "" {
			resolved.BaseURL = cfg.LateSubagentBaseURL
		}
		if cfg.LateSubagentAPIKey != "" {
			resolved.APIKey = cfg.LateSubagentAPIKey
		}
		if cfg.LateSubagentModel != "" {
			resolved.Model = cfg.LateSubagentModel
		}
	}

	if value, ok := nonEmptyEnv(lookup, "LATE_SUBAGENT_BASE_URL"); ok {
		resolved.BaseURL = value
	}
	if value, ok := nonEmptyEnv(lookup, "LATE_SUBAGENT_API_KEY"); ok {
		resolved.APIKey = value
	}
	if value, ok := nonEmptyEnv(lookup, "LATE_SUBAGENT_MODEL"); ok {
		resolved.Model = value
	}

	return resolved
}

// ResolveSaveSubagentHistories determines whether subagent history
// persistence is enabled. Precedence: explicit CLI flag > saved session
// preference > config file. There is intentionally no environment-variable
// override.
func ResolveSaveSubagentHistories(cfg *Config, cliExplicit bool, cliValue bool, savedPreference *bool) bool {
	if cliExplicit {
		return cliValue
	}
	if savedPreference != nil {
		return *savedPreference
	}
	if cfg != nil {
		return cfg.SaveSubagentHistories
	}
	return false
}

// ResolvePermissionMode returns the effective permission mode.
// Precedence: exactly one explicitly-set CLI flag > config.json
// permission-mode entry > PermissionModeAskForUserApproval. The flags
// are mutually exclusive: setting more than one is an error. An
// unrecognized config.json value yields a warning and falls back to
// the safe default.
func ResolvePermissionMode(cfg *Config, askFlag, unsupervisedFlag, forceRevaluateFlag bool) (mode string, warning string, err error) {
	set := 0
	for _, v := range []bool{askFlag, unsupervisedFlag, forceRevaluateFlag} {
		if v {
			set++
		}
	}
	if set > 1 {
		return "", "", fmt.Errorf("permission flags are mutually exclusive; pass at most one of -%s, -%s, -%s",
			PermissionModeAskForUserApproval, PermissionModeUnsupervised, PermissionModeForceRevaluate)
	}
	switch {
	case askFlag:
		return PermissionModeAskForUserApproval, "", nil
	case unsupervisedFlag:
		return PermissionModeUnsupervised, "", nil
	case forceRevaluateFlag:
		return PermissionModeForceRevaluate, "", nil
	}
	if cfg != nil && cfg.PermissionMode != "" {
		switch cfg.PermissionMode {
		case PermissionModeAskForUserApproval, PermissionModeUnsupervised, PermissionModeForceRevaluate:
			return cfg.PermissionMode, "", nil
		default:
			return PermissionModeAskForUserApproval,
				fmt.Sprintf("ignoring invalid config.json permission-mode %q; using %q", cfg.PermissionMode, PermissionModeAskForUserApproval),
				nil
		}
	}
	return PermissionModeAskForUserApproval, "", nil
}

// ResolveSubagentTimeout resolves the global wall-clock budget for a single
// subagent run. Precedence: explicitly passed CLI flag > config.json
// "subagent_timeout" entry > DefaultSubagentTimeout (24h).
//
// A config value that parses via time.ParseDuration passes through as-is:
// "0" or a negative value means unlimited — callers guard with "> 0" and
// treat any non-positive budget as unlimited. An unparseable non-empty
// config value is ignored: the default is returned together with a warning
// for the caller to surface.
func ResolveSubagentTimeout(cfg *Config, cliExplicit bool, cliValue time.Duration) (time.Duration, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.SubagentTimeout != "" {
		if parsed, err := time.ParseDuration(cfg.SubagentTimeout); err == nil {
			return parsed, ""
		}
		return DefaultSubagentTimeout, fmt.Sprintf("ignoring invalid config.json subagent_timeout %q; using default 24h", cfg.SubagentTimeout)
	}
	return DefaultSubagentTimeout, ""
}

// ResolveShowTodoPane returns whether the todos side pane should be open
// when the TUI starts. The todos panel is open by default; set
// "show-todo-pane": false in config.json to start with it closed.
// An absent entry (nil pointer) resolves to true, an explicit false to
// false, and an explicit true to true. A nil config also resolves to the
// open default. Terminal width is not considered here: the TUI silently
// keeps the pane closed below 85 columns.
func (cfg *Config) ResolveShowTodoPane() bool {
	if cfg == nil || cfg.ShowTodoPane == nil {
		return true
	}
	return *cfg.ShowTodoPane
}

// ResolveCompactionThreshold returns the effective compaction threshold
// percentage and a warning string, mirroring ResolvePermissionMode's
// invalid-value pattern: 0 (unset) means DefaultCompactionThresholdPercent,
// values in 1-100 are honored as-is, and anything else falls back to the
// default with a warning.
func ResolveCompactionThreshold(cfg *Config) (threshold int, warning string) {
	if cfg == nil {
		return DefaultCompactionThresholdPercent, ""
	}
	if cfg.CompactionThresholdPercent >= 1 && cfg.CompactionThresholdPercent <= 100 {
		return cfg.CompactionThresholdPercent, ""
	}
	if cfg.CompactionThresholdPercent == 0 {
		return DefaultCompactionThresholdPercent, ""
	}
	return DefaultCompactionThresholdPercent,
		fmt.Sprintf("ignoring invalid config.json compaction-threshold-percent %d; using %d",
			cfg.CompactionThresholdPercent, DefaultCompactionThresholdPercent)
}

// IsValidCompactionMode reports whether mode is one of the accepted
// compaction-mode values (off, shadow, enabled).
func IsValidCompactionMode(mode string) bool {
	switch mode {
	case CompactionModeOff, CompactionModeShadow, CompactionModeEnabled:
		return true
	}
	return false
}

// ResolveCompactionMode returns the effective compaction mode from
// config.json and a warning string, mirroring ResolveCompactionThreshold's
// invalid-value pattern: empty (unset) means DefaultCompactionMode, valid
// values are honored as-is, and anything else falls back to the default with
// a warning. The --compaction-mode CLI flag overrides the resolved value and
// is validated with IsValidCompactionMode by the caller (main).
func ResolveCompactionMode(cfg *Config) (mode string, warning string) {
	if cfg == nil || cfg.CompactionMode == "" {
		return DefaultCompactionMode, ""
	}
	if IsValidCompactionMode(cfg.CompactionMode) {
		return cfg.CompactionMode, ""
	}
	return DefaultCompactionMode,
		fmt.Sprintf("ignoring invalid config.json compaction-mode %q; using %q",
			cfg.CompactionMode, DefaultCompactionMode)
}

// ResolveAutocompact returns the effective JEV auto-compaction switch and
// threshold percentage, mirroring ResolveCompactionThreshold's invalid-value
// pattern: percent 0 (unset) means DefaultJevAutocompactPercent, values in
// 1-100 are honored as-is, and anything else falls back to the default with
// a warning. The switch is a plain boolean: absent (false) means the
// auto-trigger never fires.
func ResolveAutocompact(cfg *Config) (enabled bool, percent int, warning string) {
	if cfg == nil {
		return false, DefaultJevAutocompactPercent, ""
	}
	percent = DefaultJevAutocompactPercent
	switch {
	case cfg.JevAutocompactPercent >= 1 && cfg.JevAutocompactPercent <= 100:
		percent = cfg.JevAutocompactPercent
	case cfg.JevAutocompactPercent == 0:
		// Unset: keep the default.
	default:
		warning = fmt.Sprintf("ignoring invalid config.json jev-autocompact-percent %d; using %d",
			cfg.JevAutocompactPercent, DefaultJevAutocompactPercent)
	}
	return cfg.JevAutocompact, percent, warning
}

// ResolveCompactionScoreThreshold resolves the elision score threshold: the
// score strictly below which segments are elided when compaction-mode is
// "enabled" (also the /jev-compact-context walk's keep threshold). It is a
// DIFFERENT knob from compaction-threshold-percent (the context-usage level
// the info bar reports headroom for) and from jev-autocompact-percent (the
// context-usage level that fires the auto-trigger).
//
// Precedence: an explicitly passed -compaction-threshold flag > config.json
// compaction-threshold > DefaultCompactionThreshold (0.35). The caller
// passes flagValue = 0 when the flag was NOT on the command line (the
// flag.Visit detection in main) — flag values are indistinguishable from
// defaults through the flag package alone, and 0 is itself invalid, so it
// doubles cleanly as the "not set" sentinel. Valid range is (0,1] for both
// sources: an explicitly passed but out-of-range flag warns and falls
// through to the config entry (or the default), and an out-of-range config
// value warns and falls back to the default.
func ResolveCompactionScoreThreshold(cfg *Config, flagValue float64) (threshold float64, warning string) {
	if flagValue > 0 && flagValue <= 1 {
		// Explicit, valid flag wins over everything.
		return flagValue, ""
	}
	if cfg != nil && cfg.CompactionThreshold > 0 && cfg.CompactionThreshold <= 1 {
		threshold = cfg.CompactionThreshold
	} else {
		threshold = DefaultCompactionThreshold
	}
	switch {
	case flagValue != 0:
		// Explicit but out of range: the caller meant to set it, so say so.
		warning = fmt.Sprintf("ignoring invalid -compaction-threshold %v; using %v", flagValue, threshold)
	case cfg != nil && (cfg.CompactionThreshold < 0 || cfg.CompactionThreshold > 1):
		// Present but invalid. 0 is the silent unset (see the Config doc);
		// anything else outside (0,1] warns.
		warning = fmt.Sprintf("ignoring invalid config.json compaction-threshold %v; using %v", cfg.CompactionThreshold, threshold)
	}
	return threshold, warning
}

// ResolveCompactionMaxElidePercent returns the effective compaction
// elide-fraction tripwire percentage and a warning string, mirroring
// ResolveCompactionThreshold's invalid-value pattern: 0 (unset) means
// DefaultCompactionMaxElidePercent, values in 1-100 are honored as-is, and
// anything else falls back to the default with a warning.
func ResolveCompactionMaxElidePercent(cfg *Config) (percent int, warning string) {
	if cfg == nil {
		return DefaultCompactionMaxElidePercent, ""
	}
	if cfg.CompactionMaxElidePercent >= 1 && cfg.CompactionMaxElidePercent <= 100 {
		return cfg.CompactionMaxElidePercent, ""
	}
	if cfg.CompactionMaxElidePercent == 0 {
		return DefaultCompactionMaxElidePercent, ""
	}
	return DefaultCompactionMaxElidePercent,
		fmt.Sprintf("ignoring invalid config.json compaction-max-elide-percent %d; using %d",
			cfg.CompactionMaxElidePercent, DefaultCompactionMaxElidePercent)
}

// ResolveCompactionProtectedFloor returns the effective score floor (as a
// percentage) under which protected segment kinds may be elided, mirroring
// ResolveCompactionThreshold's invalid-value pattern: 0 (unset) means
// DefaultCompactionProtectedFloorPercent, values in 1-100 are honored
// as-is, and anything else falls back to the default with a warning.
func ResolveCompactionProtectedFloor(cfg *Config) (percent int, warning string) {
	if cfg == nil {
		return DefaultCompactionProtectedFloorPercent, ""
	}
	if cfg.CompactionProtectedFloor >= 1 && cfg.CompactionProtectedFloor <= 100 {
		return cfg.CompactionProtectedFloor, ""
	}
	if cfg.CompactionProtectedFloor == 0 {
		return DefaultCompactionProtectedFloorPercent, ""
	}
	return DefaultCompactionProtectedFloorPercent,
		fmt.Sprintf("ignoring invalid config.json compaction-protected-floor %d; using %d",
			cfg.CompactionProtectedFloor, DefaultCompactionProtectedFloorPercent)
}

// IsValidCompactionBackend reports whether backend is one of the accepted
// compaction-backend values (offline).
func IsValidCompactionBackend(backend string) bool {
	return backend == CompactionBackendOffline
}

// ResolveCompactionBackend returns the effective compaction-backend value
// from config.json plus a warning, mirroring ResolveCompactionMode's
// invalid-value pattern. Precedence: a set config value WINS — scoring goes
// exactly where the user pointed it, and the env-based backend resolution
// (JEV_API / auto-detection inside compaction.ResolveBackendEnv) is only
// consulted when this entry is ABSENT. So:
//
//   - ("", "") — unset (or a nil config): the caller resolves a backend
//     from the environment as before.
//   - ("offline", "") — the offline scripted scorer; the caller builds the
//     offline pipeline and never resolves a backend or needs a key.
//   - ("", warning) — an unrecognized value: the config entry is ignored
//     with the warning, and the caller falls back to the env-based
//     resolution.
//
// The warning names the config key so the user can fix the typo in
// config.json rather than guess which entry was rejected.
func ResolveCompactionBackend(cfg *Config) (backend string, warning string) {
	if cfg == nil || cfg.CompactionBackend == "" {
		return "", ""
	}
	if IsValidCompactionBackend(cfg.CompactionBackend) {
		return cfg.CompactionBackend, ""
	}
	return "", fmt.Sprintf("ignoring invalid config.json compaction-backend %q (known values: %s); resolving the backend from the environment",
		cfg.CompactionBackend, CompactionBackendOffline)
}

// ResolveCompactionRetrieval returns whether the compaction store's
// retrieve() read side is enabled, mirroring ResolveAutocompact's resolver
// shape (value plus warning). The switch is a plain boolean defaulting to
// false: absent means retrieval never runs. A bool config entry has no
// invalid VALUE — a wrong-typed config.json value fails the whole parse and
// is surfaced by the degraded-config guard — so the warning return carries
// the one invalid COMBINATION instead: retrieval enabled while
// compaction-mode is not "enabled". The record store only fills when
// relocation runs (mode "enabled"), so in shadow or off mode retrieval
// would score an eternally empty store and inject nothing; the resolver
// still honors the switch (harmless no-op) and lets the warning explain.
func ResolveCompactionRetrieval(cfg *Config) (enabled bool, warning string) {
	if cfg == nil || !cfg.CompactionRetrieval {
		return false, ""
	}
	mode, _ := ResolveCompactionMode(cfg)
	if mode != CompactionModeEnabled {
		return true, fmt.Sprintf("config.json compaction-retrieval has no effect while compaction-mode is %q: the record store only fills in %q mode",
			mode, CompactionModeEnabled)
	}
	return true, ""
}

func nonEmptyEnv(lookup EnvLookup, key string) (string, bool) {
	if lookup == nil {
		return "", false
	}

	value, ok := lookup(key)
	if !ok || value == "" {
		return "", false
	}

	return value, true
}

func ensureSecureConfigPermissions(configDir, configPath string) error {
	if runtime.GOOS == "windows" {
		return nil
	}

	if err := tightenPermission(configDir, configDirPerm); err != nil {
		return fmt.Errorf("failed to set config directory permissions: %w", err)
	}

	if err := tightenPermission(configPath, configFilePerm); err != nil {
		return fmt.Errorf("failed to set config file permissions: %w", err)
	}

	return nil
}

func tightenPermission(path string, required os.FileMode) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.Mode().Perm() == required {
		return nil
	}

	return os.Chmod(path, required)
}

// GetModelForAgent returns the ModelSetting for a given agent type.
// If not found, it returns false.
func (cfg *Config) GetModelForAgent(agentType string) (ModelSetting, bool) {
	if cfg == nil || cfg.AgentModels == nil || cfg.Models == nil {
		return ModelSetting{}, false
	}
	modelRef, exists := cfg.AgentModels[agentType]
	if !exists {
		return ModelSetting{}, false
	}
	// Prefer stable IDs so providers exposing the same model name remain
	// distinguishable.
	for _, m := range cfg.Models {
		if m.ID != "" && m.ID == modelRef {
			return m, true
		}
	}
	// Backward compatibility for existing name-based agent_models entries.
	for _, m := range cfg.Models {
		if m.Model == modelRef {
			return m, true
		}
	}
	return ModelSetting{}, false
}

// SaveConfig atomically writes the configuration back to config.json.
// A degraded config (loaded from an invalid config.json) is never saved:
// the caller must fix or remove the file first, so a fallback default can
// never clobber the user's hand-edited config.
func SaveConfig(cfg *Config) error {
	if cfg != nil && cfg.Degraded {
		return fmt.Errorf("refusing to save config: it was loaded from an invalid config.json; fix or remove the file first")
	}
	lateConfigDir, err := pathutil.LateConfigDir()
	if err != nil {
		return err
	}
	if err := tightenConfigDirPermission(lateConfigDir); err != nil {
		return err
	}

	configPath := filepath.Join(lateConfigDir, "config.json")
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(lateConfigDir, ".config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temporary config: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if err := tmpFile.Chmod(configFilePerm); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to secure temporary config: %w", err)
	}
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to write temporary config: %w", err)
	}
	if err := tmpFile.Sync(); err != nil {
		tmpFile.Close()
		return fmt.Errorf("failed to sync temporary config: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("failed to close temporary config: %w", err)
	}
	if err := os.Rename(tmpPath, configPath); err != nil {
		return fmt.Errorf("failed to replace config: %w", err)
	}
	return ensureSecureConfigPermissions(lateConfigDir, configPath)
}

func tightenConfigDirPermission(configDir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := tightenPermission(configDir, configDirPerm); err != nil {
		return fmt.Errorf("failed to set config directory permissions: %w", err)
	}
	return nil
}
