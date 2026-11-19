package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"telegram:123456", "telegram_123456"},
		{"discord:987654321", "discord_987654321"},
		{"slack:C01234", "slack_C01234"},
		{"no-colons-here", "no-colons-here"},
		{"multiple:colons:here", "multiple_colons_here"},
		{"agent:main:telegram:group:-1003822706455/12", "agent_main_telegram_group_-1003822706455_12"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeFilename(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSave_WithColonInKey(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	// Create a session with a key containing colon (typical channel session key).
	key := "telegram:123456"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")

	// Save should succeed even though the key contains ':'
	if err := sm.Save(key); err != nil {
		t.Fatalf("Save(%q) failed: %v", key, err)
	}

	// The file on disk should use sanitized name.
	expectedFile := filepath.Join(tmpDir, "telegram_123456.json")
	if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
		t.Fatalf("expected session file %s to exist", expectedFile)
	}

	// Load into a fresh manager and verify the session round-trips.
	sm2 := NewSessionManager(tmpDir)
	history := sm2.GetHistory(key)
	if len(history) != 1 {
		t.Fatalf("expected 1 message after reload, got %d", len(history))
	}
	if history[0].Content != "hello" {
		t.Errorf("expected message content %q, got %q", "hello", history[0].Content)
	}
}

func TestSave_RejectsPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	// Invalid names that must still be rejected.
	badKeys := []string{"", ".", ".."}
	for _, key := range badKeys {
		sm.GetOrCreate(key)
		if err := sm.Save(key); err == nil {
			t.Errorf("Save(%q) should have failed but didn't", key)
		}
	}

	// Keys containing path separators are sanitized (no subdirs created).
	sm.GetOrCreate("foo/bar")
	if err := sm.Save("foo/bar"); err != nil {
		t.Fatalf("Save(\"foo/bar\") after sanitize should succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "foo_bar.json")); os.IsNotExist(err) {
		t.Errorf("expected foo_bar.json in storage (sanitized from foo/bar)")
	}
}

func TestLoadSessions_NormalizesMissingCreatedAt(t *testing.T) {
	tmpDir := t.TempDir()
	sessionPath := filepath.Join(tmpDir, "telegram_legacy.json")
	legacy := `{
  "key": "telegram:legacy",
  "messages": [
    {
      "role": "user",
      "content": "hello"
    }
  ],
  "created": "2026-01-01T00:00:00Z",
  "updated": "2026-01-01T00:00:00Z"
}`

	if err := os.WriteFile(sessionPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sm := NewSessionManager(tmpDir)
	history := sm.GetHistory("telegram:legacy")
	if len(history) != 1 {
		t.Fatalf("history = %d, want 1", len(history))
	}
	if history[0].CreatedAt == nil || history[0].CreatedAt.IsZero() {
		t.Fatalf("history[0].CreatedAt = %v, want non-zero timestamp", history[0].CreatedAt)
	}
}

func TestNewSessionManager_ReplacesRegularFileAtStoragePath(t *testing.T) {
	base := t.TempDir()
	storage := filepath.Join(base, "sessions")
	if err := os.WriteFile(storage, []byte("legacy placeholder"), 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}

	sm := NewSessionManager(storage)

	info, err := os.Stat(storage)
	if err != nil || !info.IsDir() {
		t.Fatalf("storage path is not a directory: info=%v err=%v", info, err)
	}
	matches, _ := filepath.Glob(storage + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("expected one corrupt backup, got %v", matches)
	}

	sm.GetOrCreate("agent:test")
	if err := sm.Save("agent:test"); err != nil {
		t.Fatalf("Save() after replacing storage = %v", err)
	}
	if _, err := os.Stat(filepath.Join(storage, "agent_test.json")); err != nil {
		t.Fatalf("session json missing: %v", err)
	}
}

func TestSessionManager_SaveRecreatesMissingStorageDir(t *testing.T) {
	storage := filepath.Join(t.TempDir(), "sessions")
	sm := NewSessionManager(storage)
	sm.GetOrCreate("agent:test")

	if err := os.RemoveAll(storage); err != nil {
		t.Fatalf("remove storage: %v", err)
	}
	if err := sm.Save("agent:test"); err != nil {
		t.Fatalf("Save() should recreate storage dir: %v", err)
	}
	if info, err := os.Stat(storage); err != nil || !info.IsDir() {
		t.Fatalf("storage not recreated: info=%v err=%v", info, err)
	}
}
