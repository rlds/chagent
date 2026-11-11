# DSH 工具组（独立分组）

> 日期：2026-11-02。实现位置：`pkg/tools/dsh/*`、注册点 `pkg/agent/instance.go`（文件/会话/目标/jobs）、
> `pkg/agent/agent_mcp.go`（MCP resources）、`pkg/agent/agent_init.go`（子代理控制）、配置 `pkg/config/dsh_tools_config.go`。
> DSH 参考：`packages/{fs/tool-fs-search,jobs/tool-jobs,mcp/mcp-resources,experimental/tool-session-query,subagent/tool-subagent-control,goal/tool-goal}`。
> 原则：**功能与 Chagent 已有工具完全一致的不迁移**；DPH 原生但 Chagent 缺失、对 chess 场景有价值的工具用 Go 重写，
> 统一放入独立工具组（`PromptMetadata{capability, dsh, tool_registry:dsh}`），可整组/分组开关。

## 1. 配置

```json
{
  "tools": {
    "dsh": {
      "enabled": true,
      "fs_search": { "enabled": true },
      "jobs": { "enabled": true },
      "mcp_resources": { "enabled": true },
      "session_query": { "enabled": true },
      "subagent_control": { "enabled": true },
      "goal": { "enabled": true }
    }
  }
}
```

- 省略 `enabled` = 启用；显式 `false` 关闭该组/子组。
- 组工具遵守 agent 的 `tools` allowlist（与原有工具一致）。
- 关闭 MCP 或未配置 server 时，`mcp_resources` 子组不会注册工具；关闭 subagent 时 `subagent_control` 不注册。

## 2. 已迁移工具（14 个）

| 子组 | 工具 | 行为/上限 | 安全边界 |
|---|---|---|---|
| 文件搜索 | `glob`、`grep` | `glob` 支持 `**/*` 通配，返回 workspace 相对路径；`grep` 支持 Go 正则、大小写/文件过滤，返回 `path:line: text`；结果上限/超时 | 仅 workspace（沿用 `restrict_to_workspace`/`allow_read_paths`）；不跟随符号链接；只读、无 shell |
| 后台 jobs | `job_run`、`job_list`、`job_output`、`job_kill` | 后台进程组；输出落 `<ws>/.chagent/jobs/<id>/output.log`（0700/0600，4MiB 截断）；`job_output` 支持 offset/tail/wait；per-job timeout；并发上限 8，保留最近 50 | 工作区 cwd；deny/allow 模式同 exec；拒绝 `..`/越界绝对路径；`Close()`/reload 终止全部 job |
| MCP 资源 | `list_mcp_resources`、`list_mcp_resource_templates`、`read_mcp_resource` | 读取已连接 MCP server 的 resources；分页；大文本/二进制落 `<ws>/.artifacts/mcp-resources/`，二进制不进上下文 | 只读；server 未连接/歧义/超限可诊断；URI/大小上限 |
| 会话检索 | `session_search`、`session_trace` | `search` 字面/正则检索 `sessions/*.jsonl`，返回 session/turn/`path:line`/role/snippet；`trace` 输出会话与回合时间线摘要 | 仅 `<ws>/sessions` 只读；Lstat 拒绝符号链接；路径穿越防护；文件/字节/超时上限；片段密钥遮罩 |
| 子代理控制 | `list_agents`、`wait_agent`、`interrupt_agent` | 列举/等待/取消 spawn 产生的子代理任务；真实取消才报“已中断”，不可中断明确报错 | 按 channel/chat 可见性过滤；结果截断；超时 |
| 目标 | `create_goal`、`get_goal`、`update_goal` | durable goal 存 `<ws>/state/goals/<id>.json`（0700/0600 原子写）；状态 active/complete/blocked；complete/blocked 需 note/evidence | 仅 workspace；损坏文件跳过并诊断；并发安全 |

## 3. 不迁移的工具及原因

### 3.1 功能与现有工具完全一致（按用户要求跳过）

| DSH 工具 | Chagent 等价 |
|---|---|
| `read`/`write`/`edit` | `read_file`/`write_file`/`edit_file`（+`append_file`/`list_dir`） |
| `bash`/`pwsh` | `exec`（一次性 shell） |
| `web_search`/`web_fetch` | 同名工具 |
| `skill` | `skill` |
| `todo_write` | `todo_write` |
| `schedule_create/list/update/delete` | `cron`（定时任务语义等价） |
| `read_image` | `load_image` |
| `str_replace_editor` | `edit_file` 已覆盖文本替换；结构化 patch 编辑器作为后续可选项 |

