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
	p21TaskHeader      = "## 任务判定与记录"
	p21TaskFile        = "tasks/TASKS.md"
	p21TaskLog         = "tasks/TASK_LOG.md"
	p21StaleRule       = "创建超过 7 天"
	p22MemoryHeader    = "## 记忆维护"
	p22MemoryCore      = "只保留跨会话仍然成立的核心内容"
	p23RoleHeader      = "## 角色判定与履职"
	p23RoleSnippet     = "身份类型与角色由任务目标、业务场景、输入材料与验收标准决定"
	p23DutySnippet     = "判定身份后严格按该角色职责执行"
	p23OutputSnippet   = "每次任务判定或阶段汇报先给出身份类型与角色"
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

// TestTaskAndMemoryPolicyPromptEnabledByDefault 锁定默认行为：普通聊天（未开启
// autonomy/workspace 特性）也必须注入 P-21 任务判定/记录与 P-22 记忆维护策略，
// 前提是文件工具可用。
func TestTaskAndMemoryPolicyPromptEnabledByDefault(t *testing.T) {
	cb := newPolicyTestBuilder(t)
	prompt := cb.BuildSystemPrompt()

	assertPromptContains(t, prompt,
		p21TaskHeader,
		p21TaskFile,
		p21TaskLog,
		p21StaleRule,
		"判定结果必须在新的输出开头明确列出",
		"在最终输出中说明本次在用户请求之外额外完成的内容",
		p22MemoryHeader,
		p22MemoryCore,
		"清理记忆后，在最终任务完成输出中说明清理了什么",
		"**任务判定**",
	)
	assertPromptExcludes(t, prompt,
		"Task determination",
		"recorded in tasks/TASKS.md",
		"Memory hygiene",
	)

	assertKernelPolicyPart(t, cb.BuildSystemPromptParts(), "kernel.task_policy", PromptSourceTaskPolicy)
	assertKernelPolicyPart(t, cb.BuildSystemPromptParts(), "kernel.memory_policy", PromptSourceMemoryPolicy)
}

// TestTaskAndMemoryPolicyPromptRequiresFileTools 校验门控：缺少 read_file 或
// 写文件工具、或本次请求禁用工具规则时，不注入要求写文件的任务/记忆策略，
// 也不在 identity 中加入任务判定规则。
func TestTaskAndMemoryPolicyPromptRequiresFileTools(t *testing.T) {
	cb := newPolicyTestBuilder(t)

	cases := []struct {
		name    string
		opts    systemPromptBuildOptions
		want    bool
		mention bool
	}{
		{
			name:    "all tools",
			opts:    systemPromptBuildOptions{IncludeToolUseRule: true},
			want:    true,
			mention: true,
		},
		{
			name: "read and write",
			opts: systemPromptBuildOptions{
				IncludeToolUseRule: true,
				AllowedTools:       []string{"read_file", "write_file"},
			},
			want: true,
		},
		{
			name: "read and edit",
			opts: systemPromptBuildOptions{
				IncludeToolUseRule: true,
				AllowedTools:       []string{"read_file", "edit_file"},
			},
			want: true,
		},
		{
			name: "read only",
			opts: systemPromptBuildOptions{
				IncludeToolUseRule: true,
				AllowedTools:       []string{"read_file"},
			},
		},
		{
			name: "write only",
			opts: systemPromptBuildOptions{
				IncludeToolUseRule: true,
				AllowedTools:       []string{"write_file"},
			},
		},
		{
			name: "tool use rule off",
			opts: systemPromptBuildOptions{IncludeToolUseRule: false},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt := renderPromptPartsLegacy(cb.buildSystemPromptParts(tc.opts))
			for _, snippet := range []string{p21TaskHeader, p21TaskFile, p22MemoryHeader, "**任务判定**"} {
				got := strings.Contains(prompt, snippet)
				if got != tc.want {
					t.Fatalf("prompt contains %q = %v, want %v:\n%s", snippet, got, tc.want, prompt)
				}
			}
			// todo_write / task_update 的协同句只在对应工具可用时出现。
			todoLine := "多步任务用 todo_write 维护待办"
			taskUpdateLine := "会话任务状态用 task_update 管理"
			if got := strings.Contains(prompt, todoLine); got != tc.mention {
				t.Fatalf("prompt contains todo_write coordination line = %v, want %v", got, tc.mention)
			}
			if got := strings.Contains(prompt, taskUpdateLine); got != tc.mention {
				t.Fatalf("prompt contains task_update coordination line = %v, want %v", got, tc.mention)
			}
		})
	}
}

