# DSH 工具/功能差异与继续集成建议

> 核对日期：2026-11-02。
> DSH：`/Users/wudaorui/code/deepseek-harness`（`master @ 22ddee3792`，v0.2.1-rc.3，只读）。
> chagent：`/Users/wudaorui/code/chagent`（`v2 @ 5da70e0b` + 未提交修复工作树）。
> 方法：源码静态枚举（DSH `defineTool(`/`ctx.tools.register` 全量扫描 + bundle 依赖矩阵；chagent
> `instance.go`/`agent_init.go`/`agent_mcp.go` 注册点）+ 任务书约束核对。
> 证据：`build/functest-20261030/analysis/dsh-parity/`（`evidence-dsh-inventory.txt`、
> `evidence-chagent-inventory.txt`、`dsh-tool-lines.txt`、`tools-inventory.md`）。
> 本文只给差异与集成建议，不代表已实施。

## 0.1 迁移进展（2026-11-02 更新）

按“功能完全一致的不迁移，其他按 DSH 代码用 Go 重写并集成”的决策，已新增独立 DSH 工具组
`tools.dsh`（14 个工具）：`glob`/`grep`、`job_run`/`job_list`/`job_output`/`job_kill`、
`list_mcp_resources`/`list_mcp_resource_templates`/`read_mcp_resource`、
`session_search`/`session_trace`、`list_agents`/`wait_agent`/`interrupt_agent`、
`create_goal`/`get_goal`/`update_goal`。详见 [DSH 工具组](dsh-tools.zh.md)。
以下差异矩阵保留为设计依据；§4 的 P0/P1 建议已落地，剩余不适用项见 dsh-tools §3。

## 0. 结论摘要

- **DSH 默认启用约 40 个工具名**（base bundle + standard preset），另有约 20 个可选/实验工具。
  chagent 当前默认加载 **33 个内置工具**（不含 gateway 注册的 `cron`；含 DSH 11 个常驻工具），
  外加 `cron`、MCP 动态工具与 MCP resources 三件套；`reaction`/`spawn_status` 默认关闭。
  完整清单与开关见 [工具参考](tools.zh.md)。
- 大部分能力语义等价或被有意裁剪。已集成且默认启用：**glob/grep**、**后台 jobs**、
  **子代理控制面（list/wait/interrupt）**、**MCP resources 三件套**、**会话检索**。
  仍可评估：子代理消息 steer（`send_message`）、持久 PTY、结构化 patch 编辑器。
- 明确不建议：LSP、worktree、插件市场/管理器、交互式 ask_user、run_code、workflow/ralph（workflow 专项评估见 [workflow 必要性评估](workflow-assessment.zh.md)）、
  plan-mode、`working_directory` 等与 chess 单 agent/pico 场景不匹配的能力。

## 1. DSH 工具清单（默认启用 vs 可选）

### 1.1 默认启用（shipped bundle，约 40 个）

| 域 | 工具（file:line 见证据文件） | 说明 |
|---|---|---|
| 文件 | `read`、`read_image`、`write`、`edit` | `packages/fs/tool-fs` |
| 文件搜索 | **`glob`、`grep`** | `packages/fs/tool-fs-search`；默认即启用，支持隐藏/忽略文件与 rg 语法 |
| Shell | `bash`、`pwsh` | 一次性执行，含后台 job/沙箱升权语义 |
| 后台 jobs | **`job_output`、`job_list`、`job_kill`** | `packages/jobs/tool-jobs`；长命令后台执行与输出读取 |
| 技能 | `skill` | 渐进披露，读取 SKILL.md（与 chagent 同名） |
| 目标 | `get_goal`、`create_goal`、`update_goal` | durable goal + phase/blocked；goal-round driver 驱动续跑 |
| 计划 | `exit_plan_mode` | plan 模式切换（工具目录不变） |
| 子代理 | `subagent`、`subagent_fork`、`list_subagent_models` | base bundle 挂两次 subagent → fork 语义 |
| 子代理控制 | `send_message`、`interrupt_agent`、`list_agents` | `subagent-control` |
| 工作流 | `workflow` | JS 编排 fan-out subagent（需 Node PTC 运行时；chess 无需求，见 [必要性评估](workflow-assessment.zh.md)） |
| 待办 | `todo_write` | 整表替换 + todos 投影 |
| Web | `web_search`、`web_fetch` | 与 chagent 同名 |
| 会话 | `working_directory` | session cwd（与 chagent 固定 workspace 契约冲突） |
| MCP 资源 | **`list_mcp_resources`、`list_mcp_resource_templates`、`read_mcp_resource`** | `packages/mcp/mcp-resources`；base bundle 默认挂载 |
| 代码执行 | `run_code`（PTC） | base 默认执行器之一，ptc preset 切换 |
| 交互/展示 | `ask_user_question`、`present` | web-app standard preset |
| 调度 | `schedule_create/list/update/delete` | web-app standard preset |
| 插件 | `plugin_manager`、`plugin_market`、`cordis_inspect_list/query` | DSH 专属插件运行时 |

