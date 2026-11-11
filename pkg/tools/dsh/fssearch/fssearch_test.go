package fssearch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// --- shared test helpers ------------------------------------------------

func newTestWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir
}

func writeTestFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return full
}

func execTool(t *testing.T, tool toolshared.Tool, args map[string]any) *toolshared.ToolResult {
	t.Helper()
	return tool.Execute(context.Background(), args)
}

func assertContains(t *testing.T, res *toolshared.ToolResult, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(res.ForLLM, sub) {
			t.Errorf("result does not contain %q:\n%s", sub, res.ForLLM)
		}
	}
}

func assertNotContains(t *testing.T, res *toolshared.ToolResult, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(res.ForLLM, sub) {
			t.Errorf("result unexpectedly contains %q:\n%s", sub, res.ForLLM)
		}
	}
}

// --- glob matcher -------------------------------------------------------

func TestCompileGlob_Matches(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "x/y/b.go", true},
		{"*.go", "a.md", false},
		{"**/*.go", "a.go", true},
		{"**/*.go", "x/y/b.go", true},
		{"src/**/*.go", "src/a.go", true},
		{"src/**/*.go", "src/x/y/b.go", true},
		{"src/**/*.go", "a.go", false},
		{"src/**/*.go", "src2/a.go", false},
		{"a/**/b.txt", "a/b.txt", true},
		{"a/**/b.txt", "a/x/b.txt", true},
		{"a/**/b.txt", "a/x/y/b.txt", true},
		{"src/**", "src/x/y.go", true},
		{"a?c.txt", "abc.txt", true},
		{"a?c.txt", "ac.txt", false},
		{"[ab].txt", "a.txt", true},
		{"[ab].txt", "b.txt", true},
		{"[ab].txt", "c.txt", false},
		{"[!a].txt", "b.txt", true},
		{"[!a].txt", "a.txt", false},
		{"{a,b}.go", "a.go", true},
		{"{a,b}.go", "b.go", true},
		{"{a,b}.go", "c.go", false},
		{"*.{js,ts}", "x/entry.ts", true},
		{"**/test/**/*.go", "pkg/test/unit/x.go", true},
		{"**/test/**/*.go", "pkg/x.go", false},
		{"sub/*.go", "sub/x/y.go", false},
		{"./src/*.go", "src/a.go", true},
		{"[^a-z].txt", "1.txt", true},
		{"[^a-z].txt", "b.txt", false},
		{"[a-c]x", "bx", true},
		{"[a-c]x", "dx", false},
		{"a\\*b", "a*b", true},
		{"a\\*b", "axb", false},
		{"a}*.txt", "a}1.txt", true},
		{"[\\.]", ".", true},
		{"[\\.]", "x", false},
		{"[a\\-c]", "-", true},
		{"[a\\-c]", "b", false},
	}
	for _, tc := range cases {
		m, err := compileGlob(tc.pattern)
		if err != nil {
			t.Fatalf("compileGlob(%q) error: %v", tc.pattern, err)
		}
		if got := m.Match(tc.path); got != tc.want {
			t.Errorf("pattern %q path %q: got %v want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestCompileGlob_Invalid(t *testing.T) {
	for _, pattern := range []string{
		"", "   ", "/abs/*.go", "a//b", "a/", "[bad", "{", "[]", "[z-a]", "a\\", "[a-", "{a,{b,c}",
	} {
		if _, err := compileGlob(pattern); err == nil {
			t.Errorf("compileGlob(%q): expected error, got nil", pattern)
		}
	}
	long := strings.Repeat("a", DefaultMaxPatternLen+1)
	if _, err := compileGlob(long); err == nil {
		t.Error("expected length error for over-long pattern")
	}
	// Brace expansion is bounded.
	tooMany := "{a,b,c,d}{e,f,g,h}{i,j,k,l}{m,n}"
	if _, err := compileGlob(tooMany); err == nil {
		t.Error("expected too-many-branches error for brace expansion")
	}
	tooDeep := "{a,{b,{c,{d,{e,{f,{g,{h,{i,j}}}}}}}}}"
	if _, err := compileGlob(tooDeep); err == nil {
		t.Error("expected nesting-depth error for brace expansion")
	}
}

func TestValidateInclude(t *testing.T) {
	for _, ok := range []string{"*.go", "*.{go,md}", "src/*.go", "**/*.txt", "[a,b].go"} {
		if _, err := validateInclude(ok); err != nil {
			t.Errorf("validateInclude(%q): unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"", "  ", "!*.go", "*.go,*.md"} {
		if _, err := validateInclude(bad); err == nil {
			t.Errorf("validateInclude(%q): expected error, got nil", bad)
		}
	}
}

func TestPreviewLine(t *testing.T) {
	if got := previewLine("hello\n", 100); got != "hello" {
		t.Errorf("trailing newline not stripped: %q", got)
	}
	if got := previewLine("hello\r\n", 100); got != "hello" {
		t.Errorf("trailing CRLF not stripped: %q", got)
	}
	if got := previewLine("hello", 100); got != "hello" {
		t.Errorf("short line changed: %q", got)
	}
	if got := previewLine("hello world", 5); got != "hello…" {
		t.Errorf("preview truncation = %q, want %q", got, "hello…")
	}
	// UTF-8 boundary: cutting mid-rune must not produce invalid UTF-8.
	long := strings.Repeat("é", 20)
	got := previewLine(long, 5)
	if !utf8.ValidString(got) {
		t.Errorf("preview is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated preview has no ellipsis: %q", got)
	}
}

func TestDisplayPath(t *testing.T) {
	ws := newTestWorkspace(t)
	file := writeTestFile(t, ws, "sub/a.go", "x")
	if got := displayPath(ws, ws, file); got != "sub/a.go" {
		t.Errorf("workspace display path = %q", got)
	}
	outsideRoot := t.TempDir()
	outsideFile := writeTestFile(t, outsideRoot, "x/y.txt", "x")
	if got := displayPath(ws, outsideRoot, outsideFile); got != "x/y.txt" {
		t.Errorf("root-relative display path = %q", got)
	}
	if got := displayPath("", outsideRoot, outsideFile); got != "x/y.txt" {
		t.Errorf("empty-workspace display path = %q", got)
	}
}

func TestIgnoreDirSet(t *testing.T) {
	def := ignoreDirSet(nil)
	for _, want := range []string{".git", "node_modules"} {
		if _, ok := def[want]; !ok {
			t.Errorf("default ignore set missing %q", want)
		}
	}
	if len(ignoreDirSet([]string{})) != 0 {
		t.Error("empty non-nil IgnoreDirs should disable ignoring")
	}
	custom := ignoreDirSet([]string{"vendor", ""})
	if _, ok := custom["vendor"]; !ok {
		t.Error("custom ignore set missing vendor")
	}
	if len(custom) != 1 {
		t.Errorf("empty ignore name should be dropped, got %v", custom)
	}
}

func TestStringArgAndBoolArg(t *testing.T) {
	args := map[string]any{"s": "v", "bad": 1, "b": true, "badB": "true"}
	if v, ok, err := stringArg(args, "s"); err != nil || !ok || v != "v" {
		t.Fatalf("stringArg(s) = %q %v %v", v, ok, err)
	}
	if _, ok, err := stringArg(args, "missing"); err != nil || ok {
		t.Fatalf("stringArg(missing) = ok=%v err=%v", ok, err)
	}
	if _, _, err := stringArg(args, "bad"); err == nil {
		t.Error("stringArg on non-string should error")
	}
	if v, err := boolArg(args, "b"); err != nil || !v {
		t.Fatalf("boolArg(b) = %v %v", v, err)
	}
	if _, err := boolArg(args, "badB"); err == nil {
		t.Error("boolArg on string should error")
	}
}

func TestMatchWindowFor(t *testing.T) {
	if got := matchWindowFor(10); got != minMatchWindowBytes {
		t.Errorf("small preview window = %d, want %d", got, minMatchWindowBytes)
	}
	if got := matchWindowFor(1 << 20); got != maxMatchWindowBytes {
		t.Errorf("large preview window = %d, want %d", got, maxMatchWindowBytes)
	}
	if got := matchWindowFor(100 * 1024); got != 400*1024 {
		t.Errorf("preview*4 window = %d, want %d", got, 400*1024)
	}
}

func TestMetadataFor(t *testing.T) {
	meta := metadataFor()
	if meta.Layer != "capability" || meta.Slot != "dsh" || meta.Source != "tool_registry:dsh" {
		t.Fatalf("unexpected PromptMetadata: %+v", meta)
	}
}

func TestResolveRootErrors(t *testing.T) {
	if _, err := resolveRoot("", "", true, nil); err == nil {
		t.Error("expected error for empty workspace")
	}
	ws := newTestWorkspace(t)
	if _, err := resolveRoot(ws, "missing-dir", true, nil); err == nil {
		t.Error("expected error for missing path")
	}
}

func TestTimeoutError(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := timeoutError(ctx, "glob"); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Errorf("timeoutError = %v", err)
	}
	canceled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := timeoutError(canceled, "grep"); err == nil || !strings.Contains(err.Error(), "取消") {
		t.Errorf("timeoutError(canceled) = %v", err)
	}
	if !errors.Is(context.DeadlineExceeded, context.DeadlineExceeded) {
		t.Fatal("sanity")
	}
}
