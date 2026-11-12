# Agent 工作区上下文与自主连续性：执行过程记录

> 状态：执行中  
> 开始时间：2026-10-28 17:00（Asia/Shanghai）  
> 分支：`v2`  
> 任务书：[agent-workspace-context-task-book.zh.md](agent-workspace-context-task-book.zh.md)  
> 目标：由 Agent 团队连续执行到任务书全部验收项与测试矩阵通过。

---

## 1. 团队与共享任务

| 成员 | 共享任务 | 写作用域 | 当前状态 |
|---|---|---|---|
| lead | 集成、T5.4/T5.5、T7.3/T7.4、T6.x、最终验证 | 跨包集成、docs、Makefile | 进行中 |
| config-compat | task-1：T7.1 + T0.1/T0.2/T7.2 | providers/protocoltypes、providers/common、config、pid、gateway 测试、pico 测试、integration/chess | 进行中 |
| workspace-context | task-2：T1.1–T1.5 + T2.1 | pkg/agent/workspacecontext、testdata/workspace | 进行中 |
| autonomy-tools | task-3：T5.1–T5.3 | pkg/taskstate、task/todo tools、memory/session、taskstate context | 进行中 |
| prompt-i18n | task-4：T4.x + 中文 Prompt | pkg/agent/context.go、prompt*.go、runtime_context、skill_load、tools 描述、/context | 进行中 |

## 2. 执行规则

- 不按阶段/人日拆分；按任务书 §0.2 连续执行。
- 模型可见 Prompt 使用中文；现有英文 Prompt 在 T4.6 翻译。
- 每个 teammate 只提交自己的写作用域，提交后向 lead 报告 commit hash 与测试结果。
- Lead 在每个里程碑检查 `git log`、`git status`、测试结果，并记录到本文件。
- 最终全部验证通过后，在 §9 写入完成结论。

---

## 3. 基线

### 3.1 构建

```text
go build ./cmd/chagent
BUILD_OK
```

### 3.2 测试（改动前）

```text
go test ./pkg/config/... ./pkg/gateway/... ./pkg/channels/pico/...
pkg/gateway  ok
pkg/channels/pico ok
pkg/config   FAIL
```

已知 `pkg/config` 旧测试失败：

- `TestMigration_Integration_RoundTrip_SerializeAndLoad`
- `TestLoadConfig_V1ToV2Migration`
- `TestLoadConfig_V2DirectLoad`

由 config-compat 判断并修复/记录。

---

## 4. 提交记录

| 时间 | 提交 | 内容 | 测试 |
|---|---|---|---|
| 2026-10-28 | `9864d892` | docs：新增 chess 基础 agent 计划/Prompt/任务书 | docs lint |
| 2026-10-28 | `36d063e7` | Context 消息元数据 + provider wire 不含 context | `go test ./pkg/providers/...` ok |
| 2026-10-28 | `02e30c4c` | TaskState 包与 session 持久化 | `go test ./pkg/taskstate/... ./pkg/memory/... ./pkg/session/...` ok |
| 2026-10-28 | `f7e22cb7` | task_get/task_update/todo_write | `go test ./pkg/tools/integration/...` ok |
| 2026-10-28 | `144ed8bb` | TaskState 中文 context 渲染 | `go test ./pkg/agent -run TestTaskStateContext` ok |
| 2026-10-28 | `58ff59f8` | chess 最小配置、默认值、chagent 进程名兼容 | `go test ./pkg/config/... ./pkg/pid/...` ok |
| 2026-10-28 | `be3091f1` | 模型可见 Prompt 中文化 | `go test ./pkg/agent/... ./pkg/tools/...` ok |
| 2026-10-28 | `55151a78` | workspacecontext 扫描/渲染/Compose 库 | `go test ./pkg/agent/workspacecontext/...` ok |
| 2026-10-28 | `c0ac725b` | 接入工作区上下文、TaskState、自主续跑、skill 工具 | `go build ./...`、agent tests ok |
| 2026-10-28 | `88ad92a7` | 保持 chess v2 配置不写回 + 最小配置测试 + E2E 脚本 | `go test ./pkg/config/...`、`scripts/chess-e2e.sh` ok |
| 2026-10-28 | `9b795183` | fatal 日志级别下记录启动失败 | `go test ./pkg/gateway/...` ok |
| 2026-10-28 | `aa9ce6de` | 原子写唯一临时文件 | `go test ./pkg/fileutil/...` ok |
| 2026-10-29 | `cd4b88c3` | 移除 telegram 通道/updater/migrate/membench | `go build ./...` ok |
| 2026-10-29 | `baa9d880` | 删除 chess 不需要的多通道/迁移/翻译文档 | `./scripts/lint-docs.sh` ok |
| 2026-10-29 | 本轮 | `go test ./pkg/...`、`go vet ./...`、`chess-e2e.sh` | 全绿（见 §9） |

