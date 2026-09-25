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

// DefaultBashTimeout is the default wall-clock budget for a single bash tool
// call, applied when neither the --bash-timeout flag nor the config.json
// "bash-timeout" entry provides a value. "0" (or negative) means unlimited.
const DefaultBashTimeout = 10 * time.Minute

// DefaultSubagentIdleTimeout is the default "truly idle" notification
// threshold for the subagent idle watchdog, applied when neither the
// --subagent-idle-timeout flag nor the config.json "subagent-idle-timeout"
// entry provides a value. "0" means off.
const DefaultSubagentIdleTimeout = 15 * time.Minute

// DefaultSubagentMaxTurns is the default maximum number of turns per
// subagent, applied when neither the --subagent-max-turns flag nor the
// config.json "subagent-max-turns" entry provides a value. 0 means
// unlimited.
const DefaultSubagentMaxTurns = 500

// DefaultMaxConcurrentLLMRequests is the default process-wide cap on
// concurrent in-flight LLM requests, applied when neither the
// --max-concurrent-llm-requests flag nor the config.json
// "max-concurrent-llm-requests" entry provides a value. 0 means unlimited.
const DefaultMaxConcurrentLLMRequests = 6

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

	// JevAutocompactPercent is the per-model override of the global
	// autocompact trigger: different models have different context sizes, so
	// the same "compact at N% of the window" level is not right for every
	// model. It reuses the top-level jev-autocompact-percent key name inside
	// the entry. 0/unset = use the global; values 1-100 are honored and win
	// over the global for every agent whose agent_models entry routes to
	// this model (see Config.AutocompactPercentForAgent); out-of-range
	// values warn at startup and are ignored (see Config.AutocompactWarnings).
	JevAutocompactPercent int `json:"jev-autocompact-percent,omitempty"`
}

// Reference returns the stable value stored in agent_models. Model is retained
// as a fallback for configurations created before model IDs were introduced.
func (m ModelSetting) Reference() string {
	if m.ID != "" {
		return m.ID
	}
	return m.Model
}

