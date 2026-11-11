# workspacecontext

工作区指令上下文（workspace instruction context）的发现、读取、渲染与对账库。

## 职责

- **发现**：从工作区向上寻找项目根（`.git` 等 marker），收集候选指令文件
  （`AGENTS.md`、`CLAUDE.md`、`*.local.md`），可选择扫描用户级目录。
- **有界读取**：单文件与整链字节预算、UTF-8 安全截断、symlink 策略、去重
  （同目录 `AGENTS.md` 优先于 `CLAUDE.md`）。
- **渲染**：baseline（首次完整）与 delta（additional / updated / removed /
  promotion），带稳定 digest 与转义，防止指令正文伪造 frame 结束标记。
- **对账**：resume/压缩后依据 session history 中已持久化的 context 消息重建，
  identity 变化时整表替换，不重复注入 baseline。

## 入口

```go
p := workspacecontext.NewProjector(cfg, workspace, excludePath)
res, err := p.Compose(ctx, history, touchedPaths)
// res.Message == nil 表示无变化；否则是待追加的 user-role context 消息
```

`Compose` 的失败语义：单文件不可用保留 last-good；文件消失产生 remove；
空文件是合法内容；发现阶段 marker 错误返回 error（上层跳过本次注入，不中断 turn）。

## 与 agent loop 的接线

- 首轮：`pipeline.SetupTurn` 注入 baseline（`WorkspaceContext` 非 nil 时）。
- turn 内：`prestep.go` 的 `context.workspace`（order 100）在工具触碰后注入 delta。
- 触碰来源：只有成功的 `read_file`/`write_file`/`edit_file`/`append_file` 才会
  记录路径（`turnState.recordContextTouch`）；`list_dir`/`exec` 不触发。
- 持久化：context 消息与普通消息一样写入 session JSONL，带上
  `Context.Kind=workspace-instructions` 元数据；provider wire 序列化不会包含该元数据。

## 配置

`agents.defaults.workspace_context`（见 `pkg/config/context_config.go`）：
`enabled`、`max_bytes`、`max_source_bytes`、`project_root_markers`、
`instruction_files`、`local_instruction_files`、`user_global`、`follow_symlinks`。

## 测试

`go test ./pkg/agent/workspacecontext/... -count=1`

- `golden_test.go`：baseline/delta 渲染 golden。
- `discovery_test.go`：项目根、候选、嵌套、越界、去重。
- `render_test.go`：预算、截断、转义、promote/remove。
- `state_test.go` / `compose_test.go`：identity、resume、错误与 last-good 语义。
- `testdata/`：共享 fixture（含大文件、escape 用例、用户级目录）。
