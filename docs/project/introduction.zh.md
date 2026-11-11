# Chagent Chess 基础 Agent · 项目介绍

> 面向：第一次接触本仓库的开发者与集成方。
> 性质：介绍性文档；当前实现状态、测试结论与残余问题见 [项目状态](status.zh.md) 与 [问题库](../issues/README.md)。
> 核对日期：2026-11-02（HEAD `5da70e0b` + 未提交修复工作树）。

## 1. 项目是什么

Chagent 是 [chess](https://github.com/sipeed/chess) 项目的**命令行基础 agent**：
chess 客户端通过一个常驻 `gateway` 进程把模型、工具、工作区指令与任务执行能力交给
Chagent，Chagent 通过内置的 `pico` WebSocket 通道收发消息。

本仓库只保留 chess 基础 agent 仍然需要的部分：

- `gateway` 常驻进程（HTTP 健康检查/热重载 + pico 通道 + agent loop）；
- 模型 provider（OpenAI Chat Completions 兼容 + Anthropic Messages 兼容两类通用协议）；
- 工具系统、工作区/运行时上下文、TaskState 与自主续跑、技能、MCP、Hook；
- 会话持久化（JSONL）与任务状态持久化。

**已删除、不再属于本项目范围**：Web 管理界面、多通道（telegram/discord/slack/微信等）、
硬件与设备、音频/语音、evolution/自更新、迁移工具、营销与多语言文档。

## 2. 能力边界

| 能力 | 是否支持 | 说明 |
|---|---|---|
| 常驻 gateway + 健康检查 | ✅ | `GET /health`、`GET /ready`、`POST /reload` |
| pico WebSocket 通道 | ✅ | 内置通道，token 固定 `md5(--name)`，无需 `channels` 配置 |
| agent loop / 多轮工具调用 | ✅ | 单 turn 工具迭代、并行工具批调度、loop guard |
| 工作区指令上下文 | ✅ | 根 `AGENTS.md` 作为 agent definition；`CLAUDE.md`/子目录文件作为持久上下文（baseline + delta） |
| 运行时上下文 / 任务状态 | ✅ | runtime snapshot、TaskState 作为 durable context、自主续跑 |
| 上下文管理 | ✅ | Turn 边界压缩、工具结果裁剪 + spill、摘要、临近上限提醒 |
| 工具集 | ✅ | 文件/目录、`exec`、web、message、spawn/subagent、task/todo、skill、MCP |
| 技能（skills） | ✅ | 技能目录默认作为 durable context，`skill` 工具按需加载正文 |
| MCP | ✅ | 配置 `tools.mcp.servers` 即自动启用；显式 `enabled=false` 可关闭 |
| Hook | ✅ | 内置/进程 hook，observer/interceptor/approval 超时可配 |
| 多模型 provider | ✅（限定两类协议） | OpenAI Chat Completions 兼容、Anthropic Messages 兼容；厂商专属 SDK/CLI/OAuth 已删 |
| 多通道 / Web UI | ❌ | 已删除；唯一通道是 `pico`（及主动连接的 `pico_client`） |
| 硬件、语音、自更新、迁移 | ❌ | 已删除 |
| surface replace、plan mode、turn-stopping 注入、jobs 交付历史 | ❌（未迁移） | 见 [仍存在的问题](../issues/open.zh.md) 的 DSH 未迁移项 |

## 3. 运行形态与核心组件

一个 `gateway` 进程内包含：channel manager（仅 pico）、agent loop、工具注册、cron/heartbeat、
媒体清理、health server 与配置热重载。核心代码位置：

| 组件 | 关键包/文件 | 说明 |
|---|---|---|
| 进程入口 | `cmd/chagent/` | `gateway`、`version` 子命令 |
| gateway | `pkg/gateway/` | `Run`/`runGateway`、监听、`/health` `/ready` `/reload`、SIGTERM 收尾 |
| pid/单例 | `pkg/pid/` | `.chagent.pid`（JSON，0600）、单例拒绝、stale pid 清理 |
| pico 通道 | `pkg/channels/pico/` | `/pico/ws`、Bearer 鉴权、`message.send`、typing、ping/pong |
| agent loop | `pkg/agent/` | turn 协调、pipeline、pre-step 注入、工具执行、自主续跑 |
| 工作区上下文 | `pkg/agent/workspacecontext/` | 发现/渲染/组合/delta 投影 |
| 配置 | `pkg/config/` | 唯一 v2 JSON 配置、默认值、严格校验 |
| 模型 provider | `pkg/providers/` | `httpapi`（OpenAI 兼容）与 `anthropic_messages` |
| 工具 | `pkg/tools/` | 注册表、文件工具、`exec`、集成工具（task/todo/skill/MCP…） |
| 持久化 | `pkg/session/`、`pkg/memory/`、`pkg/taskstate/`、`pkg/state/` | session JSONL + meta、长期记忆、任务状态 |

## 4. 目录结构（精简）

```text
cmd/chagent/                入口（cobra CLI：gateway / version）
config/config.example.json   当前唯一配置格式（v2）的可加载样例
pkg/
  agent/                     agent loop、上下文、pre-step、自主续跑、工具接线
  agent/workspacecontext/   工作区指令发现/渲染/组合/对账
  channels/                  通道抽象 + pico/pico_client 协议通道
  config/                    配置加载、默认值、校验（v2 唯一）
  gateway/                   HTTP/WS 服务、health/ready/reload、信号处理
  pid/ health/               pid 文件与单例、健康检查
  providers/                 通用模型协议（OpenAI 兼容 / Anthropic Messages）
  tools/                     工具注册表与全部工具
  taskstate/ session/ memory/ state/                     任务状态与持久化
  mcp/ skills/ cron/ heartbeat/ events/ bus/ routing/ …   运行时子系统
scripts/                     构建/E2E/测量/文档检查脚本
docs/                        中文文档集（本文件在 docs/project/）
integration/                 仅保留 MCP 集成套件（Docker-backed）
bin/                         编译输出（gitignored，`bin/chagent`）
build/                       测试产物目录（gitignored）
vendor/                      本地 vendor（gitignored，按需 go mod vendor）
```

## 5. 快速上手

### 5.1 构建

编译入口：`scripts/build.sh`（输出 `bin/chagent`，可设置 `VERSION`/`GO_BUILD_TAGS`）。
启动入口：`scripts/start.sh [gateway 参数]`（缺省自动构建并透传 `--name/--config` 等）。

```bash
./scripts/build.sh        # 输出到 bin/chagent
./bin/chagent version
```

`go.mod` 的 `go` 指令以仓库当前值为准（当前 `1.27.3`）；使用 vendor 模式构建。
`make build` 同样输出 `bin/chagent`（及其平台文件名）。

### 5.2 配置（唯一 v2 JSON）

只有一种配置方式：`version: 2` 的 JSON 文件。最小配置只需 `model_list`（单数 `api_key`）：

```json
{
  "version": 2,
  "model_list": [
    {
      "model_name": "glm",
      "provider": "zhipu",
      "model": "glm-5.3",
      "api_base": "https://open.bigmodel.cn/api/paas/v4",
      "api_key": "YOUR_API_KEY"
    }
  ]
}
```

其余配置（工作区、工具、Hook、MCP、gateway）都有默认值；标准完整样例见
[Chess 配置指南](../guides/chess-config.zh.md)，可加载样例见
[`config/config.example.json`](../../config/config.example.json)，全部字段默认值/校验规则见
[配置](../guides/configuration.zh.md)。

### 5.3 启动

```bash
CHAGENT_HOME=/path/to/home \
CHAGENT_WORKSPACE=/path/to/workspace \
CHAGENT_CONFIG=/path/to/workspace/config.json \
./chagent gateway --name chess-agent
```

启动后：

1. 监听成功即写入 `<workspace>/.chagent.pid`（JSON：`pid/token/version/port/host`，权限 `0600`）；
2. 提供 `GET /health`、`GET /ready`、`POST /reload`、`GET /pico/ws?session_id=...`；
3. 日志写入 `$CHAGENT_HOME/logs/gateway.log`（panic 日志在 `gateway_panic.log`）；
4. `port=0` 时由系统分配端口，实际端口写入 pid 文件。

### 5.4 客户端连接（pico）

- 连接 `ws://127.0.0.1:<port>/pico/ws?session_id=<session>`；
- 鉴权头 `Authorization: Bearer <pico token>`，token = `md5(--name)`（`--name` 省略按 `default`）；
- 常用消息：`ping`/`pong`、`message.send`、`typing.start`/`typing.stop`；
- 更完整的启动/关闭/配置/排错见 [Chess 集成指南](../chess-integration.md)。

### 5.5 关闭与重载

- `SIGTERM`：契约是 **5 秒内退出并删除 pid 文件**（`/reload` 或 `hot_reload` 之后同样成立）；
- `POST /reload`：带 pid 文件中的 token 请求头重新加载配置；
- 单例：已有存活 gateway 时第二实例会被拒绝。

### 5.6 本地验证

```bash
go test ./pkg/... -count=1        # 全量包测试
go vet ./...                      # 静态检查
./scripts/lint-docs.sh            # 文档规范
./scripts/chess-e2e.sh            # 启动/health/pico ping-pong/SIGTERM/pid
./scripts/measure-startup.sh      # 体积/启动时间/内存
```

## 6. 关键概念

| 概念 | 一句话说明 | 详细文档 |
|---|---|---|
| agent definition | 工作区根 `AGENTS.md`（或新格式 `AGENT.md`）解析为 agent definition 进入 system prompt；不会重复作为 workspace-context 注入 | [代码地图](../architecture/code-map.zh.md)、[配置 §3.2](../guides/configuration.zh.md) |
| workspace context | `CLAUDE.md`、`AGENTS.local.md`、子目录指令文件作为持久上下文，支持工具触碰后的 delta 刷新 | [工作区上下文](../architecture/workspace-context.zh.md) |
| runtime context | 时间/会话/发送者等运行时事实以 snapshot 方式按变化注入（`system` 为旧回退） | [运行时上下文](../architecture/runtime-context.zh.md) |
| durable context | session JSONL 中带 kind 的上下文消息：`workspace-instructions`/`runtime-context`/`task-state`/`task-round`/`skill-catalog`/`loop-guard`/`context-pressure` | [会话系统](../architecture/session-system.zh.md) |
| TaskState + 自主续跑 | 目标/轮次/无进展计数与 active→complete/blocked 状态机；`autonomy` 控制自动续跑轮数 | [任务状态与自主](../architecture/taskstate-autonomy.zh.md) |
| 上下文压缩 | Turn 边界裁剪 + 工具结果确定性裁剪（8192 字符阈值，头 4096/尾 1024）+ spill 落盘 + 摘要 | [配置 §3.2](../guides/configuration.zh.md) |
| spill | 被裁剪的超大工具输出落盘到 `<workspace>/.chagent/spill/`（0700/0600，默认 30 天） | [回滚与降级](../operations/rollback.zh.md) |
| Hook | 内置/进程 hook：observer、interceptor（可修改/拒绝工具调用）、approval | [Hook 系统](../architecture/hooks/README.zh.md) |
| MCP | `tools.mcp.servers` 配置后自动启用；工具名 `mcp_<server>_<tool>` | [工具参考](../reference/tools.zh.md) |
| skills | 技能目录作为 durable context（`catalog_mode=context/system`），`skill` 工具按需加载正文 | [工具参考](../reference/tools.zh.md) |
| pico token | 恒定 `md5(--name)` 的 32 位十六进制值，配置中的显式 token 会被覆盖 | [Chess 配置指南](../guides/chess-config.zh.md) |
| pid 文件 | `<workspace>/.chagent.pid`，JSON 字段与权限见上；单例与进程发现都依赖它 | [Chess 集成指南](../chess-integration.md) |

## 7. 与 chess 的契约

| 契约 | 内容 |
|---|---|
| 环境变量 | 仅 `CHAGENT_HOME`、`CHAGENT_WORKSPACE`、`CHAGENT_CONFIG` 三个 |
| 配置 | 唯一 `version: 2` JSON；启动与运行**不写回**配置文件；v0/v1/v3、`channel_list`、复数 `api_keys` 明确拒绝 |
| 进程发现 | `<workspace>/.chagent.pid`：`{pid, token, version, port, host}`，权限 `0600` |
| HTTP | `GET /health`、`GET /ready`、`POST /reload`（token 鉴权） |
| 通道 | `GET /pico/ws?session_id=...`，`Authorization: Bearer md5(--name)` |
| 信号 | `SIGTERM` ≤5 秒退出、pid 文件删除；`SIGKILL` 仅作为异常兜底 |
| 生命周期 | 单例保护；`hot_reload`/`POST /reload` 后仍满足 SIGTERM 契约 |
| 启动参数 | 仅 `--name`（只影响 pico token 派生，不改变其他行为） |

## 8. 文档导航

- **项目与状态**：[项目介绍](introduction.zh.md) · [项目状态](status.zh.md) · [问题库](../issues/README.md)
- **上手**：[根 README](../../README.md) · [Chess 配置指南](../guides/chess-config.zh.md) · [Chess 集成指南](../chess-integration.md) · [配置](../guides/configuration.zh.md) · [Chess Pico E2E](../guides/chess-pico-e2e.zh.md)
- **架构与设计**：[代码地图](../architecture/code-map.zh.md) · [工作区上下文](../architecture/workspace-context.zh.md) · [运行时上下文](../architecture/runtime-context.zh.md) · [任务状态与自主](../architecture/taskstate-autonomy.zh.md) · [路由系统](../architecture/routing-system.zh.md) · [会话系统](../architecture/session-system.zh.md) · [运行时事件](../architecture/runtime-events.zh.md) · [Hook 系统](../architecture/hooks/README.zh.md)
- **测试与验收**：[新实现 Agent 完整真实场景功能测试报告](../design/agent-functional-test-report.zh.md) · [DSH → Chagent 迁移报告](../design/dsh-migration-report.zh.md) · [裁剪清单](../design/chess-trim-inventory.md)
- **操作与安全**：[故障排查](../operations/troubleshooting.zh.md) · [调试](../operations/debug.zh.md) · [回滚与降级](../operations/rollback.zh.md) · [安全配置](../security/credential_encryption.zh.md)
- **参考**：[工具参考](../reference/tools.zh.md) · [Provider](../guides/providers.zh.md) · [anthropic-messages 协议](../reference/anthropic-messages-x-api-key.md)
- **并行专题（由对应文档维护，若未产出以 owner 交付为准）**：[DSH 对标](../reference/dsh-parity.zh.md) · [上下文与 DSH 对比](../architecture/context-vs-dsh.zh.md) · [MCP 支持边界](../reference/mcp-support.zh.md) · [健壮性分析](../operations/robustness.zh.md)
