# Agent 健壮性与故障恢复报告

> 核对日期：2026-11-02。
> 对象：Chagent chess 基础 agent（HEAD `5da70e0b` + 未提交修复工作树）。
> 方法：两层独立验证——**进程级/文件级**（无模型，脚本化 fake provider 驱动真实 turn/工具/session）
> 与**真实链路故障注入**（真实 gateway + pico WS + 本地脚本化 OpenAI 兼容 provider）。
> 证据：
> - 进程级：`build/functest-20261030/robustness-process/`（`summary.md`、`logs/A–F.log`、`logs/RESULTS.tsv`、`work/<case>/`）
> - 故障注入：`build/functest-20261030/robustness-live/`（`results.json`、`run_matrix.py`、`A1-*`…`G1-*`）
> - mock provider fixture：`scripts/fixtures/mock_openai_server.py`（可复用）
> - 代码审查/单测：`pkg/agent` 既有故障/边界测试、`pkg/mcp` 协议测试
> 更新：2026-11-02 晚些时候已修复本报告发现的全部缺陷；复跑故障注入矩阵 **35 PASS / 0 FAIL**，
> 进程级矩阵 **110 PASS / 0 FAIL / 1 SKIP**（另 2 项残留检查 PASS）。

## 0. 结论摘要

- **生命周期/协议主路径健壮**：10 轮启停、active turn 中 SIGTERM、**reload×20 后 SIGTERM（P0 回归）**
  全部通过（exit 1.3–17.6ms、pid 清理、无 hang dump、fd/线程有界）；崩溃（SIGKILL）后 session JSONL
  可解析、重启自愈 stale pid、历史前缀保留并可继续对话。
- 进程级矩阵（task-27）：**109 PASS / 3 FAIL / 1 SKIP**；故障注入矩阵（task-26，本报告补充）：
  **35 项 32 PASS / 3 FAIL**，FAIL 均对应真实缺陷或可诊断性缺口。
- **真实缺陷（均已修复）**：P1×2 + P2×3 + P3×1：
  - **P1-1：session `meta.json` 损坏后该 session 永久不可用（无自愈）** → 已自愈重建（FIX-R11，OPEN-12 关闭）；
  - **P1-2（已修复）：hook/MCP 等可选子系统初始化失败时 agent 消息循环静默退出**——已改为
    “记录 Error 并降级继续服务”，gateway 侧记录 Run 异常；E3/E4 复测 **PASS**（修复：`pkg/agent/agent.go`
    与 `pkg/gateway/gateway.go`）；
  - **P2-1：SIGKILL gateway 时进行中的 exec 子进程成为孤儿** → 父死看门狗杀进程组（FIX-R12，OPEN-13 关闭）；
  - **P2-2：超大参数导致的工具错误被 large-payload 过滤器替换** → 错误保留头尾诊断（FIX-R14，OPEN-18 关闭）；
  - **P2-3：fallback 成功时缺少 Info 级 `Fallback: succeeded` 日志** → FallbackChain 成功路径补 Info（FIX-R14）；
  - **P3：日志目录只读时以 panic 退出** → 降级为 stderr 告警并继续服务（FIX-R13，OPEN-15 关闭）。
- 主要残余风险：prune/spill 故障路径、工具 panic、MCP 初始化超时/中途断开、goroutine 级泄漏未覆盖；
  覆盖率与未提交工作树见 `docs/issues/open.zh.md`。

## 1. 测试范围与方法