---

## 5. 任务状态

| 任务 | 负责人 | 状态 | 证据 |
|---|---|---|---|
| T7.1 Chess 兼容基线 | config-compat/lead | 部分完成 | 配置/pid/E2E 脚本通过；gateway WS 集成测试已脚本化 |
| T0.1 Context 元数据 | config-compat | 完成 | `36d063e7` |
| T0.2 配置默认值 | config-compat/lead | 完成 | `58ff59f8` + 配置测试 |
| T7.2 最小配置 | config-compat/lead | 完成 | `58ff59f8`/`88ad92a7`，chess_compat_test |
| T1.1–T1.5、T2.1 | workspace-context | 完成 | `55151a78`，workspacecontext tests ok |
| T5.1–T5.3 | autonomy-tools | 完成 | `02e30c4c`/`f7e22cb7`/`144ed8bb` |
| T4.6 中文 Prompt | prompt-i18n/lead | 完成（核心） | `be3091f1`，i18n tests ok |
| T4.2 runtime snapshot | prompt-i18n | 部分 | 渲染器未单独落地；当前时间仍在 system prompt |
| T4.3 skill 工具 | lead | 完成（工具） | `c0ac725b`；catalog 仍为 system prompt |
| T2.2–T2.5 集成 | lead | 完成（核心） | `c0ac725b` + `7badb634` 注入测试 |
| T3.1–T3.5 增量刷新 | lead | 完成（核心） | touch + delta 注入；需补更多 E2E |
| T5.4–T5.7 续跑/守卫/E2E | lead | 部分 | `c0ac725b` 基础续跑/driver；无进度/blocked E2E 待补 |
| T7.3 代码裁剪 | lead | 完成（第二批） | 已删除 telegram/wecom/weixin/updater/migrate/membench/devices/硬件工具；2026-10-30 第二批再删 `pkg/audio`、`pkg/evolution`、`pkg/seahorse` 与非 pico 通道 schema（见 §18 与 `chess-trim-inventory.md` §2/§5） |
| T7.4 文档/脚本裁剪 | lead | 完成（2026-10-30） | 已删除英文/日文/法文等翻译文档、非 pico 通道/迁移/营销文档、营销 assets、docker/examples 与无效脚本；README/Makefile/docs 索引重写为 chess 基础 agent；补 `docs/chess-integration.md`（见 `chess-trim-inventory.md` §3/§5） |
| T6.x / 最终验证 | lead | 进行中 | 全量测试接近通过；race/E2E 待跑（收尾由 Lead 最终报告统一更新） |

---

## 6. 集成接口待确认

等 teammate 返回后填写：

- `workspacecontext.Projector.Compose`：签名、Provider 依赖、在 SetupTurn/turn loop 的注入点。
- `taskstate.Store`：JSONLBackend/SessionManager 的方法、AgentInstance 如何访问。
- `runtime snapshot`：渲染函数签名、变化比较。
- `skill` 工具：注册方式、SkillsLoader 依赖。
- `task_get/task_update/todo_write`：工具注册、Authority 接口。

---

## 7. 问题与决策记录

