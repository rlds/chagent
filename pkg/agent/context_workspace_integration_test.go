package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/common"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// T2.5 集成测试：跨回合、resume、压缩以及禁用回退下的工作区指令基线注入。
// 这些测试用会记录发送给模型的确切消息列表的 fake provider，驱动真实 agent loop
// （SetupTurn + BuildMessages + 会话 JSONL）。
//
// 夹具布局说明：AGENT.md 是 agent 定义文件，会被 NewAgentInstance 从工作区上下文发现中排除；
// AGENTS.md 是指令文件，必须作为持久上下文消息注入。

const wctxSessionKey = "wctx-it-main"

type wctxRecordingProvider struct {
	mu       sync.Mutex
	requests [][]providers.Message
}

func (p *wctxRecordingProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]providers.Message(nil), messages...))
	p.mu.Unlock()
	return &providers.LLMResponse{
		Content:   "好的，已处理。",
		ToolCalls: []providers.ToolCall{},
	}, nil
}

func (p *wctxRecordingProvider) GetDefaultModel() string { return "wctx-recording-model" }

func (p *wctxRecordingProvider) lastRequest(t *testing.T) []providers.Message {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatal("provider did not receive any request")
	}
	return p.requests[len(p.requests)-1]
}

func (p *wctxRecordingProvider) allRequests() [][]providers.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]providers.Message, len(p.requests))
	copy(out, p.requests)
	return out
}

// wctxWorkspaceContextMessages 返回 provider 请求中的持久工作区指令上下文消息
// （不含 system prompt）。
func wctxWorkspaceContextMessages(messages []providers.Message) []providers.Message {
	out := make([]providers.Message, 0, 1)
	for _, msg := range messages {
		if msg.Context == nil {
			continue
		}
		if msg.Context.Kind == protocoltypes.ContextKindWorkspaceInstructions {
			out = append(out, msg)
		}
	}
	return out
}

// wctxNewConfig 构建启用工作区上下文的最小配置，并预置 AGENT.md（定义文件，
// 从发现中排除）与 AGENTS.md（必须成为基线上下文消息的指令文件）。
func wctxNewConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	workspace := t.TempDir()
	writeFile := func(name, content string) {
		t.Helper()
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeFile("AGENT.md", "# Agent\n\n你是一个测试 agent。\n")
	writeFile("AGENTS.md", "# 项目规则\n\n始终使用中文回复，并在改动前先读代码。\n")

	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         workspace,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 5,
				ContextWindow:     200000,
				WorkspaceContext:  config.WorkspaceContextConfig{Enabled: true},
			},
		},
	}
	return cfg, workspace
}

func wctxRunTurn(t *testing.T, al *AgentLoop, content string) string {
	t.Helper()
	agent := al.GetRegistry().GetDefaultAgent()
	if agent == nil {
		t.Fatal("no default agent")
	}
	resp, _, _, err := al.runAgentLoop(context.Background(), agent, processOptions{
		Dispatch: DispatchRequest{
			SessionKey:  wctxSessionKey,
			UserMessage: content,
		},
		DefaultResponse: defaultResponse,
		EnableSummary:   false,
		SendResponse:    false,
	})
	if err != nil {
		t.Fatalf("runAgentLoop() error = %v", err)
	}
	return resp
}

func wctxDefaultAgent(t *testing.T, al *AgentLoop) *AgentInstance {
	t.Helper()
	agent := al.GetRegistry().GetDefaultAgent()
	if agent == nil {
		t.Fatal("no default agent")
	}
	return agent
}

// TestContextWorkspaceFirstTurnOrder 覆盖 T2.5 场景 1：首个模型请求顺序为
// system → 工作区基线上下文 → 当前用户消息，且基线带工作区指令来源信息被持久化。
func TestContextWorkspaceFirstTurnOrder(t *testing.T) {
	cfg, _ := wctxNewConfig(t)
	provider := &wctxRecordingProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()

	if resp := wctxRunTurn(t, al, "第一个问题"); resp == "" {
		t.Fatal("empty response")
	}

	messages := provider.lastRequest(t)
	if len(messages) != 3 {
		t.Fatalf("first request length = %d, want 3 (system, context, user):\n%+v", len(messages), messages)
	}
	if messages[0].Role != "system" {
		t.Fatalf("messages[0].Role = %q, want system", messages[0].Role)
	}
	baseline := messages[1]
	if baseline.Role != "user" {
		t.Fatalf("baseline role = %q, want user", baseline.Role)
	}
	if baseline.Context == nil ||
		baseline.Context.Kind != protocoltypes.ContextKindWorkspaceInstructions ||
		!baseline.Context.Baseline {
		t.Fatalf("baseline metadata = %+v", baseline.Context)
	}
	if !strings.Contains(baseline.Content, "AGENTS.md") ||
		!strings.Contains(baseline.Content, "始终使用中文回复") {
		t.Fatalf("baseline missing instruction file content:\n%s", baseline.Content)
	}
	if strings.Contains(baseline.Content, "你是一个测试 agent") {
		t.Fatalf("baseline must not include the excluded AGENT.md definition:\n%s", baseline.Content)
	}
	if last := messages[2]; last.Role != "user" || last.Content != "第一个问题" {
		t.Fatalf("last message = %+v, want current user message", last)
	}

	// 基线是持久的：必须写入会话日志，让 replay/resume/压缩像对待普通 user 角色上下文一样对待它。
	history := wctxDefaultAgent(t, al).Sessions.GetHistory(wctxSessionKey)
	persisted := wctxWorkspaceContextMessages(history)
	if len(persisted) != 1 || persisted[0].Content != baseline.Content {
		t.Fatalf("persisted baseline count = %d, want 1 with same content: %#v", len(persisted), persisted)
	}
}

