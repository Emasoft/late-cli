package agent

import (
	"encoding/json"
	"fmt"
	"late/internal/assets"
	"late/internal/client"
	"late/internal/common"
	"late/internal/executor"
	"late/internal/orchestrator"
	"late/internal/session"
	"late/internal/tui"
	"os"
	"strings"
	"time"
)

// buildSubagentSystemPrompt resolves the subagent config for agentType and
// renders its prompt file with the CWD/gemma-thinking transforms shared by
// the fresh-spawn and resume constructors.
//
// cwdOverride wins when non-empty (the worktree path or, on resume, the
// spawn-time WorkingDir); otherwise the process CWD is used. Failing
// Getwd keeps the raw placeholder (the historical behavior).
func buildSubagentSystemPrompt(agentType string, injectCWD, gemmaThinking bool, cwdOverride string) (string, error) {
	configs := assets.GetSubagents()
	var config *assets.SubagentConfig
	for _, c := range configs {
		if c.Name == agentType {
			temp := c
			config = &temp
			break
		}
	}

	if config == nil {
		return "", fmt.Errorf("unknown agent type: %s", agentType)
	}

	content, err := assets.PromptsFS.ReadFile(config.PromptFile)
	if err != nil {
		return "", fmt.Errorf("failed to load embedded subagent prompt: %w", err)
	}
	systemPrompt := string(content)

	if injectCWD {
		cwd := cwdOverride
		if cwd == "" {
			if wd, err := os.Getwd(); err == nil {
				cwd = wd
			}
		}
		if cwd != "" {
			systemPrompt = common.ReplacePlaceholders(systemPrompt, map[string]string{
				"${{CWD}}": cwd,
			})
		}
	}

	if gemmaThinking {
		systemPrompt = "<|think|>" + systemPrompt
	}
	return systemPrompt, nil
}

// subagentParentSession asserts the parent carries a usable *session.Session
// (for the manifest) and is a *orchestrator.BaseOrchestrator (for the
// mutex-protected child-ID counter and AddChild).
func subagentParentSession(parent common.Orchestrator) (*orchestrator.BaseOrchestrator, *session.Session, error) {
	baseParent, ok := parent.(*orchestrator.BaseOrchestrator)
	if !ok {
		return nil, nil, fmt.Errorf("subagent parent must be a *orchestrator.BaseOrchestrator")
	}
	parentSession := (*session.Session)(nil)
	if ps, ok := parent.(interface{ Session() *session.Session }); ok {
		parentSession = ps.Session()
	}
	return baseParent, parentSession, nil
}

// registerSubagentTools fills a fresh child session's registry exactly like
// a fresh spawn: inherit everything from the parent registry except the
// orchestrator-only tools (spawn_subagent, the plan writer, todo tools),
// then register the config's allowed_tools intersected with the global
// enable map (todo tools stripped again, never registerable for children).
func registerSubagentTools(childSession *session.Session, parent common.Orchestrator, config *assets.SubagentConfig, enabledTools map[string]bool) {
	// Inherit all tools from parent (including MCP tools)
	if parent != nil && parent.Registry() != nil {
		for _, t := range parent.Registry().All() {
			// Skip spawn_subagent and write_implementation_plan to prevent recursion/confusion
			name := t.Name()
			if name == "spawn_subagent" || name == "write_implementation_plan" ||
				name == "create_todos" || name == "list_todos" || name == "finish_todo" {
				continue
			}
			childSession.Registry.Register(t)
		}
	}

	// Register explicitly allowed tools for the subagent
	subagentTools := make(map[string]bool)
	for _, t := range config.AllowedTools {
		if enabledTools[t] { // only enable if it's also enabled globally
			subagentTools[t] = true
		}
	}

	// Todo tools are orchestrator-only: never register them for subagents,
	// even if a subagent config lists them in allowed_tools.
	for _, name := range []string{"create_todos", "list_todos", "finish_todo"} {
		delete(subagentTools, name)
	}
	executor.RegisterTools(childSession.Registry, subagentTools)
}

