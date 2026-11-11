# DSH 上下文窗口管理与 Prompt 插入机制（面向 chagent 的移植研究）

> 研究对象：`/Users/wudaorui/code/deepseek-harness`（DSH，TypeScript / pnpm monorepo / Cordis 插件运行时）。
> 本文所有 **DSH 证据路径** 均相对 DSH 仓库根；**chagent 证据路径** 相对 chagent 仓库根，并以 `chagent:` 前缀标注。
> 本文解决的范围是「上下文窗口 → 压缩 → 历史装配 → prompt 插入」这一条链；agent 循环/重试/工具调度请配合同目录 `dsh-agent-loop-model-exec.zh.md` 阅读。
> 只读研究，未修改 DSH 与 chagent 的生产代码。

### 阅读约定

| 术语 | 含义 | 落盘位置 |
|---|---|---|
| Turn | 一次用户输入驱动的连续工作 | `turn/start` … `turn/end` |
| Step | 一次模型请求 | `step/start` … `step/end` |
| Surface | 由 `surfaceOp` 标记的**模型可见事件序列**（可 append 也可 replace） | 由日志折叠得出，不单独存储 |
| Projection | 由日志增量折叠出的 O(1) 纯状态（usage、压缩压力、todos、plan、time…） | `sessionProjections` 注册 |
| Context 注入 | 以 `user/message` 形式进入 surface 的“上下文消息”（runtime/skill/time…） | 事件日志（可回放） |

结论先行：DSH 的上下文管理不是「拼 prompt 字符串」，而是 **日志 + surface 折叠 + O(1) 投影 + 事务化压缩 + 有序 prompt 注册表**。每一层都可从持久事件重放，因此压缩、spill、prompt 注入都能做到「崩溃可恢复、重复执行幂等、缓存前缀稳定」。

---

## 1. 请求生命周期：上下文在每个位置被谁改动

一次 step 的上下文装配顺序（数字为执行序）：

1. **pre-step**：`agent/pre-step` waterfall。插件可 reject、可向本步注入 `user/message`（runtime snapshot、时间提醒、skill 目录、plan 旁白、goal 轮次提示）。证据：`packages/core/agent-loop/src/agent.ts:276-310`。
2. **压缩决策**：`agent/pre-step` 的压缩监听在 waterfall 中完成决策；注入型监听器（time-context、tool-skill 目录、plan、goal）普遍是 `await next()` 之后再追加消息，因此注入不会先撑爆窗口。证据：`packages/compaction/compaction-basic/src/index.ts:157-175`，`packages/context/time-context/src/index.ts:199-242`，`packages/skill/tool-skill/src/index.ts:215-253`。
3. **system prompt 装配**：`ctx.systemPrompt.assemble()` 在 step 开始前完成一次（`agent.ts:287`）；请求阶段只用 `refreshContext()` 刷新 runtime context（`agent.ts:439-443`，`packages/core/system-prompt/src/index.ts:587-607`）。
4. **system prompt 落盘**：渲染结果作为 `system/message` surface 节点提交（全新会话首个系统节点即 node 0；后续按路由能力原地 replace 或 in-history append）。证据：`agent.ts:446-454`，`packages/core/agent-loop/src/runtime-context.ts:68-116`。
5. **消息注入**：claimed 消息 + runtime snapshot 以 `user/message` append（`agent.ts:456-470`）。
6. **请求构建**：日志追加 `request/header`（含 tools、adapterDefaults）与 `request/context`（含 contextWindow），随后 `session.deriveMessages()` 折叠出最终 `messages` 并深冻结（`agent.ts:648-737`）。
7. **压缩（请求后失败路径）**：provider 返回 `CONTEXT_WINDOW_EXCEEDED` → `agent/request-error` → 压缩 → 若 surface 替换代数前进则 `retry`（`compaction-basic/src/index.ts:213-257`）。

Go 移植要点：先把「日志事件 → surface 节点 → 请求消息」这条派生链做成唯一真源；不要在内存里维护第二份 `messages []`，否则 compaction/spill/prompt 注入都无法做到可重放。

---

## 2. Token 预算、估算与 usage 跟踪

### 2.1 两层定价：固定启发式 + 路由定价

DSH 有且只有一个计量服务 `ctx.tokenMeter`（`packages/llm/token-meter/src/index.ts:87-101`），但价格分两层：

- **固定启发式** `estimate.ts`：`CHARS_PER_TOKEN = 4`、每块 `BLOCK_OVERHEAD = 4`、每条消息 `ROLE_OVERHEAD = 4`；文本/推理按 `ceil(len/4)+4`，tool-call 按 name+arguments，其它块按 JSON 结构价。证据：`packages/llm/token-meter/src/estimate.ts:13-19, 42-63, 73-91, 99-102`。
- **路由定价** `route-pricing.ts`：图片按其路由声明的视觉 token + 模型可见文本重定价；文件按其 request 时投影的 handle 文本重定价。证据：`packages/llm/token-meter/src/route-pricing.ts:32-75`，图片价来源 `LlmRuntime.imageRequestPricing`（`packages/llm/llm/src/index.ts:681-683`）。

关键设计：**shadow price 协议**。surface 上的每个 replace 事件由紧邻其前的日志事件（`compaction/summary` 或 `compaction/prune`）记录「被遮蔽内容按固定启发式计算出的 token 数」，于是用量投影只需做 O(1) 加减，不必保留每个节点的历史价格。证据：`packages/compaction/compaction/src/types.ts:73-89`、`packages/llm/token-meter/src/surface-fold.ts:107-142`。

把两层的用途分清（这是最容易移植错的点）：

| 用途 | 使用哪种价格 | 证据 |
|---|---|---|
| 压缩阈值判定、尾部保留选择、区间选择 | 路由价格 `node.tokens` | `packages/compaction/compaction-basic/src/region.ts:117-155`、`config.ts:191-194` |
| O(1) 投影 fold、shadow 记账 | 固定启发式 `node.heuristicTokens` | `packages/llm/token-meter/src/surface-fold.ts:107-142` |
| 摘要是否真的变小（shrink check） | 摘要（纯文本）启发式 vs 被替换区间路由价 | `region.ts:410-422` |

### 2.2 `TokenMeasurement`：一次可重放的请求压力快照

`packages/llm/token-meter/src/types.ts:22-54`：

```ts
interface TokenMeasurement {
  logRevision: SessionLogOffset          // 消费到哪个日志位置
  baseline: { kind:'none'|'estimated'|'usage'; tokens:number; usage?:TokenUsage }
  surfaceDeltaTokens: number             // 相对锚点的有符号增减
  totalTokens: number                    // max(0, baseline + delta)
  surfaceTokens: number                  // 当前 surface 路由价总和
  nodes: readonly TokenSurfaceNode[]     // 位置序：{seq, tokens, heuristicTokens}
}
```

测量算法（`packages/llm/token-meter/src/index.ts:151-196`）：

