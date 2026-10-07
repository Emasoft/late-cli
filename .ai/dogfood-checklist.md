# Step 20 — Isolated-Environment Dogfood Checklist

**Date:** 2026-09-24 · **Branch:** `local/full` · **Commit:** `8958965` (clean tree)
**Binary:** `/tmp/late-dogfood` built with `go build -o /tmp/late-dogfood ./cmd/late`
(`-version` reports `late dev`; `install-dev.sh` builds via `make build`, which stamps
`2.0.0-rc.1` — the installed binary will report `late 2.0.0-rc.1`)

**Isolation guarantee:** every invocation below ran with `HOME`/`XDG_CONFIG_HOME`
pointed at ONE temp dir and all `*_API_KEY`/`JEV_API` env vars stripped, so no run
could touch the real config, the real sessions, or any LLM backend. Proof: every
output path below names the temp dir, all late state (config.json, mcp_config.json,
skills/, compaction-shadow.jsonl) was created inside the temp HOME, and the real
`~/Library/Application Support/late/config.json` mtime predates the run window.
(The fresh mtimes on the real `~/.local/share/late/{compaction-shadow.jsonl,sessions}`
during the window belong to the user's own live `late --continue-project` process,
PID 17203 — not to these runs.)

**Verdict: GO** — every CLI verification passed; scripted TUI checks passed on all
points drivable headlessly. No reinstall was performed in this step.

---

## Part 1 — CLI verification (verbatim outputs)

### (a) `late -help` — binary healthy

```
exit=0  stdout_bytes=0  stderr_bytes=6963
--- stderr head (usage rendering) ---
Late — the AI agent that always stays sharp.
Isolates execution steps to keep the model's context clean during long workflows.

Usage:
  late [flags]
```

PASS: exits 0, renders the full flag usage.

### (b1) Malformed config fixture (darwin path, temp HOME)

```
$HOME/Library/Application Support/late/config.json:
{"models": [}
```

### (b2) `late -replay-shadow=0.35` with the malformed config

```
exit=0
--- stdout ---
No shadow log at <TEMPHOME>/.local/share/late/compaction-shadow.jsonl — nothing scored yet (compaction modes shadow and enabled write it).
--- stderr ---
(empty)
```

PASS, with a code-order caveat: `-replay-shadow` is handled at `cmd/late/main.go:196`,
BEFORE `appconfig.LoadConfig()` at `main.go:438`, so the config-load warning cannot
appear on this path — the run proves the app does not die on a broken config and that
the shadow log path resolves under the TEMP HOME (`~/.local/share/late/...` via
`os.UserHomeDir`). The config-warning proof is (b3).

### (b3) `late -check-compaction` with the malformed config — config error surfaces, app continues

```
exit=1
--- stdout ---
late compaction preflight
  [FAIL] backend     0.00ms  no compaction backend configured (set the provider key or run with -compaction-mode pointing at a gateway): compaction: no System One backend available (tried typesafe, openrouter, gateway): typesafe: compaction: no API key for backend "typesafe": set TYPESAFE_API_KEY=<key> in the environment, or write the key to <TEMPHOME>/Library/Application Support/late/compaction-typesafe.key; openrouter: compaction: no API key for backend "openrouter": set OPENROUTER_API_KEY=<key> in the environment, or write the key to <TEMPHOME>/Library/Application Support/late/compaction-openrouter.key; gateway: compaction: backend "gateway" needs an endpoint URL: set JEV_GATEWAY_URL=https://your-gateway/decisions
result: FAIL (stage "backend" failed)
cost: n/a (the decisions client does not track token usage)
--- stderr ---
Warning: Failed to load app config: failed to parse <TEMPHOME>/Library/Application Support/late/config.json: invalid character '}' looking for beginning of value
```

