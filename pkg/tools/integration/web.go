package integrationtools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	kagiopenapi "github.com/kagisearch/kagi-openapi-golang"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/utils"
)

const (
	userAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	sogouUserAgent  = "Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.0 Mobile/15E148 Safari/604.1"
	userAgentHonest = "chagent/%s (+https://github.com/rlds/chagent; AI assistant bot)"

	// web 工具各提供方的 HTTP 客户端超时。
	searchTimeout     = 10 * time.Second // Brave、Tavily、DuckDuckGo
	perplexityTimeout = 30 * time.Second // Perplexity（基于 LLM，较慢）
	fetchTimeout      = 60 * time.Second // WebFetchTool

	defaultMaxChars = 50000
	maxRedirects    = 5
)

// 用于 HTML 文本提取的预编译正则表达式
var (
	reScript     = regexp.MustCompile(`<script[\s\S]*?</script>`)
	reStyle      = regexp.MustCompile(`<style[\s\S]*?</style>`)
	reTags       = regexp.MustCompile(`<[^>]+>`)
	reWhitespace = regexp.MustCompile(`[^\S\n]+`)
	reBlankLines = regexp.MustCompile(`\n{3,}`)

	// DuckDuckGo 结果提取
	reDDGLink = regexp.MustCompile(
		`<a[^>]*class="[^"]*result__a[^"]*"[^>]*href="([^"]+)"[^>]*>([\s\S]*?)</a>`,
	)
	reDDGSnippet = regexp.MustCompile(
		`<a class="result__snippet[^"]*".*?>([\s\S]*?)</a>`,
	)
	reSogouTitle = regexp.MustCompile(
		`<a\s+class="?resultLink"?\s+href="([^"]+)"[^>]*id="sogou_vr_\d+_\d+"[^>]*>\s*(.*?)\s*</a>`,
	)
	reSogouSnippet = regexp.MustCompile(`<div class="clamp\d*[^"]*">\s*(.*?)\s*</div>`)
	reSogouRealURL = regexp.MustCompile(`url=([^&]+)`)
)

type APIKeyPool struct {
	keys    []string
	current uint32
}

func NewAPIKeyPool(keys []string) *APIKeyPool {
	return &APIKeyPool{
		keys: keys,
	}
}

type APIKeyIterator struct {
	pool     *APIKeyPool
	startIdx uint32
	attempt  uint32
}

func (p *APIKeyPool) NewIterator() *APIKeyIterator {
	if len(p.keys) == 0 {
		return &APIKeyIterator{pool: p}
	}
	idx := atomic.AddUint32(&p.current, 1) - 1
	return &APIKeyIterator{
		pool:     p,
		startIdx: idx,
	}
}

func (it *APIKeyIterator) Next() (string, bool) {
	length := uint32(len(it.pool.keys))
	if length == 0 || it.attempt >= length {
		return "", false
	}
	key := it.pool.keys[(it.startIdx+it.attempt)%length]
	it.attempt++
	return key, true
}

type SearchProvider interface {
	Search(ctx context.Context, query string, count int, rangeCode string) (string, error)
}

type SearchResultItem struct {
	Title     string
	URL       string
	Snippet   string
	Published string
}

func extractSogouURL(href string) string {
	match := reSogouRealURL.FindStringSubmatch(href)
	if len(match) < 2 {
		return ""
	}
	decoded, err := url.QueryUnescape(match[1])
	if err != nil {
		return ""
	}
	return decoded
}

func applySogouRangeHint(query string, rangeCode string) string {
	switch rangeCode {
	case "d":
		return query + " 最近一天"
	case "w":
		return query + " 最近一周"
	case "m":
		return query + " 最近一个月"
	case "y":
		return query + " 最近一年"
	default:
		return query
	}
}

func normalizeSearchRange(raw string) (string, error) {
	rangeCode := strings.ToLower(strings.TrimSpace(raw))
	switch rangeCode {
	case "", "d", "w", "m", "y":
		return rangeCode, nil
	default:
		return "", fmt.Errorf("range 必须是 d、w、m、y 之一")
	}
}

func mapBraveFreshness(rangeCode string) string {
	switch rangeCode {
	case "d":
		return "pd"
	case "w":
		return "pw"
	case "m":
		return "pm"
	case "y":
		return "py"
	default:
		return ""
	}
}

func mapTavilyTimeRange(rangeCode string) string {
	switch rangeCode {
	case "d":
		return "day"
	case "w":
		return "week"
	case "m":
		return "month"
	case "y":
		return "year"
	default:
		return ""
	}
}

func mapPerplexityRecencyFilter(rangeCode string) string {
	switch rangeCode {
	case "d":
		return "day"
	case "w":
		return "week"
	case "m":
		return "month"
	case "y":
		return "year"
	default:
		return ""
	}
}

func mapDuckDuckGoDateFilter(rangeCode string) string {
	switch rangeCode {
	case "d":
		return "d"
	case "w":
		return "w"
	case "m":
		return "m"
	case "y":
		return "t"
	default:
		return ""
	}
}

func mapSearXNGTimeRange(rangeCode string) string {
	switch rangeCode {
	case "d":
		return "day"
	case "w":
		return "week"
	case "m":
		return "month"
	case "y":
		return "year"
	default:
		return ""
	}
}

func mapGLMRecencyFilter(rangeCode string) string {
	switch rangeCode {
	case "d":
		return "oneDay"
	case "w":
		return "oneWeek"
	case "m":
		return "oneMonth"
	case "y":
		return "oneYear"
	default:
		return "noLimit"
	}
}

func mapBaiduRecencyFilter(rangeCode string) string {
	switch rangeCode {
	case "d", "w":
		// 百度不提供按天粒度的过滤。使用最接近的受支持窗口，
		// 以保留时效性偏好，而不是悄悄丢掉该过滤条件。
		return "week"
	case "m":
		return "month"
	case "y":
		return "year"
	default:
		return ""
	}
}

func mapKagiLensTimeFilter(rangeCode string, now time.Time) *kagiopenapi.SearchRequestLens {
	lens := kagiopenapi.NewSearchRequestLens()
	switch rangeCode {
	case "d":
		lens.SetTimeRelative("day")
	case "w":
		lens.SetTimeRelative("week")
	case "m":
		lens.SetTimeRelative("month")
	case "y":
		lens.SetTimeAfter(now.AddDate(-1, 0, 0).Format("2006-01-02"))
	default:
		return nil
	}
	return lens
}

type BraveSearchProvider struct {
	keyPool *APIKeyPool
	proxy   string
	client  *http.Client
}

