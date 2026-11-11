package workspacecontext

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/providers"
)

func mustProjector(t *testing.T, cfg Config, workspace, excludePath string) *Projector {
	t.Helper()
	projector, err := NewProjector(cfg, workspace, excludePath)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	return projector
}

func mustCompose(t *testing.T, projector *Projector, history []providers.Message, touched []string) ComposeResult {
	t.Helper()
	result, err := projector.Compose(context.Background(), history, touched)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	return result
}

func appendHistory(history []providers.Message, result ComposeResult) []providers.Message {
	if result.Message == nil {
		return history
	}
	return append(history, *result.Message)
}

func TestComposeBaseline(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")

	result := mustCompose(t, projector, nil, nil)
	if result.Message == nil {
		t.Fatal("baseline compose returned no message")
	}
	if result.Message.Role != "user" {
		t.Fatalf("message role = %q, want user", result.Message.Role)
	}
	meta := result.Message.Context
	if meta == nil {
		t.Fatal("baseline message has no Context metadata")
	}
	if meta.Kind != ContextKindWorkspaceInstructions {
		t.Fatalf("kind = %q", meta.Kind)
	}
	if meta.Form != ContextFormReminder {
		t.Fatalf("form = %q, want reminder", meta.Form)
	}
	if !meta.Baseline {
		t.Fatal("Baseline = false, want true")
	}
	if meta.BaselineIdentity == "" {
		t.Fatal("BaselineIdentity is empty")
	}
	text := result.Message.Content
	for _, want := range []string{
		frameOpen,
		baselineIntro,
		"来自以下文件的指令：AGENTS.md",
		"来自以下文件的指令：AGENTS.local.md",
		frameClose,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("baseline missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "CLAUDE.md") {
		t.Fatalf("deduped CLAUDE.md leaked into baseline:\n%s", text)
	}
	if strings.Contains(text, workspace) {
		t.Fatalf("baseline leaks absolute workspace path:\n%s", text)
	}
	if !result.Stats.Baseline || result.Stats.Files < 2 || result.Stats.Bytes != len(text) {
		t.Fatalf("stats = %+v, want baseline files>=2 bytes=%d", result.Stats, len(text))
	}
	for _, change := range meta.Changes {
		if change.Action != ChangeActionSet {
			t.Fatalf("baseline change = %+v, want set", change)
		}
		if change.Scope == "" || change.Path == "" {
			t.Fatalf("baseline change missing scope/path: %+v", change)
		}
	}
}

func TestComposeNoChanges(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")

	first := mustCompose(t, projector, nil, nil)
	history := appendHistory(nil, first)

	second := mustCompose(t, projector, history, nil)
	if second.Message != nil {
		t.Fatalf("second compose injected a duplicate message:\n%s", second.Message.Content)
	}

	// Cold projector over the same history must not duplicate the baseline either
	// (resume/restart semantics).
	cold := mustProjector(t, DefaultConfig(), workspace, "")
	third := mustCompose(t, cold, history, nil)
	if third.Message != nil {
		t.Fatalf("cold projector injected a duplicate message:\n%s", third.Message.Content)
	}
}

func TestComposeAdditionalOnTouch(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")
	first := mustCompose(t, projector, nil, nil)
	history := appendHistory(nil, first)

	touched := filepath.Join(workspace, "packages", "app", "main.go")
	result := mustCompose(t, projector, history, []string{touched})
	if result.Message == nil {
		t.Fatal("touch did not inject nested instructions")
	}
	meta := result.Message.Context
	if meta == nil || meta.Baseline {
		t.Fatalf("delta metadata = %+v, want non-baseline", meta)
	}
	text := result.Message.Content
	for _, want := range []string{
		"来自以下文件的新增指令：packages/app/AGENTS.md",
		"以下指令适用于 `packages/app` 下的工作；",
		"修改 `packages/app` 时同步更新文档。",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("delta missing %q:\n%s", want, text)
		}
	}
	if len(meta.Changes) != 1 || meta.Changes[0].Action != ChangeActionSet {
		t.Fatalf("delta changes = %+v, want one set", meta.Changes)
	}

	history = appendHistory(history, result)
	again := mustCompose(t, projector, history, []string{touched})
	if again.Message != nil {
		t.Fatalf("same touch injected the delta twice:\n%s", again.Message.Content)
	}
}

