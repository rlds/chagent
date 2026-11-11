package skills

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxConcurrentSearches = 2
)

// SearchResult 表示技能注册表搜索返回的单条结果。
type SearchResult struct {
	Score        float64 `json:"score"`
	Slug         string  `json:"slug"`
	DisplayName  string  `json:"display_name"`
	Summary      string  `json:"summary"`
	Version      string  `json:"version"`
	RegistryName string  `json:"registry_name"`
}

// SkillMeta 保存注册表中某个技能的元数据。
type SkillMeta struct {
	Slug             string `json:"slug"`
	DisplayName      string `json:"display_name"`
	Summary          string `json:"summary"`
	LatestVersion    string `json:"latest_version"`
	IsMalwareBlocked bool   `json:"is_malware_blocked"`
	IsSuspicious     bool   `json:"is_suspicious"`
	RegistryName     string `json:"registry_name"`
}

// InstallResult 由 DownloadAndInstall 返回，携带元数据
// 供调用方进行审核和用户提示。
type InstallResult struct {
	Version          string
	IsMalwareBlocked bool
	IsSuspicious     bool
	Summary          string
}

// RegistryProvider 根据配置创建注册表实例。
// 不同的 hub 可以实现该接口以接入共享管理器。
type RegistryProvider interface {
	IsEnabled() bool
	BuildRegistry() SkillRegistry
}

// SkillRegistry 是所有技能注册表都必须实现的接口。
// 每个注册表代表不同的技能来源（例如 clawhub.ai）
type SkillRegistry interface {
	// Name 返回该注册表的唯一名称（例如 "clawhub"）。
	Name() string
	// ResolveInstallDirName 返回给定安装目标在 workspace/skills 下使用的目录名。
	// 不同注册表对目标的解释方式可能不同（例如 slug 与 owner/repo/path）。
	ResolveInstallDirName(target string) (string, error)
	// SkillURL 返回技能 slug 的网页 URL（如果注册表提供该能力）。
	// version 是可选的，供 URL 依赖 ref 的注册表使用。
	SkillURL(slug, version string) string
	// Search 在注册表中搜索匹配查询的技能。
	Search(ctx context.Context, query string, limit int) ([]SearchResult, error)
	// GetSkillMeta 根据 slug 获取指定技能的元数据。
	GetSkillMeta(ctx context.Context, slug string) (*SkillMeta, error)
	// DownloadAndInstall 获取元数据、解析版本、下载并将技能安装到 targetDir。
	// 返回一个 InstallResult，携带元数据供调用方进行审核和用户提示。
	DownloadAndInstall(ctx context.Context, slug, version, targetDir string) (*InstallResult, error)
}

// InstallTargetNormalizer 由能够将用户提供的安装目标规范化为
// 稳定 slug（用于来源元数据）的注册表实现。
type InstallTargetNormalizer interface {
	NormalizeInstallTarget(target string) string
}

func NormalizeInstallTargetForRegistryInstance(registry SkillRegistry, target string) string {
	if registry == nil || target == "" {
		return target
	}
	normalizer, ok := registry.(InstallTargetNormalizer)
	if !ok {
		return target
	}
	normalized := normalizer.NormalizeInstallTarget(target)
	if normalized == "" {
		return target
	}
	return normalized
}

// RegistryConfig 保存所有技能注册表的配置。
// 它是 NewRegistryManagerFromConfig 的输入。
type RegistryConfig struct {
	Providers             []RegistryProvider
	MaxConcurrentSearches int
}

// ClawHubConfig 配置 ClawHub 注册表。
type ClawHubConfig struct {
	Enabled         bool
	BaseURL         string
	AuthToken       string
	SearchPath      string // 例如 "/api/v1/search"
	SkillsPath      string // 例如 "/api/v1/skills"
	DownloadPath    string // 例如 "/api/v1/download"
	Timeout         int    // 秒；0 = 默认（30 秒）
	MaxZipSize      int    // 字节；0 = 默认（50MB）
	MaxResponseSize int    // 字节；0 = 默认（2MB）
}

// RegistryManager 协调多个技能注册表。
// 它会并发地向各注册表分发搜索请求，并将安装请求路由到正确的注册表。
type RegistryManager struct {
	registries    []SkillRegistry
	maxConcurrent int
	mu            sync.RWMutex
}

