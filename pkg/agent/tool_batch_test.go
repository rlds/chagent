// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/tools"
)

// -----------------------------------------------------------------------------
// runToolBatch 调度器测试
// -----------------------------------------------------------------------------

// batchExecFunc 构建记录执行顺序的工具执行器。
func batchExecFunc(
	order *[]int,
	mu *sync.Mutex,
	fn func(item toolBatchItem) (*tools.ToolResult, error),
) execToolBatchFn {
	return func(_ context.Context, index int, tool string, args map[string]any) (*tools.ToolResult, error) {
		if mu != nil && order != nil {
			mu.Lock()
			*order = append(*order, index)
			mu.Unlock()
		}
		return fn(toolBatchItem{Index: index, Tool: tool, Args: args})
	}
}

func TestBatchPreservesModelOrder(t *testing.T) {
	items := []toolBatchItem{
		{Index: 0, Tool: "read_file", Args: map[string]any{"path": "a"}},
		{Index: 1, Tool: "read_file", Args: map[string]any{"path": "b"}},
		{Index: 2, Tool: "read_file", Args: map[string]any{"path": "c"}},
		{Index: 3, Tool: "read_file", Args: map[string]any{"path": "d"}},
	}
	execFn := func(_ context.Context, index int, _ string, args map[string]any) (*tools.ToolResult, error) {
		// 后面的调用先完成：完成顺序不得泄漏到输出顺序。
		time.Sleep(time.Duration(4-index) * 10 * time.Millisecond)
		return tools.SilentResult(fmt.Sprintf("content-%s", args["path"])), nil
	}

	outcomes := runToolBatch(context.Background(), items, execFn, 4, nil, nil)
	if len(outcomes) != len(items) {
		t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(items))
	}
	for i, outcome := range outcomes {
		if outcome.Index != items[i].Index {
			t.Fatalf("outcomes[%d].Index = %d, want %d", i, outcome.Index, items[i].Index)
		}
		if outcome.Skipped {
			t.Fatalf("outcomes[%d] unexpectedly skipped", i)
		}
		if outcome.Result == nil {
			t.Fatalf("outcomes[%d].Result is nil", i)
		}
		want := fmt.Sprintf("content-%s", items[i].Args["path"])
		if outcome.Result.ForLLM != want {
			t.Fatalf("outcomes[%d] = %q, want %q (model order broken)", i, outcome.Result.ForLLM, want)
		}
	}
}

func TestBatchRespectsParallelLimit(t *testing.T) {
	items := make([]toolBatchItem, 6)
	for i := range items {
		items[i] = toolBatchItem{Index: i, Tool: "read_file"}
	}

	var active, maxSeen atomic.Int64
	execFn := func(_ context.Context, _ int, _ string, _ map[string]any) (*tools.ToolResult, error) {
		current := active.Add(1)
		for {
			seen := maxSeen.Load()
			if current <= seen || maxSeen.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return tools.SilentResult("ok"), nil
	}

	outcomes := runToolBatch(context.Background(), items, execFn, 2, nil, nil)
	if len(outcomes) != len(items) {
		t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(items))
	}
	if got := maxSeen.Load(); got > 2 {
		t.Fatalf("observed %d concurrent executions, want <= 2", got)
	}
	if got := maxSeen.Load(); got < 2 {
		t.Fatalf("observed %d concurrent executions, want 2 (rolling pool inactive)", got)
	}
}

