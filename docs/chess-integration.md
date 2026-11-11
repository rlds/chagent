# Chess 集成指南（启动 / 关闭 / 配置 / 排错）

> 适用范围：Chagent 作为 **chess 基础 agent** 的部署与运维集成。
> 关键代码路径：`cmd/chagent`、`pkg/gateway/`、`pkg/channels/pico/`、
> `pkg/config/`、`pkg/pid/`。
> 核对日期：2026-10-30（HEAD 为 chess 专用裁剪后实现）。

## 1. 这份文档解决什么

chess/client 只依赖一个长驻进程提供能力，不存在 Web 管理界面、多通道、硬件/语音
或自更新组件。本文给出与 chess 对接所需的最小契约：

- 如何构建、启动、关闭 gateway；
- 需要外部提供哪些配置（唯一 v2 JSON）；
- 进程发现（pid/端口/health）与 pico 通信；
- 常见故障的判断与处理。

配置字段细节见 [Chess 配置指南](guides/chess-config.zh.md)，配置项默认值与校验规则见
[配置](guides/configuration.zh.md)，E2E 脚本与链路见
[Chess Pico 端到端](guides/chess-pico-e2e.zh.md)。

## 2. 构建

```bash
# 仓库根目录
./scripts/build.sh          # 输出 bin/chagent
./bin/chagent version
# 或：make build
```

只发布一个二进制 **`bin/chagent`**（Go module `github.com/rlds/chagent`）。`go.mod`
的 `go` 指令以仓库当前值为准；构建无第三方运行时依赖（`CGO_ENABLED=0` 可交叉编译）。
启动用 `./scripts/start.sh --name <agent>`。**重命名不兼容旧名称**：旧 `PICOCLAW_*`/`DDMCCLAW_*`、旧二进制与 `.picoclaw` home 不再支持；外部对接需改用 `CHAGENT_*`、`bin/chagent`、`~/.chagent`。

## 3. 配置：唯一 v2 JSON

只有一种配置方式：`version: 2` 的 JSON 文件（默认 `config.json`，路径由
`CHAGENT_CONFIG` 指定）。v0/v1/v3、YAML 主配置、`channel_list`、复数
`api_keys`、逐字段环境变量覆盖均不支持；版本不是 2 时启动直接失败。
完整样例见 [Chess 配置指南](guides/chess-config.zh.md)，仓库内可加载的最小样例见
[`config/config.example.json`](../config/config.example.json)。

最小配置（`model_list` 必需，单数 `api_key`）：

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

只支持两类通用协议：OpenAI Chat Completions 兼容（`provider: openai` 及 zhipu /
deepseek 等预设别名）与 Anthropic Messages 兼容（`provider: anthropic` /
`anthropic-messages` / `anthropic-bearer`）。自定义服务用 `provider: openai` +
`api_base`。

三个进程发现环境变量（profile 只读这三个）：

| 变量 | 作用 |
|---|---|
| `CHAGENT_HOME` | 安装主目录（`logs/`、默认 workspace 的根） |
| `CHAGENT_WORKSPACE` | agent 工作区；pid 文件落盘目录，也是默认文件权限边界 |
| `CHAGENT_CONFIG` | 配置文件路径 |

配置里的 `agents.defaults.workspace` 未显式提供时，workspace 按
`CHAGENT_WORKSPACE` → 配置文件所在目录 → 进程 cwd 的顺序回退。

### pico token：`md5(--name)`

pico 是内置通道，配置里**不需要** `channels`，也不需要写 token：

```text
pico token = md5(--name)      # 32 位十六进制
```

- `--name` 省略时按 `default` 计算；
- 配置中写入的 `channels.pico.token`（占位符或显式值）会在启动时被
  `md5(--name)` 覆盖，不参与认证；
- 外部 pico 客户端用同一个 md5 值作为 `Authorization: Bearer <token>`。

## 4. 启动

```bash
CHAGENT_HOME=/path/to/home \
CHAGENT_WORKSPACE=/path/to/workspace \
CHAGENT_CONFIG=/path/to/workspace/config.json \
./chagent gateway --name chess-agent
```

- 唯一启动参数是 `--name`（可省略）；它只决定 pico token 的 md5 输入，不改变
  其他行为；
- `gateway` 是唯一常驻子命令；`version` 用于查看构建信息（已删除 agent/cron/
  mcp/model/skills/status 等子命令）。

启动后：

1. 监听成功后立即写 `<workspace>/.chagent.pid`（JSON，权限 `0600`，字段
   `pid/token/version/port/host`；`token` 为 32 位 hex，用于本机进程发现）；
2. 提供如下 HTTP/WS 入口（`port=0` 时由系统分配，实际端口写入 pid 文件）：

