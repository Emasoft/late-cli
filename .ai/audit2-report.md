# SYSTEM AUDIT 2/4 — Session persistence + history + subagent manifest + compaction store/walk

Scope: `internal/session/*.go`, `internal/compaction/{store,shadow,relocate}.go`, plus call sites in `cmd/late/main.go`, `internal/orchestrator/base.go`.
Baseline: branch `local/full`, uncommitted Audit-1 fixes in `internal/client/client.go`, `internal/executor/executor.go` — untouched, nothing committed.

Gates (all green):
- `gofmt` — touched files clean (5 pre-existing unformatted files in mcp/plugin packages, untouched).
- `go build ./...` — OK.
- `go test -race -count=1 ./...` — OK (full run; the 4 packages with my changes re-ran green after the last edit).
- `golangci-lint v2.13.2` — 0 issues in touched files. One PRE-EXISTING baseline issue remains in `internal/compaction/client.go:755` (S1024 `time.Until`), untouched/out of scope. Two pre-existing relocate.go lint findings fixed (see F8).

---

## FIXED (code + tests)

### F1 — Stale-writer race: snapshot/commit writes could revert disk (HIGH)
`SnapshotHistory` copied under `historyMu` but wrote outside it; `saveAndNotify` marshaled `s.History` unlocked. Timeline: ticker copies state S0 → a commit appends M and saves S1 → the snapshot's slower write lands after → disk reverts to S0: the committed message disappears from disk (a crash in that window loses it for real), and in the reverse ordering a pre-pop/pre-rewind snapshot resurrects a popped/rolled-back turn that `--continue` then reloads. Child sessions run this for real: the 5s snapshot ticker (`cmd/late/subagent_transcript.go:76`) races the child runner's `appendMessage` commits on the same file.
**Fix** (`internal/session/session.go`): `historyGen atomic.Uint64` counts every history mutation (append, truncate, pop, append-to-last, conversation reset — all bumped under `historyMu`); `persistMu` serializes the final write; new `persistHistorySnapshot` re-checks the generation under `persistMu` immediately before the rename and skips stale writes (the newest writer always lands last). `saveAndNotify` and `SnapshotHistory` now copy history + capture gen under the lock; new `PersistHistory()` is the locked, gen-checked entry point for out-of-package writers.
**Tests** (`internal/session/persistence_gen_test.go`): stale-snapshot write skipped; fresh write still persists; pop resists stale snapshot; `PersistHistory` happy path.

### F2 — Data races on `s.History` unlocked reads (MEDIUM, `-race`-visible)
`saveAndNotify` (`len(s.History)`, marshal input), `StartStream` (request build), `StartNewConversation` (preserve-copy), orchestrator `Rewind`/rollback (`base.go:987,1166,1178`) and `main.go:2112` read the slice header without `historyMu` while mutators and the snapshot ticker hold it.
**Fix**: `saveAndNotify` copies under the lock; `StartStream` builds its request from a locked copy; `StartNewConversation` copies under the lock (and bumps gen — an in-flight pre-reset snapshot would otherwise write the OLD conversation's bytes into the NEW history path, since `persistHistorySnapshot` resolves `HistoryPath` at write time); `Rewind` and the compaction runner now use `sess.PersistHistory()` instead of `session.SaveHistory(path, sess.History)`.

### F3 — In-place `Parts[i].Text +=` races a snapshot's marshal (MEDIUM)
`AppendToLastMessage` mutated `History[last].Content.Parts[i]` in place under `historyMu` — but slice fields alias their backing array through the struct copy a snapshot takes, so the snapshot's out-of-lock `json.Marshal` raced the element write (lens #2's "are messages ever mutated in place after append?" — this is the one, besides the compaction walk, which is safe: it holds `historyMu` for the whole walk incl. `msg.Content` swaps, verified at `compact.go:371-372, 703`). Currently production-unused, but exported and test-reachable.
**Fix**: parts slice cloned before mutation, header rebound; gen bumped. **Test**: `TestAppendToLastMessageDoesNotRaceSnapshotMarshal` (clean under `-race`).

