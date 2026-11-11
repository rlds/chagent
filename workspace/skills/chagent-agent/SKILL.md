---
name: chagent-agent
description: "配置、扩展、调试或参与 Chagent 自身的开发。当任务涉及 Chagent CLI 命令、config.json、gateway、认证、模型、技能、MCP 服务器、cron、路由、会话、自我演化、内置斜杠命令或仓库内部实现时使用。请使用 Chagent 原生的流程、术语、路径和配置。"
metadata: {"nanobot":{"emoji":"🦞"}}
---

# Chagent Agent 技能

Chagent 是一个轻量级个人 AI 助手与智能体框架，具备原生 CLI、聊天网关、MCP 集成、可安装技能、会话路由、定时任务和自我演化能力。

当任务与 **Chagent 本身**相关时使用本技能：上手入门、配置、调试、新增功能、扩展 CLI、修改路由/会话行为、处理技能或 MCP 支持，或为这个仓库贡献代码。

## 工作立场

当本技能启用时，请完全服从 Chagent 的真实架构：

- 优先使用 Chagent 的命令、配置键、工作区布局和文档。
- 在行为、命名和工作流程上遵循 Chagent 的源码与文档。
- 把仓库代码和已提交的文档视为唯一事实来源。

## 快速开始

```bash
# 初始化 ~/.chagent/config.json 和 ~/.chagent/workspace
chagent onboard

# 为某个提供商完成认证
chagent auth login --provider openai

# 查看或切换默认模型
chagent model
chagent model my-default-model

# 一次性提问
chagent agent -m "Hello"

# 交互式 CLI 聊天
chagent agent

# 启动用于聊天渠道的网关
chagent gateway

# 检查运行时/配置健康状况
chagent status

# 浏览已安装的技能和 MCP 服务器
chagent skills list
chagent mcp list
```

## 命令概览


### 核心 CLI

```bash
chagent onboard
chagent agent [-m MESSAGE] [--session KEY] [--model MODEL] [--debug]
chagent gateway [--debug] [--no-truncate] [--allow-empty] [--host HOST]
chagent status
chagent version
chagent migrate
```

### 配置与模型

```bash
# 显示或修改默认配置的模型别名
chagent model
chagent model <model_name>

# 从兼容 OpenAI 的端点添加模型
chagent model add --api-base URL --api-key KEY
chagent model add -b http://localhost:8000/v1 -k dummy -m my-model -n local

# 将配置重置为出厂默认值（保留敏感密钥）
chagent config reset
chagent config reset --force
```

注意：

- **没有**通用的 `chagent config edit` 或 `chagent config set` 命令。
- 高级修改通常直接编辑 `~/.chagent/config.json`。

### 认证

```bash
chagent auth login --provider openai
chagent auth login --provider anthropic --setup-token
chagent auth login --provider antigravity --device-code
chagent auth models
chagent auth status
chagent auth logout --provider openai
chagent auth weixin
chagent auth wecom --timeout 10m
```

### 技能

```bash
chagent skills list
chagent skills show <name>
chagent skills search "query"
chagent skills install owner/repo/path
chagent skills install --registry clawhub <slug>
chagent skills remove <name>
chagent skills list-builtin
chagent skills install-builtin
```

技能加载优先级为：

1. `~/.chagent/workspace/skills`
2. `~/.chagent/skills`
3. 内置的嵌入式技能

### MCP

```bash
chagent mcp add filesystem -- npx -y @modelcontextprotocol/server-filesystem /tmp
chagent mcp add --deferred github --env-file .env.github -- npx -y @modelcontextprotocol/server-github
chagent mcp list
chagent mcp list --status
chagent mcp show github
chagent mcp test github
chagent mcp edit
chagent mcp remove github
```

注意：

- `chagent mcp` 是 `tools.mcp.servers` 的**配置管理器**。
- 它**不会**自行保持服务器运行；后续由 host/gateway 负责加载。

### Cron