// AutocompactPercentOverride reports the entry's per-model autocompact
// trigger override. The second return is true only for a valid percentage
// (1-100): 0/unset means "no override — use the global", and an out-of-range
// value is ignored with a startup warning (Config.AutocompactWarnings), so
// both resolve to the global threshold.
func (m ModelSetting) AutocompactPercentOverride() (int, bool) {
	if m.JevAutocompactPercent >= 1 && m.JevAutocompactPercent <= 100 {
		return m.JevAutocompactPercent, true
	}
	return 0, false
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
	// The value is a FlexBool, so config.json accepts the on/off synonyms
	// ("yes", "on", 1, ...) alongside true/false.
	SaveSubagentHistories FlexBool `json:"save_subagent_histories,omitempty"`

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

	// ----------------------------------------------------------------------
	// CLI-equivalent settings (flag > config > default)
	//
	// Every field in this section mirrors a command-line flag one-to-one:
	// the JSON key is the flag name in kebab-case, exactly as the user
	// passes it (`--option-name <VALUE>` becomes "option-name": <VALUE>).
	// Resolution follows the mandatory precedence: an explicitly passed CLI
	// flag (detected via flag.Visit in main, since config.json loads after
	// flag.Parse) wins over the config entry, which wins over the built-in
	// default. Each field's doc comment states its flag equivalent, its
	// default, and its unset/zero semantics; the matching Resolve* function
	// in this file implements the precedence and is the only supported way
	// to read the setting.
	// ----------------------------------------------------------------------

	// SystemPrompt replaces the built-in system prompt with this text.
	// Flag: --system-prompt. Empty (unset) keeps the built-in prompt.
	// Priority (identical to the flags): system-prompt-file >
	// system-prompt > LATE_SYSTEM_PROMPT env > built-in prompt.
	SystemPrompt string `json:"system-prompt,omitempty"`

	// SystemPromptFile replaces the built-in system prompt with the
	// contents of this file. Flag: --system-prompt-file. Empty (unset)
	// keeps the built-in prompt. An unreadable path is a hard error, the
	// same as for the flag.
	SystemPromptFile string `json:"system-prompt-file,omitempty"`

	// AppendSystemPrompt is appended to the final system prompt (after any
	// replacement above). Flag: --append-system-prompt. Empty (unset)
	// appends nothing.
	AppendSystemPrompt string `json:"append-system-prompt,omitempty"`

	// InjectCWD replaces ${{CWD}} in the system prompt with the working
	// directory. Flag: --inject-cwd. Default true. *FlexBool tri-state: nil
	// (absent entry) = unset → default, so an explicit false is
	// distinguishable from unset (mirrors ShowTodoPane). FlexBool values
	// accept the on/off synonyms (true/on/enabled/yes/1, ...).
	InjectCWD *FlexBool `json:"inject-cwd,omitempty"`

	// GemmaThinking prepends the Gemma <|think|> token to the system
	// prompt. Flag: --gemma-thinking. Default false; a plain FlexBool
	// suffices because the default is false.
	GemmaThinking FlexBool `json:"gemma-thinking,omitempty"`

	// UseTools offers tools to the main agent at all. Flag: --use-tools.
	// Default true. *FlexBool tri-state: nil (absent entry) = unset →
	// default.
	UseTools *FlexBool `json:"use-tools,omitempty"`

	// EnableBash enables the bash tool. Flag: --enable-bash. Default
	// true. *FlexBool tri-state: nil (absent entry) = unset → default. This
	// is the MASTER switch: config.json enabled_tools.bash provides per-tool
	// granularity and is ANDed with it — either being false disables the
	// bash tool (see ResolveEnableBash).
	EnableBash *FlexBool `json:"enable-bash,omitempty"`

	// BashTimeout is the max wall-clock time for one bash tool call.
	// Flag: --bash-timeout. Duration STRING parsed with
	// time.ParseDuration (e.g. "10m", "1h30m"); empty (unset) means
	// DefaultBashTimeout (10m); "0" (or negative) means unlimited.
	BashTimeout string `json:"bash-timeout,omitempty"`

	// EnableSqz compresses bash tool output with the external 'sqz' binary
	// when it is available. Flag: --enable-sqz. Default false.
	EnableSqz FlexBool `json:"enable-sqz,omitempty"`

	// EnableImages force-enables image attachments even when the backend
	// does not advertise vision support. Flag: --enable-images.
	// Default false.
	EnableImages FlexBool `json:"enable-images,omitempty"`

	// EnableSubagents allows the agent to spawn subagents.
	// Flag: --enable-subagents. Default true. *FlexBool tri-state: nil
	// (absent entry) = unset → default.
	EnableSubagents *FlexBool `json:"enable-subagents,omitempty"`

	// SubagentMaxTurns is the maximum number of turns per subagent.
	// Flag: --subagent-max-turns. Default DefaultSubagentMaxTurns (500).
	// *int tri-state: nil (absent entry) = unset → default; 0 = unlimited
	// (the executor treats maxTurns <= 0 as unbounded, exactly like the
	// flag); a negative value is invalid, warns, and falls back to the
	// default (see ResolveSubagentMaxTurns).
	SubagentMaxTurns *int `json:"subagent-max-turns,omitempty"`

	// SubagentIdleTimeout notifies when a subagent has been truly idle (no
	// stream progress, no in-flight tool, no nested spawn) for this long.
	// Flag: --subagent-idle-timeout. Duration STRING parsed with
	// time.ParseDuration; empty (unset) means DefaultSubagentIdleTimeout
	// (15m); "0" means off.
	SubagentIdleTimeout string `json:"subagent-idle-timeout,omitempty"`

	// SubagentIdleKillAfter kills a subagent that stays truly idle past
	// this duration. Flag: --subagent-idle-kill-after. Duration STRING
	// parsed with time.ParseDuration; empty (unset) means the built-in
	// default (0 = notify only, never kill); "0" means notify only.
	SubagentIdleKillAfter string `json:"subagent-idle-kill-after,omitempty"`

	// MaxStreamRetries is the retry budget for LLM stream errors; 0
	// disables retrying. Flag: --max-stream-retries. *int tri-state: nil
	// (absent entry) = unset → the LATE_MAX_STREAM_RETRIES env value, or
	// executor.DefaultMaxStreamRetries when the env is unset.
	// Precedence: flag > env > config > default (see
	// ResolveMaxStreamRetries).
	MaxStreamRetries *int `json:"max-stream-retries,omitempty"`

	// MaxConcurrentLLMRequests is the process-wide cap on concurrent
	// in-flight LLM requests across all agents and subagents.
	// Flag: --max-concurrent-llm-requests. Default
	// DefaultMaxConcurrentLLMRequests (6). *int tri-state: nil (absent
	// entry) = unset → default; 0 = unlimited; negative is invalid, warns,
	// and falls back to the default (see ResolveMaxConcurrentLLMRequests).
	MaxConcurrentLLMRequests *int `json:"max-concurrent-llm-requests,omitempty"`

	// SuppressThinkingWords biases anti-overthinking tokens (requires the
	// same model for the main agent and subagents). Flag:
	// --suppress-thinking-words. Default false.
	SuppressThinkingWords FlexBool `json:"suppress-thinking-words,omitempty"`

	// LogitBias is the main-agent token bias, in the same format the
	// --logit-bias flag accepts: a JSON object or comma-separated
	// TOKEN_ID:BIAS pairs. Empty (unset) sends no bias. Parsed by the
	// caller with client.ParseLogitBias; the resolver is a pass-through.
	LogitBias string `json:"logit-bias,omitempty"`

	// SubagentLogitBias is the subagent token bias, in the same format the
	// --subagent-logit-bias flag accepts. Empty (unset) sends no bias.
	SubagentLogitBias string `json:"subagent-logit-bias,omitempty"`

	// ShowCWD shows the git branch / working directory in the status bar.
	// Flag: --show-cwd. Default true. *FlexBool tri-state: nil (absent
	// entry) = unset → default.
	ShowCWD *FlexBool `json:"show-cwd,omitempty"`

	// Legacy subagent fields for backward compatibility
	SubagentBaseURL string `json:"subagent_base_url,omitempty"`
	SubagentAPIKey  string `json:"subagent_api_key,omitempty"`
	SubagentModel   string `json:"subagent_model,omitempty"`

	SkillsDir string `json:"skills_dir,omitempty"`

	// ShowTodoPane controls whether the todos side pane starts open.
	// The todos panel is open by default; set false to start with it
	// closed. Terminals narrower than 85 columns always start with the
	// pane closed; it can be opened later with /todos.
	ShowTodoPane *FlexBool `json:"show-todo-pane,omitempty"`

	Theme       string            `json:"theme,omitempty"`
	Models      []ModelSetting    `json:"models,omitempty"`
	AgentModels map[string]string `json:"agent_models,omitempty"`

	// ShowInfoBar toggles the single-line info footer rendered below the
	// TUI status bar (version, model, context usage, uptime, ...). Toggled
	// at runtime with the /infobar slash command, which persists the new
	// value back to config.json.
	ShowInfoBar FlexBool `json:"show-info-bar,omitempty"`

	// ShowTimestamps toggles the [HH:MM:SS] prefix rendered at the start
	// of each transcript message block. Toggled at runtime with the
	// /timestamps slash command, which persists the new value back to
	// config.json.
	ShowTimestamps FlexBool `json:"show-timestamps,omitempty"`

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
	JevAutocompact FlexBool `json:"jev-autocompact,omitempty"`

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
	CompactionRetrieval FlexBool `json:"compaction-retrieval,omitempty"`

	// Degraded is defense-in-depth for SaveConfig: a config loaded from an
	// invalid config.json must never be persisted, because it is a
	// fallback default, not the user's real settings. Since the strict
	// config rework, LoadConfig returns a nil config together with a
	// rendered ConfigParseError for every content problem and main exits
	// before the TUI starts (strict-config rule R2/R3), so NO normal
	// startup path sets this flag anymore — it exists so that a future
	// caller that hands a partially-loaded config to SaveConfig still
	// cannot clobber the user's hand-edited config.json. It is never
	// serialized (json:"-").
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

// LoadConfig loads and strictly parses config.json.
//
// Behavior contract:
//
//   - Missing file (fresh install): a default config is written and returned
//     with a nil error — late starts normally.
//   - Any content problem (JSON syntax error, unknown top-level entry,
//     wrong-typed value, invalid enum value, invalid boolean synonym) is
//     FATAL: a rendered *ConfigParseError naming the exact file, line, and
//     column is returned together with a nil config, and the caller (main)
//     must print it and exit instead of starting on fallback defaults
//     (strict-config rules R2/R3 — no fallback-to-defaults startup).
//   - The file existing but being unreadable, or the config directory being
//     uncreatable, is likewise fatal (nil config, non-nil error).
//   - A failed post-load permission hardening is NOT fatal: the config
//     content is fully valid, so it is returned together with the
//     permission error for the caller to surface as a warning.
func LoadConfig() (*Config, error) {
	lateConfigDir, err := pathutil.LateConfigDir()
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(lateConfigDir, "config.json")

	content, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Fresh install: pre-populate with a default config that
			// enables everything. A missing file is not a config error.
			fallback := defaultConfig()
			defaultData, _ := json.MarshalIndent(fallback, "", "  ")

			// Ensure directory exists
			if err := os.MkdirAll(lateConfigDir, configDirPerm); err != nil {
				return nil, fmt.Errorf("failed to create config directory: %w", err)
			}

			if err := os.WriteFile(configPath, defaultData, configFilePerm); err != nil {
				return nil, fmt.Errorf("failed to write default config: %w", err)
			}

			if err := ensureSecureConfigPermissions(lateConfigDir, configPath); err != nil {
				// The default config was written successfully; a failed
				// permission hardening must not abort the fresh install.
				return &fallback, err
			}

			return &fallback, nil
		}

		// The file exists but cannot be read (e.g. it is a directory).
		// Starting on fallback defaults would silently ignore every user
		// setting, so this is fatal under the strict-config rules.
		return nil, fmt.Errorf("failed to read %s: %w", configPath, err)
	}

	permErr := ensureSecureConfigPermissions(lateConfigDir, configPath)

	cfg, err := parseConfigContent(configPath, content)
	if err != nil {
		// Strict config: a broken config.json never yields a fallback
		// config. The caller must render the error and abort.
		return nil, err
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
		// Permission hardening failed, but the config content is fully
		// valid: return it so the caller can warn and continue.
		return cfg, permErr
	}

	return cfg, nil
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
		return cfg.SaveSubagentHistories.Bool()
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

