package isolation

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/rlds/chagent/pkg"
	"github.com/rlds/chagent/pkg/config"
)

// MountRule 描述在 Linux 隔离视图内暴露的源到目标挂载。
type MountRule struct {
	Source string
	Target string
	Mode   string
}

// AccessRule 描述某主机路径在 Windows 一侧实际生效的访问规则。
type AccessRule struct {
	Path string
	Mode string
}

// UserEnv 包含注入到隔离子进程中的、按实例重定向的用户目录。
type UserEnv struct {
	Home         string
	Tmp          string
	Config       string
	Cache        string
	State        string
	AppData      string
	LocalAppData string
}

var (
	isolationMu      sync.RWMutex
	currentIsolation = config.DefaultConfig().Isolation
)

// Configure 更新进程级隔离状态，供后续子进程启动时使用。
func Configure(cfg *config.Config) {
	isolationMu.Lock()
	defer isolationMu.Unlock()
	if cfg == nil {
		defaults := config.DefaultConfig()
		currentIsolation = defaults.Isolation
		return
	}
	currentIsolation = cfg.Isolation
}

// CurrentConfig 返回当前生效的隔离设置。
func CurrentConfig() config.IsolationConfig {
	isolationMu.RLock()
	defer isolationMu.RUnlock()
	return currentIsolation
}

// ResolveInstanceRoot 解析用于构建隔离文件系统
// 和重定向用户环境的实例根目录。
func ResolveInstanceRoot() (string, error) {
	root := filepath.Clean(config.GetHome())
	if root == "." {
		return "", fmt.Errorf("instance root resolved to current directory")
	}
	return root, nil
}

// PrepareInstanceRoot 创建隔离运行时所需的目录。
func PrepareInstanceRoot(root string) error {
	for _, dir := range InstanceDirs(root) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("prepare instance dir %s: %w", dir, err)
		}
	}
	return nil
}

// InstanceDirs 返回支持隔离的子进程在实例根目录下
// 必须具备的目录。
func InstanceDirs(root string) []string {
	dirs := []string{
		root,
		filepath.Join(root, "skills"),
		filepath.Join(root, "logs"),
		filepath.Join(root, "cache"),
		filepath.Join(root, "state"),
		filepath.Join(root, "runtime-user-env"),
		filepath.Join(root, "runtime-user-env", "home"),
		filepath.Join(root, "runtime-user-env", "tmp"),
		filepath.Join(root, "runtime-user-env", "config"),
		filepath.Join(root, "runtime-user-env", "cache"),
		filepath.Join(root, "runtime-user-env", "state"),
	}
	dirs = append(dirs, filepath.Join(root, pkg.WorkspaceName))
	if runtime.GOOS == "windows" {
		dirs = append(dirs,
			filepath.Join(root, "runtime-user-env", "AppData", "Roaming"),
			filepath.Join(root, "runtime-user-env", "AppData", "Local"),
		)
	}
	return dirs
}

// ResolveUserEnv 推导以实例运行时区域为根的重定向用户目录。
func ResolveUserEnv(root string) UserEnv {
	base := filepath.Join(root, "runtime-user-env")
	return UserEnv{
		Home:         filepath.Join(base, "home"),
		Tmp:          filepath.Join(base, "tmp"),
		Config:       filepath.Join(base, "config"),
		Cache:        filepath.Join(base, "cache"),
		State:        filepath.Join(base, "state"),
		AppData:      filepath.Join(base, "AppData", "Roaming"),
		LocalAppData: filepath.Join(base, "AppData", "Local"),
	}
}

// ApplyUserEnv 重写子进程环境，使 home、临时目录以及平台相关的
// 用户数据目录都指向实例根目录内部。
func ApplyUserEnv(cmd *exec.Cmd, root string) {
	userEnv := ResolveUserEnv(root)
	envMap := make(map[string]string)
	for _, item := range cmd.Environ() {
		if idx := strings.IndexRune(item, '='); idx > 0 {
			envMap[item[:idx]] = item[idx+1:]
		}
	}

	if runtime.GOOS == "windows" {
		envMap["USERPROFILE"] = userEnv.Home
		envMap["HOME"] = userEnv.Home
		envMap["TEMP"] = userEnv.Tmp
		envMap["TMP"] = userEnv.Tmp
		envMap["APPDATA"] = userEnv.AppData
		envMap["LOCALAPPDATA"] = userEnv.LocalAppData
	} else {
		envMap["HOME"] = userEnv.Home
		envMap["TMPDIR"] = userEnv.Tmp
		envMap["XDG_CONFIG_HOME"] = userEnv.Config
		envMap["XDG_CACHE_HOME"] = userEnv.Cache
		envMap["XDG_STATE_HOME"] = userEnv.State
	}

	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = env
}

