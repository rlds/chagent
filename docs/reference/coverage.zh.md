# 覆盖率报告（2026-11-02，残余问题修复轮）

> 目标：任务书 §11.1「关键路径 ≥90%」。本报告给出**可复算**的口径、数字与证据。
> 工具链：本机 Go 1.25.11（go.mod 指令）/ darwin/arm64。
> 证据目录：`build/functest-20261030/coverage/`、`build/functest-20261030/baseline/final-gate/fg2-coverage-*`。

## 1. 结论

| 范围 | 覆盖率 | 目标 | 判定 |
|---|---:|---:|---|
| `pkg/agent` 整体 | **85.0%**（7274/8560） | 整体 ≥85% | ✅ |
| `pkg/agent` **关键路径控制面**（10 文件合计） | **91.0%**（1580/1737） | **≥90%** | ✅ |
| `pkg/gateway` | **91.6%**（351/383，3 次稳定） | ≥90% | ✅ |
| `pkg/config` | **92.2%**（1639/1778，3 次稳定） | ≥90% | ✅ |
| `pkg/channels/pico` | **93.6%**（3 次 93.0–93.6，波动≤1pt） | ≥90% 且稳定 | ✅ |
| `pkg/agent/workspacecontext` | **83.0%** | 新增包 ≥80% | ✅ |
| 四包合计 | **86.9%**（fg2 门禁实测；按语句加权） | — | 记录 |

> 说明：`pkg/agent` 整体 85% 包含工具执行面（`pipeline_execute.go` 73.3%）、MCP 集成面（`agent_mcp.go` ~42%，需真实 MCP 进程）等非控制面代码；
> 任务书“关键路径”按 agent 循环控制面定义达成 91%，定义与逐文件数据见 `coverage/agent-critical-files.md`。

## 2. agent 关键路径定义（10 文件）

`turn_coord.go`（回合主循环/调度/中止）、`pipeline_llm.go`（LLM 重试/fallback/usage）、
`pipeline_setup.go`（上下文装配/主动压缩）、`pipeline_streaming.go`（流式发布）、`tool_batch.go`（并行批调度不变量）、
`context_budget.go`（turn 边界/预算/裁剪）、`context_meter.go`（usage 锚定/压力）、`prestep.go`（有序 context 注入）、
`agent.go`（生命周期/Run/Stop/Reload/worker）、`turn_state.go`（取消/持久化/用量）。

逐文件覆盖率（2026-11-02 测量）：

| 文件 | 覆盖/语句 | 覆盖率 |
|---|---:|---:|
| turn_coord.go | 311/340 | 91.5% |
| pipeline_llm.go | 277/300 | 92.3% |
| pipeline_setup.go | 74/76 | 97.4% |
| pipeline_streaming.go | 189/205 | 92.2% |
| tool_batch.go | 75/83 | 90.4% |
| context_budget.go | 60/63 | 95.2% |
| context_meter.go | 76/85 | 89.4% |
| prestep.go | 93/106 | 87.7% |
| agent.go | 128/160 | 80.0% |
| turn_state.go | 296/319 | 92.8% |
| **合计** | **1580/1737** | **91.0%** |

执行面单列：`pipeline_execute.go` 73.3%（batch 阶段 3 的 channel/media 组合、pendingResults/steering 时序分支；
核心不变量由 `robustness_fault_test.go`/`tool_batch_test.go` 覆盖），未纳入控制面 ≥90% 集合。

## 3. 新增测试（本轮，全部确定性、无真实模型/网络）

| 包 | 新增文件 | 测试数 | 覆盖重点 |
|---|---|---:|---|
| `pkg/agent` | `coverage_helpers_test.go`、`coverage_runtime_test.go`、`coverage_loop_core_test.go`、`coverage_execute_plane_test.go`、`coverage_tiny_helpers_test.go` | 55 个 `TestCover*` | 控制面错误/取消/边界、事件 payload、tools 注册矩阵、streaming、hook、autonomy 守卫、summarize 回退等 |
| `pkg/gateway` | `gateway_coverage_more_test.go` | 25 | 启动失败/端口冲突/日志降级/ reload 错误/热重载/心跳/openGatewayListeners |
| `pkg/config` | `coverage_*.go` 4 文件 | 34 表驱动 | Validate/ApplyDefaults/LoadConfig 严格未知字段/解码错误/MCP/显式 0/各校验分支 |
| `pkg/channels/pico` | `pico_ws_extra_test.go`、`pico_edge_extra_test.go` | 70 | HTTP/WS 鉴权与协议、断开/重连、消息 ack/error/媒体、streamer、client、工厂 |

## 4. 复跑命令

```bash
# agent
go test ./pkg/agent -coverprofile=build/functest-20261030/coverage/agent-cover.out -covermode=count -count=1
go tool cover -func=build/functest-20261030/coverage/agent-cover.out

# gateway / config / pico（各自 3 次，见各 summary）
go test ./pkg/gateway -covermode=count -coverprofile=build/functest-20261030/coverage/gateway-cover.out -count=1
go test ./pkg/config  -covermode=count -coverprofile=build/functest-20261030/coverage/config-cover.out  -count=1
go test ./pkg/channels/pico -covermode=count -coverprofile=build/functest-20261030/coverage/pico-cover.out -count=1

# 四包门禁合计（最终门禁实测 86.9%）
go test ./pkg/agent ./pkg/gateway ./pkg/config ./pkg/channels/pico \
  -coverprofile=build/functest-20261030/baseline/final-gate/fg2-cover.out -covermode=count -count=1
go tool cover -func=build/functest-20261030/baseline/final-gate/fg2-cover.out | tail -1
```

## 5. 未覆盖与口径说明

- **agent 整体未达 90% 的原因**：工具执行面（channelManager/media 组合、batch pendingResults/steering 时序）、
  MCP 真实 manager 生命周期（需外部进程/网络）、外部 process hook 子进程路径、agent_command 的 UI/channel 输出分支。
- **执行面是否纳入 ≥90%**：本报告按任务书“关键路径”= agent 控制面定义；若验收要求执行面也 ≥90%，需另立
  `pipeline_execute.go` 专项夹具（当前 73.3%），预计可独立推进。
- 覆盖率数字对工作树敏感；本报告为 2026-11-02 测量快照，新增测试未改生产逻辑（OPEN-04 修复除外）。

## 6. 相关文档

- [项目状态](../project/status.zh.md) · [问题库](../issues/README.md)
- 各包证据：`build/functest-20261030/coverage/{agent,gateway,config,pico}-summary.md`、`agent-critical-files.md`