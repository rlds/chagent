// Package sessionquery 提供 DSH 会话检索工具组的 Go 重写：
// `session_search`（在 workspace 的会话历史中检索关键词/正则）与
// `session_trace`（按会话或回合输出时间线摘要）。
//
// 数据源（只读）：`<workspace>/sessions/*.jsonl` 与其同名 `*.meta.json`。
// JSONL 每行是一条 `providers.Message` 的 JSON（与 pkg/memory/jsonl.go 的持久化格式一致）；
// meta 中 `skip` 表示被逻辑截断的头部行数（与 JSONLStore.readMessages 一致，本工具同样跳过）。
//
// 安全边界：
//   - 只读取 `<workspace>/sessions` 目录下一层的普通 `*.jsonl` 文件；
//   - 不跟随符号链接（文件与目录条目都用 Lstat 判定，符号链接条目跳过并诊断）；
//   - 结果路径一律输出 workspace 相对路径，且拒绝任何解析到 workspace 之外的路径；
//   - 单次调用有文件数/总字节/单行字节/结果数/超时上限（见 Options）；
//   - 返回的摘要片段经过敏感信息遮罩：疑似 API key/token/secret/password/Authorization
//     等键值以及常见 token 前缀与私钥头会被替换为 `***`，避免把密钥/环境变量内容回显给模型。
//
// 与 DSH `packages/experimental/tool-session-query` 的差异（本轮裁剪）：
// DSH 该工具组围绕 SessionEvent 事件流提供 5 个工具（session_search/session_event_search/
// session_trace/session_event_trace/session_event_read），带 lineage（父子会话）、
// event surface（current/shadowed/log-only）、cursor 分页与 service-boundary 授权层。
// chagent 没有事件投影与 lineage 存储，本轮只重写其中 2 个模型工具并适配现有 JSONL：
//   - session_search：按“每个会话/每条消息”命中，返回 session/回合/文件行号/角色/片段；
//   - session_trace：把 lineage trace 替换为“会话→回合”时间线摘要（消息数、工具调用数、
//     起止时间、meta 摘要、每回合行号范围与角色计数）。
//
// 工具仅只读；两个工具都声明 ConcurrencySafe（可与其他只读工具并行派发）。
package sessionquery

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/tools/shared"
)

const (
	// SearchToolName / TraceToolName 是注册到工具注册表的稳定名称。
	SearchToolName = "session_search"
	TraceToolName  = "session_trace"

	// DefaultLimit 是一次调用默认返回的结果数；MaxLimit 是其硬上限。
	DefaultLimit = 20
	MaxLimit     = 100

	// DefaultMaxFiles 是一次调用最多扫描的会话文件数。
	DefaultMaxFiles = 256
	// DefaultMaxTotalBytes 是一次调用最多扫描的 JSONL 字节数。
	DefaultMaxTotalBytes int64 = 16 << 20
	// DefaultMaxLineBytes 是单行 JSONL 的最大字节数；超长行会被跳过并诊断
	// （防止一条畸形/超大工具结果把内存或扫描时间拖垮）。
	DefaultMaxLineBytes = 1 << 20
	// DefaultTimeout 是一次扫描的协作式总超时。
	DefaultTimeout = 5 * time.Second
	// DefaultMaxSnippetRunes 是命中摘要片段的最大 rune 数（超出以 `…` 截断）。
	DefaultMaxSnippetRunes = 160
	// DefaultMaxQueryRunes 是查询串的最大 rune 数，防止病态正则/字面串拖慢扫描。
	DefaultMaxQueryRunes = 512
	// snippetContextRunes 是摘要片段在命中位置两侧保留的上下文 rune 数。
	snippetContextRunes = 48

	// dshSlot / dshSource 是 DSH 工具组的提示元数据。
	dshSlot   = "dsh"
	dshSource = "tool_registry:dsh"

	// sessionsDirName 是 workspace 下的会话目录名（与 pkg/memory 的 JSONLStore 保持一致）。
	sessionsDirName = "sessions"
)

