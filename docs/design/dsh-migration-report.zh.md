# DSH → Chagent Agent Loop 机制迁移报告

> 状态：本轮迁移已完成（模型执行 / 上下文窗口管理 / Agent 循环 / Prompt 注入四条线）
> 日期：2026-10-30
> 关联文档：
> - 执行审计：`docs/design/chagent-taskbook-audit.zh.md`
> - DSH 模型执行与循环：`docs/design/dsh-agent-loop-model-exec.zh.md`
> - DSH 上下文与 prompt：`docs/design/dsh-context-prompt.zh.md`
> - 任务书：`docs/design/agent-workspace-context-task-book.zh.md`
> - Prompt 规范：`docs/design/agent-workspace-context-prompts.zh.md`
> 参考仓库：`/Users/wudaorui/code/deepseek-harness`（DSH）

---

## 0. 结论摘要

### 0.1 本轮做了什么

在 **不引入事件日志重写、不改 provider 协议** 的前提下，把 DSH 中与 agent loop 直接相关、且在 chagent 现有架构里可落地的机制迁移进来，并在原有实现上改造：

| # | 迁移机制 | 落点 | 状态 |
|---|---|---|---|
| 1 | pre-step 有序注入表 + next-step 队列（`additionalContexts` 语义） | `pkg/agent/prestep.go`；`turn_coord.go` / `pipeline_setup.go` 接线 | 已接入 |
| 2 | repeat-tool-reminder 循环卫生 guard（阈值 3/5/8、规范化参数、透明排除、用户插话重置） | `pkg/agent/loop_guard.go` + `pipeline_execute.go` 接线 | 已接入 |
| 3 | tool-result pruner（8192/4096/1024、rune 安全、幂等）+ 压缩时“先裁剪后丢 turn” | `pkg/agent/tool_result_prune.go` + `context_legacy.go` | 已接入 |
| 4 | 上下文压力策略（DSH 0.8 / 0.16 / 65536） | `pkg/agent/context_pressure.go` | 库层完成，触发点尚未切换（见 §4） |
| 5 | Turn 边界语义：context 消息不是 user turn（T2.3） | `pkg/agent/context_budget.go` | 已接入 |
| 6 | 摘要跳过 context 正文；摘要/压缩提示中文化 | `pkg/agent/context_legacy.go` | 已接入 |
| 7 | state 型 context 的 replace 语义（runtime snapshot / task-state 只保留最新） | `pkg/agent/prestep.go: supersedeContextMessages` | 已接入（模型可见层） |
| 8 | 策略 Prompt P-01/P-02/P-11 按开关注入 system prompt（T4.5） | `pkg/agent/prompt_policy.go` + `context.go` / `prompt.go` / `instance.go` | 已接入 |
| 9 | 新的持久 context kind `loop-guard` | `pkg/providers/protocoltypes/types.go` | 已接入 |

### 0.2 迁移完成度（按 agent-loop 直接关联机制计 34 项）

| 维度 | 迁移前已具备 | 本轮新增/改造 | 仍为部分 | 未迁移 | 本轮后完成度* |
|---|---|---|---|---|---|
| 模型执行（8 项） | 4 | 0 | 3 | 1 | 50%（含部分 87.5%） |
| 上下文窗口管理（8 项） | 2 | 3 | 1 | 2 | 62.5%（含部分 75%） |
| Agent 循环（9 项） | 3 | 1 | 3 | 2 | 44.4%（含部分 77.8%） |
| Prompt 注入（9 项） | 3 | 2 | 3 | 1 | 55.6%（含部分 88.9%） |
| **合计（34 项）** | **12（35.3%）** | **6（17.6%）** | **10（29.4%）** | **6（17.6%）** | **52.9%（含部分 82.4%）** |

\* 完成度 =（已具备 + 本轮新增）/ 总项；“含部分”把“部分”按 50% 计为可接受但未闭环。

