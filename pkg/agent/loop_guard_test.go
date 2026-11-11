// Chagent - 超轻量级个人 AI agent

package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// loopGuardObserve 记录一次调用并返回提示（如果有）。
func loopGuardObserve(guard *repeatToolGuard, toolName string, args map[string]any) *providers.Message {
	return guard.Observe(toolName, args)
}

// loopGuardHitCounts 执行 n 次相同调用并报告产生了多少条提示。
// 只关心“是否触发”的测试使用它。
func loopGuardHitCounts(guard *repeatToolGuard, toolName string, args map[string]any, n int) int {
	hits := 0
	for i := 0; i < n; i++ {
		if loopGuardObserve(guard, toolName, args) != nil {
			hits++
		}
	}
	return hits
}

func TestRepeatToolGuardThresholds(t *testing.T) {
	guard := newRepeatToolGuard(DefaultLoopGuardConfig())
	args := map[string]any{"path": "docs/readme.md"}
	wantHits := map[int]bool{3: true, 5: true, 8: true}

	for call := 1; call <= 12; call++ {
		msg := loopGuardObserve(guard, "read_file", args)
		if hit := msg != nil; hit != wantHits[call] {
			t.Fatalf("call %d: reminder fired=%v, want %v", call, hit, wantHits[call])
		}
		if msg == nil {
			continue
		}
		switch call {
		case 3:
			if msg.Content != loopGuardBriefReminder {
				t.Fatalf("call 3: got brief reminder %q, want the shared brief constant", msg.Content)
			}
		case 5, 8:
			if want := fmt.Sprintf("连续调用次数：%d", call); !strings.Contains(msg.Content, want) {
				t.Fatalf("call %d: detailed reminder %q missing %q", call, msg.Content, want)
			}
		}
	}
}

func TestRepeatToolGuardDifferentCallResets(t *testing.T) {
	guard := newRepeatToolGuard(DefaultLoopGuardConfig())
	first := map[string]any{"path": "a.md"}
	second := map[string]any{"path": "b.md"}

	if hits := loopGuardHitCounts(guard, "read_file", first, 2); hits != 0 {
		t.Fatalf("two calls fired %d reminders, want 0", hits)
	}
	// 参数不同会把链重置为 1，因此还不会有提示。
	if msg := loopGuardObserve(guard, "read_file", second); msg != nil {
		t.Fatalf("first call with new arguments fired a reminder: %q", msg.Content)
	}
	if hits := loopGuardHitCounts(guard, "read_file", second, 1); hits != 0 {
		t.Fatalf("second call of the new chain fired %d reminders, want 0", hits)
	}
	// 回到第一个身份：新链从 1 开始，因此前两次静默，第三次触发。
	if hits := loopGuardHitCounts(guard, "read_file", first, 2); hits != 0 {
		t.Fatalf("restarted chain fired %d reminders, want 0", hits)
	}
	if msg := loopGuardObserve(guard, "read_file", first); msg == nil {
		t.Fatal("third call of the restarted chain did not fire a reminder")
	}
	// 工具名不同但参数相同属于不同的链。
	other := newRepeatToolGuard(DefaultLoopGuardConfig())
	loopGuardObserve(other, "read_file", first)
	loopGuardObserve(other, "read_file", first)
	if msg := loopGuardObserve(other, "write_file", first); msg != nil {
		t.Fatalf("different tool started mid-chain fired a reminder: %q", msg.Content)
	}
}

func TestRepeatToolGuardCanonicalArgumentsIgnoreKeyOrder(t *testing.T) {
	// Go map 字面量没有顺序，因此用不同插入顺序构造两个值，
	// 以检验规范化（包括嵌套 map）。
	argsA := map[string]any{"path": "docs/x.md"}
	nestedA := map[string]any{"alpha": 1}
	nestedA["beta"] = 2
	argsA["nested"] = nestedA

	argsB := map[string]any{"nested": map[string]any{"beta": 2, "alpha": 1}}
	argsB["path"] = "docs/x.md"

	canonicalA := canonicalLoopGuardArguments(argsA)
	canonicalB := canonicalLoopGuardArguments(argsB)
	if canonicalA != canonicalB {
		t.Fatalf("canonical arguments differ:\n A=%q\n B=%q", canonicalA, canonicalB)
	}

	guard := newRepeatToolGuard(DefaultLoopGuardConfig())
	loopGuardObserve(guard, "read_file", argsA)
	loopGuardObserve(guard, "read_file", argsB)
	if msg := loopGuardObserve(guard, "read_file", argsA); msg == nil {
		t.Fatal("key-order variants did not share one chain")
	}
}

