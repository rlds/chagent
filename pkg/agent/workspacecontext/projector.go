package workspacecontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/rlds/chagent/pkg/providers"
)

const baselineIdentityVersion = "workspace-context-v1"

// BaselineInfo 记录最近一次可见的基线。
type BaselineInfo struct {
	Identity string
	Seq      int
}

// VisibleState 是根据会话历史重建的指令状态。它从不依赖 projector 的内存状态，
// 因此能在压缩与进程重启后继续存在。
type VisibleState struct {
	Baseline *BaselineInfo
	// Scopes 映射内部作用域键 -> 最后应用的变更。后出现的变更优先。
	Scopes map[string]Change
	// Seen 表示是否找到过任何工作区指令上下文消息。
	// 它用于区分全新会话（尚无上下文）与基线已被压缩丢弃的会话。
	Seen bool
}

// ScanVisible 按从旧到新的顺序重放所有工作区指令上下文消息。其他上下文类型
// 会被忽略。基线消息只记录其身份标识；持久作用域由该消息的 Changes 重建。
func ScanVisible(history []providers.Message) VisibleState {
	state := VisibleState{Scopes: make(map[string]Change)}
	for index, message := range history {
		meta := message.Context
		if meta == nil || meta.Kind != ContextKindWorkspaceInstructions {
			continue
		}
		state.Seen = true
		if meta.Baseline {
			state.Baseline = &BaselineInfo{Identity: meta.BaselineIdentity, Seq: index}
		}
		for _, change := range meta.Changes {
			scope := change.Scope
			if scope == "" {
				continue
			}
			switch change.Action {
			case ChangeActionRemove:
				delete(state.Scopes, scope)
			case ChangeActionSet, ChangeActionReplace:
				state.Scopes[scope] = change
			}
		}
	}
	return state
}

// BaselineIdentity 为一个工作区的发现/预算语义生成指纹。它有意不包含文件内容
// 和时间戳：只有在指令集合会被以不同方式发现时它才变化。
func BaselineIdentity(cfg Config, projectRoot, workspace string) string {
	return BaselineIdentityWithExclude(cfg, projectRoot, workspace, "")
}

