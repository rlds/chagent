package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/utils"
)

const (
	MaxRegexPatternLength = 200
	RegexSearchToolName   = "tool_search_tool_regex"
	BM25SearchToolName    = "tool_search_tool_bm25"
)

type RegexSearchTool struct {
	registry         *ToolRegistry
	ttl              int
	maxSearchResults int
}

func NewRegexSearchTool(r *ToolRegistry, ttl int, maxSearchResults int) *RegexSearchTool {
	return &RegexSearchTool{registry: r, ttl: ttl, maxSearchResults: maxSearchResults}
}

func (t *RegexSearchTool) Name() string {
	return RegexSearchToolName
}

func (t *RegexSearchTool) Description() string {
	return "通过正则模式按需搜索当前隐藏的工具。返回已发现工具的 JSON schema。"
}

func (t *RegexSearchTool) PromptMetadata() PromptMetadata {
	return PromptMetadata{
		Layer:  ToolPromptLayerCapability,
		Slot:   ToolPromptSlotTooling,
		Source: ToolPromptSourceDiscovery,
	}
}

func (t *RegexSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"pattern": map[string]any{
				"type":        "string",
				"description": "用于匹配工具名或描述的正则表达式",
			},
		},
		"required": []string{"pattern"},
	}
}

func (t *RegexSearchTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	pattern, ok := args["pattern"].(string)
	if !ok || strings.TrimSpace(pattern) == "" {
		// 空字符串正则 (?i) 会匹配所有隐藏工具，
		// 把大量数据一次性塞进上下文并浪费 token。
		return ErrorResult("缺少或无效的 'pattern' 参数，必须是非空字符串。")
	}

	if len(pattern) > MaxRegexPatternLength {
		logger.WarnCF("discovery", "Regex pattern rejected (too long)", map[string]any{"len": len(pattern)})
		return ErrorResult(fmt.Sprintf("pattern 太长：最多允许 %d 个字符", MaxRegexPatternLength))
	}

	logger.DebugCF("discovery", "Regex search", map[string]any{"pattern": pattern})

	res, err := t.registry.SearchRegex(pattern, t.maxSearchResults)
	if err != nil {
		logger.WarnCF("discovery", "Invalid regex pattern", map[string]any{"pattern": pattern, "error": err.Error()})
		return ErrorResult(fmt.Sprintf("正则表达式语法无效：%v。请修正后重试。", err))
	}

	logger.InfoCF("discovery", "Regex search completed", map[string]any{"pattern": pattern, "results": len(res)})
	return formatDiscoveryResponse(t.registry, res, t.ttl)
}

type BM25SearchTool struct {
	registry         *ToolRegistry
	ttl              int
	maxSearchResults int

	// 缓存：仅在注册表版本变化时重建。
	cacheMu      sync.Mutex
	cachedEngine *bm25CachedEngine
	cacheVersion uint64
}

func NewBM25SearchTool(r *ToolRegistry, ttl int, maxSearchResults int) *BM25SearchTool {
	return &BM25SearchTool{registry: r, ttl: ttl, maxSearchResults: maxSearchResults}
}

func (t *BM25SearchTool) Name() string {
	return BM25SearchToolName
}

func (t *BM25SearchTool) Description() string {
	return "通过自然语言描述要执行的动作，按需搜索当前隐藏的工具。返回已发现工具的 JSON schema。"
}

func (t *BM25SearchTool) PromptMetadata() PromptMetadata {
	return PromptMetadata{
		Layer:  ToolPromptLayerCapability,
		Slot:   ToolPromptSlotTooling,
		Source: ToolPromptSourceDiscovery,
	}
}

func (t *BM25SearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "搜索关键词",
			},
		},
		"required": []string{"query"},
	}
}

func (t *BM25SearchTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		// 空字符串查询会匹配所有隐藏工具，
		// 把大量数据一次性塞进上下文并浪费 token。
		return ErrorResult("缺少或无效的 'query' 参数，必须是非空字符串。")
	}

	logger.DebugCF("discovery", "BM25 search", map[string]any{"query": query})

	cached := t.getOrBuildEngine()
	if cached == nil {
		logger.DebugCF("discovery", "BM25 search: no hidden tools available", nil)
		return SilentResult("没有找到匹配查询的工具。")
	}

	ranked := cached.engine.Search(query, t.maxSearchResults)
	if len(ranked) == 0 {
		logger.DebugCF("discovery", "BM25 search: no matches", map[string]any{"query": query})
		return SilentResult("没有找到匹配查询的工具。")
	}

	results := make([]ToolSearchResult, len(ranked))
	for i, r := range ranked {
		results[i] = ToolSearchResult{
			Name:        r.Document.Name,
			Description: r.Document.Description,
		}
	}

	logger.InfoCF("discovery", "BM25 search completed", map[string]any{"query": query, "results": len(results)})
	return formatDiscoveryResponse(t.registry, results, t.ttl)
}

