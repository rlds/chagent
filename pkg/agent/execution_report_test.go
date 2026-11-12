// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlds/chagent/pkg/agent/interfaces"
	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/channels"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/tools"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeExecutionReportChannel 只实现 execution.report 相关的行为；
// 其余 channels.Channel 方法通过嵌入接口提供（测试路径不会调用它们）。
type fakeExecutionReportChannel struct {
	channels.Channel

	mu      sync.Mutex
	reports []map[string]any
	chats   []string
	err     error
	block   chan struct{}
}

func (c *fakeExecutionReportChannel) SendExecutionReport(
	_ context.Context,
	chatID string,
	payload map[string]any,
) error {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	copied := make(map[string]any, len(payload))
	for key, value := range payload {
		copied[key] = value
	}
	c.reports = append(c.reports, copied)
	c.chats = append(c.chats, chatID)
	return nil
}

func (c *fakeExecutionReportChannel) snapshot() ([]map[string]any, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	reports := make([]map[string]any, len(c.reports))
	copy(reports, c.reports)
	chats := make([]string, len(c.chats))
	copy(chats, c.chats)
	return reports, chats
}

// fakeChannelManager 只实现 GetChannel；其余 ChannelManager 方法由嵌入接口提供。
type fakeChannelManager struct {
	interfaces.ChannelManager
	ch channels.Channel
}

func (m *fakeChannelManager) GetChannel(name string) (channels.Channel, bool) {
	if m.ch == nil {
		return nil, false
	}
	return m.ch, true
}

// newExecutionReportTestLoop 构造一个只带 execution.report 依赖的 AgentLoop。
func newExecutionReportTestLoop(t *testing.T, cfg *config.Config, ch channels.Channel) (*AgentLoop, *executionReportSink) {
	t.Helper()
	al := &AgentLoop{cfg: cfg}
	al.channelManager = &fakeChannelManager{ch: ch}
	al.executionReports = newExecutionReportSink(al)
	t.Cleanup(al.executionReports.close)
	return al, al.executionReports
}

func newExecutionReportTurnState(agent *AgentInstance) *turnState {
	if agent == nil {
		agent = &AgentInstance{ID: "agent-1", Model: "test-model"}
	}
	return &turnState{
		agent:      agent,
		sessionKey: "sk-test-session",
		chatID:     "pico:session-1",
		channel:    "pico",
		turnID:     "turn-1",
		opts: processOptions{
			Dispatch: DispatchRequest{
				InboundContext: &bus.InboundContext{
					Channel:  "pico",
					ChatID:   "pico:session-1",
					SenderID: "user-1",
				},
			},
			TurnSource: "human",
		},
	}
}

func waitForExecutionReports(t *testing.T, ch *fakeExecutionReportChannel, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		reports, _ := ch.snapshot()
		if len(reports) >= want {
			return reports
		}
		time.Sleep(5 * time.Millisecond)
	}
	reports, _ := ch.snapshot()
	t.Fatalf("execution.report count = %d, want %d", len(reports), want)
	return nil
}

// ---------------------------------------------------------------------------
// 默认开启 / 显式关闭
// ---------------------------------------------------------------------------

func TestExecutionReportEnabledByDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	if !cfg.GetExecutionReport().IsEnabled() {
		t.Fatal("DefaultConfig must keep execution.report enabled")
	}

	ch := &fakeExecutionReportChannel{}
	al, _ := newExecutionReportTestLoop(t, cfg, ch)
	ts := newExecutionReportTurnState(nil)

	al.reportToolExecution(ts, toolExecutionReport{
		ToolName:   "read_file",
		ToolCallID: "call-1",
		Arguments:  map[string]any{"path": "/tmp/a.txt"},
		Result:     tools.UserResult("ok"),
		StartedAt:  time.Now(),
		Duration:   10 * time.Millisecond,
		Iteration:  1,
	})

	reports := waitForExecutionReports(t, ch, 1)
	if got := reports[0][executionReportKeyToolName]; got != "read_file" {
		t.Fatalf("tool_name = %v, want read_file", got)
	}
}

func TestExecutionReportDisabledByExplicitFalse(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ExecutionReport.SetEnabled(false)

	ch := &fakeExecutionReportChannel{}
	al, _ := newExecutionReportTestLoop(t, cfg, ch)
	ts := newExecutionReportTurnState(nil)

	al.reportToolExecution(ts, toolExecutionReport{
		ToolName:   "read_file",
		ToolCallID: "call-1",
		Result:     tools.UserResult("ok"),
		StartedAt:  time.Now(),
	})

	// 给异步发送器足够的时间窗口，然后断言确实没有任何上报。
	time.Sleep(100 * time.Millisecond)
	reports, _ := ch.snapshot()
	if len(reports) != 0 {
		t.Fatalf("disabled execution.report emitted %d report(s)", len(reports))
	}
}