// BaselineIdentityWithExclude 是带有被排除定义路径的 BaselineIdentity，
// projector 知道该路径，但独立的调用方可能不知道。
func BaselineIdentityWithExclude(cfg Config, projectRoot, workspace, excludePath string) string {
	cfg = cfg.normalize()
	relProjectRoot := relativeForIdentity(workspace, projectRoot)
	relExclude := ""
	if excludePath != "" {
		relExclude = relativeForIdentity(workspace, excludePath)
	}
	chagentHome := ""
	agentsHome := ""
	if cfg.UserGlobal {
		chagentHome = cleanAbs(cfg.ChagentHome)
		agentsHome = cleanAbs(cfg.AgentsHome)
	}
	payload := struct {
		Version               string   `json:"version"`
		ProjectRoot           string   `json:"project_root"`
		UserGlobal            bool     `json:"user_global"`
		ChagentHome           string   `json:"chagent_home,omitempty"`
		AgentsHome            string   `json:"agents_home,omitempty"`
		FollowSymlinks        bool     `json:"follow_symlinks"`
		ProjectRootMarkers    []string `json:"project_root_markers"`
		InstructionFiles      []string `json:"instruction_files"`
		LocalInstructionFiles []string `json:"local_instruction_files"`
		MaxBytes              int      `json:"max_bytes"`
		MaxSourceBytes        int      `json:"max_source_bytes"`
		ExcludePath           string   `json:"exclude_path,omitempty"`
	}{
		Version:               baselineIdentityVersion,
		ProjectRoot:           relProjectRoot,
		UserGlobal:            cfg.UserGlobal,
		ChagentHome:           chagentHome,
		AgentsHome:            agentsHome,
		FollowSymlinks:        cfg.FollowSymlinks,
		ProjectRootMarkers:    cfg.ProjectRootMarkers,
		InstructionFiles:      cfg.InstructionFiles,
		LocalInstructionFiles: cfg.LocalInstructionFiles,
		MaxBytes:              cfg.MaxBytes,
		MaxSourceBytes:        cfg.MaxSourceBytes,
		ExcludePath:           relExclude,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// 对于这个全为 string/int 的负载，json.Marshal 不会失败；此处仅作防御。
		encoded = []byte(fmt.Sprintf("%v", payload))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func relativeForIdentity(base, target string) string {
	if base == "" || target == "" {
		return ""
	}
	absBase, baseErr := filepath.Abs(base)
	absTarget, targetErr := filepath.Abs(target)
	if baseErr != nil || targetErr != nil {
		return filepath.ToSlash(target)
	}
	rel, err := filepath.Rel(filepath.Clean(absBase), filepath.Clean(absTarget))
	if err != nil {
		return filepath.ToSlash(target)
	}
	return filepath.ToSlash(rel)
}

// Projector 根据发现结果、有界读取、渲染以及可见历史状态，组合出持久的工作区
// 指令上下文消息。
//
// Projector 只保留基于 stat 的缓存；所有持久状态都会在每次 Compose 时从历史
// 重建，因此恢复/重启绝不会重复生成基线。
type Projector struct {
	cfg         Config
	workspace   string
	projectRoot string
	excludePath string

	// versions/digests/trimmed 是针对此前成功读取过的作用域的
	// 基于 stat 的快速路径缓存。
	versions map[string]FileVersion
	digests  map[string]string
	trimmed  map[string]string

	// excluded 记录因渲染预算而被省略的作用域。在基线重建或其所在目录被改动
	// 之前，它们不会被重新探测。
	excluded map[string]struct{}

	statsMu   sync.Mutex
	lastStats Stats
	lastPaths []string
}

// NewProjector 解析工作区与项目根目录并准备缓存。
// 项目根标记错误会被返回：静默猜测根目录可能加载错误的指令链。
func NewProjector(cfg Config, workspace, excludePath string) (*Projector, error) {
	cfg = cfg.normalize()
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("workspace context: resolve workspace %q: %w", workspace, err)
	}
	absWorkspace = filepath.Clean(absWorkspace)
	projectRoot, err := FindProjectRoot(absWorkspace, cfg.ProjectRootMarkers)
	if err != nil {
		return nil, fmt.Errorf("workspace context: %w", err)
	}
	projector := &Projector{
		cfg:         cfg,
		workspace:   absWorkspace,
		projectRoot: projectRoot,
		excludePath: "",
		versions:    make(map[string]FileVersion),
		digests:     make(map[string]string),
		trimmed:     make(map[string]string),
		excluded:    make(map[string]struct{}),
	}
	if strings.TrimSpace(excludePath) != "" {
		projector.excludePath = cleanAbs(excludePath)
	}
	return projector, nil
}

// Config 返回 projector 使用的规范化配置。
func (p *Projector) Config() Config { return p.cfg }

// SnapshotStats 返回最近一次渲染的统计信息和模型可见的路径。
func (p *Projector) SnapshotStats() (Stats, []string) {
	if p == nil {
		return Stats{}, nil
	}
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	return p.lastStats, append([]string(nil), p.lastPaths...)
}

func (p *Projector) recordStats(stats Stats, paths []string) {
	if p == nil {
		return
	}
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	p.lastStats = stats
	p.lastPaths = append([]string(nil), paths...)
}

// ProjectRoot 返回解析后的项目根目录。
func (p *Projector) ProjectRoot() string { return p.projectRoot }

// Workspace 返回工作区的绝对路径。
func (p *Projector) Workspace() string { return p.workspace }

func (p *Projector) identity() string {
	return BaselineIdentityWithExclude(p.cfg, p.projectRoot, p.workspace, p.excludePath)
}

// 当有内容发生变化时，Compose 返回一条持久上下文消息；没有新内容可注入时，
// Message 为 nil。
//
// touchedPaths 是自上次 compose 以来文件工具成功改动过的文件路径；它们会触发
// 在其父目录中进行嵌套发现。
func (p *Projector) Compose(ctx context.Context, history []providers.Message, touched []string) (ComposeResult, error) {
	if p == nil {
		return ComposeResult{}, nil
	}
	if err := ctx.Err(); err != nil {
		return ComposeResult{}, err
	}
	if !p.cfg.Enabled {
		return ComposeResult{}, nil
	}

	visible := ScanVisible(history)
	identity := p.identity()
	baselineVisible := visible.Baseline != nil && visible.Baseline.Identity == identity
	rebuild := !baselineVisible
	if rebuild {
		// 新基线下会重新启用对因预算被排除作用域的探测。
		p.excluded = make(map[string]struct{})
	}

	touchedScopes := make(map[string]struct{})
	touchedSpecs := p.touchedSpecs(touched)
	for _, spec := range touchedSpecs {
		touchedScopes[spec.scope] = struct{}{}
	}

	specs, err := p.collectSpecs(visible, touchedSpecs, !rebuild)
	if err != nil {
		return ComposeResult{}, fmt.Errorf("workspace context discovery: %w", err)
	}

	specByScope := make(map[string]candidateSpec, len(specs))
	probes := make([]probeResult, 0, len(specs))
	for _, spec := range specs {
		if err := ctx.Err(); err != nil {
			return ComposeResult{}, err
		}
		if _, skipped := p.excluded[spec.scope]; skipped {
			if _, touchedNow := touchedScopes[spec.scope]; !touchedNow {
				continue
			}
			delete(p.excluded, spec.scope)
		}
		probe := p.probe(spec, visible, !rebuild)
		probe.spec = spec
		specByScope[spec.scope] = spec
		probes = append(probes, probe)
	}

	var okCandidates []Candidate
	var absentSpecs []candidateSpec
	unavailableScopes := make(map[string]struct{})
	tooLargeScopes := make(map[string]struct{})
	contentLoaded := make(map[string]bool)
	for _, probe := range probes {
		switch probe.status {
		case probeStatusOK:
			okCandidates = append(okCandidates, probe.candidate)
			contentLoaded[probe.spec.scope] = probe.hasContent
		case probeStatusAbsent:
			absentSpecs = append(absentSpecs, probe.spec)
		case probeStatusTooLarge:
			tooLargeScopes[probe.spec.scope] = struct{}{}
		case probeStatusUnavailable:
			unavailableScopes[probe.spec.scope] = struct{}{}
		}
	}

	kept, deduped := DedupeByCandidateGroup(okCandidates)
	finalKept := make([]Candidate, 0, len(kept))
	for _, candidate := range kept {
		spec, hasSpec := specByScope[candidate.Scope]
		if !hasSpec {
			continue
		}
		if !contentLoaded[candidate.Scope] {
			loaded := p.loadContent(spec)
			switch loaded.status {
			case probeStatusOK:
				candidate = loaded.candidate
				contentLoaded[candidate.Scope] = loaded.hasContent
			case probeStatusAbsent:
				absentSpecs = append(absentSpecs, spec)
				continue
			case probeStatusTooLarge:
				tooLargeScopes[candidate.Scope] = struct{}{}
				continue
			default:
				unavailableScopes[candidate.Scope] = struct{}{}
				continue
			}
		}
		finalKept = append(finalKept, candidate)
	}

	if rebuild {
		return p.composeBaseline(visible, identity, finalKept, unavailableScopes, tooLargeScopes)
	}
	return p.composeDelta(visible, finalKept, deduped, absentSpecs, unavailableScopes, tooLargeScopes, specByScope)
}

// composeBaseline 渲染完整基线（可选地替换此前的基线），并在同一条消息中
// 移除已过时的可见作用域。
func (p *Projector) composeBaseline(
	visible VisibleState,
	identity string,
	kept []Candidate,
	unavailableScopes map[string]struct{},
	tooLargeScopes map[string]struct{},
) (ComposeResult, error) {
	replace := visible.Seen
	rendered := RenderBaseline(kept, p.cfg, replace)

	renderedScopes := make(map[string]struct{}, len(rendered.Changes))
	for _, change := range rendered.Changes {
		renderedScopes[change.Scope] = struct{}{}
	}

	removalItems := make([]ChangeItem, 0, len(visible.Scopes))
	for scope, change := range visible.Scopes {
		if _, stillRendered := renderedScopes[scope]; stillRendered {
			continue
		}
		if _, bad := unavailableScopes[scope]; bad {
			continue
		}
		if _, bad := tooLargeScopes[scope]; bad {
			continue
		}
		removalItems = append(removalItems, ChangeItem{
			Action: ChangeActionRemove,
			Scope:  scope,
			Path:   change.Path,
			Digest: change.Digest,
		})
	}
	sort.Slice(removalItems, func(i, j int) bool { return removalItems[i].Path < removalItems[j].Path })

	text := rendered.Text
	changes := rendered.Changes
	if len(removalItems) > 0 {
		removalRendered := RenderChanges(removalItems, p.cfg)
		if merged := mergeFrames(text, removalRendered.Text, 2*p.cfg.MaxBytes); merged != "" {
			text = merged
			changes = append(changes, removalRendered.Changes...)
		}
	}
	if text == "" {
		return ComposeResult{}, nil
	}

	p.recordCaches(kept)
	for scope := range visible.Scopes {
		if _, stillRendered := renderedScopes[scope]; !stillRendered {
			delete(p.versions, scope)
			delete(p.digests, scope)
			delete(p.trimmed, scope)
		}
	}
	p.excluded = make(map[string]struct{})
	displayToScope := make(map[string]string, len(kept)+len(visible.Scopes))
	for _, candidate := range kept {
		displayToScope[candidate.DisplayPath] = candidate.Scope
	}
	for scope, change := range visible.Scopes {
		displayToScope[change.Path] = scope
	}
	p.markExcluded(rendered.Omitted, displayToScope)

	result := ComposeResult{
		Message: &providers.Message{
			Role:    "user",
			Content: text,
			Context: &ContextMeta{
				Kind:             ContextKindWorkspaceInstructions,
				Form:             ContextFormReminder,
				Baseline:         true,
				BaselineIdentity: identity,
				Changes:          changes,
			},
		},
		Stats: statsForChanges(rendered, true, changes),
	}
	result.Stats.Bytes = len(text)
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	p.recordStats(result.Stats, paths)
	return result, nil
}

// composeDelta 探测当前作用域集合，并且只渲染真实的变更。
func (p *Projector) composeDelta(
	visible VisibleState,
	kept []Candidate,
	deduped []Candidate,
	absentSpecs []candidateSpec,
	unavailableScopes map[string]struct{},
	tooLargeScopes map[string]struct{},
	specByScope map[string]candidateSpec,
) (ComposeResult, error) {
	// 如果某个作用域可见但无法读取，则保留最后一次成功的内容。此时同一去重组中
	// 的其他文件不得被提升：我们无法将它们的内容与这个不可读的胜出者比较。
	blockedGroups := make(map[string]struct{})
	for scope := range unavailableScopes {
		if _, active := visible.Scopes[scope]; !active {
			continue
		}
		if spec, ok := specByScope[scope]; ok {
			blockedGroups[candidateGroupForSpec(spec)] = struct{}{}
		}
	}
	for scope := range tooLargeScopes {
		if _, active := visible.Scopes[scope]; !active {
			continue
		}
		if spec, ok := specByScope[scope]; ok {
			blockedGroups[candidateGroupForSpec(spec)] = struct{}{}
		}
	}
	groupBlocked := func(scope string) bool {
		spec, ok := specByScope[scope]
		if !ok {
			return false
		}
		_, blocked := blockedGroups[candidateGroupForSpec(spec)]
		return blocked
	}

	items := make([]ChangeItem, 0, len(kept))
	for _, candidate := range kept {
		active, exists := visible.Scopes[candidate.Scope]
		action := ChangeActionSet
		if exists {
			if active.Digest == candidate.Digest {
				continue
			}
			action = ChangeActionReplace
		} else if len(blockedGroups) > 0 && groupBlocked(candidate.Scope) {
			continue
		}
		items = append(items, changeItemFor(candidate, action))
	}
	for _, candidate := range deduped {
		if _, active := visible.Scopes[candidate.Scope]; !active {
			continue
		}
		if len(blockedGroups) > 0 && groupBlocked(candidate.Scope) {
			continue
		}
		items = append(items, removeItemForSpec(specForCandidate(candidate), candidate.Digest))
	}
	for _, spec := range absentSpecs {
		active, exists := visible.Scopes[spec.scope]
		if !exists {
			continue
		}
		items = append(items, ChangeItem{
			Action:     ChangeActionRemove,
			Scope:      spec.scope,
			Path:       active.Path,
			Directory:  spec.directory,
			UserGlobal: spec.userGlobal,
		})
	}
	if len(items) == 0 {
		p.recordCaches(kept)
		return ComposeResult{}, nil
	}

	rendered := RenderChanges(items, p.cfg)
	if rendered.Text == "" {
		// 预算过小，连一个框架都放不下：下一轮重试，且不污染缓存或排除集合。
		return ComposeResult{Stats: Stats{Baseline: false, Bytes: 0}}, nil
	}

	applied := make(map[string]struct{}, len(rendered.Changes))
	for _, change := range rendered.Changes {
		applied[change.Scope] = struct{}{}
	}
	for _, candidate := range kept {
		if _, ok := applied[candidate.Scope]; ok {
			p.recordCandidateCache(candidate)
		}
	}
	for _, change := range rendered.Changes {
		if change.Action == ChangeActionRemove {
			delete(p.versions, change.Scope)
			delete(p.digests, change.Scope)
			delete(p.trimmed, change.Scope)
		}
	}
	for _, candidate := range kept {
		if _, ok := applied[candidate.Scope]; !ok {
			if _, active := visible.Scopes[candidate.Scope]; active {
				// 该作用域没有变化；保持缓存温热。
				if cachedVersion, ok := p.versions[candidate.Scope]; ok && cachedVersion.Equal(candidate.Version) {
					continue
				}
				p.recordCandidateCache(candidate)
			}
		}
	}

	// 因预算而被省略的段落会从后续探测中排除，直到基线重建或其所在目录被改动。
	displayToScope := make(map[string]string, len(items))
	for _, item := range items {
		displayToScope[item.Path] = item.Scope
	}
	p.markExcluded(rendered.Omitted, displayToScope)
	if len(rendered.Changes) == 0 {
		// 降级框架中没有任何已应用的段落：不要无限重试，也不要注入仅含提示的
		// 消息（没有状态转换能通过预算，因此下一轮会从未变的可见状态重试）。
		for _, item := range items {
			p.excluded[item.Scope] = struct{}{}
		}
		return ComposeResult{}, nil
	}

	result := ComposeResult{
		Message: &providers.Message{
			Role:    "user",
			Content: rendered.Text,
			Context: &ContextMeta{
				Kind:    ContextKindWorkspaceInstructions,
				Form:    ContextFormReminder,
				Changes: rendered.Changes,
			},
		},
		Stats: statsForChanges(rendered, false, rendered.Changes),
	}
	result.Stats.Bytes = len(rendered.Text)
	paths := make([]string, 0, len(rendered.Changes))
	for _, change := range rendered.Changes {
		paths = append(paths, change.Path)
	}
	p.recordStats(result.Stats, paths)
	return result, nil
}

// markExcluded 记录显示路径因预算而被省略的作用域。
func (p *Projector) markExcluded(omitted []string, displayToScope map[string]string) {
	for _, display := range omitted {
		if scope, ok := displayToScope[display]; ok {
			p.excluded[scope] = struct{}{}
		}
	}
}

// mergeFrames 把增量框架的主体追加到基线框架之后。当合并后的框架会超过硬性上限
// 时返回空字符串；此时调用方只保留基线框架。
func mergeFrames(baselineText, deltaText string, limit int) string {
	if deltaText == "" {
		return baselineText
	}
	if baselineText == "" {
		if len(deltaText) <= limit {
			return deltaText
		}
		return ""
	}
	body := strings.TrimPrefix(deltaText, frameOpen+"\n")
	body = strings.TrimSuffix(body, "\n"+frameClose)
	if body == "" || body == deltaText {
		return ""
	}
	merged := strings.TrimSuffix(baselineText, "\n"+frameClose) + blockSeparator + body + "\n" + frameClose
	if len(merged) > limit {
		return ""
	}
	return merged
}

func (p *Projector) recordCaches(candidates []Candidate) {
	for _, candidate := range candidates {
		p.recordCandidateCache(candidate)
	}
}

func (p *Projector) recordCandidateCache(candidate Candidate) {
	if candidate.Scope == "" {
		return
	}
	if candidate.Digest == "" {
		ensureDigests(&candidate)
	}
	p.versions[candidate.Scope] = candidate.Version
	p.digests[candidate.Scope] = candidate.Digest
	p.trimmed[candidate.Scope] = candidate.TrimmedDigest
}

func statsForChanges(rendered Rendered, baseline bool, changes []Change) Stats {
	stats := Stats{
		Baseline:  baseline,
		Bytes:     len(rendered.Text),
		Changes:   len(changes),
		Omitted:   len(rendered.Omitted),
		Truncated: len(rendered.Truncated),
	}
	for _, change := range changes {
		if change.Action == ChangeActionRemove {
			continue
		}
		stats.Files++
		if isUserScope(change.Scope) {
			stats.UserGlobal++
		} else {
			stats.Project++
		}
	}
	return stats
}

func isUserScope(scope string) bool {
	directory, _, ok := splitScope(scope)
	if !ok {
		return false
	}
	return directory == scopeUserChagent || directory == scopeUserAgents
}

func changeItemFor(candidate Candidate, action string) ChangeItem {
	return ChangeItem{
		Action:     action,
		Scope:      candidate.Scope,
		Directory:  candidate.Directory,
		Path:       candidate.DisplayPath,
		Digest:     candidate.Digest,
		Content:    candidate.Content,
		UserGlobal: candidate.UserGlobal,
	}
}

func removeItemForSpec(spec candidateSpec, digest string) ChangeItem {
	return ChangeItem{
		Action:     ChangeActionRemove,
		Scope:      spec.scope,
		Directory:  spec.directory,
		Path:       spec.displayPath,
		Digest:     digest,
		UserGlobal: spec.userGlobal,
	}
}

// -----------------------------------------------------------------------------
// 探测与缓存基础设施
// -----------------------------------------------------------------------------

type probeStatus int

const (
	probeStatusOK probeStatus = iota
	probeStatusAbsent
	probeStatusUnavailable
	probeStatusTooLarge
)

type candidateSpec struct {
	absPath     string
	directory   string
	displayPath string
	name        string
	userGlobal  bool
	scope       string
}

type probeResult struct {
	spec       candidateSpec
	status     probeStatus
	candidate  Candidate
	hasContent bool
}

func (p *Projector) allowedRoots() []string {
	roots := []string{p.workspace, p.projectRoot}
	if p.cfg.UserGlobal {
		if p.cfg.ChagentHome != "" {
			roots = append(roots, p.cfg.ChagentHome)
		}
		if p.cfg.AgentsHome != "" {
			roots = append(roots, p.cfg.AgentsHome)
		}
	}
	return roots
}

func (p *Projector) readPolicy() ReadPolicy {
	return ReadPolicy{
		FollowSymlinks: p.cfg.FollowSymlinks,
		AllowedRoots:   p.allowedRoots(),
	}
}

// probe 对单个候选文件做 stat 检查，并在需要时进行有界读取。当 allowCache 为真、
// 且 stat 版本与此前读取的版本一致、其摘要仍与可见状态匹配时，会跳过内容读取。
func (p *Projector) probe(spec candidateSpec, visible VisibleState, allowCache bool) probeResult {
	if !p.cfg.FollowSymlinks {
		if info, err := os.Lstat(spec.absPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
			p.warn("skipping symlinked instruction file (follow_symlinks=false)", spec.absPath)
			return probeResult{status: probeStatusUnavailable}
		}
	}
	version, err := StatVersion(spec.absPath)
	if err != nil {
		return probeResult{status: probeStatusForError(err)}
	}
	if allowCache {
		cachedVersion, cachedOK := p.versions[spec.scope]
		visibleChange, visibleOK := visible.Scopes[spec.scope]
		cachedDigest := p.digests[spec.scope]
		cachedTrimmed := p.trimmed[spec.scope]
		if cachedOK && cachedVersion.Equal(version) && visibleOK && visibleChange.Digest == cachedDigest && cachedTrimmed != "" {
			candidate := candidateForSpec(spec)
			candidate.Version = version
			candidate.Digest = cachedDigest
			candidate.TrimmedDigest = cachedTrimmed
			return probeResult{spec: spec, status: probeStatusOK, candidate: candidate, hasContent: false}
		}
	}
	return p.loadContent(spec)
}

// loadContent 按配置的符号链接策略和有界大小读取候选内容。
func (p *Projector) loadContent(spec candidateSpec) probeResult {
	content, version, err := ReadBoundedWithPolicy(spec.absPath, int64(p.cfg.MaxSourceBytes), p.readPolicy())
	if err != nil {
		return probeResult{spec: spec, status: probeStatusForError(err)}
	}
	candidate := candidateForSpec(spec)
	candidate.Content = string(content)
	candidate.Version = version
	candidate.Digest = ContentDigestString(candidate.Content)
	candidate.TrimmedDigest = TrimmedDigestString(candidate.Content)
	return probeResult{spec: spec, status: probeStatusOK, candidate: candidate, hasContent: true}
}

func probeStatusForError(err error) probeStatus {
	switch {
	case IsAbsent(err):
		return probeStatusAbsent
	case errors.Is(err, ErrSourceTooLarge):
		return probeStatusTooLarge
	default:
		return probeStatusUnavailable
	}
}

func candidateForSpec(spec candidateSpec) Candidate {
	return Candidate{
		AbsolutePath: spec.absPath,
		DisplayPath:  spec.displayPath,
		Directory:    spec.directory,
		Name:         spec.name,
		Scope:        spec.scope,
		UserGlobal:   spec.userGlobal,
	}
}

func specForCandidate(candidate Candidate) candidateSpec {
	return candidateSpec{
		absPath:     candidate.AbsolutePath,
		directory:   candidate.Directory,
		displayPath: candidate.DisplayPath,
		name:        candidate.Name,
		userGlobal:  candidate.UserGlobal,
		scope:       candidate.Scope,
	}
}

func (p *Projector) warn(message, path string) {
	Warnf("%s: %s", message, path)
}

// -----------------------------------------------------------------------------
// 规格收集
// -----------------------------------------------------------------------------

// collectSpecs 构建有序的探测列表：先是基线发现链，其次是改动过的嵌套目录，
// 最后是此前可见但已不再属于基线链的嵌套作用域。顺序从宽泛到具体，
// 从而保持去重语义（“最早者胜出”）。
func (p *Projector) collectSpecs(visible VisibleState, touchedSpecs []candidateSpec, includeVisibleAndTouched bool) ([]candidateSpec, error) {
	discovered, err := DiscoverBaseline(p.cfg, p.workspace, p.excludePath)
	if err != nil {
		return nil, err
	}
	specs := make([]candidateSpec, 0, len(discovered)+len(touchedSpecs))
	seen := make(map[string]struct{}, len(discovered)+len(touchedSpecs))
	add := func(spec candidateSpec) {
		if spec.scope == "" || spec.absPath == "" {
			return
		}
		if _, ok := seen[spec.scope]; ok {
			return
		}
		seen[spec.scope] = struct{}{}
		specs = append(specs, spec)
	}
	for _, candidate := range discovered {
		add(candidateSpec{
			absPath:     candidate.AbsolutePath,
			directory:   candidate.Directory,
			displayPath: candidate.DisplayPath,
			name:        candidate.Name,
			userGlobal:  candidate.UserGlobal,
			scope:       candidate.Scope,
		})
	}
	if includeVisibleAndTouched {
		for _, spec := range touchedSpecs {
			add(spec)
		}
		for _, spec := range p.visibleOnlySpecs(visible, seen) {
			add(spec)
		}
	}
	return specs, nil
}

// touchedSpecs 返回文件工具改动过的目录中指令文件的候选规格。只考虑严格位于
// 工作区之下的目录；其余目录已由基线链覆盖。
func (p *Projector) touchedSpecs(touched []string) []candidateSpec {
	names := make([]string, 0, len(p.cfg.InstructionFiles)+len(p.cfg.LocalInstructionFiles))
	names = append(names, p.cfg.InstructionFiles...)
	names = append(names, p.cfg.LocalInstructionFiles...)
	unique := make(map[string]candidateSpec)
	for _, touch := range touched {
		if strings.TrimSpace(touch) == "" {
			continue
		}
		for _, dir := range DescendantDirs(p.workspace, touch) {
			relDir, err := filepath.Rel(p.projectRoot, dir)
			if err != nil {
				continue
			}
			for _, name := range names {
				spec := p.projectSpec(dir, relDir, name)
				unique[spec.scope] = spec
			}
		}
	}
	return sortedSpecs(unique)
}

// visibleOnlySpecs 为不属于当前发现集合的可见作用域重建规格
// （例如在早前轮次中改动过的嵌套目录）。
func (p *Projector) visibleOnlySpecs(visible VisibleState, seen map[string]struct{}) []candidateSpec {
	unique := make(map[string]candidateSpec)
	for scope, change := range visible.Scopes {
		if _, ok := seen[scope]; ok {
			continue
		}
		spec, ok := p.specFromVisible(change)
		if !ok {
			continue
		}
		unique[scope] = spec
	}
	return sortedSpecs(unique)
}

func (p *Projector) specFromVisible(change Change) (candidateSpec, bool) {
	directory, name, ok := splitScope(change.Scope)
	if !ok || name == "" {
		return candidateSpec{}, false
	}
	switch directory {
	case scopeUserChagent:
		if p.cfg.ChagentHome == "" {
			return candidateSpec{}, false
		}
		return candidateSpec{
			absPath:     filepath.Join(cleanAbs(p.cfg.ChagentHome), name),
			directory:   directory,
			displayPath: change.Path,
			name:        name,
			userGlobal:  true,
			scope:       change.Scope,
		}, true
	case scopeUserAgents:
		if p.cfg.AgentsHome == "" {
			return candidateSpec{}, false
		}
		return candidateSpec{
			absPath:     filepath.Join(cleanAbs(p.cfg.AgentsHome), name),
			directory:   directory,
			displayPath: change.Path,
			name:        name,
			userGlobal:  true,
			scope:       change.Scope,
		}, true
	default:
		absDir := p.projectRoot
		relative := directory
		if strings.HasPrefix(relative, "./") {
			relative = relative[2:]
		}
		if relative != "." && relative != "" {
			absDir = filepath.Join(p.projectRoot, filepath.FromSlash(relative))
		}
		return candidateSpec{
			absPath:     filepath.Join(absDir, name),
			directory:   directory,
			displayPath: change.Path,
			name:        name,
			scope:       change.Scope,
		}, true
	}
}

func (p *Projector) projectSpec(absDir, relDir, name string) candidateSpec {
	directory := projectDirectoryKey(relDir)
	return candidateSpec{
		absPath:     filepath.Join(absDir, name),
		directory:   directory,
		displayPath: projectDisplayPath(relDir, name),
		name:        name,
		scope:       scopeKey(directory, name),
	}
}

func sortedSpecs(unique map[string]candidateSpec) []candidateSpec {
	specs := make([]candidateSpec, 0, len(unique))
	for _, spec := range unique {
		specs = append(specs, spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specSortKey(specs[i]) < specSortKey(specs[j]) })
	return specs
}

func candidateGroupForSpec(spec candidateSpec) string {
	if spec.userGlobal {
		return "user-global"
	}
	return spec.directory
}

func specSortKey(spec candidateSpec) string {
	if spec.userGlobal {
		return "0" + spec.scope
	}
	depth := 0
	if spec.directory != "." {
		depth = strings.Count(strings.Trim(spec.directory, "./"), "/") + 1
	}
	return fmt.Sprintf("1%03d%s", depth, spec.scope)
}
