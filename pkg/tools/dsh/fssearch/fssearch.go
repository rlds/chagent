// Package fssearch 以只读、受工作区限制的工具形式，实现 DSH 文件搜索工具组
// （glob / grep）。
//
// 其行为是对 DSH `packages/fs/tool-fs-search` 工具的 Go 标准库重写，
// 裁剪为对棋类 agent 有用的那部分子集：
//
//   - glob：`pattern` + 可选 `path`，支持 `**`、`*`、`?`、`[...]` 与 `{a,b}`
//     通配，结果按修改时间排序（最新在前），包含隐藏文件，
//     默认跳过 VCS 元数据与 node_modules。
//   - grep：`pattern`（Go 正则表达式，可选固定字符串与大小写不敏感匹配），
//     可选 `path` 以及单个正向 `include` 通配，结果格式为 `path:line: text`。
//
// 所有操作都用与 pkg/tools/fs 相同的 `restrict_to_workspace` + allow_read_paths
// 语义限制在 agent 工作区内。搜索绝不跟随符号链接，且每次搜索都受条目数、
// 文件大小、扫描字节数、结果数量与超时上限约束。
package fssearch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	fstools "github.com/rlds/chagent/pkg/tools/fs"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

const (
	// ToolNameGlob 是 glob 工具面向模型的名称。
	ToolNameGlob = "glob"
	// ToolNameGrep 是 grep 工具面向模型的名称。
	ToolNameGrep = "grep"

	// PromptMetadataLayer/Slot/Source 在系统提示注册表中标识本工具组
	// （"capability" / "dsh" / "tool_registry:dsh"）。
	PromptMetadataSlot   = "dsh"
	PromptMetadataSource = "tool_registry:dsh"

	// DefaultGlobMaxResults 限制单次 glob 调用返回的路径数量。
	// 与 DSH 的 glob 上限一致。
	DefaultGlobMaxResults = 100
	// DefaultGrepMaxMatches 限制单次 grep 调用返回的 `path:line: text` 行数。
	// 与 DSH 的 grep 上限一致。
	DefaultGrepMaxMatches = 250
	// DefaultGrepMaxLineBytes 限制单行匹配内容保留的预览长度。
	DefaultGrepMaxLineBytes = 2000
	// DefaultGrepMaxFileBytes 超过该大小的文件会被跳过。
	DefaultGrepMaxFileBytes = 1 << 20 // 1 MiB
	// DefaultMaxEntries 限制单次搜索访问的目录条目数。
	DefaultMaxEntries = 20000
	// DefaultMaxScanBytes 限制单次 grep 调用读取的字节数。
	DefaultMaxScanBytes = 64 << 20 // 64 MiB
	// DefaultTimeout 限制单次搜索调用的耗时。
	DefaultTimeout = 10 * time.Second
	// DefaultMaxPatternLen 限制 glob/正则模式长度，以避免病态的匹配开销。
	DefaultMaxPatternLen = 1024

	// minMatchWindowBytes 是正则匹配使用的每行最小缓冲区。对于极长的行，
	// 若匹配起点超过该窗口则不会被检测到，以此在压缩过的文件上保持内存可控。
	minMatchWindowBytes = 64 << 10
	// maxMatchWindowBytes 限制每行匹配窗口的上限。
	maxMatchWindowBytes = 1 << 20
)

// DefaultIgnoreDirs 是默认情况下绝不进入的目录名。可通过导出的 IgnoreDirs
// 字段按工具覆盖（将其设为空的非 nil 切片即可关闭忽略）。
var DefaultIgnoreDirs = []string{
	".git", ".svn", ".hg", ".bzr", ".jj", ".sl",
	"node_modules",
}

// errStopWalk 表示访问器要求提前结束遍历。
var errStopWalk = errors.New("stop walk")

// walkStats 记录一次受限目录遍历的执行情况。
type walkStats struct {
	entries      int // 已访问的目录条目数
	files        int // 交给访问器的普通文件数
	nonRegular   int // 被跳过的符号链接/套接字/设备
	fatalWalkErr error
	canceled     bool
	timeout      bool
	scanLimited  bool // 已达到条目数上限
}

