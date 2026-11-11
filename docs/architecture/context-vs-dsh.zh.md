# 上下文处理 vs DSH：迁移对照、差异与缺陷判定

> 核对日期：2026-11-02。
> DSH：`/Users/wudaorui/code/deepseek-harness`，`master @ 22ddee37`（只读）。
> chagent：`/Users/wudaorui/code/chagent`，`ddmcv4 @ 5da70e0b` + 修复工作树（D1/D4/D5/P0 等 53 个未提交文件）。
> 方法：只读源码 + 既有测试/真实场景产物（task-6、task-15），本轮未跑真实模型；行号证据见
> `build/functest-20261030/analysis/context-parity/`。
>
> 判定口径：
> - **真实缺陷**：行为错误、与文档/声明不符、可复现失败，且不是有意取舍。
> - **设计取舍**：DSH 有对应机制，chagent 有意采用不同策略，且代码注释/文档已说明理由。
> - **未实现**：DSH 有、chagent 无对应机制或仅有模型可见层近似。

## 1. 结论摘要

- 上下文窗口管理主线迁移完整：token 估算/预算、三类压缩触发、pruner+spill、turn 边界、summary、
  durable context 注入与 supersede、skill catalog、resume/重启恢复均有实现与真实场景证据
  （task-15：9 相位 / 94 断言 0 失败，`build/functest-20261030/context/`）。
- 本轮复核**未发现新的 P0/P1 真实缺陷**；已知 D1/D4/D5/P0 已修复并复测（§2）。
- 残余差距集中在架构级与次级循环：**surface replace / durable 可重建投影缺失**（P2）、
  **压缩安全切点用 turn 边界而非 tool-pairing**（P2）、per-model 压缩策略与 headroom 未接线（P3）、
  plan mode / goal-round / turn-stopping 注入未迁移（P3）、spill 清理与事件日志形态差异（P3）。
- chess 场景（单 agent、单模型、无 plan mode、以自主任务轮为长任务载体）影响低；建议按 §4 排序处理。

## 2. 机制对照表