// ValidateExposePaths 在启动子进程之前校验用户提供的路径暴露规则。
func ValidateExposePaths(items []config.ExposePath) error {
	seen := map[string]struct{}{}
	for _, item := range items {
		if item.Source == "" {
			return fmt.Errorf("source is required")
		}
		if item.Mode != "ro" && item.Mode != "rw" {
			return fmt.Errorf("invalid expose_paths mode: %s", item.Mode)
		}

		source := filepath.Clean(item.Source)
		target := item.Target
		if target == "" {
			target = source
		}
		target = filepath.Clean(target)

		if !filepath.IsAbs(source) || !filepath.IsAbs(target) {
			return fmt.Errorf("source and target must be absolute paths")
		}
		if _, ok := seen[target]; ok {
			return fmt.Errorf("duplicate expose_path target: %s", target)
		}
		seen[target] = struct{}{}
	}
	return nil
}

// NormalizeExposePath 填充隐式默认值并清理路径值，
// 使合并与校验逻辑可以基于规范化路径工作。
func NormalizeExposePath(item config.ExposePath) config.ExposePath {
	source := filepath.Clean(item.Source)
	target := item.Target
	if target == "" {
		target = source
	}
	return config.ExposePath{
		Source: source,
		Target: filepath.Clean(target),
		Mode:   item.Mode,
	}
}

// DefaultExposePaths 返回当前平台运行隔离子进程
// 所必需的最小内置主机路径集合。
func DefaultExposePaths(root string) []config.ExposePath {
	items := []config.ExposePath{{
		Source: root,
		Target: root,
		Mode:   "rw",
	}}
	if runtime.GOOS == "linux" {
		items = append(items, defaultLinuxSystemExposePaths()...)
	}
	return items
}

func defaultLinuxSystemExposePaths() []config.ExposePath {
	return existingExposePaths([]config.ExposePath{
		{Source: "/usr", Target: "/usr", Mode: "ro"},
		{Source: "/bin", Target: "/bin", Mode: "ro"},
		{Source: "/lib", Target: "/lib", Mode: "ro"},
		{Source: "/lib64", Target: "/lib64", Mode: "ro"},
		{Source: "/etc/resolv.conf", Target: "/etc/resolv.conf", Mode: "ro"},
		{Source: "/etc/hosts", Target: "/etc/hosts", Mode: "ro"},
		{Source: "/etc/nsswitch.conf", Target: "/etc/nsswitch.conf", Mode: "ro"},
		{Source: "/etc/passwd", Target: "/etc/passwd", Mode: "ro"},
		{Source: "/etc/group", Target: "/etc/group", Mode: "ro"},
		{Source: "/etc/ssl", Target: "/etc/ssl", Mode: "ro"},
		{Source: "/etc/pki", Target: "/etc/pki", Mode: "ro"},
		{Source: "/etc/ca-certificates", Target: "/etc/ca-certificates", Mode: "ro"},
		{Source: "/usr/share/ca-certificates", Target: "/usr/share/ca-certificates", Mode: "ro"},
		{Source: "/usr/local/share/ca-certificates", Target: "/usr/local/share/ca-certificates", Mode: "ro"},
		{Source: "/etc/alternatives", Target: "/etc/alternatives", Mode: "ro"},
		{Source: "/usr/share/zoneinfo", Target: "/usr/share/zoneinfo", Mode: "ro"},
		{Source: "/etc/localtime", Target: "/etc/localtime", Mode: "ro"},
	})
}

