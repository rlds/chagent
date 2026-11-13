# 模型可见文本语言规范与审计（中文）

> 适用范围：chagent 最终发送给 LLM 的所有文本 —— system prompt、注入上下文、工具说明/参数、工具结果包装、合成回合文本、默认回复。
> 审计基线：`de3e4437`（项目重命名 chagent）+ 本次工作树修改。
> 核对日期：2026-11-02。审计工具：`scripts/check-model-visible-chinese.py`（报告型，不要求零误报）。

## 0. 结论

- **本次负责的 `pkg/agent` / `pkg/agent/workspacecontext` 模型可见文本已 100% 中文**：
  - system prompt（identity/rules/workspace/memory/output/runtime/summary/skills/policy prompts）全部中文；
  - 注入 context（workspace/runtime/task-state/task-round/skill-catalog/loop-guard/context-pressure）全部中文；
  - 合成回合文本（中断提示、任务续跑、图片占位、子代理结果）全部中文；
  - 默认回复（空响应 / 工具迭代上限 / 附件已发送）已中文（由 agent-loop owner 完成）。
  - 运行时 dump（overlay 探针）审计结果：**英文候选 0 条**（见 `build/functest-20261030/model-text/dump-audit.txt`）。
- **翻译轮后全仓模型可见文本已全部中文化**：pipeline_execute/tool_batch/tool_result_prune/agent_command/agent_message/pipeline_llm/commands、pkg/tools 核心/integration/dsh、normalization/diff/MCP 标记等均已由 task-42..47 完成；最终扫描仅剩机器协议/技术标识/内部非模型例外，见 §8。

## 1. 判定标准

| 类别 | 是否模型可见 | 要求 |
|---|---|---|
| system prompt 正文 | 是 | 中文 |
| 注入 context 的 `Content` | 是 | 中文 |
| 工具 `Description()` / `Parameters()` 的 description 文本 | 是 | 中文 |
| 工具结果（`ToolResult.ForLLM`）与包装标记 | 是 | 中文 |
| 合成 user/assistant 消息（中断提示、续跑提示、子代理结果、图片占位） | 是 | 中文 |
| 默认回复 / 工具上限回复 | 是（作为最终回复再进入后续上下文） | 中文 |
| 结构化标记（XML tag 名、JSON 字段、协议前缀） | 是，但属于机器协议 | 允许英文（例外） |
| 日志 / 内部错误 / PromptSource 描述符 / PromptPart.Title | 否 | 不限（本次审计仅区分，不要求翻译） |

## 2. 分类清单与证据

### 2.1 System prompt

| 组成 | 语言 | 证据（file:line） |
|---|---|---|
| 身份与硬规则（`# chagent`、`## 工作区`、`## 规则`、`**精确执行**`、`**语言与表达**`） | ✅ 中文 | `pkg/agent/context.go`（身份/规则）、`pkg/agent/prompt_policy.go`（任务与表达策略） |
| 上下文摘要说明 | ✅ 中文 | `pkg/agent/context.go:213` |
| 运行时事实（snapshot 模式） | ✅ 中文 | `pkg/agent/runtime_context.go:19-45`（当前时间/平台/工作区/会话/发送者，中文标签） |
| 策略 Prompt P-01 / P-02 / P-11 | ✅ 中文 | `pkg/agent/prompt_policy.go:12-30` |
| 策略 Prompt P-21 / P-22（任务判定/记录、记忆维护） | ✅ 中文 | `pkg/agent/prompt_policy.go`（`taskPolicyPrompt` / `memoryPolicyPrompt`），文件工具可用时默认注入 |
| 策略 Prompt P-23（角色判定与履职） | ✅ 中文 | `pkg/agent/prompt_policy.go`（`rolePolicyPrompt`），默认始终注入 |
| 工具发现 / MCP / agent 发现 contributor | ✅ 中文 | `pkg/agent/prompt_contributors.go:100` 起（MCP 中文文案；测试 `TestModelVisibleContributorPromptsAreChinese` 锁定） |
| turn profile 规则（**始终使用工具**…） | ✅ 中文 | `pkg/agent/turn_profile_policy.go:71` |
| 技能目录（system 回退模式） | ✅ 中文 | `pkg/agent/context.go:339` 段 + `skill_catalog_context.go:261`（技能是可复用的…） |
| 子代理（SubTurn）system 覆盖文本 | ✅ 中文（内容由调用方提供；内置默认提示为中文） | `pkg/agent/prompt_turn.go:124-136` |
| PromptPart.Title / PromptSource 描述符 | ⚪ 英文（内部元数据，不渲染进模型） | 例：`pkg/agent/prompt.go:181-314`、`prompt_policy.go:56-64`、`skill_catalog_context.go:69-76` —— **非模型可见，例外** |

