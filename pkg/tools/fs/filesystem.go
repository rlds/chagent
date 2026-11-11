package fstools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/fileutil"
	"github.com/rlds/chagent/pkg/logger"
)

const MaxReadFileSize = 64 * 1024 // 上限 64KB，避免上下文溢出

func ValidatePathWithAllowPaths(
	path, workspace string,
	restrict bool,
	patterns []*regexp.Regexp,
) (string, error) {
	return validatePathWithAllowPaths(path, workspace, restrict, patterns)
}

func IsAllowedPath(path string, patterns []*regexp.Regexp) bool {
	return isAllowedPath(path, patterns)
}

func validatePathWithAllowPaths(
	path, workspace string,
	restrict bool,
	patterns []*regexp.Regexp,
) (string, error) {
	if workspace == "" {
		return path, fmt.Errorf("未定义工作区")
	}

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("解析工作区路径失败：%w", err)
	}

	var absPath string
	if filepath.IsAbs(path) {
		absPath = filepath.Clean(path)
	} else {
		absPath, err = filepath.Abs(filepath.Join(absWorkspace, path))
		if err != nil {
			return "", fmt.Errorf("解析文件路径失败：%w", err)
		}
	}

	if restrict {
		if isAllowedPath(absPath, patterns) {
			return absPath, nil
		}

		// 先解析工作区：在 macOS 上 /var 是指向 /private/var 的符号链接，
		// 因此规范化的绝对路径也必须与规范化后的工作区进行比较。
		workspaceReal := absWorkspace
		if resolvedWorkspace, resolveErr := filepath.EvalSymlinks(absWorkspace); resolveErr == nil {
			workspaceReal = resolvedWorkspace
		}
		if !isWithinWorkspace(absPath, absWorkspace) && !isWithinWorkspace(absPath, workspaceReal) {
			return "", fmt.Errorf("拒绝访问：路径位于工作区之外")
		}

		var resolved string

		if resolved, err = filepath.EvalSymlinks(absPath); err == nil {
			if !isWithinWorkspace(resolved, workspaceReal) {
				return "", fmt.Errorf("拒绝访问：符号链接解析到工作区之外")
			}
		} else if os.IsNotExist(err) {
			var parentResolved string
			if parentResolved, err = resolveExistingAncestor(filepath.Dir(absPath)); err == nil {
				if !isWithinWorkspace(parentResolved, workspaceReal) {
					return "", fmt.Errorf("拒绝访问：符号链接解析到工作区之外")
				}
			} else if !os.IsNotExist(err) {
				return "", fmt.Errorf("解析路径失败：%w", err)
			}
		} else {
			return "", fmt.Errorf("解析路径失败：%w", err)
		}
	}

	return absPath, nil
}

func isAllowedPath(path string, patterns []*regexp.Regexp) bool {
	if len(patterns) == 0 {
		return false
	}

	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return false
	}
	if !matchesAllowedPath(cleaned, patterns) {
		return false
	}

	resolved, err := resolvePathAgainstExistingAncestor(cleaned)
	if err != nil {
		return false
	}

	return matchesAllowedPath(resolved, patterns)
}

func matchesAllowedPath(path string, patterns []*regexp.Regexp) bool {
	cleaned := filepath.Clean(path)
	for _, pattern := range patterns {
		if pattern.MatchString(cleaned) {
			return true
		}
		if root, ok := extractAllowedPathRoot(pattern); ok && isWithinAllowedRoot(cleaned, root) {
			return true
		}
	}
	return false
}

func extractAllowedPathRoot(pattern *regexp.Regexp) (string, bool) {
	raw := pattern.String()
	if !strings.HasPrefix(raw, "^") {
		return "", false
	}

	literal := strings.TrimPrefix(raw, "^")

	// 识别常见的“目录前缀”形式：^<literal>(?:/|$)
	literal = strings.TrimSuffix(literal, "(?:/|$)")
	literal = strings.TrimSuffix(literal, `(?:\\|$)`)

	// 剔除可选的锚定目录后缀后仍含正则操作符的模式将被拒绝。这样任意正则
	// 的匹配行为保持不变，只对字面量路径启用规范化前缀匹配。
	if containsUnescapedRegexMeta(literal) {
		return "", false
	}

	unescaped, ok := unescapeRegexLiteral(literal)
	if !ok || unescaped == "" {
		return "", false
	}

	return filepath.Clean(unescaped), filepath.IsAbs(unescaped)
}

