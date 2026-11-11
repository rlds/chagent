package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/taskstate"
)

// =============================================================================
// 自主闭环 E2E 测试使用的脚本化 provider 与辅助函数。
// =============================================================================

type autonomyLLMStep struct {
	content   string
	toolCalls []providers.ToolCall
	err       error
}

type autonomyLLMCall struct {
	messages []providers.Message
}

// scriptedAutonomyProvider 按固定脚本回放响应；脚本耗尽后最后一步会重复，
// 从而让多轮测试保持确定性。
type scriptedAutonomyProvider struct {
	mu    sync.Mutex
	steps []autonomyLLMStep
	calls []autonomyLLMCall
}

func (p *scriptedAutonomyProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	idx := len(p.calls)
	p.calls = append(p.calls, autonomyLLMCall{messages: append([]providers.Message(nil), messages...)})
	step := autonomyLLMStep{}
	if len(p.steps) > 0 {
		if idx >= len(p.steps) {
			idx = len(p.steps) - 1
		}
		step = p.steps[idx]
	}
	p.mu.Unlock()

	if step.err != nil {
		return nil, step.err
	}
	return &providers.LLMResponse{
		Content:   step.content,
		ToolCalls: step.toolCalls,
	}, nil
}

func (p *scriptedAutonomyProvider) GetDefaultModel() string { return "scripted-autonomy-model" }

func (p *scriptedAutonomyProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *scriptedAutonomyProvider) callMessages(call int) []providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	if call < 0 || call >= len(p.calls) {
		return nil
	}
	return p.calls[call].messages
}

func newAutonomyTestLoop(
	t *testing.T,
	provider providers.LLMProvider,
	autonomy config.AutonomyConfig,
) (*AgentLoop, *config.Config, *bus.MessageBus) {
	t.Helper()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
				Autonomy:          autonomy,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	t.Cleanup(func() {
		al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)
	})
	return al, cfg, msgBus
}

func defaultAutonomyConfig() config.AutonomyConfig {
	return config.AutonomyConfig{
		Enabled:          true,
		MaxRounds:        20,
		RoundDelayMS:     0,
		NoProgressRounds: 3,
	}
}

func autonomyAgentLoop(t *testing.T, al *AgentLoop) (*AgentInstance, taskstate.Store) {
	t.Helper()
	registry := al.GetRegistry()
	agent := registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected a default agent")
	}
	store, ok := agent.Sessions.(taskstate.Store)
	if !ok {
		t.Fatal("default agent session store does not implement taskstate.Store")
	}
	return agent, store
}

func autonomySessionKey(agent *AgentInstance, suffix string) string {
	return "agent:" + agent.ID + ":" + suffix
}

func seedAutonomyState(t *testing.T, store taskstate.Store, sessionKey string, state taskstate.State) {
	t.Helper()
	if err := store.SetTaskState(sessionKey, state); err != nil {
		t.Fatalf("SetTaskState(%q) failed: %v", sessionKey, err)
	}
}

func newAutonomyState(t *testing.T, objective string, maxRounds int) taskstate.State {
	t.Helper()
	now := time.Now()
	state, err := taskstate.New(objective, maxRounds, now)
	if err != nil {
		t.Fatalf("taskstate.New failed: %v", err)
	}
	return state
}

func autonomyTurnState(
	al *AgentLoop,
	agent *AgentInstance,
	sessionKey, channel, chatID string,
	turnSource string,
) *turnState {
	opts := processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  sessionKey,
			UserMessage: "继续推进任务",
			InboundContext: &bus.InboundContext{
				Channel:  channel,
				ChatID:   chatID,
				ChatType: "direct",
			},
		},
		SendResponse: false,
		TurnSource:   turnSource,
	}
	scope := al.newTurnEventScope(agent.ID, sessionKey, nil)
	return newTurnState(agent, opts, scope)
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func subscribeAutonomyEvents(
	t *testing.T,
	al *AgentLoop,
) (<-chan runtimeevents.Event, func()) {
	t.Helper()
	sub, ch, err := al.RuntimeEventBus().
		Channel().
		OfKind(runtimeevents.KindAgentAutonomyRound).
		SubscribeChan(context.Background(), runtimeevents.SubscribeOptions{Name: "autonomy-e2e", Buffer: 64})
	if err != nil {
		t.Fatalf("subscribe autonomy events failed: %v", err)
	}
	return ch, func() { _ = sub.Close() }
}