### 1.2 可选/实验（需显式插件/ profile）

`str_replace_editor`（Claude-Code 风结构化编辑）、persistent `bash`/`pwsh`、`load_workspace_dependencies`、
`subagent_oar`、agent-team 9 个（`spawn_teammate`/`wait_agent`/`team_task_*` 等）、`ralph`、
session-query 5 个（`session_search`/`session_trace`/`session_event_*`）、terminal 6 个
（`terminal_open/send/read/signal/close/list`）、`create_worktree`、`propose_action`、`lsp`（只读导航四件套）。

## 2. chagent 当前工具清单（33 默认内置 + cron + MCP 动态）

默认 33 个内置工具（不含 gateway 注册的 `cron`）：文件 `read_file`/`write_file`/`edit_file`/`append_file`/`list_dir`、
Shell `exec`、搜索 `glob`/`grep`、后台 `job_run`/`job_list`/`job_output`/`job_kill`、
任务 `task_get`/`task_update`/`todo_write`、目标 `create_goal`/`get_goal`/`update_goal`、
技能 `skill`/`find_skills`/`install_skill`、会话 `session_search`/`session_trace`、
子代理 `spawn`/`subagent`/`list_agents`/`wait_agent`/`interrupt_agent`、
消息 `message`/`send_file`/`load_image`、Web `web_search`/`web_fetch`。
另有 `cron`（gateway 注册）、`reaction`/`spawn_status`（默认关闭）、`delegate`（多 agent 时自动注册）、
MCP 动态 `mcp_<server>_<tool>` 与 MCP resources 三件套。
完整清单与开关见 [工具参考](tools.zh.md)。注册点：`pkg/agent/instance.go`、`agent_init.go`、
`agent_mcp.go`、`gateway.go`（cron）。

## 3. 差异矩阵

| 能力 | DSH | chagent | 结论 |
|---|---|---|---|
| 文件读/写/编辑 | read/write/edit/read_image | read_file/write_file/edit_file/append_file/load_image | ✅ 等价（命名不同）；DSH `str_replace_editor` 可选 |
| 文件搜索 | **glob/grep（默认）** | `glob`/`grep`（DSH 工具组，默认启用） | ✅ 已集成 |
| Shell | bash/pwsh（一次性/持久） | exec（一次性 + `background=true` 会话） | ✅ 后台命令由 `job_*` 覆盖；无持久 PTY |
| 后台 jobs | **job_list/output/kill（默认）** | `job_run`/`job_list`/`job_output`/`job_kill` | ✅ 已集成 |
| 任务/待办 | todo_write + goal 工具 | task_get/task_update/todo_write + goal 工具 | ✅ TaskState + todo + durable goal；注意三套状态的使用边界 |
| 子代理 | subagent/fork + list/interrupt/send/wait | spawn/subagent/delegate + list/wait/interrupt | ⚠️ 缺运行中子代理消息 steer（`send_message`） |
| 会话检索 | session-query（可选插件）：search/trace/event | `session_search`/`session_trace` | ✅ 已集成（不含 event surface） |
| MCP | 工具 + **resources 三件套（默认）** | MCP tools + resources 三件套（MCP 连接后注册） | ✅ 已集成 |
| 调度 | schedule_*（默认，提醒语义） | cron（定时任务） | ✅ 等价能力；接口形态不同 |
| 计划/目标驱动 | plan-mode / goal-round driver | TaskState + autonomy | ✅ 语义覆盖（任务书明确 chess 不需要 plan mode） |
| 交互 | ask_user_question、present | 无（message 单向） | ❌ 不适用于 pico 异步通道 |
| 工作目录/工作树 | working_directory / create_worktree | 固定 workspace | ❌ 与安全契约冲突，不做 |
| LSP/插件/代码执行/PTC/ralph/workflow | 若干 | 无 | ❌ 与 chess 单 agent 场景不匹配或依赖 DSH 运行时 |