// Options 控制单次扫描的资源上限；零值字段在 normalized() 中回落到默认值。
type Options struct {
	// DefaultLimit 是未指定 limit 时使用的结果数（默认 DefaultLimit）。
	DefaultLimit int
	// MaxLimit 是 limit 的硬上限（默认 MaxLimit）。
	MaxLimit int
	// MaxFiles 是最多扫描的会话文件数（默认 DefaultMaxFiles）。
	MaxFiles int
	// MaxTotalBytes 是最多扫描的 JSONL 字节数（默认 DefaultMaxTotalBytes）。
	MaxTotalBytes int64
	// MaxLineBytes 是单行最大字节数，超长行跳过并诊断（默认 DefaultMaxLineBytes）。
	MaxLineBytes int
	// Timeout 是协作式扫描总超时；<=0 时回落到 DefaultTimeout。
	Timeout time.Duration
	// MaxSnippetRunes 是摘要片段最大 rune 数（默认 DefaultMaxSnippetRunes）。
	MaxSnippetRunes int
	// MaxQueryRunes 是查询串最大 rune 数（默认 DefaultMaxQueryRunes）。
	MaxQueryRunes int
}

// DefaultOptions 返回生产默认上限。
func DefaultOptions() Options {
	return Options{
		DefaultLimit:    DefaultLimit,
		MaxLimit:        MaxLimit,
		MaxFiles:        DefaultMaxFiles,
		MaxTotalBytes:   DefaultMaxTotalBytes,
		MaxLineBytes:    DefaultMaxLineBytes,
		Timeout:         DefaultTimeout,
		MaxSnippetRunes: DefaultMaxSnippetRunes,
		MaxQueryRunes:   DefaultMaxQueryRunes,
	}
}

// normalized 填充零值/非法值，保证后续扫描有确定上限。
func (o Options) normalized() Options {
	def := DefaultOptions()
	if o.DefaultLimit <= 0 {
		o.DefaultLimit = def.DefaultLimit
	}
	if o.MaxLimit <= 0 {
		o.MaxLimit = def.MaxLimit
	}
	if o.MaxLimit < o.DefaultLimit {
		o.MaxLimit = o.DefaultLimit
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = def.MaxFiles
	}
	if o.MaxTotalBytes <= 0 {
		o.MaxTotalBytes = def.MaxTotalBytes
	}
	if o.MaxLineBytes <= 0 {
		o.MaxLineBytes = def.MaxLineBytes
	}
	if o.Timeout <= 0 {
		o.Timeout = def.Timeout
	}
	if o.MaxSnippetRunes <= 0 {
		o.MaxSnippetRunes = def.MaxSnippetRunes
	}
	if o.MaxQueryRunes <= 0 {
		o.MaxQueryRunes = def.MaxQueryRunes
	}
	return o
}

// dshPromptMetadata 是 DSH 工具组的稳定提示元数据。
func dshPromptMetadata() toolshared.PromptMetadata {
	return toolshared.PromptMetadata{
		Layer:  toolshared.ToolPromptLayerCapability,
		Slot:   dshSlot,
		Source: dshSource,
	}
}

// ---------------------------------------------------------------------------
// 会话文件发现
// ---------------------------------------------------------------------------