// -------------------------------------------------------------------------
// CLI-equivalent setting resolvers (flag > config > default)
//
// Each resolver below implements the mandatory precedence for one setting
// that mirrors a command-line flag: an explicitly passed flag wins over the
// config.json entry, which wins over the built-in default. The caller (main)
// passes cliExplicit = true only when flag.Visit reported the flag on the
// command line — config loads after flag.Parse, so that is the only reliable
// explicit-flag signal — together with the parsed flag value. Every resolver
// returns the effective value plus an optional warning for the caller to
// surface; a nil cfg behaves like an absent entry. Boolean and plain-string
// settings have no invalid VALUE (a wrong-typed config.json entry fails the
// whole parse with a line/column error and aborts startup — strict-config
// rules R2/R3), so their warning return is always empty.
// -------------------------------------------------------------------------

// resolveDurationString is the shared body of the duration-string resolvers
// (mirrors ResolveSubagentTimeout's parsing rules): raw is the config entry;
// empty means unset and the default applies; a value that parses via
// time.ParseDuration passes through as-is (callers define non-positive
// semantics — unlimited or off); an unparseable non-empty value warns and
// falls back to the default. key is the config.json key named in warnings.
func resolveDurationString(raw string, cliExplicit bool, cliValue, def time.Duration, key string) (time.Duration, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil {
			return parsed, ""
		}
		return def, fmt.Sprintf("ignoring invalid config.json %s %q; using default %s", key, raw, def)
	}
	return def, ""
}

