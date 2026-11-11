# ⚙️ 配置指南

> 适用范围：Chagent（chess 基础 agent）的运行时配置。
> 核对日期：2026-10-30
> 完整的最小配置与标准样例见 [Chess 配置指南](chess-config.zh.md)；本页补充环境变量、
> 默认值、工作区布局与校验规则。

---

## 1. 配置文件位置

只有一种配置方式：**`version: 2` 的 JSON 配置文件**（默认 `config.json`）。

| 变量 | 描述 | 默认值 |
|---|---|---|
| `CHAGENT_CONFIG` | 配置文件的完整路径 | `$CHAGENT_HOME/config.json` |
| `CHAGENT_HOME` | Chagent 数据根目录 | `~/.chagent` |
| `CHAGENT_WORKSPACE` | 覆盖工作区路径（优先于配置里的 `agents.defaults.workspace`） | 配置文件所在目录 / 当前目录 |

```bash
# 使用显式配置与工作区启动 gateway
CHAGENT_CONFIG=/srv/chess/config.json \
CHAGENT_WORKSPACE=/srv/chess \
./chagent gateway --name chess-agent
```

已不再支持的旧方式（启动即报错或忽略）：v0/v1/v3 版本、YAML 主配置、`.security.yml`、
`channel_list`、`api_keys` 复数、`CHAGENT_*` 环境变量别名、逐字段环境变量覆盖。
配置文件是唯一来源，启动与运行都不会重写它。

---

## 2. 必需配置

`model_list`（每个模型必须有单数 `api_key`）是唯一必须外部提供的内容：

```json
{
  "version": 2,
  "model_list": [
    {
      "model_name": "glm",
      "model": "glm-5.3",
      "provider": "zhipu",
      "api_base": "https://example.invalid/v1",
      "api_key": "YOUR_KEY"
    }
  ]
}
```

pico 通道内置：`config.json` 不需要 `channels` 与 token。网关与外部调用方约定
`pico token = md5(--name)`（省略 `--name` 时按 `default` 计算）；token 只在内存中使用，
不写入配置文件。外部 pico 客户端用同一个 md5 值做 `Authorization: Bearer <md5>`。

其余配置节（`agents.defaults`、`tools`、`hooks`、`gateway` 等）全部有内置默认值，
不写即可运行。标准部署建议显式写明工作区/上下文/工具/Hook，见
[Chess 配置指南](chess-config.zh.md)。

---

## 3. 关键默认值

### 3.1 Agent 与循环

| 配置 | 默认 | 说明 |
|---|---|---|
| `agents.defaults.max_tool_iterations` | 300 | 单 turn 工具迭代安全阀（不是正常终止条件） |
| `agents.defaults.max_tokens` | 8192 | 输出预留 |
| `agents.defaults.context_window` | `max_tokens * 4` | 未显式配置时按输出预留 4 倍估算 |
| `agents.defaults.steering_mode` | `one-at-a-time` | 运行中插入用户消息的策略 |
| `agents.defaults.max_parallel_turns` | 0（串行） | 并发 turn 上限 |
| `agents.defaults.max_llm_retries` | 2 | 同路由瞬时错误重试次数 |
| `agents.defaults.llm_retry_backoff_secs` | 2 | 重试退避基数（秒，线性） |

### 3.2 上下文（workspace / runtime / autonomy）

| 配置 | 默认 | 说明 |
|---|---|---|
| `workspace_context.enabled` | true | 扫描 CLAUDE.md 与子目录 AGENTS.md 作为持久上下文（根 AGENTS.md 例外，见下） |
| `workspace_context.max_bytes` | 32768 | 每链预算；超出截断并附中文 notice |
| `workspace_context.user_global` | false | chess 只扫描工作区，避免多 agent 串扰 |
| `runtime_context.mode` | `snapshot` | 运行时事实作为变化才追加的 context 消息（`system` 为旧回退） |
| `runtime_context.refresh_interval_seconds` | 600 | 时间等易变事实的刷新粒度 |
| `autonomy.enabled` | true | 自主续跑开关 |
| `autonomy.max_rounds` | 20 | 自主回合上限（1–200） |
| `autonomy.no_progress_rounds` | 3 | 连续无进展阻断阈值（1–10） |
| `autonomy.round_delay_ms` | 1000 | 回合间延迟 |
| `autonomy.resume_on_start` | false | 进程启动时不自动恢复 active 任务（需显式 resume） |
| `autonomy.send_intermediate` | false | 中间自主轮不向聊天窗口推送 |
| `tools.skills.catalog_mode` | `context` | 技能目录作为 durable context（`system` 为旧回退；skill_load 关闭时自动回退 system） |

根 `AGENTS.md` 的身份：工作区根目录的 `AGENTS.md`（或新格式 `AGENT.md`）由
`pkg/agent/definition.go` 作为 **agent definition** 解析并进入 system prompt
（frontmatter 可配置 name/tools/model/skills 等）；它同时被
`workspacecontext` 以 `excludePath` 排除，**不会重复作为 workspace-context
消息注入**，避免同一份指令出现两次。`CLAUDE.md`、`AGENTS.local.md` 以及
子目录（如 `packages/app/AGENTS.md`）中的指令仍按持久上下文注入，并在文件
被工具触碰修改后按 delta 更新。