// TestContextWorkspaceSecondTurnNoDuplicateBaseline 覆盖 T2.5 场景 2 与“连续无变化”规则：
// 第二个回合不得重复注入基线，较早的回合按顺序保留在历史中。
func TestContextWorkspaceSecondTurnNoDuplicateBaseline(t *testing.T) {
	cfg, _ := wctxNewConfig(t)
	provider := &wctxRecordingProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()

	wctxRunTurn(t, al, "第一问")
	wctxRunTurn(t, al, "第二问")

	messages := provider.lastRequest(t)
	contexts := wctxWorkspaceContextMessages(messages)
	if len(contexts) != 1 {
		t.Fatalf("second request carries %d workspace baselines, want 1:\n%+v", len(contexts), messages)
	}
	if first := messages[0]; first.Role != "system" {
		t.Fatalf("messages[0].Role = %q, want system", first.Role)
	}
	// system → 基线 → user1 → assistant1 → user2
	if len(messages) != 5 {
		t.Fatalf("second request length = %d, want 5:\n%+v", len(messages), messages)
	}
	if messages[1].Role != "user" || messages[1].Context == nil {
		t.Fatalf("expected baseline at index 1, got %+v", messages[1])
	}
	if messages[2].Content != "第一问" || messages[4].Content != "第二问" {
		t.Fatalf("history order broken: %+v", messages)
	}
}

// TestContextWorkspaceResumeNoDuplicateBaseline 覆盖 T2.5 场景 3：新的 AgentLoop（进程重启）
// 读取同一工作区/会话 JSONL 时必须看到已持久化的基线，且不注入重复基线。
func TestContextWorkspaceResumeNoDuplicateBaseline(t *testing.T) {
	cfg, workspace := wctxNewConfig(t)

	firstProvider := &wctxRecordingProvider{}
	first := NewAgentLoop(cfg, bus.NewMessageBus(), firstProvider)
	wctxRunTurn(t, first, "重启前的问题")
	historyBefore := wctxDefaultAgent(t, first).Sessions.GetHistory(wctxSessionKey)
	if len(wctxWorkspaceContextMessages(historyBefore)) != 1 {
		t.Fatalf("baseline was not persisted before restart: %#v", historyBefore)
	}
	first.Close()

	if _, err := os.Stat(filepath.Join(workspace, "sessions")); err != nil {
		t.Fatalf("session store directory missing after first process: %v", err)
	}

	resumedProvider := &wctxRecordingProvider{}
	resumed := NewAgentLoop(cfg, bus.NewMessageBus(), resumedProvider)
	defer resumed.Close()

	historyAfter := wctxDefaultAgent(t, resumed).Sessions.GetHistory(wctxSessionKey)
	if len(historyAfter) == 0 {
		t.Fatal("resumed session history is empty; JSONL resume did not load")
	}
	if len(wctxWorkspaceContextMessages(historyAfter)) != 1 {
		t.Fatalf("resumed history has %d baselines, want 1", len(wctxWorkspaceContextMessages(historyAfter)))
	}

	wctxRunTurn(t, resumed, "重启后的问题")
	messages := resumedProvider.lastRequest(t)
	contexts := wctxWorkspaceContextMessages(messages)
	if len(contexts) != 1 {
		t.Fatalf("resumed request carries %d baselines, want 1:\n%+v", len(contexts), messages)
	}
	if contexts[0].Context == nil || !contexts[0].Context.Baseline {
		t.Fatalf("resumed baseline metadata = %+v", contexts[0].Context)
	}
}