// ResolveSystemPrompt resolves the system-prompt replacement text
// (flag: --system-prompt). Precedence: explicitly passed flag (even empty,
// so -system-prompt="" can undo a config entry for one run) > config.json
// "system-prompt" > "" (keep the built-in prompt).
func ResolveSystemPrompt(cfg *Config, cliExplicit bool, cliValue string) (string, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.SystemPrompt != "" {
		return cfg.SystemPrompt, ""
	}
	return "", ""
}

// ResolveSystemPromptFile resolves the system-prompt replacement file path
// (flag: --system-prompt-file). Precedence: explicitly passed flag >
// config.json "system-prompt-file" > "" (no replacement). The caller reads
// the file and keeps the flag's hard-error behavior for an unreadable path.
func ResolveSystemPromptFile(cfg *Config, cliExplicit bool, cliValue string) (string, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.SystemPromptFile != "" {
		return cfg.SystemPromptFile, ""
	}
	return "", ""
}

// ResolveAppendSystemPrompt resolves the text appended to the final system
// prompt (flag: --append-system-prompt). Precedence: explicitly passed flag
// > config.json "append-system-prompt" > "" (append nothing). Appending
// always happens after the file/text replacement resolution — the same
// order the flags use.
func ResolveAppendSystemPrompt(cfg *Config, cliExplicit bool, cliValue string) (string, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.AppendSystemPrompt != "" {
		return cfg.AppendSystemPrompt, ""
	}
	return "", ""
}

