package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorruptMetaSelfHealsFromJSONL locks the crash-recovery behaviour: a
// truncated/corrupt meta.json must not make a session permanently unusable.
// The store backs the file up and rebuilds count/skip from the JSONL history.
func TestCorruptMetaSelfHealsFromJSONL(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	key := "agent:main:heal"
	ctx := context.Background()
	if err := store.AddMessage(ctx, key, "user", "before-corruption"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.AddMessage(ctx, key, "assistant", "reply"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	metaPath := filepath.Join(dir, sanitizeKey(key)+".meta.json")
	if err := os.WriteFile(metaPath, []byte(`{"key":"agent:main:heal","count":2,"skip":1`), 0o644); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}

	meta, err := store.GetSessionMeta(ctx, key)
	if err != nil {
		t.Fatalf("GetSessionMeta after corrupt meta: %v", err)
	}
	if meta.Key != key {
		t.Fatalf("healed meta key = %q, want %q", meta.Key, key)
	}
	if meta.Count != 2 || meta.Skip != 0 {
		t.Fatalf("healed meta count/skip = %d/%d, want 2/0", meta.Count, meta.Skip)
	}

	// The corrupt original must be preserved for forensics.
	matches, _ := filepath.Glob(filepath.Join(dir, sanitizeKey(key)+".meta.json.corrupt-*"))
	if len(matches) != 1 {
		t.Fatalf("expected exactly one corrupt meta backup, got %v", matches)
	}

	// History must remain readable and appends must work after self-heal.
	history, err := store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory after self-heal: %v", err)
	}
	if len(history) != 2 || history[1].Content != "reply" {
		t.Fatalf("history after self-heal = %#v, want 2 messages", history)
	}
	if err := store.AddMessage(ctx, key, "user", "after-heal"); err != nil {
		t.Fatalf("AddMessage after self-heal: %v", err)
	}
	history, err = store.GetHistory(ctx, key)
	if err != nil {
		t.Fatalf("GetHistory after append: %v", err)
	}
	if len(history) != 3 || history[2].Content != "after-heal" {
		t.Fatalf("history after append = %#v", history)
	}
}

// TestCorruptMetaWithoutJSONLStillHeals covers a session that only has a meta
// file (or an empty JSONL): rebuilding must produce a usable zero-count meta.
func TestCorruptMetaWithoutJSONLStillHeals(t *testing.T) {
	dir := t.TempDir()
	store, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatalf("NewJSONLStore: %v", err)
	}
	key := "agent:main:meta-only"
	metaPath := filepath.Join(dir, sanitizeKey(key)+".meta.json")
	if err := os.WriteFile(metaPath, []byte("not-json"), 0o644); err != nil {
		t.Fatalf("corrupt meta: %v", err)
	}
	meta, err := store.GetSessionMeta(context.Background(), key)
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if meta.Count != 0 || meta.Skip != 0 {
		t.Fatalf("healed meta = %+v, want count/skip 0", meta)
	}
	if data, _ := os.ReadFile(metaPath); !strings.Contains(string(data), `"key"`) {
		t.Fatalf("healed meta file not written: %q", string(data))
	}
}
