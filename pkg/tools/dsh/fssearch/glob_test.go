package fssearch

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func buildGlobTree(t *testing.T) string {
	t.Helper()
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "a.go", "package main\n")
	writeTestFile(t, ws, "b.md", "# doc\n")
	writeTestFile(t, ws, ".hidden.go", "package hidden\n")
	writeTestFile(t, ws, "src/main.go", "package main\n")
	writeTestFile(t, ws, "src/util/helper.go", "package util\n")
	writeTestFile(t, ws, "src/x/y/deep.go", "package deep\n")
	writeTestFile(t, ws, "src/app.test.js", "test\n")
	writeTestFile(t, ws, "sub/a.txt", "text\n")
	writeTestFile(t, ws, ".git/ignored.go", "package ignored\n")
	writeTestFile(t, ws, "node_modules/pkg/index.js", "module\n")
	return ws
}

func TestGlobTool_Metadata(t *testing.T) {
	tool := NewGlobTool(t.TempDir(), true, nil)
	if tool.Name() != "glob" {
		t.Errorf("Name = %q", tool.Name())
	}
	if tool.Description() == "" || !strings.Contains(tool.Description(), "文件") {
		t.Errorf("Description is not the expected Chinese text: %q", tool.Description())
	}
	meta := tool.PromptMetadata()
	if meta.Layer != "capability" || meta.Slot != "dsh" || meta.Source != "tool_registry:dsh" {
		t.Errorf("PromptMetadata = %+v", meta)
	}
	params := tool.Parameters()
	required, _ := params["required"].([]string)
	if len(required) != 1 || required[0] != "pattern" {
		t.Errorf("Parameters required = %v", params["required"])
	}
	props, _ := params["properties"].(map[string]any)
	for _, key := range []string{"pattern", "path"} {
		if _, ok := props[key]; !ok {
			t.Errorf("Parameters missing %q", key)
		}
	}
}

func TestGlobTool_BasicPatterns(t *testing.T) {
	ws := buildGlobTree(t)
	tool := NewGlobTool(ws, true, nil)

	res := execTool(t, tool, map[string]any{"pattern": "*.go"})
	assertContains(t, res, "a.go", "src/main.go", "src/util/helper.go", "src/x/y/deep.go", ".hidden.go")
	assertNotContains(t, res, ".git/ignored.go", "node_modules/pkg/index.js", "b.md")

	res = execTool(t, tool, map[string]any{"pattern": "src/**/*.go"})
	assertContains(t, res, "src/main.go", "src/util/helper.go", "src/x/y/deep.go")
	assertNotContains(t, res, "a.go\n", ".hidden.go")

	res = execTool(t, tool, map[string]any{"pattern": "**/*.test.js"})
	assertContains(t, res, "src/app.test.js")

	res = execTool(t, tool, map[string]any{"pattern": "*.nomatch"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "未找到匹配的文件")
}

func TestGlobTool_IgnoresDirsAndCustomConfig(t *testing.T) {
	ws := buildGlobTree(t)
	tool := NewGlobTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "**/*.js"})
	assertNotContains(t, res, ".git", "node_modules")
	assertContains(t, res, "src/app.test.js")

	// Custom empty ignore list → walk everything (including .git).
	all := NewGlobTool(ws, true, nil)
	all.IgnoreDirs = []string{}
	res = execTool(t, all, map[string]any{"pattern": "ignored.go"})
	assertContains(t, res, ".git/ignored.go")

	// Custom ignore set replaces the defaults entirely.
	custom := NewGlobTool(ws, true, nil)
	custom.IgnoreDirs = []string{"src"}
	res = execTool(t, custom, map[string]any{"pattern": "*.go"})
	assertContains(t, res, "a.go", ".git/ignored.go")
	assertNotContains(t, res, "src/main.go")
}

func TestGlobTool_PathScope(t *testing.T) {
	ws := buildGlobTree(t)
	tool := NewGlobTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "*.go", "path": "src"})
	assertContains(t, res, "src/main.go", "src/x/y/deep.go")
	assertNotContains(t, res, "a.go\n")

	res = execTool(t, tool, map[string]any{"pattern": "main.go", "path": "src/main.go"})
	assertContains(t, res, "src/main.go")

	res = execTool(t, tool, map[string]any{"pattern": "nomatch*", "path": "src/main.go"})
	assertContains(t, res, "未找到匹配的文件")

	res = execTool(t, tool, map[string]any{"pattern": "*.go", "path": "missing-dir"})
	if !res.IsError {
		t.Fatalf("expected error for missing path, got: %s", res.ForLLM)
	}
	assertContains(t, res, "路径不存在")
}