func TestRepeatToolGuardUntrackedTransparent(t *testing.T) {
	guard := newRepeatToolGuard(DefaultLoopGuardConfig())
	args := map[string]any{"path": "docs/readme.md"}

	loopGuardObserve(guard, "read_file", args) // count 1
	loopGuardObserve(guard, "read_file", args) // count 2
	// 默认排除项本身不得产生提示。
	if msg := loopGuardObserve(guard, "todo_write", map[string]any{"todos": []any{}}); msg != nil {
		t.Fatalf("excluded tool fired a reminder: %q", msg.Content)
	}
	// 被排除的调用也不得重置 read_file 链：下一次调用是连续第三次被跟踪调用，必须触发。
	if msg := loopGuardObserve(guard, "read_file", args); msg == nil {
		t.Fatal("excluded call reset the chain; third tracked call did not fire")
	}

	// 无限重复被排除的工具始终保持沉默。
	excluded := newRepeatToolGuard(DefaultLoopGuardConfig())
	for call := 0; call < 10; call++ {
		if msg := loopGuardObserve(excluded, "todo_write", nil); msg != nil {
			t.Fatalf("excluded tool call %d fired a reminder: %q", call+1, msg.Content)
		}
		if msg := loopGuardObserve(excluded, "task_get", map[string]any{"id": "t1"}); msg != nil {
			t.Fatalf("excluded task_get call %d fired a reminder: %q", call+1, msg.Content)
		}
	}
}

func TestRepeatToolGuardIncludeFilter(t *testing.T) {
	cfg := DefaultLoopGuardConfig()
	cfg.Include = []string{"read_*"}
	guard := newRepeatToolGuard(cfg)

	if hits := loopGuardHitCounts(guard, "write_file", map[string]any{"path": "a"}, 6); hits != 0 {
		t.Fatalf("untracked tool fired %d reminders, want 0", hits)
	}
	// Include 不得被上面未跟踪的调用破坏。
	if hits := loopGuardHitCounts(guard, "read_file", map[string]any{"path": "a"}, 3); hits != 1 {
		t.Fatalf("included tool fired %d reminders over 3 calls, want 1", hits)
	}

	// Exclude 优先于 Include。
	cfg = DefaultLoopGuardConfig()
	cfg.Include = []string{"read_*"}
	cfg.Exclude = []string{"read_secret"}
	guarded := newRepeatToolGuard(cfg)
	if hits := loopGuardHitCounts(guarded, "read_secret", map[string]any{"path": "a"}, 8); hits != 0 {
		t.Fatalf("exclude did not win over include: %d reminders, want 0", hits)
	}

	// 精确模式与裸 "*" 通配符按文档行为工作。
	if hits := loopGuardHitCounts(newRepeatToolGuard(DefaultLoopGuardConfig()), "read_file", map[string]any{"p": 1}, 3); hits != 1 {
		t.Fatalf("default (track everything) fired %d reminders, want 1", hits)
	}
	allCfg := DefaultLoopGuardConfig()
	allCfg.Include = []string{"*"}
	allCfg.Exclude = []string{} // 非 nil 的空切片显式禁用默认排除项
	if hits := loopGuardHitCounts(newRepeatToolGuard(allCfg), "todo_write", nil, 3); hits != 1 {
		t.Fatalf("bare wildcard include fired %d reminders, want 1", hits)
	}
	emptyCfg := DefaultLoopGuardConfig()
	emptyCfg.Include = []string{}
	emptyCfg.Exclude = []string{}
	if hits := loopGuardHitCounts(newRepeatToolGuard(emptyCfg), "any_tool", nil, 3); hits != 1 {
		t.Fatalf("empty include fired %d reminders, want 1", hits)
	}
}

