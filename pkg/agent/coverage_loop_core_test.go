// Chagent - 超轻量级个人 AI agent
//
// pkg/agent 覆盖率提升（task-39 / OPEN-01）：循环核心（LLM 调用/回合协调/
// 流式发布/工具执行 hook 分支）的错误、取消与边界分支测试。
// 仅测试；不修改生产代码。

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/routing"
	"github.com/rlds/chagent/pkg/tools"
)

// ---------------------------------------------------------------------------
// 流式发布器
// ---------------------------------------------------------------------------

// coverStreamer 实现 bus.Streamer/ReasoningStreamer/LLMUsageStreamer/
// ContextUsageStreamer 以及可选 SetModelName/ClearFinalizedStreamMarker。
type coverStreamer struct {
	updateErr      error
	finalizeErr    error
	reasoningErr   error
	finalizeCtxErr error
	finalizedWith  []string
	usage          *protocoltypes.UsageInfo
	modelName      string
	markerCleared  bool
	canceled       bool
	reasoningDone  string
}

func (s *coverStreamer) Update(context.Context, string) error { return s.updateErr }
func (s *coverStreamer) Finalize(_ context.Context, content string) error {
	if s.finalizeErr != nil {
		return s.finalizeErr
	}
	s.finalizedWith = append(s.finalizedWith, content)
	return nil
}
func (s *coverStreamer) Cancel(context.Context) { s.canceled = true }

func (s *coverStreamer) UpdateReasoning(context.Context, string) error { return s.reasoningErr }
func (s *coverStreamer) FinalizeReasoning(_ context.Context, content string) error {
	if s.reasoningErr != nil {
		return s.reasoningErr
	}
	s.reasoningDone = content
	return nil
}
func (s *coverStreamer) SetUsage(usage *protocoltypes.UsageInfo) { s.usage = usage }
func (s *coverStreamer) SetModelName(name string)                { s.modelName = name }
func (s *coverStreamer) ClearFinalizedStreamMarker()             { s.markerCleared = true }
func (s *coverStreamer) FinalizeWithContext(ctx context.Context, content string, _ *bus.ContextUsage) error {
	if s.finalizeCtxErr != nil {
		return s.finalizeCtxErr
	}
	return s.Finalize(ctx, content)
}

// baseStreamer 只实现 bus.Streamer（用于覆盖非 streamer 能力分支）。
type baseStreamer struct{ finalized bool }

func (s *baseStreamer) Update(context.Context, string) error { return nil }
func (s *baseStreamer) Finalize(context.Context, string) error {
	s.finalized = true
	return nil
}
func (s *baseStreamer) Cancel(context.Context) {}

