package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/skills"
	"github.com/rlds/chagent/pkg/tokenizer"
	"github.com/rlds/chagent/pkg/utils"
)

type ContextBuilder struct {
	workspace          string
	skillsLoader       *skills.SkillsLoader
	memory             *MemoryStore
	splitOnMarker      bool
	runtimeContextMode string
	agentDiscovery     func(agentID string) []AgentDescriptor
	promptRegistry     *PromptRegistry
	policyPrompts      policyPromptContributor

	// skillCatalogMode 选择技能目录投递方式（T4.3）："context"
	// （默认；持久上下文消息 + `skill` 工具）或 "system"（遗留 system prompt 片段）。
	// 空值规范化为 "context"。
	skillCatalogMode   string
	skillCatalogSource *SkillCatalogSource

	// system prompt 缓存，避免每次调用都重新构建。
	// 修复 issue #607：整个上下文被反复重新处理的问题。
	// 当工作区源文件变化时缓存自动失效（mtime 检查）。
	systemPromptMutex  sync.RWMutex
	cachedSystemPrompt string
	cachedAt           time.Time // 缓存构建时在被跟踪路径中观测到的最新 mtime

	// existedAtCache 记录上次构建缓存时哪些源文件路径存在。这样
	// sourceFilesChanged 就能检测到新建的文件（缓存时不存在、现在存在）
	// 或删除的文件（缓存时存在、现在已消失）——两者都应触发缓存重建。
	existedAtCache map[string]bool

	// skillFilesAtCache 在构建缓存时快照技能树的文件集合与 mtime。
	// 这样能捕捉到可能不会更新顶层技能根目录 mtime 的嵌套文件
	// 创建/删除/mtime 变化。
	skillFilesAtCache map[string]time.Time
}

func (cb *ContextBuilder) WithToolDiscovery(useBM25, useRegex bool) *ContextBuilder {
	if useBM25 || useRegex {
		if err := cb.RegisterPromptContributor(toolDiscoveryPromptContributor{
			useBM25:  useBM25,
			useRegex: useRegex,
		}); err != nil {
			logger.WarnCF(
				"agent",
				"Failed to register tool discovery prompt contributor",
				map[string]any{
					"error": err.Error(),
				},
			)
		}
	}
	return cb
}

func (cb *ContextBuilder) WithSplitOnMarker(enabled bool) *ContextBuilder {
	cb.splitOnMarker = enabled
	return cb
}

// WithPolicyPrompts 启用 system prompt 中 DSH 风格的策略片段：
// autonomyEnabled 为真时输出 P-01/P-11（自主/完成契约），
// workspaceContextEnabled 为真时输出 P-02（工作区指令策略）。
// 两个开关默认 false；都关闭时不渲染任何策略片段，
// system prompt 与引入策略前逐字节一致（普通聊天行为）。
func (cb *ContextBuilder) WithPolicyPrompts(autonomyEnabled, workspaceContextEnabled bool) *ContextBuilder {
	if cb == nil {
		return cb
	}
	next := policyPromptContributor{
		autonomyEnabled:         autonomyEnabled,
		workspaceContextEnabled: workspaceContextEnabled,
	}
	if cb.policyPrompts == next {
		return cb
	}
	cb.policyPrompts = next
	cb.InvalidateCache()
	return cb
}

// WithRuntimeContextMode 选择 "snapshot"（持久的 P-08 上下文消息）或
// "system"（遗留：把时间/会话渲染进 system prompt）。空值保留遗留行为，
// 以兼容测试和未配置该选项的调用方。
func (cb *ContextBuilder) WithRuntimeContextMode(mode string) *ContextBuilder {
	cb.runtimeContextMode = strings.ToLower(strings.TrimSpace(mode))
	return cb
}

func (cb *ContextBuilder) runtimeSnapshotMode() bool {
	return cb != nil && cb.runtimeContextMode == "snapshot"
}

func (cb *ContextBuilder) WithAgentDiscovery(
	agentID string,
	discover func(agentID string) []AgentDescriptor,
) *ContextBuilder {
	cb.agentDiscovery = discover
	if discover != nil {
		if err := cb.RegisterPromptContributor(agentDiscoveryPromptContributor{
			agentID:  agentID,
			discover: discover,
		}); err != nil {
			logger.WarnCF(
				"agent",
				"Failed to register agent discovery prompt contributor",
				map[string]any{
					"error": err.Error(),
				},
			)
		}
	}
	return cb
}

func getGlobalConfigDir() string {
	return config.GetHome()
}

func NewContextBuilder(workspace string) *ContextBuilder {
	// 内置技能：当前项目中的 skills 目录
	// 使用当前工作目录下的 skills/ 目录
	builtinSkillsDir := strings.TrimSpace(os.Getenv(config.EnvBuiltinSkills))
	if builtinSkillsDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			// os.Getwd 失败极为罕见；回退为空字符串，
			// 这样 filepath.Join 会生成相对的 "skills" 路径，
			// 保持原有的查找行为。
			wd = ""
		}
		builtinSkillsDir = filepath.Join(wd, "skills")
	}
	globalSkillsDir := filepath.Join(getGlobalConfigDir(), "skills")

	cb := &ContextBuilder{
		workspace:        workspace,
		skillsLoader:     skills.NewSkillsLoader(workspace, globalSkillsDir, builtinSkillsDir),
		memory:           NewMemoryStore(workspace),
		promptRegistry:   NewPromptRegistry(),
		skillCatalogMode: config.SkillsCatalogModeContext,
	}
	// 内置技能目录在默认 "context" 模式下是上下文来源；
	// WithSkillCatalogMode("system") 会把它变回 system 片段。
	cb.skillCatalogSource = NewSkillCatalogSource(cb.skillsLoader)
	if err := cb.RegisterContextSource(cb.skillCatalogSource); err != nil {
		logger.WarnCF("agent", "Failed to register builtin skill catalog context source", map[string]any{
			"error": err.Error(),
		})
	}
	return cb
}

func (cb *ContextBuilder) RegisterPromptSource(desc PromptSourceDescriptor) error {
	err := cb.promptRegistryOrDefault().RegisterSource(desc)
	if err == nil {
		cb.InvalidateCache()
	}
	return err
}

func (cb *ContextBuilder) RegisterPromptContributor(contributor PromptContributor) error {
	err := cb.promptRegistryOrDefault().RegisterContributor(contributor)
	if err == nil {
		cb.InvalidateCache()
	}
	return err
}

func (cb *ContextBuilder) promptRegistryOrDefault() *PromptRegistry {
	if cb.promptRegistry == nil {
		cb.promptRegistry = NewPromptRegistry()
	}
	return cb.promptRegistry
}