> 说明：这张表衡量的是“机制是否有对应实现”，**不**衡量质量与测试强度。测试缺口见 `chagent-taskbook-audit.zh.md` §5 与本报告 §4。

### 0.3 与任务书的直接对应

| 任务书项 | 本轮迁移后状态 |
|---|---|
| T2.3 压缩/turn 边界/摘要适配 | **本闭环**：context 不再是 turn 边界；摘要跳过 context 正文；压缩先裁剪工具结果 |
| T4.5 Prompt 文本接入（P-01/P-02/P-11） | **本闭环**：按 autonomy/workspace_context 开关注入，关闭时 system prompt 不变 |
| T2.2 / T3.3 注入顺序 | 由 pre-step 有序表统一（workspace 100 → runtime 200 → task-state 300 → context-pressure 350 → skill-catalog 400 → loop-guard 500；350/400 与 loop-guard 的 500 为第二轮补齐，见 §7） |
| T5.5 无进展/循环守卫（part） | 新增工具级重复调用守卫；回合级 no-progress 仍沿用既有实现 |
| T4.1 Context source 注册扩展 | 仍为“内部固定表”，不是对外注册 API（见 §4） |
| T4.3 技能渐进披露 | **第二轮已接入**：`skill` / `skill_load` 工具 + 技能目录默认作为 durable context（`tools.skills.catalog_mode` 可选 `context`/`system`；`skill_load` 关闭时回退 system），见 §7 |

---

## 1. 审计基线（来自独立审计）

`docs/design/chagent-taskbook-audit.zh.md` 以 HEAD `0072c43a` 为基线，对 43 项任务给出：

- 完成 11、部分 23、未完成 9；
- 任务书点名的 169 个测试中，字面存在 69 个（40.8%）；
- 执行报告自述存在过期/过度声明（T4.2 实际已落地、T5 系列未完成、T7.3 裁剪未收敛）。

本报告只覆盖“模型执行 / 上下文窗口 / Agent 循环 / Prompt 注入”这条主线，其余任务（裁剪、chess E2E、文档）以审计报告为准。

---

## 2. 逐维度机制对照

### 2.1 模型执行

| DSH 机制 | chagent 迁移前 | 本轮动作 | 结论 |
|---|---|---|---|
| provider 抽象 + 路由 + fallback | `providers.LLMProvider`、`FallbackChain`、light-model routing | 未改 | 已具备 |
| 请求构建（单 system、history、tools） | `ContextBuilder.BuildMessagesFromPrompt` | 未改 | 已具备 |
| 流式 + tool_call 增量组装 | `pipeline_streaming.go`、`providers.NormalizeToolCall` | 未改 | 已具备 |
| 重试（transient/backoff/context overflow） | `pipeline_llm.go` retry loop + fallback | 未改 | 已具备 |
| 结构化失败分类 + 持久 retry 记录 | `providers.ClassifyError` + 事件；无跨重启 retry 记录 | 未改 | 部分 |
| 并行工具调度（屏障/并发池/模型序提交/abort 合成结果） | 串行 `for` 循环 | 未改（明确列为后续 P0） | 未迁移 |
| 中断落盘与恢复修复（每个 tool-call 必有 tool-result） | hard abort 时 restore session；无合成结果 | 未改 | 部分 |
| usage 锚定 token 计量 | 捕获 usage；预算仍纯估算 | 未改 | 部分 |

### 2.2 上下文窗口管理

