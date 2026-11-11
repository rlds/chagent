package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rlds/chagent/pkg/providers"
)

// SubTurnSpawner 是派生子回合（sub-turn）的接口。
// 这样可以避免 tools 与 agent 包之间的循环依赖。
type SubTurnSpawner interface {
	SpawnSubTurn(ctx context.Context, cfg SubTurnConfig) (*ToolResult, error)
}

// SubTurnConfig 保存派生 sub-turn 所需的配置。
type SubTurnConfig struct {
	Model              string
	Tools              []Tool
	SystemPrompt       string
	MaxTokens          int
	Temperature        float64
	Async              bool          // true 表示异步（spawn），false 表示同步（subagent）
	Critical           bool          // 父回合正常结束后继续运行
	Timeout            time.Duration // 0 = 使用默认值（5 分钟）
	MaxContextRunes    int           // 0 = 自动，-1 = 不限制，>0 = 显式上限
	ActualSystemPrompt string
	InitialMessages    []providers.Message
	InitialTokenBudget *atomic.Int64 // 团队成员共享的 token 预算；无预算时为 nil
	TargetAgentID      string        // 若设置，则以该 agent 身份运行（使用其 workspace、模型与工具）
}

type SubagentTask struct {
	ID            string
	Task          string
	Label         string
	AgentID       string
	OriginChannel string
	OriginChatID  string
	Status        string
	Result        string
	Created       int64

	// cancel 在收到中断请求时取消任务上下文。任务创建时若没有可取消的
	// 上下文，则该字段为 nil。
	cancel context.CancelFunc
	// done 在任务进入终态时恰好被关闭一次。
	done chan struct{}
}

// SubagentTask.Status 使用的任务状态值。
const (
	SubagentStatusRunning   = "running"
	SubagentStatusCompleted = "completed"
	SubagentStatusFailed    = "failed"
	SubagentStatusCanceled  = "canceled"
)

// 控制面错误。它们会被包装，使调用方可以使用 errors.Is，并保留
// 可诊断的信息（任务并未被中断时绝不声称已中断）。
var (
	ErrSubagentTaskNotFound         = errors.New("subagent 任务不存在")
	ErrSubagentTaskNotRunning       = errors.New("subagent 任务未在运行")
	ErrSubagentTaskNotInterruptible = errors.New("subagent 任务不可中断")
)

type SpawnSubTurnFunc func(
	ctx context.Context,
	task, label, agentID string,
	tools *ToolRegistry,
	maxTokens int,
	temperature float64,
	hasMaxTokens, hasTemperature bool,
) (*ToolResult, error)

type SubagentManager struct {
	tasks          map[string]*SubagentTask
	mu             sync.RWMutex
	provider       providers.LLMProvider
	defaultModel   string
	workspace      string
	tools          *ToolRegistry
	maxIterations  int
	maxTokens      int
	temperature    float64
	hasMaxTokens   bool
	hasTemperature bool
	nextID         int
	spawner        SpawnSubTurnFunc

	// mediaResolver 在旧版 RunToolLoop 回退路径中，每次调用 LLM 之前解析
	// 工具循环消息里的 media:// 引用。
	// 这让 subagent 复用与主 agent 循环相同的媒体处理行为，同时无需导入
	// pkg/agent 而造成循环依赖。
	mediaResolver func([]providers.Message) []providers.Message
}

func NewSubagentManager(
	provider providers.LLMProvider,
	defaultModel, workspace string,
) *SubagentManager {
	return &SubagentManager{
		tasks:         make(map[string]*SubagentTask),
		provider:      provider,
		defaultModel:  defaultModel,
		workspace:     workspace,
		tools:         NewToolRegistry(),
		maxIterations: 10,
		nextID:        1,
	}
}

func (sm *SubagentManager) SetSpawner(spawner SpawnSubTurnFunc) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.spawner = spawner
}

// SetMediaResolver 注入一个消息预处理器，在每次工具循环迭代前把 media:// 引用
// 解析为 LLM 可直接使用的内容。
// 该选项仅由旧版 RunToolLoop 回退路径使用。
func (sm *SubagentManager) SetMediaResolver(
	resolver func([]providers.Message) []providers.Message,
) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.mediaResolver = resolver
}

// SetLLMOptions 设置 subagent 调用 LLM 时的最大 token 数与温度。
func (sm *SubagentManager) SetLLMOptions(maxTokens int, temperature float64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.maxTokens = maxTokens
	sm.hasMaxTokens = true
	sm.temperature = temperature
	sm.hasTemperature = true
}

