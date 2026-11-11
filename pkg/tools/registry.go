package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/media"
	"github.com/rlds/chagent/pkg/providers"
)

type ToolEntry struct {
	Tool   Tool
	IsCore bool
	TTL    int
}

type ToolRegistry struct {
	tools      map[string]*ToolEntry
	mu         sync.RWMutex
	version    atomic.Uint64 // 在 Register/RegisterHidden 时自增，用于缓存失效
	mediaStore media.MediaStore
	allowlist  map[string]struct{}
}

type mediaStoreAware interface {
	SetMediaStore(store media.MediaStore)
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]*ToolEntry),
	}
}

// SetAllowlist 把工具注册限制在给定的运行时工具名内。
// nil 切片表示“全部允许”。非 nil 的空切片表示“全部禁止”。
func (r *ToolRegistry) SetAllowlist(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if names == nil {
		r.allowlist = nil
		return
	}

	allowlist := make(map[string]struct{}, len(names))
	for _, name := range names {
		trimmed := strings.ToLower(strings.TrimSpace(name))
		if trimmed == "" {
			continue
		}
		allowlist[trimmed] = struct{}{}
	}
	r.allowlist = allowlist
}

func (r *ToolRegistry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := tool.Name()
	if !r.toolAllowedLocked(name) {
		logger.DebugCF(
			"tools",
			"Skipped core tool registration by agent allowlist",
			map[string]any{"name": name},
		)
		return
	}
	if _, exists := r.tools[name]; exists {
		logger.WarnCF("tools", "Tool registration overwrites existing tool",
			map[string]any{"name": name})
	}
	r.tools[name] = &ToolEntry{
		Tool:   tool,
		IsCore: true,
		TTL:    0, // 核心工具不使用 TTL
	}
	if aware, ok := tool.(mediaStoreAware); ok && r.mediaStore != nil {
		aware.SetMediaStore(r.mediaStore)
	}
	r.version.Add(1)
	logger.DebugCF("tools", "Registered core tool", map[string]any{"name": name})
}

// Unregister 按名称移除工具。它供动态提供方（MCP
// tools/list_changed）使用，这些提供方必须丢弃服务端不再暴露的工具。
func (r *ToolRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return
	}
	delete(r.tools, name)
	r.version.Add(1)
}

// RegisterHidden 保存隐藏工具（仅能通过 TTL 变为可见）
func (r *ToolRegistry) RegisterHidden(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := tool.Name()
	if !r.toolAllowedLocked(name) {
		logger.DebugCF(
			"tools",
			"Skipped hidden tool registration by agent allowlist",
			map[string]any{"name": name},
		)
		return
	}
	if _, exists := r.tools[name]; exists {
		logger.WarnCF("tools", "Hidden tool registration overwrites existing tool",
			map[string]any{"name": name})
	}
	r.tools[name] = &ToolEntry{
		Tool:   tool,
		IsCore: false,
		TTL:    0,
	}
	if aware, ok := tool.(mediaStoreAware); ok && r.mediaStore != nil {
		aware.SetMediaStore(r.mediaStore)
	}
	r.version.Add(1)
	logger.DebugCF("tools", "Registered hidden tool", map[string]any{"name": name})
}

// SetMediaStore 把 MediaStore 注入到所有能够消费它的已注册工具，
// 并记录下来供后续注册使用。
func (r *ToolRegistry) SetMediaStore(store media.MediaStore) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.mediaStore = store
	for _, entry := range r.tools {
		if aware, ok := entry.Tool.(mediaStoreAware); ok {
			aware.SetMediaStore(store)
		}
	}
}

// PromoteTools 原子地为多个非核心工具设置 TTL。
// 这可避免并发的 TickTTL 在多次提升之间递减计数。
func (r *ToolRegistry) PromoteTools(names []string, ttl int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	promoted := 0
	for _, name := range names {
		if entry, exists := r.tools[name]; exists {
			if !entry.IsCore {
				entry.TTL = ttl
				promoted++
			}
		}
	}
	logger.DebugCF(
		"tools",
		"PromoteTools completed",
		map[string]any{"requested": len(names), "promoted": promoted, "ttl": ttl},
	)
}