```bash
chagent cron list
chagent cron add --name "Daily summary" --message "Summarize today's logs" --cron "0 18 * * *"
chagent cron add --name "Ping" --message "heartbeat" --every 300 --deliver
chagent cron enable <job-id>
chagent cron disable <job-id>
chagent cron remove <job-id>
```

注意：

- 当前 CLI 的 `chagent cron add` 只支持周期性任务：`--every` 或 `--cron`。
- 一次性 `at_seconds` 任务在 cron 系统中存在，但目前的 CLI 还没有把它作为一等参数。

## 提供商

Chagent 通过 `model_list` 支持 30 多家 LLM 提供商。

Chagent 中的凭证配置方式有：

- `model_list[].api_keys` 用于大多数托管 API
- `chagent auth login --provider ...` 用于内置的认证辅助流程（`openai`、`anthropic`、`antigravity`）
- 本地或自托管端点，用于 `ollama`、`lmstudio`、`vllm` 和 `litellm` 这类提供商
- 外部平台凭证，用于 `bedrock`、`azure` 和 `github-copilot` 这类提供商

`chagent auth login` **不**覆盖所有提供商。对大多数提供商而言，常规做法是新增一条包含 `provider`、`model` 和 `api_keys` 的 `model_list` 条目。

### 常见提供商对照表

| 提供商 | `provider` 取值 | Chagent 中的认证方式 |
| --- | --- | --- |
| OpenAI | `openai` | 通过 `chagent auth login --provider openai` 使用 OAuth 辅助流程，或配置 `model_list[].api_keys` |
| Anthropic | `anthropic` | 在 `model_list[].api_keys` 中配置 API key，或通过 `chagent auth login --provider anthropic` 的辅助流程 |
| Anthropic Messages API | `anthropic-messages` | 在 `model_list[].api_keys` 中配置 API key |
| Google Gemini | `gemini` | 在 `model_list[].api_keys` 中配置 API key |
| OpenRouter | `openrouter` | 在 `model_list[].api_keys` 中配置 API key |
| Zhipu / GLM | `zhipu` | 在 `model_list[].api_keys` 中配置 API key |
| DeepSeek | `deepseek` | 在 `model_list[].api_keys` 中配置 API key |
| VolcEngine / Doubao | `volcengine` | 在 `model_list[].api_keys` 中配置 API key |
| Qwen / DashScope | `qwen` | 在 `model_list[].api_keys` 中配置 API key |
| Moonshot / Kimi | `moonshot` | 在 `model_list[].api_keys` 中配置 API key |
| MiniMax | `minimax` | 在 `model_list[].api_keys` 中配置 API key |
| Mistral | `mistral` | 在 `model_list[].api_keys` 中配置 API key |
| Groq | `groq` | 在 `model_list[].api_keys` 中配置 API key |
| NVIDIA NIM | `nvidia` | 在 `model_list[].api_keys` 中配置 API key |
| Cerebras | `cerebras` | 在 `model_list[].api_keys` 中配置 API key |
| Azure OpenAI | `azure` | 在 `model_list` 中配置 `api_key`；若构建时包含 `azidentity` 支持，则也可使用 Microsoft Entra ID |
| AWS Bedrock | `bedrock` | AWS 凭证，并需构建时启用 Bedrock（`go build -tags bedrock`） |
| Antigravity | `antigravity` | 通过 `chagent auth login --provider antigravity` 使用 OAuth 辅助流程 |
| GitHub Copilot | `github-copilot` | 外部 Copilot gRPC 端点，默认 `localhost:4321` |
| Ollama | `ollama` | 本地端点，无需 API key |
| LM Studio | `lmstudio` | 本地端点，API key 可选 |
| vLLM | `vllm` | 本地兼容 OpenAI 的端点 |
| LiteLLM | `litellm` | 代理端点，凭证取决于代理所期望的模型 |
| Claude CLI | `claude-cli` | 本地 Claude CLI 提供商，由工作区/运行时配置 |
| Codex CLI | `codex-cli` | 本地 Codex CLI 提供商，由工作区/运行时配置 |

