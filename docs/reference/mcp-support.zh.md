# MCP 支持矩阵（MCP Support）

> 适用范围：Chagent chess 基础 agent 的 MCP 客户端能力、协议版本、传输与最新协议特性支持。
> 关键代码路径：`pkg/mcp/manager.go`、`pkg/tools/integration/mcp_tool.go`、`pkg/config/config.go`（`MCPConfig`）。
> SDK：`github.com/modelcontextprotocol/go-sdk v1.6.1` 的本地最小 fork（`third_party/mcp-go-sdk`，见其 `PATCH.md`），启用 2026-06-30 原生支持。
> 核对日期：2026-11-02（协议版本常量见 `pkg/mcp/manager.go`；独立探针证据见
> `build/functest-20261030/verify/mcp-probe/`，gitignored）。

## 1. 协议版本

客户端在 `initialize` 中始终请求 **SDK 最新协议版本**，服务端可以协商降级；不是本仓库
自己维护版本号（无本地覆盖逻辑）。

| 协议版本 | 支持 | 说明 |
|---|---|---|
| **`2026-06-30`** | ✅（**initialize 请求版本 / 原生支持**） | 本地 go-sdk fork 的 `latestProtocolVersion`；客户端请求该版本，服务端返回 2026-06-30 时**直接接受、不降级**。启用 SDK 已内置的 SEP-2575 标准 headers 等特性；本项目 `LatestMCPProtocolVersion`。 |
| `2025-11-25` | ✅（可协商） | 服务端可降级到该版本；客户端接受。 |
| `2025-06-18` | ✅（可协商） | 本版本引入 `structuredContent`；本项目已透传到模型可见结果。 |
| `2025-03-26` | ✅（可协商） | 引入 Streamable HTTP；本项目 `type=http` 使用该传输。 |
| `2024-11-05` | ✅（可协商） | 旧版 HTTP+SSE 传输；`type=sse` 仅用于兼容旧 server。 |
| 其它（如 `2027-01-01`） | ❌ | 客户端以 `unsupported protocol version` 拒绝，不会静默接受。 |

证据链：
- 代码：`third_party/mcp-go-sdk/mcp/shared.go` 将 `latestProtocolVersion` 设为 `2026-06-30` 并把其加入
  `supportedProtocolVersions`（上游 v1.6.1 已有该常量与 SEP-2575 headers 逻辑，只是未启用）；
  `newMCPClient()` 使用 SDK 默认行为（请求 latest）；`MCPProtocolVersions` 常量锁定矩阵。
- 测试：`pkg/mcp/protocol_version_test.go`（官方 SDK in-memory fake server 捕获 initialize 参数；
  断言请求 `2026-06-30`、服务端降级 `2025-11-25`/`2025-06-18`/`2024-11-05` 仍可连接、未知版本被拒绝）。
- 独立探针（Lead 执行）：`build/functest-20261030/verify/mcp-probe/` 实测 initialize
  `protocolVersion=2026-06-30`，连接日志 `Connected to MCP server protocol=2026-06-30`。

## 2. 传输

| `type` | 协议 | 状态 | 说明 |
|---|---|---|---|
| `stdio` | 全部 | ✅ | `command`/`args`/`env`/`env_file`；子进程隔离传输。子进程生命周期绑定 MCP session（不是 reload/turn 请求 ctx），reload 或回合结束后 server 不会被误杀；session/manager 关闭时由 transport.Close 回收（含 SIGTERM/SIGKILL 兜底）。 |
| `http` / `streamable-http` | 2025-03-26+ | ✅ | Streamable HTTP，`DisableStandaloneSSE=true`（一问一答式 POST，不保持独立 SSE 流）。 |
| `sse` | 2024-11-05 专用 | ⚠️ 兼容 | 旧版 HTTP+SSE（GET /sse + POST endpoint）。新 server 请用 `http`。 |

未显式写 `type` 时按配置自动推断：有 `command` → `stdio`；有 `url` → **`sse`（旧版 HTTP+SSE，兼容语义）**，见 `pkg/config/mcp_transport.go:21-32`（URL→sse 分支在 :25-26）。**要使用最新 Streamable HTTP，必须显式写 `"type": "http"`（或 `streamable-http`）**。