```text
measure(session, requestHeader?):
  header  = requestHeader ?? 日志中最新 request/header
  surface = priceSurface(折叠出的 priced surface, 路由图片价, 文件投影文本)
  anchor  = 最新一次 assistant/message 形成的锚点（usage 可为 null，见 measurement-projection.ts:61-78）
  if anchor != null and header == anchor.header:
      anchorSurface = priceSurface(anchor.nodes) + anchor.assistantTokens
      estimated     = estimateToolsTokens(header) + anchorSurface
      if anchor.usage != null and usageTokens(anchor.usage) >= estimated:
          baseline = {kind:'usage', tokens: usageTokens(usage)}
      else:
          baseline = {kind:'estimated', tokens: estimated}
      delta = surface.surfaceTokens - anchorSurface
  else if header == null and surface == 0:
      baseline = {kind:'none',0}; delta = 0
  else:
      baseline = {kind:'estimated', tokens: estimateToolsTokens(header)+surface.surfaceTokens}; delta = 0
  return totalTokens = max(0, baseline.tokens + delta)
```

其中 `usageTokens = input + cacheRead + cacheWrite + output`（`index.ts:56-62`）。锚点的两条使用条件必须同时满足：**header 完全相同**、**usage 不小于同前缀的完整启发式估价**（否则按 estimated 处理）。这把 provider 的真实 usage 当作下界锚点，同时避免 provider 少报导致的低估。

运行时缓存结构（`index.ts:45-54, 230-293`）：`states: WeakMap<Session, ReplayState>` 保存「折叠到哪、priced surface、最近 header、锚点 header/nodes、pending measurement」；`_sync()` 增量追赶日志尾巴，遇到 `replace` 用 `planSurfaceTokens/commitSurfaceTokens` 做计划-提交；若再入日志发现 checkpoint 引用不一致，整份丢弃并从头重放。**价格缓存永远可重建，不是真源。**

Go 移植要点：
- 预算模型必须是 `total = max(0, baseline + surfaceDelta)`，其中 `baseline` 可来自 provider usage，也可以是纯估算；不要只维护一个 `usedTokens` 变量。
- 接口建议：`type Meter interface { Measure(sess *Session, hdr *Header) Measurement; EstimateMessage(m Message) int }`，内部用 `logRevision` 做增量折叠。
- 必须实现 `surface replace` 的 O(1) 记账（事件前一条 shadow price），否则每次改历史都要全量重算，压缩循环会 O(n²)。

### 2.3 usage 跟踪：重试不能重复累加

`tokenUsage` 投影（`packages/llm/token-meter/src/usage-projection.ts:111-144`）的规则：

- `assistant/message` / `assistant/attempt` 携带的 usage 是**同 turn+step 的替换样本**：若与 `last` 的 turn/step 相同，先从 totals 减掉旧样本再加新样本（`addReplacing`，`:34-43`）。
- `llm/retry-started` 会把 `last` 清空，使重试后的成功样本重新累加（`:117-121`）。
- 桶口径：`uncachedInputTokens/outputTokens/cacheReadTokens/cacheWriteTokens`（`:21-26`），不重复计 reasoning。

Go 移植要点：usage 统计要按 `(turn, step)` 幂等 upsert，而不是见到一条 usage 就 `+=`；否则重试/中断会让 UI 与压缩阈值都失真。

### 2.4 上下文压力投影：给“下一次请求”而不是“上一次请求”的数字

`contextPressure` 投影（`usage-projection.ts:167-212`）维护四个槽：

- `contextWindow`：来最新 `request/context` 事件；
- `pressureTokens = input + cacheRead + cacheWrite`（不含 output，`pressureFrom` 见 `:77-79`）；
- `surfaceTokens`：随每次 surface 增删做 O(1) 加减（`foldSurfaceProjection`）；
- 发布 `projectedTokens = max(0, pressureTokens + surfaceTokens - sampledSurfaceTokens)`（`:202-210`），即“锚点样本 + 之后 surface 的净变化”。这样**压缩本身不产生 usage，但投影能立刻反映下一次请求会变小**。

注意其文档明确说明：`pressureTokens/contextWindow/projectedTokens` 是「不同时刻的 last-wins 槽位」，不是原子观测（`docs/subsystems/token-meter.zh.md` 与 `projection.ts` 注释）；只用于展示与参考，计费/门控必须用 `measure()`。

### 2.5 context window 与 max output 的配置来源

1. **模型能力由 adapter 提供**：`LlmRuntime.prepareCall()` 调 adapter 的 `prepareCall()` 得到 `model`，校验后放入 `PreparedLlmCall`：`context.contextWindow`、`defaultMaxTokens`、`systemPromptUpdate`、`toolUpdate`、`inputModalities`。证据：`packages/llm/llm/src/index.ts:956-994`。
2. **合法性校验**：`contextWindow` 必须是正整数（`index.ts:794-800`）；`defaultMaxTokens` 必须正安全整数（`:820-827`）。
3. **DeepSeek 内置 catalog**：官方 `context_window = 1_048_576`（`packages/llm/llm-deepseek/src/models.ts:4-22`）；连接级 `DEFAULT_MAX_TOKENS = 256_000`（`packages/llm/llm-deepseek/src/defaults.ts:8`），catalog/模型级 `maxTokens` 可覆盖（`config.ts:96, 151-155, 322`）。
4. **pi-ai 适配器**：catalog 的 `contextWindow` 进入 adapter 解析结果（`packages/llm/llm-pi-ai/src/adapter.ts:337`），并用于流式侧溢出判定（`adapter.ts:426`）。
5. **请求时记录**：`buildRequest()` 把 `contextWindow`、`systemPromptUpdate` 写入 `request/context`，仅当变化时追加（`agent.ts:702-716`）；Session 侧 fold 为 `requestContext()`（`packages/core/session/src/index.ts:939-956`）。

Go 移植要点：
- 配置模型：`ModelInfo{ContextWindow, DefaultMaxTokens, SystemPromptUpdate, ToolUpdate}` 必须由 provider adapter 返回并校验，不能散落在全局 config；压缩策略消费「路由后的实际窗口」，而不是部署默认值。
- 输出预留：`reserved = header.maxTokens ?? adapter.defaultMaxTokens ?? 0`（`compaction-basic/src/index.ts:59-67`），必须显式参与预算。

### 2.6 超限检测的三条路径

DSH 同时从三条路径识别“超窗”，互补：

1. **错误文本分类**：`isContextWindowExceededError(detail)` 用 4 组正则覆盖 `context length/window exceeded`、`too large for context`、`exceeds model context` 等措辞（`packages/llm/llm/src/error.ts:53-89`），DeepSeek transport 将其映射为稳定 code `CONTEXT_WINDOW_EXCEEDED`（`packages/llm/llm-deepseek/src/transport.ts:32`）。
2. **usage 回推**：pi-ai 的 `isContextOverflow(message, contextWindow)` 用「返回 usage ≥ catalog 窗口」或「length 且零输出」判定（`packages/llm/llm-pi-ai/src/stream.ts:75-105, 222-236`）。
3. **预判**：压缩后端在 `agent/pre-step` 用 `measure().totalTokens >= thresholdTokens` 主动触发（见 §3.3）。

