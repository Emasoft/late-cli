# Tool-output soft-wrap fix — report

Branch `local/full`, commit **acdafa9** "fix(tui): soft-wrap tool output blocks — no more hard-cut lines", pushed to `origin/local/full` (ef9af4d..acdafa9).

## Where the cut happened

1. `internal/tui/transcript.go` `case "tool"` (render path, ~line 628): each source line was emitted as ONE raw row — `toolStyle.Render("│ "+line)` — with no wrapping. Every other role wraps via lipgloss `Width()` styles or glamour; only the tool block rendered raw.
2. `padRow()` (transcript.go, final per-row pass): rows wider than the viewport were hard-cut with `ansi.Truncate(row, width, "")`. For tool rows this was the visible bug: a long gh-comment body lost everything past the terminal edge.

## What changed (internal/tui/transcript.go)

- `case "tool"` now builds body rows through the new `toolBodyRows(content, width)` instead of one raw row per source line.
- **Soft wrap**: `ansi.Wrap` (charmbracelet/x/ansi, same GraphemeWidth measure as `ansi.StringWidth` used by `padRow`) — word-aware, hard-breaks tokens longer than the limit (300-char URLs still wrap), ANSI-escape preserving, emoji/CJK-safe. Source-line leading whitespace survives on the first row; continuation rows get a two-space `toolWrapIndent` so wrapped code stays visually nested. Ultra-narrow terminals drop the indent before giving up (`toolWrapLimit`); `padRow` remains as the degenerate clamp.
- **Cap, never silent**: rendered rows per expanded tool block capped at `maxToolBodyRows` (400). When hit, a tail hint renders: `…[output continues — N more lines]`, or `…[output continues — N more lines; archived copy: <path>]` when the executor's `…[full output archived: <path> — …]` marker is present in the content (`archivedToolOutputPath` parses both marker forms). N counts source lines not fully shown.
- Performance: rows are still cached content-addressed in the block cache (key already covers content+collapse; width changes already invalidate the whole render); one style pass per cache miss, viewport pagination unchanged.

## Sweep of other `ansi.Truncate` sites (all audited)

- `view.go:967` status-bar row — single-line status → truncate **correct, kept**.
- `view.go:1200` `truncateWithEllipsis` (tool-call badge preview, status strings) — intentional single-line → **kept**.
- `view.go:1780/1798` `renderActivityAt` (↳ thinking/tool activity spinner rows) — single-line status → **kept**.
- `transcript.go` `padRow` — block-level clamp kept as safety net; tool rows now pre-wrap so it no longer fires for them.
- Collapsed tool summary + permission notices — single-line summary / wrapped notice style → unchanged.
- "↳ Executing"-style tool badges are single-line status rows → truncate by design, kept.

## Tests (internal/tui/transcript_toolwrap_test.go, 7 tests)

- 300-char single-line tool output at width 80 → wraps into ≥4 rows; squash-containment proves ALL content visible; every row ≤ viewport width (the regression for the reported bug).
- Mixed multi-line content wraps per line; leading indent preserved; nothing lost.
- Cap: 1000 lines → exactly 400 rows + hint `…[output continues — 600 more lines]`; shown/unshown line assertions.
- Cap hint carries the archived-copy path (short path exact; long path across wrapped rows).
- Wide unicode: 60×🚀 + CJK + ZWJ family emoji wrap without splitting graphemes, widths within limit.
- `archivedToolOutputPath` unit test (both executor marker forms + no marker).
- Capped path proven through the full model render.

## Gates (verbatim)

- `gofmt -l internal/tui/` → no output (clean).
- `go build ./...` → BUILD_OK.
- `go test -race -count=1 ./...` → all 19 packages ok (late/internal/tui 36.595s; cmd/late 9.822s; client 13.374s; executor 10.910s; orchestrator 12.670s; …).
- `golangci-lint run ./internal/tui/...` → **0 issues**. Note: installed binary is v2.14.0 (task named v2.13.2; 2.14.0 is what this machine has).

## Notes

- Repo HEAD was ef9af4d (one commit ahead of the 4c5556f named in the task); tree was clean.
- Bash gate was flaky (re-issued OTPs); all gate commands ran and passed after OTP approval.
