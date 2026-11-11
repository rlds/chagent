# 变更摘要（2026-11-11）

> 范围：默认 system prompt 的任务/记忆/角色策略、工作区任务板、工具开关兼容与工具清单核对。
> 基线：`e4a86dd`（仓库快照 ddmcv4）
> 关键提交：
> - `e80e0a2` feat(agent): 默认提示词增加任务、记忆与角色判定策略
> - `49bf30c` fix(config): 兼容扁平工具开关并核对默认工具清单
> - 后续清理提交：删除未被引用的仓库 `workspace/` 示例目录

## 1. 背景与目标

本轮变更解决三类问题：

1. 默认 system prompt 缺少“输入是否为任务”“任务如何记录/归档/接管”“角色与职责如何判定”的稳定协议；
2. 外部配置界面输出的按工具名扁平开关（`tools.glob`、`tools.task_get` 等）会被当前加载器拒绝，导致 gateway 无法启动；
3. 默认工具清单、重叠关系与缺口缺少权威中文文档。

## 2. 默认系统提示词

新增三条 kernel/policy 策略，见 `pkg/agent/prompt_policy.go`：

| ID | 来源 | 注入条件 | 作用 |
|---|---|---|---|
| P-21 | `kernel.task_policy` | `read_file` + `write_file`/`edit_file` 可用且未禁用工具规则 | 判定输入是否为任务；`tasks/TASKS.md` 记录；新增输入归类；完成归档；遗留任务判定 |
| P-22 | `kernel.memory_policy` | 同 P-21 | 记忆只保留核心内容；清理历史/临时/已恢复异常；清理结果需说明 |
| P-23 | `kernel.role_policy` | 默认始终注入 | 必须判定身份类型与职责；行业/公司类型为可选补充；按角色专业标准执行 |

要点：

- **任务判定**：需要产生可验证结果的输入是任务；纯问答/解释/闲聊不建任务。
- **任务板**：未完成任务记录在 `tasks/TASKS.md`；每条含编号、创建时间、来源、身份类型与角色/职责（必填）、公司类型（如已判定，可选）、目标、验收标准、状态与证据；全部完成后归档到 `tasks/TASK_LOG.md`。
- **新增输入归类**：执行期间的新输入先判定“原任务补充/修正”或“独立新任务”；补充合入原任务，新任务单独登记。
- **遗留任务**：按创建时间与上下文判定；7 天内或仍相关 → 继续；超过 7 天且无进展/依赖变化 → 可判定不执行并标记 `abandoned`；判定结果必须在新的输出开头列出（编号、目标、判定、理由）。
- **角色判定**：身份类型与职责是必判定项；角色覆盖商业公司角色（前端/后端/架构/SRE、产品设计/开发、运营、商品运营、销售、市场公关、政府关系、财务、投资融资、审计、法务、人力、管理层等）；行业/公司类型（电商、生鲜电商、半导体、芯片设计、AI、机械设计、无人机、机器人、餐饮等）为可选补充；判定后按角色职责与专业标准执行，并在阶段输出中给出角色、职责、依据。
- **identity 规则**：新增 `**任务判定**`、`**角色判定**`；`**记忆**` 改为只保存跨会话核心内容并清理堆积。
- **门控**：角色策略不依赖文件工具，默认始终注入；任务/记忆策略在缺少文件工具或禁用工具规则时整段省略，避免要求模型写不可访问的文件。

## 3. 工作区任务板

任务板协议由 P-21 定义：模型在任务产生时于实际工作区创建 `tasks/TASKS.md`，全部完成后归档到
`tasks/TASK_LOG.md`，不依赖仓库内模板。`e80e0a2` 曾加入 `workspace/tasks/` 示例模板；
后续清理确认仓库 `workspace/` 示例目录未被代码、脚本、测试或打包引用，已整体删除。

## 4. 配置与工具

### 4.1 扁平工具开关兼容

新增 `pkg/config/flat_tool_aliases.go`，接受外部界面输出的按工具名扁平开关并归一化：

| 扁平开关 | 归一化目标 |
|---|---|
| `tools.task_get` / `task_update` | `tools.task.enabled` |
| `tools.todo_write` | `tools.todo.enabled` |
| `tools.skill` | `tools.skill_load.enabled` |
| `tools.glob` / `grep` | `tools.dsh.glob` / `grep` |
| `tools.job_run` / `job_list` / `job_output` / `job_kill` | `tools.dsh.jobs.enabled` |
| `tools.create_goal` / `get_goal` / `update_goal` | `tools.dsh.goal.enabled` |
| `tools.session_search` / `session_trace` | `tools.dsh.session_query.enabled` |
| `tools.list_agents` / `wait_agent` / `interrupt_agent` | `tools.dsh.subagent_control.enabled` |
| `tools.list_mcp_resources` / `list_mcp_resource_templates` / `read_mcp_resource` | `tools.dsh.mcp_resources.enabled` |
| `tools.delegate` | 仅显式 `false` 时关闭多 agent 自动注册 |

规则：同一分组任一扁平开关为 `false` 即关闭该分组；显式 `true` 不覆盖嵌套配置中的显式关闭；标准嵌套写法优先。

### 4.2 工具清单核对结果

默认配置（不含 MCP、不含 gateway 注册的 `cron`）加载 **33 个内置工具**；加入 `cron` 后 34 个。

