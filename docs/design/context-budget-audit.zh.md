# 上下文预算与 `<context_pressure>` 超限排查记录

> **2026-11-12 复核修订**：本记录 §2.4 的「回合内长到高水位」主因判断对 `mcp-gateway-1014` 记录不成立（首条消息固定开销约 56.6K）；§2.2 的「提醒 100% == 硬阈值」仅在 `max_tokens ≥ 0.2×context_window` 时成立；§2.3 的 169% 未含输出预留。最新根因与修复顺序见 [context-budget-audit-review-2026-11-12.zh.md](context-budget-audit-review-2026-11-12.zh.md)。
>
> 状态：**排查完成；2026-11-12 已复核修订，修复方案待排期**
> 日期：2026-11-12（Asia/Shanghai）
> 触发：历史 agent 会话出现模型可见提示「上下文使用约 221693 tokens（预算约 98304，已用 226%）」
> 排查方式：只读源码 + 单元测试 + 本机真实会话 JSONL 检索；本轮未跑真实模型
> 关联文档：[上下文处理 vs DSH](../architecture/context-vs-dsh.zh.md)、[配置](../guides/configuration.zh.md)、[模型可见文本](../reference/model-facing-text.zh.md)、[会话系统](../architecture/session-system.zh.md)

---

## 0. 结论摘要

| # | 结论 | 证据 |
|---|---|---|
| 1 | 该提示是 chagent 自有的 `<context_pressure>` 临近上限提醒（DSH 无此机制），只提醒、不触发压缩；同一 band 每 turn 最多一条 | `pkg/agent/context_meter.go:160-169`、`pkg/agent/context_meter.go:172-209`、`pkg/agent/prestep.go:266-294` |
| 2 | 「预算约 98304」不是模型窗口，而是**软提醒预算**：默认 `max_tokens=32768`、`context_window` 未配时按 `×4` 猜成 131072，则 `min(0.8×131072, 131072−32768)=98304` | `pkg/config/defaults.go:31`、`pkg/agent/instance.go:274-282`、`pkg/agent/context_meter.go:116-140` |
| 3 | 默认配置下，98304 同时是**硬压缩阈值**与**摘要阈值**（三者重合），所以提醒一旦 >100%，请求已经越过硬阈值；226%（221693 tokens）≈ **1.69× 假设窗口** | `pkg/agent/context_budget.go:120-134`、`pkg/config/defaults.go:34` |
| 4 | 超限主因是**回合内没有压缩触发点**：压缩只在回合开始的 `SetupTurn` 与 provider 报 context 溢出后的重试里发生；回合内多轮工具调用期间请求可以单调增长到远超预算 | `pkg/agent/pipeline_setup.go:61-128`、`pkg/agent/pipeline_llm.go:344-451` |
| 5 | **需要调整，但不是调大默认预算**：应显式配置模型真实窗口/输出上限，并把压缩触发下沉到回合内每步（0.8 提前）+ 补单 turn 压缩切点与工具结果时点裁剪 | 见 §6 |
| 6 | 已用单测固化公式与提醒行为：`go test ./pkg/agent -run 'TestContextPromptBudget\|TestContextPressureNotice' -count=1` → `ok` | 本轮执行记录 |

一句话：**226% 不是参数太小，而是「窗口取值靠猜 + 压缩触发太晚（只在回合边界）」的叠加症状；调大默认值只会掩盖，不会修复。**

---

## 1. 排查范围与方法

### 1.1 代码阅读清单

