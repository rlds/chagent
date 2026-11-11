package workspacecontext

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rlds/chagent/pkg/logger"
)

// Warnf 记录非致命的发现/读取诊断信息。它以变量形式提供，便于测试
// 和集成重定向。
var Warnf = func(format string, args ...any) {
	logger.WarnCF("workspacecontext", fmt.Sprintf(format, args...), nil)
}

// FindProjectRoot 从 workspace 向上遍历，返回第一个包含任一标记的目录。
// 文件系统根目录永远不会被视为项目根：未找到任何标记时，返回 workspace 本身。
//
// 缺失的标记（包括 ENOTDIR）会被跳过。其他任何 stat 错误
// （权限、I/O）都会被返回：静默越过不可读的目录可能加载错误的指令链。
func FindProjectRoot(workspace string, markers []string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %q: %w", workspace, err)
	}
	abs = filepath.Clean(abs)
	markers = validCandidateNames(markers)
	dir := abs
	for {
		for _, marker := range markers {
			markerPath := filepath.Join(dir, marker)
			if _, statErr := os.Stat(markerPath); statErr == nil {
				return dir, nil
			} else if errors.Is(statErr, fs.ErrNotExist) || errors.Is(statErr, syscall.ENOTDIR) {
				continue
			} else {
				return "", fmt.Errorf("stat project root marker %q in %q: %w", marker, dir, statErr)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil
		}
		dir = parent
	}
}

// AncestorChain 返回从 root 到 dir（含两端）的目录列表，按从宽泛到具体的顺序排列。
// 当 dir 位于 root 之外时，只返回 dir 并记录一条警告。
func AncestorChain(root, dir string) []string {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		Warnf("ancestor chain: resolve root %q: %v", root, err)
		return []string{cleanAbs(dir)}
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		Warnf("ancestor chain: resolve dir %q: %v", dir, err)
		return []string{cleanAbs(root)}
	}
	absRoot = filepath.Clean(absRoot)
	absDir = filepath.Clean(absDir)
	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		Warnf("ancestor chain: %q is not inside root %q; falling back to the directory itself", absDir, absRoot)
		return []string{absDir}
	}
	chain := []string{absRoot}
	if rel == "." {
		return chain
	}
	current := absRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		chain = append(chain, current)
	}
	return chain
}

// DescendantDirs 返回 root 之下、位于通往 touchedPath 路径上的目录，
// 按从宽泛到具体的顺序排列。root 本身被排除，因为基线链已经覆盖了它。
// root 之外的路径以及直接在 root 处发生的触碰不会产生任何目录。
func DescendantDirs(root, touchedPath string) []string {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		Warnf("descendant dirs: resolve root %q: %v", root, err)
		return nil
	}
	absTouched, err := filepath.Abs(touchedPath)
	if err != nil {
		Warnf("descendant dirs: resolve touch %q: %v", touchedPath, err)
		return nil
	}
	targetDir := filepath.Dir(filepath.Clean(absTouched))
	if info, statErr := os.Stat(absTouched); statErr == nil && info.IsDir() {
		targetDir = filepath.Clean(absTouched)
	}
	rel, err := filepath.Rel(filepath.Clean(absRoot), targetDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	dirs := make([]string, 0, 2)
	current := filepath.Clean(absRoot)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		dirs = append(dirs, current)
	}
	return dirs
}