func TestRepeatToolGuardResetOnNewUserInput(t *testing.T) {
	guard := newRepeatToolGuard(DefaultLoopGuardConfig())
	args := map[string]any{"path": "docs/readme.md"}

	if msg := loopGuardObserve(guard, "read_file", args); msg != nil {
		t.Fatalf("first call fired: %q", msg.Content)
	}
	loopGuardObserve(guard, "read_file", args)
	if msg := loopGuardObserve(guard, "read_file", args); msg == nil {
		t.Fatal("third call did not fire before reset")
	}

	guard.Reset()
	if hits := loopGuardHitCounts(guard, "read_file", args, 2); hits != 0 {
		t.Fatalf("after reset, two calls fired %d reminders, want 0", hits)
	}
	if msg := loopGuardObserve(guard, "read_file", args); msg == nil {
		t.Fatal("after reset, the third call did not fire")
	}

	// Reset 还必须清空身份，而不仅是计数器。
	guard.Reset()
	if msg := loopGuardObserve(guard, "write_file", map[string]any{"path": "x"}); msg != nil {
		t.Fatalf("first call after reset fired: %q", msg.Content)
	}
}

func TestRepeatToolGuardDisabled(t *testing.T) {
	cfg := DefaultLoopGuardConfig()
	cfg.Enabled = false
	guard := newRepeatToolGuard(cfg)

	if hits := loopGuardHitCounts(guard, "read_file", map[string]any{"path": "a"}, 10); hits != 0 {
		t.Fatalf("disabled guard fired %d reminders, want 0", hits)
	}

	// 零值配置同样处于禁用状态。
	zero := newRepeatToolGuard(LoopGuardConfig{})
	if hits := loopGuardHitCounts(zero, "read_file", map[string]any{"path": "a"}, 10); hits != 0 {
		t.Fatalf("zero-value config guard fired %d reminders, want 0", hits)
	}
}

func TestRepeatToolGuardDetailedReminderPreviewCap(t *testing.T) {
	const previewCap = 20
	cfg := DefaultLoopGuardConfig()
	cfg.ArgumentsPreviewChars = previewCap
	guard := newRepeatToolGuard(cfg)

	args := map[string]any{"path": strings.Repeat("长", 100)}
	var msg *providers.Message
	for call := 1; call <= 5; call++ {
		msg = loopGuardObserve(guard, "read_file", args)
	}
	if msg == nil {
		t.Fatal("detailed threshold did not fire")
	}

	canonical := canonicalLoopGuardArguments(args)
	totalRunes := utf8.RuneCountInString(canonical)
	if totalRunes <= previewCap {
		t.Fatalf("test arguments are not long enough: %d runes", totalRunes)
	}
	wantSuffix := fmt.Sprintf("…（另有 %d 字符）", totalRunes-previewCap)
	if !strings.Contains(msg.Content, wantSuffix) {
		t.Fatalf("detailed reminder %q missing truncation suffix %q", msg.Content, wantSuffix)
	}
	head := string([]rune(canonical)[:previewCap])
	if !strings.Contains(msg.Content, head) {
		t.Fatalf("detailed reminder %q missing preview head %q", msg.Content, head)
	}
	if strings.Contains(msg.Content, canonical) {
		t.Fatal("detailed reminder quoted the full canonical arguments despite the cap")
	}

	// 较短的参数列表会被完整引用。
	short := newRepeatToolGuard(DefaultLoopGuardConfig())
	shortArgs := map[string]any{"path": "a"}
	var shortMsg *providers.Message
	for call := 1; call <= 5; call++ {
		shortMsg = loopGuardObserve(short, "read_file", shortArgs)
	}
	if shortMsg == nil {
		t.Fatal("detailed threshold did not fire for short arguments")
	}
	if !strings.Contains(shortMsg.Content, canonicalLoopGuardArguments(shortArgs)) {
		t.Fatalf("short detailed reminder %q does not quote the full arguments", shortMsg.Content)
	}
}