// TickTTL 只对非核心工具递减 TTL
func (r *ToolRegistry) TickTTL() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.tools {
		if !entry.IsCore && entry.TTL > 0 {
			entry.TTL--
		}
	}
}

// Version 返回当前注册表版本（原子读取）。
func (r *ToolRegistry) Version() uint64 {
	return r.version.Load()
}

func (r *ToolRegistry) toolAllowedLocked(name string) bool {
	if r.allowlist == nil {
		return true
	}
	if isToolDiscoveryToolName(name) {
		// 发现类工具属于 MCP 控制平面：只要配置了就必须始终可用，
		// 这样延迟加载的 MCP 工具仍能被解锁。针对单个 agent 的允许列表
		// 仍会在 RegisterHidden 期间作用于隐藏的 MCP 工具本身。
		return true
	}
	_, ok := r.allowlist[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// HasRegistered 报告注册表中是否存在某个工具名，
// 包括 TTL 当前为 0 的隐藏工具。
func (r *ToolRegistry) HasRegistered(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.tools[name]
	return ok
}

// HiddenToolSnapshot 保存隐藏工具的一致快照以及拍摄快照时的
// 注册表版本。供 BM25SearchTool 缓存使用。
type HiddenToolSnapshot struct {
	Docs    []HiddenToolDoc
	Version uint64
}

// HiddenToolDoc 是隐藏工具用于搜索索引的轻量表示。
type HiddenToolDoc struct {
	Name        string
	Description string
}

// SnapshotHiddenTools 在一次读锁内返回所有非核心工具以及当前注册表
// 版本，从而保证这两个值之间的一致性。
func (r *ToolRegistry) SnapshotHiddenTools() HiddenToolSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	docs := make([]HiddenToolDoc, 0, len(r.tools))
	for name, entry := range r.tools {
		if !entry.IsCore {
			docs = append(docs, HiddenToolDoc{
				Name:        name,
				Description: entry.Tool.Description(),
			})
		}
	}
	return HiddenToolSnapshot{
		Docs:    docs,
		Version: r.version.Load(),
	}
}

func (r *ToolRegistry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.tools[name]
	if !ok {
		return nil, false
	}
	// TTL 已过期的隐藏工具不可调用。
	if !entry.IsCore && entry.TTL <= 0 {
		return nil, false
	}
	return entry.Tool, true
}

func (r *ToolRegistry) Execute(ctx context.Context, name string, args map[string]any) *ToolResult {
	return r.ExecuteWithContext(ctx, name, args, "", "", nil)
}

// ExecuteWithContext 在带 channel/chatID 上下文的条件下执行工具，并可选传入异步回调。
// 若工具实现了 AsyncExecutor 且提供了非 nil 回调，则会调用 ExecuteAsync 而不是
// Execute——回调只是参数，绝不会作为可变状态保存在工具上。
func (r *ToolRegistry) ExecuteWithContext(
	ctx context.Context,
	name string,
	args map[string]any,
	channel, chatID string,
	asyncCallback AsyncCallback,
) *ToolResult {
	logger.InfoCF("tool", "Tool execution started",
		map[string]any{
			"tool": name,
			"args": args,
		})

	tool, ok := r.Get(name)
	if !ok {
		logger.ErrorCF("tool", "Tool not found",
			map[string]any{
				"tool": name,
			})
		return ErrorResult(
			fmt.Sprintf("工具 %q 不存在", name),
		).WithError(fmt.Errorf("tool not found"))
	}

	// 依据工具声明的 schema 校验参数。
	if err := validateToolArgs(tool.Parameters(), args); err != nil {
		logger.WarnCF("tool", "Tool argument validation failed",
			map[string]any{"tool": name, "error": err.Error()})
		return ErrorResult(fmt.Sprintf("工具 %q 的参数无效：%s", name, err)).
			WithError(fmt.Errorf("argument validation failed: %w", err))
	}

	// 把 channel/chatID 注入 ctx，使工具可通过 ToolChannel(ctx)/ToolChatID(ctx) 读取。
	// 总是注入——具体需要什么由工具自行校验。
	ctx = WithToolContext(ctx, channel, chatID)

	// 若工具实现了 AsyncExecutor 且提供了回调，则走 ExecuteAsync 分支。
	// 回调是调用参数，而不是工具实例上的可变状态。
	var result *ToolResult
	start := time.Now()

	// 使用 recover 捕获工具执行期间的任何 panic
	// 这样某个工具崩溃不会导致整个 agent 退出
	func() {
		defer func() {
			if re := recover(); re != nil {
				logger.RecoverPanicNoExit(re)
				errMsg := fmt.Sprintf("工具 '%s' 执行崩溃（panic）：%v", name, re)
				logger.ErrorCF("tool", "Tool execution panic recovered",
					map[string]any{
						"tool":  name,
						"panic": fmt.Sprintf("%v", re),
					})
				result = &ToolResult{
					ForLLM:  errMsg,
					ForUser: errMsg,
					IsError: true,
					Err:     fmt.Errorf("panic: %v", re),
				}
			}
		}()

		if asyncExec, ok := tool.(AsyncExecutor); ok && asyncCallback != nil {
			logger.DebugCF("tool", "Executing async tool via ExecuteAsync",
				map[string]any{
					"tool": name,
				})
			result = asyncExec.ExecuteAsync(ctx, args, asyncCallback)
		} else {
			result = tool.Execute(ctx, args)
		}
	}()

	// 处理 nil 结果（正常情况下不会发生，属于防御性处理）
	if result == nil {
		result = &ToolResult{
			ForLLM:  fmt.Sprintf("工具 '%s' 意外返回了 nil 结果", name),
			ForUser: fmt.Sprintf("工具 '%s' 意外返回了 nil 结果", name),
			IsError: true,
			Err:     fmt.Errorf("nil result from tool"),
		}
	}

	result = normalizeToolResult(result, name, r.mediaStore, channel, chatID)

	duration := time.Since(start)

	// 按结果类型记录日志
	if result.IsError {
		logger.ErrorCF("tool", "Tool execution failed",
			map[string]any{
				"tool":     name,
				"duration": duration.Milliseconds(),
				"error":    result.ForLLM,
			})
	} else if result.Async {
		logger.InfoCF("tool", "Tool started (async)",
			map[string]any{
				"tool":     name,
				"duration": duration.Milliseconds(),
			})
	} else {
		logger.InfoCF("tool", "Tool execution completed",
			map[string]any{
				"tool":          name,
				"duration_ms":   duration.Milliseconds(),
				"result_length": len(result.ContentForLLM()),
			})
	}

	return result
}

// sortedToolNames 按排序顺序返回工具名，以便进行确定性遍历。
// 这对 KV 缓存稳定性至关重要：不确定的 map 遍历会在每次调用时产生
// 不同的系统提示与工具定义，即使没有任何工具变化也会让 LLM 的前缀缓存失效。
func (r *ToolRegistry) sortedToolNames() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *ToolRegistry) GetDefinitions() []map[string]any {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	definitions := make([]map[string]any, 0, len(sorted))
	for _, name := range sorted {
		entry := r.tools[name]

		if !entry.IsCore && entry.TTL <= 0 {
			continue
		}

		definitions = append(definitions, ToolToSchema(r.tools[name].Tool))
	}
	return definitions
}