// ---------------------------------------------------------------------------
// 分类：skill / mcp / tool
// ---------------------------------------------------------------------------

func TestClassifyExecution_SkillTools(t *testing.T) {
	for _, toolName := range []string{"skill", "use_skill", " Skill "} {
		got := classifyExecution(nil, nil, toolName, map[string]any{"name": "code-review"})
		if got.ExecutionType != executionTypeSkill {
			t.Fatalf("%q execution_type = %q, want skill", toolName, got.ExecutionType)
		}
		if got.SkillName != "code-review" {
			t.Fatalf("%q skill_name = %q, want code-review", toolName, got.SkillName)
		}
	}
}

func TestClassifyExecution_MCPUsesRegistryOrigin(t *testing.T) {
	registry := tools.NewToolRegistry()
	mcpTool := tools.NewMCPTool(nil, "chess", &mcp.Tool{Name: "agent_channel_list"})
	registry.Register(mcpTool)
	agent := &AgentInstance{ID: "agent-1", Tools: registry}

	got := classifyExecution(agent, config.DefaultConfig(), mcpTool.Name(), nil)
	if got.ExecutionType != executionTypeMCP {
		t.Fatalf("execution_type = %q, want mcp", got.ExecutionType)
	}
	if got.MCPServer != "chess" {
		t.Fatalf("mcp_server = %q, want chess", got.MCPServer)
	}
	if got.MCPTool != "agent_channel_list" {
		t.Fatalf("mcp_tool = %q, want agent_channel_list", got.MCPTool)
	}
}

func TestClassifyExecution_MCPFallsBackToNameParsing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Tools.MCP.Servers = map[string]config.MCPServerConfig{
		"knowledge-base": {},
	}

	// 没有注册表可查时，用配置中的 server 名做最长前缀匹配。
	got := classifyExecution(nil, cfg, "mcp_knowledge-base_search_docs", nil)
	if got.ExecutionType != executionTypeMCP || got.MCPServer != "knowledge-base" || got.MCPTool != "search_docs" {
		t.Fatalf("classified = %+v, want mcp/knowledge-base/search_docs", got)
	}

	// 双下划线分隔的形态（mcp__server__tool）。
	got = classifyExecution(nil, cfg, "mcp__ops__restart", nil)
	if got.MCPServer != "ops" || got.MCPTool != "restart" {
		t.Fatalf("classified = %+v, want mcp/ops/restart", got)
	}

	// 无法解析 server 时不伪造：server 留空，工具名取剩余部分。
	got = classifyExecution(nil, nil, "mcp_unknown_tool", nil)
	if got.ExecutionType != executionTypeMCP || got.MCPServer != "" || got.MCPTool != "unknown_tool" {
		t.Fatalf("classified = %+v, want mcp//unknown_tool", got)
	}
}

func TestClassifyExecution_PlainTool(t *testing.T) {
	got := classifyExecution(nil, nil, "read_file", nil)
	if got.ExecutionType != executionTypeTool {
		t.Fatalf("execution_type = %q, want tool", got.ExecutionType)
	}
}

// ---------------------------------------------------------------------------
// 载荷：duration / status / 事件 id / 隐私
// ---------------------------------------------------------------------------