func appendUniquePath(paths []string, path string) []string {
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	return append(paths, path)
}

func containsUnescapedRegexMeta(s string) bool {
	escaped := false
	for _, r := range s {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		switch r {
		case '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '|':
			return true
		}
	}
	return escaped
}

func unescapeRegexLiteral(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))

	escaped := false
	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		b.WriteRune(r)
	}

	if escaped {
		return "", false
	}

	return b.String(), true
}

func isWithinAllowedRoot(path, root string) bool {
	candidate := filepath.Clean(path)
	allowedVariants := []string{filepath.Clean(root)}

	if resolvedRoot, err := resolvePathAgainstExistingAncestor(root); err == nil {
		allowedVariants = appendUniquePath(allowedVariants, filepath.Clean(resolvedRoot))
	}

	for _, allowedRoot := range allowedVariants {
		if isWithinWorkspace(candidate, allowedRoot) {
			return true
		}
	}

	return false
}

func resolveExistingAncestor(path string) (string, error) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if filepath.Dir(current) == current {
			return "", os.ErrNotExist
		}
	}
}

func resolvePathAgainstExistingAncestor(path string) (string, error) {
	cleaned := filepath.Clean(path)
	for current := cleaned; ; current = filepath.Dir(current) {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			suffix, relErr := filepath.Rel(current, cleaned)
			if relErr != nil {
				return "", relErr
			}
			if suffix == "." {
				return filepath.Clean(resolved), nil
			}
			return filepath.Clean(filepath.Join(resolved, suffix)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if filepath.Dir(current) == current {
			return "", os.ErrNotExist
		}
	}
}

func isWithinWorkspace(candidate, workspace string) bool {
	rel, err := filepath.Rel(filepath.Clean(workspace), filepath.Clean(candidate))
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

type ReadFileTool struct {
	fs      fileSystem
	maxSize int64
}

type ReadFileLinesTool struct {
	fs      fileSystem
	maxSize int64
}

func NewReadFileTool(
	workspace string,
	restrict bool,
	maxReadFileSize int,
	allowPaths ...[]*regexp.Regexp,
) *ReadFileTool {
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}

	maxSize := int64(maxReadFileSize)
	if maxSize <= 0 {
		maxSize = MaxReadFileSize
	}

	return &ReadFileTool{
		fs:      buildFs(workspace, restrict, patterns),
		maxSize: maxSize,
	}
}

func NewReadFileBytesTool(
	workspace string,
	restrict bool,
	maxReadFileSize int,
	allowPaths ...[]*regexp.Regexp,
) *ReadFileTool {
	return NewReadFileTool(workspace, restrict, maxReadFileSize, allowPaths...)
}

func NewReadFileLinesTool(
	workspace string,
	restrict bool,
	maxReadFileSize int,
	allowPaths ...[]*regexp.Regexp,
) *ReadFileLinesTool {
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}

	maxSize := int64(maxReadFileSize)
	if maxSize <= 0 {
		maxSize = MaxReadFileSize
	}

	return &ReadFileLinesTool{
		fs:      buildFs(workspace, restrict, patterns),
		maxSize: maxSize,
	}
}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

func (t *ReadFileLinesTool) Name() string {
	return "read_file"
}

func (t *ReadFileTool) Description() string {
	return "读取文件内容。支持通过 `offset` 和 `length` 分页读取。"
}

func (t *ReadFileLinesTool) Description() string {
	return "从文件系统读取 UTF-8 文本文件。输出始终包含行号，格式为 `LINE_NUMBER|LINE_CONTENT`（从 1 开始）。大文本文件可通过 `start_line` 和 `max_lines` 分段读取。"
}

func (t *ReadFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要读取的文件路径。",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "起始读取的字节偏移量。",
				"default":     0,
			},
			"length": map[string]any{
				"type":        "integer",
				"description": "最多读取的字节数。",
				"default":     t.maxSize,
			},
		},
		"required": []string{"path"},
	}
}