### 其他兼容 OpenAI 的厂商

Chagent 还为以下厂商提供一等元数据或路由支持：

- `venice`
- `vivgrid`
- `longcat`
- `modelscope`
- `mimo`
- `novita`
- `byteplus`
- `shengsuanyun`
- `avian`
- `zai-coding`
- `gpt4free`

完整提供商矩阵、默认 API 基础地址、协议族以及各厂商示例，请阅读 `docs/guides/providers.md`。

## 内置工具族

Chagent 的工具在 `config.json` 的 `tools` 下配置，并在运行时动态注册。

重要的启用规则：

- 大多数工具通过 `tools.*` 单独启用或禁用
- 智能体级别的允许列表可进一步限制工具的可见性
- `turn_profile.tools` 可为某次请求或某个智能体收窄工具暴露范围
- 延迟加载的 MCP 工具在被工具发现解锁之前保持隐藏

### 运行时工具族

| 工具族 | 运行时工具名 | 提供的功能 |
| --- | --- | --- |
| 文件系统 | `read_file`, `write_file`, `list_dir`, `edit_file`, `append_file` | 读取、写入、列出和修改工作区文件 |
| Web | `web_search`, `web_fetch` | 搜索网页并抓取可读的页面内容 |
| 命令执行 | `exec` | 在拒绝规则护栏下执行 shell 命令 |
| 调度 | `cron` | 定时任务、提醒、周期任务和命令任务 |
| 技能注册表 | `find_skills`, `install_skill` | 从已配置的注册表中搜索并安装技能 |
| MCP | `mcp_<server>_<tool>` | 由已连接的 MCP 服务器提供的工具 |
| MCP 发现 | `tool_search_tool_bm25`, `tool_search_tool_regex` | 按需发现延迟隐藏的 MCP 工具 |
| 硬件 | `i2c`, `spi`, `serial` | 对受支持设备和板卡的硬件访问 |
| 消息 | `message`, `reaction` | 通过渠道集成发送外发消息和表情回应 |
| 媒体 | `send_file`, `load_image`, `send_tts` | 发送文件、将本地图片载入上下文、生成 TTS 输出 |
| 子智能体 | `spawn`, `subagent`, `spawn_status`, `delegate` | 后台任务、同步子轮次、任务状态、多智能体委派 |

### 工具注册说明

- 仅在 TTS 提供商可用时才注册 `send_tts`。
- `spawn` 和 `spawn_status` 需要启用 `subagent` 支持。
- 仅当存在多个智能体时才会自动注册 `delegate`。
- MCP 发现工具仅在启用延迟 MCP 发现时才有意义。
- `message` 既可配置为发送外发媒体，也可发送纯文本。

各工具的配置请阅读 `docs/reference/tools_configuration.md`。

## 专用子智能体与 spawn

Chagent 具有一等的子智能体模型，用于长时间运行的工作、隔离的子问题和多智能体专业化分工。

核心理念是：

- 用 `spawn` 处理应继续运行且不阻塞当前轮次的后台工作
- 当父级需要立即拿到结果时，用 `subagent` 处理隔离的同步子任务
- 用 `delegate` 将任务交给具备自身身份、模型、工作区和工具的特定对等智能体
- 用 `spawn_status` 或 `/subagents` 查看当前正在运行的内容

### 选择合适的子智能体工具

| 工具 | 执行方式 | 最佳用途 |
| --- | --- | --- |
| `spawn` | 异步后台任务 | 网络调研、API 轮询、长时间扫描，以及可稍后回报的工作 |
| `subagent` | 同步隔离子轮次 | 必须在父级继续之前返回的聚焦分析、转换或校验 |
| `delegate` | 同步交接给具名对等智能体 | 应以专用智能体而非通用子轮次身份运行的工作 |
| `spawn_status` | 仅查看/状态 | 检查运行中、已完成、失败或已取消的生成任务 |