func (p *BraveSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if p.keyPool == nil || len(p.keyPool.keys) == 0 {
		return "", errors.New("未提供 API key")
	}

	searchURL := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d",
		url.QueryEscape(query), count)
	if freshness := mapBraveFreshness(rangeCode); freshness != "" {
		searchURL += "&freshness=" + url.QueryEscape(freshness)
	}

	var lastErr error
	iter := p.keyPool.NewIterator()

	for {
		apiKey, ok := iter.Next()
		if !ok {
			break
		}

		req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
		if err != nil {
			return "", fmt.Errorf("创建请求失败：%w", err)
		}

		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Subscription-Token", apiKey)

		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("请求失败：%w", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			lastErr = fmt.Errorf("读取响应失败：%w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("API 错误（状态码 %d）：%s", resp.StatusCode, string(body))
			if resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode == http.StatusUnauthorized ||
				resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode >= 500 {
				continue
			}
			return "", lastErr
		}

		var searchResp struct {
			Web struct {
				Results []struct {
					Title       string `json:"title"`
					URL         string `json:"url"`
					Description string `json:"description"`
				} `json:"results"`
			} `json:"web"`
		}

		if err := json.Unmarshal(body, &searchResp); err != nil {
			// 为便于调试记录错误响应体
			return "", fmt.Errorf("解析响应失败：%w", err)
		}

		results := searchResp.Web.Results
		if len(results) == 0 {
			// 当 API 返回 200 但没有任何结果时记录一条警告。
			// 这有助于诊断 API 格式变化，或响应体与预期结构不符
			// 的静默错误。
			bodyPreview := string(body)
			if len(bodyPreview) > 300 {
				bodyPreview = bodyPreview[:300]
			}
			logger.WarnCF("web_search", "Brave API returned empty results",
				map[string]any{
					"query":        query,
					"status":       resp.StatusCode,
					"body_preview": bodyPreview,
				})
			return fmt.Sprintf("未找到结果：%s", query), nil
		}

		var lines []string
		lines = append(lines, fmt.Sprintf("搜索结果：%s", query))
		for i, item := range results {
			if i >= count {
				break
			}
			lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, item.Title, item.URL))
			if item.Description != "" {
				lines = append(lines, fmt.Sprintf("   %s", item.Description))
			}
		}

		return strings.Join(lines, "\n"), nil
	}

	return "", fmt.Errorf("所有 API key 均失败，最后一个错误：%w", lastErr)
}

type TavilySearchProvider struct {
	keyPool *APIKeyPool
	baseURL string
	proxy   string
	client  *http.Client
}

func (p *TavilySearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if p.keyPool == nil || len(p.keyPool.keys) == 0 {
		return "", errors.New("未提供 API key")
	}

	searchURL := p.baseURL
	if searchURL == "" {
		searchURL = "https://api.tavily.com/search"
	}

	var lastErr error
	iter := p.keyPool.NewIterator()

	for {
		apiKey, ok := iter.Next()
		if !ok {
			break
		}

		payload := map[string]any{
			"api_key":             apiKey,
			"query":               query,
			"search_depth":        "advanced",
			"include_answer":      false,
			"include_images":      false,
			"include_raw_content": false,
			"max_results":         count,
		}
		if timeRange := mapTavilyTimeRange(rangeCode); timeRange != "" {
			payload["time_range"] = timeRange
		}

		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("序列化请求体失败：%w", err)
		}

		req, err := http.NewRequestWithContext(ctx, "POST", searchURL, bytes.NewBuffer(bodyBytes))
		if err != nil {
			return "", fmt.Errorf("创建请求失败：%w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", userAgent)

		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("请求失败：%w", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			lastErr = fmt.Errorf("读取响应失败：%w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("Tavily API 错误（状态码 %d）：%s", resp.StatusCode, string(body))
			if resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode == http.StatusUnauthorized ||
				resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode >= 500 {
				continue
			}
			return "", lastErr
		}

		var searchResp struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Content string `json:"content"`
			} `json:"results"`
		}

		if err := json.Unmarshal(body, &searchResp); err != nil {
			return "", fmt.Errorf("解析响应失败：%w", err)
		}

		results := searchResp.Results
		if len(results) == 0 {
			return fmt.Sprintf("未找到结果：%s", query), nil
		}

		var lines []string
		lines = append(lines, fmt.Sprintf("搜索结果：%s（来源：Tavily）", query))
		for i, item := range results {
			if i >= count {
				break
			}
			lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, item.Title, item.URL))
			if item.Content != "" {
				lines = append(lines, fmt.Sprintf("   %s", item.Content))
			}
		}

		return strings.Join(lines, "\n"), nil
	}

	return "", fmt.Errorf("所有 API key 均失败，最后一个错误：%w", lastErr)
}

type KagiSearchProvider struct {
	keyPool *APIKeyPool
	baseURL string
	proxy   string
	client  *http.Client
}

func (p *KagiSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if p.keyPool == nil || len(p.keyPool.keys) == 0 {
		return "", errors.New("未提供 API key")
	}

	client := p.client
	if client == nil {
		client = &http.Client{Timeout: searchTimeout}
	}

	apiClient := newKagiAPIClient(client, p.baseURL)
	searchReq := kagiopenapi.NewSearchRequest(query)
	searchReq.SetLimit(int32(count))
	if lens := mapKagiLensTimeFilter(rangeCode, time.Now().UTC()); lens != nil {
		searchReq.SetLens(*lens)
	}

	var lastErr error
	iter := p.keyPool.NewIterator()

	for {
		apiKey, ok := iter.Next()
		if !ok {
			break
		}

		authCtx := context.WithValue(ctx, kagiopenapi.ContextAccessToken, apiKey)
		searchResp, httpResp, err := apiClient.SearchAPI.Search(authCtx).SearchRequest(*searchReq).Execute()
		if httpResp != nil && httpResp.Body != nil {
			defer httpResp.Body.Close()
		}
		if err != nil {
			if httpResp != nil {
				if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
					results, parseErr := fallbackKagiSearchResults(httpResp, count)
					if parseErr != nil {
						return "", parseErr
					}
					return formatKagiSearchResults(query, results), nil
				}
				lastErr = kagiStatusError(httpResp.StatusCode)
				if httpResp.StatusCode == http.StatusTooManyRequests ||
					httpResp.StatusCode == http.StatusUnauthorized ||
					httpResp.StatusCode == http.StatusForbidden ||
					httpResp.StatusCode >= 500 {
					continue
				}
				return "", lastErr
			}
			lastErr = fmt.Errorf("请求失败：%w", err)
			continue
		}

		results := kagiSearchResults(searchResp, count)
		if len(results) == 0 {
			return fmt.Sprintf("未找到结果：%s", query), nil
		}

		return formatKagiSearchResults(query, results), nil
	}

	return "", fmt.Errorf("所有 API key 均失败，最后一个错误：%w", lastErr)
}

func newKagiAPIClient(client *http.Client, baseURL string) *kagiopenapi.APIClient {
	cfg := kagiopenapi.NewConfiguration()
	cfg.UserAgent = fmt.Sprintf(userAgentHonest, config.Version)
	cfg.HTTPClient = client
	cfg.Servers = kagiopenapi.ServerConfigurations{{
		URL:         kagiServerURL(baseURL),
		Description: "Kagi Search API endpoint",
	}}
	return kagiopenapi.NewAPIClient(cfg)
}

func formatKagiSearchResults(query string, results []SearchResultItem) string {
	if len(results) == 0 {
		return fmt.Sprintf("未找到结果：%s", query)
	}
	lines := []string{fmt.Sprintf("搜索结果：%s（来源：Kagi）", query)}
	for i, item := range results {
		title := item.Title
		if title == "" {
			title = item.URL
		}
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, title, item.URL))
		if item.Published != "" {
			lines = append(lines, fmt.Sprintf("   发布时间：%s", item.Published))
		}
		if item.Snippet != "" {
			lines = append(lines, fmt.Sprintf("   %s", item.Snippet))
		}
	}
	return strings.Join(lines, "\n")
}

func kagiServerURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "https://kagi.com/api/v1"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimSuffix(baseURL, "/")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if strings.HasSuffix(parsed.Path, "/search") {
		parsed.Path = strings.TrimSuffix(parsed.Path, "/search")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return strings.TrimRight(parsed.String(), "/")
}

