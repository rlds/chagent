# Agent 上下文 Prompt 设计规范（DSH 借鉴与 Chagent 适配）

> 状态：提案（Proposal）  
> 日期：2026-10-28  
> 关联文档：
> - [工作区上下文与自主连续性改进计划](agent-workspace-context-plan.zh.md)
> - [详细任务书](agent-workspace-context-task-book.zh.md)
> - DSH 参考仓库：`../../deepseek-harness/`

> **v0.2 范围变更**：Chagent 是 chess 项目的基础 agent。本文的 Prompt 不得假设存在非 pico 通道、Chagent Web UI、硬件/音频/设备工具或 chess 未使用的 provider；所有“可选/不实现”的标注以 [计划文档](agent-workspace-context-plan.zh.md) §6.14 裁剪清单为准。

> **v0.3 语言要求**：所有新增/改造的模型可见 Prompt 使用**中文**；原 Chagent 的英文 system prompt、工具描述、上下文模板在实现时翻译为中文（代码标识符/协议字段/文件路径保持原样）。DSH 原文英文块仅作对照参考，不直接上线；实现以 **§4.10 中文 Prompt 定稿** 为准。英文术语（workspace、pico、tool call 等）可保留在中文句中。

---

## 1. 目的与原则

本文把 DSH 中经过验证的 prompt / context 设计提炼成 Chagent 可直接实现的规范。它回答四个问题：

1. 哪些内容应该进入 **system prompt**，哪些应该作为 **durable context** 追加？
2. 工作区指令、运行时事实、任务状态、技能目录分别用什么 prompt 形态？
3. 这些 prompt 在什么时机注入、如何避免重复、如何处理更新/删除/预算/转义？
4. 如何用 golden test 保证 prompt 不被后续重构悄悄改坏？

### 1.1 设计原则

| # | 原则 | 说明 |
|---|---|---|
| P1 | 稳定前缀优先 | 身份、硬规则、能力说明放 system prompt；值/文件内容/任务状态放 context |
| P2 | 上下文是历史事实 | context 用普通 user-role 消息，带类型化 source，可 replay/resume |
| P3 | 变化才追加 | baseline 一次；delta 只在 set/replace/remove 时出现；不变不重复 |
| P4 | 语义显式 | 开头说明“这是工作区指令/附加/更新/删除”，不要依赖隐藏标记 |
| P5 | 优先级显式 | 更具体文件优先；不得覆盖 system/developer/direct user 指令 |
| P6 | 有界 | 每链预算、单文件上限、UTF-8 安全截断、可见 notice |
| P7 | 不可信内容 | 转义 frame 结束标记；不把指令内容当系统命令；不记录敏感内容 |
| P8 | 完成/停止有证据 | 不以空响应/无 tool call/迭代上限作为完成；blocked 有阈值 |
| P9 | 工具与 prompt 一致 | 工具描述、schema、system policy、context 中的规则必须一致 |
| P10 | 可测试 | 每段 prompt 有稳定的关键短语/顺序断言；prompt 变更有 golden diff |

---

## 2. Prompt 分层与顺序

### 2.1 目标请求形态

最终仍保持 Chagent 的 provider 兼容形态：

```text
messages = [
  system: 稳定身份 + 硬规则 + 工具/自主/上下文策略,
  ...history: 过去的 user/assistant/tool + 旧的 context 消息,
  context: 工作区指令 baseline/delta,
  context: runtime snapshot,
  context: task state (required),
  context: skill catalog,
  user: 当前用户消息 / steering / task_round
]
```

命名约定：

- `Context.Kind` 使用 kebab-case：`workspace-instructions`、`runtime-context`、`task-state`、`task-round`、`skill-catalog`；
- 模型可见标签使用下划线：`<system-reminder>`、`<task_state>`、`<task_round>`、`<skill_content>`；
- 两者不要混用，避免对账时字符串不匹配。

约束：

- **只有一条 system message**（Anthropic/Codex 兼容）；
- context 消息是 `role: "user"`，但在元数据上标记 `Context.Kind`；
- 最新 context 在 history 之后、当前 user 之前（Chagent 选择；DSH 把 baseline 折在 claimed user 之后，二者都是 append-only，差异只影响同一 turn 的局部顺序）；
- `task_round` 是 autonomous 轮次的 user 消息，排在 task state 之后；
- 不把 context 放进 system prompt，除非配置显式回退 `runtime_context.mode="system"`。

### 2.2 分层表

| 层 | 内容 | 交付 | 变化频率 | 预算 |
|---|---|---|---|---|
| L0 身份/硬规则 | chagent identity、workspace、工具规则、安全规则 | system | 低 | 固定 |
| L1 策略 | 自主契约、工作区指令策略、任务完成/阻塞策略、任务判定/记录、角色判定/履职与记忆维护策略 | system | 低 | 固定 |
| L2 能力 | tool schemas、skill catalog 摘要 | system/tools 或 context | 中 | 固定/可配 |
| L3 工作区指令 | baseline、additional、updated、removed | durable context | 首次/触碰 | 每链 maxBytes |
| L4 运行时 | 时间、workspace、session、sender | durable snapshot | 变化才追加 | 小 |
| L5 任务 | TaskState（objective/phase/round/todos/blocker/evidence）+ 文件任务板 `tasks/TASKS.md` / `tasks/TASK_LOG.md` | durable context（TaskState）/ 工作区文件（任务板） | 任务变化 | 小 |
| L6 技能 | 技能 catalog（name+description） | durable context | 成员变化 | 可配 |
| L7 对话 | 用户消息、steering、task_round、工具结果 | history | 每轮 | 由压缩管理 |

### 2.3 Prompt 扩展机制

