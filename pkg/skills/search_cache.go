package skills

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// SearchCache 为搜索结果提供轻量级缓存。
// 它使用基于 trigram 的相似度将相近的查询匹配到缓存结果，
// 从而避免重复的 API 调用。支持并发访问，线程安全。
type SearchCache struct {
	mu         sync.RWMutex
	entries    map[string]*cacheEntry
	order      []string // LRU 顺序：最旧的在前。
	maxEntries int
	ttl        time.Duration
}

type cacheEntry struct {
	query     string
	trigrams  []uint32
	results   []SearchResult
	createdAt time.Time
}

// similarityThreshold 是缓存命中所需的最低 trigram Jaccard 相似度。
const similarityThreshold = 0.7

// NewSearchCache 创建一个新的搜索缓存。
// maxEntries 是缓存查询的最大数量（超出时按 LRU 淘汰）。
// ttl 是每个条目的存活时间，过期后失效。
func NewSearchCache(maxEntries int, ttl time.Duration) *SearchCache {
	if maxEntries <= 0 {
		maxEntries = 50
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &SearchCache{
		entries:    make(map[string]*cacheEntry),
		order:      make([]string, 0),
		maxEntries: maxEntries,
		ttl:        ttl,
	}
}

// Get 查找某个查询的结果。如果找到则返回缓存结果和 true
// （精确匹配或相似度高于阈值）。未命中时返回 nil, false。
func (sc *SearchCache) Get(query string) ([]SearchResult, bool) {
	normalized := normalizeQuery(query)
	if normalized == "" {
		return nil, false
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	// 先进行精确匹配。
	if entry, ok := sc.entries[normalized]; ok {
		if time.Since(entry.createdAt) < sc.ttl {
			sc.moveToEndLocked(normalized)
			return copyResults(entry.results), true
		}
	}

	// 相似度匹配。
	queryTrigrams := buildTrigrams(normalized)
	var bestEntry *cacheEntry
	var bestSim float64

	for _, entry := range sc.entries {
		if time.Since(entry.createdAt) >= sc.ttl {
			continue // 跳过已过期的条目。
		}
		sim := jaccardSimilarity(queryTrigrams, entry.trigrams)
		if sim > bestSim {
			bestSim = sim
			bestEntry = entry
		}
	}

	if bestSim >= similarityThreshold && bestEntry != nil {
		sc.moveToEndLocked(bestEntry.query)
		return copyResults(bestEntry.results), true
	}

	return nil, false
}

// Put 存储某个查询的结果。达到容量上限时淘汰最旧的条目。
func (sc *SearchCache) Put(query string, results []SearchResult) {
	normalized := normalizeQuery(query)
	if normalized == "" {
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	// 先淘汰已过期的条目。
	sc.evictExpiredLocked()

	// 如果已存在，则更新。
	if _, ok := sc.entries[normalized]; ok {
		sc.entries[normalized] = &cacheEntry{
			query:     normalized,
			trigrams:  buildTrigrams(normalized),
			results:   copyResults(results),
			createdAt: time.Now(),
		}
		// 移动到 LRU 顺序的末尾。
		sc.moveToEndLocked(normalized)
		return
	}

	// 达到容量上限时淘汰 LRU 条目。
	for len(sc.entries) >= sc.maxEntries && len(sc.order) > 0 {
		oldest := sc.order[0]
		sc.order = sc.order[1:]
		delete(sc.entries, oldest)
	}

	// 插入新条目。
	sc.entries[normalized] = &cacheEntry{
		query:     normalized,
		trigrams:  buildTrigrams(normalized),
		results:   copyResults(results),
		createdAt: time.Now(),
	}
	sc.order = append(sc.order, normalized)
}

// Len 返回条目数量（用于测试）。
func (sc *SearchCache) Len() int {
	sc.mu.RLock()
	defer sc.mu.RUnlock()
	return len(sc.entries)
}

// --- 内部实现 ---

func (sc *SearchCache) evictExpiredLocked() {
	now := time.Now()
	newOrder := make([]string, 0, len(sc.order))
	for _, key := range sc.order {
		entry, ok := sc.entries[key]
		if !ok || now.Sub(entry.createdAt) >= sc.ttl {
			delete(sc.entries, key)
			continue
		}
		newOrder = append(newOrder, key)
	}
	sc.order = newOrder
}

func (sc *SearchCache) moveToEndLocked(key string) {
	for i, k := range sc.order {
		if k == key {
			sc.order = append(sc.order[:i], sc.order[i+1:]...)
			break
		}
	}
	sc.order = append(sc.order, key)
}

func normalizeQuery(q string) string {
	return strings.ToLower(strings.TrimSpace(q))
}

// buildTrigrams 生成字符串的 trigram 哈希。
// 示例："hello" → {"hel", "ell", "llo"}
// "hel" -> 0x0068656c -> 4 字节；而字符串本身占 16 字节
func buildTrigrams(s string) []uint32 {
	if len(s) < 3 {
		return nil
	}

	trigrams := make([]uint32, 0, len(s)-2)
	for i := 0; i <= len(s)-3; i++ {
		trigrams = append(trigrams, uint32(s[i])<<16|uint32(s[i+1])<<8|uint32(s[i+2]))
	}

	// 排序并去重
	slices.Sort(trigrams)
	n := 1
	for i := 1; i < len(trigrams); i++ {
		if trigrams[i] != trigrams[i-1] {
			trigrams[n] = trigrams[i]
			n++
		}
	}

	return trigrams[:n]
}

// jaccardSimilarity 计算 |A ∩ B| / |A ∪ B|。
func jaccardSimilarity(a, b []uint32) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	i, j := 0, 0
	intersection := 0

	for i < len(a) && j < len(b) {
		if a[i] == b[j] {
			intersection++
			i++
			j++
		} else if a[i] < b[j] {
			i++
		} else {
			j++
		}
	}

	union := len(a) + len(b) - intersection
	return float64(intersection) / float64(union)
}

func copyResults(results []SearchResult) []SearchResult {
	if results == nil {
		return nil
	}
	cp := make([]SearchResult, len(results))
	copy(cp, results)
	return cp
}