| 层 | 覆盖 | 方法 |
|---|---|---|
| 进程级 A | SIGKILL mid-turn → 重启；session/JSONL/meta/task_state 一致性 | 真实 gateway + fake provider（脚本化 HTTP），真实 exec/MCP/session |
| 进程级 B | 10× start→health→SIGTERM；active turn 中 SIGTERM；reload×20 后 SIGTERM；hot_reload | 同上；pid/health/exit/fd/线程/残留采集 |
| 进程级 C | JSONL 截断/中部非法行/空文件/超长行；meta 损坏/缺失；task_state 非法 | 手工构造损坏文件后启动/对话 |
| 进程级 D | 同 workspace 单例；双 workspace 并发；同 session 双 WS 客户端 | 真实进程/WS |
| 进程级 E | MCP stdio 子进程、session 增长、临时文件、全局残留 | 进程/文件系统检查 |
| 进程级 F | 超大/异常 config；workspace/pid/日志目录只读或缺失；截断 JSON | 启动错误与退出码 |
| 故障注入 A | provider 500/拒绝/超时/畸形 JSON/流中断/429/context 400/fallback/恢复 | 本地 mock OpenAI 服务 + 真实 gateway |
| 故障注入 B | 未知工具/非法 args/空 content/超长 args/50 次调用/loop guard/混合 content/异常 finish_reason | 脚本化模型响应 + 真实工具 |
| 故障注入 C | exec 超时/越界读/只读写/目录缺失/大输出 | 真实工具 |
| 故障注入 D | turn 中 /stop、长任务中 SIGTERM、WS 断开、turn 中 reload、并发 session | 真实 gateway |
| 故障注入 E | hook 超时/非法 JSON/启动失败；MCP 不可达/isError | process hook + MCP fixture |
| 故障注入 F/G | JSONL/meta 损坏重启；5 轮启停；100 turn 资源 | 真实 gateway + mock provider |

**故障注入工具**：`scripts/fixtures/mock_openai_server.py` 为纯标准库 OpenAI 兼容服务（含 SSE），
plan step 覆盖 text/empty/tool/tools/mixed/unknown_tool/huge_args/invalid-args/http/malformed/sleep/
stream_cut/stream_malformed/weird_finish；运行时 `POST /__control` 可切场景，无需重启 gateway；
每个请求写 JSONL 作为重试/降级次数的原始证据。每用例独立 temp `HOME/WORKSPACE/CONFIG`、`port=0`、
独立 `gateway --name`，pico WS 驱动，结束 `SIGTERM` 并校验 pid 清理。

## 2. 结果矩阵

### 2.1 进程级（`robustness-process/summary.md`）

| 组 | 结果 | 关键证据 |
|---|---|---|
| A 崩溃恢复（18） | **17 PASS / 1 FAIL(P2)** | SIGKILL 后 JSONL 可解析、meta.count 一致、重启后新回合成功；FAIL=exec 子进程孤儿 |
| B 生命周期压力（17） | **17 PASS / 0** | 10 轮 exit 1.3–17.6ms；active-turn SIGTERM 8.1ms；reload×20 后 SIGTERM 3.8ms、无 hang、fd 12→12 |
| C 数据损坏（37） | **35 PASS / 2 FAIL(P1)** | 截断/非法行/空文件/超长行/meta 缺失/task_state 非法均安全；FAIL=meta 损坏后 session 永久失败 |
| D 并发/单例（15） | **15 PASS / 0** | 单例拒绝 7.8ms；双 workspace 隔离；同 session 双 WS 均回复且持久化 |
| E 资源/清理（15） | **14 PASS / 0 / 1 SKIP** | MCP 子进程随启停；无 tmp/pid 残留；SKIP=spill 未在无压力场景触发 |
| F 配置/文件系统边界（9） | **9 PASS / 0** | 8MB+未知字段 51ms 拒绝；只读 workspace rc=1 可诊断；缺失目录自动创建；非法 pid 视为 stale |

### 2.2 故障注入（`robustness-live/results.json`，35 项）