Chagent 已有 `PromptRegistry` / `PromptContributor`（[`pkg/agent/prompt.go:321`](../../pkg/agent/prompt.go#L321)），但所有 part 都进 system。为了支持“工作区指令/运行时/任务/技能”这些不同生命周期的 context，建议增加 context source 扩展点：

```go
type ContextSource interface {
    ContextSource() PromptSourceDescriptor
    ContributeContext(ctx context.Context, req PromptBuildRequest) ([]ContextContribution, error)
}

type ContextContribution struct {
    Text     string
    Meta     providers.ContextMeta
    Required bool
    Order    int
}
```

扩展规则：

1. **先选交付形态**：
   - 值无关、长期稳定、必须出现在前缀 → system section；
   - 值/文件内容/任务状态、需要变化才追加/可回放 → context source。
2. **声明 source**：`ID`、`Owner`、`Description`、`Allowed`、`StableByDefault`；重复 ID 拒绝。
3. **声明顺序**：context 按 `Order` 升序；同 order 按 source ID 稳定排序；不允许把 system 的 layer/slot 顺序直接套到 context。
4. **声明 required**：`Required=true` 的 context 在“关闭可选运行时上下文”时仍保留（任务状态、工作目录等操作事实）。
5. **只发变化**：source 自己负责 digest/可见状态比较；框架只做空文本过滤和最终排序。
6. **安全**：source 输出必须经过统一 escape/预算；不允许 source 自己拼 frame 结束标记。
7. **测试**：每个 source 至少一个 golden；新增 source 不得改变未注册时的请求。

扩展示例（运行时快照）：

```go
type runtimeContextSource struct{ builder *RuntimeContextBuilder }

func (s *runtimeContextSource) ContextSource() PromptSourceDescriptor {
    return PromptSourceDescriptor{
        ID: "runtime.context", Owner: "agent",
        Description: "Current time/session/workspace snapshot",
        Allowed: []PromptPlacement{{Layer: PromptLayerContext, Slot: PromptSlotRuntime}},
        StableByDefault: false,
    }
}

func (s *runtimeContextSource) ContributeContext(
    _ context.Context, req PromptBuildRequest,
) ([]ContextContribution, error) {
    text := s.builder.Build(req)
    if text == "" { return nil, nil }
    return []ContextContribution{{
        Text: text, Order: 200, Required: true,
        Meta: providers.ContextMeta{Kind: "runtime-context", Form: "snapshot"},
    }}, nil
}
```

这样“扩展 prompt”不是改 `BuildSystemPromptParts` 的硬编码，而是注册一个有序、可测试、可回放的 source。

---

## 3. Prompt 清单

| ID | 名称 | 交付 | 触发 | Required | 主要用途 |
|---|---|---|---|---|---|
| P-01 | Autonomy contract | system | 始终（autonomy 启用时） | — | 禁止提前停止，定义完成/阻塞 |
| P-02 | Workspace instruction policy | system | 始终（workspace_context 启用时） | — | 说明 context 的优先级/更新语义 |
| P-03 | Workspace baseline | context | 首次/resume identity 变化 | 否 | 完整工作区规则 |
| P-04 | Additional instruction | context | 触碰后新目录 | 否 | 嵌套规则 |
| P-05 | Updated instruction | context | 已加载文件内容变化 | 否 | 替换旧规则 |
| P-06 | Removed instruction | context | 文件消失/变成重复 | 否 | 告知旧规则失效 |
| P-07 | Budget notice | context（frame 内） | 省略/截断 | 否 | 透明预算 |
| P-08 | Runtime snapshot | context | 值变化/时间间隔 | 是 | 当前时间/会话/工作区 |
| P-09 | Task state | context | TaskState 存在 | 是 | 目标/阶段/轮次/待办 |
| P-10 | Task round | user | 自主续跑 | — | 开始新一轮 |
| P-11 | Completion/blocker policy | system | 始终（autonomy 启用时） | — | 完成证据/阻塞阈值 |
| P-12 | `todo_write` description | tool schema | 工具可见 | — | 计划/进度 |
| P-13 | `task_get`/`task_update` description | tool schema | 工具可见 | — | 目标状态控制 |
| P-14 | Skill catalog + `skill` description | context/tool | 技能可见 | 否 | 渐进披露 |
| P-15 | Goal policy（可选） | system | goal 工具启用 | — | 兼容 DSH goal 语义 |
| P-16 | File reference guidance（Chess 不实现） | system | — | — | 仅未来参考 |
| P-17 | Working directory context（Chess 不实现） | context | — | — | 仅未来参考 |
| P-18 | No-progress continuation | context | 空响应/迭代上限/无进展 | — | 不静默停止 |
| P-19 | Completion evidence checklist | context/system | 完成前 | — | 验证完成 |
| P-20 | Chess base-agent role | system | 始终（chess profile） | — | 说明 workspace 由 chess 同步、只有 pico 通道、不要假设 server API/其他 agent |
| P-21 | Task determination / file protocol | system | 文件工具可用时 | — | 判定任务、tasks/TASKS.md 记录、新增输入归类、完成归档、遗留任务判定 |
| P-22 | Memory hygiene | system | 文件工具可用时 | — | 记忆只保留核心内容，清理历史/临时/已恢复异常并说明 |
| P-23 | Role and identity determination | system | 默认始终 | — | 必须判定任务对应的身份类型、角色与职责；行业/公司类型（电商、生鲜电商、半导体、芯片设计、AI、机械设计、无人机、机器人、餐饮等）为可选补充判定；按角色职责与专业标准执行并对判定与结果负责 |

---

## 4. Prompt 详述

### P-01 Autonomy contract

**目的**：把“持续执行直到完成”变成模型可见的硬约束，避免模型因为一个步骤结束、工具没有新信息、或不确定下一步而停止。

**注入**：system prompt，`kernel.identity` 之后、工具策略之前；只在 `autonomy.enabled=true` 时出现，避免影响普通聊天。

**Chagent 适配文（英文，模型可见）**：

```markdown
## Autonomy

You operate autonomously until the task is complete. Do not stop merely because a step finished, a tool returned no new information, or you are unsure what to do next. Re-read the task state and the workspace, then continue with the next concrete action.

Stop only when one of these is true:
1. The task state is marked complete and verifiable evidence shows the whole objective is achieved.
2. The user explicitly stops, pauses, or replaces the task.
3. The same concrete blocking condition has persisted for at least 3 consecutive rounds and cannot be resolved with the available tools; record it in blocked_reason.
4. A configured resource limit is reached; record a checkpoint and report the limit.

Never claim completion without evidence. Never ask for permission for actions already allowed by the current policy. If you are unsure, inspect the workspace, tool results, and task state instead of stopping.
```

**DSH 依据**：goal-round prompt 要求“Treat the current workspace, tool results, and durable session state as authoritative… Make concrete progress and verify the result… Before claiming completion, gather evidence”；goal policy 要求“Mark complete only when the objective is actually achieved… blocked only after the same condition persists for at least 3 consecutive goal rounds”（[tool-goal README](../../../deepseek-harness/packages/goal/tool-goal/README.md)）。

**测试**：断言包含 `Stop only when`、`Never claim completion without evidence`、`at least 3 consecutive rounds`；普通聊天（autonomy off）不出现。

---

### P-02 Workspace instruction policy

**目的**：告诉模型 context 中的工作区指令是什么、优先级如何、更新/删除时以哪条为准。

**注入**：system prompt，简短；不重复具体文件内容。

**Chagent 适配文**：

```markdown
## Workspace instructions

Workspace instructions arrive as separate context messages. More specific files take precedence over broader ones, and they never override system, developer, or direct user instructions. Treat the most recent instruction snapshot as current: an Updated notice replaces the earlier text for that file, and a Removed notice means the earlier text no longer applies.
```

**DSH 依据**：baseline intro 原文：

```markdown
The following workspace instructions may be relevant to your work. Use them as guidance when applicable. More specific instructions take precedence over broader ones. They do not override system, developer, or direct user instructions.
```

**测试**：断言 `More specific files take precedence`、`never override system`、`Removed notice`。

---

### P-03 Workspace baseline

**目的**：首次请求或 resume identity 变化时，一次性提供用户级 + 项目链的完整规则。

**注入**：durable user-role context；在 history 之后、当前 user 之前；`Baseline=true`。

**模板**：

```markdown
<system-reminder>
The following workspace instructions may be relevant to your work. Use them as guidance when applicable. More specific instructions take precedence over broader ones. They do not override system, developer, or direct user instructions.

Instructions from: ~/.chagent/AGENTS.md

<content>

Instructions from: AGENTS.md

<content>
</system-reminder>
```

**替换 baseline 模板**（identity 不兼容、旧 baseline 可见时）：

```markdown
<system-reminder>
This complete workspace instruction baseline replaces all earlier workspace instruction baselines. The following workspace instructions may be relevant to your work. Use them as guidance when applicable. More specific instructions take precedence over broader ones. They do not override system, developer, or direct user instructions.

Instructions from: AGENTS.md

<content>
</system-reminder>
```

**空替换模板**：

```markdown
<system-reminder>
This complete workspace instruction baseline replaces all earlier workspace instruction baselines. No workspace instructions are currently active.
</system-reminder>
```

**规则**：

- **Chess 默认 `user_global=false`**：baseline 只包含 chess workspace 的项目链；下面的 `~/.chagent` / `~/.agents` 段落仅在显式开启用户级时出现；
- `<content>` 是版式占位符，实际渲染时直接放文件正文，不保留占位符；
- 一个 baseline 只包含一次；之后只发 delta；
- 用户级在前，项目链从宽到具体；
- 用户级显示路径归一化为 `~/.chagent/AGENTS.md` / `~/.agents/AGENTS.md`；自定义 `CHAGENT_HOME` 时使用对应 home 的显示名；
- 同组 trim 内容重复只保留最早一份；
- 内容中的 `</system-reminder>` 必须转义；
- baseline 消息的 `Context.Changes` 只记录**实际渲染/保留**的文件的 `set`，便于 resume 对账；被预算省略的文件不写入 changes。

**测试**：顺序、去重、空替换、identity 变化、转义。

---

### P-04 Additional instruction

**目的**：模型触碰/读取了更深目录后，补充该目录适用的规则。

**注入**：durable context delta；`action=set`。

**模板**：

```markdown
<system-reminder>
Additional instructions from: packages/app/AGENTS.md

These instructions apply to work under `packages/app`. Use them as guidance when relevant; more specific instructions take precedence. They do not override system, developer, or direct user instructions.

<content>
</system-reminder>
```

**用户级补充**（如果用户级文件在 baseline 之后才出现）：

```markdown
<system-reminder>
Additional instructions from: ~/.agents/AGENTS.md

These user-global instructions apply to all work. Use them as guidance when relevant; more specific instructions take precedence. They do not override system, developer, or direct user instructions.

<content>
</system-reminder>
```

**测试**：scope 句子使用相对目录；用户级使用 user-global 句子；不出现内部 scope key。

---

### P-05 Updated instruction

**目的**：已加载文件内容变化后，明确替换旧内容，避免模型同时遵循新旧规则。

**注入**：durable context delta；`action=replace`。

**模板**：

```markdown
<system-reminder>
Updated instructions from: packages/app/AGENTS.md

This file changed after it was loaded. Use the following content instead of the previously loaded instructions from this file.

<content>
</system-reminder>
```

**规则**：

- digest 相同不发；
- 文件被预算丢弃后又变化，可重新尝试；
- 同一批次多个文件更新合并为一条消息。

**测试**：修改文件后出现 `Updated instructions from`；新消息明确替代旧内容（历史中的旧消息仍在，但模型被指示以新内容为准）。

---

### P-06 Removed instruction

**目的**：文件删除或变成重复时，明确告诉模型旧规则失效。

**注入**：durable context delta；`action=remove`。

**模板**：

```markdown
<system-reminder>
Instructions removed: packages/app/AGENTS.md

The previously loaded instructions from this file no longer apply.
</system-reminder>
```

**规则**：

- 删除是确定事实；读取失败不是删除；
- 重复候选被移除时也发 removal；
- 同一批次多个删除合并为一条消息。

**测试**：删除后出现 `Instructions removed`；权限错误不产生 removal。

---

### P-07 Budget notice

**目的**：预算省略/截断时透明告知，不让模型以为自己看到了完整规则。

**注入**：放在同一个 `<system-reminder>` frame 的顶部。

**模板**：

```text
Workspace instruction budget 32768 bytes: omitted packages/app/AGENTS.md; truncated AGENTS.md from 40000 to 12000 bytes
```

**规则**：

- 每链独立预算；合并消息最多 `2 * maxBytes`；
- 宽文件先整体丢弃；最具体文件可截断；
- 截断按 UTF-8 rune 边界；
- notice 本身也转义。

**测试**：超预算、只截断、只省略、两链合并、极小预算降级。

---

### P-08 Runtime snapshot

**目的**：把时间/会话/工作区等会变化的事实移出 system prompt，只在变化时追加。

**注入**：durable context snapshot；`required=true`；变化才追加。

**模板**：

```markdown
Current time: 2026-10-28T16:15:55+08:00 (Asia/Shanghai).
Workspace: "/Users/me/project" (project root: "/Users/me/project").
Session: channel=telegram, chat=12345.
Sender: Alice (alice@example.com).
```

**规则**：

- 时间精确到分钟或秒由配置决定；默认 10 分钟最小间隔；
- session/sender 变化时追加；
- 没有变化不重复；
- 不把 snapshot 文本写入 system prompt；
- 如果未来支持 `cd`，追加一行 `Current working directory: "<path>".`

**DSH 依据**：time-context 三行读数、working-directory 的 required context、harness-dev-context 的“值不进 system prompt”。

**测试**：同一分钟内不重复；时间推进后追加；system prompt hash 不变。

---

### P-09 Task state

**目的**：让模型在每一轮都看到持久目标、阶段、轮次、待办和阻塞原因，避免压缩/重启后失忆。

**注入**：durable context，`required=true`；在 runtime snapshot 之后、skill catalog 之前。

**模板**：

```markdown
<task_state>
Objective: "Refactor the provider layer and keep all tests green"
Phase: active
Round: 2/20
Todos:
- [completed] Inventory existing provider interfaces
- [in_progress] Introduce the common protocol types
- [pending] Migrate Anthropic adapter
Last checkpoint: 2026-10-28T16:10:00+08:00
Evidence:
- `pkg/providers/protocoltypes/types.go` added
Blocked reason: none
</task_state>
```

**规则**：

- `Phase` 为 `blocked` 时必须显示 `Blocked reason`（code + message）；
- `complete` 时可显示最终 evidence；
- 没有 TaskState 时不注入（普通聊天）；
- 任务状态变化才追加；`todo_write`/`task_update` 之后下一轮生效；
- 与 P-10 的 `Round` 保持一致；
- 长任务轮次很多时，可只在 objective/phase/todos/evidence/blocker 发生实质变化时追加 TaskState，把 `Round` 放在每轮的 P-10 `task_round` 中，避免每轮重复一大段状态；
- TaskState 渲染有限长：objective/evidence 超长时截断并保留关键路径，避免任务状态本身撑爆上下文。

**测试**：字段顺序、phase 渲染、blocked reason、无任务不出现。

---

### P-10 Task round

**目的**：自主续跑时，给模型一个明确的“新一轮”指令，不伪造用户消息。

**注入**：user-role 消息，`Context.Kind="task-round"`；排在 task state 之后；`SendResponse=false`。

**Chagent 适配文**：

```markdown
<task_round>
Objective: "Refactor the provider layer and keep all tests green"
Round: 3/20

Continue working toward the objective in this same session. Treat the current workspace, tool results, and durable task state as authoritative; inspect them instead of assuming earlier narration is still current. Make concrete progress and verify the result. Before claiming completion, gather evidence that the whole objective is achieved, read the current task state, and mark it complete. If work remains, leave the task active for the next round. Follow the configured completion and blocker policy before reporting a blocker.
</task_round>
```

**DSH 原文**：

```markdown
<goal_round>
Objective: "<objective>"
Round: <n>/<maxGoalRounds>

Continue working toward the objective in this same session. Treat the current workspace, tool results, and durable session state as authoritative; inspect them instead of assuming earlier narration is still current. Make concrete progress and verify the result. Before claiming completion, gather evidence that the whole objective is achieved, read the current goal, and mark it complete. If work remains, leave the goal active for the next round. Follow the configured goal-tool policy before reporting a blocker.
</goal_round>
```

**测试**：包含 objective/round；不包含用户名/伪造来源；只有 active task 才生成。

---

### P-11 Completion/blocker policy

**目的**：把“完成”和“阻塞”变成有证据、有阈值的动作，而不是模型的一句话。

**注入**：system prompt，与 P-01 相邻；或作为 `task_update` 工具描述的一部分（两处必须一致）。

**Chagent 适配文**：

```markdown
Mark the task complete only when the objective is actually achieved and the workspace or tool results contain evidence for every todo. Cite the evidence in the task update. Mark the task blocked only after the same concrete blocking condition persists for at least 3 consecutive rounds and cannot be resolved with available tools; report the condition in blocked_reason. Difficulty, uncertainty, or useful remaining work is not blocked. Do not end a round with neither completion nor a new blocker.
```

**DSH 原文**：

```markdown
Mark complete only when the objective is actually achieved. Mark blocked only after the same blocking condition persists for at least 3 consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, or useful remaining work is not blocked.
```

**测试**：`at least 3 consecutive rounds`、`Difficulty, uncertainty, or useful remaining work is not blocked`；工具拒绝不足 3 轮的 block。

---

### P-12 `todo_write` tool description

**目的**：给模型一个稳定的计划面，显示当前做什么、还差什么。

**Schema**：

```json
{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The COMPLETE task list, replacing any previous list.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "content": {
            "type": "string",
            "description": "What the task is — a short imperative line."
          },
          "status": {
            "type": "string",
            "description": "pending (not started) | in_progress (now) | completed (done).",
            "enum": ["pending", "in_progress", "completed"]
          }
        },
        "required": ["content", "status"]
      }
    }
  },
  "required": ["todos"]
}
```

**描述（DSH 原文）**：

```markdown
Record and update a task list to plan multi-step work and show progress; skip it for trivial single-step tasks. Add one todo per concrete step before you start. While work remains, keep the todos being worked on `in_progress`, several only when work runs in parallel. Mark each todo `completed` as soon as it is done.
```

**成功结果**：

```text
Updated todo list: 2 pending, 1 in progress, 3 completed.
```

**失败结果**：

```text
Error: invalid todo: `content` must be a non-empty string
Error: invalid todos: duplicate content "<content>"
Error: todo_write requires an owning agent session
Error: invalid todos: at most one task may be in_progress (got <n>)
```

Chagent 用 `Autonomy.AllowParallelInProgress` 控制是否允许多个 `in_progress`；默认 `false`，与 DSH 的 `allowParallelInProgress` 语义一致。由于本设计要求先有 TaskState，Chagent 可在描述末尾追加一句：

```markdown
If no task state exists for this session, call task_update start first.
```

**测试**：整表替换、重复内容拒绝、空内容拒绝、并行开关、结果计数。

---

### P-13 `task_get` / `task_update` tool description

**目的**：让模型读取/修改持久目标状态；把完成/阻塞的权威检查放在工具执行时，而不是只靠 prompt。

**`task_get` schema**：

```json
{
  "type": "object",
  "properties": {}
}
```

**描述**：

```markdown
Read the current session task state: objective, phase, round, todos, evidence, and blocked reason. Use it before updating the state or when you are unsure what remains.
```

**结果**（紧凑 JSON，字段稳定）：

```json
{"task":{"objective":"...","phase":"active","round":2,"max_rounds":20,"todos":[{"content":"...","status":"in_progress"}],"evidence":["..."],"blocked_reason":null}}
```

**`task_update` schema**：

```json
{
  "type": "object",
  "properties": {
    "action": {
      "type": "string",
      "description": "start, pause, resume, complete, or block. start requires a direct human or scheduler request. complete requires evidence. block requires the same concrete condition for at least 3 consecutive rounds.",
      "enum": ["start", "pause", "resume", "complete", "block"]
    },
    "objective": {
      "type": "string",
      "description": "Objective; required for start, optional for resume edits."
    },
    "max_rounds": {
      "type": "integer",
      "description": "Optional round cap for start/resume."
    },
    "blocked_reason": {
      "type": "string",
      "description": "Required for block: the concrete condition that persisted across rounds."
    },
    "evidence": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Concrete evidence for complete: file paths, test output, command results, or user confirmation."
    }
  },
  "required": ["action"]
}
```

**描述**：

```markdown
Update the current session task state. Use start to create a task from an explicit human or scheduler request; pause/resume/complete/block to change its phase. Resuming a blocked task requires a direct human request. Do not mark complete without concrete evidence. Do not mark blocked before the same condition has persisted for at least 3 consecutive rounds.
```

**测试**：权限、phase 转换、evidence 必填、blocked 阈值、round 上限、结果 JSON 稳定。

---

### P-14 Skill catalog + `skill` tool description

**目的**：渐进披露技能正文，避免全量 catalog/正文进入 system prompt；让模型按需加载。

**Catalog 模板（DSH 原文）**：

```markdown
<system-reminder>
A skill is a reusable set of task-specific instructions. The following skills are available in this session:

<available_skills>
- `<name>`: <normalized-and-capped-description>
</available_skills>

If the user names a skill, or the task clearly matches a skill's description, call the `skill` tool with the exact skill name before taking task actions. Load all applicable skills, then follow their full instructions. This catalog contains summaries only; do not infer or follow a skill's instructions until it has been loaded.
A user may also invoke a skill directly; its <skill_content> block then appears in this conversation. Follow it, and do not call the `skill` tool again for that skill.
</system-reminder>
```

**`skill` tool description（DSH 原文）**：

```markdown
Load the full instructions for a skill. Call it before acting on a task that names or clearly matches a skill in the session skill catalog.
```

**Schema**：

```json
{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "The exact skill name from the available skills list."
    }
  },
  "required": ["name"]
}
```

**Chagent 适配**：

- 保留现有 `find_skills` / `install_skill`（市场发现/安装）；
- 新增本地 `skill` 加载器，读取 workspace/global/builtin 的 `SKILL.md`；
- catalog 改为 durable context，成员变化时整表替换；
- 现有全量 `capability.skill_catalog` 作为 fallback 保留一个版本，验证后移除；
- `skill` 结果使用统一 `<skill_content>` 包裹，正文转义；
- 如果 Chagent 暂不支持 `/name` 显式调用，删除 catalog 模板最后一句（“A user may also invoke...”），避免承诺不存在的能力。

**测试**：catalog 只含 name+description；加载后返回正文；未知/禁用技能报错；catalog 变更替换；移除后空 catalog 退役旧名。

---

### P-15 Goal policy（可选）

如果未来实现 DSH 式 goal 工具，直接使用 DSH 原文，避免自创语义：

```markdown
create_goal may infer goal intent from a direct human request in any language. After session resume or fork, an active goal is disarmed: when a human asks to continue or resume in any wording or language, use update_goal action resume to rearm it. Mark complete only when the objective is actually achieved. Mark blocked only after the same blocking condition persists for at least 3 consecutive goal rounds, and report that concrete condition in blocked_reason; difficulty, uncertainty, or useful remaining work is not blocked.
```

**当前建议**：Chagent 先用单 TaskState（P-09/P-13），不引入 goal/todo 两套状态；如果未来需要多目标/跨轮次 goal，再迁移到 DSH 模型。

---

### P-16 File reference guidance（Chess 不实现）

**Chess 范围：不实现**。chess 的 `@file`/补全由 chess 前端负责；Chagent 只接收最终文本。保留本节仅作为未来参考。

```markdown
Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter. Anything else is a file: use the read tool when its contents are needed, and do not claim to have inspected it before reading. @"..." quotes a path containing spaces.
```

来源：[file-reference-local README](../../../deepseek-harness/packages/context/file-reference-local/README.md)。

---

### P-17 Working directory context（Chess 不实现）

**Chess 范围：不实现**。workspace 固定，文件工具/sandbox 不做 session cwd；保留本节仅作为未来参考。

```markdown
Current working directory: "<JSON-quoted absolute path>".
```

来源：[working-directory README](../../../deepseek-harness/packages/session/working-directory/README.md)。

**当前 Chagent**：workspace 固定，不需要该 context；如果实现，必须同时改文件工具、sandbox、`exec` 的 cwd 语义。

---

### P-18 No-progress continuation

**目的**：当一轮以空响应、无 tool call、迭代上限或无进展结束时，明确告诉模型“这不是完成”，并给出下一步。

**注入**：durable context，在下一轮开始前；也可以并入 P-10 的 task_round 前部。

**模板**：

```markdown
<system-reminder>
The previous round ended without a verified completion. Do not treat an empty response, a completed tool call, or reaching an iteration limit as task completion. Re-read the task state, inspect the workspace, and continue with the next concrete action. If the same blocking condition persists, record it; do not stop silently.
</system-reminder>
```

**规则**：

- 只对 active task 出现；
- 不替代 P-10；P-10 是新一轮指令，P-18 是失败原因提示；
- 不把“模型说了不同的话”算作进展；
- 连续 3 轮无进展 → `blocked("no-progress")`。

**测试**：空响应后出现；普通聊天不出现；blocked 后不再出现。

---

### P-19 Completion evidence checklist

**目的**：在模型准备 complete 之前，强制检查“整个目标”而不是最后一个子任务。

**注入**：system prompt 的 completion policy 内，或 `task_update` 描述中。

**模板**：

```markdown
Before completing:
- Every todo is completed.
- Each completion maps to concrete evidence: a file path, test output, command result, or user confirmation.
- The objective itself, not only the last subtask, is satisfied.

If any item is missing, continue working.
```

**测试**：complete 需要 evidence；缺 evidence 的工具调用失败；prompt 包含三个检查项。

---

### P-20 Chess base-agent role

**目的**：让模型知道自己运行在 chess 管理的工作区里，不要假设已被删除的通道/工具/服务端 API。

**注入**：system prompt，`kernel.identity` 之后；只在 `chess profile` 开启时出现。

**Chagent 适配文**：

```markdown
## Runtime role

You are the base agent for a Chess-managed workspace. Your workspace files are synchronized by the Chess client; treat them as the source of truth. You communicate with the outside world only through the Pico channel. Do not assume access to the Chess server API, other agents' workspaces, or channels/tools that are not present in this session. If a capability is missing, inspect the available tools and workspace instead of inventing one.
```

**理由**：

- chess 的 agent 通过 pico 与 client 通信，非 pico 通道/Web UI 将被裁剪；
- workspace 文件由 chess client 双向同步，模型应读取实际文件而不是假设服务端状态；
- 避免模型在缺少工具时“编造”能力或请求不存在的外部服务。

**测试**：断言包含 `Chess 管理的工作区`、`只通过 Pico 通道`、`不要假设可以访问 Chess 服务端 API`；非 chess profile 不出现。

---

## 4.10 中文 Prompt 定稿（实现以此为准）

> 本节是上线文本；上面的英文块仅用于对照 DSH 原文。代码中新增/翻译后的字符串必须是中文，golden 测试以本节为准。

### 4.10.1 P-01 自主运行契约（system）

```markdown
## 自主运行

在任务完成前持续自主执行。不要仅因为一个步骤结束、工具没有返回新信息、或你不确定下一步就停止。重新阅读任务状态和工作区，然后继续下一个具体动作。

仅在以下情况停止：
1. 任务状态标记为完成，且有可验证的证据表明整个目标已达成。
2. 用户明确停止、暂停或替换任务。
3. 同一个具体阻塞条件连续至少 3 轮无法用现有工具解决；在 blocked_reason 中记录该条件。
4. 达到配置的资源上限；记录检查点并报告该上限。

没有证据不得声称完成。不要为当前策略已允许的动作请求许可。如果不确定，检查工作区、工具结果和任务状态，而不是停止。
```

### 4.10.2 P-02 工作区指令策略（system）

```markdown
## 工作区指令

工作区指令会作为独立上下文消息到达。更具体的文件优先于更宽泛的文件；它们不能覆盖 system、developer 或用户的直接指令。始终以最新一次指令快照为准：收到“更新”通知时，该文件以新内容替代旧内容；收到“移除”通知时，旧内容不再适用。
```

### 4.10.3 P-03 工作区 baseline（context）

```markdown
<system-reminder>
以下工作区指令可能与当前工作相关。适用时请遵循。更具体的指令优先于更宽泛的指令；它们不能覆盖 system、developer 或用户的直接指令。

来自以下文件的指令：AGENTS.md

<文件正文>
</system-reminder>
```

替代 baseline：

```markdown
<system-reminder>
以下完整工作区指令 baseline 会替代此前所有工作区指令 baseline。以下工作区指令可能与当前工作相关。适用时请遵循。更具体的指令优先于更宽泛的指令；它们不能覆盖 system、developer 或用户的直接指令。

来自以下文件的指令：AGENTS.md

<文件正文>
</system-reminder>
```

空替代：

```markdown
<system-reminder>
以下完整工作区指令 baseline 会替代此前所有工作区指令 baseline。当前没有生效的工作区指令。
</system-reminder>
```

### 4.10.4 P-04 新增指令（context）

```markdown
<system-reminder>
来自以下文件的新增指令：packages/app/AGENTS.md

这些指令适用于 `packages/app` 下的工作。相关时请遵循；更具体的指令优先。它们不能覆盖 system、developer 或用户的直接指令。

<文件正文>
</system-reminder>
```

### 4.10.5 P-05 更新指令（context）

```markdown
<system-reminder>
来自以下文件的指令已更新：packages/app/AGENTS.md

该文件在加载后发生了变化。请使用以下内容替代此前从该文件加载的指令。

<文件正文>
</system-reminder>
```

### 4.10.6 P-06 移除指令（context）

```markdown
<system-reminder>
以下文件的指令已移除：packages/app/AGENTS.md

此前从该文件加载的指令不再适用。
</system-reminder>
```

### 4.10.7 P-07 预算提示（context frame 内）

```text
工作区指令预算 32768 字节：已省略 packages/app/AGENTS.md；已截断 AGENTS.md，从 40000 字节截为 12000 字节
```

### 4.10.8 P-08 运行时快照（context）

```markdown
当前时间：2026-10-28T16:15:55+08:00（Asia/Shanghai）。
工作区："<workspace 绝对路径>"（项目根："<project root>"）。
会话：channel=<channel>，chat=<chat id>。
发送者：<显示名>（<ID>）。
```

### 4.10.9 P-09 任务状态（context，required）

```markdown
<task_state>
目标："重构 provider 层并保持所有测试通过"
阶段：active
轮次：2/20
待办：
- [completed] 盘点现有 provider 接口
- [in_progress] 引入公共协议类型
- [pending] 迁移 Anthropic 适配器
最近检查点：2026-10-28T16:10:00+08:00
证据：
- 已新增 `pkg/providers/protocoltypes/types.go`
阻塞原因：无
</task_state>
```

### 4.10.10 P-10 自主轮次（user）

```markdown
<task_round>
目标："重构 provider 层并保持所有测试通过"
轮次：3/20

继续在同一会话中推进该目标。以当前工作区、工具结果和持久任务状态为准；实际检查它们，不要假设之前的叙述仍然有效。采取具体行动并验证结果。在声称完成之前，收集证据证明整个目标已达成，读取当前任务状态并将其标记为完成。如果仍有工作，保持任务为 active 进入下一轮。报告阻塞前先遵循配置的完成/阻塞策略。
</task_round>
```

### 4.10.11 P-11 完成/阻塞策略（system）

```markdown
只有当目标实际达成，并且工作区或工具结果中每个待办都有对应证据时，才可标记任务完成；在任务更新中引用这些证据。只有当同一个具体阻塞条件连续至少 3 轮持续存在且现有工具无法解决时，才可标记任务阻塞；在 blocked_reason 中报告该条件。困难、不确定或仍有有用的剩余工作都不算阻塞。不要在一轮结束时既不完成也不给出新的阻塞原因。
```

### 4.10.12 P-12 `todo_write` 工具描述

```markdown
记录并更新任务清单，用于规划多步工作并展示进度；简单单步任务可跳过。开始前为每个具体步骤添加一条待办。工作未完成时，保持正在处理的任务为 in_progress；只有并行工作时才允许多个 in_progress。任务一完成就标记为 completed。
```

Schema 字段描述：

- `todos`：完整任务清单，替换此前的清单。
- `content`：任务是什么，一句简短命令式描述。
- `status`：pending（未开始）| in_progress（进行中）| completed（已完成）。

成功结果：

```text
任务清单已更新：2 项待处理，1 项进行中，3 项已完成。
```

失败结果：

```text
错误：无效待办：`content` 必须是非空字符串
错误：无效待办：内容重复 "<content>"
错误：todo_write 需要所属的 agent session
错误：无效待办：最多只能有一个 in_progress（当前 <n> 个）
```

### 4.10.13 P-13 `task_get` / `task_update` 工具描述

`task_get`：

```markdown
读取当前会话的任务状态：目标、阶段、轮次、待办、证据和阻塞原因。更新状态前或不确定还剩什么工作时使用。
```

结果（紧凑 JSON，字段稳定）：

```json
{"task":{"objective":"...","phase":"active","round":2,"max_rounds":20,"todos":[{"content":"...","status":"in_progress"}],"evidence":["..."],"blocked_reason":null}}
```

`task_update`：

```markdown
更新当前会话的任务状态。使用 start 从明确的人类或调度器请求创建任务；使用 pause/resume/complete/block 改变阶段。恢复 blocked 任务需要直接的人类请求。没有具体证据不要标记完成。同一个条件未连续持续至少 3 轮前不要标记阻塞。
```

### 4.10.14 P-14 技能目录与 `skill` 工具

```markdown
<system-reminder>
技能是一组可复用的任务专用指令。当前会话可用的技能如下：

<available_skills>
- `<name>`：<中文或原文描述>
</available_skills>

如果用户点名某个技能，或任务明显匹配某个技能描述，在采取任务动作前调用 `skill` 工具并传入准确技能名。加载所有适用技能，然后遵循其完整指令。本目录只包含摘要；在加载之前不要推断或执行技能指令。
</system-reminder>
```

`skill` 工具描述：

```markdown
加载技能的完整指令。当任务点名或明显匹配会话技能目录中的技能时，在采取行动前调用。
```

### 4.10.15 P-18 无进展续跑（context）

```markdown
<system-reminder>
上一轮结束时没有可验证的完成。不要把空响应、一次完成的工具调用或达到迭代上限当作任务完成。重新读取任务状态，检查工作区，然后继续下一个具体动作。如果同一个阻塞条件持续存在，记录它；不要静默停止。
</system-reminder>
```

### 4.10.16 P-19 完成证据检查（system）

```markdown
完成前请检查：
- 每个待办都已完成。
- 每个完成项都有具体证据：文件路径、测试输出、命令结果或用户确认。
- 整个目标本身已满足，而不仅是最后一个子任务。

任何一项不满足就继续工作。
```

### 4.10.17 P-20 Chess 基础 Agent 角色（system）

```markdown
## 运行角色

你是 Chess 管理的工作区中的基础 Agent。工作区文件由 Chess 客户端同步，请把它们作为事实来源。你只通过 Pico 通道与外界通信。不要假设可以访问 Chess 服务端 API、其他 Agent 的工作区，或当前会话中不存在的通道/工具。如果缺少某项能力，请检查可用工具和工作区，不要编造。
```

### 4.10.18 P-21 任务判定与记录（system，文件工具可用时默认注入）

```markdown
## 任务判定与记录

- 每轮先判定用户输入：需要产生可验证结果（修改文件、执行命令、检索并交付、部署、修复等）的是任务；纯问答、解释、闲聊、征求意见不建任务。
- 需要执行的任务记录在 tasks/TASKS.md（不存在时创建）。每条任务包含：编号（TASK-YYYYMMDD-NNN）、创建时间（RFC3339，带时区）、来源、身份类型与角色/职责（必填）、公司类型（如已判定则记录，可选）、目标、验收标准、状态（pending / in_progress / blocked / completed / abandoned）和证据。
- 执行期间用户新增输入时，先判定它是当前任务的补充/修正，还是独立的新需求：补充直接合并进该任务的目标与验收标准；新需求登记为新任务，原任务状态不变。
- 任务完成时在 tasks/TASKS.md 标记 completed，写入完成时间与证据；全部任务完成后，把已完成条目移到 tasks/TASK_LOG.md（保留编号、身份类型与角色/职责、公司类型（如已判定）、创建时间、完成时间、目标、验收标准、证据与结果），tasks/TASKS.md 只保留未完成项。
- 开始新一轮或接管会话时检查 tasks/TASKS.md 中的遗留任务，按创建时间与当前上下文判定：创建时间在 7 天内，或任务仍与当前请求相关、依赖未变化 → 继续执行；创建超过 7 天且无进展证据、前置条件或依赖已变化 → 判定不执行，标记 abandoned 并写明原因后移入 tasks/TASK_LOG.md；无法判断时按继续处理，不得静默丢弃。
- 判定结果必须在新的输出开头明确列出：任务编号、目标、判定（继续/不执行）、理由。判定继续的任务按原验收标准推进，并在最终输出中说明本次在用户请求之外额外完成的内容。
- 任务文件内容必须与实际执行一致；没有写入或工具结果证据，不得声称已记录或已完成。
```

`todo_write` / `task_update` 可用时分别追加协同句（“多步任务用 todo_write 维护待办…” / “会话任务状态用 task_update 管理…”）。文件工具（`read_file` 且 `write_file` 或 `edit_file`）不可用，或本次请求禁用工具规则时，整段省略；此时 identity 中也不加入 `**任务判定**` 规则。

### 4.10.19 P-22 记忆维护（system，文件工具可用时默认注入）

```markdown
## 记忆维护

- memory/MEMORY.md 只保留跨会话仍然成立的核心内容：用户身份与长期偏好、稳定约定、关键决策与结论。
- 历史过程、一次性事件、临时状态、已完成事项的叙述，以及已经恢复正常的异常，不写入长期记忆；发现已有此类内容时主动合并或删除。
- 任务进度、待办和遗留任务判定不写入长期记忆；它们只记录在 tasks/ 下。
- 每日笔记 memory/YYYYMM/YYYYMMDD.md 属于过程记录；其中确有长期价值的结论提炼进 MEMORY.md，其余不保留。
- 清理记忆后，在最终任务完成输出中说明清理了什么；没有清理则不说明，不得虚构清理。
```

### 4.10.20 P-23 角色判定与履职（system，默认注入）

```markdown
## 角色判定与履职

- 执行或整理任务前，必须先判定自己在该任务中的身份类型与角色，并明确该角色的职责、权限边界、验收责任与判定依据；身份类型与职责是任务的必判定项。
- 身份类型覆盖商业公司具有的角色：软件开发工程师（前端/后端/移动端）、架构师、SRE/运维工程师、硬件/嵌入式研发工程师、测试工程师、数据/算法工程师、安全工程师、产品经理、产品设计（UX/UI）、产品开发、运营、商品运营、销售、市场、公关、政府关系、财务、投资融资、审计、法务、人力/行政，以及 CEO/业务负责人、CTO/技术负责人、CFO、COO、CMO、CLO 等管理角色；同一身份在不同公司类型下职责不同，以任务验收标准为准。
- 在判定身份类型与角色的基础上，如任务涉及具体行业或公司类型，可进一步判定行业/公司类型与业务场景（电商、生鲜电商、半导体、芯片设计、AI、机械设计、无人机、机器人、餐饮等）作为职责与专业标准的补充；行业/公司类型判定是可选项，不要求必须判定，也不得因无法判定而停止身份类型判定。
- 身份类型与角色由任务目标、业务场景、输入材料与验收标准决定，随任务改变；同一会话中的不同任务分别判定身份类型与角色，不得沿用上一任务的职责假设。
- 判定身份后严格按该角色职责执行：只做职责范围内的事，使用与该角色匹配的专业技能、方法和工具，对职责内的判定、动作与结果负责；职责边界外或能力不足时明确说明，不得以角色名义编造或越权。
- 按角色对应的专业标准判定与执行：研发关注正确性、可维护性、测试、兼容、可观测性与安全；产品设计关注用户需求、信息架构、交互与可用性；产品开发关注范围、落地与跨职能协同；运营与商品运营关注流程、指标、供给、库存与履约；销售关注客户需求、商机、报价、合同与回款；市场/公关关注信息一致性、品牌与舆情；政府关系关注合规、政策、许可与利益相关方；财务关注核算、预算、现金流、税务与内控；投资融资关注估值、尽调、条款、资金结构与回报风险；审计关注独立性、证据、控制测试与整改；法务关注合规、合同、知识产权与诉讼风险；人力/行政关注组织、招聘、绩效、员工关系与行政保障；管理层关注方向、优先级、资源、风险与治理。
- 若已判定行业/公司类型，再按该类型对应的行业标准判定与执行：电商/生鲜电商关注商品、供应链、仓储履约、平台/店铺运营、客服与合规；半导体/芯片设计关注设计、验证、版图、工艺、封装测试与良率；AI 关注数据、算法、模型评估、MLOps 与安全；机械设计关注结构、强度、公差、仿真与工艺；无人机/机器人关注控制、导航、传感器、嵌入式、安全与认证；餐饮关注门店运营、供应链、品控、食品安全与选址。
- 每次任务判定或阶段汇报先给出身份类型与角色（身份类型、角色、职责、依据），如已判定行业/公司类型则一并给出；再给执行动作、证据和结果；结论必须与身份类型、角色职责、专业标准和证据一致，不得给出超出证据的断言。
- 任务完成前核对身份类型、角色、职责、专业标准（以及已判定的行业/公司类型标准）、验收标准与证据是否一致；不一致就修正或保持未完成，不得宣称完成。
```

启用工具规则时追加：“按角色职责选用对应技能和工具；技能或工具不匹配时先加载或确认，不得用无关能力替代。”文件工具可用时追加：“在 tasks/TASKS.md 中为每条任务标注身份类型、角色与职责（必填）以及已判定的公司类型（可选）；任务目标或上下文变化时重新判定并更新记录。”

### 4.10.21 未实现 Prompt

- P-15 Goal policy、P-16 @file、P-17 working directory：Chess 当前不实现；如未来实现，再翻译为中文。

### 4.10.22 现有英文 Prompt 的翻译范围

实现 T4.5 时必须把以下现有英文内容翻译为中文：

- `pkg/agent/context.go` 的 identity、rules、workspace、memory、output policy、dynamic context；
- `pkg/agent/context.go`/`prompt.go` 中所有模型可见的 prompt part 文案；
- `pkg/agent/prompt_contributors.go` 的 tool discovery/MCP/agent discovery 文案；
- `pkg/tools` 中模型可见的工具 Description/参数描述（至少 chess 默认启用的工具）；
- 技能 catalog、active skills、summary 前缀、错误提示中模型可见的部分。

代码标识符、JSON 字段、文件路径、协议类型（`pico`、`tool call` 等）保持原样；中文句内可保留必要英文术语。

---

## 5. Prompt 组装、排序与去重

### 5.1 System prompt 顺序

建议顺序（数字为 `sortPromptParts` 的 layer/slot 优先级，现有实现见 [`pkg/agent/prompt.go:444`](../../pkg/agent/prompt.go#L444)）：

| 顺序 | Part | 说明 |
|---|---|---|
| 1 | `kernel.identity` | 身份、workspace、硬规则 |
| 2 | `kernel.chess_role` | P-20（chess profile） |
| 3 | `kernel.autonomy` | P-01/P-11（autonomy 开启） |
| 4 | `kernel.memory_policy` | P-22（文件工具可用时默认注入） |
| 5 | `kernel.path_tool_policy` | 路径/工具/验证（始终注入） |
| 6 | `kernel.role_policy` | P-23（默认注入） |
| 7 | `kernel.task_policy` | P-21（文件工具可用时默认注入） |
| 8 | `kernel.workspace_policy` | P-02（workspace_context 开启） |
| 9 | `instruction.workspace` | 仅 definition/persona（`AGENT.md`/`SOUL.md`/`USER.md`/`IDENTITY.md`） |
| 10 | `capability.tooling` | 工具发现/MCP 说明 |
| 11 | `capability.skill_catalog` | 仅在未迁移到 context 时保留 |
| 12 | `context.memory` | 记忆（可选迁移） |
| 13 | `context.runtime` | 仅在 `mode="system"` 回退时保留 |
| 14 | `context.summary` | 摘要 |

### 5.2 Context 顺序

| 顺序 | Context | 说明 |
|---|---|---|
| 1 | workspace baseline | 首次/resume |
| 2 | workspace delta | additional/updated/removed |
| 3 | runtime snapshot | 变化才追加 |
| 4 | task state | required |
| 5 | skill catalog | 渐进披露 |
| 6 | task round / 当前 user | 最后 |

### 5.3 去重与变化

- **baseline**：`identity` 相同且可见 baseline 存在 → 不重复；identity 变 → 完整替换 + 旧 scope removal。
- **delta**：scope + digest 相同 → 不生成；内容变 → replace；文件消失 → remove。
- **runtime**：文本相同 → 不生成；变化 → 追加。
- **task state**：状态 digest 相同 → 不生成；变化 → 追加。
- **skill catalog**：成员/描述 digest 相同 → 不生成；变化 → 整表替换（含空表退役）。
- **跨来源不合并**：不同 `Kind` 的 context 不合并，便于对账和调试。

### 5.4 Token / KV Cache 影响

| Prompt | Token 影响 | KV Cache 影响 |
|---|---|---|
| system L0/L1 | 固定 | 稳定前缀；改文案才失效 |
| workspace baseline | 首次一次性，受 maxBytes | 追加在历史后；不破坏前缀 |
| workspace delta | 每次变化一条 | append-only |
| runtime snapshot | 变化才一条 | append-only；system 不变 |
| task state | 任务变化才一条 | append-only |
| skill catalog | 成员变化才一条 | append-only |
| task_round | 每轮一条 | append-only |

---

## 6. 失败、安全与转义

### 6.1 必须转义的内容

- 所有工作区指令正文、预算 notice、additional/updated/removed 文本；
- 技能正文（转义 `</skill_content>`）；
- 任务 objective/evidence/blocker（转义 `</task_state>`，可能含用户输入）；
- runtime snapshot 中的路径/发送者（转义 `</system-reminder>`）；
- 任何进入 `<system-reminder>` / `<task_state>` / `<available_skills>` / `<skill_content>` 的模型可见文本，必须转义对应 frame 的结束标记。

### 6.2 安全规则

- 不把指令文件内容写入日志；只写路径、字节数、digest 前缀；
- 读取有 `max_source_bytes`；超限不读半截；
- 默认不跟随符号链接；开启时校验目标在允许根内；
- 不把工作区指令当作 system/developer 指令；prompt 明确“不得覆盖”；
- 不因为上下文读取失败而让 turn 失败；保留 last-good；
- `task_update` / `todo_write` 的权限检查在工具执行时做，不能只靠 prompt。

### 6.3 失败提示语

| 情况 | 模型可见文本 |
|---|---|
| 文件读取失败但之前加载过 | 不发消息（保留 last-good）；日志 warn |
| 文件消失 | P-06 removal |
| 预算省略 | P-07 notice |
| 预算截断 | P-07 notice |
| 技能加载失败 | `Error: skill "<name>" is unknown or no longer available` |
| 任务不存在 | `Error: no active task state for this session` |
| 完成缺证据 | `Error: task cannot be completed without evidence` |
| 阻塞不足 3 轮 | `Error: blocked requires the same condition for at least 3 consecutive rounds (got <n>)` |

---

## 7. Golden test 清单

每段 prompt 至少有一个 golden 测试，断言关键短语、顺序和缺失条件。建议放在 `pkg/agent/workspacecontext`、`pkg/agent`、`pkg/tools` 的测试中。

| ID | 测试 | 断言 |
|---|---|---|
| G-01 | baseline 顺序 | 用户级开启时 `~/.chagent/AGENTS.md` 在 `~/.agents/AGENTS.md` 前；项目级在用户级后；宽目录在具体目录前；chess 默认只项目链 |
| G-02 | baseline 去重 | 相同 trim 内容只出现一次；`CLAUDE.md` 重复不出现 |
| G-03 | additional | 触碰 `packages/app` 后出现 `Additional instructions from: packages/app/AGENTS.md`，且包含 `These instructions apply to work under` |
| G-04 | updated | 修改文件后出现 `Updated instructions from`，新内容存在，旧内容不被重复渲染 |
| G-05 | removed | 删除文件后出现 `Instructions removed`，含 `no longer apply` |
| G-06 | budget | 超预算时出现 `Workspace instruction budget`，含 omitted/truncated 路径 |
| G-07 | escape | 内容含 `</system-reminder>` 时输出 `<\/system-reminder>`，frame 仍闭合 |
| G-08 | runtime | 同值不重复；变化后追加 `Current time:`/`Workspace:` |
| G-09 | task state | 含 `Objective:`、`Phase:`、`Round:`、`Todos:`；blocked 含 `Blocked reason:` |
| G-10 | task round | 含 objective、`Round: n/m`、`Treat the current workspace` |
| G-11 | autonomy | 含 `Stop only when`、`Never claim completion without evidence`、`at least 3 consecutive rounds` |
| G-12 | todo | 工具描述含 `The COMPLETE task list`；结果 `Updated todo list: ...` |
| G-13 | skill | catalog 只含 name+description；`skill` 描述含 `Load the full instructions` |
| G-14 | no duplicate | 连续 5 轮无变化，context 数量不增加 |
| G-15 | compaction | baseline 被丢弃后下一轮重建；旧 scope 有 removal |
| G-16 | no premature stop | 空响应/迭代上限后出现 P-18/P-10，而不是 final answer |
| G-17 | chess role | 含 `Chess-managed workspace`、`only through the Pico channel`、`Do not assume access to the Chess server API`；非 chess profile 不出现 |
| G-18 | task policy | 文件工具可用时默认含 `## 任务判定与记录`、`tasks/TASKS.md`、`tasks/TASK_LOG.md`、7 天遗留任务判定；仅有 `read_file` 或禁用工具规则时不出现 |
| G-19 | memory policy | 文件工具可用时默认含 `## 记忆维护`、`只保留跨会话仍然成立的核心内容`、清理说明要求；无写工具时不出现 |
| G-20 | role policy | 默认始终含 `## 角色判定与履职`；身份类型与职责为必判定项并覆盖商业公司角色；行业/公司类型（电商/生鲜电商/半导体/芯片设计/AI/机械设计/无人机/机器人/餐饮）为可选补充；工具规则启用时含技能/工具协同句，文件工具可用时含任务记录身份类型/角色（必填）与公司类型（可选）要求 |

---

## 8. 与现有 prompt 的迁移映射

| 现有 Part | 新交付 | 动作 |
|---|---|---|
| `kernel.identity` | system | 保留；加入 P-02/P-01/P-20 的短策略 |
| `instruction.workspace`（根 AGENT/AGENTS/SOUL/USER/IDENTITY） | system | 保留为 persona；根 AGENTS.md 若同时是 definition 则从 instruction chain 排除 |
| `capability.skill_catalog` | context（M4） | 改为 P-14；保留一个版本的 fallback |
| `context.memory` | context（后续） | 作为 memory source 迁移；先保留 |
| `context.runtime` | context snapshot（M4） | 改为 P-08；`mode=system` 回退 |
| `context.summary` | system（暂留） | 压缩摘要仍在 system；后续可评估 context 化 |
| `capability.active_skills` | context（M4） | 用户显式调用/主动技能作为上下文；避免 system 膨胀 |
| 新增 `kernel.autonomy` | system | P-01/P-11 |
| 新增 `kernel.task_policy` | system | P-21（文件工具可用时默认注入） |
| 新增 `kernel.memory_policy` | system | P-22（文件工具可用时默认注入） |
| 新增 `kernel.role_policy` | system | P-23（默认始终注入） |
| 新增 `kernel.chess_role` | system | P-20（chess profile） |
| 新增 `kernel.workspace_policy` | system | P-02 |
| 新增 workspace context | context | P-03..P-07 |
| 新增 task state | context | P-09 |
| 新增 task tools | tools | P-12/P-13 |
| 新增 skill tool | tools | P-14 |

---

## 9. Prompt 反模式

| 反模式 | 问题 | 正确做法 |
|---|---|---|
| 把时间/会话/sender 放 system prompt | 每请求改写前缀，KV Cache 失效 | 放 runtime snapshot，变化才追加 |
| 每轮重发完整 baseline | token 浪费、噪声 | baseline 一次；delta 只发变化 |
| 文件变化后只发新全文、不发 `Updated` | 模型同时看到新旧规则 | P-05 明确替代 |
| 文件消失后不发 `Removed` | 模型继续遵循旧规则 | P-06 |
| 超预算静默截断 | 模型以为规则完整 | P-07 notice |
| 指令内容不转义 | 可提前关闭 frame | 转义 `</system-reminder>` |
| 用“无 tool call”判定完成 | 自主任务提前停止 | 检查 TaskState；P-18 |
| 用 `max_tool_iterations` 作为完成 | 长任务被硬截断 | checkpoint + 下一轮 |
| 让模型为已允许动作请求许可 | 自主性下降 | P-01 明确“不要请求已允许动作的许可” |
| 完成不要 evidence | 假完成 | P-11/P-19 |
| blocked 无阈值 | 遇到困难就停 | 3 轮阈值 + 工具校验 |
| 工具描述与 prompt 规则不一致 | 模型行为分裂 | 单一事实来源；golden 测试 |

---

## 10. 评审检查表

- [ ] 每段 prompt 有明确 ID、注入点、触发条件、失败语义。
- [ ] 所有模型可见内容有 escape 策略。
- [ ] 所有预算/上限有默认值和硬上限。
- [ ] 所有 context 有 source 元数据，可 replay/resume。
- [ ] 完成/阻塞/停止规则在 prompt 与工具执行两处一致。
- [ ] 普通聊天（autonomy off、workspace_context off）不出现新增 prompt。
- [ ] Golden test 覆盖顺序、去重、更新、删除、预算、转义、自主停止。
- [ ] 文档与代码中的术语一致：workspace instructions、context、task state、task round、todo。