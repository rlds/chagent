//go:build linux

package isolation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
)

func applyPlatformIsolation(cmd *exec.Cmd, isolation config.IsolationConfig, root string) error {
	if !isolation.Enabled {
		return nil
	}
	// 目前 bubblewrap 是 Linux 上唯一受支持的后端。当其不可用时直接失败关闭，
	// 而不是静默地在无隔离的情况下运行子进程。
	bwrapPath, err := exec.LookPath("bwrap")
	if err != nil {
		hint := bwrapInstallHint()
		disableHint := `set "isolation.enabled": false in config.json`
		logger.WarnCF("isolation", "bubblewrap is required for Linux isolation",
			map[string]any{
				"binary":            "bwrap",
				"install":           hint,
				"disable_isolation": disableHint,
				"risk":              "disabling isolation lets child processes run without Linux filesystem isolation",
			})
		return fmt.Errorf(
			"linux isolation requires bwrap and does not fall back automatically: %w; install bubblewrap with one of: %s; or disable isolation by setting %s; disabling isolation means child processes can run without Linux filesystem isolation and may access or modify more host files",
			err,
			hint,
			disableHint,
		)
	}
	if cmd == nil || cmd.Path == "" || len(cmd.Args) == 0 {
		return nil
	}

	originalPath := cmd.Path
	originalArgs := append([]string{}, cmd.Args...)
	_, execDir, err := resolveLinuxWorkingDir(cmd.Dir, originalPath)
	if err != nil {
		return err
	}
	resolvedPath, err := resolveLinuxCommandPath(originalPath, execDir)
	if err != nil {
		return err
	}

	// 从配置好的挂载计划出发，仅额外加入可执行文件、其解析后的路径、实际工作目录，
	// 以及为保持原始命令语义所需的绝对路径参数。
	plan := BuildLinuxMountPlan(root, isolation.ExposePaths)
	plan = ensureLinuxMountRule(plan, resolvedPath, resolvedPath, "ro")
	plan = ensureLinuxMountRule(plan, filepath.Dir(resolvedPath), filepath.Dir(resolvedPath), "ro")
	if resolved, resolveErr := filepath.EvalSymlinks(resolvedPath); resolveErr == nil && resolved != resolvedPath {
		plan = ensureLinuxMountRule(plan, resolved, resolved, "ro")
		plan = ensureLinuxMountRule(plan, filepath.Dir(resolved), filepath.Dir(resolved), "ro")
	}
	if execDir != "" {
		plan = ensureLinuxMountRule(plan, execDir, execDir, "rw")
		if resolved, resolveErr := filepath.EvalSymlinks(execDir); resolveErr == nil && resolved != execDir {
			plan = ensureLinuxMountRule(plan, resolved, resolved, "rw")
		}
	}
	plan = appendLinuxArgumentMounts(plan, originalArgs[1:])
	logger.DebugCF("isolation", "linux isolation mount plan",
		map[string]any{
			"root":        root,
			"command":     resolvedPath,
			"working_dir": execDir,
			"mounts":      formatLinuxMountPlan(plan),
		})
	bwrapArgs, err := buildLinuxBwrapArgs(originalPath, resolvedPath, originalArgs, execDir, plan)
	if err != nil {
		return err
	}

	cmd.Path = bwrapPath
	cmd.Args = bwrapArgs
	cmd.Dir = ""
	return nil
}

func bwrapInstallHint() string {
	return "apt install bubblewrap; dnf install bubblewrap; yum install bubblewrap; pacman -S bubblewrap; apk add bubblewrap"
}

// formatLinuxMountPlan 将内部计划重塑为结构化日志所需的格式。
func formatLinuxMountPlan(plan []MountRule) []map[string]string {
	formatted := make([]map[string]string, 0, len(plan))
	for _, rule := range plan {
		formatted = append(formatted, map[string]string{
			"source": rule.Source,
			"target": rule.Target,
			"mode":   rule.Mode,
		})
	}
	return formatted
}

func postStartPlatformIsolation(cmd *exec.Cmd, isolation config.IsolationConfig, root string) error {
	return nil
}

func cleanupPendingPlatformResources(cmd *exec.Cmd) {
}

