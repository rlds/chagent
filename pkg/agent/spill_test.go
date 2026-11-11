// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// newTestSpillStore 在每测试的临时工作区中构建存储。
func newTestSpillStore(t *testing.T) *SpillStore {
	t.Helper()
	store := NewSpillStore(t.TempDir())
	if store == nil {
		t.Fatal("NewSpillStore(t.TempDir()) = nil, want a usable store")
	}
	return store
}

func TestSpillStoreSaveAndRead(t *testing.T) {
	store := newTestSpillStore(t)
	content := "full tool output\nwith 中文 and 🙂 emoji\n"

	path, err := store.Save("session-1", "tool-tc1", content)
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("Save() path = %q, want absolute path", path)
	}
	if !strings.HasPrefix(path, store.Dir()) {
		t.Errorf("Save() path = %q, want under store dir %q", path, store.Dir())
	}
	if strings.HasSuffix(path, ".txt") == false {
		t.Errorf("Save() path = %q, want .txt suffix", path)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(got) != content {
		t.Errorf("stored content = %q, want %q", string(got), content)
	}

	// 内容、会话/标签相同：按内容寻址且幂等。
	again, err := store.Save("session-1", "tool-tc1", content)
	if err != nil {
		t.Fatalf("second Save() error = %v", err)
	}
	if again != path {
		t.Errorf("second Save() path = %q, want %q", again, path)
	}

	// 不同内容必须得到不同的文件。
	other, err := store.Save("session-1", "tool-tc1", content+"x")
	if err != nil {
		t.Fatalf("third Save() error = %v", err)
	}
	if other == path {
		t.Error("different content reused the same path")
	}
}

func TestSpillStorePathSanitization(t *testing.T) {
	store := newTestSpillStore(t)

	sessionKeys := []string{
		"../../etc/evil session",
		"telegram:123/../../x",
		"你好 世界/../..",
		"./.",
		"..",
	}
	labels := []string{
		"tool-" + "../../passwd",
		"tool-中文 标签/..",
		"../..",
		"tool-a b\tc",
	}

	safeSegment := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

	for _, sessionKey := range sessionKeys {
		for _, label := range labels {
			path, err := store.Save(sessionKey, label, "payload for "+sessionKey+"/"+label)
			if err != nil {
				t.Fatalf("Save(%q, %q) error = %v", sessionKey, label, err)
			}

			rel, relErr := filepath.Rel(store.Dir(), path)
			if relErr != nil {
				t.Fatalf("Rel(%q, %q) error = %v", store.Dir(), path, relErr)
			}
			if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				t.Fatalf("spilled path %q escapes store dir %q", path, store.Dir())
			}

			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) != 2 {
				t.Fatalf("relative path %q: got %d components, want <session>/<file>", rel, len(parts))
			}
			for _, part := range parts {
				if !safeSegment.MatchString(part) {
					t.Errorf("path segment %q is not sanitized [A-Za-z0-9._-]", part)
				}
				if part == "." || part == ".." {
					t.Errorf("path segment %q is a traversal component", part)
				}
			}
			if !strings.HasSuffix(parts[1], ".txt") {
				t.Errorf("file segment %q missing .txt suffix", parts[1])
			}

			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile(%q) error = %v", path, readErr)
			}
			if want := "payload for " + sessionKey + "/" + label; string(got) != want {
				t.Errorf("stored content = %q, want %q", string(got), want)
			}
		}
	}
}

func TestSpillStoreMode0600(t *testing.T) {
	store := newTestSpillStore(t)

	path, err := store.Save("session-mode", "tool-tc9", "secret output")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}

	// 会话目录同样必须是私有的（0700）。
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", filepath.Dir(path), err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("session dir mode = %o, want 0700", got)
	}
}

