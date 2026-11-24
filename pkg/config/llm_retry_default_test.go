package config

import "testing"

func TestDefaultMaxLLMRetries(t *testing.T) {
	if got := DefaultConfig().Agents.Defaults.MaxLLMRetries; got != 6 {
		t.Fatalf("default max_llm_retries=%d, want 6", got)
	}
}
