# 工作区上下文（Workspace Context）

> 适用范围：Chagent chess 基础 agent 的工作区指令发现、渲染、注入与失效机制。
> 关键代码路径：`pkg/agent/workspacecontext/`、`pkg/agent/workspace_context_runtime.go`、
> `pkg/config/context_config.go`。
> 核对日期：2026-10-29

工作区上下文让 agent 在每一轮与工作区相关的交互中都能看到项目级指令
（`AGENTS.md` / `CLAUDE.md` / 本地覆盖文件等），并跟踪这些文件的浏览与修改，
从而保持上下文一致性。chess profile 默认**开启**注入（`enabled=true`），
只扫描 agent 工作区（`user_global=false`）。

## 配置与默认值

配置位于 `agents.defaults.workspace_context`（见 `pkg/config/context_config.go`）：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 是否启用工作区上下文注入 |
| `max_bytes` | `32768` | 注入到上下文的累计最大字节数 |
| `max_source_bytes` | `1048576` | 读取候选文件时单文件字节上限保护 |
| `project_root_markers` | `[".git"]` | 标记项目根的目录/文件名 |
| `instruction_files` | `["AGENTS.md", "CLAUDE.md"]` | 全局指令文件名 |
| `local_instruction_files` | `["AGENTS.local.md", "CLAUDE.local.md"]` | 本地覆盖指令文件名 |
| `user_global` | `false` | 是否把用户全局目录（`~/.agents`）纳入扫描 |
| `follow_symlinks` | `false` | 是否跟随符号链接 |

环境变量前缀：`CHAGENT_AGENTS_DEFAULTS_WORKSPACE_CONTEXT_*`。
配置缺省时由 `ApplyDefaults()` 回填默认值；`Validate()` 拒绝非法候选名
（含 `/`、`\`、`..`）或非正数限制。

## 发现与投影（pkg/agent/workspacecontext）

- `discovery.go`：从当前目录向上寻找项目根（命中 `project_root_markers`），
  收集候选指令文件。
- `render.go`：把候选文件渲染成上下文片段，统计消耗字节，超出 `max_bytes` 时截断。
- `compose.go`：把全局指令与本地覆盖指令按优先级组合成最终注入内容。
- `digest.go`：计算内容摘要，用于失效检测。
- `projector.go`：`Projector` 封装完整发现→渲染→组合流程，供 `AgentLoop` 使用；
  初始化失败时输出告警并禁用注入（不阻断启动）。
- `types.go` / `version_*.go`：数据类型与平台版本信息。

入口在 `pkg/agent/instance.go` 中构建 `workspacecontext.Projector`，
配置来自 `agents.defaults.workspace_context`；同时把用户目录 `~/.agents`
设为 `AgentsHome`（当 `user_global` 启用时纳入扫描）。

## 运行时注入与触碰跟踪（pkg/agent/workspace_context_runtime.go）

- `recordContextTouch(toolName, args, isError)`：记录一次成功的**文件工具触碰**。
  `fileTouchTools` = `read_file`、`write_file`、`edit_file`、`append_file`
  （不解析 shell 导航，只信任文件系统工具 seam）。路径解析相对工作区，去重记录。
- `consumeContextTouches()`：取走并清空累积触碰。
- `persistRuntimeContextMessage(...)`：把持久化上下文消息写入会话，让
  replay/resume/compaction 都能看到它，和用户消息一样参与上下文。
- 工作区指令注入：当触碰的文件命中候选指令文件时，按需刷新投影内容并注入。

## 在模型上下文中

工作区指令作为独立 context source 注入（区别于 system prompt、runtime
snapshot、task 状态等），生命周期独立：文件改动、触碰记录、轮次结束都会触发
重新计算与持久化。相关设计见
[设计：工作区上下文与自主连续性改进计划](../design/agent-workspace-context-plan.zh.md)（保留的历史提案）。

## 相关文档

- [运行时上下文](runtime-context.zh.md)
- [任务状态与自主](taskstate-autonomy.zh.md)
- [代码地图](code-map.zh.md)
- [Chess 最小配置](../guides/chess-config.zh.md)