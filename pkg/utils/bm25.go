// Package utils 提供共享、可复用的算法。
// 本文件实现一个通用的 BM25 搜索引擎。
//
// 用法：
//
//	type MyDoc struct { ID string; Body string }
//
//	corpus := []MyDoc{...}
//	engine := bm25.New(corpus, func(d MyDoc) string {
//	    return d.ID + " " + d.Body
//	})
//	results := engine.Search("my query", 5)
package utils

import (
	"math"
	"sort"
	"strings"
)

// ── 调优默认值 ────────────────────────────────────────────────────────────────

const (
	// DefaultBM25K1 是词频饱和因子（典型取值范围 1.2–2.0）。
	// 取值越大，重复出现的词权重越高。
	DefaultBM25K1 = 1.2

	// DefaultBM25B 是文档长度归一化因子（0 表示不归一化，1 表示完全归一化）。
	DefaultBM25B = 0.75
)

// BM25Engine 是面向通用语料的 BM25 搜索引擎。
// T 是文档类型；调用方需提供 TextFunc，用于从每篇文档中
// 提取可检索的文本。
//
// 引擎在构造时一次性预计算索引，并在后续搜索中复用该索引。
// 如果语料内容发生变化，请重新构造一个新的引擎。
type BM25Engine[T any] struct {
	corpus   []T
	textFunc func(T) string
	k1       float64
	b        float64
	index    *bm25Index
}

// BM25Option 是用于配置 BM25Engine 的函数式选项。
type BM25Option func(*bm25Config)

type bm25Config struct {
	k1 float64
	b  float64
}

type bm25Index struct {
	entries    []bm25DocEntry
	idf        map[string]float32
	docLenNorm []float32
	posting    map[string][]int32
}

type bm25DocEntry struct {
	tf map[string]uint32
}

// WithK1 覆盖词频饱和常数（默认 1.2）。
func WithK1(k1 float64) BM25Option {
	return func(c *bm25Config) { c.k1 = k1 }
}

// WithB 覆盖文档长度归一化因子（默认 0.75）。
func WithB(b float64) BM25Option {
	return func(c *bm25Config) { c.b = b }
}

// NewBM25Engine 为给定语料创建一个 BM25Engine。
//
//   - corpus   : 任意类型 T 的文档切片。
//   - textFunc : 返回某个文档可检索文本的函数。
//   - opts     : 可选的调优项（WithK1、WithB）。
//
// 语料切片以引用方式使用，不会被复制。调用方不得在调用 Search() 期间
// 并发修改该切片。
func NewBM25Engine[T any](corpus []T, textFunc func(T) string, opts ...BM25Option) *BM25Engine[T] {
	cfg := bm25Config{k1: DefaultBM25K1, b: DefaultBM25B}
	for _, o := range opts {
		o(&cfg)
	}
	engine := &BM25Engine[T]{
		corpus:   corpus,
		textFunc: textFunc,
		k1:       cfg.k1,
		b:        cfg.b,
	}
	engine.index = buildBM25Index(corpus, textFunc, cfg.k1, cfg.b)
	return engine
}

// BM25Result 是单次 Search 调用返回的一条排序结果。
type BM25Result[T any] struct {
	Document T
	Score    float32
}