func TestBatchExclusiveBarrier(t *testing.T) {
	items := []toolBatchItem{
		{Index: 0, Tool: "read_file"},
		{Index: 1, Tool: "read_file"},
		{Index: 2, Tool: "exec"}, // 独占屏障
		{Index: 3, Tool: "read_file"},
	}

	var mu sync.Mutex
	var sequence []string
	release := make(chan struct{})
	startedSafe := make(chan struct{}, 2)
	var activeSafe atomic.Int64

	execFn := func(_ context.Context, index int, tool string, _ map[string]any) (*tools.ToolResult, error) {
		if tool == "exec" {
			mu.Lock()
			sequence = append(sequence, "exec")
			mu.Unlock()
			if n := activeSafe.Load(); n != 0 {
				t.Errorf("exclusive barrier started with %d parallel-safe calls still running", n)
			}
			if index != 2 {
				t.Errorf("exclusive call index = %d, want 2", index)
			}
			return tools.SilentResult("exclusive"), nil
		}
		activeSafe.Add(1)
		startedSafe <- struct{}{}
		<-release
		activeSafe.Add(-1)
		mu.Lock()
		sequence = append(sequence, fmt.Sprintf("safe-%d", index))
		mu.Unlock()
		return tools.SilentResult("safe"), nil
	}

	done := make(chan []toolBatchOutcome, 1)
	go func() {
		done <- runToolBatch(context.Background(), items, execFn, 4, nil, nil)
	}()

	// 等两个安全调用都在途后，确认独占调用尚未开始（它是屏障）。
	for i := 0; i < 2; i++ {
		select {
		case <-startedSafe:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for parallel-safe calls to start")
		}
	}
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	for _, entry := range sequence {
		if entry == "exec" {
			mu.Unlock()
			t.Fatal("exclusive call started before parallel-safe calls settled")
		}
	}
	mu.Unlock()

	close(release)
	var outcomes []toolBatchOutcome
	select {
	case outcomes = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for batch to finish")
	}
	if len(outcomes) != len(items) {
		t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(items))
	}
	// 尾部的安全调用（下标 3）必须在独占调用之后开始。
	mu.Lock()
	defer mu.Unlock()
	execPos, safe3Pos := -1, -1
	for i, entry := range sequence {
		switch entry {
		case "exec":
			execPos = i
		case "safe-3":
			safe3Pos = i
		}
	}
	if execPos < 0 || safe3Pos < 0 || execPos > safe3Pos {
		t.Fatalf("exclusive barrier order broken: %v", sequence)
	}
}

func TestBatchAbortSynthesizesResults(t *testing.T) {
	items := []toolBatchItem{
		{Index: 0, Tool: "read_file"},
		{Index: 1, Tool: "read_file"},
		{Index: 2, Tool: "read_file"},
		{Index: 3, Tool: "read_file"},
	}
	abort := make(chan struct{})
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once

	execFn := func(_ context.Context, index int, _ string, _ map[string]any) (*tools.ToolResult, error) {
		if index == 0 {
			startedOnce.Do(func() { close(started) })
			<-release
			return tools.SilentResult("first"), nil
		}
		return tools.SilentResult(fmt.Sprintf("unexpected-%d", index)), nil
	}

	// maxParallel=1 让调用 1..3 保持排队，使中止能在派发前抓到它们。
	done := make(chan []toolBatchOutcome, 1)
	go func() {
		done <- runToolBatch(context.Background(), items, execFn, 1, abort, nil)
	}()
	<-started
	close(abort)
	close(release)

	var outcomes []toolBatchOutcome
	select {
	case outcomes = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for aborted batch")
	}
	if len(outcomes) != len(items) {
		t.Fatalf("len(outcomes) = %d, want %d (tool-call/result invariant)", len(outcomes), len(items))
	}
	if outcomes[0].Skipped || outcomes[0].Result == nil || outcomes[0].Result.ForLLM != "first" {
		t.Fatalf("outcomes[0] = %+v, want executed result", outcomes[0])
	}
	for i := 1; i < len(outcomes); i++ {
		outcome := outcomes[i]
		if !outcome.Skipped {
			t.Fatalf("outcomes[%d] should be skipped after abort: %+v", i, outcome)
		}
		if outcome.SkipReason != toolBatchAbortedBeforeDispatch {
			t.Fatalf("outcomes[%d].SkipReason = %q, want %q", i, outcome.SkipReason, toolBatchAbortedBeforeDispatch)
		}
		if outcome.Result == nil || outcome.Result.ForLLM != toolBatchAbortedBeforeDispatch {
			t.Fatalf("outcomes[%d] missing synthesized abort result: %+v", i, outcome)
		}
		if outcome.Index != items[i].Index {
			t.Fatalf("outcomes[%d].Index = %d, want %d", i, outcome.Index, items[i].Index)
		}
	}
}

