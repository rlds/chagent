# 工具参考（Tools）

> 适用范围：Chagent chess 基础 agent 的内置工具、启用方式与安全边界。
> 关键代码路径：`pkg/agent/instance.go`、`pkg/agent/agent_init.go`、
> `pkg/tools/`、`pkg/tools/fs/`、`pkg/tools/integration/`。
> 核对日期：2026-10-30

工具是否可用由 `config.json` 的 `tools.<name>.enabled` 决定；**不写 tools 时使用默认值**。
不需要外部提供工具清单：编译进二进制的工具按默认/显式开关注册；MCP 工具在配置
server 后自动发现并注册。

## 默认启用的核心工具

| 工具 | 说明 |
|---|---|
| `read_file` | 读取工作区文件；默认 64KB 上限。 |
| `write_file` | 写文件；覆盖已存在文件需 `overwrite=true`。 |
| `edit_file` | 精确替换文本。 |
| `append_file` | 追加内容。 |
| `list_dir` | 列目录。 |
| `exec` | 执行 shell；默认禁止危险模式、60s 超时。 |
| `task_get` / `task_update` | 读取/推进 durable TaskState。 |
| `todo_write` | 更新任务清单。 |
| `skill` | 按技能名加载完整指令（渐进披露）。 |
| `message` | 向当前 pico 会话发送文本/媒体。 |
| `reaction` | 给消息添加表情回应。 |
| `send_file` | 发送本地文件。 |
| `load_image` | 加载本地图片给视觉模型。 |
| `find_skills` / `install_skill` | 搜索/安装技能（需要 skills 开关）。 |
| `spawn` / `spawn_status` / `subagent` | 子代理任务。 |
| `delegate` | 委托给目标 agent。 |
| `web_search` / `web_fetch` | 网络搜索/抓取（默认启用，受 web 配置影响）。 |
| `cron` | 管理定时任务；在 gateway 启动时由 `setupCronTool` 注册（`tools.cron.enabled`，默认 true）。 |

`spawn`/`spawn_status` 依赖 `subagent` 同时启用。
MCP 工具名为 `mcp_<server>_<tool>`，只在配置了 `tools.mcp.servers` 时出现；协议/传输/特性支持矩阵见 [MCP 支持矩阵](mcp-support.zh.md)。

## 工具开关（可选）

默认值已是最优，通常不需要写 `tools`。需要显式关闭可选工具时：

```json
{
  "tools": {
    "web": { "enabled": false },
    "send_file": { "enabled": false },
    "find_skills": { "enabled": false }
  }
}
```

注意形状是 `tools.<tool_name>.enabled`，不是 `tools.enabled` 映射。

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

工具发现默认关闭（`tools.mcp.discovery.enabled=false`）；需要隐藏工具按需搜索时再开启。
显式写 `"tools.mcp.enabled": false` 可关闭 MCP（即使配置了 servers）；省略则按 servers 自动启用。
MCP 协议版本/传输/最新协议特性（`structuredContent`、roots、list_changed、elicitation、
tasks）的支持矩阵与已知限制详见 [MCP 支持矩阵](mcp-support.zh.md)。

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

- 文件/任务/技能工具：`pkg/agent/instance.go`；
- 共享工具（web/message/spawn/subagent/delegate 等）：`pkg/agent/agent_init.go`；
- MCP 动态工具：`pkg/agent/agent_mcp.go` + `pkg/tools/integration/mcp_tool.go`。

## 相关文档

- [Chess 配置](../guides/chess-config.zh.md)
- [工作区上下文](../architecture/workspace-context.zh.md)
- [任务状态与自主](../architecture/taskstate-autonomy.zh.md)