func kagiStatusError(statusCode int) error {
	switch statusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("Kagi 搜索 API 认证失败（状态码 %d）", statusCode)
	case http.StatusForbidden:
		return fmt.Errorf("Kagi 搜索 API 请求被拒绝（状态码 %d）", statusCode)
	case http.StatusTooManyRequests:
		return fmt.Errorf("Kagi 搜索 API 触发限流（状态码 %d）", statusCode)
	default:
		if statusCode >= 500 {
			return fmt.Errorf("Kagi 搜索 API 服务端错误（状态码 %d）", statusCode)
		}
		return fmt.Errorf("Kagi 搜索 API 错误（状态码 %d）", statusCode)
	}
}

type kagiFallbackResult struct {
	Type      int    `json:"t"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	Time      string `json:"time"`
	Published string `json:"published"`
}

func fallbackKagiSearchResults(resp *http.Response, count int) ([]SearchResultItem, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("解析响应失败：响应体为空")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	return parseFallbackKagiSearchResults(body, count)
}

func parseFallbackKagiSearchResults(body []byte, count int) ([]SearchResultItem, error) {
	if count <= 0 {
		count = 10
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("解析响应失败：%w", err)
	}
	data := bytes.TrimSpace(envelope.Data)
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, nil
	}

	results := make([]SearchResultItem, 0, count)
	switch data[0] {
	case '{':
		var modern struct {
			Search []kagiFallbackResult `json:"search"`
		}
		if err := json.Unmarshal(data, &modern); err != nil {
			return nil, fmt.Errorf("解析响应失败：%w", err)
		}
		appendFallbackKagiResults(&results, modern.Search, count, false)
	case '[':
		var legacy []kagiFallbackResult
		if err := json.Unmarshal(data, &legacy); err != nil {
			return nil, fmt.Errorf("解析响应失败：%w", err)
		}
		appendFallbackKagiResults(&results, legacy, count, true)
	default:
		return nil, fmt.Errorf("解析响应失败：数据结构不符合预期")
	}
	return results, nil
}

func appendFallbackKagiResults(
	results *[]SearchResultItem,
	items []kagiFallbackResult,
	count int,
	requireLegacyType bool,
) {
	for _, item := range items {
		if len(*results) >= count {
			return
		}
		if requireLegacyType && item.Type != 0 {
			continue
		}
		urlStr := strings.TrimSpace(item.URL)
		if urlStr == "" {
			continue
		}
		published := strings.TrimSpace(item.Published)
		if published == "" {
			published = strings.TrimSpace(item.Time)
		}
		*results = append(*results, SearchResultItem{
			Title:     cleanSearchText(item.Title),
			URL:       urlStr,
			Snippet:   cleanSearchText(item.Snippet),
			Published: published,
		})
	}
}

func kagiSearchResults(searchResp *kagiopenapi.Search200Response, count int) []SearchResultItem {
	if count <= 0 {
		count = 10
	}
	if searchResp == nil || searchResp.Data == nil {
		return nil
	}

	results := make([]SearchResultItem, 0, count)
	for _, item := range searchResp.Data.Search {
		if len(results) >= count {
			break
		}
		urlStr := strings.TrimSpace(item.GetUrl())
		if urlStr == "" {
			continue
		}
		results = append(results, SearchResultItem{
			Title:     cleanSearchText(item.GetTitle()),
			URL:       urlStr,
			Snippet:   cleanSearchText(item.GetSnippet()),
			Published: strings.TrimSpace(item.GetTime()),
		})
	}
	return results
}

func cleanSearchText(content string) string {
	return strings.TrimSpace(html.UnescapeString(stripTags(content)))
}

type SogouSearchProvider struct {
	proxy  string
	client *http.Client
}

type GeminiSearchProvider struct {
	apiKey string
	model  string
	proxy  string
	client *http.Client
}

func (p *GeminiSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if strings.TrimSpace(p.apiKey) == "" {
		return "", errors.New("未提供 API key")
	}
	model := strings.TrimSpace(p.model)
	if model == "" {
		model = "gemini-2.5-flash"
	}

	payload := map[string]any{
		"contents": []map[string]any{{
			"parts": []map[string]string{{"text": query}},
		}},
		"tools": []map[string]any{{"google_search": map[string]any{}}},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("序列化请求体失败：%w", err)
	}

	endpoint := fmt.Sprintf(
		"https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent",
		url.PathEscape(model),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("创建请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", p.apiKey)
	req.Header.Set("User-Agent", fmt.Sprintf(userAgentHonest, config.Version))

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("读取响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Gemini 搜索 API 错误（状态码 %d）：%s", resp.StatusCode, string(body))
	}

	var searchResp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			GroundingMetadata struct {
				GroundingChunks []struct {
					Web struct {
						URI   string `json:"uri"`
						Title string `json:"title"`
					} `json:"web"`
				} `json:"groundingChunks"`
			} `json:"groundingMetadata"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return "", fmt.Errorf("解析响应失败：%w", err)
	}
	if len(searchResp.Candidates) == 0 {
		return fmt.Sprintf("未找到结果：%s", query), nil
	}

	candidate := searchResp.Candidates[0]
	lines := []string{fmt.Sprintf("搜索结果：%s（来源：Gemini Google Search）", query)}
	for _, part := range candidate.Content.Parts {
		if strings.TrimSpace(part.Text) != "" {
			lines = append(lines, strings.TrimSpace(part.Text))
		}
	}
	citationCount := 0
	for _, chunk := range candidate.GroundingMetadata.GroundingChunks {
		if strings.TrimSpace(chunk.Web.URI) == "" {
			continue
		}
		citationCount++
		title := strings.TrimSpace(chunk.Web.Title)
		if title == "" {
			title = chunk.Web.URI
		}
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", citationCount, title, chunk.Web.URI))
		if citationCount >= count {
			break
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (p *SogouSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	const sogouWAPURL = "https://wap.sogou.com/web/searchList.jsp"

	results := make([]SearchResultItem, 0, count)
	seenURLs := make(map[string]bool)
	maxPages := min(3, (count+1)/2+1)

	for page := 1; page <= maxPages && len(results) < count; page++ {
		params := url.Values{}
		params.Set("keyword", applySogouRangeHint(query, rangeCode))
		params.Set("v", "5")
		params.Set("p", fmt.Sprintf("%d", page))

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, sogouWAPURL+"?"+params.Encode(), nil)
		if err != nil {
			return "", fmt.Errorf("创建请求失败：%w", err)
		}
		req.Header.Set("User-Agent", sogouUserAgent)

		resp, err := p.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("请求失败：%w", err)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("读取响应失败：%w", err)
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("搜狗返回状态码 %d", resp.StatusCode)
		}

		html := string(body)
		if len(html) < 200 {
			break
		}

		matches := reSogouTitle.FindAllStringSubmatch(html, -1)
		for _, match := range matches {
			if len(match) < 3 {
				continue
			}

			title := stripTags(match[2])
			link := extractSogouURL(match[1])
			if title == "" || link == "" || seenURLs[link] {
				continue
			}
			seenURLs[link] = true

			start := strings.Index(html, match[0])
			snippet := ""
			if start >= 0 {
				after := html[start+len(match[0]):]
				if len(after) > 2000 {
					after = after[:2000]
				}
				if snippetMatch := reSogouSnippet.FindStringSubmatch(after); len(snippetMatch) > 1 {
					snippet = stripTags(snippetMatch[1])
				}
			}

			results = append(results, SearchResultItem{
				Title:   title,
				URL:     link,
				Snippet: snippet,
			})
			if len(results) >= count {
				break
			}
		}
	}

	if len(results) == 0 {
		return fmt.Sprintf("未找到结果：%s", query), nil
	}

	lines := []string{fmt.Sprintf("搜索结果：%s（来源：搜狗）", query)}
	for i, item := range results {
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, item.Title, item.URL))
		if item.Snippet != "" {
			lines = append(lines, fmt.Sprintf("   %s", item.Snippet))
		}
	}
	return strings.Join(lines, "\n"), nil
}