## 3. 客户端 capability 声明

| capability | 是否声明 | 说明 |
|---|---|---|
| `roots`（`listChanged:true`） | ✅ 声明 | SDK 默认；但 **roots 列表当前为空**（见 §4 已知限制）。 |
| `sampling` | ❌ 不声明 | chagent 不把自身模型暴露为 MCP sampling 后端。 |
| `elicitation` | ❌ 不声明 | 无交互式 UI 可渲染 form/url；不声明是安全默认（服务端会得到“不支持”，而不是永久挂起）。 |
| `experimental` / `extensions` | ❌ 无 | 未使用。 |

## 4. 最新协议特性支持表

| 特性 | 协议版本 | 状态 | 说明 / 原因 / 后续建议 |
|---|---|---|---|
| tools（`tools/list`、`tools/call`） | 2024-11-05+ | ✅ 已支持 | 连接后列出工具并包装为 `mcp_<server>_<tool>`；支持分页。 |
| `structuredContent` | 2025-06-18+ | ✅ 已支持 | `CallToolResult.StructuredContent` 非空时以紧凑 JSON 追加到模型可见结果（标记 `[MCP 结构化内容]`），纳入大文本 artifact/spill 机制；与 Content 重复时去重。 |
| `tools/list_changed` 自动刷新 | 2025-06-18+（通知更早） | ✅ 已支持（Manager + agent 重注册） | `ClientOptions.ToolListChangedHandler` 收到通知后异步 `tools/list` 刷新 `ServerConnection.Tools`，经 `WithToolListChangedHandler`/`SetToolListChangedHandler` 回调；`pkg/agent/agent_mcp.go` 消费回调对 agent registry 做增量注册/注销（新工具可立即调用，移除工具立即不可见）。live E2E P11 用 fixture 运行期 add/remove 工具验证：重注册日志 `tool_count=2→1`、真实模型成功调用新增的 `mcp_p11_echo_dynamic`、移除后不再可调用。单测：`pkg/mcp/tool_list_changed_test.go`。 |
| roots 实际值 | 2024-11-05+ | ⚠️ 能力已声明、列表为空（已知限制） | 未给 SDK client 注册 `file://` workspace root，filesystem 类 server 拿不到工作区路径（`roots/list` 返回空数组）。接线成本低但涉及 manager/connect 重构，本阶段只记录：在 `connectServer` 前 `client.AddRoots(&mcp.Root{URI: fileURI(workspace)})` 即可。 |
| elicitation（form/url） | 2025-06-18（url 2025-11-25） | ❌ 不支持 | chess/pico 无交互 UI；不声明 capability 避免 server 等待永不返回的请求。后续如需：增加配置开关 `decline_elicitation`，声明 capability 并实现 handler 一律返回 `action=decline`（无 UI 的可安全默认），但会改变 server 行为，需单独评审。 |
| sampling | 2024-11-05+ | ❌ 不支持 | 不声明 `sampling` capability；不实现 `CreateMessageHandler`。 |
| resources / prompts | 2024-11-05+ | ⚠️ resources 部分支持（DSH 工具组）；prompts 未暴露 | resources 通过 DSH 工具组 `pkg/tools/dsh/mcpresources` 暴露三件套：`list_mcp_resources`、`list_mcp_resource_templates`、`read_mcp_resource`（只读，大文本/二进制落 `.artifacts/mcp-resources/`，带大小/数量上限）；`resources/list_changed` 与 `resources/subscribe` 尚未处理（工具列表变化不会自动刷新）。prompts 未暴露；后续如需按 `get_prompt` 工具 + 权限边界单独评审。 |
| tasks | 2025-11-25 新增 | ❌ 不支持 | Go SDK v1.6.1（含本地 fork）**未实现** tasks（无 `tasks/*` 方法/类型）；协议版本原生支持不等于 tasks 特性支持。后续：升级到实现 tasks 的 SDK 版本，再评估与本地 durable TaskState 的映射。 |
| logging 通知 | 2024-11-05+ | ❌ 忽略 | 未设置 `LoggingMessageHandler`；服务端日志通知不会进入 chagent 日志（不影响工具调用）。 |