| DSH 机制 | 迁移前 | 本轮动作 | 结论 |
|---|---|---|---|
| token 估算 + 预算检查 | `pkg/tokenizer` + `isOverContextBudget` | 未改 | 已具备 |
| 压缩触发（proactive / overflow-retry / summarize） | 三类触发已有 | 保留 | 已具备 |
| 压缩策略：安全边界 / 保留区 / 摘要 | 以 user role 为边界（context 会被误当 turn） | **新增 `isTurnBoundary`**，parse/findSafe/trim/forceCompression/summarize 全部改用统一判定 | 本轮改造 |
| tool-result pruner | 无 | **新增 `PruneToolResults`** 并接入 `forceCompression`（先裁剪、够用则不丢 turn） | 本轮迁移 |
| 上下文压力阈值策略（0.8/0.16/65536） | 固定 `isOverContextBudget` 判定 | **新增 `ContextPressurePolicy`**（纯函数 + 测试），触发点未切换 | 库层完成 |
| spill（大输出落盘/引用回填） | 无 | ~~未迁移~~ **第二轮已接入**：`pkg/agent/spill.go` 落盘 `<workspace>/.chagent/spill/`（目录 0700/文件 0600），裁剪 marker 回填路径，保留 30 天，失败降级（见 §7） | 已迁移（第二轮） |
| state 型 context replace 语义 | runtime/taskstate 变化即追加 | **`supersedeContextMessages`**：模型可见层只保留最新 runtime/task-state | 本轮部分迁移 |
| 压缩收敛证明（surface replaceGeneration / 压缩次数有界） | 无 | 未迁移 | 未迁移 |

### 2.3 Agent 循环

| DSH 机制 | 迁移前 | 本轮动作 | 结论 |
|---|---|---|---|
| 主循环 + 阶段状态机 | `turnState` 阶段 + `runTurn` 协调器 | 未改 | 已具备 |
| SubTurn / subagent | `subturn.go` | 未改 | 已具备 |
| steering / inbox | `steering.go` | **新增 `pendingStepContexts`** 作为 next-step 队列，pre-step 统一 flush | 本轮增强 |
| goal/autonomy 回合驱动 | `autonomy_runtime.go` 基础续跑 | 未改（审计指出的守卫/事件/E2E 缺口仍在） | 部分 |
| plan mode | 无 | 未迁移 | 未迁移 |
| 后台 jobs / schedule | cron、heartbeat | 未改 | 部分 |
| repeat-tool guard | 无 | **新增 `repeatToolGuard`**（阈值 3/5/8，中文建议式提醒，不阻断） | 本轮迁移 |
| 终止语义（安全阀 + 不静默停止） | `max_tool_iterations` + task_round 续跑 | 未改（T5.5 仍部分） | 部分 |
| turn-stopping 收尾注入 | 无 | 未迁移 | 未迁移 |

### 2.4 Prompt 注入

| DSH 机制 | 迁移前 | 本轮动作 | 结论 |
|---|---|---|---|
| PromptRegistry 分层/slot/order | `prompt.go` 已有 | 未改 | 已具备 |
| 中央 pre-step 注入表 | 硬编码 3 处调用 | **新增 `builtinPreStepContextSources`**（显式 order） | 本轮迁移 |
| runtime snapshot “变化才注入” | `workspace_context_runtime.go` | 未改（新增 supersede，避免多次快照累积） | 已具备 + 增强 |
| task state “变化才注入” | `taskstate_context.go` | 同上 | 已具备 + 增强 |
| 策略 Prompt P-01/P-01/P-11 | 无 | **新增 policy contributor**，按开关注入 kernel 层 | 本轮迁移 |
| skill 渐进披露（catalog context + skill tool） | `skill`/`skill_load` 工具存在；catalog 在 system | 未改 | 部分 |
| Context source 对外注册 API | 无（内部固定表） | 未迁移 | 未迁移 |
| 每步 reminder（guard/limit/todo） | 只有 task-round 续跑 | repeat-tool guard 已接入；临近上限/上下文压力提醒未做 | 部分 |
| system prompt 前缀缓存稳定性 | CacheControl + 缓存；summary 仍拼在 system | 未改 | 部分 |

---

## 3. 本轮实现明细

### 3.1 pre-step 注入表（`pkg/agent/prestep.go`）