func receiveAutonomyEvent(
	t *testing.T,
	ch <-chan runtimeevents.Event,
	timeout time.Duration,
) (AutonomyRoundPayload, bool) {
	t.Helper()
	select {
	case evt := <-ch:
		payload, ok := evt.Payload.(AutonomyRoundPayload)
		if !ok {
			t.Fatalf("unexpected payload type %T", evt.Payload)
		}
		return payload, true
	case <-time.After(timeout):
		return AutonomyRoundPayload{}, false
	}
}

func receiveOutbound(t *testing.T, msgBus *bus.MessageBus, timeout time.Duration) (bus.OutboundMessage, bool) {
	t.Helper()
	select {
	case msg := <-msgBus.OutboundChan():
		return msg, true
	case <-time.After(timeout):
		return bus.OutboundMessage{}, false
	}
}

// =============================================================================
// 调度与守卫测试
// =============================================================================

func TestAutonomySchedulesNextRound(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "继续推进"}}}
	al, _, _ := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "schedule-next")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "完成自主续跑", 3))
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-schedule", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})
	// RoundDelayMS=0 时轮次可能立即开始，因此观察计数器而不是瞬态的待处理槽位。
	waitUntil(t, 5*time.Second, "first autonomy round to run", func() bool {
		state, ok := store.GetTaskState(sessionKey)
		return ok && (state.Round >= 1 || provider.callCount() >= 1)
	})

	// 连续三轮没有进展会触发无进展守卫并终止。
	waitUntil(t, 5*time.Second, "task to become terminal", func() bool {
		state, ok := store.GetTaskState(sessionKey)
		return ok && state.Phase != taskstate.PhaseActive
	})

	state, _ := store.GetTaskState(sessionKey)
	if state.Round < 2 {
		t.Fatalf("round = %d, want at least 2 rounds scheduled", state.Round)
	}
	if state.Phase != taskstate.PhaseBlocked {
		t.Fatalf("phase = %s, want blocked", state.Phase)
	}
	if state.BlockedReason == nil {
		t.Fatalf("blocked reason = %+v, want a driver guard reason", state.BlockedReason)
	}
	switch state.BlockedReason.Code {
	case taskstate.BlockedCodeNoProgress, taskstate.BlockedCodeRoundLimit:
		// 两个驱动守卫都是合法的终止原因；先触发哪个取决于配置的 max_rounds / no_progress_rounds 顺序。
	default:
		t.Fatalf("blocked reason = %+v, want no-progress or round-limit", state.BlockedReason)
	}
	if got := provider.callCount(); got < 2 {
		t.Fatalf("provider calls = %d, want at least 2 autonomous rounds", got)
	}
}

func TestAutonomyStopsOnNoProgress(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "没有实际进展"}}}
	cfg := defaultAutonomyConfig()
	cfg.NoProgressRounds = 1
	al, _, msgBus := newAutonomyTestLoop(t, provider, cfg)
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "no-progress")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "无进展任务", 10))
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-no-progress", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})

	waitUntil(t, 5*time.Second, "no-progress block", func() bool {
		state, ok := store.GetTaskState(sessionKey)
		return ok && state.Phase != taskstate.PhaseActive
	})
	state, _ := store.GetTaskState(sessionKey)
	if state.Phase != taskstate.PhaseBlocked {
		t.Fatalf("phase = %s, want blocked", state.Phase)
	}
	if state.BlockedReason == nil || state.BlockedReason.Code != taskstate.BlockedCodeNoProgress {
		t.Fatalf("blocked reason = %+v, want no-progress", state.BlockedReason)
	}
	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("no-progress block must not queue another round")
	}
	msg, ok := receiveOutbound(t, msgBus, time.Second)
	if !ok {
		t.Fatal("expected a final summary for the no-progress block")
	}
	if !strings.Contains(msg.Content, taskstate.BlockedCodeNoProgress) {
		t.Fatalf("summary = %q, want no-progress reason", msg.Content)
	}
}

