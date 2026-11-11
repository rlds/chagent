# Chess 配置指南（唯一配置方式）

> 适用范围：Chagent chess 基础 agent 的全部配置；面向部署与运维。
> 关键代码路径：`pkg/config/config.go`（`LoadConfig`）、`pkg/config/config_loader.go`、
> `pkg/config/agent_name.go`、`pkg/config/minimal.go`、`pkg/config/defaults.go`、
> `pkg/config/context_config.go`。
> 核对日期：2026-10-30

## 唯一配置方式

只有一种配置方式：**`version: 2` 的 JSON 配置文件**（默认 `config.json`）。

- 不支持 v0/v1/v3、YAML 主配置、`.security.yml`、旧绑定、`channel_list`、`api_keys` 复数；
- 不支持逐字段 `CHAGENT_*` 环境变量覆盖（配置文件是唯一来源）；
- 启动与运行**不会重写**配置文件；
- 版本不是 `2` 时启动直接报错。

## pico 通道：不需要外部配置

pico 是内置通道，`config.json` **不需要 `channels` 配置**，也不需要 token。
网关和外部调用方使用同一个约定：

```text
pico token = md5(--name)
```

- `--name` 可省略，省略时按 `default` 计算；
- 例：`--name chess-agent` → `md5("chess-agent")`；
- token 只在内存中使用，不写入 `config.json`；
- 外部 pico 客户端用同一个 md5 值做 `Authorization: Bearer <md5>`；
- 如需限制来源等额外通道行为，可选写 `channels.pico.allow_from` 等，但 token 仍由 md5 决定。

## 最小配置

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

只有 `model_list`（含 `api_key`）是必须外部提供的内容。启动：

```bash
CHAGENT_CONFIG=/path/workspace/config.json \
CHAGENT_WORKSPACE=/path/workspace \
./chagent gateway --name chess-agent
```

## 标准完整配置样例（工作区 / 工具 / Hook）

以下三部分建议显式写清楚；不写时也有默认值，但标准部署应显式指定。

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
  ],

  "agents": {
    "defaults": {
      "workspace": "/path/to/workspace",
      "restrict_to_workspace": true,
      "allow_read_outside_workspace": false,
      "max_tool_iterations": 300,
      "steering_mode": "all",
      "max_parallel_turns": 20,
      "workspace_context": {
        "enabled": true,
        "user_global": false,
        "max_bytes": 32768
      },
      "runtime_context": {
        "mode": "snapshot",
        "refresh_interval_seconds": 600,
        "include_sender": true
      },
      "autonomy": {
        "enabled": true,
        "max_rounds": 20,
        "no_progress_rounds": 3,
        "resume_on_start": false
      }
    }
  },

  "tools": {
    "allow_read_paths": [],
    "allow_write_paths": [],
    "read_file": { "enabled": true, "max_read_file_size": 65536 },
    "write_file": { "enabled": true },
    "edit_file": { "enabled": true },
    "append_file": { "enabled": true },
    "list_dir": { "enabled": true },
    "exec": {
      "enabled": true,
      "enable_deny_patterns": true,
      "allow_remote": true,
      "timeout_seconds": 60
    },
    "task": { "enabled": true },
    "todo": { "enabled": true },
    "skill_load": { "enabled": true },
    "web": { "enabled": false },
    "web_fetch": { "enabled": false },
    "mcp": {
      "servers": {
        "example": {
          "type": "http",
          "url": "https://mcp.example.invalid/mcp",
          "enabled": true,
          "headers": { "Authorization": "Bearer YOUR_MCP_TOKEN" }
        }
      }
    }
  },

  "hooks": {
    "enabled": true,
    "defaults": {
      "observer_timeout_ms": 500,
      "interceptor_timeout_ms": 5000,
      "approval_timeout_ms": 60000
    },
    "builtins": {},
    "processes": {}
  },

  "gateway": {
    "host": "127.0.0.1",
    "port": 0
  }
}
```

### 工作区

- `agents.defaults.workspace` 显式指定 agent 工作区；
- 留空时回退：`CHAGENT_WORKSPACE` → 配置文件目录 → 进程 cwd；
- `restrict_to_workspace=true`、`allow_read_outside_workspace=false` 是安全默认；
- 访问工作区外文件必须显式配置 `tools.allow_read_paths` / `tools.allow_write_paths`。

### 工具

- 工具默认已启用；`tools.<tool>.enabled` 只用于显式关闭/开启非默认工具；
- 配置了 `tools.mcp.servers` 即自动启用 MCP，无需 `tools.mcp.enabled`；
- 默认工具清单与依赖见 [工具参考](../reference/tools.zh.md)。

### Hook

- 不需要 Hook 时保持空 `builtins` / `processes`，不产生任何效果；
- 需要 Hook 时在 `builtins` 或 `processes` 中定义，超时使用 `hooks.defaults`；
- Hook 是可选能力，属于显式外部配置，不是运行必需项。

## 仍需外部提供的配置

| 场景 | 配置 | 必需性 |
|---|---|---|
| 模型 | `model_list`（单数 `api_key`） | 必需 |
| 工作区 | `agents.defaults.workspace` 或 `CHAGENT_WORKSPACE` | 建议显式 |
| MCP 工具 | `tools.mcp.servers`（URL/token/headers） | 仅用 MCP 时 |
| 工作区外路径 | `tools.allow_read_paths` / `tools.allow_write_paths` | 仅越界访问时 |
| Hook | `hooks.builtins` / `hooks.processes` | 仅用 Hook 时 |
| 可选工具开关 | `tools.<tool>.enabled` | 仅改默认时 |

pico token、工具清单、MCP 启用开关、默认路径权限、Hook 超时等**不需要**外部配置。

## 进程发现环境变量（仅三个）

| 变量 | 作用 |
|---|---|
| `CHAGENT_CONFIG` | 配置文件路径 |
| `CHAGENT_WORKSPACE` | 工作区（pid 文件落盘目录） |
| `CHAGENT_HOME` | 安装主目录 |

## 启动参数

唯一参数：`--name`（可省略）。它只用于 pico token 的 md5 约定，不改变其他行为。
