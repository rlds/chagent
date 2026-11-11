package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/rlds/chagent/pkg/config"
)

// containsHan reports whether s contains at least one Han (CJK) character.
// It is used to verify that model-visible prompts are written in Chinese.
func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func assertPromptContains(t *testing.T, text string, snippets ...string) {
	t.Helper()
	for _, snippet := range snippets {
		if !strings.Contains(text, snippet) {
			t.Fatalf("model-visible prompt missing Chinese snippet %q:\n%s", snippet, text)
		}
	}
}

func assertPromptExcludes(t *testing.T, text string, snippets ...string) {
	t.Helper()
	for _, snippet := range snippets {
		if strings.Contains(text, snippet) {
			t.Fatalf("model-visible prompt still contains legacy English snippet %q:\n%s", snippet, text)
		}
	}
}

// TestModelVisiblePromptsAreChinese locks the T4.6 language contract: the
// built-in identity/rules/workspace/memory/output/runtime/summary/skills prompt
// text must be Chinese, while the legacy English sentences must not come back.
func TestModelVisiblePromptsAreChinese(t *testing.T) {
	t.Setenv("CHAGENT_BUILTIN_SKILLS", t.TempDir())

	workspace := setupWorkspace(t, map[string]string{
		"AGENT.md":         "# Agent\n这是一个测试 agent。",
		"memory/MEMORY.md": "# Memory\n用户喜欢 Go。",
		"skills/demo-skill/SKILL.md": "---\nname: demo-skill\ndescription: 演示技能\n---\n" +
			"# demo-skill\n\n按步骤执行演示任务。",
	})
	defer os.RemoveAll(workspace)

	cb := NewContextBuilder(workspace).WithSplitOnMarker(true)

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage:    "你好",
		Channel:           "pico",
		ChatID:            "chat-1",
		SenderID:          "discord:123",
		SenderDisplayName: "测试用户",
		Summary:           "此前的对话摘要。",
		ActiveSkills:      []string{"demo-skill"},
	})
	if len(messages) == 0 || messages[0].Role != "system" {
		t.Fatalf("expected a system message, got %#v", messages)
	}
	systemPrompt := messages[0].Content

	assertPromptContains(t, systemPrompt,
		"# chagent",
		"你是 chagent，一个严格按用户意图完成任务的执行型 AI Agent。",
		"## 工作区",
		"## 规则",
		"**精确执行**",
		"**上下文摘要**",
		"**语言与表达**",
		"## 任务与表达",
		"## 任务判定与记录",
		"## 角色判定与履职",
		"## 记忆维护",
		"**记忆**",
		"## 当前时间",
		"## 当前会话",
		"## 当前发送者",
		"当前发送者：测试用户（ID：discord:123）",
		"# 激活的技能",
		"# 记忆",
		"# 多消息输出",
		"CONTEXT_SUMMARY: 早期对话摘要",
	)

	assertPromptExcludes(t, systemPrompt,
		"You are chagent",
		"Be helpful and accurate",
		"The following skills extend your capabilities",
		"To use one, read its SKILL.md",
		"Follow these active skill instructions",
		"# Active Skills",
		"# Skills",
		"## Long-term Memory",
		"## Recent Daily Notes",
		"MULTI-MESSAGE OUTPUT",
		"Current Time",
		"Current Session",
		"Current Sender",
		"Current sender:",
		"ALWAYS use tools",
		"CONTEXT_SUMMARY: Approximate summary",
	)

	// T4.3：默认 "context" 目录模式把技能目录移出了 system prompt；
	// 只有 "system" 回退模式仍在那里渲染它。
	if strings.Contains(systemPrompt, "# 技能") ||
		strings.Contains(systemPrompt, "<skills>") ||
		strings.Contains(systemPrompt, "演示技能") {
		t.Fatalf("default context mode must not render the skill catalog into the system prompt:\n%s", systemPrompt)
	}

	catalog := cb.CollectContextMessages(context.Background(), PromptBuildRequest{})
	if len(catalog) != 1 {
		t.Fatalf("expected one skill catalog context message, got %#v", catalog)
	}
	assertPromptContains(t, catalog[0].Content,
		"技能是可复用的任务专用指令集",
		"`demo-skill`",
		"演示技能",
		"`skill` 工具",
	)
	assertPromptExcludes(t, catalog[0].Content,
		"The following skills extend your capabilities",
		"To use one, read its SKILL.md",
	)

	fallbackPrompt := NewContextBuilder(workspace).
		WithSkillCatalogMode(config.SkillsCatalogModeSystem).
		BuildSystemPrompt()
	assertPromptContains(t, fallbackPrompt,
		"# 技能",
		"以下技能提供可复用任务指令。",
		"demo-skill",
	)
}