func TestAutonomyStopsOnComplete(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "不应调用"}}}
	al, _, msgBus := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "stops-complete")
	state := newAutonomyState(t, "整理发布说明", 5)
	if err := state.Complete([]string{"docs/release-notes.md 已更新", "go test ./pkg/agent 通过"}, time.Now()); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	seedAutonomyState(t, store, sessionKey, state)
	events, stop := subscribeAutonomyEvents(t, al)
	defer stop()
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-complete", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})

	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("completed task must not schedule another round")
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0 for a completed task", got)
	}
	msg, ok := receiveOutbound(t, msgBus, time.Second)
	if !ok {
		t.Fatal("expected a final summary outbound message")
	}
	for _, want := range []string{"任务已完成", "整理发布说明", "证据", "release-notes.md"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("summary missing %q:\n%s", want, msg.Content)
		}
	}
	payload, ok := receiveAutonomyEvent(t, events, time.Second)
	if !ok {
		t.Fatal("expected an ended autonomy round event")
	}
	if payload.Phase != AutonomyRoundPhaseEnded || payload.Reason != AutonomyRoundReasonCompleted {
		t.Fatalf("event payload = %+v, want ended/completed", payload)
	}
	if payload.Objective != "整理发布说明" {
		t.Fatalf("event objective = %q", payload.Objective)
	}
}

func TestAutonomyStopsOnBlocked(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "不应调用"}}}
	al, _, msgBus := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "stops-blocked")
	state := newAutonomyState(t, "部署到生产环境", 5)
	if err := state.ForceBlock("model-reported", "缺少生产环境凭据", time.Now()); err != nil {
		t.Fatalf("ForceBlock failed: %v", err)
	}
	seedAutonomyState(t, store, sessionKey, state)
	events, stop := subscribeAutonomyEvents(t, al)
	defer stop()
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-blocked", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})

	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("blocked task must not schedule another round")
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0 for a blocked task", got)
	}
	msg, ok := receiveOutbound(t, msgBus, time.Second)
	if !ok {
		t.Fatal("expected a final summary outbound message")
	}
	for _, want := range []string{"任务已阻塞", "部署到生产环境", "model-reported", "缺少生产环境凭据"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("summary missing %q:\n%s", want, msg.Content)
		}
	}
	payload, ok := receiveAutonomyEvent(t, events, time.Second)
	if !ok {
		t.Fatal("expected an ended autonomy round event")
	}
	if payload.Reason != AutonomyRoundReasonBlocked {
		t.Fatalf("event reason = %q, want blocked", payload.Reason)
	}
}

func TestAutonomyStopsOnRoundLimit(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "不应调用"}}}
	al, _, msgBus := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "stops-round-limit")
	state := newAutonomyState(t, "跑完所有轮次", 2)
	state.Round = 2
	seedAutonomyState(t, store, sessionKey, state)
	events, stop := subscribeAutonomyEvents(t, al)
	defer stop()
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-limit", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})

	if al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("round-limited task must not schedule another round")
	}
	got, _ := store.GetTaskState(sessionKey)
	if got.Phase != taskstate.PhaseBlocked {
		t.Fatalf("phase = %s, want blocked", got.Phase)
	}
	if got.BlockedReason == nil || got.BlockedReason.Code != taskstate.BlockedCodeRoundLimit {
		t.Fatalf("blocked reason = %+v, want round-limit", got.BlockedReason)
	}
	msg, ok := receiveOutbound(t, msgBus, time.Second)
	if !ok {
		t.Fatal("expected a final summary outbound message")
	}
	for _, want := range []string{"任务已阻塞", "round-limit", "跑完所有轮次"} {
		if !strings.Contains(msg.Content, want) {
			t.Fatalf("summary missing %q:\n%s", want, msg.Content)
		}
	}
	payload, ok := receiveAutonomyEvent(t, events, time.Second)
	if !ok {
		t.Fatal("expected an ended autonomy round event")
	}
	if payload.Reason != AutonomyRoundReasonRoundLimit {
		t.Fatalf("event reason = %q, want round-limit", payload.Reason)
	}
	if payload.Round != 2 || payload.MaxRounds != 2 {
		t.Fatalf("event round = %d/%d, want 2/2", payload.Round, payload.MaxRounds)
	}
}

