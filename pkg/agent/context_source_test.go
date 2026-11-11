// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// testContextSource 是 T4.1 测试使用的脚本化 ContextSource。
// panickingDescriptorSource 在报告描述符时 panic；
// registry 必须把它转换为错误，而不是让进程崩溃。
type panickingDescriptorSource struct{}

func (panickingDescriptorSource) ContextSource() PromptSourceDescriptor {
	panic("descriptor boom")
}

func (panickingDescriptorSource) ContributeContext(
	_ context.Context,
	_ PromptBuildRequest,
) ([]ContextContribution, error) {
	return nil, nil
}

type testContextSource struct {
	desc          PromptSourceDescriptor
	contributions []ContextContribution
	err           error
	panicValue    any
	calls         int
}

func (s *testContextSource) ContextSource() PromptSourceDescriptor {
	return s.desc
}

func (s *testContextSource) ContributeContext(
	_ context.Context,
	_ PromptBuildRequest,
) ([]ContextContribution, error) {
	s.calls++
	if s.panicValue != nil {
		panic(s.panicValue)
	}
	if s.err != nil {
		return nil, s.err
	}
	return append([]ContextContribution(nil), s.contributions...), nil
}

func newTestContextSource(id string, order int, text string) *testContextSource {
	return &testContextSource{
		desc: PromptSourceDescriptor{
			ID:    PromptSourceID(id),
			Owner: "test",
		},
		contributions: []ContextContribution{{
			Text:  text,
			Order: order,
			// 给每个辅助贡献独立的 scope，避免默认的 kind+scope 去重把测试夹具合并。
			Meta: protocoltypes.ContextMeta{Kind: "test-context", BaselineIdentity: id},
		}},
	}
}

func TestRegisterContextSource(t *testing.T) {
	t.Run("valid source is collected", func(t *testing.T) {
		registry := NewPromptRegistry()
		source := newTestContextSource("plugin:demo", 10, "演示上下文")
		if err := registry.RegisterContextSource(source); err != nil {
			t.Fatalf("RegisterContextSource() error = %v", err)
		}
		messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
		if len(messages) != 1 || messages[0].Content != "演示上下文" {
			t.Fatalf("CollectContext() = %+v, want one message with the contribution text", messages)
		}
		if messages[0].Context == nil || messages[0].Context.Kind != "test-context" {
			t.Fatalf("message context meta = %+v, want kind test-context", messages[0].Context)
		}
		if messages[0].PromptSource != "plugin:demo" {
			t.Fatalf("message PromptSource = %q, want plugin:demo", messages[0].PromptSource)
		}
		if messages[0].Role != "user" {
			t.Fatalf("message Role = %q, want user", messages[0].Role)
		}
	})

	t.Run("duplicate id is rejected", func(t *testing.T) {
		registry := NewPromptRegistry()
		if err := registry.RegisterContextSource(newTestContextSource("plugin:dup", 0, "a")); err != nil {
			t.Fatalf("first RegisterContextSource() error = %v", err)
		}
		err := registry.RegisterContextSource(newTestContextSource("plugin:dup", 1, "b"))
		if err == nil {
			t.Fatal("RegisterContextSource() with duplicate id should fail")
		}
		if !strings.Contains(err.Error(), "already registered") {
			t.Fatalf("duplicate error = %v, want already registered", err)
		}
	})

	t.Run("invalid descriptors are rejected", func(t *testing.T) {
		registry := NewPromptRegistry()
		if err := registry.RegisterContextSource(nil); err == nil {
			t.Fatal("nil context source should be rejected")
		}
		missingID := &testContextSource{desc: PromptSourceDescriptor{Owner: "test"}}
		if err := registry.RegisterContextSource(missingID); err == nil {
			t.Fatal("context source without id should be rejected")
		}
		missingOwner := &testContextSource{desc: PromptSourceDescriptor{ID: "plugin:no-owner"}}
		if err := registry.RegisterContextSource(missingOwner); err == nil {
			t.Fatal("context source without owner should be rejected")
		}
		if err := registry.RegisterContextSource(panickingDescriptorSource{}); err == nil {
			t.Fatal("panicking descriptor should be rejected with an error")
		}
	})

	t.Run("no registered source keeps nil behaviour", func(t *testing.T) {
		registry := NewPromptRegistry()
		if got := registry.CollectContext(context.Background(), PromptBuildRequest{}); got != nil {
			t.Fatalf("CollectContext() = %+v, want nil without registered sources", got)
		}
		var nilRegistry *PromptRegistry
		if got := nilRegistry.CollectContext(context.Background(), PromptBuildRequest{}); got != nil {
			t.Fatalf("nil registry CollectContext() = %+v, want nil", got)
		}
	})

	t.Run("builder helper registers and collects", func(t *testing.T) {
		builder := newIsolatedSkillCatalogBuilder(t, nil)
		source := newTestContextSource("plugin:builder", 5, "builder 插件上下文")
		if err := builder.RegisterContextSource(source); err != nil {
			t.Fatalf("RegisterContextSource() error = %v", err)
		}
		messages := builder.CollectContextMessages(context.Background(), PromptBuildRequest{})
		// 该工作区的技能目录为空，因此只期望插件贡献。
		if len(messages) != 1 || messages[0].Content != "builder 插件上下文" {
			t.Fatalf("CollectContextMessages() = %+v, want plugin contribution only", messages)
		}
		if called := source.calls; called != 1 {
			t.Fatalf("source calls = %d, want 1", called)
		}
	})
}