func TestBatchErrorIsolation(t *testing.T) {
	items := []toolBatchItem{
		{Index: 0, Tool: "read_file"},
		{Index: 1, Tool: "read_file"},
		{Index: 2, Tool: "read_file"},
	}
	execFn := func(_ context.Context, index int, _ string, _ map[string]any) (*tools.ToolResult, error) {
		switch index {
		case 0:
			return nil, fmt.Errorf("boom")
		case 1:
			panic("tool exploded")
		default:
			return tools.SilentResult("fine"), nil
		}
	}

	outcomes := runToolBatch(context.Background(), items, execFn, 3, nil, nil)
	if len(outcomes) != len(items) {
		t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(items))
	}
	if outcomes[0].Result == nil || !outcomes[0].Result.IsError || !strings.Contains(outcomes[0].Result.ForLLM, "boom") {
		t.Fatalf("error from executor not isolated: %+v", outcomes[0])
	}
	if outcomes[1].Result == nil || !outcomes[1].Result.IsError || !strings.Contains(outcomes[1].Result.ForLLM, "panic") {
		t.Fatalf("panic not isolated into an error result: %+v", outcomes[1])
	}
	if outcomes[2].Result == nil || outcomes[2].Result.IsError || outcomes[2].Result.ForLLM != "fine" {
		t.Fatalf("healthy call affected by sibling failures: %+v", outcomes[2])
	}
}

func TestBatchAllSafeParallel(t *testing.T) {
	items := []toolBatchItem{
		{Index: 0, Tool: "read_file"},
		{Index: 1, Tool: "list_dir"},
		{Index: 2, Tool: "web_search"},
	}
	const wantConcurrent = 3
	started := make(chan struct{}, wantConcurrent)
	release := make(chan struct{})

	execFn := func(_ context.Context, _ int, _ string, _ map[string]any) (*tools.ToolResult, error) {
		started <- struct{}{}
		<-release
		return tools.SilentResult("ok"), nil
	}

	done := make(chan []toolBatchOutcome, 1)
	go func() { done <- runToolBatch(context.Background(), items, execFn, wantConcurrent, nil, nil) }()

	for i := 0; i < wantConcurrent; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d calls started concurrently", i, wantConcurrent)
		}
	}
	close(release)
	select {
	case outcomes := <-done:
		if len(outcomes) != len(items) {
			t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(items))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for parallel batch")
	}
}

// measureBatchConcurrency 用 isSafe 判定跑完整个批次，返回观测到的最大并发数。
func measureBatchConcurrency(t *testing.T, items []toolBatchItem, isSafe func(tool string) bool) int64 {
	t.Helper()

	var active, maxSeen atomic.Int64
	execFn := func(_ context.Context, _ int, _ string, _ map[string]any) (*tools.ToolResult, error) {
		current := active.Add(1)
		for {
			seen := maxSeen.Load()
			if current <= seen || maxSeen.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		active.Add(-1)
		return tools.SilentResult("ok"), nil
	}

	outcomes := runToolBatch(context.Background(), items, execFn, 4, nil, isSafe)
	if len(outcomes) != len(items) {
		t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(items))
	}
	return maxSeen.Load()
}