## 5. 配置示例

仅配置 server 即自动启用 MCP（`tools.mcp.enabled` 可省略）：

```json
{
  "tools": {
    "mcp": {
      "keepalive_interval": 30,
      "max_inline_text_chars": 16384,
      "servers": {
        "chess": {
          "type": "http",
          "url": "https://mcp.example.invalid/mcp",
          "enabled": true,
          "headers": { "Authorization": "Bearer YOUR_MCP_TOKEN" }
        },
        "local-fs": {
          "type": "stdio",
          "command": "npx",
          "args": ["-y", "@modelcontextprotocol/server-filesystem", "/workspace"],
          "env": { "LOG_LEVEL": "info" },
          "enabled": true
        }
      }
    }
  }
}
```

语义：
- 省略 `enabled` + 配了 `servers` → 自动启用（servers-only）。
- **显式** `"tools.mcp.enabled": false` → 关闭 MCP（即使配了 servers；见 `MCPConfig.ExplicitDisabled`）。
- 单个 server 的 `"enabled": false` 只跳过该 server；全部 server disabled 时不连接、不报错。
- `keepalive_interval`：秒；`0`=默认 30s；负数=关闭 keepalive。
- `max_inline_text_chars`：MCP 文本内联上限（超出落 `.artifacts/mcp/`）。
- `discovery.enabled`：MCP 工具发现/隐藏（默认关闭）。

## 6. 已知限制汇总（按优先级）

1. **roots 列表为空**：能力已声明，filesystem server 无法自动定位工作区；接线点见 §4。
2. **resources/list_changed 未处理**：`resources/list_changed` / `resources/subscribe` 未接线，
   `list_mcp_resources` 读到的是调用时快照；`tools/list_changed` 已完整支持（Manager 刷新 +
   agent 增量注册/注销，见 §4）。
3. **elicitation / sampling / prompts / tasks 不支持**：原因与后续见 §4，不假装支持。
4. Streamable HTTP 关闭独立 SSE 流，因此 server-initiated 请求仍可用（同连接响应方向），但
   服务端主动推送的 out-of-band SSE 消息不会被消费。
5. **stdio 子进程生命周期**（已修复，保留说明）：实现曾用请求 ctx 作为 `exec.CommandContext`
   的 context，导致 `/reload` 或回合结束后 stdio MCP server 被误杀（工具调用靠重连兜底）。
   现改为 session 绑定：`context.WithoutCancel` + transport.Close 显式 cancel/回收；
   回归测试 `pkg/mcp/stdio_lifecycle_test.go`（connect ctx 取消后仍可 tools/call、Close 后子进程退出），
   live E2E P11 亦覆盖 reload 后持续调用。

## 7. 测试与校验

```bash
go test ./pkg/mcp/... ./pkg/tools/integration/... ./pkg/config/... -count=1
go build ./...
```

- `pkg/mcp/protocol_version_test.go`：initialize 版本断言（latest / 降级 / 未知拒绝）+ 默认 capability 断言。
- `pkg/tools/integration/mcp_tool_test.go`：structuredContent 追加/仅结构化/去重/大文本 spill/marshal 失败降级。
- `pkg/mcp/manager_test.go`：连接生命周期、显式关闭、servers-only 自动启用、重连等。
- `pkg/mcp/tool_list_changed_test.go`：`tools/list_changed` → Manager 刷新 + 回调。
- `pkg/mcp/stdio_lifecycle_test.go`：stdio 子进程生命周期绑定 MCP session（不再随请求 ctx 被杀）。
- live E2E（真实模型）：`python3 scripts/agent-e2e-scenario.py --phases P0,P11`（P11 阶段，产物
  `build/functest-20261030/live/p11-mcp/`）：servers-only 自动启用、协商 2026-06-30、真实模型
  `tools/call` echo、fixture 运行期动态增删工具（agent 重注册 + 新工具可调用 + 移除即不可用）、
  显式 `enabled=false` reload 后不连接/工具不可用；期望 PASS=24 FAIL=0。