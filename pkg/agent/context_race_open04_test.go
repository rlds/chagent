// OPEN-04 regression: asynchronous summarize must not drop durable context
// injected while its LLM call is in flight, and must not overwrite a newer
// compression summary/history rewrite (version CAS on the final apply).
package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// open04BlockingProvider blocks inside Chat until release is closed, so the
// test can deterministically inject durable context during the summarization
// LLM call (which normally takes seconds).
type open04BlockingProvider struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	calls     atomic.Int32
}

func newOpen04BlockingProvider() *open04BlockingProvider {
	return &open04BlockingProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *open04BlockingProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls.Add(1)
	p.startOnce.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return &providers.LLMResponse{Content: "OPEN04_SUMMARY"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (p *open04BlockingProvider) GetDefaultModel() string { return "open04-mock" }

func (p *open04BlockingProvider) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("summarization provider was not called; summarizeSession did not start")
	}
}

func (p *open04BlockingProvider) callCount() int { return int(p.calls.Load()) }

// open04SummarizeHistory is the fixed pre-summarize history used by the tests:
// real turns interleaved with one durable workspace-instructions context message.
func open04SummarizeHistory() []providers.Message {
	return []providers.Message{
		msgUser("OPEN04 Q1"),
		msgAssistant("OPEN04 A1"),
		msgUser("OPEN04 Q2"),
		msgAssistant("OPEN04 A2"),
		userContextMsg(protocoltypes.ContextKindWorkspaceInstructions, "OPEN04 CTX-OLD"),
		msgUser("OPEN04 Q3"),
		msgAssistant("OPEN04 A3"),
		msgUser("OPEN04 Q4"),
		msgAssistant("OPEN04 A4"),
		msgUser("OPEN04 Q5"),
	}
}

// runOpen04SummarizeWithMutation starts summarizeSession in a goroutine, waits
// until its provider call is in flight, runs mutate, then releases the LLM call
// and returns the resulting history/summary snapshot.
func runOpen04SummarizeWithMutation(
	t *testing.T,
	window, maxTokens int,
	provider *open04BlockingProvider,
	sessionKey string,
	history []providers.Message,
	mutate func(agent *AgentInstance),
) ([]providers.Message, string) {
	t.Helper()
	_, agent, lcm := newCompressionTestLoop(t, window, maxTokens, provider)
	agent.Sessions.SetHistory(sessionKey, history)

	done := make(chan struct{})
	go func() {
		defer close(done)
		lcm.summarizeSession(agent, sessionKey)
	}()

	provider.waitStarted(t)
	mutate(agent)
	close(provider.release)
	<-done

	return agent.Sessions.GetHistory(sessionKey), agent.Sessions.GetSummary(sessionKey)
}

// TestOpen04SummarizeKeepsContextInjectedDuringLLM is the primary regression:
// a durable context message appended while the summarizer is waiting on the
// model must survive the final truncation. Pre-fix, truncation used a stale
// keepCount, so it dropped the oldest message of the retained tail (here Q3)
// instead of only the summarized prefix.
func TestOpen04SummarizeKeepsContextInjectedDuringLLM(t *testing.T) {
	provider := newOpen04BlockingProvider()
	history := open04SummarizeHistory()
	safeCut := findSafeBoundary(history, len(history)-4)
	if safeCut <= 0 {
		t.Fatalf("unexpected safeCut %d for test history", safeCut)
	}
	oldTail := history[safeCut:]

	const sessionKey = "open04-summarize-context-race"
	injected := userContextMsg(protocoltypes.ContextKindRuntimeContext, "OPEN04 CTX-DURING-SUMMARIZE")

	got, summary := runOpen04SummarizeWithMutation(t, 100_000, 4096, provider, sessionKey, history,
		func(agent *AgentInstance) {
			agent.Sessions.AddFullMessage(sessionKey, injected)
		})

	if provider.callCount() != 1 {
		t.Fatalf("summarize provider calls = %d, want 1 (test history must fit one batch)", provider.callCount())
	}
	if summary != "OPEN04_SUMMARY" {
		t.Fatalf("summary = %q, want OPEN04_SUMMARY", summary)
	}

	want := append(append([]providers.Message{}, oldTail...), injected)
	if len(got) != len(want) {
		t.Fatalf("history len = %d, want %d (retained tail + context injected during summarize)\n kept=%s",
			len(got), len(want), open04Contents(got))
	}
	for i := range want {
		if !open04SameMessage(got[i], want[i]) {
			t.Fatalf("history[%d] = %q/%s, want %q/%s\n kept=%s", i,
				got[i].Role, got[i].Content, want[i].Role, want[i].Content, open04Contents(got))
		}
	}
	if !open04ContainsContext(got, "OPEN04 CTX-DURING-SUMMARIZE") {
		t.Fatalf("durable context injected during summarize was lost: %s", open04Contents(got))
	}
	if !open04ContainsContent(got, "OPEN04 Q3") {
		t.Fatalf("retained tail lost its oldest turn (OPEN04 Q3): %s", open04Contents(got))
	}
}

