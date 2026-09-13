# Late 快速入门指南

[English](quickstart.md) | [简体中文](quickstart.zh-CN.md)

从安装 Late 到运行第一个自主编码任务，只需几分钟。

## 安装

### Homebrew — Linux / macOS

```bash
brew tap mlhher/late && brew install late
```

### 通用安装脚本 — Linux / macOS / Windows WSL

```bash
curl -sfL https://raw.githubusercontent.com/mlhher/late-cli/main/install.sh | bash
```

适用于 Linux、macOS 和原生 Windows 的二进制文件可从 [GitHub Releases](https://github.com/mlhher/late-cli/releases) 下载并手动安装。

---

## 本地模型：零配置

Late 会自动查找运行在 `localhost:8080` 上、兼容 OpenAI API 的 `llama-server`。

像往常一样使用你的 GGUF 模型启动 `llama-server`，然后在项目目录中启动 Late：

```bash
cd your-project
late
```

就这么简单。如果 `llama-server` 已经运行在 `:8080`，Late 会自动发现它，无需任何 Late 配置。

---

## 云端 / 远程模型

Late 可与兼容 OpenAI API 的服务配合使用，包括 DeepSeek、Claude、GPT、Kimi、GLM、OpenRouter 等。

设置：

```bash
# DeepSeek
export OPENAI_BASE_URL="https://api.deepseek.com" # your-api-url
export OPENAI_API_KEY="sk-123" # your-api-key
export OPENAI_MODEL="deepseek-flash" # your-model-name
```

然后运行：

```bash
cd your-project
late
```

之后也可以将模型设置持久化到 Late 的 `config.json` 中，无需每次都重新导出环境变量。

---

## 给 Late 一个任务

你可以像与另一位工程师交流一样向 Late 描述任务。

例如：

```text
为 api/users.go 中的 CreateUser handler 添加输入验证。

检查 email 和 name 字段是否为空，如果为空则返回带有 JSON 错误信息的 400 响应，

并添加回归测试。
```

也可以交给它规模更大的任务：

```text
重构 database 包以使用连接池。

保持现有行为不变，更新受影响的测试，并验证

完整测试套件仍然能够通过。
```

Late 的编排器会规划工作，并将实现和研究任务委托给隔离的子智能体，而不是把每次文件读取、命令结果、代码修改和测试日志全部塞进一个不断膨胀的上下文中。

---

## TUI

Late 会在同一个终端界面中显示编排器和当前活动的子智能体。

常用操作：

| 按键 / 命令             | 操作                   |
| ------------------- | -------------------- |
| `Tab`               | 在编排器和活动子智能体之间切换      |
| `Ctrl+O`            | 附加文件                 |
| `Esc` / `Ctrl+G`    | 停止当前正在运行的智能体         |
| `/model`            | 更改编排器或工作智能体所使用的模型    |
| `/rewind`           | 回退到对话中的较早位置          |
| `/themes`           | 更改 TUI 主题            |
| `/compose`          | 在 `$EDITOR` 中编写较长的指令 |
| `Ctrl+D` / `Ctrl+C` | 退出                   |

随时输入 `/` 即可打开命令选择器。

当 Late 创建子智能体时，每个子智能体都会在工作期间显示在独立的标签页中，并在完成任务后消失。

---

## 工具审批

具有潜在破坏性的命令和文件修改需要获得批准，除非你已经为相应作用域授予了权限。

出现提示时，可以选择批准：

* 仅本次；
* 当前会话；
* 当前项目；
* 全局。

只读操作通常会自动处理。

授权会随时间过期，而不会永久保持有效。

---

## 使用 Podman 完全自主运行

对于无人值守任务、大型重构或过夜运行，可以使用 `late-podman`。

它会在隔离的 rootless Podman 容器中运行 Late，而不是让智能体不受限制地访问宿主机。

如果项目包含受支持的 devcontainer 配置，直接运行：

```bash
late-podman
```

Late 会自动查找项目中的容器配置，包括 `.devcontainer/devcontainer.json`。示例可参考 Late 自己的 [devcontainer.json](../.devcontainer/devcontainer.json)。

也可以显式指定镜像：

```bash
late-podman --image your-development-image
```

`--` 之后的参数会直接传递给 Late：

```bash
late-podman -- --continue
```

当前工作区会以读写方式挂载到 `/workspace`。Late 使用独立的会话卷和缓存卷，在可用时转发 SSH agent，并在存在 Late 配置时以只读方式挂载该配置。默认情况下，不会暴露宿主机的 home 目录或容器 socket。

> **注意：** `late-podman` 需要 Linux 并安装 Podman。Silverblue 和 Universal Blue 开箱即用，包括对 SELinux 的适配处理。

> **注意：** Devcontainer 配置可能声明额外的挂载项。Late 在构建沙箱时会遵循这些配置。

---

## 混合模型路由

默认情况下，Late 的编排器和工作智能体使用相同的模型。

你也可以让一个模型负责规划，另一个模型负责执行：

```bash
export LATE_SUBAGENT_MODEL="worker-model"
export LATE_SUBAGENT_BASE_URL="http://localhost:8080"
export LATE_SUBAGENT_API_KEY="your-other-key"
```

也可以在 Late 内使用 `/model` 交互式更改模型。

这样可以将架构设计和规划任务路由给能力更强的模型，同时使用较小或更快的模型处理工作智能体任务。

---

## 启动时直接提供 Prompt

用于脚本或无人值守工作流时：

```bash
late --prompt "Run the test suite, diagnose the failures, and fix them."
```

如果使用 `late-podman`：

```bash
late-podman -- --prompt "Refactor this package and verify all tests."
```

---

## 恢复之前的工作

Late 会自动保存会话。

恢复上一次会话：

```bash
late --continue
```

或者查看已保存的会话：

```bash
late session list
```

---

## 插件、Skills 与 MCP

Late 支持：

* 插件
* Agent Skills
* MCP 服务器
* 自定义 slash 命令
* 主题
* 生命周期 hooks
* 自定义工具

安装插件：

```bash
late plugin install <package>
```

插件可以从 npm、Git 仓库、本地目录或已配置的 registry 安装。

有关插件开发和 manifest 格式，请参阅 [Plugin SDK](plugin-sdk.md)。

---

## 配置

持久化配置文件位于：

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

配置优先级为：

1. 环境变量
2. `config.json`
3. Late 默认值

对于运行在 `localhost:8080` 上的标准本地 `llama-server` 配置，无需创建配置文件。

---