| # | 机制 | DSH 行为（file:line） | chagent 现状（file:line） | 差异/结论 |
|---|---|---|---|---|
| 1 | token 估算与计量 | 固定启发式 4 chars/token + 结构开销（`llm/token-meter/src/estimate.ts:13-60`）；每次请求用 `measure()` 以 **provider usage 为锚**重放计价（`llm/token-meter/src/index.ts:151-196`，锚点 `:173-175`） | CJK 加权启发式（`pkg/tokenizer/estimator.go`，`context_budget.go:108-136`）；`/context` 用同源估算（`context_usage.go:28-89`）；**usage 锚点已修复**：主回合 `CallLLM(turnCtx, turnCtx, …)`（`turn_coord.go:216`）→ `recordUsageAnchor`（`pipeline_llm.go:502-505`）+ 校正（`context_meter.go:37-59,144-157`） | 基本对齐；DSH 锚点是可重放投影（session event 驱动），chagent 锚点是 per-turn state（跨回合不继承）→ 首调用无锚点属预期（设计取舍，task-15 已记录） |
| 2 | 压力/预算公式 | `threshold=floor(min(window*ratio, window-maxOut-headroom))`，`retain=floor((window-maxOut)*retainRatio)`（`compaction/compaction-basic/src/config.ts:153-216`） | `threshold` 策略库已移植（`context_pressure.go:55-122`）但**触发仍用** `isOverContextBudget`（`context_budget.go:121-136`）；提醒预算 `min(window*0.8, window-maxTokens)`（`context_meter.go:116-140`） | 部分迁移：DSH 默认 80% 提前压缩；chagent 在“消息+工具+输出预留 > 窗口”才压缩（更保守、更晚），hawkeye 注释已说明（`context_pressure.go:22-25`）→ 设计取舍 |
| 3 | 压缩触发入口 | pre-step 压力、模型切换、`agent/request-error` overflow（`compaction-basic/src/index.ts:143-260`），最多 `maxOverflowRetries` | SetupTurn 主动预算检查（`pipeline_setup.go:60-115`）、provider context 错误重试压缩（`pipeline_llm.go:345-410`）、回合后 summarize（`pipeline_execute.go:856,1303`、`pipeline_finalize.go:80`；`context_legacy.go:40-58`） | 三类触发对齐；DSH 多“模型切换”触发、overflow 重试预算与回退语义更细 → 未实现（P3，chess 不切模型影响低） |
| 4 | 压缩安全切点 | 基于 surface 的 **tool-pairing 平衡**（`compaction/src/tool-pairing.ts:104-126`），保留近端 `retainTokens`（`compaction-basic/src/region.ts:117-160`），不平衡直接报错（`region.ts:349-354`） | **turn 边界**：`isTurnBoundary` 仅真实 user / task-round（`context_budget.go:23-31`），`findSafeBoundary`（`:63-104`）、`nextHistoryTrimStart`（`:171-188`）、`forceCompression`（`context_legacy.go:160-177`） | 差距：同一 turn 内多次工具往返时 DSH 可安全压缩，chagent 只能裁 pruner 或整轮丢弃（单 turn 时甚至只保留该 user 消息）→ 设计取舍+已知限制（迁移报告 §2.1），P2 建议补 tool-pairing 切点 |
| 5 | 压缩产物与注入方式 | 生成 `<compacted-summary>` checkpoint，作为**替换 user 消息**落 surface（`compaction-basic/src/summarizer.ts:21-23,184-196`；`compaction/src/checkpoint.ts:14-50`），压缩区从 surface 阴影化 | summary 写入 `meta.summary`，以 system 前缀 `CONTEXT_SUMMARY:` 注入（`context.go` 的 `contextSummaryPrefix`），历史用 `TruncateHistory`（`context_legacy.go:299-313`；`memory/jsonl.go:719-800`） | 语义近似、形态不同：chagent 无 surface 替换，只能截断+重写 JSONL → 见 #9（部分迁移/设计取舍） |
| 6 | tool-result pruner | 默认 8192/4096/1024 码点，marker 固定（`compaction/compaction-tool-result-pruner/src/config.ts:7-20`）；在压力压缩与 overflow 压缩前执行（`compaction-basic/src/index.ts:303,326-347`） | 同默认值（`tool_result_prune.go:36-44`），压缩前先裁剪（`context_legacy.go:120-158`），幂等跳过已裁剪（`tool_result_prune.go:138-143,174-176`） | 对齐；DSH 另有 tool-result 时点的 spill-policy 可选层（见 #7），chagent 无 → 设计取舍（P3） |
| 7 | spill | 本地会话目录 `session-<sha256(id)[:12]>`，目录 0700/文件 0600（`spill/spill-local/src/store.ts:78-115`）；启动期按 `cleanupPeriodDays` 清扫（`spill/spill-local/src/cleanup.ts:272-330`）；可选 tool-result 时点 spill（`spill/spill-policy/src/index.ts:50-114`） | `<workspace>/.chagent/spill/<session>/<label>-<sha256[:16]>.txt`，目录 0700/文件 0600（`spill.go:96-131,281-306`），30 天保留但**懒清理**（`spill.go:28-37,209-240` 每小时最多一次） | 路径/权限/内容回填对齐（task-15 验证 size/头尾一致）；清理为懒执行、无 tool-result 时点 spill → 设计取舍（P3，已有 D10 记录） |
| 8 | session surface replace | 事件日志为唯一事实源，surface 有 `replaceGeneration`，替换需覆盖被阴影节点（`core/session/src/surface.ts:249,363-571`）；token-meter/pruner 均基于 surface 重放 | JSONL append-only + `meta.skip/count`；截断只改 skip、`Compact` 物理重写（`memory/jsonl.go:47-68,662-830`）；压缩/摘要用 `SetHistory` 重写整文件（`context_legacy.go:143-144,190-191`） | **未实现（架构级）**：无 surface 投影、无替换代次、无跨进程可重建投影；历史仍会增长（依赖 Compact）→ P2 |
| 9 | 事件日志/可观测 | session event log（append-only、可重放）+ 多投影（`session/session-projection`）；checkpoint 事件（`session/session-checkpoint-policy/src/index.ts:1-100`） | runtime event logger 打印稳定事件（`events/kind.go:18-21,118-119`；`runtime_event_logger.go:177-190`），`agent.context.compress`/`agent.session.summarize` 有 payload；无 durable event log/重放 | 部分迁移：可观测性够用，但事件不是可重放事实源 → P2/P3（与 #8 同源） |
| 10 | pre-step 注入顺序 | 单一 `agent/pre-step` 瀑布 + 中央顺序表（`core/system-prompt/src/index.ts:136-200`：section/context 顺序）；上下文在同一水位注入并持久化 | 有序表 `100/200/300/350/400/500`（`prestep.go:24-87,94-108`；`pipeline_setup.go:33-42` 首轮等价注入）；修复后回合内每步再 `supersede`（`turn_coord.go:192-198`） | 对齐（顺序语义一致）；DSH 为插件瀑布可扩展，chagent 侧为内置表 + ContextSource 注册（`context_source.go:40-55,279-323`）→ 设计取舍 |
| 11 | workspace instructions / runtime / task state | `agent-instructions` baseline+增量刷新（`context/agent-instructions/src/index.ts:52-140`）；time-context 每步刷新（`context/time-context/src/index.ts:135-210`）；runtime snapshot 为 loop-owned surface 消息（`core/agent-loop/src/runtime-context.ts:1-80`） | workspace baseline/delta/remove（`workspacecontext/projector.go:238-567`）；runtime snapshot 按 digest+时间桶（`workspace_context_runtime.go:162-210`、`runtime_context.go:51`）；task-state 内容变化才注入（`workspace_context_runtime.go:124-157`） | 对齐；time 刷新粒度从“每步”变为“刷新间隔桶”（默认 600s）→ 设计取舍（缓存友好）；附：仅放 AGENTS.md 时它被 legacy agent 定义消费而不进 workspace context（文档 §3.2 与实现偏差，P3 文档级） |
| 12 | skill catalog 渐进披露 | 会话级 durable catalog（`skill/tool-skill/src/index.ts:30-46,169-215`），`skill` 工具按需加载正文 | 默认 `catalog_mode=context`（`skill_catalog_context.go:26-43,109-196`；`config/context_config.go`），replace/removed 语义、单副本 | 对齐（task-15 H 组验证：压缩后恰好 1 条、模型可见） |
| 13 | resume / 重建 | session format 多代迁移 + checkpoint policy + resume 首个 pre-step 标新 series（`core/agent-loop/src/agent.ts:117,669-672`；`session/session-format*`） | 重启即 JSONL 回放；首轮 SetupTurn 重新注入 workspace/runtime/skill（`pipeline_setup.go:33-42`；`workspace_context_runtime.go:184-210` digest 去重） | 部分迁移：功能等价（task-15 D 组 11/11 PASS：27==27、无重复注入、task_state/spill 保留），无 session 格式版本/迁移与 request-series 标定 → 设计取舍/未实现（P3） |
| 14 | per-step reminders / plan / todo / goal | repeat-tool-reminder 在 pre-step 注入（`guard/repeat-tool-reminder/src/index.ts:236`）；plan-mode 每步注入（`plan/plan-mode/src/index.ts:211`）；goal-round 驱动（`goal/goal-round-driver/src/index.ts:380`） | 重复工具守卫 3/5/8（`loop_guard.go:19-32,135-190`，pre-step order 500）；task-state/task-round 覆盖自主轮（`taskstate_context.go:16-75`；`autonomy_runtime.go:50-52,650,961-970`） | reminders/loop guard 对齐；**plan mode / goal-round / turn-stopping / jobs 注入未实现**（迁移报告 §4 已列）→ 未实现（P3，chess 不需要 plan mode） |
| 15 | 临近上限提醒 | 无模型可见 near-limit 提醒插件；压力经 token-meter 投影 + 自动压缩处理 | 额外机制：0.85/0.95 两档、中文 `<context_pressure>`、每 band 每 turn ≤1（`context_meter.go:160-212`；`prestep.go:266-294`）；已修复首迭代触发（`pipeline_setup.go:147`） | chagent 超出 DSH 的增强（设计新增）；task-15 E 组 7/7 PASS（首迭代 2 条、无重复/刷屏） |