| 文件 | 关注点 |
|---|---|
| `pkg/agent/context_meter.go` | 提醒预算公式、锚点校正、band 去重、提示文案 |
| `pkg/agent/context_usage.go` | `/context` 口径（已用/压缩阈值/摘要阈值，百分比钳制） |
| `pkg/agent/context_budget.go` | 硬预算判定 `isOverContextBudget`、按 turn 边界裁剪 |
| `pkg/agent/context_legacy.go` | 强制压缩：pruner + spill + 丢弃最旧约 50% 回合 |
| `pkg/agent/context_pressure.go` | DSH 压力策略纯函数（0.8 / 0.16 / 65536）及接线状态 |
| `pkg/agent/prestep.go` | 提醒注入位置（order 350）与注入顺序 |
| `pkg/agent/pipeline_setup.go` | 回合开始的主动压缩与提醒首注 |
| `pkg/agent/pipeline_llm.go` | 只有 provider 溢出错误才触发的重试压缩 |
| `pkg/agent/tool_result_prune.go` | 工具结果中段裁剪（仅在强制压缩内调用） |
| `pkg/config/defaults.go`、`pkg/agent/instance.go` | `max_tokens` / `context_window` 默认值与回退 |
| `pkg/config/config_loader.go` | 默认值合并语义（省略 vs 显式 0） |
| `pkg/tokenizer/estimator.go` | CJK/ASCII 加权启发式估算 |

### 1.2 实际执行

- 单元测试：`go test ./pkg/agent -run 'TestContextPromptBudget|TestContextPressureNotice' -count=1` → PASS（公式 104857/24576 与 band 行为由测试锁定）。
- 真实会话检索：在 `~/chessdata/*/`（chess 侧 chagent agent 工作区）与 `~/.chagent` 中检索提示原文与数字，命中两个工作区的同机制记录（§2.4）。
- 文档对照：`docs/guides/configuration.zh.md`、`docs/architecture/context-vs-dsh.zh.md`、`docs/reference/model-facing-text.zh.md`。
- 未做：真实模型复跑、provider 侧 usage 抓包（建议见 §7）。

---

## 2. 现象与数字反推

### 2.1 提示原文

```
<context_pressure>
上下文使用约 221693 tokens（预算约 98304，已用 226%）。
优先完成当前子任务，把关键结论写入任务状态或文件；避免重复读取大文件；继续前先总结已完成内容。
</context_pressure>
```

这是 `providers.Message{Role: "user"}` 形态的持久上下文消息，kind=`context-pressure`，由 pre-step 注入并落盘（`pkg/agent/prestep.go:278-293`）。它**不是用户输入**，也**不阻塞调用**。

### 2.2 「预算约 98304」的来历

```text
默认 max_tokens           = 32768              (pkg/config/defaults.go:31)
未配 context_window       = max_tokens × 4      (pkg/agent/instance.go:274-282)
                          = 131072
提醒预算 budget           = min(0.8 × window, window − maxTokens)   (pkg/agent/context_meter.go:116-140)
                          = min(104857, 98304) = 98304
```

默认配置下三个阈值恰好重合：

| 阈值 | 公式 | 默认值 |
|---|---|---|
| 硬压缩触发（提示词侧） | `window − maxTokens` | **98304** |
| 提醒预算 | `min(0.8×window, window−maxTokens)` | **98304** |
| 摘要阈值（仅 history） | `window × summarize_token_percent(75%)` | **98304** |

> 重要推论：**提醒 100% == 硬压缩阈值**；提醒 >100% 意味着提示词已越过硬预算仍在发送。

### 2.3 226% 的换算

- `used = 221693`，`budget = 98304` → `ratio = 2.2552` → `percent = int(225.52+0.5) = 226%`（`pkg/agent/context_meter.go:161`）。
- 相对硬窗口 131072：`221693 / 131072 = 169%`。
- `used` 不是 provider 直接回报值，而是「启发式估算 × 最近一次 usage 锚点校正」：校正比例钳制在 `[0.25, 4]`，且**不低于启发式估算**（`pkg/agent/context_meter.go:45-60, 144-157`）。因此 221693 至少等于启发式估算，偏保守而非偏小。

### 2.4 本机历史记录对照

| 来源 | 配置 | 提示数字 | 反推窗口 | used/window | used/budget |
|---|---|---|---|---|---|
| 本次报告 | 推断为默认/样例配置（`max_tokens=32768`，窗口 131072） | 221693 / 98304 | 131072 | **169%** | **226%** |
| `mcp-gateway-1014` 会话 | `max_tokens=8192`，未配 `context_window` | 56683、56770 / 24576（2 次） | 32768 | **173%** | **231%** |
| `docs-curator-1012` 会话 | `max_tokens=8192`，未配 `context_window` | 22350 / 24576 | 32768 | 68% | 91%（正常档位） |
| 同上 | 同上 | 23494 / 24576 | 32768 | 72% | 96%（正常档位） |