- 默认关闭：`reaction`、`spawn_status`。
- 多 agent 时自动注册：`delegate`；扁平 `tools.delegate.enabled=false` 可关闭。
- MCP 连接后追加 `mcp_<server>_<tool>` 与 resources 三件套。
- 工具名无重复；以下为有意的功能重叠：
  - `task_update` / `todo_write` / `create_goal` 等：会话 TaskState、步骤清单、durable goal 三层状态；
  - `spawn` / `subagent` / `delegate`：异步/同步/跨 agent 委派；
  - `spawn_status` / `list_agents` / `wait_agent`：状态查询；
  - `exec background` / `job_run`：后台执行；
  - `message` 媒体 / `send_file`；`read_file` / `load_image`。
- 仍可评估的缺口：子代理消息 steer（DSH `send_message`）、`str_replace_editor`、持久 PTY shell；不属于当前默认运行缺口。

### 4.3 提示词与工具参数对齐

修正 `pkg/agent/prompt_policy.go` 中的分页提示：`read_file` 默认 bytes 模式使用 `offset`/`length`，行模式使用 `start_line`/`max_lines`；原“offset/limit”与实际 schema 不一致。

## 5. 文档更新

- `docs/reference/tools.zh.md`：默认工具清单、开关、扁平兼容、重叠关系、缺口与注册位置。
- `docs/reference/dsh-tools.zh.md`：DSH 分组配置与扁平开关兼容。
- `docs/reference/dsh-parity.zh.md`：更新默认工具数量、已集成能力与剩余差异。
- `docs/guides/configuration.zh.md`：补充扁平开关兼容与归一化规则。

## 6. 测试与真实模型验证

### 6.1 自动化测试

| 检查 | 结果 |
|---|---|
| `go test ./pkg/... -count=1` | 通过 |
| `go vet ./...` | 通过 |
| `./scripts/lint-docs.sh` | 通过 |
| `go test ./pkg/config -run TestLoadConfigFlatToolAliases` | 通过 |
| `go test ./pkg/agent -run 'TestTaskAndMemoryPolicy|TestRolePolicy'` | 通过 |

新增测试：

- `pkg/config/flat_tool_aliases_test.go`：扁平开关归一化、全 true 不改默认、未知工具仍拒绝。
- `pkg/agent/prompt_policy_test.go`：P-21/P-22 默认注入与文件工具门控；P-23 默认注入、角色范围、条件协同句。
- `pkg/agent/prompt_i18n_test.go`：新增中文标题断言。

说明：全量测试首轮曾出现一次 `TestGatewayConcurrentReloadCalls` 的 503 时序抖动；该测试单独 `-count=3` 与全量重跑均通过。

### 6.2 真实 agent 流程

使用配置 `/Users/wudaorui/chessdata/weichat/weichat/config.json`（测试脚本仅提取模型与网关配置）：

- `scripts/live-agent-test.py`：gateway health、pico ping/pong、真实模型对话、工作区指令注入、`/context`、`task_update`/`todo_write`/`task_get`、TaskState 持久化、SIGTERM/pid 清理全部通过。
- 任务流验证：模型创建 `tasks/TASKS.md`，记录身份类型/职责、可选公司类型、验收标准；补充输入合并到原任务；新需求登记为独立任务。
- 记忆验证：模型删除历史过程、已恢复异常与临时状态，保留核心偏好，并在最终输出说明清理内容。
- 配置验证：修复前使用原配置启动 gateway 会因 22 个 unknown field 失败；修复后用临时 workspace 副本启动，`/health` 通过、33 个内置工具加载。

## 7. 注意事项与后续

1. 运行中的 gateway（PID 86130，旧二进制）不会自动应用本次修复；部署新二进制后需重启该 gateway。
2. 用户原始 `config.json` 未被改写；扁平开关兼容在加载期生效。
3. 子代理消息 steer（`send_message`）尚未实现；如需要运行中子代理追加指令，应单独立项。
4. `task_update` / `todo_write` / goal 工具的状态边界需在后续使用中保持一致，避免三套状态互相矛盾。
5. 仓库 `workspace/` 示例目录已删除：代码、脚本、测试、打包均未引用；任务文件由运行时按需创建。

## 8. 变更文件

```text
pkg/agent/prompt_policy.go
pkg/agent/context.go
pkg/agent/prompt.go
pkg/agent/prompt_policy_test.go
pkg/agent/prompt_i18n_test.go
pkg/agent/agent_init.go
pkg/config/config.go
pkg/config/config_loader.go
pkg/config/flat_tool_aliases.go
pkg/config/flat_tool_aliases_test.go
docs/architecture/taskstate-autonomy.zh.md
docs/design/agent-workspace-context-prompts.zh.md
docs/reference/model-facing-text.zh.md
docs/reference/tools.zh.md
docs/reference/dsh-tools.zh.md
docs/reference/dsh-parity.zh.md
docs/guides/configuration.zh.md
docs/README.md
docs/project/change-summary-2026-11-11.zh.md
```

## 9. 复现命令

```bash
# 全量测试
go test ./pkg/... -count=1
go vet ./...
./scripts/lint-docs.sh

# 真实 agent 流程
python3 scripts/live-agent-test.py /Users/wudaorui/chessdata/weichat/weichat/config.json
```

工具清单核对方式：用临时 Go 程序调用 `config.LoadConfig` 与 `agent.NewAgentLoop`，打印
`agent.Tools.List()`；当前默认清单的权威结果已固化在
[工具参考](../reference/tools.zh.md)。