# Chess Pico 端到端（E2E）

> 适用范围：验证 Chagent chess 基础 agent 的启动、健康检查、pico 通道、
> 自主任务、SIGTERM 退出与 pid 清理的端到端流程。
> 关键代码路径：`scripts/chess-e2e.sh`、`scripts/live-agent-test.py`、
> `scripts/measure-startup.sh`、`pkg/gateway/`、`pkg/channels/pico/`。
> 核对日期：2026-10-29

## 自动化脚本

| 脚本 | 用途 |
| --- | --- |
| `scripts/chess-e2e.sh` | 启动→health→pico ping/pong→SIGTERM→pid 清理 |
| `scripts/live-agent-test.py` | 真实模型功能测试（pico 鉴权、真实对话、工作区指令注入、`/context`、task/todo 工具、TaskState 持久化、SIGTERM/pid 清理） |
| `scripts/measure-startup.sh` | 体积/启动时间/内存测量 |
| `scripts/run-integration-tests.sh` | 集成测试 |

```bash
# 全量测试
go test ./pkg/... -count=1

# 静态检查 / 文档检查
go vet ./...
./scripts/lint-docs.sh

# Chess 兼容 E2E（启动、health、pico ping/pong、SIGTERM、pid 清理）
./scripts/chess-e2e.sh

# 体积/启动时间/内存测量
./scripts/measure-startup.sh

# 真实模型功能测试（读取外部 config 的 model_list，临时 workspace + chess 默认配置）
./scripts/live-agent-test.py /path/to/your/config.json
```

## 端到端链路

1. **启动**：`./chagent gateway --name chagent` 写入
   `<workspace>/.chagent.pid`（`port=0` 时系统分配端口）。
2. **健康检查**：`GET /health` 返回 ok；`GET /ready` 就绪后返回 ready，
   未就绪返回 503（`pkg/health/`）。
3. **Pico 通道**：`GET /pico/ws?session_id=...` 使用
   `Authorization: Bearer <pico token>` 鉴权；WebSocket 建立后支持
   `message.send` / `message.create` / `typing.*` / `ping` / `pong`。
4. **自主任务**：`task_update start` 创建任务 → 自主驱动按轮次推进 →
   `task_get` 读取状态 → `todo_write` 维护待办 → 达到目标后
   `task_update complete`（需证据）。
5. **退出**：收到 `SIGTERM` 后，gateway 在超时窗口内优雅退出，清理 pid 文件。

## 关键行为验证

- **pid 单例**：已存在有效 pid 且进程存活时拒绝再次启动；pid==1 视为陈旧；
  陈旧 pid 自动清理（`pkg/pid/pidfile.go`）。
- **配置不写回**：启动加载配置但不重写原文件；pico token 不来自配置
  chess/client 可读（`pkg/config`）。
- **真实模型**：`live-agent-test.py` 读取外部 config 的 `model_list`，
  用临时 workspace + chess 默认配置，实际调用模型验证注入与持久化。

## 相关文档

- [Chess 最小配置](chess-config.zh.md)
- [代码地图](../architecture/code-map.zh.md)
- [任务状态与自主](../architecture/taskstate-autonomy.zh.md)
- [工具参考](../reference/tools.zh.md)

## --name 与 pico token

pico 内置，`config.json` 不需要 `channels`/token。约定：

```text
pico token = md5(--name)
```

E2E 脚本传 `--name chess-e2e`，外部 pico 客户端用 `md5("chess-e2e")` 作为
`Authorization: Bearer`。`--name` 省略时使用 `default`；token 不写入配置。