// buildLinuxBwrapArgs 将挂载计划转换为 bubblewrap 命令行，
// 以便在隔离的挂载视图中重新执行原始进程。
func buildLinuxBwrapArgs(
	originalPath string,
	resolvedPath string,
	originalArgs []string,
	execDir string,
	plan []MountRule,
) ([]string, error) {
	bwrapArgs := []string{
		"bwrap",
		"--die-with-parent",
		"--unshare-ipc",
		"--proc", "/proc",
		"--dev", "/dev",
	}
	for _, rule := range plan {
		flag, err := linuxBindFlag(rule)
		if err != nil {
			return nil, err
		}
		bwrapArgs = append(bwrapArgs, flag, rule.Source, rule.Target)
	}
	if execDir != "" {
		bwrapArgs = append(bwrapArgs, "--chdir", execDir)
	}
	execPath := originalPath
	if isRelativeCommandPath(originalPath) {
		execPath = resolvedPath
	}
	bwrapArgs = append(bwrapArgs, "--", execPath)
	if len(originalArgs) > 1 {
		bwrapArgs = append(bwrapArgs, originalArgs[1:]...)
	}
	return bwrapArgs, nil
}

func resolveLinuxWorkingDir(originalDir, originalPath string) (string, string, error) {
	if originalDir != "" {
		resolved, err := filepath.Abs(originalDir)
		if err != nil {
			return "", "", fmt.Errorf("resolve command dir %s: %w", originalDir, err)
		}
		return resolved, resolved, nil
	}
	if !isRelativeCommandPath(originalPath) {
		return "", "", nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("resolve current working dir: %w", err)
	}
	return "", wd, nil
}

func resolveLinuxCommandPath(originalPath, execDir string) (string, error) {
	if filepath.IsAbs(originalPath) || !isRelativeCommandPath(originalPath) {
		return filepath.Clean(originalPath), nil
	}
	base := execDir
	if base == "" {
		var err error
		base, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve current working dir: %w", err)
		}
	}
	return filepath.Clean(filepath.Join(base, originalPath)), nil
}

func appendLinuxArgumentMounts(plan []MountRule, args []string) []MountRule {
	for _, arg := range args {
		path, ok := linuxArgumentPath(arg)
		if !ok {
			continue
		}
		clean := filepath.Clean(path)
		if info, err := os.Stat(clean); err == nil {
			mode := "ro"
			if info.IsDir() {
				mode = "rw"
			}
			plan = ensureLinuxMountRule(plan, clean, clean, mode)
			if resolved, resolveErr := filepath.EvalSymlinks(clean); resolveErr == nil && resolved != clean {
				plan = ensureLinuxMountRule(plan, resolved, resolved, mode)
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			continue
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			continue
		}
		if _, err := os.Stat(parent); err == nil {
			plan = ensureLinuxMountRule(plan, parent, parent, "rw")
		}
	}
	return plan
}

func linuxArgumentPath(arg string) (string, bool) {
	if filepath.IsAbs(arg) {
		return arg, true
	}
	idx := strings.IndexRune(arg, '=')
	if idx <= 0 || idx == len(arg)-1 {
		return "", false
	}
	value := arg[idx+1:]
	if !filepath.IsAbs(value) {
		return "", false
	}
	return value, true
}

func isRelativeCommandPath(path string) bool {
	return !filepath.IsAbs(path) && strings.ContainsRune(path, filepath.Separator)
}

// ensureLinuxMountRule 追加一条挂载规则，除非已有其他规则占用同一目标路径。
func ensureLinuxMountRule(plan []MountRule, source, target, mode string) []MountRule {
	cleanSource := filepath.Clean(source)
	cleanTarget := filepath.Clean(target)
	for _, rule := range plan {
		if filepath.Clean(rule.Target) == cleanTarget {
			return plan
		}
	}
	return append(plan, MountRule{Source: cleanSource, Target: cleanTarget, Mode: mode})
}

// linuxBindFlag 根据挂载模式选择正确的 bubblewrap 绑定标志。
func linuxBindFlag(rule MountRule) (string, error) {
	info, err := os.Stat(rule.Source)
	if err != nil {
		return "", fmt.Errorf("stat linux mount source %s: %w", rule.Source, err)
	}
	if !info.IsDir() {
		if rule.Mode == "rw" {
			return "--bind", nil
		}
		return "--ro-bind", nil
	}
	if rule.Mode == "rw" {
		return "--bind", nil
	}
	return "--ro-bind", nil
}
