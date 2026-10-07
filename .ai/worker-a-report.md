# Worker A report — Issue #1: OTP-approved exact commands are never gated again

Branch `local/full` @ d532a31. Nothing committed (per instructions). Scope: OTP validation path + allowlist store + gate message.

## Rule implemented
Once a gated command is re-run with a VALID `otp_code`, that exact command string is persisted into the **persistent GLOBAL** allow-list store that every late instance loads at policy-evaluation time → same exact string never prompts again, in any process/session. Different strings still gate; hard blocks always stay hard.

## Changes

### internal/tool/permissions.go (store)
- `persistedCommandEntry` gains `TimesApproved int` (`json:"times_approved,omitempty"`) — optional, backward/forward compatible with the existing `entries` format (`{"version", "entries": {key: {flags, saved_at, expires_at, version}}}`).
- Reserved exact-entry namespace (documented at the constants):
  - key = `ExactCommandKey(command)` = `"::exact::" + strings.TrimSpace(command)`
  - flags = `["__exact__"]` (marker; `allCommandsAllowlisted` only looks up parsed keys like `"rm"`/`"git log"`, which can never start with `:`, so exact entries can never widen — nor be widened by — flag-level allowances).
  - Normalization: boundary whitespace trimmed on store AND lookup; interior whitespace significant (`"rm  -rf x"` ≠ `"rm -rf x"`), mirroring the byte-for-byte OTP registry rule.
- `writeAllowedCommandsFile` — write path extracted from `SaveAllowedCommand` (behavior-preserving; optional `countBumps` for the approval counter).
- `SaveExactAllowedCommand(command string, global bool) error` — merges into the existing store, refreshes TTL, increments `times_approved` (prior count preserved across saves).
- `CountExactCommandApprovals(command string) int` — sums `times_approved` for the exact key across global+local raw stores (expired/removed-but-on-disk entries still count as past approvals — documented).
- `ExactCommandKey` exported for fixtures/inspection.

### internal/tool/ast_bridge.go (policy bypass)
- `astAnalyzer.exactAllowed` built in `newASTAnalyzer` from `::exact::` keys of the merged allow-list (`LoadAllAllowedCommands()` = global+local+session, re-read from disk on every `getAnalyzer` call → every instance/reload picks it up).
- In `Analyze`: exact approval clears `NeedsConfirmation` **only**, in two places — after `policy.Decide` (guarded by `!d.IsBlocked`) and on the parse-error path (after `parseErrorHardBlock`). Hard refusals (cd, redirects, search gate, unsafe cwd) are unreachable by design: they run before the gate in `revaluate.go`/middleware.

### internal/tool/otp.go (counters + message)
- Session approval counter `RecordOTPApproval`/`SessionOTPApprovals` (guarded by `otpMu`, reset with the OTP registry on conversation reset).
- `OTPRevaluateMessageDetailed(code, priorGlobal, session)` — block message now reports prior global approvals (0 → "never been approved and allowlisted before"), session count when >0, and the note "this exact command will be auto-allowed after this approval (global persistent allow-list, same exact string only)". Legacy `OTPRevaluateMessage` delegates unchanged.

### internal/tui/revaluate.go (gate)
- On `ConsumeOTP` success: best-effort `_ = tool.SaveExactAllowedCommand(params.Command, true)` (failed write never vetoes a granted approval; documented) + `RecordOTPApproval`.
- Block path emits `OTPRevaluateMessageDetailed(…, CountExactCommandApprovals, SessionOTPApprovals)`.
- Check order documented: allowlist check runs first via `RequiresConfirmation` → an exact-allowlisted command returns at the "safe command" branch and never reaches the OTP branch.

## Tests (all pass; `go test -race -count=1 ./...` fully green)
- `internal/tool/exact_allowlist_test.go` (new, 8 tests): global store format/marker/count; boundary-trim vs interior-whitespace; count increment + TTL refresh (pinned clock); local scope; **fresh-instance bypass** (`&ShellTool{}` after save → no confirmation) with different-command and double-space still gated; hard blocks survive exact approval; session counter + reset; message content.
- `internal/tui/revaluate_allowlist_test.go` (new, 5 tests): end-to-end block → OTP → execute → **fresh registry/middleware executes with NO otp_code**; allowlist-first ordering (no OTP issued for allowlisted command); invalid OTP persists nothing; block-message counters (fixture: expired entry with times_approved=3 + 2 session approvals); different command still gated after approval.
- `internal/tui/revaluate_test.go` (updated): `TestHandleForceRevaluate_OTPSingleUse` third phase — old expectation was "re-used OTP re-blocks the same command"; under issue #1 the command now passes via the persisted allowlist (gate returns not-handled → middleware auto-approves). OTP single-use itself is still asserted via `tool.ConsumeOTP` returning false after consumption, plus stale-OTP-rejected-for-a-different-command.

## Gates
- `gofmt -l internal/tool internal/tui`: clean.
- `go build ./...`: OK.
- `go test -race -count=1 ./...`: all 19 packages ok (re-run on final tree).
- `golangci-lint v2.13.2` (installed to temp GOBIN): 2 findings, both **pre-existing and outside my diff** — `internal/compaction/client.go:755` (S1024 `time.Until`) and `internal/tool/diagnostics.go:35` (unused `diagnosticsSink`; zero callers repo-wide, verified). 0 issues in touched files. Left unfixed (out of scope).
- Nothing committed.

## Notes for the coordinator
- Exact approvals are written to the global store only (`true` at the call site); `CountExactCommandApprovals` sums both scopes for the message.
- `os.UserConfigDir()` drives the global path (macOS: `~/Library/Application Support/late/allowed_commands.json`; Linux: `$XDG_CONFIG_HOME/late/...`); the issue's "~/.late" wording is approximate — this is the store the policy already loads.
- Tests redirect the global dir via `$HOME`/`$XDG_CONFIG_HOME`; windows store tests skip (consistent with existing gate tests).