### 2.2 注入上下文（durable context）

| kind | 语言 | 证据（file:line） |
|---|---|---|
| `workspace-instructions` baseline/delta | ✅ 中文 | `pkg/agent/workspacecontext/render.go:20-36`（以下工作区指令…） |
| `runtime-context` | ✅ 中文 | `pkg/agent/runtime_context.go:19-45` |
| `task-state` | ✅ 中文 | `pkg/agent/taskstate_context.go:75,98`（RenderTaskState，中文键：目标/阶段/轮次/待办） |
| `task-round`（自主续跑提示 P-10） | ✅ 中文 | `pkg/agent/autonomy_runtime.go:52-57`（<task_round> 目标/轮次…） |
| `skill-catalog` | ✅ 中文 | `pkg/agent/skill_catalog_context.go:261-265`（技能是可复用的…/使用提示/截断说明） |
| `loop-guard` 重复工具提醒 | ✅ 中文 | `pkg/agent/loop_guard.go:296-308`（检测到重复的工具调用…） |
| `context-pressure` 临近上限提醒 | ✅ 中文 | `pkg/agent/context_meter.go:160-169`（上下文使用约…） |
| 上下文贡献注册（plugin ContextSource） | ⚪ 由插件提供；内置来源中文 | `pkg/agent/context_source.go:262-275`（消息包装；文本由来源提供） |
| Pre-step 有序表 | ✅（各来源文中文） | `pkg/agent/prestep.go:44-108` |

### 2.3 合成回合文本 / 工具结果包装

| 文本 | 语言 | 证据 |
|---|---|---|
| 中断提示 | ✅ 中文 | `pkg/agent/turn_state.go:811-819`（已请求中断…；turn_state.go 属 task-42，当前已是中文） |
| 任务续跑提示 | ✅ 中文 | `pkg/agent/autonomy_runtime.go:963-974` + `:52-57` |
| 工具结果图片占位 | ✅ 中文（本次补译） | `pkg/agent/prompt_turn.go:212` `[上方工具结果中的图片已加载]` |
| 子代理结果标记 | ✅ 中文（本次补译） | `pkg/agent/prompt_turn.go:226` `[子代理结果] %s` |
| 默认回复（空响应/工具上限/附件已发送） | ✅ 中文 | `pkg/agent/agent.go:125-127`（任务书 task-42 已交付） |
| tool-result pruner 标记 | ⚠️ 英文（task-43 范围） | `pkg/agent/tool_result_prune.go:18,131` `…[tool output middle pruned]…` |
| 工具批调度合成结果 | ⚠️ 英文（task-42 范围） | `pkg/agent/tool_batch.go:20,84,176,183`（`tool call aborted before dispatch` 等） |

### 2.4 工具说明与参数（`pkg/tools/**`）