type DuckDuckGoSearchProvider struct {
	proxy  string
	client *http.Client
}

func (p *DuckDuckGoSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	searchURL := fmt.Sprintf("https://html.duckduckgo.com/html/?q=%s", url.QueryEscape(query))
	if dateFilter := mapDuckDuckGoDateFilter(rangeCode); dateFilter != "" {
		searchURL += "&df=" + url.QueryEscape(dateFilter)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败：%w", err)
	}

	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("读取响应失败：%w", err)
	}

	return p.extractResults(string(body), count, query)
}

func (p *DuckDuckGoSearchProvider) extractResults(
	html string,
	count int,
	query string,
) (string, error) {
	// DuckDuckGo HTML 的简单正则提取
	// 策略：直接找到所有结果容器或关键锚点

	// 先直接尝试查找结果链接，因为它们最关键
	// 模式：<a class="result__a" href="...">Title</a>
	// 之前的正则有点严格。这里让它在属性顺序/内容上更灵活
	matches := reDDGLink.FindAllStringSubmatch(html, count+5)

	if len(matches) == 0 {
		return fmt.Sprintf("未找到结果或解析失败。查询词：%s", query), nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("搜索结果：%s（来源：DuckDuckGo）", query))

	// 预编译摘要正则，在循环内部使用
	// 我们会按链接位置相对地查找摘要，必要时也可以全局查找
	// 但简单的全局摘要搜索可能会与顺序不匹配。
	// 由于只有原始 HTML 字符串，这里就直接全局提取摘要并假定顺序一致（对正则而言简单但有风险）
	// 或者更好：假定在 HTML 中摘要紧跟链接之后

	// 更优的正则思路：遍历文本并按顺序匹配
	// 但现在先把所有摘要都抓出来
	snippetMatches := reDDGSnippet.FindAllStringSubmatch(html, count+5)

	maxItems := min(len(matches), count)

	for i := range maxItems {
		urlStr := matches[i][1]
		title := stripTags(matches[i][2])
		title = strings.TrimSpace(title)

		// 需要时进行 URL 解码
		if strings.Contains(urlStr, "uddg=") {
			if u, err := url.QueryUnescape(urlStr); err == nil {
				_, after, ok := strings.Cut(u, "uddg=")
				if ok {
					urlStr = after
				}
			}
		}

		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, title, urlStr))

		// 若摘要可用且索引对得上，尝试一并附上
		if i < len(snippetMatches) {
			snippet := stripTags(snippetMatches[i][1])
			snippet = strings.TrimSpace(snippet)
			if snippet != "" {
				lines = append(lines, fmt.Sprintf("   %s", snippet))
			}
		}
	}

	return strings.Join(lines, "\n"), nil
}

func stripTags(content string) string {
	return reTags.ReplaceAllString(content, "")
}

type PerplexitySearchProvider struct {
	keyPool *APIKeyPool
	proxy   string
	client  *http.Client
}