```go
const (
    preStepOrderWorkspaceContext = 100
    preStepOrderRuntimeContext   = 200
    preStepOrderTaskState        = 300
    preStepOrderContextPressure  = 350
    preStepOrderSkillCatalog     = 400
    preStepOrderLoopGuard        = 500
)

type preStepContextSource struct {
    ID    string
    Order int
    Inject func(ctx context.Context, ts *turnState, al *AgentLoop, messages []providers.Message) []providers.Message
}
```

- `runPreStepContextSources`：turn 内每次 LLM 调用前按 order 升序执行（第一轮只有 100/200/300/400；第二轮新增 350（context-pressure）与 400（skill-catalog）、loop-guard 从 400 调整为 500）：`context.workspace`(100) → `context.runtime`(200) → `context.task_state`(300) → `context.pressure_notice`(350) → `context.skill_catalog`(400) → `context.loop_guard`(500)；`NoHistory` 只 flush 临时提醒、不注入持久 context。
- `enqueueStepContextMessage` / `flushStepContextMessages`：工具执行只“投递到 next-step 队列”，由 pre-step flush 统一追加并持久化——对应 DSH `additionalContexts` 语义，工具不再直接改消息切片。
- `supersedeContextMessages`：模型可见历史里 runtime-context / task-state 只保留最新一条；workspace baseline/delta 保留（它们是增量记录）。旧副本仍留在 session JSONL 供审计，压缩时再收敛。

### 3.2 repeat-tool guard（`pkg/agent/loop_guard.go` + 接线）

- 阈值 `[3,5,8]`：第 3 次简短提醒，第 5/8 次详细提醒（含工具名、连续次数、参数预览）。
- 链键 = `toolName + "\x00" + canonicalArgs`（`json.Marshal` 天然排序 key）；被 exclude/include 排除的调用对链透明。
- 默认排除 `todo_write/task_get/task_update`；参数预览 rune 安全截断 500 字符。
- 提醒是 `role:"user"` 的合成消息，`Context.Kind="loop-guard"`，只建议不阻断。
- 接线：`pipeline_execute.go` 在调用真正执行前 `observeToolCallForLoopGuard`；`turn_coord.go` 在真实 steering 注入时 `resetLoopGuardChain`（用户插话后重复不算循环）。

### 3.3 tool-result pruner + 压缩优先裁剪（`pkg/agent/tool_result_prune.go`）

- 默认 `ThresholdChars=8192`、`HeadChars=4096`、`TailChars=1024`（对齐 DSH）。
- 仅 `role=="tool"` 的纯文本结果；按 Unicode 码点切片；已含标记则跳过（幂等）；返回新切片不改入参。
- `forceCompression`：先裁剪；若裁剪后估算已回到预算内，则只持久化裁剪结果、**不丢任何 Turn**；否则在裁剪后的历史上继续按 Turn 丢弃。
- 第二轮已补齐 spill：裁剪前把原始中间内容写入 `<workspace>/.chagent/spill/`（目录 0700、文件 0600、默认保留 30 天），marker 回填文件路径；session JSONL 仍是 append-only、无 replace 语义，但被裁剪的原始内容可通过 spill 路径回放（见 §7）。

### 3.4 上下文压力策略（`pkg/agent/context_pressure.go`）

```go
type ContextPressurePolicy struct {
    ThresholdRatio float64 // 0.8
    RetainRatio    float64 // 0.16
    HeadroomTokens int     // 65536
}
func (p ContextPressurePolicy) Threshold(contextWindow, maxOutput int) int
func (p ContextPressurePolicy) RetainTokens(contextWindow, maxOutput int) int
func (p ContextPressurePolicy) Pressure(...) Pressure
```

纯函数、无副作用；与现有 `isOverContextBudget` 并存。本轮**不切换**默认触发阈值，避免破坏既有压缩测试与线上行为；切换是 §4 的待办。

