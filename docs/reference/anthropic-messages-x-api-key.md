# anthropic-messages 协议（X-API-Key 认证）详细说明

> 面向 agent / 开发者的协议参考。描述 `provider: "anthropic-messages"` 的配置方法、
> 请求链路、认证细节、响应解析与限制。代码位于
> `pkg/providers/anthropic_messages/provider.go`，工厂入口位于
> `pkg/providers/factory_provider.go` 的 `case "anthropic-messages"`。

## 1. 协议定位

chagent 中有三个 Anthropic 相关协议，**不要混淆**：

| 协议 | 请求格式 | 认证头 | 流式 | 典型场景 |
|------|----------|--------|------|----------|
| `anthropic` | **原生 Anthropic Messages**（与 `anthropic-messages` 相同实现，仅协议名别名） | `X-API-Key: <key>` | 不支持 | 标准 Anthropic API 或私有网关 |
| `anthropic-messages` | **原生 Anthropic Messages** | `X-API-Key: <key>` | **不支持** | 标准 Anthropic API / 兼容 X-API-Key 的私网网关 |
| `anthropic-bearer` | **原生 Anthropic Messages** | `Authorization: Bearer <token>` | **不支持** | 使用 Bearer token 的私网网关 |

`anthropic-messages` 与 `anthropic-bearer` 共用同一个 provider 实现，
仅认证头不同（见 [provider.go:133-138](../../pkg/providers/anthropic_messages/provider.go) 的 switch）。

## 2. 最小配置示例

```jsonc
{
  "agents": {
    "defaults": {
      "model_name": "claude-opus-4-7",   // 指向 model_list 中的别名
      "max_tokens": 8192                  // 必填，Anthropic 协议要求 max_tokens
    }
  },
  "model_list": [
    {
      "model_name": "claude-opus-4-7",   // 用户侧别名（必填）
      "model": "claude-opus-4-7",        // 发给 API 的模型 ID（必填）
      "provider": "anthropic-messages",  // 关键：指定协议
      "api_base": "https://api.anthropic.com/v1",
      "api_key": "sk-ant-xxxx",       // X-API-Key 的值
      "enabled": true
    }
  ]
}
```

## 3. 字段逐项说明

### 3.1 `provider`（协议选择）

- **显式写明最安全**。省略时按 `model` 前缀推断（`ExtractProtocol`，factory_provider.go:56）：
  - `"anthropic/claude-opus-4-7"` → 推断为 `anthropic`（与 `anthropic-messages` 相同的原生实现）
  - 无前缀 `"claude-opus-4-7"` → **默认 `openai`**（完全错误的格式）
  - 只有第一段被剥离：`"nvidia/z-ai/glm-5.1"` → provider=`nvidia`，model=`z-ai/glm-5.1`
- 结论：要使用本协议，**必须** `provider: "anthropic-messages"`，或 `provider` 省略但
  model 前缀写 `"anthropic-messages/..."`（前缀即协议名，不推荐）。
- 协议名会经 `NormalizeProvider` 小写归一；别名见 `provider_metadata.go`
  （`anthropic` 的别名是 `claude`；`anthropic-bearer` 的别名是 `anthropic-bearer-auth`）。`auth_method` 字段已移除。

### 3.2 `api_base`（URL 规则，易踩坑）

工厂入口对 `api_base` 为空的条目回退到 `https://api.anthropic.com/v1`。
随后 provider 构造函数调用 `common.NormalizeBaseURL(apiBase, default, appendV1Suffix=true)`：

1. 去掉尾部所有 `/`
2. 若以 `/v1` 结尾则**剥离**
3. **强制追加 `/v1`**

最终请求地址永远是 `<归一化结果>/messages`（`url.JoinPath(p.apiBase, "messages")`）。

| 配置的 api_base | 实际请求地址 |
|-----------------|--------------|
| `https://claude.ddmc-inc.com/anthropic` | `https://claude.ddmc-inc.com/anthropic/v1/messages` |
| `https://claude.ddmc-inc.com/anthropic/v1` | `https://claude.ddmc-inc.com/anthropic/v1/messages`（同上） |
| `https://claude.ddmc-inc.com/anthropic/` | 同上（尾斜杠被清理） |
| （省略） | `https://api.anthropic.com/v1/messages` |

⚠️ 网关路由若不带 `/v1` 段（如直接暴露 `.../anthropic/messages`），本协议**无法**适配，
因为 `/v1` 是强制追加的。需要网关侧兼容或改用其他协议。

### 3.3 `api_key`（认证凭据）

- 类型是字符串数组（`SecureStrings`）。**第一个 key** 作为 `X-API-Key` 头的值
  （`cfg.APIKey()` 取首元素）。
- 配置多个 key 时，加载阶段会把同一条目展开为多个"虚拟条目"实现轮换/容错
  （虚拟条目不会回写到配置文件）。
- 同名 `model_name` 的多条目按 round-robin 负载均衡（config.go 的 findMatches 逻辑）。
- key 为空时工厂直接报错：`api_key is required for anthropic-messages protocol`。

### 3.4 `enabled`

- 省略时在加载阶段推断：**有 api_key → 自动启用**；`model_name == "local-model"` → 自动启用；
  其余（无 key 且非 local-model）→ **禁用**。
- 显式写 `"enabled": false` 优先于推断。
- 禁用的条目不参与模型解析与 fallback。

### 3.5 其他可选字段