func TestGlobTool_SymlinksNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are not reliably available on Windows")
	}
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "real.go", "package real\n")
	outside := newTestWorkspace(t)
	writeTestFile(t, outside, "outside.go", "package outside\n")
	if err := os.Symlink(outside, filepath.Join(ws, "linkdir")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if err := os.Symlink(filepath.Join(ws, "real.go"), filepath.Join(ws, "linkfile.go")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	tool := NewGlobTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "*.go"})
	assertContains(t, res, "real.go")
	assertNotContains(t, res, "outside.go", "linkfile.go", "linkdir")
}

func TestGlobTool_SortsByModTimeDesc(t *testing.T) {
	ws := newTestWorkspace(t)
	base := time.Now().Add(-10 * time.Hour)
	oldest := writeTestFile(t, ws, "oldest.txt", "1")
	middle := writeTestFile(t, ws, "middle.txt", "2")
	newest := writeTestFile(t, ws, "newest.txt", "3")
	if err := os.Chtimes(oldest, base, base); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(middle, base.Add(time.Hour), base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newest, base.Add(2*time.Hour), base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	tool := NewGlobTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "*.txt"})
	lines := strings.Split(strings.TrimSpace(res.ForLLM), "\n")
	if len(lines) != 3 || lines[0] != "newest.txt" || lines[1] != "middle.txt" || lines[2] != "oldest.txt" {
		t.Fatalf("unexpected modification-time order: %v", lines)
	}

	// Equal modification times fall back to a stable lexical order.
	same := []string{"b.txt", "a.txt", "c.txt"}
	for _, name := range same {
		full := writeTestFile(t, ws, "same/"+name, "x")
		if err := os.Chtimes(full, base, base); err != nil {
			t.Fatal(err)
		}
	}
	res = execTool(t, tool, map[string]any{"pattern": "same/*.txt"})
	if strings.TrimSpace(res.ForLLM) != "same/a.txt\nsame/b.txt\nsame/c.txt" {
		t.Fatalf("tie-break order unexpected: %q", res.ForLLM)
	}
}