// Search 依据 query 对语料排序，并返回前 k 条结果。
// 没有匹配项时返回空切片（而非 nil）。
//
// 复杂度：在 NewBM25Engine 完成一次性索引之后，每次搜索为
// O(|Q|×avgPostingLen + candidates × log k)。
func (e *BM25Engine[T]) Search(query string, topK int) []BM25Result[T] {
	if topK <= 0 {
		return []BM25Result[T]{}
	}

	queryTerms := bm25Tokenize(query)
	if len(queryTerms) == 0 {
		return []BM25Result[T]{}
	}

	if len(e.corpus) == 0 || e.index == nil {
		return []BM25Result[T]{}
	}

	// 步骤 4：通过倒排列表打分
	// 对查询词去重，避免同一个词被重复计权。
	unique := bm25Dedupe(queryTerms)

	scores := make(map[int32]float32)
	for _, term := range unique {
		termIDF, ok := e.index.idf[term]
		if !ok {
			continue // 该词不在词表中 → 贡献为零
		}
		for _, docID := range e.index.posting[term] {
			freq := float32(e.index.entries[docID].tf[term])
			// TF_norm = freq * (k1+1) / (freq + docLenNorm)
			tfNorm := freq * float32(e.k1+1) / (freq + e.index.docLenNorm[docID])
			scores[docID] += termIDF * tfNorm
		}
	}

	if len(scores) == 0 {
		return []BM25Result[T]{}
	}

	// 步骤 5：用固定大小的最小堆取 top-K
	heap := make([]bm25ScoredDoc, 0, topK)

	for docID, sc := range scores {
		switch {
		case len(heap) < topK:
			heap = append(heap, bm25ScoredDoc{docID: docID, score: sc})
			if len(heap) == topK {
				bm25MinHeapify(heap)
			}
		case sc > heap[0].score:
			heap[0] = bm25ScoredDoc{docID: docID, score: sc}
			bm25SiftDown(heap, 0)
		}
	}

	sort.Slice(heap, func(i, j int) bool { return heap[i].score > heap[j].score })

	out := make([]BM25Result[T], len(heap))
	for i, h := range heap {
		out[i] = BM25Result[T]{
			Document: e.corpus[h.docID],
			Score:    h.score,
		}
	}
	return out
}

func buildBM25Index[T any](corpus []T, textFunc func(T) string, k1, b float64) *bm25Index {
	N := len(corpus)
	if N == 0 {
		return nil
	}

	entries := make([]bm25DocEntry, N)
	rawLens := make([]int, N)
	df := make(map[string]int, 64)
	totalLen := 0

	for i, doc := range corpus {
		tokens := bm25Tokenize(textFunc(doc))
		totalLen += len(tokens)
		rawLens[i] = len(tokens)

		tf := make(map[string]uint32, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		for term := range tf {
			df[term]++
		}

		entries[i] = bm25DocEntry{tf: tf}
	}

	avgDocLen := float64(totalLen) / float64(N)
	if avgDocLen == 0 {
		avgDocLen = 1
	}

	idf := make(map[string]float32, len(df))
	for term, freq := range df {
		idf[term] = float32(math.Log(
			(float64(N)-float64(freq)+0.5)/(float64(freq)+0.5) + 1,
		))
	}

	docLenNorm := make([]float32, N)
	for i, rawLen := range rawLens {
		docLenNorm[i] = float32(k1 * (1 - b + b*float64(rawLen)/avgDocLen))
	}

	posting := make(map[string][]int32, len(df))
	for i, entry := range entries {
		for term := range entry.tf {
			posting[term] = append(posting[term], int32(i))
		}
	}

	return &bm25Index{
		entries:    entries,
		idf:        idf,
		docLenNorm: docLenNorm,
		posting:    posting,
	}
}

// bm25Tokenize 将 s 切分为小写词元，并去除首尾标点。
func bm25Tokenize(s string) []string {
	raw := strings.Fields(strings.ToLower(s))
	out := raw[:0] // 复用底层数组以避免额外分配
	for _, t := range raw {
		t = strings.Trim(t, ".,;:!?\"'()/\\-_")
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// bm25Dedupe 返回去除重复词元后的新切片，
// 并保留首次出现的顺序。
func bm25Dedupe(tokens []string) []string {
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

type bm25ScoredDoc struct {
	docID int32
	score float32
}

// bm25MinHeapify 使用 Floyd 算法原地构建最小堆：O(k)。
func bm25MinHeapify(h []bm25ScoredDoc) {
	for i := len(h)/2 - 1; i >= 0; i-- {
		bm25SiftDown(h, i)
	}
}

// bm25SiftDown 从节点 i 开始恢复最小堆性质：O(log k)。
func bm25SiftDown(h []bm25ScoredDoc, i int) {
	n := len(h)
	for {
		smallest := i
		l, r := 2*i+1, 2*i+2
		if l < n && h[l].score < h[smallest].score {
			smallest = l
		}
		if r < n && h[r].score < h[smallest].score {
			smallest = r
		}
		if smallest == i {
			break
		}
		h[i], h[smallest] = h[smallest], h[i]
		i = smallest
	}
}
