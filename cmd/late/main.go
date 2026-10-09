package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"late/internal/agent"
	"late/internal/common"
	"late/internal/executor"
	"late/internal/git"
	"late/internal/orchestrator"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"late/internal/assets"
	"late/internal/client"
	"late/internal/compaction"
	appconfig "late/internal/config"
	"late/internal/mcp"
	"late/internal/pathutil"
	"late/internal/plugin"
	"late/internal/session"
	"late/internal/skill"
	"late/internal/tool"
	"late/internal/tui"

	"encoding/json"
	"text/tabwriter"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"golang.org/x/term"
)

// forceRevaluateUsage is the -h description of
// -force-revaluate-dangerous-commands.
//
// IMPORTANT: this string must contain no back-quoted word. flag.PrintDefaults
// renders the first back-quoted word of a usage string as the flag's value
// name, which would advertise the flag as taking an argument. The OTP code is
// never passed on the CLI: late generates a random single-use code at runtime
// and hands it to the agent in the tool-result block message; the agent
// re-runs the command passing it in the bash tool's otp_code parameter.
const forceRevaluateUsage = "Unsupervised execution, but the first attempt to run a potentially dangerous command is blocked; late issues the agent a random single-use OTP code, bound to that exact command, which it must pass in the bash tool's otp_code parameter to re-run."

// askForUserApprovalUsage is the -h description of -ask-for-user-approval.
//
// Like forceRevaluateUsage, this string must contain no back-quoted word:
// flag.PrintDefaults renders the first back-quoted word as the flag's value
// name, which would advertise this boolean flag as taking an argument.
const askForUserApprovalUsage = "Require explicit user approval before running potentially dangerous commands (default; overrides config.json permission-mode)."

// pluginInlineTool adapts a plugin.InlineTool (defined in internal/plugin/tools.go)
// into a common.Tool so the CLI's session registry can dispatch invocations to
// plugin-declared runners. It exists because upstream repurposed
// tool.ScriptTool for skill dispatch only; for arbitrary plugin-defined tools,
// we wrap them here.
//
// The wrapper synthesizes a client.ToolCall from the executor's (args
// json.RawMessage) payload by stitching in the registered name — args is
// strictly the JSON parameters (e.g. {"path": "/foo"}) the model emitted;
// the function name is provided by the registry at dispatch time, so we
// surface the wrapped name rather than re-parse it from args.
type pluginInlineTool struct {
	name        string
	description string
	parameters  json.RawMessage
	runner      func(ctx context.Context, call client.ToolCall) (string, error)
}

func (p pluginInlineTool) Name() string                { return p.name }
func (p pluginInlineTool) Description() string         { return p.description }
func (p pluginInlineTool) Parameters() json.RawMessage { return p.parameters }

// RequiresConfirmation always returns true: an inline tool runs an
// arbitrary plugin script, so it must go through the normal user
// confirmation flow like skill scripts (tool.ScriptTool) and MCP tools
// (tool adapter). The plugin docs promise exactly this — plugin-example.md:
// "user confirmation still prompts the user".
func (p pluginInlineTool) RequiresConfirmation(args json.RawMessage) bool {
	return true
}
func (p pluginInlineTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return p.runner(ctx, client.ToolCall{
		Type:     "function",
		Function: client.FunctionCall{Name: p.name, Arguments: string(args)},
	})
}
func (p pluginInlineTool) CallString(args json.RawMessage) string {
	return fmt.Sprintf("Calling plugin tool %q...", p.name)
}

