# Chagent 任务书执行审计与当前 Agent 机制地图

> 审计人：teammate `pico-audit`（只读审计，只新增本文件）  
> 审计对象：`/Users/wudaorui/code/chagent`  
> 审计快照：`git HEAD = 0072c43a`（2026-10-30），外加审计期间（10:58–11:39 +08）观察到的工作树未提交改动  
> 依据：[任务书](agent-workspace-context-task-book.zh.md)、[执行报告](agent-workspace-context-execution-report.md)、实际生产代码与实测 `go test`

**快照声明（重要）**：审计期间 `pkg/agent` 正被其他 teammate/Lead 并发修改。工作树在 11:25–11:39 出现了新文件 `prestep.go`、`loop_guard.go`、`context_pressure.go`、`prompt_policy.go`、`tool_result_prune.go`（均为未提交、未跟踪），并修改了 `pipeline_setup.go`、`turn_coord.go`、`turn_state.go`、`context.go`、`instance.go`、`pipeline_execute.go`、`prompt.go`、`protocoltypes/types.go`。因此：

- 本报告中标注 **「HEAD 状态」** 的判定，针对已提交的 `0072c43a`；
- 标注 **「本轮修复中」** 的项，指工作树已出现实现但尚未提交/未完成测试收敛，**不能按“已完成”记账**；
- 文件行号以 2026-10-30 11:39 的工作树为准，后续提交会使行号小幅漂移，但函数名稳定，可据此定位。

---

## 0. 审计结论摘要

### 0.1 统计（43 项任务）

| 判定 | 数量 | 任务 |
|---|---|---|
| 完成 | 11 | T0.1、T1.1、T1.2、T1.3、T1.4、T1.5、T2.1、T4.4、T5.1、T5.2、T5.3 |
| 部分 | 23 | T0.2、T0.3、T0.4、T2.2、T2.4、T3.1、T3.2、T3.3、T3.4、T4.2、T4.3、T4.6、T5.4、T5.5、T5.6、T6.1、T6.3、T6.4、T7.1、T7.2、T7.3、T7.5、T7.6 |
| 未完成 | 9 | T2.3、T2.5、T3.5、T4.1、T4.5、T5.7、T6.2、T6.5、T7.4 |

其中 **本轮修复中** 的 6 类工作（Lead 口头同步）对应关系：

| 本轮在迁移的机制 | 影响的任务 | 当前工作树证据 | 状态 |
|---|---|---|---|
| 上下文窗口/压缩 turn 边界 | T2.3、T0.4 | `context_budget.go` 尚未改动 | 未落地 |
| tool-result pruner | 任务书未列（缺口项） | `pkg/agent/tool_result_prune.go`、`tool_result_prune_test.go` | 仅库 + 单测，**未接入** LLM 请求路径 |
| context pressure 策略 | T0.4/T6.2（压缩触发策略） | `pkg/agent/context_pressure.go`、`context_pressure_test.go` | 纯函数已写，**未被 `isOverContextBudget` 使用** |
| repeat-tool guard | 任务书未列（新机制） | `pkg/agent/loop_guard.go`、`loop_guard_test.go` | 组件 + 单测，**未接入** `ExecuteTools` |
| pre-step 注入表 | T2.2/T3.3（注入顺序重构） | `prestep.go`，已接入 `turn_coord.go:195-203`、`pipeline_setup.go:40` | 已接入但未提交 |
| P-01/P-02/P-11 策略 prompt | T4.5 | `prompt_policy.go` + `context.go:291-293` 调用 + `prompt.go:29,50-52` | 已接入但**无 golden/关键短语测试**、未提交 |

### 0.2 总体判断

1. **库层（T1/T2.1/T5.1/T5.2/T5.3）质量高**：`pkg/agent/workspacecontext` 59 个测试全绿、`pkg/taskstate` + 任务工具测试全绿，命名与任务书几乎 1:1。
2. **接线层证据薄弱**：T2.2/T3.1/T3.3 的注入/触碰代码存在，但**几乎没有指定测试**；T2.3（压缩边界）、T2.5/T3.5/T5.7（集成/E2E）完全缺失。
3. **任务书要求的 169 个命名测试中，只有 69 个（40.8%）字面存在**；缺口集中在 T2/T3/T4.1–T4.3/T5.4–T5.7/T7.1–T7.2。
4. **执行报告存在明显失准/过期**（详见 §6），不能作为验收依据。
5. **代码裁剪（T7.3/T7.4）与文档（T6.3）尚未收敛**，README 的“已删除”声明超前于代码现实。

---

## 1. 审计方法与证据基线

### 1.1 判定标准

- **完成**：任务书要求的接口/行为在生产代码中完整落地，且任务书列出的测试（或等价测试）存在并实测通过。
- **部分**：生产代码核心已落地，但测试缺失、子项缺失、或与任务书明确步骤存在偏差。
- **未完成**：生产代码缺失关键实现，或只有文档/纯函数而未接入主路径。
- **未验证**：无法在本次审计中实测（需真实 API/其他仓库/长时程脚本），报告中显式标注，不做推断。

### 1.2 实测证据（命令与结果）

| 时间 | 命令 | 结果 |
|---|---|---|
| 10:59 | `go build ./cmd/chagent` | **OK**（exit 0） |
| 11:01 | `go test ./pkg/... -count=1` | **PASS**（exit 0，全部包 ok） |
| 11:01 | `go test ./pkg/agent/... -count=1` | **PASS**（agent 73.8s；workspacecontext 0.26s） |
| 11:05 | `go vet ./...` | **PASS**（exit 0） |
| 11:05 | `./scripts/lint-docs.sh` | **OK** |
| 11:18 | `go test -race ./pkg/agent -count=1` | **FAIL**：3 个 DATA RACE（T6.5 修复后 **PASS**，见 A.4） |
| 11:20 | `./scripts/chess-e2e.sh` | **PASS**（health 200 / pico ping-pong / SIGTERM / pid 清理） |
| 11:24 | `go test ./pkg/agent/workspacecontext/... ./pkg/taskstate/... ./pkg/tools/integration/... ./pkg/providers/protocoltypes/... ./pkg/providers/common/... -count=1` | **全 ok** |
| 11:24 | `go test ./pkg/agent -run 'TestInject…|TestTaskStateContext|TestBuildRuntimeSnapshot|TestAdvanceAutonomy…|TestPauseTaskState|TestModelVisiblePromptsAreChinese' -v` | 19 个用例 **全 PASS**（详见附录 A.2） |
| 11:33 | `go test ./pkg/agent -count=1`（工作树正在被并发编辑） | **FAIL**（迁移中瞬时失败） |
| 11:35 | 同上重跑 | **PASS**（73.6s） |

race 失败明细（**2026-10-30 12:57 勘误**：此前把 3 个 race 都归为“测试基建”，经根因定位只有 1 个是测试基建，另 2 个是**生产代码**的 close/send 竞争；已由 T6.5/task-12 修复，详见附录 A.4）：

- Race #2（测试基建）`TestAgentLoop_Steering_DirectResponseContinuesWithQueuedMessage`：`blockingDirectProvider.Chat` 在锁内把 `firstStarted` 置 nil（`steering_test.go:516`），测试主 goroutine 无锁读同一字段（`:1203`）；修法：字段构造后不可变 + `sync.Once`。
- Race #A（生产代码）`TestAgentLoop_HookRespond_ResponseHandledMediaPreservesOutboundContext` 等：`channels.Manager.StopAll` 的 `close(w.queue)`（`manager.go:1364`）与 `dispatchOutbound` 的 `w.queue <- msg`（`:1655`）竞争；根因是 cancel dispatcher 后未 join 就 close worker queue。
- Race #B（生产代码）`TestDeliverSubTurnResult_RaceWithFinish`：`turnState.Finish` 的 `close(pendingResults)`（`turn_state.go:832`）与 `deliverSubTurnResult` 的 send（`subturn.go:577`）竞争。

### 1.3 未验证项（显式声明）