func (t *ReadFileLinesTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要读取的文件路径。",
			},
			"start_line": map[string]any{
				"type":        "integer",
				"description": "起始行号（从 1 开始，包含）。",
				"default":     1,
			},
			"max_lines": map[string]any{
				"type":        "integer",
				"description": "最多读取的行数。",
			},
		},
		"required": []string{"path"},
	}
}

func (t *ReadFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path 参数必填")
	}

	// offset（可选，默认 0）
	offset, err := getInt64Arg(args, "offset", 0)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if offset < 0 {
		return ErrorResult("offset 必须 >= 0")
	}

	// length（可选，上限为 MaxReadFileSize）
	length, err := getInt64Arg(args, "length", t.maxSize)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if length <= 0 {
		return ErrorResult("length 必须 > 0")
	}
	if length > t.maxSize {
		length = t.maxSize
	}

	file, err := t.fs.Open(path)
	if err != nil {
		return ErrorResult(err.Error())
	}
	defer file.Close()

	// 测量总大小
	totalSize := int64(-1) // -1 表示未知
	if info, statErr := file.Stat(); statErr == nil {
		totalSize = info.Size()
	}

	// 先嗅探前 512 字节以检测二进制内容，避免把它加载进 LLM 上下文。
	// 之后 seek 回 0 即可恢复文件状态。
	sniff := make([]byte, 512)
	sniffN, _ := file.Read(sniff)

	// 在应用调用方的 offset 前，先把读取位置重置到开头。
	if seeker, ok := file.(io.Seeker); ok {
		_, err = seeker.Seek(0, io.SeekStart)
		if err != nil {
			return ErrorResult(fmt.Sprintf("二进制探测后重置文件位置失败：%v", err))
		}
	} else {
		// 不可 seek：前面已消耗 sniffN 字节，下面丢弃数据以到达请求的
		// offset 时需要考虑这些字节。
		// 若 offset < sniffN，说明已读取的数据已经覆盖该位置，而不可 seek 的
		// 流无法回放这些数据，因此返回明确的错误。
		if offset < int64(sniffN) && offset > 0 {
			return ErrorResult(
				"文件不可 seek：二进制探测后无法定位到前 512 字节内的 offset",
			)
		}
	}

	// 定位到请求的 offset。
	if seeker, ok := file.(io.Seeker); ok {
		_, err = seeker.Seek(offset, io.SeekStart)
		if err != nil {
			return ErrorResult(fmt.Sprintf("定位到 offset %d 失败：%v", offset, err))
		}
	} else if offset > 0 {
		// 不可 seek 流的兜底方案：丢弃开头的字节。
		// 上面已消耗 sniffN 字节，因此需要减去它们。
		remaining := offset - int64(sniffN)
		if remaining > 0 {
			_, err = io.CopyN(io.Discard, file, remaining)
			if err != nil {
				return ErrorResult(fmt.Sprintf("前进到 offset %d 失败：%v", offset, err))
			}
		}
	}

	// 读取 length+1 字节，以便可靠判断是否还有更多内容，而不依赖 totalSize
	//（不可 seek 的流可能为 -1）。这样可避免在最后一页误报 TRUNCATED 消息。
	probe := make([]byte, length+1)
	n, err := io.ReadFull(file, probe)
	// 修复：io.ReadFull 在部分读取（0 < n < len）时返回 io.ErrUnexpectedEOF，
	// 仅在 n == 0 时返回 io.EOF。二者都是正常的终止状态，只有其余错误
	// 才是真正的失败。
	if err != nil && err != io.EOF && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrorResult(fmt.Sprintf("读取文件内容失败：%v", err))
	}

	// 只有真正读到了额外的那一个探测字节，hasMore 才为 true。
	hasMore := int64(n) > length
	data := probe[:min(int64(n), length)]

	if len(data) == 0 {
		return NewToolResult("[文件结束 - 该 offset 处没有内容]")
	}

	// 构造元数据头。
	// 使用 filepath.Base(path) 而不是原始路径，避免把内部文件系统结构
	// 泄露到 LLM 上下文中。
	readEnd := offset + int64(len(data))
	// 使用 ASCII 连字符减号而不是 en-dash（U+2013），以便下游工具和
	// 日志处理器能够解析该头部。
	readRange := fmt.Sprintf("字节 %d-%d", offset, readEnd-1)

	displayPath := filepath.Base(path)
	var header string
	if totalSize >= 0 {
		header = fmt.Sprintf(
			"[文件: %s | 总大小: %d 字节 | 本次读取: %s]",
			displayPath, totalSize, readRange,
		)
	} else {
		header = fmt.Sprintf(
			"[文件: %s | 本次读取: %s | 总大小未知]",
			displayPath, readRange,
		)
	}

	if hasMore {
		header += fmt.Sprintf(
			"\n[已截断 - 文件还有更多内容。请再次调用 read_file 并设置 offset=%d 继续。]",
			readEnd,
		)
	} else {
		header += "\n[文件结束 - 没有更多内容。]"
	}

	logger.DebugCF("tool", "ReadFileTool execution completed successfully",
		map[string]any{
			"path":       path,
			"bytes_read": len(data),
			"has_more":   hasMore,
		})

	return NewToolResult(header + "\n\n" + string(data))
}