// walkVisitor 会对遍历根目录下的每个普通文件调用。返回 errStopWalk 可提前
// 结束遍历（例如触达搜索上限）。
type walkVisitor func(relPath, absPath string, info fs.FileInfo) error

// walkRegularFiles 在应用统一安全规则的前提下遍历 rootAbs：上下文/超时检查、
// 不跟随符号链接、忽略指定目录以及条目数上限。relPath 是展示用路径
// （尽可能使用相对工作区的路径）。
func walkRegularFiles(
	ctx context.Context,
	rootAbs, workspace string,
	ignoreDirs map[string]struct{},
	maxEntries int,
	visit walkVisitor,
) (walkStats, error) {
	stats := walkStats{}
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	err := filepath.WalkDir(rootAbs, func(absPath string, d fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			stats.canceled = errors.Is(ctx.Err(), context.Canceled)
			stats.timeout = errors.Is(ctx.Err(), context.DeadlineExceeded)
			return errStopWalk
		default:
		}
		if err != nil {
			if absPath == rootAbs {
				return err
			}
			stats.fatalWalkErr = err // 尽力而为：子树不可读时继续遍历
			return nil
		}
		stats.entries++
		if stats.entries > maxEntries {
			stats.scanLimited = true
			return errStopWalk
		}
		if d.IsDir() {
			if absPath == rootAbs {
				return nil
			}
			if _, skip := ignoreDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			stats.nonRegular++ // 绝不跟随链接
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			stats.nonRegular++
			return nil
		}
		if !info.Mode().IsRegular() {
			stats.nonRegular++
			return nil
		}
		stats.files++
		if visitErr := visit(displayPath(workspace, rootAbs, absPath), absPath, info); visitErr != nil {
			if errors.Is(visitErr, errStopWalk) {
				return errStopWalk
			}
			return visitErr
		}
		return nil
	})
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	return stats, err
}

// displayPath 尽可能把 absPath 渲染为相对工作区的路径，其次相对搜索根目录，
// 最后才使用绝对路径。路径始终使用正斜杠。
func displayPath(workspace, rootAbs, absPath string) string {
	var rel string
	var err error
	if workspace != "" {
		if rel, err = filepath.Rel(workspace, absPath); err == nil && !isOutsideRel(rel) {
			return filepath.ToSlash(rel)
		}
	}
	if rootAbs != "" {
		if rel, err = filepath.Rel(rootAbs, absPath); err == nil && !isOutsideRel(rel) {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(absPath)
}

func isOutsideRel(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveRoot 用标准的 restrict/allow_read_paths 规则校验模型给出的搜索根目录
// （或工作区本身）。
func resolveRoot(workspace, requested string, restrict bool, allowReadPaths []*regexp.Regexp) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", errors.New("workspace 未配置，无法搜索")
	}
	target := workspace
	if strings.TrimSpace(requested) != "" {
		target = requested
	}
	abs, err := fstools.ValidatePathWithAllowPaths(target, workspace, restrict, allowReadPaths)
	if err != nil {
		return "", fmt.Errorf("拒绝访问: %w", err)
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", fmt.Errorf("路径不存在: %s", target)
		}
		return "", fmt.Errorf("无法访问路径 %q: %w", target, statErr)
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", fmt.Errorf("不支持的路径类型: %s", target)
	}
	return abs, nil
}

