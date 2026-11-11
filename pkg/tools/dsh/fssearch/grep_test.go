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

func buildGrepTree(t *testing.T) string {
	t.Helper()
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "a.go", "package main\nfunc Hello() {}\n// TODO: hello world\n")
	writeTestFile(t, ws, "b.txt", "hello world\nHELLO WORLD\nhello again\n")
	writeTestFile(t, ws, "sub/c.go", "package sub\nvar X = \"hello\"\n")
	writeTestFile(t, ws, ".hidden.md", "hello hidden\n")
	writeTestFile(t, ws, "data.bin", "\x00hello\x00\n")
	writeTestFile(t, ws, "fixed/f1.txt", "a.b\n")
	writeTestFile(t, ws, "fixed/f2.txt", "axb\n")
	writeTestFile(t, ws, "many/many.txt", strings.Repeat("hello\n", 10))
	long := strings.Repeat("a", 5000) + "hello\n"
	writeTestFile(t, ws, "longline.txt", long)
	writeTestFile(t, ws, "large.go", strings.Repeat("x", 200)+"hello\n")
	return ws
}

func TestGrepTool_Metadata(t *testing.T) {
	tool := NewGrepTool(t.TempDir(), true, nil)
	if tool.Name() != "grep" {
		t.Errorf("Name = %q", tool.Name())
	}
	if tool.Description() == "" || !strings.Contains(tool.Description(), "正则") {
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
	for _, key := range []string{"pattern", "path", "include", "ignore_case", "fixed_strings"} {
		if _, ok := props[key]; !ok {
			t.Errorf("Parameters missing %q", key)
		}
	}
}

func TestGrepTool_BasicMatchesAndFormat(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res,
		"a.go:3: // TODO: hello world",
		"b.txt:1: hello world",
		"sub/c.go:2: var X = \"hello\"",
		".hidden.md:1: hello hidden",
	)
	assertNotContains(t, res, "data.bin", "HELLO WORLD")
	if !strings.Contains(res.ForLLM, "\n") {
		t.Error("expected multiple match lines")
	}
}

func TestGrepTool_IgnoreCase(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello", "ignore_case": true})
	assertContains(t, res, "b.txt:2: HELLO WORLD")
}

func TestGrepTool_FixedStrings(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)

	res := execTool(t, tool, map[string]any{"pattern": "a.b"})
	assertContains(t, res, "fixed/f1.txt:1: a.b", "fixed/f2.txt:1: axb")

	res = execTool(t, tool, map[string]any{"pattern": "a.b", "fixed_strings": true})
	assertContains(t, res, "fixed/f1.txt:1: a.b")
	assertNotContains(t, res, "fixed/f2.txt")
}

func TestGrepTool_RegularExpression(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": `^func\s+\w+\s*\(\)`})
	assertContains(t, res, "a.go:2: func Hello() {}")
}

func TestGrepTool_IncludeFilter(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)

	res := execTool(t, tool, map[string]any{"pattern": "hello", "include": "*.go"})
	assertContains(t, res, "a.go:3:", "sub/c.go:2:")
	assertNotContains(t, res, "b.txt", ".hidden.md")

	res = execTool(t, tool, map[string]any{"pattern": "hello", "include": "*.{txt,md}"})
	assertContains(t, res, "b.txt:1:", ".hidden.md:1:")
	assertNotContains(t, res, "a.go")
}

func TestGrepTool_IncludeValidation(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	for _, bad := range []string{"", "!*.go", "*.go,*.md"} {
		res := execTool(t, tool, map[string]any{"pattern": "hello", "include": bad})
		if !res.IsError || !strings.Contains(res.ForLLM, "include") {
			t.Errorf("include %q: expected diagnosable error, got: %v %s", bad, res.IsError, res.ForLLM)
		}
	}
}

