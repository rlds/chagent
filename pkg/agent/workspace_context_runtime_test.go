package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/agent/workspacecontext"
	"github.com/rlds/chagent/pkg/session"
)

// TestInjectWorkspaceContextPersistsDurableMessage locks the T2.2 seam: the
// baseline is appended before the current user message and stored in session
// history with workspace-instruction provenance.
func TestInjectWorkspaceContextPersistsDurableMessage(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte("# 规则\n\n始终使用中文回复。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	projector, err := workspacecontext.NewProjector(workspacecontext.DefaultConfig(), workspace, "")
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	sessions := session.NewSessionManager("")
	agent := &AgentInstance{WorkspaceContext: projector, Sessions: sessions}
	ts := &turnState{agent: agent, workspace: workspace, sessionKey: "s1"}

	history := ts.injectWorkspaceContext(context.Background(), &AgentLoop{}, nil, nil)
	if len(history) != 1 {
		t.Fatalf("history length = %d, want one baseline", len(history))
	}
	msg := history[0]
	if msg.Context == nil || msg.Context.Kind != "workspace-instructions" || !msg.Context.Baseline {
		t.Fatalf("baseline metadata = %+v", msg.Context)
	}
	if !strings.Contains(msg.Content, "始终使用中文回复") {
		t.Fatalf("baseline missing instruction body:\n%s", msg.Content)
	}
	if !strings.Contains(msg.Content, "来自以下文件的指令：AGENTS.md") {
		t.Fatalf("baseline missing Chinese section header:\n%s", msg.Content)
	}
	if got := sessions.GetHistory("s1"); len(got) != 1 || got[0].Content != msg.Content {
		t.Fatalf("baseline was not persisted: %#v", got)
	}

	// A second compose over the same history must not duplicate the baseline.
	again := ts.injectWorkspaceContext(context.Background(), &AgentLoop{}, history, nil)
	if len(again) != 1 {
		t.Fatalf("second compose duplicated baseline: %d messages", len(again))
	}
}

func TestInjectRuntimeContextSnapshotPersistsAndDeduplicates(t *testing.T) {
	workspace := t.TempDir()
	sessions := session.NewSessionManager("")
	agent := &AgentInstance{
		Sessions:               sessions,
		Workspace:              workspace,
		RuntimeContextMode:     "snapshot",
		RuntimeContextInterval: time.Hour,
		RuntimeContextSender:   true,
	}
	ts := &turnState{
		agent:      agent,
		workspace:  workspace,
		sessionKey: "runtime-1",
		channel:    "pico",
		chatID:     "chat",
		opts:       processOptions{SenderID: "u1", SenderDisplayName: "张三"},
	}
	history := ts.injectRuntimeContext(context.Background(), &AgentLoop{}, nil)
	if len(history) != 1 {
		t.Fatalf("runtime history = %d, want 1", len(history))
	}
	if !strings.Contains(history[0].Content, "当前时间：") || !strings.Contains(history[0].Content, "当前发送者：张三") {
		t.Fatalf("runtime snapshot missing Chinese facts:\n%s", history[0].Content)
	}
	again := ts.injectRuntimeContext(context.Background(), &AgentLoop{}, history)
	if len(again) != 1 {
		t.Fatalf("runtime snapshot duplicated within interval: %d", len(again))
	}
}
