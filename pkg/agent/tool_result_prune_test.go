// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/providers"
)

// toolResultPrunedLen 返回裁剪后负载的码点长度：头部 + 标记 + 尾部。
func toolResultPrunedLen(head, tail int) int {
	return head + utf8.RuneCountInString(ToolResultPruneMarker) + tail
}

func TestPruneToolResultsDefaults(t *testing.T) {
	got := DefaultToolResultPruneConfig()
	want := ToolResultPruneConfig{ThresholdChars: 8192, HeadChars: 4096, TailChars: 1024}
	if got != want {
		t.Errorf("DefaultToolResultPruneConfig() = %+v, want %+v", got, want)
	}
	if !strings.Contains(ToolResultPruneMarker, "工具输出中段已裁剪") {
		t.Errorf("marker %q should say what was dropped", ToolResultPruneMarker)
	}
}

func TestPruneToolResultsPrunesOversized(t *testing.T) {
	content := strings.Repeat("a", 10_000)
	history := []providers.Message{
		msgUser("run the tool"),
		msgAssistantTC("tc1"),
		msgTool("tc1", content),
	}

	out, pruned, saved := PruneToolResults(history, DefaultToolResultPruneConfig())
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	wantLen := toolResultPrunedLen(4096, 1024)
	if got := utf8.RuneCountInString(out[2].Content); got != wantLen {
		t.Fatalf("pruned content len = %d, want %d", got, wantLen)
	}
	if saved != 10_000-wantLen {
		t.Errorf("savedChars = %d, want %d", saved, 10_000-wantLen)
	}

	runes := []rune(content)
	want := string(runes[:4096]) + ToolResultPruneMarker + string(runes[len(runes)-1024:])
	if out[2].Content != want {
		t.Errorf("pruned content = %q, want head+marker+tail", out[2].Content)
	}
	if !strings.HasPrefix(out[2].Content, "aaa") {
		t.Errorf("pruned content should keep the head: %q", out[2].Content[:16])
	}

	// 无关消息必须原样复制。
	if out[0].Content != history[0].Content || out[1].Content != history[1].Content {
		t.Error("non-tool messages must not be modified")
	}
}

func TestPruneToolResultsBoundary(t *testing.T) {
	cfg := DefaultToolResultPruneConfig()

	atThreshold := []providers.Message{msgTool("tc1", strings.Repeat("b", cfg.ThresholdChars))}
	out, pruned, saved := PruneToolResults(atThreshold, cfg)
	if pruned != 0 || saved != 0 {
		t.Fatalf("content at threshold: pruned = %d, saved = %d, want 0/0", pruned, saved)
	}
	if out[0].Content != atThreshold[0].Content {
		t.Error("content at threshold must be untouched")
	}

	overThreshold := []providers.Message{msgTool("tc1", strings.Repeat("b", cfg.ThresholdChars+1))}
	_, pruned, saved = PruneToolResults(overThreshold, cfg)
	if pruned != 1 {
		t.Fatalf("content over threshold: pruned = %d, want 1", pruned)
	}
	if want := cfg.ThresholdChars + 1 - toolResultPrunedLen(4096, 1024); saved != want {
		t.Errorf("content over threshold: saved = %d, want %d", saved, want)
	}
}

