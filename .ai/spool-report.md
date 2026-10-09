# Tool-call SPOOL — perfect resume implementation report

Branch `local/full`, base `4c5556f`. All gates green: gofmt clean, `go build ./...` OK,
full `go test ./... -race -count=1` OK (all 19 packages), golangci-lint v2.13.2 → **0 issues**.

## What landed

### 1. `internal/tool/spool.go` (new, ~250 lines with docs)

- `type Spool struct { dir string }`, `NewSpool(dir) *Spool` (lazy MkdirAll 0700),
  `(*Spool).New(callID) (*SpoolWriter, error)` — opens `<dir>/partial-<callID>.txt` (0600).
- `SpoolWriter` wraps the `os.File`: `Write(p)` is a strict pass-through (always returns
  `len(p), nil`; file-write errors are swallowed by design — losing transcript bytes must
  never fail a tool call), plus `Close()` (idempotent), `Path()`, `Empty()`, `Discard()`,
  and `Promote()`.
- `Promote()` renames the partial file to the content-addressed OutputArchive name
  `<sha256[:16]>.txt` (hash computed incrementally over the bytes that actually reached the
  file) and returns the new path; empty transcripts are removed and return `""`.
- Process-wide wiring mirroring `SetShellTimeout`: `SetShellSpoolDir(dir)` + package var
  `shellSpoolDir` (empty = spooling disabled), `newShellSpoolCallID()` mints
  `<unixnano>-<seq>` IDs (atomic sequence disambiguates same-nanosecond parallel calls),
  `startShellSpool(dir)` is fail-open (a failed open only disables spooling for that call).
- `runShellCapturing(cmd, spool)` is the tee seam: `cmd.Stdout` and `cmd.Stderr` share one
  `io.MultiWriter(buf, spool)` (identical writer value ⇒ os/exec serializes writes, same
  semantics as the previous `CombinedOutput`), so bytes reach disk as the command produces
  them and survive a process-group SIGKILL.
- `spoolFailureNote()` / `finishSpoolSuccess()` finalize the spool on the failure/success
  paths; `spoolFailureWording` is the shared constant for the retry-pointer sentence.

### 2. `internal/tool/implementations.go` (ShellTool.Execute)

- Replaced `output, err := cmd.CombinedOutput()` with `output, err := runShellCapturing(cmd, spool)`
  (spool opened after timeout resolution / validation, i.e. only for calls that actually run).
- Failure paths append the retry pointer to the result (`spoolFailureNote`), transcript stays
  on disk as `partial-<id>.txt`:
  - timeout: `errors.New(msg + note)` (this path returns an error, which ExecuteToolCalls
    surfaces as `Error executing tool bash: <msg>`);
  - kill-after-start race ("killed by cancellation before start"): result string + note;
  - non-zero exit (`Command failed with exit code N`) and plain exec error: result + note.
- Success path: `finishSpoolSuccess(spool)` — silent commands discard the empty transcript,
  non-empty ones promote to `<sha256[:16]>.txt`. Binary-output success also finalizes.
- Result strings for successful calls are byte-identical to before (spooling is invisible).

### 3. `cmd/late/main.go` (wiring, root + children)

- Root: inside the existing `session.NewOutputArchive(toolArchive)` success block,
  `tool.SetShellSpoolDir(toolArchive)` — the spool dir IS the session's `tool-outputs` dir,
  so failed calls leave `partial-*.txt` there and successful ones promote into the same
  content-addressed namespace. In-memory sessions (invalid session ID) get no spool,
  mirroring the archive.
- Children: `buildAndWireChild` re-installs `SetShellSpoolDir(env.toolArchive)` next to the
  existing archive re-install, so every spawned/resumed child shells through the same dir
  independent of install ordering.

### 4. Resume synthesis (`cmd/late/subagent_resume.go`)

- `spoolDirForHistoryPath(historyPath)` maps a child's history
  (`<session>/subagents/<id>.json`) to the shared spool dir (`<session>/tool-outputs`).
- `orphanedSpoolTranscripts(dir)` globs `partial-*.txt`, sorted (call IDs embed unix-nano
  start times ⇒ name order is chronological); missing dir ⇒ nil.
- `consumeSpoolTranscript(path)` renames to `path.consumed` (atomic same-dir rename).
- `synthesizeSpoolTranscriptNotes(child, spoolDir)` runs in `restoreInterruptedSubagentLive`
  right after `NewResumedSubagentOrchestrator` (before the background relaunch ⇒ before the
  child's first resumed request is rendered): for each orphan it appends to the child's
  LOADED history a message and consumes the file. Exactly one resume ever surfaces a given
  transcript; failure to append is logged, not fatal.
- **Message shape decision (design point 4):** role `"user"` with a `[late harness]` prefix —
  the same attribution as the executor's coder failure note. NOT role `"tool"`:
  `SanitizeForRequest` drops tool messages whose `ToolCallID` does not match a pending
  assistant tool_call in the loaded history (dangling-result poison), so a tool-role note
  could never reach the model. Content:
  `"[late harness] tool call failed: interrupted by a previous late exit. The transcript of
  its execution was preserved here — read it to decide whether to retry the tool call: <path>"`
  — the same sentence shape the live failure path emits (shared wording via
  `tool.spoolFailureWording` semantics).

### 5. Tests

`internal/tool/spool_test.go` (5 tests): pass-through/promote/mode-0600 contract;
empty-transcript discard; **cancelled long-running command** (echo + sleep 300, cancel at
200ms via ctx) leaves exactly one non-empty partial transcript containing the pre-kill
output, and the failed result carries the retry pointer; success promotes to the
content-addressed name with no partial left over and silent commands leave nothing;
exact failed-result wording (`Command failed with exit code 3` + full pointer sentence);
spooling disabled ⇒ nothing written.

`cmd/late/spool_resume_test.go` (4 tests): full synthesis through a real
`NewResumedSubagentOrchestrator` — two orphans surface as two user-role `[late harness]`
notes in chronological order on the loaded history, consumed markers written, noise
(`.consumed`, promoted archive) untouched, second pass surfaces nothing (idempotent);
`SanitizeForRequest` end-to-end — the user-role note survives the request render while the
counterfactual tool-role dangling note is dropped (pins the shape decision); discovery
helper glob/order/ignore rules; history-path→spool-dir mapping.

## Deliberate scope decisions

- No `SetToolSpool`/`SetToolSpoolForCall` executor hooks: the design asked for ShellTool
  constructor/setter injection only. Only shell calls spool; the executor seam stays untouched.
- Spooling is fail-open end to end: an unusable spool dir silently disables the feature
  (with an errorlog line per failed open), never a failed tool call.
- The kill-after-start race and in-flight-watchdog paths append the note (transcript exists,
  bytes were captured) — cancellation result wording itself is unchanged beyond the appended
  pointer, so `IsShellFailureResult` prefix matching is unaffected (notes are appended,
  never prefixed).
- `.consumed` markers are never re-surfaced and are garbage-collected with the session folder.

## Verification

```
gofmt -l (touched files)                  → clean
go build ./...                            → OK
go test ./... -race -count=1              → ok (19 packages, 0 failures)
golangci-lint v2.13.2 run ./...           → 0 issues
```