// SetTools 设置 subagent 执行时使用的工具注册表。
// 若未设置，subagent 将可以使用传入的工具。
func (sm *SubagentManager) SetTools(tools *ToolRegistry) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.tools = tools
}

// RegisterTool 为 subagent 执行注册一个工具。
func (sm *SubagentManager) RegisterTool(tool Tool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.tools.Register(tool)
}

func (sm *SubagentManager) Spawn(
	ctx context.Context,
	task, label, agentID, originChannel, originChatID string,
	callback AsyncCallback,
) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	taskID := fmt.Sprintf("subagent-%d", sm.nextID)
	sm.nextID++

	taskCtx, cancel := context.WithCancel(ctx)
	subagentTask := &SubagentTask{
		ID:            taskID,
		Task:          task,
		Label:         label,
		AgentID:       agentID,
		OriginChannel: originChannel,
		OriginChatID:  originChatID,
		Status:        SubagentStatusRunning,
		Created:       time.Now().UnixMilli(),
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	sm.tasks[taskID] = subagentTask

	// 在后台启动任务，并支持上下文取消
	go sm.runTask(taskCtx, subagentTask, callback)

	if label != "" {
		return fmt.Sprintf("已派生 subagent「%s」处理任务：%s", label, task), nil
	}
	return fmt.Sprintf("已派生 subagent 处理任务：%s", task), nil
}

// SubagentSystemPrompt 返回一次 subagent 运行所用的系统提示词。
// label 可选（作为 subagent 的身份标识展示）；task 在这里也是可选的——
// 旧版调用方改用用户消息传入；background 标记父回合可能已经结束的
// 异步 spawn 运行。
func SubagentSystemPrompt(label, task string, background bool) string {
	role := "一个后台 subagent"
	if !background {
		role = "一个子 agent"
	}
	if label != "" {
		role += fmt.Sprintf("（标注为 %q）", label)
	}
	prompt := fmt.Sprintf(
		"你是%s。请独立完成任务，按需使用工具，然后报告简明结果。",
		role,
	)
	if task != "" {
		prompt += "\n\n任务：" + task
	}
	return prompt
}

func (sm *SubagentManager) runTask(
	ctx context.Context,
	task *SubagentTask,
	callback AsyncCallback,
) {
	task.Status = SubagentStatusRunning
	task.Created = time.Now().UnixMilli()
	// TODO(eventbus): 一旦 subagent 在 pkg/agent 内被建模为子回合，
	// 就应从父 AgentLoop 发出 SubTurnEnd 与 SubTurnResultDelivered，
	// 而不是由这个旧版 manager 发出。

	// 启动前检查上下文是否已被取消
	select {
	case <-ctx.Done():
		sm.mu.Lock()
		task.Status = SubagentStatusCanceled
		task.Result = "任务在执行前已取消"
		sm.finishTaskLocked(task)
		sm.mu.Unlock()
		return
	default:
	}

	sm.mu.RLock()
	spawner := sm.spawner
	tools := sm.tools
	maxIter := sm.maxIterations
	maxTokens := sm.maxTokens
	temperature := sm.temperature
	hasMaxTokens := sm.hasMaxTokens
	hasTemperature := sm.hasTemperature
	mediaResolver := sm.mediaResolver
	sm.mu.RUnlock()

	var result *ToolResult
	var err error

	if spawner != nil {
		result, err = spawner(
			ctx,
			task.Task,
			task.Label,
			task.AgentID,
			tools,
			maxTokens,
			temperature,
			hasMaxTokens,
			hasTemperature,
		)
	} else {
		// 回退到旧版 RunToolLoop
		systemPrompt := SubagentSystemPrompt(task.Label, "", false)

		messages := []providers.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: task.Task},
		}

		var llmOptions map[string]any
		if hasMaxTokens || hasTemperature {
			llmOptions = map[string]any{}
			if hasMaxTokens {
				llmOptions["max_tokens"] = maxTokens
			}
			if hasTemperature {
				llmOptions["temperature"] = temperature
			}
		}

		var loopResult *ToolLoopResult
		loopResult, err = RunToolLoop(ctx, ToolLoopConfig{
			Provider:      sm.provider,
			Model:         sm.defaultModel,
			Tools:         tools,
			MaxIterations: maxIter,
			LLMOptions:    llmOptions,
			MediaResolver: mediaResolver,
		}, messages, task.OriginChannel, task.OriginChatID)

		if err == nil {
			result = &ToolResult{
				ForLLM: fmt.Sprintf(
					"subagent「%s」已完成（迭代 %d 次）：%s",
					task.Label,
					loopResult.Iterations,
					loopResult.Content,
				),
				ForUser: loopResult.Content,
				Silent:  false,
				IsError: false,
				Async:   false,
			}
		}
	}

	sm.mu.Lock()
	defer func() {
		sm.mu.Unlock()
		// 若提供了 callback 且 result 已设置，则调用它
		if callback != nil && result != nil {
			callback(ctx, result)
		}
	}()

	if err != nil {
		task.Status = SubagentStatusFailed
		task.Result = fmt.Sprintf("错误：%v", err)
		// 检查是否已取消
		if ctx.Err() != nil {
			task.Status = SubagentStatusCanceled
			task.Result = "任务在执行过程中已取消"
		}
		result = &ToolResult{
			ForLLM:  task.Result,
			ForUser: "",
			Silent:  false,
			IsError: true,
			Async:   false,
			Err:     err,
		}
	} else {
		task.Status = SubagentStatusCompleted
		task.Result = result.ForLLM
	}
	sm.finishTaskLocked(task)
}

