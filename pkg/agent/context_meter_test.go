// Chagent - 超轻量级个人 AI agent

package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

func testContextMeterAgent(contextWindow, maxTokens int) *AgentInstance {
	return &AgentInstance{ContextWindow: contextWindow, MaxTokens: maxTokens}
}

func TestEstimateRequestTokens(t *testing.T) {
	messages := []providers.Message{
		{Role: "user", Content: strings.Repeat("a", 250)},
		{Role: "assistant", Content: strings.Repeat("b", 250)},
	}
	small := estimateRequestTokens(messages, nil)
	if small <= 0 {
		t.Fatalf("estimate = %d, want > 0", small)
	}
	big := estimateRequestTokens(append(messages, providers.Message{
		Role:    "user",
		Content: strings.Repeat("c", 2500),
	}), nil)
	if big <= small {
		t.Fatalf("estimate did not grow: %d -> %d", small, big)
	}
	defs := []providers.ToolDefinition{{
		Type: "function",
		Function: providers.ToolFunctionDefinition{
			Name:        "read_file",
			Description: "读取文件内容",
			Parameters:  map[string]any{"type": "object"},
		},
	}}
	withTools := estimateRequestTokens(messages, defs)
	if withTools <= small {
		t.Fatalf("tool definitions not counted: %d (no tools) vs %d", small, withTools)
	}
}

func TestUsageAnchorCorrectionRatio(t *testing.T) {
	ts := &turnState{agent: testContextMeterAgent(32768, 8192)}
	if got := ts.usageAnchorSnapshot().correctionRatio(); got != 1 {
		t.Fatalf("nil anchor ratio = %v, want 1", got)
	}

	messages := []providers.Message{{Role: "user", Content: strings.Repeat("a", 1000)}}
	ts.recordUsageAnchor(&providers.UsageInfo{PromptTokens: 1000}, messages, nil)
	anchor := ts.usageAnchorSnapshot()
	if anchor == nil {
		t.Fatal("anchor not recorded")
	}
	if anchor.promptTokens != 1000 {
		t.Errorf("anchor prompt tokens = %d, want 1000", anchor.promptTokens)
	}
	// 1000 个 ASCII 字符的启发式估算约为 400 token；
	// 比例必须钳制在 [0.25, 4] 且只能把估算往上校正。
	ratio := anchor.correctionRatio()
	if ratio < 0.25 || ratio > 4 {
		t.Fatalf("ratio out of clamp band: %v", ratio)
	}
	calibrated := calibratedPromptTokens(ts, messages, nil)
	estimated := estimateRequestTokens(messages, nil)
	if calibrated < estimated {
		t.Fatalf("calibration shrank below heuristic: %d < %d", calibrated, estimated)
	}
}

func TestUsageAnchorIgnoresZeroUsage(t *testing.T) {
	ts := &turnState{agent: testContextMeterAgent(32768, 8192)}
	messages := []providers.Message{{Role: "user", Content: "hi"}}
	ts.recordUsageAnchor(nil, messages, nil)
	if ts.usageAnchorSnapshot() != nil {
		t.Fatal("nil usage recorded an anchor")
	}
	ts.recordUsageAnchor(&providers.UsageInfo{PromptTokens: 0}, messages, nil)
	if ts.usageAnchorSnapshot() != nil {
		t.Fatal("zero prompt tokens recorded an anchor")
	}
}

func TestContextPromptBudget(t *testing.T) {
	cases := []struct {
		name           string
		window         int
		maxTokens      int
		wantGreaterOne bool
	}{
		{"large window uses ratio bound", 131072, 8192, true},
		{"small window uses output reservation", 32768, 8192, true},
		{"missing agent", 0, 8192, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var agent *AgentInstance
			if tc.window > 0 {
				agent = testContextMeterAgent(tc.window, tc.maxTokens)
			}
			budget := contextPromptBudget(agent, DefaultContextPressurePolicy())
			if tc.wantGreaterOne && budget <= 1 {
				t.Fatalf("budget = %d, want > 1", budget)
			}
			if !tc.wantGreaterOne && budget != 0 {
				t.Fatalf("budget = %d, want 0", budget)
			}
			if tc.window > 0 && budget > tc.window {
				t.Fatalf("budget %d exceeds window %d", budget, tc.window)
			}
		})
	}

	// 大窗口下比例上限占优：0.8 * 131072 = 104857。
	if got := contextPromptBudget(testContextMeterAgent(131072, 8192), DefaultContextPressurePolicy()); got != 104857 {
		t.Errorf("large-window budget = %d, want 104857", got)
	}
	// 小默认窗口下输出预留占优：
	// min(0.8*32768=26214, 32768-8192=24576) = 24576。
	if got := contextPromptBudget(testContextMeterAgent(32768, 8192), DefaultContextPressurePolicy()); got != 24576 {
		t.Errorf("small-window budget = %d, want 24576", got)
	}
}

