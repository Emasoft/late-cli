# Worker A2 report — cwd-scoped exact-allowlist + OTP binding

Branch `local/full`. Nothing committed (per instructions). Built **on top of** Worker A's uncommitted changes (permissions.go / revaluate.go / ast_bridge.go / otp.go + tests) — none reverted, none committed.

## Refinement implemented
The allowlist key now includes the **execution cwd** (the dir the command would actually run in): `"rm -rf ./bin"` executed in `/tmp` is a DIFFERENT command than the same string in the user's home dir, because relative paths resolve against cwd. The same applies to the pending-OTP registry: a re-run must match BOTH the same argv AND the same cwd to be valid; a re-run from a different cwd is a different command and needs a fresh OTP.

## Cwd resolution (single source of truth)
- New `tool.ResolveShellExecCwd(ctx, cwdParam)` (internal/tool/implementations.go) mirrors ShellTool.Execute's `cmd.Dir` resolution **exactly**: explicit `cwd` param > worktree dir (`common.WorktreeDirKey`, spawn_subagent "worktree" children) > process CWD (`os.Getwd`). Cleaned; relative param stays relative (os/exec resolves it against the process CWD, and key canonicalization applies the same rule). `ShellTool.Execute` was refactored to call the helper for `cmd.Dir` (behavior-preserving — same validation, same error text) so resolution cannot drift.
- The force-revaluate gate (internal/tui/revaluate.go) resolves `execCwd` via the helper and uses it for **every** (cwd, argv)-keyed decision.

## Key format (documented at the constants, internal/tool/permissions.go)
- `ExactCommandKey(cwd, command)` = `"::exact::"` + **canonical cwd** + `"\x00"` + trimmed command (`exactCommandKeySeparator = "\x00"`).
- Separator: NUL is safe for the JSON store — a directory path can never contain NUL on any supported platform, and `encoding/json` escapes NUL in object keys as `\u0000`. Split via `splitExactCommandKey` (first-NUL cut after the prefix). Pre-cwd-scoping keys (no separator) simply never match and re-gate.
- Cwd canonicalization (`canonicalExecCwd`): absolute + symlink-resolved (nearest existing ancestor) + cleaned, so `os.Getwd`'s result, a caller path via symlink, trailing separators, `/a/b/../c` spellings, relative params and `""` (= process CWD) all collapse to one key. Best-effort fallback keeps keys stable if the cwd vanishes.
- **Design notes (documented in code + pinned by tests):** relative-path commands (`rm -rf ./bin`) vs absolute-path equivalents are SEPARATE entries by design — the raw argv is what was approved; the cwd scoping exists so relative paths resolve the way execution resolves them. Boundary whitespace is still trimmed on store+lookup (allowlist rule) while interior whitespace stays significant.

## Every use updated
- `SaveExactAllowedCommand(cwd, command, global)` — persists under the (cwd, argv) key.
- `CountExactCommandApprovals(cwd, command)` — counters are per (cwd, argv), summed across global+local.
- ast_bridge.go bypass check — `astAnalyzer.exactAllowed` now stores full store keys; both lookup sites (parsed-policy path and parse-error path) do `exactAllowed[ExactCommandKey(a.cwd, command)]`, so a fresh instance reloaded from disk bypasses only in the approved cwd. Hard blocks still always win.
- OTP binding (internal/tool/otp.go) — `IssueOTP(cwd, command)`, `ConsumeOTP(cwd, command, code)`, `RecordOTPApproval(cwd, command)`, `SessionOTPApprovals(cwd, command)` all key on `otpKey(cwd, command)` = canonical cwd + NUL + **raw** argv (byte-for-byte — the registry keeps its exact-argv rule, e.g. leading/trailing space variants stay different commands; the trimming rule belongs to the allowlist only).
- `persistedCommandEntry` gains `Cwd string \`json:"cwd,omitempty"\`` — the entry records the canonical cwd **visibly** (derived from the key on write in `writeAllowedCommandsFile`) so humans reading allowed_commands.json understand the scoping. Optional field; pre-existing files stay compatible.
- `ShellTool.RequiresConfirmationForCwd(command, execCwd)` (new) — confirmation decision against an already-resolved execution cwd; the gate uses it because only it holds the ctx needed for the worktree default. Plain `RequiresConfirmation` is untouched (no-ctx callers keep their behavior; the canonicalized `""` → process-CWD fallback keeps those lookups consistent for cwd-less calls).

## Block message
`OTPRevaluateMessageDetailed(cwd, code, prior, session)` now shows the cwd: "This gate applies to the command executed in working directory `<canonical cwd>`: the same command run from a different working directory is a different command and needs its own approval." The auto-allow note now reads "same exact string in the same working directory only". The legacy `OTPRevaluateMessage(code)` renderer is unchanged (no cwd context ⇒ no cwd sentence). The `otp_code` tool-parameter description says the code is bound to the exact command string AND its working directory.

## Tests
Updated to the cwd-aware signatures: internal/tool/exact_allowlist_test.go, otp_test.go, internal/tui/revaluate_test.go, revaluate_allowlist_test.go (incl. the fixture store key).
New:
- tool: `TestExactAllowlist_RelativeAndAbsolutePathCommandsAreSeparateEntries`, `TestSaveExactAllowedCommand_CwdScopesEntriesSeparately` (separate entries, visible cwd field, per-cwd counters), `TestExactCommandKey_CwdComponentAndSeparator` (format, NUL split, canonical spellings, "" = process CWD), `TestOTPBinding_IsCwdScoped` (fresh OTP per cwd, no counter leakage), cwd coverage in the fresh-instance bypass test (same argv+cwd bypasses incl. trailing-separator spelling; different cwd / no-cwd / different argv still gate) and in the analyzer hard-block test (different cwd stays gated).
- tui: `TestHandleForceRevaluate_SameCommandDifferentCwdNeedsFreshOTP` (same argv in cwdB + cwdA's code → still blocked with a FRESH code; (cwdA, argv) approval persists; cwdB has zero persisted/session approvals; cwdA retry executes), `TestHandleForceRevaluate_WorktreeChildCwdScoping` (WorktreeDirKey default is the bound cwd; fresh instance replays worktree-scoped call without OTP; the same command without the worktree context gates again), `TestHandleForceRevaluate_BlockMessageShowsExecutionCwd` (explicit param cwd and process-default cwd both shown).

## Gates
- `gofmt` on all touched files: clean. (Pre-existing unformatted, untouched files exist elsewhere: internal/mcp/config.go, internal/plugin/{command,installer,manifest,skills_cleanup_test}.go.)
- `go build ./...`: pass.
- FULL `go test -race -count=1 ./...`: all packages ok.
- `golangci-lint` v2.13.2 on ./internal/tool/... ./internal/tui/...: **0 issues in touched files**. One pre-existing finding in code this task did not touch: `internal/tool/diagnostics.go:35` `func diagnosticsSink is unused` — present at HEAD (committed, unmodified by Worker A or A2; nothing references it in internal/tool).

## Not done / notes
- `cmd/late/main.go` `forceRevaluateUsage` still says "bound to that exact command" (not updated to mention cwd — -h copy is test-pinned and outside the requested scope).
- Nothing committed; working tree holds Worker A's + A2's changes.