### 专用对等智能体

专用子智能体通过多智能体系统配置，而不仅靠临时提示词。

两个重要层次是：

- `config.json` 通过 `subagents.allow_agents` 定义存在哪些对等智能体，以及某个智能体被允许生成哪些对等智能体
- 每个智能体的 `AGENT.md` 定义使其值得被生成的身份：`name`、`description`、工具、技能、MCP 服务器以及可选的模型覆盖

最小结构：

```json
{
  "agents": {
    "list": [
      {
        "id": "main",
        "default": true,
        "subagents": {
          "allow_agents": ["research"]
        }
      },
      {
        "id": "research"
      }
    ]
  }
}
```

专家的 `AGENT.md` 示例：

```md
---
name: Research Agent
description: Specialist for deep web research, evidence gathering, and synthesis.
tools: [web_search, web_fetch, message]
skills: [deep-research]
---
```

### 智能体自动发现

当某个智能体拥有 `spawn` 工具且至少有一个被允许的对等智能体时，Chagent 会自动向系统提示词中注入一份轻量的智能体注册表。

这意味着：

- 模型无需调用单独的 `list_agents` 工具即可看到符合条件的对等智能体
- 只展示可生成的对等智能体
- 当前智能体被省略
- 发现过程使用对等智能体稳定的 `id`、`name` 和 `description`

实际上，正是这一点让定向的 `spawn(..., agent_id="research")` 或 `delegate(agent_id="research", ...)` 变得可靠。

### 运行行为

需要注意的子智能体执行语义：

- 子智能体在隔离的临时会话历史中运行，因此它们的推理过程和中间步骤不会污染父会话
- `spawn` 立即返回，并在 goroutine 中启动后台工作
- `subagent` 等待完成并直接返回结果
- `delegate` 是同步的，并以目标对等智能体的身份运行，而不是作为通用子任务
- 当存在渠道/聊天上下文时，`spawn_status` 的作用域限定为当前会话
- 所有子智能体仍共享同一工作区安全边界；它们不会绕过沙箱或路径限制

运行时限制与生命周期规则：

- 嵌套子轮次的深度限制为 3
- 并发数限制为每个父轮次 5 个子轮次
- 等待并发槽位 30 秒后超时
- 生成的后台任务会被标记为 critical，以便在父级优雅完成时仍能存活
- 强制中止仍会级联到子轮次和孙轮次

### 实用模式

在以下情况使用 `spawn`：

- 任务会持续较长时间
- 结果可以稍后送达
- 当前轮次应继续推进

在以下情况使用 `subagent`：

- 你需要与父上下文隔离
- 你希望对一个有边界的子问题进行独立尝试
- 父级必须在规划下一步之前等待答案

在以下情况使用 `delegate`：

- 某个具名对等智能体明显更适合该任务
- 该对等智能体拥有更窄的工具/技能/模型配置
- 你希望任务以对等智能体自身的工作区/运行时身份运行

### 示例

后台的专业调研：

```text
spawn(
  task="Search the web for the latest Chagent MCP integration patterns and summarize them.",
  label="mcp-research",
  agent_id="research"
)
```

同步的隔离检查：

```text
subagent(
  task="Review this config for risky tool exposure and return only the concrete findings."
)
```

同步交接给具名对等智能体：

```text
delegate(
  agent_id="research",
  task="Collect three primary-source references for current provider authentication behavior."
)
```

### 可观测性

实时可见性方面：

- 调用 `spawn_status` 可查看单个任务，或列出当前会话中可见的任务
- 在聊天渠道中使用 `/subagents` 可展示当前会话的活动子智能体树

## 语音、转写与 TTS

Chagent 可以转写入站音频并合成出站语音，但语音配置与其他运行时部分一样，都是模型驱动的。

重要模式是：

- ASR 使用 `voice.model_name`
- TTS 使用 `voice.tts_model_name`
- 两者都通过 `model_list` 中的具名条目解析
- 密钥应保存在 `.security.yml` 中，而不是内联在 `voice` 里