// ignoreDirSet 把工具的 IgnoreDirs 展开为查找集合（nil → 使用默认值，
// 空切片 → 不忽略任何目录）。
func ignoreDirSet(configured []string) map[string]struct{} {
	names := configured
	if names == nil {
		names = DefaultIgnoreDirs
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

// --- glob 匹配 -----------------------------------------------------------

// globMatcher 用一个或多个 glob 模式匹配以斜杠分隔的相对路径。不含 "/" 的
// 模式会在任意深度上匹配文件名本身（DSH / ripgrep 语义），`**` 匹配零个或
// 多个路径段，`*` / `?` / `[...]` 仅在单个段内生效（Go path.Match 语义）。
type globMatcher struct {
	patterns []globPattern
}

type globPattern struct {
	raw      string
	segments []string
	basename bool // 模式中不含 "/" → 在任意深度匹配文件名本身
}

// compileGlob 校验并展开 glob 模式（包括 `{a,b}` 花括号分支），生成匹配器。
func compileGlob(pattern string) (*globMatcher, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("pattern 不能为空")
	}
	if len(pattern) > DefaultMaxPatternLen {
		return nil, fmt.Errorf("pattern 过长（上限 %d 字符）", DefaultMaxPatternLen)
	}
	normalized := filepath.ToSlash(pattern)
	if strings.HasPrefix(normalized, "/") {
		return nil, errors.New("pattern 必须是相对路径（不能以 / 开头）")
	}
	for strings.HasPrefix(normalized, "./") {
		normalized = normalized[2:]
	}
	if normalized == "" || normalized == "." {
		return nil, errors.New("pattern 不能为空")
	}
	if err := validateBraceBalance(normalized); err != nil {
		return nil, err
	}
	expanded, err := expandBraces(normalized, 0)
	if err != nil {
		return nil, err
	}
	m := &globMatcher{}
	for _, raw := range expanded {
		segments := strings.Split(raw, "/")
		for _, seg := range segments {
			if seg == "" {
				return nil, errors.New("pattern 不能包含空的路径段（如 // 或结尾 /）")
			}
			if seg != "**" {
				// 提前校验该段的语法（畸形的字符类、结尾的反斜杠
				// 都会在这里报错）。
				if valErr := validateSegmentPattern(seg); valErr != nil {
					return nil, fmt.Errorf("无效的 glob 模式 %q: %w", pattern, valErr)
				}
			}
		}
		m.patterns = append(m.patterns, globPattern{
			raw:      raw,
			segments: segments,
			basename: !strings.Contains(raw, "/"),
		})
	}
	if len(m.patterns) > 64 {
		return nil, errors.New("glob 展开后的分支过多")
	}
	return m, nil
}

// Match 报告以斜杠分隔的相对路径是否匹配任一展开后的模式。
func (m *globMatcher) Match(relPath string) bool {
	if m == nil {
		return false
	}
	candidate := strings.TrimPrefix(filepath.ToSlash(relPath), "./")
	segments := strings.Split(candidate, "/")
	for _, p := range m.patterns {
		if p.basename {
			if len(segments) == 0 {
				continue
			}
			if ok, err := matchSegment(p.segments[0], segments[len(segments)-1]); err == nil && ok {
				return true
			}
			continue
		}
		if matchSegments(p.segments, segments) {
			return true
		}
	}
	return false
}

// matchSegments 用 glob 段匹配路径段；`**` 会消费零个或多个段。
func matchSegments(patternSegs, nameSegs []string) bool {
	if len(patternSegs) == 0 {
		return len(nameSegs) == 0
	}
	if patternSegs[0] == "**" {
		for i := 0; i <= len(nameSegs); i++ {
			if matchSegments(patternSegs[1:], nameSegs[i:]) {
				return true
			}
		}
		return false
	}
	if len(nameSegs) == 0 {
		return false
	}
	if ok, err := matchSegment(patternSegs[0], nameSegs[0]); err != nil || !ok {
		return false
	}
	return matchSegments(patternSegs[1:], nameSegs[1:])
}

// matchSegment 用单个 glob 路径段匹配单个路径段。
//
// 支持的语法：`*`（任意个非分隔符字符）、`?`（单个字符）、`[abc]` / `[a-z]`
// 字符类（可选择用 `!` 或 `^` 取反）以及 `\` 转义。段内不会包含路径分隔符。
func matchSegment(pattern, name string) (bool, error) {
	pat := []rune(pattern)
	runes := []rune(name)
	var match func(pi, ni int) (bool, error)
	match = func(pi, ni int) (bool, error) {
		for pi < len(pat) {
			switch pat[pi] {
			case '*':
				for pi < len(pat) && pat[pi] == '*' {
					pi++
				}
				if pi == len(pat) {
					return true, nil
				}
				for k := ni; k <= len(runes); k++ {
					ok, err := match(pi, k)
					if err != nil {
						return false, err
					}
					if ok {
						return true, nil
					}
				}
				return false, nil
			case '?':
				if ni >= len(runes) {
					return false, nil
				}
				pi++
				ni++
			case '[':
				ranges, negated, next, err := parseCharClass(pat, pi)
				if err != nil {
					return false, err
				}
				if ni >= len(runes) {
					return false, nil
				}
				if runeInRanges(runes[ni], ranges) == negated {
					return false, nil
				}
				pi = next
				ni++
			case '\\':
				if pi+1 >= len(pat) {
					return false, errors.New("glob 模式以反斜杠结尾")
				}
				if ni >= len(runes) || runes[ni] != pat[pi+1] {
					return false, nil
				}
				pi += 2
				ni++
			default:
				if ni >= len(runes) || runes[ni] != pat[pi] {
					return false, nil
				}
				pi++
				ni++
			}
		}
		return ni == len(runes), nil
	}
	return match(0, 0)
}

// validateSegmentPattern 只检查单个 glob 段的语法错误，而不与名字做匹配
// （这样即使字面前缀不匹配，也不会掩盖后缀的语法错误）。
func validateSegmentPattern(segment string) error {
	pat := []rune(segment)
	for i := 0; i < len(pat); {
		switch pat[i] {
		case '*', '?':
			i++
		case '[':
			_, _, next, err := parseCharClass(pat, i)
			if err != nil {
				return err
			}
			i = next
		case '\\':
			if i+1 >= len(pat) {
				return errors.New("glob 模式段以反斜杠结尾")
			}
			i += 2
		default:
			i++
		}
	}
	return nil
}

// parseCharClass 从 pat[start] 开始解析 `[...]`。它返回符文区间、字符类是否
// 取反，以及紧跟在闭合 `]` 之后的索引。
func parseCharClass(pat []rune, start int) (ranges [][2]rune, negated bool, next int, err error) {
	i := start + 1
	if i >= len(pat) {
		return nil, false, 0, errors.New("未闭合的字符类 [")
	}
	if pat[i] == '!' || pat[i] == '^' {
		negated = true
		i++
	}
	first := true
	for {
		if i >= len(pat) {
			return nil, false, 0, errors.New("未闭合的字符类 [")
		}
		if pat[i] == ']' && !first {
			if len(ranges) == 0 {
				return nil, false, 0, errors.New("空的字符类 []")
			}
			return ranges, negated, i + 1, nil
		}
		first = false
		lo, consumed, parseErr := parseClassRune(pat, i)
		if parseErr != nil {
			return nil, false, 0, parseErr
		}
		i = consumed
		if i+1 < len(pat) && pat[i] == '-' && pat[i+1] != ']' {
			hi, consumedHi, hiErr := parseClassRune(pat, i+1)
			if hiErr != nil {
				return nil, false, 0, hiErr
			}
			if hi < lo {
				return nil, false, 0, fmt.Errorf("无效的字符类范围 %c-%c", lo, hi)
			}
			ranges = append(ranges, [2]rune{lo, hi})
			i = consumedHi
			continue
		}
		ranges = append(ranges, [2]rune{lo, lo})
	}
}

// parseClassRune 解析字符类中的一个（可能带转义的）符文。
func parseClassRune(pat []rune, i int) (rune, int, error) {
	if i >= len(pat) {
		return 0, 0, errors.New("未闭合的字符类 [")
	}
	if pat[i] == '\\' {
		if i+1 >= len(pat) {
			return 0, 0, errors.New("字符类中的反斜杠结尾")
		}
		return pat[i+1], i + 2, nil
	}
	return pat[i], i + 1, nil
}

func runeInRanges(r rune, ranges [][2]rune) bool {
	for _, rng := range ranges {
		if r >= rng[0] && r <= rng[1] {
			return true
		}
	}
	return false
}

// validateBraceBalance 拒绝含有未闭合 `{` 的 glob 模式。孤立的 `}` 会被
// 当作普通字符处理（与常见 glob 行为一致）。
func validateBraceBalance(pattern string) error {
	depth := 0
	inClass := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '{':
			if !inClass {
				depth++
			}
		case '}':
			if !inClass && depth > 0 {
				depth--
			}
		}
	}
	if depth != 0 {
		return errors.New("未闭合的 { 花括号")
	}
	return nil
}