func main() {
	// Parse flags
	helpReq := flag.Bool("help", false, "Show this help and exit.")
	systemPromptReq := flag.String("system-prompt", "", "Replace the built-in system prompt with this text; config.json \"system-prompt\" applies unless the flag is passed")
	systemPromptFileReq := flag.String("system-prompt-file", "", "Replace the built-in system prompt with a file's contents (highest priority); config.json \"system-prompt-file\" applies unless the flag is passed")
	useToolsReq := flag.Bool("use-tools", true, "Offer tools to the main agent at all; config.json \"use-tools\" applies unless the flag is passed")
	enableBashReq := flag.Bool("enable-bash", true, "Enable the bash tool (master switch; enabled_tools.bash provides per-tool granularity); config.json \"enable-bash\" applies unless the flag is passed")
	bashTimeout := flag.Duration("bash-timeout", appconfig.DefaultBashTimeout, "Max wall-clock time for one bash tool call (0 = unlimited); config.json \"bash-timeout\" applies unless the flag is passed")
	injectCWDReq := flag.Bool("inject-cwd", true, "Replace ${{CWD}} in the system prompt with the working directory; config.json \"inject-cwd\" applies unless the flag is passed")
	enableSubagentsReq := flag.Bool("enable-subagents", true, "Allow the agent to spawn subagents; config.json \"enable-subagents\" applies unless the flag is passed")
	gemmaThinkingReq := flag.Bool("gemma-thinking", false, "Prepend the Gemma <|think|> token to the system prompt; config.json \"gemma-thinking\" applies unless the flag is passed")
	subagentMaxTurns := flag.Int("subagent-max-turns", appconfig.DefaultSubagentMaxTurns, "Maximum turns per subagent (0 = unlimited); config.json \"subagent-max-turns\" applies unless the flag is passed")
	subagentTimeout := flag.Duration("subagent-timeout", appconfig.DefaultSubagentTimeout, "Max wall-clock time for one subagent run (0 = unlimited; config.json subagent_timeout applies unless the flag is passed)")
	subagentIdleTimeout := flag.Duration("subagent-idle-timeout", appconfig.DefaultSubagentIdleTimeout, "Notify when a subagent has been truly idle (no stream progress, no in-flight tool, no nested spawn) for this long (0 = off); config.json \"subagent-idle-timeout\" applies unless the flag is passed")
	subagentIdleKillAfter := flag.Duration("subagent-idle-kill-after", 0, "Kill a subagent that stays truly idle past this duration (0 = notify only); config.json \"subagent-idle-kill-after\" applies unless the flag is passed")
	// LATE_MAX_STREAM_RETRIES optionally overrides the default retry budget
	// for LLM stream errors; an explicit -max-stream-retries flag wins over
	// it. Full precedence: flag > env > config.json "max-stream-retries" >
	// executor.DefaultMaxStreamRetries (ResolveMaxStreamRetries).
	maxStreamRetriesDefault := executor.DefaultMaxStreamRetries
	maxStreamRetriesEnvSet := false
	if v := os.Getenv("LATE_MAX_STREAM_RETRIES"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			maxStreamRetriesDefault = parsed
			maxStreamRetriesEnvSet = true
		} else {
			fmt.Fprintf(os.Stderr, "Warning: ignoring invalid LATE_MAX_STREAM_RETRIES %q: %v\n", v, err)
		}
	}
	maxStreamRetries := flag.Int("max-stream-retries", maxStreamRetriesDefault, "Retries for LLM stream errors with backoff; 0 disables. Env: LATE_MAX_STREAM_RETRIES; config.json \"max-stream-retries\" (flag > env > config > default)")
	// maxConcurrentLLM backs the process-wide LLM concurrency limiter in
	// internal/client: the root agent and every subagent share one bound, so
	// parallel agents queue instead of stampeding the provider's
	// account-level concurrency limit (429s).
	maxConcurrentLLM := flag.Int("max-concurrent-llm-requests", appconfig.DefaultMaxConcurrentLLMRequests, "Process-wide cap on concurrent in-flight LLM requests across all agents and subagents (0 = unlimited); config.json \"max-concurrent-llm-requests\" applies unless the flag is passed")

	saveSubagentHistoriesReq := flag.Bool("save-subagent-histories", false, "Persist subagent histories to disk (overrides session and config). Default: on when a session folder exists.")
	enableSqzReq := flag.Bool("enable-sqz", false, "Compress bash tool output with the external 'sqz' binary if available; config.json \"enable-sqz\" applies unless the flag is passed")
	appendSystemPromptReq := flag.String("append-system-prompt", "", "Append this text to the final system prompt; config.json \"append-system-prompt\" applies unless the flag is passed")
	versionReq := flag.Bool("version", false, "Print the version and exit.")
	unsupervisedReq := flag.Bool("i-promise-i-have-backups-and-will-not-file-issues", false, "UNSUPPORTED: run every tool without user confirmation.")
	forceRevaluateReq := flag.Bool("force-revaluate-dangerous-commands", false, forceRevaluateUsage)
	askForUserApprovalReq := flag.Bool("ask-for-user-approval", false, askForUserApprovalUsage)
	enableImagesReq := flag.Bool("enable-images", false, "Force-enable image attachments even if the backend does not advertise vision support; config.json \"enable-images\" applies unless the flag is passed")
	continueReq := flag.Bool("continue", false, "Resume the most recently updated session, regardless of which project directory it was started in.")
	continueProjectReq := flag.Bool("continue-project", false, "Resume the most recently updated session for the current project (git repo root of the working directory, or the working directory outside a repo); mutually exclusive with -continue.")
	showCWDReq := flag.Bool("show-cwd", true, "Show the git branch / working directory in the status bar; config.json \"show-cwd\" applies unless the flag is passed")
	themeReq := flag.String("theme", "", "Plugin theme id ('plugin:name' or bare name); env: LATE_THEME.")
	promptReq := flag.String("prompt", "", "Start the agent immediately with this prompt.")
	logitBiasReq := flag.String("logit-bias", "", "Main-agent token bias: JSON object or comma-separated TOKEN_ID:BIAS pairs; config.json \"logit-bias\" applies unless the flag is passed")
	suppressThinkingWordsReq := flag.Bool("suppress-thinking-words", false, "Bias anti-overthinking tokens (requires the same model for main agent and subagents); config.json \"suppress-thinking-words\" applies unless the flag is passed")
	subagentLogitBiasReq := flag.String("subagent-logit-bias", "", "Subagent token bias: JSON object or comma-separated TOKEN_ID:BIAS pairs; config.json \"subagent-logit-bias\" applies unless the flag is passed")

	// Compaction (staged rollout of the jev-compaction port): off = no
	// scoring at all; shadow = score tool outputs + shadow log only
	// (default, no behavior change); enabled = additionally relocate
	// low-scoring segments out of oversized tool results (registers the
	// expand tool so originals stay retrievable).
	compactionModeReq := flag.String("compaction-mode", "", "Tool-output compaction stage: off, shadow (score + shadow log only), or enabled (also relocate low-scoring segments; adds the expand tool). Overrides config.json compaction-mode. Default: shadow.")
	compactionThresholdReq := flag.Float64("compaction-threshold", compaction.DefaultRelocationThreshold, "Score (0-1] below which tool-output segments are elided when -compaction-mode=enabled. Overrides config.json compaction-threshold; default 0.35.")
	replayShadowReq := flag.String("replay-shadow", "", "Replay the default shadow log at the given comma-separated thresholds (e.g. 0.10,0.35,0.50): print the kept/relocated/tokens-saved/still-missed table plus the false-negative rate, then exit. Read-only; the TUI does not start.")
	checkCompactionReq := flag.Bool("check-compaction", false, "Run the compaction preflight against the resolved System One backend — real requests checking (1) decisions answers and parse, (2) the gate relocates something from a real tool output, (3) a pointer expands back byte for byte — print the per-stage report and exit (0 pass, 1 fail; the TUI does not start). Pairs with -compaction-mode. With config.json compaction-backend \"offline\" the same three stages run against the deterministic local scripted scorer: no key, no network.")

	flag.Usage = func() {
		writeHelp(os.Stderr, flag.CommandLine)
	}
	flag.Parse()

	// Record which flags were explicitly passed on the command line. The app
	// config loads AFTER flag.Parse below, so flag.Visit (which reports only
	// command-line-set flags) is the only reliable "explicit flag > config"
	// precedence signal for every resolver of a CLI-equivalent setting
	// (ResolveSubagentTimeout, ResolveBashTimeout, ResolveUseTools, ...).
	explicitFlags := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicitFlags[f.Name] = true })

	if *versionReq {
		// Full one-line build identity: version + stamped commit/build
		// date, degrading to the bare dev banner for a plain `go build`.
		fmt.Println(common.VersionDisplay())
		return
	}

	if *helpReq {
		flag.Usage()
		return
	}

	// -replay-shadow: read-only offline replay of the default shadow log —
	// one kept/relocated/tokens-saved/still-missed row per given threshold
	// (re-decided from the recorded scores, no scorer round trip) plus the
	// false-negative rate — then exit without starting the TUI.
	//
	// This branch deliberately runs BEFORE appconfig.LoadConfig: the replay
	// consumes only the shadow log, never config.json, and LoadConfig has
	// side effects a read-only diagnostic must not take — it CREATES a
	// default config.json when the file is missing and tightens the config
	// dir/file permissions. The price is that the startup config warnings
	// (invalid compaction-mode, compaction-threshold-percent, …) are not
	// printed on this path; they surface on any normal run or
	// -check-compaction (which resolves the config below). If a replay ever
	// needs to honor a config setting, move this branch below the
	// LoadConfig block and accept the side effects.
	if *replayShadowReq != "" {
		thresholds, err := parseReplayThresholds(*replayShadowReq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if err := runReplayShadow(thresholds); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// --continue and --continue-project are mutually exclusive: both select
	// the session to resume, so asking for two is ambiguous (same rule and
	// messaging style as the permission flags).
	if err := validateContinueFlags(*continueReq, *continueProjectReq); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	var loadedHistoryPath string
	var resumedSessionTitle string
	var loadedSessionMeta *session.SessionMeta

	switch {
	case *continueReq:
		// --continue: resume the most recently updated session overall,
		// regardless of the project directory it was started in.
		meta, err := resolveContinueSession()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting latest session: %v\n", err)
			os.Exit(1)
		}
		if meta == nil {
			fmt.Fprintln(os.Stderr, "No sessions found to continue.")
			fmt.Fprintln(os.Stderr, "Use `late session list` to see saved sessions, or `late session load <id>` to resume one directly.")
			os.Exit(1)
		}
		loadedHistoryPath = meta.HistoryPath
		resumedSessionTitle = fmt.Sprintf("Resumed session: %s (%s)", meta.ID, meta.Title)
		loadedSessionMeta = meta
	case *continueProjectReq:
		// --continue-project: resume the most recently updated session of
		// the current project (git repo root of the working directory, or
		// the working directory outside a repo). It works from inside a
		// subdirectory because the repo root is matched, not the CWD.
		meta, err := resolveContinueProjectSession()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting latest session: %v\n", err)
			os.Exit(1)
		}
		if meta == nil {
			if projectDir, dirErr := resolveContinueProjectDir(); dirErr == nil {
				fmt.Fprintf(os.Stderr, "No sessions found to continue in project %s.\n", projectDir)
			} else {
				fmt.Fprintln(os.Stderr, "No sessions found to continue in the current project.")
			}
			fmt.Fprintln(os.Stderr, "Use `late session list` to see sessions started in other projects, or `late session load <id>` to resume one directly.")
			os.Exit(1)
		}
		loadedHistoryPath = meta.HistoryPath
		resumedSessionTitle = fmt.Sprintf("Resumed session: %s (%s)", meta.ID, meta.Title)
		loadedSessionMeta = meta
	case flag.NArg() > 0 && flag.Arg(0) == "session":
		sessCmdResult := handleSessionCommand(flag.Args()[1:])
		if sessCmdResult.ShouldExit {
			return
		}
		loadedHistoryPath = sessCmdResult.HistoryPath
		loadedSessionMeta = sessCmdResult.Meta
	}

	if flag.NArg() > 0 && flag.Arg(0) == "worktree" {
		shouldExit := handleWorktreeCommand(flag.Args()[1:])
		if shouldExit {
			return
		}
	}

	// Plugin command handler — dispatches before TUI startup
	var pluginManager *plugin.PluginManager
	cwd, _ := os.Getwd()
	projectPluginsDir := filepath.Join(cwd, common.LateProjectPluginsDir())
	if flag.NArg() > 0 && flag.Arg(0) == "plugin" {
		pluginsDir, err := common.LatePluginsDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to get plugins directory: %v\n", err)
		} else {
			pm := plugin.NewPluginManager(pluginsDir)
			if _, err := os.Stat(projectPluginsDir); err == nil {
				pm.SetProjectDir(projectPluginsDir)
			}
			if err := pm.Discover(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to discover plugins: %v\n", err)
			}
			pluginManager = pm
			if plugin.HandlePluginCommand(pm, flag.Args()[1:]) {
				return
			}
		}
	}

	// The system prompt is assembled after appconfig.LoadConfig below: its
	// inputs (--system-prompt/-file/-append, --inject-cwd, --gemma-thinking,
	// --enable-bash) are CLI-equivalent settings resolved with the mandatory
	// flag > config > default precedence, which needs the loaded config.
	// Nothing between the flag block and LoadConfig consumes the prompt.

	// Sessions setup

	// Define history path with timestamp-based session ID
	sessionsDir, err := session.SessionDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get session directory: %v\n", err)
		os.Exit(1)
	}
	sessionID := fmt.Sprintf("session-%s", time.Now().Format("20060102-150405"))
	historyPath := filepath.Join(sessionsDir, sessionID+".json")

	if loadedHistoryPath != "" {
		historyPath = loadedHistoryPath
	}

	// Effective session ID for this run — derived from the FINAL history path so
	// resumed sessions keep their original ID (the sessionID var above is a fresh
	// timestamp even on resume). Used to place subagent histories under the right
	// per-session folder. The helper falls back to "" for empty or unsafe IDs,
	// which disables subagent history persistence (in-memory fallback) instead of
	// writing files outside the session folder.
	effectiveSessionID := deriveEffectiveSessionID(historyPath)

	// Load existing history. A corrupt/unreadable file is backed up
	// (LoadHistoryRecovering) before the first save can overwrite it, and
	// the run degrades to an empty history with a loud warning — silently
	// starting over (the old behavior) also silently destroyed the user's
	// session on the next save.
	history, err := session.LoadHistoryRecovering(historyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load history %s (%v); starting with an empty history (the previous bytes were backed up alongside it if they could be read)\n", historyPath, err)
		history = []client.ChatMessage{}
	}
	// Initialize MCP client
	mcpClient := mcp.NewClient()
	defer mcpClient.Close()

	// Load MCP configuration
	config, err := mcp.LoadMCPConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to load MCP config: %v\n", err)
	}

	// Plugin discovery and surface registration
	var (
		skillsDir string
		skillsErr error
	)
	if pluginManager == nil {
		pluginsDir, err := common.LatePluginsDir()
		if err == nil {
			pm := plugin.NewPluginManager(pluginsDir)
			// Set project-local dir if it exists
			if _, statErr := os.Stat(projectPluginsDir); statErr == nil {
				pm.SetProjectDir(projectPluginsDir)
			}
			if err := pm.Discover(); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to discover plugins: %v\n", err)
			} else {
				// Keep the manager even with zero plugins so plugin command
				// dispatch and hooks remain safely available.
				pluginManager = pm
				// Reconcile skill links even when this project has no plugins.
				skillsDir, skillsErr = pathutil.LateSkillsDir()
				if skillsErr == nil {
					if err := pm.RegisterPluginSkills(skillsDir); err != nil {
						fmt.Fprintf(os.Stderr, "Warning: failed to register plugin skills: %v\n", err)
					}
				}
				if pm.Count() > 0 {
					// Connect plugin MCP servers
					pluginMCP := pm.BuildMCPConfigMap()
					if len(pluginMCP) > 0 && config == nil {
						config = &mcp.MCPConfig{McpServers: make(map[string]mcp.MCPServer)}
					}
					if len(pluginMCP) > 0 && config != nil {
						for name, srv := range pluginMCP {
							config.McpServers[name] = mcp.MCPServer{
								Command:       srv.Command,
								Args:          srv.Args,
								Env:           srv.Env,
								URL:           srv.URL,
								TransportType: srv.TransportType,
								Disabled:      srv.Disabled,
								Dir:           srv.Dir,
							}
						}
					}
				}
			}
		}
	}
	// Load App configuration. Strict config (R2/R3): a config.json with a
	// syntax error, an unknown entry, a wrong-typed value, or an invalid
	// enum/boolean value is FATAL — the rendered line/column error goes to
	// stderr and late exits 1 BEFORE the TUI starts, never silently
	// continuing on fallback defaults. The only recoverable load error is
	// a post-load permission-hardening failure, which returns a fully
	// valid config alongside the error and only warns. (-help/-version
	// never reach this point: they are handled above, before LoadConfig.)
	appConfig, err := appconfig.LoadConfig()
	if shouldExitOnConfigLoadError(appConfig, err) {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	configLoadWarning := initialBootstrapStatus(err)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to load app config: %v\n", err)
	}
	// Surface an invalid compaction-threshold-percent the same way the
	// invalid permission-mode is reported: warn once and use the default.
	if _, compactionWarning := appconfig.ResolveCompactionThreshold(appConfig); compactionWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", compactionWarning)
	}
	// Same warn-and-fall-back pattern for the auto-compaction threshold.
	if _, _, autocompactWarning := appconfig.ResolveAutocompact(appConfig); autocompactWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", autocompactWarning)
	}
	// Per-model jev-autocompact-percent overrides use the same key inside
	// each models[] entry; an out-of-range per-model value warns and falls
	// back to the global threshold (it cannot fail the strict parse — that
	// covers key names, not value ranges).
	for _, modelWarning := range appConfig.AutocompactWarnings() {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", modelWarning)
	}
	enabledTools := make(map[string]bool)
	if appConfig != nil {
		for toolName, enabled := range appConfig.EnabledTools {
			enabledTools[toolName] = enabled
		}
	}

	// ------------------------------------------------------------------
	// CLI-equivalent settings (flag > config > default).
	//
	// Every setting below mirrors a command-line flag one-to-one; its
	// config.json key is the flag name in kebab-case. Each appconfig
	// resolver implements the mandatory precedence — explicitly passed
	// flag (explicitFlags, recorded from flag.Visit above) > config.json
	// > built-in default — and returns an optional warning surfaced on
	// stderr like every other invalid config entry.
	// ------------------------------------------------------------------
	reportWarning := func(warning string) {
		if warning != "" {
			fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
		}
	}

	// Activate the process-wide LLM concurrency limiter before the first
	// client is created (the main agent, every subagent, and the model
	// switcher share this one bound). Resolved here — not at flag.Parse
	// time — because config.json can lower or lift the flag's built-in
	// default; nothing between the flag block and here issues LLM requests.
	resolvedMaxConcurrentLLM, maxConcurrentLLMWarning := appconfig.ResolveMaxConcurrentLLMRequests(appConfig, explicitFlags["max-concurrent-llm-requests"], *maxConcurrentLLM)
	reportWarning(maxConcurrentLLMWarning)
	client.SetLLMConcurrency(resolvedMaxConcurrentLLM)

	resolvedEnableSqz, _ := appconfig.ResolveEnableSqz(appConfig, explicitFlags["enable-sqz"], *enableSqzReq)
	tool.SetSqzEnabled(resolvedEnableSqz)

	// Shell tool bound: 0/negative (--bash-timeout=0) disables it —
	// ShellTool.Execute treats a non-positive timeout as unbounded.
	resolvedBashTimeout, bashTimeoutWarning := appconfig.ResolveBashTimeout(appConfig, explicitFlags["bash-timeout"], *bashTimeout)
	reportWarning(bashTimeoutWarning)
	tool.SetShellTimeout(resolvedBashTimeout)

	// System prompt group. Priority (identical to the flags): file > text >
	// LATE_SYSTEM_PROMPT env > built-in; the append is always applied last.
	resolvedSystemPrompt, _ := appconfig.ResolveSystemPrompt(appConfig, explicitFlags["system-prompt"], *systemPromptReq)
	resolvedSystemPromptFile, _ := appconfig.ResolveSystemPromptFile(appConfig, explicitFlags["system-prompt-file"], *systemPromptFileReq)
	resolvedAppendSystemPrompt, _ := appconfig.ResolveAppendSystemPrompt(appConfig, explicitFlags["append-system-prompt"], *appendSystemPromptReq)
	resolvedInjectCWD, _ := appconfig.ResolveInjectCWD(appConfig, explicitFlags["inject-cwd"], *injectCWDReq)
	resolvedGemmaThinking, _ := appconfig.ResolveGemmaThinking(appConfig, explicitFlags["gemma-thinking"], *gemmaThinkingReq)
	resolvedEnableBash, _ := appconfig.ResolveEnableBash(appConfig, explicitFlags["enable-bash"], *enableBashReq)

	// Subagent group.
	resolvedEnableSubagents, _ := appconfig.ResolveEnableSubagents(appConfig, explicitFlags["enable-subagents"], *enableSubagentsReq)
	resolvedSubagentMaxTurns, subagentMaxTurnsWarning := appconfig.ResolveSubagentMaxTurns(appConfig, explicitFlags["subagent-max-turns"], *subagentMaxTurns)
	reportWarning(subagentMaxTurnsWarning)
	resolvedSubagentIdleTimeout, subagentIdleTimeoutWarning := appconfig.ResolveSubagentIdleTimeout(appConfig, explicitFlags["subagent-idle-timeout"], *subagentIdleTimeout)
	reportWarning(subagentIdleTimeoutWarning)
	resolvedSubagentIdleKillAfter, subagentIdleKillAfterWarning := appconfig.ResolveSubagentIdleKillAfter(appConfig, explicitFlags["subagent-idle-kill-after"], *subagentIdleKillAfter)
	reportWarning(subagentIdleKillAfterWarning)

	// Streaming / model group.
	resolvedMaxStreamRetries, maxStreamRetriesWarning := appconfig.ResolveMaxStreamRetries(appConfig, explicitFlags["max-stream-retries"], *maxStreamRetries, maxStreamRetriesEnvSet, maxStreamRetriesDefault)
	reportWarning(maxStreamRetriesWarning)
	resolvedEnableImages, _ := appconfig.ResolveEnableImages(appConfig, explicitFlags["enable-images"], *enableImagesReq)
	resolvedSuppressThinkingWords, _ := appconfig.ResolveSuppressThinkingWords(appConfig, explicitFlags["suppress-thinking-words"], *suppressThinkingWordsReq)
	resolvedLogitBias, _ := appconfig.ResolveLogitBias(appConfig, explicitFlags["logit-bias"], *logitBiasReq)
	resolvedSubagentLogitBias, _ := appconfig.ResolveSubagentLogitBias(appConfig, explicitFlags["subagent-logit-bias"], *subagentLogitBiasReq)

	// Session / TUI group.
	resolvedShowCWD, _ := appconfig.ResolveShowCWD(appConfig, explicitFlags["show-cwd"], *showCWDReq)
	resolvedUseTools, _ := appconfig.ResolveUseTools(appConfig, explicitFlags["use-tools"], *useToolsReq)

	// Determine system prompt
	// Priority: --system-prompt-file > --system-prompt > LATE_SYSTEM_PROMPT
	// env var (each of the first two is itself resolved flag > config >
	// default above, so a config file beats a config text and either flag
	// beats either config entry).
	var systemPrompt string

	if resolvedSystemPromptFile != "" {
		content, err := os.ReadFile(resolvedSystemPromptFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading system prompt file: %v\n", err)
			os.Exit(1)
		}
		systemPrompt = string(content)
	} else if resolvedSystemPrompt != "" {
		systemPrompt = resolvedSystemPrompt
	} else if envPrompt := os.Getenv("LATE_SYSTEM_PROMPT"); envPrompt != "" {
		systemPrompt = envPrompt
	} else {
		content, _ := assets.PromptsFS.ReadFile("prompts/instruction-orchestrator.md")
		systemPrompt = string(content)
	}

	if resolvedInjectCWD {
		cwd, err := os.Getwd()
		if err == nil {
			systemPrompt = common.ReplacePlaceholders(systemPrompt, map[string]string{
				"${{CWD}}": cwd,
			})
		}
	}

	if resolvedGemmaThinking {
		systemPrompt = "<|think|>" + systemPrompt
	}

	if !resolvedEnableBash {
		systemPrompt = common.ReplacePlaceholders(systemPrompt,
			map[string]string{
				"${{NOTICE}}": "Bash is disabled. You must not attempt to use execute any bash commands. Doing so will result in an error.",
			})
	}

	if runtime.GOOS == "windows" {
		systemPrompt += "\n\n## Platform Note\nYou are running on **Windows** and commands execute in **PowerShell**. Prefer PowerShell-native commands and syntax:\n- Prefer `Get-ChildItem` (or `dir`) for directory listing\n- Prefer `Get-Content` for reading files\n- Prefer `Remove-Item` for deleting files/directories\n- Prefer `Copy-Item` and `Move-Item` for copy/move operations\n- Prefer `New-Item -ItemType Directory` for explicit directory creation\n- Use PowerShell quoting/escaping rules and avoid Unix-only shell syntax\n- Do NOT use bash/sh-specific features unless explicitly required"
	}

	if resolvedAppendSystemPrompt != "" {
		systemPrompt = systemPrompt + resolvedAppendSystemPrompt
	}

	// Resolve the global subagent run budget with precedence
	// explicit --subagent-timeout flag > config.json "subagent_timeout" >
	// DefaultSubagentTimeout (24h). A non-positive resolved budget ("0" or
	// negative) means unlimited; an invalid config value is ignored with a
	// warning and the default applies instead.
	resolvedSubagentTimeout, subagentTimeoutWarning := appconfig.ResolveSubagentTimeout(appConfig, explicitFlags["subagent-timeout"], *subagentTimeout)
	if subagentTimeoutWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", subagentTimeoutWarning)
	}

	// Parse the resolved main-agent logit bias override if provided. A
	// malformed FLAG value keeps the historical hard error (the user typed
	// it on this very command line); a malformed CONFIG value warns and
	// proceeds without a bias, like every other invalid config entry.
	var explicitUserLogitBias map[string]int
	if resolvedLogitBias != "" {
		parsed, err := client.ParseLogitBias(resolvedLogitBias)
		if err != nil {
			if explicitFlags["logit-bias"] {
				fmt.Fprintf(os.Stderr, "Error parsing --logit-bias: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Warning: ignoring invalid config.json logit-bias: %v\n", err)
		} else {
			explicitUserLogitBias = parsed
		}
	}

	var explicitSubagentLogitBias map[string]int
	if resolvedSubagentLogitBias != "" {
		parsed, err := client.ParseLogitBias(resolvedSubagentLogitBias)
		if err != nil {
			if explicitFlags["subagent-logit-bias"] {
				fmt.Fprintf(os.Stderr, "Error parsing --subagent-logit-bias: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "Warning: ignoring invalid config.json subagent-logit-bias: %v\n", err)
		} else {
			explicitSubagentLogitBias = parsed
		}
	}

	// Resolve subagent history persistence
	// (explicit CLI flag > saved session preference > config file >
	// DefaultSaveSubagentHistories, which is ON since the subagent manifest
	// landed: interrupted children's work only survives when the session
	// folder persists their histories).
	// Legacy meta migration: metas written by default-false versions carry
	// save_subagent_histories:false that records the old default, not a user
	// choice — ResolveSaveSubagentHistories demotes that saved false to
	// "absent" (new ON default) unless the config has an explicit entry.
	saveSubagentHistoriesCLI := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "save-subagent-histories" {
			saveSubagentHistoriesCLI = true
		}
	})
	var storedSubagentHistoryPreference *bool
	if loadedSessionMeta != nil {
		storedSubagentHistoryPreference = loadedSessionMeta.SaveSubagentHistories
	}
	saveSubagentHistories := appconfig.ResolveSaveSubagentHistories(appConfig, saveSubagentHistoriesCLI, *saveSubagentHistoriesReq, storedSubagentHistoryPreference)

	// Resolve the effective permission mode
	// (explicit CLI flag > config.json permission-mode > ask-for-user-approval).
	permissionMode, permissionModeWarning, err := appconfig.ResolvePermissionMode(appConfig, *askForUserApprovalReq, *unsupervisedReq, *forceRevaluateReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if permissionModeWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", permissionModeWarning)
	}

	// Initialize Core Components
	resolvedOpenAIConfig := appconfig.ResolveOpenAISettings(appConfig)
	resolvedClientConfig := client.Config{
		BaseURL:      resolvedOpenAIConfig.BaseURL,
		APIKey:       resolvedOpenAIConfig.APIKey,
		Model:        resolvedOpenAIConfig.Model,
		EnableImages: resolvedEnableImages,
		LogitBias:    explicitUserLogitBias,
		AppVersion:   common.Version,
	}
	if appConfig != nil {
		if setting, ok := appConfig.GetModelForAgent("orchestrator"); ok {
			resolvedClientConfig.BaseURL = setting.URL
			resolvedClientConfig.APIKey = setting.Key
			resolvedClientConfig.Model = setting.Model
		}
	}
	resolvedSubagentConfig := appconfig.ResolveSubagentSettings(appConfig, resolvedOpenAIConfig)

	// Resolve the startup state of the todos side pane. The panel is open
	// by default; config.json "show-todo-pane": false starts with it
	// closed. Width handling happens in the TUI (see model.ShowTodoPane).
	showTodoPane := appConfig.ResolveShowTodoPane()

	// Validate --suppress-thinking-words: only allowed in homogeneous setups
	if err := validateSuppressThinkingWords(resolvedSuppressThinkingWords, resolvedClientConfig.Model, resolvedSubagentConfig.Model, appConfig); err != nil {
		fmt.Fprintf(os.Stderr, "Error: --suppress-thinking-words is currently only supported when orchestrator and subagents use the same model: %v\n", err)
		os.Exit(1)
	}

	c := client.NewClient(resolvedClientConfig)
	// An explicit context-size-tokens declaration wins over discovery: it is
	// applied BEFORE any request (and before DiscoverBackend can run) and
	// the client never lets a probe result clobber it. Without it the
	// context size stays -1 for providers that never advertise their window,
	// and the predictive compaction heuristic stays dark. Declared on the
	// root client; the subagent client below mirrors its own routed entry.
	if appConfig != nil {
		if setting, ok := appConfig.GetModelForAgent("orchestrator"); ok {
			if ctxSize, ok := setting.ContextSizeOverride(); ok {
				c.SetContextSize(ctxSize)
			}
		}
	}

	// Initialize Subagent Client
	subagentClient := c
	if len(explicitSubagentLogitBias) > 0 || len(explicitUserLogitBias) > 0 ||
		resolvedSubagentConfig.BaseURL != resolvedClientConfig.BaseURL ||
		resolvedSubagentConfig.APIKey != resolvedClientConfig.APIKey ||
		resolvedSubagentConfig.Model != resolvedClientConfig.Model {
		subagentClient = client.NewClient(client.Config{
			BaseURL:      resolvedSubagentConfig.BaseURL,
			APIKey:       resolvedSubagentConfig.APIKey,
			Model:        resolvedSubagentConfig.Model,
			EnableImages: resolvedEnableImages,
			LogitBias:    explicitSubagentLogitBias,
			AppVersion:   common.Version,
		})
		if appConfig != nil {
			if setting, ok := appConfig.GetModelForAgent("subagent"); ok {
				if ctxSize, ok := setting.ContextSizeOverride(); ok {
					subagentClient.SetContextSize(ctxSize)
				}
			}
		}
	}

	// Flag overrides
	// The bash master switch ANDs with enabled_tools.bash (per-tool
	// granularity): either being false disables the bash tool, exactly as
	// the -enable-bash=false flag always has.
	if !resolvedEnableBash {
		enabledTools["bash"] = false
	}

	// Main agent is a planner: explicitly enable planner tools and disable coding tools
	mainTools := make(map[string]bool)
	for k, v := range enabledTools {
		mainTools[k] = v
	}
	mainTools["write_implementation_plan"] = true
	mainTools["create_todos"] = true
	mainTools["list_todos"] = true
	mainTools["finish_todo"] = true
	mainTools["write_file"] = false
	mainTools["target_edit"] = false

	sess := session.New(c, historyPath, history, systemPrompt, resolvedUseTools)
	if loadedSessionMeta != nil {
		sess.SetSubagentMetadata(loadedSessionMeta.SubagentSeq, loadedSessionMeta.SaveSubagentHistories)
		if loadedSessionMeta.WorkingDir != "" {
			sess.SetWorkingDir(loadedSessionMeta.WorkingDir)
		}
		// Restore the compaction high-water mark so the frozen prefix stays
		// append-only across restarts: resumed sessions never re-score or
		// rewrite messages a previous run already froze. Legacy sidecars
		// without the field carry zero — the count-based prefix then applies.
		sess.SetCompactionHighWater(loadedSessionMeta.CompactionHighWater)
	} else {
		sess.SetSubagentMetadata(0, &saveSubagentHistories)
	}

	// Manifest resume integration (Phase 1/2): close any spawn_subagent tool
	// call left dangling by a previous late exit with a synthesized,
	// persisted tool result built from the session manifest (running-at-exit
	// records = "interrupted"). Persisting keeps resume idempotent: a second
	// resume finds no dangling calls. A failure must not block resuming —
	// the request-time sanitizer still closes the exchange — so it is logged
	// and the session continues without the synthesized results. The loaded
	// manifest feeds the TUI-side restore (below, once rootAgent exists).
	restoredSubagents := make(map[string]bool)
	resumedManifest, synthesizeErr := synthesizeDanglingSpawnResults(sess)
	if synthesizeErr != nil {
		common.LogErrorf("subagent-manifest", "subagent resume synthesis failed: %v", synthesizeErr)
	}
	executor.RegisterTools(sess.Registry, mainTools)

	// Register MCP tools into the session registry.
	// MCP tool names are now namespaced as "{server}__{tool}" (sanitized —
	// e.g. "graph-rag__list_files"). For backwards compatibility with
	// configs that disable tools by bare name (e.g. "list_files": false),
	// we check the namespaced name first, then fall back to the bare name
	// so existing configs keep working without modification.
	//
	// pluginToolNames records every plugin-provided tool registered here so
	// toolSync tracks the initial tool set.
	var pluginToolNames []string
	// usedToolNames records every name registered below (MCP first, then
	// inline) so inline tools are deduped against MCP names too — without
	// this, a plugin's inline tool can silently overwrite an MCP-backed
	// tool that sanitizes to the same namespaced name.
	usedToolNames := make(map[string]bool)
	for _, t := range mcpClient.GetTools() {
		if !mcpToolEnabled(t, enabledTools) {
			continue
		}
		sess.Registry.Register(t)
		pluginToolNames = append(pluginToolNames, t.Name())
		usedToolNames[t.Name()] = true
	}

	// Register inline plugin tools (declared in the manifest's `late.tools`
	// field). Each inline tool is run as a local script via runHook and
	// hooks into the same ToolMiddleware chain as MCP-backed tools so
	// onToolCall hooks, confirmations, and tool-result reporting all work
	// uniformly for plugin-declared tools.
	if pluginManager != nil {
		for _, t := range pluginManager.GetInlineTools(usedToolNames) {
			if !toolEnabled(enabledTools, t.Name) {
				continue
			}
			sess.Registry.Register(pluginInlineTool{
				name:        t.Name,
				description: t.Description,
				parameters:  t.Parameters,
				runner:      t.Runner,
			})
			pluginToolNames = append(pluginToolNames, t.Name)
		}
	}

	// Compaction (staged rollout stage 2 of the jev-compaction port).
	// Mode resolution: the -compaction-mode flag beats config.json
	// compaction-mode; both are validated against the same three values
	// (invalid → warn + shadow, the safe default).
	compactionMode, compactionModeWarning := appconfig.ResolveCompactionMode(appConfig)
	if *compactionModeReq != "" {
		if appconfig.IsValidCompactionMode(*compactionModeReq) {
			compactionMode = *compactionModeReq
			compactionModeWarning = ""
		} else {
			compactionMode = appconfig.DefaultCompactionMode
			compactionModeWarning = fmt.Sprintf("ignoring invalid -compaction-mode %q; using %q",
				*compactionModeReq, appconfig.DefaultCompactionMode)
		}
	}
	if compactionModeWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", compactionModeWarning)
	}

	// Retrieval read side (Step 17): compaction-retrieval scores the record
	// store's digest against the current task before every stream request
	// and stages the top-k relevant records into the outgoing request's work
	// area (ephemeral — never the frozen prefix, never persisted). Resolved
	// with the same warn-on-invalid pattern as the other compaction knobs;
	// the warning fires for the inert combinations (mode not "enabled",
	// where the store never fills).
	compactionRetrieval, compactionRetrievalWarning := appconfig.ResolveCompactionRetrieval(appConfig)
	if compactionRetrievalWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", compactionRetrievalWarning)
	}

	// Backend selection (Step 18): config.json compaction-backend points
	// scoring at a specific scorer. The only value today is "offline" — the
	// deterministic scripted scorer (no API key, no network; demos and tests
	// only, its scores are content hashes). A set value WINS over the
	// environment: JEV_API and auto-detection are consulted only when the
	// entry is absent, because the config entry is the explicit statement
	// about where scoring happens. Invalid values warn and fall back to the
	// env-based path.
	compactionBackendName, compactionBackendWarning := appconfig.ResolveCompactionBackend(appConfig)
	if compactionBackendWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", compactionBackendWarning)
	}

	// -check-compaction: run the compaction preflight (Step 16) against the
	// backend THIS run would resolve and exit — the TUI never starts. The
	// mode resolution above is deliberately shared with the normal startup
	// path (the check must vet exactly the backend the session would use),
	// and -compaction-mode pairs with the flag, so an invalid mode warns
	// here the same way it would in a real run. The check itself exercises
	// scoring, the gate, and expansion — a superset of what shadow mode does
	// — so it applies in every mode: it is the "would have caught the
	// too-small local backend before integration" tool. With the offline
	// backend selected in config.json the same stages run against the
	// scripted scorer — no key, no network, and they pass by construction.
	if *checkCompactionReq {
		os.Exit(runCompactionCheck(compactionBackendName == appconfig.CompactionBackendOffline))
	}

	// Elision threshold: segments scoring strictly below it are relocated
	// out of oversized tool results when compaction-mode is enabled
	// (default per the upstream repo's own shadow-log replay data).
	// Precedence: an explicitly passed -compaction-threshold flag >
	// config.json compaction-threshold > the 0.35 default. The resolver
	// receives the flag value only when it was explicitly passed
	// (explicitFlags from flag.Visit — config loads after flag.Parse, so
	// this is the only reliable explicit-flag signal); 0 otherwise, so the
	// config entry can win over the flag's built-in default.
	compactionThresholdFlagValue := 0.0
	if explicitFlags["compaction-threshold"] {
		compactionThresholdFlagValue = *compactionThresholdReq
	}
	compactionThreshold, compactionThresholdWarning := appconfig.ResolveCompactionScoreThreshold(appConfig, compactionThresholdFlagValue)
	if compactionThresholdWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", compactionThresholdWarning)
	}

	// Gate safety knobs (reference-parity semantics for the elide decision).
	// Max elide fraction: a scorer that wants to drop more than this share
	// of an output's tokens is distrusted and nothing is elided. Protected
	// floor: stacktrace and diff segments are only elided below this score.
	compactionMaxElidePercent, maxElideWarning := appconfig.ResolveCompactionMaxElidePercent(appConfig)
	if maxElideWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", maxElideWarning)
	}
	compactionProtectedFloorPercent, protectedFloorWarning := appconfig.ResolveCompactionProtectedFloor(appConfig)
	if protectedFloorWarning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", protectedFloorWarning)
	}

	// The TUI's /jev-compact-context command and auto-trigger reuse this
	// pipeline (its scoring client) and elide store; both stay nil when
	// compaction is off or no backend resolved, which disables them.
	var (
		compactionPipeline  *compaction.Pipeline
		compactionStore     *compaction.Store
		compactionShadowLog *compaction.ShadowLog
		// compactionBackend is the resolved backend behind the pipeline,
		// captured for the Step 16 startup probe; nil when compaction is
		// off, no backend resolved, or the offline scripted scorer is
		// selected (nothing to probe — there is no network to reach).
		compactionBackend *compaction.ResolvedBackend
		// historyGate is the gate config the pipeline was applied (the same
		// reference-parity safety semantics — keep threshold, elide-fraction
		// tripwire, protected-kind floors) handed to the history compaction
		// runner so the walk's keep/elide calls mirror the tool-output
		// path's; nil when compaction is off or no pipeline was built.
		historyGate *compaction.GateConfig
	)
	if compactionMode != appconfig.CompactionModeOff {
		// Scoring source: the offline scripted scorer (compaction-backend
		// "offline") or, when the config entry is absent/invalid, the
		// env-resolved System One backend as before. The config entry wins:
		// an "offline" session never resolves a backend, never needs a key,
		// and never sends a request.
		compactionOffline := compactionBackendName == appconfig.CompactionBackendOffline
		var (
			backend   compaction.ResolvedBackend
			scoringOK bool
		)
		if compactionOffline {
			scoringOK = true
		} else {
			resolved, backendErr := compaction.ResolveBackendEnv("")
			if backendErr != nil {
				if compactionMode == appconfig.CompactionModeEnabled {
					// Relocation without a backend would fail-open every
					// oversized result (nothing ever elided): warn and drop to
					// the shadow stage per the staged rollout.
					fmt.Fprintf(os.Stderr, "Warning: compaction-mode %q needs a System One backend (%v); falling back to %q\n",
						appconfig.CompactionModeEnabled, backendErr, appconfig.CompactionModeShadow)
					compactionMode = appconfig.CompactionModeShadow
				} else {
					fmt.Fprintf(os.Stderr, "Warning: compaction-mode %q has no System One backend (%v)\n",
						appconfig.CompactionModeShadow, backendErr)
				}
			} else {
				backend = resolved
				scoringOK = true
			}
		}
		// The pipeline only exists behind usable scoring: without it every
		// decisions call would burn retries and fail-open, so this run
		// proceeds with compaction off instead (the warning above explains).
		// Offline scoring is always usable — that is the point of the demo
		// path.
		if scoringOK {
			shadowLog, shadowErr := compaction.NewShadowLog()
			if shadowErr != nil {
				// Logging is best-effort: scoring (and relocation) still
				// run without it.
				fmt.Fprintf(os.Stderr, "Warning: compaction shadow log unavailable (%v); continuing without it\n", shadowErr)
				shadowLog = nil
			}
			compactionShadowLog = shadowLog
			var pipeline *compaction.Pipeline
			if compactionOffline {
				// Step 18 demo path: the same segmentation, gate, pointers,
				// and store over the deterministic scripted scorer. Demos
				// and tests only — the scripted scores are content hashes,
				// not essentialness judgments, and must never become a
				// production default.
				pipeline = compaction.NewOfflinePipeline(compaction.PipelineOptions{Shadow: shadowLog})
			} else {
				compactionBackend = &backend
				pipeline = compaction.NewPipeline(backend, "", shadowLog, compaction.PipelineOptions{})
			}
			// GateConfig: the reference-parity elision safety semantics —
			// keep threshold, elide-fraction tripwire, and protected-kind
			// floors — threaded from config.json (defaults mirror the
			// reference pipeline.py). KeepThreshold uses the SAME resolved
			// compactionThreshold as EnableRelocation below — one source of
			// truth for the elision cutoff (flag > config > default).
			gate := compaction.DefaultGateConfig()
			gate.KeepThreshold = compactionThreshold
			gate.MaxElideFraction = float64(compactionMaxElidePercent) / 100
			protectedFloor := float64(compactionProtectedFloorPercent) / 100
			gate.ProtectedKinds = map[compaction.SegmentKind]float64{
				compaction.KindStacktrace: protectedFloor,
				compaction.KindDiff:       protectedFloor,
			}
			pipeline.ApplyGateConfig(gate)
			// The history walk (/jev-compact-context + the auto-trigger)
			// runs the same gate: identical keep/elide calls on both
			// compaction paths, same source of truth for every knob.
			historyGate = &gate
			if compactionMode == appconfig.CompactionModeEnabled {
				// The record store persists elided originals across
				// restarts: [[elided …]] pointers saved into a session
				// history must still resolve after `late` exits, so
				// relocation is backed by the append-only JSONL store at
				// compaction.DefaultStorePath instead of a throwaway
				// in-memory map. Open failure degrades to the in-memory
				// store — compaction keeps working, pointers merely stop
				// surviving restarts (the shadow-log warning pattern).
				store := openCompactionStore()
				// Outcomes: the expand tool attributes every expand back to
				// the record and its contributing segment ids through the
				// shadow log attached here (Step 13's false-negative
				// ledger). Nil-safe — a missing shadow log simply disables
				// outcome logging.
				store = store.WithShadowLog(compactionShadowLog)
				pipeline.EnableRelocation(store, compactionThreshold)
				// The expand tool returns relocated originals. Registered on
				// the main registry before any spawn: subagents inherit it
				// (and the same store) from the parent registry.
				sess.Registry.Register(tool.ExpandTool{Store: store})
				compactionStore = store
			}
			// Shared by the root agent and every subagent: ExecuteToolCalls
			// consults it for both (shadow mode scores and logs without
			// changing results).
			executor.SetToolResultCompactor(pipeline)
			compactionPipeline = pipeline
		}
	}

	// Tool-output archiving (default behavior, no config key): oversized
	// tool outputs are written under the session's folder and the
	// conversation carries only the compact reference form. One archive for
	// the run — the root agent and every subagent share it, mirroring the
	// compactor install above. Rooted at the active session's folder so the
	// archive dies with the session folder (RemoveSessionFolder); an
	// in-memory session (no derived session ID) fails the validity check
	// and simply gets no archive — everything stays inline.
	toolArchive, archiveDirErr := session.OutputArchiveDir(effectiveSessionID)
	if archiveDirErr == nil {
		arch, archErr := session.NewOutputArchive(toolArchive)
		if archErr != nil {
			// Archiving is fail-open end to end: ExecuteToolCalls keeps
			// results inline on any archive error, so a failed setup only
			// disables the feature.
			fmt.Fprintf(os.Stderr, "Warning: tool-output archiving disabled (%v)\n", archErr)
			toolArchive = ""
		} else {
			// Root install; the subagent runner below re-installs the same
			// archive for every spawn so children archive their outputs
			// even if the root install is ever made conditional.
			executor.SetToolResultArchiver(arch)
			// Shell-call spooling shares this directory: failed calls leave
			// their partial transcript here as partial-<id>.txt for the
			// resume synthesis to reference, and successful ones promote to
			// the same content-addressed archive naming. Shared by the root
			// agent and every subagent, like the archiver above.
			tool.SetShellSpoolDir(toolArchive)
		}
	} else {
		toolArchive = ""
	}

	// Resolve theme: --theme flag > $LATE_THEME > config.json > bundled base.
	themeID := *themeReq
	if themeID == "" {
		themeID = os.Getenv("LATE_THEME")
	}
	if themeID == "" && appConfig != nil && appConfig.Theme != "" {
		themeID = appConfig.Theme
	}
	themeBytes := tui.LateTheme
	if themeID != "" && themeID != "default" && pluginManager != nil {
		if info, err := pluginManager.GetTheme(themeID); err == nil && info != nil {
			if merged, mErr := tui.ResolveRenderTheme(info.ID, info.Glamour); mErr == nil {
				themeBytes = merged
				themeID = info.ID
				fmt.Fprintf(os.Stderr, "Applied plugin theme: %s\n", info.ID)
			} else {
				themeID = "default"
			}
		} else {
			if err != nil {
				fmt.Fprintf(os.Stderr, "Theme lookup failed for %q: %v\n", themeID, err)
			}
			themeID = "default"
		}
	} else {
		themeID = "default"
	}
	// Initialize common renderer
	renderer, _ := glamour.NewTermRenderer(
		glamour.WithStylesFromJSONBytes(themeBytes),
		glamour.WithWordWrap(80),
		glamour.WithPreservedNewLines(),
	)

	// Create root orchestrator
	// We'll add middlewares later once the program is started
	rootAgent := orchestrator.NewBaseOrchestrator(common.MainAgentID, sess, nil, 0)
	// Idle watchdog policy applies to the root agent too: an orchestrator
	// stuck with no stream progress, tool, or nested spawn reports idle (and,
	// with --subagent-idle-kill-after, cancels its own run).
	rootAgent.SetIdlePolicy(resolvedSubagentIdleTimeout, resolvedSubagentIdleKillAfter)

	// TUI rehydration (Phase 3b): the interrupted children the resume
	// synthesis reported re-enter the TUI on the root, so the preserved
	// transcripts are browsable like any other historical child (tab
	// switching, transcript view). With subagents enabled the LIVE restore
	// runs below, inside the enabled block, where the scheduler and client
	// routing already exist — interrupted children are relaunched through
	// the same {"resume": "<id>"} machinery instead of mounting read-only.
	// Here (subagents disabled — no live machinery) the read-only
	// projections take over. This runs before the TUI program exists and
	// before any live spawn can register a child — AddChild is synchronous
	// on the root's mutex, so there is no ordering race with the runner
	// below.
	if resumedManifest != nil && !resolvedEnableSubagents {
		restoreInterruptedSubagents(rootAgent, resumedManifest, restoredSubagents, nil)
	}
	model := tui.NewModel(rootAgent, renderer, appConfig)
	model.SetActiveThemeStyles(themeBytes)
	if themeID != "" {
		model.SelectedTheme = themeID
	}
	model.ApplyOrchestratorModel = func(setting appconfig.ModelSetting) tea.Cmd {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Either both biases or none should be sent: only pass logit biases
			// if the switched model matches the configured orchestrator model.
			var bias map[string]int
			if setting.Model == resolvedClientConfig.Model {
				bias = c.LogitBias()
			}
			sess.SetClient(newModelClient(ctx, setting, resolvedEnableImages, bias))
			return nil
		}
	}
	if appConfig != nil {
		if setting, ok := appConfig.GetModelForAgent("orchestrator"); ok {
			model.ModelName = setting.Model
		} else {
			model.ModelName = resolvedOpenAIConfig.Model
		}

		var subagentInfos []string
		for _, sub := range assets.GetSubagents() {
			if setting, ok := appConfig.GetModelForAgent(sub.Name); ok {
				subagentInfos = append(subagentInfos, fmt.Sprintf("%s:%s", sub.Name, setting.Model))
			}
		}
		if len(subagentInfos) > 0 {
			model.SubagentInfo = strings.Join(subagentInfos, ", ")
		} else {
			model.SubagentInfo = resolvedSubagentConfig.Model
		}
	} else {
		model.ModelName = resolvedOpenAIConfig.Model
		model.SubagentInfo = resolvedSubagentConfig.Model
	}

	// Register plugin command handler + message hook into the TUI.
	if pluginManager != nil {
		if pluginManager.HasMessageSendHooks() {
			model.MessageHook = func(text string) string {
				return pluginManager.HookedMessage(context.Background(), text)
			}
		}
		model.CommandHandler = pluginManager.HandleCommand
	}

	// History compaction for /jev-compact-context + the auto-trigger: the
	// session's CompactContext shares the pipeline's scoring client and its
	// elide-id space (the same store the expand tool reads). Shadow mode
	// reports without mutating; enabled mode persists the compacted history.
	if compactionPipeline != nil {
		if compactionStore == nil {
			// Shadow mode: the history walk still mints pointer ids for its
			// honest report, so it needs a store even though nothing is
			// applied; a fresh one keeps those ids out of the (absent)
			// expand tool's id space.
			compactionStore = compaction.NewStore()
		}
		model.Compactor = historyCompactionRunner(sess, compactionPipeline.HistoryScorer(), compactionStore,
			compactionMode != appconfig.CompactionModeEnabled, compactionThreshold, compactionShadowLog, historyGate)
		// The TUI's one-shot 413 payload-recovery compaction only fires when
		// compaction can actually shrink history: mode "enabled" (after the
		// shadow fallback above, which downgrades to shadow when the
		// backend is unavailable — compactionMode is re-read here, so the
		// fallback is honored), not shadow report-only runs.
		//
		// Ordering invariant: this assignment runs before tea.NewProgram
		// below, and the TUI can only observe a 413 after a run starts —
		// which requires a submitted message through the live program. So
		// no event can trigger the recovery before the flag is set: the
		// startup race is closed by construction, not by synchronization.
		model.CompactionApplies = compactionMode == appconfig.CompactionModeEnabled
	}

	// Context-exhaustion safeguard (Phase B): install the executor's
	// programmatic compaction hook so a stream request the provider rejects
	// (or truncates) for context exhaustion is recovered in-process — the
	// executor compacts the ROOT session's history and retries the request,
	// instead of surfacing the deterministic failure to the user.
	//
	// The closure reuses historyCompactionRunner (the same adapter the TUI's
	// /jev-compact-context flow uses), which handles persisting the mutated
	// history, the error-log record, and the shadow-log run summary.
	//
	// Mutating (non-shadow) mode only: compaction-mode "shadow" computes the
	// honest would-save report without rewriting history — it cannot shrink
	// the request, so running it as "recovery" would burn a scoring round
	// and change nothing. With compaction off (no pipeline, compactionMode
	// off) or no scorer (backend unresolved), the hook is still installed
	// but fails with the typed executor.ErrCompactionUnavailable so the
	// executor's error text can say exactly why recovery was impossible
	// (guidance naming /jev-compact-context) instead of a bare failure.
	// Process-wide like every executor hook (root + subagents share it); a
	// subagent's context-exhaustion failure recovers through the root
	// session's history, which is the conversation the guard can shrink.
	if compactionPipeline != nil && compactionMode != appconfig.CompactionModeOff {
		runner := historyCompactionRunner(sess, compactionPipeline.HistoryScorer(), compactionStore,
			compactionMode != appconfig.CompactionModeEnabled, compactionThreshold, compactionShadowLog, historyGate)
		executor.SetContextCompactor(func(ctx context.Context) (session.CompactionReport, error) {
			return runner(ctx)
		})
	} else {
		// Recovery is impossible in this configuration: install a typed
		// failure so RunLoop's guard reports "auto-compaction unavailable"
		// rather than a generic error, and still records the event.
		executor.SetContextCompactor(func(ctx context.Context) (session.CompactionReport, error) {
			return session.CompactionReport{}, executor.ErrCompactionUnavailable
		})
	}

	// Retrieval hooks (Step 17): BaseOrchestrator runs the hook at every
	// turn start — right before that turn's stream request — so each agent
	// (root and every subagent, which each own a session) stages retrieved
	// context into its own request's work area. The hook owns its errors:
	// a failed retrieval warns once and the turn proceeds without it; the
	// next turns retry, so one flaky scoring round never disables the
	// feature for the session.
	var retrievalWarnOnce sync.Once
	var retrievalHookFor func(s *session.Session) func(context.Context)
	// diagSink reads the mid-session diagnostics sink at hook-run time: diag
	// (below, after the TUI program exists) assigns it, so closures created
	// here — before the program starts — route their warnings through the
	// live TUI instead of raw stderr. The write happens before p.Run() and
	// before any agent run can start (runs begin only when the TUI submits
	// a message), so the assignment happens-before every read. nil (CLI
	// flows, bootstrap) keeps the os.Stderr fallback.
	var diagSink func(msg string)
	if compactionRetrieval && compactionPipeline != nil {
		retrievalHookFor = func(s *session.Session) func(context.Context) {
			return func(ctx context.Context) {
				if _, err := s.InjectRetrieved(ctx, compactionPipeline, compactionStore,
					compaction.DefaultRetrieveK, compaction.DefaultRetrieveBudgetTokens, compaction.DefaultRetrieveThreshold); err != nil {
					retrievalWarnOnce.Do(func() {
						if diagSink != nil {
							diagSink(fmt.Sprintf("Warning: compaction retrieval skipped (%v); later turns retry\n", err))
							return
						}
						fmt.Fprintf(os.Stderr, "Warning: compaction retrieval skipped (%v); later turns retry\n", err)
					})
				}
			}
		}
		rootAgent.SetRetrievalHook(retrievalHookFor(sess))
	}

	// Register plugin slash commands + theme catalog so plugin commands fire
	// when the user presses Enter.
	if pluginManager != nil && pluginManager.Count() > 0 {
		model.SetPluginCommands(pluginManager.PluginCommands())

		// Map plugin.ThemeInfo to tui.ThemeEntry so the /themes picker and
		// inline `/themes <name>` can resolve plugin themes at runtime.
		// Always include DefaultThemeEntry first so users can revert.
		pluginThemes := pluginManager.AllThemes()
		if len(pluginThemes) > 0 {
			entries := make([]tui.ThemeEntry, 0, len(pluginThemes)+1)
			entries = append(entries, tui.DefaultThemeEntry)
			for _, info := range pluginThemes {
				entries = append(entries, tui.ThemeEntry{
					ID:         info.ID,
					PluginName: info.PluginName,
					ThemeName:  info.ThemeName,
					Glamour:    info.Glamour,
				})
			}
			model.SetThemes(entries)
		}
	}

	// Fire OnSessionStart hooks for every enabled plugin in parallel. This
	// runs once, before the orchestrator is dispatched, so plugin scripts
	// can warm caches, register tools, or print startup announcements.
	if pluginManager != nil {
		pluginManager.CallOnSessionStartHooks()
	}

	// Detect if subagents use a different model/backend
	if resolvedSubagentConfig.BaseURL != resolvedOpenAIConfig.BaseURL ||
		resolvedSubagentConfig.APIKey != resolvedOpenAIConfig.APIKey ||
		resolvedSubagentConfig.Model != resolvedOpenAIConfig.Model {
		model.SubagentInfo = resolvedSubagentConfig.Model
	}
	model.ShowCWD = resolvedShowCWD
	// Set ShowTodoPane BEFORE the SetSize calls below: SetSize -> updateLayout
	// reserves the side-pane width for terminals >= 85 cols and silently
	// closes the pane again below that threshold — the same guard as the
	// /todos command, without a startup toast. Users on narrow terminals
	// can open it with /todos later, which shows the standard toast. If the
	// size cannot be detected here, the initial WindowSizeMsg runs the same
	// updateLayout path.
	model.ShowTodoPane = showTodoPane
	model.LazyHistory = true

	// SkillsInfo: estimated size of the skill surface for the TUI info bar.
	// executor.RegisterTools builds the activate_skill map from the same
	// directories; re-discovering here (read-only) keeps the executor API
	// untouched while giving the TUI the same view of the skills on disk.
	model.SkillsInfo = skillsInfoForTUI()

	pOpts := []tea.ProgramOption{
		tea.WithFPS(tui.FrameRate),
	}
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 && h > 0 {
		model.SetSize(w, h)
		pOpts = append(pOpts, tea.WithWindowSize(w, h))
	} else if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil && w > 0 && h > 0 {
		model.SetSize(w, h)
		pOpts = append(pOpts, tea.WithWindowSize(w, h))
	}

	// A degraded app config surfaces as the initial status-bar text so the
	// user sees it on the first paint; the async BootstrapStatusMsg traffic
	// below replaces it as soon as backend discovery reports. "Starting..."
	// only applies to a clean config load.
	if configLoadWarning != "" {
		model.BootstrapStatus = configLoadWarning
	} else {
		model.BootstrapStatus = "Starting..."
	}
	p := tea.NewProgram(model, pOpts...)

	// diag is the mid-session diagnostics sink: hook timeouts/errors, hook
	// stderr, and dropped-progress-event notices are delivered to the live
	// TUI as DiagnosticMsg warning toasts instead of raw
	// fmt.Fprintf(os.Stderr, ...) writes, which paint text over the
	// alt-screen (duplicated footer rows, displaced agent-name line). The
	// trailing newline the stderr formatting carries is trimmed here so the
	// toast text is clean. Sources without a sink installed (CLI flows,
	// pre-TUI bootstrap) still fall back to os.Stderr. Every diagnostic is
	// ALSO appended to the durable critical-error log
	// (~/.local/share/late/late-errors.log): a toast disappears with the
	// terminal, the file does not — best-effort, never fails the caller.
	diag := func(msg string) {
		common.LogError("diagnostic", strings.TrimRight(msg, "\n"))
		p.Send(tui.DiagnosticMsg{Text: strings.TrimRight(msg, "\n")})
	}
	// Publish the sink to closures created before the program existed (see
	// diagSink above), and give the compaction pipeline's one-time
	// auth-poison warning the same route: it can fire mid-session (first
	// scoring call after a key is revoked) and must not paint raw stderr
	// over the alt-screen either.
	diagSink = diag
	if compactionPipeline != nil {
		compactionPipeline.SetWarningSink(diag)
	}

	// toolSync serializes plugin/MCP tool-registry refreshes triggered by
	// MCP servers' own tools/list_changed notifications (wired via
	// mcpClient.OnToolsChanged below). It recomputes the full current tool/
	// command/theme set and diffs it against the last set sent to the TUI.
	toolSync := &pluginToolSync{prev: append([]string(nil), pluginToolNames...)}
	mcpClient.OnToolsChanged = func() {
		toolSync.refresh(p, mcpClient, pluginManager, enabledTools)
	}

	// Wire TUI integration
	go func() {
		// Set messenger first
		p.Send(tui.SetMessengerMsg{Messenger: p})

		// Install the diagnostics sink now that the program is live: every
		// mid-session diagnostic source reports through the TUI instead of
		// raw stderr (see diag above). Subagent orchestrators are separate
		// instances created per spawn — the runner below installs the same
		// sink on each child.
		if pluginManager != nil {
			pluginManager.SetDiagnostics(diag)
		}
		rootAgent.SetDiagnostics(diag)
		tool.SetDiagnostics(diag)

		if resumedSessionTitle != "" {
			p.Send(tui.BootstrapStatusMsg{
				Text:   resumedSessionTitle,
				Active: false,
			})
		}

		// Create context with InputProvider
		ctx := context.WithValue(context.Background(), common.InputProviderKey, tui.NewTUIInputProvider(p))
		switch permissionMode {
		case appconfig.PermissionModeUnsupervised:
			ctx = context.WithValue(ctx, common.SkipConfirmationKey, true)
		case appconfig.PermissionModeForceRevaluate:
			ctx = context.WithValue(ctx, common.SkipConfirmationKey, true)
			ctx = context.WithValue(ctx, common.ForceRevaluateKey, true)
		}
		ctx = context.WithValue(ctx, common.MaxStreamRetriesKey, resolvedMaxStreamRetries)
		rootAgent.SetContext(ctx)

		// Set middlewares (see buildMiddlewares for ordering rationale).
		rootAgent.SetMiddlewares(buildMiddlewares(pluginManager, p, sess.Registry))

		// Start forwarding events from the root agent to the TUI
		ForwardOrchestratorEvents(p, rootAgent)

		// Wait only in this background goroutine: the TUI remains usable while
		// connections and discovery finish, but --prompt needs their results.
		runBootstrap(p, mcpClient, config, c, subagentClient, sess, enabledTools, pluginManager, toolSync, resolvedSuppressThinkingWords, explicitUserLogitBias, explicitSubagentLogitBias)

		if *promptReq != "" {
			p.Send(tui.StartPromptMsg(*promptReq))
		}
	}()

	// Startup compaction probe (Step 16): one cheap ScoreBatch with a single
	// small item against the resolved backend, in its own goroutine so the
	// first paint never waits for the backend. A failure never tears the
	// pipeline down — scoring is fail-open by contract and shadow mode is
	// harmless — it warns once on stderr, surfaces the reason in the status
	// bar, and, ONLY for a typed auth rejection, disables the session's
	// scoring through the same path a live 401 takes (the probe's client is
	// a throwaway, so without this the live pipeline would learn on its
	// first real scoring call against a backend that can only say 401). The
	// probe is deliberately NOT logged as a shadow decision: it is not a
	// scoring decision, and one probe line per launch would pollute the
	// replay ledger.
	if compactionPipeline != nil && compactionBackend != nil {
		probeBackend := *compactionBackend
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), compactionProbeTimeout)
			defer cancel()
			if err := compaction.ProbeBackend(ctx, probeBackend, ""); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: compaction backend probe failed (%v); scoring fails open this session\n", err)
				common.LogErrorf("compaction", "backend probe failed: %v", err)
				p.Send(tui.BootstrapStatusMsg{
					Text:    "compaction: backend probe failed — scoring fails open",
					Warning: true,
				})
				var ce *compaction.Error
				if errors.As(err, &ce) && ce.Kind == compaction.KindAuth {
					compactionPipeline.DisableAuth(ce.Error())
				}
				return
			}
			p.Send(tui.BootstrapStatusMsg{Text: "compaction: backend probe OK", Active: false})
		}()
	}

	if resolvedEnableSubagents {

		// runEnv bundles the startup-scope collaborators every spawned or
		// resumed child shares (see subagentRunEnv).
		runEnv := &subagentRunEnv{
			pluginManager:    pluginManager,
			messenger:        p,
			root:             rootAgent,
			sess:             sess,
			toolArchive:      toolArchive,
			retrievalHookFor: retrievalHookFor,
			diag:             diag,
			idleTimeout:      resolvedSubagentIdleTimeout,
			idleKillAfter:    resolvedSubagentIdleKillAfter,
		}

		// The background half of subagent execution (spawn_subagent
		// "execution": "parallel"/"serial"): one scheduler per session owns
		// the parallel/serial state machine, the completion notifications
		// into this session, and the scheduler-driven manifest transitions
		// (queued at enqueue, running at actual launch). Terminal statuses
		// stay with the run closure via classifyAndReportSubagentOutcome.
		scheduler := newSubagentScheduler(sess, func(id, status string) error {
			return sess.MarkSubagentStatus(id, status, "", "", "")
		})

		// clientForAgentType resolves an agent type's routed client (the
		// agent_models entry, falling back to the default subagent client):
		// one closure shared by the unfreeze relaunch and the startup
		// live-restore, so both build children exactly like the fresh-spawn
		// path does.
		clientForAgentType := func(agentType string) *client.Client {
			if appConfig == nil {
				return nil
			}
			setting, ok := appConfig.GetModelForAgent(agentType)
			if !ok {
				return nil
			}
			var biasForSubagent map[string]int
			if setting.Model == resolvedSubagentConfig.Model {
				biasForSubagent = subagentClient.LogitBias()
			}
			routed := client.NewClient(client.Config{
				BaseURL:      setting.URL,
				APIKey:       setting.Key,
				Model:        setting.Model,
				EnableImages: resolvedEnableImages,
				LogitBias:    biasForSubagent,
				AppVersion:   common.Version,
			})
			routed.DiscoverBackend(context.Background())
			return routed
		}

		// unfreezeDeps bundles everything the unfreeze relaunch needs that
		// the runner resolved at startup — the model routing and the prompt
		// switches mirror the fresh-spawn path exactly.
		unfreeze := unfreezeDeps{
			clientFor:        clientForAgentType,
			defaultClient:    subagentClient,
			enabledTools:     enabledTools,
			injectCWD:        resolvedInjectCWD,
			gemmaThinking:    resolvedGemmaThinking,
			subagentMaxTurns: resolvedSubagentMaxTurns,
			messenger:        p,
			globalBudget:     resolvedSubagentTimeout,
		}

		// liveRestore re-lists the children the previous exit interrupted —
		// now LIVE: the same resume machinery the {"resume": "<id>"} path
		// uses relaunches them in the background through the scheduler, so a
		// message addressed to a restored child continues it instead of
		// refusing read-only. It runs after the scheduler exists and before
		// the TUI program starts (the restore runs inside the startup
		// goroutine, whose ordering with respect to the TUI run loop is
		// benign: AddChild is synchronous, and the run's events reach the
		// TUI whenever the forwarder attaches). A nil liveRestore (subagents
		// disabled) keeps the read-only stub fallback.
		liveRestore := &liveRestoreDeps{
			root:          rootAgent,
			sessionID:     effectiveSessionID,
			sess:          sess,
			scheduler:     scheduler,
			clientFor:     clientForAgentType,
			defaultClient: subagentClient,
			enabledTools:  enabledTools,
			injectCWD:     resolvedInjectCWD,
			gemmaThinking: resolvedGemmaThinking,
			maxTurns:      resolvedSubagentMaxTurns,
			messenger:     p,
			runEnv:        runEnv,
			globalBudget:  resolvedSubagentTimeout,
		}

		// The live restore runs here, inside the goroutine, after the
		// forwarder below has started: relaunching a child sends its
		// events through the root's channel, and a forwarder attached
		// first can never drop them. Ordering with the TUI program is
		// benign — p.Send parks messages in Bubble Tea's queue until the
		// run loop starts. When subagents are disabled the read-only
		// fallback above already handled the manifest.
		if resumedManifest != nil {
			restoreInterruptedSubagents(rootAgent, resumedManifest, restoredSubagents, liveRestore)
		}

		// resumeSubagent implements spawn_subagent's "resume" argument
		// (Phase C): the parent asks for a dead/crashed subagent by ID and
		// gets the SAME child back — same ID, full persisted history,
		// complete fresh-spawn tool surface — running again from where its
		// history stopped. A closure inside main so it can read the
		// resolved startup settings; the pure record validation it relies
		// on (validateResumeRecord) is package-level and unit-tested.
		resumeSubagent := func(ctx context.Context, request tool.SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
			manifest, err := session.LoadSubagentManifest(effectiveSessionID)
			if err != nil {
				return "", fmt.Errorf("resume %s: failed to load the session manifest: %w", request.ResumeID, err)
			}
			record, ok := manifest.Get(request.ResumeID)
			if !ok {
				return "", fmt.Errorf("no subagent %q in this session's manifest — resume needs the ID the original spawn reported", request.ResumeID)
			}
			if err := validateResumeRecord(record); err != nil {
				return "", err
			}

			// Re-validate a recorded worktree against the CURRENT
			// repository before trusting it (the worktree may have been
			// pruned between runs); a worktree that no longer resolves
			// still resumes, just with the recorded directory dropped from
			// the prompt and the tool context.
			worktreeDir := ""
			if record.WorktreePath != "" {
				if wt, wtErr := tool.ValidateWorktree(record.WorktreePath); wtErr == nil {
					worktreeDir = wt
				} else {
					common.LogErrorf("subagent-resume", "subagent %s worktree no longer valid: %v", record.ID, wtErr)
				}
			}

			agentType := record.AgentType
			if agentType == "" {
				agentType = request.AgentType
			}

			runBudget := effectiveSubagentBudget(timeoutOverride, resolvedSubagentTimeout)
			runCtx := ctx
			var runCancel context.CancelFunc = func() {}
			if runBudget > 0 {
				runCtx, runCancel = context.WithTimeout(ctx, runBudget)
			}
			// Carry the cancel inside the context for the stall watchdog
			// (withStallCancel) — a resumed run can wedge exactly like a
			// fresh one.
			runCtx = withStallCancel(runCtx, runCancel)
			defer runCancel()

			var currentSubagentClient *client.Client
			if appConfig != nil {
				if setting, ok := appConfig.GetModelForAgent(agentType); ok {
					var biasForSubagent map[string]int
					if setting.Model == resolvedSubagentConfig.Model {
						biasForSubagent = subagentClient.LogitBias()
					}
					currentSubagentClient = client.NewClient(client.Config{
						BaseURL:      setting.URL,
						APIKey:       setting.Key,
						Model:        setting.Model,
						EnableImages: resolvedEnableImages,
						LogitBias:    biasForSubagent,
						AppVersion:   common.Version,
					})
					currentSubagentClient.DiscoverBackend(ctx)
				}
			}
			if currentSubagentClient == nil {
				currentSubagentClient = subagentClient
			}

			child, _, err := agent.NewResumedSubagentOrchestrator(currentSubagentClient, *record, agentType, enabledTools, resolvedInjectCWD, resolvedGemmaThinking, resolvedSubagentMaxTurns, rootAgent, p)
			if err != nil {
				return "", fmt.Errorf("resume %s failed: %w", record.ID, err)
			}

			res, err := buildAndWireChild(runEnv, child, wireChildConfig{
				runCtx:        runCtx,
				worktree:      worktreeDir,
				runBudget:     runBudget,
				requestCancel: runCancel,
			})
			return classifyAndReportSubagentOutcome(sess, child, agentType, record.Goal, res, err, subagentOutcomeContext{
				runCtx:     runCtx,
				runBudget:  runBudget,
				resumed:    true,
				finalWords: fmt.Sprintf("The %s subagent was resumed with its full prior context and completed its task. Final result:\n\n%s", agentType, res),
			})
		}

		runner := func(ctx context.Context, request tool.SubagentSpawnRequest, timeoutOverride *time.Duration) (string, error) {
			// Resume mode: restore a previously interrupted child instead
			// of spawning fresh. The manifest record must exist and be
			// non-terminal — a completed/failed/cancelled child is done
			// for good, and the parent is told to spawn fresh instead.
			// Resume runs synchronously regardless of the (ignored)
			// "execution" argument: the parent explicitly waits for the
			// restored child's continuation.
			if request.IsResume() {
				return resumeSubagent(ctx, request, timeoutOverride)
			}

			// Lifecycle actions (Phase 3): freeze pauses a live background
			// child, unfreeze relaunches a frozen one. Both are id-based
			// and ignore the spawn arguments entirely.
			switch request.Action {
			case tool.SubagentActionFreeze:
				return freezeRunningSubagent(scheduler, sess, effectiveSessionID, getSubagentActionID(request))
			case tool.SubagentActionUnfreeze:
				unfreeze.timeoutOverride = timeoutOverride
				return unfreezeFrozenSubagent(scheduler, runEnv, unfreeze, sess, effectiveSessionID, getSubagentActionID(request), ctx)
			case "":
				// Not an action: a normal spawn or resume below.
			default:
				return fmt.Sprintf("Error: unknown action %q — valid actions: %q, %q (or omit \"action\" to spawn/resume)",
					request.Action, tool.SubagentActionFreeze, tool.SubagentActionUnfreeze), nil
			}

			goal := request.Goal
			ctxFiles := request.CtxFiles
			agentType := request.AgentType
			worktree := request.Worktree

			var currentSubagentClient *client.Client
			// The child's agent_models routing decides two things: which
			// client it runs on, and (via the per-model parallel gate)
			// whether a requested "parallel" execution may actually run in
			// parallel. No routing = default subagent client = conservative
			// serial: a model must explicitly opt in with
			// "allow_parallel_execution": true on its models[] entry (see
			// appconfig.ModelSetting for the single-instance/rate-limit
			// motivation).
			var modelAllowsParallel bool
			var modelRef string
			if appConfig != nil {
				if setting, ok := appConfig.GetModelForAgent(agentType); ok {
					modelAllowsParallel = setting.AllowsParallelExecution()
					modelRef = setting.Reference()
					currentSubagentClient = clientForAgentType(agentType)
				}
			}
			if currentSubagentClient == nil {
				currentSubagentClient = subagentClient
			}

			child, err := agent.NewSubagentOrchestratorWithWorktree(currentSubagentClient, goal, ctxFiles, agentType, enabledTools, resolvedInjectCWD, resolvedGemmaThinking, resolvedSubagentMaxTurns, effectiveSessionID, saveSubagentHistories, worktree, rootAgent, p)
			if err != nil {
				return "", err
			}

			// Execution mode (validated by the tool; "" normalized to the
			// sync default). Sync keeps the historical blocking path;
			// parallel/serial hand the fully built child to the scheduler
			// and return a launch/queue acknowledgement immediately. The
			// EFFECTIVE mode applies the per-model parallel gate first: a
			// requested parallel on a model without
			// "allow_parallel_execution": true (or with no agent_models
			// routing at all) is downgraded to serial, so the scheduler only
			// ever receives effective modes.
			execMode, _ := tool.NormalizeSubagentExecution(request.Execution)
			if execMode != tool.SubagentExecutionSync {
				scheduling := subagentScheduling{
					Requested: execMode,
					Effective: effectiveSubagentExecutionMode(execMode, modelAllowsParallel),
					ModelRef:  modelRef,
				}
				return launchBackgroundSubagent(scheduler, runEnv, sess, effectiveSessionID, child, agentType, goal, worktree, ctx, timeoutOverride, resolvedSubagentTimeout, scheduling)
			}

			// Effective wall-clock budget for this run. Context layering:
			// parent ctx (cancellation) ⊇ run budget (deadline) — runCtx is
			// derived from ctx, so cancelling the parent still cancels the
			// child while the budget only adds a deadline. Precedence:
			// per-spawn override when positive > global resolved budget; an
			// explicit per-spawn "0" (unlimited) suppresses the global
			// budget; an absent override falls back to the global value.
			// runBudget is kept in a local var so error classification can
			// report e.g. "time budget exhausted (2h)".
			runBudget := effectiveSubagentBudget(timeoutOverride, resolvedSubagentTimeout)
			runCtx := ctx
			var runCancel context.CancelFunc = func() {}
			if runBudget > 0 {
				runCtx, runCancel = context.WithTimeout(ctx, runBudget)
			}
			defer runCancel()

			// Carry the cancel inside the context for the stall watchdog
			// (withStallCancel) — a wedged run must be cancellable even when
			// the budget is unlimited (runCancel stays a noop then).
			runCtx = withStallCancel(runCtx, runCancel)

			// Everything the fresh-spawn and resume paths share — archive
			// reinstall, retrieval hook, diagnostics, run context (parent
			// ctx + budget + worktree), idle policy, parent heartbeat and
			// busy marking, and the mid-turn snapshot ticker — lives in
			// buildAndWireChild, and the outcome classification + manifest
			// terminal write in classifyAndReportSubagentOutcome; the
			// runner body only keeps the per-path differences (constructor,
			// final wording).
			res, err := buildAndWireChild(runEnv, child, wireChildConfig{
				runCtx:    runCtx,
				worktree:  worktree,
				runBudget: runBudget,
			})
			return classifyAndReportSubagentOutcome(sess, child, agentType, goal, res, err, subagentOutcomeContext{
				runCtx:    runCtx,
				runBudget: runBudget,
			})
		}

		sess.Registry.Register(tool.SpawnSubagentTool{
			Runner: runner,
		})
		sess.Registry.Register(tool.SubagentResultsTool{
			Lookup: subagentResultsLookup(sess, scheduler, effectiveSessionID),
		})
	}

	if _, err := p.Run(); err != nil {
		fmt.Printf("Unspecified error: %v", err)
		os.Exit(1)
	}
}