func TestContextSourceOrder(t *testing.T) {
	registry := NewPromptRegistry()
	// 注册顺序故意与贡献顺序不同。
	late := newTestContextSource("ctx:z", 200, "order-200")
	firstB := newTestContextSource("ctx:b", 100, "order-100-b")
	firstC := newTestContextSource("ctx:c", 100, "order-100-c")
	for _, source := range []ContextSource{late, firstC, firstB} {
		if err := registry.RegisterContextSource(source); err != nil {
			t.Fatalf("RegisterContextSource() error = %v", err)
		}
	}

	messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
	got := make([]string, 0, len(messages))
	for _, msg := range messages {
		got = append(got, msg.Content)
	}
	want := []string{"order-100-b", "order-100-c", "order-200"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("context order = %v, want %v", got, want)
	}
}

func TestContextSourceRequired(t *testing.T) {
	t.Run("required survives optional suppression", func(t *testing.T) {
		registry := NewPromptRegistry()
		required := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:required", Owner: "test"},
			contributions: []ContextContribution{{
				Text:     "任务状态：必须保留",
				Required: true,
				Order:    10,
				Meta:     protocoltypes.ContextMeta{Kind: "task-state"},
			}},
		}
		optional := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:optional", Owner: "test"},
			contributions: []ContextContribution{{
				Text:  "可选提示：可以丢弃",
				Order: 20,
				Meta:  protocoltypes.ContextMeta{Kind: "optional-context"},
			}},
		}
		for _, source := range []ContextSource{required, optional} {
			if err := registry.RegisterContextSource(source); err != nil {
				t.Fatalf("RegisterContextSource() error = %v", err)
			}
		}

		messages := registry.CollectContext(context.Background(), PromptBuildRequest{
			SuppressOptionalContext: true,
		})
		if len(messages) != 1 || messages[0].Content != "任务状态：必须保留" {
			t.Fatalf("CollectContext(SuppressOptionalContext) = %+v, want only the required message", messages)
		}
	})

	t.Run("required wins over later optional with same kind and scope", func(t *testing.T) {
		registry := NewPromptRegistry()
		required := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:required", Owner: "test"},
			contributions: []ContextContribution{{
				Text:     "required-wins",
				Required: true,
				Order:    10,
				Meta:     protocoltypes.ContextMeta{Kind: "shared-kind"},
			}},
		}
		optional := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:optional", Owner: "test"},
			contributions: []ContextContribution{{
				Text:  "optional-loses",
				Order: 20,
				Meta:  protocoltypes.ContextMeta{Kind: "shared-kind"},
			}},
		}
		for _, source := range []ContextSource{required, optional} {
			if err := registry.RegisterContextSource(source); err != nil {
				t.Fatalf("RegisterContextSource() error = %v", err)
			}
		}

		messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
		if len(messages) != 1 || messages[0].Content != "required-wins" {
			t.Fatalf("CollectContext() = %+v, want required contribution to win", messages)
		}
	})

	t.Run("later required replaces earlier optional", func(t *testing.T) {
		registry := NewPromptRegistry()
		optional := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:optional", Owner: "test"},
			contributions: []ContextContribution{{
				Text:  "optional-first",
				Order: 10,
				Meta:  protocoltypes.ContextMeta{Kind: "shared-kind"},
			}},
		}
		required := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:required", Owner: "test"},
			contributions: []ContextContribution{{
				Text:     "required-second",
				Required: true,
				Order:    20,
				Meta:     protocoltypes.ContextMeta{Kind: "shared-kind"},
			}},
		}
		for _, source := range []ContextSource{optional, required} {
			if err := registry.RegisterContextSource(source); err != nil {
				t.Fatalf("RegisterContextSource() error = %v", err)
			}
		}

		messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
		if len(messages) != 1 || messages[0].Content != "required-second" {
			t.Fatalf("CollectContext() = %+v, want later required contribution", messages)
		}
	})
}