| 入口 | 用途 |
|---|---|
| `GET /health` | 存活检查，返回 200 |
| `GET /ready` | 就绪检查，返回 200 |
| `POST /reload` | 重新加载配置（受本机 token 鉴权，请求头携带 pid 文件中的 token） |
| `GET /pico/ws?session_id=...` | pico WebSocket，Bearer 为 `md5(--name)` |

3. 日志写入 `<CHAGENT_HOME>/logs/gateway.log`（网关 panic 会重定向到
   `<home>/logs/gateway_panic.log`）；
4. 进程 `cwd` 为 workspace。

## 5. 关闭与重启

- 正常关闭：向 gateway 进程发送 `SIGTERM`。契约是 **5 秒内退出、删除 pid
  文件**；该契约对 `/reload` 或 `hot_reload` 之后的进程同样成立（回归测试
  `pkg/channels/manager_test.go` 覆盖 reload 后 shutdown）。
- 不要用 `SIGKILL` 做常规关闭：pid 文件会残留，下次启动前需确认记录 PID 已死。
- 重启：同一 workspace 下先确认上一实例已退出（pid 文件已删除），再以相同
  `--name`、config、workspace 启动即可。活跃实例存在时第二实例会被单例检查拒绝，
  错误信息形如 `gateway is already running (PID: <pid>, version: <ver>)`。
- 配置热更新：`gateway.hot_reload=true` 时配置文件被改写会自动重载；也可显式
  `POST /reload`。重载不重启进程、不改变 pico token（token 仍为 md5(--name)）。

回滚/降级开关（关闭新机制而不改代码）见
[回滚与降级演练](operations/rollback.zh.md)：`workspace_context.enabled=false`、
`runtime_context.mode="system"`、`autonomy.enabled=false`、
`tools.skills.catalog_mode="system"` 等。

## 6. 排错

| 症状 | 优先检查 | 处理 |
|---|---|---|
| 启动即报 `unsupported config version` | `config.json` 的 `version` | 只支持 `version: 2`；v3/旧 schema 需重写 |
| 启动即报 `model_list api_keys is not supported; use the singular api_key` | `model_list[*]` | 复数 `api_keys` 改成单数 `api_key` |
| 启动即报 `model "..." not found in model_list` | `agents.defaults.model_name` 与 `model_list[*].model_name` | 二者必须一致；且 `provider`/`model` 组合可解析 |
| `gateway is already running (PID: ...)` | pid 文件 | 确认进程是否真的存活；`kill -TERM` 后重试，必要时手工删除 pid 文件（仅当确认进程已退出） |
| `/health` 长时间不返回 200 | gateway 日志、端口占用、provider 初始化 | 看 `<home>/logs/gateway.log`；`port=0` 时从 pid 文件取实际端口；检查模型配置是否可创建 provider |
| pico 客户端 401/拒绝 | Bearer token 是否等于 `md5(--name)` | 用同一 `--name` 重新计算 md5；配置里的显式 token 不生效 |
| pico 连接成功但没有回复 | workspace/模型/provider 配置 | 检查 workspace 可写、`api_key` 有效；看 gateway 日志中的 LLM/tool 错误 |
| `SIGTERM` 后进程不退出 / pid 残留 | 是否在 `/reload`、`hot_reload` 之后 | 正常路径应 ≤5s 退出；若挂住，收集 `gateway_panic.log` 的 goroutine dump 与复现步骤并上报（reload 后 shutdown 已有回归测试覆盖） |
| 配置文件被写回/出现额外字段 | 版本与来源 | 当前加载**不会**重写配置文件；若被改写，检查是否有外部进程在写同一路径 |
| 工具/MCP/Hook 不生效 | `tools.*`、`tools.mcp.servers`、`hooks.*` | 见 [工具参考](reference/tools.zh.md)、[配置](guides/configuration.zh.md) §3；MCP 只要配置 `servers` 即自动启用 |

排错入口汇总：[疑难解答](operations/troubleshooting.zh.md)、
[调试](operations/debug.zh.md)。日志关键字：`Gateway started on`、
`Health endpoints available at`、`error loading config`、`Tool call`、
`Forced compression pruned tool results`。

## 7. 相关文档

- [Chess 配置指南](guides/chess-config.zh.md)：唯一配置方式与标准完整样例；
- [Chess Pico 端到端](guides/chess-pico-e2e.zh.md)：`chess-e2e.sh` /
  `live-agent-test.py` / `measure-startup.sh`；
- [配置](guides/configuration.zh.md)：字段默认值与校验规则；
- [回滚与降级演练](operations/rollback.zh.md)：配置级回退开关；
- [架构：代码地图](architecture/code-map.zh.md)、[路由系统](architecture/routing-system.zh.md)、
  [会话系统](architecture/session-system.zh.md)。