func (cb *ContextBuilder) getIdentity(includeToolUseRule bool, allowedTools []string) string {
	workspacePath, _ := filepath.Abs(filepath.Join(cb.workspace))
	version := config.FormatVersion()
	fileToolsAvailable := includeToolUseRule && taskFileToolsAvailable(allowedTools)
	rules := []string{}
	if includeToolUseRule {
		rules = append(rules, toolUseSystemPromptRule())
	}
	rules = append(
		rules,
		"**精确执行** - 绝对准确地理解用户意图与约束，严格按用户提交的任务与验收条件完成；不得擅自扩大或缩减任务范围。",
	)
	rules = append(rules, "**角色判定** - 身份类型/角色与任务所属行业/公司类型均为按需判定，非必填；仅当这些信息能直接帮助任务执行时才判定，无法判定或对执行无直接帮助时不判定，不得因此阻塞任务或虚构角色/行业。判定后补充该身份对应的职责、权限边界、专业标准与验收责任，对执行过程和结果进行约束；角色随任务改变，未判定时按任务目标、验收标准与通用工程规范执行。")
	if fileToolsAvailable {
		rules = append(rules, "**任务判定** - 先判定用户输入是任务还是问答；需要执行的任务记录到 tasks/TASKS.md，完成一项就更新一项。")
	}
	rules = append(
		rules,
		"**上下文摘要** - 摘要为近似内容，不保证完整或最新；用户明确指令优先。",
		"**语言与表达** - 思考与回复使用精确、精简的中文；只描述与任务直接相关的进度、结果、证据与必要说明；禁止语气词、形容词、寒暄、emoji 及与任务无关的表达。",
	)
	if includeToolUseRule {
		memoryRule := "**记忆** - 把值得记住的用户事实保存到工作区的 memory/MEMORY.md。"
		if fileToolsAvailable {
			memoryRule = "**记忆** - 只把跨会话仍然成立的核心内容保存到 memory/MEMORY.md；历史过程、临时状态与已恢复的异常不写入，已有堆积按记忆维护策略清理。"
		}
		rules = append(rules, memoryRule)
	}
	for i, rule := range rules {
		rules[i] = fmt.Sprintf("%d. %s", i+1, rule)
	}

	return fmt.Sprintf(
		`# chagent(%s)

你是 chagent，一个严格按用户意图完成任务的执行型 AI Agent。

## 工作区
%s

该绝对路径仅用于说明边界；日常文件操作一律使用相对工作区根目录的路径（如 memory/MEMORY.md、skills/{skill-name}/SKILL.md）。

## 规则

%s
`,
		version,
		workspacePath,
		strings.Join(rules, "\n"),
	)
}

func formatToolDiscoveryRule(useBM25, useRegex bool) string {
	if !useBM25 && !useRegex {
		return ""
	}

	var toolNames []string
	if useBM25 {
		toolNames = append(toolNames, `"tool_search_tool_bm25"`)
	}
	if useRegex {
		toolNames = append(toolNames, `"tool_search_tool_regex"`)
	}

	return fmt.Sprintf(
		`**工具发现** - 当前只显示少量工具。在因缺少工具而放弃前，先用 %s 搜索；发现的工具会在下一轮解锁。只要搜索仍有结果，就不要拒绝。`,
		strings.Join(toolNames, " 或 "),
	)
}

func (cb *ContextBuilder) BuildSystemPrompt() string {
	return renderPromptPartsLegacy(cb.BuildSystemPromptParts())
}

func (cb *ContextBuilder) BuildSystemPromptParts() []PromptPart {
	return cb.buildSystemPromptParts(systemPromptBuildOptions{
		IncludeSkillCatalog: true,
		IncludeToolUseRule:  true,
	})
}

type systemPromptBuildOptions struct {
	IncludeSkillCatalog bool
	IncludeToolUseRule  bool
	AllowedSkills       []string
	AllowedTools        []string
}

