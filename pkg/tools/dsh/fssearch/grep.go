package fssearch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"time"

	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// GrepTool 使用 Go 正则表达式搜索文件内容。
//
// 它实现 toolshared.Tool 且为只读：绝不会创建、修改或删除文件。
// 零值不可用；请使用 NewGrepTool 构造。
type GrepTool struct {
	workspace      string
	restrict       bool
	allowReadPaths []*regexp.Regexp

	// MaxMatches 限制返回的 `path:line: text` 行数（默认 250）。
	MaxMatches int
	// MaxLineBytes 限制单条匹配行保留的预览长度（默认 2000）。
	MaxLineBytes int
	// MaxFileBytes 跳过大于该值的文件（默认 1 MiB）。
	MaxFileBytes int64
	// MaxScanBytes 限制单次调用读取的总字节数（默认 64 MiB）。
	MaxScanBytes int64
	// MaxEntries 限制单次调用访问的目录条目数（默认 20000）。
	MaxEntries int
	// Timeout 限制单次调用的持续时间（默认 10s）。
	Timeout time.Duration
	// IgnoreDirs 是遍历时跳过的目录名集合。
	// 为 nil 时使用 DefaultIgnoreDirs；非 nil 的空切片表示不启用忽略。
	IgnoreDirs []string
}

// NewGrepTool 创建一个绑定到 workspace 的 grep 工具。当 restrict 为 true 时，
// 搜索被限制在工作区内，除非路径匹配 allowReadPaths
// （与 pkg/tools/fs 中读取工具的语义相同）。
func NewGrepTool(workspace string, restrict bool, allowReadPaths []*regexp.Regexp) *GrepTool {
	return &GrepTool{
		workspace:      workspace,
		restrict:       restrict,
		allowReadPaths: allowReadPaths,
		MaxMatches:     DefaultGrepMaxMatches,
		MaxLineBytes:   DefaultGrepMaxLineBytes,
		MaxFileBytes:   DefaultGrepMaxFileBytes,
		MaxScanBytes:   DefaultMaxScanBytes,
		MaxEntries:     DefaultMaxEntries,
		Timeout:        DefaultTimeout,
	}
}

// 编译期接口检查（该工具必须能作为独立能力接入，
// 并保留其独立的 prompt 组元数据）。
var (
	_ toolshared.Tool                   = (*GrepTool)(nil)
	_ toolshared.PromptMetadataProvider = (*GrepTool)(nil)
)

// Name 实现 toolshared.Tool。
func (t *GrepTool) Name() string { return ToolNameGrep }

// Description 实现 toolshared.Tool。内容面向模型，使用中文。
func (t *GrepTool) Description() string {
	return "在工作区内按正则搜索文件内容，返回 `path:line: 文本` 形式的匹配行。" +
		"支持大小写忽略、固定字符串、文件 glob 过滤；默认跳过 .git、node_modules 等目录、二进制文件与大于 1MiB 的文件，" +
		fmt.Sprintf("最多返回 %d 条匹配，超长行只保留前 %d 字节预览。", t.maxMatches(), t.maxLineBytes())
}

// Parameters 实现 toolshared.Tool。
func (t *GrepTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "要搜索的正则表达式（Go regexp 语法）；fixed_strings=true 时按普通字符串匹配。",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "要搜索的文件或目录；默认为整个工作区，相对路径相对工作区解析。",
			},
			"include": map[string]any{
				"type": "string",
				"description": "只搜索匹配该 glob 的文件（单个正向模式，例如 \"*.go\"、\"*.{js,ts}\"）；" +
					"不支持以 ! 开头的排除模式或逗号分隔的多个模式。",
			},
			"ignore_case": map[string]any{
				"type":        "boolean",
				"description": "是否忽略大小写（默认 false）。",
			},
			"fixed_strings": map[string]any{
				"type":        "boolean",
				"description": "是否把 pattern 当作普通字符串而不是正则表达式（默认 false）。",
			},
		},
		"required": []string{"pattern"},
	}
}

