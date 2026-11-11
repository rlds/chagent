# Chagent 新实现 Agent · 完整真实场景功能测试验证报告

> 状态：已执行 + **已修复并复测**（真实模型 + 真实 gateway + pico WebSocket）
> 日期：2026-10-30（初测 15:38–17:30；修复与复测 17:31–18:20）
> 被测基线：`HEAD 5da70e0b`（分支 `ddmcv4`，提交信息 `1`，2026-10-30 15:09:39 +0800）
> 环境：macOS darwin/arm64，`go version go1.27.3`，`go.mod` go 指令 `1.27.3`，vendor 构建
> 依据：
> - 任务书：`docs/design/agent-workspace-context-task-book.zh.md`（v0.2，§0 执行契约、§11 测试验证矩阵）
> - 迁移设计：`docs/design/dsh-migration-report.zh.md`、`dsh-agent-loop-model-exec.zh.md`、`dsh-context-prompt.zh.md`
> - 独立审计：`docs/design/chagent-taskbook-audit.zh.md`
> - 追加用户要求（本轮）：工具控制、MCP、外部路径与权限控制、Hooks、其他重要参数调整、上下文窗口管理
> 原则：**只做测试验证，不修改生产代码/测试**；所有产物在 `build/functest-20261030/`（gitignored），
> 新增测试脚本见 `scripts/agent-e2e-scenario.py`、`scripts/agent-config-capabilities-test.py`、
> `scripts/agent-context-window-test.py`、`scripts/verify-agent-e2e-artifacts.py` 与 `scripts/fixtures/`。

---

## 0. 结论摘要

### 0.1 总判定

| 维度 | 结果 | 结论 |
|---|---|---|
| 真实场景 E2E（`scripts/agent-e2e-scenario.py`，模型 `deepseek-v4-pro`） | 修复前 **84 PASS / 2 FAIL** → 修复后 **86 PASS / 0 FAIL** | P0 reload→SIGTERM 已修复（P9.1 0.01s 退出、pid 正常删除） |
| 配置能力实测（`scripts/agent-config-capabilities-test.py`，模型 `deepseek-v4-flash`） | 修复前 **38 PASS / 2 FAIL** → 修复后 **42 PASS / 0 FAIL** | P0 clean shutdown 与 MCP 自动启用均修复；显式 0 现在可诊断拒绝 |
| 上下文窗口管理（`scripts/agent-context-window-test.py`） | 8 阶段全执行；修复前实现 0 真实缺陷（6 个原始 FAIL 为脚本误判）；修复后复跑见 §14 | 压缩/摘要/裁剪/spill/边界/幂等/重启恢复均以真实证据通过 |
| 基线命令矩阵（§11.1） | 构建/测试/race/vet/lint-docs/chess-e2e 全 PASS；`go mod tidy -diff` 修复后 **PASS**；覆盖率见 §14 | 功能可用；工程/验收门槛基本满足，覆盖率仍有缺口 |
| Chess 兼容与删除回归（§11.4/§11.5） | 修复前 **148 PASS / 12 FAIL** → 修复后复跑见 §14 | 12 FAIL（配置静默默认/删除残留/reload 死锁）均已修复或收尾 |
| 新机制代码级核对（12 项） | 修复前 9 ✅ / 2 ⚠️ / 1 ❌；修复后 D1/D2/D4/D5/MCP/parallel 已确认修复（§14） | 无 P0；3 个 P1 已修复并有回归测试 |
| 独立产物复核（`verify-agent-e2e-artifacts.py`，不信任 harness 断言） | 修复前 PASS（83/2 known-failure）→ 修复后 **PASS（85 PASS / 0 FAIL / 4 advisory）** | required 失败 0、矛盾 0；P9.1/P9.2 已知缺陷确认消除 |

### 0.2 一句话结论

**新实现 agent 的核心能力已在真实模型真实链路下验证通过**：工作区上下文（baseline/delta/touch 增量）、
真实文件修复任务、TaskState + 自主续跑、skill 渐进披露、loop-guard、上下文压缩 + 工具结果裁剪 + spill、
并行的工具批调度、pico 鉴权/pid/健康检查、重启恢复、越界拒绝、工具启停、MCP、Hooks、参数校验等均有
端到端证据。**初测发现 1 个 P0 生命周期缺陷：任何一次成功 `POST /reload` 之后 `SIGTERM` 永久挂死**
（>30s、pid 残留、只能 kill -9），直接破坏 chess「SIGTERM ≤5s、pid 清理」契约；另有 3 个 P1 与多项 P2。
**修复轮已完成**：P0 与 P1（usage 锚点死路径、pre-step 可观测性/集成测试、context-pressure 首次触发、MCP 自动启用）
及 P2（配置显式 0 校验、迭代内 supersede、批调度 capability、删除残留、文档漂移、go.mod/tidy）均已修复并复测，
见 §14。仅剩覆盖率等验收门槛缺口与少量可接受残留。

---

## 1. 测试方法与环境

### 1.1 分层策略

1. **真实场景链（本报告主证据）**：真实外部配置中的模型（OpenAI 兼容端点）→ 真实 `gateway` →
   pico WebSocket 客户端，逐轮真实对话/工具调用；临时 workspace 放在 git 仓库之外
   （避免 `workspacecontext` 向上发现仓库 `.git`），结束后整体复制回 artifacts。
2. **配置能力矩阵**：同一真实链路，按 A-F 分组验证启动/env/鉴权、工具启停、外部路径权限、MCP、Hooks、参数校验。
3. **上下文窗口专项**：小窗口（12000/1024）+ 150KB 真实工具输出确定性触发压缩/裁剪/summary/pressure。
4. **无模型基线**：构建、全量单测、race、vet、docs lint、chess-e2e、启动指标、覆盖率。
5. **代码级审计**：12 项迁移机制的生产调用链、测试实跑、可观测信号，含 `go test -overlay` 探针复现。
6. **独立产物复核**：第三方脚本只读原始 artifacts 反推 PASS/FAIL，交叉检查 harness 断言是否成立。

### 1.2 真实模型与限流说明

- 模型来源：`/Users/wudaorui/chessdata/weichat/weichat/config.json` 的 `model_list`（报告与脚本均不打印 api_key）。
- 主链路用 `deepseek-v4-pro`，配置/上下文专项用 `deepseek-v4-flash`（分散同 key 的 TPM）。
- 测试期间观察到 provider `429 tpm_rate_limit_exceeded`；harness 增加了 6 次、5s 起线性退避的
  `max_llm_retries` 与客户端重试，并在自主任务中验证了 **provider 错误 checkpoint**（任务保持 active、
  evidence 记录 `provider-error:round=…`、后续轮次继续），未因限流丢失会话/任务。
- 复跑单次约 3–10 分钟（真实模型耗时波动）。

### 1.3 产物布局

```
build/functest-20261030/
├── live/        真实场景 E2E：workspace/（sessions/meta/spill/项目文件）、gateway.log、
│                gateway-restart.log、harness.log、context-*.txt、结果 results.json / summary.md、
│                artifact-analysis.json、config.hash.{before,after-reload,after}、expectations.json
├── config/      配置能力：phase-A..F、results.json、summary.md、config-*.json（脱敏）
├── context/     上下文窗口：ABCDEFGH 证据、results.json、summary.md、workspaces/、spill 证据
├── baseline/    基线矩阵 + compat/（Chess 兼容/删除回归/reload-shutdown 复现与 goroutine dump）
├── mechanism/   代码级核对 mechanism-audit.md + 127 PASS 定向测试日志 + D1 overlay 探针
└── verify/      独立产物验证器自测与真实 artifacts 复核结果
```

> 安全提示：`build/` 已 gitignore；其中 `live/workspace/config.json` 是运行期真实配置（含明文 api_key），
> 对外分享 artifacts 前必须删除/脱敏。报告正文不包含任何密钥。

---

## 2. 真实场景 E2E（主证据）

### 2.1 场景链设计（11 个阶段，一条连续真实使用链）