- **参数 description**：全量扫描无英文（脚本对 `Parameters()` 中 `"description"` 值检查）。
- **`Description()`**：**发现 1 处英文**：`pkg/tools/cron.go:84`（"Schedule, inspect, and update reminders…"）→ 待工具 owner 补译。
- 其余工具 Description 均中文（脚本仅列出该 1 处）。
- **工具结果/包装标记仍有英文**（72 条扫描候选，非本任务写域）：`pkg/tools/normalization.go:18-237`、`pkg/tools/integration/helpers.go:20-21`、`pkg/tools/shared/diff_result.go:14-17`、`pkg/tools/integration/mcp_tool.go:474`、`pkg/tools/integration/web.go:2285`、`pkg/tools/toolloop.go:177,193`、`pkg/tools/delegate.go:67-100`、`pkg/tools/dsh/goal/**`、`pkg/tools/dsh/mcpresources/tools.go:218` 等。

## 3. 合理例外清单（允许英文）

| 例外 | 说明/示例 |
|---|---|
| 工具名/参数名/JSON 字段/枚举值 | `read_file`、`max_tool_iterations`、`config.json`、`active`、`blocked` |
| 结构化标记（XML tag 名与属性） | `<skill name="…">`、`</skill>`、`<runtime_context>`、`<task_state>`、`<context_pressure>`、`<system-reminder>`、`<available_skills>`、`<skill_content>`、`<task_round>` |
| 协议/摘要前缀 | `CONTEXT_SUMMARY:` |
| 系统角色术语 | `system`、`developer`、`baseline`、`delta`（出现在中文句中，无合适译名且需与协议一致） |
| 文件路径/URL/命令/ID | `/workspace/…`、`https://…`、`pico:user-1`、`chat-1` |
| 外部 MCP server 提供的工具名/描述/资源文本 | 由外部 server 决定，chagent 不翻译 |
| 日志 / 内部错误 / provider 错误匹配串 | `logger.*`、`fmt.Errorf`、`maximum context length` 等（不进入模型） |
| 内部元数据 | `PromptPart.Title`、`PromptSourceDescriptor.Description`、`PromptSlot/Layer`（不渲染进模型） |
| OS errno / 网络错误原文 | `no such host`、`network is unreachable`（诊断与匹配用） |

## 4. 审计工具

`scripts/check-model-visible-chinese.py`：

```bash
# 默认扫描 pkg/agent + pkg/tools，仅输出 english/mixed 候选
scripts/check-model-visible-chinese.py
# 指定目录 / JSON / 包含测试 / 全量（含 internal 分类）
scripts/check-model-visible-chinese.py --exclude-kind=none pkg/agent pkg/agent/workspacecontext
scripts/check-model-visible-chinese.py --json pkg/tools
```

- 输出格式：`path:line: [english|mixed] literal`，`english` 为纯英文串、`mixed` 为中文串中混英文句子；
- `internal` 分类（logger/error/诊断）默认省略，`--exclude-kind=none` 可保留；
- 脚本**不要求零误报**：PromptPart.Title、内部描述符会出现在候选中，需按 §3 人工判定。

## 5. 跨 owner 待办（本次未越界修改）

| 区域 | 发现 | owner 建议 |
|---|---|---|
| `pkg/agent/pipeline_execute.go` | 工具拒绝/跳过原因英文：`:147,354-358,421,460,748-752,1016,1133` | task-42/43/44（agent-loop） |
| `pkg/agent/tool_batch.go` | 批调度合成工具结果英文：`:20,84,176,183` | task-42/43/44 |
| `pkg/agent/tool_result_prune.go` | 裁剪标记英文：`:18,131`（会进入模型上下文） | task-43（context owner） |
| `pkg/agent/agent_command.go` / `agent_message.go` / `pipeline_llm.go` | 命令回复/用户提示英文（面向用户，不进入模型上下文） | task-42/43/44 |
| `pkg/tools/cron.go:84` | 工具 Description 英文 | 工具 owner |
| `pkg/tools/**` 结果/包装标记 | 72 条扫描候选（normalization/diff/MCP/web/toolloop/delegate/dsh-goal 等） | 工具 owner |

## 6. 本次修改与验证