func (cb *ContextBuilder) buildSystemPromptParts(opts systemPromptBuildOptions) []PromptPart {
	stack := NewPromptStack(cb.promptRegistryOrDefault())
	add := func(part PromptPart) {
		if err := stack.Add(part); err != nil {
			logger.WarnCF("agent", "Skipping invalid prompt part", map[string]any{
				"id":     part.ID,
				"layer":  part.Layer,
				"slot":   part.Slot,
				"source": part.Source.ID,
				"error":  err.Error(),
			})
		}
	}

	// 核心身份部分
	add(PromptPart{
		ID:      "kernel.identity",
		Layer:   PromptLayerKernel,
		Slot:    PromptSlotIdentity,
		Source:  PromptSource{ID: PromptSourceKernel, Name: "identity"},
		Title:   "chagent identity",
		Content: cb.getIdentity(opts.IncludeToolUseRule, opts.AllowedTools),
		Stable:  true,
		Cache:   PromptCacheEphemeral,
	})

	// 路径/工具/验证策略：从 DeepSeek Harness 真实 system prompt 抽取，
	// 无条件注入，是所有 chagent 会话的默认行为契约。
	add(PromptPart{
		ID:      "kernel.path_tool_policy",
		Layer:   PromptLayerKernel,
		Slot:    PromptSlotPolicy,
		Source:  PromptSource{ID: PromptSourcePathToolPolicy, Name: string(PromptSourcePathToolPolicy)},
		Title:   "path tool policy",
		Content: pathToolPolicyPrompt(opts.AllowedTools, opts.IncludeToolUseRule),
		Stable:  true,
		Cache:   PromptCacheEphemeral,
	})

	// 角色判定与履职（P-23）：默认注入，不依赖文件工具；
	// 任务记录与技能/工具协同句按可用能力追加。
	add(PromptPart{
		ID:      "kernel.role_policy",
		Layer:   PromptLayerKernel,
		Slot:    PromptSlotPolicy,
		Source:  PromptSource{ID: PromptSourceRolePolicy, Name: string(PromptSourceRolePolicy)},
		Title:   "role policy",
		Content: rolePolicyPrompt(opts.AllowedTools, opts.IncludeToolUseRule),
		Stable:  true,
		Cache:   PromptCacheEphemeral,
	})

	// 任务判定/记录（P-21）与记忆维护（P-22）：默认行为契约。
	// 依赖文件工具读写 tasks/TASKS.md、tasks/TASK_LOG.md 与 memory/MEMORY.md；
	// 缺少文件工具或禁用工具规则时自动省略，避免要求模型写入无法访问的文件。
	if content := taskPolicyPrompt(opts.AllowedTools, opts.IncludeToolUseRule); content != "" {
		add(PromptPart{
			ID:      "kernel.task_policy",
			Layer:   PromptLayerKernel,
			Slot:    PromptSlotPolicy,
			Source:  PromptSource{ID: PromptSourceTaskPolicy, Name: string(PromptSourceTaskPolicy)},
			Title:   "task policy",
			Content: content,
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		})
	}
	if content := memoryPolicyPrompt(opts.AllowedTools, opts.IncludeToolUseRule); content != "" {
		add(PromptPart{
			ID:      "kernel.memory_policy",
			Layer:   PromptLayerKernel,
			Slot:    PromptSlotPolicy,
			Source:  PromptSource{ID: PromptSourceMemoryPolicy, Name: string(PromptSourceMemoryPolicy)},
			Title:   "memory policy",
			Content: content,
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		})
	}

	// 策略片段（DSH P-01/P-02/P-11）。它们按特性开关启用，
	// 关闭时不贡献任何内容——保持默认聊天 prompt 稳定。
	if cb != nil && cb.policyPrompts.enabled() {
		if contributed, err := cb.policyPrompts.ContributePrompt(
			context.Background(),
			PromptBuildRequest{},
		); err != nil {
			logger.WarnCF("agent", "Failed to build policy prompt parts", map[string]any{
				"error": err.Error(),
			})
		} else {
			for _, part := range contributed {
				add(part)
			}
		}
	}

	// 引导（bootstrap）文件
	bootstrapContent := cb.LoadBootstrapFiles()
	if bootstrapContent != "" {
		add(PromptPart{
			ID:      "instruction.workspace",
			Layer:   PromptLayerInstruction,
			Slot:    PromptSlotWorkspace,
			Source:  PromptSource{ID: PromptSourceWorkspace, Name: "workspace"},
			Title:   "workspace instructions",
			Content: bootstrapContent,
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		})
	}

	// 技能 - 默认 "context" 模式下目录由技能目录上下文来源发布为持久上下文；
	// 这里只为遗留 "system" 回退模式渲染进 prompt。
	skillsSummary := ""
	if opts.IncludeSkillCatalog && cb.systemSkillCatalogEnabled() {
		skillsSummary = cb.buildSkillsSummary(opts.AllowedSkills)
	}
	if skillsSummary != "" {
		skillIntro := "以下技能提供可复用任务指令。"
		readFileAllowed := promptAllowsTool(
			PromptBuildRequest{AllowedTools: opts.AllowedTools},
			"read_file",
		)
		if opts.IncludeToolUseRule && readFileAllowed {
			skillIntro += "使用前用 read_file 读取对应 SKILL.md；未读取不得执行技能指令。"
		}
		add(PromptPart{
			ID:     "capability.skill_catalog",
			Layer:  PromptLayerCapability,
			Slot:   PromptSlotSkillCatalog,
			Source: PromptSource{ID: PromptSourceSkillCatalog, Name: "skill:index"},
			Title:  "skill catalog",
			Content: fmt.Sprintf(`# 技能

%s

%s`, skillIntro, skillsSummary),
			Stable: true,
			Cache:  PromptCacheEphemeral,
		})
	}

	// 记忆上下文
	memoryContext := cb.memory.GetMemoryContext()
	if memoryContext != "" {
		add(PromptPart{
			ID:      "context.memory",
			Layer:   PromptLayerContext,
			Slot:    PromptSlotMemory,
			Source:  PromptSource{ID: PromptSourceMemory, Name: "memory:workspace"},
			Title:   "memory",
			Content: "# 记忆\n\n" + memoryContext,
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		})
	}

	// 多消息发送（若启用）
	if cb.splitOnMarker {
		add(PromptPart{
			ID:     "context.output_policy.split_on_marker",
			Layer:  PromptLayerContext,
			Slot:   PromptSlotOutput,
			Source: PromptSource{ID: PromptSourceOutputPolicy, Name: "split_on_marker"},
			Title:  "multi-message output policy",
			Content: `# 多消息输出
把回复拆分为短消息，各部分之间用 <|[SPLIT]|> 分隔；每个部分作为独立消息发送。不要发送一长段。示例：第 1 部分<|[SPLIT]|>第 2 部分`,
			Stable: true,
			Cache:  PromptCacheEphemeral,
		})
	}

	stack.Seal()
	return stack.Parts()
}

// BuildSystemPromptWithCache 在缓存可用且源文件未变化时返回缓存的
// system prompt，否则重新构建并缓存。
// 源文件变化通过 mtime 检查（廉价的 stat 调用）检测。
func (cb *ContextBuilder) BuildSystemPromptWithCache() string {
	// 先尝试读锁 —— 缓存有效时的快速路径
	cb.systemPromptMutex.RLock()
	if cb.cachedSystemPrompt != "" && !cb.sourceFilesChangedLocked() {
		result := cb.cachedSystemPrompt
		cb.systemPromptMutex.RUnlock()
		return result
	}
	cb.systemPromptMutex.RUnlock()

	// 获取写锁以进行构建
	cb.systemPromptMutex.Lock()
	defer cb.systemPromptMutex.Unlock()

	// 双重检查：在等待期间可能已有其他 goroutine 完成重建
	if cb.cachedSystemPrompt != "" && !cb.sourceFilesChangedLocked() {
		return cb.cachedSystemPrompt
	}

	// 在构建 prompt 之前先快照基线（存在性 + 最大 mtime）。
	// 这样 cachedAt 反映的是构建前的状态：如果某个文件在
	// BuildSystemPrompt 期间被修改，其新 mtime 会大于 baseline.maxMtime，
	// 于是下一次 sourceFilesChangedLocked 检查会正确地触发重建。
	// 另一种做法（构建后再取基线）则可能把陈旧内容连同过新的基线一起
	// 缓存，使陈旧状态无法被发现。
	baseline := cb.buildCacheBaseline()
	prompt := cb.BuildSystemPrompt()
	cb.cachedSystemPrompt = prompt
	cb.cachedAt = baseline.maxMtime
	cb.existedAtCache = baseline.existed
	cb.skillFilesAtCache = baseline.skillFiles

	logger.DebugCF("agent", "System prompt cached",
		map[string]any{
			"length": len(prompt),
		})

	return prompt
}