| 阶段 | 真实场景 | 关键断言 |
|---|---|---|
| P0 | 启动 gateway（真实模型配置，port=0） | pid 字段/与进程一致、`/health`、`/ready`、启动不写回配置、错误 token 401、单例拒绝、pico ping/pong + typing |
| P1 | 工作区上下文基线 | 根 `AGENTS.md`（agent definition 路径）与 `CLAUDE.md`（workspace-context 路径）都被模型读到；`/context` 显示 1 文件；JSONL 有 `workspace-instructions`/`runtime-context`/`skill-catalog` |
| P2 | 上下文热更新 | 改 `CLAUDE.md` 后无需重启，下一轮即读到 `CLAUDE_MARKER_V2`；JSONL 新增 workspace delta；改 `AGENTS.md` 同样即时生效 |
| P3 | **真实开发任务** | 模型读 README/src、跑单测复现失败、修复 `src/stats.py`、重跑测试全绿、写 `FIX_REPORT.md`；独立复跑测试 rc=0；tests 文件哈希未变（未改测试作弊）；工具链 read/exec/edit(write) 均真实执行 |
| P3b | 越界与增量 | 子目录 `docs/CLAUDE.md` 被工具触碰后注入 descendant delta；`read_file /etc/hosts` 被 `path escapes workspace` 拒绝 |
| P4 | **TaskState + 自主续跑** | `task_update start` + `todo_write`；首轮保持 active；外部信号 `WAIT_FLAG.txt` 创建后**自主轮次**读取其内容并写 `AUTONOMY_NOTES.md`，凭证据 `complete`；`agent.autonomy.round` 事件 2 次；`/context` 显示 complete |
| P5 | 技能渐进披露 | `skill` 工具加载 `e2e-skill`，回复含 `SKILL_BODY_MARKER_V1`；`skill-catalog` 以 durable context 注入；`/list skills` 正常 |
| P6 | loop guard | 模型对同一目录连续 4 次 `list_dir`：JSONL 出现 `loop-guard` 提示，日志 5 次真实调用，回合正常收尾 |
| P7 | **上下文窗口管理（现场加压）** | `/reload` 热更新 `context_window=12000/max_tokens=1024`；读 552KB 大文件产生 64KB 工具结果 → proactive compression：日志 `pruned_msgs=1/spilled_msgs=1`；spill 目录 0700/文件 0600/内容 65690B；压缩后继续对话正常；tool_call/result 配对完整；`context-pressure` 提醒出现 1 次 |
| P8 | `/stop` | 任务保持 active/自主轮已调度时 `/stop`：任务变 `paused`，4s 内无新自主轮，`/context` 反映 paused |
| P9 | 重启恢复 | SIGTERM/重启；会话/任务状态恢复、模型能回忆此前修复任务、工作区上下文不重复注入、`/context` 正常 |
| P10 | 清理与产物 | 最终 SIGTERM、pid 删除、health 不可达、无孤儿进程、配置除计划内 reload 外未改写、工作区复制为 artifacts |

### 2.2 结果

- 汇总：**PASS 84 / FAIL 2 / SKIP 0**，总耗时 188.8s。
- **2 个 FAIL 均为同一 P0 缺陷**：`P9.1`（成功 `/reload` 后 SIGTERM 8.00s 仍未退出，被强制 kill）与
  `P9.2`（pid 文件残留）。对照 `P10.1`：重启后（未再 reload）空闲 SIGTERM **0.01s** 退出、pid 正常删除。
- 其它高价值运行时证据：
  - 真实修复任务：`pytest/unittest` 独立复跑 rc=0，`FIX_REPORT.md` 含 `PROJECT_REPORT_MARKER`。
  - 自主续跑：`autonomy round scheduled`×3、`agent.autonomy.round` 事件、`AUTONOMY_NOTES.md` 含外部信号内容
    `AUTONOMY_WAIT_FLAG_CONTENT_42` 与 `AUTONOMY_STEP2_MARKER`（证明是新轮次的真实工具动作）。
  - 并行工具：gateway 日志出现 7 次 `Tool call (batch)`（P3/P5/P6 的相邻只读工具被批调度）。
  - 压缩/裁剪/spill：`Forced compression pruned tool results without dropping turns` + 真实 spill 文件；
    `artifact-analysis.json` 中 `forced_compression=1, proactive_compression=1, spill=1`。
  - `context_kinds`（JSONL）：`workspace-instructions`、`runtime-context`、`task-state`、`skill-catalog`、
    `context-pressure` 均以真实现场出现；`loop-guard` 在运行期快照中确认（后因 summary 截断从最终 JSONL 消失，属合法压缩行为）。

### 2.3 证据入口

- 结果表：`build/functest-20261030/live/summary.md`、`results.json`
- 原始日志：`live/gateway.log`（console 格式）、`live/home/logs/gateway.log`（file logger JSON 行）
- 会话/任务：`live/workspace/sessions/*.jsonl`、`*.meta.json`
- 压缩/spill：`live/workspace/.chagent/spill/…`
- 分析：`live/artifact-analysis.json`、`live/context-p*.txt`

---

## 3. 基线命令矩阵（任务书 §11.1/§11.6）

| 项 | 结果 | 数据 |
|---|---|---|
| `go build ./cmd/chagent` / `go build ./...` | ✅ PASS | 0.45s / 1.03s；产物 20,646,242 B |
| `go test ./pkg/... -count=1` | ✅ PASS | 44 包（37 ok + 7 无测试），0 FAIL，87.38s |
| `go test -race` 四包 | ✅ PASS | 0 DATA RACE，85.47s |
| `go vet ./...` | ✅ PASS | 无输出 |
| `./scripts/lint-docs.sh` | ✅ PASS | `docs lint: OK` |
| `go mod tidy -diff` | ❌ FAIL（既有） | tidy 会重排 require 分组（x/crypto、x/net、x/sys 提为直接依赖）；未执行 tidy、零改动 |
| `./scripts/chess-e2e.sh` | ✅ PASS | health/pico ping-pong/SIGTERM/pid 清理 5 断言 |
| 启动/体积/RSS（3 次） | ✅ PASS | 20.6 MB / 中位 75 ms / 中位 19,296 KB（≈18.8 MiB）；较执行报告 §9 旧数据（38 MB/88–99 ms/27 MB）明显改善 |
| 覆盖率（四包） | ⚠️ 未达阈值 | 合计 **74.8%**：agent 78.9%、workspacecontext 83.0%、gateway **17.7%**、pico 62.8%、config 69.5%（任务书要求关键路径 ≥90%） |
| 四个重点包 `t.Skip` | ✅ 0 处 | 全 `pkg` 56 处集中在平台相关工具测试（带注释，任务书允许） |

**治理异常**：`HEAD 5da70e0b` 仅把 `go.mod` 的 go 指令从 `1.25.11` 改为 `1.27.3`（提交信息 `1`），
违反任务书 §0.7「不要升级 Go 版本或依赖」；本机恰好 1.27.3 故全绿，但 CI 使用 `go-version-file: go.mod`，
≤1.25.x 环境会拒绝构建。

---

## 4. 配置能力实测（工具控制 / MCP / 外部路径权限 / Hooks / 关键参数）

> 脚本：`scripts/agent-config-capabilities-test.py`（真实 gateway + pico WS + 真实模型 `deepseek-v4-flash`；
> 每场景独立临时 workspace、port=0、进程 defer kill）。证据：`build/functest-20261030/config/`。

**（最终计数与定性见 §4.4，以下为已确认行为与缺陷）**

### 4.1 工具控制

- 基线：`exec` 真实执行并返回唯一标记；`list_dir` 初始禁用时不被执行。
- 热更新：改写 `tools.<name>.enabled` 后 `/reload`，禁用 → 无对应 tool_call/成功结果、gateway 无新增成功日志；
  再启用 → 恢复成功。**工具启停真实生效**。

### 4.2 外部路径与权限

实测原始工具结果（不依赖模型口头回答）：
- 默认 `restrict_to_workspace=true`：工作区外 `read_file`/`write_file` 返回
  `access denied: path is outside the workspace`，文件未创建。
- `../` 穿越：返回 `path escapes workspace: ../…`。
- `exec` 危险命令：返回 `Command blocked by safety guard (dangerous pattern detected)`，命令未执行。
- `tools.allow_read_paths`/`allow_write_paths` 的正则语义与 reload 生效性由脚本继续验证
  （配置字段为 repo 路径正则，需要 `^/abs/path/` 锚定；详见 `config/` 证据）。

### 4.3 MCP、Hooks、参数

- MCP：本地 Python stdio MCP server fixture（`scripts/fixtures/mcp_stdio_server.py`）提供 `echo` 工具；
  配置 `tools.mcp.servers` 后 `/list mcp`、`/show mcp` 与真实调用 `mcp_<server>_echo` 的链路见
  `config/` 证据（`mcp_stdio_server.py` 为可复现 fixture）。