| 文件 | 修改 |
|---|---|
| `pkg/agent/prompt_turn.go:212` | `[Loaded image from tool result above]` → `[上方工具结果中的图片已加载]` |
| `pkg/agent/prompt_turn.go:226` | `[SubTurn Result] %s` → `[子代理结果] %s` |
| `pkg/agent/agent_test.go:4646` | 断言同步为新中文标记 |
| `pkg/agent/prompt_i18n_test.go`（新增测试） | `TestModelVisibleToolMarkersAreChinese` 锁定两个合成标记为中文 |

验证：

```bash
go test ./pkg/agent/... -count=1                 # PASS
go test ./pkg/agent -run 'TestModelVisible' -v   # 3/3 PASS
scripts/check-model-visible-chinese.py pkg/agent # 仅剩内部元数据候选（PromptPart.Title 等）
```

> 说明：`prompt_turn.go` 中的 `[上方工具结果中的图片已加载]`、`[子代理结果]` 与 task-42 的多消息/子代理范围相邻；若 task-42 owner 后续改动同一位置，以其实现为准（测试会同时覆盖）。

## 7. 相关文档

- [工具参考](tools.zh.md)
- [中文 Prompt 定稿](../design/agent-workspace-context-prompts.zh.md)
- [DSH 迁移报告](../design/dsh-migration-report.zh.md)


## 8. 最终审计（翻译轮后，2026-11-02）

- 审计命令：`python3 scripts/check-model-visible-chinese.py pkg/agent pkg/tools`（报告型）。
- 结果：**System prompt、注入 context、工具 Description/Parameters、工具结果包装/标记、默认回复全部中文**。
- 扫描器仍报的候选均为合理例外，分类如下（不进入模型文本或必须保留原文）：
  1. **机器协议标记**：XML tag（`<runtime_context>`/`<task_state>`/`<system-reminder>` 等）、`CONTEXT_SUMMARY:`、`\ No newline at end of file`（统一 diff 格式）。
  2. **技术标识**：工具名/参数名/JSON key/枚举值/状态值、路径/URL、provider/registry/MCP server 名。
  3. **外部文本**：MCP server 自身返回的 description/resource 文本、HTTP User-Agent/API endpoint 展示名、Gemini/Google Search 等专名。
  4. **内部实现**：provider 错误匹配串（`maximum context length`、`connection refused` 等，必须保留英文才能识别）、logger/事件/错误码、`PromptPart.Title`/`PromptSource.Description`（不渲染进模型）、OS errno 原文。
  5. **隐私遮罩标记**：`-----BEGIN PRIVATE KEY-----***`（mask 正则用）。
- 允许的英文占位符（不翻译）：工具名、`max_tool_iterations`、`config.json`、`SKILL.md`、`/use`、`/list skills`、`<skill>`、`status=running` 等。

> 结论：满足“模型可见提示词/prompt/工具说明/注入上下文全部中文”的要求；例外仅为机器协议或技术原文，已在本节记录。

## 9. 精确化修订（2026-11-03，按用户要求）

要求：去除“乐于助人/友好”等语气与形容词；模型必须精确理解用户意图、严格按验收条件完成任务；思考与回复使用精确、精简中文，只写进度/结果/证据，禁止语气词、形容词、寒暄、emoji、模糊指代与无关内容；回复不复述工作区绝对路径。

| 区域 | 修改 |
|---|---|
| identity / 规则 | 身份改为“严格按用户意图完成任务的执行型 Agent”；新增 `**精确执行**`、`**语言与表达**`；删除“乐于助人且准确”“情绪化填充” |
| 默认策略 | `pathToolPolicyPrompt` 增加 `## 任务与表达`（精确执行、精简中文、禁止模糊/绝对路径复述），按可用工具裁剪 |
| 上下文注入 | `workspacecontext/render.go` 基线/增量/作用域提示改为“以下为工作区指令，必须遵循……冲突以后者为准”；更新/移除语句改为精确陈述 |
| 技能 | catalog usage hint、system 技能 intro、激活技能指令改为精确条件与禁止推断 |
| 其他 | loop-guard、context-pressure、interrupt、task_round、summary prefix、agent 默认回复、agent discovery、legacy summary 提示统一去软化词、改精确陈述 |
| 测试 | 相关断言与 workspacecontext golden 同步更新；`pkg/agent`/`workspacecontext`/`pkg/config` 全绿 |

