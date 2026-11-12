# 仍存在的问题 / 可接受残留（Open Issues & Accepted Residue）

> 快照：2026-11-02（残余问题修复轮后）。HEAD `v2` + 待提交修复。
> 口径：`OPEN-*` 为仍需跟进/决策的缺口；`ACCEPT-*` 为已知但不计划处理的历史/设计残留。
> 索引与状态口径见 [问题库 README](README.md)；已修复项见 [fixed.zh.md](fixed.zh.md)。

## 一、仍存在（OPEN）

### OPEN-17【P3】DSH 工具组剩余接线/限制

- **剩余项**：
  1. MCP `resources/list_changed` 未注册 handler（DSH resources 三件套每次调用实时拉取，不依赖缓存，暂不影响正确性）；
  2. `subagent` 同步工具在当前回合内联执行，不进入 `list_agents/wait_agent/interrupt_agent` 控制面（`spawn` 创建的后台任务可管理）；
  3. DSH 的持久 terminal 6 工具、`working_directory`、`exit_plan_mode`、`ask_user_question`、`present`、plugin/workflow/ralph/agent-team、LSP、worktree、PTC `run_code` 等仍未迁移（原因见 [DSH 工具组](../reference/dsh-tools.zh.md) §3：等价/DSH 运行时/UI 专属）；其中 `workflow` 已完成专项必要性评估并决定不迁移（[评估报告](../reference/workflow-assessment.zh.md)）。
- **影响**：不阻塞 chess 主链路；属于能力边界。
- **建议**：如 chess 出现“长驻交互式终端”需求，单独立项迁移 terminal；其余保持不迁移。
- **证据**：`docs/reference/dsh-tools.zh.md`、`docs/reference/mcp-support.zh.md` §4。

## 二、已解决（本轮关闭，保留索引便于追溯）

| ID | 原问题 | 解决 | 证据 |
|---|---|---|---|
| OPEN-01 | 覆盖率 90% 阈值未达 | 关键路径口径达成：agent 控制面 **91.0%**、gateway **91.6%**、config **92.2%**、pico **93.6%**；agent 整体 85.0%、四包合计 86.9%（可复算） | [覆盖率报告](../reference/coverage.zh.md)、`build/functest-20261030/coverage/`、`fg2-coverage-*` |
| OPEN-02 | 改动未 commit、门禁未在提交后复跑 | 已分主题提交；提交后重跑 §11.1 全矩阵（全部 exit=0）与 compat | `baseline/final-gate/fg2-*`、compat logs |
| OPEN-03 | D4 supersede 仅模型可见层 | 设计约束：JSONL 保留审计副本；断言以模型可见视图/最终 JSONL+summary 重建为准 | ACCEPT-03 |
| OPEN-04 | 异步 summarize 与 durable context 竞态 | `pkg/agent/context_legacy.go` 写序列串行 + 版本/前缀 CAS + 偏移平移，保留 LLM 期间注入 context；3 个回归用例（修复前 FAIL） | FIX-R17、`analysis/open04/` |
| OPEN-05 | pico 覆盖率波动 | 补 70 个确定性测试：62.6%→93.6%，3 次运行 93.0/93.5/93.6（波动≤1pt） | `coverage/pico-*` |
| OPEN-06 | Go 版本升级依赖 | 流程约定：升级 Go 时同步 `go mod tidy` 并提交 require 分组；CI 已含 tidy 门禁 | ACCEPT-04 |
| OPEN-07 | MCP 显式关闭仅配置矩阵验证 | live E2E 新增 P11 MCP 阶段（PASS=24/0）：servers-only 自动启用、真实模型调用、显式关闭 reload、动态工具增删 | `live/p11-mcp/` |
| OPEN-08 | MCP 最新协议支持边界 | 支持矩阵收敛；2026-06-30 原生协商；tools/list_changed 端到端重注册；resources/prompts/tasks/roots/elicitation/sampling 明确边界 | `mcp-support.zh.md`、FIX-R18 |
| OPEN-09 | DSH 未迁移项 | 决策：surface replace/plan mode/turn-stopping 等属 DSH 运行时/非 chess 必需，不迁移；保留上下文对比文档作为输入 | ACCEPT-05 |
| OPEN-10 | gateway 错误分支/单例盲区 | 新增 25 个测试，gateway 覆盖率 76.5%→91.6%（3 次稳定）；单例保留进程级 compat 覆盖 | `coverage/gateway-*` |
| OPEN-11 | artifacts 含明文密钥 | 已脱敏 `live/workspace/config.json`；artifacts 分享前脱敏要求保留为操作规范 | `live/workspace/config.json`（REDACTED） |
| OPEN-12 | session meta 损坏不可用 | 自愈重建（FIX-R11） | 单测 + 进程级 C 37/37 |
| OPEN-13 | SIGKILL exec 孤儿 | 父死看门狗（FIX-R12） | A 18/18、单测 |
| OPEN-14 | hook/MCP 启动失败静默退出 | 降级继续 + gateway 记录（FIX-R9） | E3/E4 PASS |
| OPEN-15 | 只读日志目录 panic | stderr 告警降级（FIX-R13） | F 9/9 |
| OPEN-16 | Agent 循环健壮性残余 | 主要项修复：负 usage、负迭代上限、Save 失败可观测；其余为设计/防御项 | FIX-R16、ACCEPT-06 |
| OPEN-18 | 大参数错误诊断/fallback 日志 | IsError 保留头尾诊断；FallbackChain 成功 Info（FIX-R14） | B4 PASS、单测 |
| OPEN-19 | 覆盖率验收项（本轮新增） | 并入 OPEN-01 关闭 | [覆盖率报告](../reference/coverage.zh.md) |

