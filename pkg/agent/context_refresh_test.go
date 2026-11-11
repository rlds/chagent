package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/agent/workspacecontext"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/session"
)

// T3.5 增量刷新测试。它们通过 SetupTurn（基线）与 pre-step 表（触碰文件增量）
// 使用的 turnState 注入缝，驱动真实的发现/投影库（pkg/agent/workspacecontext）：
//
//   - injectWorkspaceContext      = 每轮的 baseline/delta 组装（无触碰）
//   - injectWorkspaceContextDelta = 由文件工具触碰驱动的 pre-step 组装
//
// 断言按当前行为编写；当设计措辞（promotion/预算重现）与今天的实现不一致时，
// 测试在注释中记录真实行为，而不是跳过。

const refreshSessionKey = "refresh-session"

type refreshHarness struct {
	t         *testing.T
	workspace string
	projector *workspacecontext.Projector
	sessions  session.SessionStore
	ts        *turnState
}

func newRefreshHarness(
	t *testing.T,
	projectorCfg workspacecontext.Config,
	files map[string]string,
) *refreshHarness {
	t.Helper()
	workspace := t.TempDir()
	for name, content := range files {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if !projectorCfg.Enabled {
		projectorCfg = workspacecontext.DefaultConfig()
	}
	projector, err := workspacecontext.NewProjector(projectorCfg, workspace, "")
	if err != nil {
		t.Fatalf("NewProjector() error = %v", err)
	}
	sessions := session.NewSessionManager("")
	agent := &AgentInstance{
		WorkspaceContext: projector,
		Sessions:         sessions,
		Workspace:        workspace,
	}
	return &refreshHarness{
		t:         t,
		workspace: workspace,
		projector: projector,
		sessions:  sessions,
		ts: &turnState{
			agent:      agent,
			workspace:  workspace,
			sessionKey: refreshSessionKey,
		},
	}
}

func refreshWriteFile(t *testing.T, workspace, name, content string) {
	t.Helper()
	path := filepath.Join(workspace, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// baseline 在空历史上执行 SetupTurn 风格的组装，
// 并要求恰好一条基线消息。
func (h *refreshHarness) baseline() []providers.Message {
	h.t.Helper()
	history := h.ts.injectWorkspaceContext(context.Background(), &AgentLoop{}, nil, nil)
	if len(history) != 1 {
		h.t.Fatalf("baseline injection produced %d messages, want 1: %#v", len(history), history)
	}
	msg := history[0]
	if msg.Context == nil || !msg.Context.Baseline ||
		msg.Context.Kind != protocoltypes.ContextKindWorkspaceInstructions {
		h.t.Fatalf("baseline metadata = %+v", msg.Context)
	}
	return history
}

// composeNoTouches 模拟每轮的 SetupTurn 组装（无文件触碰）。
func (h *refreshHarness) composeNoTouches(history []providers.Message) []providers.Message {
	h.t.Helper()
	return h.ts.injectWorkspaceContext(context.Background(), &AgentLoop{}, history, nil)
}

// composeTouched 模拟由已记录触碰驱动的 pre-step 增量组装。
func (h *refreshHarness) composeTouched(history []providers.Message) []providers.Message {
	h.t.Helper()
	return h.ts.injectWorkspaceContextDelta(context.Background(), &AgentLoop{}, history)
}

func (h *refreshHarness) touch(tool, path string, isError bool) {
	h.ts.recordContextTouch(tool, map[string]any{"path": path}, isError)
}

func refreshChanges(t *testing.T, msg providers.Message) []protocoltypes.ContextChange {
	t.Helper()
	if msg.Context == nil {
		t.Fatalf("context message has no metadata: %+v", msg)
	}
	return msg.Context.Changes
}

func refreshAssertContains(t *testing.T, text string, snippets ...string) {
	t.Helper()
	for _, snippet := range snippets {
		if !strings.Contains(text, snippet) {
			t.Fatalf("context message missing %q:\n%s", snippet, text)
		}
	}
}

// TestContextRefreshNestedAdditional 覆盖 T3.5 场景 1：packages/app 内成功 read_file 触碰后，
// 下一次 pre-step 组装会把嵌套 AGENTS.md 作为“新增指令”增量注入。
func TestContextRefreshNestedAdditional(t *testing.T) {
	h := newRefreshHarness(t, workspacecontext.Config{}, map[string]string{
		"AGENTS.md":              "# 根规则\n\n根目录规则：先读代码。\n",
		"packages/app/AGENTS.md": "# 应用规则\n\n应用内使用 Go 编写测试。\n",
	})
	history := h.baseline()
	if !strings.Contains(history[0].Content, "根目录规则") {
		t.Fatalf("baseline missing root instructions:\n%s", history[0].Content)
	}
	if strings.Contains(history[0].Content, "应用内使用 Go") {
		t.Fatalf("baseline must not pre-load nested instructions:\n%s", history[0].Content)
	}

	h.touch("read_file", "packages/app/main.go", false)
	history = h.composeTouched(history)
	if len(history) != 2 {
		t.Fatalf("after deep read history = %d messages, want baseline+delta: %#v", len(history), history)
	}
	delta := history[1]
	refreshAssertContains(t, delta.Content,
		"来自以下文件的新增指令：packages/app/AGENTS.md",
		"应用内使用 Go",
		"以下指令适用于 `packages/app` 下的工作",
	)
	changes := refreshChanges(t, delta)
	if len(changes) != 1 ||
		changes[0].Action != protocoltypes.ContextActionSet ||
		changes[0].Path != "packages/app/AGENTS.md" {
		t.Fatalf("delta changes = %#v, want one set for packages/app/AGENTS.md", changes)
	}
	if delta.Context.Baseline {
		t.Fatal("nested delta must not be marked as a baseline")
	}
}

// TestContextRefreshUpdated 覆盖 T3.5 场景 2（修改）：
// 变化的指令文件会以 replace 动作重新渲染为“指令已更新”增量。
func TestContextRefreshUpdated(t *testing.T) {
	h := newRefreshHarness(t, workspacecontext.Config{}, map[string]string{
		"AGENTS.md": "# 根规则\n\n旧规则：使用中文。\n",
	})
	history := h.baseline()

	refreshWriteFile(t, h.workspace, "AGENTS.md", "# 根规则\n\n新规则：始终使用中文回复，并保持简洁。\n")
	history = h.composeNoTouches(history)
	if len(history) != 2 {
		t.Fatalf("after modification history = %d messages, want baseline+update: %#v", len(history), history)
	}
	update := history[1]
	refreshAssertContains(t, update.Content,
		"来自以下文件的指令已更新：AGENTS.md",
		"新规则：始终使用中文回复，并保持简洁。",
	)
	if strings.Contains(update.Content, "旧规则：使用中文") {
		t.Fatalf("updated frame must replace, not repeat, the old content:\n%s", update.Content)
	}
	changes := refreshChanges(t, update)
	if len(changes) != 1 ||
		changes[0].Action != protocoltypes.ContextActionReplace ||
		changes[0].Path != "AGENTS.md" {
		t.Fatalf("update changes = %#v, want one replace for AGENTS.md", changes)
	}

	// 没有进一步变化：下一次组装必须是空操作。
	history = h.composeNoTouches(history)
	if len(history) != 2 {
		t.Fatalf("unchanged compose appended duplicates: %d messages", len(history))
	}
}

// TestContextRefreshRemoved 覆盖 T3.5 场景 2（删除）：删除可见指令文件只产生一次移除帧，
// 后续组装不再重复移除（移除去重）。
func TestContextRefreshRemoved(t *testing.T) {
	h := newRefreshHarness(t, workspacecontext.Config{}, map[string]string{
		"AGENTS.md": "# 根规则\n\n删除前的规则。\n",
	})
	history := h.baseline()

	if err := os.Remove(filepath.Join(h.workspace, "AGENTS.md")); err != nil {
		t.Fatalf("remove AGENTS.md: %v", err)
	}
	history = h.composeNoTouches(history)
	if len(history) != 2 {
		t.Fatalf("after deletion history = %d messages, want baseline+removal: %#v", len(history), history)
	}
	removal := history[1]
	refreshAssertContains(t, removal.Content,
		"以下文件的指令已移除：AGENTS.md",
		"此前从该文件加载的指令不再适用。",
	)
	changes := refreshChanges(t, removal)
	if len(changes) != 1 || changes[0].Action != protocoltypes.ContextActionRemove {
		t.Fatalf("removal changes = %#v, want one remove", changes)
	}

	history = h.composeNoTouches(history)
	if len(history) != 2 {
		t.Fatalf("removal was repeated on the next compose: %d messages: %#v", len(history), history)
	}
}

// TestContextRefreshIgnoresNonFileTools 覆盖 T3.5 场景 3：
// 只有成功的第一方文件工具才记录触碰。list_dir/exec 和失败的调用不得触发刷新。
func TestContextRefreshIgnoresNonFileTools(t *testing.T) {
	h := newRefreshHarness(t, workspacecontext.Config{}, map[string]string{
		"AGENTS.md":              "# 根规则\n\n根规则。\n",
		"packages/app/AGENTS.md": "# 应用规则\n\n应用规则。\n",
	})
	history := h.baseline()

	h.touch("list_dir", "packages/app", false)
	h.touch("exec", "packages/app/main.go", false)
	h.touch("read_file", "packages/app/main.go", true) // 失败的调用
	if touches := h.ts.consumeContextTouches(); len(touches) != 0 {
		t.Fatalf("non-file/failed tools were recorded as touches: %#v", touches)
	}
	history = h.composeTouched(history)
	if len(history) != 1 {
		t.Fatalf("non-file tools produced a context message: %#v", history)
	}

	// 成功的 read_file 记录绝对路径并去重。
	h.touch("read_file", "packages/app/main.go", false)
	h.touch("read_file", "packages/app/main.go", false)
	touches := h.ts.consumeContextTouches()
	if len(touches) != 1 {
		t.Fatalf("touches = %#v, want one deduplicated absolute path", touches)
	}
	if !filepath.IsAbs(touches[0]) || !strings.HasSuffix(touches[0], filepath.Join("packages", "app", "main.go")) {
		t.Fatalf("recorded touch is not an absolute workspace path: %q", touches[0])
	}
	if remaining := h.ts.consumeContextTouches(); len(remaining) != 0 {
		t.Fatalf("consumeContextTouches did not clear the queue: %#v", remaining)
	}
}

// TestContextRefreshNoDuplicateWhenUnchanged 覆盖 T3.5 场景 4+7：
// 无文件变化时重复组装不追加任何内容；触碰没有指令文件的目录同样是空操作。
func TestContextRefreshNoDuplicateWhenUnchanged(t *testing.T) {
	h := newRefreshHarness(t, workspacecontext.Config{}, map[string]string{
		"AGENTS.md": "# 根规则\n\n根规则。\n",
	})
	history := h.baseline()

	for i := 0; i < 3; i++ {
		history = h.composeNoTouches(history)
		if len(history) != 1 {
			t.Fatalf("compose %d without changes appended context: %d messages", i, len(history))
		}
	}

	refreshWriteFile(t, h.workspace, "packages/other/data.txt", "无指令文件的目录。")
	h.touch("read_file", "packages/other/data.txt", false)
	history = h.composeTouched(history)
	if len(history) != 1 {
		t.Fatalf("touch in a directory without instruction files produced context: %#v", history)
	}
}

// TestContextRefreshTouchesDoNotLeakAcrossTurns 覆盖轮次内触碰队列：
// 某一轮记录的触碰不能被下一轮的 pre-step 刷新消费。
func TestContextRefreshTouchesDoNotLeakAcrossTurns(t *testing.T) {
	h := newRefreshHarness(t, workspacecontext.Config{}, map[string]string{
		"AGENTS.md":              "# 根规则\n\n根规则。\n",
		"packages/app/AGENTS.md": "# 应用规则\n\n应用规则。\n",
	})
	history := h.baseline()

	// 第 1 轮记录了触碰，但在 pre-step 刷新运行前结束。
	h.touch("read_file", "packages/app/main.go", false)

	// 第 2 轮是共享 projector 和 sessions 的新 turnState。
	next := &turnState{
		agent:      h.ts.agent,
		workspace:  h.workspace,
		sessionKey: refreshSessionKey,
	}
	nextHistory := next.injectWorkspaceContextDelta(context.Background(), &AgentLoop{}, history)
	if len(nextHistory) != 1 {
		t.Fatalf("stale touch leaked into the next turn: %#v", nextHistory)
	}

	// 原来的轮次仍拥有它的触碰，可以自行刷新。
	history = h.composeTouched(history)
	if len(history) != 2 {
		t.Fatalf("original turn lost its touch: %d messages: %#v", len(history), history)
	}
	if !strings.Contains(history[1].Content, "packages/app/AGENTS.md") {
		t.Fatalf("delta does not reference the touched nested file:\n%s", history[1].Content)
	}
}

// TestContextRefreshDuplicatePromotion 覆盖 T3.5 的“重复 → 提升”去重规则在当前实现下的表现：
// 去重组按目录划分，因此同目录重复文件（CLAUDE.md 与 AGENTS.md 去空白后内容相同）
// 在胜出者可见时被跳过，并在移除胜出者的同一次组装中被提升。
func TestContextRefreshDuplicatePromotion(t *testing.T) {
	const shared = "# 共享规则\n\n同一份规则的重复内容。\n"
	h := newRefreshHarness(t, workspacecontext.Config{}, map[string]string{
		"AGENTS.md": shared,
		"CLAUDE.md": shared,
	})
	history := h.baseline()
	if !strings.Contains(history[0].Content, "来自以下文件的指令：AGENTS.md") {
		t.Fatalf("baseline does not render AGENTS.md as the dedupe winner:\n%s", history[0].Content)
	}
	if strings.Contains(history[0].Content, "来自以下文件的指令：CLAUDE.md") {
		t.Fatalf("baseline rendered the duplicate CLAUDE.md:\n%s", history[0].Content)
	}

	// 无变化：重复文件保持隐藏。
	history = h.composeNoTouches(history)
	if len(history) != 1 {
		t.Fatalf("unchanged compose promoted the duplicate: %#v", history)
	}

	// 删除胜出者会在同一次增量中提升同组重复文件。
	if err := os.Remove(filepath.Join(h.workspace, "AGENTS.md")); err != nil {
		t.Fatalf("remove AGENTS.md: %v", err)
	}
	history = h.composeNoTouches(history)
	if len(history) != 2 {
		t.Fatalf("after removing the dedupe winner history = %d messages: %#v", len(history), history)
	}
	delta := history[1]
	changes := refreshChanges(t, delta)
	var sawRemove, sawPromote bool
	for _, change := range changes {
		switch change.Action {
		case protocoltypes.ContextActionRemove:
			if change.Path == "AGENTS.md" {
				sawRemove = true
			}
		case protocoltypes.ContextActionSet, protocoltypes.ContextActionReplace:
			if change.Path == "CLAUDE.md" {
				sawPromote = true
			}
		}
	}
	if !sawRemove {
		t.Fatalf("delta changes = %#v, want a remove for AGENTS.md:\n%s", changes, delta.Content)
	}
	if !sawPromote {
		t.Fatalf("delta changes = %#v, want a set/replace for the promoted CLAUDE.md:\n%s", changes, delta.Content)
	}
	if !strings.Contains(delta.Content, "CLAUDE.md") {
		t.Fatalf("promoted frame does not mention the duplicate file:\n%s", delta.Content)
	}
}

// TestContextRefreshBudgetTruncationRecovers 记录 T3.5 场景 5 在当前实现下的表现：
// 超预算的嵌套指令文件不会被静默丢弃，而是带显式预算通知和截断正文渲染。
// 当文件缩小到预算内后，下一次触碰会把它重新渲染为不带通知的完整替换。
// （设计措辞是“省略 → 触碰后重现”；实现选择截断，因此测试锁定真实行为而不是跳过。）
func TestContextRefreshBudgetTruncationRecovers(t *testing.T) {
	projectorCfg := workspacecontext.DefaultConfig()
	projectorCfg.MaxBytes = 300 // 有意设得很小，以便截断嵌套文件
	h := newRefreshHarness(t, projectorCfg, map[string]string{
		"AGENTS.md":              "# 根规则\n\n根规则内容。\n",
		"packages/app/AGENTS.md": "# 应用规则\n\n" + strings.Repeat("很长的应用规则内容。", 200) + "\n",
	})
	history := h.baseline()

	h.touch("read_file", "packages/app/main.go", false)
	history = h.composeTouched(history)
	if len(history) != 2 {
		t.Fatalf("over-budget nested instructions produced %d messages: %#v", len(history), history)
	}
	truncated := history[1]
	refreshAssertContains(t, truncated.Content,
		"工作区指令预算 300 字节",
		"已截断 packages/app/AGENTS.md",
		"来自以下文件的新增指令：packages/app/AGENTS.md",
	)
	if len(truncated.Content) >= len(strings.Repeat("很长的应用规则内容。", 200)) {
		t.Fatalf("over-budget nested instructions were not truncated:\n%s", truncated.Content)
	}

	// 文件缩小后会重新渲染为完整替换：下一次触碰看到新摘要，预算通知随之消失。
	refreshWriteFile(t, h.workspace, "packages/app/AGENTS.md", "# 应用规则\n\n简短的规则。\n")
	h.touch("read_file", "packages/app/main.go", false)
	history = h.composeTouched(history)
	if len(history) != 3 {
		t.Fatalf("shrunk nested instructions did not re-render: %#v", history)
	}
	replaced := history[2]
	refreshAssertContains(t, replaced.Content,
		"来自以下文件的指令已更新：packages/app/AGENTS.md",
		"简短的规则。",
	)
	if strings.Contains(replaced.Content, "工作区指令预算") {
		t.Fatalf("replacement still carries the stale budget notice:\n%s", replaced.Content)
	}
}