// TestBatchIsSafeOverridesNameAllowlist 覆盖“名称在并行允许列表中，但工具能力
// 声明不可并行”的场景：分组必须与准入使用同一 isSafe 判定，把该调用当独占屏障。
func TestBatchIsSafeOverridesNameAllowlist(t *testing.T) {
	if !tools.IsParallelSafe("read_file") {
		t.Fatal("precondition: read_file should be name-level parallel safe")
	}
	items := []toolBatchItem{
		{Index: 0, Tool: "read_file"},
		{Index: 1, Tool: "list_dir"},
		{Index: 2, Tool: "list_dir"},
	}
	// 能力判定说 read_file 不可并行，list_dir 可并行。
	isSafe := func(tool string) bool { return tool == "list_dir" }

	exclusiveStarted := make(chan struct{})
	exclusiveRelease := make(chan struct{})
	safeStarted := make(chan struct{}, 2)
	safeRelease := make(chan struct{})

	execFn := func(_ context.Context, index int, tool string, _ map[string]any) (*tools.ToolResult, error) {
		if tool == "read_file" {
			close(exclusiveStarted)
			<-exclusiveRelease
			return tools.SilentResult("exclusive"), nil
		}
		safeStarted <- struct{}{}
		<-safeRelease
		return tools.SilentResult(fmt.Sprintf("safe-%d", index)), nil
	}

	done := make(chan []toolBatchOutcome, 1)
	go func() { done <- runToolBatch(context.Background(), items, execFn, 4, nil, isSafe) }()

	select {
	case <-exclusiveStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("exclusive read_file call never started")
	}
	// 独占调用在途时并行组不得开始：名称级允许列表不能绕过能力判定。
	select {
	case <-safeStarted:
		t.Fatal("parallel-safe group started while exclusive capability-unsafe call was running")
	case <-time.After(100 * time.Millisecond):
	}

	close(exclusiveRelease)
	for i := 0; i < 2; i++ {
		select {
		case <-safeStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 parallel-safe calls started after the exclusive barrier", i)
		}
	}
	close(safeRelease)

	select {
	case outcomes := <-done:
		if len(outcomes) != len(items) {
			t.Fatalf("len(outcomes) = %d, want %d", len(outcomes), len(items))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for batch with capability-unsafe call")
	}
}

// TestBatchUnknownToolStaysExclusive 覆盖“名称在允许表中，但注册表无法确认其
// 并行能力（未注册/未知工具）”的保守语义：isSafe 返回 false 时必须串行。
func TestBatchUnknownToolStaysExclusive(t *testing.T) {
	if !tools.IsParallelSafe("read_file") {
		t.Fatal("precondition: read_file should be name-level parallel safe")
	}
	items := []toolBatchItem{
		{Index: 0, Tool: "read_file"},
		{Index: 1, Tool: "read_file"},
	}
	// 模拟调用方按 registry 查询失败：未知工具一律不可并行。
	isSafe := func(string) bool { return false }
	if got := measureBatchConcurrency(t, items, isSafe); got != 1 {
		t.Fatalf("max concurrent = %d, want 1 (unknown/unsafe tools must stay exclusive)", got)
	}
}

// TestBatchCapabilityEnablesNonAllowlistedTool 覆盖反向情形：名称不在允许表中，
// 但能力判定为可并行时，分组仍应并行（判定以 isSafe 为唯一来源）。
func TestBatchCapabilityEnablesNonAllowlistedTool(t *testing.T) {
	if tools.IsParallelSafe("mystery_tool") {
		t.Fatal("precondition: mystery_tool should not be name-level parallel safe")
	}
	items := []toolBatchItem{
		{Index: 0, Tool: "mystery_tool"},
		{Index: 1, Tool: "mystery_tool"},
	}
	isSafe := func(tool string) bool { return tool == "mystery_tool" }
	if got := measureBatchConcurrency(t, items, isSafe); got != 2 {
		t.Fatalf("max concurrent = %d, want 2 (capability-safe tool should be grouped)", got)
	}
}

// -----------------------------------------------------------------------------
// Pipeline 集成测试
// -----------------------------------------------------------------------------

// batchProbeTool 是 fake read_file 实现，记录同时运行了多少个实例。
type batchProbeTool struct {
	active   atomic.Int64
	maxSeen  atomic.Int64
	executed atomic.Int64

	// 两次执行重叠后关闭 bothStarted（串行执行器则永不关闭）。
	// 它让测试保持快速：不需要睡眠轮询。
	bothStarted chan struct{}
	once        sync.Once
}

func newBatchProbeTool() *batchProbeTool {
	return &batchProbeTool{bothStarted: make(chan struct{})}
}

func (t *batchProbeTool) Name() string        { return "read_file" }
func (t *batchProbeTool) Description() string { return "fake read_file for batch tests" }
func (t *batchProbeTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []string{"path"},
	}
}