func TestGlobTool_MaxResultsTruncation(t *testing.T) {
	ws := newTestWorkspace(t)
	for i := 0; i < 5; i++ {
		writeTestFile(t, ws, filepath.ToSlash(filepath.Join("d", "f"+string(rune('a'+i))+".txt")), "x")
	}
	tool := NewGlobTool(ws, true, nil)
	tool.MaxResults = 2
	res := execTool(t, tool, map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := strings.Count(res.ForLLM, "d/f"); got != 2 {
		t.Errorf("expected 2 shown paths, got %d:\n%s", got, res.ForLLM)
	}
	assertContains(t, res, "共找到 5 个文件", "仅显示前 2 个")
}

func TestGlobTool_MaxEntries(t *testing.T) {
	ws := newTestWorkspace(t)
	for i := 0; i < 10; i++ {
		writeTestFile(t, ws, filepath.ToSlash(filepath.Join("d", "f"+string(rune('a'+i))+".txt")), "x")
	}
	tool := NewGlobTool(ws, true, nil)
	tool.MaxEntries = 3
	res := execTool(t, tool, map[string]any{"pattern": "*.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "扫描条目达到上限")
}

func TestGlobTool_RejectsOutsideWorkspace(t *testing.T) {
	ws := newTestWorkspace(t)
	outside := newTestWorkspace(t)
	writeTestFile(t, outside, "secret.txt", "secret\n")
	tool := NewGlobTool(ws, true, nil)
	rel := filepath.Join("..", filepath.Base(outside))
	res := execTool(t, tool, map[string]any{"pattern": "*.txt", "path": rel})
	if !res.IsError {
		t.Fatalf("expected access-denied error, got: %s", res.ForLLM)
	}
	assertContains(t, res, "拒绝访问")

	res = execTool(t, tool, map[string]any{"pattern": "*.txt", "path": outside})
	if !res.IsError {
		t.Fatalf("expected access-denied error for absolute outside path, got: %s", res.ForLLM)
	}
}

func TestGlobTool_AllowReadPaths(t *testing.T) {
	ws := newTestWorkspace(t)
	outside := newTestWorkspace(t)
	writeTestFile(t, outside, "allowed.txt", "ok\n")
	tool := NewGlobTool(ws, true, []*regexp.Regexp{regexp.MustCompile(regexp.QuoteMeta(outside))})
	res := execTool(t, tool, map[string]any{"pattern": "*.txt", "path": outside})
	if res.IsError {
		t.Fatalf("allowReadPaths should permit the outside path, got: %s", res.ForLLM)
	}
	assertContains(t, res, "allowed.txt")
}

func TestGlobTool_ArgumentErrors(t *testing.T) {
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "a.go", "x")
	tool := NewGlobTool(ws, true, nil)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing pattern", map[string]any{}, "pattern 不能为空"},
		{"blank pattern", map[string]any{"pattern": "   "}, "pattern 不能为空"},
		{"wrong pattern type", map[string]any{"pattern": 42}, "pattern 必须是字符串"},
		{"blank path", map[string]any{"pattern": "*.go", "path": " "}, "path 不能为空字符串"},
		{"wrong path type", map[string]any{"pattern": "*.go", "path": 7}, "path 必须是字符串"},
		{"absolute pattern", map[string]any{"pattern": "/etc/*"}, "相对路径"},
		{"bad pattern", map[string]any{"pattern": "[oops"}, "无效的 glob"},
		{"unbalanced brace", map[string]any{"pattern": "{a,b"}, "未闭合的 {"},
	}
	for _, tc := range cases {
		res := execTool(t, tool, tc.args)
		if !res.IsError {
			t.Errorf("%s: expected error result, got %s", tc.name, res.ForLLM)
			continue
		}
		if !strings.Contains(res.ForLLM, tc.want) {
			t.Errorf("%s: error %q does not contain %q", tc.name, res.ForLLM, tc.want)
		}
	}
	if res := execTool(t, tool, map[string]any{"pattern": "*.go", "path": "nope"}); !res.IsError {
		t.Error("expected error for nonexistent path")
	}
}

func TestGlobTool_UnreadableSubdir(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("permission bits are not enforced for root/Windows")
	}
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "top.go", "package top\n")
	locked := filepath.Join(ws, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, ws, "locked/hidden.go", "package hidden\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("chmod unsupported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	tool := NewGlobTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "*.go"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "top.go", "部分目录不可读")
}

func TestGlobTool_DefaultTimeoutWhenZero(t *testing.T) {
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "a.go", "x")
	tool := NewGlobTool(ws, true, nil)
	tool.Timeout = 0 // falls back to DefaultTimeout
	res := execTool(t, tool, map[string]any{"pattern": "*.go"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "a.go")
}

func TestGlobTool_TimeoutAndCancel(t *testing.T) {
	ws := buildGlobTree(t)
	tool := NewGlobTool(ws, true, nil)
	tool.Timeout = time.Nanosecond
	res := tool.Execute(context.Background(), map[string]any{"pattern": "*.go"})
	if !res.IsError {
		t.Fatalf("expected timeout error, got: %s", res.ForLLM)
	}
	assertContains(t, res, "超时")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res = tool.Execute(ctx, map[string]any{"pattern": "*.go"})
	if !res.IsError {
		t.Fatalf("expected cancellation error, got: %s", res.ForLLM)
	}
	assertContains(t, res, "取消")
}

func TestGlobTool_ZeroCapsUseDefaults(t *testing.T) {
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "a.go", "x")
	tool := NewGlobTool(ws, true, nil)
	tool.MaxResults = 0
	tool.MaxEntries = 0
	res := execTool(t, tool, map[string]any{"pattern": "*.go"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "a.go")
}

func TestGlobTool_EmptyWorkspace(t *testing.T) {
	tool := NewGlobTool("", true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "*.go"})
	if !res.IsError || !strings.Contains(res.ForLLM, "workspace 未配置") {
		t.Fatalf("expected workspace error, got: %s", res.ForLLM)
	}
}