func ValidateInstallTarget(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return fmt.Errorf("identifier is required and must be a non-empty string")
	}
	if strings.Contains(target, "\\") {
		return fmt.Errorf("identifier %q contains invalid path separators", target)
	}
	clean := path.Clean("/" + target)
	if clean == "/" || strings.HasPrefix(clean, "/../") || clean == "/.." {
		return fmt.Errorf("identifier %q contains invalid path traversal", target)
	}
	if strings.Contains(target, "//") {
		return fmt.Errorf("identifier %q contains empty path segments", target)
	}
	for _, segment := range strings.Split(strings.Trim(target, "/"), "/") {
		if segment == "." || segment == ".." || segment == "" {
			return fmt.Errorf("identifier %q contains invalid path segments", target)
		}
	}
	return nil
}

// NewRegistryManager 创建一个空的 RegistryManager。
func NewRegistryManager() *RegistryManager {
	return &RegistryManager{
		registries:    make([]SkillRegistry, 0),
		maxConcurrent: defaultMaxConcurrentSearches,
	}
}

// NewRegistryManagerFromConfig 根据配置构建 RegistryManager，
// 只实例化已启用的注册表。
func NewRegistryManagerFromConfig(cfg RegistryConfig) *RegistryManager {
	rm := NewRegistryManager()
	if cfg.MaxConcurrentSearches > 0 {
		rm.maxConcurrent = cfg.MaxConcurrentSearches
	}
	for _, provider := range cfg.Providers {
		if provider == nil || !provider.IsEnabled() {
			continue
		}
		registry := provider.BuildRegistry()
		if registry == nil {
			continue
		}
		rm.AddRegistry(registry)
	}
	return rm
}

// AddRegistry 向管理器添加一个注册表。
func (rm *RegistryManager) AddRegistry(r SkillRegistry) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	rm.registries = append(rm.registries, r)
}

// GetRegistry 按名称返回注册表，未找到时返回 nil。
func (rm *RegistryManager) GetRegistry(name string) SkillRegistry {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	for _, r := range rm.registries {
		if r.Name() == name {
			return r
		}
	}
	return nil
}

// SearchAll 并发地向所有注册表分发查询，
// 并按分数从高到低合并结果。
func (rm *RegistryManager) SearchAll(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	rm.mu.RLock()
	regs := make([]SkillRegistry, len(rm.registries))
	copy(regs, rm.registries)
	rm.mu.RUnlock()

	if len(regs) == 0 {
		return nil, fmt.Errorf("no registries configured")
	}

	type regResult struct {
		results []SearchResult
		err     error
	}

	// 信号量：限制并发数。
	sem := make(chan struct{}, rm.maxConcurrent)
	resultsCh := make(chan regResult, len(regs))

	var wg sync.WaitGroup
	for _, reg := range regs {
		wg.Add(1)
		go func(r SkillRegistry) {
			defer wg.Done()

			// 获取信号量槽位。
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				resultsCh <- regResult{err: ctx.Err()}
				return
			}

			searchCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
			defer cancel()

			results, err := r.Search(searchCtx, query, limit)
			if err != nil {
				slog.Warn("registry search failed", "registry", r.Name(), "error", err)
				resultsCh <- regResult{err: err}
				return
			}
			resultsCh <- regResult{results: results}
		}(reg)
	}

	// 所有 goroutine 完成后关闭结果通道。
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	var merged []SearchResult
	var lastErr error

	var anyRegistrySucceeded bool
	for rr := range resultsCh {
		if rr.err != nil {
			lastErr = rr.err
			continue
		}
		anyRegistrySucceeded = true
		merged = append(merged, rr.results...)
	}

	// 如果所有注册表都失败，则返回最后一个错误。
	if !anyRegistrySucceeded && lastErr != nil {
		return nil, fmt.Errorf("all registries failed: %w", lastErr)
	}

	// 按分数从高到低排序。
	sortByScoreDesc(merged)

	// 截断到 limit。
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}

	return merged, nil
}

// sortByScoreDesc 按 Score 降序对 SearchResults 排序（插入排序——适用于小切片）。
func sortByScoreDesc(results []SearchResult) {
	for i := 1; i < len(results); i++ {
		key := results[i]
		j := i - 1
		for j >= 0 && results[j].Score < key.Score {
			results[j+1] = results[j]
			j--
		}
		results[j+1] = key
	}
}