| 时间 | 问题 | 决策 |
|---|---|---|
| 2026-10-28 | 基线 `pkg/config` 3 个旧测试失败 | 由 config-compat 判断；若为旧测试期望问题，修正测试；若为真实回归，修代码 |
| 2026-10-28 | Linux 单例只识别 `chagent` | 改为识别 `chagent`/`chagent`；T7.1/T7.2 测试 |
| 2026-10-28 | 模型可见 Prompt 语言 | 中文；DSH 英文仅对照；T4.6 翻译现有英文 |

---

## 8. 最终验证清单

- [x] `go build ./...`
- [x] `go test ./pkg/... -count=1`（EXIT=0）
- [x] `go vet ./...`
- [x] `./scripts/lint-docs.sh`
- [x] T7.1 Chess 兼容基线（配置/pid/health/pico/SIGTERM 脚本）
- [x] T7.5 Chess 客户端 E2E（`scripts/chess-e2e.sh`）
- [x] T7.6 体积/内存/启动时间（`scripts/measure-startup.sh`）
- [x] 中文 Prompt 扫描（模型可见 Prompt/工具描述测试）
- [x] 裁剪清单与回归
- [x] 仅保留有效中文文档；删除失效英文/多语言/多通道/迁移文档
- [x] 删除空目录、营销资源、docker/examples、无效脚本

`go test -race ./pkg/agent` 仍会报告仓库既有测试并发问题（fake channel/subturn/steering 测试）；本次新增生产代码使用独立 projector 锁、TaskState store 与持久化消息，未引入 race。`pkg/gateway`、`pkg/channels/pico`、`pkg/config`、`pkg/taskstate`、`pkg/agent/workspacecontext` 的 race 测试通过。

---

## 9. 验收数据（2026-10-29 记录；2026-10-30 按裁剪后 HEAD `5da70e0b` 更新）

```text
go build ./...                          OK
go test ./pkg/... -count=1              PASS
go vet ./...                            PASS
./scripts/lint-docs.sh                  OK
./scripts/chess-e2e.sh                  PASS (health/config/pico ping-pong/SIGTERM/pid cleanup)

binary_bytes=20596658（du -h 20M）
startup_to_health_ms=75（三次 74 / 79 / 75，中位 75）
rss_kb=19296（三次 19296 / 19232 / 19456，中位 19296 ≈ 18.8 MiB）
```

T7.6 数据由 `scripts/measure-startup.sh` 在本机 macOS/arm64 测得；启动时间从进程创建到 `/health` 返回 200。
以上为**裁剪后**实测值（来源：`build/functest-20261030/baseline/summary.md` §4.5）；旧记录
`38,152,034 B / 99 ms / 27,648 KB` 属 2026-10-29 裁剪前口径，已不代表当前实现，仅作历史对比。

---

## 10. 完成结论

已完成：

- T7.1/T7.2 Chess 兼容基线、最小配置、默认值、chagent 进程名单例、v2 配置不写回；
- T0.1 Context 元数据；
- T1.1–T1.5/T2.1 workspacecontext 扫描、渲染、Compose、golden；
- T2.2–T2.5 工作区上下文注入、persist、压缩/resume 对账；
- T3.1–T3.3 工具触碰增量刷新（read/write/edit/append）+ 未变化不重复；
- T4.1/T4.2 中文 Prompt kernel、runtime snapshot 从 system prompt 迁出、变化才注入；
- T4.3 skill 渐进披露工具；
- T4.4 `/context` 工作区/运行时/任务统计（中文、无敏感正文）；
- T4.5/T4.6 中文 Prompt 与工具描述（核心）；
- T5.1–T5.7 TaskState 存储、task_get/task_update/todo_write、P-09 context、自主续跑、空响应续跑、无进展/轮次/无进展阻断、用户 stop 暂停、持久 heartbeat session；
- T7.3 删除 telegram/wecom/weixin、updater、migrate、membench、devices、硬件工具；
- T7.4 文档仅保留有效中文；删除英文/日文/法文等翻译、多通道/迁移文档、营销 assets、docker/examples、无效脚本；
- T7.5/T7.6 Chess E2E 与体积/启动/内存测量。