func TestBuildExecutionReportPayload_StatusAndDuration(t *testing.T) {
	cfg := config.DefaultConfig()
	ts := newExecutionReportTurnState(&AgentInstance{ID: "agent-1", Model: "model-x"})
	startedAt := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	payload := buildExecutionReportPayload(cfg, ts, toolExecutionReport{
		ToolName:   "exec",
		ToolCallID: "call-9",
		Arguments:  map[string]any{"command": "ls -la", "token": "secret-value"},
		Result:     tools.ErrorResult("boom: permission denied"),
		StartedAt:  startedAt,
		Duration:   1500 * time.Millisecond,
		Iteration:  3,
	})

	if got := payload[executionReportKeyStatus]; got != executionStatusError {
		t.Fatalf("status = %v, want error", got)
	}
	if got := payload[executionReportKeyDurationMS]; got != int64(1500) {
		t.Fatalf("duration_ms = %v (%T), want 1500", got, got)
	}
	if got := payload[executionReportKeyStartedAt]; got != startedAt.Format(time.RFC3339Nano) {
		t.Fatalf("started_at = %v, want %v", got, startedAt.Format(time.RFC3339Nano))
	}
	if got := payload[executionReportKeyEndedAt]; got != startedAt.Add(1500*time.Millisecond).Format(time.RFC3339Nano) {
		t.Fatalf("ended_at = %v", got)
	}
	if got := payload[executionReportKeyError].(string); !strings.Contains(got, "permission denied") {
		t.Fatalf("error = %q, want truncated tool error", got)
	}

	metadata, _ := payload[executionReportKeyMetadata].(map[string]any)
	if metadata["model"] != "model-x" || metadata["turn"] != "turn-1" || metadata["iteration"] != 3 {
		t.Fatalf("metadata = %#v", metadata)
	}

	// 隐私：参数值明文绝不出现在载荷里；结果只保留长度/摘要，不留正文。
	// （error 字段按契约携带截断后的工具错误，这是预期行为。）
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	body := string(encoded)
	for _, secret := range []string{"secret-value", "ls -la"} {
		if strings.Contains(body, secret) {
			t.Fatalf("payload leaked argument/result plaintext %q: %s", secret, body)
		}
	}
	args, _ := payload[executionReportKeyArguments].(map[string]any)
	if args["count"] != 2 {
		t.Fatalf("arguments.count = %v, want 2", args["count"])
	}
	result, _ := payload[executionReportKeyResult].(map[string]any)
	if _, ok := result["content"]; ok {
		t.Fatalf("result must not carry plaintext content: %#v", result)
	}
	if result["chars"] != len("boom: permission denied") {
		t.Fatalf("result.chars = %v", result["chars"])
	}
}

func TestBuildExecutionReportPayload_EventIDStableAndUnique(t *testing.T) {
	cfg := config.DefaultConfig()
	ts := newExecutionReportTurnState(nil)

	first := buildExecutionReportPayload(cfg, ts, toolExecutionReport{
		ToolName:   "skill",
		ToolCallID: "call-1",
		Arguments:  map[string]any{"name": "review"},
		Result:     tools.UserResult("ok"),
		StartedAt:  time.Now(),
	})
	// 同一次调用重复构建：event_id 稳定。
	again := buildExecutionReportPayload(cfg, ts, toolExecutionReport{
		ToolName:   "skill",
		ToolCallID: "call-1",
		Arguments:  map[string]any{"name": "review"},
		Result:     tools.UserResult("ok"),
		StartedAt:  time.Now().Add(time.Second),
	})
	other := buildExecutionReportPayload(cfg, ts, toolExecutionReport{
		ToolName:   "skill",
		ToolCallID: "call-2",
		Result:     tools.UserResult("ok"),
		StartedAt:  time.Now(),
	})
	noCallID := buildExecutionReportPayload(cfg, ts, toolExecutionReport{
		ToolName:  "skill",
		Result:    tools.UserResult("ok"),
		StartedAt: time.Now(),
	})

	if first[executionReportKeyEventID] != again[executionReportKeyEventID] {
		t.Fatalf("event_id not stable: %v vs %v",
			first[executionReportKeyEventID], again[executionReportKeyEventID])
	}
	if first[executionReportKeyEventID] == other[executionReportKeyEventID] {
		t.Fatalf("event_id collided across tool calls: %v", first[executionReportKeyEventID])
	}
	if id, _ := noCallID[executionReportKeyEventID].(string); !strings.HasPrefix(id, "exec-") || id == "exec-" {
		t.Fatalf("fallback event_id = %q, want UUID form", id)
	}
	if first[executionReportKeyEventID] == noCallID[executionReportKeyEventID] {
		t.Fatal("fallback event_id must not equal a call-id derived id")
	}
}

// ---------------------------------------------------------------------------
// 去重 / 非阻塞 / pico 未连接
// ---------------------------------------------------------------------------

func TestExecutionReportDeduplicatesSameEventID(t *testing.T) {
	cfg := config.DefaultConfig()
	ch := &fakeExecutionReportChannel{}
	al, _ := newExecutionReportTestLoop(t, cfg, ch)
	ts := newExecutionReportTurnState(nil)

	for i := 0; i < 3; i++ {
		al.reportToolExecution(ts, toolExecutionReport{
			ToolName:   "skill",
			ToolCallID: "call-dedup",
			Arguments:  map[string]any{"name": "review"},
			Result:     tools.UserResult("ok"),
			StartedAt:  time.Now(),
		})
	}

	reports := waitForExecutionReports(t, ch, 1)
	time.Sleep(50 * time.Millisecond)
	reports, _ = ch.snapshot()
	if len(reports) != 1 {
		t.Fatalf("duplicate event_id produced %d reports, want 1", len(reports))
	}
}