func TestCoverStreamingPublisherMethods(t *testing.T) {
	ctx := context.Background()

	var nilPublisher *streamingChunkPublisher
	nilPublisher.Update(ctx, "x")
	nilPublisher.UpdateReasoning(ctx, "x")
	nilPublisher.Finalize(ctx, "x", nil)
	nilPublisher.FinalizeReasoning(ctx, "x")
	nilPublisher.ClearFinalizedStreamMarker()
	nilPublisher.Cancel(ctx)
	if nilPublisher.Published() || nilPublisher.ReasoningPublished() || nilPublisher.Err() != nil {
		t.Fatal("nil publisher getter 语义异常")
	}

	// 基础 streamer：不使用 Reasoning/Usage/ContextUsage 扩展。
	base := &baseStreamer{}
	publisher := &streamingChunkPublisher{streamer: base, modelName: "m"}
	publisher.Update(ctx, "   ")
	publisher.Update(ctx, "hello")
	if !publisher.Published() {
		t.Fatal("Update 后应标记 published")
	}
	publisher.UpdateReasoning(ctx, "reason") // base 不实现 ReasoningStreamer → 直接返回
	if publisher.ReasoningPublished() {
		t.Fatal("base streamer 不应标记 reasoning published")
	}
	if err := publisher.Finalize(ctx, "final", nil); err != nil {
		t.Fatalf("Finalize = %v", err)
	}
	if !base.finalized {
		t.Fatal("Finalize 未调用 streamer")
	}
	if err := publisher.FinalizeReasoning(ctx, "reason"); err != nil {
		t.Fatalf("FinalizeReasoning = %v", err)
	}
	publisher.ClearFinalizedStreamMarker()
	publisher.Cancel(ctx)
	if !base.finalized || !publisher.Published() {
		t.Fatal("基础 streamer 状态异常")
	}

	// 完整 streamer：覆盖 usage/context-usage/reasoning/modelName/marker 分支。
	full := &coverStreamer{}
	publisher = &streamingChunkPublisher{streamer: full, modelName: "model-x", usage: &protocoltypes.UsageInfo{PromptTokens: 1}}
	publisher.Update(ctx, "hello")
	if full.modelName != "model-x" {
		t.Fatalf("SetModelName = %q", full.modelName)
	}
	publisher.UpdateReasoning(ctx, "reason")
	if !publisher.ReasoningPublished() {
		t.Fatal("reasoning 未标记")
	}
	if err := publisher.Finalize(ctx, "final", &bus.ContextUsage{UsedTokens: 1}); err != nil {
		t.Fatalf("Finalize = %v", err)
	}
	if full.usage == nil || len(full.finalizedWith) != 1 {
		t.Fatalf("usage/finalize 未生效: usage=%+v finalized=%v", full.usage, full.finalizedWith)
	}
	if err := publisher.FinalizeReasoning(ctx, "reason-final"); err != nil || full.reasoningDone != "reason-final" {
		t.Fatalf("FinalizeReasoning = (%q, %v)", full.reasoningDone, err)
	}
	publisher.ClearFinalizedStreamMarker()
	if !full.markerCleared {
		t.Fatal("ClearFinalizedStreamMarker 未转发")
	}
	publisher.Cancel(ctx)
	if !full.canceled {
		t.Fatal("Cancel 未转发")
	}

	// 错误分支：Update/UpdateReasoning/Finalize/FinalizeReasoning。
	errStreamer := &coverStreamer{
		updateErr:    errors.New("update"),
		reasoningErr: errors.New("reasoning"),
		finalizeErr:  errors.New("finalize"),
	}
	publisher = &streamingChunkPublisher{streamer: errStreamer}
	publisher.Update(ctx, "x")
	if publisher.Err() == nil {
		t.Fatal("Update 错误未记录")
	}
	publisher.UpdateReasoning(ctx, "x")
	if publisher.ReasoningPublished() {
		t.Fatal("UpdateReasoning 失败不应 published")
	}
	if err := publisher.Finalize(ctx, "x", nil); err == nil || !strings.Contains(err.Error(), "stream finalize") {
		t.Fatalf("Finalize 错误 = %v", err)
	}
	publisher.reasoningPublished = true
	if err := publisher.FinalizeReasoning(ctx, "x"); err == nil || !strings.Contains(err.Error(), "stream reasoning finalize") {
		t.Fatalf("FinalizeReasoning 错误 = %v", err)
	}

	// 空内容且未发布：Finalize 直接返回。
	publisher = &streamingChunkPublisher{streamer: &coverStreamer{}}
	if err := publisher.Finalize(ctx, "  ", nil); err != nil {
		t.Fatalf("空内容 Finalize = %v", err)
	}

	// ContextUsage 错误分支。
	ctxErrStreamer := &coverStreamer{finalizeCtxErr: errors.New("ctx finalize")}
	publisher = &streamingChunkPublisher{streamer: ctxErrStreamer}
	publisher.published = true
	if err := publisher.Finalize(ctx, "x", nil); err == nil {
		t.Fatal("FinalizeWithContext 错误未返回")
	}
}

func TestCoverConfiguredStreamingVisibleError(t *testing.T) {
	withCause := configuredStreamingVisibleError{err: errors.New("boom")}
	if withCause.Error() != "boom" || !errors.Is(withCause, withCause.err) {
		t.Fatalf("visible error = %q / unwrap %v", withCause.Error(), withCause.Unwrap())
	}
	plain := configuredStreamingVisibleError{}
	if plain.Error() == "" || plain.Unwrap() != nil {
		t.Fatalf("plain visible error = %q / unwrap %v", plain.Error(), plain.Unwrap())
	}
	if !isConfiguredStreamingVisibleError(withCause) || isConfiguredStreamingVisibleError(errors.New("x")) {
		t.Fatal("isConfiguredStreamingVisibleError 判定异常")
	}
}