func (cb *ContextBuilder) buildSystemPromptForRequest(
	req PromptBuildRequest,
) (string, []providers.ContentBlock) {
	if req.SuppressDefaultSystemPrompt {
		return "", nil
	}

	useDefaultCache := !req.SuppressSkillContext &&
		!req.SuppressToolUseRule &&
		len(req.AllowedSkills) == 0 &&
		len(req.AllowedTools) == 0
	if useDefaultCache {
		staticPrompt := cb.BuildSystemPromptWithCache()
		return staticPrompt, []providers.ContentBlock{
			promptContentBlock(PromptPart{
				ID:      "kernel.static",
				Layer:   PromptLayerKernel,
				Slot:    PromptSlotIdentity,
				Source:  PromptSource{ID: PromptSourceKernel, Name: "static"},
				Content: staticPrompt,
			}, &providers.CacheControl{Type: "ephemeral"}),
		}
	}

	parts := cb.buildSystemPromptParts(systemPromptBuildOptions{
		IncludeSkillCatalog: !req.SuppressSkillContext,
		IncludeToolUseRule:  !req.SuppressToolUseRule,
		AllowedSkills:       req.AllowedSkills,
		AllowedTools:        req.AllowedTools,
	})
	staticPrompt := renderPromptPartsLegacy(parts)
	blocks := make([]providers.ContentBlock, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part.Content) == "" {
			continue
		}
		blocks = append(blocks, promptContentBlock(part, cacheControlForPromptPart(part)))
	}
	return staticPrompt, blocks
}

func (cb *ContextBuilder) buildSkillsSummary(allowed []string) string {
	if cb.skillsLoader == nil {
		return ""
	}
	if len(allowed) == 0 {
		return cb.skillsLoader.BuildSkillsSummary()
	}
	allowedSet := cleanAllowedSet(allowed)
	if len(allowedSet) == 0 {
		return ""
	}

	var lines []string
	lines = append(lines, "<skills>")
	for _, s := range cb.skillsLoader.ListSkills() {
		if _, ok := allowedSet[strings.ToLower(strings.TrimSpace(s.Name))]; !ok {
			continue
		}
		// 每个技能一行，与 skills.BuildSkillsSummary 的格式保持一致。
		lines = append(lines, fmt.Sprintf(
			"  <skill name=\"%s\" path=\"%s\" source=\"%s\">%s</skill>",
			xmlEscapeForPrompt(s.Name),
			xmlEscapeForPrompt(s.Path),
			s.Source,
			xmlEscapeForPrompt(s.Description),
		))
	}
	if len(lines) == 1 {
		return ""
	}
	lines = append(lines, "</skills>")
	return strings.Join(lines, "\n")
}

func xmlEscapeForPrompt(s string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(s)
}

// EstimateSystemTokens 估算将要发送给 LLM 的完整 system 消息的 token 数，
// 复用 BuildMessages 的组装逻辑。
// 它包含：静态 prompt、动态上下文、激活的技能以及摘要，连同包装用的
// 前缀与分隔符。这样无需 BuildMessages 所需的全部按请求参数
// （media、channel、chatID、发送者等）。
func (cb *ContextBuilder) EstimateSystemTokens(summary string, activeSkills []string) int {
	staticPrompt := cb.BuildSystemPromptWithCache()

	// 动态上下文很小且随请求变化；这里使用一个有代表性的估算值。
	// 实际的 buildDynamicContext 会生成约 200-400 字符的时间/运行时/会话信息。
	const dynamicContextChars = 300

	totalChars := utf8.RuneCountInString(staticPrompt) + dynamicContextChars
	totalCJK := tokenizer.CJKRuneCount(staticPrompt)

	if skillsText := cb.buildActiveSkillsContext(activeSkills); skillsText != "" {
		totalChars += utf8.RuneCountInString(skillsText)
		totalCJK += tokenizer.CJKRuneCount(skillsText)
		totalChars += 7 // 分隔符 \n\n---\n\n
	}

	if contributedParts, err := cb.promptRegistryOrDefault().Collect(context.Background(), PromptBuildRequest{
		Summary:      summary,
		ActiveSkills: append([]string(nil), activeSkills...),
	}); err == nil {
		for _, part := range contributedParts {
			if strings.TrimSpace(part.Content) == "" {
				continue
			}
			totalChars += utf8.RuneCountInString(part.Content)
			totalCJK += tokenizer.CJKRuneCount(part.Content)
			totalChars += 7 // 分隔符
		}
	}

	if summary != "" {
		totalChars += utf8.RuneCountInString(contextSummaryPrefix) + utf8.RuneCountInString(summary)
		totalCJK += tokenizer.CJKRuneCount(contextSummaryPrefix) + tokenizer.CJKRuneCount(summary)
		totalChars += 7 // 分隔符
	}

	// tokenizer.EstimateMessageTokens 的 CJK 感知版本：ASCII 按 0.4
	// token/字符，CJK 按 0.75 token/字符（否则以中文为主的 prompt 会被
	// 低估，压缩触发得过晚）。
	ascii := totalChars - totalCJK
	if totalCJK > totalChars {
		totalCJK = totalChars
		ascii = 0
	}
	return ascii*2/5 + totalCJK*3/4
}

// InvalidateCache 清除缓存的 system prompt。
// 通常不需要，因为缓存会通过 mtime 检查自动失效，
// 但对测试或显式重新加载命令很有用。
func (cb *ContextBuilder) InvalidateCache() {
	cb.systemPromptMutex.Lock()
	defer cb.systemPromptMutex.Unlock()

	cb.cachedSystemPrompt = ""
	cb.cachedAt = time.Time{}
	cb.existedAtCache = nil
	cb.skillFilesAtCache = nil

	logger.DebugCF("agent", "System prompt cache invalidated", nil)
}

// sourcePaths 返回为缓存失效而跟踪的非技能工作区源文件
// （引导文件 + 记忆）。技能根目录单独处理，
// 因为它们既需要目录级检查，也需要递归的文件级检查。
func (cb *ContextBuilder) sourcePaths() []string {
	agentDefinition := cb.LoadAgentDefinition()
	paths := agentDefinition.trackedPaths(cb.workspace)
	paths = append(paths, filepath.Join(cb.workspace, "memory", "MEMORY.md"))
	return uniquePaths(paths)
}

// skillRoots 返回所有可能影响 BuildSkillsSummary 输出的技能根目录
// （workspace/global/builtin）。
func (cb *ContextBuilder) skillRoots() []string {
	if cb.skillsLoader == nil {
		return []string{filepath.Join(cb.workspace, "skills")}
	}

	roots := cb.skillsLoader.SkillRoots()
	if len(roots) == 0 {
		return []string{filepath.Join(cb.workspace, "skills")}
	}
	return roots
}

// cacheBaseline 保存文件存在性快照以及所有被跟踪路径中观测到的
// 最新 mtime，用作缓存的参照点。
type cacheBaseline struct {
	existed    map[string]bool
	skillFiles map[string]time.Time
	maxMtime   time.Time
}