func (t *ReadFileLinesTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path 参数必填")
	}

	startLine, err := getInt64Arg(args, "start_line", 1)
	if err != nil {
		return ErrorResult(err.Error())
	}
	if startLine < 1 {
		return ErrorResult("start_line 必须 >= 1")
	}
	if _, exists := args["offset"]; exists {
		return ErrorResult("行模式不支持 offset，请使用 start_line")
	}
	if _, exists := args["length"]; exists {
		return ErrorResult("行模式不支持 length，请使用 max_lines")
	}
	if _, exists := args["limit"]; exists {
		return ErrorResult("行模式不支持 limit，请使用 max_lines")
	}

	limit := int64(-1)
	if raw, exists := args["max_lines"]; exists && raw != nil {
		limit, err = getInt64Arg(args, "max_lines", -1)
		if err != nil {
			return ErrorResult(err.Error())
		}
		if limit <= 0 {
			return ErrorResult("max_lines 若提供则必须 > 0")
		}
	}

	file, err := t.fs.Open(path)
	if err != nil {
		return ErrorResult(err.Error())
	}
	defer file.Close()

	if info, statErr := file.Stat(); statErr == nil && info.IsDir() {
		return ErrorResult(fmt.Sprintf("打开文件失败：路径是目录：%s", path))
	}

	sample := make([]byte, 512)
	sampleN, readErr := file.Read(sample)
	if readErr != nil && readErr != io.EOF {
		return ErrorResult(fmt.Sprintf("读取文件失败：%v", readErr))
	}
	sample = sample[:sampleN]
	if isBinaryReadFileData(sample) {
		return ErrorResult("文件看起来是二进制；请将 read_file 的 mode 设为 'bytes' 做字节级读取")
	}

	reader := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(sample), file), 32*1024)

	var content strings.Builder
	lineIndex := int64(1)
	var linesRead int64
	var fileBytesRead int64
	var outputBytesRead int64
	var reachedEOF bool
	var byteBudgetTruncated bool
	var lineTruncated bool

	for lineIndex < startLine {
		hasLine, consumeErr := consumeNextLine(reader)
		if consumeErr != nil {
			return ErrorResult(fmt.Sprintf("读取文件内容失败：%v", consumeErr))
		}
		if !hasLine {
			reachedEOF = true
			break
		}
		lineIndex++
	}

	for !reachedEOF && (limit < 0 || linesRead < limit) {
		prefix := formatReadFileLinePrefix(lineIndex)
		remaining := t.maxSize - outputBytesRead - int64(len(prefix))
		if remaining <= 0 {
			byteBudgetTruncated = true
			break
		}

		line, complete, hasLine, readLineErr := readNextLinePrefix(reader, remaining)
		if readLineErr != nil {
			return ErrorResult(fmt.Sprintf("读取文件内容失败：%v", readLineErr))
		}
		if !hasLine {
			reachedEOF = true
			break
		}

		content.WriteString(prefix)
		content.Write(line)
		fileBytesRead += int64(len(line))
		outputBytesRead += int64(len(prefix) + len(line))
		linesRead++
		lineIndex++

		if !complete {
			byteBudgetTruncated = true
			lineTruncated = true
			break
		}
	}

	if !reachedEOF && !lineTruncated {
		hasMoreContent, peekErr := readerHasMoreContent(reader)
		if peekErr != nil {
			return ErrorResult(fmt.Sprintf("检查剩余文件内容失败：%v", peekErr))
		}
		if !hasMoreContent {
			reachedEOF = true
			byteBudgetTruncated = false
		}
	}

	if linesRead == 0 && content.Len() == 0 {
		return NewToolResult(fmt.Sprintf("[文件结束 - start_line=%d 及之后没有内容]", startLine))
	}

	start := startLine
	endLine := startLine + linesRead - 1
	displayPath := filepath.Base(path)
	header := fmt.Sprintf(
		"[文件: %s | 本次读取: 第 %d-%d 行（从 1 开始）| 文件字节: %d | 输出字节: %d]",
		displayPath, start, endLine, fileBytesRead, outputBytesRead,
	)

	switch {
	case lineTruncated:
		header += fmt.Sprintf(
			"\n[已截断 - 第 %d 行超过 %d 字节读取预算，行中间被截断。]",
			endLine,
			t.maxSize,
		)
	case byteBudgetTruncated:
		if limit > 0 {
			header += fmt.Sprintf(
				"\n[已截断 - 达到字节预算。请再次调用 read_file 并设置 start_line=%d、max_lines=%d 从下一行继续。]",
				startLine+linesRead,
				limit,
			)
		} else {
			header += fmt.Sprintf(
				"\n[已截断 - 达到字节预算。请再次调用 read_file 并设置 start_line=%d 从下一行继续。]",
				startLine+linesRead,
			)
		}
	case !reachedEOF && limit > 0 && linesRead >= limit:
		header += fmt.Sprintf(
			"\n[部分内容 - 还有更多内容。请再次调用 read_file 并设置 start_line=%d、max_lines=%d 继续。]",
			startLine+linesRead,
			limit,
		)
	default:
		header += "\n[文件结束 - 没有更多内容。]"
	}

	logger.DebugCF("tool", "ReadFileTool execution completed successfully",
		map[string]any{
			"path":              path,
			"lines_read":        linesRead,
			"file_bytes_read":   fileBytesRead,
			"output_bytes_read": outputBytesRead,
			"truncated":         byteBudgetTruncated,
			"tool":              t.Name(),
		})

	return NewToolResult(header + "\n\n" + content.String())
}

