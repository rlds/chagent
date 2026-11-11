package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/utils"
)

const (
	defaultClawHubTimeout  = 30 * time.Second
	defaultMaxZipSize      = 50 * 1024 * 1024 // 50 MB
	defaultMaxResponseSize = 2 * 1024 * 1024  // 2 MB
)

func init() {
	RegisterRegistryProviderBuilder("clawhub", func(_ string, cfg config.SkillRegistryConfig) RegistryProvider {
		privateCfg := clawHubRegistryPrivateConfig{}
		if err := cfg.DecodeParam(&privateCfg); err != nil {
			slog.Warn("invalid clawhub private config", "error", err)
		}
		return ClawHubConfig{
			Enabled:         cfg.Enabled,
			BaseURL:         cfg.BaseURL,
			AuthToken:       cfg.AuthToken.String(),
			SearchPath:      privateCfg.SearchPath,
			SkillsPath:      privateCfg.SkillsPath,
			DownloadPath:    privateCfg.DownloadPath,
			Timeout:         privateCfg.Timeout,
			MaxZipSize:      privateCfg.MaxZipSize,
			MaxResponseSize: privateCfg.MaxResponseSize,
		}
	})
}

type clawHubRegistryPrivateConfig struct {
	SearchPath      string `json:"search_path"`
	SkillsPath      string `json:"skills_path"`
	DownloadPath    string `json:"download_path"`
	Timeout         int    `json:"timeout"`
	MaxZipSize      int    `json:"max_zip_size"`
	MaxResponseSize int    `json:"max_response_size"`
}

// ClawHubRegistry 为 ClawHub 平台实现 SkillRegistry。
type ClawHubRegistry struct {
	baseURL         string
	authToken       string // 可选——用于提升速率限制
	searchPath      string // 搜索 API
	skillsPath      string // 用于获取技能元数据
	downloadPath    string // 用于获取下载用的 ZIP 文件
	maxZipSize      int
	maxResponseSize int
	client          *http.Client
}

// NewClawHubRegistry 根据配置创建一个新的 ClawHub 注册表客户端。
func NewClawHubRegistry(cfg ClawHubConfig) *ClawHubRegistry {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://clawhub.ai"
	}
	searchPath := cfg.SearchPath
	if searchPath == "" {
		searchPath = "/api/v1/search"
	}
	skillsPath := cfg.SkillsPath
	if skillsPath == "" {
		skillsPath = "/api/v1/skills"
	}
	downloadPath := cfg.DownloadPath
	if downloadPath == "" {
		downloadPath = "/api/v1/download"
	}

	timeout := defaultClawHubTimeout
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout) * time.Second
	}

	maxZip := defaultMaxZipSize
	if cfg.MaxZipSize > 0 {
		maxZip = cfg.MaxZipSize
	}

	maxResp := defaultMaxResponseSize
	if cfg.MaxResponseSize > 0 {
		maxResp = cfg.MaxResponseSize
	}

	return &ClawHubRegistry{
		baseURL:         baseURL,
		authToken:       cfg.AuthToken,
		searchPath:      searchPath,
		skillsPath:      skillsPath,
		downloadPath:    downloadPath,
		maxZipSize:      maxZip,
		maxResponseSize: maxResp,
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        5,
				IdleConnTimeout:     30 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
	}
}

func (c *ClawHubRegistry) Name() string {
	return "clawhub"
}

func (c *ClawHubRegistry) ResolveInstallDirName(target string) (string, error) {
	if err := utils.ValidateSkillIdentifier(target); err != nil {
		return "", err
	}
	return target, nil
}

func (c *ClawHubRegistry) SkillURL(slug, _ string) string {
	if slug == "" {
		return ""
	}
	return c.baseURL + "/skills/" + url.PathEscape(slug)
}

func (c ClawHubConfig) IsEnabled() bool {
	return c.Enabled
}

func (c ClawHubConfig) BuildRegistry() SkillRegistry {
	return NewClawHubRegistry(c)
}

// --- 搜索 ---

type clawhubSearchResponse struct {
	Results []clawhubSearchResult `json:"results"`
}

type clawhubSearchResult struct {
	Score       float64 `json:"score"`
	Slug        *string `json:"slug"`
	DisplayName *string `json:"displayName"`
	Summary     *string `json:"summary"`
	Version     *string `json:"version"`
}

func (c *ClawHubRegistry) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	u, err := url.Parse(c.baseURL + c.searchPath)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	q := u.Query()
	q.Set("q", query)
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}

	var resp clawhubSearchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}

	results := make([]SearchResult, 0, len(resp.Results))
	for _, r := range resp.Results {
		slug := utils.DerefStr(r.Slug, "")
		if slug == "" {
			continue
		}

		summary := utils.DerefStr(r.Summary, "")
		if summary == "" {
			continue
		}

		displayName := utils.DerefStr(r.DisplayName, "")
		if displayName == "" {
			displayName = slug
		}

		results = append(results, SearchResult{
			Score:        r.Score,
			Slug:         slug,
			DisplayName:  displayName,
			Summary:      summary,
			Version:      utils.DerefStr(r.Version, ""),
			RegistryName: c.Name(),
		})
	}

	return results, nil
}