### STT（语音 -> 文本）

来自受支持渠道的语音和音频消息可以在智能体层面自动转写。

推荐的配置方式：

1. 向 `model_list` 添加一条具备 ASR 能力的模型条目
2. 将 `voice.model_name` 设为该条目的 `model_name`
3. 将匹配的 API key 存入 `.security.yml`
4. 如果希望转写结果回显到聊天中，可选设置 `voice.echo_transcription`

示例：

```json
{
  "model_list": [
    {
      "model_name": "voice-groq",
      "model": "groq/whisper-large-v3-turbo"
    }
  ],
  "voice": {
    "model_name": "voice-groq",
    "echo_transcription": true
  }
}
```

```yaml
model_list:
  voice-groq:
    api_keys:
      - "gsk_your_groq_key"
```

### 常见 ASR 路由

| 路由 | 示例模型 | 说明 |
| --- | --- | --- |
| Groq Whisper | `groq/whisper-large-v3-turbo` | 快速的兼容 OpenAI 的 Whisper 转写，是常见的默认选择 |
| OpenAI Whisper | `openai/whisper-1` | 通过兼容 OpenAI 的音频端点进行标准 Whisper 转写 |
| ElevenLabs Scribe | `provider: elevenlabs`, `model: scribe_v1` | 使用 Chagent 专用的 ElevenLabs 转写路径 |
| 支持音频的聊天模型 | `gemini/gemini-2.5-flash`, `openai/gpt-4o-audio-preview` | 多模态音频转写路径；部分模型组合仍在演进中 |

需要注意的检测行为：

- `voice.model_name` 是首选且推荐的方式
- 如果它解析到 ElevenLabs 模型，Chagent 使用 ElevenLabs 转写器
- 如果它解析到兼容 Whisper 的模型，Chagent 使用 Whisper 转写路径
- 如果它解析到支持音频的多模态模型，Chagent 可使用音频模型转写
- 如果省略 `voice.model_name`，Chagent 仍会扫描 `model_list`，以兼容旧版自动检测的 ASR 条目

### TTS（文本 -> 语音）

出站语音由 `voice.tts_model_name` 驱动，并在提供商可用时通过 `send_tts` 暴露。

推荐的配置方式：

1. 向 `model_list` 添加一条具备 TTS 能力的模型条目
2. 将 `voice.tts_model_name` 设为该条目的 `model_name`
3. 将 API key 存入 `.security.yml`
4. 如果提供商需要特定于模型的 TTS 字段，将它们加到 `model_list[].extra_body` 下
5. 如果希望智能体输出语音文件，在工具配置中启用 `send_tts`

示例：

```json
{
  "model_list": [
    {
      "model_name": "openai-tts",
      "model": "openai/tts-1"
    }
  ],
  "voice": {
    "tts_model_name": "openai-tts"
  }
}
```

```yaml
model_list:
  openai-tts:
    api_keys:
      - "sk-openai-your-key"
```

使用 OpenRouter MAI Voice 2 的示例：

```json
{
  "model_list": [
    {
      "model_name": "mai-voice-2",
      "provider": "openrouter",
      "model": "microsoft/mai-voice-2",
      "api_base": "https://openrouter.ai/api/v1",
      "extra_body": {
        "voice": "en-US-Harper:MAI-Voice-2",
        "response_format": "mp3"
      }
    }
  ],
  "voice": {
    "tts_model_name": "mai-voice-2"
  }
}
```

```yaml
model_list:
  mai-voice-2:
    api_keys:
      - "sk-or-your-openrouter-key"
```

### 当前 TTS 提供商路径

| 提供商路径 | 示例模型 | 说明 |
| --- | --- | --- |
| 兼容 OpenAI 的语音合成 | `openai/tts-1` | 支持最好的路径；Chagent 发送 OpenAI 风格的 `/audio/speech` 请求 |
| 小米 MiMo | `mimo/mimo-v2-tts` | 小米 MiMo 专用 TTS 提供商路径，输出 MP3 |