// TestRolePolicyPromptEnabledByDefault 锁定默认行为：角色判定与履职策略不依赖
// 文件工具，普通聊天也必须注入；identity 中同时包含角色判定规则。
func TestRolePolicyPromptEnabledByDefault(t *testing.T) {
	cb := newPolicyTestBuilder(t)
	prompt := cb.BuildSystemPrompt()

	assertPromptContains(t, prompt,
		p23RoleHeader,
		p23RoleSnippet,
		p23DutySnippet,
		p23OutputSnippet,
		"必须先判定自己在该任务中的身份类型与角色",
		"身份类型与职责是任务的必判定项",
		"身份类型覆盖商业公司具有的角色",
		"可进一步判定行业/公司类型与业务场景",
		"行业/公司类型判定是可选项，不要求必须判定",
		"电商、生鲜电商、半导体、芯片设计、AI、机械设计、无人机、机器人、餐饮",
		"软件开发工程师（前端/后端/移动端）",
		"架构师",
		"SRE/运维工程师",
		"硬件/嵌入式研发工程师",
		"产品经理",
		"产品设计（UX/UI）",
		"产品开发",
		"运营",
		"商品运营",
		"销售",
		"公关",
		"政府关系",
		"财务",
		"投资融资",
		"审计",
		"法务",
		"人力/行政",
		"CTO/技术负责人",
		"CFO",
		"研发关注正确性",
		"产品设计关注用户需求",
		"销售关注客户需求",
		"财务关注核算",
		"投资融资关注估值",
		"审计关注独立性",
		"法务关注合规",
		"政府关系关注合规",
		"若已判定行业/公司类型",
		"电商/生鲜电商关注商品",
		"半导体/芯片设计关注设计",
		"无人机/机器人关注控制",
		"餐饮关注门店运营",
		"任务完成前核对身份类型、角色、职责、专业标准（以及已判定的行业/公司类型标准）、验收标准与证据是否一致",
		"**角色判定**",
		"身份类型与职责",
		"按角色职责选用对应技能和工具",
		"在 tasks/TASKS.md 中为每条任务标注身份类型、角色与职责（必填）以及已判定的公司类型（可选）",
	)
	assertPromptExcludes(t, prompt,
		"Role determination",
		"duties and responsibilities",
		"role-based execution",
	)

	assertKernelPolicyPart(t, cb.BuildSystemPromptParts(), "kernel.role_policy", PromptSourceRolePolicy)
}

// TestRolePolicyPromptConditionalLines 校验角色策略的协同句按可用能力裁剪：
// 无工具规则时保留角色判定；文件工具可用时追加任务记录要求。
func TestRolePolicyPromptConditionalLines(t *testing.T) {
	cb := newPolicyTestBuilder(t)

	cases := []struct {
		name         string
		opts         systemPromptBuildOptions
		wantRole     bool
		wantToolLine bool
		wantTaskLine bool
	}{
		{
			name:         "all tools",
			opts:         systemPromptBuildOptions{IncludeToolUseRule: true},
			wantRole:     true,
			wantToolLine: true,
			wantTaskLine: true,
		},
		{
			name: "read and write",
			opts: systemPromptBuildOptions{
				IncludeToolUseRule: true,
				AllowedTools:       []string{"read_file", "write_file"},
			},
			wantRole:     true,
			wantToolLine: true,
			wantTaskLine: true,
		},
		{
			name: "no file tools",
			opts: systemPromptBuildOptions{
				IncludeToolUseRule: true,
				AllowedTools:       []string{"web_search"},
			},
			wantRole:     true,
			wantToolLine: true,
		},
		{
			name:     "tool use rule off",
			opts:     systemPromptBuildOptions{IncludeToolUseRule: false},
			wantRole: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt := renderPromptPartsLegacy(cb.buildSystemPromptParts(tc.opts))
			if got := strings.Contains(prompt, p23RoleHeader); got != tc.wantRole {
				t.Fatalf("prompt contains role policy = %v, want %v:\n%s", got, tc.wantRole, prompt)
			}
			toolLine := "按角色职责选用对应技能和工具"
			taskLine := "在 tasks/TASKS.md 中为每条任务标注身份类型、角色与职责（必填）以及已判定的公司类型（可选）"
			if got := strings.Contains(prompt, toolLine); got != tc.wantToolLine {
				t.Fatalf("prompt contains role tool line = %v, want %v:\n%s", got, tc.wantToolLine, prompt)
			}
			if got := strings.Contains(prompt, taskLine); got != tc.wantTaskLine {
				t.Fatalf("prompt contains role task line = %v, want %v:\n%s", got, tc.wantTaskLine, prompt)
			}
		})
	}
}

func assertKernelPolicyPart(t *testing.T, parts []PromptPart, id string, source PromptSourceID) {
	t.Helper()
	for _, part := range parts {
		if part.ID != id {
			continue
		}
		if part.Layer != PromptLayerKernel || part.Slot != PromptSlotPolicy {
			t.Fatalf("part %s placement = %s/%s, want kernel/policy", id, part.Layer, part.Slot)
		}
		if part.Source.ID != source {
			t.Fatalf("part %s source = %s, want %s", id, part.Source.ID, source)
		}
		if !part.Stable || part.Cache != PromptCacheEphemeral {
			t.Fatalf("part %s Stable/Cache = %v/%q, want true/ephemeral", id, part.Stable, part.Cache)
		}
		return
	}
	t.Fatalf("BuildSystemPromptParts() missing part %q", id)
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

// TestPolicyPromptSourcesRegistered 校验六个策略来源 id 已注册到内置 prompt 来源表，
// 且只允许 kernel/policy。
func TestPolicyPromptSourcesRegistered(t *testing.T) {
	registry := NewPromptRegistry()
	for _, source := range []PromptSourceID{
		PromptSourceAutonomyPolicy,
		PromptSourceCompletionPolicy,
		PromptSourceWorkspacePolicy,
		PromptSourceTaskPolicy,
		PromptSourceMemoryPolicy,
		PromptSourceRolePolicy,
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
