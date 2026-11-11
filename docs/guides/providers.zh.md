# 🔌 提供商与模型配置

> 返回 [README](../README.md)

Chagent 只保留**两类通用协议**，不再内置任何厂商专属协议、SDK、CLI 或 OAuth 适配器。所有模型都通过下面两类 HTTP 协议接入：

| 协议族                   | `provider` 值                                    | 说明                                                                                          |
| ------------------------ | ------------------------------------------------ | --------------------------------------------------------------------------------------------- |
| OpenAI Chat Completions 兼容 | `openai` 及下方预设别名                      | 任何兼容 OpenAI Chat Completions 的端点。自建/未列出的服务用 `provider: openai` + `api_base` 接入。 |
| Anthropic Messages 兼容  | `anthropic` / `anthropic-messages` / `anthropic-bearer` | 原生 Anthropic Messages HTTP 协议；`anthropic` 是 `anthropic-messages` 的别名，`anthropic-bearer` 为 Bearer Token 认证变体。 |

> **迁移提示**：`gemini`、`azure`、`bedrock`、`github-copilot`、`antigravity`、`claude-cli`、`codex-cli` 等厂商专属协议已移除；`auth_method` / `connect_mode` 字段也已删除。若上游提供 OpenAI 兼容端点，改用 `provider: openai` + `api_base`，或直接使用下方预设别名。

### 协议一：OpenAI Chat Completions 兼容

- 认证：`Authorization: Bearer <api_key>`（本地服务通常可省略 key）。
- 关键配置：`api_base` 指向 `<host>/v1` 这类 OpenAI 兼容根路径；`model` 填上游原生模型 ID。
- 适用于：OpenAI 及其所有兼容网关、聚合服务、自建推理服务（Ollama / vLLM / LM Studio / LiteLLM 等）。

### 协议二：Anthropic Messages 兼容

- 认证：`x-api-key`（`anthropic` / `anthropic-messages`）或 `Authorization: Bearer`（`anthropic-bearer`）。
- 默认 API Base：`https://api.anthropic.com/v1`。
- 适用于：Anthropic 官方 API 及兼容 Messages 协议的服务。

## OpenAI 兼容预设

下表预设共用同一套 OpenAI 兼容实现，只是默认 `api_base` 与展示信息不同；设置 `provider` 为预设名即可。

