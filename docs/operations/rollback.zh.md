# 回滚与降级演练

> 适用：Chagent（chess 基础 agent）在上下文/自主续跑/技能目录等新机制引入后的
> 故障回退。核对日期：2026-10-30。
> 原则：所有新机制都有配置级回退，回退不需要改代码、不需要改配置文件以外的数据。

## 1. 快速降级开关

| 机制 | 回退配置 | 行为 |
|---|---|---|
| 工作区指令上下文 | `agents.defaults.workspace_context.enabled=false` | 不扫描/不注入 AGENTS.md，回到仅 system prompt |
| 运行时快照 | `agents.defaults.runtime_context.mode="system"` | 时间/会话/发送者回到 system prompt（旧路径） |
| 自主续跑 | `agents.defaults.autonomy.enabled=false` | turn 结束后不再调度自主轮；已有任务状态保留 |
| 自主续跑（单会话） | `/stop` 或 `task_update pause` | 暂停该会话任务，不自动恢复 |
| 技能目录 context | `tools.skills.catalog_mode="system"` | 技能表回到 system prompt（旧行为） |
| 技能渐进披露 | `tools.skill_load.enabled=false` | 不注册 `skill` 工具，目录自动回退 system |
| 旧压缩策略 | `agents.defaults.context_manager="legacy"`（默认） | 使用 legacy ContextManager（摘要 + 强制压缩） |
| 工具结果裁剪 | 无外部开关 | 裁剪只在压缩触发时发生；如需保留完整输出，见 spill 目录 |
| 并行工具调度 | 无需开关 | 仅在“无已挂载 hook + 相邻 parallel-safe 调用 + 无排队 steering”时启用；否则串行 |
| 重复工具提醒 | 无外部开关（阈值 3/5/8） | 仅建议式提醒，从不阻断调用 |
| 上下文压力提醒 | 无外部开关 | 仅建议式提醒；硬保护仍是 `isOverContextBudget` |

## 2. 回滚演练步骤（每次发版前）

1. **备份**：`cp config.json config.json.bak`；确认 `sessions/` 有近期备份策略。
2. **关新机制**：依次设置上表开关为回退值，启动 `gateway --name <name>`：
   - `curl -fsS http://127.0.0.1:<port>/health` 返回 200；
   - 发送一条普通聊天消息，确认有最终答复；
   - `/context` 不再出现 workspace/runtime/task 段（或按开关预期降级）；
   - `.chagent.pid` 正常写入并在 SIGTERM 后删除。
3. **数据兼容**：回退后旧 session 仍可加载（context 消息被忽略，不影响 user/assistant
   历史）；确认没有 JSONL 解析错误。
4. **恢复**：删回退键（或恢复备份 config.json），重启并验证 `/health` 与一轮对话。
5. **spill 清理**：如回滚后不再需要落盘内容，可删除 `<workspace>/.chagent/spill/`；
   该目录只被裁剪 marker 引用，删除不影响会话可读性。

## 3. 故障排查联动

- 启动失败：`docs/operations/troubleshooting.zh.md`、`docs/operations/debug.zh.md`。
- 配置校验错误：见 `docs/guides/configuration.zh.md` §5（加载即失败，不做静默降级）。
- pico/chess 联调：`docs/guides/chess-pico-e2e.zh.md` 的 E2E 脚本。

## 4. 记录要求

每次回滚演练记录：日期、版本/commit、触发原因、回退项与耗时、验证命令输出摘要、
恢复时间。建议追加到发布记录或 `docs/design/agent-workspace-context-execution-report.md`。