// ---------------------------------------------------------------------------
// pipeline_llm.go: CallLLM 错误/取消/重试分支
// ---------------------------------------------------------------------------

func newCoverExec(ts *turnState, messages []providers.Message) *turnExecution {
	return &turnExecution{messages: messages, callMessages: messages, history: messages}
}

// withCoverProvider 填充 CallLLM 直接调用所需的 provider/model 字段。
func withCoverProvider(exec *turnExecution, provider providers.LLMProvider) *turnExecution {
	exec.activeProvider = provider
	exec.activeModel = "cover-model"
	exec.llmModel = "cover-model"
	exec.llmModelName = "cover-model"
	exec.activeModelConfig = &config.ModelConfig{ModelName: "cover-model"}
	return exec
}

func TestCoverCallLLMVisionUnsupported(t *testing.T) {
	provider := &erroredResponseProvider{err: errors.New("no endpoints found that support image input")}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	ts := newTurnState(agent, makeTestProcessOpts("coverage-vision"), turnEventScope{turnID: "coverage-vision", context: newTurnContext(nil, nil, nil)})
	exec := newCoverExec(ts, []providers.Message{{
		Role:    "user",
		Content: "看图",
		Media:   []string{"media://abc"},
	}})
	withCoverProvider(exec, provider)
	ctrl, err := NewPipeline(al).CallLLM(context.Background(), context.Background(), ts, exec, 1)
	if ctrl != ControlBreak || err == nil {
		t.Fatalf("vision unsupported = (%v, %v), want (ControlBreak, error)", ctrl, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "vision") &&
		!strings.Contains(strings.ToLower(err.Error()), "image") {
		t.Fatalf("vision 错误信息 = %v", err)
	}
}

// coverLLMHook 是 LLMInterceptor 的测试 hook。
type coverLLMHook struct {
	before HookAction
	after  HookAction
	reason string
}

func (h *coverLLMHook) BeforeLLM(_ context.Context, req *LLMHookRequest) (*LLMHookRequest, HookDecision, error) {
	return req, HookDecision{Action: h.before, Reason: h.reason}, nil
}
func (h *coverLLMHook) AfterLLM(_ context.Context, resp *LLMHookResponse) (*LLMHookResponse, HookDecision, error) {
	return resp, HookDecision{Action: h.after, Reason: h.reason}, nil
}

func TestCoverCallLLMHookAborts(t *testing.T) {
	cases := []struct {
		name        string
		hook        coverLLMHook
		wantHard    bool
		wantAborted bool
	}{
		{"before abort", coverLLMHook{before: HookActionAbortTurn}, false, true},
		{"before hard abort", coverLLMHook{before: HookActionHardAbort}, true, false},
		{"after abort", coverLLMHook{before: HookActionContinue, after: HookActionAbortTurn}, false, true},
		{"after hard abort", coverLLMHook{before: HookActionContinue, after: HookActionHardAbort}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "ok", finishReason: "stop"}}}
			al, agent, cleanup := newHookTestLoop(t, provider)
			defer cleanup()
			hook := tc.hook
			if err := al.MountHook(NamedHook("cover-llm-hook", &hook)); err != nil {
				t.Fatalf("MountHook: %v", err)
			}
			ts := newTurnState(agent, makeTestProcessOpts("coverage-llm-abort"), turnEventScope{turnID: "coverage-llm-abort", context: newTurnContext(nil, nil, nil)})
			exec := withCoverProvider(newCoverExec(ts, []providers.Message{{Role: "user", Content: "hi"}}), provider)
			ctrl, err := NewPipeline(al).CallLLM(context.Background(), context.Background(), ts, exec, 1)
			if err != nil {
				t.Fatalf("hook abort 不应返回错误: %v", err)
			}
			if ctrl != ControlBreak {
				t.Fatalf("ctrl = %v, want ControlBreak", ctrl)
			}
			if exec.abortedByHardAbort != tc.wantHard || exec.abortedByHook != tc.wantAborted {
				t.Fatalf("abortedByHardAbort=%v abortedByHook=%v, want %v/%v",
					exec.abortedByHardAbort, exec.abortedByHook, tc.wantHard, tc.wantAborted)
			}
		})
	}
}

