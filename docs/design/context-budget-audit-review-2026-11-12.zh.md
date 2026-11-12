# 上下文预算审计复核（2026-11-12）

**审计对象**: `docs/design/context-budget-audit.zh.md`
**范围**: chagent 上下文计量、预算、压缩、提醒、配置默认值、真实 JSONL
**方式**: 只读源码 + 单测 + 51 个历史会话 JSONL 复算；未修改运行代码

## 1. 复核结论

原审计的**机制描述基本正确**，但存在三处关键偏差：

1. 把「回合内长到 1.7× 窗口」当作主因，不能解释 `mcp-gateway-1014`：该会话**第 1 条消息**历史仅 13 token，提醒已为 56683/24576（231%），固定开销约 56.6K。
2. 98304 同时是提醒/硬压缩/摘要阈值的结论只在 `max_tokens = 0.25×context_window` 时成立。`max_tokens ≥ 0.2×context_window` 才满足“提醒 100% == 硬阈值”；否则提醒 100% 可能仍未越过硬阈值。
3. 文档 §2.3 的 169% 是 `used/window`；按实际请求还应加输出预留，221693+32768=254461，约为 131072 的 **194%**。

## 2. 已证实根因

### P0-1 固定开销型超限

- 提醒使用全量工具定义（`pkg/agent/prestep.go:274-277`），硬检查也包含 tools（`pkg/agent/context_budget.go:120-135`）。
- 压缩层 `isHistoryOverBudget` 只计算 history+summary，tools 传 `nil`（`pkg/agent/context_legacy.go:221-232`），会产生 **假 fit**；回到 setup 再用含 tools 的口径重算，仍超限，继续删历史。
- 工具 schema + system 是固定开销，压缩只删回合/工具结果，**动不了固定开销**。
- 实测：
  - `mcp-gateway-1014` 首条提醒 56683，历史估算 13，固定开销约 56.6K；
  - `docs-curator-1012` 两次提醒差值稳定 ≈4574，证明 setup 提醒为纯启发式、固定开销稳定。

### P0-2 硬守卫不硬

- setup 压缩后仍 `fit=false` 只记录日志，继续发送（`pkg/agent/pipeline_setup.go:118-126`）。
- `trimHistoryToFitContextWindow` 无安全切点时返回空历史 + `false`（`pkg/agent/context_budget.go:153-167`），调用方仍发送；测试已锁定该行为。
- 结果可能是静默清空历史后继续请求，而不是阻断或降级工具。

### P0-3 窗口靠猜

- 未配置 `context_window` 时 `max_tokens×4`（`pkg/agent/instance.go:274-282`）。
- `pkg/config/config.go` 的 `ModelConfig`/`AgentConfig` 没有 `context_window` 覆盖；无法按模型/Agent 配置。
- 无 `max_tokens < context_window` 校验；极端配置会让提醒预算被钳为 1 token（`context_meter.go:129-137`）。

### P1-4 回合内无预算检查 + 工具结果无上界

- `Compact` 仅 `pipeline_setup.go:65`（回合开始）与 `pipeline_llm.go:368`（provider 溢出重试）；回合内 prestep 只有提醒（`prestep.go:266-284`）。
- 工具结果没有执行期内联上限，pruner 只在强制压缩中运行（`context_legacy.go:148`）；单条大输出可直接跨越预算。

### P1-5 provider 溢出兜底不可靠

- `pipeline_llm.go:298-309` 用英文字符串匹配；中文错误、413 等可能不触发。
- `pipeline_llm.go:287-289` 的 streaming visible error 在上下文判定前 break。
- 重试压缩仍 `!fit` 时直接失败（`pipeline_llm.go:451-457`）。

### P2-6 多套“已用”口径

| 口径 | 组成 |
|---|---|
| 提醒 | messages + 全量 tools，不含输出预留 |
| 硬检查 | 过滤后 tools + max_tokens |
| `/context` | 持久历史 + 全量 tools + system，百分比钳 100 |
| 压缩层 | history + summary，tools=nil |

### P2-7 提醒去重非单调