- `scripts/live-agent-test.py`（需真实模型 API）**未运行**；执行报告 §11/§12 的真实模型结论**未独立复现**。
- `scripts/measure-startup.sh`（T7.6 体积/启动/RSS）**未运行**；报告中的 38 MB / 99 ms / 27 MB 数据**未复现**。
- chess 客户端侧 E2E（`chess/client`）**未运行**（跨仓库，且任务限定本仓库只读）。
- `go test -race` 仅跑了 `./pkg/agent`；`pkg/gateway`、`pkg/channels/pico`、`pkg/config` 的 race 结果**未验证**（执行报告称通过）。
- CONTEXT/任务书要求的“5 次 system prompt hash 记录”“benchmark 前后对比”无产出文件，**未验证**。

---

## 2. 任务书执行状态表

### 2.1 T0 组：基础能力

| 任务 | 判定 | 生产代码证据 | 测试证据 | 说明 |
|---|---|---|---|---|
| T0.1 统一 Context 元数据 | **完成** | `pkg/providers/protocoltypes/types.go:135-140`（kind 常量，另含 `loop-guard`）、`:158-166`（ContextMeta）、`:185-209`（helpers）；`pkg/providers/common/common.go:96`（`SerializeMessages` 显式挑字段） | `pkg/providers/protocoltypes/context_test.go:11,73,92`；`pkg/providers/common/context_wire_test.go:15`；实测全 PASS | wire payload 不含 `context` 已验证 |
| T0.2 配置与开关 | **部分** | `pkg/config/context_config.go:40-291`（三套结构 + defaults + Validate + ApplyContextDefaults）；`pkg/config/config.go:436-457`；`config.go:1382-1383` 调用 | 仅 `pkg/config/chess_compat_test.go:42` 顺带断言 autonomy/user_global 默认；**任务书 4 个命名测试均不存在** | 缺：`WorkspaceContext`/`RuntimeContext`/`Autonomy` 的环境变量开关（T0.2 步骤 5）；配置边界/非法值测试 |
| T0.3 事件、统计与 `/context` | **部分** | 统计已完成：`pkg/commands/runtime.go:31-75`、`pkg/commands/cmd_context.go:27,51,76,94`、`pkg/agent/agent_command.go:344-421` | `pkg/commands/cmd_context_test.go:8,26`（PASS，名称与任务书不同） | **缺事件**：`KindAgentContextInjected` / `ContextInjectedPayload` 全仓库 0 命中（`grep -rn "ContextInjected" pkg/` 为空） |
| T0.4 测试基线、golden 与基准 | **部分** | 无 system prompt golden 目录；benchmark 仅有 `pkg/agent/context_cache_test.go:744 BenchmarkBuildMessagesWithCache` | `BenchmarkBuildSystemPromptWithCache` / `BenchmarkSourceFilesChanged` / `BenchmarkBuildMessages` 均不存在；无 5 次 hash 记录、无基准数据写入执行报告 | 后续 T1.5 的 fixture（`pkg/agent/workspacecontext/testdata/`，80 KB）可复用；context-pressure 策略为本轮对 T0.4/T6.2 的补充（未接入） |

### 2.2 T1 组：工作区扫描与渲染库

| 任务 | 判定 | 生产代码证据 | 测试证据（实测 PASS） | 说明 |
|---|---|---|---|---|
| T1.1 包骨架/项目根/发现 | **完成** | `workspacecontext/discovery.go:30 FindProjectRoot`、`:60 AncestorChain`、`:97 DescendantDirs`、`:138 DiscoverBaseline`；`types.go:79-... Config` | `discovery_test.go:11,30,45,68,87,122,158` 等 7 个命名测试全在且通过 | 唯一缺项：`pkg/agent/workspacecontext/README.md` 不存在（`types.go:7-8` 仍引用 README.md 且注释仍写“not wired into the agent loop yet”，已过期） |
| T1.2 有界读取/去重/版本 | **完成** | `discovery.go:269,274 ReadBounded*`、`:313 StatVersion`、`:400 DedupeByCandidateGroup`；`digest.go:14-31` | `discovery_test.go:242,260,288,306,325,346,372,390` 8 个全在且通过 | — |
| T1.3 渲染器 | **完成** | `workspacecontext/render.go:403 RenderBaseline`、`:450 RenderChanges`、`:54 escapeFrameBody`、`:162 renderChain`（预算）、`:212 budgetNotice`、`:343 degradeFrame` | `render_test.go` 14 个命名测试全在且通过；中文文案见 `render.go:17-38` | 与 P-03..P-07 对齐，frame 闭合/UTF-8/2×maxBytes 均有测试 |
| T1.4 可见状态与 identity | **完成** | `projector.go:43 ScanVisible`、`:73,79 BaselineIdentity(WithExclude)`；`VisibleState` 定义于 `projector.go:30-38` | `state_test.go:24,52,68,105,140` 5 个全在且通过 | identity 只含发现/预算语义，无时间/内容 |
| T1.5 golden 汇总 | **完成** | `workspacecontext/testdata/golden/*.txt`（baseline/replace/empty/additional/updated/removed/budget/escape/two_chains 共 9 份）+ `testdata/workspace/`（含 `big/`、`escape/`、`packages/app|lib/`、userhome） | `golden_test.go:52-142`，`-update` flag（`golden_test.go:11-19`） | CI 不依赖绝对路径；缺“fixture token/byte 数记录”到报告 |

### 2.3 T2 组：持久上下文注入

| 任务 | 判定 | 生产代码证据 | 测试证据 | 说明 |
|---|---|---|---|---|
| T2.1 Projector.Compose | **完成** | `projector.go:173 NewProjector`、`:238 Compose`、`:341 composeBaseline`、`:431 composeDelta`；`types.go:215 ComposeResult` | `compose_test.go` 11 个测试（含 `TestComposeBudgetExclusion`）全 PASS | baseline 替换 + removal 合并、last-good、预算排除均有覆盖 |
| T2.2 SetupTurn 注入 | **部分** | `pkg/agent/pipeline_setup.go:33-35`（Compose 注入 history）→ `:40 supersedeContextMessages` → `:42 captureRestorePoint` → `:51 BuildMessagesFromPrompt`；`workspace_context_runtime.go:77 persistRuntimeContextMessage`（AddFullMessage+record+ingest） | 只有 1 个等价测试：`workspace_context_runtime_test.go:18 TestInjectWorkspaceContextPersistsDurableMessage`（PASS）；任务书 5 个命名测试全不存在 | 顺序“system → history(context) → user”成立（当前 user 在 `pipeline_setup.go:134` 持久化）；缺 Compose 失败继续、NoHistory 跳过、二次 turn 不重复的显式测试 |
| T2.3 压缩/turn 边界适配 | **未完成**（本轮修复中） | `context_budget.go:20 parseTurnBoundaries` 仍把所有 `role=="user"` 当 turn 边界（含 context 消息）；全仓库无 `isTurnBoundary`；`context_legacy.go:194-204 summarizeSession` 只按 `role user/assistant` 过滤，**会把 workspace/runtime/task-state 大段内容塞进摘要**；`trimHistoryToFitContextWindow`（`context_budget.go:122`）裁剪后不重新 Compose | 5 个命名测试全不存在 | 这是当前最严重的“任务书明确步骤未落地”项；Lead 已口头确认本轮在迁移“上下文窗口/压缩 turn 边界” |
| T2.4 Resume/替换/重复抑制 | **部分** | `ScanVisible`（`projector.go:43`）冷启动重建；identity 不匹配→`replace=true`+旧 scope removal（`projector.go:333-384`）；压缩丢 baseline→下次 `replace` | 等价覆盖：`TestComposeBaselineReplacement`、`TestComposeNoChanges`、`TestComposeBaseline`（PASS）；任务书 5 个命名测试全不存在 | 旧 session（无 context）首次注入 baseline 逻辑成立（`visible.Seen=false → replace=false`），但“重启/resume 不重复”无专门测试 |
| T2.5 集成测试 | **未完成** | 无 `pkg/agent/context_workspace_test.go`；无任何端到端注入顺序测试 | 0 | 场景 1–6 均未覆盖（尤其是“provider 请求体不含 context”在 agent 层无断言，仅 common 层有） |

### 2.4 T3 组：增量刷新

