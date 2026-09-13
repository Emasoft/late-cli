# Late Quickstart Guide

[English](quickstart.md) | [简体中文](quickstart.zh-CN.md)

Get from install to your first autonomous coding task in a couple of minutes.

## Install

### Homebrew — Linux / macOS

```bash
brew tap mlhher/late && brew install late
```

### Universal installer — Linux / macOS / Windows WSL

```bash
curl -sfL https://raw.githubusercontent.com/mlhher/late-cli/main/install.sh | bash
```

Manual binaries for Linux, macOS, and native Windows are available from the [GitHub Releases](https://github.com/mlhher/late-cli/releases).

---

## Local Models: Zero Configuration

Late automatically looks for an OpenAI-compatible `llama-server` on `localhost:8080`.

Start `llama-server` with your GGUF model as usual, then launch Late from your project:

```bash
cd your-project
late
```

That's it. If `llama-server` is already running on `:8080`, Late finds it automatically—no Late configuration required.

---

## Cloud/Remote Models

Late works with OpenAI-compatible APIs including DeepSeek, Claude, GPT, Kimi, GLM, OpenRouter, and others.

Set:

```bash
# DeepSeek
export OPENAI_BASE_URL="https://api.deepseek.com" # your-api-url
export OPENAI_API_KEY="sk-123" # your-api-key
export OPENAI_MODEL="deepseek-flash" # your-model-name
```

Then run:

```bash
cd your-project
late
```

You can later persist model settings in Late's `config.json` instead of exporting environment variables every time.

---

## Give Late a Task

Talk to Late like you would another engineer.

For example:

```text
Add input validation to the CreateUser handler in api/users.go.
Check for empty email and name fields, return 400 with a JSON error,
and add regression tests.
```

Or give it something much larger:

```text
Refactor the database package to use connection pooling.
Keep existing behavior intact, update affected tests, and verify
the full test suite still passes.
```

Late's orchestrator plans the work and delegates implementation and research to isolated subagents rather than carrying every file read, command result, edit, and test log in one growing context.

---

## The TUI

Late keeps the orchestrator and active subagents visible inside the same terminal interface.

The essentials:

| Key / Command       | Action                                               |
| ------------------- | ---------------------------------------------------- |
| `Tab`               | Switch between the orchestrator and active subagents |
| `Ctrl+O`            | Attach a file                                        |
| `Esc` / `Ctrl+G`    | Stop the currently running agent                     |
| `/model`            | Change orchestrator or worker models                 |
| `/rewind`           | Rewind to an earlier point in the conversation       |
| `/themes`           | Change the TUI theme                                 |
| `/compose`          | Draft a longer instruction in your `$EDITOR`         |
| `Ctrl+D` / `Ctrl+C` | Quit                                                 |

Type `/` at any time to open the command picker.

When Late creates subagents, each appears in its own tab while it works and disappears after completing its task.

---

## Tool Approval

Potentially destructive commands and file changes require approval unless you have already granted permission for that scope.

When prompted, you can approve:

* once;
* for the current session;
* for the current project;
* globally.

Read-only operations are generally handled automatically.

Approvals decay over time rather than becoming permanent trust forever.

---

## Run Fully Autonomously with Podman

For unattended work, large refactors, or overnight runs, use `late-podman`.

It runs Late inside an isolated rootless Podman container rather than giving the agent unrestricted access to your host.

From a project with a supported devcontainer:

```bash
late-podman
```

Late automatically looks for container configuration in the project, including `.devcontainer/devcontainer.json`. For an example check Late's own [devocontainer.json](../.devcontainer/devcontainer.json).

You can also specify an image explicitly:

```bash
late-podman --image your-development-image
```

Arguments after `--` are forwarded to Late:

```bash
late-podman -- --continue
```

Your current workspace is mounted read-write at `/workspace`. Late keeps its own session and cache volumes, forwards your SSH agent when available, and mounts your Late configuration read-only if present. Your host home directory and container socket are not exposed by default.

> **Note:** `late-podman` requires Linux with Podman installed. Silverblue and Universal Blue are supported out of the box, including SELinux-aware container handling.

> **Note:** Devcontainer configurations may declare additional mounts. Late respects those when constructing the sandbox.

---

## Hybrid Model Routing

By default, Late uses the same model for the orchestrator and its workers.

You can instead use one model for planning and another for execution:

```bash
export LATE_SUBAGENT_MODEL="worker-model"
export LATE_SUBAGENT_BASE_URL="http://localhost:8080"
export LATE_SUBAGENT_API_KEY="your-other-key"
```

Or use `/model` from inside Late to change models interactively.

This is useful for routing architecture and planning to a stronger model while using smaller or faster models for worker tasks.

---

## Start with a Prompt

For scripts or unattended workflows:

```bash
late --prompt "Run the test suite, diagnose the failures, and fix them."
```

If using `late-podman`:

```bash
late-podman -- --prompt "Refactor this package and verify all tests."
```

---

## Resume Previous Work

Late automatically saves sessions.

Resume the previous session:

```bash
late --continue
```

Or inspect saved sessions:

```bash
late session list
```

---

## Plugins, Skills, and MCP

Late supports:

* Plugins
* Agent Skills
* MCP servers
* Custom slash commands
* Themes
* Lifecycle hooks
* Custom tools

Install a plugin:

```bash
late plugin install <package>
```

Plugins can be installed from npm, Git repositories, local directories, or a configured registry.

For plugin development and the manifest format, see [Plugin SDK](plugin-sdk.md).

---

## Configuration

Persistent configuration lives at:

**Linux**

```text
~/.config/late/config.json
```

**macOS**

```text
~/Library/Application Support/late/config.json
```

**Windows**

```text
%APPDATA%\late\config.json
```

Configuration precedence is:

1. Environment variables
2. `config.json`
3. Late defaults

For the standard local `llama-server` setup on `localhost:8080`, you do not need to create a configuration file.

---