// DiscoverBaseline 返回工作区/项目链的有序基线候选
// （启用时还包括用户全局根）。
//
// 顺序与发现规则一致：先用户全局根（chagent home，然后是 agents home），
// 再从宽泛到具体的项目目录，而在同一个目录内，
// 指令文件先于本地覆盖文件。
//
// 发现过程只对路径做 stat，绝不读取内容。存在但无法探测的路径会作为
// Unavailable 候选返回，而不是让整个发现失败；只有项目根标记错误才作为错误返回。
func DiscoverBaseline(cfg Config, workspace, excludePath string) ([]Candidate, error) {
	cfg = cfg.normalize()
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace %q: %w", workspace, err)
	}
	absWorkspace = filepath.Clean(absWorkspace)

	projectRoot, err := FindProjectRoot(absWorkspace, cfg.ProjectRootMarkers)
	if err != nil {
		return nil, err
	}

	candidates := make([]Candidate, 0, 8)
	if cfg.UserGlobal {
		userRoots := []struct {
			directory string
			home      string
		}{
			{scopeUserChagent, cfg.ChagentHome},
			{scopeUserAgents, cfg.AgentsHome},
		}
		for _, root := range userRoots {
			if root.home == "" {
				continue
			}
			absHome, err := filepath.Abs(root.home)
			if err != nil {
				Warnf("discover: resolve user-global home %q: %v", root.home, err)
				continue
			}
			candidate := probeCandidate(candidateProbe{
				absolutePath: filepath.Join(absHome, userGlobalInstructionFile),
				directory:    root.directory,
				displayPath:  userGlobalDisplayPath(absHome, userGlobalInstructionFile),
				name:         userGlobalInstructionFile,
				userGlobal:   true,
				excludePath:  excludePath,
			})
			if candidate != nil {
				candidates = append(candidates, *candidate)
			}
		}
	}

	chain := AncestorChain(projectRoot, absWorkspace)
	for _, dir := range chain {
		relativeDir := "."
		if rel, relErr := filepath.Rel(projectRoot, dir); relErr == nil {
			relativeDir = rel
		}
		for _, name := range cfg.InstructionFiles {
			candidate := probeCandidate(candidateProbe{
				absolutePath: filepath.Join(dir, name),
				directory:    projectDirectoryKey(relativeDir),
				displayPath:  projectDisplayPath(relativeDir, name),
				name:         name,
				excludePath:  excludePath,
			})
			if candidate != nil {
				candidates = append(candidates, *candidate)
			}
		}
		for _, name := range cfg.LocalInstructionFiles {
			candidate := probeCandidate(candidateProbe{
				absolutePath: filepath.Join(dir, name),
				directory:    projectDirectoryKey(relativeDir),
				displayPath:  projectDisplayPath(relativeDir, name),
				name:         name,
				excludePath:  excludePath,
			})
			if candidate != nil {
				candidates = append(candidates, *candidate)
			}
		}
	}
	return candidates, nil
}

// candidateProbe 是 probeCandidate 的输入。
type candidateProbe struct {
	absolutePath string
	directory    string
	displayPath  string
	name         string
	userGlobal   bool
	excludePath  string
}

// probeCandidate 对单个候选路径执行 stat。路径不存在或被排除时返回 nil。
// 非"未找到"的 stat 错误会产生 Unavailable 候选，
// 以便投影器保留上一次成功的内容。
func probeCandidate(probe candidateProbe) *Candidate {
	if probe.excludePath != "" && sameFile(probe.absolutePath, probe.excludePath) {
		return nil
	}
	version, err := StatVersion(probe.absolutePath)
	candidate := &Candidate{
		AbsolutePath: probe.absolutePath,
		DisplayPath:  probe.displayPath,
		Directory:    probe.directory,
		Name:         probe.name,
		Scope:        scopeKey(probe.directory, probe.name),
		Version:      version,
		UserGlobal:   probe.userGlobal,
	}
	if err == nil {
		return candidate
	}
	if IsAbsent(err) {
		return nil
	}
	candidate.Unavailable = true
	Warnf("discover: cannot stat %s: %v", probe.absolutePath, err)
	return candidate
}

// ReadPolicy 控制指令读取时的符号链接处理方式。
type ReadPolicy struct {
	// FollowSymlinks 在路径最后一段是符号链接时允许跟随。
	// 为 false（默认值）时，符号链接候选会被跳过。
	FollowSymlinks bool
	// AllowedRoots 是求值后的符号链接目标唯一允许所在的根。
	// 为空时，即使设置了 FollowSymlinks，符号链接也总会被拒绝。
	AllowedRoots []string
}

// ReadBounded 从 path 最多读取 maxBytes 字节。对超限文件绝不返回部分内容：
// 文件超过限制时返回 ErrSourceTooLarge 且不返回内容。
// 错误会被归类为 ErrAbsent 或 ErrUnavailable，
// 使调用方能够区分删除与读取失败。
func ReadBounded(path string, maxBytes int64) ([]byte, FileVersion, error) {
	return ReadBoundedWithPolicy(path, maxBytes, ReadPolicy{})
}