| 任务 | 判定 | 生产代码证据 | 测试证据 | 说明 |
|---|---|---|---|---|
| T3.1 工具触碰记录 | **部分** | `workspace_context_runtime.go:30 recordContextTouch`、`:61 consumeContextTouches`；调用点 `pipeline_execute.go:728`（正常工具路径）与 `:332`（**HookActionRespond 路径**） | **0 个测试**（任务书 6 个命名测试全缺） | 偏差 1：任务书要求 hook `respond` **不记录**，代码在 `:332` 记录了；偏差 2：相对路径按 `ts.workspace` 拼接正确，但无去重/失败忽略的测试证明 |
| T3.2 嵌套 scope 对账 | **部分** | `projector.go:868 touchedSpecs` → `DescendantDirs`（`discovery.go:97`）；预算排除 `projector.go:584 markExcluded`、`:275-280` touched 重新启用 | 等价覆盖：`TestComposeAdditionalOnTouch`、`TestComposeBudgetExclusion`（PASS）；6 个命名测试 0 个存在 | 多文件/工作区外/删除文件/嵌套重复无专门测试 |
| T3.3 迭代间注入 | **部分** | 已重构为有序 pre-step 表：`turn_coord.go:195-203` → `prestep.go:81 runPreStepContextSources`（workspace 100 → runtime 200 → task_state 300 → loop_guard 400）；steering 注入在 `turn_coord.go:152-182` 先于 context | 0 个命名测试；`prestep_test.go:17 TestPreStepContextSourcesOrder` 只测顺序常量表白盒 | 偏差：`NoHistory` 轮**只** flush loop-guard 消息（`turn_coord.go:193-195`），任务书要求 context 仍可 ephemeral 注入；Compose 失败“清空 touches”无测试 |
| T3.4 子任务/steering/生命周期 | **部分** | SubTurn 独立 `turnState`/session（`subturn.go:269 spawnSubTurn`、`:616 ephemeralSessionStore`）；steering 先于 context 顺序成立；`consumeContextTouches` 保证触碰不跨 turn | `subturn_test.go`/`steering_test.go` 有大量独立测试，但**无 context 专属测试**（4 个命名测试 0 个存在） | NoHistory subagent 不持久化 context 成立（`ts.opts.NoHistory` 分支），但也**不注入 ephemeral context**；stop 时 touches 随 turnState 丢弃，无显式清理/测试 |
| T3.5 测试与基准 | **未完成** | 无 context touch/refresh 测试文件、无 benchmark | 0 | 8 个场景无覆盖；无“无变化只 stat”benchmark |

### 2.5 T4 组：Prompt、运行时快照与工具

| 任务 | 判定 | 生产代码证据 | 测试证据 | 说明 |
|---|---|---|---|---|
| T4.1 Context source 注册扩展 | **未完成** | `prompt.go:133 PromptContributor` 只产出 **system** PromptPart；无 `ContextSource`/`ContributeContext`/`RegisterContextSource`/`Delivery`（grep 全空）；prestep 表（`prestep.go:45`）是内部固定表，**不对外开放注册** | 0 | 插件/hook 仍无法贡献 context；这是 T4.1 的实质缺口，prestep 不替代它 |
| T4.2 运行时上下文快照 | **部分** | 已落地：`runtime_context.go:20 BuildRuntimeSnapshot`、`:56 RuntimeSnapshotDigest`；`workspace_context_runtime.go:162 injectRuntimeContext`（digest+interval bucket 去重）；snapshot 模式下 system 不含 dynamic（`context.go:967` 按 `runtimeSnapshotMode()` 分支）；`instance.go` 注入 mode/interval/sender | `runtime_context_test.go:16 TestBuildRuntimeSnapshotChinese`、`:33 TestRuntimeSnapshotDigestIgnoresTime`、`:43 TestSnapshotModeKeepsRuntimeOutOfSystemPrompt`、`workspace_context_runtime_test.go:65 TestInjectRuntimeContextSnapshotPersistsAndDeduplicates` 全 PASS；5 个命名测试中 2 个有等价覆盖（`RuntimeSnapshotOnlyOnChange`→`TestInjectRuntimeContextSnapshotPersistsAndDeduplicates`；`SystemPromptStableAcrossTurns`→`TestSnapshotModeKeepsRuntimeOutOfSystemPrompt` 部分覆盖），缺 `TestRuntimeSnapshotTimeZone`/`TestRuntimeModeSystemFallback`/`TestEstimateSystemTokensSnapshot` | 偏差：`EstimateSystemTokens` 仍硬编码 `dynamicContextChars = 300`（`context.go:524`），任务书步骤 7 要求去掉该假设。**执行报告称“渲染器未单独落地、当前时间仍在 system prompt”是过期结论** |
| T4.3 技能渐进披露 | **部分** | `skill` 工具已实现并注册：`pkg/tools/integration/skill_load.go:25,29,48`、`instance.go:164-168`（受 `tools.skill_load` 控制）；正文 `<skill_content>` 转义 | **0 个测试**（6 个命名测试全缺） | catalog 仍在 system prompt（`context.go` 中 `capability.skill_catalog`，T1.3 段），没有 `skills.catalog_mode` 配置；任务书“渐进披露 catalog”的目标未达成 |
| T4.4 `/context` 诊断 | **完成** | `commands/cmd_context.go:27 formatContextStats` + `:51/76/94` 三段中文输出（路径/字节/预算/省略/截断、runtime 快照时间、任务 phase/round/todos）；`agent_command.go:344-421` 填充 | `commands/cmd_context_test.go:8 TestFormatContextStatsChineseSections`、`:26 TestFormatContextStatsNoWorkspace`（PASS） | 不输出指令正文；无 context 时显示“未加载/已禁用” |
| T4.5 Prompt 文本接入与 golden | **未完成**（本轮修复中） | HEAD 无 P-01/P-02/P-11；工作树已有 `prompt_policy.go:14-30`（自主运行/工作区指令/完成策略中文文本）+ `prompt.go:29,50-52`（policy slot/source）+ `context.go:291-293` 调用 | 无 golden、无关键短语/开关关闭断言 | 未提交、未跑测试；T4.5 验收（G-08/G-11 golden、关闭开关无新增）尚不成立 |
| T4.6 英文 Prompt 中文化 | **部分** | 核心 prompt 已中文（`context.go:161-201` identity/rules、skills/memory/output/summary 文案）；工具描述 17/26 中文 | `prompt_i18n_test.go` 3 个测试 PASS | **仍有 9 个工具 Description 为英文**：`pkg/tools/search_tool.go`（2 个 tool_search）、`pkg/tools/delegate.go:41`、`pkg/tools/integration/web.go:1927,2077`（web_search/web_fetch，默认启用）、`tts_send.go`、`reaction.go`、`fs/load_image.go`、`fs/send_file.go`；另有英文模型可见文本 `pkg/tools/subagent.go:181`（subagent system prompt）、`integration/web.go:1107`（搜索助手 prompt） |

### 2.6 T5 组：自主连续性