// ResolveUseTools resolves whether tools are offered to the main agent at
// all (flag: --use-tools). Precedence: explicitly passed flag >
// config.json "use-tools" > true. The config entry is a *FlexBool: nil
// (absent) = unset; values accept the on/off synonyms.
func ResolveUseTools(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.UseTools != nil {
		return cfg.UseTools.Bool(), ""
	}
	return true, ""
}

// ResolveEnableBash resolves the bash tool's MASTER switch (flag:
// --enable-bash). Precedence: explicitly passed flag > config.json
// "enable-bash" > true. The config entry is a *FlexBool: nil (absent) =
// unset. enabled_tools.bash in config.json provides per-tool granularity and
// is ANDed with this switch by the caller — either being false disables the
// bash tool, exactly as the flag's false always has.
func ResolveEnableBash(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.EnableBash != nil {
		return cfg.EnableBash.Bool(), ""
	}
	return true, ""
}

// ResolveInjectCWD resolves whether ${{CWD}} is replaced with the working
// directory in the system prompt (flag: --inject-cwd). Precedence:
// explicitly passed flag > config.json "inject-cwd" > true. The config
// entry is a *FlexBool: nil (absent) = unset.
func ResolveInjectCWD(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.InjectCWD != nil {
		return cfg.InjectCWD.Bool(), ""
	}
	return true, ""
}