// hardAbortTriggerProvider 在首次调用时通过 turnState 上下文请求 hard abort 并返回瞬时错误。
type hardAbortTriggerProvider struct {
	calls int
}

func (p *hardAbortTriggerProvider) Chat(ctx context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any) (*providers.LLMResponse, error) {
	p.calls++
	if ts := turnStateFromContext(ctx); ts != nil {
		_ = ts.requestHardAbort()
	}
	// CallLLM 只有在 hard abort 已请求且错误为 context.Canceled 时才走中止分支。
	return nil, context.Canceled
}
func (p *hardAbortTriggerProvider) GetDefaultModel() string { return "hard-abort-trigger" }

func TestCoverCallLLMHardAbortDuringRetryBackoff(t *testing.T) {
	provider := &hardAbortTriggerProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	al.cfg.Agents.Defaults.LLMRetryBackoffSecs = 1
	al.cfg.Agents.Defaults.MaxLLMRetries = 1
	ts := newTurnState(agent, makeTestProcessOpts("coverage-hard-abort-retry"), turnEventScope{turnID: "coverage-hard-abort-retry", context: newTurnContext(nil, nil, nil)})
	turnCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turnCtx = withTurnState(turnCtx, ts)
	exec := withCoverProvider(newCoverExec(ts, []providers.Message{{Role: "user", Content: "hi"}}), provider)
	ctrl, err := NewPipeline(al).CallLLM(turnCtx, turnCtx, ts, exec, 1)
	if err != nil {
		t.Fatalf("hard abort retry 不应返回错误: %v", err)
	}
	if ctrl != ControlBreak || !exec.abortedByHardAbort {
		t.Fatalf("ctrl=%v abortedByHardAbort=%v, want ControlBreak/true", ctrl, exec.abortedByHardAbort)
	}
}

func TestCoverCallLLMGracefulContextRetry(t *testing.T) {
	provider := &faultOverflowThenSuccessProvider{}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	ts := newTurnState(agent, makeTestProcessOpts("coverage-graceful-retry"), turnEventScope{turnID: "coverage-graceful-retry", context: newTurnContext(nil, nil, nil)})
	ts.requestGracefulInterrupt("请总结后停止")
	exec := withCoverProvider(newCoverExec(ts, []providers.Message{{Role: "user", Content: "hi"}}), provider)
	ctrl, err := NewPipeline(al).CallLLM(context.Background(), context.Background(), ts, exec, 1)
	if err != nil {
		t.Fatalf("graceful context retry = %v", err)
	}
	if ctrl != ControlBreak {
		t.Fatalf("ctrl = %v, want ControlBreak", ctrl)
	}
}

