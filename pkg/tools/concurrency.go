// Chagent - 超轻量级个人 AI agent

package tools

import "strings"

// ConcurrencySafe 是工具可选实现的能力接口，用于显式声明它是否可以在
// 同一模型步内与其他并发安全工具并行运行。
//
// pkg/agent 中的调度器在实现返回 true 时把工具视为并行安全。
// 不实现该接口的工具回退到本文件中保守的名称级允许列表。
type ConcurrencySafe interface {
	ConcurrencySafe() bool
}

// parallelSafeToolNames 是工具未实现 ConcurrencySafe 时使用的保守名称级允许列表。
//
// 只有只读工具属于这里：它们可以无顺序保证地并发读取同一工作区、网络或会话状态。
// 任何有副作用的工具（write_file、edit_file、append_file、exec、spawn、message 等）
// 都必须留在独占路径，除非其实现显式实现 ConcurrencySafe 并返回 true。
var parallelSafeToolNames = map[string]struct{}{
	"read_file":       {},
	"read_file_lines": {},
	"list_dir":        {},
	"grep":            {},
	"find_by_name":    {},
	"web_search":      {},
	"web_fetch":       {},
	"skill":           {},
	"skill_load":      {},
	"task_get":        {},
	"memory_search":   {},
}

// IsParallelSafe 报告 toolName 是否在可在一个工具批次中并发运行的保守名称级允许列表中。
//
// 名称会被规范化（去空白、小写），避免 provider 大小写差异意外绕过允许列表。
func IsParallelSafe(toolName string) bool {
	name := strings.ToLower(strings.TrimSpace(toolName))
	if name == "" {
		return false
	}
	_, ok := parallelSafeToolNames[name]
	return ok
}

// IsToolParallelSafe 是 IsParallelSafe 的实现感知版本：
// 优先使用工具显式的 ConcurrencySafe 能力；工具未实现时回退到名称级允许列表。
//
// 调度器使用该辅助函数，使第一方工具无需中央登记即可选择加入或退出并行执行，
// 而第三方/MCP 工具保持保守默认值。
func IsToolParallelSafe(tool Tool) bool {
	if tool == nil {
		return false
	}
	if safe, ok := tool.(ConcurrencySafe); ok {
		return safe.ConcurrencySafe()
	}
	return IsParallelSafe(tool.Name())
}