Go 移植要点：错误分类必须产生**稳定 code**，绝不能依赖 message 字符串在业务层 `strings.Contains`；同时保留一个 usage 回推兜底（有的 provider 不报错，只返回 length）。

---

## 3. 压缩（compaction）

DSH 的压缩是独立的 capability seam（`ctx.compaction`），不属于 agent loop 主干；三种角色：Service Definition（`packages/compaction/compaction`）、Provider（`compaction-basic`、`command-compact`、`compaction-tool-result-pruner`、`compaction-image-offload`）、Consumer（`/compact` 命令）。证据：`docs/subsystems/compaction.zh.md`，`packages/compaction/compaction/src/index.ts:119-193`。

### 3.1 持久事务：start / summary / end + 一次 surface replace

压缩不改写历史，只**追加**：

```text
compaction/start   {compactionId, sourceCommandId?, turn:number|null}   // 日志锁
compaction/summary {compactionId, summary, shadowedRange{start,end},
                    shadowedSeqs, shadowedTokenCount, provider, model,
                    maxTokens?, usage?, rawOutput?, llmStreamCall?}    // 计量 + 摘要
compaction/end     {compactionId, turn, error?}                         // 释放锁
user/message       frameSummary(summary) + surfaceOp{op:'replace',startSeq,endSeq}
                   + sourceEventSeqs=[start,summary,...shadowed]        // 唯一 surface 变更
```

证据：`packages/compaction/compaction/src/types.ts:17-91`，提交顺序 `packages/compaction/compaction-basic/src/region.ts:204-239, 471-522`。

三个契约要特别注意：

- `compaction/start` 与 `compaction/end` 之间是**日志级锁**：中途崩溃会留下 unmatched start，后续入口按 busy 拒绝；若其后出现了 `session/end-seed`（fork/新生命周期），则该遗留锁被视为陈旧证据忽略。证据：`region.ts:301-320, 566-600`。
- `shadowedRange` 是**surface 位置跨度**，不是 seq 数值区间：一次 replace 可能把新高 seq 摘要放到旧区间位置，所以 `start` 可能大于 `end`；真正的权威集合是 `shadowedSeqs`（surface 顺序）。证据：`compaction/src/types.ts:107-118`。
- 摘要替换消息**必须**由 `compactCheckpointSource(compactionId)` 构造（`packages/compaction/compaction/src/checkpoint.ts`），让消费方无需认识具体后端即可识别压缩检查点（`region.ts:410-413`）。

### 3.2 触发阈值与时机（四个入口）

配置（`packages/compaction/compaction-basic/src/config.ts:19-23, 68-110`）：

| 参数 | 默认 | 含义 |
|---|---|---|
| `thresholdRatio` | `0.8` | 压力阈值 = 窗口 × 0.8 |
| `headroomTokens` | `65_536` | 为压缩/波动预留的 token |
| `retainRatio` | `0.16` | 保留尾部 = 消息预算 × 0.16 |
| `retainTokens` | — | 显式尾部预算（与 ratio 互斥） |
| `maxTokens` | `headroomTokens` | 摘要调用自身的输出上限 |
| `compactionRetries` | `1` | 一次触发后再压几轮 |
| `maxOverflowRetries` | `1` | overflow 恢复重试上限 |
| `auto` | `true` | 是否注册自动入口 |
| `modelPolicies` | `[]` | 按 provider/model 精确覆盖 |

具体预算（`config.ts:141-217`）：

```text
messageBudget  = contextWindow - reservedCompletionTokens
pressureBudget = messageBudget - headroomTokens
thresholdTokens = floor(min(contextWindow * thresholdRatio, pressureBudget))
retainTokens    = retainTokens ?? floor(messageBudget * retainRatio)
不变量：retainTokens < thresholdTokens；pressureBudget > 0
```

四个触发入口：

1. **每步压力**：`agent/pre-step` → `compactIfNeeded(agent,'pressure',signal)`；异常只 warn 并继续 turn（`compaction-basic/src/index.ts:157-175`）。`TargetPressureConfigError` 按目标 key 只警告一次（去重）。
2. **模型切换**：`agent/request`（prepend）在路由选择后、header 落盘前，用**新 header** 度量；必须压到阈值以下才允许切换；失败抛 `MODEL_SWITCH_COMPACTION_FAILED` 并保留旧 header（`index.ts:183-203, 315-321`）。
3. **provider 溢出**：`agent/request-error` 且 `code === CONTEXT_WINDOW_EXCEEDED` → `compactIfNeeded(agent,'context-overflow',signal)`；溢出路径会先跑一次免费的剪枝、再无视保留尾部选一个可用区间（`index.ts:213-257, 292-313`）。
4. **人工**：`/compact` → `compactNow()`，要求 agent idle，写独立 `turn:null` 标记对，并在返回前 flush 持久化（`index.ts:407-459`，`command-compact/src/index.ts:59-79`）。

### 3.3 预算判定与重试收敛

`compactPressure()`（`index.ts:316-371`）的主循环：

```text
measure(header)                          // 用待生效 header 度量
if measure.total < threshold: return null
if pruner exists: pruner.pruneSession(); re-measure   // 先做模型无关缩减
if measure.total < threshold: return null
for attempt in 0..compactionRetries:
    range = selectCompactableRange(session, measure, retainTokens)
    if range == null:
        if result==null && !requireBelowThreshold: return null
        throw "no safe range"            // 模型切换场景必须失败
    result = compactRegion(range)
    measure = re-measure
    if measure.total < threshold: return result
throw "still above threshold after N attempts"
```

三个值得移植的细节：

- **先剪枝后摘要**：模型无关的确定性剪枝不消耗 LLM 调用，优先落地；压缩失败时这块缩减依然有效（`index.ts:344-350`）。
- **重试有界且按目标重算**：每轮重新选区间、重新测量，不缓存第一次的 range。
- **“有效进度”用 replace 代数证明**：overflow 恢复只有在 `surface.replaceGeneration > 进入时` 时才返回 `{kind:'retry'}`；即使后续摘要抛错，只要剪枝已落盘也算有进度（`index.ts:225-256`）。取消优先于一切。

### 3.4 保留区与 safe boundary

`selectCompactableRange()`（`region.ts:117-155`）：

