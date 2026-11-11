# DSH 模型执行与 Agent 循环机制（面向 chagent 的移植研究）

> 研究对象：`/Users/wudaorui/code/deepseek-harness`（DSH，TypeScript / pnpm monorepo / Cordis 插件运行时）。
> 本文所有证据路径均相对 DSH 仓库根。只读研究，未修改 DSH 任何文件。
>
> 阅读约定：
> - **Turn（轮）**：一次用户可见的输入批次驱动的一段连续工作；以 `turn/start`…`turn/end` 成对持久化。
> - **Step（步）**：一次模型请求；以 `step/start`…`step/end` 成对持久化。一个 turn 可含任意多个 step。
> - **Request / Attempt（请求 / 尝试）**：一个 step 内可因重试产生多次尝试；只有成功的一次落 `assistant/message`，失败的落 `assistant/attempt`。
> - **Session log（会话日志）**：唯一真源。模型消息历史、工具历史、prompt、inbox、重试状态全部由事件日志折叠（fold）或投影（projection）得出，而不是内存里另存一份。

---

## 1. 总体架构：事件日志 + 服务注册 + 扩展点

DSH 的 agent 外壳很薄，真正的机制在"事件 + 投影"里：

1. **服务注册**（Cordis `Service`）：`ctx.agents`、`ctx.sessions`、`ctx.llm`、`ctx.tools`、`ctx.systemPrompt`、`ctx.sessionProjections`、`ctx.jobs`、`ctx.goals` 等。
2. **会话事件日志**：`Session.append(type, data, surfaceOp?)` 落盘；`surfaceOp`（`append` / `replace`）决定该事件是否成为"模型可见历史"的一个节点（surface node）。见 `packages/core/session/src/index.ts:999`（`deriveMessages()`）与 `packages/core/session/src/types.ts:294-400`（事件表）。
3. **投影**（`sessionProjections.register(...)`）：每个子系统声明 `key + init + apply`，从日志增量折叠出纯状态（inbox、goal、token 用量、重试计数、plan mode、todos…）。重启后只重放日志即可恢复。
4. **扩展点**：`agent/pre-step`（waterfall，可 reject）、`agent/request`（waterfall，改请求配置）、`agent/request-error`（waterfall，决定是否重试）、`agent/turn-stopping`（serial）、`llm/stream`（waterfall，包住适配器流）、`tools/pre-execute|execute|post-execute`（waterfall）、`system-prompt/assemble`（waterfall）、`session/event`（emit）。

Go 移植要点：
- 一定要先定"日志 + 折叠"这一层，否则后面的 prompt 注入、compaction、崩溃修复、重试都会各自维护一份易失状态，无法保证重启后一致。
- 扩展点用"命名钩子 + 显式优先级（prepend/append）+ 返回决策 union"表达，比 Go 里到处传 `*Agent` 硬编码 if 更可控。

---

## 2. 主 Agent 循环（`packages/core/agent-loop`）

### 2.1 阶段状态机

`packages/core/agent-loop/src/agent.ts:49-58`

```ts
type Phase =
  | { kind: 'idle'; lastTurn: number }
  | { kind: 'maintenance'; abort: AbortController; lastTurn: number; wakeRequested: boolean }
  | { kind: 'running'; abort: AbortController; turn: number; step: number; wakeRequested: boolean }
```

- `idle`：无驱动。
- `maintenance`：非 turn 的独占任务（手动 compaction 等），见 `runMaintenance()`（`agent.ts:192-213`）；期间仍可 `wakeRequested` 在结束后补一次驱动。
- `running`：持有本 turn 的 `AbortController`；`turn`/`step` 是持久化进度。

状态转换唯一入口 `setPhase()`（`agent.ts:154-161`）会发 `agent/status` 事件 —— 这是 goal driver / compaction / UI 的触发信号。

### 2.2 入队与唤醒：inbox 两个队列

`agent.ts:163-190`：

```ts
send(msg, target, wakeup)      // target: 'next-turn' | 'next-step'
followup(msg) = send(msg, 'next-turn', true)   // 新开一轮
steer(msg)    = send(msg, 'next-step', true)   // 下一步之前、并唤醒（运行中插入）
inject(msg)   = send(msg, 'next-step', false)  // 只排队，不唤醒
cancel(cause, {keepInbox})                     // 默认清空 inbox 并 abort 当前活动
```

队列本身是**持久化投影**：`ReactLoopInbox`（`packages/core/agent-loop/src/inbox.ts:80-230`）每次 splice 都 `session.append('agent/inbox/spliced', …)`；claim 时把 `next-step` 整批 + 可选一条 `next-turn` 取走（`inbox.ts:108-118`）。这样崩溃重启后未消费的输入还在。

关键边界（`agent.ts:163-170`）：若在"已 abort 但尚未收敛"的活动上 `wakeup`，消息被改投 `next-turn`（不能加入已死的一轮）。这是"steering 消息不丢失"的核心不变量。

Go 移植要点：
- 用两个切片 + 一个持久化 splice 事件（`target/start/deleteCount/inserted`）表达 inbox，而不是 channel。channel 无法回放、无法在重启后恢复、也无法拒绝"已 abort 的 turn"。
- `followup` 与 `steer` 的差别只在目标队列与是否唤醒；`inject` 是"只排队"，用于后台任务通知等不应打断模型的情形。

### 2.3 驱动器收敛

`agent.ts:223-274`：

```ts
wakeDriver():
  if phase != idle:  // maintenance 或被 abort 的 run
     if reason != disposed && (maintenance || wakeAfterAbort) -> phase.wakeRequested = true
     return
  phase = running(turn=lastTurn, step=0)
  kick()

kick():
  try { while (await this.turn()) {} }   // 一个 driver 连续消费积压的 next-turn
  catch { /* 错误已在 turn 边界上报 */ }
  finally { phase = idle(lastTurn); if (wakeRequested && inbox.hasPending) wakeDriver() }
```

**一个 driver 会把队列里所有 next-turn 连续消化完**（`turn()` 返回 `true` 表示还有 pending），而不是每轮起一个 goroutine。并发入口只有 `wakeDriver` 这一处，`idle` 判定天然串行。

### 2.4 Turn 生命周期

`agent.ts:320-426`（省略注释）：