func TestGrepTool_MaxMatchesTruncation(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	tool.MaxMatches = 3
	res := execTool(t, tool, map[string]any{"pattern": "hello", "path": "many"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if got := strings.Count(res.ForLLM, "many/many.txt:"); got != 3 {
		t.Errorf("expected 3 capped matches, got %d:\n%s", got, res.ForLLM)
	}
	assertContains(t, res, "仅显示前 3 条匹配")
}

func TestGrepTool_MaxLineBytesPreview(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	tool.MaxLineBytes = 20
	res := execTool(t, tool, map[string]any{"pattern": "hello", "path": "longline.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "longline.txt:1: aaaa", "…")
	for _, line := range strings.Split(res.ForLLM, "\n") {
		if strings.HasPrefix(line, "longline.txt:1:") && len(line) > 60 {
			t.Errorf("preview not truncated: len=%d %q", len(line), line)
		}
	}
}

func TestGrepTool_SkipsBinaryAndLargeFiles(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	tool.MaxFileBytes = 100
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	assertContains(t, res, "二进制文件", "个大于 100 字节的文件")
	assertNotContains(t, res, "data.bin", "large.go")
}

func TestGrepTool_SymlinksNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks are not reliably available on Windows")
	}
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "real.txt", "hello real\n")
	if err := os.Symlink(filepath.Join(ws, "real.txt"), filepath.Join(ws, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	assertContains(t, res, "real.txt:1: hello real")
	assertNotContains(t, res, "link.txt")
}

func TestGrepTool_SingleFilePath(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello", "path": "b.txt"})
	assertContains(t, res, "b.txt:1:", "b.txt:3:")
	assertNotContains(t, res, "a.go", "sub/c.go")
}

func TestGrepTool_RejectsOutsideWorkspace(t *testing.T) {
	ws := newTestWorkspace(t)
	outside := newTestWorkspace(t)
	writeTestFile(t, outside, "secret.txt", "hello secret\n")
	tool := NewGrepTool(ws, true, nil)
	rel := filepath.Join("..", filepath.Base(outside))
	res := execTool(t, tool, map[string]any{"pattern": "hello", "path": rel})
	if !res.IsError || !strings.Contains(res.ForLLM, "拒绝访问") {
		t.Fatalf("expected access-denied error, got: %v %s", res.IsError, res.ForLLM)
	}
	res = execTool(t, tool, map[string]any{"pattern": "hello", "path": outside})
	if !res.IsError {
		t.Fatalf("expected access-denied error for absolute outside path, got: %s", res.ForLLM)
	}
}

func TestGrepTool_AllowReadPaths(t *testing.T) {
	ws := newTestWorkspace(t)
	outside := newTestWorkspace(t)
	writeTestFile(t, outside, "allowed.txt", "hello allowed\n")
	tool := NewGrepTool(ws, true, []*regexp.Regexp{regexp.MustCompile(regexp.QuoteMeta(outside))})
	res := execTool(t, tool, map[string]any{"pattern": "hello", "path": outside})
	if res.IsError {
		t.Fatalf("allowReadPaths should permit outside path, got: %s", res.ForLLM)
	}
	assertContains(t, res, "allowed.txt:1: hello allowed")
}

func TestGrepTool_NoMatch(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "zzz-nothing-zzz"})
	assertContains(t, res, "未找到匹配的内容")
}

func TestGrepTool_ArgumentErrors(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing pattern", map[string]any{}, "pattern 不能为空"},
		{"empty pattern", map[string]any{"pattern": ""}, "pattern 不能为空"},
		{"wrong pattern type", map[string]any{"pattern": 1}, "pattern 必须是字符串"},
		{"invalid regex", map[string]any{"pattern": "("}, "无效的正则表达式"},
		{"blank path", map[string]any{"pattern": "hello", "path": " "}, "path 不能为空字符串"},
		{"wrong path type", map[string]any{"pattern": "hello", "path": 1}, "path 必须是字符串"},
		{"wrong bool", map[string]any{"pattern": "hello", "ignore_case": "yes"}, "ignore_case 必须是布尔值"},
		{"wrong bool2", map[string]any{"pattern": "hello", "fixed_strings": "x"}, "fixed_strings 必须是布尔值"},
		{"bad include type", map[string]any{"pattern": "hello", "include": 5}, "include 必须是字符串"},
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
	if res := execTool(t, tool, map[string]any{"pattern": "hello", "path": "nope"}); !res.IsError ||
		!strings.Contains(res.ForLLM, "路径不存在") {
		t.Errorf("expected nonexistent path error, got: %v %s", res.IsError, res.ForLLM)
	}
	if res := execTool(t, tool, map[string]any{"pattern": strings.Repeat("a", DefaultMaxPatternLen+1)}); !res.IsError {
		t.Error("expected over-long pattern error")
	}
}

func TestGrepTool_ScanByteLimit(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	tool.MaxScanBytes = 30
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "扫描量达到上限")
}

