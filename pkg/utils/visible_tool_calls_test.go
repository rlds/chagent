package utils

import (
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/providers"
)

func TestBuildVisibleToolCalls_DoesNotTruncateExplanation(t *testing.T) {
	explanation := "Read README.md first to confirm the current project structure before editing the config example."
	toolCalls := []providers.ToolCall{{
		ID:   "call_1",
		Type: "function",
		Function: &providers.FunctionCall{
			Name:      "read_file",
			Arguments: `{"path":"README.md","start_line":1,"end_line":10,"extra":"abcdefghijklmnopqrstuvwxyz"}`,
		},
		ExtraContent: &providers.ExtraContent{
			ToolFeedbackExplanation: explanation,
		},
	}}

	visible := BuildVisibleToolCalls(toolCalls, 20)
	if len(visible) != 1 {
		t.Fatalf("len(visible) = %d, want 1", len(visible))
	}
	if visible[0].ExtraContent == nil || visible[0].ExtraContent.ToolFeedbackExplanation != explanation {
		t.Fatalf("visible explanation = %#v, want %q", visible[0].ExtraContent, explanation)
	}
	if visible[0].Function == nil || visible[0].Function.Arguments == "" {
		t.Fatalf("visible function = %#v, want truncated args preview", visible[0].Function)
	}
}

func TestBuildVisibleToolCalls_ZeroMaxKeepsFullArguments(t *testing.T) {
	longValue := strings.Repeat("long-argument-", 200)
	toolCalls := []providers.ToolCall{{
		ID:   "call_long",
		Type: "function",
		Function: &providers.FunctionCall{
			Name:      "exec",
			Arguments: `{"command":"` + longValue + `"}`,
		},
	}}

	visible := BuildVisibleToolCalls(toolCalls, 0)
	if len(visible) != 1 || visible[0].Function == nil {
		t.Fatalf("visible = %#v", visible)
	}
	if !strings.Contains(visible[0].Function.Arguments, longValue) {
		t.Fatalf("zero max should keep full arguments, got len=%d", len(visible[0].Function.Arguments))
	}
}