func TestCoverCallLLMModelFromAPIAndReasoningFallback(t *testing.T) {
	provider := &reasoningOnlyProvider{usage: &providers.UsageInfo{PromptTokens: 3}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	opts := makeTestProcessOpts("coverage-model-api")
	opts.Dispatch.SessionKey = "coverage-model-api"
	opts.Dispatch.UserMessage = "hi"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-model-api", context: newTurnContext(nil, nil, nil)})
	result, err := al.runTurn(context.Background(), ts, NewPipeline(al))
	if err != nil {
		t.Fatalf("runTurn: %v", err)
	}
	if result.modelName != "api-reported-model" {
		t.Fatalf("modelName = %q, want api-reported-model", result.modelName)
	}
	if result.finalContent != "只有推理内容" {
		t.Fatalf("finalContent = %q, want reasoning fallback", result.finalContent)
	}
}

type reasoningOnlyProvider struct{ usage *providers.UsageInfo }

func (p *reasoningOnlyProvider) Chat(context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{
		Content:          "",
		ReasoningContent: "只有推理内容",
		Model:            "api-reported-model",
		Usage:            p.usage,
		FinishReason:     "stop",
	}, nil
}
func (p *reasoningOnlyProvider) GetDefaultModel() string { return "reasoning-only" }

func TestCoverProviderAndRewriteHelpers(t *testing.T) {
	if _, err := providerForFallbackCandidate(nil, nil, nil, "m", "m"); err == nil {
		t.Fatal("nil active provider 应返回错误")
	}
	pipeline := &Pipeline{}
	exec := &turnExecution{llmModel: ""}
	ts := &turnState{agent: &AgentInstance{}}
	pipeline.applyBeforeLLMModelRewrite(ts, exec) // 空 model：直接返回
	if exec.llmModel != "" {
		t.Fatalf("空 model 被改写为 %q", exec.llmModel)
	}
	exec.llmModel = "gpt-x"
	pipeline.applyBeforeLLMModelRewrite(ts, exec) // nil cfg：使用默认 provider
	if exec.llmModel == "" {
		t.Fatal("applyBeforeLLMModelRewrite 未设置 model")
	}
	pipeline.applyBeforeLLMModelRewrite(nil, exec) // nil ts：安全返回
	pipeline.applyBeforeLLMModelRewrite(ts, nil)   // nil exec：安全返回

	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("connection reset by peer"), true},
		{errors.New("deadline exceeded"), true},
		{errors.New("random failure"), false},
	}
	for _, tc := range cases {
		if _, got := transientLLMRetryReason(tc.err); got != tc.want {
			t.Errorf("transientLLMRetryReason(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// erroredResponseProvider 固定返回错误。
type erroredResponseProvider struct{ err error }

func (p *erroredResponseProvider) Chat(context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any) (*providers.LLMResponse, error) {
	return nil, p.err
}
func (p *erroredResponseProvider) GetDefaultModel() string { return "errored-provider" }

// ---------------------------------------------------------------------------
// turn_coord.go: 中止 / 父回合结束 / pendingResults / 任务续跑 / finalize 错误
// ---------------------------------------------------------------------------

func TestCoverTurnPendingStopAtStart(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "must not run", finishReason: "stop"}}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	sessionKey := "coverage-pending-stop"
	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch.SessionKey = sessionKey
	opts.Dispatch.UserMessage = "hi"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-pending-stop", context: newTurnContext(nil, nil, nil)})
	al.markPendingStop(sessionKey)
	result, err := al.runTurn(context.Background(), ts, NewPipeline(al))
	if err != nil {
		t.Fatalf("pending stop runTurn = %v", err)
	}
	if result.status != TurnEndStatusAborted {
		t.Fatalf("status = %q, want aborted", result.status)
	}
}

func TestCoverTurnParentEndedBreaks(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "ok", finishReason: "stop"}}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	parent := &turnState{}
	parent.parentEnded.Store(true)
	for _, critical := range []bool{false, true} {
		opts := makeTestProcessOpts("coverage-parent-end")
		opts.Dispatch.SessionKey = "coverage-parent-end"
		opts.Dispatch.UserMessage = "hi"
		ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-parent-end", context: newTurnContext(nil, nil, nil)})
		ts.parentTurnState = parent
		ts.critical = critical
		if _, err := al.runTurn(context.Background(), ts, NewPipeline(al)); err != nil {
			t.Fatalf("critical=%v parent ended runTurn = %v", critical, err)
		}
	}
}

func TestCoverTurnPendingResultsDelivery(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "done", finishReason: "stop"}}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	opts := makeTestProcessOpts("coverage-pending-results")
	opts.Dispatch.SessionKey = "coverage-pending-results"
	opts.Dispatch.UserMessage = "hi"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-pending-results", context: newTurnContext(nil, nil, nil)})
	results := make(chan *tools.ToolResult, 1)
	results <- &tools.ToolResult{ForLLM: "子回合结果"}
	ts.pendingResults = results
	if _, err := al.runTurn(context.Background(), ts, NewPipeline(al)); err != nil {
		t.Fatalf("pendingResults runTurn = %v", err)
	}
}