```ts
async turn(): Promise<boolean> {
  const turn = phase.turn + 1
  session.append('turn/start', { turn })
  let turnEnds: TurnEndReason | null = null
  let target: InboxTarget = 'next-turn'
  try {
    while (true) {
      const step = phase.step + 1
      const decision = await preStep(target, { turn, step })       // ① 收集本步输入
      if (decision.kind === 'reject') { turnEnds = { kind: 'blocked' }; return false }
      if (turnEnds && decision.messages.length === 0) break         // ② 无事可做
      if (phase.step === 0 && decision.messages.length === 0) {     // ② 空首步不花模型调用
        turnEnds = { kind: 'completed' }; return false
      }
      session.append('step/start', { turn, step })
      phase.step = step
      const toolRecovery = new ToolCallRecovery()                   // ③ 未回填工具结果的兜底
      const stopRecovery = ctx.on('session/event', (s, e) => { if (s === session) toolRecovery.observe(e) })
      try {
        const stepEnd = await step(decision)                        // ④ 模型 + 工具
        if (turnEnds?.kind !== 'max-tokens') turnEnds = stepEnd     //    max-tokens 粘性
      } catch (e) {
        for (const ev of toolRecovery.results())                    // ⑤ 异常也要补齐 tool/result
          session.append('tool/result', ev.data, { surfaceOp: 'append', … })
        throw e
      } finally {
        stopRecovery()
        session.append('step/end', { turn, step })
        decision.toolSet.dispose()                                  // 工具可见性准入随步关闭
      }
      if (turnEnds && inbox.nextStep.length === 0) {
        await dispatch.serial('agent/turn-stopping', { turn, signal })  // ⑥ 收尾钩子（可再注入）
      }
      if (turnEnds && inbox.nextStep.length === 0) break
      target = 'next-step'                                          // ⑦ 后续步只消费 next-step
    }
  } catch (e) {
    turnEnds = abortedCancelCause(signal) ? { kind: 'aborted', reason } : { kind: 'error', error: … }
    throw e
  } finally {
    session.append('turn/end', { turn, reason: turnEnds! })         // ⑧ 一定有 turn/end
  }
  if (!inbox.hasPending) return false
  phase.abort = new AbortController()                               // ⑨ 新一轮用新 controller
  phase.step = 0
  return true
}
```

要点：
- **`turn/end` 必须写**（`finally`），`reason` 是封闭 union：`completed | aborted | blocked | error | max-tokens | interrupted | forked`（`packages/core/session/src/types.ts:206-238`）。
- `max-tokens` 是**粘性**的：后续步正常完成也不能把本轮降级为 completed（`agent.ts:361-366`）。
- `turn-stopping` 是最后注入机会（hooks / deliverables 用：`packages/deliverables/workspace-changes/src/index.ts:155`、`packages/experimental/hooks-claude-code/src/index.ts:275`）；它跑完后如果 inbox 又有 next-step，turn 会继续而不是结束。
- 工具准入 `toolSet` 的生命周期是"一步"，`step` 结束即 `dispose()`（`agent.ts:383-386`）。

### 2.5 Step：一次请求的完整闭环

`agent.ts:428-603`，按顺序：

```ts
async step(decision) {
  const renderedPrompt = renderPrompt(decision.assembly)      // ① 本步渲染一次 system prompt
  let firstAttempt = true
  while (true) {                                             // ② 重试循环（每个 attempt 重建请求）
    const { config, preparedCall } = await prepareRequest(turn, step, decision.selection, signal)
    const currentContext = systemPrompt.refreshContext(assembly, …)   // ③ 动态 context 重算
    const sections = renderContextSections(currentContext)
    const context = runtimeContext.project(joinContextSections(sections), sections)
    const startsRequestSeries = firstAttempt && decision.startsRequestSeries === true
    const commits = systemPrompt.project(renderedPrompt, {          // ④ system prompt 落盘策略
      inHistory: preparedCall?.systemPromptUpdate === 'in-history',
      startsSeries: startsRequestSeries
        || this.requestSurfaceGeneration !== this.session.surface.contentGeneration
        || (preparedCall?.toolUpdate === undefined && this.toolsChanged(assembly.tools)),
    })
    for (const { message, intent } of commits)
      session.append('system/message', { turn, step, message }, intent)
    if (firstAttempt)                                          // ⑤ 本步认领的用户输入落盘（只一次）
      for (const m of decision.messages) … append('user/message', …)
    if (context) session.append('user/message', context, { surfaceOp: 'append' })
    const request = buildRequest(config, preparedCall, assembly.tools, {turn, step}, startsRequestSeries, signal)
    const live = new AssistantStreamAttempt(…)                  // ⑥ 流式帧 + 累积器
    for await (const chunk of stream) { signal.throwIfAborted(); live.push(chunk) }
    if (finish.kind === 'error' || finish.kind === 'aborted') {  // ⑦ 失败：落 attempt，问 request-error
      session.append('assistant/attempt', …)
      const action = await dispatch.waterfall('agent/request-error', {…, failure, retryPolicy})
      if (action?.kind !== 'retry') throw new LlmError(finish.failure…)
      continue                                                  // 重试 = 下一次 attempt
    }
    session.append('assistant/message', { message, usage, stream }, { surfaceOp: 'append' })
    if (finish.kind === 'max-tokens') return { kind: 'max-tokens' }
    const toolCalls = message.content.filter(b => b.type === 'tool-call')
    if (toolCalls.length === 0) return { kind: 'completed' }     // ⑧ 无工具调用 = 本轮结束信号
    const result = await tools.execute({ calls, signal, agent, log: {turn, step},
      maxParallelToolCalls: agentLoop.config.maxParallelToolCalls.get(), toolSet: decision.toolSet })
    for (const extra of result.additionalContexts)               // ⑨ 工具附加上下文进 next-step
      inbox.splice('next-step', inbox.nextStep.length, 0, [extra])
    return result.concluded ? { kind: 'completed' } : null       // ⑩ null = 同一步里继续下一次尝试
  }
}
```

**停止条件（完整清单）** —— 这是与 chagent 差异最大的地方：
1. 模型没有返回 tool-call → `completed`。
2. 某个工具声明"本步即结论"（`ToolExecutionResult.concludesTurn`，`packages/core/tools/src/contracts.ts` + `pipeline.ts` 的 `concludeTurn()`）→ `completed`。
3. finish 原因为 `max-tokens` → `max-tokens`（粘性）。
4. 无可用输入（空首步 / reject）→ `completed` / `blocked`，不产生模型调用。
5. 取消 → `aborted`。
6. 错误（LlmError / 工具激活失败 / 其它）→ `error`。

**DSH 没有"每轮最大 step 数"配置**。搜索 `maxSteps/maxIterations` 无命中；唯一相关配置是 `maxParallelToolCalls`（`packages/core/agent-loop/src/index.ts:295-307`，默认 10）。防止失控靠三层：外层 turn 只能由 inbox 驱动（有界输入）、`max-tokens` 粘性、以及取消/超时（`guard/timeout-policy`）。需要长跑时用 goal round driver 跨 turn 续跑（见 §5.2），而不是提高迭代上限。

---

## 3. 模型执行层

### 3.1 请求构建：顺序与不可变

`buildRequest()`（`agent.ts:649-737`）——一次请求的权威顺序：