| 字段 | 作用 |
|------|------|
| `request_timeout` | HTTP 客户端超时秒数；0 或省略 = 默认 **120s**（provider.go:36） |
| `tool_schema_transform` | 工具 schema 兼容变换（如 `"simple"`），在工厂层包装 |
| `streaming.enabled` | **对本协议无效**（见 §7 限制） |
| `extra_body` / `custom_headers` | 本协议的 provider **不读取**这两个字段（仅 openai 系 HTTPProvider 支持）；需要额外头时网关侧处理或换协议 |
| `user_agent` | 自定义 UA；省略时为 `Chagent/<version>` |

## 4. 认证细节（发出请求时）

每个请求固定携带的头部：

```
POST <api_base>/v1/messages HTTP/1.1
Content-Type: application/json
Anthropic-Version: 2023-06-01          # 固定值，常量 defaultAPIVersion
X-API-Key: sk-ant-xxxx                 # 本协议的认证头
User-Agent: Chagent/<version>         # 可选覆盖
```

- `Anthropic-Version` 固定为 `2023-06-01`，不可配置。
- 认证失败（HTTP 401）返回错误：`authentication failed (401): check your API key`。

## 5. 请求体格式

由 `buildRequestBody` 从内部统一消息格式转换：

```jsonc
{
  "model": "claude-opus-4-7",     // 来自候选模型 ID
  "max_tokens": 8192,             // 必填，来自 agents.defaults.max_tokens；缺失直接报错
  "temperature": 0.7,             // 仅当 options 提供时携带
  "system": "...",                // 所有 role=system 消息用 "\n\n" 拼接后放顶层
  "messages": [
    { "role": "user", "content": "..." },
    { "role": "assistant", "content": [
        { "type": "text", "text": "..." },
        { "type": "tool_use", "id": "toolu_x", "name": "read_file", "input": {...} }
    ]},
    { "role": "user", "content": [
        { "type": "tool_result", "tool_use_id": "toolu_x", "content": "..." }
    ]}
  ],
  "tools": [
    { "name": "...", "description": "...", "input_schema": {...} }
  ]
}
```

转换规则要点：

- **system 消息**：全部拼接为顶层 `system` 字符串（不放进 messages）。
- **tool result**：内部 `role=tool`（或带 `ToolCallID` 的 user 消息）→ Anthropic 的
  `role=user` + `tool_result` block；连续的 tool result 会**合并进同一条 user 消息**。
- **assistant 的 ToolCalls** → `tool_use` block；`Arguments` 为 nil 时兜底为 `{}`（GLM-4 兼容）。
- **无 system 消息时不发 `system` 字段；无 tools 时不发 `tools` 字段**。

## 6. 响应解析与 model_name 上报

`parseResponseBody` 处理非流式 JSON 响应：

- `content[]` 中的 `text` block 拼接为 `Content`；`tool_use` block 转为 `ToolCalls`
  （同时填充 `Function.Name/Arguments` 便于内部处理）。
- `stop_reason` 映射：`tool_use`→`tool_calls`，`max_tokens`→`length`（agent 层再归一为
  `truncated`），`end_turn`/`stop_sequence`/其他→`stop`。
- usage 映射（对齐 OpenAI 语义）：
  - `prompt_tokens = input_tokens + cache_creation_input_tokens + cache_read_input_tokens`
  - `completion_tokens = output_tokens`
  - `cached_tokens = cache_read_input_tokens`
- **`model` 字段提取**（本次新增）：响应根部的 `model`（服务端实际使用的模型 ID）
  写入 `LLMResponse.Model`。agent 层随后**优先用它覆盖 `exec.llmModelName`**，
  因此会话历史（`Message.ModelName`）、流式展示和对外发布的
  `OutboundMessage.Context.Raw["model_name"]` 上报的都是服务端返回的真实模型名。
  响应不含 `model` 字段时回落到配置别名，行为与旧版一致。

## 7. 限制与注意事项

1. **不支持流式**。`anthropicmessages.Provider` 只实现 `Chat` + `GetDefaultModel`，
   未实现 `StreamingProvider`/`StreamingEventProvider` 接口。配置了
   `streaming.enabled` 也不会生效；agent 会以非流式调用，通道侧表现为整段回复
   （pico 通道仍会有 reasoning/interim 消息，但不是逐 token）。
2. **不支持 `extra_body` / `custom_headers`**。这两个字段只有 openai 系 provider 读取。
3. **不支持 `proxy` 字段**（构造函数没有 proxy 参数；`anthropic` 协议的
   OpenAI 兼容路径才支持）。
4. **thinking / cache_control**：当前实现不发送 `thinking` 参数，也不设置
   `cache_control`，`system` 以纯字符串发送（非 block 数组）。prompt caching 由
   服务端自动处理时 usage 中的 cache 字段仍会正确统计。
5. **URL 必须容忍强制 `/v1`**（见 §3.2）。
6. 未知 HTTP 状态码统一报 `API request failed with status <code>: <body>`；
   429 会带上响应体内容，供上层 fallback 分类为限流。

## 8. 排错速查

| 现象 | 可能原因 |
|------|----------|
| `api_key is required for anthropic-messages protocol` | `api_key` 缺失或为空 |
| 401 authentication failed | key 无效 / 网关不认 X-API-Key（网关若要 Bearer，应改用 `anthropic-bearer`） |
| 404 endpoint not found | 网关不接受 `<api_base>/v1/messages` 路径（§3.2 的 `/v1` 强制拼接） |
| 400 bad request（模型不存在） | `model` 字段写错，或网关要求带命名空间前缀的 ID |
| 响应格式错误 / 请求被网关拒收 | 误把 OpenAI 兼容网关配成了本协议（应改 `anthropic` 或 `custom`） |
| 上报的 model_name 与配置别名不一致 | 预期行为：优先取响应中的 `model` 字段（§6） |
| 上报的 model_name 一直是配置别名 | 网关响应未带 `model` 字段，回落到别名 |