### 3.5 Turn 边界与摘要（`pkg/agent/context_budget.go` / `context_legacy.go`）

- `isTurnBoundary(msg)`：仅真实 user 消息或 `task-round` context 起新 Turn；workspace/runtime/task-state/skill-catalog/loop-guard 都是 Turn 内 rider。
- `parseTurnBoundaries` / `isSafeBoundary` / `findSafeBoundary` / `nextHistoryTrimStart` / `forceCompression` / `findNearestUserMessage` 全部使用统一判定。
- `summarizeSession`：跳过 context 消息正文；emergency compression note、摘要/合并/回退提示全部中文化。
- 既有测试断言 `"Emergency compression"` 已按中文化更新为 `"【紧急压缩】"`（未删除、未放宽断言）。

### 3.6 策略 Prompt（`pkg/agent/prompt_policy.go` + `context.go`/`prompt.go`/`instance.go`）

- P-01 自主契约、P-11 完成/阻塞策略：`autonomy.enabled=true` 时注入 kernel 层。
- P-02 工作区指令优先级：`workspace_context.enabled=true` 时注入。
- 两者关闭时**不产生任何 part**，默认聊天 system prompt 与改动前一致（由既有 golden/缓存测试与新增开关测试共同保证）。
- 新增 `PromptSlotPolicy` 与 3 个 source 描述；`ContextBuilder.WithPolicyPrompts(...)` 链式选项，`instance.go` 按配置开关调用。

### 3.7 新增持久 kind

`protocoltypes.ContextKindLoopGuard = "loop-guard"` + `Message.IsLoopGuard()`；wire 序列化不包含 `Context`（既有约束）。

---

## 4. 未迁移 / 后续优先级

### P0（影响“agent 长任务可靠性”）

1. **并行工具调度**（DSH `executeToolBatch`）：排他屏障、滚动并发池、模型序提交、abort 时为未启动调用补合成结果。前置条件：把“每个 tool-call 必有 tool-result”写成硬不变量。**第二轮已完成**（见 §7 第 5 项；R5 收尾补齐相邻调用的 capability 判定）。
2. **自主闭环收敛**（T5.4/T5.5/T5.7）：steering/pending 守卫、complete/blocked 最终总结、`KindAgentAutonomyRound` 事件、provider 错误 checkpoint、`resume_on_start`、`max_tool_iterations` 改为安全阀。**第二轮已完成**（见 §7 第 1 项）。
3. **skill catalog 迁 context**（T4.3）+ `skills.catalog_mode`：避免技能目录污染 system 前缀缓存。**第二轮已完成**（见 §7 第 2 项）。
4. **Context source 对外注册**（T4.1）：把 `builtinPreStepContextSources` 变成可注册接口，插件/hook 可贡献 context。**第二轮已完成**（见 §7 第 3 项）。
5. **spill**：pruner 裁剪前把原始中间内容写入 workspace 文件并在标记里给路径，弥补“不可回放”。**第二轮已完成**：`pkg/agent/spill.go` 落盘 `<workspace>/.chagent/spill/`（0700/0600、30 天清理、失败降级），marker 回填路径（见 §7）。

### P1

6. 上下文压力策略切换为触发判据（`context_pressure.go` 接入 `isOverContextBudget` / SetupTurn / CallLLM 的 pre-call 检查，并保持旧阈值可回退）。**R5 部分完成**：首次 LLM 调用前已能评估并注入 `context-pressure` 提醒（`pipeline_setup.go` + `prestep.go` order 350），硬保护仍是 `isOverContextBudget`；把触发判据完全替换为策略阈值仍待做。
7. usage 锚定 token 计量（DSH `TokenMeter` 投影）：用最近一次 provider `prompt_tokens` 做锚点，估算增量。**R5 已完成**：`turn_coord.go` 主回合 `CallLLM` 改传 `turnCtx`，`pipeline_llm.go` 的 usage 锚点记录已命中主回合（回归测试 `TestCallLLMMainTurnRecordsUsageAnchor`）。
8. 结构化失败对象 + 持久 retry 记录 + “切模型前先压缩”收敛。
9. `turn-stopping` 收尾注入点；plan mode。
10. 摘要从 system prompt 迁出（作为可 replace 的 context 消息），彻底稳定 system 前缀缓存。
11. 每步“临近上限”提醒（剩余 token/轮次），由 pre-step 表注入。**R5 已完成**：`context.pressure_notice`（order 350）在首轮与后续迭代均可注入（回归测试 `TestTurnContextPressureNoticeOnFirstIteration`）。

