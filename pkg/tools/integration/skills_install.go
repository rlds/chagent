package integrationtools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/fileutil"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/skills"
	"github.com/rlds/chagent/pkg/utils"
)

const defaultSkillRegistryName = "github"

var persistInstalledSkillOriginMeta = writeOriginMeta

// InstallSkillTool 允许 LLM agent 从注册表安装技能。
// 它与 FindSkillsTool 共用同一个 RegistryManager，
// 因此 config 中配置的所有注册表都可用于安装。
type InstallSkillTool struct {
	registryMgr *skills.RegistryManager
	workspace   string
	mu          sync.Mutex
}

// NewInstallSkillTool 创建一个新的 InstallSkillTool。
// registryMgr 是共享的注册表管理器（与 FindSkillsTool 使用同一实例）。
// workspace 是根工作区目录；技能会被安装到 {workspace}/skills/{slug}/。
func NewInstallSkillTool(registryMgr *skills.RegistryManager, workspace string) *InstallSkillTool {
	return &InstallSkillTool{
		registryMgr: registryMgr,
		workspace:   workspace,
		mu:          sync.Mutex{},
	}
}

func (t *InstallSkillTool) Name() string {
	return "install_skill"
}

func (t *InstallSkillTool) Description() string {
	return "通过 slug 从注册表安装技能。省略 registry 时默认使用 GitHub。下载并解压技能到工作区。先使用 find_skills 发现可用技能。"
}

func (t *InstallSkillTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"slug": map[string]any{
				"type":        "string",
				"description": "要安装的技能唯一 slug（例如 'github'、'docker-compose'）",
			},
			"version": map[string]any{
				"type":        "string",
				"description": "要安装的指定版本（可选，默认最新）",
			},
			"registry": map[string]any{
				"type":        "string",
				"description": "安装来源注册表（可选，默认 'github'）",
			},
			"force": map[string]any{
				"type":        "boolean",
				"description": "技能已存在时强制重装（默认 false）",
			},
		},
		"required": []string{"slug"},
	}
}