1. `canonicalHeader({ config, adapterDefaults?, tools? })`，与上一份 header 比较后写 `request/header`（`reason: initial|resume|change|series`，`startsSeries` 标记）。
2. 工具集合变化时追加 `developer/message`（`tool-addition` / `tool-removal`，带 `headerSeq`），让模型知道工具清单变了（`agent.ts:685-699`）。
3. `request/context`（provider/model/contextWindow/systemPromptUpdate）变化时记录路由元数据。
4. `session.deriveMessages()` 得到模型消息数组（**由 surface 折叠而来，已含 system prompt 节点与运行时 context 节点**）。
5. `Object.freeze` + 对每个消息 `deepFreeze`（`frozenMessages` WeakSet 去重），`markAgentLoopRequest(request)` 打标。
6. 最终 `GenerateOptions = { ...header.config, messages, toolHistory: session.toolHistory(), tools?, sessionId, signal }`。

由此得到两条硬规则：
- **请求内容 = 会话日志的纯函数**。监听 `llm/stream` 的中间件只允许读、不允许改（`packages/llm/llm/src/index.ts:63-75` 的注释明确"deep-frozen，mutation throws"）。这让重试/重放天然幂等。
- **system prompt 不是 `GenerateOptions.system`**（那是给 one-shot 调用者的）；loop 把它作为 surface 节点 0 的 `system/message` 放进 `messages`，由适配器映射到 provider 的 system 槽（`packages/llm/llm-deepseek/src/serialize.ts:46-160`）。好处：system prompt 的变更与固化策略可以独立推导（§6.2）。

### 3.2 provider 抽象与路由

- `LlmAdapter` 抽象类（`packages/llm/llm/src/index.ts:191-302`）：`providerInfo / listModels / resolveModel / prepareCall / stream / providerRetryPolicy / imageRequestPricing`。
- 注册表按 `provider` 字符串路由，一个 provider 一个 adapter 实例；`registration(provider)` 缺失抛 `LlmError(code: 'NO_ADAPTER')`（`index.ts:1024-1027`）。
- **`prepareCall` 返回一次性句柄**（`index.ts:956-1020`）：在记录 header 前绑定"这一次调用的 adapter 注册 + 解析后的 modelInfo + 默认值"，因此 HMR/热更新不会把 A 适配器的能力结果配到 B 适配器的请求上。句柄 `stream()` 只能派发一次，且校验传入 config 与已解析 config 深度相等（否则 `INVALID_PREPARED_CALL`）。
- `adapterDefaults`：区分"调用者显式选择"和"适配器兜底"。`requestProposal()`（`agent.ts:264-272`）在下一步提议配置时会把兜底字段删掉，避免把默认值错误固化成用户选择。
- 路由选择是**显式**的：`AgentModel`（`packages/core/agent/src/model-selection.ts`）持久化 `lastUsed / pending`；`agent/request` waterfall 可以改配置，但改 provider/model/reasoningEffort 会直接报错（`agent.ts:628-633`），必须在 pre-step 之前用 `agent.model.select()`。**DSH 没有"失败后自动 fallback 到另一个模型"的内置机制** —— 只有重试（同路由）与 compaction-on-model-switch（§3.6）。降级必须由上层显式切模型。

### 3.3 流式协议

`packages/llm/llm/src/types.ts:470-482`：

```ts
type StreamChunk =
  | { type: 'block-start'; index; blockType }
  | { type: 'text-delta'; index; text }
  | { type: 'reasoning-delta'; index; text }
  | { type: 'tool-call-delta'; index; id; name?; argumentsDelta }   // 参数是字符串增量
  | { type: 'block-end'; index; block: ContentBlock }
  | { type: 'usage'; usage: TokenUsage }
  | { type: 'finish'; reason: FinishReason; replayState? }
```

`FinishReason = stop | tool-calls | max-tokens | aborted{failure} | error{failure}`（`types.ts:156-165`）。适配器把抛出异常**转成终止 chunk** 而不是 throw（`index.ts:1185-1195` `adapterFailureChunk`），只有中间件/清理/消费端错误才抛出 —— 这样"失败"也走统一的落盘路径。

`AssistantStreamAttempt`（`packages/core/agent-loop/src/assistant-stream.ts:17-140`）同时做三件事：
1. `AssistantStreamAccumulator`：把 chunk 打包成可持久化的紧凑记录（`text-chunks`/`tool-call-chunks` 的 dt+texts 数组），落 `assistant/message.stream`，用于精确回放。
2. `BlockAssembler`：增量组装成 `ContentBlock[]`。
3. 实时帧 `agent/assistant-stream`（`start/chunk/end` + revision），供 UI 流式渲染；`end` outcome 只有在**持久化成功后**才发 `committed`，否则 `abandoned`（`assistant-stream.ts:90-121`）。

### 3.4 tool_call 解析

`packages/llm/llm/src/assembler.ts`：
- `tool-call-delta` 只累积 `argumentsDelta` 字符串；**不做增量 JSON 解析**（避免半截 JSON 的容错地狱）。
- `block-end` 提供的 `block` 是权威值；delta 后到的杂散块被忽略（"first close wins"，`assembler.ts:76-84`）。
- 未闭合的 tool-call 在 `block-end` 缺失时用累计值合成，id 缺失则生成 `call-{index}`（`assembler.ts:110-124`）。
- `finish.kind === 'max-tokens'` 时**丢弃所有 tool-call 块**（`assembler.ts:133-145`）—— 被截断的参数不可执行，宁可不调用；同时 replay envelope 的 blocks 同步裁剪，保证"发出的块"和"回放的块"永远一致。
- `interruptedBlocks()`（`assembler.ts:169-180`）在中断时只保留非空白的 text/reasoning，**丢弃 tool-call**：中断发生在派发之前，保留它就必须伪造一个工具结果。

### 3.5 并行工具调度

`packages/core/tools/src/batch.ts:50-235`（`executeToolBatch`）：

```ts
planned = calls.map(parseArguments)                 // 参数解析失败保留原始字符串，交给工具自己报错
while (next < planned.length) {
  const mode = pipeline.executionMode(first.exec).kind // 'parallel' | 'exclusive'
  const group = mode === 'parallel' ? planned.slice(next) : [first]   // 排他调用 = 屏障
  const outcome = await runGroup(pipeline, settlement, maxParallel, group, mode, signal)
  next += outcome.consumed; concluded ||= outcome.concluded
  if (outcome.aborted) { for (rest) settlement.skip(call.block); return settlement.result(concluded) }
}
```

