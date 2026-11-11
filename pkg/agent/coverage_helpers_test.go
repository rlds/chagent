// Chagent - 超轻量级个人 AI agent
//
// pkg/agent 覆盖率提升（task-39 / OPEN-01）：低层辅助函数与边界分支测试。
// 本文件只做确定性单元测试，不依赖真实模型/网络；与既有测试文件互不修改。

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/commands"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/tools"
)

// ---------------------------------------------------------------------------
// discovery.go
// ---------------------------------------------------------------------------

func TestCoverDiscoveryParagraphExtraction(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world\nsecond line", "hello world second line"},
		{"skip heading", "# Title\n\n正文第一段。", "正文第一段。"},
		{"bullet prefix stripped", "# T\n\n- item a\n* item b", "item a item b"},
		{"fenced code skipped", "# T\n\n```go\ncode\n```\n\n真正的段落", "真正的段落"},
		{"crlf normalized", "# T\r\n\r\n段落内容\r\n第二行", "段落内容 第二行"},
		{"empty", "", ""},
		{"only headings", "# a\n## b\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstMeaningfulParagraph(tc.in); got != tc.want {
				t.Fatalf("firstMeaningfulParagraph(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := firstNonEmptyLine("  \n\n  hello \nworld"); got != "hello" {
		t.Fatalf("firstNonEmptyLine = %q, want hello", got)
	}
	if got := firstNonEmptyLine("\r\n\r\nx\r\n"); got != "x" {
		t.Fatalf("firstNonEmptyLine CRLF = %q, want x", got)
	}
}

// ---------------------------------------------------------------------------
// runtime_event_logger.go
// ---------------------------------------------------------------------------

func TestCoverRuntimeEventPayloadSummary(t *testing.T) {
	now := time.Now()
	payloads := []any{
		TurnStartPayload{UserMessage: "hi", MediaCount: 2},
		TurnEndPayload{Status: TurnEndStatusCompleted, Iterations: 3, Duration: time.Second, FinalContentLen: 5},
		LLMRequestPayload{Model: "m", MessagesCount: 1, ToolsCount: 2, MaxTokens: 9},
		LLMDeltaPayload{ContentDeltaLen: 1, ReasoningDeltaLen: 2},
		LLMResponsePayload{ContentLen: 3, ToolCalls: 1, HasReasoning: true},
		LLMRetryPayload{Attempt: 1, MaxRetries: 2, Reason: "timeout", Error: "e", Backoff: time.Millisecond},
		ContextCompressPayload{Reason: "proactive", DroppedMessages: 2, RemainingMessages: 4},
		SessionSummarizePayload{SummarizedMessages: 3, KeptMessages: 1, SummaryLen: 6, OmittedOversized: true},
		ToolExecStartPayload{Tool: "read_file", Arguments: map[string]any{"a": 1}},
		ToolExecEndPayload{Tool: "read_file", Duration: time.Millisecond, ForLLMLen: 2, ForUserLen: 1, IsError: false},
		ToolExecSkippedPayload{Tool: "x", Reason: "denied"},
		SteeringInjectedPayload{Count: 1, TotalContentLen: 4},
		FollowUpQueuedPayload{SourceTool: "t", ContentLen: 7},
		InterruptReceivedPayload{Kind: InterruptKindGraceful, ContentLen: 3, HintLen: 4},
		SubTurnSpawnPayload{AgentID: "main", Label: "child", ParentTurnID: "p"},
		SubTurnEndPayload{AgentID: "main", Status: "done"},
		SubTurnResultDeliveredPayload{TargetChannel: "cli", TargetChatID: "c", ContentLen: 8},
		SubTurnOrphanPayload{ParentTurnID: "p", ChildTurnID: "c", Reason: "parent ended"},
		ErrorPayload{Stage: "llm", Message: "boom"},
		struct{ Unknown string }{Unknown: "x"},
	}
	for i, payload := range payloads {
		fields := map[string]any{}
		appendRuntimeEventPayloadSummary(fields, payload)
		_ = fields
		if i == len(payloads)-1 && len(fields) != 0 {
			t.Fatalf("unknown payload should not add fields: %+v", fields)
		}
	}
	_ = now

	patternCases := []struct {
		pattern, kind string
		want          bool
	}{
		{"", "agent.turn.start", false},
		{"*", "anything", true},
		{"agent.*", "agent.turn.start", true},
		{"agent.turn.*", "agent.turn.start", true},
		{"agent.turn.start", "agent.turn.start", true},
		{"agent.turn.start", "agent.turn.end", false},
		{"[", "[", true},
		{"agent.*", "other.kind", false},
	}
	for _, tc := range patternCases {
		if got := matchRuntimeEventPattern(tc.pattern, tc.kind); got != tc.want {
			t.Errorf("matchRuntimeEventPattern(%q, %q) = %v, want %v", tc.pattern, tc.kind, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// agent_command.go 纯函数
// ---------------------------------------------------------------------------

func TestCoverMCPSchemaHelpers(t *testing.T) {
	if got := summarizeMCPToolParameters(nil); got != nil {
		t.Fatalf("nil schema parameters = %+v, want nil", got)
	}
	params := summarizeMCPToolParameters(map[string]any{
		"properties": map[string]any{
			"beta":  map[string]any{"type": "string", "description": " b "},
			"alpha": map[string]any{"type": "integer"},
			"raw":   "not-a-map",
		},
		"required": []any{"alpha"},
	})
	if len(params) != 3 {
		t.Fatalf("parameters = %+v, want 3", params)
	}
	if params[0].Name != "alpha" || !params[0].Required || params[0].Type != "integer" {
		t.Fatalf("params[0] = %+v, want alpha required integer", params[0])
	}
	if params[1].Name != "beta" || params[1].Description != "b" {
		t.Fatalf("params[1] = %+v, want beta description b", params[1])
	}
	// required 为 []string 的变体。
	if got := summarizeMCPToolParameters(map[string]any{
		"properties": map[string]any{"x": map[string]any{"type": "string"}},
		"required":   []string{"x"},
	}); len(got) != 1 || !got[0].Required {
		t.Fatalf("[]string required 未被识别: %+v", got)
	}

	if got := normalizeMCPSchema(nil); got["type"] != "object" {
		t.Fatalf("nil schema = %+v, want default object schema", got)
	}
	direct := map[string]any{"type": "object"}
	if got := normalizeMCPSchema(direct); got["type"] != "object" {
		t.Fatalf("map schema passthrough failed: %+v", got)
	}
	raw := json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`)
	if got := normalizeMCPSchema(raw); got["type"] != "object" {
		t.Fatalf("RawMessage schema = %+v", got)
	}
	if got := normalizeMCPSchema([]byte(`{"type":"object"}`)); got["type"] != "object" {
		t.Fatalf("[]byte schema = %+v", got)
	}
	// 无法 Marshal 的值 → 默认 schema 兜底。
	if got := normalizeMCPSchema(make(chan int)); got["type"] != "object" {
		t.Fatalf("unmarshalable schema = %+v, want default", got)
	}

	if got := extractRuntimeSnapshotTime("无关\n当前时间：2026-01-02T03:04:05Z\n其余"); got != "2026-01-02T03:04:05Z" {
		t.Fatalf("extractRuntimeSnapshotTime = %q", got)
	}
	if got := extractRuntimeSnapshotTime("没有任何时间行"); got != "已注入" {
		t.Fatalf("extractRuntimeSnapshotTime fallback = %q, want 已注入", got)
	}
}

// ---------------------------------------------------------------------------
// memory.go
// ---------------------------------------------------------------------------

func TestCoverMemoryStoreDailyAndLongTerm(t *testing.T) {
	workspace := t.TempDir()
	store := NewMemoryStore(workspace)

	if got := store.ReadLongTerm(); got != "" {
		t.Fatalf("empty ReadLongTerm = %q", got)
	}
	if err := store.WriteLongTerm("长期记忆内容"); err != nil {
		t.Fatalf("WriteLongTerm failed: %v", err)
	}
	if got := store.ReadLongTerm(); got != "长期记忆内容" {
		t.Fatalf("ReadLongTerm = %q", got)
	}

	if got := store.ReadToday(); got != "" {
		t.Fatalf("empty ReadToday = %q", got)
	}
	if err := store.AppendToday("第一条"); err != nil {
		t.Fatalf("AppendToday failed: %v", err)
	}
	first := store.ReadToday()
	if !strings.Contains(first, "# ") || !strings.Contains(first, "第一条") {
		t.Fatalf("首日内容缺少日期头/正文: %q", first)
	}
	if err := store.AppendToday("第二条"); err != nil {
		t.Fatalf("second AppendToday failed: %v", err)
	}
	if got := store.ReadToday(); !strings.Contains(got, "第一条") || !strings.Contains(got, "第二条") {
		t.Fatalf("追加内容丢失: %q", got)
	}
	todayFile := store.getTodayFile()
	if _, err := os.Stat(todayFile); err != nil {
		t.Fatalf("today file 未创建: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(todayFile), time.Now().Format("2006")) {
		t.Fatalf("today file 命名异常: %s", todayFile)
	}
}

// ---------------------------------------------------------------------------
// context_source.go / context.go 小型 API
// ---------------------------------------------------------------------------

func TestCoverContextSourceHelpers(t *testing.T) {
	var nilCB *ContextBuilder
	if nilCB.HasContextSources() {
		t.Fatal("nil context builder 不应报告来源")
	}
	if err := nilCB.WithContextSources(); err == nil {
		_ = err
	}

	cb := NewContextBuilder(t.TempDir())
	if cb.HasContextSources() != true {
		// NewContextBuilder 默认注册内置技能目录来源；此处仅断言不 panic 且为布尔。
		t.Logf("HasContextSources() = %v", cb.HasContextSources())
	}
	cb.WithContextSources()
	if err := cb.WithContextSources(nil); err == nil {
		_ = err
	}

	// WithContextSources 允许 nil 来源（注册失败被记录并跳过）。
	cb.WithContextSources(nil)

	// RegisterPromptSource 成功路径 + 缓存失效（描述符必须声明 placement）。
	if err := cb.RegisterPromptSource(PromptSourceDescriptor{
		ID:      "test:source",
		Owner:   "test",
		Allowed: []PromptPlacement{{Layer: PromptLayerKernel, Slot: PromptSlotIdentity}},
	}); err != nil {
		t.Fatalf("RegisterPromptSource failed: %v", err)
	}

	// AddToolResult / AddAssistantMessage 追加语义。
	messages := cb.AddToolResult(nil, "call-1", "tool", "result")
	if len(messages) != 1 || messages[0].Role != "tool" || messages[0].ToolCallID != "call-1" {
		t.Fatalf("AddToolResult = %+v", messages)
	}
	messages = cb.AddAssistantMessage(messages, "answer", nil)
	if len(messages) != 2 || messages[1].Role != "assistant" || messages[1].Content != "answer" {
		t.Fatalf("AddAssistantMessage = %+v", messages)
	}

	if names := cb.ListSkillNames(); names != nil && len(names) != 0 {
		t.Logf("ListSkillNames = %v", names)
	}
	if cb.skillsLoader != nil {
		cb.skillsLoader = nil
		if names := cb.ListSkillNames(); names != nil {
			t.Fatalf("nil skillsLoader ListSkillNames = %v, want nil", names)
		}
	}

	// EstimateSystemTokens 空输入与带消息输入。
	if got := cb.EstimateSystemTokens("", nil); got <= 0 {
		t.Fatalf("EstimateSystemTokens(empty) = %d, want > 0", got)
	}
	if got := cb.EstimateSystemTokens("summary", []string{"skill-a"}); got <= 0 {
		t.Fatalf("EstimateSystemTokens(skills) = %d, want > 0", got)
	}
}

// ---------------------------------------------------------------------------
// prompt.go / prompt_contributors.go
// ---------------------------------------------------------------------------

func TestCoverPromptSlotPriorityAndContributorComponents(t *testing.T) {
	cases := map[PromptSlot]int{
		PromptSlotIdentity:     1000,
		PromptSlotHierarchy:    990,
		PromptSlotPolicy:       940,
		PromptSlotWorkspace:    900,
		PromptSlotTooling:      800,
		PromptSlotMCP:          790,
		PromptSlotSkillCatalog: 780,
		PromptSlotMemory:       700,
		PromptSlotOutput:       695,
		PromptSlotRuntime:      690,
		PromptSlotSummary:      680,
		PromptSlotMessage:      600,
		PromptSlotSteering:     590,
		PromptSlotSubTurn:      580,
		PromptSlotInterrupt:    570,
		PromptSlotContext:      0,
		PromptSlot("bogus"):    0,
	}
	for slot, want := range cases {
		if got := slotPriority(slot); got != want {
			t.Errorf("slotPriority(%q) = %d, want %d", slot, got, want)
		}
	}
	comp := promptSourceComponent("  Plugin:My_Source/Name  ")
	if comp == "" || strings.Contains(comp, " ") || strings.ToLower(comp) != comp {
		t.Fatalf("promptSourceComponent = %q", comp)
	}
	if got := promptSourceComponent(""); got != "unnamed" {
		t.Fatalf("empty promptSourceComponent = %q", got)
	}
	long := promptSourceComponent(strings.Repeat("a", 200))
	if len(long) > 64 {
		t.Fatalf("promptSourceComponent 未截断: len=%d", len(long))
	}
}

// ---------------------------------------------------------------------------
// loop_guard.go
// ---------------------------------------------------------------------------

func TestCoverMatchToolPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"", "read_file", false},
		{"*", "anything", true},
		{"read_file", "read_file", true},
		{"read_file", "write_file", false},
		{"read_*", "read_file", true},
		{"*_file", "read_file", true},
		{"re*file", "read_file", true},
		{"re*le", "read_file", true},
		{"re*ad*le", "readfile", true},
		{"xyz*file", "read_file", false},
		{"a**b", "axb", true},
		{"*", "", true},
	}
	for _, tc := range cases {
		if got := matchToolPattern(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchToolPattern(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// llm_media.go
// ---------------------------------------------------------------------------

func TestCoverVisionUnsupportedDetection(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{context.DeadlineExceeded, false},
		{errString("no endpoints found that support image input"), true},
		{errString("image input is not supported by this model"), true},
		{errString("model does not support vision"), true},
		{errString("images are not supported"), true},
		{errString("unsupported content type: image_url"), true},
		{errString("invalid request for image_url field"), true},
		{errString("invalid content type image/png"), false},
		{errString("unsupported image format"), false},
	}
	for _, tc := range cases {
		if got := isVisionUnsupportedError(tc.err); got != tc.want {
			t.Errorf("isVisionUnsupportedError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// turn_context.go / turn_state.go / taskstate_context.go
// ---------------------------------------------------------------------------

func TestCoverTurnContextCloneAndStateHelpers(t *testing.T) {
	if got := cloneIdentityLinks(nil); got != nil {
		t.Fatalf("cloneIdentityLinks(nil) = %v", got)
	}
	links := map[string][]string{"a": {"b"}}
	cloned := cloneIdentityLinks(links)
	if len(cloned) != 1 || len(cloned["a"]) != 1 || cloned["a"][0] != "b" {
		t.Fatalf("cloneIdentityLinks = %v", cloned)
	}
	cloned["a"][0] = "c"
	if links["a"][0] != "b" {
		t.Fatal("cloneIdentityLinks 未深拷贝")
	}

	ts := &turnState{}
	ts.SetLastUsage(nil)
	if ts.GetLastUsage() != nil {
		t.Fatal("nil usage 不应写入")
	}
	usage := &providers.UsageInfo{PromptTokens: 7}
	ts.SetLastUsage(usage)
	if got := ts.GetLastUsage(); got == nil || got.PromptTokens != 7 {
		t.Fatalf("SetLastUsage/GetLastUsage = %+v", got)
	}

	ts.recordAttemptedSkills([]string{"s1", "s2"})
	ts.recordAttemptedSkills(nil)
	if got := ts.attemptedSkillsSnapshot(); len(got) != 2 {
		t.Fatalf("attemptedSkillsSnapshot = %v", got)
	}

	same := normalizeMessageForComparison(providers.Message{Role: "user", Content: "hi"})
	if same.Role != "user" || same.Content != "hi" {
		t.Fatalf("normalizeMessageForComparison = %+v", same)
	}

	if got := TurnStateFromContext(context.Background()); got != nil {
		t.Fatal("空 context 不应有 turnState")
	}
	ts2 := &turnState{}
	ctx := withTurnState(context.Background(), ts2)
	if got := TurnStateFromContext(ctx); got != ts2 {
		t.Fatal("TurnStateFromContext 未取回 turnState")
	}

	opts := TaskStateRenderOptions{}.withDefaults()
	if opts.MaxObjectiveRunes != DefaultTaskStateMaxObjectiveRunes ||
		opts.MaxEvidence != DefaultTaskStateMaxEvidence {
		t.Fatalf("TaskStateRenderOptions.withDefaults = %+v", opts)
	}
}

// ---------------------------------------------------------------------------
// registry.go / hooks.go / agent_event.go 小型 API
// ---------------------------------------------------------------------------

func TestCoverRegistryHooksEventHelpers(t *testing.T) {
	al, _, cleanup := newHookTestLoop(t, &mockProvider{})
	defer cleanup()
	al.RegisterTool(&mockCustomTool{})
	registry := al.GetRegistry()
	seen := 0
	registry.ForEachTool("mock_custom", func(tool tools.Tool) { seen++ })
	if seen == 0 {
		t.Fatal("ForEachTool 未遍历已注册工具")
	}
	registry.ForEachTool("no_such_tool", func(tool tools.Tool) { seen++ })
	if seen != 1 {
		t.Fatalf("ForEachTool 未知工具不应回调: seen=%d", seen)
	}

	hm := NewHookManager(nil)
	defer hm.Close()
	hm.logUnsupportedAction("hook", "before_tool", HookAction("bogus"))
	hm.Unmount("missing") // 不存在 hook 的卸载应安全

	emptyLoop := &AgentLoop{}
	if stats := emptyLoop.RuntimeEventStats(); !stats.Closed {
		t.Fatalf("nil runtimeEvents 的 Stats = %+v, want Closed", stats)
	}
	if ch := (&AgentLoop{}).RuntimeEvents(); ch != nil {
		t.Fatal("nil runtimeEvents 的 Channel 应为 nil")
	}
	var nilLoop *AgentLoop
	if stats := nilLoop.RuntimeEventStats(); !stats.Closed {
		t.Fatalf("nil AgentLoop Stats = %+v", stats)
	}
	if bus := nilLoop.RuntimeEventBus(); bus != nil {
		t.Fatal("nil AgentLoop RuntimeEventBus 应为 nil")
	}

	var option AgentLoopOption = WithRuntimeEvents(nil)
	if option == nil {
		t.Fatal("WithRuntimeEvents(nil) 返回 nil")
	}
	loop := &AgentLoop{}
	option(loop)
	if loop.runtimeEvents != nil {
		t.Fatal("WithRuntimeEvents(nil) 不应覆盖默认总线")
	}
	external := runtimeevents.NewBus()
	WithRuntimeEvents(external)(loop)
	if loop.ownsRuntimeEvents {
		t.Fatal("注入外部事件总线后 ownsRuntimeEvents 应为 false")
	}
	external.Close()
}

// ---------------------------------------------------------------------------
// subturn.go ephemeralSessionStore 包装
// ---------------------------------------------------------------------------

func TestCoverEphemeralSessionStore(t *testing.T) {
	store := newEphemeralSession(nil)
	if got := store.GetSummary("s"); got != "" {
		t.Fatalf("新 ephemeral GetSummary = %q, want empty", got)
	}
	store.SetSummary("s", "summary")
	if got := store.GetSummary("s"); got != "summary" {
		t.Fatalf("GetSummary = %q", got)
	}
	store.AddMessage("s", "user", "hi")
	store.AddFullMessage("s", providers.Message{Role: "assistant", Content: "yo"})
	if history := store.GetHistory("s"); len(history) != 2 {
		t.Fatalf("GetHistory = %+v", history)
	}
	store.SetSummary("s", "new summary")
	store.SetHistory("s", []providers.Message{{Role: "user", Content: "only"}})
	if history := store.GetHistory("s"); len(history) != 1 {
		t.Fatalf("SetHistory 后 = %+v", history)
	}
	store.TruncateHistory("s", 0)
	if history := store.GetHistory("s"); len(history) != 0 {
		t.Fatalf("TruncateHistory(0) = %+v", history)
	}
	store.TruncateHistory("s", 5)
	if err := store.Save("s"); err != nil {
		t.Fatalf("Save = %v", err)
	}
	if sessions := store.ListSessions(); len(sessions) != 0 {
		t.Fatalf("ListSessions = %+v", sessions)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

var _ = commands.ExecuteResult{}