// effectiveSubagentBudget selects the wall-clock budget for one subagent run.
// Precedence: a positive per-spawn override wins; an explicit per-spawn "0"
// (unlimited) suppresses the global budget; an absent override falls back to
// the global budget. Any non-positive result means unlimited (no deadline) —
// callers guard with "> 0" before deriving a WithTimeout context.
func effectiveSubagentBudget(timeoutOverride *time.Duration, globalBudget time.Duration) time.Duration {
	if timeoutOverride != nil {
		if *timeoutOverride > 0 {
			return *timeoutOverride
		}
		return 0 // explicit per-spawn unlimited suppresses the global budget
	}
	return globalBudget
}

// validateResumeRecord decides whether a manifest record can be live-resumed
// (spawn_subagent's "resume" argument): only an interrupted child — a
// non-terminal record (running, queued, or frozen) that a previous late exit
// killed — is resumable. Terminal records are done for good: completed work
// needs no resume, and failed/cancelled work was already terminated with a
// recorded cause, so the parent must spawn fresh instead. A record without
// a persisted history cannot be restored either: the conversation was
// in-memory only and is gone.
func validateResumeRecord(record *session.SubagentRecord) error {
	switch record.Status {
	case session.SubagentStatusRunning, session.SubagentStatusQueued, session.SubagentStatusFrozen:
		// The resumable cases: running-at-read = interrupted by exit
		// (legacy manifests); queued = accepted by the scheduler but
		// never launched before the exit; frozen = the explicit
		// interrupted status the resume synthesis persists.
	case session.SubagentStatusCompleted, session.SubagentStatusFailed, session.SubagentStatusCancelled:
		return terminalResumeError(record)
	default:
		return fmt.Errorf("subagent %s has unknown manifest status %q; spawn a fresh agent instead", record.ID, record.Status)
	}
	if record.HistoryPath == "" {
		return fmt.Errorf("subagent %s has no persisted history (subagent history persistence was disabled for that run) — its conversation cannot be restored; spawn a fresh agent instead", record.ID)
	}
	return nil
}