func formatReadFileLinePrefix(lineNumber int64) string {
	return strconv.FormatInt(lineNumber, 10) + "|"
}

func isBinaryReadFileData(data []byte) bool {
	if len(data) == 0 {
		return false
	}

	sample := data
	if len(sample) > 512 {
		sample = sample[:512]
	}

	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}

	contentType := http.DetectContentType(sample)
	if strings.HasPrefix(contentType, "text/") {
		return false
	}
	if strings.HasSuffix(contentType, "/json") ||
		strings.HasSuffix(contentType, "+json") ||
		strings.HasSuffix(contentType, "/xml") ||
		strings.HasSuffix(contentType, "+xml") ||
		strings.Contains(contentType, "javascript") {
		return false
	}

	if !utf8.Valid(sample) {
		return true
	}

	controlChars := 0
	for _, b := range sample {
		if b < 0x20 && b != '\n' && b != '\r' && b != '\t' && b != '\f' && b != '\b' {
			controlChars++
		}
	}

	return float64(controlChars)/float64(len(sample)) > 0.1
}

func consumeNextLine(reader *bufio.Reader) (bool, error) {
	sawData := false

	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			sawData = true
		}

		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			return sawData, nil
		default:
			return false, err
		}
	}
}

func readNextLinePrefix(reader *bufio.Reader, maxBytes int64) ([]byte, bool, bool, error) {
	if maxBytes <= 0 {
		return nil, false, false, nil
	}

	var out bytes.Buffer
	sawData := false
	complete := true

	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			sawData = true
			if remaining := maxBytes - int64(out.Len()); remaining > 0 {
				take := len(fragment)
				if int64(take) > remaining {
					take = int(remaining)
					complete = false
				}
				out.Write(fragment[:take])
			} else {
				complete = false
			}
		}

		switch {
		case err == nil:
			return out.Bytes(), complete, sawData, nil
		case errors.Is(err, bufio.ErrBufferFull):
			if !complete {
				return out.Bytes(), false, true, nil
			}
			continue
		case errors.Is(err, io.EOF):
			if !sawData {
				return nil, true, false, nil
			}
			return out.Bytes(), complete, true, nil
		default:
			return nil, false, false, err
		}
	}
}

