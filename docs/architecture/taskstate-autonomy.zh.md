# 任务状态与自主（TaskState & Autonomy）

> 适用范围：Chagent chess 基础 agent 的会话任务状态机与自主续跑驱动。
> 关键代码路径：`pkg/taskstate/`、`pkg/agent/autonomy_runtime.go`、
> `pkg/agent/taskstate_context.go`、`pkg/tools/integration/task_*.go`、`todo_write.go`。
> 核对日期：2026-10-29

会话任务状态（TaskState）是自主续跑驱动与任务控制工具共享的持久化状态。
该包刻意自包含（只依赖标准库），便于被 `pkg/memory`、`pkg/session`、
`pkg/tools`、`pkg/agent` 引用而不产生循环依赖。

## 状态模型（pkg/taskstate/types.go）

- `StateVersion = 1`：当前写入版本。
- `Phase`：`active` / `paused` / `blocked` / `complete`。
- `Todo`：`{content, status}`，`status` ∈ `pending` / `in_progress` / `completed`
  （保持为字符串以便与 prompt 规范一致）。
- `BlockedReason`：`{code, message}`，code ∈ `round-limit` / `no-progress` /
  `model-reported`。
- 常量：`DefaultMaxRounds=20`、`MaxRoundsLimit=200`、
  `DefaultNoProgressRounds=3`、`DefaultBlockedThreshold=3`。

## 状态机（pkg/taskstate/transitions.go）

- 合法转换：`active` → `paused`/`blocked`/`complete`；`paused` → `active`；
  `blocked` → `active`（需直接人类请求）。
- `task_update` 工具按配置的 `blocked_threshold`（默认 3 轮同一条件持续）才允许
  标记 `blocked`；`complete` 需要提供证据。
- `State` 提供 `ShouldContinue()`、`AdvanceRound()`、`RecordNoProgressRound()`、
  `SetRoundLimitBlocked()`、`SetNoProgressBlocked()` 等接口。

## 自主驱动（pkg/agent/autonomy_runtime.go）

- `renderTaskRoundPrompt()`：渲染中文 P-10 轮次 prompt：
  `目标 / 轮次 / 继续在同一会话推进 / 以工作区、工具结果、持久状态为准 /
  采取具体行动并验证 / 完成前收集证据 / 保持 active 或遵循阻塞策略`。
- `advanceAutonomyRound()`：应用 P-18 进程守卫：
  1. `ShouldContinue()` 为假 → 不调度；
  2. `cfg.MaxRounds` 限制 `state.MaxRounds`（上限 200）；
  3. 达到轮次上限 → `round-limit` 阻塞；
  4. 一轮未刷新 `LastProgressAt` → 记一次无进展，达到阈值 → `no-progress` 阻塞；
  5. 通过守卫后 `AdvanceRound()` 并返回“应调度新轮次”。
- `maybeScheduleAutonomyRound()`：上一轮正常结束（非 aborted/error）、
  深度为 0、无 `NoHistory`、存在 active TaskState 时，异步调度下一轮，
  每轮从全新 turn 开始（不扭曲 `max_tool_iterations` 与压缩边界）。

## 配置（agents.defaults.autonomy）

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 自主续跑开关 |
| `max_rounds` | `20` | 每会话轮次上限（1–200） |
| `round_delay_ms` | `1000` | 轮次间隔 |
| `no_progress_rounds` | `3` | 无进展判定阈值（1–10） |
| `resume_on_start` | `false` | 进程启动时不自动续跑 |
| `send_intermediate` | `false` | 是否把中间轮次结果发给用户 |
| `allow_parallel_in_progress` | `false` | 是否允许并行 in-progress 任务 |

环境变量前缀：`CHAGENT_AGENTS_DEFAULTS_AUTONOMY_*`。

## 任务工具

- `task_get`（`pkg/tools/integration/task_get.go`）：读取目标、阶段、轮次、待办、
  证据、阻塞原因。
- `task_update`（`pkg/tools/integration/task_update.go`）：`start` 需要直接人类或
  调度器请求；`complete` 需要证据；`block` 需要同一条件连续 N 轮。
- `todo_write`（`pkg/tools/integration/todo_write.go`）：维护待办清单。
- `task_authority.go`：工具权限/来源检查。

工具在 `pkg/agent/instance.go` 中按 `tools.enabled.task` / `tools.enabled.todo`
注册（`TaskToolOptions` 把 `BlockedThreshold`、`DefaultMaxRounds`、
`AllowParallelInProgress` 传入）。

## 任务文件协议（默认 system prompt）

P-21 `kernel.task_policy`、P-22 `kernel.memory_policy` 与 P-23 `kernel.role_policy`
（`pkg/agent/prompt_policy.go`）在默认 system prompt 中生效，把会话级任务板落到工作区文件：

- `tasks/TASKS.md`：只记录未完成任务；每条含编号（`TASK-YYYYMMDD-NNN`）、创建时间
  （RFC3339，带时区）、来源、身份类型与角色/职责（必填）、公司类型（如已判定，可选）、
  目标、验收标准、状态（`pending` / `in_progress` / `blocked` / `completed` / `abandoned`）
  与证据。
- `tasks/TASK_LOG.md`：全部任务完成后，已完成条目移入此处归档（含身份类型与角色/职责、
  公司类型（如已判定））；判定不执行的遗留任务标记 `abandoned` 并写明原因后也归档。
- 用户在执行期间新增输入时，模型先判定“当前任务补充/修正”还是“新需求任务”：
  补充合入原任务，新需求登记为新任务。
- 角色判定（P-23）默认注入：执行或整理任务前**必须**判定任务对应的身份类型、角色与职责
  （软件开发/前端/后端、架构、SRE/运维、硬件/嵌入式、产品设计/开发、运营、商品运营、
  销售、市场公关、政府关系、财务、投资融资、审计、法务、人力、管理层等），
  明确职责、权限边界与验收责任；行业/公司类型（电商、生鲜电商、半导体、芯片设计、AI、
  机械设计、无人机、机器人、餐饮等）是在身份类型基础上的**可选**补充判定，不要求必须判定；
  角色随任务改变，按角色职责、已判定行业/公司类型对应的行业标准与专业标准选用技能与工具，
  并对判定与结果负责。
- 遗留任务按创建时间判定：7 天内或仍相关/依赖未变 → 继续；
  超过 7 天且无进展、前置条件或依赖已变化 → 可判定不执行；
  判定结果必须在新的输出开头列出（编号、目标、判定、理由），继续执行的任务在最终输出
  中说明额外完成的内容。

状态必须与 TaskState、`todo_write` 一致：TaskState 是会话持久状态，任务文件是跨轮次/跨会话
可读的任务板，二者不能互相矛盾。`task_update` / `todo_write` 不可用时，提示词只保留通用任务
判定，不注入依赖具体工具的协同句。

## 相关文档

- [工作区上下文](workspace-context.zh.md)
- [运行时上下文](runtime-context.zh.md)
- [工具参考](../reference/tools.zh.md)
- [Chess 最小配置](../guides/chess-config.zh.md)