// buildCacheBaseline 记录当前存在哪些被跟踪路径，并计算所有被跟踪文件
// 以及技能目录内容中的最新 mtime。
// 在构建缓存时于写锁下调用。
func (cb *ContextBuilder) buildCacheBaseline() cacheBaseline {
	skillRoots := cb.skillRoots()

	// 所有跟踪存在性的路径：源文件 + 全部技能根目录。
	allPaths := append(cb.sourcePaths(), skillRoots...)

	existed := make(map[string]bool, len(allPaths))
	skillFiles := make(map[string]time.Time)
	var maxMtime time.Time

	for _, p := range allPaths {
		info, err := os.Stat(p)
		existed[p] = err == nil
		if err == nil && info.ModTime().After(maxMtime) {
			maxMtime = info.ModTime()
		}
	}

	// 递归遍历所有技能根目录，快照技能文件及其 mtime。
	// 使用 os.Stat（而非 d.Info），以与 sourceFilesChanged 的检查保持一致。
	for _, root := range skillRoots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr == nil && !d.IsDir() {
				if info, err := os.Stat(path); err == nil {
					skillFiles[path] = info.ModTime()
					if info.ModTime().After(maxMtime) {
						maxMtime = info.ModTime()
					}
				}
			}
			return nil
		})
	}

	// 如果还没有任何被跟踪文件存在（空工作区），maxMtime 为零。
	// 此时使用一个非常早的非零时间，以便：
	// 1. cachedAt.IsZero() 不会触发无休止的重建。
	// 2. 之后创建的任何真实文件其 mtime 都 > cachedAt，因此能被
	//    fileChangedSince 检测到（不同于 time.Now()，后者可能与某个
	//    mtime <= Now 的文件发生竞态）。
	if maxMtime.IsZero() {
		maxMtime = time.Unix(1, 0)
	}

	return cacheBaseline{existed: existed, skillFiles: skillFiles, maxMtime: maxMtime}
}

// sourceFilesChangedLocked 检查自上次构建缓存以来是否有工作区源文件
// 被修改、创建或删除。
//
// 重要：调用方必须至少持有 systemPromptMutex 的读锁。
// Go 的 sync.RWMutex 不可重入，因此本函数绝不能自行获取锁
// （否则在 BuildSystemPromptWithCache 中调用时会死锁，
// 那里已经持有 RLock 或 Lock）。
func (cb *ContextBuilder) sourceFilesChangedLocked() bool {
	if cb.cachedAt.IsZero() {
		return true
	}

	// 检查被跟踪的源文件（引导文件 + 记忆）。
	if slices.ContainsFunc(cb.sourcePaths(), cb.fileChangedSince) {
		return true
	}

	// --- 技能根目录（workspace/global/builtin） ---
	//
	// 对每个根目录：
	// 1. 创建/删除以及根目录 mtime 变化由 fileChangedSince 跟踪。
	// 2. 嵌套文件的创建/删除/mtime 变化由技能文件快照跟踪。
	for _, root := range cb.skillRoots() {
		if cb.fileChangedSince(root) {
			return true
		}
	}
	if skillFilesChangedSince(cb.skillRoots(), cb.skillFilesAtCache) {
		return true
	}

	return false
}

// fileChangedSince 在某个被跟踪的源文件自缓存构建以来被修改、
// 新建或删除时返回 true。
//
// 四种情况：
//   - 缓存时存在，现在存在 -> 检查 mtime
//   - 缓存时存在，现已消失 -> 已变化（删除）
//   - 缓存时不存在，现在存在 -> 已变化（创建）
//   - 缓存时不存在，现在也不存在 -> 无变化
func (cb *ContextBuilder) fileChangedSince(path string) bool {
	// 防御性处理：如果 existedAtCache 从未初始化，则视为已变化，
	// 让缓存重建，而不是静默地继续提供陈旧数据。
	if cb.existedAtCache == nil {
		return true
	}

	existedBefore := cb.existedAtCache[path]
	info, err := os.Stat(path)
	existsNow := err == nil

	if existedBefore != existsNow {
		return true // 文件被创建或删除
	}
	if !existsNow {
		return false // 之前不存在，现在也不存在
	}
	return info.ModTime().After(cb.cachedAt)
}

// errWalkStop 是一个哨兵错误，用于提前终止 filepath.WalkDir。
// 使用专门的错误（而不是 fs.SkipAll）能让提前退出的意图更明确，
// 并避免在回调的 err 参数非 nil 时返回 nil 而触发的 nilerr linter 警告。
var errWalkStop = errors.New("walk stop")

// skillFilesChangedSince 将当前的递归技能文件树与缓存时的快照进行对比。
// 任何创建/删除/mtime 的偏移都会使缓存失效。
func skillFilesChangedSince(skillRoots []string, filesAtCache map[string]time.Time) bool {
	// 防御性处理：如果快照从未初始化，则强制重建。
	if filesAtCache == nil {
		return true
	}

	// 检查缓存的文件仍然存在且 mtime 未变。
	for path, cachedMtime := range filesAtCache {
		info, err := os.Stat(path)
		if err != nil {
			// 此前跟踪的文件消失了（或变得不可访问）：
			// 无论哪种情况，缓存的技能摘要现在都可能已陈旧。
			return true
		}
		if !info.ModTime().Equal(cachedMtime) {
			return true
		}
	}

	// 检查任何技能根目录下都没有出现新文件。
	changed := false
	for _, root := range skillRoots {
		if strings.TrimSpace(root) == "" {
			continue
		}

		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				// 把意外的遍历错误视为已变化，以避免陈旧缓存。
				if !os.IsNotExist(walkErr) {
					changed = true
					return errWalkStop
				}
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if _, ok := filesAtCache[path]; !ok {
				changed = true
				return errWalkStop
			}
			return nil
		})

		if changed {
			return true
		}
		if err != nil && !errors.Is(err, errWalkStop) && !os.IsNotExist(err) {
			logger.DebugCF("agent", "skills walk error", map[string]any{"error": err.Error()})
			return true
		}
	}

	return false
}

func (cb *ContextBuilder) LoadBootstrapFiles() string {
	var sb strings.Builder

	agentDefinition := cb.LoadAgentDefinition()
	if agentDefinition.Agent != nil {
		label := string(agentDefinition.Source)
		if label == "" {
			label = relativeWorkspacePath(cb.workspace, agentDefinition.Agent.Path)
		}
		fmt.Fprintf(&sb, "## %s\n\n%s\n\n", label, agentDefinition.Agent.Body)
	}
	if agentDefinition.Soul != nil {
		fmt.Fprintf(
			&sb,
			"## %s\n\n%s\n\n",
			relativeWorkspacePath(cb.workspace, agentDefinition.Soul.Path),
			agentDefinition.Soul.Content,
		)
	}
	if agentDefinition.User != nil {
		fmt.Fprintf(&sb, "## %s\n\n%s\n\n", "USER.md", agentDefinition.User.Content)
	}

	if agentDefinition.Source != AgentDefinitionSourceAgent {
		filePath := filepath.Join(cb.workspace, "IDENTITY.md")
		if data, err := os.ReadFile(filePath); err == nil {
			fmt.Fprintf(&sb, "## %s\n\n%s\n\n", "IDENTITY.md", data)
		}
	}

	return sb.String()
}