| 任务 | 判定 | 生产代码证据 | 测试证据 | 说明 |
|---|---|---|---|---|
| T5.1 TaskState 存储 | **完成** | `pkg/taskstate/types.go`、`transitions.go:78,90,111,135,154,232,243,250,255,268,273,278`；`pkg/memory/taskstate.go:15,35`（JSONL meta + 原子写） | `taskstate_test.go` 10 个 + `pkg/memory/taskstate_test.go:23,106` + `task_tools_test.go:596`，全部 PASS | `SetTaskState` 走 `fileutil.WriteFileAtomic`，旧 meta 无字段返回 false |
| T5.2 任务工具 | **完成** | `pkg/tools/integration/task_get.go`、`task_update.go`、`task_tools.go`、`todo_write.go:88`（MarkProgress）、`task_authority.go`（来源判定）；注册 `instance.go:135-154` | `task_tools_test.go` 15 个测试（含 start 需人类、complete 需证据、block 阈值、todo 整表/重复/并行策略、会话所有权、store 不支持）全 PASS | 描述/参数为中文并与 P-12/P-13 对齐（`task_update.go` 顶部） |
| T5.3 TaskState context | **完成** | `taskstate_context.go:75,98 RenderTaskState(WithOptions)`、`:161 escape`、`:170 truncate`、`:187 TaskStateDigest`、`:222 TaskStateChanged`；注入 `workspace_context_runtime.go:124 injectTaskStateContext`（内容相同不重复） | `taskstate_context_test.go` 9 个测试 PASS（active/blocked/paused/complete/escape/truncate/evidence/仅变化/普通聊天无） | 命名与任务书略有差异（`DisabledForPlainChat`）；缺 `AfterCompaction` 显式测试，但实现从 session meta 重渲染，不依赖 history 旧消息 |
| T5.4 自主续跑驱动 | **部分** | `autonomy_runtime.go:64 maybeScheduleAutonomyRound`（async goroutine `:126-141`）、`:19 renderTaskRoundPrompt`（P-10 中文）、`:30 advanceAutonomyRound`；调用点 `agent.go:644`；options 设 `SendResponse=false/SendToolFeedback=true/TurnSource=autonomy` | 仅 `runtime_context_test.go:63,84` 两个 round 守卫测试 PASS；**任务书 9 个命名测试 0 个存在**；全仓库无 `maybeScheduleAutonomyRound` 的测试调用 | 缺口：①调度前未检查 steering/pending 用户消息队列；②`send_intermediate` 仅有配置字段、无实现；③complete/blocked 轮**不发送最终总结**（`SendResponse=false` 恒定）；④无 `KindAgentAutonomyRound` 事件；⑤reserve 后立即持久化，无 commit 语义；⑥“同一 session 只有一个待执行 round”仅靠 `getActiveTurnState` 检查，存在竞态窗口 |
| T5.5 停止/进度/错误守卫 | **部分** | 空响应续跑：`turn_coord.go:273-277`（active task 时注入 task_round 再继续）；无进展/轮次上限：`autonomy_runtime.go:30-58`（`SetNoProgressBlocked`/`SetRoundLimitBlocked`）；blocker 3 轮阈值：`taskstate/transitions.go:135,154`；用户 stop→paused：`agent_stop.go:80` + `autonomy_runtime.go:141` | `TestAdvanceAutonomyRoundNoProgressBlocks`、`TestAdvanceAutonomyRoundProgressContinues`、`TestPauseTaskStateForStop`（PASS）；8 个命名测试 0 个存在 | 缺口：①max_tool_iterations 耗尽仍返回 `toolLimitResponse` 作为最终答复（`turn_coord.go:295`），无 checkpoint；②provider 错误：`maybeScheduleAutonomyRound` 对 `TurnEndStatusError` 直接返回（`autonomy_runtime.go:75`），无 checkpoint/恢复消息；③无进展判定只看 `LastProgressAt`（仅 todo_write/task_update 会 MarkProgress），**不统计成功副作用工具**，3 轮真实文件改动也会被误判无进展；④`resume_on_start` 仅有配置字段（`context_config.go:190`），无任何调用点；⑤停止原因仅日志，无事件/最终消息 |
| T5.6 Heartbeat/持久 session | **部分** | `agent_message.go:70-112 ProcessHeartbeat`：`sessionKey="autonomy:heartbeat"`，仅 `HEARTBEAT_OK` 走 `NoHistory=true`，其余持久会话；`TurnSource="scheduler"` | **0 个测试**（5 个命名测试全缺） | 缺口：HEARTBEAT.md→TaskState 映射、已有 active/paused/blocked 的继续/不恢复策略、独立配置项、`source=heartbeat` 事件标注、与主会话竞争保护 |
| T5.7 自主 E2E | **未完成** | 无 `pkg/agent/autonomy_e2e_test.go` | 0 | 9 个场景（空响应→第二轮→完成、max iterations→checkpoint、重启恢复、压缩后重建、stop、3 轮无进展、round limit、complete 无证据、autonomy off）全无端到端覆盖 |

### 2.7 T6 组：加固、文档与发布

| 任务 | 判定 | 生产代码证据 | 测试证据 | 说明 |
|---|---|---|---|---|
| T6.1 安全加固 | **部分** | frame 转义 `render.go:54`；UTF-8 边界 `render.go:59`；读取上限 `discovery.go:269/274`；符号链接 `discovery.go:328-379`；候选名校验 `context_config.go:117-130`；TaskState 转义/截断 `taskstate_context.go:161,170`；预算 notice 用 display path（`render.go:212`） | 分散覆盖：`TestRenderEscapeClosingTag`、`TestTaskStateContextEscapesClosingTag`、`TestSymlink*`、`TestReadBoundedTooLarge`、`TestTaskBlockThreshold` 等 PASS | 无 T6.1 专项测试矩阵；“日志不输出正文”未系统验证（抽样：agent 日志多为长度/路径） |
| T6.2 性能与缓存 | **未完成**（本轮 context pressure 属补充） | 仍是旧的全局 `maxMtime` 失效策略 + 递归 `WalkDir`：`context.go:616 buildCacheBaseline`、`:670 sourceFilesChangedLocked`、`:694 skillFilesChangedSince`（旧上下文文件行号）；未改逐文件 `FileVersion{Size,ModTime,Inode}` | 无 benchmark 前后对比；无“无变化不读内容/不 WalkDir”断言 | 本轮的 `context_pressure.go` 只是阈值策略，**不改**缓存失效模型，不能视为 T6.2 完成 |
| T6.3 文档与配置参考 | **部分** | `README.md` 已重写为 chess 基础 agent；`docs/architecture/workspace-context.zh.md`、`runtime-context.zh.md`、`taskstate-autonomy.zh.md`、`code-map.zh.md` 等存在；`docs/guides/chess-config.zh.md`、`configuration.zh.md` 含默认值表 | `./scripts/lint-docs.sh` OK | 缺：`pkg/agent/workspacecontext/README.md`；`docs/reference/configuration.md`（现为 guides/configuration.zh.md）；**文档与代码漂移**：`docs/guides/configuration.zh.md:15-168` 仍写 `CHAGENT_*` 环境变量与 `chagent agent` 子命令，代码已不支持/已删除（见 §6） |
| T6.4 默认值与回滚演练 | **部分** | 默认值即 chess profile：`config/context_config.go:52-63,143-149,197-207`；回退开关可用（`workspace_context.enabled=false`、`runtime_context.mode=system`、`autonomy.enabled=false`）| 仅 `TestChessMinimalConfigLoads` 断言部分默认值；无回滚演练测试/记录 | 无“关闭全部新功能”的启动/对话/停止演练，`docs/chess-integration.md` 不存在（回滚步骤无文档承载） |
| T6.5 最终验证 | **未完成** | — | build/test/vet/lint-docs 通过（见 §1.2），但 `go test -race ./pkg/agent` FAIL（3 races）；E2E 不在 `go test` 内；benchmark 对照缺失 | §11 矩阵中“目标包 -race 全绿”“benchmark 对照”“T6.5 全清单”不成立；另有“手动场景 1–13”无记录 |

### 2.8 T7 组：Chess 兼容、最小配置与裁剪