```text
nodes = measurement.nodes（必须与 session.surface.nodes 逐项相等，否则报错）
firstIdx = 0
if surface node 0 是 system/message: firstIdx = 1        // 永不压系统提示
from tail to head: accumulate node.tokens until >= retainTokens
  keepFromIdx = 当前下标
  if keepFromIdx <= firstIdx: return null
while keepFromIdx > firstIdx and !toolPairingBalancedBefore(nodes[keepFromIdx]):
  keepFromIdx--                                          // 向前找安全的切点
if keepFromIdx <= firstIdx: return null
return { start: nodes[firstIdx], end: nodes[keepFromIdx-1] }
```

**safe boundary 不走「turn 边界」，而是走工具调用/结果配对**（`packages/compaction/compaction/src/tool-pairing.ts:29-126`）：

- `assistant/message` 中每个 `tool-call` 使在途计数 `+1`，每条 `tool/result` 使 `-1`；
- surface 的第 i 个切点是“平衡的”当且仅当切点前在途计数为 0；
- 用 `WeakMap<Session, BalanceCache>` 缓存 `cutBalanced[] + indexBySeq + inProgressToolCalls`，以 `surface.replaceGeneration` 判定是否失效；只增量折叠新节点；若检测到 `tool/result` 无对应 call 则报“surface 损坏”。
- 区间两端分别调用 `toolPairingBalancedBefore/After` 校验（`region.ts:337-358`），因此**同一 turn 内已闭合的 step 可以被压缩**，而整轮压缩做不到这一点。

### 3.5 摘要 prompt：复用会话前缀以命中 KV cache

`packages/compaction/compaction-basic/src/summarizer.ts:25-67` 的 `COMPACTION_INSTRUCTION` 要点：

- 角色切换为 "compaction engine"，要求输出**固定 Markdown 结构**（Primary Request and Intent / Key Technical Concepts / Files and Code / Errors and Fixes / Pending Jobs / Current Work / Next Step / Critical Context），空节写 `(none)`，不得丢节。
- 明确规则：保留精确路径、命令、错误串、标识符、数值、签名；忠实记录用户反馈；不得提及“本次压缩”；只输出 checkpoint 文本，不调工具。
- 若上文已含 `<compacted-summary>` 块，视为**前一次 checkpoint**：保留仍为真的事实、丢弃过时事实、合并为新单一摘要（幂等/去重）。

调用方式（`summarizer.ts:120-181`）：

```text
messages = [ ...会话自己的 system 头, ...被压缩区间按 surface 顺序投影的消息,
             {role:'user', content:[COMPACTION_INSTRUCTION]} ]
options = { provider, model, messages, toolHistory,
            tools: 原 header.tools, maxTokens, sessionId, purpose:'compaction', signal }
stream → BlockAssembler → 只保留 text（拒绝图片输出，:213-221）
finish.kind == max-tokens → 视为失败（:203-207）
```

关键点：**取会话自己的系统提示、tools、消息前缀作为摘要请求的前缀**，只把指令追加在最后，于是这是一个“真实前缀”，provider 可复用 KV cache（`summarizer.ts:109-118` 注释；`region.ts:544-563` 构造输入）。摘要目标解析优先级：显式 `summarizationProvider/Model` → 最近路由 header → agent options（`summarizer.ts:127-142`）。

摘要落盘时用固定包裹（`summarizer.ts:69-71, 188-194`）：

```text
<checkpoint 前言：这是自动生成的压缩上下文，视为既定背景，不要复述>
<compacted-summary>
...模型输出...
</compacted-summary>
```

### 3.6 事务与失败/降级路径

`compactSurfaceRegion()`（`region.ts:173-276`）的事务骨架：

```text
validateSurfaceRegion(start,end)      // 必须当前 surface、start 位置<=end、两端 balanced
inspectCompactionEntryState()         // openTurn / unmatched start / latest end-seed
assertCompactionInactive()            // 已有未闭合压缩 → busy
begin(owner)                          // owner=当前 turn 号 或 null（手动）
append compaction/start               // 日志锁先于任何 await
prepared = prepareCompaction()        // 记录 measurement + selectedNodes + 价格 + 回放输入
summarized = summarizeCompaction()    // 失败可走 compaction/summary-error waterfall 恢复重试
assertStable()                        // 异步期间 surface 是否仍可替换
pending = commitCompactionBody()      // append summary + append user/message(replace)
append compaction/end
catch:
  append compaction/end {error}       // 恰一次关闭；关闭失败归为 commit 阶段
finally:
  if flush: await flush()             // 手动路径可要求持久化
```

**稳定性校验**有两种：

- `whole-surface`（自动/当前 turn）：用 `measure().nodes` 深度比较，任何 surface 变化都判 `SurfaceChangedError`（`region.ts:430-440`）；
- `selected-span`（手动）：只要求被选区间仍是同一个当前、连续、等价定价、balanced 的替换目标；区间外新增注入不使其失效（`region.ts:442-468`）——这允许压缩期间排队提示词/空闲注入继续落地。

**收缩检查**（`region.ts:410-422`）：把 framed checkpoint 用固定启发式定价，与被替换区间的**路由价**比较，`framedSummaryTokenCount >= shadowedRouteTokenCount` 直接失败（“摘要没有变小”）。这一条防止“压缩后反而更大”的死循环。

**失败分类**（`compaction/src/index.ts:34-64`，`region.ts:278-299`）供人工路径报告：`busy / cancelled / changed / summary / commit / persistence`；其中 `changed`、`summary` 会闭合失败尝试并持久化日志但不写替换；`commit` 可能已部分变更；`persistence` 表示内存标记对已闭合但 flush 失败；取消始终抛出原始 abort 原因。

**模型无关剪枝**（`compaction-tool-result-pruner`）：对当前 surface 中每个超预算的 `tool/result` 做头/中/尾确定性替换（默认 `thresholdChars=8192, headChars=4096, tailChars=1024`，marker `[... tool result middle pruned ...]`），只允许改 `content`，替换前紧邻写 `compaction/prune` shadow price。证据：`config.ts:7-14, 55-63`，`index.ts:83-120, 136-182`。

**图片离线**（`compaction-image-offload`）：当路由返回 `IMAGE_OFFLOAD_REQUIRED` 时，选择最老的保留图片位置写一条 `image/offload` 耐久决策（log-only，无 surfaceOp），此后投影用占位文本替换这些出现位置（`packages/compaction/compaction-image-offload/src/index.ts:25-40`，`src/projection.ts:40-74`）。它既是 request-error 恢复，也是 `compaction/summary-error` 的恢复监听器。

Go 移植要点：
- 预算模型：`threshold = min(window*ratio, window-reserved-headroom)`；`retain < threshold` 必须在加载时校验（fail loud）。
- 接口：`Compactor{ CompactIfNeeded(agent, trigger, ctx) (Result, error); CompactRegion(start,end); CompactNow() }`，Provider 负责阈值/保留/摘要，Definition 只定义事件与替换语义。
- 触发点：pre-step（压力）、request（模型切换）、request-error（overflow）、idle maintenance（手动）。
- 降级：`no safe range` 对自动压力返回 null；对模型切换必须失败并保留旧路由；对 overflow 只在“有耐久进度”时重试；摘要有界重试（默认 1）。
- 幂等/去重：用 `compaction/start|end` 日志锁；用 `shadowedSeqs` 而非 seq 区间；替换必须走同一事务 ID；摘要 prompt 要处理“已有 checkpoint 合并”场景。