func TestAutonomyRespectsUserStop(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "用户消息处理完成"}}}
	cfg := defaultAutonomyConfig()
	cfg.RoundDelayMS = 5000 // 延迟轮次在测试期间绝不能触发
	al, _, _ := newAutonomyTestLoop(t, provider, cfg)
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "user-stop")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "等待用户决定", 10))
	events, stop := subscribeAutonomyEvents(t, al)
	defer stop()
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-stop", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})
	if !al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("expected a delayed autonomy round")
	}

	// 通过正常入站路径投递一条真实用户消息：它必须在轮次 goroutine 触发前取消延迟轮次。
	if _, _, _, err := al.processMessage(context.Background(), bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:  "pico",
			ChatID:   "chat-stop",
			ChatType: "direct",
			SenderID: "user-1",
		},
		SessionKey: sessionKey,
		Content:    "先停下来处理这个",
	}); err != nil {
		t.Fatalf("processMessage failed: %v", err)
	}

	// 新的用户回合可以合法地为仍活跃的任务调度一个新的后续轮次，因此不变量是：
	// 用户消息之前排队的延迟轮次没有触发，且用户回合确实执行了。
	if got := provider.callCount(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 (user turn only)", got)
	}

	sawUserStop := false
	deadline := time.After(2 * time.Second)
	for !sawUserStop {
		select {
		case evt := <-events:
			payload, ok := evt.Payload.(AutonomyRoundPayload)
			if ok && payload.Phase == AutonomyRoundPhaseEnded && payload.Reason == AutonomyRoundReasonUserStop {
				sawUserStop = true
			}
		case <-deadline:
			t.Fatal("expected an ended/user-stop autonomy event")
		}
	}
}

func TestAutonomyNoDuplicateRound(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "不应调用"}}}
	cfg := defaultAutonomyConfig()
	cfg.RoundDelayMS = 5000
	al, _, _ := newAutonomyTestLoop(t, provider, cfg)
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "no-duplicate")
	state := newAutonomyState(t, "只调度一轮", 10)
	seedAutonomyState(t, store, sessionKey, state)
	events, stop := subscribeAutonomyEvents(t, al)
	defer stop()
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-dup", autonomyTurnSource)
	opts := ts.opts
	opts.Dispatch.UserMessage = renderTaskRoundPrompt(state)

	al.scheduleAutonomyRound(context.Background(), agent, opts, cfg, state, autonomyRoundSourceDriver)
	al.scheduleAutonomyRound(context.Background(), agent, opts, cfg, state, autonomyRoundSourceDriver)

	// 已有轮次排队时，第二次驱动决策同样必须是空操作。
	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})
	persisted, _ := store.GetTaskState(sessionKey)
	if persisted.Round != 0 {
		t.Fatalf("round = %d, want 0 because the duplicate scheduling was rejected", persisted.Round)
	}

	scheduled := 0
	deadline := time.After(300 * time.Millisecond)
collect:
	for {
		select {
		case evt := <-events:
			payload, ok := evt.Payload.(AutonomyRoundPayload)
			if ok && payload.Phase == AutonomyRoundPhaseScheduled {
				scheduled++
			}
		case <-deadline:
			break collect
		}
	}
	if scheduled != 1 {
		t.Fatalf("scheduled events = %d, want exactly 1", scheduled)
	}
	if got := provider.callCount(); got != 0 {
		t.Fatalf("provider calls = %d, want 0 (delayed round must not fire)", got)
	}
}

func TestAutonomyRoundEvent(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "轮次结束"}}}
	al, _, _ := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "round-event")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "观测自主轮次事件", 1))
	events, stop := subscribeAutonomyEvents(t, al)
	defer stop()
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-event", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})

	phases := make(map[AutonomyRoundPhase]AutonomyRoundPayload)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (phases[AutonomyRoundPhaseScheduled].Round == 0 ||
		phases[AutonomyRoundPhaseEnded].Round == 0) {
		payload, ok := receiveAutonomyEvent(t, events, 200*time.Millisecond)
		if !ok {
			continue
		}
		phases[payload.Phase] = payload
	}

	scheduled, ok := phases[AutonomyRoundPhaseScheduled]
	if !ok || scheduled.Round != 1 {
		t.Fatalf("scheduled payload = %+v, want round 1", scheduled)
	}
	if scheduled.Source != autonomyRoundSourceDriver {
		t.Fatalf("scheduled source = %q, want %q", scheduled.Source, autonomyRoundSourceDriver)
	}
	started, ok := phases[AutonomyRoundPhaseStarted]
	if !ok || started.Round != 1 {
		t.Fatalf("started payload = %+v, want round 1", started)
	}
	ended, ok := phases[AutonomyRoundPhaseEnded]
	if !ok {
		t.Fatal("missing ended event")
	}
	if ended.Reason != AutonomyRoundReasonRoundLimit {
		t.Fatalf("ended reason = %q, want round-limit", ended.Reason)
	}
	if ended.Objective != "观测自主轮次事件" {
		t.Fatalf("ended objective = %q", ended.Objective)
	}
	if ended.MaxRounds != 1 {
		t.Fatalf("ended max rounds = %d, want 1", ended.MaxRounds)
	}
}