| 预设                | `provider` 值     | 默认 API Base                                       | 获取 API Key / 备注                                                     |
| ------------------- | ----------------- | --------------------------------------------------- | ----------------------------------------------------------------------- |
| **OpenAI**          | `openai`          | `https://api.openai.com/v1`                         | [获取密钥](https://platform.openai.com)                                 |
| **OpenRouter**      | `openrouter`      | `https://openrouter.ai/api/v1`                      | [获取密钥](https://openrouter.ai/keys)                                  |
| **DeepSeek**        | `deepseek`        | `https://api.deepseek.com/v1`                       | [获取密钥](https://platform.deepseek.com)                               |
| **智谱 AI (GLM)**   | `zhipu`（`glm`）  | `https://open.bigmodel.cn/api/paas/v4`              | [获取密钥](https://open.bigmodel.cn/usercenter/proj-mgmt/apikeys)       |
| **通义千问 (Qwen)** | `qwen-portal`（`qwen`） | `https://dashscope.aliyuncs.com/compatible-mode/v1` | [获取密钥](https://dashscope.console.aliyun.com)                        |
| **Qwen 国际站**     | `qwen-intl`       | `https://dashscope-intl.aliyuncs.com/compatible-mode/v1` | 阿里云国际站 DashScope                                              |
| **Qwen 美国站**     | `qwen-us`         | `https://dashscope-us.aliyuncs.com/compatible-mode/v1`   | 阿里云美国站 DashScope                                              |
| **Moonshot (Kimi)** | `moonshot`        | `https://api.moonshot.cn/v1`                        | [获取密钥](https://platform.moonshot.cn)                                |
| **Groq**            | `groq`            | `https://api.groq.com/openai/v1`                    | [获取密钥](https://console.groq.com)                                    |
| **Cerebras**        | `cerebras`        | `https://api.cerebras.ai/v1`                        | [获取密钥](https://cerebras.ai)                                         |
| **NVIDIA**          | `nvidia`          | `https://integrate.api.nvidia.com/v1`               | [获取密钥](https://build.nvidia.com)                                    |
| **Mistral AI**      | `mistral`         | `https://api.mistral.ai/v1`                         | [获取密钥](https://console.mistral.ai)                                  |
| **Venice AI**       | `venice`          | `https://api.venice.ai/api/v1`                      | [获取密钥](https://venice.ai)                                           |
| **火山引擎 (Doubao)** | `volcengine`    | `https://ark.cn-beijing.volces.com/api/v3`          | [获取密钥](https://www.volcengine.com/activity/codingplan)              |
| **神算云**          | `shengsuanyun`    | `https://router.shengsuanyun.com/api/v1`            | 控制台获取                                                              |
| **SiliconFlow**     | `siliconflow`     | `https://api.siliconflow.cn/v1`                     | [获取密钥](https://siliconflow.cn)                                      |
| **Vivgrid**         | `vivgrid`         | `https://api.vivgrid.com/v1`                        | [获取密钥](https://vivgrid.com)                                         |
| **LongCat**         | `longcat`         | `https://api.longcat.chat/openai`                   | [获取密钥](https://longcat.chat/platform)                               |
| **ModelScope (魔搭)** | `modelscope`    | `https://api-inference.modelscope.cn/v1`            | [获取 Token](https://modelscope.cn/my/tokens)                           |
| **小米 MiMo**       | `mimo`            | `https://api.xiaomimimo.com/v1`                     | [获取密钥](https://platform.xiaomimimo.com)                              |
| **MiniMax**         | `minimax`         | `https://api.minimaxi.com/v1`                       | [获取密钥](https://platform.minimaxi.com)                                |
| **Z.ai**            | `zai`             | `https://api.z.ai/api/coding/paas/v4`               | [获取密钥](https://z.ai)                                                |
| **Avian**           | `avian`           | `https://api.avian.io/v1`                           | [获取密钥](https://avian.io)                                            |
| **NEAR AI Cloud**   | `nearai`          | `https://cloud-api.near.ai/v1`                      | [获取密钥](https://near.ai)                                             |
| **Novita AI**       | `novita`          | `https://api.novita.ai/openai`                      | [获取密钥](https://novita.ai)                                           |
| **Alibaba Coding Plan** | `alibaba-coding` | `https://coding-intl.dashscope.aliyuncs.com/v1`  | 阿里云 Coding Plan 控制台                                               |
| **Ollama (本地)**   | `ollama`          | `http://localhost:11434/v1`                         | 本地，无需密钥                                                          |
| **LM Studio (本地)** | `lmstudio`       | `http://localhost:1234/v1`                          | 本地，可选密钥                                                          |
| **vLLM (本地)**     | `vllm`            | `http://localhost:8000/v1`                          | 本地，无需密钥                                                          |
| **LiteLLM Proxy**   | `litellm`         | `http://localhost:4000/v1`                          | 你的 LiteLLM 代理密钥                                                   |
| **GPT4Free (本地)** | `gpt4free`        | `http://localhost:1337/v1`                          | 本地，无需密钥                                                          |

> 表中 `qwen` 是 `qwen-portal` 的别名，`glm` 是 `zhipu` 的别名。更多别名见 `provider_metadata.go`。

### 手动接入未列出的 OpenAI 兼容服务

不需要新的协议实现，配置 `provider: openai` 并把 `api_base` 指向对方端点即可：

```json
{
  "model_list": [
    {
      "model_name": "my-gateway-model",
      "provider": "openai",
      "model": "my-model-id",
      "api_base": "https://my-gateway.example.com/v1",
      "api_key": "sk-your-key"
    }
  ]
}
```

<a id="模型配置-model_list"></a>
## 模型配置 (model_list)

> Chagent 推荐显式 `provider` + 原生 `model` 的配置方式，例如 `"provider": "zhipu", "model": "glm-4.7"`。如果未设置 `provider`，旧的单字段 `provider/model` 写法仍然兼容。

如果你想看 agent 分发和轻量模型路由的完整示例，请看 [路由使用指南](routing-guide.zh.md)。

该设计同时支持**多 Agent 场景**，提供灵活的 Provider 选择：

- **不同 Agent 使用不同 Provider**：每个 Agent 可以使用自己的 LLM provider
- **模型回退（Fallback）**：配置主模型和备用模型，提高可靠性
- **负载均衡**：在多个 API 端点之间分配请求
- **集中化配置**：在一个地方管理所有 provider

### 基础配置示例

```json
{
  "model_list": [
    {
      "model_name": "gpt-5.4",
      "provider": "openai",
      "model": "gpt-5.4",
      "api_key": "sk-your-openai-key"
    },
    {
      "model_name": "claude-sonnet-4.6",
      "provider": "anthropic",
      "model": "claude-sonnet-4-6",
      "api_key": "sk-ant-your-key"
    },
    {
      "model_name": "glm-4.7",
      "provider": "zhipu",
      "model": "glm-4.7",
      "api_key": "your-zhipu-key"
    },
    {
      "model_name": "deepseek-chat",
      "provider": "deepseek",
      "model": "deepseek-chat",
      "api_key": "sk-your-deepseek-key"
    }
  ],
  "agents": {
    "defaults": {
      "model_name": "gpt-5.4"
    }
  }
}
```

### `model_list` 条目字段

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `model_name` | string | 是 | 在 agent 配置中引用此模型的唯一名称 |
| `provider` | string | 否 | provider 标识（`openai`、`anthropic`、`anthropic-messages`、`anthropic-bearer` 或 OpenAI 兼容预设）。设置后，Chagent 会将 `model` 原样发送给该 provider |
| `model` | string | 是 | 当设置 `provider` 时，这里填写 provider 原生模型 ID。若未设置 `provider`，仍兼容旧的 `provider/model` 写法 |
| `api_key` | string | 是* | 认证密钥。多个密钥可按请求轮换。本地 provider（Ollama、LM Studio、vLLM）不需要 |
| `api_base` | string | 否 | 覆盖默认的 API 端点 URL |
| `proxy` | string | 否 | 此模型条目的 HTTP 代理 URL |
| `user_agent` | string | 否 | 自定义 `User-Agent` 请求头（支持 OpenAI 兼容与 Anthropic Messages provider） |
| `request_timeout` | int | 否 | 请求超时时间（秒），默认值因 provider 而异 |
| `streaming.enabled` | bool | 否 | 是否允许此模型条目尝试 provider 流式请求，默认 `false`。它只表达模型/端点能力 opt-in，实际还需要当前 channel 的 `settings.streaming.enabled` 同时开启 |
| `max_tokens_field` | string | 否 | 覆盖请求体中 max tokens 的字段名（如 o1 模型使用 `max_completion_tokens`） |
| `thinking_level` | string | 否 | 扩展思考级别：`off`、`low`、`medium`、`high`、`xhigh` 或 `adaptive` |
| `extra_body` | object | 否 | 注入到每个请求体中的额外字段 |
| `custom_headers` | object | 否 | 注入到每个请求中的额外 HTTP 请求头（例如 `{"X-Source":"coding-plan"}`）。若键名与内置请求头同名，会覆盖内置值（如 `Authorization`、`User-Agent`、`Content-Type`、`Accept`）。 |
| `rpm` | int | 否 | 每分钟请求速率限制 |
| `fallbacks` | string[] | 否 | 自动故障转移的备用模型名称 |
| `enabled` | bool | 否 | 是否启用此模型条目（默认：`true`） |

不需要流式时请省略 `streaming` 配置块。写 `"streaming": {"enabled": false}` 是可选的，手写或生成配置时都不需要。

### `provider` / `model` 解析规则

Chagent 按下面的规则解析 `provider` 和最终发给上游的模型 ID：

- 如果设置了 `provider`，则直接使用 `model`。
- 如果未设置 `provider`，则把 `model` 中第一个 `/` 之前的字段当作 provider，第一个 `/` 之后的全部内容当作最终模型 ID。

示例：

| 配置 | 解析后的 Provider | 实际发送的模型 ID |
| --- | --- | --- |
| `"provider": "openai", "model": "gpt-5.4"` | `openai` | `gpt-5.4` |
| `"provider": "anthropic", "model": "claude-sonnet-4-6"` | `anthropic` | `claude-sonnet-4-6` |
| `"model": "openai/gpt-5.4"` | `openai` | `gpt-5.4` |
| `"provider": "openrouter", "model": "openai/gpt-5.4"` | `openrouter` | `openai/gpt-5.4` |
| `"model": "openrouter/openai/gpt-5.4"` | `openrouter` | `openai/gpt-5.4` |

---

## 📝 API Key 对比

| 服务 | 价格 | 适用场景 |
| --- | --- | --- |
| **OpenRouter** | 免费: 200K tokens/月 | 多模型聚合 (Claude, GPT-4 等) |
| **火山引擎 CodingPlan** | ¥9.9/首月 | 最适合国内用户，多种 SOTA 模型（豆包、DeepSeek 等） |
| **智谱 (Zhipu)** | 免费: 200K tokens/月 | 适合中国用户 |
| **Brave Search** | $5/1000 次查询 | 网络搜索功能 |
| **SearXNG** | 免费（自建） | 隐私优先的元搜索引擎（70+ 搜索引擎） |
| **Groq** | 免费额度可用 | 极速推理 (Llama, Mixtral) |
| **Cerebras** | 免费额度可用 | 极速推理 (Llama, Qwen 等) |
| **LongCat** | 免费: 最多 5M tokens/天 | 极速推理 |
| **ModelScope (魔搭)** | 免费: 2000 次请求/天 | 推理 (Qwen, GLM, DeepSeek 等) |