func (t *InstallSkillTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	// 安装锁，用于防止并发的目录操作。
	// 理想情况下应按 `slug` 粒度加锁，目前是按 `workspace` 粒度。
	t.mu.Lock()
	defer t.mu.Unlock()

	slug, _ := args["slug"].(string)
	if strings.TrimSpace(slug) == "" {
		return ErrorResult("slug 为必填项，且必须是非空字符串")
	}

	// 校验 registry
	registryName, _ := args["registry"].(string)
	if registryName == "" {
		registryName = defaultSkillRegistryName
	}
	if err := utils.ValidateSkillIdentifier(registryName); err != nil {
		return ErrorResult(fmt.Sprintf("registry %q 无效：%s", registryName, err.Error()))
	}

	// 解析要使用哪个 registry。
	registry := t.registryMgr.GetRegistry(registryName)
	if registry == nil {
		return ErrorResult(fmt.Sprintf("未找到 registry %q", registryName))
	}

	// 校验目标并解析安装目录。
	dirName, err := registry.ResolveInstallDirName(slug)
	if err != nil {
		return ErrorResult(fmt.Sprintf("slug %q 无效：%s", slug, err.Error()))
	}

	version, _ := args["version"].(string)
	force, _ := args["force"].(bool)

	// 检查是否已经安装。
	skillsDir := filepath.Join(t.workspace, "skills")
	targetDir := filepath.Join(skillsDir, dirName)
	backupDir := ""
	restorePreviousInstall := func() {
		if backupDir == "" {
			return
		}
		if rmErr := os.RemoveAll(targetDir); rmErr != nil {
			logger.ErrorCF("tool", "Failed to remove failed install before restore",
				map[string]any{
					"tool":       "install_skill",
					"target_dir": targetDir,
					"error":      rmErr.Error(),
				})
			return
		}
		if restoreErr := os.Rename(backupDir, targetDir); restoreErr != nil {
			logger.ErrorCF("tool", "Failed to restore previous install after failed reinstall",
				map[string]any{
					"tool":       "install_skill",
					"backup_dir": backupDir,
					"target_dir": targetDir,
					"error":      restoreErr.Error(),
				})
			return
		}
		backupDir = ""
	}

	if !force {
		if _, statErr := os.Stat(targetDir); statErr == nil {
			return ErrorResult(
				fmt.Sprintf("技能 %q 已安装于 %s；如需重装请使用 force=true。", slug, targetDir),
			)
		}
	} else {
		if _, statErr := os.Stat(targetDir); statErr == nil {
			backupDir = filepath.Join(skillsDir, fmt.Sprintf(".%s.chagent-backup-%d", dirName, time.Now().UnixNano()))
			if renameErr := os.Rename(targetDir, backupDir); renameErr != nil {
				return ErrorResult(fmt.Sprintf("为 %q 准备重装失败：%v", slug, renameErr))
			}
		} else if !os.IsNotExist(statErr) {
			return ErrorResult(fmt.Sprintf("检查 %q 的现有安装失败：%v", slug, statErr))
		}
	}

	// 确保 skills 目录存在。
	if mkdirErr := os.MkdirAll(skillsDir, 0o755); mkdirErr != nil {
		restorePreviousInstall()
		return ErrorResult(fmt.Sprintf("创建 skills 目录失败：%v", mkdirErr))
	}

	// 下载并安装（内部处理元数据、版本解析与解压）。
	result, err := registry.DownloadAndInstall(ctx, slug, version, targetDir)
	if err != nil {
		// 清理未完成的安装。
		rmErr := os.RemoveAll(targetDir)
		if rmErr != nil {
			logger.ErrorCF("tool", "Failed to remove partial install",
				map[string]any{
					"tool":       "install_skill",
					"target_dir": targetDir,
					"error":      rmErr.Error(),
				})
		}
		restorePreviousInstall()
		return ErrorResult(fmt.Sprintf("安装技能 %q 失败：%v", slug, err))
	}

	// 内容审核：拦截恶意软件。
	if result.IsMalwareBlocked {
		rmErr := os.RemoveAll(targetDir)
		if rmErr != nil {
			logger.ErrorCF("tool", "Failed to remove partial install",
				map[string]any{
					"tool":       "install_skill",
					"target_dir": targetDir,
					"error":      rmErr.Error(),
				})
		}
		restorePreviousInstall()
		return ErrorResult(fmt.Sprintf("技能 %q 被标记为恶意，无法安装", slug))
	}

	if !workspaceHasValidInstalledSkill(t.workspace, dirName) {
		rmErr := os.RemoveAll(targetDir)
		if rmErr != nil {
			logger.ErrorCF("tool", "Failed to remove invalid installed skill",
				map[string]any{
					"tool":       "install_skill",
					"target_dir": targetDir,
					"error":      rmErr.Error(),
				})
		}
		restorePreviousInstall()
		return ErrorResult(fmt.Sprintf("安装技能 %q 失败：registry 归档不是有效技能", slug))
	}

	// 写入来源元数据。
	if err := persistInstalledSkillOriginMeta(targetDir, registry, slug, result.Version); err != nil {
		logger.ErrorCF("tool", "Failed to write origin metadata",
			map[string]any{
				"tool":     "install_skill",
				"error":    err.Error(),
				"target":   targetDir,
				"registry": registry.Name(),
				"slug":     slug,
				"version":  result.Version,
			})
		rmErr := os.RemoveAll(targetDir)
		if rmErr != nil {
			logger.ErrorCF("tool", "Failed to roll back install after metadata write failure",
				map[string]any{
					"tool":       "install_skill",
					"target_dir": targetDir,
					"error":      rmErr.Error(),
				})
		}
		restorePreviousInstall()
		return ErrorResult(fmt.Sprintf("持久化技能 %q 的元数据失败：%v", slug, err))
	}
	if backupDir != "" {
		if rmErr := os.RemoveAll(backupDir); rmErr != nil {
			logger.ErrorCF("tool", "Failed to remove previous install backup after successful reinstall",
				map[string]any{
					"tool":       "install_skill",
					"backup_dir": backupDir,
					"error":      rmErr.Error(),
				})
		}
	}

	// 构造结果；若技能可疑则附带审核警告。
	var output string
	if result.IsSuspicious {
		output = fmt.Sprintf("⚠️ 警告：技能 %q 被标记为可疑（可能包含风险模式）。\n\n", slug)
	}
	output += fmt.Sprintf("已成功安装技能 %q v%s（来源：%s registry）。\n安装位置：%s\n",
		slug, result.Version, registry.Name(), targetDir)

	if result.Summary != "" {
		output += fmt.Sprintf("描述：%s\n", result.Summary)
	}
	output += "\n该技能现已可用，可在当前会话中加载。"

	return SilentResult(output)
}

// originMeta 记录技能是从哪个 registry 安装的。
type originMeta struct {
	Version          int    `json:"version"`
	OriginKind       string `json:"origin_kind,omitempty"`
	Registry         string `json:"registry"`
	Slug             string `json:"slug"`
	RegistryURL      string `json:"registry_url,omitempty"`
	InstalledVersion string `json:"installed_version"`
	InstalledAt      int64  `json:"installed_at"`
}

func writeOriginMeta(targetDir string, registry skills.SkillRegistry, slug, version string) error {
	normalizedSlug, registryURL := skills.BuildInstallMetadataForRegistryInstance(registry, slug, version)
	registryName := ""
	if registry != nil {
		registryName = registry.Name()
	}

	meta := originMeta{
		Version:          1,
		OriginKind:       "third_party",
		Registry:         registryName,
		Slug:             normalizedSlug,
		RegistryURL:      registryURL,
		InstalledVersion: version,
		InstalledAt:      time.Now().UnixMilli(),
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}

	// 使用统一的原子写入工具，并显式同步以保证闪存存储的可靠性。
	return fileutil.WriteFileAtomic(filepath.Join(targetDir, ".skill-origin.json"), data, 0o600)
}

func workspaceHasValidInstalledSkill(workspace, directory string) bool {
	loader := skills.NewSkillsLoader(workspace, "", "")
	for _, skill := range loader.ListSkills() {
		if skill.Source != "workspace" {
			continue
		}
		if filepath.Base(filepath.Dir(skill.Path)) == directory {
			return true
		}
	}
	return false
}