| 用例 | 结果 | 判定说明（证据目录 `robustness-live/<用例>/`） |
|---|---|---|
| A1 http-500-retry-recover | **PASS** | 2 次请求+`Transient LLM error`；错误上报后切回 ok，同进程后续 turn 正常 |
| A2 conn-refused | **PASS** | `failed to send request` 可诊断；`/health=200`、turn 不 hang |
| A3 provider-timeout | **PASS** | mock 延迟 8s + `request_timeout=2`：3.7s 结束、超时归类重试 |
| A4 malformed-json | **PASS** | `failed to parse JSON response`；切回 ok 后恢复 |
| A5 stream-midcut | **PASS** | SSE 半个 JSON 事件后断连：流错误可诊断、无崩溃、后续流式恢复 |
| A6 http-429 | **PASS** | 按 rate_limit 重试（2 次请求），turn 正常收尾 |
| A7 context-400-retry | **PASS** | 首次 400 `context_length_exceeded` → 压缩重试后成功（2 次请求） |
| A8 fallback | **PASS（含 P2-3）** | 主模型拒绝、fallback 回复 `FALLBACK_OK`；Info 级 fallback 成功日志缺失 |
| B1 unknown-tool | **PASS** | `tool "totally_unknown_tool" not found` 回灌模型，turn 继续 |
| B2 invalid-args | **PASS** | `invalid arguments … missing required property "path"` 可诊断 |
| B3 empty-content | **PASS** | 默认回复兜底，会话有收尾 |
| B4 huge-args | **FAIL（P2-2）** | 工具失败已记录，但模型可见错误被 large-payload 过滤为固定文案，无法自纠 |
| B5 50-tool-calls | **PASS** | `max_tool_iterations=50` 执行 50 次后命中上限收尾，不 hang、健康 |
| B6 loop-guard | **PASS** | 同工具同参：session 注入 2 条 `context.kind=loop-guard`（阈值 3/5） |
| B7 mixed-content-tool | **PASS** | content+tool_calls 混合：工具执行、后续正常收尾 |
| B8 weird-finish | **PASS** | `finish_reason=length` 截断内容正常返回 |
| C1 exec-timeout | **PASS** | `sleep 3` vs `timeout_seconds=1`：`Command timed out after 1s`，1.7s 结束 |
| C2 read-outside | **PASS** | `path escapes workspace: /etc/hosts` |
| C3 write-readonly | **PASS** | `failed to rename temp file … permission denied` |
| C4 listdir-missing | **PASS** | `openat no-such-dir-xyz: no such file or directory` |
| C5 large-output | **PASS（spill 未覆盖）** | 200KB 输出被 large-payload 过滤器入模前替换；仅触发 Proactive compression/context-pressure |
| D1 stop-inflight | **PASS** | `/stop` 返回 `Task stopped. "slow please" was canceled.` |
| D2 sigterm-inflight | **PASS** | clean exit 0.01s、pid 删除（P0 修复回归，见报告 §14.1） |
| D3 ws-disconnect | **PASS** | turn 中断开 WS 并重连；gateway 健康、新 turn 正常 |
| D4 reload-inflight | **PASS** | turn 中 `POST /reload`=200、健康、reload 后新 turn 正常 |
| D5 concurrent-sessions | **PASS** | 两 session 并发均有回复；session JSONL 按 chat 隔离无串扰 |
| E1 hook-timeout | **PASS** | `Interceptor hook timed out` 告警后工具仍执行（fail-open） |
| E2 hook-invalid-json | **PASS** | 告警可见、工具仍执行（fail-open） |
| E3 hook-start-failure | **PASS** | turn 有回复、hook 错误可见、gateway 健康（FIX-R9） |
| E4 mcp-unreachable | **PASS** | turn 有回复、MCP 错误可见、agent loop 仍可服务（FIX-R9） |
| E5 mcp-iserror | **PASS** | 模型收到 `MCP tool returned error: MCP_FAULT_ERROR: …` |
| F1 corrupt-jsonl | **PASS** | 重启成功、新 turn 正常、`skipping corrupt line` |
| F2 corrupt-meta（新会话路径） | **PASS** | 重启成功、新 turn 正常（会话降级；对比进程级 C 的既有 session 损坏 P1-1） |
| F3 ten-cycles | **PASS** | 同 workspace 5 轮启停：exit 0.0s×5、pid 全清理、无残留 |
| G1 100-turns | **PASS（仅记录）** | RSS 20.4→26.2MB（+5.8MB）、fd 12→16、100 轮全部有回复 |

## 3. 真实缺陷

### P1-1 session `meta.json` 损坏 → 该 session 永久不可用