// buildDynamicContext 返回一段简短的、包含按请求信息的动态上下文字符串。
// 它每次请求都会变化（时间、会话），因此不属于缓存 prompt 的一部分。
// LLM 侧的 KV 缓存复用由各 provider 适配器的原生机制实现：
//   - Anthropic：在静态 SystemParts 块上按块设置 cache_control（ephemeral）
//   - OpenAI / Codex：使用 prompt_cache_key 进行基于前缀的缓存
//
// 参见：https://docs.anthropic.com/en/docs/build-with-claude/prompt-caching
// 参见：https://platform.openai.com/docs/guides/prompt-caching
func formatCurrentSenderLine(senderID, senderDisplayName string) string {
	senderID = strings.TrimSpace(senderID)
	senderDisplayName = strings.TrimSpace(senderDisplayName)

	switch {
	case senderDisplayName != "" && senderID != "":
		return fmt.Sprintf("当前发送者：%s（ID：%s）", senderDisplayName, senderID)
	case senderDisplayName != "":
		return fmt.Sprintf("当前发送者：%s", senderDisplayName)
	case senderID != "":
		return fmt.Sprintf("当前发送者：%s", senderID)
	default:
		return ""
	}
}

func (cb *ContextBuilder) buildDynamicContext(
	channel, chatID, senderID, senderDisplayName string,
) string {
	now := time.Now().Format("2006-01-02 15:04 (Monday)")

	var sb strings.Builder
	fmt.Fprintf(&sb, "## 当前时间\n%s\n平台：%s/%s", now, runtime.GOOS, runtime.GOARCH)

	if channel != "" && chatID != "" {
		fmt.Fprintf(&sb, "\n\n## 当前会话\n通道：%s\nChat ID：%s", channel, chatID)
	}
	if senderLine := formatCurrentSenderLine(senderID, senderDisplayName); senderLine != "" {
		fmt.Fprintf(&sb, "\n\n## 当前发送者\n%s", senderLine)
	}

	return sb.String()
}

func (cb *ContextBuilder) BuildMessages(
	history []providers.Message,
	summary string,
	currentMessage string,
	media []string,
	channel, chatID, senderID, senderDisplayName string,
	activeSkills ...string,
) []providers.Message {
	return cb.BuildMessagesFromPrompt(PromptBuildRequest{
		History:           history,
		Summary:           summary,
		CurrentMessage:    currentMessage,
		Media:             media,
		Channel:           channel,
		ChatID:            chatID,
		SenderID:          senderID,
		SenderDisplayName: senderDisplayName,
		ActiveSkills:      append([]string(nil), activeSkills...),
	})
}

// contextSummaryPrefix 位于注入 system 消息的滚动对话摘要之前。
// 它必须与 EstimateSystemTokens 保持同步，后者在预算 token 时会计算其长度。
const contextSummaryPrefix = "CONTEXT_SUMMARY: 早期对话摘要（近似，不保证完整或最新）；用户明确指令优先。\n\n"