// ConcurrencySafe 让该 fake 显式选择并行路径，
// 从而检验能力接口，而不只是名称级允许列表。
func (t *batchProbeTool) ConcurrencySafe() bool { return true }

func (t *batchProbeTool) Execute(_ context.Context, args map[string]any) *tools.ToolResult {
	t.executed.Add(1)
	current := t.active.Add(1)
	for {
		seen := t.maxSeen.Load()
		if current <= seen || t.maxSeen.CompareAndSwap(seen, current) {
			break
		}
	}
	// 当调度器并发运行我们时与兄弟调用重叠。串行执行器会短暂等待且从不发出 bothStarted，
	// 断言通过 maxSeen 检测到这一点。
	if t.active.Load() >= 2 {
		t.once.Do(func() { close(t.bothStarted) })
	}
	select {
	case <-t.bothStarted:
	case <-time.After(250 * time.Millisecond):
	}
	t.active.Add(-1)
	path, _ := args["path"].(string)
	return tools.SilentResult("content:" + path)
}

// batchCaptureProvider 在首个请求返回两个工具调用，之后返回最终答案，
// 并捕获每个请求发送的消息。
type batchCaptureProvider struct {
	mu       sync.Mutex
	calls    int
	requests [][]providers.Message
}

func (p *batchCaptureProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.requests = append(p.requests, append([]providers.Message(nil), messages...))
	if p.calls == 1 {
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{
					ID:        "call_1",
					Type:      "function",
					Name:      "read_file",
					Arguments: map[string]any{"path": "a.txt"},
					Function: &providers.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"a.txt"}`,
					},
				},
				{
					ID:        "call_2",
					Type:      "function",
					Name:      "read_file",
					Arguments: map[string]any{"path": "b.txt"},
					Function: &providers.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"b.txt"}`,
					},
				},
			},
		}, nil
	}
	return &providers.LLMResponse{Content: "done", ToolCalls: []providers.ToolCall{}}, nil
}

func (p *batchCaptureProvider) GetDefaultModel() string { return "batch-mock" }

// batchCapabilityProbe 是可按名字/能力定制的 fake 工具。
type batchCapabilityProbe struct {
	name string
	safe bool
	exec func(ctx context.Context) *tools.ToolResult
}

func (p *batchCapabilityProbe) Name() string        { return p.name }
func (p *batchCapabilityProbe) Description() string { return "capability probe " + p.name }
func (p *batchCapabilityProbe) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{"type": "string"},
			"path":    map[string]any{"type": "string"},
		},
	}
}

// ConcurrencySafe 显式声明能力：这个名字在并行允许列表中的工具也可以声明不可并行。
func (p *batchCapabilityProbe) ConcurrencySafe() bool { return p.safe }

func (p *batchCapabilityProbe) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	if p.exec != nil {
		return p.exec(ctx)
	}
	return tools.SilentResult(p.name)
}