func readerHasMoreContent(reader *bufio.Reader) (bool, error) {
	_, err := reader.Peek(1)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, io.EOF):
		return false, nil
	default:
		return false, err
	}
}

// getInt64Arg 从 args map 中提取整数参数；键不存在时返回提供的默认值。
func getInt64Arg(args map[string]any, key string, defaultVal int64) (int64, error) {
	raw, exists := args[key]
	if !exists {
		return defaultVal, nil
	}

	switch v := raw.(type) {
	case float64:
		if v != math.Trunc(v) {
			return 0, fmt.Errorf("%s 必须是整数，得到浮点数 %v", key, v)
		}
		if v > math.MaxInt64 || v < math.MinInt64 {
			return 0, fmt.Errorf("%s 的值 %v 超出 int64 范围", key, v)
		}
		return int64(v), nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s 参数的整数格式无效：%w", key, err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%s 参数不支持类型 %T", raw, key)
	}
}

type WriteFileTool struct {
	fs fileSystem
}

func NewWriteFileTool(
	workspace string,
	restrict bool,
	allowPaths ...[]*regexp.Regexp,
) *WriteFileTool {
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}
	return &WriteFileTool{fs: buildFs(workspace, restrict, patterns)}
}

func (t *WriteFileTool) Name() string {
	return "write_file"
}

func (t *WriteFileTool) Description() string {
	return "把内容写入文件。参数解码后按字节原样写入。遵循标准 JSON 转义：\\n 表示换行，\\\\n 表示字面量反斜杠加 n。如果文件已存在，必须设置 overwrite=true 才能覆盖。"
}

func (t *WriteFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要写入的文件路径",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "要写入文件的内容。遵循标准 JSON 转义：\\n 表示换行，\\\\n 表示字面量反斜杠加 n。",
			},
			"overwrite": map[string]any{
				"type":        "boolean",
				"description": "覆盖已有文件时必须设为 true。",
				"default":     false,
			},
		},
		"required": []string{"path", "content"},
	}
}

func (t *WriteFileTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		return ErrorResult("path 参数必填")
	}

	content, ok := args["content"].(string)
	if !ok {
		return ErrorResult("content 参数必填")
	}

	overwrite, _ := args["overwrite"].(bool)

	if !overwrite {
		if _, err := t.fs.Open(path); err == nil {
			return ErrorResult(
				fmt.Sprintf("文件 %s 已存在。如需覆盖请设置 overwrite=true。", path),
			)
		}
	}

	if err := t.fs.WriteFile(path, []byte(content)); err != nil {
		return ErrorResult(err.Error())
	}

	return SilentResult(fmt.Sprintf("文件已写入：%s", path))
}

type ListDirTool struct {
	fs fileSystem
}

func NewListDirTool(workspace string, restrict bool, allowPaths ...[]*regexp.Regexp) *ListDirTool {
	var patterns []*regexp.Regexp
	if len(allowPaths) > 0 {
		patterns = allowPaths[0]
	}
	return &ListDirTool{fs: buildFs(workspace, restrict, patterns)}
}

func (t *ListDirTool) Name() string {
	return "list_dir"
}

func (t *ListDirTool) Description() string {
	return "列出路径下的文件和目录"
}

func (t *ListDirTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "要列出的路径",
			},
		},
		"required": []string{"path"},
	}
}

func (t *ListDirTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	path, ok := args["path"].(string)
	if !ok {
		path = "."
	}

	entries, err := t.fs.ReadDir(path)
	if err != nil {
		return ErrorResult(fmt.Sprintf("读取目录失败：%v", err))
	}
	return formatDirEntries(entries)
}