// saveSpawnRecord registers the running record in the PARENT session's
// manifest. It happens AFTER the child's initial goal message is persisted,
// so the recorded HistoryPath already exists on disk — an interrupted
// child's preserved work is always actually there.
//
// The child session itself skips metadata by design (skipMetadata in
// session.NewSubagentSession), so the manifest — keyed by the parent
// session's folder — must be reached through the parent. Only root sessions
// carry a session folder (the record key is derived from the history path),
// so an in-memory parent session is a no-op. Session is not part of
// common.Orchestrator; the interface assertion mirrors the
// SetContext/SetIdlePolicy ones in cmd/late: if the concrete parent type
// ever changes, the spawn simply skips the manifest instead of failing. A
// failed write is NOT fatal to the spawn: losing a spawn record only
// degrades resume (the child keeps running and its history still lands on
// disk), while failing the spawn would lose the whole run over a
// bookkeeping hiccup.
func saveSpawnRecord(parentSession *session.Session, rec session.SubagentRecord) {
	if parentSession == nil {
		return
	}
	if err := parentSession.SaveSubagentRecord(rec); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record subagent %s in the session manifest: %v\n", rec.ID, err)
	}
}

// NewSubagentOrchestrator creates a new BaseOrchestrator for a subagent.
func NewSubagentOrchestrator(
	c *client.Client,
	goal string,
	ctxFiles []string,
	agentType string,
	enabledTools map[string]bool,
	injectCWD bool,
	gemmaThinking bool,
	maxTurns int,
	parentSessionID string,
	saveSubagentHistory bool,
	parent common.Orchestrator,
	messenger tui.Messenger,
) (common.Orchestrator, error) {
	return newSubagentOrchestrator(c, goal, ctxFiles, agentType, enabledTools, injectCWD, gemmaThinking, maxTurns, parentSessionID, saveSubagentHistory, "", parent, messenger)
}

// NewSubagentOrchestratorWithWorktree is NewSubagentOrchestrator for a child
// spawned into a pre-validated git worktree (the spawn_subagent
// "worktree" argument, resolved by tool.ValidateWorktree before the runner
// runs). The worktree path replaces the process CWD in the system prompt's
// ${{CWD}} and is recorded in the manifest as WorktreePath so a resume can
// restore the same working environment.
func NewSubagentOrchestratorWithWorktree(
	c *client.Client,
	goal string,
	ctxFiles []string,
	agentType string,
	enabledTools map[string]bool,
	injectCWD bool,
	gemmaThinking bool,
	maxTurns int,
	parentSessionID string,
	saveSubagentHistory bool,
	worktreeDir string,
	parent common.Orchestrator,
	messenger tui.Messenger,
) (common.Orchestrator, error) {
	return newSubagentOrchestrator(c, goal, ctxFiles, agentType, enabledTools, injectCWD, gemmaThinking, maxTurns, parentSessionID, saveSubagentHistory, worktreeDir, parent, messenger)
}