func TestPruneToolResultsCJKAndEmoji(t *testing.T) {
	// 每个单元包含两个 CJK rune 和一个 4 字节 emoji：按字节切分要么切开 emoji，
	// 要么产生非法 UTF-8。
	unit := "你好🙂"
	content := strings.Repeat(unit, 3000)
	totalRunes := 9000
	if got := utf8.RuneCountInString(content); got != totalRunes {
		t.Fatalf("test setup: rune count = %d, want %d", got, totalRunes)
	}

	out, pruned, saved := PruneToolResults(
		[]providers.Message{msgTool("tc1", content)},
		DefaultToolResultPruneConfig(),
	)
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	prunedContent := out[0].Content
	if !utf8.ValidString(prunedContent) {
		t.Fatal("pruned content is not valid UTF-8")
	}
	if strings.ContainsRune(prunedContent, utf8.RuneError) {
		t.Fatalf("pruned content contains U+FFFD: %q", prunedContent)
	}

	wantLen := toolResultPrunedLen(4096, 1024)
	if got := utf8.RuneCountInString(prunedContent); got != wantLen {
		t.Fatalf("pruned rune count = %d, want %d", got, wantLen)
	}
	if saved != totalRunes-wantLen {
		t.Errorf("savedChars = %d, want %d", saved, totalRunes-wantLen)
	}

	runes := []rune(content)
	want := string(runes[:4096]) + ToolResultPruneMarker + string(runes[totalRunes-1024:])
	if prunedContent != want {
		t.Error("CJK/emoji pruning must be rune-safe head+marker+tail")
	}
}

func TestPruneToolResultsIdempotent(t *testing.T) {
	history := []providers.Message{msgTool("tc1", strings.Repeat("x", 12_000))}

	first, pruned, saved := PruneToolResults(history, DefaultToolResultPruneConfig())
	if pruned != 1 || saved == 0 {
		t.Fatalf("first pass: pruned = %d, saved = %d, want 1/>0", pruned, saved)
	}

	second, prunedAgain, savedAgain := PruneToolResults(first, DefaultToolResultPruneConfig())
	if prunedAgain != 0 || savedAgain != 0 {
		t.Fatalf("second pass: pruned = %d, saved = %d, want 0/0", prunedAgain, savedAgain)
	}
	if second[0].Content != first[0].Content {
		t.Error("second pass must leave already-pruned content untouched")
	}
}