// terminalResumeError is the model-facing error for resuming a child whose
// manifest record is already terminal: the work is done (or done failing),
// so the parent must spawn fresh instead.
func terminalResumeError(record *session.SubagentRecord) error {
	switch record.Status {
	case session.SubagentStatusCompleted:
		return fmt.Errorf("agent %s already terminated (completed); spawn a fresh agent instead", record.ID)
	default:
		cause := record.Cause
		if cause == "" {
			cause = record.Status
		}
		return fmt.Errorf("agent %s already terminated (%s); spawn a fresh agent instead", record.ID, cause)
	}
}

// subagentOutcomeContext carries the per-run values the outcome classifier
// needs: the run context (for deadline detection) and whether the run was a
// resume (which only changes the wording).
type subagentOutcomeContext struct {
	runCtx    context.Context
	runBudget time.Duration
	// resumed marks the wording as post-resume ("terminated abnormally
	// again after resuming").
	resumed bool
	// finalWords is the completed-run wording; empty falls back to the
	// fresh-spawn default.
	finalWords string
}

// classifyAndReportSubagentOutcome is the shared tail of the fresh-spawn
// runner and the resume path: classify the termination BEFORE looking at
// err (budget, idle watchdog, user kill, and crashes all surface
// differently here — the idle-kill case must precede the user-cancel case,
// because a watchdog kill surfaces as a plain context.Canceled and only
// IdleKillReason distinguishes it from a user stop), write the terminal
// manifest record through sess, and build the model-facing result text.
// IdleKillReason is not part of common.Orchestrator; the interface
// assertion mirrors the SetContext/SetIdlePolicy ones in the runner.
func classifyAndReportSubagentOutcome(sess *session.Session, child common.Orchestrator, agentType, goal, res string, err error, octx subagentOutcomeContext) (string, error) {
	childIdleKillReason := ""
	if sa, ok := child.(interface{ IdleKillReason() string }); ok {
		childIdleKillReason = sa.IdleKillReason()
	}
	var cause string
	switch {
	case strings.HasPrefix(childIdleKillReason, "stalled:"):
		// Stall auto-resume (worker S): the watchdog's stall verdict is
		// already a complete, model-readable cause ("stalled: no activity
		// for ...; blocked in-flight tool call"). Use it VERBATIM — the
		// "stalled:" prefix must survive into the manifest record and the
		// parent notification, because the subagent_results lookup and the
		// resume directive branch below both key on it.
		cause = childIdleKillReason
	case childIdleKillReason != "":
		cause = fmt.Sprintf("idle: killed by the harness idle watchdog (%s)", childIdleKillReason)
	case octx.runBudget > 0 && octx.runCtx.Err() == context.DeadlineExceeded:
		cause = fmt.Sprintf("time budget exhausted (%s)", octx.runBudget)
	case errors.Is(err, context.Canceled) || child.IsStopRequested():
		cause = "cancelled or killed by the user"
	case err != nil:
		cause = fmt.Sprintf("crashed: %v", err)
	}
	if cause != "" {
		abnormal := "terminated abnormally"
		if octx.resumed {
			abnormal = "terminated abnormally again after resuming"
		}
		// Abnormal termination: hand the parent a pruned transcript so it
		// can understand the cause and resume without redoing the work.
		transcriptPath, terr := writeSubagentTranscript(child, agentType, goal, cause)
		summary := lastActionPreview(child.History(), 500)
		result := fmt.Sprintf("The %s subagent %s (%s).\nFull pruned transcript: %s\nLast actions:\n%s", agentType, abnormal, cause, transcriptPath, summary)
		if terr != nil {
			result += fmt.Sprintf("\n(transcript unavailable: %v)", terr)
		}
		// Stall auto-resume directive: a STALLED child is the one failure
		// shape whose checkpoint IS the preserved history — the parent can
		// restore it exactly and continue where it wedged. Spell the
		// directive out in the parent-facing text so the orchestrator can
		// act without guessing (both the sync result and the background
		// completion notification flow through here).
		if strings.HasPrefix(childIdleKillReason, "stalled:") {
			result += fmt.Sprintf("\nThis agent's conversation is fully preserved. Resume it to continue exactly where it stopped: call spawn_subagent with {\"resume\": %q}.", child.ID())
		}
		// Manifest terminal record (Phase 1): the parent can now see
		// at resume time that this child ended abnormally, with the
		// transcript preserved. Non-fatal: the transcript and the
		// returned result above already carry the information.
		childID := child.ID()
		if markErr := sess.MarkSubagentStatus(childID, session.SubagentStatusFailed, cause, "", transcriptPath); markErr != nil {
			common.LogErrorf("subagent-manifest", "failed to record terminal status for %s: %v", childID, markErr)
		}
		return result, nil
	}

	// Completed with the user's stop-request flag still set: the run
	// produced a final output before the cancellation landed, but it
	// is still a cancelled child for resume purposes.
	childID := child.ID()
	if child.IsStopRequested() {
		cancelled := "The subagent task was explicitly cancelled by the user"
		if octx.resumed {
			cancelled = "The resumed subagent task was explicitly cancelled by the user"
		}
		final := fmt.Sprintf("%s. Final output before cancellation:\n\n%s", cancelled, res)
		if markErr := sess.MarkSubagentStatus(childID, session.SubagentStatusCancelled, "cancelled or killed by the user", "", ""); markErr != nil {
			common.LogErrorf("subagent-manifest", "failed to record terminal status for %s: %v", childID, markErr)
		}
		return final, nil
	}

	if markErr := sess.MarkSubagentStatus(childID, session.SubagentStatusCompleted, "", previewText(res, manifestResultPreviewLimit), ""); markErr != nil {
		common.LogErrorf("subagent-manifest", "failed to record terminal status for %s: %v", childID, markErr)
	}
	if octx.finalWords != "" {
		return octx.finalWords, nil
	}
	return fmt.Sprintf("The subagent successfully completed its task. Final result:\n\n%s", res), nil
}

