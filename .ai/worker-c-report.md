# Worker C report — issue #1 suggestions #3 (archiver inline-return UX) + #4 (archiver side)

Branch `local/full`, on top of uncommitted Worker A/A2/B changes. Commit NOTHING (confirmed: working tree still dirty, no commits made).

## What changed

### 1. Three-band inline/archiver rule (hysteresis-free) — `internal/executor/executor.go`
- `ArchiveThresholdChars = 1024` (unchanged): ≤1024 → inline, not archived.
- NEW `ArchiveInlineFullChars = 3072`: outputs in (1024, 3072] are **archived for durability but returned FULL inline** — small enough to be immediately useful, archive is only backup, no marker added.
- Above 3072 → archived + replaced by the compact reference form.
- NEW `ArchiveTailChars = 500`.
- `maybeArchiveToolResult` guards updated (nil archive / expand exemption / fail-open all preserved). Banding = two byte-length comparisons against fixed constants, no state, no time-dependence.

### 2. Richer reference form — `internal/session/output_archive.go`
`FormatReference(path, output, headChars, tailChars)` now renders:

```
<head 2000 runes>
…[output truncated: N chars total. Head above / tail below. Full output archived: <path> — read_file it to view everything]
<tail 500 runes>
```

- Head AND tail rune-safe (`truncateRunes` for head, rune-slice for tail).
- Marker carries total size (rune count; bands stay byte-based — documented) + path.
- Deterministic per (output, path): no timestamps/randomness — cache-stability invariant preserved and re-pinned by tests.
- When output fits headChars: nothing cut, no truncation language, pointer still present.

### 3. Self-documenting pointer (task item 2)
- Marker names the sanctioned retrieval tool: `read_file <path>` — never cat/grep (gated).
- **Path-safety verification (REQUIRED check)**: read_file resolves paths via `resolveWorktreePath` (internal/tool/permissions.go:215) which returns **absolute paths unchanged** — no worktree containment or path-safety rejection applies. The archive path is absolute, inside the session dir (mode 0600 file / 0700 dir), and readable. **No bug, no fix needed.** Proven by test `TestExecuteToolCallsArchivesLargeOutputs`: `tool.NewReadFileTool().Execute` on the extracted archive path returns err=nil with the FULL original (middle line `stdout line-0250` absent from head+tail of the reference, plus the tail line `stdout line-0499`).

### 4. Tests — `internal/executor/output_archive_test.go` (updated + new)
- `TestFormatReferenceHeadTailComposition` (new): head 2000 + marker + tail 500, no middle content, no cat/grep, deterministic double-call, short-output (nothing-cut) form.
- `TestOutputArchiveRuneSafeHeadAndTail`: multi-byte + invalid-UTF-8 safety on both cuts.
- `TestMaybeArchiveToolResultGuards`: boundary-pinned bands — 1024 inline; 1025 and 3072 full-inline **and archived** (exactly 2 files); 3073 exact reference form; 8000 shrinks with marker metadata + read_file pointer; expand exempt.
- `TestExecuteToolCallsArchivesLargeOutputs`: integration — >3072 → reference form + archive round-trip + **read_file compatibility**; (1024,3072] → full inline + archived; ≤1024 → inline, not archived.
- `TestExecuteToolCallsArchiveThenCompactionPassesThrough` kept green (output bumped above the inline-first band; reference ~2.6k chars still passes the 4000-char compactor untouched).
- `TestOutputArchiveWriteDedupeAndFormat`: expectations updated to the new form.

### 5. Docs
- `README.md` (Tool-Output Archiving bullet) and `docs/quickstart.md`: three-band rule, head+tail+marker shape, read_file pointer, hysteresis-free note. (quickstart.zh-CN.md does not document the archiver — untouched. `.ai/audit4-report.md` is a historical report — untouched.)

## Gates
- `gofmt -l` on touched files: clean.
- `go build ./...`: pass. `go vet` (session, executor): pass.
- `go test -race -count=1 ./...`: **all green** (one intermediate failure was a test-side path-extraction bug — marker's `]` now closes after the tool phrase; fixed by cutting at ` — read_file`).
- `golangci-lint` **v2.13.2** run on `./internal/session/... ./internal/executor/...`: **0 issues**.
- Forbidden files untouched: permissions.go, revaluate.go, ast_bridge.go, ast analyzers, readonly.go all show only Worker A/A2/B pre-existing diffs.

## Notes for the main agent
- `FormatReference` signature is now 4-arg (headChars, tailChars) — sole production caller is executor.go:~166; all call sites updated.
- Marker sizes are rune counts while thresholds are byte lengths (documented in both packages): multi-byte outputs may land one band "earlier", which only errs toward showing more inline.
- The TUI transcript/tool-badge handling of the old `[full output archived: <path>]` line was analyzed in `.ai/audit4-report.md` (no `case "tool"` in the renderer; the reference line never reaches the markdown renderer). The new marker keeps the same `…[…]` shape and lives in the same message role, so that analysis still holds; the added tail line after the marker is plain text in the same tool-role content.