// expandBraces 展开 `{a,b}` 分支（支持嵌套，并有数量上限）。
func expandBraces(pattern string, depth int) ([]string, error) {
	if depth > 8 {
		return nil, errors.New("glob 花括号嵌套过深")
	}
	open, close := findBracePair(pattern)
	if open < 0 {
		return []string{pattern}, nil
	}
	options, err := splitBraceOptions(pattern[open+1 : close])
	if err != nil {
		return nil, err
	}
	prefix, suffix := pattern[:open], pattern[close+1:]
	var out []string
	for _, option := range options {
		sub, subErr := expandBraces(prefix+option+suffix, depth+1)
		if subErr != nil {
			return nil, subErr
		}
		out = append(out, sub...)
		if len(out) > 64 {
			return nil, errors.New("glob 花括号展开后的分支过多")
		}
	}
	return out, nil
}

// findBracePair 返回第一个顶层 '{' 及其配对 '}' 的索引，字符类内或转义位置
// 上的花括号会被忽略。
func findBracePair(pattern string) (int, int) {
	open := -1
	depth := 0
	inClass := false
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '\\':
			i++
		case '[':
			if !inClass {
				inClass = true
			}
		case ']':
			inClass = false
		case '{':
			if inClass {
				continue
			}
			if depth == 0 {
				open = i
			}
			depth++
		case '}':
			if inClass || depth == 0 {
				continue
			}
			depth--
			if depth == 0 && open >= 0 {
				return open, i
			}
		}
	}
	return -1, -1
}

