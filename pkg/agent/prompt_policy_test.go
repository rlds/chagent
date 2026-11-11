package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/config"
)

// 策略 prompt golden 测试（DSH P-01/P-02/P-11）。中文定稿定义在
// docs/design/agent-workspace-context-prompts.zh.md §4.10；这些测试同时锁定文本与“默认关闭”契约。

const (
	p01AutonomyHeader  = "## 自主运行"
	p01AutonomySnippet = "在任务完成前持续自主执行"
	p01BlockedSnippet  = "连续至少 3 轮无法用现有工具解决"
	p01EvidenceSnippet = "没有证据不得声称完成"
	p11CompleteSnippet = "只有当目标实际达成"
	p11BlockedSnippet  = "才可标记任务阻塞"
	p11NotBlocker      = "目标困难、结果不确定或仍有剩余工作，均不构成阻塞"
	p02WorkspaceHeader = "## 工作区指令"
	p02Precedence      = "更具体的文件优先于更宽泛的文件"
	p02NoOverride      = "不能覆盖 system、developer 或用户的直接指令"
	p02LatestSnapshot  = "始终以最新一次指令快照为准"
	p02UpdateSnippet   = "收到“更新”通知时"
)

func newPolicyTestBuilder(t *testing.T) *ContextBuilder {
	t.Helper()
	t.Setenv("CHAGENT_BUILTIN_SKILLS", t.TempDir())
	workspace := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\n项目规则：使用中文回答。",
	})
	t.Cleanup(func() { os.RemoveAll(workspace) })
	return NewContextBuilder(workspace)
}

func assertPolicyAbsent(t *testing.T, prompt, feature string) {
	t.Helper()
	for _, snippet := range []string{
		p01AutonomyHeader,
		p01AutonomySnippet,
		p11CompleteSnippet,
		p02WorkspaceHeader,
	} {
		if strings.Contains(prompt, snippet) {
			t.Fatalf("%s: prompt unexpectedly contains %q:\n%s", feature, snippet, prompt)
		}
	}
}

// TestPolicyPromptsDisabledByDefault 锁定兼容性契约：未调用 WithPolicyPrompts 的
// ContextBuilder（普通聊天）不渲染任何策略片段，因此其 system prompt 与引入策略前逐字节一致。
func TestPolicyPromptsDisabledByDefault(t *testing.T) {
	cb := newPolicyTestBuilder(t)

	prompt := cb.BuildSystemPrompt()
	assertPolicyAbsent(t, prompt, "default builder")

	explicit := NewContextBuilder(cb.workspace).WithPolicyPrompts(false, false).BuildSystemPrompt()
	if prompt != explicit {
		t.Fatalf("default system prompt differs from explicit disabled policy prompt:\ndefault:\n%s\nexplicit:\n%s", prompt, explicit)
	}

	cached := cb.BuildSystemPromptWithCache()
	if cached != prompt {
		t.Fatalf("cached system prompt differs from BuildSystemPrompt result:\ncached:\n%s\nbuilt:\n%s", cached, prompt)
	}
}

func TestAutonomyPolicyPromptEnabled(t *testing.T) {
	cb := newPolicyTestBuilder(t).WithPolicyPrompts(true, false)
	prompt := cb.BuildSystemPrompt()

	assertPromptContains(t, prompt,
		p01AutonomyHeader,
		p01AutonomySnippet,
		p01BlockedSnippet,
		p01EvidenceSnippet,
		p11CompleteSnippet,
		p11BlockedSnippet,
		p11NotBlocker,
	)
	if strings.Contains(prompt, p02WorkspaceHeader) {
		t.Fatalf("autonomy-only prompt unexpectedly contains workspace policy %q:\n%s", p02WorkspaceHeader, prompt)
	}
}

func TestAutonomyPolicyPromptDisabled(t *testing.T) {
	// 工作区上下文开启、autonomy 关闭：P-01/P-11 必须不出现。
	prompt := newPolicyTestBuilder(t).WithPolicyPrompts(false, true).BuildSystemPrompt()
	for _, snippet := range []string{p01AutonomyHeader, p01AutonomySnippet, p11CompleteSnippet, p11NotBlocker} {
		if strings.Contains(prompt, snippet) {
			t.Fatalf("autonomy disabled but prompt contains %q:\n%s", snippet, prompt)
		}
	}
}