func TestComposeReplace(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")
	first := mustCompose(t, projector, nil, nil)
	history := appendHistory(nil, first)

	newContent := "# 根指令 v2\n\n- 更新后的内容更长一些，确保版本信息变化。\n- 第二条新增规则。\n"
	writeFile(t, filepath.Join(workspace, "AGENTS.md"), newContent)

	result := mustCompose(t, projector, history, nil)
	if result.Message == nil {
		t.Fatal("file change did not inject a delta")
	}
	text := result.Message.Content
	for _, want := range []string{
		"来自以下文件的指令已更新：AGENTS.md",
		updatedBodySentence,
		"更新后的内容更长一些",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("replace delta missing %q:\n%s", want, text)
		}
	}
	// Changing the dedupe winner also promotes the previously hidden duplicate,
	// so a correct delta contains replace(AGENTS.md) + set(CLAUDE.md).
	hasReplace, hasPromotion := false, false
	for _, change := range result.Message.Context.Changes {
		switch {
		case change.Scope == scopeKey(".", "AGENTS.md") && change.Action == ChangeActionReplace:
			hasReplace = true
		case change.Scope == scopeKey(".", "CLAUDE.md") && change.Action == ChangeActionSet:
			hasPromotion = true
		}
	}
	if !hasReplace || !hasPromotion {
		t.Fatalf("changes = %+v, want replace AGENTS.md + set CLAUDE.md", result.Message.Context.Changes)
	}
	if strings.Contains(text, "来自以下文件的指令：AGENTS.md") {
		t.Fatalf("replace delta rendered a baseline section:\n%s", text)
	}
}

func TestComposeRemove(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")
	first := mustCompose(t, projector, nil, nil)
	history := appendHistory(nil, first)

	removeFile(t, filepath.Join(workspace, "AGENTS.local.md"))
	result := mustCompose(t, projector, history, nil)
	if result.Message == nil {
		t.Fatal("file removal did not inject a delta")
	}
	text := result.Message.Content
	for _, want := range []string{
		"以下文件的指令已移除：AGENTS.local.md",
		"此前从该文件加载的指令不再适用。",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("remove delta missing %q:\n%s", want, text)
		}
	}
	if len(result.Message.Context.Changes) != 1 || result.Message.Context.Changes[0].Action != ChangeActionRemove {
		t.Fatalf("changes = %+v, want one remove", result.Message.Context.Changes)
	}

	history = appendHistory(history, result)
	again := mustCompose(t, projector, history, nil)
	if again.Message != nil {
		t.Fatalf("removed file was reported twice:\n%s", again.Message.Content)
	}
}

func TestComposeDuplicatePromotion(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")
	first := mustCompose(t, projector, nil, nil)
	history := appendHistory(nil, first)
	if strings.Contains(first.Message.Content, "CLAUDE.md") {
		t.Fatal("baseline unexpectedly contains the deduped CLAUDE.md")
	}

	// AGENTS.md and CLAUDE.md have identical trimmed content. Removing the
	// winner must promote CLAUDE.md (set) and remove AGENTS.md (remove).
	removeFile(t, filepath.Join(workspace, "AGENTS.md"))
	result := mustCompose(t, projector, history, nil)
	if result.Message == nil {
		t.Fatal("duplicate promotion did not inject a delta")
	}
	text := result.Message.Content
	if !strings.Contains(text, "来自以下文件的新增指令：CLAUDE.md") {
		t.Fatalf("promoted CLAUDE.md missing:\n%s", text)
	}
	if !strings.Contains(text, "以下文件的指令已移除：AGENTS.md") {
		t.Fatalf("removed AGENTS.md missing:\n%s", text)
	}
	actions := make(map[string]string)
	for _, change := range result.Message.Context.Changes {
		actions[change.Path] = change.Action
	}
	if actions["CLAUDE.md"] != ChangeActionSet {
		t.Fatalf("CLAUDE.md action = %q, want set (%+v)", actions["CLAUDE.md"], actions)
	}
	if actions["AGENTS.md"] != ChangeActionRemove {
		t.Fatalf("AGENTS.md action = %q, want remove (%+v)", actions["AGENTS.md"], actions)
	}
}