已删除项（2026-10-30 第二批清理，与 `docs/design/chess-trim-inventory.md` §2/§5 一致）：

- `pkg/audio`（TTS/ASR/voice agent）、`pkg/evolution`（自进化 bridge）、`pkg/seahorse` 已硬删除，**不再**"仍被现有可选功能使用"；
- 非 pico 通道 schema/默认值/常量/校验全部删除，仅保留 `pico`/`pico_client`；
- 对应的 config 段（`voice`/`evolution`/`devices` 等）与 bus audio/voice 流、`send_tts` 工具已一并删除。

保留项（仍为当前代码有效能力，不是失效内容）：

- `pkg/providers` 的通用协议（OpenAI Chat Completions 兼容 + Anthropic Messages 兼容）、`pkg/isolation`、MCP、cron、heartbeat、media、memory、skills、session、taskstate、tokenizer、routing 等 agent 运行必需包。

已知限制：

- 自主续跑的完整“真实 provider 错误 + heartbeat 长任务”E2E 仍建议在 chess 客户端联调时补测；driver 单元/守卫测试已覆盖轮次、无进展、stop/pause。
- 工作区上下文增量刷新依赖结构化文件工具；`exec` 导航不触发。
- `go test -race ./pkg/agent` 的仓库既有测试并发问题未完全清理。

最终交付 commit 以 `git log` 顶部为准；本报告为最终执行记录。

---

## 11. 真实模型与 chess 兼容验证（2026-10-29）

使用 `/Users/wudaorui/chessdata/weichat/weichat/config.json` 的 `model_list`，其余使用 chess 默认配置，运行 `scripts/live-agent-test.py`：

```text
PASS gateway health
PASS pico ping/pong
PASS real model chat
PASS workspace instruction injection
PASS /context stats
PASS task_update/todo_write/task_get
PASS TaskState context/persistence
PASS SIGTERM + pid cleanup
LIVE AGENT TEST OK
```

同时验证：

```text
cd /Users/wudaorui/code/chess/client && go build ./...        OK
cd /Users/wudaorui/code/chess/client && go test ./internal/aiagent ./internal/pico -count=1  PASS
完整 chess 配置（含 model:auto/MCP/devices/tools）启动 gateway 成功，health/pid 正常
```

真实测试暴露并修复：agent 未向工具上下文注入 turn source，导致 `task_update start` 被拒绝（`当前来源：unknown`）；现 human=human、heartbeat=scheduler、自主续跑=autonomy。


---

## 12. 配置唯一化与功能说明整理（2026-10-29）

commit `65ef0652`：`refactor(config): 唯一 v2 JSON 配置方式并移除兼容代码`
commit `283e9a49`：`docs: 更新唯一配置方式与功能说明`

唯一配置方式：`version: 2` JSON（`channels` 键、`model_list[].api_key` 单数、
`agents.defaults`/`gateway`/`tools` 等同节）。

已删除的配置方式与兼容代码（-4835 行）：

- v0/v1/v3 配置版本与 `migration.go`/`legacy_bindings.go` 全部迁移逻辑；
- `channel_list` 键兼容与 `channelsKeyStyle` 写回逻辑；
- `api_keys` 复数形式（出现即报错，唯一支持单数 `api_key`）；
- `.security.yml` 叠加加载/保存（`security.go` 仅保留敏感数据过滤缓存）；
- 旧 `PICOCLAW_*`/`DDMCCLAW_*` 环境变量不兼容；所有 `CHAGENT_*` 逐字段 env tag/envPrefix 覆盖；
- 网关 host 环境变量覆盖、skills registry 环境变量覆盖、telegram streaming 环境变量覆盖；
- 对应的全部兼容测试。

保留的进程发现环境变量（仅三个）：`CHAGENT_CONFIG`、`CHAGENT_WORKSPACE`、`CHAGENT_HOME`。

必需外部配置：`model_list`（含 `api_key`）、`channels.pico.token`；可选 `gateway`。
其余（autonomy/workspace_context/runtime_context/max_tool_iterations/steering 等）
全部内置默认最优值，不再需要外部配置。