func TestExecutionReportDoesNotBlockToolExecution(t *testing.T) {
	cfg := config.DefaultConfig()
	ch := &fakeExecutionReportChannel{block: make(chan struct{})}
	al, _ := newExecutionReportTestLoop(t, cfg, ch)
	ts := newExecutionReportTurnState(nil)

	started := time.Now()
	for i := 0; i < 8; i++ {
		al.reportToolExecution(ts, toolExecutionReport{
			ToolName:   "exec",
			ToolCallID: "call-blocking-" + string(rune('a'+i)),
			Result:     tools.UserResult("ok"),
			StartedAt:  time.Now(),
			Duration:   time.Millisecond,
		})
	}
	elapsed := time.Since(started)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("report enqueue blocked tool path for %s", elapsed)
	}

	// 释放阻塞的发送器，确认它最终能排空（不要求报告数量）。
	close(ch.block)
}

func TestExecutionReportPicoNotConnectedDoesNotBlock(t *testing.T) {
	cfg := config.DefaultConfig()
	// 模拟 pico 未连接：发送返回 ErrSendFailed。
	ch := &fakeExecutionReportChannel{err: channels.ErrSendFailed}
	al, sink := newExecutionReportTestLoop(t, cfg, ch)
	ts := newExecutionReportTurnState(nil)

	started := time.Now()
	for i := 0; i < 4; i++ {
		al.reportToolExecution(ts, toolExecutionReport{
			ToolName:   "read_file",
			ToolCallID: "call-offline-" + string(rune('a'+i)),
			Result:     tools.UserResult("ok"),
			StartedAt:  time.Now(),
		})
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("report with disconnected pico blocked for %s", elapsed)
	}

	// 发送失败必须被吞掉（工具结果与主循环不受影响），sink 仍然存活。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && sink.failed.Load() < 4 {
		time.Sleep(5 * time.Millisecond)
	}
	if failed := sink.failed.Load(); failed < 4 {
		t.Fatalf("failed counter = %d, want >= 4", failed)
	}
}

func TestExecutionReportNilSinkAndChannelAreSafe(t *testing.T) {
	// sink 为 nil 时不得 panic（例如测试或未初始化的 loop）。
	al := &AgentLoop{cfg: config.DefaultConfig()}
	al.reportToolExecution(newExecutionReportTurnState(nil), toolExecutionReport{
		ToolName: "read_file",
		Result:   tools.UserResult("ok"),
	})

	// 通道不存在时同样只跳过。
	al2, _ := newExecutionReportTestLoop(t, config.DefaultConfig(), nil)
	al2.reportToolExecution(newExecutionReportTurnState(nil), toolExecutionReport{
		ToolName:   "read_file",
		ToolCallID: "call-no-channel",
		Result:     tools.UserResult("ok"),
		StartedAt:  time.Now(),
	})
	time.Sleep(50 * time.Millisecond)
}

// ---------------------------------------------------------------------------
// 端到端接线：工具执行完成路径真的会上报
// ---------------------------------------------------------------------------

type executionReportFakeTool struct {
	name    string
	content string
	isError bool
}

func (t *executionReportFakeTool) Name() string        { return t.name }
func (t *executionReportFakeTool) Description() string { return "fake tool" }
func (t *executionReportFakeTool) Parameters() map[string]any {
	return map[string]any{"type": "object"}
}
func (t *executionReportFakeTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	// 每次返回新的结果对象：注册表会就地规范化并修改它，共享指针会让并发批次
	// 路径出现（测试自身的）数据竞争。
	if t.isError {
		return tools.ErrorResult(t.content)
	}
	return tools.UserResult(t.content)
}

