package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rlds/chagent/pkg/agent/workspacecontext"
	"github.com/rlds/chagent/pkg/providers"
)

// T0.4 / T6.2 性能基线。这里不设硬性阈值（CI 机器差异较大），目的是让数字在本地可复现，
// 并捕获“每个请求都重新读取指令文件”“无变化时仍遍历整个技能树”这类回归。
//
// 运行方式：
//
//	go test ./pkg/agent -run '^$' -bench 'BenchmarkBuild' -benchtime=10x
//	go test ./pkg/agent -run '^$' -bench 'BenchmarkWorkspaceContext' -benchtime=10x

func benchWorkspace(tb testing.TB) string {
	tb.Helper()
	workspace := tb.TempDir()
	files := map[string]string{
		"AGENT.md":               "# Agent\n\n基准测试 agent。\n",
		"AGENTS.md":              "# 项目规则\n\n始终使用中文回复。\n",
		"memory/MEMORY.md":       "# 记忆\n\n用户偏好简洁回答。\n",
		"packages/app/AGENTS.md": "# 应用规则\n\n包内使用 Go。\n",
	}
	for name, content := range files {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			tb.Fatalf("write %s: %v", name, err)
		}
	}
	return workspace
}

// BenchmarkBuildSystemPromptWithCache 测量每个请求都会走的稳态（缓存命中）
// system prompt 路径。
func BenchmarkBuildSystemPromptWithCache(b *testing.B) {
	workspace := benchWorkspace(b)
	cb := NewContextBuilder(workspace)
	// 预热缓存；计时循环只能对跟踪的文件做 stat。
	_ = cb.BuildSystemPromptWithCache()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if prompt := cb.BuildSystemPromptWithCache(); prompt == "" {
			b.Fatal("empty system prompt")
		}
	}
}

// BenchmarkSourceFilesChanged 测量缓存校验的 stat 路径（当前实现还会遍历技能根目录，
// 因此该基准记录了 T6.2 计划中的“仅元数据”改写之前的基线）。
func BenchmarkSourceFilesChanged(b *testing.B) {
	workspace := benchWorkspace(b)
	cb := NewContextBuilder(workspace)
	_ = cb.BuildSystemPromptWithCache() // 填充 cachedAt/existedAtCache/技能快照

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		cb.systemPromptMutex.RLock()
		changed := cb.sourceFilesChangedLocked()
		cb.systemPromptMutex.RUnlock()
		if changed {
			b.Fatal("unexpected cache invalidation on an unchanged workspace")
		}
	}
}

// BenchmarkBuildMessages 测量带 100 条历史的完整请求组装
// （system prompt 来自缓存）。
func BenchmarkBuildMessages(b *testing.B) {
	workspace := benchWorkspace(b)
	cb := NewContextBuilder(workspace)
	_ = cb.BuildSystemPromptWithCache()

	history := make([]providers.Message, 0, 100)
	for i := 0; i < 100; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		history = append(history, providers.Message{
			Role:    role,
			Content: "这是一条用于基准测试的历史消息，包含一些中文内容以便接近真实 token 分布。",
		})
	}

	req := PromptBuildRequest{
		History:        history,
		CurrentMessage: "基准测试当前问题",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		messages := cb.BuildMessagesFromPrompt(req)
		if len(messages) < len(history)+1 {
			b.Fatalf("built %d messages, want at least %d", len(messages), len(history)+1)
		}
	}
}

// BenchmarkWorkspaceContextComposeNoChanges 测量无变化时每轮的 workspace context
// 组装：可见基线摘要一致后，发现/探测必须只做 stat，不重新读取任何指令文件。
func BenchmarkWorkspaceContextComposeNoChanges(b *testing.B) {
	workspace := benchWorkspace(b)
	projector, err := workspacecontext.NewProjector(workspacecontext.DefaultConfig(), workspace, "")
	if err != nil {
		b.Fatalf("NewProjector: %v", err)
	}
	baseline, err := projector.Compose(context.Background(), nil, nil)
	if err != nil || baseline.Message == nil {
		b.Fatalf("baseline compose = %#v, %v", baseline, err)
	}
	history := []providers.Message{*baseline.Message}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := projector.Compose(ctx, history, nil)
		if err != nil {
			b.Fatalf("Compose: %v", err)
		}
		if result.Message != nil {
			b.Fatalf("unchanged compose injected a message: %#v", result.Message.Context)
		}
	}
}