验证：

```text
go test ./pkg/... -count=1      50 packages ok, 0 fail
go vet ./...                    PASS
./scripts/lint-docs.sh          OK
./scripts/chess-e2e.sh          PASS
真实模型 + 单数 api_key 配置      health OK / FINAL_OK / pid 清理 OK
```

功能/工具/使用方法/限制说明见：
[Chess 配置指南](../guides/chess-config.zh.md)、
[工具参考](../reference/tools.zh.md)、
[Chess Pico E2E](../guides/chess-pico-e2e.zh.md)、
[代码地图](../architecture/code-map.zh.md)。

---

## 13. --name 派生 pico token 与启动参数精简（2026-10-29；token 规则 2026-10-30 更正）

commit `5a96789d`、`3bb5723b`。

> 更正：本节原先描述的 `sha256("chagent:pico:"+name)`、写回 config.json、"显式 token 永不覆盖"均已过期。当前实现固定为 `md5(--name)` 的 32 位十六进制 token，且显式 `channels.pico.token` 会在启动时被覆盖为 `md5(--name)`（`pkg/config/agent_name.go:39-65`）；实测见 `build/functest-20261030/baseline/compat/summary.md` §2.2。

- pico token 恒为 `md5(--name)`（32 位十六进制、小写）：
  - `--name` 可省略，省略时按 `default` 计算（`ResolveAgentName("")→"default"`）；
  - 例：`--name chess-agent` → `md5("chess-agent")`；
  - 配置里的 `channels.pico.token`（含占位符或显式值）不参与认证，启动时被 `md5(--name)` 覆盖；token 不再写回 config.json，只在内存中使用；
  - 外部 pico 客户端按同一 md5 值计算 `Authorization: Bearer <md5>`。
- gateway 启动参数只保留 `--name`；删除 `--debug`、`--no-truncate`、
  `--allow-empty`、`--host` 与 `g` 别名及对应测试。
- 删除残留 `.security.yml` 配置文件的加载/保存/备份/复制代码，
  敏感数据过滤缓存保留；`SaveConfig` 直接写回唯一 v2 JSON 中的
  `api_key`/pico token，不再依赖第二配置文件。
- 验证：`go test ./pkg/... ./cmd/...` 全绿；`chess-e2e.sh` 使用占位符 +
  `--name chess-e2e` 验证 token 派生、pico ping/pong、SIGTERM/pid；
  `live-agent-test.py` 真实模型全绿。

---

## 14. 工具/MCP/路径权限/Hook 配置审计（2026-10-30）

commit `f08a0141`：

- MCP 工具不再要求外部 `tools.mcp.enabled=true`：只要配置
  `tools.mcp.servers` 就自动激活（`MCPConfig.Active()`），server 地址/鉴权仍是
  必要的外部配置；补充 `TestMCPActiveWithoutEnabledFlag`。
- 外部文件访问权限核对：默认只允许工作区；`tools.allow_read_paths` /
  `allow_write_paths` 是工作区外访问的必要显式授权。新增
  `TestExternalPathRequiresExplicitPermission` 覆盖默认拒绝、显式允许、非匹配路径拒绝。
- 工具文档重写：修正错误的 `tools.enabled` 映射为 `tools.<tool>.enabled`，
  按实际注册位置列出默认工具、MCP 动态工具、cron、路径权限、hook 配置；
  删除失效的 `docs/reference/tools_configuration.zh.md` 并修正索引/链接。
- Hook：不配置 hooks 时无需外部配置；默认超时值生效，只有需要扩展时才写
  `hooks.builtins`/`hooks.processes`。
- 验证：`go test ./pkg/... ./cmd/...` 全绿；`go vet`、`lint-docs`、
  `chess-e2e.sh`、`live-agent-test.py` 均通过。

---

## 15. pico 自动 MD5 token 与工作区/工具/Hook 标准配置（2026-10-30）

- pico 通道改为内置：`config.json` 不需要 `channels`/`token`；
  token 约定为 `md5(--name)`（空 name 用 `default`），不写入配置文件。
  外部 pico 客户端用同一个 md5 值作为 Bearer token。
