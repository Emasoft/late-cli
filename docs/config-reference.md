# config.json Reference

The complete reference for Late's `config.json`: every accepted key, its type,
its default, the CLI flag it mirrors, and what it does. This file is parsed
**strictly** — an unknown key, a wrong-typed value, an invalid enum value, or a
syntax error aborts startup with a located error (see
[Strict parsing](#strict-parsing)) — so every key below is validated against
the `Config` struct in `internal/config/config.go` by a test
(`internal/config/docs_test.go`).

For a guided setup read the [Quickstart](quickstart.md) first; this page is the
exhaustive reference.

## File location

| Platform | Path |
| --- | --- |
| macOS | `~/Library/Application Support/late/config.json` |
| Linux | `~/.config/late/config.json` (honors `XDG_CONFIG_HOME`) |
| Windows | `%APPDATA%\late\config.json` |

A missing file is not an error: on first run Late writes a default config that
enables every built-in tool. The file and its directory are permission-hardened
to `0600` / `0700` on every load. Two naming conventions coexist: the
CLI-equivalent entries use kebab-case (the JSON key is the flag name), while
the older provider/tool entries use snake_case — both spellings are exactly as
listed below.

## Precedence

For every CLI-equivalent entry the resolution is:

> **explicitly passed flag > config.json > built-in default**

Config loads *after* `flag.Parse`, and a flag that was not passed never wins —
Late records which flags were explicitly passed (`flag.Visit`) and defers to
the config entry otherwise. Passing a flag on the command line always overrides
the config value for that run, even when the flag's own value equals its
default.

Layered exceptions to that order:

* `max-stream-retries` — **flag > `LATE_MAX_STREAM_RETRIES` env > config >
  built-in default (10)**.
* Provider settings — the environment overrides config when set:
  `OPENAI_BASE_URL`, `OPENAI_API_KEY`, `OPENAI_MODEL`,
  `LATE_SUBAGENT_BASE_URL`, `LATE_SUBAGENT_API_KEY`, `LATE_SUBAGENT_MODEL`.
  Within config, `late_subagent_*` wins over the legacy `subagent_*` entries,
  which win over the main `openai_*` values.
* `theme` — `--theme` flag > `LATE_THEME` env > config > bundled base theme.
* `compaction-backend` — the one entry where config beats the environment: a
  set value wins over `JEV_API` / auto-detection; the environment is consulted
  only when the entry is absent.
* `save_subagent_histories` — flag > per-session saved preference > config.
* The three `permission-mode` flags are mutually exclusive: pass at most one.

## Boolean values (on/off synonyms)

Every `boolean-with-synonyms` entry is a `FlexBool`: in addition to the JSON
literals it accepts everyday synonyms, case-insensitively and with surrounding
whitespace tolerated (`"  On  "` is `true`). The JSON numbers `1` and `0` are
accepted too.

| Meaning | Accepted values |
| --- | --- |
| true side | `true`, `on`, `enabled`, `enable`, `active`, `activated`, `yes`, `y`, `1` |
| false side | `false`, `off`, `disabled`, `disable`, `no`, `not`, `n`, `0` |

Anything else is a fatal strict-parsing error. Saved configs always round-trip
to the plain `true`/`false` literals.

Tri-state boolean entries (`inject-cwd`, `use-tools`, `enable-bash`,
`enable-subagents`, `show-cwd`, `show-todo-pane`) distinguish an *absent* entry
(use the default) from an explicit `false`; for plain boolean entries absent
and `false` behave the same.

## Strict parsing

config.json is user-authored by hand, so every problem is fatal and located.
Late never falls back to defaults and starts anyway: the process prints one
line and exits instead of launching the TUI.

* **Syntax errors** — including trailing garbage and an empty file.
* **Unknown top-level entries** — with a did-you-mean suggestion when a known
  key is within edit distance 3, otherwise the full list of valid entries.
  The same rule extends into `models[]`: every entry's keys are validated
  against the known set (`id`, `url`, `key`, `model`,
  `jev-autocompact-percent`), reporting an unknown key at its exact position
  inside the entry and naming the entry. Map-typed sections (`enabled_tools`,
  `agent_models`) are data — any keys are accepted there.
* **Wrong-typed values** — e.g. a string where a number is required.
* **Invalid enum values** (`compaction-mode`, `permission-mode`).
* **Invalid boolean synonyms** for `boolean-with-synonyms` entries.

Every error renders the exact file, 1-based line, and 1-based column (columns
count runes, so UTF-8 content reports the position your editor shows):

```
error in <full path> at line L, column C: <detail>
```

Real examples of each shape:

```
error in /Users/u/Library/Application Support/late/config.json at line 2, column 3: "compaction_mode" is not a valid config.json entry. Did you mean "compaction-mode"?
```

```
error in /Users/u/Library/Application Support/late/config.json at line 7, column 5: "compaction-threshold-percent" must be a number, found a string
```

```
error in /Users/u/Library/Application Support/late/config.json at line 9, column 22: "shado" is not a valid compaction-mode value. Did you mean "shadow"?
```

```
error in /Users/u/Library/Application Support/late/config.json at line 4, column 20: "actve" is not a valid boolean value for "use-tools". Accepted values are: true, on, enabled, enable, active, activated, yes, y, 1 (true) or false, off, disabled, disable, no, not, n, 0 (false); case-insensitive, surrounding whitespace allowed
```

```
error in /Users/u/Library/Application Support/late/config.json at line 12, column 7: models[local] entry "jev-autocompact_percent" is not a valid entry key. Did you mean "jev-autocompact-percent"?
```

Note the difference from *value-range* problems: a value that parses but is out
of range (e.g. `compaction-threshold-percent: 400`) only warns at startup and
falls back to that setting's default — the strict errors above abort the whole
startup, a range warning does not.

## Examples

### Minimal starter

```json
{
  "models": [
    {
      "id": "local",
      "url": "http://localhost:8080",
      "key": "",
      "model": "qwen3.6-35b-a3b"
    }
  ]
}
```

### Fuller example

The compaction block (score cutoff, context percentages, gate knobs, offline
backend, auto-compaction, retrieval) plus the CLI-equivalent block. Note that
`"compaction-backend": "offline"` selects the deterministic scripted scorer for
demos and tests — no API key, no network — and must never become a production
default. The `frontier` model entry also carries a per-model
`jev-autocompact-percent` override: the agents routed to it auto-compact at
70% of their window while every other agent uses the global 99%.

```json
{
  "enabled_tools": {
    "read_file": true,
    "write_file": true,
    "target_edit": true,
    "bash": true
  },
  "permission-mode": "ask-for-user-approval",
  "models": [
    {
      "id": "frontier",
      "url": "https://api.deepseek.com",
      "key": "sk-your-key",
      "model": "deepseek-flash",
      "jev-autocompact-percent": 70
    },
    {
      "id": "local",
      "url": "http://localhost:8080",
      "key": "",
      "model": "qwen3.6-35b-a3b"
    }
  ],
  "agent_models": {
    "orchestrator": "frontier",
    "coder": "local"
  },

  "compaction-mode": "enabled",
  "compaction-threshold": 0.65,
  "compaction-threshold-percent": 80,
  "compaction-max-elide-percent": 70,
  "compaction-protected-floor": 5,
  "compaction-backend": "offline",
  "jev-autocompact": true,
  "jev-autocompact-percent": 99,
  "compaction-retrieval": true,

  "bash-timeout": "10m",
  "subagent-max-turns": 500,
  "subagent_timeout": "2h",
  "max-stream-retries": 10,
  "max-concurrent-llm-requests": 6,
  "enable-images": true,
  "save_subagent_histories": true,
  "inject-cwd": true,
  "show-cwd": true
}
```

## Key reference

Types: `string`, `number`, `boolean-with-synonyms` (see the synonym table
above), `duration-string` (parsed with `time.ParseDuration`, e.g. `"10m"`,
`"1h30m"`, `"0"`), `array`, `object`. "CLI flag" is the one-to-one flag
equivalent; Go's flag package accepts one or two dashes (`-flag` / `--flag`).

### Models and providers

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `models` | array | `[]` | — | Model registry for `/model` and `agent_models`; each entry is `{id, url, key, model, jev-autocompact-percent}` (see below). |
| `agent_models` | object | `{}` | — | Maps agent roles (`orchestrator`, `researcher`, `coder`, …) to a `models` entry `id` (or, legacy, its model name); persisted by `/model`. |
| `openai_base_url` | string | `http://localhost:8080` | — | Base URL of the main OpenAI-compatible API; `OPENAI_BASE_URL` env overrides when set. |
| `openai_api_key` | string | `""` | — | API key for the main provider; `OPENAI_API_KEY` env overrides when set. |
| `openai_model` | string | `""` | — | Main model id used when no `models`/`agent_models` routing applies; `OPENAI_MODEL` env overrides when set. |
| `late_subagent_base_url` | string | `""` (inherits main) | — | Dedicated subagent base URL; wins over the legacy `subagent_base_url`; `LATE_SUBAGENT_BASE_URL` env overrides when set. |
| `late_subagent_api_key` | string | `""` (inherits main) | — | Dedicated subagent API key; wins over the legacy `subagent_api_key`; `LATE_SUBAGENT_API_KEY` env overrides when set. |
| `late_subagent_model` | string | `""` (inherits main) | — | Dedicated subagent model; wins over the legacy `subagent_model`; `LATE_SUBAGENT_MODEL` env overrides when set. |

### Subagents

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `enable-subagents` | boolean-with-synonyms | `true` | `--enable-subagents` | Allow the agent to spawn subagents at all (tri-state). |
| `subagent-max-turns` | number | `500` | `--subagent-max-turns` | Maximum turns per subagent; `0` = unlimited; a negative value warns and falls back to the default. |
| `subagent_timeout` | duration-string | `"24h"` | `--subagent-timeout` | Wall-clock budget for one subagent run; `"0"` or negative = unlimited; note the underscore spelling; unparseable values warn and fall back. |
| `subagent-idle-timeout` | duration-string | `"15m"` | `--subagent-idle-timeout` | Notify when a subagent has been truly idle (no stream progress, no in-flight tool, no nested spawn) this long; `"0"` = off. |
| `subagent-idle-kill-after` | duration-string | `"0"` | `--subagent-idle-kill-after` | Kill a subagent that stays truly idle past this duration; `"0"` = notify only. |
| `save_subagent_histories` | boolean-with-synonyms | `false` | `--save-subagent-histories` | Persist subagent conversation histories under `<sessions>/<session-id>/subagents/` (a per-session saved preference sits between the flag and this entry). |

### System prompt

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `system-prompt` | string | `""` | `--system-prompt` | Replaces the built-in system prompt; priority: system-prompt-file > system-prompt > `LATE_SYSTEM_PROMPT` env > built-in prompt. |
| `system-prompt-file` | string | `""` | `--system-prompt-file` | Replaces the built-in system prompt with this file's contents; an unreadable path is a hard error, the same as for the flag. |
| `append-system-prompt` | string | `""` | `--append-system-prompt` | Appended to the final system prompt after any replacement above. |
| `inject-cwd` | boolean-with-synonyms | `true` | `--inject-cwd` | Replace `${{CWD}}` in the system prompt with the working directory (tri-state). |
| `gemma-thinking` | boolean-with-synonyms | `false` | `--gemma-thinking` | Prepend the Gemma `<\|think\|>` token to the system prompt. |

### Tools and output

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `enabled_tools` | object | all built-in tools `true` | — | Per-tool switches (see below); missing entries are filled from the defaults. |
| `use-tools` | boolean-with-synonyms | `true` | `--use-tools` | Offer tools to the main agent at all (tri-state). |
| `enable-bash` | boolean-with-synonyms | `true` | `--enable-bash` | Master switch for the bash tool, ANDed with `enabled_tools.bash` — either being `false` disables it (tri-state). |
| `bash-timeout` | duration-string | `"10m"` | `--bash-timeout` | Max wall-clock time for one bash tool call; `"0"` or negative = unlimited. |
| `enable-sqz` | boolean-with-synonyms | `false` | `--enable-sqz` | Compress bash tool output with the external `sqz` binary when it is available. |
| `enable-images` | boolean-with-synonyms | `false` | `--enable-images` | Force-enable image attachments even when the backend does not advertise vision support. |
| `logit-bias` | string | `""` | `--logit-bias` | Main-agent token bias: a JSON object or comma-separated `TOKEN_ID:BIAS` pairs. |
| `subagent-logit-bias` | string | `""` | `--subagent-logit-bias` | Subagent token bias, same format as `logit-bias`. |
| `suppress-thinking-words` | boolean-with-synonyms | `false` | `--suppress-thinking-words` | Bias anti-overthinking tokens (requires the same model for the main agent and subagents). |

### Supervision and retries

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `permission-mode` | string | `"ask-for-user-approval"` | `--ask-for-user-approval` / `--i-promise-i-have-backups-and-will-not-file-issues` / `--force-revaluate-dangerous-commands` | How dangerous commands are supervised: one of the three mode values, each also a CLI flag; the flags override config and are mutually exclusive; an invalid value warns and falls back to the safe default. |
| `max-stream-retries` | number | `10` | `--max-stream-retries` | Retry budget for LLM stream errors with backoff; `0` disables retrying; precedence flag > `LATE_MAX_STREAM_RETRIES` env > this entry > default; a negative value warns and falls back. |
| `max-concurrent-llm-requests` | number | `6` | `--max-concurrent-llm-requests` | Process-wide cap on concurrent in-flight LLM requests across all agents and subagents; `0` = unlimited; a negative value warns and falls back to the default. |

### TUI

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `show-cwd` | boolean-with-synonyms | `true` | `--show-cwd` | Show the git branch / working directory in the status bar (tri-state). |
| `show-todo-pane` | boolean-with-synonyms | `true` | — | Whether the todos side pane starts open; terminals narrower than 85 columns always start closed; `/todos` toggles. |
| `show-info-bar` | boolean-with-synonyms | `false` | — | Single-line info footer below the status bar (version, model, context usage, uptime, …); toggled and persisted by `/infobar`. |
| `show-timestamps` | boolean-with-synonyms | `false` | — | `[HH:MM:SS]` prefix on transcript message blocks; toggled and persisted by `/timestamps`. |
| `theme` | string | `""` | `--theme` | Plugin theme id (`plugin:name` or bare name); precedence `--theme` > `LATE_THEME` env > this entry > bundled base; persisted by `/themes`. |
| `skills_dir` | string | `""` | — | Legacy entry, currently unused: skill discovery reads the platform skills directory (`~/.config/late/skills/` etc.) and project-local `.late/skills/`, not this value. |

### Context compaction

`compaction-threshold`, `compaction-threshold-percent`, and
`jev-autocompact-percent` are three different knobs: a per-segment SCORE
cutoff, the context-usage level the info bar reports headroom for, and the
context-usage level that fires the auto-trigger.

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `compaction-mode` | string | `"shadow"` | `--compaction-mode` | Staged rollout stage: `off` (no scoring), `shadow` (score + shadow log only, no behavior change), or `enabled` (also relocate low-scoring segments and register the `expand` tool); invalid values warn and fall back to `shadow`. |
| `compaction-threshold` | number | `0.35` | `--compaction-threshold` | Elision score cutoff: segments scoring strictly below it are elided when `compaction-mode` is `enabled`; valid range (0,1]; out-of-range values warn and fall back. |
| `compaction-threshold-percent` | number | `80` | — | Context-usage percentage the TUI info bar reports remaining headroom against; 1-100 valid, `0` = default, anything else warns and falls back. |
| `compaction-max-elide-percent` | number | `70` | — | Elide-fraction tripwire: when the scorer wants to elide more than this share of an output's tokens it is distrusted and NOTHING is elided; 1-100 valid; cannot be fully disabled. |
| `compaction-protected-floor` | number | `5` | — | Score floor (as a percentage) under which protected segment kinds (stacktrace, diff) may be elided; at any higher score they are kept; 1-100 valid. |
| `compaction-backend` | string | `""` | — | Where scores come from; only `"offline"` today (deterministic scripted scorer, demos/tests only); a set value wins over `JEV_API`/auto-detection; an invalid value warns and falls back to env resolution. |
| `compaction-retrieval` | boolean-with-synonyms | `false` | — | Read side of the record store: before every request the top-k relevant digest summaries are appended to the request's work area; inert (warns) unless `compaction-mode` is `enabled`. |
| `jev-autocompact` | boolean-with-synonyms | `false` | — | Run the full-history compaction (`/jev-compact-context` flow) automatically when context usage crosses `jev-autocompact-percent`. |
| `jev-autocompact-percent` | number | `99` | — | Context-usage percentage that fires the auto-trigger; 1-100 valid, `0` = default, anything else warns and falls back. Each `models[]` entry can override it for the agents routed to that model (see the `models` entries schema below). |

### Legacy entries

| Key | Type | Default | CLI flag | Description |
| --- | --- | --- | --- | --- |
| `subagent_base_url` | string | `""` | — | Legacy subagent base URL; superseded by `late_subagent_base_url`, which wins when both are set. |
| `subagent_api_key` | string | `""` | — | Legacy subagent API key; superseded by `late_subagent_api_key`. |
| `subagent_model` | string | `""` | — | Legacy subagent model; superseded by `late_subagent_model`. |

## Nested schemas

### `models` entries

Each element of the `models` array is an object:

* `id` (string, optional) — stable identifier referenced by `agent_models` and
  the `/model` picker; omit it to fall back to the model name.
* `url` (string, required) — OpenAI-compatible base URL.
* `key` (string, required, may be `""`) — API key; local servers need none.
* `model` (string, required) — the model name the provider serves.
* `jev-autocompact-percent` (number 1-100, optional, default = the global
  `jev-autocompact-percent`) — per-model override of the auto-compaction
  trigger: different models have different context sizes, so the percentage
  at which compaction should fire is a property of the model, not just of the
  installation. Resolution for any agent: its `agent_models`-routed model
  entry's value (when valid) > the global `jev-autocompact-percent` > `99`.
  Out-of-range values warn at startup and fall back to the global (the strict
  parser covers key names, not value ranges).

### `agent_models` values

Keys are agent roles (`orchestrator`, `researcher`, `coder`); values reference
a `models` entry by its `id` (preferred — providers exposing the same model
name stay distinguishable) or, for configs created before ids existed, by the
model name.

### `enabled_tools` entries

Keys are tool names, values booleans. Defaults (all `true`): `read_file`,
`write_file`, `target_edit`, `spawn_subagent`, `bash`, `search_content`,
`find_files`, `create_todos`, `list_todos`, `finish_todo`. Missing entries are
merged from the defaults on load. `bash` is ANDed with `enable-bash`. When
`compaction-mode` is `enabled`, the `expand` tool is additionally registered so
elided originals stay retrievable.

## Internal fields

The `Config` struct also carries `Degraded` (tag `json:"-"`): an internal
defense-in-depth flag so `SaveConfig` never persists a config that was loaded
from an invalid file. It is never serialized and is **not** a config.json key —
setting it would be a fatal unknown-key error.