func (p *PerplexitySearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if p.keyPool == nil || len(p.keyPool.keys) == 0 {
		return "", errors.New("未提供 API key")
	}

	searchURL := "https://api.perplexity.ai/chat/completions"

	var lastErr error
	iter := p.keyPool.NewIterator()

	for {
		apiKey, ok := iter.Next()
		if !ok {
			break
		}

		payload := map[string]any{
			"model": "sonar",
			"messages": []map[string]string{
				{
					"role":    "system",
					"content": "你是一个搜索助手。请按以下格式提供简洁的搜索结果：\n1. 标题\n   URL\n   简要描述\n\n不要添加额外评论。",
				},
				{
					"role": "user",
					"content": fmt.Sprintf(
						"搜索：%s。最多提供 %d 条相关结果。",
						query,
						count,
					),
				},
			},
			"max_tokens": 1000,
		}
		if recencyFilter := mapPerplexityRecencyFilter(rangeCode); recencyFilter != "" {
			payload["search_recency_filter"] = recencyFilter
		}

		payloadBytes, err := json.Marshal(payload)
		if err != nil {
			return "", fmt.Errorf("序列化请求失败：%w", err)
		}

		req, err := http.NewRequestWithContext(
			ctx,
			"POST",
			searchURL,
			strings.NewReader(string(payloadBytes)),
		)
		if err != nil {
			return "", fmt.Errorf("创建请求失败：%w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("User-Agent", userAgent)

		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("请求失败：%w", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			lastErr = fmt.Errorf("读取响应失败：%w", err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("Perplexity API 错误：%s", string(body))
			if resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode == http.StatusUnauthorized ||
				resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode >= 500 {
				continue
			}
			return "", lastErr
		}

		var searchResp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}

		if err := json.Unmarshal(body, &searchResp); err != nil {
			return "", fmt.Errorf("解析响应失败：%w", err)
		}

		if len(searchResp.Choices) == 0 {
			return fmt.Sprintf("未找到结果：%s", query), nil
		}

		return fmt.Sprintf(
			"Results for: %s (via Perplexity)\n%s",
			query,
			searchResp.Choices[0].Message.Content,
		), nil
	}

	return "", fmt.Errorf("所有 API key 均失败，最后一个错误：%w", lastErr)
}

type SearXNGSearchProvider struct {
	baseURL string
	proxy   string
	client  *http.Client
}

func (p *SearXNGSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if p.baseURL == "" {
		return "", errors.New("未提供 SearXNG URL")
	}

	searchURL := fmt.Sprintf("%s/search?q=%s&format=json&categories=general",
		strings.TrimSuffix(p.baseURL, "/"),
		url.QueryEscape(query))
	if timeRange := mapSearXNGTimeRange(rangeCode); timeRange != "" {
		searchURL += "&time_range=" + url.QueryEscape(timeRange)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", searchURL, nil)
	if err != nil {
		return "", fmt.Errorf("创建请求失败：%w", err)
	}

	client := p.client
	if client == nil {
		client = &http.Client{Timeout: searchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("SearXNG 返回状态码 %d", resp.StatusCode)
	}

	var result struct {
		Results []struct {
			Title   string  `json:"title"`
			URL     string  `json:"url"`
			Content string  `json:"content"`
			Engine  string  `json:"engine"`
			Score   float64 `json:"score"`
		} `json:"results"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析响应失败：%w", err)
	}

	if len(result.Results) == 0 {
		return fmt.Sprintf("未找到结果：%s", query), nil
	}

	// 将结果限制为请求的条数
	if len(result.Results) > count {
		result.Results = result.Results[:count]
	}

	// 以 Chagent 的标准格式输出结果
	var b strings.Builder
	b.WriteString(fmt.Sprintf("搜索结果：%s（来源：SearXNG）\n", query))
	for i, r := range result.Results {
		b.WriteString(fmt.Sprintf("%d. %s\n", i+1, r.Title))
		b.WriteString(fmt.Sprintf("   %s\n", r.URL))
		if r.Content != "" {
			b.WriteString(fmt.Sprintf("   %s\n", r.Content))
		}
	}

	return b.String(), nil
}

type GLMSearchProvider struct {
	apiKey       string
	baseURL      string
	searchEngine string
	proxy        string
	client       *http.Client
}

func (p *GLMSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if p.apiKey == "" {
		return "", errors.New("未提供 API key")
	}

	searchURL := p.baseURL
	if searchURL == "" {
		searchURL = "https://open.bigmodel.cn/api/paas/v4/web_search"
	}

	payload := map[string]any{
		"search_query":  query,
		"search_engine": p.searchEngine,
		"search_intent": false,
		"count":         count,
		"content_size":  "medium",
	}
	if recencyFilter := mapGLMRecencyFilter(rangeCode); recencyFilter != "" {
		payload["search_recency_filter"] = recencyFilter
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("序列化请求体失败：%w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", searchURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("创建请求失败：%w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取响应失败：%w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GLM 搜索 API 错误（状态码 %d）：%s", resp.StatusCode, string(body))
	}

	var searchResp struct {
		SearchResult []struct {
			Title   string `json:"title"`
			Content string `json:"content"`
			Link    string `json:"link"`
		} `json:"search_result"`
	}

	if err := json.Unmarshal(body, &searchResp); err != nil {
		return "", fmt.Errorf("解析响应失败：%w", err)
	}

	results := searchResp.SearchResult
	if len(results) == 0 {
		return fmt.Sprintf("未找到结果：%s", query), nil
	}

	var lines []string
	lines = append(lines, fmt.Sprintf("搜索结果：%s（来源：GLM Search）", query))
	for i, item := range results {
		if i >= count {
			break
		}
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, item.Title, item.Link))
		if item.Content != "" {
			lines = append(lines, fmt.Sprintf("   %s", item.Content))
		}
	}

	return strings.Join(lines, "\n"), nil
}

type BaiduSearchProvider struct {
	apiKey  string
	baseURL string
	proxy   string
	client  *http.Client
}

func (p *BaiduSearchProvider) Search(
	ctx context.Context,
	query string,
	count int,
	rangeCode string,
) (string, error) {
	if p.apiKey == "" {
		return "", errors.New("未提供 API key")
	}

	searchURL := p.baseURL
	if searchURL == "" {
		searchURL = "https://qianfan.baidubce.com/v2/ai_search/web_search"
	}

	payload := map[string]any{
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": query,
			},
		},
		"search_source":        "baidu_search_v2",
		"resource_type_filter": []map[string]any{{"type": "web", "top_k": count}},
	}
	if recencyFilter := mapBaiduRecencyFilter(rangeCode); recencyFilter != "" {
		payload["search_recency_filter"] = recencyFilter
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("序列化请求体失败：%w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", searchURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("创建请求失败：%w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("百度搜索请求失败：%w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("读取响应失败：%w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("百度搜索 API 错误 %d：%s", resp.StatusCode, string(body))
	}

	var result struct {
		References []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"references"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("解析响应失败：%w", err)
	}

	if len(result.References) == 0 {
		return fmt.Sprintf("未找到结果：%s", query), nil
	}

	lines := []string{fmt.Sprintf("搜索结果：%s（来源：百度搜索）", query)}
	for i, item := range result.References {
		if i >= count {
			break
		}
		lines = append(lines, fmt.Sprintf("%d. %s\n   %s", i+1, item.Title, item.URL))
		if item.Content != "" {
			lines = append(lines, fmt.Sprintf("   %s", item.Content))
		}
	}

	return strings.Join(lines, "\n"), nil
}

type WebSearchTool struct {
	provider         SearchProvider
	maxResults       int
	providerResolver func(query string) (SearchProvider, int)
}

type WebSearchToolOptions struct {
	Provider              string
	BraveAPIKeys          []string
	BraveMaxResults       int
	BraveEnabled          bool
	TavilyAPIKeys         []string
	TavilyBaseURL         string
	TavilyMaxResults      int
	TavilyEnabled         bool
	KagiAPIKeys           []string
	KagiBaseURL           string
	KagiMaxResults        int
	KagiEnabled           bool
	SogouMaxResults       int
	SogouEnabled          bool
	DuckDuckGoMaxResults  int
	DuckDuckGoEnabled     bool
	GeminiAPIKey          string
	GeminiModel           string
	GeminiMaxResults      int
	GeminiEnabled         bool
	PerplexityAPIKeys     []string
	PerplexityMaxResults  int
	PerplexityEnabled     bool
	SearXNGBaseURL        string
	SearXNGMaxResults     int
	SearXNGEnabled        bool
	GLMSearchAPIKey       string
	GLMSearchBaseURL      string
	GLMSearchEngine       string
	GLMSearchMaxResults   int
	GLMSearchEnabled      bool
	BaiduSearchAPIKey     string
	BaiduSearchBaseURL    string
	BaiduSearchMaxResults int
	BaiduSearchEnabled    bool
	Proxy                 string
}

func WebSearchToolOptionsFromConfig(cfg *config.Config) WebSearchToolOptions {
	return WebSearchToolOptions{
		Provider:              cfg.Tools.Web.Provider,
		BraveAPIKeys:          cfg.Tools.Web.Brave.APIKeys.Values(),
		BraveMaxResults:       cfg.Tools.Web.Brave.MaxResults,
		BraveEnabled:          cfg.Tools.Web.Brave.Enabled,
		TavilyAPIKeys:         cfg.Tools.Web.Tavily.APIKeys.Values(),
		TavilyBaseURL:         cfg.Tools.Web.Tavily.BaseURL,
		TavilyMaxResults:      cfg.Tools.Web.Tavily.MaxResults,
		TavilyEnabled:         cfg.Tools.Web.Tavily.Enabled,
		KagiAPIKeys:           cfg.Tools.Web.Kagi.APIKeys.Values(),
		KagiBaseURL:           cfg.Tools.Web.Kagi.BaseURL,
		KagiMaxResults:        cfg.Tools.Web.Kagi.MaxResults,
		KagiEnabled:           cfg.Tools.Web.Kagi.Enabled,
		SogouMaxResults:       cfg.Tools.Web.Sogou.MaxResults,
		SogouEnabled:          cfg.Tools.Web.Sogou.Enabled,
		DuckDuckGoMaxResults:  cfg.Tools.Web.DuckDuckGo.MaxResults,
		DuckDuckGoEnabled:     cfg.Tools.Web.DuckDuckGo.Enabled,
		GeminiAPIKey:          cfg.Tools.Web.Gemini.APIKey.String(),
		GeminiModel:           cfg.Tools.Web.Gemini.Model,
		GeminiMaxResults:      cfg.Tools.Web.Gemini.MaxResults,
		GeminiEnabled:         cfg.Tools.Web.Gemini.Enabled,
		PerplexityAPIKeys:     cfg.Tools.Web.Perplexity.APIKeys.Values(),
		PerplexityMaxResults:  cfg.Tools.Web.Perplexity.MaxResults,
		PerplexityEnabled:     cfg.Tools.Web.Perplexity.Enabled,
		SearXNGBaseURL:        cfg.Tools.Web.SearXNG.BaseURL,
		SearXNGMaxResults:     cfg.Tools.Web.SearXNG.MaxResults,
		SearXNGEnabled:        cfg.Tools.Web.SearXNG.Enabled,
		GLMSearchAPIKey:       cfg.Tools.Web.GLMSearch.APIKey.String(),
		GLMSearchBaseURL:      cfg.Tools.Web.GLMSearch.BaseURL,
		GLMSearchEngine:       cfg.Tools.Web.GLMSearch.SearchEngine,
		GLMSearchMaxResults:   cfg.Tools.Web.GLMSearch.MaxResults,
		GLMSearchEnabled:      cfg.Tools.Web.GLMSearch.Enabled,
		BaiduSearchAPIKey:     cfg.Tools.Web.BaiduSearch.APIKey.String(),
		BaiduSearchBaseURL:    cfg.Tools.Web.BaiduSearch.BaseURL,
		BaiduSearchMaxResults: cfg.Tools.Web.BaiduSearch.MaxResults,
		BaiduSearchEnabled:    cfg.Tools.Web.BaiduSearch.Enabled,
		Proxy:                 cfg.Tools.Web.Proxy,
	}
}

func WebSearchProviderReady(opts WebSearchToolOptions, name string) bool {
	return opts.providerReady(name)
}

func ResolveWebSearchProviderName(opts WebSearchToolOptions, query string) (string, error) {
	return opts.resolveProviderName(query)
}

var (
	knownWebSearchProviders = []string{
		"sogou",
		"duckduckgo",
		"gemini",
		"brave",
		"tavily",
		"kagi",
		"perplexity",
		"searxng",
		"glm_search",
		"baidu_search",
	}
	autoPrimaryWebSearchProviders  = []string{"perplexity", "brave", "kagi", "searxng", "tavily", "gemini"}
	autoFallbackWebSearchProviders = []string{"baidu_search", "glm_search"}
)

func isKnownWebSearchProvider(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, known := range knownWebSearchProviders {
		if name == known {
			return true
		}
	}
	return false
}

func (opts WebSearchToolOptions) providerReady(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "sogou":
		return opts.SogouEnabled
	case "duckduckgo":
		return opts.DuckDuckGoEnabled
	case "gemini":
		return opts.GeminiEnabled && strings.TrimSpace(opts.GeminiAPIKey) != ""
	case "brave":
		return opts.BraveEnabled && len(opts.BraveAPIKeys) > 0
	case "tavily":
		return opts.TavilyEnabled && len(opts.TavilyAPIKeys) > 0
	case "kagi":
		return opts.KagiEnabled && len(opts.KagiAPIKeys) > 0
	case "perplexity":
		return opts.PerplexityEnabled && len(opts.PerplexityAPIKeys) > 0
	case "searxng":
		return opts.SearXNGEnabled && strings.TrimSpace(opts.SearXNGBaseURL) != ""
	case "glm_search":
		return opts.GLMSearchEnabled && strings.TrimSpace(opts.GLMSearchAPIKey) != ""
	case "baidu_search":
		return opts.BaiduSearchEnabled && strings.TrimSpace(opts.BaiduSearchAPIKey) != ""
	default:
		return false
	}
}

func (opts WebSearchToolOptions) normalizedProviderName() string {
	providerName := strings.ToLower(strings.TrimSpace(opts.Provider))
	if providerName != "" && providerName != "auto" && !isKnownWebSearchProvider(providerName) {
		// 运行期容忍过期或被手工编辑过的配置值：
		// 将它们视为 "auto"，并回退到下一个就绪的提供方。
		return "auto"
	}
	return providerName
}

func (opts WebSearchToolOptions) resolveProviderName(query string) (string, error) {
	providerName := opts.normalizedProviderName()
	if providerName != "" && providerName != "auto" && opts.providerReady(providerName) {
		return providerName, nil
	}

	sogouReady := opts.providerReady("sogou")
	duckReady := opts.providerReady("duckduckgo")

	for _, name := range autoPrimaryWebSearchProviders {
		if opts.providerReady(name) {
			return name, nil
		}
	}

	if sogouReady && duckReady {
		if prefersDuckDuckGoQuery(query) {
			return "duckduckgo", nil
		}
		return "sogou", nil
	}
	if sogouReady {
		return "sogou", nil
	}
	if duckReady {
		return "duckduckgo", nil
	}

	for _, name := range autoFallbackWebSearchProviders {
		if opts.providerReady(name) {
			return name, nil
		}
	}

	return "", nil
}

func (opts WebSearchToolOptions) providerByName(name string) (SearchProvider, int, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "auto":
		return nil, 0, nil
	case "sogou":
		if !opts.providerReady("sogou") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 Sogou 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.SogouMaxResults > 0 {
			maxResults = min(opts.SogouMaxResults, 10)
		}
		return &SogouSearchProvider{
			proxy:  opts.Proxy,
			client: client,
		}, maxResults, nil
	case "perplexity":
		if !opts.providerReady("perplexity") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, perplexityTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 Perplexity 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.PerplexityMaxResults > 0 {
			maxResults = min(opts.PerplexityMaxResults, 10)
		}
		return &PerplexitySearchProvider{
			keyPool: NewAPIKeyPool(opts.PerplexityAPIKeys),
			proxy:   opts.Proxy,
			client:  client,
		}, maxResults, nil
	case "brave":
		if !opts.providerReady("brave") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 Brave 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.BraveMaxResults > 0 {
			maxResults = min(opts.BraveMaxResults, 10)
		}
		return &BraveSearchProvider{
			keyPool: NewAPIKeyPool(opts.BraveAPIKeys),
			proxy:   opts.Proxy,
			client:  client,
		}, maxResults, nil
	case "gemini":
		if !opts.providerReady("gemini") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 Gemini 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.GeminiMaxResults > 0 {
			maxResults = min(opts.GeminiMaxResults, 10)
		}
		return &GeminiSearchProvider{
			apiKey: opts.GeminiAPIKey,
			model:  opts.GeminiModel,
			proxy:  opts.Proxy,
			client: client,
		}, maxResults, nil
	case "searxng":
		if !opts.providerReady("searxng") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 SearXNG 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.SearXNGMaxResults > 0 {
			maxResults = min(opts.SearXNGMaxResults, 10)
		}
		return &SearXNGSearchProvider{
			baseURL: opts.SearXNGBaseURL,
			proxy:   opts.Proxy,
			client:  client,
		}, maxResults, nil
	case "tavily":
		if !opts.providerReady("tavily") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 Tavily 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.TavilyMaxResults > 0 {
			maxResults = min(opts.TavilyMaxResults, 10)
		}
		return &TavilySearchProvider{
			keyPool: NewAPIKeyPool(opts.TavilyAPIKeys),
			baseURL: opts.TavilyBaseURL,
			proxy:   opts.Proxy,
			client:  client,
		}, maxResults, nil
	case "kagi":
		if !opts.providerReady("kagi") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 Kagi 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.KagiMaxResults > 0 {
			maxResults = min(opts.KagiMaxResults, 10)
		}
		return &KagiSearchProvider{
			keyPool: NewAPIKeyPool(opts.KagiAPIKeys),
			baseURL: opts.KagiBaseURL,
			proxy:   opts.Proxy,
			client:  client,
		}, maxResults, nil
	case "duckduckgo":
		if !opts.providerReady("duckduckgo") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 DuckDuckGo 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.DuckDuckGoMaxResults > 0 {
			maxResults = min(opts.DuckDuckGoMaxResults, 10)
		}
		return &DuckDuckGoSearchProvider{
			proxy:  opts.Proxy,
			client: client,
		}, maxResults, nil
	case "baidu_search":
		if !opts.providerReady("baidu_search") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, perplexityTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 Baidu Search 的 HTTP client 失败：%w", err)
		}
		maxResults := 10
		if opts.BaiduSearchMaxResults > 0 {
			maxResults = min(opts.BaiduSearchMaxResults, 10)
		}
		return &BaiduSearchProvider{
			apiKey:  opts.BaiduSearchAPIKey,
			baseURL: opts.BaiduSearchBaseURL,
			proxy:   opts.Proxy,
			client:  client,
		}, maxResults, nil
	case "glm_search":
		if !opts.providerReady("glm_search") {
			return nil, 0, nil
		}
		client, err := utils.CreateHTTPClient(opts.Proxy, searchTimeout)
		if err != nil {
			return nil, 0, fmt.Errorf("创建 GLM Search 的 HTTP client 失败：%w", err)
		}
		searchEngine := opts.GLMSearchEngine
		if searchEngine == "" {
			searchEngine = "search_std"
		}
		maxResults := 10
		if opts.GLMSearchMaxResults > 0 {
			maxResults = min(opts.GLMSearchMaxResults, 10)
		}
		return &GLMSearchProvider{
			apiKey:       opts.GLMSearchAPIKey,
			baseURL:      opts.GLMSearchBaseURL,
			searchEngine: searchEngine,
			proxy:        opts.Proxy,
			client:       client,
		}, maxResults, nil
	default:
		return nil, 0, fmt.Errorf("未知的搜索 provider %q", name)
	}
}

func containsHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func containsLatinLetter(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) && unicode.In(r, unicode.Latin) {
			return true
		}
	}
	return false
}

func prefersDuckDuckGoQuery(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	if containsHan(trimmed) {
		return false
	}
	if containsLatinLetter(trimmed) {
		return true
	}
	return false
}

func (opts WebSearchToolOptions) buildProviderResolver() (func(query string) (SearchProvider, int), error) {
	providersByName := make(map[string]SearchProvider, len(knownWebSearchProviders))
	maxResultsByName := make(map[string]int, len(knownWebSearchProviders))

	for _, name := range knownWebSearchProviders {
		if !opts.providerReady(name) {
			continue
		}
		provider, maxResults, err := opts.providerByName(name)
		if err != nil {
			return nil, err
		}
		if provider == nil {
			continue
		}
		providersByName[name] = provider
		maxResultsByName[name] = maxResults
	}

	return func(query string) (SearchProvider, int) {
		name, err := opts.resolveProviderName(query)
		if err != nil {
			return nil, 0
		}
		provider, ok := providersByName[name]
		if !ok {
			return nil, 0
		}
		return provider, maxResultsByName[name]
	}, nil
}

func NewWebSearchTool(opts WebSearchToolOptions) (*WebSearchTool, error) {
	resolver, err := opts.buildProviderResolver()
	if err != nil {
		return nil, err
	}
	provider, maxResults := resolver("")
	if provider == nil {
		return nil, nil
	}

	return &WebSearchTool{
		provider:         provider,
		maxResults:       maxResults,
		providerResolver: resolver,
	}, nil
}

func (t *WebSearchTool) Name() string {
	return "web_search"
}

func (t *WebSearchTool) Description() string {
	return "搜索互联网获取当前信息。支持 query、count 与可选时间范围过滤。返回搜索结果的标题、URL 与摘要片段。"
}

func (t *WebSearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "搜索关键词",
			},
			"count": map[string]any{
				"type":        "integer",
				"description": "结果数量（默认 10，最大 10）",
				"minimum":     1.0,
				"maximum":     10.0,
			},
			"range": map[string]any{
				"type":        "string",
				"description": "可选时间过滤：d（天）、w（周）、m（月）、y（年）",
				"enum":        []string{"d", "w", "m", "y"},
			},
		},
		"required": []string{"query"},
	}
}

func (t *WebSearchTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return ErrorResult("query 为必填项")
	}
	query = strings.TrimSpace(query)

	provider := t.provider
	maxResults := t.maxResults
	if t.providerResolver != nil {
		provider, maxResults = t.providerResolver(query)
	}
	if provider == nil {
		return ErrorResult("搜索 provider 未配置")
	}

	count64, err := getInt64Arg(args, "count", int64(maxResults))
	if err != nil {
		return ErrorResult(err.Error())
	}
	count := maxResults
	if count64 > 0 && count64 <= 10 {
		count = min(int(count64), maxResults)
	}

	rangeCode, err := normalizeSearchRange("")
	if err != nil {
		return ErrorResult(err.Error())
	}
	if rawRange, exists := args["range"]; exists {
		rangeStr, ok := rawRange.(string)
		if !ok {
			return ErrorResult("range 必须是字符串")
		}
		rangeCode, err = normalizeSearchRange(rangeStr)
		if err != nil {
			return ErrorResult(err.Error())
		}
	}

	result, err := provider.Search(ctx, query, count, rangeCode)
	if err != nil {
		return ErrorResult(fmt.Sprintf("搜索失败：%v", err))
	}

	return &ToolResult{
		ForLLM:  result,
		ForUser: result,
	}
}

type WebFetchTool struct {
	maxChars        int
	proxy           string
	client          *http.Client
	format          string
	fetchLimitBytes int64
	whitelist       *utils.PrivateHostWhitelist
}

func NewWebFetchTool(maxChars int, format string, fetchLimitBytes int64) (*WebFetchTool, error) {
	// createHTTPClient 在 proxy 字符串为空时不会失败。
	return NewWebFetchToolWithConfig(maxChars, "", format, fetchLimitBytes, nil)
}

// allowPrivateWebFetchHosts 控制是否允许回环 / 私有主机。
// 正常运行时该值为 false，以减少 SSRF 暴露面，测试可以临时覆盖它。
var allowPrivateWebFetchHosts atomic.Bool

func NewWebFetchToolWithProxy(
	maxChars int,
	proxy string,
	format string,
	fetchLimitBytes int64,
	privateHostWhitelist []string,
) (*WebFetchTool, error) {
	return NewWebFetchToolWithConfig(maxChars, proxy, format, fetchLimitBytes, privateHostWhitelist)
}

func NewWebFetchToolWithConfig(
	maxChars int,
	proxy string,
	format string,
	fetchLimitBytes int64,
	privateHostWhitelist []string,
) (*WebFetchTool, error) {
	if maxChars <= 0 {
		maxChars = defaultMaxChars
	}
	whitelist, err := utils.NewPrivateHostWhitelist(privateHostWhitelist)
	if err != nil {
		return nil, fmt.Errorf("failed to parse web fetch private host whitelist: %w", err)
	}
	client, err := utils.CreateSafeHTTPClient(utils.SafeHTTPClientOptions{
		ProxyURL:             proxy,
		Timeout:              fetchTimeout,
		PrivateHostWhitelist: privateHostWhitelist,
		AllowPrivateHosts: func() bool {
			return allowPrivateWebFetchHosts.Load()
		},
		MaxRedirects: maxRedirects,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP client for web fetch: %w", err)
	}
	if fetchLimitBytes <= 0 {
		fetchLimitBytes = 10 * 1024 * 1024 // 安全兜底值
	}
	return &WebFetchTool{
		maxChars:        maxChars,
		proxy:           proxy,
		client:          client,
		format:          format,
		fetchLimitBytes: fetchLimitBytes,
		whitelist:       whitelist,
	}, nil
}

func (t *WebFetchTool) Name() string {
	return "web_fetch"
}

func (t *WebFetchTool) Description() string {
	return "抓取 URL 并提取可读内容（HTML 转文本）。用于获取天气、新闻、文章或任意网页内容。"
}

func (t *WebFetchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"url": map[string]any{
				"type":        "string",
				"description": "要抓取的 URL",
			},
			"maxChars": map[string]any{
				"type":        "integer",
				"description": "最多提取的字符数",
				"minimum":     100.0,
			},
		},
		"required": []string{"url"},
	}
}

func (t *WebFetchTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	urlStr, ok := args["url"].(string)
	if !ok {
		return ErrorResult("url 为必填项")
	}

	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return ErrorResult(fmt.Sprintf("URL 无效：%v", err))
	}

	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return ErrorResult("仅允许 http/https URL")
	}

	if parsedURL.Host == "" {
		return ErrorResult("URL 缺少域名")
	}

	// 轻量预检：不做 DNS 解析就拦截明显的 localhost / 字面量 IP。
	// 真正的 SSRF 防护是连接时的 newSafeDialContext。
	hostname := parsedURL.Hostname()
	if utils.IsObviousPrivateHost(hostname, t.whitelist, func() bool {
		return allowPrivateWebFetchHosts.Load()
	}) {
		return ErrorResult("不允许抓取私有或本地网络地址")
	}

	maxChars := t.maxChars
	if mc, ok := args["maxChars"].(float64); ok {
		if int(mc) > 100 {
			maxChars = int(mc)
		}
	}

	doFetch := func(ua string) (*http.Response, []byte, error) {
		req, reqErr := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
		if reqErr != nil {
			return nil, nil, fmt.Errorf("创建请求失败：%w", reqErr)
		}
		utils.AllowConfiguredProxyFirstHop(req, t.client.Transport)
		req.Header.Set("User-Agent", ua)
		resp, doErr := t.client.Do(req)
		if doErr != nil {
			return nil, nil, fmt.Errorf("请求失败：%w", doErr)
		}
		resp.Body = http.MaxBytesReader(nil, resp.Body, t.fetchLimitBytes)

		b, readErr := io.ReadAll(resp.Body)
		return resp, b, readErr
	}

	resp, body, err := doFetch(userAgent)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}

	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return ErrorResult(
				fmt.Sprintf(
					"读取响应失败：超过 %d 字节大小上限",
					t.fetchLimitBytes,
				),
			)
		}
		return ErrorResult(err.Error())
	}

	// Cloudflare（及类似 WAF）会用 403 + cf-mitigated: challenge 表示机器人挑战。
	// 此时用可标识 chagent 的诚实 User-Agent 重试一次，某些运维方会
	// 明确把这类标识加入 AI 助手的允许列表。
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("Cf-Mitigated") == "challenge" {
		logger.DebugCF("tool", "Cloudflare challenge detected, retrying with honest User-Agent",
			map[string]any{"url": urlStr})
		honestUA := fmt.Sprintf(userAgentHonest, config.Version)
		resp2, body2, err2 := doFetch(honestUA)
		if resp2 != nil && resp2.Body != nil {
			defer resp2.Body.Close()
		}

		if err2 == nil {
			resp, body = resp2, body2
		} else {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err2, &maxBytesErr) {
				return ErrorResult(
					fmt.Sprintf("读取响应失败：超过 %d 字节大小上限", t.fetchLimitBytes),
				)
			}
			return ErrorResult(err2.Error())
		}
	}

	bodyStr := string(body)
	contentType := resp.Header.Get("Content-Type")

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// 此处最常见的错误是响应头为空时的 "mime: no media type"。
		logger.WarnCF("tool", "Failed to parse Content-Type", map[string]any{
			"raw_header": contentType,
			"error":      err.Error(),
		})

		// 安全兜底
		mediaType = "application/octet-stream"
	}

	charset, hasCharset := params["charset"]
	if hasCharset {
		// 如果 charset 不是 utf-8，可能需要在把 bodyStr 交给
		// HTML/Markdown 解析器之前先做转换
		if strings.ToLower(charset) != "utf-8" {
			logger.WarnCF(
				"tool",
				"Note: the content is not in UTF-8",
				map[string]any{"charset": charset},
			)
		}
	}

	var text, extractor string

	switch {
	case mediaType == "application/json":
		var jsonData any
		if err := json.Unmarshal(body, &jsonData); err != nil {
			text = bodyStr
			extractor = "raw"
			break
		}

		formatted, err := json.MarshalIndent(jsonData, "", "  ")
		if err != nil {
			text = bodyStr
			extractor = "raw"
			break
		}

		text = string(formatted)
		extractor = "json"

	case mediaType == "text/html" || looksLikeHTML(bodyStr):
		switch strings.ToLower(t.format) {
		case "markdown":
			var err error
			text, err = utils.HtmlToMarkdown(bodyStr)
			if err != nil {
				return ErrorResult(fmt.Sprintf("HTML 转 Markdown 失败：%v", err))
			}
			extractor = "markdown"

		default:
			text = t.extractText(bodyStr)
			extractor = "text"
		}

	default:
		text = bodyStr
		extractor = "raw"
	}

	truncated := len(text) > maxChars
	if truncated {
		text = text[:maxChars] + "\n[内容因大小上限被截断]"
	}

	result := map[string]any{
		"url":       urlStr,
		"status":    resp.StatusCode,
		"extractor": extractor,
		"truncated": truncated,
		"length":    len(text),
		"text":      text,
	}

	resultJSON, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		return ErrorResult(fmt.Sprintf("序列化结果失败：%v", marshalErr))
	}

	return &ToolResult{
		ForLLM: string(resultJSON),
		ForUser: fmt.Sprintf(
			"Fetched %d bytes from %s (extractor: %s, truncated: %v)",
			len(text),
			urlStr,
			extractor,
			truncated,
		),
	}
}

func looksLikeHTML(body string) bool {
	if body == "" {
		return false
	}

	lower := strings.ToLower(body)

	return strings.HasPrefix(body, "<!doctype") ||
		strings.HasPrefix(lower, "<html")
}

func (t *WebFetchTool) extractText(htmlContent string) string {
	result := reScript.ReplaceAllLiteralString(htmlContent, "")
	result = reStyle.ReplaceAllLiteralString(result, "")
	result = reTags.ReplaceAllLiteralString(result, "")

	result = strings.TrimSpace(result)

	result = reWhitespace.ReplaceAllString(result, " ")
	result = reBlankLines.ReplaceAllString(result, "\n\n")

	lines := strings.Split(result, "\n")
	var cleanLines []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleanLines = append(cleanLines, line)
		}
	}

	return strings.Join(cleanLines, "\n")
}

func newSafeDialContext(
	dialer *net.Dialer,
	whitelist *utils.PrivateHostWhitelist,
) func(context.Context, string, string) (net.Conn, error) {
	return utils.NewSafeDialContext(dialer, whitelist, func() bool {
		return allowPrivateWebFetchHosts.Load()
	})
}

func newPrivateHostWhitelist(entries []string) (*utils.PrivateHostWhitelist, error) {
	return utils.NewPrivateHostWhitelist(entries)
}

func isPrivateOrRestrictedIP(ip net.IP) bool {
	return utils.IsPrivateOrRestrictedIP(ip)
}
