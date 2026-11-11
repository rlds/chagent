// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package providers

import (
	"context"
	"os"
	"testing"

	"github.com/rlds/chagent/pkg/config"
)

// TestIntegration_AnthropicBearer_EndToEnd tests the full path from
// ModelConfig -> factory -> provider -> real API with bearer auth.
//
// Run with real credentials:
//
//	BEARER_API_BASE=https://your-gateway/anthropic \
//	BEARER_API_TOKEN=your-token \
//	go test -run TestIntegration_AnthropicBearer_EndToEnd -v ./pkg/providers/
func TestIntegration_AnthropicBearer_EndToEnd(t *testing.T) {
	apiBase := os.Getenv("BEARER_API_BASE")
	token := os.Getenv("BEARER_API_TOKEN")
	if apiBase == "" || token == "" {
		t.Skip("skipping: BEARER_API_BASE and BEARER_API_TOKEN not set")
	}

	// Step 1: Build a ModelConfig exactly as it would appear in config.json
	cfg := &config.ModelConfig{
		ModelName: "claude-private-gateway",
		Model:     "anthropic-bearer/claude-sonnet-4-5-latest",
		APIBase:   apiBase,
	}
	cfg.SetAPIKey(token)

	if err := cfg.Validate(); err != nil {
		t.Fatalf("ModelConfig.Validate() error: %v", err)
	}

	// Step 2: Create provider via factory (same path the agent uses)
	provider, modelID, err := CreateProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("CreateProviderFromConfig() error: %v", err)
	}
	t.Logf("modelID = %q", modelID)
	t.Logf("provider type = %T", provider)

	// Step 3: Basic chat call
	t.Run("simple_chat", func(t *testing.T) {
		resp, err := provider.Chat(
			context.Background(),
			[]Message{{Role: "user", Content: "用一个词回答：天空是什么颜色？"}},
			nil,
			modelID,
			map[string]any{"max_tokens": 20},
		)
		if err != nil {
			t.Fatalf("Chat() error: %v", err)
		}
		if resp.Content == "" {
			t.Fatal("response content is empty")
		}
		t.Logf("response: %s", resp.Content)
		if resp.Usage == nil {
			t.Error("usage is nil")
		} else {
			t.Logf("usage: input=%d output=%d total=%d",
				resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
		}
		if resp.FinishReason == "" {
			t.Error("finish_reason is empty")
		}
	})

	// Step 4: Multi-turn conversation
	t.Run("multi_turn", func(t *testing.T) {
		messages := []Message{
			{Role: "user", Content: "我叫张三，记住我的名字。"},
			{Role: "assistant", Content: "好的，张三！我记住了你的名字。"},
			{Role: "user", Content: "我叫什么名字？用一个词回答。"},
		}
		resp, err := provider.Chat(
			context.Background(),
			messages,
			nil,
			modelID,
			map[string]any{"max_tokens": 20},
		)
		if err != nil {
			t.Fatalf("Chat() error: %v", err)
		}
		t.Logf("response: %s", resp.Content)
		if resp.Content == "" {
			t.Error("response content is empty")
		}
	})

	// Step 5: System prompt
	t.Run("with_system_prompt", func(t *testing.T) {
		messages := []Message{
			{Role: "system", Content: "你是一个只会说三个字的助手，无论用户说什么都只回复三个字。"},
			{Role: "user", Content: "你好！"},
		}
		resp, err := provider.Chat(
			context.Background(),
			messages,
			nil,
			modelID,
			map[string]any{"max_tokens": 20},
		)
		if err != nil {
			t.Fatalf("Chat() error: %v", err)
		}
		t.Logf("response: %s", resp.Content)
	})

	// Step 6: With tool definitions (verify no error in request building)
	t.Run("with_tools", func(t *testing.T) {
		tools := []ToolDefinition{
			{
				Function: ToolFunctionDefinition{
					Name:        "get_weather",
					Description: "获取指定城市的当前天气",
					Parameters: map[string]any{
						"type": "object",
						"properties": map[string]any{
							"city": map[string]any{
								"type":        "string",
								"description": "城市名称",
							},
						},
						"required": []string{"city"},
					},
				},
			},
		}
		resp, err := provider.Chat(
			context.Background(),
			[]Message{{Role: "user", Content: "北京天气怎么样？"}},
			tools,
			modelID,
			map[string]any{"max_tokens": 200},
		)
		if err != nil {
			t.Fatalf("Chat() error: %v", err)
		}
		t.Logf("response: %s", resp.Content)
		t.Logf("tool_calls: %d", len(resp.ToolCalls))
		t.Logf("finish_reason: %s", resp.FinishReason)
	})
}

// TestIntegration_AnthropicBearer_SystemMessage tests that system messages
// are properly sent via the bearer-auth provider.
func TestIntegration_AnthropicBearer_SystemMessage(t *testing.T) {
	apiBase := os.Getenv("BEARER_API_BASE")
	token := os.Getenv("BEARER_API_TOKEN")
	if apiBase == "" || token == "" {
		t.Skip("skipping: BEARER_API_BASE and BEARER_API_TOKEN not set")
	}

	cfg := &config.ModelConfig{
		ModelName: "test-bearer-sys",
		Model:     "anthropic-bearer/claude-sonnet-4-5-latest",
		APIBase:   apiBase,
	}
	cfg.SetAPIKey(token)

	provider, modelID, err := CreateProviderFromConfig(cfg)
	if err != nil {
		t.Fatalf("CreateProviderFromConfig() error: %v", err)
	}

	// System message should be passed through correctly
	resp, err := provider.Chat(
		context.Background(),
		[]Message{
			{Role: "system", Content: "你是一个简短的助手，总是用不超过10个字回答。"},
			{Role: "user", Content: "介绍一下你自己"},
		},
		nil,
		modelID,
		map[string]any{"max_tokens": 50},
	)
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	t.Logf("response: %s", resp.Content)
	if resp.Content == "" {
		t.Error("empty response")
	}
}
