package protocoltypes

import (
	"encoding/json"
	"testing"
	"time"
)

// TestMessageContextJSONRoundTrip verifies that ContextMeta survives a JSON
// round trip (session/JSONL persistence) with every field intact.
func TestMessageContextJSONRoundTrip(t *testing.T) {
	created := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	msg := Message{
		Role:      "user",
		Content:   "<system-reminder>context</system-reminder>",
		CreatedAt: &created,
		Context: &ContextMeta{
			Kind:             ContextKindWorkspaceInstructions,
			Form:             ContextFormReminder,
			Baseline:         true,
			BaselineIdentity: "chain:abc123",
			Changes: []ContextChange{
				{Action: ContextActionSet, Scope: "project", Path: "AGENTS.md", Digest: "sha256:deadbeef"},
				{Action: ContextActionReplace, Scope: "project", Path: "packages/app/AGENTS.md", Digest: "sha256:cafebabe"},
			},
			Sections: []ContextSection{
				{Name: "project", Text: "project instructions"},
			},
		},
	}

	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded Message
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.Context == nil {
		t.Fatal("Context = nil after round trip")
	}
	if decoded.Context.Kind != ContextKindWorkspaceInstructions {
		t.Errorf("Context.Kind = %q, want %q", decoded.Context.Kind, ContextKindWorkspaceInstructions)
	}
	if decoded.Context.Form != ContextFormReminder {
		t.Errorf("Context.Form = %q, want %q", decoded.Context.Form, ContextFormReminder)
	}
	if !decoded.Context.Baseline {
		t.Error("Context.Baseline = false, want true")
	}
	if decoded.Context.BaselineIdentity != "chain:abc123" {
		t.Errorf("Context.BaselineIdentity = %q", decoded.Context.BaselineIdentity)
	}
	if len(decoded.Context.Changes) != 2 {
		t.Fatalf("len(Context.Changes) = %d, want 2", len(decoded.Context.Changes))
	}
	if decoded.Context.Changes[1].Action != ContextActionReplace {
		t.Errorf("Changes[1].Action = %q, want %q", decoded.Context.Changes[1].Action, ContextActionReplace)
	}
	if len(decoded.Context.Sections) != 1 || decoded.Context.Sections[0].Name != "project" {
		t.Errorf("Context.Sections = %#v", decoded.Context.Sections)
	}
	if decoded.Role != "user" || decoded.Content != msg.Content {
		t.Errorf("message body changed: role=%q content=%q", decoded.Role, decoded.Content)
	}
}

// TestOldSessionWithoutContextLoads verifies that JSON persisted by an older
// version (no context field) still decodes and is treated as a normal message.
func TestOldSessionWithoutContextLoads(t *testing.T) {
	raw := []byte(`{"role":"user","content":"hello from old session","model_name":"glm-4.7"}`)

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("json.Unmarshal old session message: %v", err)
	}
	if msg.Context != nil {
		t.Fatalf("Context = %#v, want nil for legacy message", msg.Context)
	}
	if msg.IsContextMessage() {
		t.Error("legacy message must not be reported as context message")
	}
	if msg.Role != "user" || msg.Content != "hello from old session" {
		t.Errorf("decoded message = %#v", msg)
	}
}

// TestContextHelpers verifies kind helpers used by turn-boundary and resume logic.
func TestContextHelpers(t *testing.T) {
	tests := []struct {
		name          string
		ctx           *ContextMeta
		wantContext   bool
		wantWorkspace bool
		wantRuntime   bool
		wantTaskState bool
	}{
		{name: "nil context", ctx: nil},
		{name: "empty kind", ctx: &ContextMeta{Form: ContextFormReminder}},
		{
			name:          "workspace instructions",
			ctx:           &ContextMeta{Kind: ContextKindWorkspaceInstructions},
			wantContext:   true,
			wantWorkspace: true,
		},
		{
			name:        "runtime context",
			ctx:         &ContextMeta{Kind: ContextKindRuntimeContext},
			wantContext: true,
			wantRuntime: true,
		},
		{
			name:          "task state",
			ctx:           &ContextMeta{Kind: ContextKindTaskState},
			wantContext:   true,
			wantTaskState: true,
		},
		{
			name:        "task round",
			ctx:         &ContextMeta{Kind: ContextKindTaskRound},
			wantContext: true,
		},
		{
			name:        "skill catalog",
			ctx:         &ContextMeta{Kind: ContextKindSkillCatalog},
			wantContext: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := Message{Role: "user", Content: "x", Context: tt.ctx}
			if got := msg.IsContextMessage(); got != tt.wantContext {
				t.Errorf("IsContextMessage() = %v, want %v", got, tt.wantContext)
			}
			if got := msg.IsWorkspaceInstructions(); got != tt.wantWorkspace {
				t.Errorf("IsWorkspaceInstructions() = %v, want %v", got, tt.wantWorkspace)
			}
			if got := msg.IsRuntimeContext(); got != tt.wantRuntime {
				t.Errorf("IsRuntimeContext() = %v, want %v", got, tt.wantRuntime)
			}
			if got := msg.IsTaskState(); got != tt.wantTaskState {
				t.Errorf("IsTaskState() = %v, want %v", got, tt.wantTaskState)
			}
		})
	}
}

// TestContextKindConstantsAreStable locks the persisted kind strings; these are
// written to session JSON, so changing them would break resume/reconciliation.
func TestContextKindConstantsAreStable(t *testing.T) {
	want := map[string]string{
		"workspace": ContextKindWorkspaceInstructions,
		"runtime":   ContextKindRuntimeContext,
		"taskstate": ContextKindTaskState,
		"taskround": ContextKindTaskRound,
		"skills":    ContextKindSkillCatalog,
	}
	if want["workspace"] != "workspace-instructions" {
		t.Errorf("ContextKindWorkspaceInstructions = %q", want["workspace"])
	}
	if want["runtime"] != "runtime-context" {
		t.Errorf("ContextKindRuntimeContext = %q", want["runtime"])
	}
	if want["taskstate"] != "task-state" {
		t.Errorf("ContextKindTaskState = %q", want["taskstate"])
	}
	if want["taskround"] != "task-round" {
		t.Errorf("ContextKindTaskRound = %q", want["taskround"])
	}
	if want["skills"] != "skill-catalog" {
		t.Errorf("ContextKindSkillCatalog = %q", want["skills"])
	}
}