// splitBraceOptions 在顶层逗号处切分 `a,b`。
func splitBraceOptions(body string) ([]string, error) {
	var options []string
	depth := 0
	inClass := false
	start := 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '{':
			if !inClass {
				depth++
			}
		case '}':
			if !inClass {
				depth--
			}
		case ',':
			if depth == 0 && !inClass {
				options = append(options, body[start:i])
				start = i + 1
			}
		}
	}
	options = append(options, body[start:])
	if len(options) == 0 {
		return nil, errors.New("空的花括号分支")
	}
	return options, nil
}

// validateInclude 落实 DSH 的 `include` 约定：必须且只能是一个正向 glob 过滤
// （不能为空值、不能以 "!" 开头、不能是顶层逗号列表）。
func validateInclude(include string) (*globMatcher, error) {
	if strings.TrimSpace(include) == "" {
		return nil, errors.New("include 不能为空")
	}
	if strings.HasPrefix(include, "!") {
		return nil, errors.New("include 只支持正向 glob 过滤，不支持以 ! 开头的排除模式")
	}
	if hasTopLevelComma(include) {
		return nil, errors.New("include 只能是一个 glob；多个模式请使用 {a,b} 形式")
	}
	return compileGlob(include)
}

func hasTopLevelComma(s string) bool {
	depth := 0
	inClass := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '{':
			if !inClass {
				depth++
			}
		case '}':
			if !inClass && depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 && !inClass {
				return true
			}
		}
	}
	return false
}

// --- 文件读取辅助函数 -----------------------------------------------------