| 任务 | 判定 | 生产代码/测试证据 | 实测 | 说明 |
|---|---|---|---|---|
| T7.1 Chess 兼容基线 | **部分** | pid 单例：`pkg/pid/process_name.go` + `process_name_test.go:50 TestSingletonRecognizesChagentProcessName`（存在）；`pidfile_test.go` 20+ 测试；pico/gateway 单测存在 | `chess-e2e.sh` **PASS**（health 200 port=58798、config 未被改写、pico ping/pong、SIGTERM、pid 清理） | **缺 `integration/chess/compat_test.go`（根 `integration/` 下只有 docker/mcp 套件）**；无 `integration/chess/BASELINE.md`；gateway 测试未覆盖启动契约（`gateway_test.go` 仅启动失败/关闭/bus） |
| T7.2 最小配置与默认值 | **部分** | `config/minimal.go:214-236 defaultWorkspaceChain`（CHAGENT_WORKSPACE→config 目录→cwd→home）；`config.go:1397-1401 inferDefaultModel/Provider`；`config/agent_name.go` md5 pico token；`chess_compat_test.go:42` | `TestChessMinimalConfigLoads`、`TestPicoChannelAutoEnabledWithoutConfig`、`TestNameDerivedPicoTokenAutoEnablesChannel`、`TestSingletonRecognizesChagentProcessName` PASS | 与任务书冲突（且执行报告 §12 已记录为“最终决策”）：legacy `CHAGENT_*` 别名被删除（grep `CHAGENT_` 于 Go 代码为 0），`channel_list`/v2 key-style 兼容被删除；14 个命名测试仅 1 个字面存在 |
| T7.3 代码裁剪 | **部分** | 已删：telegram/wecom/weixin 通道目录、`pkg/devices`、`pkg/tools/hardware*`、updater、migrate、cmd/membench；**未删**：`pkg/audio`、`pkg/evolution` + `agent/evolution_bridge.go`、全部 provider 适配器（anthropic/azure/bedrock/cli/httpapi/oauth…）、CLI 6 个非 gateway/version 子命令（`cmd/chagent/main.go:109-118` 注册 agent/status/cron/mcp/skills/model）、15 个通道配置 schema（`config/defaults.go:533-615 defaultChannels`）、`Devices`/`Voice` 配置 | 构建 OK | `docs/design/chess-trim-inventory.md` 过期：已删项仍标 `[ ]`，基线/前后体积/依赖数/包数全为“待填”；无 before/after 数据 |
| T7.4 前端/文档/脚本裁剪 | **未完成** | `assets/`、`web/`、`docker/`、`examples/` 目录已不存在；但 `Makefile` 仍含 `build-launcher`、`build-all`、`docker-*`、`build-macos-app`、`mem`（membench）等目标；`docs/` 仍有 operations/security/hooks/providers/docker 等非 chess 文档 | `lint-docs` OK | 验收“Makefile 只保留 build/test/lint/chess-e2e”“无指向已删目录的脚本/CI”不成立；README 的“多通道/Web/硬件已删除”与 CLI/config 现实不符 |
| T7.5 Chess 客户端 E2E | **部分** | `scripts/chess-e2e.sh`（心脉：minimal v2 config、md5 token、health、pico WS ping/pong、SIGTERM、pid）| **PASS**（见 §1.2） | 缺 `integration/chess/e2e_test.go`；脚本未覆盖“重启 client 后重新启动 agent”“改写 config 后重启生效”“无孤儿进程/诊断日志最后 50 行”部分断言 |
| T7.6 体积/内存/启动验收 | **部分** | `scripts/measure-startup.sh` 存在（构建、pid、health、RSS） | **未运行**（未验证） | 无 `integration/chess/BASELINE.md`；执行报告 §9 只有单次/两次数字，无 5 次中位数、无裁剪前基线对照、无 goroutine/ticker 对比 |

---

## 3. 当前 Chagent Agent 执行机制地图（file:line，快照 2026-10-30 11:39）

### 3.1 入口与主循环

- `pkg/agent/agent.go:146 AgentLoop.Run`：worker pool（`agent_init.go:61` 用 `MaxParallelTurns`，默认 20，`config/defaults.go:37`）。
- `pkg/agent/agent.go:530 runAgentLoop`：规范化 options → turn profile 解析 → session 元数据 → `newTurnState`（`:568`）→ `NewPipeline`（`:569`）→ `runTurn`（`:570`）；turn 结束后处理 followUps（`:578`）、outbound 发布（`:600-628`）、**`maybeScheduleAutonomyRound`（`:644`）**。
- `pkg/agent/turn_coord.go:17 runTurn`：注册 active turn、turn start 事件、`pipeline.SetupTurn`（`:81`）、迭代循环（`:92`）、steering 注入（`:152-182`）、**pre-step 上下文刷新（`:195-203`）**、`CallLLM`（`:211`）、`ExecuteTools`（`:245`）、`Finalize`（`:238/279/307`）；空响应且任务 active 时注入 task_round（`:273-277`）；迭代上限兜底 `toolLimitResponse`（`:295`）。

### 3.2 turn 生命周期 / pipeline