- Hooks：process hook（`scripts/fixtures/hook_gate.py`）以 JSON-RPC over stdio 挂载：
  - observer：runtime_event 到达 hook 日志（PASS）；
  - `before_tool` deny_tool：工具被拒且模型看到 hook 拒绝原因（PASS）；
  - `approve_tool` approved=false：工具被拒（PASS）；
  - 热禁用 hook + `/reload`：hook 不再触发、exec 恢复（PASS）；
  - `after_tool` modify：fixture 返回 modify 但 agent 侧未应用，需按 §4.4 定性（脚本结构 vs 产品缺陷）。
- 参数校验（真实启动进程）：
  - 加载期拒绝且可诊断：`version=1`、`api_keys` 复数、`autonomy.max_rounds=201`、
    `no_progress_rounds=11`、`runtime_context.mode=bogus`、`tools.skills.catalog_mode=bogus`（rc=1）；
  - **静默降级**：`autonomy.max_rounds=0`、`no_progress_rounds=0`、`workspace_context.max_bytes=0`
    不会报错，被 `ApplyDefaults` 补成默认值（违反「非法值可诊断」预期，P2 缺陷，见 §10）；
  - `max_tool_iterations=3`：3 次工具调用后命中安全阀，turn 正常收尾、gateway 健康（不 hang）；
  - `catalog_mode=context|system`：两种模式均可启动；context 模式 JSONL 出现 `skill-catalog`，system 模式无
    durable catalog 但 `skill` 工具可用；`steering_mode=one-at-a-time`/`max_parallel_turns=1` 不崩溃。

### 4.4 汇总与定性

最终结果（`config/results.json` + `summary.md`，全量 A-F 一次运行）：**38 PASS / 2 FAIL / 0 SKIP（40 项）**，总耗时约 244s（含 E 组修正 prompt 复跑约 275s）。2 个 FAIL 定性：

| ID | 现象 | 定性 |
|---|---|---|
| CFG-P0-1 | **成功 `/reload` 后 SIGTERM 永久挂死**（20.01s 后强杀、pid 残留；未 reload 对照 0.0–1.3ms 正常） | **P0 真实生产缺陷**，与 §5/§10 同一根因 |
| CFG-P1-1 | **仅配置 `tools.mcp.servers`（不写 `tools.mcp.enabled`）时 MCP 不自动启用**：connected=False、gateway 日志 `MCP integration is disabled` | **P1 真实缺陷**：`pkg/mcp/manager.go:204` 用 `if !mcpCfg.Enabled` 而不是 `MCPConfig.Active()`；与 `docs/reference/tools.zh.md`「配置 servers 即自动启用」及 `MCPConfig.Active()` 的设计相反。显式写 `enabled: true` 后链路正常（D1-D3 PASS） |
| — | E3 deny-tool 首轮：测试命令标记含 `MUST_NOT_RUN`，模型自行拒绝执行导致 hook 未触发 | 测试 prompt 问题，非产品缺陷；改中性标记后复跑 **E3 PASS** |

> 其它能力：A 10/11（仅 P0）、B 工具启停 4/4、C 外部路径权限 4/4、D MCP 4/5（D1-D3 全链路 PASS）、
> E Hooks 5/5、F 参数校验/关键参数 12/12。

（6 类非法值正确拒绝；`max_rounds=0`/`no_progress_rounds=0` 被静默补默认，脚本以“实际行为”记录）。

> 说明：F 组的 `max_tool_iterations=3` 安全阀、`catalog_mode=context|system` 两种模式、steering/并发参数
> 在 phase-F 的独立运行中已验证通过（见 `config/phase-F/` 证据）；最终合并脚本只汇总了 F1–F8。

---

## 5. Chess 兼容、删除回归与生命周期（任务书 §11.4/§11.5）

> 证据：`build/functest-20261030/baseline/compat/`（`summary.md`、`run-all.sh`、`results.tsv`、
> `08-reload-shutdown.sh`、goroutine dump）。

### 5.1 结果

**148 PASS / 12 FAIL**（含追加的 §11.4 剩余项与 reload-shutdown 组合验证）：

| 检查项 | 结果 | 关键实证 |
|---|---|---|
| pid 文件（路径/字段/0600/写入时机/port=0/退出删除/cwd） | ✅ 27/27 | pid 27.7ms 写入 < health 45.1ms；出现即监听；权限 0600；SIGTERM 1.3–4ms 退出并删除 |
| env（CHAGENT_* 生效；旧名不兼容；cwd） | ✅ 21/21 | CHAGENT_HOME/WORKSPACE/CONFIG 全生效；旧 PICOCLAW_*/DDMCCLAW_* 已显式不支持（重命名决策） |
| 配置不写回（v2 channels+单数 api_key；无 channel_list；最小配置） | ✅ 22/22 | 前后 sha256 完全一致；最小配置可启动/health/pico ping；磁盘不被改写 |
| 配置校验（7 类非法值） | ❌ 6/8 | `max_rounds=0`、`max_bytes=0` 静默补默认（其余 5 类正确拒绝） |
| 端口/重启/单例（SIGTERM≤5s、pid 删除、重启、单例拒绝、stale pid） | ✅ 20/20 | **未 reload 时**全部符合契约 |
| 删除回归（包引用/build/非 pico 配置/provider/CLI/残留） | ❌ 23/28 | Go 构建图干净；5 处非代码残留（见 §10 P2-5） |
| 追加：message.send/typing/reload 鉴权与生效 | ✅ 16/19 | 3 个 FAIL 与死锁同源 |
| 追加：**reload→SIGTERM 对照实验** | ❌ 12/14 | 见 §5.2 |

### 5.2 🔴 P0 缺陷：配置 reload 后 SIGTERM 永久挂死（shutdown 死锁）

- **现象**：启动 → 一次成功 `POST /reload`（或 `hot_reload=true` 时改配置自动重载）→ SIGTERM：
  进程 **>30s 不退出**；`/health` 已不可达（半停机）；`.chagent.pid` 不删除；再次启动被单例拒绝
  （`gateway is already running (PID: <挂死进程>)`）→ 只能 `kill -9`。
  **未 reload 的对照组 1.3ms 正常退出**，稳定复现 3/3。
- **独立复现（三路）**：本报告 harness P9.1/P9.2（8s 超时 + pid 残留）；config 能力测试 A11；
  baseline compat `08-reload-shutdown.sh`（>30s，含 SIGQUIT goroutine dump）。
- **根因（已定位）**：
  1. `Manager.StartAll`（`pkg/channels/manager.go:1159-1160`）创建 `dispatchCtx` 存入 `m.dispatchTask`，
     dispatcher goroutine 在 `manager.go:1237-1252` 启动并 `dispatchWG.Add(3)`；
  2. reload 路径 `gateway.stopAndCleanupServices(isReload=true)` **跳过 StopAll**（`pkg/gateway/gateway.go:478-481`），
     dispatcher 继续使用启动时 ctx；随后 `Manager.Reload` 在 `manager.go:1886-1887` **用新 cancel 覆盖
     `m.dispatchTask`**，但不重启 dispatcher、不做 WG 记账（pico 未变时甚至不新建 worker）；
  3. SIGTERM → `Manager.StopAll`（`manager.go:1314-1339`）持 `m.mu` 写锁，`cancel()` 取消的是**孤儿 ctx**，
     旧 dispatcher 不会退出 → `m.dispatchWG.Wait()`（`manager.go:1339`）永久阻塞。
  - goroutine dump：`goroutine 1 [sync.WaitGroup.Wait] → channels.(*Manager).StopAll manager.go:1339 →
    shutdownGateway gateway.go:508`，3 个 dispatcher 仍存活。
- **影响**：chess 客户端若走 `/reload` 或开启 `hot_reload`，刷新配置后无法正常 stop/restart，且 pid 残留阻止下次启动。
- **违反**：任务书 §0.1 第 7 条、§11.4「SIGTERM 5s 内退出、pid 删除」、T7.5。
- **修复方向（仅建议，本轮未改代码）**：
  - 方案 A（最小）：`Manager.Reload` 不覆盖 `m.dispatchTask`（dispatcher 生命周期归 StartAll/StopAll），
    added-channel worker 复用传入 ctx；
  - 方案 B（彻底）：Manager 增加常驻 `dispatchCtx`，StartAll 创建、StopAll cancel、Reload 复用并按需重建 dispatcher
    并正确 `Add(3)`。
  - 回归用例：`reload 成功 → SIGTERM → ≤5s 退出且 pid 删除`（现有 `chess-e2e.sh` 未覆盖该组合）。