`runGroup`（`batch.ts:117-232`）：
- **滚动并发池**：`inFlight.size < maxParallelToolCalls` 就继续启动；启动前重新读一次该调用的执行模式（工具注册表可能在步内变化），若后面的调用变成排他则停止补位，留给下一个屏障。
- **派发可重叠，提交必须模型序**：`commitReady()` 只从 `committed` 起连续推进，`pipeline.complete()` 之后才写 `tool/result`。
- **abort 语义**：不再启动新调用，等待已启动的收敛，然后给所有未启动的调用写**合成错误结果** `"tool call aborted before dispatch"`（`batch.ts:224-231`, `skip()` 268-283）。这是关键不变量：**日志里每个 tool-call 都必须有 tool-result**，否则回放/续跑会失配。
- **调度器内部失败**：`ToolBatchExecutionError(error, partial)`（`packages/core/tools/src/executor.ts:89-95`）携带已提交结果与 context；step 的 catch 会把这些 context 补进 inbox，并在记录恢复结果后抛出（`agent.ts:591-597`）。

工具流水线（`packages/core/tools/src/pipeline.ts`）：
1. `createExecution`（:120-186）：快照参数（非 JSON 安全直接 TypeError）、拒绝未知工具、记录 `deferContext/concludeTurn` 能力。
2. `prepare`（:190-244）：`tools/pre-execute` waterfall → 允许/拒绝/取消/询问（`ask` 走审批服务，无审批通道即拒绝）→ `checkGuards` → 返回 `dispatch` 或 `post-result`。
3. `dispatch`（:295-333）：`tools/execute` waterfall（超时/重试/指标包装层在此）→ 融合 caller signal 与 wrapper signal → 调工具体。
4. `complete` → `postExecute`（:486-540）：`tools/post-execute` waterfall，返回 `accept | block(feedback) | 替换内容`，可附 `additionalContexts`。
5. `tools/result` emit（冻结结果，观察者失败被吞掉）。

### 3.6 错误分类、重试与降级

**分类**：所有模型失败归一为 `LlmFailure { message, code, status?, providerRetryAfterMs?, requestId?, offloadImages? }`（`packages/llm/llm/src/adapter-failure.ts:18-40`）。只有 Harness 自己定义的 code 才可信，第三方 SDK 的 code 一律降为 `UNKNOWN`（同文件 `harnessErrorCode`）。典型 code：`AUTH`、`RATE_LIMIT`、`SERVER`、`TIMEOUT`、`TRANSPORT`、`EMPTY_RESPONSE`、`CONTEXT_WINDOW_EXCEEDED`、`QUOTA`、`NO_ADAPTER`、`INVALID_CREDENTIAL`、`IMAGE_OFFLOAD_REQUIRED`（`error.ts`）。

**重试策略由 provider 拥有**（`packages/llm/llm/src/retry-policy.ts:14-19`）：

| 项 | 默认 |
|---|---|
| mode | `normal`（另有 `always`） |
| maxRetries | 5 |
| initialDelayMs / maxDelayMs | 500 / 10_000 |
| jitterRatio | 0.1（对称抖动） |
| retryableCodes | `EMPTY_RESPONSE, RATE_LIMIT, SERVER, TIMEOUT, TRANSPORT` |

**执行者是可选插件** `packages/llm/llm-retry/src/index.ts`：
- 挂 `agent/request-error` waterfall（:239-247）。
- `normal` 模式：code 不在 `retryableCodes` → 交给下游（默认不重试）；`maxRetries` 用尽 → 下游。
- `always` 模式：先跑下游决策，若下游不重试也重试，直到成功/取消/dispose。
- 延迟：`providerRetryAfterMs` 优先；超过 `maxDelayMs` 时 normal 模式直接放弃（不睡过长），always 模式退回本地退避。`localDelay()` 指数退避 + 抖动（:66-72）。
- **每次重试先持久化** `llm/retry`（含 retryId/turn/step/policyKey/delayMs/failure），延迟结束再写 `llm/retry-started`（:160-200）。投影按 `provider+policyKey` 记 `{retry, retryId}`；`step/start`/`turn/end` 清空（:126-136）。→ 崩溃恢复后不会把已经退避过的次数算丢。
- 返回 `{ kind: 'retry' }` 后，step 的 `continue` 会**重新走 prepareRequest/buildRequest**（新的 attempt、新的 header 记录），而不是复用旧请求对象。

**上下文溢出（"降级"的另一半）** 在 `packages/compaction/compaction-basic/src/index.ts:216-267`：`agent/request-error` 收到 `CONTEXT_WINDOW_EXCEEDED` → 触发一次 `context-overflow` compaction；只有摘要**真的落地**（surface `replaceGeneration` 前进）才返回 retry，最多 `maxOverflowRetries` 次。切模型时也会先做一次压力 compaction（:184-215），失败则抛 `MODEL_SWITCH_COMPACTION_FAILED` 并保留旧模型。

**模型切换提示**：`installModelSelection()`（`packages/core/agent/src/model-selection.ts:159-186`）在 pre-step 末尾追加一条"模型已切换"用户消息 —— 让模型知道自己换了路由，而不是静默换模型。

### 3.7 usage / token

- `TokenUsage` 字段**互斥**：`inputTokens` 只含未缓存输入，缓存读/写单列，`totalTokens` 可选（`packages/llm/llm/src/types.ts:167-189`）。DeepSeek 的 `prompt_tokens` 会把缓存命中减出去。
- 流里的 `usage` chunk → `BlockAssembler.usage` → `assistant/message` 事件的 `usage` 字段（`agent.ts:567-576`）。
- `TokenMeter`（`packages/llm/token-meter/src/index.ts:87-230`）是一个**投影服务**：折叠 `assistant/message.usage` 与 surface 节点估算，给出 `contextPressure { pressureTokens, contextWindow }`、`measureSurface`、`estimateMessage`；compaction 的阈值判断读它（`docs` 与 compaction 配置 `thresholdRatio/retainRatio/headroomTokens`）。
- 每轮/会话的用量统计由 `session-stats`、`turn-usage` 等投影另算（`packages/llm/token-meter/src/turn-usage.ts`）。

### 3.8 abort / interrupt

四层信号：
1. **factory lifetime**：插件卸载时 abort，reason `agent loop is not active`（`index.ts:61-140`）。
2. **turn/activity**：`phase.abort`，每个 turn 新建（`agent.ts:421`）。
3. **step 内每个 await 之后** `signal.throwIfAborted()`（`agent.ts` 多处）。
4. **工具/流内部**：工具拿到 `exec.signal`，适配器拿到 `request.signal`。

取消原因用封闭 union：`user | parent | hook{reason} | disposed`（`packages/core/session/src/types.ts:194-201`）；turn/end 记录为 `{kind:'aborted', reason}`。

