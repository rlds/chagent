package integrationtools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/taskstate"
)

// TaskToolOptions 配置任务控制工具（task_get、task_update、
// todo_write）。
//
// 零值只提供工具默认值；存储必须通过 Store 或 Sessions 提供
// （后者是 agent 的 SessionStore，执行时会被断言为
// session.TaskStateStore）。
type TaskToolOptions struct {
	// Sessions 是所属 agent 的会话存储。当 Store 为 nil 时，
	// 通过把 Sessions 断言为 taskstate.Store 来解析存储。
	Sessions session.SessionStore
	// Store 直接提供持久化的任务状态存储。它优先于 Sessions，
	// 主要用于测试。
	Store taskstate.Store
	// Authority 解析当前回合的来源。默认为
	// DefaultAuthority（由上下文注入的来源）。
	Authority Authority
	// Now 覆盖时钟。默认为 time.Now。
	Now func() time.Time
	// BlockedThreshold 是允许判定 blocked 之前，同一阻塞条件
	// 必须持续存在的连续轮数。默认为 3。
	BlockedThreshold int
	// DefaultMaxRounds 用于 task_update start 时省略 max_rounds
	// 的情况。默认为 taskstate.DefaultMaxRounds。
	DefaultMaxRounds int
	// AllowParallelInProgress 允许 todo_write 中存在多个
	// in_progress 待办。Chess 默认为 false。
	AllowParallelInProgress bool
}

func (o TaskToolOptions) authority() Authority {
	if o.Authority != nil {
		return o.Authority
	}
	return DefaultAuthority{}
}

func (o TaskToolOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o TaskToolOptions) blockedThreshold() int {
	if o.BlockedThreshold > 0 {
		return o.BlockedThreshold
	}
	return taskstate.DefaultBlockedThreshold
}

func (o TaskToolOptions) defaultMaxRounds() int {
	if o.DefaultMaxRounds > 0 {
		return o.DefaultMaxRounds
	}
	return taskstate.DefaultMaxRounds
}

// resolveStore 返回任务状态存储；配置的存储不支持任务状态时返回 nil。
func (o TaskToolOptions) resolveStore() taskstate.Store {
	if o.Store != nil {
		return o.Store
	}
	if o.Sessions == nil {
		return nil
	}
	store, _ := o.Sessions.(taskstate.Store)
	return store
}

// taskStateToolBase 由各任务工具内嵌。
type taskStateToolBase struct {
	opts TaskToolOptions
}

// resolve 执行每次调用通用的检查（所属会话 + 存储支持）。
func (t taskStateToolBase) resolve(ctx context.Context, toolName string) (string, taskstate.Store, *ToolResult) {
	sessionKey := strings.TrimSpace(ToolSessionKey(ctx))
	if sessionKey == "" {
		return "", nil, ErrorResult(fmt.Sprintf("错误：%s 需要所属的 agent session", toolName))
	}
	store := t.opts.resolveStore()
	if store == nil {
		return "", nil, ErrorResult(fmt.Sprintf(
			"错误：%s 需要支持 TaskState 的会话存储；当前存储不支持任务状态持久化",
			toolName,
		))
	}
	return sessionKey, store, nil
}

// loadState 加载任务状态；会话没有任务时 found 为 false。
func loadState(store taskstate.Store, sessionKey string) (taskstate.State, bool) {
	state, ok := store.GetTaskState(sessionKey)
	if !ok || state.IsZero() {
		return taskstate.State{}, false
	}
	return state.Clone(), true
}

// persistState 校验并持久化状态。
func persistState(store taskstate.Store, sessionKey string, state taskstate.State) *ToolResult {
	if err := state.Validate(); err != nil {
		return ErrorResult(fmt.Sprintf("错误：%v", err))
	}
	if err := store.SetTaskState(sessionKey, state); err != nil {
		return ErrorResult(fmt.Sprintf("错误：写入任务状态失败：%v", err))
	}
	return nil
}

// getStringArg 返回去除首尾空白的字符串参数。
func getStringArg(args map[string]any, key string) (string, bool, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("`%s` 必须是字符串", key)
	}
	return strings.TrimSpace(value), true, nil
}

// getIntArg 返回可选的整数参数。
func getIntArg(args map[string]any, key string) (int, bool, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return 0, false, nil
	}
	value, err := getInt64Arg(args, key, 0)
	if err != nil {
		return 0, true, fmt.Errorf("`%s` 必须是整数", key)
	}
	return int(value), true, nil
}

// getStringSliceArg 返回规范化后的字符串切片参数。非字符串条目
// 会被拒绝，以免工具静默丢弃 evidence。
func getStringSliceArg(args map[string]any, key string) ([]string, bool, error) {
	raw, exists := args[key]
	if !exists || raw == nil {
		return nil, false, nil
	}
	items, ok := raw.([]any)
	if !ok {
		if typed, typedOK := raw.([]string); typedOK {
			items = make([]any, len(typed))
			for i, item := range typed {
				items[i] = item
			}
		} else {
			return nil, true, fmt.Errorf("`%s` 必须是字符串数组", key)
		}
	}
	values := make([]string, 0, len(items))
	for i, item := range items {
		value, ok := item.(string)
		if !ok {
			return nil, true, fmt.Errorf("`%s` 的第 %d 项必须是字符串", key, i+1)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		values = append(values, value)
	}
	return values, true, nil
}

// getTodoArgs 解析并校验 todo_write 的 `todos` 参数。
func getTodoArgs(args map[string]any, allowParallelInProgress bool) ([]taskstate.Todo, error) {
	raw, exists := args["todos"]
	if !exists || raw == nil {
		return nil, fmt.Errorf("无效待办：`todos` 不能为空")
	}
	var items []any
	switch typed := raw.(type) {
	case []any:
		items = typed
	case []map[string]any:
		items = make([]any, len(typed))
		for i, entry := range typed {
			items[i] = entry
		}
	default:
		return nil, fmt.Errorf("无效待办：`todos` 必须是数组")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("无效待办：任务清单不能为空")
	}

	todos := make([]taskstate.Todo, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	inProgress := 0
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("无效待办：第 %d 项必须是对象", i+1)
		}
		contentRaw, contentExists := entry["content"]
		content, ok := contentRaw.(string)
		if !contentExists || !ok || strings.TrimSpace(content) == "" {
			return nil, fmt.Errorf("无效待办：`content` 必须是非空字符串")
		}
		content = strings.TrimSpace(content)
		statusRaw, statusExists := entry["status"]
		status, ok := statusRaw.(string)
		if !statusExists || !ok || !taskstate.ValidTodoStatus(status) {
			return nil, fmt.Errorf("无效待办：`status` 必须是 pending、in_progress 或 completed")
		}
		if _, duplicate := seen[content]; duplicate {
			return nil, fmt.Errorf("无效待办：内容重复 %q", content)
		}
		seen[content] = struct{}{}
		if status == taskstate.TodoInProgress {
			inProgress++
		}
		todos = append(todos, taskstate.Todo{Content: content, Status: status})
	}
	if !allowParallelInProgress && inProgress > 1 {
		return nil, fmt.Errorf("无效待办：最多只能有一个 in_progress（当前 %d 个）", inProgress)
	}
	return todos, nil
}
