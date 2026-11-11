package common

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// TestSerializeMessagesIgnoresContextMeta verifies that runtime-injected
// context metadata never leaks into the OpenAI-compatible wire payload.
// Providers must keep sending "one system message + history"; the context
// field is a session-only concern.
func TestSerializeMessagesIgnoresContextMeta(t *testing.T) {
	messages := []Message{
		{Role: "system", Content: "system prompt"},
		{
			Role:    "user",
			Content: "<system-reminder>workspace instructions</system-reminder>",
			Context: &protocoltypes.ContextMeta{
				Kind:             protocoltypes.ContextKindWorkspaceInstructions,
				Form:             protocoltypes.ContextFormReminder,
				Baseline:         true,
				BaselineIdentity: "identity",
				Changes: []protocoltypes.ContextChange{
					{Action: protocoltypes.ContextActionSet, Scope: "project", Path: "AGENTS.md", Digest: "d"},
				},
				Sections: []protocoltypes.ContextSection{{Name: "project", Text: "text"}},
			},
		},
		{Role: "assistant", Content: "reply"},
	}

	wire := SerializeMessages(messages)
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(wire): %v", err)
	}

	if strings.Contains(string(raw), `"context"`) {
		t.Fatalf("wire payload contains context field: %s", raw)
	}
	if strings.Contains(string(raw), "baseline_identity") || strings.Contains(string(raw), "system_reminder") {
		t.Fatalf("wire payload leaks context metadata: %s", raw)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal wire: %v", err)
	}
	if len(decoded) != 3 {
		t.Fatalf("len(wire) = %d, want 3", len(decoded))
	}
	if decoded[1]["role"] != "user" {
		t.Errorf("wire[1].role = %v", decoded[1]["role"])
	}
	if decoded[1]["content"] != messages[1].Content {
		t.Errorf("wire[1].content = %v, want original content", decoded[1]["content"])
	}
}

// TestSerializeMessagesKeepsSystemPartsInternal locks that SystemParts, like
// Context, stays out of the provider wire.
func TestSerializeMessagesKeepsSystemPartsInternal(t *testing.T) {
	messages := []Message{
		{
			Role:    "system",
			Content: "prompt",
			SystemParts: []protocoltypes.ContentBlock{
				{Type: "text", Text: "part"},
			},
		},
	}
	raw, err := json.Marshal(SerializeMessages(messages))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(raw), "system_parts") {
		t.Fatalf("wire payload contains system_parts: %s", raw)
	}
}