中断后的落盘（`agent.ts:492-533`）：
- **流已开始**：把已到达的 text/reasoning 写成 `assistant/message`，带 `interrupted: true` + 已收 usage —— 用户已经看到的文本不会消失。
- **流未开始**：写 `assistant/attempt`（无 message）。
- 工具批：见 §3.5 的合成结果。
- resume 时：`interruptedTurnClosers(persisted)` 补 `tool/result`（`TOOL_NOT_STARTED` / `TOOL_OUTCOME_UNKNOWN` 两种措辞）+ `step/end` + `turn/end{kind:'interrupted'}`（`packages/core/session/src/repair.ts:57-100`，调用点在 `packages/core/agent-loop/src/index.ts:801-806`）。

Go 移植要点：`context.Context` 不够表达"取消原因"。需要一个 `CancelCause` 枚举跟着 `context.Cause`；并且**取消后必须继续把已产生的部分写成持久记录**，否则 log 里会出现"有 tool-call 无 tool-result"的不平衡尾巴，重启器必须能修。

---

## 4. Session 生命周期与崩溃修复

- 创建/恢复统一走 `AgentLoop.create / resume`（`packages/core/agent-loop/src/index.ts:668-850`）。resume 的顺序很讲究：
  1. **先拿写句柄**（`persistence.open(id, 'write')`）—— 用"写所有权"排他，而不是先读后写。
  2. 冷读全量日志 → `interruptedTurnClosers()` 计算合成事件 → 通过同一个 handle 一次批量 append（"语义修复是 agent 层的职责，持久化层只保证物理有效"）。
  3. `SessionPreparation.create(sessions.prepare(id, { seed:[...persisted, ...closers], meta, inheritedEventCount }))`。
  4. 发布 agent，`lastTurn` 由 `agents.turnBoundaryOf(session).lastTurn` 得出（:569）。
- `deriveMessages()`（`packages/core/session/src/index.ts:999-1035`）：
  - 只走 `surface.nodes`（带 `surfaceOp` 的消息事件），所以 chunk/turn 边界等事件天然不进历史。
  - compaction 的 `replace` 会把被遮蔽节点从 surface 删除 → **历史真的变短**，而不是"在内存里拼一个摘要"。
  - 缓存按 `surface.contentGeneration` 失效：纯尾追加 O(新增节点)，替换则重建。
- 会话 fork：`packages/core/session/src/fork.ts`；在源 turn 未闭合处切分时用 `forked` 原因补 closer（`repair.ts` 的 `OpenTurnCloseCause`）。

Go 移植要点：
- 事件要有 `seq`（单调）+ `time`；合成修复事件复用最后一个真实事件的 time，保证"不发明未来时间"。
- `surfaceOp` 概念（append/replace(startSeq,endSeq)）是 compaction 与 system prompt 归一化的公共基础设施，值得单独建模。

---

## 5. 各类 Agent 循环

### 5.1 主循环之外的第二种循环：subagent

- 子 agent 是**独立 session + 独立 agent**，不是同 session 的子循环：
  - `resolveChildDepth(parent, maxDepth)` 用父 session 持久化的 depth 做单调下界，防止 resume 后越权（`packages/subagent/subagent/src/child-agent.ts:44-62`）。
  - 子 agent 继承父的最新 `requestHeader` 路由（provider/model/effort）+ `maxTokens`，并把自己 `subagentDepth` 写进 options（`child-agent.ts:75-137`）。
  - 子会话的 runtime context 里注入"你在为谁工作/深度/汇报方式"（`child-agent.ts:205` `childCtx.systemPrompt.context`）。
- 完成检测：`manager.ts` 等 `child.whenIdle()`（:258, :1047），再从子会话/结构化输出里 `captureLocalResult()`（:1117-1162）；完成后把结果作为**一条 user 消息**投给父 agent（`parent.followup(message)`，:689），父的下一轮就看到结果。
- 结构化结果工具（`structured.ts`）一旦提交就关闭输入（`manager.ts:624` 的 `INPUT_CLOSED`）：子 agent 不能再被追加输入。
- 生命周期：`waitForChildren(agent)` 供 goal driver 等"后代全部收敛"；卸载时逐个 `cancel({kind:'parent'})` + `whenIdle()`。

### 5.2 第三种循环：goal round driver（同会话跨轮续跑）

`packages/goal/goal-round-driver/src/index.ts`。它不用新循环，而是**用 turn 当回合**：

```ts
drive(state):
  if needsCheckpoint: sessions.flush(agent.session); 复查就绪        // ① 先落盘再决定
  if attempt: attempt=undefined; needsCheckpoint=true; requested=true; return  // ② 上一轮已认领→再驱动一次
  goal = currentGoal(state)
  if !drivable(goal) return                       // phase==='active' && activation==='armed'
  if subagents: await waitForChildren(agent)      // ③ 后代在跑就别开新回合（只会等）
  if goal.roundsStarted >= goal.maxGoalRounds:
     goals.block(goal, {code:'round-limit'})       // ④ 回合上限→block，不是继续
     return
  msg = userMessage(renderGoalRoundPrompt(goal, round),
                    source={kind:'goal', goalId, revision, round})   // ⑤ 可见、可回放的续跑提示
  agent.followup(msg)                              // ⑥ 唤醒新一轮 turn
```

护栏（这是最值得抄的部分）：
- 触发源：`agent/status(idle)`、`goal/changed`、inbox inserted/claimed/discarded、`session/event(turn/end)`；全部走 `requestDrive` 合并到**一个 agent 局部串行 driver**（:224-247）。
- `agent/pre-step` 里做**租约校验**：queue 时的 `{goalId, revision, round}` 必须仍然匹配当前 goal 且 `round === roundsStarted+1`，否则 `{kind:'reject'}` 并把被误认领的其它消息放回 inbox（:380-460，`restoreOtherClaimed`）。校验通过后返回 `{...decision, startsRequestSeries:true}`，保证新回合是新请求序列。
- 回合上限 / max-tokens / abort / 排队失败 / prompt 被拒 都会把 goal 置 `blocked` 或 `disarm`，而不是无限重试（:182-195, :340-360, :420-455）。
- 宿主 pause 时若 agent 正在跑：`agent.cancel({kind:'user'}, {keepInbox:true})` 立刻停手；模型自己调 `update_goal` pause 则让本回合正常收尾（:305-315）。
- 每回合提示词写明"以当前 workspace / 工具结果 / 会话状态为准，不要相信之前的叙述"（`goal-round-driver/src/prompt.ts:10-24`）—— 对抗模型对陈旧上下文的自信。

> 与 §2.5 呼应：DSH 用"回合数上限 + 每回合无 step 上限"，而不是"单轮 step 上限"。这是可移植性最强的一个决策。

### 5.3 plan mode：把"模式"做成 prompt section + pre-step 事件

