# Chagent 文档

本仓库只保留 chess 基础 agent 仍然有效的中文文档。其余多通道、迁移、英文/日文/法文等翻译文档已删除。

## 项目

- [项目介绍](project/introduction.zh.md)：项目是什么、能力边界、快速上手、关键概念、与 chess 的契约
- [项目状态（2026-11-02 快照）](project/status.zh.md)：HEAD/工作树状态、功能完成度、测试与验证结论、已知残余
- [变更摘要（2026-11-11）](project/change-summary-2026-11-11.zh.md)：默认提示词任务/记忆/角色策略、任务板、工具开关兼容与工具清单核对
- [重命名记录（PicoClaw → Chagent，不兼容旧名）](project/rename-to-chagent.zh.md)：名称映射、构建启动、外部对接修改

## 问题库

- [问题库索引](issues/README.md)：状态口径（已修复/仍存在/可接受残留）与全部条目索引
- [已修复问题](issues/fixed.zh.md)：FIX-*，含现象、根因、修复点、回归测试与验证证据
- [仍存在的问题 / 可接受残留](issues/open.zh.md)：OPEN-* 与 ACCEPT-*

## 架构

- [代码地图](architecture/code-map.zh.md)
- [工作区上下文](architecture/workspace-context.zh.md)
- [运行时上下文](architecture/runtime-context.zh.md)
- [任务状态与自主](architecture/taskstate-autonomy.zh.md)
- [路由系统](architecture/routing-system.zh.md)
- [运行时事件](architecture/runtime-events.zh.md)
- [会话系统](architecture/session-system.zh.md)
- [上下文处理 vs DSH](architecture/context-vs-dsh.zh.md)
- [Hook 系统索引](architecture/hooks/README.zh.md)
- [Hook JSON 协议](architecture/hooks/hook-json-protocol.zh.md)
- [Hook 工具注入](architecture/hooks/plugin-tool-injection.zh.md)

## 指南

- [Chess 最小配置](guides/chess-config.zh.md)
- [Chess 集成（启动/关闭/配置/排错）](chess-integration.md)
- [Chess Pico 端到端](guides/chess-pico-e2e.zh.md)
- [配置](guides/configuration.zh.md)
- [Provider](guides/providers.zh.md)
- [路由指南](guides/routing-guide.zh.md)
- [会话指南](guides/session-guide.zh.md)
- [Spawn 任务](guides/spawn-tasks.zh.md)

## 设计文档

- [工作区上下文与自主连续性改进计划](design/agent-workspace-context-plan.zh.md)
- [Prompt 规范](design/agent-workspace-context-prompts.zh.md)
- [执行任务书](design/agent-workspace-context-task-book.zh.md)
- [执行过程记录](design/agent-workspace-context-execution-report.md)
- [任务书执行审计](design/chagent-taskbook-audit.zh.md)
- [DSH 模型执行与 Agent 循环](design/dsh-agent-loop-model-exec.zh.md)
- [DSH 上下文与 Prompt](design/dsh-context-prompt.zh.md)
- [上下文预算与 context_pressure 超限排查记录](design/context-budget-audit.zh.md)
- [上下文预算审计复核（固定开销根因）](design/context-budget-audit-review-2026-11-12.zh.md)
- [DSH → Chagent 迁移报告](design/dsh-migration-report.zh.md)
- [裁剪清单](design/chess-trim-inventory.md)
- [新实现 Agent 完整真实场景功能测试报告](design/agent-functional-test-report.zh.md)
- [Hook 系统设计](design/hook-system-design.zh.md)

## 操作与安全

- [调试](operations/debug.zh.md)
- [故障排查](operations/troubleshooting.zh.md)
- [回滚与降级演练](operations/rollback.zh.md)
- [Agent 健壮性与故障恢复](operations/robustness.zh.md)
- [安全配置](security/credential_encryption.zh.md)
- [敏感数据过滤](security/sensitive_data_filtering.zh.md)

## 参考

- [工具参考](reference/tools.zh.md)
- [MCP 支持矩阵](reference/mcp-support.zh.md)
- [DSH 工具/功能对标](reference/dsh-parity.zh.md)
- [DSH 工具组（已迁移）](reference/dsh-tools.zh.md)
- [DSH workflow 必要性评估（决定不迁移）](reference/workflow-assessment.zh.md)
- [覆盖率报告（关键路径 ≥90% 口径）](reference/coverage.zh.md)
- [模型可见文本中文规范与审计](reference/model-facing-text.zh.md)
- [工具与权限](reference/tools.zh.md)
- [anthropic-messages 协议（X-API-Key）](reference/anthropic-messages-x-api-key.md)