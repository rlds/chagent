// Package workspacecontext 实现工作区指令文件（AGENTS.md / CLAUDE.md 及其
// 本地覆盖文件）的发现、有界读取、渲染与持久化上下文投影。
//
// 本包是一个纯库：它自身从不调用模型 provider，目前也尚未接入 agent 循环。
// 预期的接入点是 SetupTurn（见 README.md 与 Projector.Compose）。
//
// 发现范围遵循 chess 契约：
//   - user_global 默认为 false，因此除非显式启用，只扫描工作区/项目链；
//   - 当前生效的 agent 定义文件（例如作为旧版定义使用的根目录 AGENTS.md）
//     通过 excludePath 排除；
//   - 符号链接默认被忽略，当 FollowSymlinks 开启时必须解析到允许的根目录之内。
package workspacecontext

import (
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// 上下文元数据别名。规范类型位于 pkg/providers/protocoltypes（任务书 T0.1）；
// 本包保留短别名，使发现/渲染代码读起来更自然。
type (
	// ContextMeta 是附加在上下文消息上的持久化元数据。
	ContextMeta = protocoltypes.ContextMeta
	// Change 是一次已应用的工作区指令变更。
	Change = protocoltypes.ContextChange
)

// 工作区指令上下文常量（任务书 T0.1 / prompt 规范）。
const (
	ContextKindWorkspaceInstructions = protocoltypes.ContextKindWorkspaceInstructions
	ContextFormReminder              = protocoltypes.ContextFormReminder
	ChangeActionSet                  = protocoltypes.ContextActionSet
	ChangeActionReplace              = protocoltypes.ContextActionReplace
	ChangeActionRemove               = protocoltypes.ContextActionRemove
)

// 默认预算与发现相关常量。它们与文档化的 chess profile 默认值一致
// （方案 §6.2.1 / 任务书 T0.2）。
const (
	// DefaultMaxBytes 是单条指令链（user-global 或 project）的渲染预算。
	// 合并后的消息上限为 2*MaxBytes。
	DefaultMaxBytes = 32768
	// DefaultMaxSourceBytes 是单文件的读取上限。
	DefaultMaxSourceBytes = 1048576

	// UserGlobal 指令文件始终读取 AGENTS.md；本地覆盖文件仅作用于项目目录。
	userGlobalInstructionFile = "AGENTS.md"

	// user-global 链使用的内部 scope 目录名。项目目录绝不会直接使用这些
	// 名称（见 projectDirectoryDisplay）。
	scopeUserChagent = "user-chagent"
	scopeUserAgents  = "user-agents"

	// scopeSeparator 将目录名与候选文件名拼成内部 scope key。
	// 它绝不能出现在模型可见的文本中。
	scopeSeparator = "\x00"
)

// Config 控制工作区指令的发现、读取与渲染。
type Config struct {
	Enabled               bool
	MaxBytes              int
	MaxSourceBytes        int
	ProjectRootMarkers    []string
	InstructionFiles      []string
	LocalInstructionFiles []string
	UserGlobal            bool
	FollowSymlinks        bool
	ChagentHome           string
	AgentsHome            string
}

// DefaultConfig 返回 chess profile 的默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:               true,
		MaxBytes:              DefaultMaxBytes,
		MaxSourceBytes:        DefaultMaxSourceBytes,
		ProjectRootMarkers:    []string{".git"},
		InstructionFiles:      []string{"AGENTS.md", "CLAUDE.md"},
		LocalInstructionFiles: []string{"AGENTS.local.md", "CLAUDE.local.md"},
		UserGlobal:            false,
		FollowSymlinks:        false,
	}
}

// normalize 用默认值填充零值，并丢弃非法的候选名称。
// 非法名称绝不会进入文件系统：候选名必须是纯文件名。
func (c Config) normalize() Config {
	out := c
	if out.MaxBytes <= 0 {
		out.MaxBytes = DefaultMaxBytes
	}
	if out.MaxSourceBytes <= 0 {
		out.MaxSourceBytes = DefaultMaxSourceBytes
	}
	if len(out.ProjectRootMarkers) == 0 {
		out.ProjectRootMarkers = []string{".git"}
	}
	if len(out.InstructionFiles) == 0 {
		out.InstructionFiles = []string{"AGENTS.md", "CLAUDE.md"}
	}
	if len(out.LocalInstructionFiles) == 0 {
		out.LocalInstructionFiles = []string{"AGENTS.local.md", "CLAUDE.local.md"}
	}
	out.ProjectRootMarkers = validCandidateNames(out.ProjectRootMarkers)
	out.InstructionFiles = validCandidateNames(out.InstructionFiles)
	out.LocalInstructionFiles = validCandidateNames(out.LocalInstructionFiles)
	out.ChagentHome = strings.TrimSpace(out.ChagentHome)
	out.AgentsHome = strings.TrimSpace(out.AgentsHome)
	return out
}

// validCandidateNames 丢弃空名称、分隔符以及父目录穿越。带错误的完整校验
// 位于 config 包中；发现环节采取防御式处理，使非法配置永远无法逃逸出
// 工作区根目录。
func validCandidateNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name == "." || name == ".." {
			continue
		}
		if strings.ContainsAny(name, "/\\") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// Candidate 是一个已发现的指令文件。Content 仅在完成有界读取后才会填充；