// PromptMetadata 实现 toolshared.PromptMetadataProvider：该工具属于
// 独立的 "dsh" 工具组。
func (t *GrepTool) PromptMetadata() toolshared.PromptMetadata { return metadataFor() }

func (t *GrepTool) maxMatches() int {
	if t.MaxMatches <= 0 {
		return DefaultGrepMaxMatches
	}
	return t.MaxMatches
}

func (t *GrepTool) maxLineBytes() int {
	if t.MaxLineBytes <= 0 {
		return DefaultGrepMaxLineBytes
	}
	return t.MaxLineBytes
}

func (t *GrepTool) maxFileBytes() int64 {
	if t.MaxFileBytes <= 0 {
		return DefaultGrepMaxFileBytes
	}
	return t.MaxFileBytes
}

func (t *GrepTool) maxScanBytes() int64 {
	if t.MaxScanBytes <= 0 {
		return DefaultMaxScanBytes
	}
	return t.MaxScanBytes
}

func (t *GrepTool) maxEntries() int {
	if t.MaxEntries <= 0 {
		return DefaultMaxEntries
	}
	return t.MaxEntries
}

type grepMatch struct {
	path string
	line int
	text string
}

type grepRunState struct {
	matches     []grepMatch
	scanned     int64
	skippedBig  int
	binary      int
	unreadable  int
	scanLimited bool
	truncated   bool
	canceled    bool
}