func TestExecuteToolsEmitsExecutionReportForSkill(t *testing.T) {
	cfg := config.DefaultConfig()
	ch := &fakeExecutionReportChannel{}
	al, _ := newExecutionReportTestLoop(t, cfg, ch)

	registry := tools.NewToolRegistry()
	registry.Register(&executionReportFakeTool{name: "skill", content: "skill loaded"})

	agent := &AgentInstance{ID: "agent-1", Model: "model-x", Tools: registry}
	ts := newExecutionReportTurnState(agent)
	ts.opts.NoHistory = true

	exec := &turnExecution{
		messages: []providers.Message{{Role: "user", Content: "跑一下 skill"}},
		normalizedToolCalls: []providers.ToolCall{{
			ID:        "call-e2e-skill",
			Type:      "function",
			Name:      "skill",
			Arguments: map[string]any{"name": "code-review"},
		}},
	}

	pipeline := &Pipeline{al: al, Cfg: cfg}
	control := pipeline.ExecuteTools(context.Background(), context.Background(), ts, exec, 2)
	if control != ToolControlContinue {
		t.Fatalf("ExecuteTools control = %v, want continue", control)
	}

	reports := waitForExecutionReports(t, ch, 1)
	report := reports[0]
	if got := report[executionReportKeyExecutionType]; got != executionTypeSkill {
		t.Fatalf("execution_type = %v, want skill", got)
	}
	if got := report[executionReportKeySkillName]; got != "code-review" {
		t.Fatalf("skill_name = %v, want code-review", got)
	}
	if got := report[executionReportKeyStatus]; got != executionStatusSuccess {
		t.Fatalf("status = %v, want success", got)
	}
	if got := report[executionReportKeyToolCallID]; got != "call-e2e-skill" {
		t.Fatalf("tool_call_id = %v, want call-e2e-skill", got)
	}
	if metadata, _ := report[executionReportKeyMetadata].(map[string]any); metadata["iteration"] != 2 {
		t.Fatalf("metadata.iteration = %v, want 2", metadata["iteration"])
	}
	if _, chats := ch.snapshot(); len(chats) != 1 || chats[0] != "pico:session-1" {
		t.Fatalf("report chat ids = %v, want [pico:session-1]", chats)
	}
}

func TestExecuteToolsSkipsReportWhenDisabled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ExecutionReport.SetEnabled(false)
	ch := &fakeExecutionReportChannel{}
	al, _ := newExecutionReportTestLoop(t, cfg, ch)

	registry := tools.NewToolRegistry()
	registry.Register(&executionReportFakeTool{name: "exec", content: "failed", isError: true})

	agent := &AgentInstance{ID: "agent-1", Model: "model-x", Tools: registry}
	ts := newExecutionReportTurnState(agent)
	ts.opts.NoHistory = true

	exec := &turnExecution{
		messages: []providers.Message{{Role: "user", Content: "跑一下"}},
		normalizedToolCalls: []providers.ToolCall{{
			ID:        "call-e2e-disabled",
			Type:      "function",
			Name:      "exec",
			Arguments: map[string]any{"command": "false"},
		}},
	}

	pipeline := &Pipeline{al: al, Cfg: cfg}
	pipeline.ExecuteTools(context.Background(), context.Background(), ts, exec, 1)

	time.Sleep(100 * time.Millisecond)
	if reports, _ := ch.snapshot(); len(reports) != 0 {
		t.Fatalf("disabled execution_report emitted %d report(s)", len(reports))
	}
}

func TestExecuteToolsEmitsExecutionReportFromConcurrentBatch(t *testing.T) {
	cfg := config.DefaultConfig()
	ch := &fakeExecutionReportChannel{}
	al, _ := newExecutionReportTestLoop(t, cfg, ch)

	registry := tools.NewToolRegistry()
	// "skill" 在名称级并行安全列表里，因此两个连续调用会走并发批次路径。
	registry.Register(&executionReportFakeTool{name: "skill", content: "loaded"})

	agent := &AgentInstance{ID: "agent-1", Model: "model-x", Tools: registry}
	ts := newExecutionReportTurnState(agent)
	ts.opts.NoHistory = true

	exec := &turnExecution{
		messages: []providers.Message{{Role: "user", Content: "并行跑两个 skill"}},
		normalizedToolCalls: []providers.ToolCall{
			{ID: "call-batch-1", Type: "function", Name: "skill", Arguments: map[string]any{"name": "first"}},
			{ID: "call-batch-2", Type: "function", Name: "skill", Arguments: map[string]any{"name": "second"}},
		},
	}

	pipeline := &Pipeline{al: al, Cfg: cfg}
	pipeline.ExecuteTools(context.Background(), context.Background(), ts, exec, 3)

	reports := waitForExecutionReports(t, ch, 2)
	if len(reports) != 2 {
		t.Fatalf("batch reports = %d, want 2", len(reports))
	}
	names := map[string]bool{}
	for _, report := range reports {
		if report[executionReportKeyExecutionType] != executionTypeSkill {
			t.Fatalf("execution_type = %v, want skill", report[executionReportKeyExecutionType])
		}
		names[report[executionReportKeySkillName].(string)] = true
		if report[executionReportKeyDurationMS] == nil {
			t.Fatalf("missing duration_ms: %#v", report)
		}
	}
	if !names["first"] || !names["second"] {
		t.Fatalf("skill names = %v, want first+second", names)
	}
}