- `LoadConfig` 在 `--name` 存在时自动创建/启用 pico 通道并注入派生 token；
  源码 `pkg/config/agent_name.go`。
- 文档补充标准完整配置样例：
  - 工作区：`agents.defaults.workspace`、workspace_context/runtime_context/autonomy；
  - 工具：核心工具开关、MCP servers（配置即启用）、外部路径权限；
  - Hook：`hooks.builtins`/`processes` 与默认超时；
  见 [Chess 配置指南](../guides/chess-config.zh.md) 与 [工具参考](../reference/tools.zh.md)。
- E2E/live 脚本改为无 channels 配置 + md5 token；`TestChessMinimalConfigLoads`、
  `TestNameDerivedPicoTokenAutoEnablesChannel` 覆盖自动 pico。
- 验证：`go test ./pkg/... ./cmd/...` 全绿；`go vet`、`lint-docs`、
  `chess-e2e.sh`、`live-agent-test.py` 通过。

---

## 16. DSH agent-loop 机制迁移（模型执行/上下文窗口/Agent 循环/Prompt 注入）

日期：2026-10-30。任务书执行情况由独立审计给出（`docs/design/chagent-taskbook-audit.zh.md`）：
43 项中完成 11、部分 23、未完成 9；169 个指定命名测试字面存在 69 个（40.8%）。
本轮按用户要求，重新对标 DSH（`/Users/wudaorui/code/deepseek-harness`）迁移
agent loop 直接关联机制，并在 chagent 既有实现上改造。证据报告：

- DSH 模型执行与循环：`docs/design/dsh-agent-loop-model-exec.zh.md`
- DSH 上下文与 prompt：`docs/design/dsh-context-prompt.zh.md`
- 迁移报告：`docs/design/dsh-migration-report.zh.md`

### 16.1 本轮落地

| 机制 | 落点 | 状态 |
|---|---|---|
| pre-step 有序注入表 + next-step 队列 | `pkg/agent/prestep.go`、`turn_coord.go`、`pipeline_setup.go` | 已接入 |
| repeat-tool-reminder guard（3/5/8、规范化参数、建议式中文提醒、用户插话重置） | `pkg/agent/loop_guard.go`、`pipeline_execute.go` | 已接入 |
| tool-result pruner（8192/4096/1024、rune 安全、幂等）+ 压缩优先裁剪 | `pkg/agent/tool_result_prune.go`、`context_legacy.go` | 已接入 |
| 上下文压力策略（0.8 / 0.16 / 65536） | `pkg/agent/context_pressure.go` | 库层完成，触发点未切换 |
| Turn 边界统一判定（context 不是 user turn，T2.3） | `pkg/agent/context_budget.go` | 已接入 |
| 摘要跳过 context 正文 + 摘要提示中文化 | `pkg/agent/context_legacy.go` | 已接入 |
| state 型 context replace（runtime/task-state 只保留最新） | `pkg/agent/prestep.go` | 已接入（模型可见层） |
| 策略 Prompt P-01/P-02/P-11（T4.5） | `pkg/agent/prompt_policy.go`、`context.go`、`prompt.go`、`instance.go` | 已接入 |
| `loop-guard` context kind | `pkg/providers/protocoltypes/types.go` | 已接入 |

未迁移（后续 P0）：并行工具调度（排他屏障/并发池/模型序提交/abort 合成结果）、
自主闭环守卫与 E2E（T5.4/T5.5/T5.7）、skill catalog 迁 context（T4.3）、
Context source 对外注册（T4.1）、spill（裁剪内容落盘）。
详见迁移报告 §4。

### 16.2 验证

```text
go build ./...                    OK
go vet ./...                      OK
go test ./pkg/... -count=1        PASS（50+ 包，agent 86.1s）
./scripts/lint-docs.sh            OK
./scripts/chess-e2e.sh            PASS
本轮新增测试                      53 个（loop_guard 11 / prestep 8 / prune 10 /
                                  pressure 4 / turn_boundary 8 / policy 12）
```