### 已知限制（本轮明确不解决）

- session JSONL 仍是 append-only，没有 DSH 的 `surface replace`；`supersedeContextMessages` 只在模型可见层生效，磁盘历史仍会增长，依赖压缩收敛。
- 压缩切点仍以 Turn 边界为准：同一 Turn 内出现超大工具轮时无法安全压缩（DSH 用 tool-call/result 配对平衡做切点，见 `dsh-context-prompt.zh.md` §8.2），这是后续升级点。
- session JSONL 仍是 append-only：spill 文件让被裁剪的原始内容可按路径回放，但 JSONL 内的旧消息不会被原地替换，磁盘历史增长仍依赖压缩与 spill 30 天过期清理收敛。
- `go test -race ./pkg/agent` 仍有 3 个仓库既有测试基础设施 DATA RACE（审计报告已定位），本轮新增代码未引入 race。
- 本轮未新增外部依赖、未改 provider 协议、未改 config 外部配置格式。

---

## 5. 验证

```text
gofmt -l pkg/agent ...                        干净
go build ./...                                 OK
go vet ./...                                   OK
go test ./pkg/agent -count=1                   PASS（含新增 prestep/loop_guard/prune/pressure/policy 测试）
go test ./pkg/... -count=1                     PASS
./scripts/lint-docs.sh                         OK
./scripts/chess-e2e.sh                         PASS（审计基线复跑）
```

新增测试（本轮）：

```text
TestPreStepContextSourcesOrder
TestFlushStepContextMessagesAppendsWithoutHistory
TestFlushStepContextMessagesSkipsBlankContent
TestRunPreStepContextSourcesNoHistoryNoop
TestSupersedeContextMessages
TestSupersedeKeepsOnlyNewestRuntimeSnapshot
TestLoopGuardReminderFlushPath
TestLoopGuardResetOnUserInterjection
TestRepeatToolGuard*（11 个，来自 loop_guard_test.go）
TestPruneToolResults* / TestToolResultPrune*
TestContextPressure*
TestTurnBoundaries* / TestFindSafeBoundary* / TestForceCompression*
TestPolicyPrompt* / TestAutonomyPolicy* / TestWorkspacePolicy*
TestSummarize*（context 跳过 + 中文提示）
```

本轮新增测试共 **53 个**：`loop_guard_test.go` 11、`prestep_test.go` 8、`tool_result_prune_test.go` 10、`context_pressure_test.go` 4、`context_turn_boundary_test.go` 8、`prompt_policy_test.go` 12，全部 PASS。

指标（本轮未做 benchmark，以下为审计基线 + 新增护栏测试）：

```text
go test ./pkg/agent 耗时        ~74s（全绿）
workspacecontext tests          59 PASS（审计基线）
本轮新增测试                    53 PASS
tool-result pruner 示例         12000 字符 → 5151 字符（savedChars=6849，估算 token 4806→2066）
forceCompression 裁剪优先分支   6 条消息全保留，DroppedMessages=0，请求 4947→2207（窗口 4800）
```

---

## 6. 结论