上下文压缩：先按 Turn 边界裁剪，裁剪前先对超大工具输出做确定性裁剪
（8192 字符阈值，保留头 4096 + 尾 1024）；被裁剪的中间内容会落盘到
`<workspace>/.chagent/spill/`（目录 0700、文件 0600、默认保留 30 天），
裁剪标记里带完整输出路径。摘要/压缩提示均为中文。

### 3.3 安全与权限

| 配置 | 默认 | 说明 |
|---|---|---|
| `agents.defaults.restrict_to_workspace` | true | 文件工具默认限制在工作区 |
| `tools.allow_read_paths` | [] | 工作区外读取的必要显式授权（glob） |
| `tools.allow_write_paths` | [] | 工作区外写入的必要显式授权（glob） |
| `tools.exec.enable_deny_patterns` | true | 危险命令模式拦截 |

MCP 工具不需要 `tools.mcp.enabled=true`：只要配置 `tools.mcp.servers` 就自动激活；
显式 `"tools.mcp.enabled": false` 可关闭 MCP；server 地址/鉴权仍是必要的外部配置。
MCP 协议版本、传输与最新协议特性（`structuredContent` 透传、roots、list_changed、
elicitation、tasks）的支持矩阵详见 [MCP 支持矩阵](../reference/mcp-support.zh.md)。
Hook 不配置时不生效，默认超时值内置；
需要扩展时写 `hooks.builtins` / `hooks.processes`。详见 [工具参考](../reference/tools.zh.md)。

---

## 4. 工作区布局

默认工作区为 `~/.chagent/workspace`（或配置的 `agents.defaults.workspace`）：

```text
workspace/
├── sessions/          # 会话 JSONL 历史（唯一历史来源）
├── memory/            # 长期记忆（MEMORY.md 等）
├── state/             # 持久化状态（最近通道等）
├── cron/              # 定时任务数据
├── skills/            # 技能（skills/<name>/SKILL.md）
├── .chagent/spill/   # 大工具输出落盘（自动清理）
├── AGENTS.md          # 工作区指令（也可用 CLAUDE.md / *.local.md）
├── HEARTBEAT.md       # 周期任务提示
└── config.json        # 也可显式放在工作区
```

`AGENTS.md` / `CLAUDE.md` 与技能文件的变更会在下一轮请求时自动生效，无需重启 gateway。

---

## 5. 校验规则（加载即失败）

- `version` 必须为 `2`；
- `model_list[].api_key` 必须是单数字段（出现 `api_keys` 直接报错）；
- 显式配置的数值型 context/autonomy 字段必须是正数：`autonomy.max_rounds`（1–200）、
  `autonomy.no_progress_rounds`（1–10）、`workspace_context.max_bytes`、
  `workspace_context.max_source_bytes`（> 0）；显式写 `0`/负数会在加载期报错，
  不会被静默替换为默认值；省略字段（或写 `null`）才使用默认值；
  `autonomy.round_delay_ms=0` 是合法值（不延迟），负数非法；
- `runtime_context.mode` 只允许 `system` / `snapshot`；
- `tools.skills.catalog_mode` 只允许 `context` / `system`；
- 候选指令文件名不能含 `/`、`\`、`..`。

非法值不会被静默降级为危险行为：加载阶段直接返回错误。

---

## 6. 延伸阅读

- [Chess 配置指南（唯一配置方式与标准样例）](chess-config.zh.md)
- [工具参考](../reference/tools.zh.md)
- [Chess Pico E2E](chess-pico-e2e.zh.md)
- [代码地图](../architecture/code-map.zh.md)
- [工作区上下文](../architecture/workspace-context.zh.md)
- [运行时上下文](../architecture/runtime-context.zh.md)
- [任务状态与自主续跑](../architecture/taskstate-autonomy.zh.md)


## DSH 工具组（tools.dsh）

DSH 原生、但 Chagent 原先缺失的工具已用 Go 重写为独立工具组（默认启用）：

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

工具清单、能力与安全边界见 [DSH 工具组](../reference/dsh-tools.zh.md)；整组默认启用，可整组或按子组关闭。

外部配置界面可能写出按工具名的扁平开关（`tools.glob.enabled`、`tools.job_run.enabled`、
`tools.task_get.enabled`、`tools.skill.enabled` 等）。加载器会接受这些键并归一化到
`tools.dsh.*` / `tools.task` / `tools.todo` / `tools.skill_load`；同一分组内任一扁平开关为
`false` 会关闭该分组，标准嵌套写法仍优先。完整映射见
[工具参考](../reference/tools.zh.md#扁平工具开关兼容)。


## 环境变量前缀（重命名后不兼容旧名称）

Chagent 的环境变量统一使用 **`CHAGENT_`** 前缀（例如 `CHAGENT_HOME`、`CHAGENT_WORKSPACE`、
`CHAGENT_CONFIG`、`CHAGENT_LOG_LEVEL`）。项目由 `picoclaw` 重命名为 `chagent` 时已明确
**不保留旧名称兼容**：`PICOCLAW_*` / `DDMCCLAW_*` 环境变量、旧 `picoclaw` 二进制和 `~/.picoclaw`
home 不再被识别；外部对接（chess 等）需同步改用新名称（`bin/chagent`、`CHAGENT_*`、`~/.chagent`、
`.chagent.pid`）。