// ToolSearchResult 表示返回给 LLM 的搜索结果。
// JSON 响应中省略了 Parameters 以节省上下文 token；
// 提升为正式工具后，LLM 会通过 ToProviderDefs 看到完整的 schema。
type ToolSearchResult struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (r *ToolRegistry) SearchRegex(pattern string, maxSearchResults int) ([]ToolSearchResult, error) {
	if maxSearchResults <= 0 {
		return nil, nil
	}

	regex, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil, fmt.Errorf("编译正则表达式失败 %q：%w", pattern, err)
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	var results []ToolSearchResult

	// 按排序顺序遍历，保证多次调用结果确定。
	for _, name := range r.sortedToolNames() {
		entry := r.tools[name]
		// 只在隐藏工具中搜索（Core 工具已经可见）
		if !entry.IsCore {
			// 直接调用接口方法！无需反射或反序列化。
			desc := entry.Tool.Description()

			if regex.MatchString(name) || regex.MatchString(desc) {
				results = append(results, ToolSearchResult{
					Name:        name,
					Description: desc,
				})
				if len(results) >= maxSearchResults {
					break // 达到上限即停止搜索！节省 CPU。
				}
			}
		}
	}

	return results, nil
}

func formatDiscoveryResponse(registry *ToolRegistry, results []ToolSearchResult, ttl int) *ToolResult {
	if len(results) == 0 {
		return SilentResult("没有找到匹配查询的工具。")
	}

	names := make([]string, len(results))
	for i, r := range results {
		names[i] = r.Name
	}
	registry.PromoteTools(names, ttl)
	logger.InfoCF("discovery", "Promoted tools", map[string]any{"tools": names, "ttl": ttl})

	b, err := json.Marshal(results)
	if err != nil {
		return ErrorResult("格式化搜索结果失败：" + err.Error())
	}

	msg := fmt.Sprintf(
		"找到 %d 个工具：\n%s\n\n成功：这些工具已临时解锁为原生工具！下次回复中可直接像普通工具一样调用。",
		len(results),
		string(b),
	)

	return SilentResult(msg)
}

// 轻量内部类型，用作 BM25 的语料文档。
type searchDoc struct {
	Name        string
	Description string
}

// bm25CachedEngine 包装一个 BM25Engine 及其语料快照。
type bm25CachedEngine struct {
	engine *utils.BM25Engine[searchDoc]
}

// snapshotToSearchDocs 将 HiddenToolSnapshot 转换为 BM25 的 searchDoc 切片。
func snapshotToSearchDocs(snap HiddenToolSnapshot) []searchDoc {
	docs := make([]searchDoc, len(snap.Docs))
	for i, d := range snap.Docs {
		docs[i] = searchDoc{Name: d.Name, Description: d.Description}
	}
	return docs
}

// buildBM25Engine 根据 searchDocs 切片创建 BM25Engine。
func buildBM25Engine(docs []searchDoc) *utils.BM25Engine[searchDoc] {
	return utils.NewBM25Engine(
		docs,
		func(doc searchDoc) string {
			return doc.Name + " " + doc.Description
		},
	)
}

// getOrBuildEngine 返回缓存的 BM25 引擎，仅当注册表版本发生变化
// （注册了新工具）时才重建。
func (t *BM25SearchTool) getOrBuildEngine() *bm25CachedEngine {
	// 快速路径：不加锁的乐观检查。
	if t.cachedEngine != nil && t.cacheVersion == t.registry.Version() {
		return t.cachedEngine
	}

	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()

	// 快照与版本在同一次注册表 RLock 内读取，
	// 保证一致性（不会出现 TOCTOU）。
	snap := t.registry.SnapshotHiddenTools()

	// 复查：等待 cacheMu 期间可能已有其他 goroutine 完成重建。
	if t.cachedEngine != nil && t.cacheVersion == snap.Version {
		return t.cachedEngine
	}

	docs := snapshotToSearchDocs(snap)
	if len(docs) == 0 {
		t.cachedEngine = nil
		t.cacheVersion = snap.Version
		return nil
	}

	cached := &bm25CachedEngine{engine: buildBM25Engine(docs)}
	t.cachedEngine = cached
	t.cacheVersion = snap.Version
	logger.DebugCF("discovery", "BM25 engine rebuilt", map[string]any{"docs": len(docs), "version": snap.Version})
	return cached
}

func isToolDiscoveryToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case BM25SearchToolName, RegexSearchToolName:
		return true
	default:
		return false
	}
}

// SearchBM25 通过 utils.BM25Engine 使用 BM25 为隐藏工具按 query 排序。
// 这个非缓存版本每次调用都会重建引擎，供测试以及
// 任何未持有 BM25SearchTool 实例的代码使用。
func (r *ToolRegistry) SearchBM25(query string, maxSearchResults int) []ToolSearchResult {
	snap := r.SnapshotHiddenTools()
	docs := snapshotToSearchDocs(snap)
	if len(docs) == 0 {
		return nil
	}

	ranked := buildBM25Engine(docs).Search(query, maxSearchResults)
	if len(ranked) == 0 {
		return nil
	}

	out := make([]ToolSearchResult, len(ranked))
	for i, r := range ranked {
		out[i] = ToolSearchResult{
			Name:        r.Document.Name,
			Description: r.Document.Description,
		}
	}
	return out
}