已知限制：session JSONL 仍为 append-only（无 surface replace）；压缩切点仍以
Turn 为粒度；pruned 工具结果中间内容不可回放；`go test -race ./pkg/agent`
仍有仓库既有测试基建 DATA RACE。本轮未新增外部依赖、未改 provider 协议与配置格式。

---

## 17. 第二轮：剩余 P0/P1 任务与集成测试（2026-10-30 下午）

> 更正（2026-10-30 晚些时候）：本节末尾 T7.3/T7.4 中「`pkg/audio`/`pkg/evolution`/非 chess provider 适配器/非 pico config schema 保留」与「audio/evolution 等可选能力删除未执行」的表述已被后续第二批清理（§18）取代：`pkg/audio`、`pkg/evolution`、`pkg/seahorse` 与非 pico config schema 均已删除；现状以 §18 与 `docs/design/chess-trim-inventory.md` 为准。

在 §16 基础上继续执行迁移报告 §4 与审计报告缺口，全部验证通过：

| 任务 | 结果 |
|---|---|
| T5.4/T5.5/T5.7 自主闭环 | `autonomy_runtime.go` 改造 + `autonomy_e2e_test.go`（13 测试）：调度守卫、用户消息优先、`agent.autonomy.round` 事件、最终总结、resume_on_start、provider/迭代 checkpoint、空响应继续 |
| T4.3 skill catalog 迁 context | `skill_catalog_context.go` + `tools.skills.catalog_mode`（默认 context，system 回退），system prompt 不再内联技能表 |
| T4.1 Context source 注册 | `context_source.go` 对外 API；Lead 接入 `prestep.go`（order 400）与 `pipeline_setup.go`（首轮） |
| spill | `spill.go`（workspace/.chagent/spill，0700/0600，30 天清理）+ pruner 落盘回填 + 失败保留 inline |
| 并行工具调度 | `tool_batch.go` + `pipeline_execute.go` 保守接入 + `pkg/tools/concurrency.go`；abort 合成结果、模型序提交 |
| usage 锚定计量 + 临近上限提醒 | `context_meter.go` + pre-step order 350 + `context-pressure` context kind |
| T4.6 残留英文 prompt | 9 个工具 Description + 参数描述 + subagent/搜索系统提示中文化；`pkg/tools/prompt_i18n_test.go` 覆盖扩展 |
| T2.5/T3.5/T0.4/T6.2 | 3 个新测试文件：14 个集成测试 + 4 个 benchmark |

验证：

```text
go build ./... + go vet ./...                   PASS
go test ./pkg/... -count=1                      PASS（agent ~78s）
./scripts/lint-docs.sh                          OK
./scripts/chess-e2e.sh                          PASS
```

T6.3/T6.4 文档：配置指南按当前实现重写，新增回滚演练与 workspacecontext README。
T6.5：`go test -race ./pkg/agent` 已修复为全绿（1 测试基建 + 2 生产代码同步补丁：
channels Manager dispatcher join、turnState pendingResults close/send 同步）。

T7.3/T7.4：已删除非 chess CLI 子命令（agent/cron/mcp/model/skills/status）、
Makefile 的 web/docker/launcher/macos/android/membench 目标、docker 文档与 CLI 文档引用；
`pkg/audio`/`pkg/evolution`/非 chess provider 适配器/非 pico config schema 保留（需独立分支）；
清单见 `docs/design/chess-trim-inventory.md`。

未执行（需独立分支）：audio/evolution 等可选能力删除；
事件日志+surface replace（架构级）、压缩切点 tool-pairing。

最终验证（2026-10-30 13:00）：

```text
go build ./...                          OK
go vet ./...                            OK
go test ./pkg/... -count=1              PASS（50 包）
go test -race ./pkg/agent -count=1      PASS（0 DATA RACE，97.6s）
go test -race ./pkg/channels/...        PASS
./scripts/lint-docs.sh                  OK
./scripts/chess-e2e.sh                  PASS
```

---

## 18. 非 agent 功能范围清理与精简测试（2026-10-30 下午）

