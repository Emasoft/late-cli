# Phase 1b Report — Per-Model Parallel Gate (builds on uncommitted Phase 1)

Mission: each `models[]` config entry gains `allow_parallel_execution` (FlexBool; ABSENT = false). Only with the flag true may a subagent on that model execute in parallel; otherwise a requested `parallel` is downgraded to `serial` (joins the serial queue, FIFO, drains after everything else). Sync unchanged; orchestrator never blocked. **Committed nothing** (working tree = Phase 1 + this phase).

## Diff summary (this phase, on top of Phase 1's working tree)

```
 cmd/late/main.go                        |  runner: capture flag + model ref, compute EFFECTIVE mode
 cmd/late/subagent_scheduler.go          |  gate helper, subagentScheduling, parallelRunning counter, manifest stamp, ack note
 cmd/late/subagent_scheduler_test.go     |  +3 tests (gate table, mixed batch, serial-FIFO-before-batch)
 internal/config/config.go               |  ModelSetting.AllowParallelExecution + AllowsParallelExecution()
 internal/config/strict.go               |  knownModelEntryKeys += allow_parallel_execution
 internal/config/strict_test.go          |  exact valid-keys message extended
 internal/config/model_parallel_test.go  |  NEW: defaults/synonyms/rejection/round-trip
 internal/session/subagent_manifest.go   |  SubagentRecord.Execution/Model + MarkSubagentExecution
 internal/session/subagent_manifest_test.go | +2 tests (stamp persists, sync children omit fields)
 internal/tool/subagent.go               |  execution schema + field doc state the gate
 internal/tool/subagent_test.go          |  +1 schema-guard test
 docs/config-reference.md                |  models[] row + nested bullet
 docs/quickstart.md                      |  per-model gate paragraph
```

## Semantics implemented

1. **Config** (`internal/config/config.go`): `ModelSetting.AllowParallelExecution FlexBool \`json:"allow_parallel_execution,omitempty"\`` — plain FlexBool (zero value false = intended default; omitempty keeps false out of the file). All FlexBool synonyms accepted (true/on/enabled/enable/active/activated/yes/y/1 vs false/off/disabled/disable/no/not/n/0; case-insensitive; JSON 1/0). Helper `AllowsParallelExecution() bool`. Motivation documented: single-instance local models and rate-limited providers; parallel requires explicit per-model opt-in.
2. **Runner** (`cmd/late/main.go`): the client-resolution block already calls `GetModelForAgent(agentType)`; it now also captures `modelAllowsParallel = setting.AllowsParallelExecution()` and `modelRef = setting.Reference()` (entry id, else model name). **No model entry (no agent_models routing) = default subagent client = conservative serial** (decision documented in code + docs). Effective mode via `effectiveSubagentExecutionMode(requested, allows)`: sync→sync (untouched), serial→serial (even on allowed models), parallel→parallel only when allowed, else serial.
3. **Scheduler** (`cmd/late/subagent_scheduler.go`): `Launch` documents that it receives EFFECTIVE modes only. Added explicit `parallelRunning int` counter (incremented in startLocked, decremented in finished; serial predicate now `parallelRunning == 0 && serialRunning == ""`) so parallel accounting counts ONLY effective-parallel children; downgraded children are ordinary serial entries: serialQueue FIFO, serials drain one-at-a-time, queued parallel batch launches only after the serial chain is empty. Drain chain verified by tests.
4. **Manifest** (`internal/session/subagent_manifest.go`): `SubagentRecord.Execution` (`execution,omitempty`) = EFFECTIVE mode of background children ("" = sync; a downgraded parallel is recorded as "serial" — shows WHY a child serialized); `SubagentRecord.Model` (`model,omitempty`) = agent_models reference ("" = default model, gate closed). New locked `Session.MarkSubagentExecution(id, execution, model)` (load-modify-save under manifestMu, no-op for in-memory/unknown id), stamped in `launchBackgroundSubagent` BEFORE the scheduler transition, best-effort (logged, never fatal).
5. **Ack**: downgrade is stated to the parent model inline: `Requested "parallel" was downgraded to "serial": the agent's model (<ref|"the default subagent model">) does not allow parallel execution — set "allow_parallel_execution": true on its models[] entry.`
6. **Tool schema** (`internal/tool/subagent.go`): `execution` description now states: a requested "parallel" is downgraded to "serial" when the agent's model has allow_parallel_execution != true (no routing ⇒ always downgraded); field doc comment mirrors it.
7. **Docs**: config-reference `models` row extended + `allow_parallel_execution` bullet in `### models entries` (docs_test reflection guard green); quickstart Background-Execution section gains the per-model gate paragraph.

## Tests (all new)

- `internal/config/model_parallel_test.go`: ABSENT=false (zero value + routed entry); synonyms accepted (`true`,`"on"`,`"yes"`,`"enabled"`,`1`,`"Y"`→true; `false`,`"off"`,`"no"`,`"disabled"`,`0`,`"N"`→false); unknown value → strict parse error; SaveConfig round-trip (canonical `"allow_parallel_execution": true`, strict reload honors it, false omitted by omitempty).
- `cmd/late/subagent_scheduler_test.go`: `TestEffectiveSubagentExecutionMode` (7-case table incl. sync/serial unchanged on allowed models); `TestSchedulerModelGateMixedBatch` (A allowed + B gated, both requested parallel → A runs parallel, B queued "serial, position 1", B runs ALONE after A completes, machine empty after); `TestSchedulerModelGateSerialsRunBeforeQueuedParallelBatch` (serial live → downgraded children FIFO 1→2, allowed parallels batched behind, batch launches together after chain empty).
- `internal/session/subagent_manifest_test.go`: stamp persists (disk reload asserts `"execution": "serial"` + `"model"`), unknown-id no-op; sync records omit execution/model.
- `internal/tool/subagent_test.go`: schema must name `allow_parallel_execution`, the downgrade, and the `models[]` entry.

Pre-existing strict_test expectation extended: the "valid entry keys are: …" message now includes `allow_parallel_execution` (sorted first).

## Gates

- `gofmt -l` on all touched files: clean.
- `go build ./...`: OK (go 1.27.1).
- `go test ./... -race -count=1`: all packages ok (cmd/late 6.2s, config 1.4s, session 10.2s, tool 8.2s, tui 10.7s).
- `golangci-lint` v2.13.2 run over `./cmd/late/... ./internal/config/... ./internal/session/... ./internal/tool/...`: 1 finding — `internal/tool/diagnostics.go:35` unused `diagnosticsSink` — pre-existing at HEAD, outside this diff (documented in .ai/audit4-report.md and the phase1 report). **0 issues in touched files.**
- Nothing committed; `git status` shows Phase 1 + Phase 1b on top (untracked scheduler files still pending first commit).

## Notes for the next phase

- The gate decision is captured in three places on purpose: ack text (model-facing), manifest (resume/debug), scheduler (state machine only ever sees effective modes). Resume ignores execution (always sync) and leaves the original record's Execution/Model untouched.
- `parallelRunning`/`serialRunning` are maintained only by startLocked/finished (single choke points); the `running` map remains the State() lookup.
- Queue persistence is still NOT implemented (Phase-1 crash model unchanged): queued/running children at exit are lost → frozen on next resume.
