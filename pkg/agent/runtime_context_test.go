package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/taskstate"
)

func TestBuildRuntimeSnapshotChinese(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 30, 0, 0, time.UTC)
	text := BuildRuntimeSnapshot("pico", "chat-1", "u1", "张三", "/ws", "/ws", true, now)
	for _, want := range []string{"<runtime_context>", "当前时间：", "工作区：", "会话：通道=pico", "当前发送者：张三", "</runtime_context>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("snapshot missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Current time") {
		t.Fatalf("snapshot still contains English runtime header:\n%s", text)
	}
}

func TestRuntimeSnapshotDigestIgnoresTime(t *testing.T) {
	a := RuntimeSnapshotDigest("pico", "chat", "u", "n", "/ws", "/ws", true)
	b := RuntimeSnapshotDigest("pico", "chat", "u", "n", "/ws", "/ws", true)
	if a != b {
		t.Fatal("digest should be stable for identical facts")
	}
	if a == RuntimeSnapshotDigest("pico", "chat2", "u", "n", "/ws", "/ws", true) {
		t.Fatal("digest should change when chat changes")
	}
}

func TestSnapshotModeKeepsRuntimeOutOfSystemPrompt(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.RuntimeContext.Mode = config.RuntimeContextModeSnapshot
	cb := NewContextBuilder(t.TempDir()).WithRuntimeContextMode(cfg.Agents.Defaults.RuntimeContext.Mode)
	prompt := cb.BuildSystemPrompt()
	if strings.Contains(prompt, "## 当前时间") || strings.Contains(prompt, "当前发送者") {
		t.Fatalf("snapshot mode still renders runtime into system prompt:\n%s", prompt)
	}
}

func TestAdvanceAutonomyRoundNoProgressBlocks(t *testing.T) {
	now := time.Now()
	state, err := taskstate.New("完成测试", 20, now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultAutonomyConfig()
	cfg.NoProgressRounds = 1

	state, schedule := advanceAutonomyRound(state, cfg, now)
	if !schedule || state.Round != 1 {
		t.Fatalf("first round not scheduled: %+v", state)
	}
	// Simulate a round that advanced time but never marked progress.
	state.UpdatedAt = now.Add(time.Minute)
	state, schedule = advanceAutonomyRound(state, cfg, now.Add(time.Minute))
	if schedule {
		t.Fatalf("no-progress round should have been blocked: %+v", state)
	}
	if state.Phase != taskstate.PhaseBlocked {
		t.Fatalf("phase = %s, want blocked", state.Phase)
	}
}

func TestAdvanceAutonomyRoundProgressContinues(t *testing.T) {
	now := time.Now()
	state, err := taskstate.New("完成测试", 20, now)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultAutonomyConfig()
	state, schedule := advanceAutonomyRound(state, cfg, now)
	if !schedule {
		t.Fatal("first round not scheduled")
	}
	state.MarkProgress(now.Add(time.Second))
	state, schedule = advanceAutonomyRound(state, cfg, now.Add(time.Minute))
	if !schedule || state.Round != 2 {
		t.Fatalf("progress round not scheduled: %+v", state)
	}
	if state.NoProgressRounds != 0 {
		t.Fatalf("NoProgressRounds = %d, want 0", state.NoProgressRounds)
	}
}

func TestPauseTaskStateForStop(t *testing.T) {
	sessions := session.NewSessionManager("")
	store := sessions
	state, err := taskstate.New("持续任务", 20, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTaskState("s1", state); err != nil {
		t.Fatal(err)
	}
	al := &AgentLoop{}
	al.pauseTaskStateForStop(&AgentInstance{Sessions: sessions}, "s1")
	got, ok := store.GetTaskState("s1")
	if !ok || got.Phase != taskstate.PhasePaused {
		t.Fatalf("task not paused: %+v", got)
	}
}
