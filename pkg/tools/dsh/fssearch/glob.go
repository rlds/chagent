package fssearch

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// GlobTool 查找路径匹配 glob 模式的文件。
//
// 它实现了 toolshared.Tool，且为只读工具：绝不创建、修改或删除文件。
// 零值不可用，请用 NewGlobTool 构造。
type GlobTool struct {
	workspace      string
	restrict       bool
	allowReadPaths []*regexp.Regexp

	// MaxResults 限制返回的路径数量（默认 100）。
	MaxResults int
	// MaxEntries 限制单次调用访问的目录条目数（默认 20000）。
	MaxEntries int
	// Timeout 限制单次调用的时长（默认 10s）。
	Timeout time.Duration
	// IgnoreDirs 是遍历时跳过的目录名集合。
	// nil 表示使用 DefaultIgnoreDirs；空的非 nil 切片表示不忽略任何目录。
	IgnoreDirs []string
}

// NewGlobTool 创建一个绑定到 workspace 的 glob 工具。当 restrict 为 true 时，
// 除非路径匹配 allowReadPaths，否则搜索被限制在工作区内
// （语义与 pkg/tools/fs 的读取工具相同）。
func NewGlobTool(workspace string, restrict bool, allowReadPaths []*regexp.Regexp) *GlobTool {
	return &GlobTool{
		workspace:      workspace,
		restrict:       restrict,
		allowReadPaths: allowReadPaths,
		MaxResults:     DefaultGlobMaxResults,
		MaxEntries:     DefaultMaxEntries,
		Timeout:        DefaultTimeout,
	}
}

// 编译期接口检查（该工具必须能作为独立能力接入，并保留其独立的提示词组元数据）。
var (
	_ toolshared.Tool                   = (*GlobTool)(nil)
	_ toolshared.PromptMetadataProvider = (*GlobTool)(nil)
)

// Name 实现 toolshared.Tool。
func (t *GlobTool) Name() string { return ToolNameGlob }

// Description 实现 toolshared.Tool。该描述面向模型，使用中文。
func (t *GlobTool) Description() string {
	return "按 glob 模式在工作区内查找文件（只返回文件，不返回目录）。" +
		"支持 **（跨目录）、*、?、[...] 与 {a,b} 花括号；pattern 不含 / 时按文件名在任意深度匹配。" +
		"默认跳过 .git、.svn、node_modules 等目录，包含隐藏文件；结果按修改时间从新到旧排序，" +
		fmt.Sprintf("最多返回 %d 个相对路径。", t.maxResults())
}

// Parameters 实现 toolshared.Tool。
func (t *GlobTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type": "string",
				"description": "要匹配文件路径的 glob 模式，例如 \"**/*.go\"、\"src/**/*.test.go\"、\"*.md\"。" +
					"pattern 不含 / 时匹配任意深度下的文件名，例如 \"*.go\" 会搜索整棵树。",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "要搜索的目录或文件；默认为整个工作区，相对路径相对工作区解析。",
			},
		},
		"required": []string{"pattern"},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider：该工具属于独立的 "dsh" 工具组。
func (t *GlobTool) PromptMetadata() toolshared.PromptMetadata { return metadataFor() }

func (t *GlobTool) maxResults() int {
	if t.MaxResults <= 0 {
		return DefaultGlobMaxResults
	}
	return t.MaxResults
}

func (t *GlobTool) maxEntries() int {
	if t.MaxEntries <= 0 {
		return DefaultMaxEntries
	}
	return t.MaxEntries
}

// Execute 实现 toolshared.Tool。
func (t *GlobTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	rawPattern, present, err := stringArg(args, "pattern")
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}
	if !present || strings.TrimSpace(rawPattern) == "" {
		return toolshared.ErrorResult("参数错误: pattern 不能为空")
	}
	rawPath, hasPath, err := stringArg(args, "path")
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}
	if hasPath && strings.TrimSpace(rawPath) == "" {
		return toolshared.ErrorResult("参数错误: path 不能为空字符串（省略该参数表示搜索整个工作区）")
	}
	matcher, err := compileGlob(rawPattern)
	if err != nil {
		return toolshared.ErrorResult("参数错误: " + err.Error())
	}

	rootAbs, err := resolveRoot(t.workspace, rawPath, t.restrict, t.allowReadPaths)
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}

	searchCtx, cancel := searchContext(ctx, t.Timeout)
	defer cancel()

	var candidates []globCandidate
	addCandidate := func(relPath, absPath string, info fs.FileInfo) {
		if matcher.Match(relPath) || matcher.Match(rootRelative(rootAbs, absPath)) {
			candidates = append(candidates, globCandidate{display: relPath, modTime: info.ModTime()})
		}
	}

	rootInfo, statErr := os.Stat(rootAbs)
	if statErr != nil {
		return toolshared.ErrorResult(fmt.Sprintf("无法访问路径: %v", statErr))
	}

	var stats walkStats
	if rootInfo.Mode().IsRegular() {
		addCandidate(displayPath(t.workspace, rootAbs, rootAbs), rootAbs, rootInfo)
	} else {
		stats, err = walkRegularFiles(
			searchCtx, rootAbs, t.workspace, ignoreDirSet(t.IgnoreDirs), t.maxEntries(),
			func(relPath, absPath string, info fs.FileInfo) error {
				addCandidate(relPath, absPath, info)
				return nil
			},
		)
		if err != nil {
			return toolshared.ErrorResult(fmt.Sprintf("搜索失败: %v", err))
		}
		if stats.timeout {
			return toolshared.ErrorResult(timeoutError(searchCtx, "glob").Error())
		}
		if stats.canceled {
			return toolshared.ErrorResult("glob 已取消")
		}
	}

	total := len(candidates)
	if total == 0 {
		result := "未找到匹配的文件"
		if stats.scanLimited {
			result += truncateFooter(fmt.Sprintf("扫描条目达到上限 %d，结果可能不完整", t.maxEntries()))
		}
		return toolshared.NewToolResult(result)
	}
	sortMatchesByModTimeDesc(candidates)
	shown := candidates
	truncated := false
	if len(shown) > t.maxResults() {
		shown = shown[:t.maxResults()]
		truncated = true
	}
	lines := make([]string, 0, len(shown))
	for _, c := range shown {
		lines = append(lines, c.display)
	}
	var footer []string
	if truncated {
		footer = append(footer, fmt.Sprintf("共找到 %d 个文件，仅显示前 %d 个；请收窄 pattern 或 path", total, t.maxResults()))
	}
	if stats.scanLimited {
		footer = append(footer, fmt.Sprintf("扫描条目达到上限 %d，结果可能不完整", t.maxEntries()))
	}
	if stats.fatalWalkErr != nil {
		footer = append(footer, "部分目录不可读，结果可能不完整")
	}
	return toolshared.NewToolResult(strings.Join(lines, "\n") + truncateFooter(footer...))
}

// rootRelative 将 absPath 表示为相对于搜索根目录的路径（使用斜杠分隔）。
func rootRelative(rootAbs, absPath string) string {
	rel, err := filepath.Rel(rootAbs, absPath)
	if err != nil {
		return filepath.ToSlash(absPath)
	}
	return filepath.ToSlash(rel)
}