func TestContextSourceDedupe(t *testing.T) {
	t.Run("same kind and scope keeps the last contribution", func(t *testing.T) {
		registry := NewPromptRegistry()
		first := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:first", Owner: "test"},
			contributions: []ContextContribution{{
				Text:  "旧内容",
				Order: 10,
				Meta: protocoltypes.ContextMeta{
					Kind:             protocoltypes.ContextKindWorkspaceInstructions,
					BaselineIdentity: "AGENTS.md",
				},
			}},
		}
		second := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:second", Owner: "test"},
			contributions: []ContextContribution{{
				Text:  "新内容",
				Order: 20,
				Meta: protocoltypes.ContextMeta{
					Kind:             protocoltypes.ContextKindWorkspaceInstructions,
					BaselineIdentity: "AGENTS.md",
				},
			}},
		}
		for _, source := range []ContextSource{first, second} {
			if err := registry.RegisterContextSource(source); err != nil {
				t.Fatalf("RegisterContextSource() error = %v", err)
			}
		}

		messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
		if len(messages) != 1 || messages[0].Content != "新内容" {
			t.Fatalf("CollectContext() = %+v, want the later contribution to win", messages)
		}
	})

	t.Run("different scopes are kept", func(t *testing.T) {
		registry := NewPromptRegistry()
		source := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:scoped", Owner: "test"},
			contributions: []ContextContribution{
				{
					Text:  "AGENTS.md 内容",
					Order: 10,
					Meta: protocoltypes.ContextMeta{
						Kind:             protocoltypes.ContextKindWorkspaceInstructions,
						BaselineIdentity: "AGENTS.md",
					},
				},
				{
					Text:  "CLAUDE.md 内容",
					Order: 11,
					Meta: protocoltypes.ContextMeta{
						Kind:             protocoltypes.ContextKindWorkspaceInstructions,
						BaselineIdentity: "CLAUDE.md",
					},
				},
			},
		}
		if err := registry.RegisterContextSource(source); err != nil {
			t.Fatalf("RegisterContextSource() error = %v", err)
		}

		messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
		if len(messages) != 2 {
			t.Fatalf("CollectContext() returned %d messages, want 2 (distinct scopes)", len(messages))
		}
	})
}

func TestContextSourceEmptyIgnored(t *testing.T) {
	registry := NewPromptRegistry()
	source := &testContextSource{
		desc: PromptSourceDescriptor{ID: "ctx:empty", Owner: "test"},
		contributions: []ContextContribution{
			{Text: "", Order: 0},
			{Text: "   \n\t ", Order: 1},
			{Text: "有效内容", Order: 2, Meta: protocoltypes.ContextMeta{Kind: "valid"}},
		},
	}
	if err := registry.RegisterContextSource(source); err != nil {
		t.Fatalf("RegisterContextSource() error = %v", err)
	}

	messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
	if len(messages) != 1 || messages[0].Content != "有效内容" {
		t.Fatalf("CollectContext() = %+v, want only the non-empty contribution", messages)
	}
}

func TestContextSourceErrorWarn(t *testing.T) {
	t.Run("failing source is skipped", func(t *testing.T) {
		registry := NewPromptRegistry()
		failing := &testContextSource{
			desc: PromptSourceDescriptor{ID: "ctx:failing", Owner: "test"},
			err:  errors.New("boom"),
		}
		working := newTestContextSource("ctx:working", 10, "仍然注入")
		for _, source := range []ContextSource{failing, working} {
			if err := registry.RegisterContextSource(source); err != nil {
				t.Fatalf("RegisterContextSource() error = %v", err)
			}
		}

		messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
		if len(messages) != 1 || messages[0].Content != "仍然注入" {
			t.Fatalf("CollectContext() = %+v, want only the working source's message", messages)
		}
	})

	t.Run("panicking source does not fail collection", func(t *testing.T) {
		registry := NewPromptRegistry()
		panicking := &testContextSource{
			desc:       PromptSourceDescriptor{ID: "ctx:panicking", Owner: "test"},
			panicValue: "unexpected panic",
		}
		working := newTestContextSource("ctx:working", 10, "panic 之后仍然注入")
		for _, source := range []ContextSource{panicking, working} {
			if err := registry.RegisterContextSource(source); err != nil {
				t.Fatalf("RegisterContextSource() error = %v", err)
			}
		}

		messages := registry.CollectContext(context.Background(), PromptBuildRequest{})
		if len(messages) != 1 || messages[0].Content != "panic 之后仍然注入" {
			t.Fatalf("CollectContext() = %+v, want panic to be contained", messages)
		}
	})
}