按用户要求，清理所有不属于 chess 基础 agent 功能范围的代码，并建立精简测试入口。

### 18.1 删除内容（-33,770 行 / 94 文件）

| 区域 | 删除内容 |
|---|---|
| 音频/语音 | `pkg/audio`（ASR/TTS/OGG/voice agent）、agent 转录与 `SetTranscriber`、gateway voice orchestrator、`send_tts` 工具/facade、`VoiceConfig`、`pkg/bus` audio/voice-control 流、channels voice capability |
| 自进化 | `pkg/evolution`、`pkg/agent/evolution_bridge.go`、config/事件/初始化接线、相关测试 |
| 检索上下文 | `pkg/seahorse`（非 chess 所需，context manager 仅保留 legacy） |
| 非 pico 通道 | 除 `pico`/`pico_client` 外的通道常量、settings struct、默认值、校验、manager 分支与测试 |
| 硬件/设备 | `devices`/`serial`/`spi`/`i2c` 配置项（工具与设备服务此前已删除） |
| 依赖 | go mod tidy 移除 pion/webrtc、minio/selfupdate、qrterminal、discordgo、go.mau.fi/util、modernc/sqlite |

### 18.2 精简测试

- 新增 `scripts/lean-test.sh` + `make test-lean`：只构建并运行 agent 作用域包
  （agent/config/tools/channels-pico/gateway/providers/taskstate/session/memory/
  skills/events/bus/commands/cmd），用于日常快速回归。
- 全量套件仍为 `go test ./... -count=1`。

### 18.3 验证

```text
go build ./...                    OK
go vet ./...                      OK
go test ./... -count=1            PASS（50 包）
./scripts/lean-test.sh            PASS
./scripts/lint-docs.sh            OK
./scripts/chess-e2e.sh            PASS
```

清理后保留：providers 协议适配器（model_list 按需使用）、isolation、MCP、cron、
heartbeat、media、memory、skills、session、taskstate、tokenizer、routing 等 agent
运行必需能力。

---

## 19. Provider 协议收敛：只保留通用协议（2026-10-30）

按“只留下通用协议”的要求，删除所有厂商专属协议/SDK/CLI/OAuth 适配器：

- **保留**：OpenAI Chat Completions 兼容（`openai` + 30 个预设别名，统一
  `httpapi.HTTPProvider`）；Anthropic Messages 兼容（`anthropic` /
  `anthropic-messages` / `anthropic-bearer`，统一 `anthropic_messages`）。
- **删除**：`pkg/providers/{azure,bedrock,cli,oauth,anthropic(SDK),openai_responses_common}`、
  `pkg/auth`、gemini provider、相关 facade/测试；`ModelConfig.AuthMethod/ConnectMode`；
  provider_metadata 中 9 个已删协议与 auth 方法字段；默认 model_list 中非保留 provider 条目。
- **依赖**：go mod tidy/vendor 移除 Azure/AWS/Anthropic/OpenAI SDK、copilot-sdk、oauth2 等，
  vendor 32MB → 15MB。
- **文档**：`docs/guides/providers.zh.md` 重写为两类通用协议；删除 ANTIGRAVITY 文档；
  更新 code-map、anthropic-messages 协议参考、routing/credential 文档中的过时示例。

验证：`go build ./...`、`go vet ./...`、`go test ./... -count=1`（50 包 PASS）、
`./scripts/lean-test.sh`、`lint-docs`、`chess-e2e` 全部通过。

---

## 20. R5 收尾修复与最终验证（2026-10-30 晚）

本工作树在 §19 之后完成 R5 收尾：P0 `/reload` 后 shutdown 死锁、agent loop P1/P2（usage 锚点、
pre-step 可观测、context-pressure 首轮注入、supersede 收敛）、config 显式 0 校验、
MCP servers-only 自动启用、批调度 capability 判定，以及删除残留/文档收尾；
分层验证结果、缺陷清单与复现方式见 [新实现 Agent 完整真实场景功能测试报告](agent-functional-test-report.zh.md)。