---

## 4. Spill：大输出落盘与引用回填

### 4.1 能力 seam：只保存，不决定策略

`ctx.spillStore.saveText(input) -> SpillRef{locator,bytes,retrievalHint}`（`packages/spill/spill/src/index.ts:45-56`，类型 `packages/spill/spill/src/types.ts:62-80`）。职责边界刻意拆开：seam 只负责持久化全文并返回模型可读的 locator + 检索提示；**保留策略在 `spill-policy`，替换决策也在 `spill-policy`**（`spill/src/index.ts:8-15`）。

### 4.2 策略：超过 `maxInlineTokens` 才落盘，头尾保留 + 提示回填

`packages/spill/spill-policy/src/index.ts`：

```text
apply(ctx, {maxInlineTokens}):
  若未配置 → 插件不生效（:50-54）
  tools/post-execute（prepend，先让下游决定）:
    只处理 accept 且无 value 覆盖、非 read 工具（:134-136）
    文本+图片 → bound()
      若总定价 <= maxInlineTokens → 原样返回（:99）
      否则 saveText(owner=sessionId, source={tool,toolName,callId,label}, 全文)（:104-107）
      生成 omission notice（精确省略字节 + 图片数 + locator + 检索提示）（notice.ts:21-24）
      预算扣除 GAP 与 notice 后，头尾各分一半做保留（retention.ts:48-103）
      相邻文本块合并，避免多一层 framing（:120-126）
  存储/定价失败 → warn 并保留 inline 内容（:128-131）
  PTC：`tools/ptc-dispatch-log` 对文本 dispatch 同样 bound（:153-156）
```

`retention.ts:15-37, 48-103` 的关键点：文本按 UTF-16 surrogate 边界切片（`textSlice`），用**单调** token 估价二分找最大可保留长度（`fitText`），图片不可分割（放不下就整块省略）；每端各 `budget/2`。

### 4.3 本地后端：私有目录、0600、启动期清理

`packages/spill/spill-local/src/index.ts:31-49, 65-101`：

- 默认根：OS temp 下 `mkdtemp('dsh-spill-')`，0700；可配置 `root`。
- 文件名：`<random6hex>-<encodeSegment(suggestedName)>`，`encodeSegment` 把任意字符串注入式转成一个安全路径段（`~XXXX` 转义，杜绝 `../`、绝对路径、NUL、分隔符），见 `store.ts:55-68`；session 目录 `session-<sha256(sessionId)[:12]>`（`store.ts:78-81`）；`open(path,'wx',0o600)` 独占写（`store.ts:108-130`）。
- 清理：`cleanupPeriodDays` 默认 30（0 关闭），启动时跑**一次 best-effort** 扫描；只删 mtime 严格早于 cutoff 的普通文件，跳过 symlink 与不可信目录（root/session 目录必须当前用户所有且 group/other 不可写），空 session 目录与可剪枝的旧默认根一并清理；失败只 warn，绝不 reject。证据：`index.ts:27-28, 90-117, 133-147`，`cleanup.ts:15-23, 173-184, 196-272, 274-...`。
- 检索提示由后端给出：`'Use read with offset/limit, or grep this path to search within it.'`（`index.ts:159`）。

配套消费：`session-reference` 在预览被截断时把完整 transcript 也 spill，存储失败则降级为 `unavailable` 而不是谎称全文可用（`packages/context/session-reference/src/spill.ts:23-50`）。

Go 移植要点：
- 接口：`SpillStore.SaveText(ctx, SaveTextInput) (Ref, error)`；策略层决定阈值、头尾保留、notice 文案；后端决定路径、权限、清理。
- 触发点：工具执行后（post-execute）与 PTC dispatch 日志，不要在 agent loop 里硬编码。
- 降级：落盘失败必须**保留原 inline 内容**并记录原因；omission 文案必须含能检索的 locator。
- 幂等：`saveText` 每次生成新文件（随机名），重复保存不会覆盖；清理只按 mtime，不删正在使用的（保留 30 天）。

---

## 5. 会话历史装配

### 5.1 日志 + surface 双语义（append / replace）

- 只有 5 类事件能成为 surface 节点：`system/message`、`developer/message`、`user/message`、`assistant/message`、`tool/result`（`packages/core/session/src/surface.ts:49-56`）。
- surface 事件必须带 `surfaceOp`：`'append'` 或 `{op:'replace', startSeq, endSeq}`（`surface.ts:301-327`）。
- replace 的合法性：`startSeq/endSeq` 必须指向更早的事件（`surface.ts:397-412`）；替换区间必须在当前 surface 内且 start 位置不大于 end（`surface.ts:415-435`）；`sourceEventSeqs` 必须包含所有被遮蔽节点（`surface.ts:330-365`）；`tool/result` 替换只允许改 `content`（`surface.ts:455-485`）；**surface node 0 的系统提示只能被 `system/message` 对单节点替换**（`surface.ts:487-507`）。
- 折叠结果：`{nodes, replacements, projectedMessages}`（`surface.ts:234-242`）；`deriveEventMessage` 把事件投影为消息，空 content 的 system/developer/assistant 返回 null（`surface.ts:120-152`）。

`Session.deriveMessages()`（`packages/core/session/src/index.ts:974-1020`）是请求历史的唯一来源：

```text
if contentGeneration 变化: 清空缓存（replace 或 plugin 消息投影都会触发）
for seq in surface.nodes[derivedNodes:]:
    msg = deriveEventMessage(log[seq])
    if msg != null: push
return [...derived]          // 新数组；Message 本身共享且深冻结
```

`requestHeader()` / `requestContext()` / `toolHistory()` 都是增量 fold + 深冻结缓存（`index.ts:914-972`）。`toolHistory` 由 `ToolHistoryProjection` 折叠 `request/header` 的 tools 与 `developer/message` 的 `tool-addition/removal`，并在“当前 active 与历史声明不一致”时退化为只发当前 active（`packages/core/session/src/tool-history.ts:26-74`）。

### 5.2 repair：崩溃/中断后的尾巴修复

`packages/core/session/src/repair.ts`：

- `openTurnClosers(events, cause)` 扫描日志找到未闭合 turn/step，为未回填的 tool-call 生成**确定性** error tool/result（按 assistant 顺序，id 前缀固定），再补 `step/end`、`turn/end`（`:32-98`）。
- `ToolCallRecovery` 维护 `pendingCalls: Map<ToolCallId,{turn,step,callSeq?}>`，靠 `assistant/message` 的 tool-call、`tool/call`、`tool/result` 增删；`turn/start|end|step/end` 清空（`:105-197`）。
- 错误码二选一：`TOOL_OUTCOME_UNKNOWN`（记录过 start）或 `TOOL_NOT_STARTED`（连 start 都没有）；文本按 cause 区分 `interrupted` 与 `forked`，后者明确“父会话可能在分叉点后执行过”，要求先核实外部状态（`:14-41`）。