// ToProviderDefs 把工具定义转换为提供方兼容的格式。
// 这是 LLM 服务商 API 所期望的格式。
func (r *ToolRegistry) ToProviderDefs() []providers.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	definitions := make([]providers.ToolDefinition, 0, len(sorted))
	for _, name := range sorted {
		entry := r.tools[name]

		if !entry.IsCore && entry.TTL <= 0 {
			continue
		}

		schema := ToolToSchema(entry.Tool)

		// 带类型检查地安全提取嵌套值
		fn, ok := schema["function"].(map[string]any)
		if !ok {
			continue
		}

		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		params, _ := fn["parameters"].(map[string]any)
		metadata := promptMetadataForTool(entry.Tool)

		definitions = append(definitions, providers.ToolDefinition{
			Type: "function",
			Function: providers.ToolFunctionDefinition{
				Name:        name,
				Description: desc,
				Parameters:  params,
			},
			PromptLayer:  metadata.Layer,
			PromptSlot:   metadata.Slot,
			PromptSource: metadata.Source,
		})
	}
	return definitions
}

func promptMetadataForTool(tool Tool) PromptMetadata {
	metadata := PromptMetadata{
		Layer:  ToolPromptLayerCapability,
		Slot:   ToolPromptSlotTooling,
		Source: ToolPromptSourceRegistry,
	}
	if provider, ok := tool.(PromptMetadataProvider); ok {
		provided := provider.PromptMetadata()
		if provided.Layer != "" {
			metadata.Layer = provided.Layer
		}
		if provided.Slot != "" {
			metadata.Slot = provided.Slot
		}
		if provided.Source != "" {
			metadata.Source = provided.Source
		}
	}
	return metadata
}