// TestContextWorkspaceBaselineRebuiltAfterCompaction 覆盖 T2.5 场景 4：
// 强制紧急压缩会从模型可见历史中丢掉基线，因此下一回合必须重新注入（恰好一次）。
func TestContextWorkspaceBaselineRebuiltAfterCompaction(t *testing.T) {
	cfg, _ := wctxNewConfig(t)
	provider := &wctxRecordingProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()

	wctxRunTurn(t, al, "压缩前的问题")
	agent := wctxDefaultAgent(t, al)
	historyBefore := agent.Sessions.GetHistory(wctxSessionKey)
	if len(wctxWorkspaceContextMessages(historyBefore)) != 1 {
		t.Fatalf("expected baseline before compaction, got %#v", historyBefore)
	}

	// forceCompression 对超过 2 条的历史总会在回合边界压缩；
	// 该分支不检查预算。
	if err := al.contextManager.Compact(context.Background(), &CompactRequest{
		SessionKey: wctxSessionKey,
		Reason:     ContextCompressReasonProactive,
		Budget:     agent.ContextWindow,
	}); err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	historyAfter := agent.Sessions.GetHistory(wctxSessionKey)
	if len(historyAfter) >= len(historyBefore) {
		t.Fatalf("compaction did not shrink history: before=%d after=%d", len(historyBefore), len(historyAfter))
	}
	if got := wctxWorkspaceContextMessages(historyAfter); len(got) != 0 {
		t.Fatalf("compaction kept the baseline in visible history: %#v", got)
	}
	if summary := agent.Sessions.GetSummary(wctxSessionKey); !strings.Contains(summary, "紧急压缩") {
		t.Fatalf("summary does not record the compression note: %q", summary)
	}

	wctxRunTurn(t, al, "压缩后的问题")
	messages := provider.lastRequest(t)
	contexts := wctxWorkspaceContextMessages(messages)
	if len(contexts) != 1 {
		t.Fatalf("request after compaction carries %d baselines, want 1 rebuilt baseline:\n%+v", len(contexts), messages)
	}
	if contexts[0].Context == nil || !contexts[0].Context.Baseline {
		t.Fatalf("rebuilt baseline metadata = %+v", contexts[0].Context)
	}
	if !strings.Contains(contexts[0].Content, "始终使用中文回复") {
		t.Fatalf("rebuilt baseline lost instruction content:\n%s", contexts[0].Content)
	}
}

// TestContextWorkspaceDisabledFallsBack 覆盖 T2.5 场景 5：workspace_context 禁用时没有 projector，
// 也完全没有上下文消息（保持普通聊天的请求形态）。
func TestContextWorkspaceDisabledFallsBack(t *testing.T) {
	cfg, _ := wctxNewConfig(t)
	cfg.Agents.Defaults.WorkspaceContext.Enabled = false

	provider := &wctxRecordingProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()

	agent := wctxDefaultAgent(t, al)
	if agent.WorkspaceContext != nil {
		t.Fatal("workspace context projector must be nil when the feature is disabled")
	}

	wctxRunTurn(t, al, "第一问")
	wctxRunTurn(t, al, "第二问")

	requests := provider.allRequests()
	if len(requests) != 2 {
		t.Fatalf("provider request count = %d, want 2", len(requests))
	}
	for _, messages := range requests {
		for _, msg := range messages {
			if msg.Context != nil && msg.Context.Kind == protocoltypes.ContextKindWorkspaceInstructions {
				t.Fatalf("disabled workspace context still injected a context message: %+v", messages)
			}
		}
	}
	if got := wctxWorkspaceContextMessages(agent.Sessions.GetHistory(wctxSessionKey)); len(got) != 0 {
		t.Fatalf("disabled workspace context persisted %d context messages", len(got))
	}
}

// TestContextWorkspaceProviderPayloadOmitsContext 覆盖 T2.5 场景 6：即使基线在进程内携带 Context 元数据，
// provider 线格式也不得暴露它。序列化由 pkg/providers/common 负责（见 TestSerializeMessagesIgnoresContextMeta）；
// 本测试证明真实 agent 请求确实会产生这类消息。
func TestContextWorkspaceProviderPayloadOmitsContext(t *testing.T) {
	cfg, _ := wctxNewConfig(t)
	provider := &wctxRecordingProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	defer al.Close()

	wctxRunTurn(t, al, "检查 payload")

	messages := provider.lastRequest(t)
	if len(wctxWorkspaceContextMessages(messages)) != 1 {
		t.Fatalf("expected exactly one context message in the request: %+v", messages)
	}

	wire := common.SerializeMessages(messages)
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal(serialized messages): %v", err)
	}
	payload := string(raw)
	for _, forbidden := range []string{
		`"context"`,
		"workspace-instructions",
		"baseline_identity",
		`"changes"`,
	} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("provider payload leaks %q: %s", forbidden, payload)
		}
	}

	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal payload: %v", err)
	}
	if len(decoded) != len(messages) {
		t.Fatalf("wire message count = %d, want %d", len(decoded), len(messages))
	}
	for i, entry := range decoded {
		if _, ok := entry["context"]; ok {
			t.Fatalf("wire[%d] has a context field: %#v", i, entry)
		}
		if entry["content"] != messages[i].Content {
			t.Fatalf("wire[%d].content = %v, want %q", i, entry["content"], messages[i].Content)
		}
	}
}