### 5.3 fork：前缀 + 边界标记 + 尾巴闭合

`buildForkSeed(events, boundary)`（`packages/core/session/src/fork.ts:21-30`）：复制 inclusive 前缀 → 追加 `session/end-seed{inherited:true}` → 用 `{kind:'forked'}` 生成 closer 事件。`session/end-seed` 同时是“陈旧压缩锁”的判定边界（见 §3.1）。

### 5.4 preparation：未发布 Session 的所有权

`SessionPreparation` 用同步、幂等的 `[Symbol.dispose]()` 包裹发布前 Session，供 Provider 在发布成功/回滚时释放缓存状态（`packages/core/session/src/preparation.ts:20-49`）。Go 里对应 `defer release()` + `sync.Once`。

### 5.5 投影契约（context 状态复用同一套）

`ProjectionDefinition`（`packages/session/session-projection/src/index.ts:74-124`）：`key + stateSchema + init(header, inheritedCount) + apply(state,event) + stateVersion + 可选 wire{viewSchema,view,summary}`。硬性约定：**不关心的事件必须返回同一个 state 引用**（`Object.is` 为零下游工作）；`stateVersion` 变更即丢弃持久缓存行；`view` 必须引用稳定。token 用量、context 压力、time-context、plan、todos、runtime snapshot 的“已发布/已注入”状态都是投影。

Go 移植要点：
- 类型：`type Projectilator interface { Key() string; Version() int; Init(h Header, n int) any; Apply(state any, ev Event) any }`，用不可变/同引用返回做快路径。
- 历史装配：`deriveMessages` 必须缓存并按 `contentGeneration` 失效；不要把 surface 复制进内存作为第二真源。
- 修复：把“未闭合工具调用”做成纯函数 `closeOpenTurn(events, cause)`，由 crash recovery 与 fork 共用；合成事件必须确定性（seq/time/id）。
- fork：先写 `inherited=true` 的边界事件，再追加 closer，保证子会话能区分“继承的未闭合”与“自己中断的”。

---

## 6. Prompt 插入机制

### 6.1 system-prompt 注册表：有序、可分作用域、缓存稳定

`SystemPrompt`（`packages/core/system-prompt/src/index.ts:433-695`）把提示拆成四类可注册输入：

```ts
interface PromptSection { name; order; text: string|fn(assembleCtx); interpolate?; complete? }
interface PromptContext { name; order; text: string|fn; interpolate?; required? }
ctx.systemPrompt.tools(provider)     // 返回 ToolSchema[]
ctx.systemPrompt.variable(name, fn)  // {{name}} 变量
```

排序与稳定性：

- sections：`order` 升序，同 order 按名字**代码单元序**（`comparePromptSections`，`index.ts:252-255`）；order 由中心分配表 `SECTION_ORDERS` 决定（`:136-170`，例如 `HARNESS_IDENTITY=-1000`、`PLAN_POLICY=500`、`TOOL_BASH=1000`、`HARNESS_SOURCE=10000`、`PERSONA_SUFFIX=10200`），插件只 `getSectionOrder(name)` 取号，不能自造。
- contexts：独立的中分配表 `CONTEXT_ORDERS`（`:175-184`）。
- 作用域：scoped 注册会 **shadow** 同名 global（`layers.merge`，`:635-637`）；重复名直接抛错（`PromptLayer`，`:400-430`）。
- `complete` section：装配仍会跑 waterfall（让工具/上下文/变量可解析），之后恢复该 section 作为唯一提示；多于一个生效的 complete 直接失败（`:657-662, 686-693`）。
- 渲染：过滤空段后以 `\n\n` 连接；`interpolate` 严格解析 `{{var}}`，未注册/未赋值/畸形都抛错（`renderPrompt`，`:295-300`；`interpolate`，`:344-382`）。
- 工具：`orderTools` 按配置 `toolOrder`（必须含 `<unlisted-tools>` 标记）排序，未列出的按名字字典序插入；未知名字装配期失败（`:206-245, 673`）。

缓存稳定性来自两点：**同一 order 分配表 + 名字加序**，以及**动态事实不放在 sections 里**（见 §6.2/6.3）。

### 6.2 系统提示作为 surface 节点，而不是请求字段

`agent-loop` 渲染 `renderPrompt(assembly)` 后由 `SystemPromptProjection` 决定如何写入日志（`packages/core/agent-loop/src/runtime-context.ts:68-116`）：

```text
project(rendered, {inHistory, startsSeries}):
  nodes = 当前所有 system/message 节点
  if 无节点: append 首个 system/message（空 prompt 也会落节点；在全新会话中它即 node 0，见 runtime-context.ts:57-67 注释）
  latest = 最后一个非空节点（无则 head）
  if !inHistory || rendered=='' || (startsSeries && latest.text != rendered):
      // 不能 in-history 追加，或新序列文本变化 → 归一到 node 0
      清空所有非空后续节点（replace(seq,'')）
      if head.text != rendered: replace(head.seq, rendered)
      return updates
  if latest.text == rendered: return []            // 完全幂等：不发事件
  return append 新的 system/message                // 路由支持 in-history 时保留缓存前缀
```

`startsSeries` 的判定（`agent.ts:445-451`）：本步被 pre-step 声明新序列、或 `requestSurfaceGeneration != contentGeneration`（surface 被压缩/替换过）、或工具集合在“不支持工具更新”的路由上变化。路由能力来自 adapter 的 `systemPromptUpdate:'in-history'`（DeepSeek catalog 即如此：`packages/llm/llm-deepseek/src/models.ts:14`）。

效果：**系统提示成为派生历史的第一条消息**，既让 provider 能按前缀缓存，也允许压缩/替换体系识别 node 0 并保护它（§5.1）。

### 6.3 runtime context snapshot：可被后续快照替代的耐久消息

`PromptContext` 的文本经 `renderContextSections` → `joinContextSections` 组装（`system-prompt/src/index.ts:307-342`），头部固定一句：

```text
Current runtime context. This snapshot supersedes earlier runtime-context snapshots.
```

`RuntimeContextProjection`（`runtime-context.ts:119-168`）负责“只在变化时发一条，并允许后续快照替代”：

```text
构造时从日志反向恢复 retained：最新可见的 source.kind=='runtime-context' 节点
监听 session/event：
  - 新 runtime-context 消息 → retained = 它
  - 若某 replace 事件的 sourceEventSeqs 包含 retained.seq → retained = null（被压缩遮蔽）
project(current, sections):
  if 从未存在 && current 为空: 不发
  snapshot = current == '' ? CLEARED : current
  if retained.text == snapshot: 不发          // 幂等
  return user/message{content: snapshot, source:{kind:'runtime-context',
         form:'snapshot'|undefined, sections}}
```

