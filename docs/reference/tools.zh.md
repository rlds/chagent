# 工具参考（Tools）

> 适用范围：Chagent chess 基础 agent 的内置工具、启用方式、默认清单与安全边界。
> 关键代码路径：`pkg/agent/instance.go`、`pkg/agent/agent_init.go`、
> `pkg/gateway/gateway.go`、`pkg/tools/`、`pkg/tools/dsh/`、`pkg/tools/integration/`。
> 核对日期：2026-11-11。

工具是否可用由 `config.json` 的 `tools.<name>.enabled` 决定；**不写 tools 时使用默认值**。
编译进二进制的工具按默认/显式开关注册；MCP 工具在配置 server 后自动发现并注册。
默认配置（不含 MCP、不含 gateway 注册的 `cron`）加载 **33 个内置工具**；加入 `cron` 后为 34 个。

## 默认启用的内置工具

| 组 | 工具 | 说明 |
|---|---|---|
| 文件 | `read_file` | 读取工作区文件；默认 bytes 模式，支持 `offset`/`length` 分页（`mode=lines` 时为 `start_line`/`max_lines`，默认 64KB 上限）。 |
| 文件 | `write_file` | 写文件；覆盖已存在文件需 `overwrite=true`。 |
| 文件 | `edit_file` | 精确替换文本。 |
| 文件 | `append_file` | 追加内容。 |
| 文件 | `list_dir` | 列目录。 |
| 文件搜索 | `glob` | 按通配符查找文件，返回 workspace 相对路径（DSH 工具组）。 |
| 文件搜索 | `grep` | 按正则搜索文件内容，返回 `path:line: text`（DSH 工具组）。 |
| Shell | `exec` | 执行 shell；默认禁止危险模式、60s 超时；支持 `background=true` 后台会话。 |
| 后台任务 | `job_run` / `job_list` / `job_output` / `job_kill` | 后台进程组、输出读取、列表与终止（DSH jobs 子组）。 |
| 任务 | `task_get` / `task_update` | 读取/推进 durable TaskState（目标、阶段、轮次、待办、证据、阻塞）。 |
| 任务 | `todo_write` | 更新任务待办清单（写入 TaskState）。 |
| 目标 | `create_goal` / `get_goal` / `update_goal` | durable goal 文件（`state/goals/*.json`，DSH goal 子组）。 |
| 技能 | `skill` | 按技能名加载完整指令（渐进披露）。 |
| 技能 | `find_skills` / `install_skill` | 搜索/安装技能（需要 `tools.skills.enabled`）。 |
| 会话 | `session_search` / `session_trace` | 检索/追踪已持久化会话 JSONL（DSH session_query 子组）。 |
| 子代理 | `spawn` | 派生后台 subagent 任务（异步）。 |
| 子代理 | `subagent` | 同步执行 subagent 任务并返回结果。 |
| 子代理控制 | `list_agents` / `wait_agent` / `interrupt_agent` | 列举/等待/中断 spawn 产生的任务（需要 subagent 启用）。 |
| 消息 | `message` | 向当前 pico 会话发送文本/媒体。 |
| 消息 | `send_file` | 发送本地文件。 |
| 图片 | `load_image` | 加载本地图片给视觉模型。 |
| Web | `web_search` / `web_fetch` | 网络搜索/抓取（默认启用，受 `tools.web` 配置影响）。 |
| 调度 | `cron` | 管理定时任务；在 gateway 启动时由 `setupCronTool` 注册（`tools.cron.enabled`，默认 true）。 |

说明：

- `reaction`（表情回应）和 `spawn_status`（旧版子代理状态）**默认关闭**；需要时显式启用。
- `delegate` 只在 **配置了多个 agent** 时自动注册；扁平开关 `tools.delegate.enabled=false` 可关闭。
- `spawn`/`spawn_status` 依赖 `subagent` 同时启用；子代理控制面依赖 `spawn` 或 `subagent` 已启用。
- `find_skills`/`install_skill` 的启用还依赖 `tools.skills.enabled`。

## 工具关系与重叠说明

工具名全局唯一，注册表按名称去重；没有同名重复注册。以下工具存在**有意的能力重叠**，按场景选择：

| 工具组 | 重叠点 | 建议 |
|---|---|---|
| `task_update` / `todo_write` / `create_goal` 等 | 三者都记录任务/目标状态，但层级不同：TaskState 管会话任务阶段，todo_write 管步骤清单，goal 是跨会话 durable goal | 默认长任务用 TaskState + todo；需要跨会话目标时用 goal，不要同时维护三份互相矛盾的状态。 |
| `spawn` / `subagent` / `delegate` | 都能委派任务；spawn 异步、subagent 同步、delegate 跨 agent | 单 agent 内部并行用 spawn/subagent；多 agent 且目标 agent 更合适时用 delegate。 |
| `spawn_status` / `list_agents` / `wait_agent` | 都查询子代理状态；list/wait 是 DSH 控制面，spawn_status 是旧版兼容工具 | 新代码优先 `list_agents`/`wait_agent`；`spawn_status` 默认关闭。 |
| `exec` 后台会话 / `job_run` | 都能后台执行命令；exec 管 shell 会话，jobs 管持久后台进程与输出 | 简短后台命令用 exec `background=true`；长任务、需要列表/输出/终止时用 `job_*`。 |
| `message` 媒体 / `send_file` | `message` 在 `media_enabled=true` 时可发送媒体，`send_file` 专用于文件 | 纯文本用 message；明确发送本地文件用 send_file。 |
| `read_file` / `load_image` | 都能读取本地内容，但 load_image 把图片送入视觉模型 | 文本用 read_file；图片交给视觉模型用 load_image。 |

## 工具开关（默认嵌套写法）