func TestCoverTurnTaskContinuationOnEmptyResponse(t *testing.T) {
	provider := &scriptedAutonomyProvider{steps: []autonomyLLMStep{
		{content: ""},
		{content: "继续完成"},
	}}
	al, _, _ := newAutonomyTestLoop(t, provider, defaultAutonomyConfig())
	agent, store := autonomyAgentLoop(t, al)
	defer al.cancelAllAutonomyRounds(AutonomyRoundReasonUserStop)
	sessionKey := autonomySessionKey(agent, "coverage-task-continuation")
	seedAutonomyState(t, store, sessionKey, newAutonomyState(t, "空响应后继续", 10))

	opts := autonomyTurnState(al, agent, sessionKey, "cli", "chat", "").opts
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-task-continuation", context: newTurnContext(nil, nil, nil)})
	result, err := al.runTurn(context.Background(), ts, NewPipeline(al))
	if err != nil {
		t.Fatalf("task continuation runTurn = %v", err)
	}
	if provider.callCount() < 2 {
		t.Fatalf("provider calls = %d, want >=2（空响应应触发任务续跑）", provider.callCount())
	}
	if result.finalContent == "" {
		t.Fatal("续跑后应有最终内容")
	}
}

func TestCoverTurnFinalizeSaveError(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "done", finishReason: "stop"}}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	agent.Sessions = &faultFailingSaveStore{SessionStore: agent.Sessions, err: errors.New("save fail")}
	opts := makeTestProcessOpts("coverage-finalize-save")
	opts.Dispatch.SessionKey = "coverage-finalize-save"
	opts.Dispatch.UserMessage = "hi"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-finalize-save", context: newTurnContext(nil, nil, nil)})
	if _, err := al.runTurn(context.Background(), ts, NewPipeline(al)); err == nil {
		t.Fatal("finalize save 失败应返回错误")
	}
}

func TestCoverTurnAbortRestoreErrors(t *testing.T) {
	// hard abort + 回滚保存失败 → abortTurn 错误分支。
	provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "done", finishReason: "stop"}}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	agent.Sessions = &faultFailingSaveStore{SessionStore: agent.Sessions, err: errors.New("restore fail")}
	opts := makeTestProcessOpts("coverage-abort-restore")
	opts.Dispatch.SessionKey = "coverage-abort-restore"
	opts.Dispatch.UserMessage = "hi"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-abort-restore", context: newTurnContext(nil, nil, nil)})
	_ = ts.requestHardAbort()
	result, err := al.runTurn(context.Background(), ts, NewPipeline(al))
	if err == nil {
		t.Fatalf("回滚保存失败应返回错误, result=%+v", result)
	}
}

func TestCoverTurnHookAbortRestoreError(t *testing.T) {
	provider := &loopFixProvider{steps: []loopFixLLMStep{
		{content: "call", finishReason: "tool_calls", toolCalls: []providers.ToolCall{{ID: "c1", Name: "echo_text", Arguments: map[string]any{"text": "x"}}}},
		{content: "done", finishReason: "stop"},
	}}
	al, agent, cleanup := newHookTestLoop(t, provider)
	defer cleanup()
	al.RegisterTool(&faultEchoTool{})
	if err := al.MountHook(NamedHook("cover-abort-hook", faultAbortToolHook{})); err != nil {
		t.Fatalf("MountHook: %v", err)
	}
	agent.Sessions = &faultFailingSaveStore{SessionStore: agent.Sessions, err: errors.New("restore fail")}
	opts := makeTestProcessOpts("coverage-hook-abort-restore")
	opts.Dispatch.SessionKey = "coverage-hook-abort-restore"
	opts.Dispatch.UserMessage = "hi"
	ts := newTurnState(agent, opts, turnEventScope{turnID: "coverage-hook-abort-restore", context: newTurnContext(nil, nil, nil)})
	if _, err := al.runTurn(context.Background(), ts, NewPipeline(al)); err == nil {
		t.Fatal("hook abort 回滚保存失败应返回错误")
	}
}