- **模型执行**：chagent 原有 provider/流式/重试已接近 DSH 可用水平；差距集中在并行工具调度、结构化失败与持久重试、usage 锚定计量。
- **上下文窗口**：本轮补齐了 DSH 最关键的“安全 turn 边界 + 工具结果先裁剪 + 变化才替换”三件套，使压缩不再误伤 context/tool-call 结构；spill 与压力阈值接线已在第二轮补齐（见 §7）。
- **Agent 循环**：主循环/subturn/steering 已具备；本轮补了循环卫生 guard（重复工具调用）；自主闭环与 plan mode 仍是最大缺口（自主闭环已在第二轮补齐，见 §7）。
- **Prompt 注入**：从“硬编码 3 处调用 + 全在 system prompt”变为“有序 pre-step 表 + 开关式策略层”，与 DSH 的结构对齐；context source 对外注册与 skill 渐进披露是下一步（均已在第二轮补齐，见 §7）。
---

## 7. 第二轮迁移（2026-10-30 下午）

在第一轮（§3）基础上继续执行 §4 的 P0/P1 任务，全部落地并有测试：

| # | 机制 | 落点 | 状态 |
|---|---|---|---|
| 1 | 自主闭环：调度守卫、用户消息优先取消、轮次事件、最终总结、resume_on_start、provider/迭代 checkpoint | `pkg/agent/autonomy_runtime.go`（+870 行改造）、`agent.go`、`turn_coord.go`、`agent_message.go`、`pkg/events/kind.go`、`event_payloads.go` | 已接入 |
| 2 | skill catalog 迁 durable context + `tools.skills.catalog_mode`（context/system 回退） | `pkg/agent/skill_catalog_context.go`、`context.go`、`instance.go`、`pkg/config/config.go` | 已接入 |
| 3 | Context source 对外注册 API（T4.1） | `pkg/agent/context_source.go`、Lead 在 `prestep.go` / `pipeline_setup.go` 接线 | 已接入 |
| 4 | spill：工具输出大块落盘 + 裁剪 marker 回填 + 30 天清理 + 失败降级 | `pkg/agent/spill.go`、`tool_result_prune.go`、`context_legacy.go` | 已接入 |
| 5 | 并行工具调度：排他屏障、滚动并发池、模型序提交、abort 合成结果 | `pkg/agent/tool_batch.go`、`pipeline_execute.go`（保守接入：无已挂载 hook + ≥2 相邻 parallel-safe + 无排队 steering）、`pkg/tools/concurrency.go` | 已接入 |
| 6 | usage 锚定上下文计量 + 临近上限提醒 | `pkg/agent/context_meter.go`、`pipeline_llm.go` 记录锚点、`prestep.go` 新增 order=350 注入源、`protocoltypes.ContextKindContextPressure` | 已接入 |
| 7 | skill catalog / runtime / task-state 的 replace 语义扩展到 catalog | `prestep.go: supersedeContextMessages` | 已接入 |
| 8 | 残留英文工具 prompt 中文化（T4.6 收尾）：search/delegate/web_search/web_fetch/tts/reaction/load_image/send_file/cron + subagent/搜索系统提示 | `pkg/tools/**` | 已完成 |
| 9 | T2.5/T3.5 集成测试、T0.4/T6.2 benchmark | `pkg/agent/context_workspace_integration_test.go`、`context_refresh_test.go`、`bench_test.go` | 已完成 |

### 7.1 迁移后完成度（同一 34 项口径）

第一轮 12 项已具备 + 6 项新增；第二轮再完成/部分完成 §4 中 6 个 P0/P1 项（自主闭环、skill catalog、Context source、spill、并行调度、usage 计量），并把 P1 的“临近上限提醒”“turn 边界 replace 扩展”落地。剩余未迁移收敛为：