- 现象：损坏/截断 `<session>.meta.json` 后启动正常，但之后**每次 turn** 返回
  `Error processing message: memory: decode meta: unexpected end of JSON input`，meta 不修复、重试仍失败。
- 证据：`robustness-process/logs/C.log`、`work/C-corrupt-meta/defect-evidence.log`。
- 建议：decode 失败时备份坏文件并从 JSONL 重建 `count/skip`，或允许“丢弃 meta、空摘要继续”降级。

### P1-2 可选子系统初始化失败 → agent 消息循环静默退出（已修复）

- 现象（修复前）：hook 可执行文件不存在（E3）或 MCP server 无法启动（E4）时，gateway 启动成功、`/health=200`，
  但 WS 发送消息后**无 turn 日志、无回复**；客户端只能超时。
- 根因：`pkg/agent/agent.go` 的 `AgentLoop.Run` 在 `ensureHooksInitialized`/`ensureMCPInitialized`
  失败时 `return err`；`pkg/gateway/gateway.go` 以 `go agentLoop.Run(ctx)` 启动且丢弃该错误。
- 修复（2026-11-02）：`AgentLoop.Run` 对 hook/MCP 初始化失败改为 **Error 日志 + 降级继续**，
  并 `defer running=false`；gateway 对 Run 异常记录 Error（不再静默）。
- 复测：E3/E4 **PASS**（turn 有回复、错误可见、health 200）；对应 FIX-R9。
- 证据：`robustness-live/E3-hook-start-failure/`、`E4-mcp-unreachable/`（重跑后 gateway.log + case.json）。

### P2-1 SIGKILL gateway 时进行中的 exec 子进程成为孤儿

- 现象：gateway -9 后 `sleep 30` 子进程存活（PPID→1）。SIGTERM 正常路径无此问题；
  MCP stdio 子进程因管道关闭会自行退出。
- 证据：`robustness-process/logs/A.log`（`orphans=[pid,'sleep 30']`）。
- 建议：exec 使用独立进程组（`Setpgid`）并记录子进程组，退出/恢复时按组清理；macOS 无 PDEATHSIG。

### P2-2 超大参数工具错误被 large-payload 过滤器替换

- 现象：模型发出 50000 字符 `read_file.path`，工具失败（完整错误在 gateway log），但模型可见结果变为
  `[工具返回了看起来像大段 base64 的内容，已从模型上下文省略。]`（本轮中文化后的当前标记；初测时为英文标记 `[Tool returned a large base64-like payload; omitted from model context.]`），无法据此修正参数。
- 根因（推断）：`sanitizeToolLLMContent/looksLikeLargeBase64Payload` 对超长无空格文本的启发式误判；
  error 结果也走同一过滤。
- 证据：`robustness-live/B4-huge-args/{sessions/*.jsonl,gateway.log}`。
- 建议：对 `IsError` 结果保留错误类型/长度摘要，或对 error 内容跳过 base64 启发式。

### P2-3 fallback 成功路径缺少 Info 日志

- 现象：主模型连接拒绝、fallback 成功后行为正确，但 `gateway.log` 无 `Fallback: succeeded` Info 行
  （仅 debug `candidates=2`），排障时难以确认降级是否发生。
- 证据：`robustness-live/A8-fallback/gateway.log`。
- 建议：在 fallback 成功路径无条件记录 Info（provider/model/attempts/原因）。

### P3 日志目录只读时以 panic 退出

- 现象：`CHAGENT_HOME/logs` 只读时 `logger.Fatal`/panic 退出 rc=2；错误含路径与 permission denied、无 hang。
- 证据：`robustness-process/work/F4-readonly-logs/readonly-logs.log`。
- 建议：改为明确 error + exit 1（或回退 stderr），便于 supervisor 识别为普通启动失败。

## 4. 已充分验证的健壮性（PASS 摘要）

- **P0 回归**：reload×20 后 SIGTERM 3.8ms 退出、pid 删除、无 hang dump；hot_reload 后 SIGTERM 1.3–3.9ms。
- **Provider 故障**：500/拒绝/超时/畸形 JSON/流中断/429/context-400 均重试或降级可控，错误可诊断；
  fallback 生效；故障解除后同进程可继续服务（A1–A8）。