// ResolveEnableSubagents resolves whether the agent may spawn subagents
// (flag: --enable-subagents). Precedence: explicitly passed flag >
// config.json "enable-subagents" > true. The config entry is a *FlexBool:
// nil (absent) = unset.
func ResolveEnableSubagents(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.EnableSubagents != nil {
		return cfg.EnableSubagents.Bool(), ""
	}
	return true, ""
}

// ResolveShowCWD resolves whether the status bar shows the git branch /
// working directory (flag: --show-cwd). Precedence: explicitly passed flag >
// config.json "show-cwd" > true. The config entry is a *FlexBool: nil
// (absent) = unset.
func ResolveShowCWD(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.ShowCWD != nil {
		return cfg.ShowCWD.Bool(), ""
	}
	return true, ""
}

// ResolveGemmaThinking resolves whether the Gemma <|think|> token is
// prepended to the system prompt (flag: --gemma-thinking). Precedence:
// explicitly passed flag > config.json "gemma-thinking" > false.
func ResolveGemmaThinking(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	return cfg != nil && cfg.GemmaThinking.Bool(), ""
}

// ResolveEnableSqz resolves whether bash tool output is compressed with the
// external 'sqz' binary when available (flag: --enable-sqz). Precedence:
// explicitly passed flag > config.json "enable-sqz" > false.
func ResolveEnableSqz(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	return cfg != nil && cfg.EnableSqz.Bool(), ""
}

// ResolveEnableImages resolves whether image attachments are force-enabled
// even when the backend does not advertise vision support (flag:
// --enable-images). Precedence: explicitly passed flag > config.json
// "enable-images" > false.
func ResolveEnableImages(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	return cfg != nil && cfg.EnableImages.Bool(), ""
}

// ResolveSuppressThinkingWords resolves whether anti-overthinking tokens
// are biased (flag: --suppress-thinking-words; requires the same model for
// the main agent and subagents, which the caller validates). Precedence:
// explicitly passed flag > config.json "suppress-thinking-words" > false.
func ResolveSuppressThinkingWords(cfg *Config, cliExplicit bool, cliValue bool) (bool, string) {
	if cliExplicit {
		return cliValue, ""
	}
	return cfg != nil && cfg.SuppressThinkingWords.Bool(), ""
}

// ResolveBashTimeout resolves the max wall-clock time for one bash tool
// call (flag: --bash-timeout). Precedence: explicitly passed flag >
// config.json "bash-timeout" > DefaultBashTimeout (10m). The config entry
// is a time.ParseDuration string ("10m", "1h30m"); "0" (or negative)
// passes through — the shell tool treats any non-positive timeout as
// unlimited, exactly as for the flag.
func ResolveBashTimeout(cfg *Config, cliExplicit bool, cliValue time.Duration) (time.Duration, string) {
	var raw string
	if cfg != nil {
		raw = cfg.BashTimeout
	}
	return resolveDurationString(raw, cliExplicit, cliValue, DefaultBashTimeout, "bash-timeout")
}

// ResolveSubagentIdleTimeout resolves the "truly idle" notification
// threshold of the subagent idle watchdog (flag: --subagent-idle-timeout).
// Precedence: explicitly passed flag > config.json "subagent-idle-timeout" >
// DefaultSubagentIdleTimeout (15m). The config entry is a
// time.ParseDuration string; "0" (or negative) passes through — the
// watchdog treats a non-positive threshold as off.
func ResolveSubagentIdleTimeout(cfg *Config, cliExplicit bool, cliValue time.Duration) (time.Duration, string) {
	var raw string
	if cfg != nil {
		raw = cfg.SubagentIdleTimeout
	}
	return resolveDurationString(raw, cliExplicit, cliValue, DefaultSubagentIdleTimeout, "subagent-idle-timeout")
}