// 发现阶段本身只对路径做 stat 探测。
type Candidate struct {
	AbsolutePath  string
	DisplayPath   string
	Directory     string // 内部 scope 目录：".", "packages/app", "user-chagent"
	Name          string
	Scope         string // 内部 scope key，永不对模型可见
	Content       string
	Digest        string // 原始内容的 sha256
	TrimmedDigest string // 去除首尾空白后内容的 sha256
	Version       FileVersion
	UserGlobal    bool
	// Unavailable 标记路径存在但无法读取/探测。投影器会保留上一次有效的
	// 上下文，而不会发出移除操作。
	Unavailable bool
}

// FileVersion 标识某个磁盘上的修订版本。它被用作基于 stat 的轻量缓存键，
// 用于在读取文件内容之前做判断。
type FileVersion struct {
	Size    int64
	ModTime int64
	Inode   uint64
}

// Equal 报告两个版本是否描述同一个文件修订。
func (v FileVersion) Equal(other FileVersion) bool {
	return v == other
}

// IsZero 报告该版本是否从未被填充。
func (v FileVersion) IsZero() bool {
	return v.Size == 0 && v.ModTime == 0 && v.Inode == 0
}

// TruncatedFile 描述一个渲染后的正文因预算限制而被截断的文件。
type TruncatedFile struct {
	Path          string
	OriginalBytes int
	KeptBytes     int
}

// Rendered 是单条上下文消息的纯渲染结果。
type Rendered struct {
	Text      string
	Changes   []Change
	Included  []Candidate
	Omitted   []string
	Truncated []TruncatedFile
}

// Empty 报告是否没有渲染出任何对模型可见的内容。
func (r Rendered) Empty() bool { return r.Text == "" }

// Stats 汇总一次 Compose 调用的统计信息。它有意不包含任何文件内容。
type Stats struct {
	Baseline   bool
	Files      int
	Bytes      int
	Changes    int
	Omitted    int
	Truncated  int
	UserGlobal int
	Project    int
}

// ComposeResult 是 Projector.Compose 的输出。
//
// 当没有新内容需要注入时 Message 为 nil。非 nil 时它携带渲染后的内容
// 以及 Message.Context 元数据。
type ComposeResult struct {
	Message *providers.Message
	Stats   Stats
}

// 有界读取返回的错误。它们对失败进行分类，使调用方能区分“文件已消失”
// （发出移除操作）与“我们无法读取它”（保留上一次有效内容）。
var (
	// ErrSourceTooLarge 表示文件超过 MaxSourceBytes。该文件会被整体跳过；
	// 绝不会返回部分内容。
	ErrSourceTooLarge = errors.New("workspace instruction exceeds max_source_bytes")
	// ErrAbsent 表示路径不存在（或某一父级不是目录）。
	ErrAbsent = errors.New("workspace instruction is absent")
	// ErrUnavailable 表示路径存在但无法读取。
	ErrUnavailable = errors.New("workspace instruction is unavailable")
	// ErrSymlinkSkipped 表示符号链接策略拒绝了该路径。
	ErrSymlinkSkipped = errors.New("workspace instruction symlink skipped")
)

// IsAbsent 报告 err 是否属于确定的“不存在”。
func IsAbsent(err error) bool {
	return errors.Is(err, ErrAbsent) || errors.Is(err, fs.ErrNotExist)
}

// IsUnavailable 报告 err 是否表示“存在但不可读/不安全”。
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if IsAbsent(err) || errors.Is(err, ErrSourceTooLarge) {
		return false
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrSymlinkSkipped) {
		return true
	}
	// 权限与 I/O 错误一律视为不可用。
	return !errors.Is(err, fs.ErrNotExist)
}

// scopeKey 将 scope 目录与候选文件名拼成一个内部键。
func scopeKey(directory, name string) string {
	return directory + scopeSeparator + name
}

// splitScope 拆分内部 scope key。user-global scope 不存在分隔符歧义，
// 因为候选文件名绝不会包含该分隔符。
func splitScope(scope string) (directory, name string, ok bool) {
	idx := strings.Index(scope, scopeSeparator)
	if idx < 0 {
		return "", "", false
	}
	return scope[:idx], scope[idx+len(scopeSeparator):], true
}

// projectDirectoryDisplay 将一个内部的项目相对目录渲染为模型可见文本。
// 项目根目录为 "."；所有分隔符均为正斜杠。与保留的 user-global scope
// 名称冲突的目录会加上 "./" 前缀（方案 §6.2.2）。
func projectDirectoryDisplay(directory string) string {
	if directory == "" {
		return "."
	}
	display := filepath.ToSlash(directory)
	if display == scopeUserChagent || display == scopeUserAgents {
		return "./" + display
	}
	return display
}

// projectDisplayPath 将项目相对的文件路径渲染为使用正斜杠的形式。
func projectDisplayPath(directory, name string) string {
	dir := projectDirectoryDisplay(directory)
	if dir == "." {
		return name
	}
	return path.Join(dir, name)
}

// projectDirectoryKey 返回某个项目相对目录对应的内部 scope 目录。
// 保留名称会加上 "./" 前缀，以免与 user-global scope 冲突。
func projectDirectoryKey(directory string) string {
	if directory == "" || directory == "." {
		return "."
	}
	cleaned := path.Clean(filepath.ToSlash(directory))
	if cleaned == scopeUserChagent || cleaned == scopeUserAgents {
		return "./" + cleaned
	}
	return cleaned
}