PASS: the warning names the EXACT config path and cause on stderr (`-check-compaction`
runs after LoadConfig, so this is the CLI-level proof the error surfaces), the app does
not die at startup, continues into the preflight, and reports which stage failed with
what to configure (keys were stripped, so stage 0 fails deterministically). Exit 1 is
the documented preflight-failure code, not a crash.

### (b4) No overwrite — malformed config byte-identical after both runs

```
--- config.json now ---
{"models": [}
NO-OVERWRITE CHECK: PASS (malformed config untouched)
```

PASS: the degraded-config guard (Step 1) did not replace the user's broken file with
defaults.

### (c) `-replay-shadow` on a hand-crafted shadow log — correct math

Fixture: 5 decision entries (one legacy line without `type`), 1 `history-run` summary,
2 `expand` outcomes (one per-segment `seg-aaa`, one record-id `r:4f9d2c1b`), 1 `hit`
outcome. Recorded thresholds: 0.35 everywhere except `seg-ccc` at 0.50 (protected-kind
floor scenario).

```
$ late -replay-shadow=0.10,0.35,0.50
exit=0
--- stdout ---
Shadow log: <TEMPHOME>/.local/share/late/compaction-shadow.jsonl

threshold  kept  relocated  tokens saved  still missed
0.10       5     0          0             0
0.35       4     1          300           1
0.50       3     2          500           1

false-negative rate: 50.0%
--- stderr ---
(empty)
```

Expected vs actual — all match:
- 0.10 → kept 5, relocated 0, saved 0, missed 0
- 0.35 → kept 4, relocated 1 (`seg-aaa` 0.12), saved 300, missed 1 (`seg-aaa` has the later expand)
- 0.50 → kept 3, relocated 2 (`seg-aaa`, `seg-ccc` 0.42), saved 500, missed 1 —
  `seg-ccc` is NOT counted: it has no expand outcome; the record-id expand
  (`r:4f9d2c1b`) matches no segment id; outcomes and `history-run` lines are skipped.
- FFR 50.0% = 1 of the 2 segments elided at their own recorded threshold was later
  expanded (the Step 13 outcome-ledger linkage working end to end).
- Legacy line (no `type` field) counts as a normal elide decision — backward compat.

### (d) `-check-compaction` with `compaction-backend: "offline"` — all stages pass, exit 0

Config (temp HOME; model URL is a deliberately dead port to prove no network):
```json
{
  "models": [
    { "id": "offline-demo", "url": "http://127.0.0.1:9/v1", "key": "not-a-real-key", "model": "offline-demo" }
  ],
  "compaction-mode": "enabled",
  "compaction-backend": "offline"
}
```

```
$ late -check-compaction
exit=0
--- stdout ---
late compaction preflight
  [ok  ] backend     0.00ms  backend "offline" url none model "scripted-scorer" (api key: none — deterministic offline scorer; no network)
  [ok  ] questions   0.01ms  3/3 questions answered with numeric scores (noul protocol)
  [ok  ] gate          40ms  gate relocated 1 run(s) — 4 of 4 segments, 700 tokens — into 1 store record(s)
  [ok  ] expand      0.01ms  1 pointer(s) expanded back byte for byte (2106 bytes)
result: PASS (4/4 stages ok)
cost: n/a (the decisions client does not track token usage)
--- stderr ---
(empty)
```

PASS: the offline preflight has FOUR stages (backend + the three functional ones from
the plan: questions parse, gate relocates, pointer expands byte-for-byte). All pass
with no key and no network — the dead model URL was never contacted (0.00ms backend,
no dial errors). Exit 0.

---

## Part 2 — Scripted TUI session (tmux pty, temp HOME, offline config)

A TTY WAS drivable (tmux), so a bounded scripted session was run against the same
temp HOME + offline config. Verified live, headlessly:

| Check | Result |
|---|---|
| TUI boots and renders (banner, todo pane, status bar) | PASS |
| Footer always labels the focused agent | PASS — status bar renders `[     ]  orchestrator  ⎇ local/full` (Step 6) |
| Typing `juice` with the todo pane UNFOCUSED | PASS — letters land in the input (`❯ juice`); `j`/`k`/`g` not swallowed (Step 5, unfocused = zero input consumption) |
| `ctrl+t` focuses the todo pane | PASS — title becomes `Todos  [focused · esc to unfocus]`; ANSI capture shows focused bg `48;5;234` and bright border `38;5;179` (Step 4) |
| Pressing `x` while focused | PASS — pane unfocuses (marker gone, bg back to `48;5;232`, border dims to `38;5;235`) AND `x` reaches the input (`❯ juicex`) (Step 5 test case b) |
| `/model` picker points at the REAL config path | PASS — with no models configured it renders `No models configured in <TEMPHOME>/Library/Application Support/late/config.json` — never `~/.config/late` (Step 3); with models configured it lists the temp config's `offline-demo` per agent |

Not scriptable headlessly (left for the manual pass): mouse click focus/unfocus
(click inside the pane focuses, click outside unfocuses), and visual confirmation of
the focused background color on a real terminal.

---

## Part 3 — The 8-point MANUAL checklist (execute on the REINSTALLED binary)

The scripted session above already exercised several of these against the temp build;
the user should re-confirm each on the reinstalled binary in their real environment.
Items marked *(auto-verified)* passed in Part 1/2 and only need a spot check.

1. [ ] **Keeper/juice typing** — open `late`, type `juice` (and `keeper`): every
   letter lands in the input box; nothing is swallowed. *(auto-verified)*
2. [ ] **Todo pane focus highlight/unfocus** — `ctrl+t` (or click inside the pane)
   → pane title shows `[focused · esc to unfocus]` and the pane background changes;
   `esc` (or click outside the pane) → highlight gone; while UNFOCUSED the pane
   consumes no keystrokes. While FOCUSED, `j`/`k` scroll the pane and stay focused;
   any other letter unfocuses AND lands in the input. *(focus/unfocus + `x` auto-verified; mouse clicks are manual)*
3. [ ] **Footer orchestrator label** — the status bar always shows the focused agent
   type: `orchestrator` at root, the breadcrumb type when focused on a subagent
   (label exactly once). *(auto-verified for root)*
4. [ ] **`/model` real path** — `/model` opens the picker; the empty-models message
   names the REAL macOS path
   (`~/Library/Application Support/late/config.json`), never `~/.config/late`.
   *(auto-verified)*
5. [ ] **`/jev-compact-context` scored N/M** — run `/jev-compact-context` in a
   session with real history: the status reports `scoring N/M messages` in all three
   outcomes (completed, partially scored, aborted) and never dies at
   "stopped after 0 messages" when the backend rejects scoring (the fail-open fix);
   the shadow log gains one `history-run` line per attempt.
6. [ ] **`-check-compaction`** — `late -check-compaction` prints the per-stage
   report (backend/questions/gate/expand) and `result: PASS (4/4 stages ok)`,
   exit 0, against your real backend; on a broken backend it FAILS and names the
   broken stage. *(offline variant auto-verified)*
7. [ ] **`-replay-shadow`** — `late -replay-shadow=0.10,0.35,0.50` prints the
   kept/relocated/tokens-saved/still-missed table plus the false-negative rate,
   read-only, exit 0. *(auto-verified on a fixture)*
8. [ ] **Config error visible at startup + no overwrite** — with a malformed
   `config.json`, startup shows the warning (in the TUI status bar, not only
   stderr), the app still starts, and running `/infobar`, `/timestamps`, or
   `/model` does NOT overwrite the broken file (verify the file content is
   unchanged afterwards). *(CLI-level auto-verified via (b3)/(b4); the TUI status
   bar surface + SaveConfig refusal are manual)*

## Part 4 — Install (PERFORMED — see Part 5 for the record)