func TestRepeatToolGuardReminderIsChineseAndTyped(t *testing.T) {
	guard := newRepeatToolGuard(DefaultLoopGuardConfig())
	args := map[string]any{"path": "docs/readme.md"}

	var msg *providers.Message
	for call := 1; call <= 3; call++ {
		msg = loopGuardObserve(guard, "read_file", args)
	}
	if msg == nil {
		t.Fatal("threshold 3 did not fire")
	}
	if msg.Role != "user" {
		t.Fatalf("reminder role = %q, want user", msg.Role)
	}
	if msg.Context == nil {
		t.Fatal("reminder has no context metadata")
	}
	if msg.Context.Kind != protocoltypes.ContextKindLoopGuard {
		t.Fatalf("context kind = %q, want %q", msg.Context.Kind, protocoltypes.ContextKindLoopGuard)
	}
	if msg.Context.Form != protocoltypes.ContextFormReminder {
		t.Fatalf("context form = %q, want %q", msg.Context.Form, protocoltypes.ContextFormReminder)
	}
	if msg.Context.Digest == "" {
		t.Fatal("reminder digest is empty")
	}
	if msg.PromptSlot != "" {
		t.Fatalf("reminder sets PromptSlot = %q; it must not masquerade as a real user turn", msg.PromptSlot)
	}
	if !msg.IsLoopGuard() {
		t.Fatal("reminder does not report IsLoopGuard()")
	}
	for _, phrase := range []string{"重复", "工具", "参数"} {
		if !strings.Contains(msg.Content, phrase) {
			t.Fatalf("brief reminder %q missing Chinese phrase %q", msg.Content, phrase)
		}
	}

	// 摘要对相同的 (tool, count, key) 输入稳定，并在下一个阈值时不同。
	var detailed *providers.Message
	for call := 4; call <= 5; call++ {
		detailed = loopGuardObserve(guard, "read_file", args)
	}
	if detailed == nil {
		t.Fatal("threshold 5 did not fire")
	}
	for _, want := range []string{"工具：read_file", "连续调用次数：5", "重复", "参数"} {
		if !strings.Contains(detailed.Content, want) {
			t.Fatalf("detailed reminder %q missing %q", detailed.Content, want)
		}
	}
	if detailed.Context.Digest == msg.Context.Digest {
		t.Fatalf("digest %q did not change between thresholds", detailed.Context.Digest)
	}
}

func TestRepeatToolGuardInvalidThresholdsFallback(t *testing.T) {
	cases := []struct {
		name       string
		thresholds []int
	}{
		{name: "empty", thresholds: []int{}},
		{name: "nil", thresholds: nil},
		{name: "below two", thresholds: []int{1, 3}},
		{name: "duplicate", thresholds: []int{3, 3, 5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultLoopGuardConfig()
			cfg.Thresholds = tc.thresholds
			guard := newRepeatToolGuard(cfg)
			if hits := loopGuardHitCounts(guard, "read_file", map[string]any{"path": "a"}, 2); hits != 0 {
				t.Fatalf("fallback fired %d reminders in the first two calls, want 0", hits)
			}
			if msg := loopGuardObserve(guard, "read_file", map[string]any{"path": "a"}); msg == nil {
				t.Fatal("fallback defaults did not fire at the third call")
			}
		})
	}

	// 合法但未排序的输入会被排序而不是拒绝。
	cfg := DefaultLoopGuardConfig()
	cfg.Thresholds = []int{5, 3}
	guard := newRepeatToolGuard(cfg)
	if msg := loopGuardHitCounts(guard, "read_file", map[string]any{"path": "a"}, 3); msg != 1 {
		t.Fatalf("unsorted thresholds fired %d reminders over 3 calls, want 1", msg)
	}
	var detailed *providers.Message
	for call := 4; call <= 5; call++ {
		detailed = loopGuardObserve(guard, "read_file", map[string]any{"path": "a"})
	}
	if detailed == nil {
		t.Fatal("unsorted thresholds did not fire at the second threshold")
	}
}

func TestRepeatToolGuardMarshalFailureFallback(t *testing.T) {
	guard := newRepeatToolGuard(DefaultLoopGuardConfig())
	args := map[string]any{"callback": func() {}}
	if _, err := json.Marshal(args); err == nil {
		t.Fatal("test arguments unexpectedly marshal; the fallback path is not exercised")
	}

	if hits := loopGuardHitCounts(guard, "read_file", args, 2); hits != 0 {
		t.Fatalf("unmarshallable arguments fired %d reminders in the first two calls, want 0", hits)
	}
	if msg := loopGuardObserve(guard, "read_file", args); msg == nil {
		t.Fatal("unmarshallable arguments did not fire at the third call (no panic, degraded key)")
	}

	// nil 参数规范化为空字符串并继续被跟踪。
	nilGuard := newRepeatToolGuard(DefaultLoopGuardConfig())
	if msg := loopGuardHitCounts(nilGuard, "task_get", nil, 3); msg != 0 {
		// task_get 默认被排除，因此链永远不会开始。
		t.Fatalf("excluded task_get fired %d reminders, want 0", msg)
	}
	if msg := loopGuardHitCounts(newRepeatToolGuard(DefaultLoopGuardConfig()), "read_file", nil, 3); msg != 1 {
		t.Fatalf("nil arguments fired %d reminders over 3 calls, want 1", msg)
	}
}