// previewLine 在 UTF-8 边界处把一行截断到 maxBytes，并去掉结尾的换行符 /
// 回车符。
func previewLine(line string, maxBytes int) string {
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if maxBytes <= 0 || len(line) <= maxBytes {
		return line
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	if cut == 0 {
		return "…"
	}
	return line[:cut] + "…"
}

// isBinaryFile 判断开头的字节是否像二进制内容（包含 NUL）。
func isBinaryFile(f *os.File) (bool, error) {
	buf := make([]byte, 8000)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return bytes.IndexByte(buf[:n], 0) >= 0, nil
}

// matchWindowFor 返回正则匹配使用的每行缓冲区大小：预览上限的四倍，
// 并夹在 [64KiB, 1MiB] 区间内。
func matchWindowFor(previewBytes int) int {
	window := previewBytes * 4
	if window < minMatchWindowBytes {
		window = minMatchWindowBytes
	}
	if window > maxMatchWindowBytes {
		window = maxMatchWindowBytes
	}
	return window
}

// forEachLine 逐行读取 f，每行最多保留 matchWindowBytes 用于正则匹配，并把
// previewLine(line, previewBytes) 作为保留的预览传入。更长的行只在其前缀上
// 匹配（内存保持有界），其预览也会被截断。当 fn 返回 false 时提前返回。
func forEachLine(f io.Reader, matchWindowBytes, previewBytes int, fn func(lineNo int, lineForMatch, preview string) bool) error {
	if matchWindowBytes <= 0 {
		matchWindowBytes = minMatchWindowBytes
	}
	if previewBytes <= 0 {
		previewBytes = DefaultGrepMaxLineBytes
	}
	reader := bufio.NewReaderSize(f, 64<<10)
	lineNo := 0
	for {
		lineNo++
		line, err := readCappedLine(reader, matchWindowBytes)
		if len(line) > 0 || !errors.Is(err, io.EOF) {
			if !fn(lineNo, line, previewLine(line, previewBytes)) {
				return errStopWalk
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// readCappedLine 读取一行，最多保留 maxBytes 字节，行更长时丢弃其余部分。
func readCappedLine(r *bufio.Reader, maxBytes int) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf) < maxBytes {
			remaining := maxBytes - len(buf)
			if len(chunk) < remaining {
				buf = append(buf, chunk...)
			} else {
				buf = append(buf, chunk[:remaining]...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(buf) == 0 {
			return "", err
		}
		return string(buf), err
	}
}

// truncateFooter 生成共享的“（已截断）”页脚，在结果受限制或上限影响时附加到
// glob/grep 输出的末尾。
func truncateFooter(parts ...string) string {
	filtered := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 {
		return ""
	}
	return "\n\n(" + strings.Join(filtered, "；") + ")"
}

// sortMatchesByModTimeDesc 按修改时间从新到旧排序 glob 候选，时间相同时
// 以路径作为稳定的次序。
func sortMatchesByModTimeDesc(candidates []globCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].modTime.Equal(candidates[j].modTime) {
			return candidates[i].display < candidates[j].display
		}
		return candidates[i].modTime.After(candidates[j].modTime)
	})
}

type globCandidate struct {
	display string
	modTime time.Time
}

// searchContext 派生单次调用带超时限制的上下文。
func searchContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// timeoutError 生成便于诊断的超时/取消错误。
func timeoutError(ctx context.Context, tool string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s 超时（超过单次调用时限），请收窄 pattern/path/include 范围", tool)
	}
	return fmt.Errorf("%s 已取消", tool)
}

// metadataFor 返回本工具组共享的 PromptMetadata。
func metadataFor() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{
		Layer:  toolshared.ToolPromptLayerCapability,
		Slot:   PromptMetadataSlot,
		Source: PromptMetadataSource,
	}
}

// --- 参数辅助函数 --------------------------------------------------------

// stringArg 读取必填/可选的字符串参数。参数缺失时返回 ("", false, nil)，
// 存在但类型不对时返回带说明的错误。
func stringArg(args map[string]any, key string) (string, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("参数 %s 必须是字符串", key)
	}
	return value, true, nil
}

// boolArg 读取可选的布尔参数。
func boolArg(args map[string]any, key string) (bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return false, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, fmt.Errorf("参数 %s 必须是布尔值", key)
	}
	return value, nil
}