Reinstall the dev build as a symlink that tracks this repo (menu entry 1 =
local-dev; `--choice N` is headless and implies `--yes`):

```sh
cd /Users/emanuelesabetta/Code/late-cli && ./install-dev.sh --choice 1
```

Optional dry plan first: `./install-dev.sh --dry-run --choice 1`.
Then re-run this checklist (Part 3) against the installed binary and confirm
`late -version` reports `2.0.0-rc.1`. (This Part 4 text was written before the
fresh-eyes sweep; the actual install ran from commit `b190983`, not `8958965`.)

---

## Part 5 — Installed build (Step 22 execution record, 2026-09-24 02:37 CEST)

**Commit:** `b19098321846702f15813b83651595b23dc1fdd9` (`b190983`,
"refactor(compaction): fresh-eyes review fixes", committed
2026-09-24T02:34:58+02:00) — branch `local/full`, tracked tree clean
(git status shows only untracked `.ai/`).

**Install method:** `./install-dev.sh --choice 1` — headless mode, menu entry 1
= `local-dev` (build current branch via `make build`, install `/opt/homebrew/bin/late`
as a SYMLINK to `<repo>/bin/late` so every rebuild updates the command in place;
`--choice N` implies `--yes`). Installer exit code 0. late-podman re-linked the
same way (refuses to run on darwin, as designed). Nothing was committed, pushed,
or PR'd; the installer's remote access was read-only `git ls-remote` for its
detection report.

### Before → after (proof the reinstall replaced the stale binary)

| | BEFORE | AFTER |
|---|---|---|
| `which late` | `/opt/homebrew/bin/late` | `/opt/homebrew/bin/late` |
| install node | symlink → `<repo>/bin/late` (Sep 23 15:50) | symlink → `<repo>/bin/late` (re-pointed Sep 24 02:37) |
| `<repo>/bin/late` mtime | 2026-09-23 15:50:04 +0200 (**predates b190983** — stale) | **2026-09-24 02:37:47 +0200** (fresh, > commit time 02:34:58) |
| size / sha256 | 28,293,922 B / `07075ccd94057ecc…` | **28,486,562 B / `2f98c546a98ce897…`** |
| `late -version` | `late 2.0.0-rc.1` | `late 2.0.0-rc.1` (make stamps `2.0.0-rc.1`; unchanged string — mtime+sha prove the swap) |
| config.json mtime | 2026-09-23 18:42:14.332973599 +0200, 1382 B | **identical** — untouched by install + both diagnostics |

Installer output tail (verbatim):

```
=> Source: local-dev — build local/full @ b190983, install as symlink
=> Building late from /Users/emanuelesabetta/Code/late-cli...
Building late...
=> Installed symlink /opt/homebrew/bin/late -> /Users/emanuelesabetta/Code/late-cli/bin/late
=> Installed symlink /opt/homebrew/bin/late-podman -> /Users/emanuelesabetta/Code/late-cli/late-podman

== verification ==
symlink: /opt/homebrew/bin/late -> /Users/emanuelesabetta/Code/late-cli/bin/late
version: late 2.0.0-rc.1
late-podman: /opt/homebrew/bin/late-podman -> /Users/emanuelesabetta/Code/late-cli/late-podman
late resolves to: /opt/homebrew/bin/late (no shadowing)
```

### Diagnostic 1 — `late -replay-shadow=0.35` (READ-ONLY, real shadow log) — PASS

```
$ late -replay-shadow=0.35
Shadow log: /Users/emanuelesabetta/.local/share/late/compaction-shadow.jsonl

threshold  kept  relocated  tokens saved  still missed
0.35       5069  0          0             0

false-negative rate: 0.0%
--- exit=0 ---
```

PASS: reads the user's REAL shadow log (685,002 B, 5,069 recorded decisions from
their live sessions), prints the replay table + FFR, exit 0, writes nothing.
(0 relocated at 0.35 with FFR 0.0% is consistent with real history: recorded
decisions mostly kept, no expand outcomes yet — the table math itself was proven
on the Part 1(c) fixture.)