// List 返回所有已注册工具名的列表。
func (r *ToolRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.sortedToolNames()
}

// Clone 创建注册表的独立副本，包含相同的工具条目（对每个 ToolEntry
// 做浅拷贝）。它用于把父 agent 的工具快照交给子 agent，同时不共享同一个
// 注册表——在克隆之后于父 agent 上注册的工具（例如 spawn、spawn_status）
// 对克隆副本不可见，从而避免子 agent 的递归创建。
// 克隆中的版本计数器会重置为 0，因为它是一个新的独立注册表。
func (r *ToolRegistry) Clone() *ToolRegistry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	clone := &ToolRegistry{
		tools:      make(map[string]*ToolEntry, len(r.tools)),
		mediaStore: r.mediaStore,
	}
	if r.allowlist != nil {
		clone.allowlist = make(map[string]struct{}, len(r.allowlist))
		for name := range r.allowlist {
			clone.allowlist[name] = struct{}{}
		}
	}
	for name, entry := range r.tools {
		clone.tools[name] = &ToolEntry{
			Tool:   entry.Tool,
			IsCore: entry.IsCore,
			TTL:    entry.TTL,
		}
	}
	return clone
}

// Count 返回已注册工具的数量。
func (r *ToolRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tools)
}

// GetSummaries 返回所有已注册工具的可读摘要。
// 返回由 "name - description" 字符串组成的切片。
func (r *ToolRegistry) GetSummaries() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	summaries := make([]string, 0, len(sorted))
	for _, name := range sorted {
		entry := r.tools[name]

		if !entry.IsCore && entry.TTL <= 0 {
			continue
		}

		summaries = append(
			summaries,
			fmt.Sprintf("- `%s` - %s", entry.Tool.Name(), entry.Tool.Description()),
		)
	}
	return summaries
}

// GetAll 返回所有已注册工具（核心工具以及 TTL > 0 的非核心工具）。
// 供 SubTurn 继承父级工具集使用。
func (r *ToolRegistry) GetAll() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sorted := r.sortedToolNames()
	tools := make([]Tool, 0, len(sorted))
	for _, name := range sorted {
		entry := r.tools[name]

		// 包含核心工具以及 TTL 仍有效的非核心工具
		if entry.IsCore || entry.TTL > 0 {
			tools = append(tools, entry.Tool)
		}
	}
	return tools
}
