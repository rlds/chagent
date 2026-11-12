# Chagent Chess 基础 Agent · 项目状态

> 快照日期：**2026-11-02**（Asia/Shanghai，残余问题修复轮后）。
> 口径：分支 `v2`；项目已重命名为 **Chagent**，本轮重命名提交 `de3e4437`，随后提交中文模型文本/构建启动脚本/移除旧名兼容。
> 相关：[项目介绍](introduction.zh.md) · [问题库](../issues/README.md) · [覆盖率报告](../reference/coverage.zh.md) · [完整测试报告](../design/agent-functional-test-report.zh.md)

## 1. 代码与工作树状态

| 项 | 值 |
|---|---|
| 分支 / 最新代码提交 | `v2` / `db13b1e1`（Chagent + rlds module + 中文注释/文档/CLI/技能）；module 重命名提交 `85ec8da4` |
| 本轮提交 | P0/P1/P2 修复、E2E/配置/上下文脚本、项目文档与问题库、MCP 2026-06-30 原生、DSH 工具组 14 工具、残余缺陷修复、覆盖率/竞态/MCP 生命周期修复、**项目重命名 chagent**、中文模型文本、构建/启动脚本（详见 `git log`） |
| 工具链 | 本机 `go1.27.3 darwin/arm64`；`go.mod` 指令 `1.27.3`；vendor 模式（本地 go-sdk fork `third_party/mcp-go-sdk`） |
| 包规模 | `go list ./pkg/...` = 45 个包（含 `pkg/tools/dsh/*`） |
| 文档 | `docs/` 中文 Markdown：项目/问题库/架构/指南/参考/操作/安全/设计，索引见 [docs/README.md](../README.md)；重命名记录见 [rename-to-chagent](rename-to-chagent.zh.md)，模型文本审计见 [model-facing-text](../reference/model-facing-text.zh.md) |
| 交付状态 | 代码、测试、脚本、文档均已提交；只读门禁不产生跟踪文件（`build/` 为 gitignored 证据目录） |

## 2. 功能完成度

| 维度 | 状态 | 说明 |
|---|---|---|
| 任务书 §11 验证矩阵 | ✅ 全绿 | build/test/race/vet/lint/tidy/chess-e2e/measure/coverage 全部通过；覆盖率见 §3 |
| 项目重命名 Chagent | ✅ 完成（不兼容旧名） | module/binary/env/home/pid/技能/文档统一 chagent；Go module 为 **`github.com/rlds/chagent`**；旧 `PICOCLAW_*`/`DDMCCLAW_*`、旧二进制、`~/.picoclaw` 均不再兼容；外部对接按 [重命名记录](rename-to-chagent.zh.md) 修改 |
| 代码注释/说明/文档中文化 | ✅ 完成 | 生产代码注释（含包注释/导出 API/行内说明）、内置技能文档、脚本注释、包 README 全部中文；技术标识/命令/URL/许可证标识保留；文档目录 `docs/` 全中文 |
| 构建/启动脚本 | ✅ 完成 | `scripts/build.sh` → `bin/chagent`；`scripts/start.sh`（缺省自动构建、透传 gateway 参数）；`make build` 输出 `bin/chagent` |
| 模型可见文本中文 | ✅ 完成 | system prompt/注入 context/工具 Description+Parameters/工具结果包装/默认回复全部中文；审计见 [model-facing-text](../reference/model-facing-text.zh.md) |
| DSH agent-loop 迁移 | ✅ 完成 | 34 项口径两轮迁移；剩余 DSH 运行时/UI 专属项记录为 ACCEPT-05 |
| DSH 独立工具组 | ✅ 完成 | 14 个 Go 重写工具（glob/grep、jobs、MCP resources、session query、subagent control、goal），默认启用、可分组关闭 |
| MCP 协议 | ✅ 2026-06-30 原生 | initialize 请求 2026-06-30、不降级；2025-11-25 等可协商；structuredContent、tools/list_changed 动态重注册、stdio 生命周期修复；P11 live E2E PASS=24/0 |
| 健壮性 | ✅ 缺陷清零 | 故障注入 35/0；进程级 112/0/1（+2 残留检查）；FIX-R9/R11–R14/R17/R18 |
| 覆盖率 | ✅ 关键路径达成 | agent 控制面 91.0%、gateway 91.6%、config 92.2%、pico 93.6%；agent 整体 85.0%、四包合计 86.9% |
| 已知残余 | 仅能力边界 | 问题库 OPEN-17（DSH 剩余接线/不迁移项）+ ACCEPT-01..06 |

## 3. 测试与验证结论

| 验证 | 结果 | 证据 |
|---|---|---|
| §11.1 最终门禁 | build-cmd/build-all/test-pkg/race-4pkgs/vet/lint-docs/tidy-diff/chess-e2e/measure/coverage **全部 exit=0** | `baseline/final-gate/fg2-*.log` |
| `go test ./pkg/...` | 45 包，**0 FAIL**（91s） | `fg2-test-pkg.log` |
| `go test -race` 四包 | agent/gateway/config/pico **0 DATA RACE**（88s） | `fg2-race-4pkgs.log` |
| compat（8 脚本） | **163 PASS / 0 FAIL** | `baseline/compat/logs/` |
| 真实场景 live E2E | **101 PASS / 0 FAIL / 0 SKIP**（最终全量，真实模型）；MCP P11 专项 **24/0** | `live/final/`、`live/p11-mcp/` |
| 模型可见文本语言审计 | 6 类模型可见文本全中文；例外仅机器协议/技术标识/内部匹配串 | `build/functest-20261030/model-text/`、`docs/reference/model-facing-text.zh.md` |
| 配置能力矩阵（A–F） | **43/0**（含 MCP D0/D0b/D1–D3） | `config/` |
| 上下文窗口专项（A–H） | **94 断言 0 FAIL** | `context/` |
| 健壮性（故障注入） | **35 PASS / 0 FAIL** | `robustness-live/` |
| 健壮性（进程级） | **112 PASS / 0 FAIL / 1 SKIP**（含残留检查） | `robustness-process/` |
| 覆盖率 | agent 85.0%（控制面 **91.0%**）、gateway **91.6%**、config **92.2%**、pico **93.6%**；四包合计 **86.9%** | `coverage/`、`fg2-coverage-*` |
| 性能/体积 | 二进制 21,194,274 B；启动到 health **73 ms**；RSS **19,504 KB** | `fg2-measure.log` |