// ResolveSubagentIdleKillAfter resolves the sustained-idle duration past
// which an idle subagent is killed (flag: --subagent-idle-kill-after).
// Precedence: explicitly passed flag > config.json
// "subagent-idle-kill-after" > 0 (notify only, never kill). The config
// entry is a time.ParseDuration string; "0" passes through as notify-only.
func ResolveSubagentIdleKillAfter(cfg *Config, cliExplicit bool, cliValue time.Duration) (time.Duration, string) {
	var raw string
	if cfg != nil {
		raw = cfg.SubagentIdleKillAfter
	}
	return resolveDurationString(raw, cliExplicit, cliValue, 0, "subagent-idle-kill-after")
}

// ResolveSubagentMaxTurns resolves the maximum number of turns per subagent
// (flag: --subagent-max-turns). Precedence: explicitly passed flag >
// config.json "subagent-max-turns" > DefaultSubagentMaxTurns (500). The
// config entry is a *int: nil (absent) = unset; 0 = unlimited (the
// executor treats maxTurns <= 0 as unbounded, exactly like the flag); a
// negative value is invalid, warns, and falls back to the default.
func ResolveSubagentMaxTurns(cfg *Config, cliExplicit bool, cliValue int) (int, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.SubagentMaxTurns != nil {
		if v := *cfg.SubagentMaxTurns; v >= 0 {
			return v, ""
		}
		return DefaultSubagentMaxTurns,
			fmt.Sprintf("ignoring invalid config.json subagent-max-turns %d; using default %d", *cfg.SubagentMaxTurns, DefaultSubagentMaxTurns)
	}
	return DefaultSubagentMaxTurns, ""
}

// ResolveMaxStreamRetries resolves the LLM stream-error retry budget (flag:
// --max-stream-retries; 0 disables). Precedence: explicitly passed flag >
// LATE_MAX_STREAM_RETRIES env > config.json "max-stream-retries" >
// executor.DefaultMaxStreamRetries. The caller passes the env layer
// pre-resolved: envSet is true only when the env var is present and parses
// as an integer (main warns and ignores an unparseable value), and envValue
// is then the parsed budget — executor.DefaultMaxStreamRetries when the
// env is unset. The config entry is a *int: nil (absent) = unset; 0 =
// disabled; a negative value is invalid, warns, and falls back to envValue.
func ResolveMaxStreamRetries(cfg *Config, cliExplicit bool, cliValue int, envSet bool, envValue int) (int, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if envSet {
		return envValue, ""
	}
	if cfg != nil && cfg.MaxStreamRetries != nil {
		if v := *cfg.MaxStreamRetries; v >= 0 {
			return v, ""
		}
		return envValue,
			fmt.Sprintf("ignoring invalid config.json max-stream-retries %d; using %d", *cfg.MaxStreamRetries, envValue)
	}
	return envValue, ""
}

// ResolveMaxConcurrentLLMRequests resolves the process-wide cap on
// concurrent in-flight LLM requests (flag: --max-concurrent-llm-requests).
// Precedence: explicitly passed flag > config.json
// "max-concurrent-llm-requests" > DefaultMaxConcurrentLLMRequests (6). The
// config entry is a *int: nil (absent) = unset; 0 = unlimited; a negative
// value is invalid, warns, and falls back to the default.
func ResolveMaxConcurrentLLMRequests(cfg *Config, cliExplicit bool, cliValue int) (int, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.MaxConcurrentLLMRequests != nil {
		if v := *cfg.MaxConcurrentLLMRequests; v >= 0 {
			return v, ""
		}
		return DefaultMaxConcurrentLLMRequests,
			fmt.Sprintf("ignoring invalid config.json max-concurrent-llm-requests %d; using default %d", *cfg.MaxConcurrentLLMRequests, DefaultMaxConcurrentLLMRequests)
	}
	return DefaultMaxConcurrentLLMRequests, ""
}