func TestWorkspacePolicyPromptEnabled(t *testing.T) {
	cb := newPolicyTestBuilder(t).WithPolicyPrompts(false, true)
	prompt := cb.BuildSystemPrompt()

	assertPromptContains(t, prompt,
		p02WorkspaceHeader,
		p02Precedence,
		p02NoOverride,
		p02LatestSnapshot,
		p02UpdateSnippet,
	)
	for _, snippet := range []string{p01AutonomyHeader, p01AutonomySnippet, p11CompleteSnippet} {
		if strings.Contains(prompt, snippet) {
			t.Fatalf("workspace-policy-only prompt unexpectedly contains autonomy snippet %q:\n%s", snippet, prompt)
		}
	}
}

func TestWorkspacePolicyPromptDisabled(t *testing.T) {
	// autonomy 开启、工作区上下文关闭：P-02 必须不出现。
	prompt := newPolicyTestBuilder(t).WithPolicyPrompts(true, false).BuildSystemPrompt()
	for _, snippet := range []string{p02WorkspaceHeader, p02Precedence, p02UpdateSnippet} {
		if strings.Contains(prompt, snippet) {
			t.Fatalf("workspace policy disabled but prompt contains %q:\n%s", snippet, prompt)
		}
	}
}

func TestCompletionPolicyPromptEnabled(t *testing.T) {
	cb := newPolicyTestBuilder(t).WithPolicyPrompts(true, false)
	prompt := cb.BuildSystemPrompt()

	assertPromptContains(t, prompt,
		p11CompleteSnippet,
		"在任务更新中引用这些证据",
		"至少 3 轮持续存在且现有工具无法解决",
		p11NotBlocker,
		"不要在一轮结束时既不完成也不给出新的阻塞原因",
	)
}

func TestCompletionPolicyPromptDisabled(t *testing.T) {
	prompt := newPolicyTestBuilder(t).BuildSystemPrompt()
	for _, snippet := range []string{p11CompleteSnippet, p11NotBlocker, "在任务更新中引用这些证据"} {
		if strings.Contains(prompt, snippet) {
			t.Fatalf("completion policy disabled but prompt contains %q:\n%s", snippet, prompt)
		}
	}
}

// TestPolicyPromptOrder 校验中央 layer/slot 顺序：
// identity → P-01 自主 → P-11 完成 → P-02 工作区策略 →
// 工作区指令（bootstrap）→ 能力/上下文片段。
func TestPolicyPromptOrder(t *testing.T) {
	cb := newPolicyTestBuilder(t).WithPolicyPrompts(true, true)
	prompt := cb.BuildSystemPrompt()

	ordered := []string{
		"# chagent",
		p01AutonomyHeader,
		p11CompleteSnippet,
		p02WorkspaceHeader,
		"## AGENT.md",
	}
	positions := make([]int, len(ordered))
	for i, snippet := range ordered {
		idx := strings.Index(prompt, snippet)
		if idx < 0 {
			t.Fatalf("system prompt missing ordered snippet %q:\n%s", snippet, prompt)
		}
		positions[i] = idx
	}
	for i := 1; i < len(positions); i++ {
		if positions[i-1] >= positions[i] {
			t.Fatalf("policy prompt order broken: %q (at %d) must precede %q (at %d):\n%s",
				ordered[i-1], positions[i-1], ordered[i], positions[i], prompt)
		}
	}

	// 无论由哪个特性开关产生，渲染出的片段都必须带 kernel/policy 放置位置、
	// stable 标记与 ephemeral 缓存策略。
	parts := cb.BuildSystemPromptParts()
	wantParts := map[string]PromptSourceID{
		"kernel.autonomy_policy":   PromptSourceAutonomyPolicy,
		"kernel.completion_policy": PromptSourceCompletionPolicy,
		"kernel.workspace_policy":  PromptSourceWorkspacePolicy,
	}
	for id, source := range wantParts {
		found := false
		for _, part := range parts {
			if part.ID != id {
				continue
			}
			found = true
			if part.Layer != PromptLayerKernel || part.Slot != PromptSlotPolicy {
				t.Fatalf("part %s placement = %s/%s, want kernel/policy", id, part.Layer, part.Slot)
			}
			if part.Source.ID != source {
				t.Fatalf("part %s source = %s, want %s", id, part.Source.ID, source)
			}
			if !part.Stable || part.Cache != PromptCacheEphemeral {
				t.Fatalf("part %s Stable/Cache = %v/%q, want true/ephemeral", id, part.Stable, part.Cache)
			}
		}
		if !found {
			t.Fatalf("BuildSystemPromptParts() missing part %q", id)
		}
	}
}