## 10. 任务与记忆策略（2026-11-11，按用户要求）

要求：默认 system prompt 增加“会话内容是否是需要执行的任务”的判定；需要执行的任务记录到专门文件；执行期间新增用户输入区分“原任务补充”与“新需求任务”；全部任务完成后标记完成并移入任务日志；遗留未完成任务按创建时间判定是否继续执行，并在输出中明确任务与判定；记忆只保留核心内容，清理历史堆积、临时状态与已恢复的异常，并在最终输出说明清理。

| 区域 | 修改 |
|---|---|
| 任务策略 | 新增 `kernel.task_policy`（P-21）：任务判定、`tasks/TASKS.md` 记录格式、新增输入归类、完成标记与 `tasks/TASK_LOG.md` 归档、7 天遗留任务判定与输出要求 |
| 记忆策略 | 新增 `kernel.memory_policy`（P-22）：`memory/MEMORY.md` 只保留跨会话核心内容；清理历史/一次性/临时/已恢复异常；清理结果在最终输出说明 |
| identity | 文件工具可用时新增强调：`**任务判定**`；`**记忆**` 扩写为核心内容与清理要求 |
| 门控 | 两条策略在 `read_file` 且 `write_file`/`edit_file` 可用时默认注入；`todo_write`/`task_update` 可用时追加协同句；工具规则禁用或缺少文件工具时整段省略 |
| 任务文件 | 任务板由运行时按需创建，不依赖仓库模板 |
| 测试 | 新增 `TestTaskAndMemoryPolicyPromptEnabledByDefault`、`TestTaskAndMemoryPolicyPromptRequiresFileTools`；i18n 断言补入两个中文标题；`pkg/agent` 全绿 |

## 11. 角色判定与履职策略（2026-11-11，按用户要求）

要求：身份类型与角色、任务所属行业/公司类型均改为按需判定，非必填；仅在判定能直接提升执行准确性时判定。判定后补充对应职责、专业标准与验收约束；未判定不得阻塞任务，也不能虚构角色或行业。

| 区域 | 修改 |
|---|---|
| 角色策略 | `kernel.role_policy`（P-23）：身份类型/角色、行业/公司类型均为按需判定，非必填；仅在能直接提升执行准确性时判定。需要判定时覆盖软件开发（前端/后端/移动端）、架构、SRE/运维、硬件/嵌入式、测试、数据/算法、安全、产品经理、产品设计（UX/UI）、产品开发、运营、商品运营、销售、市场、公关、政府关系、财务、投资融资、审计、法务、人力/行政及 CEO/CTO/CFO/COO/CMO/CLO 等管理角色；判定后补充职责、权限边界、专业标准与验收责任约束执行过程和结果；未判定按任务目标/验收标准/通用工程规范执行，不得虚构或阻塞 |
| identity | `**角色判定**` 默认注入，但内容为按需判定规则（身份与行业均非必填；判定后才约束，未判定不阻塞） |
| 任务策略 | `tasks/TASKS.md` 每条任务记录“身份类型与角色/职责（如已判定，可选）”“公司类型（如已判定，可选）”；未判定不强制补写；需要时重新判定并更新 |
| 门控 | 角色策略默认始终注入；工具规则启用时追加技能/工具协同句；文件工具可用时追加任务记录要求 |
| 任务文件 | 任务记录字段由 P-21/P-23 定义，运行时创建 `tasks/TASKS.md` 与 `tasks/TASK_LOG.md`；不依赖仓库模板 |
| 测试 | `TestRolePolicyPromptEnabledByDefault` 锁定非必填语义与旧强制文案排除；`TestRolePolicyPromptConditionalLines` 锁定按能力裁剪；`pkg/agent` 全绿 |