会话文件位置（本机）：
- `~/chessdata/mcp-gateway-1014/mcp-gateway-1014/sessions/sk_v1_4482d1d5…jsonl`（第 2、5 条消息）
- `~/chessdata/docs-curator-1012/docs-curator-1012/sessions/sk_v1_4482d1d5…jsonl`（第 23、26 条消息）

**规律**：两次超过 100% 的记录都落在 **≈1.7× 窗口**，明显高于正常的 0.85/0.95 档位；这不是单次估算噪声，而是系统性的「回合内长到很高水位」。同时 `~/chessdata/weichat/weichat/config.json` 正是 `max_tokens=32768` 且未配 `context_window`，与 98304 的预算完全对应。

### 2.5 证据边界（必须说明）

1. **221693 原文未落盘**：本机 `~/chessdata`、`~/.chagent`、`~/code` 没有历史会话命中 `221693`；唯一命中是本次排查会话自身的 projcache 请求文本。本机找到的是同机制、不同数字的两条记录，结论基于代码推导 + 同机制记录，不依赖该原文。
2. **窗口值 131072 是推断**：`budget = min(0.8×window, window−maxTokens) = 98304` 不能唯一确定窗口；结合默认值与 `config/config.example.json:18` 的 `131072/32768`，以及 weichat 配置，取 131072 为最可能值。若该 agent 用了自定义配置，请以它的 `config.json` 或 `/context` 输出为准。
3. **used 的含义**：若首调用无锚点，则为纯启发式估算；有锚点则为「估算 × provider 校正」。两种情况下该值都只是估算，不等同于 provider 账单 token。

---

## 3. 机制说明（当前实现）

### 3.1 上下文链路

```text
session JSONL + meta.summary
  → ContextManager.Assemble（AssembleRequest{Budget, MaxTokens}）        pkg/agent/pipeline_setup.go:22-30
  → 注入 durable context（workspace / runtime / task-state / skill-catalog） pkg/agent/pipeline_setup.go:32-42
  → BuildMessagesFromPrompt（system + tools + history + 当前用户消息）
  → isOverContextBudget? → Compact（仅回合开始检查一次）                 pkg/agent/pipeline_setup.go:61-128
  → injectContextPressureNotice（order 350，首注）                        pkg/agent/pipeline_setup.go:146
  → 每步 pre-step：runtime / task-state / pressure / skill-catalog        pkg/agent/prestep.go:55-80
  → callLLM
      ├─ 成功：usage 锚点记录（recordUsageAnchor）
      └─ provider 报 context 错误：Compact(retry) → trim 重建 → 重试     pkg/agent/pipeline_llm.go:344-451
  → 回合结束：异步摘要 maybeSummarize
```

### 3.2 压缩触发点（关键）

| 触发点 | 位置 | 触发条件 | 是否覆盖回合内 |
|---|---|---|---|
| 回合开始主动压缩 | `pkg/agent/pipeline_setup.go:61-128` | `messages + tools + maxTokens > contextWindow` | ❌ 仅一次 |
| provider 溢出重试 | `pkg/agent/pipeline_llm.go:344-451` | 错误被判定为 context 错误且还有重试次数 | ✅ 但依赖 provider 先报错 |
| 回合后摘要 | 回合结束链路（`pkg/agent/pipeline_execute.go` / `context_legacy.go:84-118`） | history 消息数 / token 超阈值 | 仅影响下回合 |
| pre-step 提醒 | `pkg/agent/prestep.go:264-294` | 跨 0.85 / 0.95 档 | **只提醒，不压缩** |

`SetupTurn` 压缩后若 `trimHistoryToFitContextWindow` 仍 `fit=false`，只打日志 `still_overlimit` 并继续发送（`pkg/agent/pipeline_setup.go:118-128`）；重试路径才在 `!fit` 时报错终止（`pkg/agent/pipeline_llm.go:451-456`）。