运行说明：

- 首选的选取方式是 `voice.tts_model_name`
- 如果该项缺失，Chagent 仍可扫描 `model_list`，找到第一个 ID 含 `tts` 且具备 API 后端的模型
- 当前 OpenAI 风格的 TTS 请求默认使用 `voice: alloy` 和 `response_format: opus`
- 可通过 `model_list[].extra_body` 为特定 TTS 模型覆盖 `voice` 和 `response_format`
- 如果提供商拒绝 `response_format`，Chagent 会去掉该字段重试一次
- 只有在 TTS 检测成功时才会注册 `send_tts`


## 会话内斜杠命令

Chagent 共享的斜杠命令注册表位于 `pkg/commands`。

在帮助聊天渠道中的用户时使用这些命令：

```text
/start
/help
/show model
/show channel
/show agents
/show mcp <server>
/list models
/list channels
/list agents
/list skills
/list mcp
/subagents
/use <skill> [message]
/use clear
/btw <question>
```

需要注意的语义：

- `/use <skill> <message>` 为单次请求强制指定一个已安装技能。
- `/use <skill>` 为同一聊天会话的**下一条**消息预置该技能。
- `/use clear` 取消待生效的技能覆盖。
- `/btw <question>` 提出一个隔离的旁问，不会改动主会话历史。
- `/subagents` 展示当前会话的活动子智能体树。
- 未知的斜杠命令会转交正常的 LLM 处理，而不是直接失败。

Telegram 会自动注册受支持的顶层命令，如 `/start`、`/help`、`/show`、`/list`、`/use` 和 `/btw`。

## 关键路径与环境

### 重要文件

```text
~/.chagent/config.json         主配置
~/.chagent/.security.yml       存储在 config.json 之外的敏感值
~/.chagent/auth.json           OAuth/令牌存储
~/.chagent/workspace/          默认工作区
~/.chagent/workspace/skills/   工作区技能
~/.chagent/workspace/sessions/ 会话历史
~/.chagent/workspace/cron/     定时任务存储
```

默认工作区布局：

```text
~/.chagent/workspace/
├── sessions/
├── memory/
├── state/
├── cron/
├── skills/
├── AGENT.md
├── HEARTBEAT.md
├── IDENTITY.md
├── SOUL.md
└── USER.md
```

### 重要环境变量

```bash
CHAGENT_CONFIG=/path/to/config.json
CHAGENT_HOME=/path/to/chagent-home
CHAGENT_BUILTIN_SKILLS=/path/to/custom-builtin-skills
CHAGENT_LOG_LEVEL=debug
CHAGENT_GATEWAY_HOST=0.0.0.0
```

当用户反馈“配置文件不对”时，使用 `CHAGENT_CONFIG`。
当用户想要便携式或由服务托管的安装时，使用 `CHAGENT_HOME`。

## Chagent 原生概念

### 模型配置

Chagent 以模型为中心。关键字段是：

- `agents.defaults.model_name`
- `model_list`
- 可选的 `provider`
- 运行时的 `model`

重要行为：

- `agents.defaults.model_name` 必须匹配 `model_list` 中的某个 `model_name` 条目。
- 如果设置了 `provider`，Chagent 会原样把 `model` 发送给该提供商。
- 如果省略 `provider`，仍支持旧版的 `provider/model` 解析方式。

### 会话与路由

会话行为主要通过以下配置设定：

- `session.dimensions`
- `session.identity_links`
- `agents.dispatch.rules[*].session_dimensions`

可用的维度：

- `space`
- `chat`
- `topic`
- `sender`

基础的隔离始终包括：

- 智能体
- 渠道
- 账号

因此即使 `session.dimensions` 列表很短，也**不会**在所有平台上创建一整块全局记忆。

### 技能

技能就是包含 `SKILL.md` 的普通目录。加载器要求：

