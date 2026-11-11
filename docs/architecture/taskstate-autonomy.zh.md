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

## 相关文档

- [工作区上下文](workspace-context.zh.md)
- [运行时上下文](runtime-context.zh.md)
- [工具参考](../reference/tools.zh.md)
- [Chess 最小配置](../guides/chess-config.zh.md)