// stallCancelKey is the context key carrying a child run's cancel func so
// the stall watchdog callback (installed in buildAndWireChild) can cancel
// the WEDGED run. The cancel func travels in the same context it cancels —
// self-referential but safe: cancel() is stored before the watchdog can
// ever fire (the watchdog starts inside child.Execute, after SetContext).
type stallCancelKey struct{}

// getSubagentActionID extracts the "id" argument of a freeze/unfreeze
// action call. The spawn request struct does not carry it (actions have no
// goal), so it re-reads the raw arguments the same way CallString does.
func getSubagentActionID(request tool.SubagentSpawnRequest) string {
	return request.ActionID
}

// withStallCancel returns ctx carrying its own cancel func under
// stallCancelKey. Call it at run-context construction in both spawn paths
// (the sync runner's budget context, the background closure's) so the stall
// watchdog can always reach the kill switch.
func withStallCancel(ctx context.Context, cancel context.CancelFunc) context.Context {
	return context.WithValue(ctx, stallCancelKey{}, cancel)
}

// wireChildConfig carries the per-run values buildAndWireChild needs beyond
// the child itself. It is constructed inside main(), where the wiring
// inputs live.
type wireChildConfig struct {
	// runCtx is the child's execution context: parent ctx ⊇ run budget.
	runCtx context.Context
	// worktree, when non-empty, is layered into the child's tool context
	// as the execution base directory (WorktreeDirKey).
	worktree string
	// runBudget is the effective budget in force (0 = unlimited).
	runBudget time.Duration
	// requestCancel releases the budget context's resources after the
	// child finishes; nil when the caller releases it itself.
	requestCancel context.CancelFunc
}