### F4 — Compaction persisted the high-water mark BEFORE the history file (HIGH)
`CompactContext` called `UpdateCompactionHighWater(startLen)` at walk end; the caller saved the compacted history afterwards (`main.go:2112`). Crash between the two — or a failed caller save (disk full: not even a crash, a persistent condition) — left meta advanced over a still-uncompacted history file. Self-heal verified incomplete: no corruption/`ErrFrozenPrefix` (same-length history, walk starts at the mark, pointer-bearing messages skipped), but those messages are frozen uncompacted **forever** — silent permanent loss of the compaction benefit, contradicting the code's own "next run re-walks the uncovered tail" comment.
**Fix** (`internal/session/compact.go`): the walk now persists the history itself (`SaveHistory`, safe under the walk-held `historyMu`) and only then advances the mark; a failed save skips the advance and surfaces via `walkErrs`. History-file-first/meta-second is the commit protocol; next run re-walks the tail (pointer-bearing skipped, `PutRecord` idempotent). Stale doc comment corrected. Caller save retained (idempotent; covers mid-walk abort persistence).
**Tests** (`internal/session/compact_persist_order_test.go`): save failure → mark stays 0 while memory is compacted; happy path → pointers on disk + mark advanced. Ordering audit of the walk: store `PutRecord` happens **before** the pointer enters `msg.Content` (`compact.go` flushRun), and the walk holds `historyMu` throughout, so the ticker can never persist a half-walked history referencing un-Put records — verified sound.