## 3. 已修复项复核（本轮判定为已关闭）

| 修复项 | 修复点 | 判定依据（真实场景/单测） |
|---|---|---|
| **D1 usage 锚点主回合死路径** | `turn_coord.go:216` 首参 `turnCtx` → `pipeline_llm.go:502-505` 记录锚点 | task-15 F 组：iteration≥2 通知 provider/notice 中位偏差 **0.40 → 0.158**（`F.anchor_within_turn_improved` PASS）；first-call 无锚点属预期（锚点在响应后记录），断言已按此修正 |
| **D4 supersede 仅回合起点** | `turn_coord.go:192-198` 每步 pre-step 后 `supersedeContextMessages` | task-15 C 组：workspace/runtime/skill-catalog 各恰好 1 份、无重复正文 |
| **D5 pressure 仅 iteration>1** | `pipeline_setup.go:147` SetupTurn 末尾评估一次 | task-15 E 组：4 条通知中 2 条在首个 assistant 回复前注入 |
| **P0 reload→SIGTERM 挂死** | `channels/manager.go`（工作树） | task-15 D 组：SIGTERM rc=0、pid 删除、无残留、重启 33==33 |
| 显式 0 校验 / MCP 自动启用 | `config/context_config.go:247-310`、`mcp/manager.go` | 既有配置能力复跑 42/42 PASS（Lead 汇总） |