func TestGrepTool_LongLineMatchWindow(t *testing.T) {
	ws := newTestWorkspace(t)
	// The match is beyond the 64KiB match window: documented to be undetected,
	// while the next line is still matched with the correct line number.
	writeTestFile(t, ws, "wide.txt", strings.Repeat("a", 100*1024)+"hello\n"+"hello second line\n")
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "wide.txt:2: hello second line")
	if strings.Contains(res.ForLLM, "wide.txt:1:") {
		t.Errorf("match beyond the per-line window should not be reported: %s", res.ForLLM)
	}
	res = execTool(t, tool, map[string]any{"pattern": "aaaa"})
	assertContains(t, res, "wide.txt:1: aaaa")
}

func TestGrepTool_UnreadableSingleFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	ws := newTestWorkspace(t)
	locked := writeTestFile(t, ws, "locked.txt", "hello\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skipf("chmod unsupported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello", "path": "locked.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "无法读取")
}

func TestGrepTool_NonRegularPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/null on Windows")
	}
	ws := newTestWorkspace(t)
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello", "path": "/dev/null"})
	if !res.IsError {
		t.Fatalf("expected unsupported-type error, got: %s", res.ForLLM)
	}
}

func TestGrepTool_DefaultTimeoutWhenZero(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	tool.Timeout = 0
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "a.go:3:")
}

func TestGrepTool_TimeoutAndCancel(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	tool.Timeout = time.Nanosecond
	res := tool.Execute(context.Background(), map[string]any{"pattern": "hello"})
	if !res.IsError || !strings.Contains(res.ForLLM, "超时") {
		t.Fatalf("expected timeout error, got: %v %s", res.IsError, res.ForLLM)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res = tool.Execute(ctx, map[string]any{"pattern": "hello"})
	if !res.IsError || !strings.Contains(res.ForLLM, "取消") {
		t.Fatalf("expected cancellation error, got: %v %s", res.IsError, res.ForLLM)
	}
}

func TestGrepTool_ZeroCapsUseDefaults(t *testing.T) {
	ws := buildGrepTree(t)
	tool := NewGrepTool(ws, true, nil)
	tool.MaxMatches = 0
	tool.MaxLineBytes = 0
	tool.MaxFileBytes = 0
	tool.MaxScanBytes = 0
	tool.MaxEntries = 0
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "a.go:3:")
}

func TestGrepTool_EmptyWorkspace(t *testing.T) {
	tool := NewGrepTool("", true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	if !res.IsError || !strings.Contains(res.ForLLM, "workspace 未配置") {
		t.Fatalf("expected workspace error, got: %s", res.ForLLM)
	}
}

func TestGrepTool_UnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	ws := newTestWorkspace(t)
	writeTestFile(t, ws, "readable.txt", "hello readable\n")
	unreadable := writeTestFile(t, ws, "locked.txt", "hello locked\n")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Skipf("chmod unsupported: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	tool := NewGrepTool(ws, true, nil)
	res := execTool(t, tool, map[string]any{"pattern": "hello"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	assertContains(t, res, "readable.txt", "无法读取")
}