- 合法的小写或连字符技能名
- 非空的描述
- 技能目录中存在 `SKILL.md` 文件

推荐使用这种格式：

```text
workspace/skills/<skill-name>/SKILL.md
```

Chagent 加载和匹配时仅依赖 `name` 与 `description` 两个 frontmatter 字段。

### MCP 发现

Chagent 支持常驻加载和延迟加载的 MCP 工具。

使用方式：

- 当工具应在被显式发现前保持隐藏时，使用 `--deferred`
- 用 `chagent mcp show <name>` 查看活动的工具
- 调试运行中的智能体时，在聊天渠道使用 `/list mcp` 和 `/show mcp <server>`

### 自我演化

`evolution` 配置块控制自我演化：

- `enabled`
- `mode`: `observe`, `draft`, `apply`
- `state_dir`
- `min_task_count`
- `min_success_ratio`
- `cold_path_trigger`
- `cold_path_times`

先使用 `observe`。
当团队希望得到可审查的候选技能变更时，使用 `draft`。
只有在可以接受自动更新工作区技能时，才使用 `apply`。

注意：

- `cold_path_trigger: manual` 目前还没有通用的、面向用户的 CLI/API 触发入口。

## 调试工作流

从最贴近 Chagent 原生方式的做法开始：

1. 检查 `chagent status`。
2. 确认当前生效的是哪个配置文件。
3. 检查 `agents.defaults.model_name` 和 `model_list`。
4. 运行 `chagent gateway --debug` 以观察运行时情况。
5. 只有在需要查看完整提示词或工具负载时，才加上 `--no-truncate`。
6. 遇到技能问题，检查技能目录和 frontmatter。
7. 遇到 MCP 问题，使用 `chagent mcp list`、`show` 和 `test`。
8. 遇到路由/会话问题，检查 `session.dimensions` 和 `agents.dispatch.rules`。

有用的运行时事实：

- `--no-truncate` 只能与 `--debug` 一起使用。
- 网关的健康检查端点暴露 `/health`、`/ready` 和 `/reload`。
- `tool_feedback` 可以把可见的工具执行通知直接发布到聊天中。

### 日志位置

网关和运行时调试时，Chagent 会把日志写入其主目录下：

```text
~/.chagent/logs/gateway.log
~/.chagent/logs/gateway_panic.log
```

如果覆盖了 `CHAGENT_HOME`，请使用：

```text
$CHAGENT_HOME/logs/gateway.log
$CHAGENT_HOME/logs/gateway_panic.log
```

实际上，请优先检查这些位置：

- `~/.chagent/logs/` 中的持久化网关日志
- 运行 `chagent gateway` 或 `chagent agent` 的终端
- 通过 `docker compose -f docker/docker-compose.yml logs -f` 查看 Docker 的 stdout/stderr
- 如果 Chagent 由其他 supervisor 运行，则查看启动器或服务日志

常用控制手段：

- `chagent gateway --debug` 获取详细的运行时日志
- `chagent gateway --debug --no-truncate` 查看完整的提示词/工具负载
- 通过 `gateway.log_level` 或 `CHAGENT_LOG_LEVEL` 将日志详细程度提高到 `debug` 或 `info`

特殊情况：

- 独立的 `chagent agent` 命令默认使用控制台日志，除非显式设置了 `CHAGENT_LOG_FILE`
- 设置 `CHAGENT_HOOK_LOG_FILE` 后，流程钩子可写入 JSONL 文件日志；这仅针对钩子，与主网关日志路径相互独立

## 仓库地图

贡献代码时，这些路径最重要：