### 3.3 计量与锚点

- 估算：CJK 约 1.3 字符/token，ASCII 约 2.5；`EstimateMessageTokens` 覆盖 Content、ReasoningContent、ToolCalls 参数与 ID、Media（`pkg/tokenizer/estimator.go`）。
- 锚点：每次成功响应后用 provider 的 `PromptTokens` 与本次请求的启发式估算求比例，后续按比例校正（`pkg/agent/context_meter.go:45-60, 144-157`）；每回合状态，跨回合不继承（首调用无锚点属预期）。
- 口径差异：提醒使用**全量**工具定义（`pkg/agent/prestep.go:274-276`），硬检查使用 turn profile **过滤后**的工具定义（`pkg/agent/pipeline_setup.go:61`）；`/context` 把已用百分比钳到 100（`pkg/agent/context_usage.go:77`），提醒不钳制。

### 3.4 提醒注入与去重

- 档位：`0.85 / 0.95`（`pkg/agent/context_meter.go:33`）；跨过某档才提示，同档每 turn 最多一条（digest 去重，`pkg/agent/context_meter.go:189-209`）。
- 注入时机：回合开始一次（`pipeline_setup.go:146`，修复 D5 的「单次 LLM 调用回合不提示」）＋ 每步 pre-step（`prestep.go:69-72`）。
- 持久化：以 `kind=context-pressure` 的上下文消息落盘，不是真实用户回合；`supersedeContextMessages` 不处理该 kind，因此提醒会随历史保留，直到压缩丢弃所在回合。

---

## 4. 默认值清单（代码 vs 文档）

| 项 | 代码默认 | 文档/依据 | 备注 |
|---|---|---|---|
| `agents.defaults.max_tokens` | **32768**（`pkg/config/defaults.go:31`） | 配置指南写 **8192**（`docs/guides/configuration.zh.md:69`） | **不一致** |
| `max_tokens` 显式写 0 | 8192 回退（`pkg/agent/instance.go:269-272`） | — | 与“省略=32768”不同 |
| `agents.defaults.context_window` | 0 → `max_tokens×4` = **131072**（`pkg/agent/instance.go:274-282`） | 配置指南同（`configuration.zh.md:70`） | 无 provider 模型窗口元数据 |
| 配置样例 | `context_window: 131072`、`max_tokens: 32768`（`config/config.example.json:18`） | — | 与默认推导一致 |
| 硬压缩触发 | prompt+tools+maxTokens > window | `pkg/agent/context_budget.go:120-134` | 默认等价于 prompt > 98304 |
| 提醒预算 | `min(0.8×window, window−maxTokens)` | `pkg/agent/context_meter.go:116-140` | 默认 98304 |
| 摘要阈值 | `window × 75%` | `pkg/config/defaults.go:34`、`instance.go` | 默认 98304 |
| `max_tool_iterations` | 300（`defaults.go:33`） | 配置指南 | 长回合放大了超限空间 |
| 压力策略（0.8/0.16/65536） | 已实现纯函数，**未接线**到压缩触发 | `pkg/agent/context_pressure.go:42-122` | 见 context-vs-dsh C4/C5 |

文档不一致会直接造成排障困惑：看到 98304 预算时，用户按 8192 默认值无法推回 131072 窗口。

---

## 5. 根因分析

### R1（主因，高）回合内没有预算检查/压缩触发点

- 事实：`Compact` 的调用点只有两处——`pipeline_setup.go:63`（回合开始）与 `pipeline_llm.go:368`（provider 溢出重试）；pre-step 每步只注入提醒。
- 后果：一个回合内多次工具调用（`max_tool_iterations=300`）期间，`exec.callMessages` 会持续追加工具结果并重建，**prompt 单调增长**，只有 provider 报错才收缩。
- 佐证：本机可见的超限记录（mcp-gateway）停在 1.73× 窗口，本次报告按推断窗口计算为 1.69×，两者形态一致（§2.4）。

### R2（高）`context_window` 是启发式猜测，未读取模型真实窗口