// newSubagentOrchestrator is the shared fresh-spawn constructor body.
func newSubagentOrchestrator(
	c *client.Client,
	goal string,
	ctxFiles []string,
	agentType string,
	enabledTools map[string]bool,
	injectCWD bool,
	gemmaThinking bool,
	maxTurns int,
	parentSessionID string,
	saveSubagentHistory bool,
	worktreeDir string,
	parent common.Orchestrator,
	messenger tui.Messenger,
) (common.Orchestrator, error) {
	baseParent, parentSession, err := subagentParentSession(parent)
	if err != nil {
		return nil, err
	}

	if saveSubagentHistory && parentSessionID != "" {
		if _, err := session.SubagentHistoryDir(parentSessionID); err != nil {
			return nil, fmt.Errorf("failed to resolve subagent history path: %w", err)
		}
	}

	systemPrompt, err := buildSubagentSystemPrompt(agentType, injectCWD, gemmaThinking, worktreeDir)
	if err != nil {
		return nil, err
	}

	// Mint the child ID up-front so it can be embedded in the subagent
	// history path. The parent's mutex-protected counter is the only ID
	// source that cannot collide under concurrent spawns.
	id, err := baseParent.NextChildID(agentType)
	if err != nil {
		return nil, err
	}

	// Setup Subagent Session (Isolated History; persisted only when opted in)
	var subagentHistoryPath string
	if saveSubagentHistory && parentSessionID != "" {
		path, err := session.SubagentHistoryPath(parentSessionID, id)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve subagent history path: %w", err)
		}
		subagentHistoryPath = path
	}

	config := subagentConfigFor(agentType)
	childSession := session.NewSubagentSession(c, subagentHistoryPath, []client.ChatMessage{}, systemPrompt)
	registerSubagentTools(childSession, parent, config, enabledTools)

	// Construct Initial Context. Unreadable ctx_files are NAMED, not
	// silently skipped: a dropped entry used to leave "Context Files:" with
	// nothing under it while the model believed the file's contents were
	// attached — a silent context loss the child then rediscovered (or
	// reported as done). The annotation keeps the spawn alive and tells the
	// child exactly which path is missing.
	initialMsg := fmt.Sprintf("Goal: %s", goal)
	if len(ctxFiles) > 0 {
		initialMsg += "\n\nContext Files:\n"
		for _, f := range ctxFiles {
			content, err := os.ReadFile(f)
			if err == nil {
				initialMsg += fmt.Sprintf("- %s:\n```\n%s\n```\n", f, string(content))
			} else {
				initialMsg += fmt.Sprintf("- %s: (could not be read: %v)\n", f, err)
			}
		}
	}
	initialMsg = strings.TrimRight(initialMsg, "\r\n")

	if err := childSession.AddUserMessage(initialMsg); err != nil {
		return nil, fmt.Errorf("failed to add initial message: %w", err)
	}

	// Manifest registration (Phase 1). WorkingDir captures the process CWD
	// — the best available truth about where the child actually worked,
	// because late currently runs every child in the process CWD; a spawn
	// into a git worktree records that worktree instead (WorktreePath).
	saveSpawnRecord(parentSession, session.SubagentRecord{
		ID:           id,
		AgentType:    agentType,
		Goal:         goal,
		CtxFiles:     ctxFiles,
		Status:       session.SubagentStatusRunning,
		SpawnedAt:    time.Now(),
		HistoryPath:  subagentHistoryPath,
		WorkingDir:   processCWDOrEmpty(),
		WorktreePath: worktreeDir,
	})

	child := finishSubagentOrchestrator(baseParent, id, childSession, messenger, maxTurns)
	return child, nil
}

// NewResumedSubagentOrchestrator restores a previously-spawned subagent as a
// LIVE orchestrator (Phase C of subagent persistence): the same child ID,
// its full persisted history (every committed message up to the death —
// snapshot ticker + final flush guarantee it), the complete fresh-spawn
// tool surface, and no goal re-append (the loaded history already opens
// with the original goal). The parent then calls Execute("") on it and the
// child continues exactly where its history stopped.
//
// record is the manifest entry for the child (validated as
// running/interrupted by the caller before this constructor runs).
// worktreeDir overrides the CWD injected into the system prompt when
// non-empty; otherwise the record's WorktreePath (when the original spawn
// ran in a worktree) or its WorkingDir (the spawn-time process CWD) is
// used, so a resumed agent keeps working in the original project even when
// late itself was re-launched from elsewhere.
//
// Same-ID guard: AddChild has no dedup, so when the root already lists a
// child with this ID (a restore-and-then-resume within one session), the
// restored child gets a "-r1"-suffixed live twin and the caller is told
// which ID was used.
func NewResumedSubagentOrchestrator(
	c *client.Client,
	record session.SubagentRecord,
	agentType string,
	enabledTools map[string]bool,
	injectCWD bool,
	gemmaThinking bool,
	maxTurns int,
	parent common.Orchestrator,
	messenger tui.Messenger,
) (common.Orchestrator, string, error) {
	if record.ID == "" {
		return nil, "", fmt.Errorf("resume requires a non-empty child ID")
	}
	if record.AgentType != "" && record.AgentType != agentType {
		return nil, "", fmt.Errorf("resume id %s belongs to a %s subagent, not %s", record.ID, record.AgentType, agentType)
	}

	baseParent, parentSession, err := subagentParentSession(parent)
	if err != nil {
		return nil, "", err
	}

	// Exact restore: reuse the RECORD's id unless the root already holds a
	// child with it (AddChild does not dedup; a duplicate ID would break
	// TUI tab addressing and manifest writes).
	id := record.ID
	for _, existing := range baseParent.Children() {
		if existing.ID() == id {
			id = id + "-r1"
			break
		}
	}

	// CWD precedence for the system prompt: explicit worktree (the caller
	// re-validated it at resume) > the spawn-time worktree > the
	// spawn-time process CWD > the current process CWD.
	cwdOverride := firstNonEmpty(worktreeForRecord(record), record.WorkingDir)

	systemPrompt, err := buildSubagentSystemPrompt(agentType, injectCWD, gemmaThinking, cwdOverride)
	if err != nil {
		return nil, "", err
	}

	// Load the child's persisted history (its full state at death).
	history, err := session.LoadHistory(record.HistoryPath)
	if err != nil {
		return nil, "", fmt.Errorf("resume %s: failed to load preserved history: %w", record.ID, err)
	}
	// An EMPTY load — a manually deleted file, or a record whose run
	// persisted nothing — means there is nothing to restore: the loaded
	// history is the child's only goal and context (no goal re-append on
	// resume), so continuing here would run a live child with NO task at
	// all, silently. Refuse with the spawn-fresh hint instead, mirroring
	// validateResumeRecord's HistoryPath == "" rule one level deeper (the
	// file was recorded, then lost).
	if len(history) == 0 {
		return nil, "", fmt.Errorf("resume %s: the preserved history at %s is empty or missing — its conversation cannot be restored; spawn a fresh agent instead", record.ID, record.HistoryPath)
	}

	// The resumed child appends new turns to the SAME history file its
	// earlier life used, so a later resume keeps the whole picture.
	childSession := session.NewSubagentSession(c, record.HistoryPath, history, systemPrompt)
	config := subagentConfigFor(agentType)
	registerSubagentTools(childSession, parent, config, enabledTools)

	// NO goal re-append: the loaded history already carries the original
	// goal message and every committed turn after it.

	// Manifest: count the resume and flip the record back to "running" so
	// a crash mid-resume again reads as interrupted. Identity fields (goal,
	// history path, worktree) are preserved; a collision-renamed live twin
	// still reports against the original record's ID.
	if markErr := parentSessionMarkResumed(parentSession, record.ID); markErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to mark subagent %s as resumed: %v\n", record.ID, markErr)
	}

	child := finishSubagentOrchestrator(baseParent, id, childSession, messenger, maxTurns)
	return child, id, nil
}