- **模型异常输出**：未知工具/非法 args/空 content/50 次调用/loop guard/混合 content/异常 finish_reason
  均可控收尾、安全阀生效（B1–B8）。
- **工具/权限**：超时、越界读、只读写、目录缺失均返回可诊断错误且不污染会话（C1–C4）。
- **取消/生命周期**：turn 中 `/stop`、SIGTERM（0.01s）、WS 断连重连、turn 中 reload、并发 session 隔离（D1–D5）。
- **Hook/MCP 运行期故障**：hook 超时/非法 JSON fail-open 不阻塞工具；MCP isError 转工具错误（E1/E2/E5）。
- **数据容错**：JSONL 尾部损坏重启可恢复；5 轮启停无残留（F1–F3；进程级 C 更全面）。
- **资源**：100 轮后 RSS +5.8MB、fd +4，无失控迹象（G1）。

## 5. 残余风险与未覆盖

1. **C5 prune/spill 真实路径未覆盖**：大输出被 large-payload 过滤器入模前替换，spill 未触发；
   需用非 base64 形态大文本（带空格/换行）重测 prune+spill 写入失败降级与清理。
2. **工具 panic** 未注入（生产工具均有错误返回）；panic→tool error 路径未实测。
3. **MCP 初始化超时/中途断开/畸形 content** 未覆盖（仅“不可达”和 `isError`）。
4. **goroutine 级泄漏** 未直接度量（gateway 无 goroutine 指标），仅以 RSS/fd/线程近似。
5. **缺陷状态**：本报告发现的 P1/P2/P3 已全部修复并回归（FIX-R9/R11/R12/R13/R14）；
   新增单元/集成测试覆盖；剩余为设计/覆盖边界，见问题库 OPEN-01/03/04/05/06/07/08/09/10/17。
   A8 的 `Fallback: succeeded` 检查受测试候选顺序影响（首个候选即健康端点时不构成 fallback），
   产品侧已在 FallbackChain 成功路径记录 Info。
6. artifacts 内历史 `live/workspace/config.json` 含真实 key，分享前需脱敏（本任务产物已脱敏）。
7. 真实模型驱动的 token 压力/压缩边界由 `docs/design/agent-functional-test-report.zh.md`
   live/context 专项承担，本文以确定性注入为主。

## 6. 复现

```bash
# 故障注入矩阵（A–G；每用例独立 temp workspace、port=0、结束 defer kill）
python3 build/functest-20261030/robustness-live/run_matrix.py --cases A,B,C,D,E,F,G
python3 build/functest-20261030/robustness-live/run_matrix.py --cases E   # 单组（P1-2 复现）
jq -r '.[] | [.status,.id,.detail] | @tsv' build/functest-20261030/robustness-live/results.json

# mock provider 独立复用
python3 scripts/fixtures/mock_openai_server.py --port-file /tmp/mock.port \
  --log /tmp/mock.jsonl --scenario http_500
# 运行时切场景：curl -s -X POST http://127.0.0.1:$(cat /tmp/mock.port)/__control \
#   -d '{"scenario":"ok","content":"RECOVERED_OK"}'

# 进程级（约 2-3 分钟；python3+websockets/lsof/pgrep）
cd build/functest-20261030/robustness-process && ./run-all.sh
cat build/functest-20261030/robustness-process/logs/{A,B,C,D,E,F}.log

# 关键单测
go test ./pkg/agent/... ./pkg/mcp/... -count=1
```

> 判定原则：所有 PASS/FAIL 均以 `<case>/gateway.log`、`mock-requests.jsonl`、session JSONL 原文为准，
> 不依赖模型口头回复；进程级用例以退出码/退出耗时/pid 文件/进程表为准。
> 已完成能力修复（P0 reload→SIGTERM、MCP 自动启用、显式 0 校验、usage 锚点等）见
> `docs/design/agent-functional-test-report.zh.md` §14；本文缺陷为修复轮之后仍存在的健壮性缺口。