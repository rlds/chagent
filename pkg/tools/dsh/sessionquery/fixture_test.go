package sessionquery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/tools/shared"
)

// 固定时间夹具：所有测试都使用确定性时间戳，避免依赖 now()。
var (
	fixtureT0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
)

func fixtureTime(offset time.Duration) *time.Time {
	at := fixtureT0.Add(offset)
	return &at
}

// sessionFixture 描述 sessions 目录下的一个会话文件。
type sessionFixture struct {
	Stem    string
	Key     string
	Summary string
	Skip    int
	// Messages 为正常 JSONL 行；RawLines 为追加在 Messages 之后/之间的原始行（用于损坏行）。
	RawLines []string
	Messages []providers.Message
	// MetaInvalid 为 true 时写入无法解析的 meta JSON。
	MetaInvalid bool
}

// newTestWorkspace 创建临时 workspace 并返回 sessions 目录路径。
func newTestWorkspace(t *testing.T) (workspace, sessionsDir string) {
	t.Helper()
	workspace = t.TempDir()
	sessionsDir = filepath.Join(workspace, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatalf("创建 sessions 目录失败: %v", err)
	}
	return workspace, sessionsDir
}

// writeSessionFixture 写入一个会话的 JSONL 与 meta。
func writeSessionFixture(t *testing.T, sessionsDir string, fixture sessionFixture) {
	t.Helper()
	lines := make([]string, 0, len(fixture.Messages)+len(fixture.RawLines))
	for i := range fixture.Messages {
		encoded, err := json.Marshal(fixture.Messages[i])
		if err != nil {
			t.Fatalf("序列化 fixture 消息失败: %v", err)
		}
		lines = append(lines, string(encoded))
		if i == 0 {
			lines = append(lines, fixture.RawLines...)
		}
	}
	writeRawSession(t, sessionsDir, fixture.Stem, lines)

	metaPath := filepath.Join(sessionsDir, fixture.Stem+".meta.json")
	rawMeta := []byte("{not-valid-json")
	if !fixture.MetaInvalid {
		meta := sessionMetaFile{
			Key:       fixture.Key,
			Summary:   fixture.Summary,
			Skip:      fixture.Skip,
			Count:     len(fixture.Messages),
			CreatedAt: fixtureT0,
			UpdatedAt: fixtureT0.Add(time.Minute),
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("序列化 fixture meta 失败: %v", err)
		}
		rawMeta = encoded
	}
	if err := os.WriteFile(metaPath, rawMeta, 0o644); err != nil {
		t.Fatalf("写入 fixture meta 失败: %v", err)
	}
}

// writeRawSession 直接写入若干原始 JSONL 行。
func writeRawSession(t *testing.T, sessionsDir, stem string, lines []string) {
	t.Helper()
	path := filepath.Join(sessionsDir, stem+".jsonl")
	var payload []byte
	for _, line := range lines {
		payload = append(payload, line...)
		payload = append(payload, '\n')
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("写入 JSONL 失败: %v", err)
	}
}

// message helpers

func userMessage(content string, at *time.Time) providers.Message {
	return providers.Message{Role: "user", Content: content, CreatedAt: at}
}

func assistantMessage(content string, at *time.Time) providers.Message {
	return providers.Message{Role: "assistant", Content: content, CreatedAt: at}
}

func toolCallMessage(content, toolName, argsJSON, callID string, at *time.Time) providers.Message {
	return providers.Message{
		Role:             "assistant",
		Content:          content,
		CreatedAt:        at,
		ReasoningContent: "",
		ToolCalls: []providers.ToolCall{{
			ID:   callID,
			Type: "function",
			Function: &providers.FunctionCall{
				Name:      toolName,
				Arguments: argsJSON,
			},
		}},
	}
}

func toolResultMessage(content, callID string, at *time.Time) providers.Message {
	return providers.Message{Role: "tool", Content: content, ToolCallID: callID, CreatedAt: at}
}

func contextMessage(content, kind string, at *time.Time) providers.Message {
	return providers.Message{
		Role:      "user",
		Content:   content,
		CreatedAt: at,
		Context:   &protocoltypes.ContextMeta{Kind: kind, Baseline: true},
	}
}

