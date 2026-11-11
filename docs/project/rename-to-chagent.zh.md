# 项目重命名记录：PicoClaw → Chagent

> 日期：2026-11-02。性质：**破坏性重命名**；不保留旧名称兼容。

## 1. 名称映射

| 项目 | 旧 | 新 |
|---|---|---|
| 项目/产品名 | PicoClaw / picoclaw | **Chagent / chagent** |
| Go module | `github.com/sipeed/picoclaw` | `github.com/rlds/chagent` |
| 命令目录 / 二进制 | `cmd/picoclaw` / `picoclaw` | `cmd/chagent` / **`bin/chagent`** |
| CLI `Use` | `ddmcclaw` | `chagent` |
| 环境变量前缀 | `PICOCLAW_*`、`DDMCCLAW_*` | **`CHAGENT_*`** |
| 用户 home | `~/.picoclaw` | **`~/.chagent`** |
| pid 文件 | `.ddmcclaw.pid` | **`.chagent.pid`** |
| MCP client 名 | `ddmcclaw` | `chagent` |
| 内置技能目录 | `workspace/skills/picoclaw-agent` | `workspace/skills/chagent-agent` |

## 2. 构建与启动

```bash
# 编译到仓库内 bin/（用户指定目标）
./scripts/build.sh            # -> /Users/wudaorui/code/picoclaw/bin/chagent
make build                    # 同样输出 bin/chagent（含平台文件名）

# 启动 gateway（缺省自动构建；参数透传）
./scripts/start.sh --name chagent
CHAGENT_HOME=/path/to/home \
CHAGENT_WORKSPACE=/path/to/workspace \
CHAGENT_CONFIG=/path/to/workspace/config.json \
./scripts/start.sh --name chagent
```

`scripts/build.sh` 支持 `VERSION`/`GIT_COMMIT`/`BUILD_TIME`/`GO_VERSION`/`GO_BUILD_TAGS`/`BIN_DIR` 覆盖；
二进制版本信息通过 `-X github.com/rlds/chagent/pkg/config.*` 注入。

## 3. 不兼容旧名称（重要）

本次重命名明确**不做任何旧名称回退/别名**：

- 旧环境变量 `PICOCLAW_*`、`DDMCCLAW_*` **不再读取**；不再执行环境变量迁移；
- 旧二进制名 `picoclaw`/`ddmcclaw` 不再提供（仓库根目录旧二进制已删除，仅发布 `bin/chagent`）；
- 旧 home `~/.picoclaw` **不回退**（只使用 `~/.chagent` 或 `CHAGENT_HOME`）；
- 旧 pid 文件 `.ddmcclaw.pid` 不再读取/写入；
- 旧 MCP client 名 `ddmcclaw` 改为 `chagent`；
- 旧技能目录 `picoclaw-agent` 已改名 `chagent-agent`（内容/引用同步更新）。

外部对接方（chess 等）需要同步修改：

1. 启动二进制路径 → `bin/chagent`（或安装后的 `chagent`）；
2. 环境变量 → `CHAGENT_HOME/CHAGENT_WORKSPACE/CHAGENT_CONFIG/CHAGENT_LOG_LEVEL...`；
3. 读取 pid → `<workspace>/.chagent.pid`；
4. 数据目录迁移 → 将 `~/.picoclaw` 内容按需复制到 `~/.chagent`（**不做自动迁移**）；
5. 配置中的产品名/注释按需更新（配置 JSON 结构 `version: 2` 不变）。

## 4. 验证

- `go build ./...`、`go test ./pkg/... ./cmd/...` 通过；
- `./scripts/build.sh` → `bin/chagent`；`bin/chagent version` 正常；
- `./scripts/start.sh --help` 正常；`CHAGENT_HOME/…` 生效；
- 仓库内 `git grep -n 'PICOCLAW\|DDMCCLAW\|picoclaw\|ddmcclaw'` 无代码/文档引用（仅本记录与外部迁移说明保留旧名）。

## 5. 相关

- [项目介绍](introduction.zh.md) · [项目状态](status.zh.md) · [chess 集成](../chess-integration.md)
- 模型可见文本审计：[model-facing-text](../reference/model-facing-text.zh.md)
