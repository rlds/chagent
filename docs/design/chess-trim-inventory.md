# Chess 基础 Agent 裁剪清单（T7.3 / T7.4）

> 状态：已完成（2026-10-30，第二批清理：audio/evolution/seahorse/非 pico 通道 schema 已删除）  
> 依据：[改进计划 §6.14](agent-workspace-context-plan.zh.md)  
> 规则：T7.1/T7.5 兼容基线通过后才可删除；每删除一项先做引用审计；每删完一组立即 `go build ./cmd/chagent` + 相关测试。

---

## 1. 删除前基线

待填：

- 二进制大小：
- `go list ./...` 包数：
- `go mod graph` 依赖数：
- 启动到 pid / `/health`：
- 空闲 RSS：
- `go test ./pkg/...` 结果：
- `go test -race` 结果：
- T7.1 兼容测试结果：

---

## 2. 代码删除清单

| 区域 | 路径 | 引用审计命令 | 删除 | 构建 | 测试 | 备注 |
|---|---|---|---|---|---|---|
| 非 pico 通道 | `pkg/channels/telegram` | `grep -R "channels/telegram" --include='*.go' .` | [x] | [x] | [x] | 已删除（cd4b88c3） |
| 非 pico 通道 | `pkg/channels/wecom` | 同上 | [x] | [x] | [x] | 已删除（worktree 仅剩 pico） |
| 非 pico 通道 | `pkg/channels/weixin` | 同上 | [x] | [x] | [x] | 已删除（worktree 仅剩 pico） |
| 音频/语音 | `pkg/audio` | `grep -R "pkg/audio" --include='*.go' .` | [x] | [x] | [x] | 已删除：ASR/TTS、agent 转录、gateway voice orchestrator、send_tts 工具、VoiceConfig、bus audio/voice 流 |
| 硬件 | `pkg/tools/hardware`、`pkg/tools/hardware_facade.go` | `grep -R "tools/hardware" --include='*.go' .` | [x] | [x] | [x] | 已删除 |
| 设备 | `pkg/devices` | `grep -R "pkg/devices" --include='*.go' .` | [x] | [x] | [x] | 已删除 |
| 微信 | `pkg/weixin` | `grep -R "pkg/weixin" --include='*.go' .` | [x] | [x] | [x] | 已删除 |
| 进化 | `pkg/evolution`、`pkg/agent/evolution_bridge.go` | `grep -R "pkg/evolution" --include='*.go' .` | [x] | [x] | [x] | 已删除：evolution 包、bridge、config/事件接线、相关测试 |
| 自更新 | `pkg/updater` | `grep -R "pkg/updater" --include='*.go' .` | [x] | [x] | [x] | 已删除（cd4b88c3） |
| 迁移 | `pkg/migrate` | `grep -R "pkg/migrate" --include='*.go' .` | [x] | [x] | [x] | 已删除（cd4b88c3） |
| CLI | `cmd/chagent/internal/{agent,cron,mcp,model,skills,status}` | `grep -R "internal/agent" cmd/chagent` 等 | [x] | [x] | [x] | 已删除（2026-10-30），CLI 仅剩 gateway/version/helpers；`TestNewChagentCommand` 已更新 |
| CLI | `cmd/membench` | `grep -R "cmd/membench" .` | [x] | [x] | [x] | 已删除（cd4b88c3） |
| Provider | chess allowlist 之外的适配器 | 对照 `../../../chess/web/src/utils/chagentProtocols.ts`，逐个 grep | [ ] | [ ] | [ ] | 保留 openai-compatible |
| 可选 | `pkg/cron` | `grep -R "pkg/cron" --include='*.go' .` | [ ] | [ ] | [ ] | 审计 chess 是否用 |
| 可选 | `pkg/seahorse` | `grep -R "pkg/seahorse" --include='*.go' .` | [x] | [x] | [x] | 已删除（非 chess agent 所需；context manager 仅保留 legacy） |
| 可选 | `pkg/commands` | 审计 pico 命令 | [ ] | [ ] | [ ] | 保留最小 |

---

## 3. 文档/脚本/资源删除清单

| 区域 | 路径 | 删除 | lint | 备注 |
|---|---|---|---|---|
| 营销资源 | `assets/` 截图/横幅/GIF | [x] | [x] | 已删除 |
| 翻译文档 | `docs/**/*.{fr,ja,pt-br,vi,ms,it,ko,id}.md` | [x] | [x] | 已删除 |
| 非 pico 通道文档 | `docs/channels/` 非 pico | [x] | [x] | 已删除 |
| 迁移文档 | `docs/migration/` | [x] | [x] | 已删除 |
| 脚本 | `scripts/build-macos-app.sh`、`scripts/setup.iss`、`scripts/test-irc.sh`、`scripts/test-docker-mcp.sh`、`scripts/icon.icns` | [x] | [x] | 已删除 |
| 构建 | `docker/` 多通道/多发行版、`examples/`、集成中已删功能测试 | [x] | [x] | docker/examples 已删除；integration 仅保留 MCP suite（chess 仍用 MCP） |
| README/ROADMAP | 营销/多渠道段落 | [x] | [x] | README 已重写为 chess 基础 agent |
| Makefile | web/channel 目标 | [x] | [x] | 2026-10-30 重写：删除 docker/web/launcher/macos/android/membench，保留 build/test/vet/lint/check/chess 相关 |

---

## 4. 删除后验证