## 三、可接受残留（ACCEPT）

### ACCEPT-01 docs/design 历史文档旧表述

- **不处理原因**：`docs/design/chagent-taskbook-audit.zh.md`、`agent-workspace-context-plan.zh.md`、`agent-workspace-context-task-book.zh.md` 属历史审计/计划文档，保留 seahorse、旧目录结构等当时决策背景；改写会破坏历史可追溯性。
- **影响**：仅历史记录，不影响当前实现与使用文档。

### ACCEPT-02 并行专题文档（已落地，历史说明）

- **状态**：`docs/reference/dsh-parity.zh.md`、`docs/architecture/context-vs-dsh.zh.md`、`docs/reference/mcp-support.zh.md`、`docs/operations/robustness.zh.md` 均已落地，链接为正式文档；本条目仅保留历史说明。

### ACCEPT-03 D4：supersede 只收敛模型可见层

- **不处理原因**：JSONL 为 append-only 审计/回放事实源，保留同 context 的旧副本；模型可见视图已去重（D4 修复）。断言应以模型可见视图或最终 JSONL+summary 重建为准。
- **证据**：功能测试报告 §7.2/§14.4；独立产物验证 advisory `session.context_supersede`。

### ACCEPT-04 Go 版本升级流程约束

- **不处理原因**：`go.mod` 指令固定 `1.25.11` 是 tidy 门禁前提；升级 Go 会重新触发 tidy 差异，需作为独立升级任务同步 `go mod tidy`、CI `go-version-file` 与依赖分组，不是功能缺陷。
- **证据**：`fg2-tidy-diff.log`、`rv-tidy-head-repro.log`。

### ACCEPT-05 DSH 运行时/UI 专属机制不迁移

- **不处理原因**：surface replace/事件投影、plan mode、turn-stopping、plugin/workflow/ralph/agent-team、present/action-cards、LSP、worktree、PTC `run_code` 等依赖 DSH 运行时或 Web/UI；chess 单 agent + pico 场景无落地价值，已在 [上下文对比](../architecture/context-vs-dsh.zh.md) 与 [DSH 工具组](../reference/dsh-tools.zh.md) 记录理由；`workflow` 的专项结论见 [必要性评估](../reference/workflow-assessment.zh.md)。
- **影响**：能力边界清晰，不阻塞主链路；若将来需要再单独立项。

### ACCEPT-06 Agent 循环设计性/防御性残余

- **不处理原因**：`ToolRegistry` nil receiver 防御（生产构造路径不可达）、`PublishNonBlocking` 订阅者满时丢事件（设计为不阻塞）、`contextPromptBudget` 极大窗口溢出兜底（已 `<1→1`）等属于防御性/设计取舍；核心可观测性项已在 FIX-R16 修复。
- **证据**：`build/functest-20261030/analysis/agent-robustness/review.md`。