func TestContextPressureNoticeBandOncePerBand(t *testing.T) {
	// 窗口 4000 / maxTokens 1000 → 预算 = min(3200, 3000) = 3000。
	// 3000 的 85% = 2550 token；约 6400 个 ASCII 字符 ≈ 2560 token。
	ts := &turnState{agent: testContextMeterAgent(4000, 1000)}
	smallMessages := []providers.Message{{Role: "user", Content: "hi"}}
	if notice := ts.maybeContextPressureNotice(smallMessages, nil); notice != nil {
		t.Fatalf("notice fired below the first band: %q", notice.Content)
	}

	band80 := []providers.Message{{Role: "user", Content: strings.Repeat("a", 7000)}}
	notice := ts.maybeContextPressureNotice(band80, nil)
	if notice == nil {
		t.Fatal("notice did not fire at the 85% band")
	}
	if !notice.IsContextPressure() {
		t.Fatalf("notice kind = %+v, want context-pressure", notice.Context)
	}
	if notice.Role != "user" {
		t.Errorf("notice role = %q, want user", notice.Role)
	}
	if !strings.Contains(notice.Content, "上下文") {
		t.Errorf("notice is not Chinese: %q", notice.Content)
	}
	if notice.Context.Digest != "0.85" {
		t.Errorf("notice digest = %q, want 0.85", notice.Context.Digest)
	}
	// 同一区间在轮次内不重复提示。
	if again := ts.maybeContextPressureNotice(band80, nil); again != nil {
		t.Fatalf("notice repeated for the same band: %q", again.Content)
	}

	// 跨过下一个区间只多出一次通知。
	band95 := []providers.Message{{Role: "user", Content: strings.Repeat("a", 8200)}}
	next := ts.maybeContextPressureNotice(band95, nil)
	if next == nil {
		t.Fatal("notice did not fire at the 95% band")
	}
	if next.Context.Digest != "0.95" {
		t.Errorf("next digest = %q, want 0.95", next.Context.Digest)
	}
}

func TestContextMeterNoticeIsNonBoundaryContext(t *testing.T) {
	notice := contextPressureNotice(2600, 3000, 0.87)
	if !strings.Contains(notice, "87%") {
		t.Errorf("notice does not include percentage: %q", notice)
	}
	msg := providers.Message{
		Role:    "user",
		Content: notice,
		Context: &protocoltypes.ContextMeta{Kind: protocoltypes.ContextKindContextPressure},
	}
	if isTurnBoundary(msg) {
		t.Error("context-pressure notice must not start a turn")
	}
	if !msg.IsContextPressure() {
		t.Error("notice missing context-pressure helper")
	}
}

func TestUsageAnchorFreshnessWindow(t *testing.T) {
	// 锚点是每轮状态，不会跨轮陈旧；本测试只锁定记录的时间戳以及快照为副本。
	ts := &turnState{agent: testContextMeterAgent(32768, 8192)}
	messages := []providers.Message{{Role: "user", Content: strings.Repeat("a", 1000)}}
	ts.recordUsageAnchor(&providers.UsageInfo{PromptTokens: 500}, messages, nil)
	first := ts.usageAnchorSnapshot()
	if first == nil || first.at.IsZero() {
		t.Fatal("anchor timestamp not recorded")
	}
	if time.Since(first.at) > time.Minute {
		t.Fatalf("anchor timestamp looks wrong: %v", first.at)
	}
	first.promptTokens = 1
	if ts.usageAnchorSnapshot().promptTokens == 1 {
		t.Fatal("usageAnchorSnapshot returned an aliased pointer")
	}
}