func TestSpillStoreTruncatesOversize(t *testing.T) {
	t.Run("ascii cut is exact", func(t *testing.T) {
		store := newTestSpillStore(t)
		store.maxBytes = 100
		content := strings.Repeat("a", 300)

		path, err := store.Save("session-trunc", "tool-big", content)
		if err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		if !strings.HasSuffix(string(got), "... [spill truncated]") {
			t.Errorf("stored content tail = %q, want truncation notice", tailString(string(got), 40))
		}
		if want := 100 + len(spillTruncationSuffix); len(got) != want {
			t.Errorf("stored size = %d, want %d (cap + notice)", len(got), want)
		}
		if !strings.HasPrefix(string(got), strings.Repeat("a", 100)) {
			t.Error("stored content should keep the first maxBytes bytes")
		}
	})

	t.Run("multi-byte cut stays valid UTF-8", func(t *testing.T) {
		store := newTestSpillStore(t)
		store.maxBytes = 100 // 33 个完整 CJK rune = 99 字节；第 100 字节会切开一个 rune
		content := strings.Repeat("中", 100)

		path, err := store.Save("session-trunc", "tool-cjk", content)
		if err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		if !utf8.ValidString(string(got)) {
			t.Fatal("truncated content is not valid UTF-8")
		}
		if strings.ContainsRune(string(got), utf8.RuneError) {
			t.Error("truncated content contains U+FFFD")
		}
		body := strings.TrimSuffix(string(got), spillTruncationSuffix)
		if len(body) > 100 {
			t.Errorf("kept body size = %d, want <= 100", len(body))
		}
		if !strings.HasPrefix(content, body) {
			t.Error("kept body is not a prefix of the original content")
		}
	})
}

func tailString(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}

func TestSpillStoreCleanupOld(t *testing.T) {
	store := newTestSpillStore(t)

	oldPath, err := store.Save("session-old", "tool-old", "expired output")
	if err != nil {
		t.Fatalf("Save(old) error = %v", err)
	}
	freshPath, err := store.Save("session-fresh", "tool-fresh", "fresh output")
	if err != nil {
		t.Fatalf("Save(fresh) error = %v", err)
	}

	oldTime := time.Now().Add(-store.maxAge - time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("Chtimes(old) error = %v", err)
	}

	// 放入存储中的符号链接既不得被跟随，也不得被删除。
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("WriteFile(outside) error = %v", err)
	}
	linkPath := filepath.Join(filepath.Dir(oldPath), "sneaky-link.txt")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	removed, err := store.Cleanup()
	if err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if removed != 1 {
		t.Errorf("Cleanup() removed = %d, want 1", removed)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Errorf("expired spill %q still exists (err = %v)", oldPath, err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Errorf("fresh spill %q should survive: %v", freshPath, err)
	}
	if _, err := os.Lstat(linkPath); err != nil {
		t.Errorf("symlink %q should not be removed: %v", linkPath, err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "do not delete" {
		t.Errorf("symlink target changed: content=%q err=%v", string(got), err)
	}
}

func TestSpillStoreUnwritableDegrades(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		var store *SpillStore
		if got := store.Dir(); got != "" {
			t.Errorf("nil store Dir() = %q, want empty", got)
		}
		if _, err := store.Save("s", "l", "c"); err == nil {
			t.Error("nil store Save() error = nil, want error")
		}
		if removed, err := store.Cleanup(); removed != 0 || err != nil {
			t.Errorf("nil store Cleanup() = (%d, %v), want (0, nil)", removed, err)
		}
	})

	t.Run("empty workspace disables spilling", func(t *testing.T) {
		if store := NewSpillStore(""); store != nil {
			t.Errorf("NewSpillStore(\"\") = %+v, want nil", store)
		}
		if store := NewSpillStore("   "); store != nil {
			t.Errorf("NewSpillStore(blank) = %+v, want nil", store)
		}
	})

	t.Run("unwritable parent makes Save fail", func(t *testing.T) {
		workspace := t.TempDir()
		// 存储需要目录的位置存在普通文件会让 MkdirAll 失败。
		blocker := filepath.Join(workspace, ".chagent")
		if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
			t.Fatalf("setup WriteFile(%q) error = %v", blocker, err)
		}

		store := NewSpillStore(workspace)
		if store == nil {
			t.Fatal("NewSpillStore() = nil, want store (failure is per-Save)")
		}
		if _, err := store.Save("session", "tool-tc1", "content"); err == nil {
			t.Error("Save() error = nil, want an error for unwritable store")
		}
		if removed, err := store.Cleanup(); removed != 0 {
			t.Errorf("Cleanup() removed = %d, want 0", removed)
		} else if err == nil {
			// 对普通文件路径调用 WalkDir：不存在的情况已处理，文件与目录不匹配可能被报告。
			// 只要不 panic 且不删除任何内容，两种结果都可接受。
			t.Log("Cleanup() on unwritable store returned no error")
		}
	})
}