func TestCoverSelectCandidatesPrimaryBranch(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	agent.Model = "primary-model"
	agent.Candidates = []providers.FallbackCandidate{{Provider: "openai", Model: "gpt-5.4", IdentityKey: "model_name:primary", DisplayName: "primary-model"}}
	agent.LightCandidates = []providers.FallbackCandidate{{Provider: "openai", Model: "gpt-mini", IdentityKey: "model_name:light", DisplayName: "light-model"}}
	agent.Router = routing.New(routing.RouterConfig{LightModel: "light-model", Threshold: 0.35})
	complexMsg := strings.Repeat("```go\nfunc main() { println(\"x\") }\n```\n", 10) +
		strings.Repeat("请详细分析这段代码的复杂度并给出修复方案。", 20)
	history := make([]providers.Message, 0, 20)
	for i := 0; i < 10; i++ {
		history = append(history, providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file"}}})
	}
	candidates, model, usedLight := al.selectCandidates(agent, complexMsg, history)
	if usedLight {
		t.Fatal("极低阈值时文本应留在主模型")
	}
	if len(candidates) == 0 || model == "" {
		t.Fatalf("selectCandidates = (%v, %q)", candidates, model)
	}
}

func TestCoverAskSideQuestionBranches(t *testing.T) {
	al, agent, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	ctx := context.Background()

	if _, err := al.askSideQuestion(ctx, nil, nil, "问题"); err == nil {
		t.Fatal("nil agent 应返回错误")
	}
	if _, err := al.askSideQuestion(ctx, agent, nil, "   "); err == nil {
		t.Fatal("空问题应返回错误")
	}
	// opts 非 nil：正常路径与 provider 错误路径。
	useTestSideQuestionProvider(al, &sideQuestionFallbackTestProvider{model: "test-model"})
	opts := makeTestProcessOpts("coverage-side-question")
	if _, err := al.askSideQuestion(ctx, agent, &opts, "问题"); err == nil {
		t.Fatal("provider 失败应返回错误")
	}

	// provider 返回 nil response → 空结果、无错误。
	useTestSideQuestionProvider(al, &faultNilResponseProvider{})
	if got, err := al.askSideQuestion(ctx, agent, &opts, "问题"); err != nil || got != "" {
		t.Fatalf("nil response side question = (%q, %v)", got, err)
	}

	// BeforeLLM hook abort（有/无 reason）与 HardAbort。
	for _, tc := range []struct {
		name   string
		action HookAction
		reason string
	}{
		{"before abort with reason", HookActionAbortTurn, "自定义原因"},
		{"before abort no reason", HookActionAbortTurn, ""},
		{"before hard abort", HookActionHardAbort, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "ok", finishReason: "stop"}}}
			al2, agent2, cleanup2 := newHookTestLoop(t, provider)
			defer cleanup2()
			useTestSideQuestionProvider(al2, &mockProvider{})
			hook := &coverLLMHook{before: tc.action, reason: tc.reason}
			if err := al2.MountHook(NamedHook("cover-side-hook", hook)); err != nil {
				t.Fatalf("MountHook: %v", err)
			}
			opts2 := makeTestProcessOpts("coverage-side-hook")
			if _, err := al2.askSideQuestion(context.Background(), agent2, &opts2, "问题"); err == nil {
				t.Fatalf("hook %q 应返回错误", tc.name)
			}
		})
	}

	// AfterLLM hook abort（有/无 reason）。
	for _, reason := range []string{"", "另一个原因"} {
		provider := &loopFixProvider{steps: []loopFixLLMStep{{content: "ok", finishReason: "stop"}}}
		al3, agent3, cleanup3 := newHookTestLoop(t, provider)
		defer cleanup3()
		useTestSideQuestionProvider(al3, &mockProvider{})
		hook := &coverLLMHook{before: HookActionContinue, after: HookActionAbortTurn, reason: reason}
		if err := al3.MountHook(NamedHook("cover-side-after-hook", hook)); err != nil {
			t.Fatalf("MountHook: %v", err)
		}
		opts3 := makeTestProcessOpts("coverage-side-after")
		if _, err := al3.askSideQuestion(context.Background(), agent3, &opts3, "问题"); err == nil {
			t.Fatalf("AfterLLM abort(reason=%q) 应返回错误", reason)
		}
	}
}

var _ = fmt.Sprintf
var _ = time.Second