- [ ] `go build ./cmd/chagent`
- [ ] `go test ./pkg/... -count=1`
- [ ] `go test -race ./pkg/agent/... ./pkg/gateway/... ./pkg/channels/pico/... ./pkg/config/... -count=1`
- [ ] `go vet ./...`
- [ ] `./scripts/lint-docs.sh`
- [ ] T7.1 Chess 兼容基线
- [ ] T7.5 Chess E2E
- [ ] 无残留 import/配置/文档链接
- [ ] 体积/依赖下降记录

---

## 5. 结果

## 5. 结果

已删除：
- 早期（cd4b88c3 / baa9d880）：`pkg/channels/telegram`、`pkg/updater`、`pkg/migrate`、`cmd/membench`、营销资源、翻译/多通道/迁移文档、`assets/`、`docker/`、`examples/`。
- 2026-10-30（本轮）：非 chess CLI 子命令 `cmd/chagent/internal/{agent,cron,mcp,model,skills,status}`、
  Makefile 的 web/docker/launcher/macos/android/membench 目标、`docs/guides/docker.zh.md`、
  `chagent agent` CLI 相关文档引用；新增 `docs/guides/configuration.zh.md` 重写、
  `docs/operations/rollback.zh.md`、`pkg/agent/workspacecontext/README.md`。

第二批清理（2026-10-30，-33,770 行 / 94 文件）：
- 删除 `pkg/audio`（ASR/TTS/OGG/voice agent）、`pkg/evolution`、`pkg/seahorse`；
- 删除 agent 转录/`SetTranscriber`、gateway voice orchestrator、`send_tts` 工具与 facade、
  `pkg/config` 的 `voice`/`evolution`/`devices`/`send_tts`/`serial`/`spi`/`i2c` 配置、
  `pkg/bus` 的 audio/voice-control 流、`pkg/channels` 的 voice capability；
- 非 pico 通道 schema/默认值/常量/校验全部删除（仅保留 `pico`/`pico_client`）；
- `go mod tidy` 移除 pion/webrtc、minio/selfupdate、qrterminal、discordgo、
  go.mau.fi/util、modernc/sqlite 等无用依赖（go.mod require 减少 8 项，go.sum 减少 74 行），
  `go mod vendor` 重新生成（本地 vendor 32MB，未纳入 git）；
- 新增 `scripts/lean-test.sh` 与 `make test-lean`：agent 作用域精简测试。

保留（有意保留）：`pkg/providers` 中模型协议适配器（chess 的 model_list 可按需使用）、
`pkg/isolation`（子进程隔离，工具执行需要）、MCP、cron、heartbeat、media、memory、skills、
session、taskstate、tokenizer、routing 等 agent 运行所需包。

验证（清理后）：`go build ./...`、`go vet ./...`、`go test ./... -count=1`（50 包 PASS）、
`./scripts/lean-test.sh`、`./scripts/lint-docs.sh`、`./scripts/chess-e2e.sh` 全部通过。

验证（裁剪后）：`go build ./...`、`go test ./pkg/... -count=1`（50 包 PASS）、
`go test -race ./pkg/agent`（0 DATA RACE）、`go vet ./...`、`./scripts/lint-docs.sh`、
`./scripts/chess-e2e.sh` 全部通过。

---

## 6. 第三批清理：provider 协议收敛（2026-10-30）

目标：只保留通用协议，删除全部厂商专属协议/SDK/CLI/OAuth 适配器。

### 6.1 保留协议

| 协议族 | 实现 | 说明 |
|---|---|---|
| OpenAI Chat Completions 兼容 | `pkg/providers/httpapi`（HTTPProvider） | `openai` + 30 个预设别名（deepseek/openrouter/zhipu/groq/ollama/vllm/lmstudio/litellm/...）；自定义服务用 `provider: openai` + `api_base` |
| Anthropic Messages 兼容 | `pkg/providers/anthropic_messages` | `anthropic`（别名）/`anthropic-messages`（X-API-Key）/`anthropic-bearer`（Bearer） |

### 6.2 删除内容

- 目录：`pkg/providers/{azure,bedrock,cli,oauth,anthropic(SDK),openai_responses_common}`、`pkg/auth`；
- 文件：`cli_facade.go`、`oauth_facade.go`、`facade_compat_test.go`、`factory.go`、
  `httpapi/gemini_*`、`pkg/providers/common/google_common*`、`pkg/config/config_old.go`（死代码）；
- 协议分支：azure/bedrock/gemini/antigravity/claude-cli/codex-cli/github-copilot/
  alibaba-coding-anthropic/minimax 特例；`openai`/`anthropic` 的 OAuth/token 分支；
- config：`AuthMethod`/`ConnectMode` 字段、默认 model_list 中非保留 provider 条目、
  `provider_metadata` 中 9 个已删协议 entries 与 `DefaultAuthMethod/AuthMethodLocked` 字段；
- 文档：ANTIGRAVITY 使用/认证文档、协议参考与路由示例中的已删协议。

### 6.3 依赖/体积

`go mod tidy` + `go mod vendor`：移除 Azure SDK、AWS SDK、anthropic-sdk-go、openai-go、
copilot-sdk、oauth2/jsonschema 等；go.mod require 减少（较上轮再 -48 行 / go.sum -107 行），
vendor 32MB → 15MB。

### 6.4 验证

`go build ./...`、`go vet ./...`、`go test ./... -count=1`、`./scripts/lean-test.sh`、
`./scripts/lint-docs.sh`、`./scripts/chess-e2e.sh` 全绿。