// --- GetSkillMeta ---

type clawhubSkillResponse struct {
	Slug          string                 `json:"slug"`
	DisplayName   string                 `json:"displayName"`
	Summary       string                 `json:"summary"`
	LatestVersion *clawhubVersionInfo    `json:"latestVersion"`
	Moderation    *clawhubModerationInfo `json:"moderation"`
}

type clawhubVersionInfo struct {
	Version string `json:"version"`
}

type clawhubModerationInfo struct {
	IsMalwareBlocked bool `json:"isMalwareBlocked"`
	IsSuspicious     bool `json:"isSuspicious"`
}

func (c *ClawHubRegistry) GetSkillMeta(ctx context.Context, slug string) (*SkillMeta, error) {
	if err := utils.ValidateSkillIdentifier(slug); err != nil {
		return nil, fmt.Errorf("invalid slug %q: error: %s", slug, err.Error())
	}

	u := c.baseURL + c.skillsPath + "/" + url.PathEscape(slug)

	body, err := c.doGet(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("skill metadata request failed: %w", err)
	}

	var resp clawhubSkillResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse skill metadata: %w", err)
	}

	meta := &SkillMeta{
		Slug:         resp.Slug,
		DisplayName:  resp.DisplayName,
		Summary:      resp.Summary,
		RegistryName: c.Name(),
	}

	if resp.LatestVersion != nil {
		meta.LatestVersion = resp.LatestVersion.Version
	}
	if resp.Moderation != nil {
		meta.IsMalwareBlocked = resp.Moderation.IsMalwareBlocked
		meta.IsSuspicious = resp.Moderation.IsSuspicious
	}

	return meta, nil
}

// --- DownloadAndInstall ---

// DownloadAndInstall 获取元数据（带兜底）、解析版本、
// 下载技能 ZIP，并将其解压到 targetDir。
// 返回 InstallResult，供调用方进行审核决策。
func (c *ClawHubRegistry) DownloadAndInstall(
	ctx context.Context,
	slug, version, targetDir string,
) (*InstallResult, error) {
	if err := utils.ValidateSkillIdentifier(slug); err != nil {
		return nil, fmt.Errorf("invalid slug %q: error: %s", slug, err.Error())
	}

	// 步骤 1：获取元数据（带兜底）。
	result := &InstallResult{}
	meta, err := c.GetSkillMeta(ctx, slug)
	if err != nil {
		// 兜底：在没有元数据的情况下继续。
		meta = nil
	}

	if meta != nil {
		result.IsMalwareBlocked = meta.IsMalwareBlocked
		result.IsSuspicious = meta.IsSuspicious
		result.Summary = meta.Summary
	}

	// 步骤 2：解析版本。
	installVersion := version
	if installVersion == "" && meta != nil {
		installVersion = meta.LatestVersion
	}
	if installVersion == "" {
		installVersion = "latest"
	}
	result.Version = installVersion

	// 步骤 3：将 ZIP 下载到临时文件（按约 32KB 分块流式写入）。
	u, err := url.Parse(c.baseURL + c.downloadPath)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}

	q := u.Query()
	q.Set("slug", slug)
	if installVersion != "latest" {
		q.Set("version", installVersion)
	}
	u.RawQuery = q.Encode()

	tmpPath, err := c.downloadToTempFileWithRetry(ctx, u.String())
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer os.Remove(tmpPath)

	// 步骤 4：从磁盘上的文件解压。
	if err := utils.ExtractZipFile(tmpPath, targetDir); err != nil {
		return nil, err
	}

	return result, nil
}

// --- HTTP 辅助函数 ---

func (c *ClawHubRegistry) doGet(ctx context.Context, urlStr string) ([]byte, error) {
	req, err := c.newGetRequest(ctx, urlStr, "application/json")
	if err != nil {
		return nil, err
	}

	resp, err := utils.DoRequestWithRetry(c.client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 限制响应体的读取量，以避免内存问题。
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(c.maxResponseSize)))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return body, nil
}

func (c *ClawHubRegistry) newGetRequest(ctx context.Context, urlStr, accept string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	return req, nil
}

func (c *ClawHubRegistry) downloadToTempFileWithRetry(ctx context.Context, urlStr string) (string, error) {
	req, err := c.newGetRequest(ctx, urlStr, "application/zip")
	if err != nil {
		return "", err
	}

	resp, err := utils.DoRequestWithRetry(c.client, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, errBody)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(errBody[:n]))
	}

	tmpFile, err := os.CreateTemp("", "chagent-dl-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
	}

	src := io.LimitReader(resp.Body, int64(c.maxZipSize)+1)
	written, err := io.Copy(tmpFile, src)
	if err != nil {
		cleanup()
		return "", fmt.Errorf("download write failed: %w", err)
	}

	if written > int64(c.maxZipSize) {
		cleanup()
		return "", fmt.Errorf("download too large: %d bytes (max %d)", written, c.maxZipSize)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("failed to close temp file: %w", err)
	}

	return tmpPath, nil
}
