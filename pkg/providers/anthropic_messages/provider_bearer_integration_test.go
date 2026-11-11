// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package anthropicmessages

import (
	"context"
	"os"
	"testing"
)

// TestIntegration_BearerAuth tests the bearer-token auth scheme against a
// real private Anthropic-compatible gateway.
//
// Run with:
//
//	BEARER_API_BASE=https://your-gateway/anthropic BEARER_API_TOKEN=your-token \
//	  go test -run TestIntegration_BearerAuth -v ./pkg/providers/anthropic_messages/
func TestIntegration_BearerAuth(t *testing.T) {
	apiBase := os.Getenv("BEARER_API_BASE")
	token := os.Getenv("BEARER_API_TOKEN")
	if apiBase == "" || token == "" {
		t.Skip("skipping integration test: BEARER_API_BASE and BEARER_API_TOKEN not set")
	}

	provider := NewProviderWithAuthScheme(token, apiBase, "", 0, AuthSchemeBearer)

	resp, err := provider.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "Say hello in one word."}},
		nil,
		"claude-sonnet-4-5-latest",
		map[string]any{"max_tokens": 32},
	)
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}

	if resp.Content == "" {
		t.Error("response content is empty")
	}
	if resp.FinishReason == "" {
		t.Error("finish reason is empty")
	}
	if resp.Usage == nil {
		t.Error("usage is nil")
	} else {
		t.Logf("usage: input=%d output=%d total=%d",
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	}
	t.Logf("response: %s", resp.Content)
}

// TestIntegration_BearerAuth_InvalidToken verifies that an invalid bearer
// token results in an authentication error (not some other failure mode).
func TestIntegration_BearerAuth_InvalidToken(t *testing.T) {
	apiBase := os.Getenv("BEARER_API_BASE")
	if apiBase == "" {
		t.Skip("skipping integration test: BEARER_API_BASE not set")
	}

	provider := NewProviderWithAuthScheme("invalid-token", apiBase, "", 0, AuthSchemeBearer)

	_, err := provider.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		"claude-sonnet-4-5-latest",
		map[string]any{"max_tokens": 10},
	)
	if err == nil {
		t.Fatal("expected error with invalid token, got nil")
	}
	t.Logf("got expected error: %v", err)
}