func TestComposeBaselineReplacement(t *testing.T) {
	workspace := prepareWorkspace(t)
	projectorV1 := mustProjector(t, DefaultConfig(), workspace, "")
	first := mustCompose(t, projectorV1, nil, nil)
	history := appendHistory(nil, first)

	// Config change (instruction file list) changes the baseline identity, so the
	// next compose must replace the old baseline and remove stale scopes.
	cfgV2 := DefaultConfig()
	cfgV2.InstructionFiles = []string{"OTHER.md"}
	projectorV2 := mustProjector(t, cfgV2, workspace, "")
	result := mustCompose(t, projectorV2, history, nil)
	if result.Message == nil {
		t.Fatal("identity change did not inject a replacement baseline")
	}
	meta := result.Message.Context
	if meta == nil || !meta.Baseline {
		t.Fatalf("replacement metadata = %+v, want baseline", meta)
	}
	if meta.BaselineIdentity == first.Message.Context.BaselineIdentity {
		t.Fatal("baseline identity did not change")
	}
	text := result.Message.Content
	for _, want := range []string{
		baselineReplaceIntro,
		"来自以下文件的指令：AGENTS.local.md",
		"以下文件的指令已移除：AGENTS.md",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("replacement baseline missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "来自以下文件的指令：AGENTS.md\n") {
		t.Fatalf("stale AGENTS.md section still rendered:\n%s", text)
	}

	history = appendHistory(history, result)
	again := mustCompose(t, projectorV2, history, nil)
	if again.Message != nil {
		t.Fatalf("replacement baseline was injected twice:\n%s", again.Message.Content)
	}
}

func TestComposeUnavailableKeepsLastGood(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can always read files; permission errors cannot be triggered")
	}
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")
	first := mustCompose(t, projector, nil, nil)
	history := appendHistory(nil, first)

	path := filepath.Join(workspace, "AGENTS.md")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	cold := mustProjector(t, DefaultConfig(), workspace, "")
	result := mustCompose(t, cold, history, nil)
	if result.Message != nil {
		if strings.Contains(result.Message.Content, "已移除") {
			t.Fatalf("unreadable file produced a removal:\n%s", result.Message.Content)
		}
		if strings.Contains(result.Message.Content, "新增指令：CLAUDE.md") {
			t.Fatalf("duplicate was promoted while its group winner was unreadable:\n%s", result.Message.Content)
		}
	}
}

func TestComposeEmptyFile(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")
	first := mustCompose(t, projector, nil, nil)
	history := appendHistory(nil, first)

	newDir := filepath.Join(workspace, "packages", "empty")
	writeFile(t, filepath.Join(newDir, "AGENTS.md"), "")
	touched := filepath.Join(newDir, "main.go")
	result := mustCompose(t, projector, history, []string{touched})
	if result.Message == nil {
		t.Fatal("empty instruction file produced no delta; empty content is legal")
	}
	text := result.Message.Content
	if !strings.Contains(text, "来自以下文件的新增指令：packages/empty/AGENTS.md") {
		t.Fatalf("empty file section missing:\n%s", text)
	}
	if !strings.HasSuffix(text, frameClose) {
		t.Fatalf("frame not closed for empty file:\n%s", text)
	}
	if len(result.Message.Context.Changes) != 1 {
		t.Fatalf("changes = %+v, want one set", result.Message.Context.Changes)
	}
}

func TestComposeErrorDoesNotPanic(t *testing.T) {
	workspace := prepareWorkspace(t)
	projector := mustProjector(t, DefaultConfig(), workspace, "")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := projector.Compose(cancelled, nil, nil); err == nil {
		t.Fatal("Compose ignored a canceled context")
	}

	// Malformed / foreign history must not panic and must not be treated as a
	// workspace context baseline.
	history := []providers.Message{
		{Role: "user", Context: &ContextMeta{Kind: "runtime-context", Baseline: true}},
		{Role: "user", Content: "普通消息"},
	}
	if result := mustCompose(t, projector, history, nil); result.Message == nil {
		t.Fatal("fresh session with foreign context did not get a baseline")
	}

	if os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(t.TempDir(), "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	if _, err := NewProjector(DefaultConfig(), locked, ""); err == nil {
		t.Fatal("NewProjector swallowed a project root permission error")
	}
}

func TestComposeBudgetExclusion(t *testing.T) {
	workspace := prepareWorkspace(t)
	cfg := DefaultConfig()
	// The fixture chain (AGENTS.md + AGENTS.local.md) is ~234 bytes; a 150-byte
	// per-chain budget must omit the broader AGENTS.md block first.
	cfg.MaxBytes = 150
	projector := mustProjector(t, cfg, workspace, "")

	first := mustCompose(t, projector, nil, nil)
	if first.Message == nil {
		t.Fatal("baseline compose returned no message")
	}
	if len(projector.excluded) == 0 {
		t.Fatalf("baseline did not record budget-excluded scopes (omitted=%v stats=%+v)",
			first.Message.Context.Changes, first.Stats)
	}
	if !strings.Contains(first.Message.Content, "工作区指令预算 150 字节：") {
		t.Fatalf("budget notice missing:\n%s", first.Message.Content)
	}
	if strings.Contains(first.Message.Content, "来自以下文件的指令：AGENTS.md\n") {
		t.Fatalf("budget-omitted AGENTS.md was still rendered:\n%s", first.Message.Content)
	}

	history := appendHistory(nil, first)
	second := mustCompose(t, projector, history, nil)
	if second.Message != nil {
		t.Fatalf("budget-excluded scope was re-probed and injected:\n%s", second.Message.Content)
	}
	if len(projector.excluded) == 0 {
		t.Fatal("budget exclusion cache was lost across composes")
	}
}