// BuildMessagesFromPrompt 组装一次 LLM 调用所需的完整消息列表：
// system prompt（静态 + 动态 + 摘要）、历史记录以及当前用户消息。
func (cb *ContextBuilder) BuildMessagesFromPrompt(req PromptBuildRequest) []providers.Message {
	messages := []providers.Message{}

	// 默认的静态部分（身份、引导文件、技能、记忆）缓存在本地，
	// 以避免每次调用都重复进行文件 I/O 和字符串构建（修复 issue #607）。
	// Profile 定制的静态 prompt 按需构建。
	// 动态部分（时间、会话、摘要）按请求追加，除非 profile
	// 抑制了 Chagent 系统上下文。
	// 所有内容都作为单条 system 消息发送，以保证 provider 兼容性：
	// - Anthropic 适配器提取 messages[0]（Role=="system"）并把其内容映射到
	//   Messages API 请求中顶层的 "system" 参数。单个连续的 system 块
	//   使这种提取变得简单直接。
	// - Codex 只把第一条 system 消息映射到其 instructions 字段。
	// - OpenAI 兼容实现按原样透传消息。
	staticPrompt, contentBlocks := cb.buildSystemPromptForRequest(req)

	// 组装单条 system 消息：静态（已缓存）+ 动态 + 可选摘要。
	// 把所有 system 内容放在一条消息中，确保每个 provider 适配器都能
	// 正确提取（Anthropic 适配器 -> 顶层 system 参数，
	// Codex -> instructions 字段）。
	//
	// SystemParts 以结构化块的形式携带相同内容，使具备缓存感知能力的
	// 适配器（Anthropic）能够按块设置 cache_control。
	// 静态块被标记为 "ephemeral"——其前缀哈希在不同请求间保持稳定，
	// 从而启用 LLM 侧的 KV 缓存复用。
	var stringParts []string
	if strings.TrimSpace(staticPrompt) != "" {
		stringParts = append(stringParts, staticPrompt)
	}

	promptParts := append([]PromptPart(nil), req.Overlays...)
	if !req.SuppressDefaultSystemPrompt && !req.SuppressSkillContext {
		activeSkills := append([]string(nil), req.ActiveSkills...)
		if len(req.AllowedSkills) > 0 {
			activeSkills = filterNamesByTurnProfile(activeSkills, req.AllowedSkills)
		}
		promptParts = append(promptParts, cb.buildActiveSkillsPromptParts(activeSkills)...)
	}
	if !req.SuppressDefaultSystemPrompt {
		if contributedParts, err := cb.promptRegistryOrDefault().Collect(context.Background(), req); err != nil {
			logger.WarnCF("agent", "Prompt contributor collection failed", map[string]any{
				"error": err.Error(),
			})
		} else {
			promptParts = append(promptParts, contributedParts...)
		}
	}

	if len(promptParts) > 0 {
		for _, overlay := range sortPromptParts(promptParts) {
			if strings.TrimSpace(overlay.Content) == "" {
				continue
			}
			if err := cb.promptRegistryOrDefault().ValidatePart(overlay); err != nil {
				logger.WarnCF("agent", "Skipping invalid prompt overlay", map[string]any{
					"id":     overlay.ID,
					"layer":  overlay.Layer,
					"slot":   overlay.Slot,
					"source": overlay.Source.ID,
					"error":  err.Error(),
				})
				continue
			}
			stringParts = append(stringParts, overlay.Content)
			contentBlocks = append(contentBlocks, promptContentBlock(overlay, nil))
		}
	}

	dynamicChars := 0
	if !req.SuppressDefaultSystemPrompt {
		// 快照模式把这些会变化的事实移入一条持久上下文消息（P-08），
		// 使 system prompt 前缀保持缓存稳定。
		dynamicCtx := ""
		if !cb.runtimeSnapshotMode() {
			dynamicCtx = cb.buildDynamicContext(
				req.Channel,
				req.ChatID,
				req.SenderID,
				req.SenderDisplayName,
			)
		}
		dynamicChars = len(dynamicCtx)
		runtimePart := PromptPart{
			ID:      "context.runtime",
			Layer:   PromptLayerContext,
			Slot:    PromptSlotRuntime,
			Source:  PromptSource{ID: PromptSourceRuntime, Name: "runtime"},
			Title:   "runtime context",
			Content: dynamicCtx,
			Stable:  false,
			Cache:   PromptCacheNone,
		}
		if dynamicCtx != "" {
			stringParts = append(stringParts, dynamicCtx)
			contentBlocks = append(contentBlocks, promptContentBlock(runtimePart, nil))
		}

		if req.Summary != "" {
			summaryPart := PromptPart{
				ID:      "context.summary",
				Layer:   PromptLayerContext,
				Slot:    PromptSlotSummary,
				Source:  PromptSource{ID: PromptSourceSummary, Name: "context.summary"},
				Title:   "context summary",
				Content: contextSummaryPrefix + req.Summary,
				Stable:  false,
				Cache:   PromptCacheNone,
			}
			stringParts = append(stringParts, summaryPart.Content)
			contentBlocks = append(contentBlocks, promptContentBlock(summaryPart, nil))
		}
	}

	if len(stringParts) == 0 && req.ToolUseFallback {
		fallbackPart := PromptPart{
			ID:      "kernel.tool_use_fallback",
			Layer:   PromptLayerKernel,
			Slot:    PromptSlotIdentity,
			Source:  PromptSource{ID: PromptSourceKernel, Name: "tool_use_fallback"},
			Title:   "tool use fallback",
			Content: toolUseSystemPromptRule(),
			Stable:  true,
			Cache:   PromptCacheEphemeral,
		}
		stringParts = append(stringParts, fallbackPart.Content)
		contentBlocks = append(contentBlocks, promptContentBlock(fallbackPart, nil))
	}

	fullSystemPrompt := strings.Join(stringParts, "\n\n---\n\n")

	// 记录 system prompt 摘要以便调试（仅 debug 模式）。
	// 在锁保护下读取 cachedSystemPrompt，避免与并发的
	// InvalidateCache / BuildSystemPromptWithCache 写入产生数据竞争。
	cb.systemPromptMutex.RLock()
	isCached := cb.cachedSystemPrompt != ""
	cb.systemPromptMutex.RUnlock()

	logger.DebugCF("agent", "System prompt built",
		map[string]any{
			"static_chars":  len(staticPrompt),
			"dynamic_chars": dynamicChars,
			"total_chars":   len(fullSystemPrompt),
			"has_summary":   req.Summary != "",
			"overlays":      len(req.Overlays),
			"cached":        isCached,
		})

	// 记录 system prompt 的预览（避免记录过大的内容）
	preview := utils.Truncate(fullSystemPrompt, 500)
	logger.DebugCF("agent", "System prompt preview",
		map[string]any{
			"preview": preview,
		})

	history := sanitizeHistoryForProvider(req.History)

	// 单条 system 消息包含所有上下文 —— 兼容所有 provider。
	// SystemParts 让具备缓存感知能力的适配器按块设置 cache_control；
	// Content 是拼接后的回退内容，供不读取 SystemParts 的适配器使用。
	if strings.TrimSpace(fullSystemPrompt) != "" {
		messages = append(messages, providers.Message{
			Role:        "system",
			Content:     fullSystemPrompt,
			SystemParts: contentBlocks,
		})
	}

	// 添加对话历史
	messages = append(messages, history...)

	// 添加当前用户消息。仅含媒体的轮次也必须保留，
	// 这样即使用户没有附带文字，多模态 provider 也能收到上传的图片。
	if strings.TrimSpace(req.CurrentMessage) != "" || len(req.Media) > 0 {
		messages = append(messages, userPromptMessage(req.CurrentMessage, req.Media))
	}
	if len(messages) == 0 {
		messages = append(messages, userPromptMessage("", nil))
	}

	return messages
}