`packages/plan/plan-mode/src/index.ts`：
- 注册 system prompt section `plan:policy`，`order = PLAN_POLICY(500)`，文本由"当前 session 是否处于 plan mode"动态决定（:197-205）。
- 模式切换本身是**持久事件** `plan/mode`；但只在"下一个被接受的 pre-step"追加（:208-235），因为 pre-step 在 `Session.append` 发布之外，可以安全地在已打开的 turn 内写 log-only 事件。
- 退出用工具 `exit_plan_mode`（需要审批），批准后给模型一句"计划已批准，从下一步开始执行"（:270-300）。
- `/plan` 命令用 `agent.steer(...)` 把用户附带的描述塞进下一步（:250-262）。

### 5.4 后台 jobs 与 schedule

**jobs**（`packages/jobs`）：
- `JobRegistry` 契约（`jobs/src/index.ts:40-130`）：job id 可预测但**按 owner session 鉴权**（不是靠保密）；每个 job 一个**有界输出环**（ring），模型用游标读增量，观察者按绝对字节偏移读同一份字节；settlement first-wins；owner 销毁时取消并等待生产者。
- 通知策略（`jobs/tool-jobs/src/index.ts:355-411`）：owner **忙** → `owner.inject(message)`（放进 next-step，不打断当前步）；owner **闲** → `owner.followup(message)`（起新轮）。`job_output(wait:true)` 的超时返回 job 状态而不是 `TOOL_TIMEOUT`，避免模型误以为失败。
- 工具描述里直接给模型写"不要轮询、不要 sleep、不重要就 kill"的操作纪律（:319）。

**schedule**（`packages/schedule`）：
- Host 级持久定时器 + 一次交付一个事务：`schedule/resolve-agent` waterfall 解析目标 agent（`schedule/src/index.ts:74-83`）→ `agent.followup(reminderMessage)`（`schedule/src/runtime.ts:125-126`，"followup 同步写 inbox splice，所以随后的 flush 一定能看到"）→ `sessions.flush` → 记录 delivery history。
- 拒绝向 subagent 拥有的 session 交付（`index.ts:452-470`），因为子 agent 的输入是受控的。
- 周期任务（daily/weekly/cron/every）解析与去重、错过的 occurrence 只补最近一次，都在 `domain.ts`。

### 5.5 steering / inbox 的完整语义

| API | 目标队列 | 唤醒 | 典型使用 |
|---|---|---|---|
| `followup` | next-turn | 是 | 用户新消息、goal 回合、job/schedule 通知（闲时） |
| `steer` | next-step | 是 | 运行中插入用户补充（`/plan`、hooks、team） |
| `inject` | next-step | 否 | 工具附加上下文、job 通知（忙时）、模型切换提示 |
| `inbox.prepend` | next-step | — | goal/plan 需要把误认领消息放回队首 |

`claim()`（`inbox.ts:108-118`）的批语义：一个 step 认领**全部** next-step + 至多**一条** next-turn；因此 pre-step 看到的是"这批"输入，可以整体改写或 reject。被 reject 时，输入必须由调用方放回（goal driver 的 `restoreOtherClaimed` 是范例）。

### 5.6 其它循环形态（备查）

- `workflow` 工具：脚本化 fan-out（`subagent` 的批量编排），不是新调度器。
- `experimental/agent-team`：team lead + 多 teammate，共享任务板；steering 用 `send_message` 落到对方 inbox。
- 这些都在主循环之上复用 §2-§3，没有独立执行内核。

---

## 6. Loop 内的 prompt 注入点（按执行顺序）

### 6.1 system prompt 装配（每步一次）

`packages/core/system-prompt/src/index.ts:619-700`：

```
assemble(context{agent, scope, signal, modelSelection}):
  1. 取 scope 链 layer（不同层可遮蔽同名 section/context/variable）
  2. variables: global → 逐层 scope（最近者赢）
  3. sections/contexts: 按 name 合并（scope 遮蔽 global），sections 按 order 升序（同 order 按 name code-unit）
  4. toolProviders 逐个求值 → ToolSchema[]（structuredClone，deferLoading 透传）→ orderTools()
  5. waterfall('system-prompt/assemble', assembly, context)   // 最后一个改写文本/上下文的公共点
  6. completeSection 恢复：声明 complete 的 section 在 waterfall 之后重新成为唯一 prompt（不允许被改）
  7. runtimeContextSuppressed 时只保留 required 的 context
```

**中央序位表**（`system-prompt/src/index.ts:136-186`）—— 移植时可直接照抄成常量：

```
SECTION：HARNESS_IDENTITY -1000 | PERSONA_PREFIX 0 | PLAN_POLICY 500 | TEAM_POLICY 600 |
         PTC_ONLY 800 | FILE_REFERENCE 900 | TOOL_* 1000-3000 | MCP_SERVERS 3100 |
         TOOLS_SDK 5000 | DELIVERABLE_REFS 9000 | STRUCTURED_OUTPUT 9900 |
         HARNESS_SOURCE 10000 | WEB_SURFACE 10100 | PERSONA_SUFFIX 10200
CONTEXT：WORKING_DIRECTORY 100 | SANDBOX_POLICY 110 | APPROVAL_POLICY 115 |
         SUBAGENT_DELEGATION 120 | TEAM_IDENTITY 125 | HARNESS_SOURCE 200 | WEB_SURFACE 210
```

### 6.2 system prompt 如何落盘（表面节点策略）

`SystemPromptProjection.project()`（`packages/core/agent-loop/src/runtime-context.ts:93-116`）：

| 情形 | 行为 |
|---|---|
| 尚无 system 节点 | 追加（即使是空串，也占住节点 0） |
| 路由支持 in-history（`systemPromptUpdate === 'in-history'`）且未变 | 不写 |
| 支持 in-history 且变了 | **追加**一条新的 system/message（保留前缀 KV cache） |
| 路由不支持 in-history / 新序列 / 被清空 | **替换**节点 0 的文本，并把后续 system 节点置空 |
| 新序列但最新节点已经是这段文本 | 不写（移动到节点 0 会改前缀却不改 prompt） |

配套的 `request/header.startsSeries` 与 `startsRequestSeries`（pre-step 可声明）共同定义"请求序列"边界。

### 6.3 runtime context：可替代的快照

`RuntimeContextProjection`（`runtime-context.ts:119-169`）：

- 动态信息（时间、工作区、沙箱策略、审批策略、子 agent 委派说明、team 身份…）作为 **user 角色的快照消息**落盘，`source.kind = 'runtime-context'`。
- **只有变化时才写**（与 retains 的文本比较）；空快照写 `CLEARED` 标记："Current runtime context: none. Earlier runtime-context snapshots no longer apply."。
- 投影状态在构造时从日志尾部恢复（找最新的、仍在 surface 上的 own 消息），因此 resume 后不会重复注入。
- 每一步在请求前 `refreshContext()` 重算（`agent.ts:439-443`），所以"同一 turn 的多步之间时间/状态更新"能反映到下一步。