// TestModelVisibleContributorPromptsAreChinese covers tool discovery, MCP and
// agent discovery contributors, which were translated together with the core
// prompt text.
func TestModelVisibleContributorPromptsAreChinese(t *testing.T) {
	t.Setenv("CHAGENT_BUILTIN_SKILLS", t.TempDir())

	cb := NewContextBuilder(t.TempDir()).
		WithToolDiscovery(true, true).
		WithAgentDiscovery("main", func(string) []AgentDescriptor {
			return []AgentDescriptor{{
				ID:          "helper",
				Name:        "辅助 agent",
				Description: "协助处理子任务",
			}}
		})
	if err := cb.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: "GitHub Server",
		toolCount:  2,
		deferred:   false,
	}); err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "你好"})
	if len(messages) == 0 || messages[0].Role != "system" {
		t.Fatalf("expected system message, got %#v", messages)
	}
	systemPrompt := messages[0].Content

	assertPromptContains(t, systemPrompt,
		"**工具发现**",
		"MCP 服务器 `GitHub Server` 已连接",
		"提供 2 个工具",
		"以原生工具形式可用",
		"# Agent 发现",
		"允许 spawn 的对等 agent",
	)

	assertPromptExcludes(t, systemPrompt,
		"Only a few tools are visible",
		"Do not refuse while the search returns results",
		"MCP server `GitHub Server` is connected",
		"available as native tools",
		"# Agent Discovery",
		"This registry lists the peer agents",
		"Choose a peer based on its description",
	)
}

// TestChinesePromptHasNoEnglishSentenceHeuristic is a coarse safety net: the
// system prompt should not contain the legacy prompt headers even when skills
// and memory are present.
func TestChinesePromptHasNoEnglishSentenceHeuristic(t *testing.T) {
	t.Setenv("CHAGENT_BUILTIN_SKILLS", t.TempDir())

	workspace := setupWorkspace(t, map[string]string{
		"memory/MEMORY.md": "# Memory\n用户偏好简洁回答。",
	})
	defer os.RemoveAll(workspace)

	cb := NewContextBuilder(workspace)
	prompt := cb.BuildSystemPrompt()
	legacy := []string{
		"## Workspace",
		"## Rules",
		"Platform: ",
		"Channel: ",
		"Chat ID: ",
	}
	assertPromptExcludes(t, prompt, legacy...)
	if !containsHan(prompt) {
		t.Fatalf("system prompt does not contain any Chinese text:\n%s", prompt)
	}
}

// TestModelVisibleToolMarkersAreChinese covers synthetic model-visible markers
// produced for tool media and SubTurn results. They are not part of the system
// prompt but are injected into the conversation sent to the model, so they must
// be Chinese too.
func TestModelVisibleToolMarkersAreChinese(t *testing.T) {
	imageMarker := toolImageFollowUpPromptMessage(nil)
	if !strings.Contains(imageMarker.Content, "图片") {
		t.Fatalf("tool image marker is not Chinese: %q", imageMarker.Content)
	}
	assertPromptExcludes(t, imageMarker.Content, "Loaded image from tool result above")

	subTurnMarker := subTurnResultPromptMessage("子任务已完成")
	if !strings.Contains(subTurnMarker.Content, "子代理结果") || !strings.Contains(subTurnMarker.Content, "子任务已完成") {
		t.Fatalf("subturn result marker is not Chinese: %q", subTurnMarker.Content)
	}
	assertPromptExcludes(t, subTurnMarker.Content, "SubTurn Result")
}