## 4. 缺陷/差异清单（按严重度）

| ID | 严重度 | 判定 | 差异/证据 | 影响（含 chess） | 建议 |
|---|---|---|---|---|---|
| C1 | **P2** | **未实现**（已知限制） | 压缩安全切点用 turn 边界而非 tool-pairing（§2 #4；DSH `tool-pairing.ts:104-126`、`region.ts:117-160`） | 同 turn 内多轮超大工具输出时无法安全压缩：只能 pruner 或整轮丢弃；chess 长任务单轮多工具场景可能丢上下文（spill 可挽回原文） | 引入 tool-pairing 平衡切点（与 DSH 对齐），补“单 turn 内超预算”单测 |
| C2 | **P2** | **未实现**（架构级） | session surface replace / 可重建投影缺失（§2 #8；DSH `surface.ts:249,363-571`、`session-projection`） | JSONL 只增不减（靠 Compact/重写），无法替换历史节点、无 replaceGeneration；审计/回放与压缩收敛证明弱；chess 日常无感，长会话磁盘增长 | 设计 durable event log + surface 投影；短期先在压缩后稳定跑 Compact |
| C3 | **P2** | **部分迁移** | 事件日志：chagent 仅 logger 打印稳定事件（`runtime_event_logger.go:177-190`），非可重放事实源；DSH event log 即事实源 | 排障靠日志文本，无法从事件重建 surface/锚点；与 C2 同源 | 事件持久化 + 投影重建（与 C2 合并立项） |
| C4 | **P2** | **设计取舍** | 压缩触发阈值：DSH 80% 提前压缩（`config.ts:191-216`），chagent 满窗才触发（`context_budget.go:121-136`） | 压缩更晚、更依赖 provider overflow 重试；真实场景已验证可用，但峰值请求成本更高 | 可灰度启用 `ContextPressurePolicy.Threshold` 作为主动触发（现已有纯函数与测试） |
| C5 | **P3** | **设计取舍**（有注释） | `headroomTokens`（DSH 默认 65536）在 chagent 未接线（`context_pressure.go:42-48`；`context_meter.go:108-140` 注释说明小窗口不适用） | 小窗口（<64K）无影响；大窗口下提醒/压缩余量偏小 | 窗口 >65536 时按 DSH 语义接线 |
| C6 | **P3** | **未实现** | per-model 压缩策略（DSH `modelPolicies`：`compaction-basic/src/config.ts:112-139`） | chess 单模型无影响；多模型切换时无法按模型容量分别配置 | 配置面新增 model_policies（低优先） |
| C7 | **P3** | **设计取舍** | spill 清理：DSH 启动期 sweep + 保留期（`cleanup.ts:272-330`）；chagent 懒清理 30 天、每小时至多一次（`spill.go:209-240`） | 长期不清扫时磁盘增长；chess 低风险 | 启动时触发一次 Cleanup；补清理单测 |
| C8 | **P3** | **设计取舍** | 无 tool-result 时点 spill（DSH `spill-policy` 可选层）；chagent 仅在压缩裁剪时落盘（`context_legacy.go:137`） | 超大工具输出在下次压缩触发前会原样进入一次请求（token 峰值） | 可选实现结果时点 spill（按 maxInlineTokens） |
| C9 | **P3** | **未实现** | plan mode / goal-round / turn-stopping / jobs 注入（§2 #14；迁移报告 §4 已列） | chess 不需要 plan mode；自主任务由 task-state/task-round 覆盖 | 保持不迁移，文档标注 |
| C10 | **P3** | **文档/一致性偏差** | 仅放 `AGENTS.md` 时被 `loadAgentDefinition` 当 legacy agent 定义消费，不进 workspace-instructions context（task-6 观察：`/context` 显示 0 文件；配置指南 §3.2 声称扫描 AGENTS.md/CLAUDE.md）；另 `/context` 在重启后下一轮前短暂显示 0 个文件（baseline 尚未重建） | 指令仍经 system prompt 到达模型，durable context 路径缺失；用户按文档配置 AGENTS.md 会看到不一致统计 | 文档澄清或 `loadAgentDefinition` 与 workspace context 二选一互斥；`/context` 可标注“待下一轮重建” |
| C11 | **P3** | **未实现** | session 格式版本/迁移与 resume request-series 标定（DSH `session-format*`、`agent.ts:669-672`）；chagent 无格式版本 | 升级/回滚兼容性靠 JSONL 兼容性，无显式迁移；重启语义已由 task-15 D 组验证可用 | 低优先；如引入 session 格式变更需补迁移与版本字段 |