对比：chagent 的 runtime snapshot 是每轮按 digest 决定是否追加（`pkg/agent/runtime_context.go`）。DSH 多出来的是"**替代**语义 + 清空标记 + resume 恢复"，需要 surface replace 支持。

### 6.4 pre-step：每步注入与守卫的公共入口

`agent/pre-step` 是 waterfall，签名 `{agent, messages, turn, step, signal, modelSelection} + next() → PreStepDecision`（`packages/core/agent/src/runtime-types.ts:350`），`PreStepDecision = reject | enter{messages, startsRequestSeries?}`。

DSH 内已挂载的监听者（可作为清单直接移植）：

| 监听者 | 文件:行 | 作用 |
|---|---|---|
| compaction 压力检查 | `packages/compaction/compaction-basic/src/index.ts:157-178` | 超过阈值先压缩，失败只告警不阻断 |
| 时间上下文 | `packages/context/time-context/src/index.ts:199-242` | 按 `refreshIntervalMs` 注入时间读数（含"距上次多久"） |
| AGENTS.md 指令 | `packages/context/agent-instructions/src/index.ts:330-365` | 把项目指令按"声明的直接提示之后、runtime context 之前"插入 |
| 模型切换提示 | `packages/core/agent/src/model-selection.ts:159-186` | 路由变了就追加一句 |
| plan mode 事件+旁白 | `packages/plan/plan-mode/src/index.ts:211-235` | 追加 plan/mode 与"已进入/退出计划模式" |
| goal 回合租约校验 | `packages/goal/goal-round-driver/src/index.ts:380-460` | 不匹配则 reject + 回滚 inbox |
| 会话检查点 | `packages/session/session-checkpoint-policy/src/index.ts:79-82` | 每次请求前 flush 日志 |
| 归档会话门禁 | `packages/api/session-controller/src/archived-session-gate.ts:27` | 已归档会话拒绝继续 |
| 工具/上下文类 | `packages/context/tmux-context`、`session-reference`、`skill/tool-skill` | 按需注入 |

顺序语义：`prepend: true` 的监听者包在外层（先注册者在外，`next()` 之后才能改结果）；不传时是普通追加。**`next()` 的调用点决定了"在默认组装之前还是之后"能看到 claimed messages**。

### 6.5 工具结果回填与附加上下文

- 每个 `tool/call` 有对应 `tool/result`（`sourceEventSeqs: [callSeq]`），因此 UI/回放能配对（`batch.ts:237-283`）。
- 工具可以返回 `additionalContexts`（`deferContext()` 在流水线内延后追加也算），loop 把它们 **push 到 next-step 队尾**（`agent.ts:587-589`），下一步的 pre-step 会把它们当作本批输入的一部分落盘。
- `post-execute` 也可以产出 `additionalContexts`（如 `repeat-tool-reminder` 检测到连续相同调用时注入提醒，`packages/guard/repeat-tool-reminder/src/index.ts:218-233`）。
- 结论：**工具永远不直接改 assistant/user 消息**，只通过"事件 + 下一批输入"影响模型 —— 这让每个注入都可审计、可回放。

### 6.6 turn 收尾注入

`agent/turn-stopping`（serial，`agent.ts:388-392`）在"本轮已决定结束且没有 pending next-step"时运行；它可以把消息塞回 inbox 让本轮继续。适合做"收尾检查/交付物清单/提醒模型还有未收集的后台任务"。

---

## 7. Go 移植要点汇总

### 7.1 建议的数据结构

```go
// 事件日志（唯一真源）
type Event struct { Seq int64; Time int64; Type string; Data json.RawMessage; Surface *SurfaceOp }
type SurfaceOp struct { Kind SurfaceAppend | SurfaceReplace; Start, End int64; SourceSeqs []int64 }

// 会话
type Session struct { ID string; log []Event; surface []int64; contentGen int64; derived []Message }

// 阶段状态机（一个 goroutine 持有，不共享）
type Phase struct { Kind PhaseKind; Abort context.CancelCauseFunc; Turn, Step int; WakeRequested bool }
type PhaseKind int; const ( PhaseIdle PhaseKind = iota; PhaseMaintenance; PhaseRunning )

// inbox：两个切片 + 持久 splice
type Inbox struct { NextTurn, NextStep []Message }

// 请求
type Request struct {
  Provider, Model, ReasoningEffort string
  Messages []Message            // 由 surface 折叠
  Tools []ToolSchema            // 由 tool providers + order 得出
  ToolHistory *ToolHistory
  Signal context.Context
  Frozen bool                   // 只读校验（Go 里用不可变约定 + 事件日志重放）
}
```

### 7.2 状态机与边界

1. **一个 turn 内不设 step 上限**；终止只由"无 tool-call / concludesTurn / max-tokens / 输入耗尽 / cancel / error"决定。若担心失控，加**安全阀**而不是主判据，并且安全阀触发也要落 `turn/end{reason}`。
2. **空首步**（所有输入被 reject 或消息被撤销）不调用模型，但仍要开/关 turn（`completed`）。
3. **max-tokens 粘性**：一旦某步触顶，本轮最终结论不得降级为 completed。
4. **取消必须在每个 await 后复查**；取消路径优先于错误路径（`aborted` 而不是 `error`）。
5. **任何异常路径都要补齐未回填的 tool/result**（step 级 `ToolCallRecovery`），并在 turn 结束时写 `turn/end`。
6. **重试是新的 attempt**：重建请求、重新记 header；失败的 attempt 落 `assistant/attempt`，成功才落 `assistant/message`。
7. **并行工具**：排他调用是屏障；派发可并行、提交必须模型序；abort 时给未启动调用写合成错误结果。
8. **注入只通过 inbox / 事件**，不允许工具或插件直接改消息切片。
9. **prompt 装配顺序确定化**：section/context 都有显式 order，同名 scope 遮蔽 global，tool 按配置或字典序。
10. **runtime context 只在变化时写**，并支持"清空标记"与 resume 恢复。

### 7.3 与 Go 生态的适配

- `context.Context` 传递取消；**取消原因**用 `context.WithCancelCause` + 自定义 cause 枚举（对应 `user/parent/hook/disposed`）。
- 事件日志建议用 append-only JSONL（DSH 的 `session-persistence-jsonl` 同构），写句柄独占（类似 `flock`/单写者），resume 先拿写权限再读。
- waterfall 钩子用 `[]func(next func()) ...` 链 + 决策 union；不要用 Go 的 `error` 表达"拒绝/重试"这类**正常控制流**。
- 投影用 `map[ProjectionKey]any` + `Apply(event) (newState, changed)`，并记录"已消费到 seq"以支持增量重放。

---

## 8. 对 chagent 最关键的 5 个差距线索

> 以下基于对 `pkg/agent` 的抽样阅读（`pipeline_*.go`、`agent.go`、`steering.go`、`context_manager.go`、`runtime_context.go`、`config/defaults.go`），仅供设计对照，不代表 chagent 现状的全部。