- 事实：`pkg/providers` 无模型窗口元数据接口（`GetContextWindow` 仅存在于测试 mock）；默认按输出预留 4× 推导。
- 两种后果：
  - 真实窗口 **大于** 假设窗口（如 200K/256K）：221693 可能只占真实窗口的 85–111%，provider 不报错 → 永不压缩；但按 131072 算显示 169% / 226%，观感严重失真。
  - 真实窗口 **小于等于** 假设窗口：请求已超窗，只能靠 provider 报错后的重试压缩兜底，单次成本与失败风险都高。

### R3（中）单 turn 压缩切点 + pruner 只在压缩时运行

- 压缩切点只认 turn 边界（`isTurnBoundary`），单回合多工具时无法安全切分，只能 pruner 或整轮丢弃（`context-vs-dsh.zh.md` C1）。
- 工具结果 pruner 只在 `forceCompression` 内调用（`pkg/agent/context_legacy.go:126-215`），不是每次请求前；超大工具输出会先原样进入至少一次请求（C8）。

### R4（中）压缩失败后仍继续发送

- `SetupTurn` 在 `trimHistoryToFitContextWindow` 仍不 fit 时只记录 `still_overlimit`，不阻断（`pipeline_setup.go:118-128`）。这保证任务不中断，但可能产生「>100% 提醒已注入、请求仍照发」的观感。

### R5（低-中）计量口径与估算偏差

- 提醒值经锚点校正（比例上限 4×），且不低于启发式估算；提醒与硬检查的工具集口径不同；`/context` 与提醒的百分比口径不同（一个钳 100，一个不钳）。
- 影响：百分比不是账单值，不能按「226% = 超模型上限 2.26 倍」理解；只能用于相对预算判断。

### R6（低）文档与默认值不一致

- `max_tokens` 文档 8192 vs 代码 32768；省略与显式 0 的语义不同；样例配置重复了 131072/32768 组合。排障时容易误判预算来源。

---

## 6. 是否需要调整

### 6.1 结论

**需要调整，但方向不是「把默认预算调大」。** 调大预算只会让提醒数字变小、压缩更晚、峰值成本更高；真正要改的是：

1. **窗口取值真实化**（配置层，立即生效）；
2. **压缩触发下沉到回合内每步**（代码层，P1）；
3. **补单 turn 压缩切点与请求前裁剪**（代码层，P1/P2）。

### 6.2 配置建议（不改代码，先做）

原则：`context_window` 必须等于模型真实上下文窗口；`max_tokens` 取模型真实输出上限，而不是固定 32768。

| 真实窗口 | 建议 `context_window` | 建议 `max_tokens` | 硬触发水位（prompt） | 提醒预算（0.8 档） |
|---|---|---|---|---|
| 128K | 131072 | 8192–16384 | 114688–122880 | 104857 |
| 200K | 200000 | 16384 | 183616 | 160000 |
| 256K | 262144 | 16384–32768 | 229376–245760 | 209715 |

- 若本次 221693 发生在 256K 窗口模型上：配成 262144 后，同一条提示会显示约 **106%**（相对 0.8 档预算），而不是 226%；真实占用为窗口的 84.5%。
- `max_tokens=32768` 对多数模型偏大：白占 1/4 窗口，并把提醒/压缩/摘要一起压在低水位。
- 需要更早摘要时下调 `summarize_token_percent`（默认 75，只对 history 生效）。

### 6.3 代码建议（按优先级）