### F5 — Corrupt history file silently destroyed on resume (HIGH)
`main.go:336-339`: `LoadHistory` error → `history = []` with **no** warning; the first save then overwrites the file. Also fired for EACCES/EIO (a file we couldn't even read got clobbered).
**Fix**: new `session.LoadHistoryRecovering(path)` — on failure of a non-empty file, preserves the raw bytes at `<path>.corrupt-<timestamp>` (0600) and records the failure in the critical-error log (`common.LogErrorf`), then returns the error. `main.go` warns on stderr and starts empty; the bytes survive. `LoadHistory` also treats a zero-length file as an empty history (not a hard "unexpected end of JSON input") — `writeAtomic` never produces one, but external damage shouldn't brick `--continue`.
**Tests**: corrupt file → error + one byte-exact `.corrupt-*` backup + log line; clean load → no backups; empty file → empty history, nil error. Legacy-format tolerance verified separately: `MessageContent.UnmarshalJSON` accepts string-content histories (`client/types.go:48`).

### F6 — Unsupported-image rollback left the poison message on disk (HIGH)
`base.go:986-992` truncated the last user message **in memory only** — the message was already persisted by `AddUserMessage`, so `--continue` reloaded the unsupported-image turn into the same terminal error: a permanently bricked session. (Lens #8's truncation/continuity case.)
**Fix**: the rollback now calls `PopLastUserMessage` (tail-user check + truncate + high-water clamp + persistence, incl. file removal when emptied), atomic under `historyMu`; failures logged, never fatal.
**Tests**: the session-level `PopLastUserMessage` suite covers the primitive; no orchestrator-stream harness exists to drive the branch end-to-end (noted as follow-up test debt).

### F7 — Store append failures were silent (MEDIUM)
`store.persistLocked` ignored `OpenFile`/`Write` errors: the record stayed memory-only while the pointers referencing it reached history — invisible until restart turned them into "unknown elided id". Now logged to the critical-error log (Put/PutRecord cannot return errors — session's `ElideStore` interface has none — so the log is the contract, mirroring ShadowLog.Append's checked write).
**Test** (`internal/compaction/store_persist_error_test.go`): store path is a directory → log records path + record id.

### F8 — Pre-existing lint findings in relocate.go (in-scope, trivial)
`elidedTokens = 0` ineffectual (dead after the tripwire clears the flags) and `kept = append(...)` never read (vestigial accumulator) — removed, behavior-preserving; relocate tests green.

---

## VERIFIED SOUND (report-only)

1. **`writeAtomic`** (`session/persistence.go`): tmp file created in the destination directory (same-FS rename ✓), removed on every failure path (`defer`, harmless post-rename ENOENT), chmod before rename ✓. `SaveSessionMeta` duplicates the same shape inline (meta-*.tmp in sessions dir) — refactor candidate only. Windows rename-over-open-file: `os.Rename`→MoveFileEx can fail if a reader holds the file without FILE_SHARE_DELETE; late's own readers (`os.ReadFile`) hold it briefly — acceptable.
2. **Three-way interleaving** (lens #2/#5): the walk holds `historyMu` for the entire walk including scorer round trips (deliberate, documented) — snapshots/commits block, so no torn mid-walk persistence; `UpdateCompactionHighWater` inside the lock takes only `compactionMu` — no deadlock. Store-before-pointer ordering is correct at every point. Lock ordering: `historyMu` → (`compactionMu`, `persistMu` after release); `persistMu` never nests — no cycles.
3. **`LoadHistory` mid-crash truncation**: impossible from late's own writes (tmp+rename whole-document); corrupt-file handling now F5. NDJSON-vs-array: history is a JSON array by contract; the JSONL stores are separate files — no confusion.
4. **Manifest** (lens #4): in-process upsert race guarded by package-level `manifestMu` + reload-per-write (no stale in-memory copy); tmp+rename same-dir via `writeAtomic` ✓. **Remaining gap**: two late processes on the same session folder race read-modify-write (per-process mutex) → last-writer-wins can drop the other process's subagent record. Needs cross-process file locking (flock + Windows equivalent) — follow-up, not fixed (platform work).
5. **`compaction-store.jsonl` / shadow log multi-writer** (lens #6/#7): one `Write` of the whole line on O_APPEND; Go's `poll.FD.Write` loops short writes, and Linux holds the inode lock for whole buffered writes, so whole-line interleave holds on local FS in practice — POSIX does not guarantee it above `PIPE_BUF`, and between retry syscalls of one giant line another process could interleave (the loader then skips that one line, last-writer-wins per id). NFS unguaranteed. Same follow-up as #4: file locking if cross-instance correctness ever matters. Shadow `Append` checks its write error ✓.
6. **Torn tails**: `repairTornTail` (store) terminates a trailing partial line so the next append can't weld onto it; loader skips malformed lines. Narrow race: the repair's `WriteAt('\n')` could land inside a concurrent process's append (startup-time, two processes, torn file) — report-only.
7. **Edge cases**: empty histories (save skipped; pop removes the file; snapshot no-ops), zero-length content (marshals as `""`), unicode (`truncateUTF8` rune-safe; json.Marshal replaces invalid UTF-8), huge single messages (store/shadow use growable readers, no line cap; history marshal unbounded by design).
8. **`ErrFrozenPrefix` self-heal**: mark > len(history) fails loudly and changes nothing (`compact.go:374-381`) — the stale-sidecar-over-shrunken-history case is safe; with F4's ordering the remaining crash gaps all leave the mark behind the durable history.

## Follow-ups (report-only)
- Cross-process manifest/history/store locking (flock or lock-file; Windows story).
- Orchestrator-level regression test driving the unsupported-image rollback branch (needs a stream-error harness).
- `internal/compaction/client.go:755` S1024 (pre-existing, out of scope).
- `GenerateSessionMeta`/`Impersonate` still read `s.History` unlocked; safe today only because their callers are fenced (walk holds `historyMu`, same-goroutine saves) — worth a locked-copy refactor when the TUI's cross-goroutine surface grows.

## Files touched
- `internal/session/session.go` — generation+persistMu machinery, locked saves, `PersistHistory`, `AppendToLastMessage` clone, `StartStream`/`StartNewConversation` locked copies.
- `internal/session/compact.go` — save-before-advance ordering + doc fix.
- `internal/session/persistence.go` — empty-file tolerance, `LoadHistoryRecovering`.
- `internal/compaction/store.go` — append-failure logging.
- `internal/compaction/relocate.go` — dead-code lint fixes.
- `cmd/late/main.go` — recovering load + warning, `sess.PersistHistory()`.
- `internal/orchestrator/base.go` — rollback via `PopLastUserMessage`, `PersistHistory` in Rewind.
- New tests: `internal/session/persistence_gen_test.go`, `internal/session/compact_persist_order_test.go`, `internal/compaction/store_persist_error_test.go`.