1. **缺少"事件日志 + surface 投影"作为唯一真源。**
   chagent 的会话历史由 `Sessions.AddFullMessage` / `exec.messages` 维护（如 `pkg/agent/pipeline_execute.go:156`、`:332`），compaction 通过 context manager 改内存/重写历史。DSH 的 `surfaceOp: append|replace` 让"历史真的被替换"这件事成为一等公民（`packages/core/session/src/index.ts:999-1035`），system prompt 归一化、compaction、resume 修复、KV cache 前缀优化全都建立在它上面。**建议：先引入带 surface 操作的事件日志，再迁移 compaction**，否则每个新注入点都要单独处理"重写历史"。

2. **缺少统一的 `pre-step` 扩展点与声明式 prompt 序位。**
   chagent 的注入是 pipeline 里的硬编码顺序 + hooks（`BeforeLLM/BeforeTool/...`，`pkg/agent/hooks.go`），没有"插件拿到本步 claimed 输入 → 返回 enter{messages}/reject"的契约，也没有 section/context 的中央 order 表。DSH 用一张表就把 20+ 个注入源（plan/todo/team/time/workspace/sandbox/subagent/…）排序好，并允许任何插件否决本步（`agent/pre-step` + `SECTION_ORDERS/CONTEXT_ORDERS`）。**建议：先抽出 `PromptSection{Name, Order, Text, Required, Complete}` 注册表与 `PreStepDecision` 契约**，把现有 runtime snapshot / taskstate / workspace context 迁进去。

3. **工具执行是串行 `for` 循环，没有调度器语义。**
   `pkg/agent/pipeline_execute.go:127` 逐个 `range normalizedToolCalls`，因此没有"排他屏障/滚动并发池/模型序提交/abort 合成结果"这些不变量。DSH 的 `executeToolBatch` + `runGroup`（`packages/core/tools/src/batch.ts:50-235`）在保证日志平衡（每个 call 必有 result）的前提下并行化。**建议：先把 `tool/call`–`tool/result` 配对写成硬不变量（含 abort 合成结果），再引入可配置并发度**；否则并行化会立刻产生"有调用无结果"的不可恢复尾巴。

4. **终止语义：`max_tool_iterations=300` 这类硬上限 vs. DSH 的"输入有界 + 回合制"。**
   chagent 用迭代计数做主终止判据（`pkg/config/defaults.go:33`，触发文案在 `pkg/agent/agent.go:124`）。DSH 主循环无 step 上限，靠"无 tool-call / concludesTurn / max-tokens / inbox 空 / cancel"收敛，需要长跑时用 goal round driver 的**回合上限**（`maxGoalRounds`，`packages/goal/goal-round-driver/src/index.ts:182-195`）+ 每回合可见的续跑提示（`prompt.ts`）。这个模型对"模型自我驱动长任务"更友好，也更可解释。**建议：把 `max_tool_iterations` 降级为安全阀，并引入"目标回合"概念（每回合一条可见 user 提示 + 回合上限 + 守卫校验）**。

5. **失败/重试/中断的结构化程度不足，且"降级"与"重试"耦合在同一处。**
   chagent 是 `FallbackChain` + `transientLLMRetryReason`（`pkg/agent/pipeline_llm.go:699-740`）在 pipeline 内换模型/重试。DSH 把它拆成：结构化 `LlmFailure`（含 status/retryAfter/requestId）+ provider 自带的 retry policy + 可插拔的 `agent/request-error` waterfall + 每次重试先落 `llm/retry` 的持久记录（`packages/llm/llm-retry/src/index.ts:160-247`）+ "切模型必须先 compaction 且提示模型"（`compaction-basic/src/index.ts:184-215`、`model-selection.ts:159-186`）。**建议：把"重试"（同路由、可观测、可取消）与"降级"（显式切路由、需要上下文保护）拆成两条路径，并给失败一个可序列化的分类结构**，否则日志里无法区分"供应商抖动"和"我们主动换模型"。

---

## 9. 附录

### 9.1 关键文件索引（均已实际读取）

| 主题 | 路径 |
|---|---|
| 主循环 / step / 请求 | `packages/core/agent-loop/src/agent.ts`（:49, :163, :223, :276, :320, :428, :606, :649） |
| 流式尝试与落盘 | `packages/core/agent-loop/src/assistant-stream.ts` |
| system prompt 投影 / runtime context | `packages/core/agent-loop/src/runtime-context.ts` |
| inbox | `packages/core/agent-loop/src/inbox.ts`、`packages/core/agent/src/runtime-types.ts` |
| 工厂 / resume / 修复 | `packages/core/agent-loop/src/index.ts:668-850` |
| prompt 注册表与装配 | `packages/core/system-prompt/src/index.ts`（:136, :175, :295, :619） |
| LLM 服务与路由 | `packages/llm/llm/src/index.ts`（:956, :1166）、`types.ts:156-189,470-482` |
| chunk→message 组装 | `packages/llm/llm/src/assembler.ts` |
| 重试策略 | `packages/llm/llm/src/retry-policy.ts`、`packages/llm/llm-retry/src/index.ts` |
| 工具批调度 | `packages/core/tools/src/batch.ts`、`pipeline.ts`、`events.ts`、`executor.ts` |
| 会话/事件/修复 | `packages/core/session/src/{index,types,repair,surface}.ts` |
| compaction | `packages/compaction/compaction-basic/src/index.ts:143-267` |
| subagent | `packages/subagent/subagent/src/{child-agent,manager}.ts` |
| goal 回合 | `packages/goal/goal-round-driver/src/{index,prompt}.ts` |
| plan mode | `packages/plan/plan-mode/src/index.ts` |
| jobs / schedule | `packages/jobs/{jobs,tool-jobs}/src`、`packages/schedule/schedule/src/{index,runtime}.ts` |
| 逐点注入示例 | `packages/context/time-context/src/index.ts:199`、`agent-instructions:330`、`session-checkpoint-policy:79` |
| 守卫 | `packages/guard/{repeat-tool-reminder,timeout-policy}/src/index.ts` |

### 9.2 备忘：调研中发现的"名不副实"路径

- `packages/hooks/` 目录**存在但为空**；hooks 实现实际在 `packages/experimental/hooks-claude-code`、`hooks-codex`（以及 `packages/core/agent` 的 `agent/pre-step` 等）。按目录名找 hooks 会扑空。
- `packages/core/agent-loop/src/agent.ts` 同时承担 loop 驱动与请求构建（737 行），`index.ts`（837 行）同时承担工厂、resume、声明式启动；两处都比包名暗示的更重。
- `docs/agent-lifecycle.zh.md` 是概念导航，但机制细节（尤其 retry/surface 语义）以源码注释为准；源码注释质量很高，可直接作为设计文档来源。