func sanitizeHistoryForProvider(history []providers.Message) []providers.Message {
	if len(history) == 0 {
		return history
	}

	sanitized := make([]providers.Message, 0, len(history))
	for _, msg := range history {
		switch msg.Role {
		case "system":
			// 从历史中丢弃 system 消息。BuildMessages 总是
			// 自己构造单条 system 消息（静态 + 动态 + 摘要）；
			// 额外的 system 消息会破坏只接受一条的 provider
			// （Anthropic、Codex）。
			logger.DebugCF("agent", "Dropping system message from history", map[string]any{})
			continue

		case "tool":
			if len(sanitized) == 0 {
				logger.DebugCF("agent", "Dropping orphaned leading tool message", map[string]any{})
				continue
			}
			// 向前回溯以找到最近的 assistant 消息，
			// 跳过任何在前的 tool 消息（多次工具调用的情况）。
			foundAssistant := false
			for i := len(sanitized) - 1; i >= 0; i-- {
				if sanitized[i].Role == "tool" {
					continue
				}
				if sanitized[i].Role == "assistant" && len(sanitized[i].ToolCalls) > 0 {
					foundAssistant = true
				}
				break
			}
			if !foundAssistant {
				logger.DebugCF("agent", "Dropping orphaned tool message", map[string]any{})
				continue
			}
			sanitized = append(sanitized, msg)

		case "assistant":
			if len(msg.ToolCalls) > 0 {
				if len(sanitized) == 0 {
					logger.DebugCF(
						"agent",
						"Dropping assistant tool-call turn at history start",
						map[string]any{},
					)
					continue
				}
				prev := sanitized[len(sanitized)-1]
				if prev.Role != "user" && prev.Role != "tool" {
					logger.DebugCF(
						"agent",
						"Dropping assistant tool-call turn with invalid predecessor",
						map[string]any{"prev_role": prev.Role},
					)
					continue
				}
			}
			sanitized = append(sanitized, msg)

		default:
			sanitized = append(sanitized, msg)
		}
	}

	// 第二遍：确保每条带 tool_calls 的 assistant 消息后面都有
	// 与之匹配的 tool 结果消息。严格 provider（如 DeepSeek）要求：
	// “带 'tool_calls' 的 assistant 消息后面必须跟着响应每个
	// 'tool_call_id' 的 tool 消息”。
	//
	// 去重仅作用于紧随单条 assistant 工具调用消息之后的连续工具结果块。
	// 有些 provider 会在不同的轮次中合法地复用调用 ID（例如 "call_0"），
	// 所以全局去重会错误地删除后续合法的工具结果，并留下
	// assistant(tool_calls) -> assistant 这样的序列。
	final := make([]providers.Message, 0, len(sanitized))
	for i := 0; i < len(sanitized); i++ {
		msg := sanitized[i]

		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			expected := make(map[string]bool, len(msg.ToolCalls))
			invalidToolCallID := false
			for _, tc := range msg.ToolCalls {
				if tc.ID == "" {
					invalidToolCallID = true
					continue
				}
				expected[tc.ID] = false
			}

			block := make([]providers.Message, 0, len(expected))
			seenInBlock := make(map[string]bool, len(expected))
			j := i + 1
			for ; j < len(sanitized); j++ {
				next := sanitized[j]
				if next.Role != "tool" {
					break
				}
				if next.ToolCallID == "" {
					logger.DebugCF(
						"agent",
						"Dropping tool result without tool_call_id",
						map[string]any{},
					)
					continue
				}
				if _, ok := expected[next.ToolCallID]; !ok {
					logger.DebugCF("agent", "Dropping unexpected tool result", map[string]any{
						"tool_call_id": next.ToolCallID,
					})
					continue
				}
				if seenInBlock[next.ToolCallID] {
					logger.DebugCF(
						"agent",
						"Dropping duplicate tool result in tool block",
						map[string]any{
							"tool_call_id": next.ToolCallID,
						},
					)
					continue
				}
				seenInBlock[next.ToolCallID] = true
				expected[next.ToolCallID] = true
				block = append(block, next)
			}

			allFound := !invalidToolCallID
			if invalidToolCallID {
				logger.DebugCF(
					"agent",
					"Dropping assistant message with empty tool_call_id",
					map[string]any{},
				)
			}
			for toolCallID, found := range expected {
				if !found {
					allFound = false
					logger.DebugCF(
						"agent",
						"Dropping assistant message with incomplete tool results",
						map[string]any{
							"missing_tool_call_id": toolCallID,
							"expected_count":       len(expected),
							"found_count":          len(block),
						},
					)
					break
				}
			}

			if !allFound {
				i = j - 1
				continue
			}

			final = append(final, msg)
			final = append(final, block...)
			i = j - 1
			continue
		}

		if msg.Role == "tool" {
			logger.DebugCF(
				"agent",
				"Dropping orphaned tool message after validation",
				map[string]any{
					"tool_call_id": msg.ToolCallID,
				},
			)
			continue
		}

		final = append(final, msg)
	}

	return final
}

func (cb *ContextBuilder) AddToolResult(
	messages []providers.Message,
	toolCallID, toolName, result string,
) []providers.Message {
	messages = append(messages, providers.Message{
		Role:       "tool",
		Content:    result,
		ToolCallID: toolCallID,
	})
	return messages
}

func (cb *ContextBuilder) AddAssistantMessage(
	messages []providers.Message,
	content string,
	toolCalls []map[string]any,
) []providers.Message {
	msg := providers.Message{
		Role:    "assistant",
		Content: content,
	}
	// 始终添加 assistant 消息，无论其是否包含工具调用
	messages = append(messages, msg)
	return messages
}

func (cb *ContextBuilder) buildActiveSkillsContext(skillNames []string) string {
	ordered := cb.ResolveActiveSkillsForContext(skillNames)
	if len(ordered) == 0 {
		return ""
	}

	content := cb.skillsLoader.LoadSkillsForContext(ordered)
	if strings.TrimSpace(content) == "" {
		return ""
	}

	return fmt.Sprintf(`# 激活的技能

必须遵循下列已激活技能指令。

%s`, content)
}

func (cb *ContextBuilder) ResolveActiveSkillsForContext(skillNames []string) []string {
	if cb.skillsLoader == nil || len(skillNames) == 0 {
		return nil
	}

	var ordered []string
	seen := make(map[string]struct{}, len(skillNames))
	for _, name := range skillNames {
		canonical, ok := cb.ResolveSkillName(name)
		if !ok {
			continue
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		ordered = append(ordered, canonical)
	}
	if len(ordered) == 0 {
		return nil
	}
	return ordered
}

func (cb *ContextBuilder) buildActiveSkillsPromptParts(skillNames []string) []PromptPart {
	skillsText := cb.buildActiveSkillsContext(skillNames)
	if strings.TrimSpace(skillsText) == "" {
		return nil
	}

	return []PromptPart{
		{
			ID:      "capability.active_skills",
			Layer:   PromptLayerCapability,
			Slot:    PromptSlotActiveSkill,
			Source:  PromptSource{ID: PromptSourceActiveSkills, Name: "skill:active"},
			Title:   "active skills",
			Content: skillsText,
			Stable:  false,
			Cache:   PromptCacheNone,
		},
	}
}

// SkillLoader 为渐进式披露的 skill 工具暴露底层的技能加载器。
// 未配置技能目录时该加载器为 nil。
func (cb *ContextBuilder) SkillLoader() *skills.SkillsLoader {
	if cb == nil {
		return nil
	}
	return cb.skillsLoader
}

func (cb *ContextBuilder) ListSkillNames() []string {
	if cb.skillsLoader == nil {
		return nil
	}

	allSkills := cb.skillsLoader.ListSkills()
	names := make([]string, 0, len(allSkills))
	for _, skill := range allSkills {
		names = append(names, skill.Name)
	}
	return names
}

func (cb *ContextBuilder) ResolveSkillName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || cb.skillsLoader == nil {
		return "", false
	}

	for _, skill := range cb.skillsLoader.ListSkills() {
		if strings.EqualFold(skill.Name, name) {
			return skill.Name, true
		}
	}

	return "", false
}

// GetSkillsInfo 返回已加载技能的信息。
func (cb *ContextBuilder) GetSkillsInfo() map[string]any {
	allSkills := cb.skillsLoader.ListSkills()
	skillNames := make([]string, 0, len(allSkills))
	for _, s := range allSkills {
		skillNames = append(skillNames, s.Name)
	}
	return map[string]any{
		"total":     len(allSkills),
		"available": len(allSkills),
		"names":     skillNames,
	}
}