// subagentRunEnv bundles the startup-scope collaborators the shared child
// executor needs: everything main() resolved once that every spawned or
// resumed child consumes. Keeping it a plain struct lets the runner and the
// resume closure share one value and keeps buildAndWireChild at package
// level (unit-testable with stub wiring).
type subagentRunEnv struct {
	pluginManager    *plugin.PluginManager
	messenger        tui.Messenger
	root             *orchestrator.BaseOrchestrator
	sess             *session.Session
	toolArchive      string
	retrievalHookFor func(*session.Session) func(context.Context)
	diag             func(string)
	idleTimeout      time.Duration
	idleKillAfter    time.Duration
}

// buildAndWireChild is the shared executor of a spawned-or-resumed child:
// it wires everything both spawn paths need — middlewares, the shared
// tool-output archive, the retrieval hook, the diagnostics sink, the run
// context (parent ctx + budget + worktree), the idle policy, the parent
// heartbeat and nested-busy marking, and the mid-turn snapshot ticker —
// then runs child.Execute("") and returns its result. The fresh-spawn
// runner and the resume closure both call it, so a resumed child behaves
// exactly like a freshly spawned one.
func buildAndWireChild(env *subagentRunEnv, child common.Orchestrator, cfg wireChildConfig) (string, error) {
	runCtx := cfg.runCtx
	if cfg.worktree != "" {
		runCtx = context.WithValue(runCtx, common.WorktreeDirKey, cfg.worktree)
	}
	if cfg.requestCancel != nil {
		defer cfg.requestCancel()
	}

	child.SetMiddlewares(buildMiddlewares(env.pluginManager, env.messenger, child.Registry()))

	// Tool-output archiving for the child: ExecuteToolCalls reads a
	// process-wide hook, so the runner re-installs the SAME shared
	// archive before every spawn. The hook is already in place from
	// startup (the root install), but the child runs on the same
	// session folder and must never depend on install ordering —
	// re-installing the same instance is a no-op semantically.
	if env.toolArchive != "" {
		if arch, archErr := session.NewOutputArchive(env.toolArchive); archErr == nil {
			executor.SetToolResultArchiver(arch)
		}
		// The child's shell calls spool into the same shared directory (the
		// root install already pointed it here; re-installing keeps children
		// independent of install ordering, mirroring the archive above).
		tool.SetShellSpoolDir(env.toolArchive)
	}

	// Retrieval read side (Step 17): children get the same per-turn
	// hook as the root agent — the work-area injection is per-agent
	// session, while the record store and pipeline are shared.
	if env.retrievalHookFor != nil {
		if bo, ok := child.(*orchestrator.BaseOrchestrator); ok {
			bo.SetRetrievalHook(env.retrievalHookFor(bo.Session()))
		}
	}

	// Diagnostics sink (same propagation shape as the retrieval hook
	// above): the child is its own BaseOrchestrator instance, so
	// without this its dropped-events reporting would fall back to
	// raw os.Stderr mid-session and paint over the alt-screen.
	if bo, ok := child.(*orchestrator.BaseOrchestrator); ok {
		bo.SetDiagnostics(env.diag)
	}

	// Stall auto-resume (Worker S): install the child's stall watchdog
	// callback — the ONE install for this child. A tool call blocked past
	// the idle threshold with no output is a WEDGE, not work — the
	// watchdog cancels the run context so the wedged agent unwinds instead
	// of holding its tool slot (and, for a background child, its serial
	// scheduler queue) forever. The runner's outcome path then records the
	// stall and notifies the parent with the resume directive
	// (classifyAndReportSubagentOutcome). The cancel func travels inside
	// the run context (withStallCancel), so the callback needs no other
	// wiring. SetStallPolicy REPLACES any previous callback, so no second
	// install may follow this one.
	if sa, ok := child.(interface {
		SetStallPolicy(cb func(id, cause string))
	}); ok && env.idleTimeout > 0 && cfg.runCtx != nil {
		runCtx := cfg.runCtx
		sa.SetStallPolicy(func(id, cause string) {
			common.LogErrorf("subagent-stall", "subagent %s %s — cancelled; state preserved, resumable", id, cause)
			if rc, ok := runCtx.Value(stallCancelKey{}).(context.CancelFunc); ok && rc != nil {
				rc()
			}
		})
	}

	// NewSubagentOrchestrator/NewResumedSubagentOrchestrator already
	// set the child's context from the parent (agent.go:
	// child.SetContext(parent.Context())) — keep that inheritance
	// and layer the run budget on top: runCtx is derived from the
	// runner ctx, so the parent ctx (cancellation) subsumes the
	// budget (deadline). BaseOrchestrator.Execute then derives its
	// run context from this one, so the deadline reaches the
	// executor run loop. SetContext is not part of
	// common.Orchestrator; the interface assertion keeps the spawn
	// working if the concrete child type ever changes (it then
	// simply keeps the parent context and runs without a budget
	// rather than failing the spawn).
	if sa, ok := child.(interface{ SetContext(context.Context) }); ok {
		sa.SetContext(runCtx)
	}

	// The child runs its own idle watchdog with the same policy as
	// the parent (it is a BaseOrchestrator too), so a stuck nested
	// agent reports idle — or kills itself — independently.
	if sa, ok := child.(interface {
		SetIdlePolicy(idle, killAfter time.Duration)
	}); ok {
		sa.SetIdlePolicy(env.idleTimeout, env.idleKillAfter)
	}

	// Stall auto-resume is installed ONCE, above: SetStallPolicy REPLACES the
	// previous callback rather than composing, so a second install here would
	// silently disarm the cancelling hook (a callback that only logs and never
	// cancels) and leave a wedged background child holding its scheduler slot
	// forever — the stall → cancel → notify → resume loop depends on the
	// watchdog cancel reaching runCtx through the stallCancelKey wiring above.

	// The child streams on its own session, so the parent shows no
	// progress while the nested run executes. Keep the parent's
	// activity alive with a 1/minute heartbeat and mark the nested
	// spawn busy, so the parent's idle watchdog neither fires nor
	// idle-kills while its child is legitimately working.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// env.root is a *orchestrator.BaseOrchestrator, which
				// satisfies common.ActivityMarker; the parent stays
				// visibly active while the child works.
				env.root.MarkActivity()
			}
		}
	}()
	// Mark the nested spawn busy on the parent for the idle watchdog.
	env.root.BeginNestedSpawn()
	defer env.root.EndNestedSpawn()

	// Mid-turn snapshot ticker (Phase 3a): the child persists history
	// only at message-commit boundaries, so the in-flight turn's
	// stream bytes live only in the executor's accumulator until that
	// commit. While the child runs, the ticker re-snapshots its
	// session history every subagentSnapshotInterval, shrinking the
	// crash-loss window to the interval plus the final flush. The
	// child session's SnapshotHistory copies the history under its
	// historyMu and writes outside the lock, so a tick never blocks
	// the streaming goroutine and never races a concurrent append.
	// The ticker starts just before Execute and stops in a defer; the
	// deferred stop performs the final flush so the window closes at
	// outcome time (the commit path already persisted every committed
	// message, so the flush is the belt to the commit's braces).
	// Snapshot failures are logged inside the helper, never fatal.
	if childSession := childSessionFor(child); childSession != nil {
		if stopSnapshot := startSubagentSnapshotTicker(child, childSession, subagentSnapshotInterval); stopSnapshot != nil {
			defer stopSnapshot()
		}
	}

	// The child continues from its loaded history: the LLM sees the
	// full prior context and carries the goal forward (empty input =
	// no new user turn).
	return child.Execute("")
}