- **证据**：`compat/08-reload-shutdown.sh`、`compat/work/08-reload-shutdown/reload-hang-goroutines.txt`、
  `live/` P9.1/P9.2、`config/phase-A/*/gateway.log`。

---

## 6. 上下文窗口管理专项（用户追加要求）

> 脚本：`scripts/agent-context-window-test.py`；证据：`build/functest-20261030/context/`。

**8 个阶段 A–H 全部真实执行**（真实 gateway + pico WS；A 用 `deepseek-v4-pro`，其余用 `deepseek-v4-flash`；
窗口 12000 / max_tokens 1024 / summarize 阈值等按阶段配置）。最终定性：**被测实现 0 真实缺陷；
results.json 中 6 个原始 FAIL 均为脚本自身断言/时序问题，已在 summary.md 定性并修正。**

| 阶段 | 结果 | 关键真实证据 |
|---|---|---|
| A 强制压缩 / 工具结果裁剪 + spill | ✅ PASS（25） | 真实 `read_file` 输出 150,193 chars（>8192 阈值）→ `Forced compression pruned tool results without dropping turns pruned_msgs=1 saved_chars=144807 spilled_msgs=1`；spill 文件 150,193B 与原始输出完全一致，目录 0700/文件 0600，marker 内嵌绝对路径；继续加压触发 `dropped_msgs=10` 的 drop-turns 路径，首条 active 消息为真实 user（context 不是 turn 边界），tool 配对无孤儿 |
| B 异步摘要 | ✅ PASS（14） | `Memory threshold reached` + `agent.session.summarize summarized_messages=3 kept_messages=4 summary_len=106`；`meta.summary` 非空且**不含** `SKIP_CONTEXT_SENTINEL`/`WORKSPACE_SENTINEL`/`<system-reminder>`；`meta.count==JSONL 行数` |
| V durable context 重注入 | ✅ PASS（4） | workspace/runtime/skill-catalog 各恰好 1 份，跨快照无累积 |
| C turn 边界 + context replace | ✅ PASS（10） | 压缩后 active 首消息为真实 user；`<task_round>` 以 user 轮次起点持久化（kind 限制见 §10） |
| D SIGTERM 重启恢复 | ✅ PASS（11） | rc=0/pid 删除；重启 27==27、种子仍在、正常回复；workspace/runtime/skill 仍各 1（无重复注入）；task_state 保留；spill 4→4 未改写 |
| E 临近上限提醒 | ✅ PASS（修正后） | 真实 active JSONL：29 条消息中 `context-pressure` 3 条，8 个 user turn 中分布在 3 个 turn，每 turn ≤1，band 0.85/0.95 均出现，全文中文；原 FAIL 是脚本把快照累加重复计数 |
| F `/context` + usage 锚定 | ✅ PASS（修正后） | 公式吻合：`compressAt=window-maxTokens=10976`、`summarizeAt=window*100%`；重启前后 25→27 条、usage 10178→10206；原 FAIL 是自主轮异步总结抢答被当命令回复 |
| G pruner 幂等 | ✅ PASS（7） | 重复压缩 `pruned_msgs=0 saved_chars=0`（no-op）；spill 文件名含内容 sha256[:16] 且未重写 |
| H skill catalog durable context | ✅ PASS（5） | `catalog_mode=context` 时 JSONL 出现 `skill-catalog`，压缩后仍可见 |

**与 D1 相关的运行时数字**（用于排除「估算歪一个数量级」）：最重请求 provider `prompt_tokens=70607`
vs 通知估算 66287（0.94 倍）；普通 turn 0.81–1.63 倍，同量级。但**这些数字不能证明锚点被使用**；
D1 的「主回合永不记录锚点」已由代码级探针在 HEAD runtime 复现（见 §7）。

---

## 7. 新机制代码级核对（12 项）

> 证据：`build/functest-20261030/mechanism/mechanism-audit.md`；定向测试 127 PASS/0 FAIL；
> `go test ./pkg/agent/... ./pkg/config/...` PASS；未修改生产代码。

| # | 机制 | 判定 | 关键结论 |
|---|---|---|---|
| 1 | pre-step 有序注入表 + next-step 队列 | ✅ 真实接线 | 顺序 100/200/300/350/400/500；首轮由 `pipeline_setup.go:32-43` 等价注入 |
| 2 | supersede | ⚠️ 部分 | 唯一调用点 `pipeline_setup.go:42`（回合起点）；回合内每步不收敛（D4） |
| 3 | turn 边界 / 摘要跳过 context / 压缩先裁剪 | ✅ 真实接线 | `isTurnBoundary`、`forceCompression` 先 `PruneToolResultsWithSpill` |
| 4 | pruner（8192/4096/1024）+ spill | ✅ 真实接线 | 仅在压缩期运行（符合设计）；30 天清理懒执行（D10） |
| 5 | context pressure + usage 锚定 + 临近提醒 | ❌ 声称不符（D1）+ ⚠️ | **usage 锚定主回合死路径**（见下）；策略库未切触发；提醒仅 iteration>1（D5） |
| 6 | loop guard（3/5/8） | ✅ 真实接线 | 三处 observe 接线；无集成测试（D3） |
| 7 | 策略 prompt P-01/P-02/P-11 | ✅ 真实接线 | 关闭时 system prompt 不变；slot 940 |
| 8 | skill catalog durable context | ✅ 真实接线 | 默认 context，system 回退；system prompt 不再含技能表 |
| 9 | ContextSource 注册 API | ✅ 真实接线（API） | 无插件/hook 注册入口（D6） |
| 10 | 并行工具批调度 | ✅ 真实接线 | 真实场景日志出现 `Tool call (batch)`×7；capability 名实不一致（D7） |
| 11 | autonomy 闭环 | ✅ 真实接线 | 真实场景 2 轮自主续跑 + complete + 事件；checkpoint/守卫/最终总结均已接线 |
| 12 | context-pressure prestep order 350 | ✅ 真实接线 | 真实场景 `context-pressure` 出现；位置正确 |

### 7.1 P1 缺陷 D1：usage 锚定计量在主回合从不生效（已 runtime 复现）

- 声称（migration report 第二轮 §7）：usage 锚定上下文计量「已接入」。
- 实际：`turn_coord.go:23` 把 turnState 放入 `turnCtx`，但 `turn_coord.go:209` 调用
  `pipeline.CallLLM(ctx, turnCtx, …)` 的**第 1 个实参仍是原始 ctx**；`pipeline_llm.go:499-506`
  从第 1 参 ctx 取 turnState → 主回合恒为 nil；全仓库只有 SubTurn 路径（`subturn.go:430`）把
  turnState 放进第 1 参。测试仅直接调用 `ts.recordUsageAnchor`，无集成覆盖。
- **复现**：teammate 用 `go test -overlay` 虚拟注入探针（不改源码树）实测
  `主路径 usageAnchor=<nil>；SubTurn usageAnchor=&{…}`，命令与日志见
  `mechanism/overlay.json`、`mechanism/test-usage-anchor-probe.log`。
- 影响：`correctionRatio` 在主回合恒为 1，临近上限通知只能依赖启发式估算（锚定校准形同虚设）；
  `SetLastUsage/SetLastFinishReason` 同样死路径。
- 建议：`turn_coord.go:209` 首参传 `turnCtx`（并确认设置 `SetLastUsage` 的调用方同样修正），补一个经 CallLLM 的集成测试。

### 7.2 其它 P1/P2 缺陷

- **D2（P1）**：pre-step 有序表无回合级 E2E，且 `prestep.go` 无日志 → 生产不可观测；现有集成测试 fake provider
  都是单次调用，未进入第 2 次迭代。
- **D5（P1）**：context-pressure 通知只在 iteration>1 注入，单次 LLM 调用回合永不触发（真实专项中已观察到
  `context-pressure` 仅在工具迭代回合出现）。
- **D4（P2）**：supersede 只回合起点生效；同回合内多次 `task_update` 会让模型同时看到新旧 task-state。
- **D7（P2）**：批调度准入用能力接口 `IsToolParallelSafe`（`pipeline_execute.go:972`），实际分组用名称级
  `IsParallelSafe`（`tool_batch.go:91/99`）→ capability 名实不一致。