`required:true` 的 context 在启用 `includeRuntimeContext=false` 时仍保留（`system-prompt/src/index.ts:587-607, 676-679`）。`harness-dev-context` 是典型：同时注册 section（静态、可被已准入段落替代）与 context（每步当前 URL/checkout），见 `packages/context/harness-dev-context/src/index.ts:122-157`。

### 6.4 skill 渐进披露：目录摘要 + 按需加载正文

`packages/skill/tool-skill/src/index.ts` 实现三级披露：

1. **目录**：只在 `skill` 工具对当前 agent 可见且 `skills.snapshot().complete` 时发布；渲染 `<system-reminder>` + `<available_skills>`，只含 `name` 与截断 description（默认 500 字符），并明确“目录只是摘要，加载前不要推断或执行正文”（`:215-253, 256-279, 393-396`）。
2. **去重/重发布**：对 entries 做 sha256 digest（`:330-337`）；若当前可见目录 digest 一致 → 若本步 claimed 消息里又出现旧目录则删掉（避免重复）；若已发布且 digest 变化 → 发布一个 `<system-reminder>` 说明“这是替换目录，只使用本列表”（`:281-313`）。
3. **正文加载**：模型调用 `skill` 工具 → 读 `ctx.skills.get` → 工具结果返回完整正文 + resourceBase（`:82-161`）。用户 `/name` 手势是另一个确定性入口：只扫描 `source.kind=='user'` 的消息，避免外部文本伪造（`:165-206, 405-433`）。

Go 移植要点：目录用 digest 去重、变更时发**替换**语义消息；正文永远按需加载，不要塞进 system prompt。

### 6.5 todo / plan / goal 的注入差异

| 子系统 | 是否进 prompt | 机制 | 证据 |
|---|---|---|---|
| todo | **否** | `todo_write` 工具写 `todo/write` 事件 + `todos` 投影；UI 消费，模型下一轮靠工具结果知晓 | `packages/todo/tool-todo/src/index.ts:118-135, 193-200` |
| plan mode | 是，两路 | ① `plan:policy` section，text 为函数：仅当投影显示 active 才返回部署文本；② pre-step 追加 `plan/mode` log-only 事件（模式切换在 turn 内生效）及一次旁白 user 消息 | `packages/plan/plan-mode/src/index.ts:195-228` |
| goal | 是 | ① `tool:goal` section（goal 工具策略文本）；② goal-round driver 用 `Agent.followup()` 注入 `<goal_round>Objective/Round/...` 用户消息；pre-step 校验轮次预留，不合法则 reject | `packages/goal/tool-goal/src/index.ts:185-195`，`packages/goal/goal-round-driver/src/prompt.ts:12-26`，`src/index.ts:380-445` |

### 6.6 per-step reminders：投影驱动、可去重、可升级

**时间提醒**（`packages/context/time-context/src/index.ts`）：

- 投影状态：`lastMessageTime / lastInjectionTime / lastTurnInjectionTime / browserTurn / browserTimeZones`（`:35-46, 158-197`）。
- 默认 `refreshIntervalMs = 600_000`（10 分钟），阈值内不重复注入（`:135, 209-214`）。
- 文本形如 `Time sampled while preparing turn T, step S: ... / Elapsed since the preceding model-visible message|step context: ...`（`:98-113`）。
- 注入方式：`agent/pre-step` 返回 `messages: [...decision.messages, userMessage{source:{kind:'time-context',form:'snapshot',sections:[...]}}]`（`:199-242`）。

**重复工具提醒**（`packages/guard/repeat-tool-reminder/src/index.ts`）：

- 以 `agent` 为键维护「上一次同参数调用」链条：`key = JSON.stringify([toolName, canonicalize(args)])`，深 key 排序保证参数顺序无关（`:96-112, 196-214`）。
- 阈值默认 `[3,5,8]`；第一个阈值用温和提醒，后续用含 `tool / consecutive_calls / arguments` 的详细提醒，且参数**预览**截断（默认 500 字符）只影响展示、不影响链判断（`:36-57, 70-86, 125-128`）。
- 计数发生在 `tools/post-execute`（被拒绝的调用也走同一 waterfall），但**只“观察并附加”，绝不 veto**：先 `next()` 拿到下游决策，再把 reminder 放进 `additionalContexts`（`:216-231`）。
- 用户插话（`source.kind=='user'`）重置链条（`:233-239`）。

**守卫/其它**：`guard/timeout-policy` 在 `tools/execute` 外层包 deadline，只在“自己的 deadline 赢”时替换结果为 `TOOL_TIMEOUT`，见 `packages/guard/timeout-policy/src/index.ts:1-8, 50-81`。

### 6.7 Go 移植要点

- 预算模型：prompt 插入的每一项都要有 **token 体积上限**（目录 500 字符、参数预览 500、runtime snapshot 段落可控），并明确优先级（required 上下文 > 可选快照 > 目录 > 提醒）。
- 接口：`PromptRegistry{ Section(order,name,fn); Context(order,name,fn); Tools(fn); Variable(name,fn) }` + `Assembler`；落盘策略 `ProjectSystemPrompt(rendered, inHistory, startsSeries)`；快照 `ProjectRuntimeSnapshot(current, sections)`。
- 触发点：section/context 每步装配一次；runtime snapshot 在 pre-step 注入且请求前 `refreshContext`；提醒在 pre-step/post-execute；skill 目录在 pre-step；plan/goal 在各自 pre-step。
- 降级路径：skill snapshot 不完整 → 不发目录；存储/加载失败 → warn 且保留旧状态；同一 target 的配置错误只警告一次。
- 幂等/去重：system prompt 文本未变不发事件；snapshot 文本 same → 不发；skill digest same → 不发；time-context 用时间间隔；repeat guard 用链计数 + 用户插话重置。缓存稳定性：固定 order 表 + 动态事实下沉到 context 消息而不是 section。

---

## 7. 跨机制不变量清单（移植时的验收点）

1. **日志唯一真源**：messages、tools、prompt、usage 全部可由日志重放；任何缓存可丢弃重建。
2. **替换语义显式**：所有“压缩/剪枝/图片省略/运行时快照替换”都必须用显式 replace 事件 + `sourceEventSeqs` 引用被遮蔽节点。
3. **事务化压缩**：start 先落盘作为锁；失败恰一次 end（带 error）；成功 end 在替换之后。
4. **安全边界**：不拆 system 提示（node 0 保护）、不拆 tool-call/result 配对、尾部按价格累积保留。
5. **收缩可验证**：摘要（纯文本启发式）必须小于被替换区间（路由价）。
6. **有界重试 + 进度证明**：压缩/overflow 恢复只在 replace 代数前进后重试，且受配置上限约束。
7. **幂等**：system prompt 同文本不发、snapshot 同文本不发、skill 目录同 digest 不发、time-context 间隔内不发、repeat guard 只在阈值点发。
8. **降级可见**：spill 失败保留 inline 并 warn；压缩技能配置错误只 warn 不崩 turn；skill 快照不完整就不发布目录。
9. **缓存稳定**：sections/contexts 的 order 由中心表分配；动态事实只进 context 消息；in-history 更新只在路由声明支持时使用。