// =============================================================================
// 回合协调器测试：空响应 / 迭代上限 / provider 错误
// =============================================================================

func TestAutonomyEmptyResponseContinues(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{
		{content: ""}, // 任务仍活跃时出现空响应
		{content: "任务仍在推进，下一步是运行测试。"},
	}}
	cfg := defaultAutonomyConfig()
	cfg.RoundDelayMS = 5000 // 避免后续轮次与断言产生竞争
	al, _, _ := newAutonomyTestLoop(t, provider, cfg)
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "empty-response")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "空响应也要继续", 10))

	resp, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  sessionKey,
			UserMessage: "开始",
			InboundContext: &bus.InboundContext{
				Channel:  "pico",
				ChatID:   "chat-empty",
				ChatType: "direct",
			},
		},
		SendResponse: false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}
	if got := provider.callCount(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (empty response must trigger a continuation)", got)
	}
	if !strings.Contains(resp, "任务仍在推进") {
		t.Fatalf("final response = %q, want the second (non-empty) answer", resp)
	}
	secondMessages := provider.callMessages(1)
	found := false
	for _, msg := range secondMessages {
		if strings.Contains(msg.Content, "<task_round>") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("continuation call did not receive the task-round reminder")
	}
	al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)
}

func TestAutonomyMaxIterationsSchedulesNext(t *testing.T) {
	toolCall := providers.ToolCall{
		ID:        "call-1",
		Name:      "no_such_tool",
		Arguments: map[string]any{},
	}
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{toolCalls: []providers.ToolCall{toolCall}}}}
	cfg := defaultAutonomyConfig()
	cfg.RoundDelayMS = 5000 // 让后续轮次保持待处理，以便断言
	al, _, _ := newAutonomyTestLoop(t, provider, cfg)
	agent, store := autonomyAgentLoop(t, al)
	agent.MaxIterations = 1

	sessionKey := autonomySessionKey(agent, "iteration-limit")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "工具调用达到上限", 10))

	if _, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  sessionKey,
			UserMessage: "执行长任务",
			InboundContext: &bus.InboundContext{
				Channel:  "pico",
				ChatID:   "chat-limit-turn",
				ChatType: "direct",
			},
		},
		SendResponse: false,
	}); err != nil {
		t.Fatalf("runAgentLoop failed: %v", err)
	}

	state, ok := store.GetTaskState(sessionKey)
	if !ok {
		t.Fatal("task state missing")
	}
	if state.Phase != taskstate.PhaseActive {
		t.Fatalf("phase = %s, want active after an iteration-limit checkpoint", state.Phase)
	}
	if state.Round != 1 {
		t.Fatalf("round = %d, want 1 (next round scheduled, checkpoint did not advance twice)", state.Round)
	}
	hasCheckpoint := false
	for _, item := range state.Evidence {
		if strings.HasPrefix(item, autonomyEvidenceIterationPrefix) {
			hasCheckpoint = true
			break
		}
	}
	if !hasCheckpoint {
		t.Fatalf("evidence = %v, want an iteration-limit checkpoint", state.Evidence)
	}
	if !al.hasPendingAutonomyRound(sessionKey) {
		t.Fatal("expected the next autonomy round to be scheduled after the iteration limit")
	}
	al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)
}