// sessionMetaFile 是 `*.meta.json` 中本工具组消费的字段子集
// （对应 pkg/memory.SessionMeta；忽略 task_state 等无关字段）。
type sessionMetaFile struct {
	Key       string    `json:"key"`
	Summary   string    `json:"summary"`
	Skip      int       `json:"skip"`
	Count     int       `json:"count"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// sessionFile 描述一个可扫描的会话文件。
type sessionFile struct {
	// Key 是会话 key：优先取 meta.key（可还原被 sanitize 的 ':' 等字符），否则取文件名 stem。
	Key string
	// Stem 是 JSONL 文件名（不含扩展名），也是无 meta 时的会话标识。
	Stem string
	// JSONLPath / MetaPath 是绝对路径（已确认位于 sessions 目录内）。
	JSONLPath string
	MetaPath  string
	// RelPath 是用于输出的 workspace 相对路径（统一 '/'）。
	RelPath string
	Meta    sessionMetaFile
	// ModTime 是 JSONL 文件 mtime，作为缺失 created_at 时的时间兜底。
	ModTime time.Time
	Size    int64
}

// sessionDiscovery 是一次目录枚举的结果与诊断。
type sessionDiscovery struct {
	Sessions     []sessionFile
	SkippedLinks int
	BadMetaKeys  []string
}

// listSessions 枚举 `<workspace>/sessions` 下的普通 `*.jsonl` 文件。
// 符号链接、子目录与非 .jsonl 文件都被忽略；meta 读取失败不会失败整次扫描，
// 而是记录在诊断中并回落到文件名 stem。
func listSessions(workspace string) (sessionDiscovery, error) {
	var out sessionDiscovery
	sessionsDir := filepath.Join(workspace, sessionsDirName)

	dirInfo, err := os.Lstat(sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("读取会话目录失败: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return out, fmt.Errorf("会话目录不是普通目录（拒绝跟随符号链接）: %s", sessionsDirName)
	}

	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return out, fmt.Errorf("枚举会话目录失败: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			out.SkippedLinks++
			continue
		}
		abs := filepath.Join(sessionsDir, name)
		info, statErr := os.Lstat(abs)
		if statErr != nil || !info.Mode().IsRegular() {
			out.SkippedLinks++
			continue
		}
		if !pathWithinWorkspace(workspace, abs) {
			// 防御性：路径解析后越出 workspace 的条目一律不可读。
			out.SkippedLinks++
			continue
		}

		stem := strings.TrimSuffix(name, ".jsonl")
		sf := sessionFile{
			Key:       stem,
			Stem:      stem,
			JSONLPath: abs,
			RelPath:   workspaceRelPath(workspace, abs),
			ModTime:   info.ModTime(),
			Size:      info.Size(),
		}
		metaPath := filepath.Join(sessionsDir, stem+".meta.json")
		if metaInfo, metaErr := os.Lstat(metaPath); metaErr == nil &&
			metaInfo.Mode().IsRegular() && metaInfo.Mode()&os.ModeSymlink == 0 &&
			pathWithinWorkspace(workspace, metaPath) {
			sf.MetaPath = metaPath
			if raw, readErr := os.ReadFile(metaPath); readErr == nil {
				if jsonErr := json.Unmarshal(raw, &sf.Meta); jsonErr != nil {
					out.BadMetaKeys = append(out.BadMetaKeys, stem)
				} else if strings.TrimSpace(sf.Meta.Key) != "" {
					sf.Key = strings.TrimSpace(sf.Meta.Key)
				}
			} else {
				out.BadMetaKeys = append(out.BadMetaKeys, stem)
			}
		}
		out.Sessions = append(out.Sessions, sf)
	}
	return out, nil
}

// pathWithinWorkspace 报告 abs 是否位于 workspace 内（不跟随符号链接的纯路径判定）。
func pathWithinWorkspace(workspace, abs string) bool {
	rel, err := filepath.Rel(filepath.Clean(workspace), filepath.Clean(abs))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// workspaceRelPath 返回用于展示的 workspace 相对路径（统一使用 '/' 分隔符）。
func workspaceRelPath(workspace, abs string) string {
	rel, err := filepath.Rel(filepath.Clean(workspace), filepath.Clean(abs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.Base(abs)
	}
	return filepath.ToSlash(rel)
}

// matchesSessionSelector 报告会话是否匹配选择器。
// 选择器语义：空串=调用方缺省（由调用方先解析）；"*"=全部；
// 否则精确匹配 meta.key、文件名 stem 或 sanitized(key)（与 JSONL 文件名规则一致）。
func matchesSessionSelector(sf sessionFile, selector string) bool {
	if selector == "*" {
		return true
	}
	want := strings.TrimSpace(selector)
	want = strings.TrimSuffix(want, ".jsonl")
	if want == "" {
		return false
	}
	if want == sf.Key || want == sf.Stem {
		return true
	}
	return sanitizeSessionKey(want) == sf.Stem
}

// sanitizeSessionKey 复刻 pkg/memory.sanitizeKey 的文件名规则，用于把会话 key 映射到 stem。
func sanitizeSessionKey(key string) string {
	s := strings.ReplaceAll(key, ":", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// filterSessions 按选择器过滤会话；返回匹配结果。
func filterSessions(all []sessionFile, selector string) []sessionFile {
	if selector == "*" {
		return all
	}
	matched := make([]sessionFile, 0, len(all))
	for _, sf := range all {
		if matchesSessionSelector(sf, selector) {
			matched = append(matched, sf)
		}
	}
	return matched
}

// resolveDefaultSessionSelector 解析 session 参数缺省值：
// 显式 session（含 "*"）优先；否则使用工具上下文中的当前会话 key；
// 没有当前会话上下文时回落到 "*"（扫描全部），并在输出中说明。
func resolveDefaultSessionSelector(ctx context.Context, explicit string) (selector string, scopeNote string) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit, ""
	}
	if current := strings.TrimSpace(toolshared.ToolSessionKey(ctx)); current != "" {
		return current, ""
	}
	return "*", "未提供当前会话上下文，已按 session=* 检索全部会话"
}

// ---------------------------------------------------------------------------
// JSONL 行级扫描
// ---------------------------------------------------------------------------

// scanLine 是一条已解码的 JSONL 消息及其物理行号。
type scanLine struct {
	// Line 是文件内 1 起的物理行号（与编辑器/`path:line` 语义一致）。
	Line int
	Msg  providers.Message
}

// scanDiagnostics 汇总一次文件扫描中发生的降级事件。
type scanDiagnostics struct {
	FilesScanned    int
	BytesScanned    int64
	FilesSkipped    int
	CorruptLines    int
	OversizeLines   int
	UnreadableFiles []string
	HitFileLimit    bool
	HitByteLimit    bool
	HitTimeLimit    bool
}

// lineSink 接收每条已解码消息；返回 false 表示停止扫描（例如结果数已达上限）。
type lineSink func(line scanLine) bool

// scanSessionFile 逐行读取一个会话文件并把合法消息交给 sink。
//
// 诊断语义（与 pkg/memory.readMessages 对齐）：
//   - meta.skip 之前的物理行属于逻辑上已被截断的历史，直接跳过但仍计行号；
//   - 空行忽略；无法解析的 JSON 行计入 CorruptLines 后跳过（不中断整次扫描）；
//   - 超过 MaxLineBytes 的行计入 OversizeLines 后跳过（不保留超大内存）；
//   - 超时通过 HitTimeLimit 报告（调用方决定是硬错误还是部分结果）。
func scanSessionFile(
	ctx context.Context,
	sf sessionFile,
	opts Options,
	diags *scanDiagnostics,
	sink lineSink,
) error {
	f, err := os.Open(sf.JSONLPath)
	if err != nil {
		diags.UnreadableFiles = append(diags.UnreadableFiles, sf.RelPath)
		return fmt.Errorf("打开会话文件失败: %w", err)
	}
	defer f.Close()

	diags.FilesScanned++
	skip := sf.Meta.Skip
	if skip < 0 {
		skip = 0
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	lineNo := 0
	for {
		if err := ctx.Err(); err != nil {
			diags.HitTimeLimit = true
			return nil
		}
		raw, oversize, readErr := readCappedLine(reader, opts.MaxLineBytes)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			diags.UnreadableFiles = append(diags.UnreadableFiles, sf.RelPath)
			return fmt.Errorf("读取会话文件失败: %w", readErr)
		}
		if readErr == nil || len(raw) > 0 || oversize {
			lineNo++
			diags.BytesScanned += int64(len(raw))
		}
		if readErr != nil && errors.Is(readErr, io.EOF) && lineNo == 0 && len(raw) == 0 {
			break
		}
		if oversize {
			if lineNo > skip {
				diags.OversizeLines++
			}
			if readErr != nil {
				break
			}
			continue
		}
		if lineNo > skip {
			if diags.BytesScanned >= opts.MaxTotalBytes {
				diags.HitByteLimit = true
				return nil
			}
			if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 {
				var msg providers.Message
				if unmarshalErr := json.Unmarshal(trimmed, &msg); unmarshalErr != nil {
					diags.CorruptLines++
				} else if !sink(scanLine{Line: lineNo, Msg: msg}) {
					return nil
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	return nil
}

// readCappedLine 读取一行（不含换行符），长度超过 max 时返回 oversize=true 并丢弃该行内容。
// 只在内存中保留当前行，避免整体读入超大文件或为超长行分配内存。
func readCappedLine(reader *bufio.Reader, max int) ([]byte, bool, error) {
	var line []byte
	oversize := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !oversize {
			if len(line)+len(chunk) > max {
				oversize = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return trimLineEnding(line), oversize, io.EOF
			}
			return trimLineEnding(line), oversize, err
		}
		return trimLineEnding(line), oversize, nil
	}
}

// trimLineEnding 去掉行尾的 '\n' / '\r\n'。
func trimLineEnding(line []byte) []byte {
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r"))
}

// ---------------------------------------------------------------------------
// 回合与渲染辅助
// ---------------------------------------------------------------------------

// isTurnBoundaryMessage 复刻 agent 侧的回合边界定义（pkg/agent/context_budget.go）：
// 只有真实用户输入开启新回合；运行时注入的上下文消息是搭车消息；
// 自主任务轮次上下文（ContextKindTaskRound）是新的用户可见回合起点。
func isTurnBoundaryMessage(msg providers.Message) bool {
	if msg.Role != "user" {
		return false
	}
	if !msg.IsContextMessage() {
		return true
	}
	return msg.Context.Kind == protocoltypes.ContextKindTaskRound
}

// singleLine 把任意文本压成单行并折叠空白，便于 `path:line role: snippet` 输出。
func singleLine(text string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "\x00", "")), " ")
}

// truncateRunes 按 rune 截断并追加 `…`；max<=0 时原样返回。
func truncateRunes(text string, max int) string {
	if max <= 0 || utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max]) + "…"
}

// snippetAround 从 haystack 中命中位置两侧截取上下文片段；
// 命中不在 haystack 时返回开头片段（用于 tool-call 文本命中等场景）。
func snippetAround(haystack, needle string, maxRunes int) string {
	text := singleLine(haystack)
	if text == "" {
		return ""
	}
	if needle == "" {
		return truncateRunes(text, maxRunes)
	}
	// 在大小写折叠后的文本上定位命中，再把字节偏移换算成 rune 位置，
	// 避免 Unicode 大小写映射改变字节长度时切到半个 rune（甚至越界）。
	lowerText := strings.ToLower(text)
	lowerNeedle := strings.ToLower(needle)
	byteIdx := strings.Index(lowerText, lowerNeedle)
	if byteIdx < 0 {
		return truncateRunes(maskSensitiveText(text), maxRunes)
	}
	startRune := utf8.RuneCountInString(lowerText[:byteIdx])
	matchedRunes := utf8.RuneCountInString(lowerNeedle)
	totalRunes := utf8.RuneCountInString(text)
	from := startRune - snippetContextRunes
	if from < 0 {
		from = 0
	}
	to := startRune + matchedRunes + snippetContextRunes
	if to > totalRunes {
		to = totalRunes
	}
	runes := []rune(text)
	window := string(runes[from:to])
	if from > 0 {
		window = "…" + window
	}
	if to < totalRunes {
		window += "…"
	}
	return truncateRunes(maskSensitiveText(window), maxRunes)
}

// formatTimeRFC3339 格式化零值安全的 UTC 时间；零值返回 "-"。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// 参数解析
// ---------------------------------------------------------------------------

// argError 表示模型参数非法；调用方把它转换为可诊断的 ErrorResult。
type argError struct {
	Field   string
	Message string
}

func (e *argError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("参数 %q %s", e.Field, e.Message)
}

func stringArg(args map[string]any, key string) (string, bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, &argError{Field: key, Message: "必须是字符串"}
	}
	return strings.TrimSpace(value), true, nil
}

func boolArg(args map[string]any, key string, fallback bool) (bool, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return fallback, nil
	}
	value, ok := raw.(bool)
	if !ok {
		return false, &argError{Field: key, Message: "必须是布尔值"}
	}
	return value, nil
}

// intArg 解析整型参数（兼容 json.Number/float64/int 系列，拒绝小数与负值由调用方校验）。
func intArg(args map[string]any, key string) (value int, present bool, err error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch n := raw.(type) {
	case int:
		return n, true, nil
	case int64:
		return int(n), true, nil
	case float64:
		if n != float64(int(n)) {
			return 0, true, &argError{Field: key, Message: "必须是整数"}
		}
		return int(n), true, nil
	case json.Number:
		parsed, parseErr := n.Int64()
		if parseErr != nil {
			return 0, true, &argError{Field: key, Message: "必须是整数"}
		}
		return int(parsed), true, nil
	default:
		return 0, true, &argError{Field: key, Message: "必须是整数"}
	}
}

// resolveLimit 归一化 limit：非法（<=0）时回落默认值，超过硬上限时钳制。
func (o Options) resolveLimit(limit int, present bool) (value int, clamped bool, err error) {
	if !present {
		return o.DefaultLimit, false, nil
	}
	if limit <= 0 {
		return o.DefaultLimit, false, nil
	}
	if limit > o.MaxLimit {
		return o.MaxLimit, true, nil
	}
	return limit, false, nil
}

// ---------------------------------------------------------------------------
// 敏感信息遮罩
// ---------------------------------------------------------------------------

var (
	// 键值型密钥：api_key=…、token: "…"、password=…、Authorization: Bearer … 等。
	sensitiveKVRe = regexp.MustCompile(
		`(?i)\b(api[_-]?key|access[_-]?key|secret|token|password|passwd|authorization|bearer)\b(\s*[:=]\s*)["']?[A-Za-z0-9_\-./+=:@]{4,}["']?`,
	)
	// 常见 token 前缀。
	sensitiveTokenPrefixRe = regexp.MustCompile(
		`\b(?:sk-[A-Za-z0-9_\-]{8,}|ghp_[A-Za-z0-9]{8,}|gho_[A-Za-z0-9]{8,}|xox[bap]-[A-Za-z0-9-]{8,}|AKIA[0-9A-Z]{12,})\b`,
	)
	// 私钥头之后的内容全部遮罩。
	sensitivePrivateKeyRe = regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----.*`)
)

// maskSensitiveText 把疑似密钥/环境变量内容替换为 `***`。
// 说明：这是启发式遮罩（按键名/常见 token 前缀/私钥头），不保证覆盖所有密钥格式；
// 输出只回显命中片段而非整行原文，进一步降低泄露面。
func maskSensitiveText(text string) string {
	if text == "" {
		return text
	}
	masked := sensitiveKVRe.ReplaceAllStringFunc(text, func(match string) string {
		// 保留键名与分隔符，只替换值。
		loc := sensitiveKVRe.FindStringSubmatchIndex(match)
		if len(loc) >= 6 {
			return match[:loc[4]] + "***"
		}
		return "***"
	})
	masked = sensitiveTokenPrefixRe.ReplaceAllString(masked, "***")
	masked = sensitivePrivateKeyRe.ReplaceAllString(masked, "-----BEGIN PRIVATE KEY-----***")
	return masked
}

// ---------------------------------------------------------------------------
// 诊断渲染
// ---------------------------------------------------------------------------

// diagnosticLines 把扫描诊断渲染为模型可读的中文说明行。
func (d scanDiagnostics) diagnosticLines() []string {
	var lines []string
	if d.FilesSkipped > 0 {
		lines = append(lines, fmt.Sprintf("跳过 %d 个符号链接/非普通文件条目", d.FilesSkipped))
	}
	if len(d.UnreadableFiles) > 0 {
		lines = append(lines, "无法读取会话文件: "+strings.Join(d.UnreadableFiles, ", "))
	}
	if d.CorruptLines > 0 {
		lines = append(lines, fmt.Sprintf("跳过 %d 行无法解析的 JSONL（可能为写入中断的残行）", d.CorruptLines))
	}
	if d.OversizeLines > 0 {
		lines = append(lines, fmt.Sprintf("跳过 %d 行超过单行字节上限的内容", d.OversizeLines))
	}
	if d.HitFileLimit {
		lines = append(lines, "已达到单次扫描文件数上限，可能还有未扫描的会话")
	}
	if d.HitByteLimit {
		lines = append(lines, "已达到单次扫描字节上限，可能还有未扫描的内容")
	}
	if d.HitTimeLimit {
		lines = append(lines, "已达到单次扫描超时上限，结果可能不完整")
	}
	return lines
}

// appendDiagnostics 在结果文本末尾追加诊断块（无诊断时不追加）。
func appendDiagnostics(lines []string, diags scanDiagnostics) []string {
	notes := diags.diagnosticLines()
	if len(notes) == 0 {
		return lines
	}
	lines = append(lines, "", "诊断:")
	for _, note := range notes {
		lines = append(lines, "- "+note)
	}
	return lines
}

// toolError 把参数/执行错误转换为可诊断的 ErrorResult（中文前缀 + 具体原因）。
func toolError(toolName string, err error) *toolshared.ToolResult {
	return toolshared.ErrorResult(fmt.Sprintf("%s 失败: %v", toolName, err))
}