// compactionCheckTimeout bounds the whole -check-compaction preflight (three
// stages of real requests against the backend, one attempt each; the offline
// backend's stages are local and finish in microseconds) and
// compactionProbeTimeout bounds the light startup probe. Generous enough for
// a slow local gateway, short enough that a dead endpoint cannot hang the
// flag or the startup path.
const (
	compactionCheckTimeout = 90 * time.Second
	compactionProbeTimeout = 30 * time.Second
)

// runCompactionCheck runs the compaction preflight against the backend the
// normal startup path resolves — the same compaction.ResolveBackendEnv("")
// call the pipeline wiring makes — and returns the process exit code: 0 when
// every stage passes, 1 otherwise. A missing backend or key is stage 0's
// failure: the report then says what to configure instead of starting a run
// that cannot score anything. With offline (config.json compaction-backend
// "offline") the same three stages run against the deterministic scripted
// scorer instead: no backend is resolved, no key is consulted, no request is
// sent, and the stages pass by construction — the demo path's self-test.
func runCompactionCheck(offline bool) int {
	if offline {
		ctx, cancel := context.WithTimeout(context.Background(), compactionCheckTimeout)
		defer cancel()
		results, ok := compaction.RunPreflightOffline(ctx)
		fmt.Print(compaction.FormatCheckReport(results, ok))
		if !ok {
			return 1
		}
		return 0
	}
	backend, backendErr := compaction.ResolveBackendEnv("")
	if backendErr != nil {
		fmt.Print(compaction.FormatCheckReport(noBackendCheckResults(backendErr), false))
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), compactionCheckTimeout)
	defer cancel()
	results, ok := compaction.RunPreflight(ctx, backend, "", nil)
	fmt.Print(compaction.FormatCheckReport(results, ok))
	if !ok {
		return 1
	}
	return 0
}

// noBackendCheckResults builds the stage-0 failure report for a run with no
// resolved compaction backend: the three real stages cannot run without one,
// and the detail carries the guidance plus the resolver's typed reason (which
// backend was tried and what each was missing).
func noBackendCheckResults(backendErr error) []compaction.CheckResult {
	return []compaction.CheckResult{{
		Stage:  compaction.CheckStageBackend,
		OK:     false,
		Detail: fmt.Sprintf("no compaction backend configured (set the provider key or run with -compaction-mode pointing at a gateway): %v", backendErr),
	}}
}

// openCompactionStore opens the persistent elided-record store at the
// default path (compaction.DefaultStorePath), degrading to the in-memory
// store — with a stderr warning — when the path cannot be resolved or the
// file cannot be opened. Compaction must keep working even when its
// persistence layer fails, exactly like the shadow log: the session loses
// only cross-restart pointer resolution, nothing else.
func openCompactionStore() *compaction.Store {
	path, err := compaction.DefaultStorePath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: compaction record store path unavailable (%v); continuing in-memory — elided originals will not survive restarts\n", err)
		common.LogErrorf("compaction-store", "record store path unavailable: %v", err)
		return compaction.NewStore()
	}
	return openCompactionStoreAt(path)
}

// openCompactionStoreAt is openCompactionStore for an explicit path; split
// out so tests can exercise the degrade-to-in-memory fallback without
// touching the real user store.
func openCompactionStoreAt(path string) *compaction.Store {
	store, err := compaction.OpenStore(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: compaction record store unavailable (%v); continuing in-memory — elided originals will not survive restarts\n", err)
		common.LogErrorf("compaction-store", "record store unavailable at %s: %v", path, err)
		return compaction.NewStore()
	}
	return store
}

// parseReplayThresholds parses a -replay-shadow value: a comma-separated
// list of keep thresholds in (0, 1], e.g. "0.10,0.35,0.50". Surrounding
// whitespace is tolerated. Empty entries, non-numeric values, and
// out-of-range thresholds are errors — the caller asked for an explicit
// replay, so silently clamping would misrepresent it.
func parseReplayThresholds(s string) ([]float64, error) {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("invalid -replay-shadow value %q: empty threshold", s)
		}
		th, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid -replay-shadow threshold %q: %v", part, err)
		}
		if th <= 0 || th > 1 {
			return nil, fmt.Errorf("invalid -replay-shadow threshold %v: must be in (0, 1]", th)
		}
		out = append(out, th)
	}
	return out, nil
}

// runReplayShadow prints the replay table (one row per threshold, re-decided
// from the log's recorded scores without any scorer round trip) plus the
// false-negative rate line for the default shadow log, then returns; the
// caller exits. Strictly read-only: a missing log is reported as "nothing
// scored yet" and nothing is created — the only constructor reached,
// NewShadowLogAt, runs after a stat confirmed the file exists (its parent
// mkdir is then a no-op) and nothing appends to it.
func runReplayShadow(thresholds []float64) error {
	path, err := compaction.DefaultShadowPath()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("No shadow log at %s — nothing scored yet (compaction modes shadow and enabled write it).\n", path)
			return nil
		}
		return err
	}
	shadowLog, err := compaction.NewShadowLogAt(path)
	if err != nil {
		return err
	}
	rows, err := shadowLog.ReplayTable(thresholds)
	if err != nil {
		return err
	}
	ffr, err := shadowLog.FalseNegativeRate()
	if err != nil {
		return err
	}
	fmt.Printf("Shadow log: %s\n\n", path)
	fmt.Print(formatReplayTable(rows, ffr))
	return nil
}

// formatReplayTable renders the replay rows as an aligned table — threshold,
// kept, relocated, tokens saved, still missed — followed by the
// false-negative rate line. Split from runReplayShadow so tests can pin the
// exact rendering.
func formatReplayTable(rows []compaction.ReplayRow, ffr float64) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "threshold\tkept\trelocated\ttokens saved\tstill missed")
	for _, r := range rows {
		fmt.Fprintf(tw, "%.2f\t%d\t%d\t%d\t%d\n", r.Threshold, r.Kept, r.Relocated, r.TokensSaved, r.StillMissed)
	}
	tw.Flush()
	fmt.Fprintf(&b, "\nfalse-negative rate: %.1f%%\n", ffr*100)
	return b.String()
}

// historyCompactionRunner adapts the live session for the TUI's
// /jev-compact-context command and auto-trigger: each call runs one
// session.CompactContext pass — scoring history segments against the ongoing
// task with the pipeline's decision client and relocating low scorers into
// the shared elide store — and persists the mutated history the same way the
// orchestrator's own SaveHistory call sites do. Shadow runs (compaction-mode
// "shadow") compute the honest would-save report without touching history,
// so they skip persistence. threshold mirrors the pipeline's elision
// threshold so both compaction paths make the same keep/elide calls.
// shadowLog receives one "history-run" summary line per run — shadow or
// mutating, failed or clean — so runs are auditable next to the per-segment
// decisions they produced; it may be nil (logging is unavailable), and an
// append failure is a stderr warning, never a run failure. gate is the
// pipeline's GateConfig: with it the walk makes the same per-kind-floor and
// max-elide-fraction-tripwire calls the tool-output path makes (and threads
// the shadow log down for the walk's own tripwire entries); nil keeps the
// legacy flat-threshold walk (tests that pin that behavior pass nil).
func historyCompactionRunner(sess *session.Session, scorer session.HistoryScorer, store session.ElideStore, shadow bool, threshold float64, shadowLog *compaction.ShadowLog, gate *compaction.GateConfig) func(context.Context) (session.CompactionReport, error) {
	return func(ctx context.Context) (session.CompactionReport, error) {
		report, err := sess.CompactContext(ctx, scorer, store, session.CompactionOptions{
			Threshold:  threshold,
			ShadowOnly: shadow,
			Gate:       gate,
			ShadowLog:  shadowLog,
		})
		if !shadow {
			// The walk mutated (or partially mutated — a mid-walk scorer
			// failure leaves consistent pointers and stored originals)
			// history: persist it even when err != nil. PersistHistory reads
			// the history under historyMu (a plain session.SaveHistory of
			// sess.History raced the snapshot ticker's copy) and is
			// generation-checked; for a fully-walked session it rewrites the
			// same bytes the walk itself just persisted.
			if saveErr := sess.PersistHistory(); saveErr != nil {
				err = errors.Join(err, fmt.Errorf("saving compacted history: %w", saveErr))
			}
		}
		if err != nil {
			// Durable record of the failure (walk aborts, save failures):
			// the toast/TUI notice is ephemeral, the error log is not.
			// Best-effort — never fails the run.
			common.LogErrorf("compaction", "history compaction run failed (shadow=%v): %v", shadow, err)
		}
		if shadowLog != nil {
			run := compaction.RunSummary{
				Shadow:       shadow,
				Scanned:      report.MessagesScanned,
				Scored:       report.MessagesScored,
				Elided:       report.SegmentsElided,
				TokensBefore: report.TokensBefore,
				TokensAfter:  report.TokensAfter,
				TokensSaved:  report.TokensSaved,
			}
			if err != nil {
				run.Err = err.Error()
			}
			// Best-effort: a logging failure must never fail the compaction
			// itself, so it only surfaces as a warning.
			if appendErr := shadowLog.AppendRun(report.TaskHash, run); appendErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: compaction run summary not logged (%v)\n", appendErr)
			}
		}
		return report, err
	}
}

// deriveEffectiveSessionID derives this run's session ID from the FINAL
// history path so resumed sessions keep their original ID. It returns ""
// for empty or unsafe results (a crafted meta file could claim an ID like
// ".."), which disables subagent history persistence for the run
// (in-memory fallback) instead of writing files outside the session folder.
func deriveEffectiveSessionID(historyPath string) string {
	id := strings.TrimSuffix(filepath.Base(historyPath), ".json")
	if id == "" || id == "." || id == ".." {
		return ""
	}
	return id
}

// initialBootstrapStatus decides what the TUI status bar shows before the
// async backend-discovery messages arrive. A failed app-config load returns
// the "config error: ..." warning (the error already names the exact config
// path); a clean load returns "" so the caller falls back to "Starting...".
// With the strict config rules this only fires for a recoverable load error
// (the post-load permission-hardening failure): every content error exits
// before the TUI starts.
func initialBootstrapStatus(loadErr error) string {
	if loadErr != nil {
		return fmt.Sprintf("config error: %v", loadErr)
	}
	return ""
}

// shouldExitOnConfigLoadError decides whether a failed app-config load must
// abort startup. Strict-config rule (R2/R3): late never starts on a
// config.json it could not fully load — a silent fallback to defaults would
// drop the user's real settings. LoadConfig signals those fatal cases
// (syntax errors, unknown entries, wrong-typed values, invalid enum and
// boolean values, unreadable file) by returning a nil config together with
// the rendered error; an error that still produced a usable config (the
// post-load permission-hardening failure) only warns.
func shouldExitOnConfigLoadError(cfg *appconfig.Config, err error) bool {
	return err != nil && cfg == nil
}

func newModelClient(ctx context.Context, setting appconfig.ModelSetting, enableImages bool, logitBias map[string]int) *client.Client {
	c := client.NewClient(client.Config{
		BaseURL:      setting.URL,
		APIKey:       setting.Key,
		Model:        setting.Model,
		EnableImages: enableImages,
		LogitBias:    logitBias,
		AppVersion:   common.Version,
	})
	// An explicit context-size-tokens declaration must be applied BEFORE
	// DiscoverBackend: the setter marks the value explicit, so discovery
	// still identifies the backend but never overwrites the declared
	// window. Applied after the (harmless) constructor either way — the
	// guard inside DiscoverBackend makes the ordering race-free.
	if ctxSize, ok := setting.ContextSizeOverride(); ok {
		c.SetContextSize(ctxSize)
	}
	c.DiscoverBackend(ctx)
	return c
}

// skillsInfoForTUI estimates the token footprint of the skill instructions
// available to agents (user + project skills directories, the same source
// executor.RegisterTools uses) so the TUI info bar can display it without
// re-reading skill files per frame. Count is the number of discovered
// skills; Tokens uses the common cl100k estimator over each skill's
// instruction body.
func skillsInfoForTUI() tui.SkillsInfo {
	info := tui.SkillsInfo{}
	skillDirs := []string{}
	if userSkillsDir, err := pathutil.LateSkillsDir(); err == nil {
		skillDirs = append(skillDirs, userSkillsDir)
	}
	skillDirs = append(skillDirs, pathutil.LateProjectSkillsDir())
	skills, err := skill.DiscoverSkills(skillDirs)
	if err != nil {
		return info
	}
	for _, s := range skills {
		info.Count++
		info.Tokens += common.EstimateTokenCount(s.Instructions)
	}
	return info
}

func validateSuppressThinkingWords(suppressThinkingWords bool, orchestratorModel, subagentModel string, appConfig *appconfig.Config) error {
	if !suppressThinkingWords {
		return nil
	}
	if orchestratorModel != subagentModel {
		return fmt.Errorf("orchestrator and subagents use different models (%q vs %q)", orchestratorModel, subagentModel)
	}
	if appConfig != nil {
		for _, sub := range assets.GetSubagents() {
			if setting, ok := appConfig.GetModelForAgent(sub.Name); ok && setting.Model != orchestratorModel {
				return fmt.Errorf("subagent %q uses a different model (%q vs %q)", sub.Name, setting.Model, orchestratorModel)
			}
		}
	}
	return nil
}

// buildMiddlewares assembles the tool-call middleware chain for rootAgent and subagents.
// Middlewares are applied innermost-last, so the plugin onToolCall hooks
// run FIRST (outermost), then the TUI confirmation, then the onToolResult
// hooks. Confirmation must see the arguments AFTER plugins mutated them —
// otherwise a plugin could change the arguments after the user approved
// the call.
func buildMiddlewares(pluginManager *plugin.PluginManager, p tui.Messenger, registry *common.ToolRegistry) []common.ToolMiddleware {
	mws := []common.ToolMiddleware{}
	if pluginManager != nil {
		mws = append(mws, pluginManager.BuildHookMiddlewares(func(ctx context.Context, tc client.ToolCall) bool {
			return tui.ToolRequiresConfirmation(ctx, registry, tc)
		})...)
	}
	mws = append(mws, tui.TUIConfirmMiddleware(p, registry))
	if pluginManager != nil {
		mws = append(mws, pluginManager.BuildToolResultMiddlewares()...)
	}
	return mws
}

// pluginToolSync serializes tool/command/theme refreshes sent to the TUI.
// An MCP server's own tools/list_changed notification (wired via
// mcp.Client.OnToolsChanged) can trigger it to recompute the current set.
// Without the mutex, concurrent refreshes could interleave and send a
// diff computed against a stale `prev`.
type pluginToolSync struct {
	mu   sync.Mutex
	prev []string
}

// refresh recomputes the full current tool set (MCP + inline, with
// cross-source name collisions resolved the same way as the initial
// registration in main()), plus the current plugin commands/themes, and
// sends one PluginChangeMsg diffed against the last set this synced. The
// full command/theme set is always included — never a partial message —
// so a tools-only trigger (an MCP tool list change) can't blank out
// plugin commands/themes in the TUI.
func (s *pluginToolSync) refresh(p *tea.Program, mcpClient *mcp.Client, pluginManager *plugin.PluginManager, enabledTools map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	used := make(map[string]bool)
	var added []common.Tool
	for _, t := range mcpClient.GetTools() {
		if !mcpToolEnabled(t, enabledTools) {
			continue
		}
		added = append(added, t)
		used[t.Name()] = true
	}

	var cmds []string
	var entries []tui.ThemeEntry
	if pluginManager != nil {
		for _, t := range pluginManager.GetInlineTools(used) {
			if !toolEnabled(enabledTools, t.Name) {
				continue
			}
			added = append(added, pluginInlineTool{
				name:        t.Name,
				description: t.Description,
				parameters:  t.Parameters,
				runner:      t.Runner,
			})
		}

		cmds = pluginManager.PluginCommands()
		pluginThemes := pluginManager.AllThemes()
		if len(pluginThemes) > 0 {
			entries = make([]tui.ThemeEntry, 0, len(pluginThemes)+1)
			entries = append(entries, tui.DefaultThemeEntry)
			for _, info := range pluginThemes {
				entries = append(entries, tui.ThemeEntry{
					ID:         info.ID,
					PluginName: info.PluginName,
					ThemeName:  info.ThemeName,
					Glamour:    info.Glamour,
				})
			}
		}
	}

	p.Send(tui.PluginChangeMsg{
		Commands:     cmds,
		Themes:       entries,
		RemovedTools: s.prev,
		AddedTools:   added,
	})
	s.prev = s.prev[:0]
	for _, t := range added {
		s.prev = append(s.prev, t.Name())
	}
}

