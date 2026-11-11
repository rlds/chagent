# 问题库（Issue Tracker）

> 本目录是 Chagent chess 基础 agent 的**轻量问题库**：记录已修复问题、仍存在的问题与可接受残留。
> 快照：2026-11-02，HEAD `5da70e0b` + 未提交修复工作树。
> 相关：[项目状态](../project/status.zh.md) · [项目介绍](../project/introduction.zh.md) · [完整测试报告](../design/agent-functional-test-report.zh.md)

## 目录结构

| 文件 | 内容 |
|---|---|
| [fixed.zh.md](fixed.zh.md) | 已修复问题（FIX-*）：现象、根因、修复点、回归测试、验证证据 |
| [open.zh.md](open.zh.md) | 仍存在的问题/缺口（OPEN-*）与可接受残留（ACCEPT-*） |

## 状态口径

| 状态 | 含义 | 判定要求 |
|---|---|---|
| **已修复**（FIX-*） | 缺陷已修，且有回归测试与复测证据 | 必须给出：现象、根因、修复文件（未 commit 标注"工作树"）、回归测试名、验证证据路径 |
| **仍存在**（OPEN-*） | 已知缺口/风险/待决策项 | 必须给出：影响、证据、建议、优先级；不阻塞当前功能时标注优先级 |
| **可接受残留**（ACCEPT-*） | 已知但不计划在本轮处理的历史痕迹 | 说明不处理原因（历史记录/由其他文档维护/无功能影响） |

## 索引

### 已修复（详见 [fixed.zh.md](fixed.zh.md)）

