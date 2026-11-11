# Chagent Chess 基础 Agent

本仓库是 [chess](https://github.com/sipeed/chess) 项目的命令行基础 agent，只保留 chess 客户端需要的功能：`gateway` 进程、`pico` 通道、模型 provider、工作区上下文与自主任务执行。多通道、Web 管理界面、硬件/设备/迁移/自更新等无关功能已删除。

## 构建与启动

```bash
# 编译到 bin/chagent（当前平台）
./scripts/build.sh
./bin/chagent version

# 启动 gateway（自动使用 bin/chagent；缺省时自动编译）
CHAGENT_HOME=/path/to/home \
CHAGENT_WORKSPACE=/path/to/workspace \
CHAGENT_CONFIG=/path/to/workspace/config.json \
./scripts/start.sh --name chagent
```

**重命名不兼容旧名称**：项目已从 `picoclaw` 重命名为 `chagent`；旧 `PICOCLAW_*`/`DDMCCLAW_*` 环境变量、旧二进制/`.picoclaw` home 均**不再兼容**，外部对接需改用 `CHAGENT_*`、`bin/chagent` 与 `~/.chagent`。

启动后会写入 `<workspace>/.chagent.pid`（`port=0` 时由系统分配端口），并提供：

- `GET /health`
- `GET /ready`
- `POST /reload`
- `GET /pico/ws?session_id=...`（`Authorization: Bearer <pico token>`）

收到 `SIGTERM` 后会在 5 秒内退出并清理 pid 文件。

## 最小配置

```json
{
  "version": 2,
  "model_list": [
    {
      "model_name": "glm",
      "model": "glm-5.3",
      "provider": "zhipu",
      "api_base": "https://open.bigmodel.cn/api/paas/v4",
      "api_key": "YOUR_KEY"
    }
  ]
}
```

pico 通道内置，不需要 `channels` 配置：pico token 固定为 `md5(--name)`。
例如 `--name chess-agent` 的 token 为 `md5("chess-agent")`；`--name` 省略时按
`default` 计算。外部 pico 客户端用同一个 md5 值作为 Bearer token。

工作区、工具、Hook 的标准配置样例见 [Chess 配置指南](docs/guides/chess-config.zh.md)。

## 常用命令

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
./scripts/live-agent-test.py /Users/wudaorui/chessdata/weichat/weichat/config.json
```

`live-agent-test.py` 会实际调用模型并验证：pico 鉴权/ping、真实对话、工作区指令注入、`/context`、`task_update`/`todo_write`/`task_get`、TaskState 持久化、SIGTERM/pid 清理。

## 文档

仅保留有效中文文档，入口见 [docs/README.md](docs/README.md)。

## 许可证

MIT，见 [LICENSE](LICENSE)。