| 优先级 | 改动 | 落点 | 预期效果 |
|---|---|---|---|
| **P1** | 回合内每步预算检查/压缩：调用 LLM 前按 `ContextPressurePolicy.Threshold`（0.8）判断，超阈值同步 `Compact` 并重建 `callMessages` | `prestep.go`（order 340 附近）或 `pipeline_llm.go` 调用前；复用 `pkg/agent/context_pressure.go:55-95` | 直接消除“回合内长到 1.7× 窗口”；对应 C4 |
| **P1** | 单 turn 内压缩切点改为 tool-pairing，避免整轮丢弃或无法压缩 | `context_budget.go` 的 `trimHistoryToFitContextWindow` / `findSafeBoundary` | 长任务单回合可安全压缩；对应 C1 |
| **P2** | 工具结果 pruner 在每次请求前幂等运行；可选结果时点 spill | `prestep` / `pipeline_llm` 请求构建处；`tool_result_prune.go` | 压低峰值；对应 C8 |
| **P2** | 窗口 >65536 时接线 `headroomTokens`；多模型补 per-model policy | `context_meter.go:116-140`、配置面 | 大窗口余量正确；对应 C5/C6 |
| **P3** | 统一口径与文档：`configuration.zh.md:69` 的 8192 → 32768；明确省略 vs 显式 0；`/context` 与提醒的百分比一致；工具集口径统一 | 文档 + `context_usage.go`、`prestep.go` | 排障不再困惑 |

**接线要点（P1 第 1 项）**：`ContextPressurePolicy.Threshold` 已实现且有测试，但当前没有调用方（`context-vs-dsh.zh.md` C4 明确记录）。建议先在 `prestep` 增加一个“预算守卫”步骤：构建 `callMessages` 后判断 `promptTokens > Threshold(window, maxTokens)`，超过则 `Compact(reason=proactive)` 并重建；保留 `isOverContextBudget` 作为最终硬守卫。需要注意压缩失败时不得死循环（限制每回合最多 N 次主动压缩）。

### 6.4 不建议

- 只把默认 `context_window` 调成 200K/256K：对未知模型是激进假设，会把压缩推迟到真正危险的位置；默认值应保持保守启发式，**已知模型显式配置**。
- 只调大提醒预算（或改公式）来“消灭 226%”：掩盖问题，峰值请求成本不变。
- 把提醒做成硬中断：当前“提示 + 硬守卫”的分工是合理的（模型可自行收尾，不破坏任务连续性），问题在守卫触发太晚。

---

## 7. 验证与回归

### 7.1 已执行

```bash
go test ./pkg/agent -run 'TestContextPromptBudget|TestContextPressureNotice' -count=1
# ok  github.com/rlds/chagent/pkg/agent
```

`TestContextPromptBudget` 锁定了 `min(0.8w, w−m)` 公式（104857 / 24576 两个样例），`TestContextPressureNotice*` 锁定 band 去重与提示文案。

### 7.2 建议新增的测试

1. **单 turn 内超预算**：构造一个回合内 N 次大工具结果（如每次 60KB），断言在 0.8 水位执行 `Compact`、`tool_call`/`tool_result` 配对完整、`isOverContextBudget` 最终为 false。
2. **tool-pairing 切点**：压缩后 retained 历史的第一个非 system 消息必须是真实 user 或 task-round；不得以 tool 结果开头。
3. **口径一致性**：同一状态下 `/context` 的 `UsedTokens` 与提醒中的 used 一致（允许锚点差异），百分比均不出现“一个 100%、一个 226%”。
4. **配置默认值**：省略 `max_tokens` 与显式 0 的行为差异要么统一，要么在文档中写清；`configuration.zh.md` 与 `defaults.go` 对齐。
5. **锚点边界**：首调用无锚点、锚点比例钳制 `[0.25,4]` 且不低于估算。

### 7.3 真实场景

- `scripts/agent-context-window-test.py --phases E,F`（E：pressure 提示中文/band 去重/非每步；F：`/context` 与 provider usage 锚点）。
- 重点看该脚本的 `MAIN_WINDOW=12000 / MAIN_MAX_TOKENS=1024` 组合下，回合内大工具输出是否在 0.8 水位触发压缩（当前实现预期不触发，可作为修复前后的对照）。

---

## 8. 待确认项

1. **该历史 agent 的模型与真实上下文窗口**：决定 221693 是「未超真实窗口但被小窗口误报」还是「已超窗、靠重试兜底」。查该会话日志中的 `Context window error detected` / `Proactive compression`（`pipeline_llm.go:354`、`pipeline_setup.go:63`），或模型文档。
2. **是否保留 32768 输出预留**：取决于目标模型的 max output；建议按模型能力显式配置。
3. **回合内每步预算检查的开销**：估算为 O(messages)，可在 `turnState` 内增量维护 token 计数；若接线，需要压测长会话。
4. **主动压缩次数上限**：避免压缩失败导致的反复压缩（建议每回合 ≤2 次，并在失败时只记录、不阻断）。