// finishTaskLocked 关闭任务的 done 通道并释放其上下文。
// 调用方必须持有 sm.mu。
func (sm *SubagentManager) finishTaskLocked(task *SubagentTask) {
	if task == nil {
		return
	}
	if task.done != nil {
		close(task.done)
		task.done = nil
	}
	if task.cancel != nil {
		task.cancel()
		task.cancel = nil
	}
}

// TrackedSubagentTask 是调用方自行执行任务时所注册任务的句柄
// （例如 spawn 工具委托给 AgentLoop 的 sub-turn spawner）。
// 它暴露可取消的上下文，使 interrupt_agent 能真正取消工作；
// 同时提供 Finish，让调用方发布终态与结果。
type TrackedSubagentTask struct {
	ID   string
	Ctx  context.Context
	task *SubagentTask
	sm   *SubagentManager
	once sync.Once
}

// TrackSpawn 注册一个正在运行的 subagent 任务，但不启动它。调用方负责
// 在 Ctx 下执行该工作，并恰好调用一次 Finish。
func (sm *SubagentManager) TrackSpawn(
	parent context.Context,
	task, label, agentID, originChannel, originChatID string,
) (*TrackedSubagentTask, error) {
	if sm == nil {
		return nil, errors.New("subagent 管理器为 nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	taskCtx, cancel := context.WithCancel(parent)

	sm.mu.Lock()
	taskID := fmt.Sprintf("subagent-%d", sm.nextID)
	sm.nextID++
	registered := &SubagentTask{
		ID:            taskID,
		Task:          task,
		Label:         label,
		AgentID:       agentID,
		OriginChannel: originChannel,
		OriginChatID:  originChatID,
		Status:        SubagentStatusRunning,
		Created:       time.Now().UnixMilli(),
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	sm.tasks[taskID] = registered
	sm.mu.Unlock()

	return &TrackedSubagentTask{ID: taskID, Ctx: taskCtx, task: registered, sm: sm}, nil
}

// Finish 记录终态/结果并释放任务上下文。
// 除 completed/failed/canceled 之外的状态都会被映射为 failed 而拒绝，
// 这样任务永远不会一直停留在 "running"。
func (h *TrackedSubagentTask) Finish(status, result string) {
	if h == nil || h.sm == nil || h.task == nil {
		return
	}
	h.once.Do(func() {
		switch status {
		case SubagentStatusCompleted, SubagentStatusFailed, SubagentStatusCanceled:
		default:
			if result == "" {
				result = fmt.Sprintf("任务以未预期的状态 %q 结束", status)
			}
			status = SubagentStatusFailed
		}
		h.sm.mu.Lock()
		h.task.Status = status
		h.task.Result = result
		h.sm.finishTaskLocked(h.task)
		h.sm.mu.Unlock()
	})
}

// CancelTask 请求取消一个正在运行的任务。当取消信号确实发出时，返回任务
// 快照和 nil 错误；当任务不存在、已结束或不可中断时，返回包装后的哨兵
// 错误，调用方不得声称任务已被中断。
func (sm *SubagentManager) CancelTask(taskID string) (SubagentTask, error) {
	sm.mu.RLock()
	task, ok := sm.tasks[taskID]
	if !ok {
		sm.mu.RUnlock()
		return SubagentTask{}, fmt.Errorf("%w：%s", ErrSubagentTaskNotFound, taskID)
	}
	snapshot := *task
	status := task.Status
	cancel := task.cancel
	sm.mu.RUnlock()

	switch status {
	case SubagentStatusRunning:
	default:
		return snapshot, fmt.Errorf("%w：%s（status=%s）", ErrSubagentTaskNotRunning, taskID, status)
	}
	if cancel == nil {
		return snapshot, fmt.Errorf("%w：%s", ErrSubagentTaskNotInterruptible, taskID)
	}
	cancel()
	return snapshot, nil
}

// WaitTask 阻塞等待，直到任务进入终态、上下文被取消，或任务 ID 不存在。
func (sm *SubagentManager) WaitTask(
	ctx context.Context,
	taskID string,
	poll time.Duration,
) (SubagentTask, error) {
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		task, ok := sm.GetTaskCopy(taskID)
		if !ok {
			return SubagentTask{}, fmt.Errorf("%w：%s", ErrSubagentTaskNotFound, taskID)
		}
		if task.Status != SubagentStatusRunning {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (sm *SubagentManager) GetTask(taskID string) (*SubagentTask, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	task, ok := sm.tasks[taskID]
	return task, ok
}

// GetTaskCopy 在读锁下取出并返回指定 ID 任务的副本，使调用方获得一致的
// 快照，不存在数据竞争。
func (sm *SubagentManager) GetTaskCopy(taskID string) (SubagentTask, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	task, ok := sm.tasks[taskID]
	if !ok {
		return SubagentTask{}, false
	}
	return *task, true
}

func (sm *SubagentManager) ListTasks() []*SubagentTask {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	tasks := make([]*SubagentTask, 0, len(sm.tasks))
	for _, task := range sm.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// ListTaskCopies 在读锁下取出所有任务的值副本，使调用方获得一致的快照，
// 不存在数据竞争。
func (sm *SubagentManager) ListTaskCopies() []SubagentTask {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	copies := make([]SubagentTask, 0, len(sm.tasks))
	for _, task := range sm.tasks {
		copies = append(copies, *task)
	}
	return copies
}

// SubagentTool 同步执行 subagent 任务并返回结果。
// 它以 Async=false 直接调用 SubTurnSpawner 进行同步执行。
type SubagentTool struct {
	spawner      SubTurnSpawner
	defaultModel string
	maxTokens    int
	temperature  float64
}

func NewSubagentTool(manager *SubagentManager) *SubagentTool {
	if manager == nil {
		return &SubagentTool{}
	}
	return &SubagentTool{
		defaultModel: manager.defaultModel,
		maxTokens:    manager.maxTokens,
		temperature:  manager.temperature,
	}
}

// SetSpawner 设置用于直接执行 sub-turn 的 SubTurnSpawner。
func (t *SubagentTool) SetSpawner(spawner SubTurnSpawner) {
	t.spawner = spawner
}

func (t *SubagentTool) Name() string {
	return "subagent"
}

func (t *SubagentTool) Description() string {
	return "同步执行 subagent 任务并返回结果。用于把特定任务委派给独立的 agent 实例。向用户返回执行摘要，向 LLM 返回完整详情。"
}

func (t *SubagentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "subagent 要完成的任务",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "可选的短标签（用于展示）",
			},
		},
		"required": []string{"task"},
	}
}

func (t *SubagentTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	task, ok := args["task"].(string)
	if !ok {
		return ErrorResult("缺少必填参数 task").WithError(fmt.Errorf("task parameter is required"))
	}

	label, ok := args["label"].(string)
	if !ok {
		label = ""
	}

	// 为 subagent 构建系统提示词
	systemPrompt := SubagentSystemPrompt(label, task, false)

	// 若 spawner 可用则使用它（直接调用 SpawnSubTurn）
	if t.spawner != nil {
		result, err := t.spawner.SpawnSubTurn(ctx, SubTurnConfig{
			Model:        t.defaultModel,
			Tools:        nil, // 将通过上下文从父级继承
			SystemPrompt: systemPrompt,
			MaxTokens:    t.maxTokens,
			Temperature:  t.temperature,
			Async:        false, // 同步执行
		})
		if err != nil {
			return ErrorResult(fmt.Sprintf("subagent 执行失败：%v", err)).WithError(err)
		}

		// 格式化结果用于展示
		userContent := result.ForLLM
		if result.ForUser != "" {
			userContent = result.ForUser
		}
		maxUserLen := 500
		if len(userContent) > maxUserLen {
			userContent = userContent[:maxUserLen] + "..."
		}

		labelStr := label
		if labelStr == "" {
			labelStr = "（未命名）"
		}
		llmContent := fmt.Sprintf("subagent 任务已完成：\n标签：%s\n结果：%s",
			labelStr, result.ForLLM)

		return &ToolResult{
			ForLLM:  llmContent,
			ForUser: userContent,
			Silent:  false,
			IsError: result.IsError,
			Async:   false,
		}
	}

	// 回退：未配置 spawner
	return ErrorResult("未配置 subagent 管理器").WithError(fmt.Errorf("spawner not set"))
}
