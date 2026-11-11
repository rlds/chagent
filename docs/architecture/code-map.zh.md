# 代码地图（Code Map）

> 适用范围：Chagent chess 基础 agent 仓库；面向开发者快速定位模块与关键文件。
> 关键代码路径：`cmd/chagent/`、`pkg/` 下各子包。
> 核对日期：2026-10-29

本仓库是 [chess](https://github.com/sipeed/chess) 项目的命令行基础 agent：只保留
`gateway` 进程、`pico` 通道、模型 provider、工作区上下文与自主任务执行所需的最小子集。
多通道、Web 管理界面、硬件/设备/迁移/自更新等无关功能已裁剪。

## 目录总览

```text
cmd/chagent/            入口（cobra CLI）
  internal/gateway/      gateway 子命令
  internal/agent/        agent 子命令
  internal/cron/ mcp/ model/ skills/ status/ version/
pkg/
  agent/                agent loop、上下文、自主、工具注册
  agent/workspacecontext/  工作区指令发现/渲染/组合
  channels/             通道抽象 + pico 协议通道
  config/               配置加载与默认值
  gateway/              HTTP gateway 服务（health/ready/reload）
  pid/                  pid 文件与单例
  health/               健康检查
  providers/            模型 provider（zhipu/openai/anthropic/…）
  taskstate/            会话任务状态（状态机/轮次）
  tools/                工具注册表与全部工具
  tools/integration/    消息/任务/技能/Web 等集成工具
  session/ memory/ state/ cron/ skills/  … 运行时数据子系统
scripts/                 构建、测试、测量、文档检查脚本
docs/                    本文档集（只保留有效中文文档）
```

## 入口：cmd/chagent/main.go

- `cmd/chagent/main.go`：cobra 根命令，注册 `gateway` 等子命令；在 Linux/Android 上
  检测 Termux 环境并设置 `SSL_CERT_FILE`。
- `cmd/chagent/internal/gateway/command.go`：`gateway` 子命令（别名 `g`），
  flags 包括 `--debug`、`--allow-empty`、`--host`。
- `cmd/chagent/internal/agent/command.go`：`agent` 子命令。
- 其余 `internal/` 子包对应 cron/mcp/model/skills/status/version 命令功能。

## 进程与单例：pkg/pid

- `pkg/pid/pidfile.go`：pid 文件写入/校验/清理；文件名固定 `.dchagent.pid`
  （即使工作区名不同也一致）。
- `PidFileData`：JSON 结构 `{pid, token, version, port, host}`，写入工作区根目录。
- 单例语义：已存在有效 pid 文件且进程存活（`isProcessRunning` + `isChagentProcess`）时，
  拒绝再次启动；pid == 1（容器内 init）视为陈旧；陈旧 pid 文件自动清理后继续启动。
- `pkg/pid/process_name.go`：检测 pid 对应进程是否为本仓库二进制（`chagent`/`chagent`）。
- `port=0` 时由系统分配端口，pid 文件记录实际端口。

## 通道：pkg/channels 与 pkg/channels/pico

- `pkg/channels/`：通道抽象（`base.go`、`manager.go`、`registry.go`、`webhook.go`），
  工厂注册、消息总线（`pkg/bus`）、媒体上传下载。
- `pkg/channels/pico/`：
  - `pico.go`：`PicoChannel` 实现 `/pico/ws` WebSocket 端点；Bearer token 鉴权；
    会话由 `?session_id=` 指定，缺省时生成 uuid；支持 `ping`/`pong`、`typing`、
    媒体下载、并发连接上限。
  - `client.go`：`PicoClientChannel` 作为客户端主动连接远端 `/pico/ws`。
  - `protocol.go`：Pico 协议消息类型与字段定义（`message.send`/`message.create`/
    `typing.*`/`ping`/`pong`/`error`）。
  - `init.go`：注册 `pico` 与 `pico_client` 两种通道工厂。
- 鉴权头：`Authorization: Bearer <pico token>`；pico 内置，token = `md5(--name)`，不读配置。

## 配置：pkg/config

- `config.go`：`Config` 结构，`CurrentVersion = 2`；`Channels` 使用 `channels` JSON key。
  只支持 v2 JSON 一种配置方式，无迁移层（见 [chess-config](../guides/chess-config.zh.md)）。
- `config_loader.go`：唯一的 `loadConfig`，将 v2 JSON 解码到 `DefaultConfig` 之上。
- `minimal.go`：`model_list[*].api_key` 单数形式校验与归一；workspace 回退链；
  默认模型/推断。
- `envkeys.go`：进程发现环境变量（`CHAGENT_HOME`、`CHAGENT_CONFIG`、
  `CHAGENT_WORKSPACE`）与 `GetHome()`。
- `defaults.go`：`DefaultConfig()`，内含各子系统默认值。
- `context_config.go`：工作区上下文 / 运行时上下文 / 自主配置的默认值与校验
  （见 [workspace-context](workspace-context.zh.md)、[runtime-context](runtime-context.zh.md)、
  [taskstate-autonomy](taskstate-autonomy.zh.md)）。
- `gateway.go` / `config_channel.go` / `security.go`：gateway 配置、通道设置、敏感值安全存储。
- 工作区回退：`CHAGENT_WORKSPACE` 未设置时，默认 `$CHAGENT_HOME/workspace`
  （`pkg.WorkspaceName`）。加载配置**不写回**原文件；pico token 只在内存中由 `--name` 派生。

## Gateway 服务：pkg/gateway

- `pkg/gateway/gateway.go`：`gateway.Run` 组装 services（cron、heartbeat、media、
  channel manager、health server），注册 `GET /health`、`GET /ready`、
  `POST /reload`；监听 `SIGTERM` 后超时（`serviceShutdownTimeout=30s`、
  `gracefulShutdownTimeout=15s`）内退出并清理 pid。
- `pkg/gateway/listen.go`：监听地址解析与端口分配（port=0 场景）。
- `pkg/gateway/events.go`：gateway 事件绑定。
- `pkg/health/`：`/health`、`/ready` 的实现（`StatusResponse`、`Server`），
  `/ready` 未就绪返回 503。

## 模型 provider：pkg/providers

- `pkg/providers/registry.go` / `factory_provider.go`：按 `provider` 名字创建模型 provider。
- 只保留两类通用协议：OpenAI Chat Completions 兼容（`openai` + 30 个预设别名，
  统一走 `httpapi.HTTPProvider`）与 Anthropic Messages 兼容
  （`anthropic`/`anthropic-messages`/`anthropic-bearer`，走 `anthropic_messages/`）。
- 目录：`anthropic_messages/`、`common/`、`httpapi/`、`openai_compat/`、
  `protocoltypes/`、`messageutil/`；`provider_metadata.go` 维护协议/预设元数据。
- 厂商专属协议（Gemini/Azure/Bedrock/Copilot/CLI/OAuth/Antigravity）已删除；
  自定义 OpenAI 兼容服务用 `provider: "openai"` + `api_base` 接入。

## Agent 与上下文：pkg/agent

- `pkg/agent/agent.go`、`agent_init.go`：`AgentLoop`/`AgentInstance` 初始化，
  共享工具注册（web/message/reaction/send_file/load_image/skills/spawn/subagent/…），
  每 agent 的工具可用 `tools.<name>.enabled` 控制。
- `pkg/agent/instance.go`：agent 实例装配；task/todo/skill 工具在实例级注册。
- `pkg/agent/workspacecontext/`：工作区指令发现（`discovery.go`）、渲染（`render.go`）、
  组合（`compose.go`）、摘要（`digest.go`）、投影（`projector.go`），
  见 [workspace-context](workspace-context.zh.md)。
- `pkg/agent/workspace_context_runtime.go`：文件工具触碰记录（context touch）、
  运行时上下文持久化、工作区指令注入与失效。
- `pkg/agent/runtime_context.go`：P-08 运行时快照渲染与摘要，
  见 [runtime-context](runtime-context.zh.md)。
- `pkg/agent/autonomy_runtime.go`：自主续跑驱动、P-10 轮次 prompt、P-18 进程守卫，
  见 [taskstate-autonomy](taskstate-autonomy.zh.md)。
- `pkg/agent/taskstate_context.go`：TaskState 作为 durable context 注入。
- 其他：`threading`（`thinking.go`）、`steering.go`、`subturn.go`、`context_manager.go`、
  `prompt*.go`、`pipeline*.go`、`hooks.go`/`hook_mount.go`（hook 系统）。

## 任务状态：pkg/taskstate

- `pkg/taskstate/types.go`：`State`（目标/阶段/轮次/无进展计数）、`Todo`、
  `BlockedReason`；常量 `DefaultMaxRounds=20`、`MaxRoundsLimit=200`、
  `DefaultNoProgressRounds=3`、`DefaultBlockedThreshold=3`。
- `pkg/taskstate/transitions.go`：状态机转换（active/paused/blocked/complete）。
- 相关工具：`pkg/tools/integration/task_get.go`、`task_update.go`、`todo_write.go`、
  `pkg/agent/taskstate_context.go`、`pkg/agent/autonomy_runtime.go`。

## 工具：pkg/tools

- `pkg/tools/registry.go`：工具注册表与启停（`tools.<name>.enabled`）。
- `pkg/tools/fs/`：文件系统工具（`read_file`、`write_file`、`edit_file`、
  `append_file`、`list_dir`、`load_image`、`send_file`、`read_file(...)` 行读取）。
- `pkg/tools/integration/`：集成工具
  （`message`、`reaction`、`web_search`、`web_fetch`、`spawn`、
  `spawn_status`、`subagent`、`delegate`、`task_get`、`task_update`、`todo_write`、
  `skill`、`find_skills`、`install_skill`、MCP、搜索）。
- `pkg/tools/shell.go`：`exec` 工具；`session.go`、`spawn.go`：子进程/会话管理。
- 工具详细清单见 [参考：工具](../reference/tools.zh.md)。

## 脚本

- `scripts/chess-e2e.sh`：启动→health→pico ping/pong→SIGTERM→pid 清理的端到端验证。
- `scripts/live-agent-test.py`：真实模型功能测试（pico 鉴权、真实对话、工作区注入、
  `/context`、task/todo 工具、TaskState 持久化、SIGTERM/pid 清理）。
- `scripts/measure-startup.sh`：体积/启动时间/内存测量。
- `scripts/lint-docs.sh`：文档命名与目录约定 lint。
- `scripts/run-integration-tests.sh`：集成测试。

## 相关文档

- [工作区上下文](workspace-context.zh.md)
- [运行时上下文](runtime-context.zh.md)
- [任务状态与自主](taskstate-autonomy.zh.md)
- [Chess 最小配置](../guides/chess-config.zh.md)
- [Chess Pico 端到端](../guides/chess-pico-e2e.zh.md)
- [工具参考](../reference/tools.zh.md)

## 启动命令与 pico token

`gateway` 只保留可选 `--name` 参数；`--debug`/`--no-truncate`/`--allow-empty`/`--host`
已删除，行为全部由 `config.json` 决定。pico 是内置通道：`channels` 可省略，
token 固定为 `md5(--name)`（空 name 用 `default`），源码见
`pkg/config/agent_name.go`；token 不写入 config。