func formatDirEntries(entries []os.DirEntry) *ToolResult {
	var result strings.Builder
	for _, entry := range entries {
		if entry.IsDir() {
			result.WriteString("目录：" + entry.Name() + "\n")
		} else {
			result.WriteString("文件：" + entry.Name() + "\n")
		}
	}
	return NewToolResult(result.String())
}

// fileSystem 抽象了文件的读取、写入和列举，使不受限（宿主文件系统）和
// 沙箱（os.Root）实现可以共用同一套多态接口。
type fileSystem interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte) error
	ReadDir(path string) ([]os.DirEntry, error)
	Open(path string) (fs.File, error)
}

// hostFs 是直接操作宿主文件系统的不受限 fileReadWriter。
type hostFs struct{}

func (h *hostFs) ReadFile(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("读取文件失败：文件不存在：%w", err)
		}
		if os.IsPermission(err) {
			return nil, fmt.Errorf("读取文件失败：拒绝访问：%w", err)
		}
		return nil, fmt.Errorf("读取文件失败：%w", err)
	}
	return content, nil
}

func (h *hostFs) ReadDir(path string) ([]os.DirEntry, error) {
	return os.ReadDir(path)
}

func (h *hostFs) WriteFile(path string, data []byte) error {
	// 使用统一的原子写入工具并显式 sync，以保证闪存存储的可靠性。
	// 使用 0o600（仅属主可读写）作为安全的默认权限。
	return fileutil.WriteFileAtomic(path, data, 0o600)
}

func (h *hostFs) Open(path string) (fs.File, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("打开文件失败：文件不存在：%w", err)
		}
		if os.IsPermission(err) {
			return nil, fmt.Errorf("打开文件失败：拒绝访问：%w", err)
		}
		return nil, fmt.Errorf("打开文件失败：%w", err)
	}
	return f, nil
}

// sandboxFs 是使用 os.Root 严格限制在指定工作区内操作的沙箱 fileSystem。
type sandboxFs struct {
	workspace string
}

func (r *sandboxFs) execute(path string, fn func(root *os.Root, relPath string) error) error {
	if r.workspace == "" {
		return fmt.Errorf("未定义工作区")
	}

	root, err := os.OpenRoot(r.workspace)
	if err != nil {
		return fmt.Errorf("打开工作区失败：%w", err)
	}
	defer root.Close()

	relPath, err := getSafeRelPath(r.workspace, path)
	if err != nil {
		return err
	}

	// Windows 上的 os.Root API 只接受正斜杠（/）
	relPath = filepath.ToSlash(relPath)

	return fn(root, relPath)
}

func (r *sandboxFs) ReadFile(path string) ([]byte, error) {
	var content []byte
	err := r.execute(path, func(root *os.Root, relPath string) error {
		fileContent, err := root.ReadFile(relPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("读取文件失败：文件不存在：%w", err)
			}
			// 对于根目录之外的路径，os.Root 会返回 "escapes from parent"
			if os.IsPermission(err) || strings.Contains(err.Error(), "escapes from parent") ||
				strings.Contains(err.Error(), "permission denied") {
				return fmt.Errorf("读取文件失败：拒绝访问：%w", err)
			}
			return fmt.Errorf("读取文件失败：%w", err)
		}
		content = fileContent
		return nil
	})
	return content, err
}