func TestPruneToolResultsSkipsStructuredAndNonTool(t *testing.T) {
	long := strings.Repeat("z", 20_000)
	tests := []struct {
		name string
		msg  providers.Message
	}{
		{
			name: "assistant text is not a tool result",
			msg:  msgAssistant(long),
		},
		{
			name: "user text is not a tool result",
			msg:  msgUser(long),
		},
		{
			name: "tool result with media is structured",
			msg: providers.Message{
				Role:    "tool",
				Content: long,
				Media:   []string{"data:image/png;base64,AAAA"},
			},
		},
		{
			name: "tool result with attachments is structured",
			msg: providers.Message{
				Role:        "tool",
				Content:     long,
				Attachments: []providers.Attachment{{Type: "file", Filename: "a.txt"}},
			},
		},
		{
			name: "tool result with system parts is structured",
			msg: providers.Message{
				Role:        "tool",
				Content:     long,
				SystemParts: []providers.ContentBlock{{Type: "text", Text: long}},
			},
		},
		{
			name: "tool result with tool calls is structured",
			msg: providers.Message{
				Role:    "tool",
				Content: long,
				ToolCalls: []providers.ToolCall{{
					ID:   "tc1",
					Type: "function",
					Function: &providers.FunctionCall{
						Name:      "f",
						Arguments: "{}",
					},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, pruned, saved := PruneToolResults(
				[]providers.Message{tt.msg}, DefaultToolResultPruneConfig())
			if pruned != 0 || saved != 0 {
				t.Fatalf("pruned = %d, saved = %d, want 0/0", pruned, saved)
			}
			if out[0].Content != tt.msg.Content {
				t.Error("structured/non-tool message must be untouched")
			}
		})
	}
}

func TestPruneToolResultsDoesNotMutateInput(t *testing.T) {
	original := strings.Repeat("q", 10_000)
	history := []providers.Message{
		msgUser("hello"),
		msgTool("tc1", original),
	}

	out, _, _ := PruneToolResults(history, DefaultToolResultPruneConfig())

	if history[1].Content != original {
		t.Error("input history was mutated")
	}
	if out[1].Content == original {
		t.Fatal("output should carry the pruned content")
	}
	// 返回的切片与输入切片分离。
	out[1].Content = "changed"
	if history[1].Content != original {
		t.Error("returned slice aliases input message storage")
	}
}

func TestPruneToolResultsMultipleMessages(t *testing.T) {
	history := []providers.Message{
		msgTool("tc1", strings.Repeat("1", 10_000)),
		msgAssistant(strings.Repeat("a", 20_000)),
		msgTool("tc2", strings.Repeat("2", 9_000)),
		msgTool("tc3", "small"),
	}

	out, pruned, saved := PruneToolResults(history, DefaultToolResultPruneConfig())
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	perMessage := 10_000 - toolResultPrunedLen(4096, 1024)
	wantSaved := perMessage + (9_000 - toolResultPrunedLen(4096, 1024))
	if saved != wantSaved {
		t.Errorf("savedChars = %d, want %d", saved, wantSaved)
	}
	if out[1].Content != history[1].Content {
		t.Error("long assistant message must be untouched")
	}
	if out[3].Content != "small" {
		t.Error("small tool result must be untouched")
	}
}

func TestPruneToolResultsConfigNormalization(t *testing.T) {
	long := strings.Repeat("n", 9_000)

	tests := []struct {
		name       string
		cfg        ToolResultPruneConfig
		wantPruned int
		wantHead   int
		wantTail   int
		wantSaved  int
	}{
		{
			name:       "zero config uses defaults",
			cfg:        ToolResultPruneConfig{},
			wantPruned: 1,
			wantHead:   4096,
			wantTail:   1024,
		},
		{
			name:       "negative head and tail clamp to zero",
			cfg:        ToolResultPruneConfig{ThresholdChars: 1000, HeadChars: -5, TailChars: -5},
			wantPruned: 1,
			wantHead:   0,
			wantTail:   0,
		},
		{
			name:       "budget larger than threshold falls back to defaults",
			cfg:        ToolResultPruneConfig{ThresholdChars: 1000, HeadChars: 900, TailChars: 200},
			wantPruned: 1,
			wantHead:   4096,
			wantTail:   1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, pruned, saved := PruneToolResults([]providers.Message{msgTool("tc1", long)}, tt.cfg)
			if pruned != tt.wantPruned {
				t.Fatalf("pruned = %d, want %d", pruned, tt.wantPruned)
			}
			wantLen := toolResultPrunedLen(tt.wantHead, tt.wantTail)
			if got := utf8.RuneCountInString(out[0].Content); got != wantLen {
				t.Errorf("pruned len = %d, want %d (head %d / tail %d)",
					got, wantLen, tt.wantHead, tt.wantTail)
			}
			if want := 9_000 - wantLen; saved != want {
				t.Errorf("savedChars = %d, want %d", saved, want)
			}
		})
	}
}

func TestPruneToolResultsEmptyHistory(t *testing.T) {
	out, pruned, saved := PruneToolResults(nil, DefaultToolResultPruneConfig())
	if len(out) != 0 {
		t.Fatalf("len(out) = %d, want 0", len(out))
	}
	if pruned != 0 || saved != 0 {
		t.Fatalf("pruned = %d, saved = %d, want 0/0", pruned, saved)
	}
}

// spilledPathFromContent 从裁剪后的工具结果中提取 spill 定位符；
// 没有 spill 标记时让测试失败。
func spilledPathFromContent(t *testing.T, content string) string {
	t.Helper()
	idx := strings.Index(content, SpillPruneMarkerPrefix)
	if idx < 0 {
		t.Fatalf("content carries no spill marker: %q", tailString(content, 120))
	}
	rest := content[idx+len(SpillPruneMarkerPrefix):]
	end := strings.Index(rest, "]…")
	if end < 0 {
		t.Fatalf("spill marker has no closing bracket: %q", tailString(rest, 120))
	}
	path := rest[:end]
	if path == "" {
		t.Fatal("spill marker carries an empty path")
	}
	return path
}

func TestPruneToolResultsWithSpill(t *testing.T) {
	store := newTestSpillStore(t)
	content := strings.Repeat("a", 10_000)
	history := []providers.Message{
		msgUser("run the tool"),
		msgAssistantTC("tc1"),
		msgTool("tc1", content),
	}

	out, pruned, saved, spilled := PruneToolResultsWithSpill(
		history, DefaultToolResultPruneConfig(), store, "session-spill")
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	if spilled != 1 {
		t.Fatalf("spilled = %d, want 1", spilled)
	}
	if saved <= 0 {
		t.Fatalf("savedChars = %d, want > 0", saved)
	}

	prunedContent := out[2].Content
	path := spilledPathFromContent(t, prunedContent)
	if !filepath.IsAbs(path) {
		t.Errorf("spill path = %q, want absolute", path)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(spill %q) error = %v", path, err)
	}
	if string(stored) != content {
		t.Errorf("spilled content len = %d, want original len %d", len(stored), len(content))
	}

	markerRunes := utf8.RuneCountInString(spillPruneMarker(path))
	runes := []rune(content)
	want := string(runes[:4096]) + spillPruneMarker(path) + string(runes[len(runes)-1024:])
	if prunedContent != want {
		t.Error("spilled pruning must be head + spill marker + tail")
	}
	if wantSaved := len(runes) - (4096 + markerRunes + 1024); saved != wantSaved {
		t.Errorf("savedChars = %d, want %d", saved, wantSaved)
	}

	// 输入绝不会被修改。
	if history[2].Content != content {
		t.Error("input history was mutated")
	}

	// 重复过程是空操作：spill 标记扩展了裁剪标记。
	_, prunedAgain, savedAgain, spilledAgain := PruneToolResultsWithSpill(
		out, DefaultToolResultPruneConfig(), store, "session-spill")
	if prunedAgain != 0 || savedAgain != 0 || spilledAgain != 0 {
		t.Errorf("second pass = (pruned %d, saved %d, spilled %d), want 0/0/0",
			prunedAgain, savedAgain, spilledAgain)
	}
}

func TestPruneSpillFailureKeepsInline(t *testing.T) {
	workspace := t.TempDir()
	// spill 存储需要 .chagent 目录的位置存在普通文件会让 Save 失败。
	if err := os.WriteFile(filepath.Join(workspace, ".chagent"), []byte("blocked"), 0o600); err != nil {
		t.Fatalf("setup WriteFile() error = %v", err)
	}
	store := NewSpillStore(workspace)
	if store == nil {
		t.Fatal("NewSpillStore() = nil, want store")
	}

	content := strings.Repeat("b", 20_000)
	history := []providers.Message{msgUser("q"), msgAssistantTC("tc1"), msgTool("tc1", content)}

	out, pruned, saved, spilled := PruneToolResultsWithSpill(
		history, DefaultToolResultPruneConfig(), store, "session-fail")
	if pruned != 0 || saved != 0 || spilled != 0 {
		t.Fatalf("pruned/saved/spilled = %d/%d/%d, want 0/0/0", pruned, saved, spilled)
	}
	if out[2].Content != content {
		t.Error("failed spill must keep the original tool result inline")
	}
	if isPrunedToolResultContent(out[2].Content) {
		t.Error("failed spill must not advertise a marker")
	}

	// nil 存储回退到遗留标记，且绝不报告 spilled。
	legacyOut, legacyPruned, _, legacySpilled := PruneToolResultsWithSpill(
		history, DefaultToolResultPruneConfig(), nil, "session-fail")
	if legacyPruned != 1 || legacySpilled != 0 {
		t.Fatalf("nil store: pruned/spilled = %d/%d, want 1/0", legacyPruned, legacySpilled)
	}
	if !strings.Contains(legacyOut[2].Content, ToolResultPruneMarker) {
		t.Error("nil store fallback should use the legacy prune marker")
	}
	if got := utf8.RuneCountInString(legacyOut[2].Content); got != toolResultPrunedLen(4096, 1024) {
		t.Errorf("nil store fallback len = %d, want %d", got, toolResultPrunedLen(4096, 1024))
	}
}

func TestPruneSpillMarkerContainsPath(t *testing.T) {
	store := newTestSpillStore(t)
	content := "BEGIN\n" + strings.Repeat("middle ", 2_000) + "\nEND"
	history := []providers.Message{msgTool("tc-path", content)}

	out, pruned, _, spilled := PruneToolResultsWithSpill(
		history, DefaultToolResultPruneConfig(), store, "session-path")
	if pruned != 1 || spilled != 1 {
		t.Fatalf("pruned/spilled = %d/%d, want 1/1", pruned, spilled)
	}
	path := spilledPathFromContent(t, out[0].Content)
	if !strings.HasPrefix(out[0].Content, "BEGIN") || !strings.HasSuffix(out[0].Content, "END") {
		t.Error("spilled pruning must preserve head and tail")
	}
	if !strings.HasPrefix(path, store.Dir()) {
		t.Errorf("spill path %q not under store dir %q", path, store.Dir())
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill locator in marker is not readable: %v", err)
	}
	if string(stored) != content {
		t.Error("spill file does not hold the full original tool output")
	}
	// 标记必须模型可读：模型可见文本会指明路径。
	if !strings.Contains(out[0].Content, path) {
		t.Errorf("marker does not contain the spill path %q", path)
	}
}

// TestForceCompressionSpillsPrunedContent 校验同步压缩路径会把超大的完整工具结果
// 写入工作区 spill 存储，并在模型可见历史中留下可读的定位符。
func TestForceCompressionSpillsPrunedContent(t *testing.T) {
	_, agent, lcm := newCompressionTestLoop(t, 4800, 100, &simpleMockProvider{response: "x"})

	// 把 spill 存储强制指向本测试拥有的目录。
	workspace := t.TempDir()
	agent.Workspace = workspace

	const sessionKey = "session-force-spill"
	toolContent := strings.Repeat("c", 12_000)
	history := []providers.Message{
		msgUser("q1"),
		msgAssistantTC("tc1"),
		msgTool("tc1", toolContent),
		msgAssistant("done"),
		msgUser("q2"),
		msgAssistant("answer"),
	}
	agent.Sessions.SetHistory(sessionKey, history)

	result, ok := lcm.forceCompression(sessionKey)
	if !ok {
		t.Fatal("forceCompression() = false, want true")
	}
	if result.DroppedMessages != 0 {
		t.Fatalf("DroppedMessages = %d, want 0 (pruning alone should be enough)", result.DroppedMessages)
	}

	kept := agent.Sessions.GetHistory(sessionKey)
	if len(kept) != len(history) {
		t.Fatalf("kept history len = %d, want %d", len(kept), len(history))
	}
	prunedContent := kept[2].Content
	path := spilledPathFromContent(t, prunedContent)

	wantDir := filepath.Join(workspace, ".chagent", spillDirName)
	if !strings.HasPrefix(path, wantDir) {
		t.Errorf("spill path = %q, want under %q", path, wantDir)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("compressed history locator is not readable: %v", err)
	}
	if string(stored) != toolContent {
		t.Error("spill file does not hold the full pruned tool output")
	}
	if strings.Contains(prunedContent, ToolResultPruneMarker) {
		t.Error("spill-aware pruning must not fall back to the locator-less marker")
	}
	if got, want := utf8.RuneCountInString(prunedContent), len([]rune(toolContent)); got >= want {
		t.Errorf("pruned len = %d, want < %d", got, want)
	}
	if isOverContextBudget(agent.ContextWindow, kept, nil, agent.MaxTokens) {
		t.Fatal("pruned+spilled history should fit the context window")
	}
}
