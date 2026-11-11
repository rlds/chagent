# 已修复问题（Fixed Issues）

> 快照：2026-11-02，HEAD `5da70e0b` + 未提交修复工作树。
> 说明：**本轮所有修复均未 commit**，因此"修复 commit"列统一标注"工作树（未提交）"；提交后补真实 commit 号即可。
> 索引与状态口径见 [问题库 README](README.md)；完整测试与复测矩阵见 [功能测试报告 §14](../design/agent-functional-test-report.zh.md#14-修复记录与修复后复测2026-10-30-17311830)。

## FIX-P0-01 reload 后 SIGTERM shutdown 死锁

- **现象**：任何一次成功 `POST /reload`（或 `hot_reload=true` 触发重载）之后发送 `SIGTERM`，进程 >30s 不退出、pid 文件残留、`/health` 不再响应，只能 `kill -9`；直接破坏 chess「SIGTERM ≤5s、pid 清理」契约。
- **根因**：`Manager.Reload` 覆盖了 `m.dispatchTask`（新建 cancel），但 `StartAll` 启动的 3 个 dispatcher 仍使用启动时的 `ctx`；`StopAll` 取消的是孤儿 ctx，`dispatchWG.Wait()` 在持有 `m.mu` 的情况下永久阻塞。
- **修复（工作树，未提交）**：`pkg/channels/manager.go` 引入常驻 `dispatchCtx`——`StartAll` 创建并保存，`Reload` 复用（不再覆盖 `dispatchTask`），`StopAll` 取消并清理 `dispatchCtx/dispatchTask`。
- **回归测试**：`TestReloadThenStopAllDoesNotDeadlock`（`pkg/channels/manager_test.go`：StartAll→2×Reload→StopAll ≤3s）；`TestGatewayReloadThenCleanShutdown`（`pkg/gateway/gateway_integration_test.go`，gateway 层）。
- **验证证据**：compat `08-reload-shutdown.sh` **9/0**（对照 3.8ms、`/reload` 后 18.5ms、hot_reload 后 8.7ms，均 ≤5s 且 pid 删除）；live P9.1 退出 **0.01s**；`build/functest-20261030/baseline/compat/logs/run-08*.log`。

## FIX-P1-01 usage 锚定计量主回合死路径

- **现象**：迁移报告声称已接入「usage 锚定 token 计量」，但主回合始终拿不到 provider usage 锚点（overlay 探针 `usageAnchor=nil`）。
- **根因**：`turn_coord.go` 调用 `pipeline.CallLLM(...)` 时首参传了外层 `ctx` 而非带 `turnState` 的 `turnCtx`，导致 `pipeline_llm.go` 的 `turnStateFromContext` 在主回合命不中。
- **修复（工作树，未提交）**：`pkg/agent/turn_coord.go` 主回合改为 `pipeline.CallLLM(turnCtx, turnCtx, ts, exec, iteration)`。
- **回归测试**：`TestCallLLMMainTurnRecordsUsageAnchor`（`pkg/agent/loop_context_regression_test.go`，断言 `LastUsage`/`FinishReason`/`usageAnchor` 记录）。
- **验证证据**：overlay 探针主回合 `usageAnchor={1234,466}`（修复前 `nil`）；上下文专项 F 组 usage 锚点断言 PASS。

## FIX-P1-02 pre-step 有序表不可观测

- **现象**：pre-step 有序注入表无日志、无回合级 E2E，生产环境无法确认注入顺序与条数（审计 P1）。
- **根因**：`runPreStepContextSources` 只有 debug 级或没有注入记录，且缺 iteration>1 的集成测试。
- **修复（工作树，未提交）**：`pkg/agent/prestep.go` 在注入时输出 Info 级结构化日志（`source_id/order/injected/iteration`，R1 保证默认配置可见）；并补回合级集成测试。
- **回归测试**：`TestTurnPreStepInjectsOrderedContextSourcesAtSecondIteration`（断言 100→200→300→350→400→500 顺序、无重复）。
- **验证证据**：agent 定向测试 135 PASS / 0 FAIL；`build/functest-20261030/mechanism/` 与功能测试报告 §14.1。

## FIX-P1-03 context-pressure 首调用不可触发

- **现象**：临近上限提醒（`context-pressure`）只在 iteration>1 触发，单次 LLM 调用的回合永远收不到提醒。
- **根因**：pre-step 只在迭代路径执行；`pipeline_setup` 初始消息构建后没有评估 pressure。
- **修复（工作树，未提交）**：`pkg/agent/pipeline_setup.go:147` 在初始消息构建后调用 `injectContextPressureNotice`，复用 band 去重逻辑（同 band 每 turn 最多一次）。
- **回归测试**：`TestTurnContextPressureNoticeOnFirstIteration`（`pkg/agent/loop_context_regression_test.go`：单次调用有通知、迭代不重复）。
- **验证证据**：上下文专项 E 组 **7/7 PASS**（4 个真实 turn 中 4 条 notice，2 条出现在首次 assistant 回复之前）；`build/functest-20261030/context/summary.md`。

## FIX-P1-04 MCP servers-only 不自动启用

- **现象**：只配置 `tools.mcp.servers`（不写 `tools.mcp.enabled=true`）时 MCP 不连接，与工具参考文档"配置 servers 即自动启用"相反。
- **根因**：`pkg/mcp/manager.go` 用 `if !mcpCfg.Enabled` 提前返回，未使用 `MCPConfig.Active()`（= `Enabled || len(Servers)>0`）。
- **修复（工作树，未提交）**：`pkg/mcp/manager.go:202` 改为 `if !mcpCfg.Active()`；并核对 `IsToolEnabled` 等使用点一致。
- **回归测试**：`TestLoadFromMCPConfig_ServersOnlyActivatesMCP` 等 4 项（`pkg/mcp/manager_test.go`）。
- **验证证据**：配置矩阵 D0 PASS（`/list mcp` Connected: yes、`/show mcp` 含工具、真实调用 `mcp_cfgcap_echo` 成功）；`build/functest-20261030/config/summary.md`。

## FIX-R5-01 MCP 显式 enabled=false 语义

- **现象**：servers-only 自动启用后，用户显式写 `tools.mcp.enabled=false` 也无法关闭 MCP（R5 复核发现）。
- **根因**：`Active()` 只看 `Enabled || servers`，无法区分"省略 enabled"与"显式 false"。
- **修复（工作树，未提交）**：`pkg/config/config.go` 增加 `MCPConfig.ExplicitDisabled`（`json:"-"`）；`pkg/config/config_loader.go` 新增 `rawMCPExplicitlyDisabled` 从原始 JSON 检测显式 false；`Active()` 优先尊重显式关闭，同时保持"省略 enabled + servers 自动启用"。
- **回归测试**：`pkg/config/mcp_active_test.go`（8 行语义矩阵 + round-trip）、`pkg/mcp/manager_test.go` 显式关闭端到端（0 次 connect）。
- **验证证据**：配置矩阵新增 **D0b**：显式 `enabled=false` + servers → integration disabled、Connected: no、reload 后无重连、fixture 无新 RPC；配置矩阵最终 **43/43 PASS**。注意：程序内直接构造 `Config`（不经 `LoadConfig`）时仍以 `Enabled` 字段为准（见 [OPEN-07](open.zh.md#open-07-r5-mcp-显式关闭仅在配置矩阵验证)）。

## FIX-P2-01 配置显式 0 被静默补默认

- **现象**：显式写 `autonomy.max_rounds=0`、`no_progress_rounds=0`、`workspace_context.max_bytes=0`/`max_source_bytes=0` 时启动成功，值被静默替换为默认值，配置笔误不可诊断。
- **根因**：`LoadConfig` 先 `ApplyContextDefaults()`（把 `<=0` 补成默认）再 `Validate()`，零值已不可区分。
- **修复（工作树，未提交）**：`pkg/config/context_config.go` 新增 `ValidateExplicitContextZeros`（指针字段区分"缺省/null"与"显式 0/负数"，报错含字段路径与允许范围）；`pkg/config/config.go:1152` 在 `ApplyContextDefaults` **之前**校验原始 JSON。`round_delay_ms=0` 仍合法。
- **回归测试**：`pkg/config/context_zeros_test.go`（0/负数拒绝、缺省/null 用默认、正数/`round_delay_ms=0` 正常）。
- **验证证据**：compat 04 `12/0`（原 6/2）；配置矩阵 F7–F9 拒绝错误含字段+范围、F13/F14 正常；错误示例 `agents.defaults.autonomy.max_rounds must be between 1 and 200, got 0`。

## FIX-P2-02 supersede 仅回合起点生效

- **现象**：同一回合内多次注入 task-state/runtime 时，模型可见历史可能残留多条旧副本，只在回合起点收敛。
- **根因**：`supersedeContextMessages` 只在 `pipeline_setup` 回合起点调用，迭代路径的 pre-step 注入后未收敛。
- **修复（工作树，未提交）**：`pkg/agent/turn_coord.go:198` 在迭代路径 pre-step 之后调用 `supersedeContextMessages`。
- **回归测试**：`TestTurnPreStepSupersedesTaskStateWithinTurn`（迭代 2 次后 task-state 恰 1 条且为最新）。
- **验证证据**：agent 测试 PASS；JSONL 仍保留审计副本（见 [OPEN-03](open.zh.md#open-03-d4-supersede-仅模型可见层jsonl-保留审计副本)）。

## FIX-P2-03 批调度 capability 名实不一致

- **现象**：批调度准入用 `tools.IsToolParallelSafe`（按工具 capability），分组却用名称级 `IsParallelSafe` 白名单，导致 `ConcurrencySafe()=false` 的已知名工具仍被并行。
- **根因**：`tool_batch.go` 分组与 `pipeline_execute.go` 准入使用两套判定。
- **修复（工作树，未提交）**：`pkg/agent/tool_batch.go` 增加统一的 `isSafe` 判定；`pipeline_execute.go` 用 registry + `IsToolParallelSafe` 传入；未注册/未知工具保守串行。
- **回归测试**：`TestBatchIsSafeOverridesNameAllowlist`、`TestBatchUnknownToolStaysExclusive`、`TestBatchCapabilityEnablesNonAllowlistedTool`、`TestExecuteToolsConcurrentBatchUsesCapabilityNotName`（`pkg/agent/tool_batch_test.go`）。
- **验证证据**：批调度 12/12 PASS；live 日志仍有 `Tool call (batch)`。

## FIX-P2-04 配置样例与删除残留

- **现象**：`config/config.example.json` 仍是 v3 schema（`version:3`、`channel_list`、telegram、devices/voice/evolution、复数 `api_keys`，启动即失败）；`.github/dependabot.yml` 仍含 `/web/frontend`；`.goreleaser.yaml` 仍构建已删的 `web/backend`/launcher/docker；architecture 文档仍引用 web 路径；缺 `docs/chess-integration.md`。
- **根因**：功能删除（T7.3/T7.4）后未同步清理示例配置、CI/发布配置与文档。
- **修复（工作树，未提交）**：`config/config.example.json` 重写为可加载的 v2 标准样例（单数 `api_key`、仅 chess 相关节、无注释）；`.goreleaser.yaml` 删除前端/后端/winres/launcher/dockers_v2/android 引用，保留 `cmd/chagent` 发布；`.github/dependabot.yml` 删除 npm 块；`docs/architecture/{routing-system,session-system}.zh.md` 改为当前实现；新增 `docs/chess-integration.md`。
- **验证证据**：compat 06 从 **23/5 → 32/0**（含 example 可启动 + `/health` 200 + pico ping + 21ms 退出）；YAML `yaml.safe_load` 全通过；`git grep 'pnpm|web/frontend|web/backend' .github/` 为空；`docs lint: OK`。

## FIX-P2-05 CI workflows 已删功能引用

- **现象**：`release.yml`/`nightly.yml` 仍 setup pnpm/Node + `web/frontend/pnpm-lock.yaml`、登录 GHCR/DockerHub、QEMU/Buildx、收集 android zip；`create_dmg.yml` 调用已删的 `make build-macos-app` 与 "Chagent Launcher.app"；`docker-build.yml` 依赖不存在的根 Dockerfile。
- **根因**：源码/发布配置裁剪后 CI 未同步。
- **修复（工作树，未提交）**：删除 `create_dmg.yml`（整份只服务已删 GUI launcher）与 `docker-build.yml`（无调用方且无 Dockerfile）；`release.yml`/`nightly.yml` 删除 pnpm/Node、QEMU/Buildx、docker 登录、`packages: write`、`DOCKERHUB_IMAGE_NAME`、`INCLUDE_ANDROID_BUNDLE`、android zip 收集；`build.yml`/`pr.yml` 检查后无需改动（integration 用的 MCP suite 仍存在）。
- **验证证据**：7 个 workflow + dependabot 全部 YAML 解析 OK；`git grep 'pnpm|web/frontend|web/backend' .github/` 与 `launcher|android|winres` 均为空；删除文件无任何引用。

## FIX-P2-06 文档漂移

- **现象**：迁移报告 pre-step 顺序写成 4 项/loop-guard=400（实际 6 项/500）、"不写 spill"过期；执行报告 §5/§9/§13 指标与 token 说明过期；`configuration.zh.md` 未说明根 `AGENTS.md` 作为 agent definition 不重复注入；routing/session 文档仍引用 web；`troubleshooting.zh.md` 仍用复数 `api_keys`；`dsh-context-prompt.zh.md` 引用已删 `context_seahorse.go`；pico token 文档过期。
- **修复（工作树，未提交）**：
  - `docs/design/dsh-migration-report.zh.md`：pre-step 100/200/300/350/400/500、spill 已接入、§7.1/P1/§7.2 与 R5 状态更新；
  - `docs/design/agent-workspace-context-execution-report.md`：§9 指标（20,596,658B→当前测量口径）、§13 md5(--name) token、§5/§10/§17 audio/evolution 状态、新增 §20 R5 链接；
  - `docs/guides/configuration.zh.md`：根 AGENTS.md 语义 + 显式 0 校验说明整理；
  - `docs/architecture/{routing-system,session-system}.zh.md`、`docs/guides/{routing-guide,session-guide}.zh.md`、`docs/operations/troubleshooting.zh.md`、`docs/design/dsh-context-prompt.zh.md` 对齐当前实现；
  - 新增 `docs/chess-integration.md` 并在 `docs/README.md` 索引。
- **验证证据**：`./scripts/lint-docs.sh` OK；compat 06 的文档检查项 PASS；`docs/issues/` 记录了各项证据。

## FIX-P2-07 go.mod go 指令与 tidy

- **现象**：HEAD `5da70e0b` 把 `go.mod` 的 go 指令从 `1.25.11` 升到 `1.27.3`（违反任务书"不要升级 Go 版本"），且 `go mod tidy -diff` 持续 FAIL；`go.mod` 残留未使用的 discordgo replace。
- **根因**：HEAD 单行提交；require 分组在 1.27.3 指令下被要求重排；discordgo 删除后 replace 与 `.golangci.yaml` allow-list 未清理。
- **修复（工作树，未提交）**：`go.mod` 回退 `go 1.25.11`；删除 discordgo replace；`go mod tidy` 后 `go mod tidy -diff` 空；`go mod vendor` 重新生成（vendor 无 discordgo 残留、无 replace 注解）；同步清理 `.golangci.yaml` 的 discordgo 条目。
- **验证证据**：`go mod tidy -diff` exit 0 空输出（`fg-tidy-diff.log`）；`go build ./...` PASS；`go test ./pkg/config/...` PASS；历史复现（1.27.3 下 tidy FAIL）见 `baseline/fix-verify/rv-tidy-head-repro.log`。

## FIX-R5-02 网关覆盖率补强

- **现象**：`pkg/gateway` 覆盖率 **17.7%**，启动/信号/reload 路径只有进程级 E2E 覆盖；P0 死锁恰好落在该盲区，违反"关键路径 ≥90%"验收口径。
- **根因**：`gateway.Run` 直接注册 `signal.Notify`，无法在进程内注入信号；缺少 gateway 生命周期集成测试。
- **修复（工作树，未提交）**：`pkg/gateway/gateway.go` 将 `Run` 拆为 `Run`（signal.Notify → 调用内部）与 `runGateway(..., sigChan <-chan os.Signal)`，行为/启动顺序/pid 时机不变；新增 `pkg/gateway/gateway_integration_test.go` 5 个进程内生命周期测试（启动/健康/reload/hot_reload/SIGTERM/pid 清理/allow-empty）。
- **验证证据**：gateway 覆盖率 **17.7% → 76.8%**（gateway.go 77.0%）；四包合计 74.8% → **76.8%**；`fg-coverage-4pkgs.log`、`fg-cover-func.log`；四包 `-race` 0 DATA RACE。

## FIX-R5-03 R8 loop-guard 接线测试

- **现象**：R8 复核发现 loop-guard 在 hook-respond 与批调度路径的计数语义（respond 计数、deny 不计数、批调度 observe 计入）缺少测试锁定。
- **修复（工作树，未提交）**：新增 `pkg/agent/loop_guard_wiring_test.go`：`TestExecuteToolsHookRespondLoopGuardObservation`、`TestExecuteToolsBatchLoopGuardObservation`。
- **验证证据**：`go test ./pkg/agent -run LoopGuard -count=3` PASS；功能测试报告 §14.6。

## FIX-R5-04 残余清理与文档 R10

- **现象**：可接受残留未清：`.gitignore` 仍有 `weixin/`、`web/frontend|backend`、`cmd/telegram/`、`docker/data` 忽略项；`.golangci.yaml` 有 discordgo allow-list；`vendor/modules.txt` 带已删 replace 注解；根 `.dockerignore` 引用已删构建路径；迁移报告/配置指南/执行报告有 R10 级过期表述。
- **修复（工作树，未提交）**：`.gitignore` 删除已删功能忽略项（保留通用规则）；`.golangci.yaml` 删除 discordgo 条目；删除根 `.dockerignore`；`go.mod`/vendor 清理 discordgo replace；更新 `dsh-migration-report` §0.3/§4/§7、`configuration.zh.md` §5、`agent-workspace-context-execution-report.md` §20。
- **验证证据**：`go mod tidy -diff` 空；`go build ./...` PASS；`./scripts/lint-docs.sh` OK；`git check-ignore` 复核通用规则仍生效且无意外文件变可见；vendor 无 discordgo（`vendor/modules.txt` 0 处）。

## 修复轮次说明

| 轮次 | 时间 | 内容 |
|---|---|---|
| 验证轮 | 2026-10-30 白天 | 真实场景/配置/上下文/兼容基线测试，产出缺陷清单 P0/P1/P2 |
| 修复轮 | 2026-10-30 17:31–18:30 | 修复 P0-01、P1-01~04、P2-01~06/08 并复测（live 86/0、config 42/0、compat 163/0） |
| R5 收尾 | 2026-10-30 晚 ~ 10-31 | MCP 显式关闭、R8 loop-guard、gateway 覆盖率补强、残余清理与文档 R10（config 43/0） |
| 最终门禁 | 2026-10-31 18:36–18:45 | 独立 baseline-verifier 复测 §11.1 全绿、compat 163/0、覆盖率 76.8%、性能无回归 |

## FIX-R6 MCP 2025-11-25 协议审计与 structuredContent 透传（历史）

- **现象/需求**：需确认并同步最新 MCP 协议；原实现未透传 2025-06-18+ 的 structuredContent，传输/版本注释过期。
- **结论**：vendor `go-sdk v1.6.1` 的 latestProtocolVersion 已是 `2025-11-25`（客户端 initialize 默认请求最新）；无需升级 SDK/改版本。新增协议版本常量、独立探针与 `pkg/mcp/protocol_version_test.go`；`mcp_tool.go` 透传 structuredContent（去重、大文本走 artifact/spill）；更新 transport 注释/日志。
- **验证**：`go run ./build/functest-20261030/verify/mcp-probe` → `protocolVersion=2025-11-25`；`go test ./pkg/mcp/... ./pkg/tools/integration/...` PASS；配置矩阵 `--phases D --merge` → **43/43 PASS**。
- **支持边界**：resources/prompts 工具、roots 实际值、tools/list_changed 自动刷新、tasks/sampling/elicitation 仍不支持，见 `docs/reference/mcp-support.zh.md`。
- **状态**：工作树（未提交）。


## FIX-R7-01 provider 返回 (nil,nil) 导致 nil deref panic

- **现象**：provider `Chat` 返回 `(nil,nil)` 时 `pipeline_llm.go` after-LLM 后处理解引用空 response → panic；生产 worker recover 后回合被静默丢弃（无错误事件/回复），直接调用 runTurn 的嵌入方进程崩溃。
- **根因**：重试循环仅以 `err` 判定成功；after-LLM 后处理无 nil 防御。
- **修复**：`pkg/agent/pipeline_llm.go:454-460` 把 `(nil,nil)` 归一化为 error，走统一错误路径（KindAgentError + 返回错误，不写半成品）。
- **回归测试**：`TestFaultNilProviderResponseReturnsError`（修复前实测 panic）。
- **证据**：`build/functest-20261030/analysis/agent-robustness/{review.md,pre-fix-failures.log,fix.diff}`。
- **状态**：工作树（未提交）。

## FIX-R7-02 HookActionAbortTurn 中途中止留下悬空 tool_call

- **现象**：assistant(tool_calls) 已持久化后，hook 返回 abort_turn 时 runTurn 直接返回、不回滚会话，历史留下无 tool 响应的悬空 tool_call（后续请求/压缩可能 400）；hard abort 有回滚，语义不一致。
- **根因**：hook abort 分支缺少会话回滚。
- **修复**：`pkg/agent/turn_coord.go:362-386` 新增 `restoreHookAbortedTurn`（与 hard abort 一致的会话回滚 + session_restore 事件），两个 hook-abort 分支调用。
- **回归测试**：`TestFaultHookAbortDuringToolExecutionRestoresSession`（修复前 dangling `[abort-call-1 abort-call-2]`）。
- **证据**：同上 `analysis/agent-robustness/`。
- **状态**：工作树（未提交）。


## FIX-R8 新增 DSH 独立工具组（14 工具 Go 重写）

- **背景**：DSH 中与 Chagent 功能不完全一致且对 chess 有价值的工具需迁移；功能完全一致的不迁移。
- **实现**：独立分组 `tools.dsh`（`pkg/tools/dsh/*`，PromptMetadata slot `dsh`），默认启用、整组/子组可关：
  `glob`/`grep`、`job_run`/`job_list`/`job_output`/`job_kill`、`list_mcp_resources`/`list_mcp_resource_templates`/`read_mcp_resource`、
  `session_search`/`session_trace`、`list_agents`/`wait_agent`/`interrupt_agent`、`create_goal`/`get_goal`/`update_goal`（共 14 个）。
- **安全/清理**：workspace 边界、符号链接不跟随、只读/资源上限、密钥遮罩；jobs 按进程组杀、`AgentInstance.Close`/reload 清理。
- **测试**：各子包 52/29/13/24/18/30 测试 PASS；`pkg/agent/dsh_tools_test.go` 锁定默认注册与组/子组开关。
- **证据**：[DSH 工具组](../reference/dsh-tools.zh.md)；`go test ./pkg/tools/dsh/... ./pkg/agent -run TestDSHToolGroup`。
- **状态**：工作树（未提交）。

## FIX-R9 可选子系统初始化失败导致 agent 消息循环静默退出

- **现象**：hook 可执行文件不存在或 MCP server 无法启动时，gateway 启动成功、`/health=200`，但之后所有 turn 无回复（健康但不可用）。
- **根因**：`AgentLoop.Run` 对 `ensureHooksInitialized`/`ensureMCPInitialized` 失败直接 `return err`；gateway `go agentLoop.Run(ctx)` 丢弃错误。
- **修复**：Run 对 hook/MCP 初始化失败改为 Error 日志 + 降级继续（并 defer `running=false`）；gateway 记录 Run 异常。
- **回归**：健壮性故障注入 E3（hook 启动失败）/E4（MCP 不可达）由 FAIL → **PASS**（turn 有回复、错误可见、health 200）。
- **证据**：`build/functest-20261030/robustness-live/{E3-hook-start-failure,E4-mcp-unreachable}`、[健壮性报告](../operations/robustness.zh.md) §3 P1-2。
- **状态**：工作树（未提交）。


## FIX-R10 MCP 2026-06-30 原生支持（不降级）

- **需求**：MCP 需支持 2025-11-25，且对 2026-06-30 原生支持（不降级到旧版本）。
- **实现**：新增 `third_party/mcp-go-sdk`（go-sdk v1.6.1 最小 fork，PATCH.md 记录），将
  `latestProtocolVersion` 设为 `2026-06-30` 并加入 `supportedProtocolVersions`；上游已有该常量与
  SEP-2575 标准 header 逻辑，仅未启用。`go.mod` 本地 replace 引用 fork。
- **效果**：客户端 initialize 请求 2026-06-30；服务端返回 2026-06-30 直接接受（不降级）；
  2025-11-25/2025-06-18/2025-03-26/2024-11-05 仍可协商降级；未知版本拒绝。
- **验证**：`go run ./build/functest-20261030/verify/mcp-probe` → `requested_protocol=2026-06-30`；
  `go test ./pkg/mcp/...` PASS（协议请求/降级/未知版本/structuredContent/list_changed）。
- **证据**：[MCP 支持矩阵](../reference/mcp-support.zh.md)、`third_party/mcp-go-sdk/PATCH.md`。
- **状态**：commit `90fd6547`。


## FIX-R11 session meta.json 损坏自愈（OPEN-12）

- **修复**：`pkg/memory/jsonl.go` 在 meta 解码失败时备份坏文件为 `<meta>.corrupt-<ts>`，按 JSONL 行数重建 `count`（`skip=0`）并原子写回；单测覆盖“有 JSONL / 仅 meta / 追加后仍可读”。
- **回归**：`go test ./pkg/memory -run CorruptMeta`；进程级健壮性 C 组由 2 FAIL → **37/37 PASS**。

## FIX-R12 SIGKILL 时 exec 子进程孤儿（OPEN-13）

- **修复**：新增父死看门狗 `pkg/tools/orphan_guard_unix.go`：exec 子进程组外挂一个独立 shell 看门狗，父进程持有管道写端；gateway 被 SIGKILL（内核关闭写端）后看门狗立即 TERM/KILL 目标进程组并退出。同步/后台 exec 均接入，会话完成/kill 时释放。
- **证据**：`TestOrphanGuardKillsProcessGroupOnParentDeath`；进程级健壮性 A 组由 1 FAIL → **18/18 PASS**（无 sleep 孤儿）。

## FIX-R13 日志目录只读时 panic 退出（OPEN-15）

- **修复**：`pkg/gateway/gateway.go` 在 `EnableFileLogging` 失败时不再 `logger.Fatal`，改为 stderr 告警 + Warn 日志并继续以控制台日志运行（不 panic、不退出）。
- **回归**：进程级 F 组 **9/9 PASS**。

## FIX-R14 大参数错误可诊断性 + fallback 成功日志（OPEN-18）

- **修复**：`pkg/tools/normalization.go` 对 `IsError` 结果不做 large-base64 替换，保留错误头 8KiB + 尾 4KiB（含 “file name too long” 等原因）并标注截断；`pkg/providers/fallback.go` 在 fallback 成功路径记录 Info `Fallback: succeeded`。
- **回归**：B4 由 FAIL → **PASS**（模型可见原因）；`TestNormalizeToolResult_*` 单测；A8 的 harness 检查受候选顺序影响（首候选健康时不构成 failover），已在健壮性报告注明。

## FIX-R15 background jobs stale 清理 + MCP tools/list_changed 重注册（OPEN-17 部分）

- **修复**：jobs Manager 启动时清理上一进程遗留的 running/stopping job 目录（pid 仍存活的保留）；MCP `tools/list_changed` 回调接入 agent 层，动态注册新工具并用新增的 `ToolRegistry.Unregister` 移除已消失工具。
- **证据**：`pkg/tools/dsh/jobs` 测试；`pkg/mcp/tool_list_changed_test.go`；agent 构建/测试通过。

## FIX-R16 负 usage / 负迭代上限 / session.Save 失败可观测性（OPEN-16）

- **修复**：`SetLastUsage` 对负计数清零并拷贝；`MaxToolIterations<0` 钳制为 1 并 Warn；工具交付路径 `session.Save` 失败改为 Error 日志 + `KindAgentError` 事件。
- **回归**：`TestSetLastUsageSanitizesNegativeCounters`、`TestNegativeMaxToolIterationsClampedToOne`；全量 `go test ./pkg/...` 通过。


## FIX-R17 异步 summarize 与 durable context 竞态（OPEN-04）

- **根因**：异步 summarize 在秒级 LLM 调用期间，durable context 注入/压缩并发 append/rewrite session；摘要落盘用旧快照 `keepCount` 调 `TruncateHistory`，静默多丢保留尾，`SetSummary` 还可能覆盖并发压缩注记。
- **修复**：`pkg/agent/context_legacy.go` 增加 `writeMu` 串行化压缩/摘要落盘；摘要落盘 `applySummaryIfUnchanged` 重读后对 summary 与稳定前缀做 CAS，仅允许尾部追加并按 `kept = keepCount + appended` 平移截断点；CAS 失败放弃（更新写者胜出，下回合重试，不丢数据）。
- **回归**：`pkg/agent/context_race_open04_test.go` 3 个确定性用例（修复前 FAIL、修复后 PASS）；`go test ./pkg/agent -count=1` + 相关 `-race` PASS。
- **证据**：`build/functest-20261030/analysis/open04/`（review.md、fix.diff、pre/post 日志）。
- **状态**：工作树（本轮提交）。

## FIX-R18 MCP stdio 子进程生命周期 + live MCP 阶段（OPEN-07/08/17）

- **根因**：`pkg/mcp` stdio 使用 `exec.CommandContext(ctx)`，ctx 来自 reload/turn 请求；请求结束 ctx 取消 → MCP server 被杀，靠重连兜底，动态 tools/list_changed 不稳定。
- **修复**：stdio 改用 `context.WithoutCancel(ctx)` 派生进程 ctx，`procCancel` 存入 isolated transport，`Close()` 时 cancel（保留 SIGTERM/SIGKILL 兜底）；新增 `pkg/mcp/stdio_lifecycle_test.go`（connect ctx 取消后 session 仍可调用、Close 后退出、reload ctx 场景）。
- **live E2E**：`scripts/agent-e2e-scenario.py` 新增 P11 MCP 阶段（真实模型，PASS=24/0）：servers-only 自动启用、2026-06-30 协商、真实工具调用、运行期 add/remove 动态重注册、显式 `enabled=false` reload 关闭；fixture 支持 2026-06-30 与 `CFGCAP_MCP_CONTROL_FILE` 动态控制。
- **证据**：`build/functest-20261030/live/p11-mcp/`；`go test ./pkg/mcp/...` PASS。
- **状态**：工作树（本轮提交）。
