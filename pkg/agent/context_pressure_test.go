// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"math"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/providers"
)

func TestContextPressureThreshold(t *testing.T) {
	defaults := DefaultContextPressurePolicy()

	tests := []struct {
		name          string
		policy        ContextPressurePolicy
		contextWindow int
		maxOutput     int
		want          int
	}{
		{
			name:          "defaults window fraction wins",
			policy:        defaults,
			contextWindow: 1_000_000,
			maxOutput:     32_768,
			// min(1_000_000*0.8, 1_000_000-32_768-65_536) = min(800_000, 901_696)
			want: 800_000,
		},
		{
			name:          "defaults headroom cap wins",
			policy:        defaults,
			contextWindow: 131_072,
			maxOutput:     32_768,
			// min(131_072*0.8=104_857, 131_072-32_768-65_536=32_768)
			want: 32_768,
		},
		{
			name:          "defaults zero output reservation",
			policy:        defaults,
			contextWindow: 131_072,
			maxOutput:     0,
			// min(104_857, 131_072-65_536=65_536)
			want: 65_536,
		},
		{
			name:          "defaults negative output reservation treated as zero",
			policy:        defaults,
			contextWindow: 131_072,
			maxOutput:     -5,
			want:          65_536,
		},
		{
			name:          "defaults lower bound guard on tiny window",
			policy:        defaults,
			contextWindow: 1000,
			maxOutput:     0,
			// pressureBudget < 0 → 以 1 为下限，使压力仍可检测
			want: 1,
		},
		{
			name:          "zero window has no threshold",
			policy:        defaults,
			contextWindow: 0,
			maxOutput:     1000,
			want:          0,
		},
		{
			name:          "custom policy without headroom",
			policy:        ContextPressurePolicy{ThresholdRatio: 0.5, RetainRatio: 0.1},
			contextWindow: 10_000,
			maxOutput:     2000,
			// min(10_000*0.5=5000, 10_000-2000-0=8000)
			want: 5000,
		},
		{
			name:          "ratio above one is clamped to full window",
			policy:        ContextPressurePolicy{ThresholdRatio: 1.5, HeadroomTokens: 0},
			contextWindow: 10_000,
			maxOutput:     1000,
			// ratio clamped to 1 → min(10_000*1, 10_000-1000-0) = 9000
			want: 9000,
		},
		{
			name:          "zero value policy falls back to default ratio",
			policy:        ContextPressurePolicy{},
			contextWindow: 10_000,
			maxOutput:     0,
			want:          8000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.Threshold(tt.contextWindow, tt.maxOutput)
			if got != tt.want {
				t.Errorf("Threshold(%d, %d) = %d, want %d",
					tt.contextWindow, tt.maxOutput, got, tt.want)
			}
		})
	}
}

func TestContextPressureRetain(t *testing.T) {
	defaults := DefaultContextPressurePolicy()

	tests := []struct {
		name          string
		policy        ContextPressurePolicy
		contextWindow int
		maxOutput     int
		want          int
	}{
		{
			name:          "defaults scale message budget",
			policy:        defaults,
			contextWindow: 131_072,
			maxOutput:     32_768,
			// floor((131_072-32_768) * 0.16) = floor(15_728.64)
			want: 15_728,
		},
		{
			name:          "defaults small window",
			policy:        defaults,
			contextWindow: 100_000,
			maxOutput:     20_000,
			// floor(80_000 * 0.16) = 12_800
			want: 12_800,
		},
		{
			name:          "custom retain ratio",
			policy:        ContextPressurePolicy{RetainRatio: 0.25},
			contextWindow: 100_000,
			maxOutput:     20_000,
			want:          20_000,
		},
		{
			name:          "retain ratio above one is clamped",
			policy:        ContextPressurePolicy{RetainRatio: 2},
			contextWindow: 10_000,
			maxOutput:     1000,
			want:          9000,
		},
		{
			name:          "output reservation consumes the message budget",
			policy:        defaults,
			contextWindow: 1000,
			maxOutput:     2000,
			want:          0,
		},
		{
			name:          "zero window has no retention",
			policy:        defaults,
			contextWindow: 0,
			maxOutput:     0,
			want:          0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.policy.RetainTokens(tt.contextWindow, tt.maxOutput)
			if got != tt.want {
				t.Errorf("RetainTokens(%d, %d) = %d, want %d",
					tt.contextWindow, tt.maxOutput, got, tt.want)
			}
		})
	}

	// 默认策略必须在现实窗口下满足 DSH 的 retain < threshold 不变量。
	window, maxOutput := 131_072, 32_768
	if retain, threshold := defaults.RetainTokens(window, maxOutput), defaults.Threshold(window, maxOutput); retain >= threshold {
		t.Errorf("default policy retain(%d) must be < threshold(%d)", retain, threshold)
	}
}

func TestContextPressureOver(t *testing.T) {
	// EstimateMessageTokens(user "a"*2000) = (2000+12)*2/5 = 804 token。
	bigUser := msgUser(strings.Repeat("a", 2000))
	policy := ContextPressurePolicy{ThresholdRatio: 0.5, RetainRatio: 0.1}

	pr := policy.Pressure([]providers.Message{bigUser}, 100, 50, 1000, 0)
	if pr.UsedTokens != 954 {
		t.Errorf("UsedTokens = %d, want 954 (100 system + 50 tools + 804 message)", pr.UsedTokens)
	}
	if pr.Threshold != 500 {
		t.Errorf("Threshold = %d, want 500", pr.Threshold)
	}
	if !pr.Over {
		t.Errorf("Over = false, want true (used %d >= threshold %d)", pr.UsedTokens, pr.Threshold)
	}
	if math.Abs(pr.Pct-95.4) > 1e-9 {
		t.Errorf("Pct = %f, want 95.4", pr.Pct)
	}

	// 纯测量不得修改输入。
	if got := bigUser.Content; len(got) != 2000 {
		t.Errorf("input message content mutated: len = %d, want 2000", len(got))
	}
}

func TestContextPressureUnder(t *testing.T) {
	policy := ContextPressurePolicy{ThresholdRatio: 0.8, RetainRatio: 0.1}
	pr := policy.Pressure(nil, 100, 50, 10_000, 0)

	if pr.UsedTokens != 150 {
		t.Errorf("UsedTokens = %d, want 150", pr.UsedTokens)
	}
	if pr.Threshold != 8000 {
		t.Errorf("Threshold = %d, want 8000", pr.Threshold)
	}
	if pr.Over {
		t.Errorf("Over = true, want false (used %d < threshold %d)", pr.UsedTokens, pr.Threshold)
	}
	if math.Abs(pr.Pct-1.5) > 1e-9 {
		t.Errorf("Pct = %f, want 1.5", pr.Pct)
	}

	// 同一测量执行两次必须完全一致（无隐藏状态）。
	if again := policy.Pressure(nil, 100, 50, 10_000, 0); again != pr {
		t.Errorf("Pressure is not deterministic: %+v != %+v", again, pr)
	}
}