### Diagnostic 2 — `late -check-compaction` (READ-ONLY, real backend) — FAIL (honest, by design)

```
$ late -check-compaction
late compaction preflight
  [ok  ] backend     0.00ms  backend "openrouter" url https://openrouter.ai/api/alpha/decisions model "~typesafe/jev-latest" (api key: env)
  [FAIL] questions     83ms  malformed request (backend rejected it): score item "probe-1": compaction: validation (400) during score-batch: malformed request (never retried — this is a bug, not a transient failure): decisions API error (400): {"error":{"message":"[\n  {\n    \"code\": \"invalid_union\",\n    \"errors\": [\n      [\n        {\n          \"expected\": \"string\",\n          \"code\": \"invalid_type\",\n          \"path\": [],\n          \"message\": \"Invalid input: expected string, received undefined\"\n        }\n      ],\n      [\n        {\n          \"expected\": \"record\",\n          \"code\": \"invalid_type\",\n          \"path\": [],\n          \"message\": \"Invalid input: expected record, received undefined\"\n        }\n      ],\n      [\n        {\n          \"expected\": \"array\",\n          \"code\": \"invalid_type\",\n          \"path\": [],\n          \"message\": \"Invalid input: expected array, received undefined\"\n        }\n      ]\n    ],\n    \"path\": [\n      \"questions\",\n      \"probe-1\",\n      \"instructions\"\n    ],\n    \"message\": \"Invalid input\"\n  },\n  { ... same union error for probe-2 ... },\n  { ... same union error for probe-3 ... }\n]","code":400}
result: FAIL (stage "questions" failed)
cost: n/a (the decisions client does not track token usage)
--- exit=1 ---
```

(Elision note: probes 2 and 3 returned the byte-identical 400 payload with only
the probe id in the error context changed — `probe-1` / `probe-2` / `probe-3`
each flagged at path `questions.probe-N.instructions`. The full untruncated
output is reproduced in the session transcript.)

**What this means (finding for the acceptance decision, NOT a new failure of
this step):** the preflight did exactly its Step 16 job — it caught, before any
integration use, that the REAL openrouter decisions endpoint rejects the current
Go score-batch payload: the request is missing the required
`questions.<id>.instructions` field (server expects string | record | array).
This is a client↔API contract mismatch in `internal/compaction/client.go`'s
request shape vs. the deployed openrouter `/api/alpha/decisions` schema.
Consequences on the real backend today:
- scoring requests fail with a typed Validation error (Step 15 taxonomy — never
  retried, no retry storm);
- in shadow/enabled mode the fail-open path (Step 7) treats it as incomplete
  scores → history compaction reports the failure instead of silently wiping,
  and per-item it degrades gracefully; but NO real scoring/elision can succeed
  against this backend until the payload adds `instructions`.
- Backend resolution, key handling (env), and stage naming all work.

Also verified read-only: the real shadow log AND config.json mtimes were
byte/mtime-identical after both diagnostics (check.go writes nothing to disk —
confirmed on the real environment, not just tests).

### Final state

- Installed: `late 2.0.0-rc.1` built from `local/full @ b190983` via
  `/opt/homebrew/bin/late -> /Users/emanuelesabetta/Code/late-cli/bin/late`
  (dev-symlink mode; future `make build` updates the command in place).
- `git status`: tracked tree clean, only untracked `.ai/`; HEAD still `b190983`;
  no new commits; nothing pushed; upstream remote untouched (installer's
  `ls-remote` is read-only).
- **STOP — user acceptance gate.** Remaining: the user runs Part 3 manually on
  this binary. Decision needed on the Part 5 `-check-compaction` finding above
  (payload contract fix = new work item, out of Step 22 scope).