// batchMixedCapabilityProvider 让模型在同一步请求 [grep, grep, read_file]：
// 两个 grep 相邻且能力安全（触发并发批次路径），read_file 名称在允许列表中
// 但实现声明 ConcurrencySafe()=false。
type batchMixedCapabilityProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *batchMixedCapabilityProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return &providers.LLMResponse{
			ToolCalls: []providers.ToolCall{
				{ID: "call_1", Type: "function", Name: "grep", Arguments: map[string]any{"pattern": "a"}},
				{ID: "call_2", Type: "function", Name: "grep", Arguments: map[string]any{"pattern": "b"}},
				{ID: "call_3", Type: "function", Name: "read_file", Arguments: map[string]any{"path": "x"}},
			},
		}, nil
	}
	return &providers.LLMResponse{Content: "done", ToolCalls: []providers.ToolCall{}}, nil
}

func (p *batchMixedCapabilityProvider) GetDefaultModel() string { return "batch-mock" }

// TestExecuteToolsConcurrentBatchUsesCapabilityNotName 是分组/准入不一致的回归测试：
// read_file 名称在允许表中，但注册工具声明 ConcurrencySafe()=false，因此它必须在
// 两个能力安全的 grep 调用之后独占运行，而不能按名称被并入并发组。
func TestExecuteToolsConcurrentBatchUsesCapabilityNotName(t *testing.T) {
	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         workspace,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, &batchMixedCapabilityProvider{})

	var mu sync.Mutex
	var sequence []string
	record := func(entry string) {
		mu.Lock()
		sequence = append(sequence, entry)
		mu.Unlock()
	}

	grepsStarted := make(chan struct{}, 2)
	releaseGreps := make(chan struct{})
	activeGreps := atomic.Int64{}

	grepProbe := &batchCapabilityProbe{name: "grep", safe: true, exec: func(context.Context) *tools.ToolResult {
		activeGreps.Add(1)
		record("grep")
		grepsStarted <- struct{}{}
		<-releaseGreps
		activeGreps.Add(-1)
		return tools.SilentResult("grep-ok")
	}}
	readFileProbe := &batchCapabilityProbe{name: "read_file", safe: false, exec: func(context.Context) *tools.ToolResult {
		record("read_file")
		return tools.SilentResult("read-ok")
	}}
	al.RegisterTool(grepProbe)
	al.RegisterTool(readFileProbe)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	type turnResult struct {
		resp string
		err  error
	}
	done := make(chan turnResult, 1)
	go func() {
		resp, err := al.ProcessDirectWithChannel(ctx, "grep twice then read", "capability-session", "test", "chat1")
		done <- turnResult{resp: resp, err: err}
	}()

	// 两个 grep 必须并发启动（它们构成并行安全组）。
	for i := 0; i < 2; i++ {
		select {
		case <-grepsStarted:
		case <-time.After(5 * time.Second):
			close(releaseGreps)
			select {
			case result := <-done:
				t.Fatalf("only %d of 2 capability-safe greps started concurrently; turn already finished: resp=%q err=%v",
					i, result.resp, result.err)
			default:
			}
			t.Fatalf("only %d of 2 capability-safe greps started concurrently", i)
		}
	}

	// grep 组在途期间，能力不安全的 read_file 不得开始。
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	readFileStartedEarly := false
	for _, entry := range sequence {
		if entry == "read_file" {
			readFileStartedEarly = true
		}
	}
	mu.Unlock()
	close(releaseGreps)
	if readFileStartedEarly {
		t.Fatal("read_file started while the capability-safe grep group was still running: grouping ignored ConcurrencySafe()")
	}

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("ProcessDirectWithChannel: %v", result.err)
		}
		if result.resp != "done" {
			t.Fatalf("response = %q, want done", result.resp)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for turn to finish")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sequence) != 3 || sequence[0] != "grep" || sequence[1] != "grep" || sequence[2] != "read_file" {
		t.Fatalf("execution sequence = %v, want two greps then exclusive read_file", sequence)
	}
}