> 说明：C1/C2/C3 与迁移报告 §2.1/§4 的已知未迁移项一致，不是本轮新发现的回归；
> C4/C5/C7/C8/C10 为两轮真实场景/配置复跑后新增的“边界与文档级”观察；本轮**无 P0/P1 真实缺陷**。

## 5. chess 场景影响与建议优先级

- 影响评估：chess 基础 agent = 单模型、单 agent、无 plan mode、长任务走自主轮 + task-state；
  因此 C1/C2/C3 属“长会话/审计/成本”级风险，不影响日常正确性；C4 影响峰值成本；C5/C6 无影响；
  C10 影响用户按文档配置 AGENTS.md 时的可观测性。
- 建议顺序：
  1. **P2**：C2+C3 立项（durable event log + surface 投影）——架构级、收益最大；
  2. **P2**：C1 tool-pairing 压缩切点（可独立实现，先补单测再接入）；
  3. **P3**：C4 灰度主动触发 → C7 启动清扫 → C10 文档/实现二选一 → C5/C6/C8/C9/C11。
- 验证建议（不跑真实模型即可）：新增单测覆盖“单 turn 内超预算的压缩切点/配对保持”、
  “surface 替换代次与投影重建”、 “spill 启动清扫”、“per-model policy 解析”；
  回归入口：`go test ./pkg/agent/... ./pkg/config/...`。

## 5. 关键差异解读（按机制族）

- **计量与触发族**：DSH 把 token-meter 当“可重放的单一事实源投影”，压缩阈值/保留区/headroom 都是模型容量参数；
  chagent 把计量当“每回合的启发式 + 单点上一次响应锚点”，触发用满窗硬阈值，另加策略库但未接线。
  结论：功能正确、成本与触发时机略保守；C4/C5 是这一族的两条待办。
- **压缩与裁剪族**：pruner 默认值、幂等、spill 路径/权限/回填、失败降级均与 DSH 对齐；
  差异集中在“切在哪”和“压完怎么落盘”：DSH 用 surface + tool-pairing 平衡切点，chagent 用 turn 边界 + JSONL 截断/重写。
  这是 C1/C2/C3 的共同来源，也是本对照中唯一需要架构级投入的部分。
- **注入与 durability 族**：pre-step 顺序、workspace/runtime/task-state/skill-catalog 的 durable context、
  replace/supersede 语义均已对齐（D4 修复后回合内也收敛）；DSH 的 surface 是持久化机制本身，
  chagent 的 durable context 只是“模型可见层的收敛”，磁盘仍是 append-only → C2/C3。
- **次级循环族**：loop guard（3/5/8）、task-state/task-round、自主轮已覆盖 DSH 的 reminders/todo/goal 主要用途；
  plan mode、turn-stopping、jobs 交付历史未迁移（C9），chess 不需要，建议明确标注而非补实现。
- **可观测性族**：chagent 的事件（`agent.context.compress`/`agent.session.summarize` 等）能支撑排障与报告，
  但不能从事件重建 surface/锚点；与 C2 合并后收益最大。

## 6. 证据索引

- `build/functest-20261030/analysis/context-parity/dsh-mechanism-lines-part1..4.txt`：
  DSH token-meter/compaction/pruner/spill/session-surface/system-prompt/agent-loop/plan/goal 关键行摘录。
- `build/functest-20261030/analysis/context-parity/chagent-mechanism-lines-part1..2.txt`：
  chagent context_budget/context_legacy/pruner/spill/meter/pressure/prestep/pipeline/context 等关键行摘录。
- `build/functest-20261030/analysis/context-parity/defect-matrix.md`：本文件 §4 的机器可读清单。
- 真实场景证据（非本轮）：`build/functest-20261030/context/{results.json,summary.md}`（task-15 修复后复跑）；
  `build/functest-20261030/analysis/context-parity/go-test-*.log`（本轮定向单测日志）。
- 已知修复结论：`docs/design/agent-functional-test-report.zh.md` §7/§14；
  `docs/design/dsh-migration-report.zh.md` §2.1/§4/§7。