- D3/D6/D8/D9/D10/D11/D12/D13/D14：见 mechanism-audit.md 缺陷总表。

---

## 8. 上下文/工作区语义补充发现

- **AGENTS.md 被 agent definition 消费并从 workspace-instructions 中排除**：只有 `AGENTS.md` 时，
  `/context` 显示「工作区指令：0 个文件」，但模型能看到其内容（legacy agent definition 路径）。
  与配置指南 §3.2「扫描 AGENTS.md/CLAUDE.md 作为持久上下文」的表述有偏差；本轮用 `CLAUDE.md` 验证了
  workspace-context 基线/delta。建议文档明确二者关系（P2 文档偏差）。
- `/context` 在重启后、下一轮前可能显示工作区指令 0 文件（baseline 尚未重建），下一轮恢复 1；属重建时机语义，
  非重复注入。
- 普通自主轮次的 `<task_round>` 提示以普通 user 消息持久化（`ContextKindTaskRound` 只在 model 空回复+
  任务 active 时由 `appendTaskContinuation` 注入），因此「kind=task-round」的持久上下文只能部分覆盖。

---

## 9. 独立产物复核（不信任 harness 断言）

- 脚本：`scripts/verify-agent-e2e-artifacts.py`（stdlib-only，独立实现，不 import harness 逻辑；支持 `--self-test`、
  `--known-failure`、数值/权限/哈希/配对/上下文 kind/日志字段级断言）。
- 自测：基线 PASS；正例 `planned_reload`、`loop_guard_compacted` PASS；**14/14** 注入缺陷变异全部被检出
  （含「harness 宣称 PASS 但原始证据不成立」的矛盾检测）。
- **真实 artifacts 复核（修复后最终）：独立结论 PASS**。`PASS 85 / FAIL 0 / WARN 4 / SKIP 2`；
  **required 失败 0、Lead 声明与原始证据矛盾 0**；P9.1/P9.2 已知缺陷确认消除（首次 SIGTERM 日志含
  `✓ Gateway stopped` 与 `remove pid file`）。修复前报告（83 PASS / 2 known-failure）保留用于对比。
- 重点交叉核查全部 PASS：loop-guard 运行时快照（`evidence/context-p6.json`）与 P6 日志 4 次真实 `list_dir`；
  P7 压缩字段 `pruned_msgs=1/spilled_msgs=1/saved_chars=34673` 与 spill 文件 65,690 字符、0700/0600、内容寻址一致；
  P4 自主轮 `AUTONOMY_NOTES.md` 含外部信号 `AUTONOMY_WAIT_FLAG_CONTENT_42` + `AUTONOMY_STEP2_MARKER`；
  P3 独立复跑 3 个 unittest 全过；config 哈希链与 artifacts 内 config.json 绑定，计划内差异仅
  `context_window/max_tokens`；tool_call↔tool_result 全配对；workspace/runtime/skill-catalog/task-state 均存在。
- 修复后 4 条 advisory（不计入矛盾）：summary 重建后 context 顺序变化；task-state 同回合去重键（D4 在 JSONL 保留审计副本的已知语义）；
  docs_claude 被压缩后不可见；artifacts 含真实明文 config（分享前脱敏）。
- 产物：修复前 `verify-artifacts.log/json`；修复后 `verify-artifacts-postfix.log/json`。

---

## 10. 缺陷清单汇总（按严重度）

| ID | 严重度 | 缺陷 | 证据/位置 | 状态 |
|---|---|---|---|---|
| **P0-1** | **P0** | 成功 `/reload` 后 SIGTERM 永久挂死（shutdown 死锁），pid 残留，无法优雅重启 | `manager.go:1159-1160/1886-1887/1314-1339`、`gateway.go:478-481`；compat `08-reload-shutdown.sh` + goroutine dump；live P9.1/P9.2；config A11 | **未修复（验证范围只报告）** |
| P1-1 | P1 | usage 锚定计量主回合死路径（声称已接入） | `turn_coord.go:209`、`pipeline_llm.go:499-506`；overlay 探针复现 | ✅ 已修复+回归测试（§14） |
| P1-2 | P1 | pre-step 有序表无回合级 E2E、无日志、生产不可观测 | `prestep.go`、`turn_coord.go:194` | ✅ 已修复+回合级测试（§14） |
| P1-3 | P1 | context-pressure 通知默认不可触发（仅 iteration>1） | `prestep.go:250-268` | ✅ 已修复+测试（§14） |
| P1-4 | P1 | 仅配置 `tools.mcp.servers` 不会自动启用 MCP（`manager.go:204` 用 `Enabled` 而非 `Active()`），与工具参考文档相反 | `pkg/mcp/manager.go:204`、`MCPConfig.Active()`；config D0 证据 | ✅ 已修复+实测（§14） |
| P2-1 | P2 | `autonomy.max_rounds=0`/`no_progress_rounds=0`/`workspace_context.max_bytes=0` 被静默补默认（应可诊断拒绝） | `config.LoadConfig` 先 ApplyDefaults 后 Validate；compat 04 | ✅ 已修复+测试（§14） |
| P2-2 | P2 | supersede 只回合起点生效 | `pipeline_setup.go:42` | ✅ 已修复+测试（§14） |
| P2-3 | P2 | 批调度 capability 名实不一致 | `pipeline_execute.go:972` vs `tool_batch.go:91/99` | ✅ 已修复+测试（§14） |
| P2-4 | P2 | 文档漂移：迁移报告 §3.1 pre-step 4 项/loop guard 400（实际 6 项/500）、§3.3「不写 spill」过期、执行报告 §9 指标与 §13 token 说明过期、§5 audio/evolution 状态过期；AGENTS.md 语义偏差；缺 `docs/chess-integration.md` | 各文档 | ✅ 已修正（§14，含新增 chess-integration.md） |
| P2-5 | P2 | 删除残留：`config/config.example.json` 仍 v3 schema（启动失败）、`.github/dependabot.yml /web/frontend`、`.goreleaser.yaml ./web/backend`、architecture 文档 4 行 web 引用、缺 `docs/chess-integration.md` | compat logs | ✅ 已修复；CI workflows 收尾见 §14 |
| P2-6 | P2 | `HEAD 5da70e0b` 升级 go.mod go 指令 1.25.11→1.27.3，违反 §0.7；CI `go-version-file` 受影响 | `git show HEAD` | ✅ 已回退 1.25.11；tidy -diff 转 PASS（§14） |
| P2-7 | P2 | 覆盖率未达 §11.1 阈值（四包 74.8%，gateway 17.7%） | `baseline/coverage-4pkgs.log` | ⚠️ 部分：新增回归后 74.9%，gateway 仍 17.7%（残余缺口） |
| P2-8 | P2 | `go mod tidy -diff` FAIL（require 分组不 tidy，既有） | `baseline/mod-tidy-diff.log` | ✅ 已修复（go 指令回退后 tidy -diff 为空） |
| P2-9 | P2 | Hook E3 deny 场景因模型拒执行命令而未触发（非产品缺陷，脚本/模型行为） | `config/phase-E/` | 记录 |
| P2-10 | P2 | pico 显式 `channels.pico.token` 被 `md5(--name)` 覆盖（README 正确，执行报告 §13 过期） | `pkg/config/agent_name.go` | 文档需更正 |
| — | — | provider 429 限流（环境因素） | 自主轮 checkpoint 正确处理；不影响结论 | 已缓解 |

---

## 11. 任务书 §11 验证矩阵对照

| 任务书项 | 结果 | 说明 |
|---|---|---|
| §11.1 基础命令 | 构建/测试/race/vet/lint/chess-e2e ✅；tidy-diff ❌；覆盖率 ⚠️ | 见 §3 |
| §11.2 单元测试矩阵 | ✅ | `go test ./pkg/...` 0 FAIL；机制定向 127 PASS；覆盖 context 元数据/配置/发现/渲染/注入/taskstate/自主/skill//context |
| §11.3 集成/E2E 矩阵 | ✅（除 P0） | 启动/关闭（未 reload）/重启/配置刷新/最小配置/工作区 baseline/更新/压缩重启/自主闭环/用户停止/裁剪回归 → §2、§5、§6 |
| §11.4 Chess 兼容专项 | ⚠️ 148/12 | 未 reload 时全通；**reload 后 SIGTERM 挂死**（P0）；配置校验 6/8 |
| §11.5 删除回归矩阵 | ⚠️ 23/28 | 代码构建图干净；5 处非代码残留 |
| §11.6 性能/体积 | ✅ | 20.6MB / 75ms / 18.8MiB（较旧报告明显改善） |
| §11.7 文档与最终报告 | ⚠️ | lint-docs 通过；`docs/chess-integration.md` 缺失；执行报告多处过期 |