func TestAutonomyProviderErrorCheckpoint(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{
		{content: "先开始任务"},                                  // 用户回合
		{err: errors.New("provider exploded: bad payload")}, // 自主轮次
	}}
	al, _, msgBus := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "provider-error")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "模型错误后恢复", 10))

	if _, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  sessionKey,
			UserMessage: "开始",
			InboundContext: &bus.InboundContext{
				Channel:  "pico",
				ChatID:   "chat-provider-error",
				ChatType: "direct",
			},
		},
		SendResponse: false,
	}); err != nil {
		t.Fatalf("initial runAgentLoop failed: %v", err)
	}

	waitUntil(t, 5*time.Second, "provider-error checkpoint", func() bool {
		state, ok := store.GetTaskState(sessionKey)
		if !ok {
			return false
		}
		for _, item := range state.Evidence {
			if strings.HasPrefix(item, autonomyEvidenceProviderPrefix) {
				return true
			}
		}
		return false
	})

	state, _ := store.GetTaskState(sessionKey)
	if state.Phase != taskstate.PhaseActive {
		t.Fatalf("phase = %s, provider errors must not silently complete the task", state.Phase)
	}
	if state.BlockedReason != nil {
		t.Fatalf("blocked reason = %+v, want none for a provider error checkpoint", state.BlockedReason)
	}
	msg, ok := receiveOutbound(t, msgBus, 2*time.Second)
	if !ok {
		t.Fatal("expected a recovery hint outbound message")
	}
	if !strings.Contains(msg.Content, "可从上一检查点恢复") {
		t.Fatalf("recovery message = %q", msg.Content)
	}
}

// =============================================================================
// 中间内容可见性与 resume
// =============================================================================

func TestAutonomyIntermediatesNotSent(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "这是中间轮内容"}}}
	al, _, msgBus := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)

	sessionKey := autonomySessionKey(agent, "intermediates")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "中间轮不外发", 1))
	ts := autonomyTurnState(al, agent, sessionKey, "pico", "chat-intermediate", autonomyTurnSource)

	al.maybeScheduleAutonomyRound(context.Background(), agent, ts, turnResult{status: TurnEndStatusCompleted})

	waitUntil(t, 5*time.Second, "task to become terminal", func() bool {
		state, ok := store.GetTaskState(sessionKey)
		return ok && state.Phase != taskstate.PhaseActive
	})

	var outbounds []bus.OutboundMessage
	deadline := time.After(300 * time.Millisecond)
collect:
	for {
		select {
		case msg := <-msgBus.OutboundChan():
			outbounds = append(outbounds, msg)
		case <-deadline:
			break collect
		}
	}
	if len(outbounds) == 0 {
		t.Fatal("expected the final summary outbound message")
	}
	for _, msg := range outbounds {
		if strings.Contains(msg.Content, "这是中间轮内容") {
			t.Fatalf("intermediate round content must not be sent when SendIntermediate=false:\n%s", msg.Content)
		}
	}
	if !strings.Contains(outbounds[0].Content, "任务已阻塞") {
		t.Fatalf("final summary = %q, want the blocked summary", outbounds[0].Content)
	}
}

func TestAutonomyResumeOnStart(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{{content: "恢复后继续"}}}
	cfg := defaultAutonomyConfig()
	cfg.ResumeOnStart = true
	al, cfgRef, _ := newAutonomyTestLoop(t, provider, cfg)
	agent, store := autonomyAgentLoop(t, al)
	if !cfgRef.Agents.Defaults.Autonomy.ResumeOnStart {
		t.Fatal("resume_on_start not applied to config")
	}

	sessionKey := autonomySessionKey(agent, "resume-on-start")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "重启后恢复任务", 1))
	agent.Sessions.AddMessage(sessionKey, "user", "任务进度记录")
	events, stop := subscribeAutonomyEvents(t, al)
	defer stop()

	if got := al.resumeAutonomyTasksOnStart(context.Background()); got != 1 {
		t.Fatalf("resumeAutonomyTasksOnStart = %d, want 1", got)
	}

	waitUntil(t, 5*time.Second, "resumed round to run and finish", func() bool {
		state, ok := store.GetTaskState(sessionKey)
		return ok && state.Phase != taskstate.PhaseActive
	})
	if provider.callCount() == 0 {
		t.Fatal("resumed task did not run an autonomy round")
	}

	sawResumeEvent := false
	deadline := time.After(300 * time.Millisecond)
drain:
	for {
		select {
		case evt := <-events:
			payload, ok := evt.Payload.(AutonomyRoundPayload)
			if ok && payload.Phase == AutonomyRoundPhaseScheduled && payload.Source == autonomyRoundSourceResume {
				sawResumeEvent = true
			}
		case <-deadline:
			break drain
		}
	}
	if !sawResumeEvent {
		t.Fatal("expected a scheduled autonomy event with source=resume")
	}
	final, _ := store.GetTaskState(sessionKey)
	if final.BlockedReason == nil || final.BlockedReason.Code != taskstate.BlockedCodeRoundLimit {
		t.Fatalf("final state = %+v, want round-limit block after the resumed round", final)
	}
}