仅 chagent 有：`message`/`reaction`/`send_file`（pico 通道）、`find_skills`/`install_skill`、`cron`、
`delegate`、`spawn_status`、`task_*`、`append_file`、`list_dir`（DSH 无 list_dir，靠 glob/bash）。

## 4. 建议下一批集成（含对任务书约束的处理）

| 优先级 | 候选 | 理由/场景 | 工作量/依赖 | 验收标准 |
|---|---|---|---|---|
| **P0（需决策）** | **`grep`/`glob`（只读、workspace 沙箱、结果上限）** | 真实开发/排障高频；DSH 默认启用；可显著减少 read/exec 往返 | 中；标准库 Walk+regexp/glob 即可，无新依赖；**与任务书 §12「不新增 grep/glob」冲突** | 先由 Lead/用户决策是否突破；若做：默认关闭开关、路径边界/结果上限/超时、错误可诊断、测试覆盖 |
| **P1** | **后台 jobs（`job_list`/`job_output`/`job_kill`）** | exec 60s 上限无法跑长命令；自主任务需要后台执行与轮询 | 中-高：进程组、输出缓冲/落盘、SIGTERM/清理、与 turn abort 关联 | 启动后台命令→轮询→kill；SIGTERM 无孤儿；超长输出落 spill；-count=3 稳定 |
| **P1** | **子代理控制面（`list_agents`/`interrupt_agent`/可选 `wait_agent`）** | 现有 spawn 无法查询/中断；多子任务编排依赖猜测 | 中：复用 subagent manager + 取消/状态查询 | 列举/中断/等待可用；取消不泄漏；会话隔离 |
| **P1** | **MCP resources 三件套（`list_mcp_resources`/`list_mcp_resource_templates`/`read_mcp_resource`）** | 最新 MCP 协议核心能力；DSH 默认启用；文件/知识库类 MCP server 常暴露 resources | 中：Go SDK 已有 ListResources/ReadResource API；工具包装 + 大小限制 + artifact | resources 可列/读/模板展开；错误可诊断；与 `docs/reference/mcp-support.zh.md` 矩阵一致 |
| **P2** | **会话检索（`session_search`/`session_trace`）** | 长任务复盘/自主排障；DSH 可选插件 | 低-中：只读 JSONL + 限行数/结果数 | 能按标记/轮次检索；结果带来源；不泄密 |
| **P2** | **持久 shell/terminal（可选）** | 构建/调试需保持 cwd/env | 高：会话生命周期/输出缓冲/清理 | 可选开关；默认不影响 exec；清理明确 |
| P3 | `str_replace_editor`、`schedule_*`、`read_image` 增强、goal 工具、LSP | 低收益/不匹配/依赖 DSH 运行时 | — | 不建议本轮投入 |

**关键约束说明**：任务书 `docs/design/agent-workspace-context-task-book.zh.md` §12 最终决策 6 明确
「不新增 working_directory/grep/glob/context_inspect 工具」。因此 `grep`/`glob` 的集成必须先做决策：
建议只突破 `glob`/`grep`（只读 + workspace 沙箱 + 无 shell 注入），继续维持不新增 `working_directory`；
替代方案是保留 `exec` + `rg`（结构化/可预测性更弱）。其余候选不触碰该约束。

## 5. 明确不建议集成

- `working_directory`、`create_worktree`：与固定 workspace/`restrict_to_workspace` 安全契约冲突；
- `ask_user_question`/`present`/action-cards：pico 是异步消息通道，无同步交互 UI/展示层；
- LSP、`run_code`(PTC)、`workflow`、`ralph`、agent-team、plugin_manager/plugin_market/cordis、
  subagent-claude-code/codex/acp 等：依赖 DSH 专属运行时/外部 CLI/多 agent 编排，chess 单 agent 无落地价值；
- plan-mode/goal-round driver：任务书明确 chess 不需要，TaskState+autonomy 已覆盖长任务。

## 6. 证据与相关文档

- 工具清单与扫描命令：`build/functest-20261030/analysis/dsh-parity/tools-inventory.md`；
- DSH 默认/可选工具与 file:line：`evidence-dsh-inventory.txt`；chagent 注册点：`evidence-chagent-inventory.txt`；
- DSH `defineTool` 原始行：`dsh-tool-lines.txt`；
- 任务书约束：`docs/design/agent-workspace-context-task-book.zh.md` §12；
- 相关对照：[上下文处理 vs DSH](../architecture/context-vs-dsh.zh.md)、[MCP 支持矩阵](mcp-support.zh.md)、
  [功能测试报告](../design/agent-functional-test-report.zh.md)。