---

## 12. 复现方式

```bash
cd /Users/wudaorui/code/chagent

# 1) 真实场景 E2E（真实模型；约 3-4 分钟）
python3 scripts/agent-e2e-scenario.py
# 产物：build/functest-20261030/live/{results.json,summary.md,workspace/,*.log}

# 2) 配置能力矩阵（真实模型；A-F）
python3 scripts/agent-config-capabilities-test.py --phases A,B,C,D,E,F
python3 scripts/agent-config-capabilities-test.py --phases A   # 单组重跑

# 3) 上下文窗口专项（A-H）
python3 scripts/agent-context-window-test.py --phases ABCDEFGH --model deepseek-v4-flash

# 4) 无模型基线（任务书 §11.1）
go build ./cmd/chagent && go build ./...
go test ./pkg/... -count=1
go test -race ./pkg/agent/... ./pkg/gateway/... ./pkg/channels/pico/... ./pkg/config/... -count=1
go vet ./... && ./scripts/lint-docs.sh && ./scripts/chess-e2e.sh
go mod tidy -diff
./scripts/measure-startup.sh

# 5) Chess 兼容/删除回归 + P0 复现
cd build/functest-20261030/baseline/compat && ./run-all.sh && ./08-reload-shutdown.sh

# 6) 独立产物复核
python3 scripts/verify-agent-e2e-artifacts.py --artifacts build/functest-20261030/live \
  --json-out build/functest-20261030/verify/verify-artifacts.json

# 7) 机制代码级核对
cd build/functest-20261030/mechanism  # 见 mechanism-audit.md 与 test-*.log
```

复现注意：`scripts/agent-e2e-scenario.py` 默认从
`/Users/wudaorui/chessdata/weichat/weichat/config.json` 读取 `model_list`（可用 `--config`/`--model` 覆盖）；
真实模型会产生 token 费用；产物目录 `build/` 已 gitignore，但包含真实明文配置，分享前须脱敏。

---

## 13. 限制与未覆盖项

1. **真实模型不确定性**：断言均基于会话 JSONL/文件/日志/task_state 等原始证据，但模型行为存在波动；
   个别检查（如第 2 轮是否再次调用 `read_file`）允许「模型从历史作答但压缩证据成立」的等价通过。
2. **未覆盖**（任务书已声明未迁移/非 chess 必需）：事件日志 surface replace、plan mode、turn-stopping 注入、
   jobs 交付历史、ContextSource 的插件/hook 注册入口、spill 定时清理（仅懒执行）。
   MCP/Hooks/路径权限已在 config 矩阵全部实测：Hooks 5/5、路径权限 4/4、MCP 4/5（仅 D0 为真实缺陷）。
3. **覆盖率**：`pkg/gateway` 17.7% 远低于阈值，属验收缺口；gateway 启动/信号/reload 路径只有进程级 E2E 覆盖，
   而 P0 缺陷恰好落在该盲区——建议补充 `reload→SIGTERM` 与 gateway 生命周期单测。
4. **本轮不修复生产代码**：所有缺陷仅报告；修复 P0-1 后需重跑 §2、§5 与 `08-reload-shutdown.sh` 回归。
5. **限流**：测试环境多个真实模型测试并行时出现 429；已通过重试与分散模型缓解，极端情况下个别断言可能超时。
6. **artifacts 含密钥**：见 §1.3 安全提示。

---

## 14. 修复记录与修复后复测（2026-10-30 17:31–18:30）

> 按「修复已发现的问题」要求，本轮对 §10 中的 P0/P1/P2 逐项修复（未 commit，属当前工作树），
> 每个生产修复都带回归测试；随后重跑真实场景、配置矩阵、上下文专项、基线/兼容与独立产物验证。

### 14.1 修复清单

| 缺陷 | 修复（file:line） | 回归测试 | 复测证据 |
|---|---|---|---|
| **P0-1** reload→SIGTERM shutdown 死锁 | `pkg/channels/manager.go`：新增常驻 `dispatchCtx`；`StartAll` 创建、`Reload` 复用（不再覆盖 `dispatchTask`）、`StopAll` 取消/清理 | `TestReloadThenStopAllDoesNotDeadlock`（StartAll→2×Reload→StopAll≤3s） | compat `08-reload-shutdown.sh`：控制 1.3ms、`/reload` 后 **8.1ms**、hot_reload 后 **8.3ms**，pid 删除，9/0；live P9.1 **0.01s** |
| P1-1 usage 锚点主回合死路径 | `pkg/agent/turn_coord.go:216`：`CallLLM(turnCtx, turnCtx, …)`，主回合 `turnStateFromContext` 命中 | `TestCallLLMMainTurnRecordsUsageAnchor`（LastUsage/FinishReason/usageAnchor=1234） | overlay 探针主路径 `usageAnchor={1234,466}`（修复前 nil） |
| P1-2 pre-step 无日志/无回合级 E2E | `pkg/agent/prestep.go:112`：注入时 Info 级结构化日志（source_id/order/injected/iteration） | `TestTurnPreStepInjectsOrderedContextSourcesAtSecondIteration`（100→200→300→350→400→500，去重） | agent 定向 135 PASS / 0 FAIL |
| P1-3 context-pressure 首调用不可触发 | `pkg/agent/pipeline_setup.go:147`：初始消息构建后评估一次 `injectContextPressureNotice`，复用 band 去重 | `TestTurnContextPressureNoticeOnFirstIteration`（单次调用有通知；迭代不重复） | 上下文专项 E 组 **7 PASS**、首迭代已能触发 |
| P1-4 MCP servers-only 不自动启用 | `pkg/mcp/manager.go:202`：`!mcpCfg.Enabled` → `!mcpCfg.Active()` | `TestLoadFromMCPConfig_ServersOnlyActivatesMCP` 等 4 项 | 配置矩阵 D0 修复后 PASS：`Connected: yes`、`/show mcp` 含工具、真实调用 `mcp_cfgcap_echo` 成功 |
| P2-1 显式 0 静默补默认 | `pkg/config/context_config.go` 新增 `ValidateExplicitContextZeros`；`pkg/config/config.go:1138` 在 ApplyDefaults **前**校验原始 JSON | `pkg/config/context_zeros_test.go`（8 子用例：0/负数拒绝、缺省/null 正常、round_delay_ms=0 合法） | compat 04：6/2 → **12/0**；配置矩阵 F7-F9 拒绝错误含字段+范围；F13/F14 正常 |
| P2-2 supersede 只回合起点 | `pkg/agent/turn_coord.go:198`：迭代路径 pre-step 后调用 `supersedeContextMessages` | `TestTurnPreStepSupersedesTaskStateWithinTurn`（迭代 2 task-state 恰 1 条且最新） | agent 测试 PASS；JSONL 仍保留审计副本 |
| P2-3 批调度 capability 名实不一致 | `pkg/agent/tool_batch.go:65-88` 新增 `isSafe`；`pipeline_execute.go:1072` 统一用 registry+`IsToolParallelSafe`（未知工具保守串行） | `TestRunToolBatchIsSafeOverridesNameAllowlist` 等 4 项（含 pipeline 端到端） | 批调度 12/12 PASS；live 日志仍有 `Tool call (batch)` |
| P2-4/P2-5 文档漂移/删除残留 | 重写 `config/config.example.json`（v2）；清理 `.goreleaser.yaml`、`.github/dependabot.yml`、architecture 文档 web 引用；新增 `docs/chess-integration.md`；迁移报告 pre-step/spill、执行报告指标/token/audio 状态、AGENTS.md 语义、CI workflows（删除 create_dmg/docker-build，清理 release/nightly 的 pnpm/docker/QEMU） | `compat 06`：23/5 → **32/0**；YAML 解析 + `git grep` 无 web/pnpm/launcher；docs lint OK | 文档/CI 改动见工作树；task-12 报告 |
| P2-6 go.mod Go 版本升级 | `go.mod` `go 1.27.3 → 1.25.11` | `go mod tidy -diff` 由 FAIL → **exit 0 空输出**；`go build ./...`、`go test ./pkg/...` PASS | `baseline/fix-verify/rv-tidy-head-repro.log` 证明 tidy FAIL 由该指令触发 |
| P2-7 覆盖率 | 新增回归测试（agent/config/channels 等）+ 本报告测试脚本；**仍未达 §11.1 90%** | `go test -coverprofile` 四包：74.8% → **74.9%**（agent 79.1、config 69.7、pico 62.1、gateway 17.7） | 见 §14.4 残余缺口 |
| P2-9 Hook E3 场景 | 测试 prompt 改为中性标记（非产品缺陷） | 配置矩阵 E 5/5 PASS | `config/phase-E` |
| P2-10 pico token 文档 | 执行报告 §13 更正为 md5(--name) 32 hex、显式 token 被覆盖 | compat 03 通过 | 文档 diff |