// ResolveLogitBias resolves the main-agent token bias string (flag:
// --logit-bias). Precedence: explicitly passed flag > config.json
// "logit-bias" > "" (no bias). The value is a pass-through in the same
// format the flag accepts (JSON object or comma-separated TOKEN_ID:BIAS
// pairs); the caller parses it with client.ParseLogitBias and decides the
// error handling per source.
func ResolveLogitBias(cfg *Config, cliExplicit bool, cliValue string) (string, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.LogitBias != "" {
		return cfg.LogitBias, ""
	}
	return "", ""
}

// ResolveSubagentLogitBias resolves the subagent token bias string (flag:
// --subagent-logit-bias). Precedence: explicitly passed flag > config.json
// "subagent-logit-bias" > "" (no bias). Pass-through, like ResolveLogitBias.
func ResolveSubagentLogitBias(cfg *Config, cliExplicit bool, cliValue string) (string, string) {
	if cliExplicit {
		return cliValue, ""
	}
	if cfg != nil && cfg.SubagentLogitBias != "" {
		return cfg.SubagentLogitBias, ""
	}
	return "", ""
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
	return cfg.ShowTodoPane.Bool()
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
	return cfg.JevAutocompact.Bool(), percent, warning
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
// false: absent means retrieval never runs. A boolean config entry has no
// invalid VALUE — a wrong-typed or nonsensical config.json value fails the
// whole parse with a line/column error and aborts startup (strict-config
// rules R2/R3) — so the warning return carries the one invalid COMBINATION
// instead: retrieval enabled while
// compaction-mode is not "enabled". The record store only fills when
// relocation runs (mode "enabled"), so in shadow or off mode retrieval
// would score an eternally empty store and inject nothing; the resolver
// still honors the switch (harmless no-op) and lets the warning explain.
func ResolveCompactionRetrieval(cfg *Config) (enabled bool, warning string) {
	if cfg == nil || !cfg.CompactionRetrieval.Bool() {
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

// AutocompactWarnings returns one warning per models[] entry whose
// jev-autocompact-percent value parses (otherwise the strict parse aborts)
// but is outside the valid 1-100 range. The global threshold applies for that
// model until the entry is fixed, exactly like an out-of-range global
// jev-autocompact-percent warns and falls back to the default. The warnings
// name the model entry (its id, or its model name when no id is set) and the
// bad value so the user can locate the offending line. A nil config has no
// entries and returns nil.
func (cfg *Config) AutocompactWarnings() []string {
	if cfg == nil {
		return nil
	}
	var warnings []string
	for _, m := range cfg.Models {
		v := m.JevAutocompactPercent
		if v == 0 || (v >= 1 && v <= 100) {
			continue
		}
		name := m.ID
		if name == "" {
			name = m.Model
		}
		warnings = append(warnings, fmt.Sprintf(
			"ignoring invalid config.json models[%s] jev-autocompact-percent %d; using the global jev-autocompact-percent for this model",
			name, v))
	}
	return warnings
}

// AutocompactPercentForAgent resolves the effective JEV auto-compaction
// trigger percentage for one agent type. Resolution order: the
// agent_models-routed model entry's per-model override (when valid, 1-100) >
// the global jev-autocompact-percent (already validated and normalized by
// ResolveAutocompact) > DefaultJevAutocompactPercent, which the caller passes
// as the global. A nil config or an agent type with no model entry just uses
// the global. The percent keys off the agent's own model because that is
// whose context window fills.
func (cfg *Config) AutocompactPercentForAgent(agentType string, global int) int {
	if cfg == nil || agentType == "" {
		return global
	}
	setting, ok := cfg.GetModelForAgent(agentType)
	if !ok {
		return global
	}
	if override, ok := setting.AutocompactPercentOverride(); ok {
		return override
	}
	return global
}

// SaveConfig atomically writes the configuration back to config.json.
// A degraded config is never saved (defense-in-depth): since the strict
// config rework no LoadConfig path returns one, but if a caller ever hands a
// partially-loaded config to SaveConfig, the refusal protects the user's
// hand-edited config.json from being clobbered by defaults.
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