// mcpToolEnabled preserves raw-name settings even when the exposed MCP
// name has been sanitized, truncated, or deduplicated.
func mcpToolEnabled(t tool.Tool, enabledTools map[string]bool) bool {
	if enabled, ok := enabledTools[t.Name()]; ok {
		return enabled
	}
	if named, ok := t.(interface{ BareName() string }); ok {
		if enabled, exists := enabledTools[named.BareName()]; exists {
			return enabled
		}
	}
	return toolEnabled(enabledTools, t.Name())
}

// toolEnabled checks exact names before legacy aliases. Unknown tools
// default to enabled.
func toolEnabled(enabledTools map[string]bool, name string) bool {
	if v, ok := enabledTools[name]; ok {
		return v
	}
	if idx := strings.Index(name, "__"); idx >= 0 {
		legacy := name[:idx] + ":" + name[idx+2:]
		if v, ok := enabledTools[legacy]; ok {
			return v
		}
	}
	if v, ok := enabledTools[common.BareToolName(name)]; ok {
		return v
	}
	// Old config keys may contain punctuation removed from exposed names.
	// A disabled matching alias wins when multiple raw keys sanitize alike.
	for raw, enabled := range enabledTools {
		if !enabled && common.SanitizeToolName(raw) == common.BareToolName(name) {
			return false
		}
	}
	return true
}

type sessionCommandResult struct {
	HistoryPath string
	Meta        *session.SessionMeta
	ShouldExit  bool
}

// validateContinueFlags enforces that at most one of --continue and
// --continue-project is passed: both select the session to resume, so
// requesting both is ambiguous. The messaging mirrors the permission-flag
// exclusivity error.
func validateContinueFlags(continueFlag, continueProjectFlag bool) error {
	if continueFlag && continueProjectFlag {
		return fmt.Errorf("continue flags are mutually exclusive; pass at most one of -continue, -continue-project")
	}
	return nil
}

// resolveContinueSession returns the session to resume for --continue: the
// most recently updated session overall, regardless of which project
// directory it was started in. It returns (nil, nil) when no sessions exist.
func resolveContinueSession() (*session.SessionMeta, error) {
	return session.GetLatestSession()
}

// resolveContinueProjectDir returns the project directory that scopes
// --continue-project: the git repository root containing the current working
// directory (so the flag also works from inside a subdirectory), or the
// working directory itself when it is not inside a git repository.
func resolveContinueProjectDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determining current directory: %w", err)
	}
	if root, ok := git.RepoRoot(cwd); ok {
		return root, nil
	}
	return cwd, nil
}

// resolveContinueProjectSession returns the session to resume for
// --continue-project: the most recently updated session whose recorded
// project directory is the current project. It returns (nil, nil) when no
// matching session exists.
func resolveContinueProjectSession() (*session.SessionMeta, error) {
	projectDir, err := resolveContinueProjectDir()
	if err != nil {
		return nil, err
	}
	return session.GetLatestSessionForDir(projectDir)
}

// handleSessionCommand processes session subcommands.
func handleSessionCommand(args []string) sessionCommandResult {
	if len(args) == 0 {
		fmt.Println("Usage: late session <list|load|delete> [args...]")
		fmt.Println("")
		fmt.Println("Commands:")
		fmt.Println("  list [-v]      List all saved sessions (use -v for verbose/detailed view)")
		fmt.Println("  load <id>      Load a session by ID (can use prefix)")
		fmt.Println("  delete <id>    Delete a session by ID")
		return sessionCommandResult{}
	}

	// Parse flags for specific commands
	verbose := false
	commandArgs := args

	switch args[0] {
	case "list":
		// Parse flags for list command
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		verbosePtr := fs.Bool("v", false, "Verbose output")
		_ = fs.Parse(args[1:])
		verbose = *verbosePtr
		commandArgs = fs.Args()
	case "load", "delete":
		// These commands don't use flags, just pass through
		// commandArgs should be args[1:] to skip the command name
		if len(args) > 1 {
			commandArgs = args[1:]
		} else {
			commandArgs = []string{}
		}
	}

	switch args[0] {
	case "list":
		handleSessionList(verbose)
		return sessionCommandResult{ShouldExit: true}
	case "load":
		if len(commandArgs) < 1 {
			fmt.Println("Error: session ID required")
			fmt.Println("Usage: late session load <id>")
			os.Exit(1)
		}
		meta := handleSessionLoad(commandArgs[0])
		return sessionCommandResult{HistoryPath: meta.HistoryPath, Meta: meta}
	case "delete":
		if len(commandArgs) < 1 {
			fmt.Println("Error: session ID required")
			fmt.Println("Usage: late session delete <id>")
			os.Exit(1)
		}
		handleSessionDelete(commandArgs[0])
		return sessionCommandResult{ShouldExit: true}
	default:
		fmt.Printf("Unknown session command: %s\n", args[0])
		handleSessionCommand([]string{})
		return sessionCommandResult{ShouldExit: true}
	}
}

// handleSessionList displays all saved sessions
func handleSessionList(verbose bool) {
	metas, err := session.ListSessions()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing sessions: %v\n", err)
		os.Exit(1)
	}

	if len(metas) == 0 {
		fmt.Println("No sessions found.")
		fmt.Println("")
		fmt.Println("Use 'late session load <id>' to load a saved session or start a new session with 'late'.")
		return
	}

	fmt.Println("Available sessions:")
	for _, meta := range metas {
		fmt.Print(strings.TrimSpace(session.FormatSessionDisplay(meta, verbose)) + "\n")
	}
	fmt.Println(session.FormatResumePrompt())
}

// handleSessionLoad returns metadata for the given session ID.
func handleSessionLoad(id string) *session.SessionMeta {
	meta, err := session.LoadSessionMeta(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading session: %v\n", err)
		os.Exit(1)
	}
	if meta == nil {
		fmt.Fprintf(os.Stderr, "Session not found: %s\n", id)
		fmt.Println("")
		fmt.Println("Use 'late session list' to see available sessions.")
		os.Exit(1)
	}

	fmt.Printf("Resuming session: %s (%s)\n", meta.ID, meta.Title)
	time.Sleep(500 * time.Millisecond) // Give user a moment to see what's happening
	return meta
}

// handleSessionDelete removes a session
func handleSessionDelete(id string) {
	// TODO: remove
	meta, err := session.LoadSessionMeta(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading session: %v\n", err)
		os.Exit(1)
	}
	if meta == nil {
		fmt.Fprintf(os.Stderr, "Session not found: %s\n", id)
		fmt.Println("")
		fmt.Println("Use 'late session list' to see available sessions.")
		os.Exit(1)
	}

	// Delete metadata
	sessionsDir, err := session.SessionDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting session directory: %v\n", err)
		os.Exit(1)
	}
	metaPath := filepath.Join(sessionsDir, meta.ID+".meta.json")
	if err := os.Remove(metaPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error deleting metadata: %v\n", err)
		os.Exit(1)
	}

	// Delete history file
	if err := os.Remove(meta.HistoryPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error deleting history: %v\n", err)
		os.Exit(1)
	}

	// Delete the session's subagent history folder (hierarchical layout). No-op for
	// legacy flat sessions without a folder. Non-fatal: the session itself is already
	// gone, so don't block the success message on leftover artifacts.
	if err := session.RemoveSessionFolder(meta.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: Failed to delete subagent history folder: %v\n", err)
	}

	fmt.Printf("Deleted session: %s\n", meta.Title)
}

// handleWorktreeCommand processes worktree subcommands
// Returns: true if a valid command was handled, false otherwise
func handleWorktreeCommand(args []string) bool {
	if len(args) == 0 {
		fmt.Println("Usage: late worktree <command> [args...]")
		fmt.Println("")
		fmt.Println("Commands:")
		fmt.Println("  list              List all worktrees")
		fmt.Println("  create <path> [branch]  Create a new worktree at given path (defaults to current branch)")
		fmt.Println("  remove <path>     Remove a worktree")
		fmt.Println("  active            Show current worktree")
		return false
	}

	switch args[0] {
	case "list":
		handleWorktreeList()
		return true
	case "create":
		if len(args) < 2 {
			fmt.Println("Error: path required for create command")
			fmt.Println("Usage: late worktree create <path> [branch]")
			return true
		}
		path := args[1]
		branch := ""
		if len(args) >= 3 {
			branch = args[2]
		}
		handleWorktreeCreate(path, branch)
		return true
	case "remove":
		if len(args) < 2 {
			fmt.Println("Error: path required for remove command")
			fmt.Println("Usage: late worktree remove <path>")
			return true
		}
		handleWorktreeRemove(args[1])
		return true
	case "active":
		handleWorktreeActive()
		return true
	default:
		fmt.Printf("Unknown worktree command: %s\n", args[0])
		fmt.Println("")
		fmt.Println("Usage: late worktree <command> [args...]")
		fmt.Println("")
		fmt.Println("Commands:")
		fmt.Println("  list              List all worktrees")
		fmt.Println("  create <path> [branch]  Create a new worktree at given path (defaults to current branch)")
		fmt.Println("  remove <path>     Remove a worktree")
		fmt.Println("  active            Show current worktree")
		return false
	}
}

// handleWorktreeList displays all git worktrees
func handleWorktreeList() {
	worktrees, err := git.ListWorktrees()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error listing worktrees: %v\n", err)
		os.Exit(1)
	}

	if len(worktrees) == 0 {
		fmt.Println("No worktrees found.")
		return
	}

	fmt.Println("Git worktrees:")
	for _, wt := range worktrees {
		fmt.Printf("  %s", wt.Path)
		if wt.IsDetached {
			fmt.Printf(" (detached from %s)", wt.Branch)
		} else {
			fmt.Printf(" (%s)", wt.Branch)
		}
		if wt.Status != "" {
			fmt.Printf(" - %s", wt.Status)
		}
		fmt.Println()
	}
}

// handleWorktreeCreate creates a new worktree at the specified path
func handleWorktreeCreate(path string, branch string) {
	// If branch not specified, use current branch
	if branch == "" {
		cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
		output, err := cmd.Output()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting current branch: %v\n", err)
			os.Exit(1)
		}
		branch = strings.TrimSpace(string(output))
	}

	// Create the worktree
	if err := git.CreateWorktree(path, branch); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating worktree: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created worktree at %s (branch: %s)\n", path, branch)
}

// handleWorktreeRemove removes an existing worktree
func handleWorktreeRemove(path string) {
	if err := git.RemoveWorktree(path); err != nil {
		fmt.Fprintf(os.Stderr, "Error removing worktree: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Removed worktree at %s\n", path)
}

// handleWorktreeActive shows the currently active worktree
func handleWorktreeActive() {
	path, err := git.GetActiveWorktree()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error getting active worktree: %v\n", err)
		os.Exit(1)
	}

	fmt.Println(path)
}

// ForwardOrchestratorEvents is a helper that recursively forwards all events from an orchestrator
// to the Bubble Tea program.
func ForwardOrchestratorEvents(p *tea.Program, o common.Orchestrator) {
	go func() {
		for event := range o.Events() {
			p.Send(tui.OrchestratorEventMsg{Event: event})
			if added, ok := event.(common.ChildAddedEvent); ok {
				ForwardOrchestratorEvents(p, added.Child)
			}
		}
	}()
}

// runBootstrap runs startup work (MCP connections and LLM backend discovery)
// concurrently in the background so the TUI renders immediately. It streams live
// animated status updates into the UI and completes when all tasks finish.
func runBootstrap(p *tea.Program, mcpClient *mcp.Client, config *mcp.MCPConfig, c *client.Client, subagentClient *client.Client, sess *session.Session, enabledTools map[string]bool, pluginManager *plugin.PluginManager, toolSync *pluginToolSync, suppressThinkingWords bool, explicitUserLogitBias, explicitSubagentLogitBias map[string]int) {
	var (
		wg             sync.WaitGroup
		mu             sync.Mutex
		connected      int
		failed         []string
		logitBiasToast *tui.ToastMsg
	)

	sendMsg := func(msg tea.Msg) {
		if p != nil {
			p.Send(msg)
		}
	}

	hasMCP := config != nil && len(config.McpServers) > 0

	// Initial notification inside TUI
	if hasMCP {
		sendMsg(tui.BootstrapStatusMsg{
			Text:   "Connecting MCP servers & discovering backend...",
			Active: true,
		})
	} else {
		sendMsg(tui.BootstrapStatusMsg{
			Text:   "Discovering model backend...",
			Active: true,
		})
	}

	// Task 1: MCP Server Connections (concurrent)
	if hasMCP {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = mcpClient.ConnectFromConfigConcurrent(context.Background(), config, func(r mcp.ServerConnectResult) {
				mu.Lock()
				defer mu.Unlock()
				if r.Err != nil {
					failed = append(failed, r.Name)
					sendMsg(tui.BootstrapStatusMsg{
						Text:    fmt.Sprintf("MCP %s failed: %v", r.Name, r.Err),
						Warning: true,
						Active:  true,
					})
					return
				}
				for _, a := range r.Adapters {
					if !mcpToolEnabled(a, enabledTools) {
						continue
					}
					sess.Registry.Register(a)
				}
				if toolSync != nil {
					toolSync.refresh(p, mcpClient, pluginManager, enabledTools)
				}
				connected++
				sendMsg(tui.BootstrapStatusMsg{
					Text:   fmt.Sprintf("MCP: %s connected", r.Name),
					Active: true,
				})
			})
		}()
	}

	// Task 2: Main LLM Backend Discovery (concurrent)
	wg.Add(1)
	go func() {
		defer wg.Done()
		b := c.DiscoverBackend(context.Background())
		ctxSize := c.ContextSize()
		ctxText := ""
		if ctxSize > 0 {
			ctxText = fmt.Sprintf(" (%dk ctx)", ctxSize/1024)
		}
		sendMsg(tui.BootstrapStatusMsg{
			Text:        fmt.Sprintf("Backend: %s%s", b, ctxText),
			Active:      true,
			RefreshView: true,
		})

		if suppressThinkingWords {
			if c.IsLlamaCPP() {
				resolveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				resolved, err := client.ResolveThinkingBiases(resolveCtx, c.BaseURL(), c.APIKey(), c.HTTPClient())
				if err != nil {
					mu.Lock()
					logitBiasToast = &tui.ToastMsg{
						Text:    "Logit bias failed: tokenize error",
						Warning: true,
					}
					mu.Unlock()
				} else {
					c.SetLogitBias(client.MergeLogitBiases(resolved, explicitUserLogitBias))
					if subagentClient != c {
						subagentClient.SetLogitBias(client.MergeLogitBiases(resolved, explicitSubagentLogitBias))
					}
					mu.Lock()
					logitBiasToast = &tui.ToastMsg{
						Text: "Applied logit biases",
					}
					mu.Unlock()
				}
			} else {
				mu.Lock()
				logitBiasToast = &tui.ToastMsg{
					Text:    "Logit bias failed: not llama.cpp",
					Warning: true,
				}
				mu.Unlock()
			}
		} else if len(explicitUserLogitBias) > 0 || len(explicitSubagentLogitBias) > 0 {
			mu.Lock()
			logitBiasToast = &tui.ToastMsg{
				Text: "Applied logit biases",
			}
			mu.Unlock()
		}
	}()

	// Task 3: Subagent LLM Backend Discovery (if distinct client)
	if subagentClient != c {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = subagentClient.DiscoverBackend(context.Background())
		}()
	}

	// Wait for all background bootstrap tasks to finish
	wg.Wait()

	// Build final summary
	mu.Lock()
	totalMCP := connected + len(failed)
	var (
		parts []string
		warn  bool
	)
	if totalMCP > 0 {
		if len(failed) == 0 {
			unit := "server"
			if totalMCP != 1 {
				unit = "servers"
			}
			parts = append(parts, fmt.Sprintf("MCP: %d %s ready", connected, unit))
		} else {
			parts = append(parts, fmt.Sprintf("MCP: %d/%d (failed: %s)", connected, totalMCP, strings.Join(failed, ", ")))
			warn = true
		}
	}

	backendType := c.Backend()
	ctxSize := c.ContextSize()
	if backendType != "" && backendType != client.BackendUnknown {
		if ctxSize > 0 {
			parts = append(parts, fmt.Sprintf("Backend: %s (%dk)", backendType, ctxSize/1024))
		} else {
			parts = append(parts, fmt.Sprintf("Backend: %s", backendType))
		}
	}
	mu.Unlock()

	summary := "Ready"
	if len(parts) > 0 {
		summary = strings.Join(parts, " • ")
	}

	sendMsg(tui.BootstrapStatusMsg{
		Text:        summary,
		Warning:     warn,
		Active:      false,
		RefreshView: true,
		NextToast:   logitBiasToast,
	})
}