- `pipeline.go:28 NewPipeline`；`pipeline_setup.go:16 SetupTurn`（Assemble → 注入 workspace/runtime/task_state → `supersedeContextMessages`（`:40`）→ capture restore point（`:42`）→ `BuildMessagesFromPrompt`（`:51`）→ 主动压缩（`:61-131`）→ 持久化当前 user（`:134`）→ 选模/候选）。
- `pipeline_llm.go:22 CallLLM`（fallback/retry、context-length 错误、streaming 分支 `pipeline_streaming.go:17 tryConfiguredStreamingLLM`、`:676 model rewrite）。
- `pipeline_execute.go:112 ExecuteTools`：顺序执行 tool calls（**无并行工具执行**）；BeforeTool/Approve/AfterTool hooks；结果持久化（`:336/731`）；**触碰记录 `:332`（hook respond）、`:728`（正常）**；steering 注入到 `exec.pendingMessages`（`:346/746`）。
- `pipeline_finalize.go:16 Finalize`；`turn_state.go:161 newTurnExecution`、`:274 newTurnState`、`:664 captureRestorePoint`、`:671 recordPersistedMessage`、`:698 ingestMessage`、`:713 restoreSession`；`turn_state.go:254 contextTouches`、`:261 pendingStepContexts`（工作树新增）。

### 3.3 ContextManager 与压缩

- 接口/注册：`context_manager.go:15 ContextManager`（Assemble/Compact/Ingest/Clear）、`:34 AssembleRequest`、`:47 CompactRequest`、`:70 RegisterContextManager`、`:88 lookupContextManager`；解析：`turn_coord.go:362 resolveContextManager`。
- legacy：`context_legacy.go:23 Assemble`、`:39 Compact`、`:77 maybeSummarize`（异步）、`:115 forceCompression`（丢最旧 ~50% turns）、`:172 summarizeSession`、`:262 findNearestUserMessage`、`:285 retryLLMCall`、`:324 summarizeBatch`、`:384 estimateTokens`。
- seahorse（可选）：`context_seahorse.go:26 newSeahorseContextManager`、`:90 Assemble`、`:127 Compact`、`:197 providerToSeahorseMessage`、`:248 seahorseToProviderMessages`、`:289 init()` 注册 `"seahorse"`。
- 预算/裁剪：`context_budget.go:20 parseTurnBoundaries`、`:33 isSafeBoundary`、`:44 findSafeBoundary`、`:102 isOverContextBudget`（messages+toolDefs+maxTokens > window）、`:122 trimHistoryToFitContextWindow`、`:152 nextHistoryTrimStart`。
- 用量：`context_usage.go:17 computeContextUsage`（`/context` 与 outbound 用）。
- 本轮工作树新增（未接入）：`context_pressure.go`（DSH 80%/16%/65536 阈值模型）、`tool_result_prune.go`（8192/4096/1024 字符中段剪除）。

### 3.4 上下文装配（ContextBuilder）

- `context.go:25 ContextBuilder`；`context.go:102 WithRuntimeContextMode`、`:107 runtimeSnapshotMode`；`:249 BuildSystemPromptParts`、`:263 buildSystemPromptParts`（identity → **policy（本轮新增 `:291-293`）** → bootstrap → skills → memory → output → contributors）、`:388 BuildSystemPromptWithCache`、`:428 buildSystemPromptForRequest`；`:519 EstimateSystemTokens`（仍含 `dynamicContextChars=300`）；`:616 buildCacheBaseline`、`:670 sourceFilesChangedLocked`、`:694 skillFilesChangedSince`（旧失效模型）；`:791 LoadBootstrapFiles`；`:848 buildDynamicContext`（仅 `mode=system`）；`:866 BuildMessages`、`:894 BuildMessagesFromPrompt`（单 system message + history + current user；snapshot 模式跳过 dynamic）、`:1077 sanitizeHistoryForProvider`（tool-call 序列修补）。
- tokenizer：`pkg/tokenizer/estimator.go:19 EstimateMessageTokens`（CJK 加权、SystemParts/Reasoning/ToolCalls/Media）、`:86 EstimateToolDefsTokens`、`:111 CJKRuneCount`、`:141 weightedTokenEstimate`。

### 3.5 Prompt registry / contributors

- `prompt.go:99 PromptPart`、`:133 PromptContributor`、`:138 PromptRegistry`、`:145 NewPromptRegistry`、`:161 builtinPromptSources`、`:306 RegisterSource`、`:324 RegisterContributor`、`:346 Collect`、`:371 ValidatePart`、`:419 NewPromptStack`、`:458 renderPromptPartsLegacy`；`:29 PromptSlotPolicy`、`:50-52` 三个 policy source（本轮新增）。
- contributors：`prompt_contributors.go:16/26 toolDiscovery`、`:63/73 mcpServer`、`:118/128 agentDiscovery`。
- turn 侧请求：`prompt_turn.go:11 promptBuildRequestForTurn`、`:56 turnProfileNativeSearchCallable`、`:74 promptBuildRequestForProcessOptions`、`:119 promptOverlaysForOptions`、`:162 promptMessageWithMetadata`。
- policy 文本（本轮工作树）：`prompt_policy.go:14-30`（P-01 自主运行 / P-02 工作区指令 / P-11 完成策略），`:76 ContributePrompt`，`:114` slot=policy。

### 3.6 持久上下文注入（workspace / runtime / taskstate）

- `workspace_context_runtime.go:30 recordContextTouch`、`:61 consumeContextTouches`、`:77 persistRuntimeContextMessage`、`:89 injectWorkspaceContext`（Compose + 持久化）、`:110 injectWorkspaceContextDelta`、`:124 injectTaskStateContext`、`:162 injectRuntimeContext`。
- `runtime_context.go:20 BuildRuntimeSnapshot`（中文 `<runtime_context>`，时间/平台/工作区/会话/发送者）、`:56 RuntimeSnapshotDigest`（不含时间）。
- `taskstate_context.go:75 RenderTaskState`、`:98 RenderTaskStateWithOptions`（P-09，objective 500 runes、todo 200、evidence 最近 5 条）、`:187 TaskStateDigest`、`:222 TaskStateChanged`。
- 有序表（本轮工作树）：`prestep.go:26-31` 顺序常量（workspace 100 → runtime 200 → taskstate 300 → loop_guard 400）、`:45 builtinPreStepContextSources`、`:81 runPreStepContextSources`、`:103 enqueueStepContextMessage`、`:126 flushStepContextMessages`、`:173 supersedeContextMessages`。

### 3.7 taskstate / autonomy

- `pkg/taskstate/types.go`（State/Todo/BlockedReason/Phase/Store + 校验）、`transitions.go`（Pause/Resume/Complete/CanBlock/RecordBlockerAttempt/MarkProgress/RecordNoProgressRound/AdvanceRound/ShouldContinue/RoundLimitReached/Set*Blocked）。
- 持久化：`pkg/memory/taskstate.go:15 GetTaskState`、`:35 SetTaskState`（JSONL meta + 原子写）。
- 工具：`pkg/tools/integration/task_get.go`、`task_update.go`（start 需人类/调度器；complete 需 evidence；block 需 3 轮同指纹）、`todo_write.go`（整表替换、并行 in_progress 策略、MarkProgress `:88`）、`task_authority.go`。
- 驱动：`autonomy_runtime.go:19 renderTaskRoundPrompt`（P-10）、`:30 advanceAutonomyRound`（轮次上限/无进展 → blocked）、`:64 maybeScheduleAutonomyRound`（条件、异步、`TurnSource=autonomy`）、`:141 pauseTaskStateForStop`、`:167 activeTaskState`、`:184 appendTaskContinuation`、`:199 turnSourceFromString`；调用点 `agent.go:644`；stop 集成 `agent_stop.go:80`。
- heartbeat：`agent_message.go:70-112`（`autonomy:heartbeat` 持久会话，`HEARTBEAT_OK` 才 NoHistory）。

### 3.8 subturn / steering

- subturn：`subturn.go:214 SpawnSubTurn`、`:245 NewSubTurnSpawner`、`:251 SpawnSubTurn`、`:269 spawnSubTurn`（独立 child ctx、并发信号量、depth 限制）、`:537 deliverSubTurnResult`、`:616 ephemeralSessionStore`（`:639-704` 内存会话、`:704 truncateLocked` 软截断）。
- steering：`steering.go:34 parseSteeringMode`、`:51 newSteeringQueue`、`:67 push`、`:87 dequeue`、`:102 dequeueScopeWithFallback`、`:188 Steer`、`:198 enqueueSteeringMessage`、`:284 dequeueSteeringMessagesForScope`、`:305 clearSteeringMessagesForScope`、`:312 continueWithSteeringMessages`、`:372 Continue`、`:424 InterruptGraceful`、`:449 InterruptHard`；注入消费点 `turn_coord.go:152-182`。
- 本轮新增（未接入）：`loop_guard.go`（重复工具调用守卫，阈值 3/5/8，排除 todo/task 工具，中文提醒），`prestep.go:98` 的队列即其投递通道，但 `enqueueStepContextMessage` / `repeatToolGuard.Observe` 目前**无生产调用点**（仅测试）。

### 3.9 工作区扫描/渲染库（`pkg/agent/workspacecontext`）

- `discovery.go:30 FindProjectRoot`、`:60 AncestorChain`、`:97 DescendantDirs`、`:138 DiscoverBaseline`、`:269/274 ReadBounded`、`:313 StatVersion`、`:400 DedupeByCandidateGroup`。
- `render.go:54 escapeFrameBody`、`:59 truncateUTF8`、`:162 renderChain`、`:212 budgetNotice`、`:243 renderContextFrame`、`:403 RenderBaseline`、`:450 RenderChanges`。
- `projector.go:43 ScanVisible`、`:73/79 BaselineIdentity`、`:173 NewProjector`、`:238 Compose`、`:341 composeBaseline`、`:431 composeDelta`、`:740 probe`、`:769 loadContent`、`:827 collectSpecs`、`:868 touchedSpecs`。
- `types.go`（Config/Candidate/FileVersion/Rendered/Stats/ComposeResult）；`digest.go`（Trimmed/Content digest）。

---

## 4. 缺口清单 Top10

按“影响验收/风险”排序；标 **本轮修复中** 的表示 Lead 已经在工作树推进，审计判定仍按 HEAD 计。

1. **T2.3 压缩/turn 边界适配未实现**（P0，**本轮修复中**）：`parseTurnBoundaries` 把 context 消息当真实 user turn；摘要不跳过 context 正文；裁剪后不重建 baseline/TaskState。风险：压缩把 workspace 指令和任务状态塞进摘要或误切，导致模型上下文错乱/膨胀。
2. **T5.4/T5.5 自主闭环缺口**（P0）：无 steering/pending 用户消息守卫（自主轮可能压过用户新消息）、complete/blocked 不发送最终总结、`send_intermediate` 无实现、无 `KindAgentAutonomyRound` 事件、provider 错误无 checkpoint、`resume_on_start` 未实现、max_tool_iterations 仍以 `toolLimitResponse` 收尾。用户可见后果：“任务未完成却静默结束”或“完成后聊天窗口没有结论”。
3. **T2.5/T3.5/T5.7 集成、E2E、benchmark 全缺**（P0）：注入顺序、二次 turn 不重复、resume、压缩重建、触碰增量、自主 9 场景、无变化只 stat 等无任何端到端验证；当前只有库层单测。
4. **T4.1 context source 注册扩展未实现**（P1）：插件/hook 只能贡献 system part，不能贡献 durable context；prestep 表是内部固定表（`prestep.go:45`），不构成对外契约。
5. **T4.5 P-01/P-02/P-11 策略 prompt 与 golden 缺失**（P0/P1，**本轮修复中**）：HEAD 无这三个 system part；工作树 `prompt_policy.go` 已写但无 golden/开关关闭断言，未提交。
6. **T4.3 skill 渐进披露只做了一半**（P1）：`skill` 工具存在且默认启用，但 catalog 仍在 system prompt、无 `skills.catalog_mode`、工具零测试（`skill_load.go` 无 `_test.go`，全仓库无引用测试）。
7. **T7.1/T7.5 无 `integration/chess` 测试，E2E 只有 shell 脚本**（P0/P1）：`chess-e2e.sh` 实测通过，但 go test 矩阵里没有启动/健康/pico/SIGTERM/重启/配置刷新断言，也没有 `BASELINE.md`。
8. **T7.3/T7.4 裁剪与 README 不符**（P1）：CLI 仍注册 6 个非 gateway/version 子命令（`cmd/chagent/main.go:109-118`）、config 仍有 15 通道 schema（`config/defaults.go:533-615`）与 Devices/Voice、`pkg/audio`/`pkg/evolution` 未删、Makefile 仍含 web/docker/membench/macos 目标；`chess-trim-inventory.md` 未更新（已删项仍空勾）。
9. **T6.2 缓存优化未做 + T0.4/T6.2 benchmark 数据缺失**（P1）：仍用全局 `maxMtime` + 递归 `WalkDir`（`context.go:616/670/694`），任务书要求的逐文件 `FileVersion` 快照与“无变化不读内容/不 WalkDir”未实现；报告无前后对比数据。**本轮修复中**（context pressure 仅阈值策略，不解决缓存模型）。
10. **T4.6 英文模型可见文本残留 + T6.3 文档漂移 + T6.4 回滚演练缺失**（P2）：9/26 工具 Description 为英文（web_search/web_fetch 默认启用）、`subagent.go:181` 与 `web.go:1107` 英文提示词；`docs/guides/configuration.zh.md` 仍写 `CHAGENT_*`/`chagent agent`，与代码不符；无回滚演练记录。

> 附：**race issue（已修复，2026-10-30 12:57）**：`go test -race ./pkg/agent` 的 3 个 DATA RACE 中 1 个来自测试辅助代码（`steering_test.go` mock 字段），2 个来自生产代码（`channels.Manager` 关闭顺序、`turnState.pendingResults` close/send）；已按 T6.5 修复并实测全绿，详见附录 A.4。

---

## 5. 测试覆盖矩阵摘要（机制 ↔ 测试 ↔ 实测）

| 机制 | 测试文件/函数（存在的） | 实测 | 缺口 |
|---|---|---|---|
| Context 元数据/线格式 | `protocoltypes/context_test.go`、`providers/common/context_wire_test.go` | PASS | provider 逐适配器审计仅 common 一处 |
| 配置默认值/边界 | `config/chess_compat_test.go`、`config_test.go`（部分） | PASS | T0.2 4 个命名测试、回滚演练 |
| 发现/读取/去重/渲染/可见状态 | `workspacecontext/*_test.go`（59 个） | PASS | README 缺失 |
| Compose baseline/delta | `workspacecontext/compose_test.go`（11） | PASS | — |
| SetupTurn 注入 | `agent/workspace_context_runtime_test.go:18`（1 个） | PASS | 4 个命名测试、顺序/失败/NoHistory |
| 压缩/turn 边界 | `context_budget_test.go`（旧语义） | PASS（旧行为） | **T2.3 5 个测试全缺** |
| 触碰/增量刷新 | —（仅 `compose_test` 间接触及） | — | **T3.1/T3.2/T3.3 命名测试 0 个** |
| Runtime snapshot | `agent/runtime_context_test.go`（3）+ `workspace_context_runtime_test.go:65` | PASS | TimeZone/SystemFallback/Estimate |
| Prompt registry/contributors | `prompt_test.go`（10+）、`prompt_i18n_test.go`（3） | PASS | policy prompt golden（本轮新增无测试） |
| Conversation context stats | `commands/cmd_context_test.go`（2） | PASS | 事件 payload 不存在 |
| TaskState schema/transitions/store | `taskstate_test.go`（10）、`memory/taskstate_test.go`（2） | PASS | — |
| 任务工具权限/阈值 | `tools/integration/task_tools_test.go`（15） | PASS | — |
| TaskState context P-09 | `agent/taskstate_context_test.go`（9） | PASS | AfterCompaction |
| 自主续跑/守卫 | `runtime_context_test.go`（2 个 round + 1 个 pause） | PASS | **T5.4 9 个、T5.5 8 个命名测试全缺；无调度/E2E** |
| Heartbeat 持久 session | — | — | 5 个命名测试全缺 |
| Skill 工具 | — | — | **0 测试**；catalog 未迁移为 context |
| Subturn/steering | `subturn_test.go`（大量）、`steering_test.go`（大量） | PASS | 与 context 交互无测试 |
| 新增 loop guard/prestep/pressure/prune/policy（工作树） | `prestep_test.go`（6）、`loop_guard_test.go`（11+）、`context_pressure_test.go`、`tool_result_prune_test.go` | 未见提交/未跑全量 | 均**未接入**主路径（除 prestep） |
| Chess 兼容/E2E/体积 | `scripts/chess-e2e.sh` | PASS | `integration/chess` go 测试、BASELINE.md |

---

## 6. 执行报告可信度核对（哪些自述不能采信）

| 执行报告位置 | 自述 | 审计发现 |
|---|---|---|
| §5 T4.2 行 | “部分：渲染器未单独落地；当前时间仍在 system prompt” | **过期**：`runtime_context.go:20` 已有独立 `BuildRuntimeSnapshot`；snapshot 模式 system prompt 不含时间（`TestSnapshotModeKeepsRuntimeOutOfSystemPrompt` PASS）。实际状态是 **部分**，但缺口在 `EstimateSystemTokens` 的 300 字符假设与测试命名 |
| §10 完成结论 | “T5.1–T5.7 …无进展/轮次阻断、用户 stop 暂停、持久 heartbeat” | **过度声明**：T5.4/T5.5 无调度/守卫/E2E 测试，`send_intermediate`/`resume_on_start` 无实现，complete/blocked 不发最终消息；T5.6 零测试；T5.7 不存在 |
| §5/§10 | “T2.2–T2.5 完成（核心）” | **不成立**：T2.3 未实现（缺 `isTurnBoundary`、摘要不跳过 context）；T2.5 无集成测试 |
| §10 | “T3.1–T3.3 完成（核心）”“T4.6 完成（核心）”“T7.3 删除 wecom/weixin/devices/hardware” | T3.1/T3.3 零测试且 hook respond 记录触碰与规格相反；T4.6 仍有 9/26 英文工具描述；T7.3 的 `chess-trim-inventory.md` 对应项仍空勾（文档与代码不一致） |
| §8 最终验证清单 | 勾选 T7.1/T7.5/T7.6“完成” | 无 `integration/chess` go 测试、无 `BASELINE.md`；T7.6 数据未复现（未运行 measurement 脚本） |
| §8 未勾选 race | “`pkg/agent` race 存在既有问题” | **属实**：实测 3 个 DATA RACE，其中 2 个为生产代码 close/send 竞争（非纯测试基建）；T6.5 已修复并 -race 全绿（A.4） |
| §12 | “唯一 v2 配置，删除 CHAGENT_* 别名与 channel_list 兼容” | 与任务书 §0.1/§T7.2 冲突，属**已记录的决策变更**，但 `docs/guides/configuration.zh.md` 未同步，形成文档/代码漂移 |

---

## 7. 建议的后续顺序（给 Lead 的优先级）

1. **先收敛工作树**：把 `prestep.go`（含 `supersedeContextMessages`）、`prompt_policy.go` 落盘提交并补测试（顺序/去重/开关关闭），再跑全量 `go test ./pkg/...`；当前工作树处于“迁移中”，不稳定的瞬时 FAIL 不应记入验收。
2. **T2.3 压缩边界**：新增 `isTurnBoundary`（context 非边界、`task-round` 是边界），`parseTurnBoundaries`/`isSafeBoundary`/`summarizeSession` 全部复用；补 `TestTurnBoundariesSkipContext`、`TestSummarizeSkipsContext`、`TestTrimRebuildsContext`。
3. **T2.5/T3.5/T5.7 集成/E2E 测试**：用 fake provider 建 `context_workspace_test.go`、`context_refresh_test.go`、`autonomy_e2e_test.go`，覆盖注入顺序、二次 turn 去重、压缩重建、触碰增量、自主 9 场景。
4. **T5.4/T5.5 收口**：调度前检查 steering/pending；complete/blocked 时 `SendResponse=true` 或显式 outbound；实现 `send_intermediate`、`resume_on_start`；新增 `KindAgentAutonomyRound`；provider 错误写 checkpoint 并保持 active/paused；max_tool_iterations 不再返回 `toolLimitResponse`。
5. **接入本轮新机制**：`tool_result_prune.go` 接入 LLM 请求前（只改内存副本）；`context_pressure.go` 接入 `SetupPipeline` 主动压缩判定；`loop_guard.go` 在 `ExecuteTools` 调用 `Observe` 并经 `enqueueStepContextMessage` 投递。
6. **T7.1/T7.5/T7.6**：补 `integration/chess/compat_test.go`/`e2e_test.go` 与 `BASELINE.md`；扩展 `chess-e2e.sh` 的重启/配置刷新断言；运行 `measure-startup.sh` 并记录 5 次中位数。
7. **T7.3/T7.4 清理**：删 CLI 子命令（保留 gateway/version）、非 chess provider、`pkg/audio`/`pkg/evolution`、通道 schema/Devices/Voice、Makefile 的 web/docker/membench 目标；更新 `chess-trim-inventory.md` 与 README 的删除声明。
8. **T6.2/T0.4 数据**：逐文件 `FileVersion` 快照替换全局 `maxMtime`，去掉技能递归 `WalkDir`；补 `BenchmarkBuildSystemPromptWithCache`/`BenchmarkSourceFilesChanged`/`BenchmarkBuildMessages` 与前后对比。
9. **T4.6 收尾**：翻译剩余 9 个工具 Description 与 `subagent.go:181`/`web.go:1107` 提示；扩展 `TestModelVisiblePromptsAreChinese` 覆盖 `pkg/tools`。
10. **文档同步**：修正 `docs/guides/configuration.zh.md` 的 `CHAGENT_*`/`chagent agent`；补 `pkg/agent/workspacecontext/README.md`、`docs/chess-integration.md`（回滚步骤）、`docs/reference/configuration.md`。

---

## 附录 A：命令与关键输出

### A.1 命令

```bash
cd /Users/wudaorui/code/chagent
go build ./cmd/chagent
go test ./pkg/... -count=1
go test ./pkg/agent/... -count=1
go test -race ./pkg/agent -count=1
go vet ./...
./scripts/lint-docs.sh
./scripts/chess-e2e.sh
go test ./pkg/agent/workspacecontext/... ./pkg/taskstate/... ./pkg/tools/integration/... \
        ./pkg/providers/protocoltypes/... ./pkg/providers/common/... -count=1
```

### A.2 关键测试逐条结果（11:24 实测，全 PASS）

```text
TestModelVisiblePromptsAreChinese
TestBuildRuntimeSnapshotChinese
TestRuntimeSnapshotDigestIgnoresTime
TestSnapshotModeKeepsRuntimeOutOfSystemPrompt
TestAdvanceAutonomyRoundNoProgressBlocks
TestAdvanceAutonomyRoundProgressContinues
TestPauseTaskStateForStop
TestTaskStateContextActive / NoState / Blocked / PausedAndComplete
TestTaskStateContextEscapesClosingTag / TruncatesObjective / EvidenceLimit
TestTaskStateContextOnlyOnChange / DisabledForPlainChat
TestInjectWorkspaceContextPersistsDurableMessage
TestInjectRuntimeContextSnapshotPersistsAndDeduplicates
```

```text
ok  github.com/rlds/chagent/pkg/agent              73.8s
ok  github.com/rlds/chagent/pkg/agent/workspacecontext  0.28s
ok  github.com/rlds/chagent/pkg/taskstate          0.014s
ok  github.com/rlds/chagent/pkg/tools/integration  0.179s
ok  github.com/rlds/chagent/pkg/providers/protocoltypes 0.012s
ok  github.com/rlds/chagent/pkg/providers/common   0.029s
```

### A.3 chess-e2e.sh 输出（11:20）

```text
PASS health 200 port=58798
PASS pico auto-enabled, config unchanged
PASS pico ping/pong
PASS SIGTERM exit
PASS pid cleanup
Chess E2E OK
```

### A.4 race 失败与修复（11:18 初测 / 12:57 修复验证）

初测（HEAD 0072c43a）：

```text
WARNING: DATA RACE (3)
--- FAIL: TestAgentLoop_HookRespond_MediaError (3.57s)
--- FAIL: TestAgentLoop_Steering_DirectResponseContinuesWithQueuedMessage (0.07s)
--- FAIL: TestDeliverSubTurnResult_RaceWithFinish (0.03s)
FAIL github.com/rlds/chagent/pkg/agent 78.766s
```

根因与修法（T6.5/task-12；测试 + 两处最小生产同步补丁）：

| # | 类型 | 竞争对（修复前） | 修法 |
|---|---|---|---|
| 2 | 测试基建 | `steering_test.go:516` `Chat` 写 `firstStarted=nil` vs 测试 `:1203` 无锁读 | 字段构造后不可变 + `sync.Once` 关闭，不再写字段 |
| A | **生产代码** | `channels.Manager.StopAll` `close(w.queue)`（`manager.go:1364`）vs `dispatchOutbound` `w.queue <- msg`（`:1655`） | Manager 增加 `dispatchWG`；StartAll 的 dispatchOutbound/dispatchOutboundMedia/runTTLJanitor 启动前 `Add(3)`、goroutine `defer Done()`；StopAll 在 `dispatchTask.cancel()` 后 `Wait()`，再 close worker queue |
| B | **生产代码** | `turnState.Finish` `close(pendingResults)`（`turn_state.go:832`）vs `deliverSubTurnResult` `pendingResults <- result`（`subturn.go:577`） | `turnState.resultsMu RWMutex`：发送持 RLock（非阻塞发送 + 1ms 重试并继续监视 `Finished()`，保留“结果可丢弃为 orphan”语义），Finish close 持 Lock；保留 recover 兜底 |

另修一处 `-race` 全量偶发失败（非 race）：`TestEvolutionBridge_TurnEndUsesExplicitAttemptTrail` / `..._UsesPayloadWorkspace` 未 `Close()` bridge，异步 `FinalizeTurn` 与 `t.TempDir` 清理竞争（"directory not empty"）；测试加 `defer bridge.Close()` 等待 `wg`。

修复后实测（2026-10-30 12:5x）：

```text
go build ./...                                        OK
go test -race ./pkg/agent -count=1                    ok  84.6s，0 DATA RACE
go test -race ./pkg/channels/... -count=1             ok  channels 14.9s / pico 1.8s
go test ./pkg/agent ./pkg/channels -count=1           ok  77.2s / 13.9s
go test -race ./pkg/agent -run 'TestDeliverSubTurnResult_RaceWithFinish|TestAgentLoop_HookRespond_ResponseHandledMediaPreservesOutboundContext' -count=5   ok
```

---

## 附录 B：任务书指定命名测试落地率（169 个命名项）

| 组 | 命名测试数 | 字面存在 | 说明 |
|---|---|---|---|
| T0.1 | 4 | 4 | 全覆盖 |
| T0.2 | 4 | 0 | 只有 chess_compat 间接断言 |
| T0.3 | 4 | 0 | 2 个等价测试（不同名） |
| T0.4 | 3（benchmark） | 0 | 仅 `BenchmarkBuildMessagesWithCache` |
| T1.1–T1.5 | 35 | 35 | 全覆盖（含 9 份 golden） |
| T2.1 | 10 | 10 | 全覆盖 |
| T2.2 | 5 | 0 | 1 个等价测试 |
| T2.3 | 5 | 0 | 未实现 |
| T2.4 | 5 | 0 | compose 测试部分等价 |
| T3.1 | 6 | 0 | 未测 |
| T3.2 | 6 | 0 | compose 测试部分等价 |
| T3.3 | 5 | 0 | 未测 |
| T3.4 | 4 | 0 | 未测 |
| T4.1 | 5 | 0 | 未实现 |
| T4.2 | 5 | 0 | 3 个等价 + 1 dedup |
| T4.3 | 6 | 0 | 未测 |
| T4.6 | 1 | 1 | 另有 2 个扩展 i18n 测试 |
| T5.1 | 6 | 6 | 全覆盖 |
| T5.2 | 9 | 9 | 全覆盖（另 6 个扩展） |
| T5.3 | 5 | 3 | Active/Blocked/OnlyOnChange |
| T5.4 | 9 | 0 | 仅 2 个 round 守卫 |
| T5.5 | 8 | 0 | 仅 3 个等价守卫 |
| T5.6 | 5 | 0 | 未测 |
| T7.2 | 14 | 1 | `TestSingletonRecognizesChagentProcessName` |
| **合计** | **169** | **69（40.8%）** | 库层覆盖好，接线/自主/裁剪层薄弱 |

> 注：`pkg/agent/workspacecontext` 共有 59 个实际测试函数（任务书 T1 命名 35 个 + 额外 24 个），全部 PASS；上表只统计任务书点名的名字。