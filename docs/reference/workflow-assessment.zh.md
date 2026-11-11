# DSH `workflow` 工具必要性评估与迁移决策

> 日期：2026-11-02。评估对象：DSH `packages/workflow`（`workflow` 工具 + `workflow-ptc` 引擎）。
> 结论先行：**对 Chagent chess 基础 agent 不构成必要功能，本轮不迁移**；若未来出现明确的“脚本化大规模子代理编排”需求，应另立 Go 原生编排设计，而不是移植 JS/PTC 运行时。
> 证据均来自 DSH 源码/README 与 Chagent 当前实现（只读分析）。

## 1. DSH `workflow` 是什么

- **模型可见工具** `workflow`：模型提交 `meta` + JavaScript `script` + `args`，脚本通过
  `agent()`、`parallel()`、`pipeline()`、`phase()`、`log()` 等 hook 扇出大量子代理，脚本最终返回 JSON；
  支持 `run_in_background`（注册为 jobs，流式输出）。见 DSH `packages/workflow/tool-workflow/README.md:12,28`。
- **执行引擎** `workflow-ptc`：在 **Node PTC 进程**中运行脚本；需要 subagents、sandbox policy、
  Node PTC runtime；默认 `maxTotalAgents=1000`，并发/超时/资源由 PTC provider 限制；
  `packages/workflow/workflow-ptc/README.md:28,43,47`。
- **使用建议（DSH 自身）**：只应在用户**显式要求 workflow/大规模多代理编排**时使用；
  一两个委派优先普通 subagent（`tool-workflow/README.md:12,28`）。
- **安全边界**：脚本运行在 Node PTC 沙箱/文件策略下，但 **文件策略不限制网络访问**
  （`workflow-ptc/README.md:59`）；VM 只是 API 边界，不是安全边界。

## 2. Chagent chess 的现状与需求

| 维度 | Chagent 现状 | 是否需要 DSH workflow |
|---|---|---|
| 任务书/合同 | `docs/design/agent-workspace-context-task-book.zh.md`、`docs/chess-integration.md` 未要求 workflow/脚本编排；chess 只需 gateway+pico+agent loop+工具+上下文+TaskState+自主续跑 | ❌ 无需求来源 |
| 多代理能力 | 已有 `spawn`/`spawn_status`/`subagent`/`delegate` + `SubagentManager`，并有 DSH 控制面工具 `list_agents`/`wait_agent`/`interrupt_agent`；并行工具批调度 `tool_batch` | ✅ 1–N 个子任务已可委派 |
| 长任务 | TaskState + autonomy（多轮续跑、检查点、恢复）；`jobs` 工具支持后台命令 | ✅ 长任务已有承载 |
| 运行时 | 单 Go 二进制；无 Node/JS/PTC 运行时；chess 裁剪目标就是轻量常驻 gateway | ❌ 迁移需新增 Node/JS 运行时与沙箱 |
| 安全 | 固定 workspace、exec deny pattern、文件边界、hook/MCP 权限；无“模型写 JS 执行”面 | ❌ 会显著扩大攻击面 |
| 运维 | 单二进制/单进程部署，无 Node 依赖；chess 侧无 PTC 版本管理 | ❌ 增加部署/版本/沙箱复杂度 |
| 兼容性 | DSH workflow 的脚本 API（JS hooks、PTC 传输、workflow 事件）是 DSH 专属契约，与 Go agent 无兼容层 | ❌ “重写”等于重做一套运行时，不是工具迁移 |

## 3. 必要性判定

| 判定标准 | 结论 |
|---|---|
| 是否是 chess 必需功能（任务书/用户契约） | 否 |
| 是否已有等价/足够替代 | 是：subagent/spawn/delegate + 并行批调度 + autonomy/TaskState + jobs |
| DSH workflow 的独有能力是否被 chess 场景需要 | 否：1000 子代理脚本扇出、后台 workflow job、JS 编排属于 DSH 大规模编排场景；chess 基础 agent 无此需求 |
| 迁移成本/风险是否可接受 | 不可接受：需引入 Node PTC 运行时、脚本沙箱、并发/资源上限、网络不受文件策略限制的安全面；与“轻量 chess 基础 agent”目标冲突 |
| 不迁移是否影响已有验收 | 否：§11.1 矩阵、live E2E、配置矩阵、健壮性均不依赖 workflow |

**决策：不迁移 `workflow` 工具**。保留在问题库 ACCEPT-05（DSH 运行时/UI 专属不迁移），并在
[DSH 工具组](dsh-tools.zh.md) §3.2 记录。

## 4. 替代方案（chess 场景）

1. **少量委派**：直接 `subagent`（同步）或 `spawn`（后台）调用；需要查询/中断时用
   `list_agents`/`wait_agent`/`interrupt_agent`。
2. **并行独立任务**：模型可在单回合发出多个工具调用，由 `tool_batch` 并行调度。
3. **跨回合长任务**：TaskState + autonomy 续跑；需要外部长命令时用 `job_*` 工具。
4. **未来若出现明确脚本化编排需求**：建议设计 **Go 原生声明式 workflow**（JSON/YAML steps/DAG、
   每步 subagent、失败/并发策略、事件可观测），而不是移植 DSH 的 JS/PTC 契约；需单独评估权限模型
   （workspace/网络/子代理配额）。

## 5. 若未来重新评估的准入条件

满足任一条件才建议立项迁移：

- chess 产品明确需要“用户可写脚本”的大规模编排，且接受 Node/PTC 运行时依赖；
- 需要 DSH workflow 的跨进程隔离/后台 job 流式语义，且无法用 Go 声明式编排满足；
- 安全评审通过“模型生成代码执行 + 网络不受文件策略限制”的风险模型；
- 有可复用的 Go 沙箱/PTC 运行时（当前 Chagent 无）。

## 6. 证据索引

- DSH：`packages/workflow/tool-workflow/README.md:12,28`、
  `packages/workflow/workflow-ptc/README.md:28,43,47,59`、
  `packages/bundle/base/package.json:113`（默认 bundle 挂载，但使用建议限定显式请求）。
- Chagent：`pkg/tools/subagent.go`、`pkg/tools/spawn.go`、`pkg/agent/tool_batch.go`、
  `pkg/agent/autonomy_runtime.go`、`pkg/tools/dsh/jobs/`；任务书/chess 集成文档（无 workflow 要求）。
- 关联：[DSH 工具组](dsh-tools.zh.md) §3.2、[DSH 工具/功能对标](dsh-parity.zh.md)、
  [问题库 ACCEPT-05](../issues/open.zh.md#accept-05-dsh-运行时ui-专属机制不迁移)。