- `cmd/chagent/main.go` — 根 CLI 装配
- `cmd/chagent/internal/agent/` — 直接的 CLI 智能体命令
- `cmd/chagent/internal/gateway/` — 网关启动与命令行参数
- `cmd/chagent/internal/auth/` — 认证流程、二维码引导
- `cmd/chagent/internal/model/` — 默认模型切换与 `model add`
- `cmd/chagent/internal/skills/` — install/list/show/remove/search 命令
- `cmd/chagent/internal/mcp/` — MCP CLI 配置管理器
- `cmd/chagent/internal/cron/` — cron CLI
- `pkg/commands/` — 共享的斜杠命令注册表
- `pkg/agent/` — 提示词组装、会话、路由、工具执行、钩子
- `pkg/skills/` — 技能加载、元数据、注册表安装
- `pkg/mcp/` — MCP 运行时集成
- `pkg/config/` — 配置模式、默认值、迁移、持久化
- `docs/guides/configuration.md` — 面向用户的配置参考
- `docs/guides/session-guide.md` — 会话行为实践
- `docs/reference/mcp-cli.md` — 权威的 MCP CLI 行为说明
- `docs/reference/cron.md` — cron 行为与限制
- `docs/operations/debug.md` — 调试工作流
- `docs/operations/troubleshooting.md` — 已知的错误配置模式
- `docs/architecture/agent-self-evolution.md` — 演化设计与安全性

## 贡献规则

修改 Chagent 时：

- 优先扩展已有的 CLI 分组和共享注册表，而不是新增一次性的平行流程。
- 让文档在 CLI 参数、斜杠命令和配置行为上与代码保持一致。
- 如果新增或修改斜杠命令，检查 `pkg/commands` 以及提到命令可用性的聊天渠道文档。
- 如果改动技能行为，验证加载顺序、命名规则和 frontmatter 假设。
- 如果改动路由或会话逻辑，请一并复核 `docs/guides/session-guide.md` 和架构文档，确保行为与文档一致。
- 如果改动 MCP，请记住 CLI 管理配置，而运行时宿主管理执行。

## 常见故障排查

### “model ... not found in model_list”

请检查：

- `agents.defaults.model_name` 与某个已配置的 `model_name` 匹配
- 目标 `model_list` 条目已启用
- `provider` 和 `model` 字段遵循 Chagent 以模型为中心的规则

### OpenRouter `free is not a valid model ID`

优先使用显式的提供商配置：

```json
{
  "provider": "openrouter",
  "model": "free"
}
```

而不是：

```json
{
  "model": "free"
}
```

### 技能不显示

请检查：

- 目录名是合法的技能名
- `SKILL.md` 存在
- frontmatter 的 `name` 和 `description` 存在且合理
- 技能位于工作区、全局或内置根目录下

### MCP 服务器存在但工具不显示

请检查：

- `tools.mcp.enabled` 为 true
- 该服务器已启用
- 延迟发现设置符合预期
- `chagent mcp test <name>` 执行成功
- `/show mcp <server>` 或 `chagent mcp show <name>` 能列出工具

### 智能体记得太多或太少

请检查：

- `session.dimensions`
- 各规则中的 `session_dimensions`
- 问题到底是会话隔离，还是摘要压缩

### 配置修改似乎未生效

请检查：

- `CHAGENT_CONFIG`
- `CHAGENT_HOME`
- 用户改的是 `config.json` 还是 `.security.yml`
- 测试的是 CLI 路径、网关路径，还是两者

## 后续需加载的文档

仅在任务需要时阅读以下文档：

- `docs/guides/configuration.md` 用于配置、路由、技能、轮次配置档和演化
- `docs/guides/session-guide.md` 用于会话隔离实践
- `docs/reference/tools_configuration.md` 用于工具专属配置
- `docs/reference/mcp-cli.md` 用于 MCP CLI 参数与存储行为
- `docs/reference/cron.md` 用于调度类型与安全校验
- `docs/operations/debug.md` 用于运行时检查
- `docs/operations/troubleshooting.md` 用于常见的提供商/模型错误

如果任务偏向代码层面而非面向用户，请先阅读 `cmd/chagent/internal/`、`pkg/commands/`、`pkg/agent/`、`pkg/skills/` 或 `pkg/mcp/` 下对应的包，再提出行为变更。