去重只记最后一个 band（`context_meter.go:197-200`）；0.95 压缩回落到 0.86 会再注入 0.85 提醒；每 turn 重置，提醒永久落盘，长期会话线性增长。

### P2-8 文档默认值错误

- `docs/guides/configuration.zh.md:69` 写 `max_tokens=8192`，代码默认 32768（`pkg/config/defaults.go:31`）；
- `:71` steering_mode 写 one-at-a-time，代码默认 `all`（`defaults.go:36`）；
- `:72` max_parallel_turns 写 0，代码默认 20（`defaults.go:37`）。

## 3. 修复顺序

### P0（先做）

1. **固定开销进入压缩决策**：压缩预算必须包含 tools/system；固定开销 > 预算时，先降级工具（MCP/大 schema/按 turn profile 裁剪），仍超限则明确报错，禁止静默清空历史。
2. **硬守卫**：`fit=false` 不得继续发送；返回明确错误或降级工具重试，并记录事件。
3. **窗口配置化**：新增 per-model/per-agent `context_window`；启动校验 `0 < max_tokens < context_window`；文档给出真实模型窗口配置示例。

### P1

4. pre-step 预算守卫（`ContextPressurePolicy.Threshold(0.8)`），每回合最多 1–2 次主动压缩，超阈值重建 `callMessages`。
5. 工具结果执行期上限/Spill，不等压缩。
6. 统一溢出错误分类：状态码 400/413 + 中英文关键字；调整 streaming visible 判定顺序。
7. 单 turn 压缩安全切点改为 tool-pairing，保留当前回合尾部。

### P2

8. 统一 `used` 计算函数（history/tools/system/输出预留同一口径），百分比一致。
9. band digest 单调（只升不降），或每会话提醒上限。
10. 修正配置文档默认值；补充 `省略`/`0`/显式值语义与校验告警。

## 4. 默认模型上下文设置

### 已确认数据

- `deepseek-v4.1-flash`：model-relay 调用记录 **#7371** 成功处理
  `prompt_tokens=235406`、`max_tokens=32768`，说明真实窗口至少能容纳
  `235406 + 32768 = 268174`；按 DeepSeek 当前上下文能力登记窗口 **1048576**。
- `glm-5.3`：运行态 `/context` 记录
  `total_tokens=524000`、`compress_at_tokens=393000`、
  `max_tokens=131000`，按官方参数登记窗口 **1048576**、输出 **131072**。

### 代码设置

`pkg/config/model_capabilities.go` 新增已知模型能力；`pkg/agent/instance.go`
在初始化时应用，`pkg/agent/agent_command.go` 在 `/model` 切换时同步更新：

| 模型 | context_window | max_tokens |
|---|---:|---:|
| `glm-5.3`（含 `-admin` 别名） | 1048576 | 131072 |
| `deepseek-v4.1-flash` | 1048576 | 32768 |

配置显式值优先于内置能力；未登记模型继续使用全局配置或保守启发式。
`ModelConfig` 同时支持 `context_window` / `max_tokens` 字段，便于后续按模型覆盖。

## 4. 配置止血建议

- 每个工作区显式配置模型真实 `context_window` 与真实 `max_tokens`；
- 建议 `max_tokens ≤ 0.2 × context_window`，保留合理余量；
- MCP 较重的工作区先减少 server / 开启 discovery / 使用 turn profile 裁剪工具；
- 大文件任务避免一次性读取，等待 P1 执行期上限修复。

## 5. 验证

```bash
cd /Users/wudaorui/code/chess/chagent
go test ./pkg/agent -run 'TestContextPromptBudget|TestContextPressureNotice' -count=1  # PASS
go test ./pkg/agent -count=1                                                          # PASS
```

本机 JSONL 复算覆盖 51 个会话，未找到 221693 原文；该数字的机制属推断，但固定开销型与单条大工具结果两条路径均可由现有代码产生。

## 6. 与原文档的关系

原文档保留为排查记录；本复核修正其 §2.4 的主因判断、§2.2 的阈值重合条件与 §2.3 的百分比口径。后续修复应以本复核的 P0 顺序执行。