// buildMainFixture 构造覆盖命中/大小写/正则/损坏行/截断/遮罩/符号链接/越界的会话集合。
//
// 目录布局（按文件名字典序）：
//
//	agent_main_corrupt.jsonl  有效行 + 损坏行 + 有效行
//	agent_main_demo.jsonl     两回合正常会话（含工具调用/上下文/密钥样式文本）
//	agent_main_long.jsonl     单行超长内容
//	agent_main_other.jsonl    另一个会话（FIXME 命中）
//	agent_main_skip.jsonl     meta.skip=1（第一行属于被截断历史）
//	agent_main_badmeta.jsonl  meta 无法解析
//	agent_main_link.jsonl     符号链接（指向 workspace 外，不应被读取）
//
// workspace 根目录还有 sessions_outside.jsonl（不在 sessions 下，不应被读取）。
func buildMainFixture(t *testing.T) (workspace, sessionsDir string) {
	t.Helper()
	workspace, sessionsDir = newTestWorkspace(t)

	writeSessionFixture(t, sessionsDir, sessionFixture{
		Stem: "agent_main_demo",
		Key:  "agent:main:demo",
		Messages: []providers.Message{
			contextMessage("工作区指令上下文", protocoltypes.ContextKindWorkspaceInstructions, fixtureTime(0)),
			userMessage("请帮我修复 bug-123，并检查配置 api_key=sk-abcdefghijklmnop", fixtureTime(time.Minute)),
			toolCallMessage("我先读文件", "read_file", `{"path":"main.go"}`, "call-1", fixtureTime(2*time.Minute)),
			toolResultMessage("file content: package main", "call-1", fixtureTime(3*time.Minute)),
			assistantMessage("修复完成", fixtureTime(4*time.Minute)),
			userMessage("再检查 SecretKey 与 password=hunter2 是否泄露", fixtureTime(5*time.Minute)),
			assistantMessage("检查完毕", fixtureTime(6*time.Minute)),
		},
		Summary: "修复 bug-123 的会话",
	})

	writeSessionFixture(t, sessionsDir, sessionFixture{
		Stem: "agent_main_other",
		Key:  "agent:main:other",
		Messages: []providers.Message{
			userMessage("另一个会话提到 FIXME 与 bug-123", fixtureTime(10*time.Minute)),
			assistantMessage("收到", fixtureTime(11*time.Minute)),
		},
	})

	writeSessionFixture(t, sessionsDir, sessionFixture{
		Stem: "agent_main_skip",
		Key:  "agent:main:skip",
		Skip: 1,
		Messages: []providers.Message{
			userMessage("历史前导（已被 meta.skip 截断）bug-123", fixtureTime(20*time.Minute)),
			userMessage("可见内容 bug-123", fixtureTime(21*time.Minute)),
		},
	})

	// 损坏行：第 2 行不是合法 JSON。
	writeRawSession(t, sessionsDir, "agent_main_corrupt", []string{
		`{"role":"user","content":"损坏行之前的 bug-123 命中"}`,
		`{not-json`,
		`{"role":"assistant","content":"损坏行之后的内容"}`,
	})
	metaBytes, err := json.Marshal(sessionMetaFile{Key: "agent:main:corrupt", CreatedAt: fixtureT0})
	if err != nil {
		t.Fatalf("序列化 meta 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionsDir, "agent_main_corrupt.meta.json"), metaBytes, 0o644); err != nil {
		t.Fatalf("写入 meta 失败: %v", err)
	}

	// 超长行（内容远超测试里设置的 MaxLineBytes）。
	writeRawSession(t, sessionsDir, "agent_main_long", []string{
		`{"role":"user","content":"` + repeatRunes("长", 4096) + ` end-marker"}`,
	})

	// meta 无法解析：仍应按文件名识别并可检索。
	writeSessionFixture(t, sessionsDir, sessionFixture{
		Stem:        "agent_main_badmeta",
		MetaInvalid: true,
		Messages: []providers.Message{
			userMessage("badmeta 会话里的 bug-123 命中", fixtureTime(30*time.Minute)),
		},
	})

	// 长片段会话：用于摘要截断断言。
	longSnippet := repeatRunes("前缀 ", 60) + "TRUNCATE-ME" + repeatRunes(" 后缀", 60)
	writeSessionFixture(t, sessionsDir, sessionFixture{
		Stem: "agent_main_snippets",
		Key:  "agent:main:snippets",
		Messages: []providers.Message{
			userMessage(longSnippet, fixtureTime(40*time.Minute)),
		},
	})

	// 符号链接（指向 workspace 外）：必须被忽略。
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.jsonl")
	if err := os.WriteFile(outsidePath, []byte(`{"role":"user","content":"outside-secret bug-123"}`+"\n"), 0o644); err != nil {
		t.Fatalf("写入符号链接目标失败: %v", err)
	}
	if err := os.Symlink(outsidePath, filepath.Join(sessionsDir, "agent_main_link.jsonl")); err != nil {
		t.Logf("当前平台不支持符号链接，跳过符号链接断言: %v", err)
	}

	// workspace 根目录下的 JSONL：不在 sessions 目录内，必须被忽略。
	if err := os.WriteFile(
		filepath.Join(workspace, "sessions_outside.jsonl"),
		[]byte(`{"role":"user","content":"outside-root-marker"}`+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("写入 sessions 目录外文件失败: %v", err)
	}

	return workspace, sessionsDir
}

// repeatRunes 生成 n 个重复 rune 的字符串。
func repeatRunes(runeText string, n int) string {
	runes := []rune(runeText)
	out := make([]rune, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, runes[i%len(runes)])
	}
	return string(out)
}

// sessionContext 构造带当前会话 key 的工具执行上下文。
func sessionContext(sessionKey string) context.Context {
	return toolshared.WithToolSessionContext(context.Background(), "main", sessionKey, nil)
}

// resultContent 返回工具结果的 LLM 文本。
func resultContent(result *toolshared.ToolResult) string {
	if result == nil {
		return ""
	}
	return result.ContentForLLM()
}
