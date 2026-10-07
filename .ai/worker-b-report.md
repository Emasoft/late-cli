# Worker B Report — Issue #1, Suggestion #2: Read/Write Split by Subcommand

Branch: `local/full` (on top of uncommitted Worker-A/A2 work; nothing committed, nothing staged)

## What was implemented

Read-only gh/git operations no longer set `NeedsConfirmation`, so they bypass
the confirmation gate AND the force-revaluate OTP flow entirely (both flow from
`analyzeBashCommand → astAnalyzer.Analyze → policy.Decide`).

### 1. `internal/tool/ast/readonly.go` (NEW — the classifier)
Conservative read-only tables consulted by the Unix AST adapter:

- **gh**: `issue view|list|status`, `pr view|list|checks|diff|status`,
  `repo view`, `search <kind>`, `run view|list|watch`, `release view|list`,
  `label list`, `gist view|list`, `config get`, `auth status`.
  - `gh api` is GET-only: mutating `-X/--method` verbs (POST/PUT/PATCH/DELETE,
    case-insensitive, incl. attached `-XGET`/`--method=X`) fail closed; body
    flags `-f/-F/--field/--raw-field/--input/--field-file` (imply POST) fail
    closed. Missing verb value fails closed.
- **git**: `log/show/diff/status/blame/rev-parse` (any args except
  `--output`/`-o*` write flags); `branch` only with a whitelisted read-only
  flag set and NO positional operands (`git branch <name>` would create —
  cannot be distinguished, so it keeps the gate); `remote` only bare, `-v/
  --verbose`, or `show/get-url`.
- **Pipe-safe filters** (standalone or as pipeline stages): cat, grep, head,
  tail, wc, jq, sort (excluding `-o/--output`), ls, tr, cut, uniq, column,
  stat, diff, strings, etc. Write-capable programs (tee, xargs, sed, awk,
  find, sh, bash) are deliberately absent → pipes through them keep the gate.
- Any statically-unresolvable word (variable/substitution/tilde) in args →
  not read-only (fails closed).

### 2. `internal/tool/ast/unix_adapter.go`
`UnixParser.Parse` tracks `readOnlyAll` across every `CallExpr`; when every
invocation classifies read-only it emits the new `ReasonReadOnly` risk flag.
New helper `unixInvocationReadOnly`.

### 3. `internal/tool/ast/ir.go`
New `ReasonReadOnly ReasonCode = "read_only"` (+ doc).

### 4. `internal/tool/ast/policy.go` (the sanctioned classification hook, minimal)
In `Decide`, after all hard blocks / deletion / subshell / expansion checks and
before the allow-list check:

```go
if hasRisk(ir, ReasonReadOnly) && !p.allowListContradicts(ir) {
    return d
}
```

- ReasonReadOnly only overrides the operator + unknown-command gates; cd,
  redirect, rm-deletion, subshell, invoke-expr and expansion all fire earlier,
  so `gh pr view && rm -rf`, `gh pr view $(cmd)`, `gh pr view > f`, `sudo gh …`
  keep their existing behavior.
- `allowListContradicts` (new helper): if a command HAS an allow-list entry
  whose stored flag set does not cover the used flags, the read-only bypass
  yields — preserving the strict flag-matching property encoded in the
  pre-existing `TestGitLogRejectsGitLogWithNewFlags` (an approved
  `git log --oneline` still gates `git log --all`).
- Doc header renumbered (rule 8 inserted).

## Windows
`WindowsParser` produces its IR via the PowerShell bridge (`ps_bridge.ps1`) and
cannot emit `ReasonReadOnly` without bridge-script changes that cannot be
tested on macOS. Windows keeps current behavior (documented in readonly.go).
Follow-up: port classification into the bridge.

## Gates (all run from repo root)
- `gofmt -l` on all touched files: clean
- `go build ./...`: OK
- `go test -race -count=1 ./...`: ALL packages pass
- `golangci-lint` v2.13.2: 0 issues in touched files. 2 PRE-EXISTING issues in
  files nobody touched (present at HEAD): `internal/compaction/client.go:755`
  (S1024) and `internal/tool/diagnostics.go:35` (unused `diagnosticsSink`).

## Tests added
- `internal/tool/ast/readonly_test.go`: table-driven (existing style, bare
  PolicyEngine, no allowlist — proves classification stands alone):
  read-only gh/git forms auto-approved (incl. `--method GET`, `-XGET`, pipes,
  `&&`/`;`/`||` chains of read-onlys, `> /dev/null`); mutating forms gated
  (gh api write verbs + body flags, gh issue/pr/repo/gist/release/label/
  config/auth mutations, git push/commit/branch -D/remote add/apply/
  diff --output, `git log -opatch.diff`); unknown/bare subcommands keep the
  current gate; dangerous combinations gated (chained rm/git push, pipes into
  xargs/tee/sh, `| git apply -`, substitution, expansion, redirects hard-
  blocked, sudo); read_only flag emission contract; read-only yields to
  contradicting allow-list entries.
- `internal/tool/readonly_split_test.go`: end-to-end through `newASTAnalyzer`
  (the analyzer ShellTool uses): read-only → no confirmation (⇒ no OTP);
  mutating/chained/unknown → still gated.

## Files touched (mine only)
- NEW `internal/tool/ast/readonly.go`, `internal/tool/ast/readonly_test.go`,
  `internal/tool/readonly_split_test.go`
- MOD `internal/tool/ast/ir.go` (+10), `internal/tool/ast/policy.go` (+~40,
  sanctioned hook), `internal/tool/ast/unix_adapter.go` (+37)
- Worker-A/A2 files untouched; working tree not committed.