// finishSubagentOrchestrator is the shared tail of both constructors: wire
// the child into the parent (context inheritance + AddChild) and return it
// as the common.Orchestrator the runner knows.
func finishSubagentOrchestrator(parent *orchestrator.BaseOrchestrator, id string, childSession *session.Session, messenger tui.Messenger, maxTurns int) common.Orchestrator {
	mws := parent.Middlewares()

	if messenger != nil {
		mws = []common.ToolMiddleware{
			tui.TUIConfirmMiddleware(messenger, childSession.Registry),
		}
	}

	child := orchestrator.NewBaseOrchestrator(id, childSession, mws, maxTurns)
	child.SetContext(parent.Context())
	parent.AddChild(child)
	return child
}

// subagentConfigFor looks up the subagent config by name; the caller has
// already validated the agent type, so a miss yields nil and the tool
// registration then registers only the inherited registry.
func subagentConfigFor(agentType string) *assets.SubagentConfig {
	for _, c := range assets.GetSubagents() {
		if c.Name == agentType {
			temp := c
			return &temp
		}
	}
	return nil
}

// parentSessionMarkResumed counts the resume against the manifest through
// the parent session; a parent without a session is a no-op.
func parentSessionMarkResumed(parentSession *session.Session, id string) error {
	if parentSession == nil {
		return nil
	}
	return parentSession.MarkSubagentResumed(id)
}

// worktreeForRecord returns the record's worktree path, empty when the
// original spawn ran in the process CWD.
func worktreeForRecord(record session.SubagentRecord) string {
	return record.WorktreePath
}

// processCWDOrEmpty captures os.Getwd for the manifest, degrading to ""
// (the field is omitempty) when the CWD cannot be read.
func processCWDOrEmpty() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// firstNonEmpty returns the first non-empty string, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func FormatToolConfirmPrompt(tc client.ToolCall) string {
	var jsonObj map[string]interface{}
	args := tc.Function.Arguments
	if err := json.Unmarshal([]byte(args), &jsonObj); err == nil {
		pretty, _ := json.MarshalIndent(jsonObj, "", "  ")
		args = string(pretty)
	}
	return fmt.Sprintf("Execute **%s**:\n\n```json\n%s\n```", tc.Function.Name, args)
}