// TestPolicyPromptsAreChinese 锁定语言契约：策略片段是模型可见的，必须是中文，
// 且旧英文句子不再出现。
func TestPolicyPromptsAreChinese(t *testing.T) {
	cb := newPolicyTestBuilder(t).WithPolicyPrompts(true, true)
	prompt := cb.BuildSystemPrompt()

	for _, snippet := range []string{
		p01AutonomyHeader, p01AutonomySnippet, p01EvidenceSnippet,
		p11CompleteSnippet, p11NotBlocker,
		p02WorkspaceHeader, p02Precedence, p02NoOverride,
	} {
		if !containsHan(snippet) {
			t.Fatalf("policy golden snippet %q is not Chinese", snippet)
		}
		assertPromptContains(t, prompt, snippet)
	}

	assertPromptExcludes(t, prompt,
		"You operate autonomously until the task is complete",
		"Stop only when",
		"Never claim completion without evidence",
		"Workspace instructions arrive as separate context messages",
		"More specific files take precedence",
		"Removed notice",
		"Be helpful and accurate",
	)
}

// TestPolicyPromptSourcesRegistered 校验三个策略来源 id 已注册到内置 prompt 来源表，
// 且只允许 kernel/policy。
func TestPolicyPromptSourcesRegistered(t *testing.T) {
	registry := NewPromptRegistry()
	for _, source := range []PromptSourceID{
		PromptSourceAutonomyPolicy,
		PromptSourceCompletionPolicy,
		PromptSourceWorkspacePolicy,
	} {
		if err := registry.ValidatePart(PromptPart{
			ID:      "policy.test",
			Layer:   PromptLayerKernel,
			Slot:    PromptSlotPolicy,
			Source:  PromptSource{ID: source},
			Content: "x",
		}); err != nil {
			t.Fatalf("ValidatePart(%s, kernel/policy) error = %v", source, err)
		}
		if err := registry.ValidatePart(PromptPart{
			ID:      "policy.test",
			Layer:   PromptLayerCapability,
			Slot:    PromptSlotTooling,
			Source:  PromptSource{ID: source},
			Content: "x",
		}); err == nil {
			t.Fatalf("ValidatePart(%s, capability/tooling) error = nil, want placement error", source)
		}
	}
}

// TestWithPolicyPromptsInvalidatesCache 确保在 system prompt 已缓存后切换特性开关
// 会带新片段重建。
func TestWithPolicyPromptsInvalidatesCache(t *testing.T) {
	cb := newPolicyTestBuilder(t)

	before := cb.BuildSystemPromptWithCache()
	cb.WithPolicyPrompts(true, true)
	after := cb.BuildSystemPromptWithCache()

	if before == after {
		t.Fatalf("system prompt cache was not invalidated by WithPolicyPrompts")
	}
	if !strings.Contains(after, p01AutonomyHeader) || !strings.Contains(after, p02WorkspaceHeader) {
		t.Fatalf("cached prompt after enabling policy is missing policy sections:\n%s", after)
	}
}

// TestNewAgentInstanceWiresPolicyPrompts 验证 pkg/agent/instance.go
// 把 autonomy/workspace_context 配置开关转发给 builder。
func TestNewAgentInstanceWiresPolicyPrompts(t *testing.T) {
	makeInstance := func(t *testing.T, autonomy, workspaceContext bool) *AgentInstance {
		t.Helper()
		tmpDir := t.TempDir()
		cfg := &config.Config{
			Agents: config.AgentsConfig{
				Defaults: config.AgentDefaults{
					Workspace:         tmpDir,
					ModelName:         "test-model",
					MaxTokens:         1234,
					MaxToolIterations: 5,
					Autonomy:          config.AutonomyConfig{Enabled: autonomy},
					WorkspaceContext:  config.WorkspaceContextConfig{Enabled: workspaceContext},
				},
			},
		}
		return NewAgentInstance(nil, &cfg.Agents.Defaults, cfg, &mockProvider{})
	}

	enabled := makeInstance(t, true, true)
	enabledPrompt := enabled.ContextBuilder.BuildSystemPrompt()
	assertPromptContains(t, enabledPrompt, p01AutonomyHeader, p11CompleteSnippet, p02WorkspaceHeader)

	disabled := makeInstance(t, false, false)
	disabledPrompt := disabled.ContextBuilder.BuildSystemPrompt()
	assertPolicyAbsent(t, disabledPrompt, "instance with autonomy/workspace_context disabled")

	autonomyOnly := makeInstance(t, true, false)
	autonomyPrompt := autonomyOnly.ContextBuilder.BuildSystemPrompt()
	assertPromptContains(t, autonomyPrompt, p01AutonomyHeader, p11CompleteSnippet)
	if strings.Contains(autonomyPrompt, p02WorkspaceHeader) {
		t.Fatalf("autonomy-only instance prompt unexpectedly contains workspace policy:\n%s", autonomyPrompt)
	}
}