### 3.2 DSH 运行时/UI 专属，chess 无可落地语义（不迁移）

| DSH 工具 | 不迁移原因 |
|---|---|
| `working_directory` | 与“固定 workspace + restrict_to_workspace”安全契约冲突，任务书 §12 明确不新增 |
| `terminal_open/read/send/signal/close/list` | 需要持久 PTY 会话管理；`exec` + `jobs` 已覆盖长命令场景；如确有需要可作为后续独立子组 |
| `exit_plan_mode` / plan-mode | 依赖 DSH plan 模式状态机；任务书明确 chess 不迁移 plan mode |
| `ask_user_question` | pico 是异步消息通道，无同步交互 UI；需要用户输入应通过普通消息轮次 |
| `present` / `propose_action` / action-cards | 依赖 DSH Web/交付层，pico 无卡片 UI |
| `plugin_manager` / `plugin_market` / `cordis_inspect_*` | DSH 插件运行时专属 |
| `workflow` / `ralph` / `agent-team`（`spawn_teammate`/`team_task_*` 等） | DSH 多 agent/JS 编排运行时；`workflow` 需 Node PTC 运行时且网络不受文件策略限制，chess 无此需求——专项评估见 [workflow 必要性评估](workflow-assessment.zh.md)，结论不迁移；`ralph`/agent-team 同理 |
| `lsp` | 需要语言服务器进程与项目语言配置，依赖重、chess 收益低 |
| `create_worktree` | git worktree 管理，chess 单 workspace 场景不需要 |
| `load_workspace_dependencies` | DSH skill 依赖机制专属；Chagent 已有技能安装 |
| `run_code`（PTC） | DSH 代码执行沙箱执行器，Chagent 无对应沙箱运行时不迁移 |
| `subagent_oar` | DSH 实验性 OAR 子代理扩展 |

> 结论：14 个 Go 重写工具已覆盖 DSH 中 Chagent 缺失且有意义的能力；其余按“等价/不适用”明确排除，后续如需可单独立项。

## 4. 生命周期与清理

- jobs：`AgentInstance.Close()` 关闭 `DSHJobs`；`AgentRegistry.Close()` 在 gateway 关停时逐个关闭；配置 reload 时
  `AgentRegistry.CloseBackgroundJobs()` 终止旧 generation 的后台 job，避免遗留子进程。
- 其他工具无后台进程：goal 使用原子文件写；session/glob/grep 只读；MCP resources 使用已连接 session；
  subagent control 复用 `SubagentManager` 生命周期。
- jobs manager 提供 `Close()` 幂等；关闭后 `job_run` 返回 `manager_closed`。

## 5. 测试与证据

```bash
go test ./pkg/tools/dsh/... -count=1
go test -race ./pkg/tools/dsh/jobs/... ./pkg/tools/dsh/sessionquery/... -count=1
go test ./pkg/agent -run TestDSHToolGroup -count=1
```

- 各子包单测：fssearch（通配/忽略/越界/符号链接）、jobs（29 例，含进程组 kill/并发上限/清理，coverage 83.5%）、
  mcpresources（13 例，fake manager + SDK in-memory session）、sessionquery（24 例，损坏行/遮罩/超时）、
  subagentctl（18 例，可中断/不可中断语义）、goal（create/get/update/持久化/并发/损坏）。
- Agent 集成测试 `pkg/agent/dsh_tools_test.go` 锁定：默认注册 11 个常驻工具、组开关关闭时全部消失、子组开关单独生效。
- 证据：`build/functest-20261030/`（历史）；本轮各包测试日志可由上述命令复跑。

## 6. 已知限制 / 后续

1. `job_output` 无 agent 完成通知/wakeup（需模型轮询或 `wait=true`）；无 stale-job 启动清扫（上次崩溃遗留的
   `job.json` 不会进入 List，仅留磁盘）。
2. `session_search/trace` 仅覆盖已持久化 JSONL，不支持 DSH 的 event surface/lineage/cursor 分页。
3. `subagent` 同步工具任务不登记到控制面（仅 `spawn` 创建的后台任务可 list/wait/interrupt）。
4. MCP resources 仅暴露为工具，未接入 MCP `notifications/resources/list_changed` 自动刷新；
   MCP 工具侧 `tools/list_changed` 目前为 Manager 层刷新+回调，agent 侧重注册仍待接线。
5. Windows 上 jobs 无法可移植地杀整棵进程树（仅直接子进程），Linux/macOS 已用进程组验证。