// existingExposePaths 只保留当前机器上真实存在的内置主机路径，
// 以免 Linux 隔离因发行版特有路径而失败。
func existingExposePaths(items []config.ExposePath) []config.ExposePath {
	filtered := make([]config.ExposePath, 0, len(items))
	for _, item := range items {
		if _, err := os.Stat(item.Source); err == nil {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

// MergeExposePaths 合并内置规则与用户覆盖项。规则以目标路径为键，
// 因此后出现的条目会替换同一目标的先前条目。
func MergeExposePaths(defaults []config.ExposePath, overrides []config.ExposePath) []config.ExposePath {
	merged := make([]config.ExposePath, 0, len(defaults)+len(overrides))
	indexByTarget := make(map[string]int, len(defaults)+len(overrides))
	appendOrReplace := func(item config.ExposePath) {
		normalized := NormalizeExposePath(item)
		if idx, ok := indexByTarget[normalized.Target]; ok {
			merged[idx] = normalized
			return
		}
		indexByTarget[normalized.Target] = len(merged)
		merged = append(merged, normalized)
	}
	for _, item := range defaults {
		appendOrReplace(item)
	}
	for _, item := range overrides {
		appendOrReplace(item)
	}
	return merged
}

// BuildLinuxMountPlan 将合并后的暴露路径配置转换为
// Linux bubblewrap 后端使用的挂载规则。
func BuildLinuxMountPlan(root string, overrides []config.ExposePath) []MountRule {
	merged := MergeExposePaths(DefaultExposePaths(root), overrides)
	plan := make([]MountRule, 0, len(merged))
	for _, item := range merged {
		plan = append(plan, MountRule{Source: item.Source, Target: item.Target, Mode: item.Mode})
	}
	return plan
}

// BuildWindowsAccessRules 推导 Windows 受限令牌后端
// 使用的主机路径访问策略。
func BuildWindowsAccessRules(root string, overrides []config.ExposePath) []AccessRule {
	merged := MergeExposePaths(nil, overrides)
	rules := make([]AccessRule, 0, len(merged)+1)
	rules = append(rules, AccessRule{Path: root, Mode: "rw"})
	for _, item := range merged {
		rules = append(rules, AccessRule{Path: item.Source, Mode: item.Mode})
	}
	return rules
}

func validateWindowsExposePaths(items []config.ExposePath) error {
	if len(items) == 0 {
		return nil
	}
	return fmt.Errorf("windows isolation does not yet support expose_paths filesystem rules")
}

// IsSupported 报告当前平台是否已有实现好的隔离后端。
func IsSupported() bool {
	return isSupportedOn(runtime.GOOS)
}

func isSupportedOn(goos string) bool {
	switch goos {
	case "linux", "windows":
		return true
	default:
		return false
	}
}

// Preflight 校验已配置的隔离状态，并在启动任何子进程
// 之前准备好实例运行时目录。
func Preflight() error {
	isolation := CurrentConfig()
	if !isolation.Enabled {
		return nil
	}
	if !IsSupported() {
		return fmt.Errorf("subprocess isolation is not supported on %s", runtime.GOOS)
	}
	root, err := ResolveInstanceRoot()
	if err != nil {
		return err
	}
	if err := PrepareInstanceRoot(root); err != nil {
		return err
	}
	if err := ValidateExposePaths(isolation.ExposePaths); err != nil {
		return err
	}
	if runtime.GOOS == "linux" {
		for _, rule := range BuildLinuxMountPlan(root, isolation.ExposePaths) {
			if rule.Source == "" || rule.Target == "" {
				return fmt.Errorf("invalid linux mount rule")
			}
		}
	}
	if runtime.GOOS == "windows" {
		if err := validateWindowsExposePaths(isolation.ExposePaths); err != nil {
			return err
		}
		for _, rule := range BuildWindowsAccessRules(root, isolation.ExposePaths) {
			if rule.Path == "" {
				return fmt.Errorf("invalid windows access rule")
			}
		}
	}
	return nil
}

// Start 为命令准备隔离、启动命令，并应用当前后端
// 所需的启动后平台钩子。
func Start(cmd *exec.Cmd) error {
	if err := PrepareCommand(cmd); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		cleanupPendingPlatformResources(cmd)
		return err
	}
	isolation := CurrentConfig()
	root := ""
	if isolation.Enabled {
		var err error
		root, err = ResolveInstanceRoot()
		if err != nil {
			terminateStartedCommand(cmd)
			return err
		}
	}
	if err := postStartPlatformIsolation(cmd, isolation, root); err != nil {
		terminateStartedCommand(cmd)
		return err
	}
	return nil
}

// Run 是 Start-and-Wait 辅助函数，保持与 Start 相同的隔离行为，
// 同时返回命令的最终退出状态。
func Run(cmd *exec.Cmd) error {
	if err := PrepareCommand(cmd); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		cleanupPendingPlatformResources(cmd)
		return err
	}
	isolation := CurrentConfig()
	root := ""
	if isolation.Enabled {
		var err error
		root, err = ResolveInstanceRoot()
		if err != nil {
			terminateStartedCommand(cmd)
			return err
		}
	}
	if err := postStartPlatformIsolation(cmd, isolation, root); err != nil {
		terminateStartedCommand(cmd)
		return err
	}
	return cmd.Wait()
}

func terminateStartedCommand(cmd *exec.Cmd) {
	cleanupPendingPlatformResources(cmd)
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// PrepareCommand 原地修改命令，使其在被调用方启动前
// 继承配置好的隔离环境。
func PrepareCommand(cmd *exec.Cmd) error {
	isolation := CurrentConfig()
	if err := Preflight(); err != nil {
		return err
	}
	if isolation.Enabled {
		root, err := ResolveInstanceRoot()
		if err != nil {
			return err
		}
		ApplyUserEnv(cmd, root)
		if err := applyPlatformIsolation(cmd, isolation, root); err != nil {
			return err
		}
	}
	return nil
}