默认值已是最优，通常不需要写 `tools`。需要显式关闭可选工具时：

```json
{
  "tools": {
    "web": { "enabled": false },
    "send_file": { "enabled": false },
    "reaction": { "enabled": true },
    "dsh": {
      "enabled": true,
      "jobs": { "enabled": false },
      "goal": { "enabled": false }
    }
  }
}
```

注意标准形状是 `tools.<group>.<enabled>`（如 `tools.dsh.jobs.enabled`），不是 `tools.enabled` 映射。

### 扁平工具开关兼容

外部配置界面可能按工具名输出扁平开关（`tools.glob.enabled`、`tools.task_get.enabled`、
`tools.job_run.enabled` 等）。加载器接受这些键并归一化到分组配置：

| 扁平开关 | 归一化目标 |
|---|---|
| `tools.task_get` / `tools.task_update` | `tools.task.enabled` |
| `tools.todo_write` | `tools.todo.enabled` |
| `tools.skill` | `tools.skill_load.enabled` |
| `tools.glob` / `tools.grep` | `tools.dsh.glob` / `tools.dsh.grep` |
| `tools.job_run` / `job_list` / `job_output` / `job_kill` | `tools.dsh.jobs.enabled` |
| `tools.create_goal` / `get_goal` / `update_goal` | `tools.dsh.goal.enabled` |
| `tools.session_search` / `session_trace` | `tools.dsh.session_query.enabled` |
| `tools.list_agents` / `wait_agent` / `interrupt_agent` | `tools.dsh.subagent_control.enabled` |
| `tools.list_mcp_resources` / `list_mcp_resource_templates` / `read_mcp_resource` | `tools.dsh.mcp_resources.enabled` |
| `tools.delegate` | 仅显式 `false` 时关闭多 agent 自动注册 |

规则：同一分组内任一扁平开关为 `false` 时关闭该分组；显式 `true` 不覆盖嵌套配置中的显式关闭。
标准写法仍优先，新配置请使用分组字段。

## MCP 工具

MCP server 的地址/鉴权必须外部提供（这是唯一必要的外部工具配置）；配置后 MCP
自动启用，不需要再写 `tools.mcp.enabled=true`：

```json
{
  "tools": {
    "mcp": {
      "servers": {
        "chess": {
          "type": "http",
          "url": "https://mcp.example.invalid/mcp",
          "enabled": true,
          "headers": { "Authorization": "Bearer YOUR_MCP_TOKEN" }
        }
      }
    }
  }
}
```

MCP 工具名为 `mcp_<server>_<tool>`。连接成功后还会注册 MCP resources 三件套
（`list_mcp_resources`、`list_mcp_resource_templates`、`read_mcp_resource`）。
工具发现默认关闭（`tools.mcp.discovery.enabled=false`）；开启后隐藏工具可通过
`tool_search_tool_bm25` / `tool_search_tool_regex` 按需发现。
显式写 `"tools.mcp.enabled": false` 可关闭 MCP（即使配置了 servers）；省略则按 servers 自动启用。
协议/传输/特性支持矩阵见 [MCP 支持矩阵](mcp-support.zh.md)。

## 文件路径与权限

默认安全边界：

| 项 | 默认 | 说明 |
|---|---|---|
| `agents.defaults.restrict_to_workspace` | `true` | 文件/exec 只能访问工作区 |
| `agents.defaults.allow_read_outside_workspace` | `false` | 不允许越界读 |
| `tools.allow_read_paths` | 空 | 仅在工作区外读取时显式列出的正则路径 |
| `tools.allow_write_paths` | 空 | 仅在工作区外写入时显式列出的正则路径 |

结论：**工作区内不需要任何外部路径配置**；只有在确实需要访问工作区外文件时，
才必须显式配置 `allow_read_paths` / `allow_write_paths`（安全敏感，属于必要外部配置）。
越界访问返回 `access denied: path is outside the workspace`。

## Hook 配置

Hook 是可选能力：不配置 hooks 时无需任何外部配置，也不会挂载任何 hook。
需要扩展时再在 `config.json` 写 `hooks.builtins` / `hooks.processes`；
`hooks.defaults` 的超时时间有默认值（observer 500ms / interceptor 5000ms /
approval 60000ms）。`hooks.enabled` 默认 true，但没有定义 hook 时无副作用。

## 注册位置

- 文件/任务/技能/DSH 常驻工具：`pkg/agent/instance.go`；
- 共享工具（web/message/reaction/send_file/load_image/skill 发现/spawn/subagent/delegate）：`pkg/agent/agent_init.go`；
- MCP 动态工具与 resources：`pkg/agent/agent_mcp.go` + `pkg/tools/integration/mcp_tool.go`；
- `cron`：`pkg/gateway/gateway.go` 的 `setupCronTool`。

## 工具缺口与后续候选

当前默认清单已覆盖文件、搜索、Shell/后台任务、任务/目标、技能、会话检索、
子代理及控制面、消息、Web、MCP tools/resources 与定时任务。仍可评估的缺口：

- 子代理消息 steer（DSH `send_message`）：当前只有 list/wait/interrupt，没有向运行中子代理追加消息的能力；
- 结构化 patch 编辑器（DSH `str_replace_editor`）：`edit_file` 已覆盖常规替换；
- 持久 PTY shell（DSH `terminal_*`）：`exec background` + `job_*` 已覆盖多数长任务场景。

上述候选需单独评估，不属于当前默认工具缺口。

## 相关文档

- [Chess 配置](../guides/chess-config.zh.md)
- [工作区上下文](../architecture/workspace-context.zh.md)
- [任务状态与自主](../architecture/taskstate-autonomy.zh.md)
- [DSH 工具组](dsh-tools.zh.md)