// Execute 实现 toolshared.Tool。
func (t *GrepTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	pattern, present, err := stringArg(args, "pattern")
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}
	if !present || pattern == "" {
		return toolshared.ErrorResult("参数错误: pattern 不能为空")
	}
	if len(pattern) > DefaultMaxPatternLen {
		return toolshared.ErrorResult(fmt.Sprintf("参数错误: pattern 过长（上限 %d 字符）", DefaultMaxPatternLen))
	}
	rawPath, hasPath, err := stringArg(args, "path")
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}
	if hasPath && strings.TrimSpace(rawPath) == "" {
		return toolshared.ErrorResult("参数错误: path 不能为空字符串（省略该参数表示搜索整个工作区）")
	}
	rawInclude, hasInclude, err := stringArg(args, "include")
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}
	ignoreCase, err := boolArg(args, "ignore_case")
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}
	fixedStrings, err := boolArg(args, "fixed_strings")
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}

	source := pattern
	if fixedStrings {
		source = regexp.QuoteMeta(source)
	}
	if ignoreCase {
		source = "(?i)" + source
	}
	re, err := regexp.Compile(source)
	if err != nil {
		return toolshared.ErrorResult(fmt.Sprintf("参数错误: 无效的正则表达式: %v", err))
	}

	var include *globMatcher
	if hasInclude {
		if include, err = validateInclude(rawInclude); err != nil {
			return toolshared.ErrorResult("参数错误: " + err.Error())
		}
	}

	rootAbs, err := resolveRoot(t.workspace, rawPath, t.restrict, t.allowReadPaths)
	if err != nil {
		return toolshared.ErrorResult(err.Error())
	}

	searchCtx, cancel := searchContext(ctx, t.Timeout)
	defer cancel()

	state := &grepRunState{}
	maxMatches := t.maxMatches()
	maxLineBytes := t.maxLineBytes()

	processFile := func(relPath, absPath string, info fs.FileInfo) error {
		if include != nil && !include.Match(relPath) && !include.Match(rootRelative(rootAbs, absPath)) {
			return nil
		}
		if info.Size() > t.maxFileBytes() {
			state.skippedBig++
			return nil
		}
		if maxScan := t.maxScanBytes(); state.scanned+info.Size() > maxScan {
			state.scanLimited = true
			return errStopWalk
		}
		f, openErr := os.Open(absPath)
		if openErr != nil {
			state.unreadable++
			return nil
		}
		defer func() { _ = f.Close() }()
		binary, binErr := isBinaryFile(f)
		if binErr != nil {
			state.unreadable++
			return nil
		}
		if binary {
			state.binary++
			return nil
		}
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr != nil {
			state.unreadable++
			return nil
		}
		state.scanned += info.Size()
		matchWindow := matchWindowFor(maxLineBytes)
		lineErr := forEachLine(f, matchWindow, maxLineBytes, func(lineNo int, lineForMatch, preview string) bool {
			if lineNo%512 == 0 {
				select {
				case <-searchCtx.Done():
					state.canceled = true
					return false
				default:
				}
			}
			if !re.MatchString(lineForMatch) {
				return true
			}
			state.matches = append(state.matches, grepMatch{
				path: relPath,
				line: lineNo,
				text: preview,
			})
			if len(state.matches) >= maxMatches {
				state.truncated = true
				return false
			}
			return true
		})
		if errors.Is(lineErr, errStopWalk) {
			return errStopWalk
		}
		if lineErr != nil {
			state.unreadable++
		}
		return nil
	}

	rootInfo, statErr := os.Stat(rootAbs)
	if statErr != nil {
		return toolshared.ErrorResult(fmt.Sprintf("无法访问路径: %v", statErr))
	}

	var stats walkStats
	if rootInfo.Mode().IsRegular() {
		if err = processFile(displayPath(t.workspace, rootAbs, rootAbs), rootAbs, rootInfo); err != nil &&
			!errors.Is(err, errStopWalk) {
			return toolshared.ErrorResult(fmt.Sprintf("搜索失败: %v", err))
		}
	} else {
		stats, err = walkRegularFiles(
			searchCtx, rootAbs, t.workspace, ignoreDirSet(t.IgnoreDirs), t.maxEntries(), processFile,
		)
		if err != nil {
			return toolshared.ErrorResult(fmt.Sprintf("搜索失败: %v", err))
		}
		if stats.timeout {
			return toolshared.ErrorResult(timeoutError(searchCtx, "grep").Error())
		}
		if stats.canceled {
			return toolshared.ErrorResult("grep 已取消")
		}
		if stats.scanLimited && len(state.matches) < maxMatches {
			state.scanLimited = true
		}
	}
	if state.canceled || errors.Is(searchCtx.Err(), context.Canceled) {
		return toolshared.ErrorResult("grep 已取消")
	}
	if errors.Is(searchCtx.Err(), context.DeadlineExceeded) {
		return toolshared.ErrorResult(timeoutError(searchCtx, "grep").Error())
	}

	var footer []string
	if state.truncated {
		footer = append(footer, fmt.Sprintf("仅显示前 %d 条匹配；请收窄 pattern/include/path", maxMatches))
	}
	if state.scanLimited {
		footer = append(footer, fmt.Sprintf("扫描量达到上限（%d 字节），结果可能不完整", t.maxScanBytes()))
	}
	if stats.scanLimited {
		footer = append(footer, fmt.Sprintf("扫描条目达到上限 %d，结果可能不完整", t.maxEntries()))
	}
	if state.skippedBig > 0 {
		footer = append(footer, fmt.Sprintf("已跳过 %d 个大于 %d 字节的文件", state.skippedBig, t.maxFileBytes()))
	}
	if state.binary > 0 {
		footer = append(footer, fmt.Sprintf("已跳过 %d 个二进制文件", state.binary))
	}
	if state.unreadable > 0 {
		footer = append(footer, fmt.Sprintf("有 %d 个文件无法读取", state.unreadable))
	}
	if stats.fatalWalkErr != nil {
		footer = append(footer, "部分目录不可读，结果可能不完整")
	}

	if len(state.matches) == 0 {
		return toolshared.NewToolResult("未找到匹配的内容" + truncateFooter(footer...))
	}
	lines := make([]string, 0, len(state.matches))
	for _, m := range state.matches {
		lines = append(lines, fmt.Sprintf("%s:%d: %s", m.path, m.line, m.text))
	}
	return toolshared.NewToolResult(strings.Join(lines, "\n") + truncateFooter(footer...))
}