| ID | 问题 | 严重度 | 修复轮次 |
|---|---|---|---|
| [FIX-P0-01](fixed.zh.md#fix-p0-01-reload-后-sigterm-shutdown-死锁) | `/reload` 后 SIGTERM 永久挂死（shutdown 死锁） | P0 | R5 / 修复轮 |
| [FIX-P1-01](fixed.zh.md#fix-p1-01-usage-锚定计量主回合死路径) | usage 锚定计量在主回合从不生效 | P1 | 修复轮 |
| [FIX-P1-02](fixed.zh.md#fix-p1-02-pre-step-有序表不可观测) | pre-step 有序表无日志/无回合级 E2E | P1 | 修复轮（R1 补默认可见） |
| [FIX-P1-03](fixed.zh.md#fix-p1-03-context-pressure-首调用不可触发) | context-pressure 提醒首调用不可触发 | P1 | 修复轮 |
| [FIX-P1-04](fixed.zh.md#fix-p1-04-mcp-servers-only-不自动启用) | 仅配 `tools.mcp.servers` 不自动启用 MCP | P1 | 修复轮 |
| [FIX-R5-01](fixed.zh.md#fix-r5-01-mcp-显式-enabledfalse-语义) | MCP 显式 `enabled=false` 未被尊重 | P2（R5） | R5 收尾 |
| [FIX-P2-01](fixed.zh.md#fix-p2-01-配置显式-0-被静默补默认) | 配置显式 `0` 被静默补默认 | P2 | 修复轮 |
| [FIX-P2-02](fixed.zh.md#fix-p2-02-supersede-仅回合起点生效) | supersede 仅回合起点生效 | P2 | 修复轮 |
| [FIX-P2-03](fixed.zh.md#fix-p2-03-批调度-capability-名实不一致) | 批调度 capability 名实不一致 | P2 | 修复轮 |
| [FIX-P2-04](fixed.zh.md#fix-p2-04-配置样例与删除残留) | 配置样例 v3/删除残留（config.example、dependabot、goreleaser、架构文档） | P2 | 修复轮 / 收尾 |
| [FIX-P2-05](fixed.zh.md#fix-p2-05-ci-workflows-已删功能引用) | CI workflows 仍引用已删 web/launcher/docker | P2 | 收尾 |
| [FIX-P2-06](fixed.zh.md#fix-p2-06-文档漂移) | 文档漂移（pre-step 顺序/spill/指标/token/AGENTS 语义等） | P2 | 修复轮 / 收尾 |
| [FIX-P2-07](fixed.zh.md#fix-p2-07-gomod-go-指令与-tidy) | go.mod go 指令升级、`go mod tidy -diff` FAIL、discordgo replace 残留 | P2 | 修复轮 / 收尾 |
| [FIX-R5-02](fixed.zh.md#fix-r5-02-网关覆盖率补强) | gateway 覆盖率 17.7%（启动/信号/reload 盲区） | P2（R5） | R5 收尾 |
| [FIX-R5-03](fixed.zh.md#fix-r5-03-r8-loop-guard-接线测试) | R8 loop-guard 接线缺少测试锁定 | P3（R5） | R5 收尾 |
| [FIX-R5-04](fixed.zh.md#fix-r5-04-残余清理与文档-r10) | `.gitignore`/`.golangci.yaml`/vendor/`.dockerignore` 残留与文档 R10 | P3（R5） | R5 收尾 |
| [FIX-R6](fixed.zh.md#fix-r6-mcp-最新协议同步2025-11-25与-structuredcontent) | MCP 最新协议（2025-11-25）同步与 structuredContent 透传 | 能力补齐 | 专题轮 |
| [FIX-R7-01](fixed.zh.md#fix-r7-01-provider-返回-nilnil-导致-nil-deref-panic) | provider 返回 (nil,nil) 导致 nil deref panic | P1 | 健壮性轮 |
| [FIX-R7-02](fixed.zh.md#fix-r7-02-hookactionabortturn-中途中止留下悬空-tool_call) | HookActionAbortTurn 中止留下悬空 tool_call | P1 | 健壮性轮 |
| [FIX-R8](fixed.zh.md#fix-r8-新增-dsh-独立工具组14-工具-go-重写) | 新增 DSH 独立工具组（14 工具 Go 重写） | 能力补齐 | DSH 工具轮 |
| [FIX-R9](fixed.zh.md#fix-r9-可选子系统初始化失败导致-agent-消息循环静默退出) | 可选子系统初始化失败致 agent 消息循环静默退出 | P1 | 健壮性修复 |
| [FIX-R10](fixed.zh.md#fix-r10-mcp-2026-06-30-原生支持不降级) | MCP 2026-06-30 原生支持（不降级） | 能力补齐 | MCP 协议轮 |
| [FIX-R11](fixed.zh.md#fix-r11-session-metajson-损坏自愈open-12) | session meta 损坏自愈 | P1 | 残余修复轮 |
| [FIX-R12](fixed.zh.md#fix-r12-sigkill-时-exec-子进程孤儿open-13) | SIGKILL 时 exec 子进程孤儿 | P2 | 残余修复轮 |
| [FIX-R13](fixed.zh.md#fix-r13-日志目录只读时-panic-退出open-15) | 只读日志目录 panic 退出 | P3 | 残余修复轮 |
| [FIX-R14](fixed.zh.md#fix-r14-大参数错误可诊断性--fallback-成功日志open-18) | 大参数错误诊断 + fallback 日志 | P2 | 残余修复轮 |
| [FIX-R15](fixed.zh.md#fix-r15-background-jobs-stale-清理--mcp-toolslist_changed-重注册open-17-部分) | jobs stale 清理 + MCP 动态重注册 | P2 | 残余修复轮 |
| [FIX-R16](fixed.zh.md#fix-r16-负-usage--负迭代上限--sessionsave-失败可观测性open-16) | 负 usage/迭代钳制/Save 可观测 | P2 | 残余修复轮 |
| [FIX-R17](fixed.zh.md#fix-r17-异步-summarize-与-durable-context-竞态open-04) | summarize 与 durable context 竞态 | P3 | 覆盖率/竞态轮 |
| [FIX-R18](fixed.zh.md#fix-r18-mcp-stdio-子进程生命周期--live-mcp-阶段open-070817) | MCP stdio 生命周期 + live P11 阶段 | P2 | MCP/覆盖率轮 |

### 仍存在（详见 [open.zh.md](open.zh.md)）

| ID | 问题/缺口 | 优先级 |
|---|---|---|
| [OPEN-17](open.zh.md#open-17p3dsh-工具组剩余接线限制) | DSH 工具组剩余项：resources/list_changed、同步 subagent 可见性、terminal/working_directory 等不迁移项 | P3 |

> 其余 OPEN-01..OPEN-19 已在“残余问题修复轮”关闭；逐项解决方式与证据见 [open.zh.md](open.zh.md) §二 的“已解决”表。

### 可接受残留（详见 [open.zh.md](open.zh.md) 末节）

- [ACCEPT-01](open.zh.md#accept-01-docsdesign-历史文档旧表述)：`docs/design` 历史审计/计划文档中的旧表述。
- [ACCEPT-02](open.zh.md#accept-02-并行专题文档已落地历史说明)：并行专题文档已落地，保留历史说明。
- [ACCEPT-03](open.zh.md#accept-03-d4supersede-只收敛模型可见层)：D4 JSONL 保留审计副本（设计约束）。
- [ACCEPT-04](open.zh.md#accept-04-go-版本升级流程约束)：Go 版本升级需同步 tidy/CI。
- [ACCEPT-05](open.zh.md#accept-05-dsh-运行时ui-专属机制不迁移)：DSH 运行时/UI 专属机制不迁移。
- [ACCEPT-06](open.zh.md#accept-06-agent-循环设计性防御性残余)：Agent 循环设计性/防御性残余。

## 维护约定

1. **新增问题**：在 [open.zh.md](open.zh.md) 追加 `OPEN-xx`，写清影响、证据（日志/JSONL/命令输出路径）、建议与优先级；不要复制大段报告，用链接引用。
2. **修复问题**：在 [fixed.zh.md](fixed.zh.md) 追加 `FIX-<severity>-xx`，补：现象、根因、修复文件（未 commit 标注"工作树"）、回归测试名、复测证据；同时在 [open.zh.md](open.zh.md) 将原 `OPEN-xx` 标记为已关闭（保留 ID 便于追溯）。
3. **证据优先**：指向 `build/functest-20261030/**` 的具体文件/章节或 `docs/design/agent-functional-test-report.zh.md` 的章节，避免只写结论。
4. **状态快照**：本库随项目状态更新；整体状态见 [项目状态](../project/status.zh.md)，每次门禁复跑后同步刷新。