1. **surface replace**（架构级，需单独设计；当前为模型可见层 supersede + append-only JSONL）。**事件日志已具备**：`pkg/events` 的 `agent.*` / `gateway.*` / `mcp.*` 等 runtime event 与 `docs/architecture/runtime-events.zh.md`；R5 收尾后 `agent-autonomy-round`、`agent.context.compress` 等事件均有稳定的 payload。
2. **压缩切点用 tool-pairing 而非 Turn 边界**（同 Turn 内超大工具轮仍不可压）。
3. **plan mode / turn-stopping 注入 / jobs 交付历史**（DSH 次级循环，chess profile 暂不需要）。
4. **T7.3/T7.4 破坏性裁剪**：已完成。非 chess CLI、Makefile 目标、docker/CLI 文档之外，
   第二批又删除了 `pkg/audio`、`pkg/evolution`、`pkg/seahorse`、非 pico 通道 schema、
   硬件/语音配置与 bus audio/voice 流，并 `go mod tidy` 清理 8 个无用依赖；
   总计 -33,770 行 / 94 文件。清单与验证见 `docs/design/chess-trim-inventory.md`。
   有意保留：isolation、MCP、cron 等 agent 运行必需包。
   追加（2026-10-30）：provider 仅保留通用协议（OpenAI Chat Completions 兼容 +
   Anthropic Messages 兼容），厂商专属协议/SDK/CLI/OAuth 已全部删除；详见
   `docs/design/agent-workspace-context-execution-report.md` §19。
5. ~~**T6.5 race**~~：已修复。3 个数据竞争（1 个测试基建 + 2 个生产代码：channels.StopAll close/send、turnState.Finish close/subturn send）已用最小同步补丁消除，`go test -race ./pkg/agent` 全绿（0 DATA RACE），并发强度与断言未降低。

> R5 收尾修复（2026-10-30 晚，工作树内完成）已闭环上述列表中的 P1 项 7（usage 锚定）与 11（临近上限提醒），并完成 P0 `/reload` 后 shutdown 死锁、config 显式 0 校验、MCP servers-only 自动启用、批调度 capability 判定；逐项结果见 `docs/design/agent-functional-test-report.zh.md`。

### 7.2 第二轮验证

```text
gofmt（改动文件）                    干净
go build ./...                       OK
go vet ./...                         OK
go test ./pkg/... -count=1           PASS
新增/迁移测试                         约 60 个（第二轮迁移口径，不含 R5 收尾）
                                     自主 13、上下文源/技能目录 17、spill 10、
                                     并行调度 8、上下文计量 8、集成/基准 18
```

R5 收尾修复新增/补充回归测试约 27 个（`pkg/channels` 1、`pkg/agent` 10、`pkg/config` 7、`pkg/mcp` 4、`pkg/gateway` 5；以工作树收尾状态为准）：P0 reload→SIGTERM 死锁（`pkg/channels/manager.go` + `TestReloadThenStopAllDoesNotDeadlock`）、agent loop P1/P2（usage 锚点、pre-step 可观测、context-pressure 首轮注入、supersede 收敛、loop-guard 接线）、config 显式 0 校验、MCP servers-only 自动启用、批调度 capability 判定。R5 后验证：`go build ./...` PASS、`go mod tidy -diff` 空、`./scripts/lint-docs.sh` OK；完整分层验证与缺陷清单见 `docs/design/agent-functional-test-report.zh.md`。

文档同步（T6.3/T6.4 部分）：`docs/guides/configuration.zh.md` 已按当前实现重写（唯一 v2 配置、CHAGENT_* 三变量、关键默认值、校验规则），新增 `docs/operations/rollback.zh.md` 回滚演练与 `pkg/agent/workspacecontext/README.md`。

已知差异（测试锁定真实行为）：批调度仅在“无已挂载 hook”时启用；steering 中途到达时已派发调用按模型序提交真实结果（不合成 skip）；自主 provider 错误不自动无限重试（保留 active + checkpoint）；迭代上限轮不写空 assistant 消息（`pipeline_finalize.go` 已跳过空内容）。