补充修复（修复后复核发现）：
- R1 pre-step 日志默认不可见 → 改为 Info 级（`prestep.go:112`），默认配置可观测；
- R5 MCP 显式 `enabled=false` 语义 → `pkg/config/config.go` 新增 `MCPConfig.ExplicitDisabled json:"-"`，`config_loader.go` 的 `rawMCPExplicitlyDisabled` 从原始 JSON 检测显式 false；`Active()` 优先关闭，同时保持「省略 enabled + servers 自动启用」。测试：`pkg/config/mcp_active_test.go`（8 行语义矩阵 + round-trip）+ `pkg/mcp/manager_test.go`（显式关闭 0 次 connect 端到端）；`go test ./pkg/config/... ./pkg/mcp/...` PASS（见 §14.5）。

### 14.2 修复后复测矩阵

| 验证 | 修复前 | 修复后 | 结论 |
|---|---|---|---|
| 真实场景 E2E（live，最终代码） | 84 PASS / 2 FAIL | **86 PASS / 0 FAIL / 0 SKIP** | P9.1 SIGTERM ≤5s、pid 删除；gateway 重构后重跑仍全通过 |
| 配置能力（A-F） | 38 PASS / 2 FAIL / 40 | **43 PASS / 0 FAIL / 43**（含 R5 新增 D0b） | A11/D0 修复，F7-F9 改为拒绝语义并 PASS；D0b 显式 enabled=false 关闭 |
| 上下文窗口（A-H） | 实现 0 缺陷（6 脚本误判） | 见 §14.5（F 组锚点断言语义修正后复跑） | 压缩/裁剪/spill/summary/边界/重启/幂等 |
| §11.1 基线矩阵 | tidy -diff FAIL；其余 PASS | **全绿（含 tidy -diff exit 0）** | `baseline/fix-verify/results.tsv` |
| 覆盖率 | 74.8%（gateway 17.7%） | **76.8%**（agent 79.2、gateway **76.8**、config 70.0、workspacecontext 83.0、pico 62.7） | ⚠️ gateway 已大幅提升，但四包仍未达 90% 阈值（§14.6） |
| Chess 兼容/删除回归 | 148 PASS / 12 FAIL | **163 PASS / 0 FAIL** | 04/06/07/08 全部转 PASS；P0 修复实测 8ms 退出 |
| 独立产物验证（最终 artifacts） | PASS（83/2 known-failure） | **PASS（85 项 / 0 FAIL / 5 advisory）** | required 失败 0、矛盾 0、known-failure 为空；P9.1/P9.2 已消除 |
| 性能 | 20,596,658B / 75ms / 19,296KB | **20,630,594B（+0.16%）/ 中位 76ms / 中位 19,376KB** | 无回归 |

### 14.3 修复的关键运行时证据

- **P0**：`08-reload-shutdown.sh` 三场景（无 reload 对照 1.3ms、`/reload` 8.1ms、hot_reload 8.3ms）全部
  ≤5s 且 pid 删除；live P9.1 0.01s；独立验证 `log.shutdown_first_run`/`log.pid_cleanup_first_run` 均 PASS。
- **D1 锚点**：overlay 探针主回合 `usageAnchor={1234,466}/lastUsage=true`（修复前 `nil`）；上下文专项
  F 组 `usage_anchor_same_order` 记录 provider/estimate 同量级；锚点按设计只影响「响应之后的请求」。
- **D5/E**：修复后 context-pressure 在首次 LLM 调用即可出现（E 组 7 PASS），且同 band 每 turn ≤1。
- **D0**：配置矩阵日志无 `MCP integration is disabled`，`/list mcp` Connected: yes，真实 `mcp_cfgcap_echo` 调用返回标记。
- **显式 0**：`max_rounds=0` 启动失败并报
  `agents.defaults.autonomy.max_rounds must be between 1 and 200, got 0`；缺省配置正常。

### 14.4 残余缺口与风险

1. **覆盖率仍未达 §11.1「关键路径 ≥90%」**：四包合计 **76.8%**；`gateway` 已通过可测试化重构 +
   进程内生命周期测试从 **17.7% → 76.8%**（详见 §14.6），但 agent 79.2%、config 70.0%、pico 62.7%
   仍未达 90%。剩余缺口主要在错误分支、平台/PTY 相关路径与 pico 长尾；继续达标需要按包补错误注入测试。
2. **全部修复未 commit**：工作树交付前需提交；提交后建议重跑 `baseline/fix-verify/` 与 `compat/run-all.sh`
   作为最终门禁（复测期工作树被并发编辑，快照见 `fix-verify/git-state*.log`）。
3. **MCP 显式关闭语义**（R5）：已按「显式 false 关闭 / 省略时 servers 自动启用」补检测（§14.5）；若后续
   用程序内构造 Config（不经 LoadConfig），仍以 `Enabled` 字段为准。
4. **D2 日志级别**：已从 Debug 升为 Info（默认可见）；若担心高频，可在 release 配置中调回 debug。
5. **低风险**：D4 迭代内 supersede 只影响模型可见请求（JSONL 保留审计副本）；D5 首迭代 tool defs 未按
   turn-profile 过滤（估算可能略偏早）；批调度对未注册工具一律串行（保守）；MCP 单 server `Enabled=false` 语义不变。
6. **可接受残留**：`.gitignore` telegram/web 忽略项、`.golangci.yaml` discordgo 条目、go.mod 未使用 discordgo
   replace、`docs/design` 历史计划文档中的旧表述（按审计历史保留）；`integration/README.md` 的已删 CLI 示例已更新。
7. **pico 覆盖率波动**（62.8→62.1）：该包源码未改，判断为异步用例波动，建议观察。
8. **异步 summarize 与 durable context 的短暂竞态**（低风险）：本回合进行中的异步 summarize 可能重写
   会话 JSONL 并丢掉刚注入的 context 消息；下一回合 SetupTurn/ContextSource 会自愈重注入（live 场景 P5.3
   在竞态下通过探针回合验证了自愈）。不影响模型当前请求，但「按 JSONL 事后断言 context 必须存在」需考虑该竞态。

### 14.5 修复后专项复跑记录

**上下文窗口 A-H（修复后）：9 个相位全部 PASS，94 条断言 0 FAIL，真实缺陷 0。**
- **D1 锚点运行时改善**：`iteration≥2` 的 context-pressure 通知 provider/notice 中位偏差
  **0.40（修复前）→ 0.158（修复后）**；首次请求无锚点属预期（锚点在响应后才记录），该断言语义已修正。
- **D5 首迭代触发**：E 组 7/7 PASS，4 个真实 user turn 中 4 条 notice，其中 2 条在首次 assistant 回复之前注入
  （band 0.85/0.95 各 ≤1、每 turn ≤2、无连续刷屏）。
- **D（重启）11/11**：SIGTERM rc=0/pid 删除；重启后 33==33、无重复注入、task_state/spill 保持一致。
- 压缩/spill 权限与内容、summary 一致性、turn 边界、pruner 幂等、skill-catalog 全部回归 PASS。
  证据：`build/functest-20261030/context/{results.json,summary.md}`（修复前基线在 `baseline-before-fix/`）。

**MCP R5 补充验证（配置矩阵 D 组，5/5 PASS，16.2s）**：
- D0：省略 `tools.mcp.enabled` + servers → 自动启用（`/list mcp` Connected: yes、`/show mcp` 含 echo）；
- **D0b（新增）**：显式 `enabled=false` + servers → disabled（`/list mcp` 显示 integration disabled、Connected: no、
  `/show mcp` 返回 MCP integration is disabled、reload 后无重连日志、fixture 无新 RPC）；
