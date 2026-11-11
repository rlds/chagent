# 运行时上下文（Runtime Context）

> 适用范围：Chagent chess 基础 agent 的运行时状态（时间/平台/工作区/会话/发送者）
> 如何到达模型，以及 P-08 运行时快照机制。
> 关键代码路径：`pkg/agent/runtime_context.go`、`pkg/agent/workspace_context_runtime.go`、
> `pkg/config/context_config.go`。
> 核对日期：2026-10-29

运行时上下文把每轮会变化的“事实”注入给模型：当前时间、平台、工作区/项目根、
会话（通道/会话 ID）、当前发送者。它只包含**会变化的**事实，系统提示词保持稳定。

## 配置（agents.defaults.runtime_context）

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `mode` | `"snapshot"` | `snapshot`：值变化时追加一条持久化上下文消息；`system`：渲染进系统提示词（旧行为） |
| `refresh_interval_seconds` | `600` | 快照刷新间隔（秒） |
| `include_sender` | `true` | 是否包含当前发送者信息 |

环境变量前缀：`CHAGENT_AGENTS_DEFAULTS_RUNTIME_CONTEXT_*`。
`Validate()` 只接受 `system` / `snapshot` 两种模式，`refresh_interval_seconds` ≥ 0。

## P-08 运行时快照（pkg/agent/runtime_context.go）

- `BuildRuntimeSnapshot(channel, chatID, senderID, senderDisplayName, workspace,
  projectRoot, includeSender, now)`：渲染中文 `<runtime_context>` 块，包含：
  - `当前时间：<RFC3339>（<时区>）`
  - `平台：<GOOS>/<GOARCH>`
  - `工作区：<workspace>（项目根：<projectRoot>）`（相同则只显示工作区）
  - `会话：通道=<channel>，会话=<chatID>`
  - `当前发送者：<senderID>/<senderDisplayName>`（`includeSender` 时）
- `RuntimeSnapshotDigest(...)`：对**非时间**事实计算 SHA-256 摘要；时间被刻意排除，
  让驱动在刷新间隔内不把时钟当作语义变化。

## 持久化与刷新（pkg/agent/workspace_context_runtime.go）

- `persistRuntimeContextMessage(...)`：把快照作为持久化上下文消息写入会话，
  保证 replay/resume/compaction 都能看到它（与用户消息同等待遇）。
- 刷新策略：仅在摘要变化（或到刷新间隔）时追加新的快照消息；摘要不变不追加。
- 与工作区指令注入同属 `workspace_context_runtime.go`，共享触碰记录与
  持久化通道。

## 两种模式的差异

| 模式 | 写入位置 | 生命周期 | 适用场景 |
| --- | --- | --- | --- |
| `system` | system prompt | 每轮重建 | 需要稳定系统提示词中携带运行时的旧行为 |
| `snapshot` | 会话消息流 | 值变化时追加持久化消息 | 默认；replay/resume/compaction 可见 |

## 相关文档

- [工作区上下文](workspace-context.zh.md)
- [任务状态与自主](taskstate-autonomy.zh.md)
- [代码地图](code-map.zh.md)
- [Chess 最小配置](../guides/chess-config.zh.md)