// ReadBoundedWithPolicy 与 ReadBounded 相同，但使用显式的符号链接策略。
func ReadBoundedWithPolicy(path string, maxBytes int64, policy ReadPolicy) ([]byte, FileVersion, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxSourceBytes
	}
	if err := checkSymlinkPolicy(path, policy); err != nil {
		return nil, FileVersion{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, FileVersion{}, classifyReadError(path, err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			Warnf("read: close %s: %v", path, closeErr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, FileVersion{}, fmt.Errorf("%w: stat %s: %w", ErrUnavailable, path, err)
	}
	version := versionFromFileInfo(info)
	if info.IsDir() {
		return nil, version, fmt.Errorf("%w: %s is a directory", ErrUnavailable, path)
	}
	if info.Size() > maxBytes {
		return nil, version, fmt.Errorf("%w: %s: size %d exceeds %d", ErrSourceTooLarge, path, info.Size(), maxBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, version, fmt.Errorf("%w: read %s: %w", ErrUnavailable, path, err)
	}
	if int64(len(content)) > maxBytes {
		return nil, version, fmt.Errorf("%w: %s: read exceeds %d", ErrSourceTooLarge, path, maxBytes)
	}
	return content, version, nil
}

// StatVersion 对 path 执行 stat，并像 ReadBounded 一样对错误分类。
// 它不读取内容，因此开销足够低，可用于每回合的缓存探测。
func StatVersion(path string) (FileVersion, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileVersion{}, classifyReadError(path, err)
	}
	return versionFromFileInfo(info), nil
}

func classifyReadError(path string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return fmt.Errorf("%w: %s: %w", ErrAbsent, path, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, path, err)
}

func checkSymlinkPolicy(path string, policy ReadPolicy) error {
	info, err := os.Lstat(path)
	if err != nil {
		return classifyReadError(path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	if !policy.FollowSymlinks {
		return fmt.Errorf("%w: %s: symlinks are disabled", ErrSymlinkSkipped, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%w: %s: resolve symlink: %w", ErrUnavailable, path, err)
	}
	if !withinAllowedRoots(resolved, policy.AllowedRoots) {
		return fmt.Errorf("%w: %s resolves outside allowed roots: %s", ErrSymlinkSkipped, path, resolved)
	}
	return nil
}

func withinAllowedRoots(target string, roots []string) bool {
	if len(roots) == 0 {
		return false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	absTarget = resolveExistingPath(absTarget)
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		absRoot = resolveExistingPath(absRoot)
		rel, relErr := filepath.Rel(absRoot, absTarget)
		if relErr != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

// resolveExistingPath 对 path 最长的已存在前缀解析符号链接，
// 使允许根的判定不会被符号链接的临时目录绕过
// （例如 macOS 上的 /var -> /private/var）。
func resolveExistingPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		if abs, absErr := filepath.Abs(resolved); absErr == nil {
			return filepath.Clean(abs)
		}
	}
	parent := filepath.Dir(path)
	if parent == path {
		return filepath.Clean(path)
	}
	resolvedParent := resolveExistingPath(parent)
	return filepath.Join(resolvedParent, filepath.Base(path))
}

// DedupeByCandidateGroup 移除那些去掉首尾空白后的内容在同一候选组中
// 已出现过的候选。用户全局根共享一个组（因此内容相同的 ~/.chagent 与 ~/.agents
// 文件会被去重），而每个项目目录自成一个组。发现顺序中最早的候选胜出，
// 因此 kept 会保持原有顺序。
func DedupeByCandidateGroup(candidates []Candidate) (kept []Candidate, deduped []Candidate) {
	seen := make(map[string]map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.Unavailable {
			kept = append(kept, candidate)
			continue
		}
		ensureDigests(&candidate)
		group := dedupeGroup(candidate)
		if seen[group] == nil {
			seen[group] = make(map[string]struct{})
		}
		if _, duplicate := seen[group][candidate.TrimmedDigest]; duplicate {
			deduped = append(deduped, candidate)
			continue
		}
		seen[group][candidate.TrimmedDigest] = struct{}{}
		kept = append(kept, candidate)
	}
	return kept, deduped
}

func dedupeGroup(candidate Candidate) string {
	if candidate.UserGlobal {
		return "user-global"
	}
	return candidate.Directory
}

// userGlobalDisplayPath 尽可能把用户全局指令路径渲染为
// ~/.chagent/AGENTS.md 或 ~/.agents/AGENTS.md。
// 不在进程用户主目录下的 home 会以绝对路径显示。
func userGlobalDisplayPath(absHome, name string) string {
	if userHome, err := os.UserHomeDir(); err == nil {
		rel, relErr := filepath.Rel(userHome, absHome)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "." {
			return path.Join("~", filepath.ToSlash(rel), name)
		}
	}
	return filepath.ToSlash(filepath.Join(absHome, name))
}

func cleanAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	if aErr != nil || bErr != nil {
		return false
	}
	return os.SameFile(aInfo, bInfo)
}