func TestExecuteToolsConcurrentBatchReadFileOrder(t *testing.T) {
	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         workspace,
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 10,
			},
		},
	}
	provider := &batchCaptureProvider{}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)

	probe := newBatchProbeTool()
	al.RegisterTool(probe)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := al.ProcessDirectWithChannel(ctx, "read both files", "batch-session", "test", "chat1")
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if resp != "done" {
		t.Fatalf("response = %q, want done", resp)
	}
	if got := probe.executed.Load(); got != 2 {
		t.Fatalf("probe executed %d calls, want 2", got)
	}
	if got := probe.maxSeen.Load(); got < 2 {
		t.Fatalf("read_file calls ran serially (max concurrent = %d), want parallel batch", got)
	}

	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if len(requests) < 2 {
		t.Fatalf("provider received %d requests, want at least 2", len(requests))
	}

	// 后续请求必须按模型顺序携带两个工具结果，
	// 并与 assistant 的 tool_calls id 配对。
	second := requests[1]
	var toolCallIDs []string
	var toolResultIDs []string
	for _, msg := range second {
		for _, tc := range msg.ToolCalls {
			toolCallIDs = append(toolCallIDs, tc.ID)
		}
		if msg.Role == "tool" {
			toolResultIDs = append(toolResultIDs, msg.ToolCallID)
		}
	}
	if len(toolCallIDs) != 2 || toolCallIDs[0] != "call_1" || toolCallIDs[1] != "call_2" {
		t.Fatalf("assistant tool call order = %v, want [call_1 call_2]", toolCallIDs)
	}
	if len(toolResultIDs) != 2 || toolResultIDs[0] != "call_1" || toolResultIDs[1] != "call_2" {
		t.Fatalf("tool result order = %v, want model order [call_1 call_2]", toolResultIDs)
	}
	var contents []string
	for _, msg := range second {
		if msg.Role == "tool" {
			contents = append(contents, msg.Content)
		}
	}
	if len(contents) != 2 || !strings.Contains(contents[0], "a.txt") || !strings.Contains(contents[1], "b.txt") {
		t.Fatalf("tool result contents = %v, want a.txt then b.txt", contents)
	}
}

func TestBatchFallsBackSequentialWhenHooks(t *testing.T) {
	registry := tools.NewToolRegistry()
	registry.Register(newBatchProbeTool())

	ts := &turnState{
		agent:      &AgentInstance{Tools: registry},
		sessionKey: "batch-hooks-session",
	}
	exec := &turnExecution{
		normalizedToolCalls: []providers.ToolCall{
			{ID: "call_1", Name: "read_file", Arguments: map[string]any{"path": "a"}},
			{ID: "call_2", Name: "read_file", Arguments: map[string]any{"path": "b"}},
		},
	}

	// 未挂载 hook：批次路径可用。
	al := &AgentLoop{hooks: NewHookManager(nil)}
	pipeline := &Pipeline{al: al}
	if !pipeline.shouldUseConcurrentToolBatch(ts, exec) {
		t.Fatal("batch path should be eligible without mounted hooks")
	}

	// 只要挂载了一个 hook 就必须强制走串行路径。
	if err := al.MountHook(NamedHook("batch-test-hook", &batchNoopHook{})); err != nil {
		t.Fatalf("MountHook: %v", err)
	}
	if pipeline.shouldUseConcurrentToolBatch(ts, exec) {
		t.Fatal("batch path must fall back to serial when hooks are mounted")
	}

	// 同样要求至少两个调用。
	execOne := &turnExecution{normalizedToolCalls: exec.normalizedToolCalls[:1]}
	al.UnmountHook("batch-test-hook")
	al.hooks = nil
	if pipeline.shouldUseConcurrentToolBatch(ts, execOne) {
		t.Fatal("single-call turns must keep the serial path")
	}
}

// batchNoopHook 是惰性 hook 注册，用于让 hook 管理器报告存在活动 hook 集合。
type batchNoopHook struct{}