- D1 显式 true 恢复连接；D2 真实调用 `mcp_cfgcap_echo` 返回标记；D3 禁用 server+/reload 后不可用。
  语义矩阵与 round-trip 测试见 `pkg/config/mcp_active_test.go`、`pkg/mcp/manager_test.go`；配置矩阵最终 43/43 PASS。
  注：live E2E 场景不含 MCP 阶段，R5 的活体验证由配置矩阵 D0b 承担（独立产物验证器亦提示该范围边界）。

**最终独立产物验证**：`build/functest-20261030/verify/verify-artifacts-final2.log/json` —— PASS
（85 PASS / 0 FAIL / 5 advisory），required 失败 0、矛盾 0、known-failure 空；context kinds 证据规则已升级为
「最终 JSONL ∪ `evidence/context-kinds-observed.json` 运行期并集」；5 条 advisory（summary 重建后 context 顺序、
D4 JSONL 审计副本、docs_claude 压缩后不可见、artifacts 明文配置、P9.8 advisory），均不计入矛盾。
注：live 场景不含 MCP 阶段，R5「MCP 显式关闭」由配置矩阵 D0b 独立实测覆盖，两者互补。

### 14.6 覆盖率补强记录（2026-10-31，第三轮收尾）

**gateway 可测试化重构（行为不变）**：`pkg/gateway/gateway.go` 将 `Run` 拆为
`Run`（signal.Notify → 调用内部）与 `runGateway(..., sigChan <-chan os.Signal)`；测试可注入假信号通道，
生产启动顺序/pid 时机/日志/服务生命周期完全不变。

**新增进程内生命周期测试 `pkg/gateway/gateway_integration_test.go`**（真实监听 + 真实 pid/health/reload）：
- `TestGatewayLifecycleStartAndCleanShutdown`：启动成功 → health → SIGTERM ≤5s → pid 删除/端口不可达；
- `TestGatewayReloadThenCleanShutdown`：**P0 gateway 层回归**——成功 `/reload` 后 SIGTERM 仍 ≤5s 干净退出；
- `TestGatewayReloadInvalidConfigKeepsServing`：坏配置 reload 异步拒绝、旧服务保持健康；
- `TestGatewayHotReloadLifecycle`：`hot_reload=true` 修改 config → watcher 自动 reload → SIGTERM 干净退出；
- `TestGatewayAllowEmptyStartupLimitedMode`：无模型 `allowEmptyStartup` 受限启动路径。
  （单例在进程内无法安全测试：logger 文件日志不可重入；该场景由进程级 compat `05-singleton`/`08` 覆盖。）

**覆盖率结果**：

| 包 | 修复前 | 补强后 |
|---|---|---|
| `pkg/gateway` | 17.7% | **76.8%** |
| 四包合计 | 74.9% | **76.8%** |
| agent | 79.1% | 79.2% |
| config | 69.7% | 70.0% |
| channels/pico | 62.1% | 62.7% |
| workspacecontext | 83.0% | 83.0% |

**R8 收尾（loop-guard 接线测试）**：新增 `pkg/agent/loop_guard_wiring_test.go`
（`TestExecuteToolsHookRespondLoopGuardObservation`、`TestExecuteToolsBatchLoopGuardObservation`），
锁定 hook-respond 计数/deny 不计数、批调度 observe 计入守卫链；`go test ./pkg/agent -run LoopGuard -count=3` PASS。

**残余清理**：删除 `.dockerignore`（无 Dockerfile/docker 目录）；`.gitignore` 移除已删功能忽略项；
`.golangci.yaml` 移除 discordgo 条目；`go.mod` 删除未使用 discordgo replace 并重新生成 vendor；
文档 R10 过期表述更新（迁移报告 §0.3/§4/§7、配置指南显式 0 校验、执行报告 §20 链接）。
验证：`go mod tidy -diff` 空、`go build ./...`、`go vet ./...`、`gofmt`、`./scripts/lint-docs.sh` OK。

**最终门禁（Lead + 独立 baseline-verifier 复测，`baseline/final-gate/`）**：§11.1 全绿
（build、`go test ./pkg/...` 90.8s 0 FAIL、`-race` 四包 0 DATA RACE、vet、lint-docs、**tidy -diff 空**、chess-e2e 5/5）；
compat 全套 **163 PASS / 0 FAIL**（`/reload` 后 SIGTERM 18.5ms、hot_reload 后 8.7ms）；性能无回归
（20,630,594B / 中位 76ms / 中位 19,376KB）；真实场景 live 在 gateway 重构后重跑 **86 PASS / 0 FAIL**；
artifact 独立验证重跑（最终结论见 `build/functest-20261030/verify/verify-artifacts-final2.log`）。

## 附录 A：参与任务与产物索引

| 任务 | 负责 | 主要产物 |
|---|---|---|
| task-1 基线矩阵 | baseline-verifier | `baseline/summary.md`、`baseline/*.log`、`cover.out` |
| task-2 机制代码级核对 | mechanism-auditor | `mechanism/mechanism-audit.md`、`test-*.log`、`overlay.json` |
| task-3 独立产物验证器 | artifact-verifier | `scripts/verify-agent-e2e-artifacts.py`、`verify/` |
| task-4 真实场景 E2E（Lead） | lead | `scripts/agent-e2e-scenario.py`、`live/` |
| task-5 配置能力实测 | config-capability-tester | `scripts/agent-config-capabilities-test.py`、`scripts/fixtures/`、`config/` |
| task-6 上下文窗口专项 | context-window-tester | `scripts/agent-context-window-test.py`、`context/` |
| task-7 Chess 兼容/删除回归 | baseline-verifier | `baseline/compat/`、`08-reload-shutdown.sh`、goroutine dump |

## 附录 B：结论与建议（给决策者）

1. **优先修复 P0-1（reload→SIGTERM 死锁）**：这是 chess 真实部署链路（配置刷新/热重载）的硬阻断；
   修复后必须补 `reload → SIGTERM ≤5s + pid 删除` 回归测试（当前 `chess-e2e.sh` 未覆盖）。
2. **修复 P1-1（usage 锚定死路径）**：一行级修复（`turn_coord.go:209` 传 `turnCtx`）+ 集成测试；
   否则 DSH 迁移声明的「usage 锚定计量」名不副实。
3. **收敛 P1-2/P1-3**：pre-step 增加可观测日志与 iteration>1 的集成测试；明确 context-pressure 的触发语义。
4. **P2 清理**：配置校验顺序（0 值可诊断）、删除残留 5 项、文档漂移（含执行报告指标与 token 说明）、
   go.mod go 指令决策、覆盖率补测。
5. 本报告所有数据可复现；归档 artifacts 前请脱敏 `live/workspace/config.json`。

## 14.8 残余问题修复轮（2026-11-02）

在本报告 §14 修复基础上，继续关闭问题库 OPEN-01..OPEN-19（详见 `docs/issues/open.zh.md` 已解决表）：

- **覆盖率达标**（任务书 §11.1 关键路径 ≥90%）：agent 控制面 **91.0%**（整体 85.0%）、gateway **91.6%**、config **92.2%**、pico **93.6%**（3 次稳定）；四包合计 86.9%。新增 55+25+34+70 个确定性测试；口径与逐文件数据见 `docs/reference/coverage.zh.md`。
- **OPEN-04 竞态修复**：`pkg/agent/context_legacy.go` 写序列串行 + summary/前缀 CAS + 偏移平移，避免异步 summarize 丢 durable context；3 个修复前失败/修复后通过回归用例。
- **MCP live 阶段 + stdio 生命周期修复**：live E2E 新增 P11 MCP（servers-only 自动启用、真实模型调用、动态 tools/list_changed 增删、显式关闭 reload），**P11 24/0**；修复 stdio MCP server 被 reload/turn ctx 误杀的生命周期缺陷。全量 live 重跑 **101 PASS / 0 FAIL / 0 SKIP**。
- **残余缺陷修复**：meta 损坏自愈、SIGKILL exec 孤儿看门狗、只读日志目录降级、大参数错误诊断保留、负 usage/迭代钳制、Save 失败可观测、jobs stale 清理、MCP tools 动态重注册。故障注入 **35/0**、进程级健壮性 **112/0/1**。
- **最终门禁（提交后复跑）**：build/test/race/vet/lint/tidy/chess-e2e/measure/coverage 全部 exit=0；compat **163 PASS / 0 FAIL**；证据 `build/functest-20261030/baseline/final-gate/fg2-*`、`baseline/compat/logs/`、`live/final/`。