---

## 9. 附录

### 9.1 关键代码位置索引

| 位置 | 作用 |
|---|---|
| `pkg/config/defaults.go:31-34` | `max_tokens=32768`、`max_tool_iterations=300`、`summarize_token_percent=75` |
| `pkg/agent/instance.go:269-282` | `max_tokens` 0→8192 回退；`context_window` 0→`max_tokens×4` |
| `pkg/agent/context_budget.go:120-134` | `isOverContextBudget`（含工具定义与输出预留） |
| `pkg/agent/context_budget.go:140-169` | `trimHistoryToFitContextWindow`（按 turn 边界裁剪） |
| `pkg/agent/context_meter.go:116-140` | `contextPromptBudget = min(0.8w, w−m)` |
| `pkg/agent/context_meter.go:144-157` | 锚点校正（钳 `[0.25,4]`，不低于估算） |
| `pkg/agent/context_meter.go:160-169` | 提示渲染（百分比四舍五入） |
| `pkg/agent/context_meter.go:172-209` | band 判定（0.85/0.95）与每 turn 去重 |
| `pkg/agent/context_pressure.go:42-122` | DSH 压力策略纯函数（已实现未接线） |
| `pkg/agent/prestep.go:264-294` | 提醒注入（order 350） |
| `pkg/agent/pipeline_setup.go:61-128` | 回合开始主动压缩；失败仍继续 |
| `pkg/agent/pipeline_setup.go:146` | 回合开始首次提醒评估 |
| `pkg/agent/pipeline_llm.go:344-451` | provider 溢出重试压缩 + trim 重建 |
| `pkg/agent/context_legacy.go:126-215` | `forceCompression`：pruner+spill，再丢最旧约 50% 回合 |
| `pkg/agent/tool_result_prune.go` | 工具结果中段裁剪（仅压缩时调用） |
| `pkg/agent/context_usage.go:28-89` | `/context` 统计口径（百分比钳 100） |
| `pkg/tokenizer/estimator.go` | CJK 加权估算 |

### 9.2 关联既有问题

| ID | 关系 |
|---|---|
| C1（P2） | 压缩切点用 turn 边界而非 tool-pairing → 单 turn 内难以压缩，本报告 R3 |
| C4（P2） | DSH 80% 提前压缩 vs chagent 满窗触发；策略库已移植未接线 → §6.3 P1 |
| C5（P3） | `headroomTokens` 未接线（窗口 >64K 时余量偏小） |
| C8（P3） | 无 tool-result 时点 spill；超大输出先进一次请求 |
| OPEN-04（已关闭，FIX-R17） | 压缩/摘要与 durable context 的写序列串行 + 版本/前缀 CAS；后续改压缩逻辑需保持该不变量 |

详见 [上下文处理 vs DSH](../architecture/context-vs-dsh.zh.md) §4。

### 9.3 复现/核对命令

```bash
# 1) 计算默认预算
python3 - <<'EOF'
window, max_tokens = 131072, 32768
budget = min(int(window*0.8), window-max_tokens)
used = 221693
print(budget, used/budget, int(used/budget*100+0.5), used/window)
# 98304 2.2552... 226 1.6914...
EOF

# 2) 单测
go test ./pkg/agent -run 'TestContextPromptBudget|TestContextPressureNotice' -count=1

# 3) 检索本机真实提醒记录（motivating records）
grep -rl "上下文使用约" ~/chessdata ~/.chagent 2>/dev/null
grep -rl "221693" ~/chessdata ~/.chagent ~/.dsh 2>/dev/null   # 历史工作区无命中；仅本次排查会话的 projcache

# 4) 真实场景回归
scripts/agent-context-window-test.py --phases E,F
```

### 9.4 变更历史

| 日期 | 变更 |
|---|---|
| 2026-11-12 | 首版：基于代码推导、单元测试与两处本机真实会话记录完成排查；给出配置与代码建议 |