func (r *sandboxFs) WriteFile(path string, data []byte) error {
	return r.execute(path, func(root *os.Root, relPath string) error {
		dir := filepath.Dir(relPath)
		if dir != "." && dir != "/" {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("创建父目录失败：%w", err)
			}
		}

		// 使用原子写入模式并显式 sync，以保证闪存存储的可靠性。
		// 使用 0o600（仅属主可读写）作为安全的默认权限。
		tmpRelPath := fmt.Sprintf(".tmp-%d-%d", os.Getpid(), time.Now().UnixNano())

		tmpFile, err := root.OpenFile(tmpRelPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			root.Remove(tmpRelPath)
			return fmt.Errorf("打开临时文件失败：%w", err)
		}

		if _, err := tmpFile.Write(data); err != nil {
			_ = tmpFile.Close()
			root.Remove(tmpRelPath)
			return fmt.Errorf("写入临时文件失败：%w", err)
		}

		// 关键：重命名前必须强制同步到存储介质。
		// 这能确保数据真正写入磁盘，而不只是停留在缓存中。
		if err := tmpFile.Sync(); err != nil {
			_ = tmpFile.Close()
			root.Remove(tmpRelPath)
			return fmt.Errorf("同步临时文件失败：%w", err)
		}

		if err := tmpFile.Close(); err != nil {
			root.Remove(tmpRelPath)
			return fmt.Errorf("关闭临时文件失败：%w", err)
		}

		if err := root.Rename(tmpRelPath, relPath); err != nil {
			root.Remove(tmpRelPath)
			return fmt.Errorf("重命名临时文件覆盖目标失败：%w", err)
		}

		// 同步目录以确保重命名持久化
		if dirFile, err := root.Open("."); err == nil {
			_ = dirFile.Sync()
			_ = dirFile.Close()
		}

		return nil
	})
}

func (r *sandboxFs) ReadDir(path string) ([]os.DirEntry, error) {
	var entries []os.DirEntry
	err := r.execute(path, func(root *os.Root, relPath string) error {
		dirEntries, err := fs.ReadDir(root.FS(), relPath)
		if err != nil {
			return err
		}
		entries = dirEntries
		return nil
	})
	return entries, err
}

func (r *sandboxFs) Open(path string) (fs.File, error) {
	var f fs.File
	err := r.execute(path, func(root *os.Root, relPath string) error {
		file, err := root.Open(relPath)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("打开文件失败：文件不存在：%w", err)
			}
			if os.IsPermission(err) || strings.Contains(err.Error(), "escapes from parent") ||
				strings.Contains(err.Error(), "permission denied") {
				return fmt.Errorf("打开文件失败：拒绝访问：%w", err)
			}
			return fmt.Errorf("打开文件失败：%w", err)
		}
		f = file
		return nil
	})
	return f, err
}

// whitelistFs 包装一个 sandboxFs，并在工作区之外的特定路径匹配任一
// 提供的模式时允许访问这些路径。
type whitelistFs struct {
	sandbox  *sandboxFs
	host     hostFs
	patterns []*regexp.Regexp
}

func (w *whitelistFs) matches(path string) bool {
	return isAllowedPath(path, w.patterns)
}

func (w *whitelistFs) ReadFile(path string) ([]byte, error) {
	if w.matches(path) {
		return w.host.ReadFile(path)
	}
	return w.sandbox.ReadFile(path)
}

func (w *whitelistFs) WriteFile(path string, data []byte) error {
	if w.matches(path) {
		return w.host.WriteFile(path, data)
	}
	return w.sandbox.WriteFile(path, data)
}

func (w *whitelistFs) ReadDir(path string) ([]os.DirEntry, error) {
	if w.matches(path) {
		return w.host.ReadDir(path)
	}
	return w.sandbox.ReadDir(path)
}

func (w *whitelistFs) Open(path string) (fs.File, error) {
	if w.matches(path) {
		return w.host.Open(path)
	}
	return w.sandbox.Open(path)
}

// buildFs 根据限制设置和可选的路径白名单模式返回合适的 fileSystem 实现。
func buildFs(workspace string, restrict bool, patterns []*regexp.Regexp) fileSystem {
	if !restrict {
		return &hostFs{}
	}
	sandbox := &sandboxFs{workspace: workspace}
	if len(patterns) > 0 {
		return &whitelistFs{sandbox: sandbox, patterns: patterns}
	}
	return sandbox
}

// 辅助函数：获取供 os.Root 使用的安全相对路径
func getSafeRelPath(workspace, path string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("未定义工作区")
	}

	rel := filepath.Clean(path)
	if filepath.IsAbs(rel) {
		var err error
		rel, err = filepath.Rel(workspace, rel)
		if err != nil {
			return "", fmt.Errorf("计算相对路径失败：%w", err)
		}
	}

	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("路径逃逸工作区：%s", path)
	}

	return rel, nil
}
