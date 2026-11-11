// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package anthropicmessages

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthScheme_XAPIKey verifies that the default (X-API-Key) auth scheme
// sends the key in the X-API-Key header.
func TestAuthScheme_XAPIKey(t *testing.T) {
	var gotAuthHeader string
	var gotXAPIKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("X-API-Key")

		resp := map[string]any{
			"id":   "msg-test",
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "hi"},
			},
			"stop_reason": "end_turn",
			"model":       "test-model",
			"usage":       map[string]int{"input_tokens": 5, "output_tokens": 2},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewProvider("test-api-key", server.URL, "")
	_, err := provider.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		"test-model",
		map[string]any{"max_tokens": 10},
	)
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}

	if gotXAPIKey != "test-api-key" {
		t.Errorf("X-API-Key header = %q, want %q", gotXAPIKey, "test-api-key")
	}
	if gotAuthHeader != "" {
		t.Errorf("Authorization header = %q, want empty", gotAuthHeader)
	}
}

// TestAuthScheme_Bearer verifies that the bearer auth scheme sends the token
// as "Authorization: Bearer <token>" and does NOT set X-API-Key.
func TestAuthScheme_Bearer(t *testing.T) {
	var gotAuthHeader string
	var gotXAPIKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("X-API-Key")

		resp := map[string]any{
			"id":   "msg-test",
			"type": "message",
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": "hi"},
			},
			"stop_reason": "end_turn",
			"model":       "test-model",
			"usage":       map[string]int{"input_tokens": 5, "output_tokens": 2},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	provider := NewProviderWithAuthScheme("my-token", server.URL, "", 0, AuthSchemeBearer)
	_, err := provider.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		"test-model",
		map[string]any{"max_tokens": 10},
	)
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}

	wantAuth := "Bearer my-token"
	if gotAuthHeader != wantAuth {
		t.Errorf("Authorization header = %q, want %q", gotAuthHeader, wantAuth)
	}
	if gotXAPIKey != "" {
		t.Errorf("X-API-Key header = %q, want empty", gotXAPIKey)
	}
}

// TestNewProviderWithAuthScheme_Default verifies that an empty authScheme
// falls back to X-API-Key (matching existing NewProvider behavior).
func TestNewProviderWithAuthScheme_Default(t *testing.T) {
	var gotXAPIKey string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotXAPIKey = r.Header.Get("X-API-Key")
		resp := map[string]any{
			"id":          "msg-test",
			"type":        "message",
			"role":        "assistant",
			"content":     []any{map[string]any{"type": "text", "text": "hi"}},
			"stop_reason": "end_turn",
			"model":       "test-model",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	// Empty authScheme should default to X-API-Key
	provider := NewProviderWithAuthScheme("key-123", server.URL, "", 0, "")
	_, err := provider.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		"test-model",
		map[string]any{"max_tokens": 10},
	)
	if err != nil {
		t.Fatalf("Chat() error: %v", err)
	}
	if gotXAPIKey != "key-123" {
		t.Errorf("X-API-Key = %q, want %q", gotXAPIKey, "key-123")
	}
}

// TestAuthScheme_Bearer_EmptyKey verifies that bearer scheme with an empty
// token still fails fast (same as X-API-Key scheme with empty key).
func TestAuthScheme_Bearer_EmptyKey(t *testing.T) {
	provider := NewProviderWithAuthScheme("", "https://example.com", "", 0, AuthSchemeBearer)
	_, err := provider.Chat(
		context.Background(),
		[]Message{{Role: "user", Content: "hi"}},
		nil,
		"test-model",
		map[string]any{"max_tokens": 10},
	)
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
}