---

## 8. 对 chagent 的映射与差距线索（供后续设计参考）

> 以下为代码对照后的线索，不构成结论；chagent 侧已有 P-08/P-09 等机制，差距是“结构”而非“有没有”。
> **注意**：调研期间 chagent 工作树正被并行修改（`context_budget.go`、`context.go`、`prestep` 等均有改动），故 chagent 侧以**文件 + 符号名**为准，行号仅为调研时的快照；DSH 侧行号可复核。

1. **计量粒度**：chagent 目前是纯字符/权重启发式（`chagent: pkg/tokenizer/estimator.go` 的 `EstimateMessageTokens`/`EstimateToolDefsTokens`，CJK 加权）+ 静态窗口预算（`pkg/agent/context_budget.go` 的 `isOverContextBudget`）。DSH 的差异是「provider usage 锚点 + 路由图片/文件定价 + surface 增量 O(1) 折叠」（`packages/llm/token-meter/src/index.ts:151-196`, `route-pricing.ts:32-75`）。缺少这层会导致图片/附件成本不可见、压缩阈值只能拍脑袋。
2. **压缩安全边界**：chagent 的切点仍是 Turn 起点（`pkg/agent/context_budget.go` 的 `isTurnBoundary/parseTurnBoundaries/findSafeBoundary`；已能排除注入型 context 消息，但仍是“轮”粒度），同一 Turn 内超大工具轮无法安全压缩；DSH 用 tool-call/result 配对平衡作为切点，允许在 turn 内已闭合的 step 处压缩（`packages/compaction/compaction/src/tool-pairing.ts:29-126`）。移植时应把“安全切点”做成独立纯函数服务，而不是继续扩 `parseTurnBoundaries`。
3. **压缩落点/回放**：legacy 路径把摘要拼进 system prompt（`chagent: pkg/agent/context.go` 的 `contextSummaryPrefix` 与 `BuildMessagesFromPrompt`），会随每次摘要变化破坏前缀缓存；虽然已有 P-08 runtime snapshot/P-09 task state 消息（`pkg/agent/workspace_context_runtime.go` 的 `injectRuntimeContext`/`injectTaskStateContext`），但摘要本身没有“作为 user 消息 replace 旧区间”的语义（对照 `region.ts:506-509`）。需要引入 surface replace 事件，才能做到压缩可重放、可审计。
4. **触发与收敛**：chagent 有 proactive/retry/summarize 三类触发（`pkg/agent/event_payloads.go` 的 `ContextCompressReason`，调用点在 `pipeline_setup.go`/`pipeline_llm.go`/`pipeline_execute.go`/`pipeline_finalize.go`），但 retry 侧主要是丢轮重试（`context_budget.go` 的 `trimHistoryToFitContextWindow`）与压缩路径 `pkg/agent/context_legacy.go` 的 `forceCompression`（先 `PruneToolResultsWithSpill` 做确定性裁剪并把原始内容落盘到 spill，再按 Turn 丢弃）；缺少 DSH 的「模型切换前必须压到阈值」「overflow 恢复用 replaceGeneration 证明确有进度」「压缩尝试次数有界」三条收敛规则（`compaction-basic/src/index.ts:183-203, 225-256, 353-370`）。
5. **prompt 插入与缓存稳定**：chagent 每次把 static + dynamic + summary 拼成单个 system message（`pkg/agent/context.go` 的 `BuildMessagesFromPrompt`），skill 目录内联在静态提示中（同文件 `buildSystemPromptParts` 的 `capability.skill_catalog`）；对照 DSH 的「system prompt 作为 surface 节点 + in-history 更新 + skill 目录 digest 重发布」（`packages/core/agent-loop/src/runtime-context.ts:68-116`, `packages/skill/tool-skill/src/index.ts:215-253, 330-337`）。差距是动态事实会污染前缀缓存，且缺少目录/正文两级渐进披露的“替换而非追加”语义。

> 调研时工作树中还有并行在做的未跟踪实现（`pkg/agent/context_pressure.go`、`tool_result_prune.go`、`prestep.go`、`prompt_policy.go`、`loop_guard.go` 等），可能正在收窄上述第 1/3/4/5 条差距；引用前请以当时实际代码为准，避免重复实现。

---

## 9. 已验证的 DSH 路径清单（本文证据来源）

| 主题 | 已读取文件 |
|---|---|
| 计量/预算 | `packages/llm/token-meter/src/{index,types,estimate,route-pricing,surface-fold,usage-projection,measurement-projection}.ts` |
| 窗口/输出配置 | `packages/llm/llm/src/{index,types,error}.ts`，`packages/llm/llm-deepseek/src/{defaults,models,config,model-info,listing,transport}.ts`，`packages/llm/llm-pi-ai/src/{adapter,stream,context,config}.ts` |
| 压缩 | `packages/compaction/compaction/src/{index,types,tool-pairing,checkpoint}.ts`，`packages/compaction/compaction-basic/src/{index,config,region,summarizer,types}.ts`，`packages/compaction/compaction-tool-result-pruner/src/{index,config,types}.ts`，`packages/compaction/compaction-image-offload/src/{index,projection}.ts`，`packages/compaction/command-compact/src/index.ts` |
| spill | `packages/spill/spill/src/{index,types}.ts`，`packages/spill/spill-policy/src/{index,retention,notice,types}.ts`，`packages/spill/spill-local/src/{index,store,cleanup}.ts` |
| 会话装配 | `packages/core/session/src/{index,surface,types,repair,fork,preparation,tool-history,request-header}.ts`，`packages/session/session-projection/src/{index,types}.ts`，`packages/core/agent-loop/src/{agent,runtime-context,index}.ts` |
| prompt | `packages/core/system-prompt/src/index.ts`，`packages/context/{time-context,harness-dev-context,session-reference,agent-instructions,file-reference}/src/*`，`packages/skill/tool-skill/src/index.ts`，`packages/plan/plan-mode/src/index.ts`，`packages/goal/{tool-goal,goal-round-driver}/src/*`，`packages/todo/tool-todo/src/index.ts`，`packages/guard/{repeat-tool-reminder,timeout-policy}/src/index.ts` |
| 中文导航（仅导航，结论以代码为准） | `docs/subsystems/{token-meter,compaction,system-prompt,session,conversation,skills,spill,core}.zh.md` |

> 说明：`.zh.md` 文档与代码在个别默认值/措辞上可能滞后；本文所有数值与行为均以上表源码为准。