## 4. 覆盖率明细

| 范围 | 基线 | 最终 | 目标 | 判定 |
|---|---:|---:|---:|---|
| `pkg/agent` 整体 | 78.9% | **85.0%** | 整体 ≥85% | ✅ |
| `pkg/agent` 关键路径控制面 | — | **91.0%** | **≥90%** | ✅ |
| `pkg/gateway` | 76.5% | **91.6%** | ≥90% | ✅ |
| `pkg/config` | 69.7% | **92.2%** | ≥90% | ✅ |
| `pkg/channels/pico` | 62.6% | **93.6%**（3 次 93.0–93.6） | ≥90% 且稳定 | ✅ |
| `pkg/agent/workspacecontext` | 83.0% | **83.0%** | 新增包 ≥80% | ✅ |
| 四包合计 | 76.7% | **86.9%** | — | 记录 |

> “关键路径”定义、逐文件数据与未覆盖原因见 [覆盖率报告](../reference/coverage.zh.md)。

## 5. 已知残余

| ID | 残余 | 影响 | 状态 |
|---|---|---|---|
| [OPEN-17](../issues/open.zh.md) | DSH 剩余接线/不迁移项：MCP resources/list_changed、同步 subagent 控制面可见性、terminal/working_directory 等 | 能力边界，不阻塞主链路 | 跟进（P3） |
| [ACCEPT-03](../issues/open.zh.md) | D4 JSONL 保留审计副本 | 事后断言需按模型可见视图 | 接受 |
| [ACCEPT-04](../issues/open.zh.md) | Go 版本升级需同步 tidy/CI | 流程约束 | 接受 |
| [ACCEPT-05](../issues/open.zh.md) | DSH 运行时/UI 专属机制不迁移（含 workflow 专项评估：不迁移） | 能力边界 | 接受 |
| [ACCEPT-06](../issues/open.zh.md) | Agent 循环设计性/防御性残余 | 低风险 | 接受 |
| 重命名破坏性变更 | 旧 `PICOCLAW_*`/`DDMCCLAW_*`、旧二进制、`~/.picoclaw` 不兼容 | 外部对接必须改用 `CHAGENT_*`/`bin/chagent`/`~/.chagent` | 已记录（[rename-to-chagent](rename-to-chagent.zh.md)） |

> OPEN-01..OPEN-19 的关闭方式/证据见 [问题库已解决表](../issues/open.zh.md#二已解决本轮关闭保留索引便于追溯)。

## 6. 证据索引

| 证据目录 | 内容 |
|---|---|
| `build/functest-20261030/baseline/final-gate/` | 最终门禁（`fg2-*`）、覆盖率、性能、兼容回归 |
| `build/functest-20261030/baseline/compat/` | 8 脚本 163 项兼容/删除/生命周期回归 |
| `build/functest-20261030/live/` | 最终全量 live E2E（101/0，`final/`）；`p11-mcp/` 为 MCP 专项（24/0） |
| `build/functest-20261030/config/` | 配置能力 A–F（MCP/Hook/路径权限/参数校验） |
| `build/functest-20261030/context/` | 上下文窗口 A–H 专项 |
| `build/functest-20261030/robustness-live/` | 35 项故障注入矩阵 |
| `build/functest-20261030/robustness-process/` | 进程级健壮性 112/0/1 |
| `build/functest-20261030/coverage/` | 四包覆盖率报告与逐函数数据 |
| `build/functest-20261030/analysis/` | DSH 对标、上下文对比、open04、agent 健壮性等专题 |
| `docs/reference/{coverage,mcp-support,dsh-parity,dsh-tools,model-facing-text}.zh.md`、`docs/project/rename-to-chagent.zh.md`、`docs/architecture/context-vs-dsh.zh.md`、`docs/operations/robustness.zh.md` | 专题文档 |

## 7. 下一步建议

1. **OPEN-17**：如 chess 出现长驻交互终端需求，单独立项迁移 terminal；MCP `resources/list_changed` 在引入资源缓存时再接线。
2. **执行面覆盖率**：若验收要求 `pipeline_execute.go` 也 ≥90%，另立执行面夹具（当前 73.3%）。
3. **发布**：按 `v2` 分支打 tag 前跑一次 `final-gate` + compat；`build/` 产物不入制品。
4. **安全**：`build/functest-20261030/live/workspace/config.json` 已脱敏；后续产物分享前继续遵守脱敏要求。
5. **外部对接迁移**：按 [重命名记录](rename-to-chagent.zh.md) 将二进制/环境变量/home/pid 全部切换到 chagent；本项目不保留旧名兼容。