// TestOpen04SummarizeDoesNotOverwriteNewerSummary simulates a concurrent
// compression writing a fresh summary during the summarize LLM call: the
// summarize must abandon its stale apply instead of overwriting the newer
// compression note and truncating with stale offsets.
func TestOpen04SummarizeDoesNotOverwriteNewerSummary(t *testing.T) {
	provider := newOpen04BlockingProvider()
	history := open04SummarizeHistory()
	const sessionKey = "open04-summarize-summary-cas"
	const compressionNote = "【紧急压缩】OPEN04_COMPRESSION_NOTE"

	got, summary := runOpen04SummarizeWithMutation(t, 100_000, 4096, provider, sessionKey, history,
		func(agent *AgentInstance) {
			agent.Sessions.SetSummary(sessionKey, compressionNote)
		})

	if summary != compressionNote {
		t.Fatalf("summary = %q, want newer compression note %q (stale summarize must be abandoned)", summary, compressionNote)
	}
	if len(got) != len(history) {
		t.Fatalf("history len = %d, want %d (stale summarize must not truncate after a newer summary)",
			len(got), len(history))
	}
	if !open04SameHistory(got, history) {
		t.Fatalf("history changed by abandoned summarize: %s", open04Contents(got))
	}
}

// TestOpen04SummarizeAbandonsAfterHistoryRewrite simulates a concurrent
// compression/SetHistory replacing the history while summarize is in flight:
// the stale summarize must not apply its summary or truncate the new history.
func TestOpen04SummarizeAbandonsAfterHistoryRewrite(t *testing.T) {
	provider := newOpen04BlockingProvider()
	history := open04SummarizeHistory()
	const sessionKey = "open04-summarize-history-rewrite"
	rewritten := []providers.Message{
		msgUser("OPEN04 REWRITTEN Q"),
		msgAssistant("OPEN04 REWRITTEN A"),
		userContextMsg(protocoltypes.ContextKindRuntimeContext, "OPEN04 REWRITTEN CTX"),
	}

	got, summary := runOpen04SummarizeWithMutation(t, 100_000, 4096, provider, sessionKey, history,
		func(agent *AgentInstance) {
			agent.Sessions.SetHistory(sessionKey, rewritten)
		})

	if summary != "" {
		t.Fatalf("summary = %q, want empty (stale summarize must be abandoned after history rewrite)", summary)
	}
	if !open04SameHistory(got, rewritten) {
		t.Fatalf("history = %s, want rewritten history %s", open04Contents(got), open04Contents(rewritten))
	}
}

func open04Contents(history []providers.Message) string {
	out := ""
	for _, msg := range history {
		kind := ""
		if msg.Context != nil {
			kind = string(msg.Context.Kind)
		}
		out += "[" + msg.Role + ":" + kind + ":" + msg.Content + "]"
	}
	return out
}

func open04ContainsContent(history []providers.Message, want string) bool {
	for _, msg := range history {
		if msg.Content == want {
			return true
		}
	}
	return false
}

func open04ContainsContext(history []providers.Message, content string) bool {
	for _, msg := range history {
		if msg.Content == content && msg.Context != nil {
			return true
		}
	}
	return false
}

func open04SameMessage(a, b providers.Message) bool {
	if a.Role != b.Role || a.Content != b.Content || a.ToolCallID != b.ToolCallID {
		return false
	}
	if (a.Context == nil) != (b.Context == nil) {
		return false
	}
	if a.Context != nil && b.Context != nil {
		return a.Context.Kind == b.Context.Kind && a.Context.Digest == b.Context.Digest
	}
	return true
}

func open04SameHistory(got, want []providers.Message) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if !open04SameMessage(got[i], want[i]) {
			return false
		}
	}
	return true
}
