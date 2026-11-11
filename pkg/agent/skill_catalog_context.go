// Chagent - 超轻量级个人 AI agent

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
	"github.com/rlds/chagent/pkg/skills"
)

// T4.3：技能的渐进披露。
//
// 默认 "context" 模式下，技能目录作为持久上下文消息（只含名称 + 描述）注入，
// 而不是 system prompt 片段；技能正文通过 `skill` 工具按需加载。
// "system" 模式则改为渲染遗留的 `# 技能` system 片段（回退）。
const (
	// SkillCatalogContextOrder 把技能目录放在任务状态/上下文压力之后、循环卫生提示之前，
	// 与 pre-step 表顺序一致（workspace 100 -> runtime 200 -> task state 300 -> pressure 350 ->
	// skill catalog 400 -> loop guard 500）。在上下文来源之间，该数值只用于一次
	// CollectContext 调用内的排序。
	SkillCatalogContextOrder = 400

	// DefaultSkillCatalogMaxBytes 限制渲染出的目录消息大小。该上限避免庞大的已安装技能树
	// 挤占工作区上下文与工具结果。
	DefaultSkillCatalogMaxBytes = 8 * 1024

	// SkillCatalogFormCatalog 标记完整目录列表消息。
	SkillCatalogFormCatalog = "catalog"
	// SkillCatalogFormRemoved 标记技能目录变空时发出的退役消息（此前的列表不再适用）。
	SkillCatalogFormRemoved = "removed"
	// SkillCatalogChangeScope 是该来源使用的 ContextChange scope。
	SkillCatalogChangeScope = "skill-catalog"
)

// SkillCatalogLister 是目录所需的 skills.SkillsLoader 子集。
// 把它定义为接口，便于测试与插件提供自己的目录。
type SkillCatalogLister interface {
	ListSkills() []skills.SkillInfo
}

// SkillCatalogSource 把已安装技能目录（名称 + 描述）发布为持久上下文，
// 并在目录变空时发出退役通知。
type SkillCatalogSource struct {
	loader   SkillCatalogLister
	mode     string
	maxBytes int
}

// NewSkillCatalogSource 创建内置技能目录上下文来源。
func NewSkillCatalogSource(loader SkillCatalogLister) *SkillCatalogSource {
	return &SkillCatalogSource{
		loader:   loader,
		mode:     config.SkillsCatalogModeContext,
		maxBytes: DefaultSkillCatalogMaxBytes,
	}
}

// ContextSource 实现 ContextSource。
func (s *SkillCatalogSource) ContextSource() PromptSourceDescriptor {
	return PromptSourceDescriptor{
		ID:              PromptSourceSkillCatalogContext,
		Owner:           "skills",
		Description:     "Durable primary-disclosure skill catalog (name + description)",
		Allowed:         []PromptPlacement{{Layer: PromptLayerContext, Slot: PromptSlotSkillCatalog}},
		StableByDefault: true,
	}
}

// SetMode 在 "context" 与 "system"（回退）投递方式之间切换。
func (s *SkillCatalogSource) SetMode(mode string) {
	if s == nil {
		return
	}
	s.mode = normalizeSkillCatalogMode(mode)
}

// Mode 返回生效的投递方式。
func (s *SkillCatalogSource) Mode() string {
	if s == nil {
		return config.SkillsCatalogModeContext
	}
	return normalizeSkillCatalogMode(s.mode)
}

// SetMaxBytes 覆盖渲染目录的字节上限（供测试和需要更紧预算的调用方使用）。
// 非正值恢复默认值。
func (s *SkillCatalogSource) SetMaxBytes(maxBytes int) {
	if s == nil {
		return
	}
	if maxBytes <= 0 {
		maxBytes = DefaultSkillCatalogMaxBytes
	}
	s.maxBytes = maxBytes
}

// ContributeContext 在成员/描述摘要自上次发布目录消息以来发生变化时输出目录，
// 并在此前发布过的目录变空时输出退役通知。
func (s *SkillCatalogSource) ContributeContext(
	_ context.Context,
	req PromptBuildRequest,
) ([]ContextContribution, error) {
	if s == nil || s.loader == nil {
		return nil, nil
	}
	// System 模式改为把目录渲染进 system prompt。
	if s.Mode() == config.SkillsCatalogModeSystem {
		return nil, nil
	}
	// 目录的存在是为了支撑 `skill` 工具；技能上下文或工具被抑制时，
	// 发布一个模型无法兑现的承诺是错误的。
	if req.SuppressDefaultSystemPrompt || req.SuppressSkillContext || req.SuppressToolUseRule {
		return nil, nil
	}
	if !promptAllowsTool(req, "skill") {
		return nil, nil
	}

	catalogSkills := filterSkillCatalogSkills(s.loader.ListSkills(), req.AllowedSkills)
	lastCatalog := lastSkillCatalogMeta(req.History)

	if len(catalogSkills) == 0 {
		// 没有安装任何技能：不发布。若此前发布过目录，则退役它，
		// 让模型不再把旧名称当作有效。
		if lastCatalog == nil || lastCatalog.Form == SkillCatalogFormRemoved {
			return nil, nil
		}
		return []ContextContribution{skillCatalogRemovedContribution()}, nil
	}

	digest := SkillCatalogDigest(catalogSkills)
	if lastCatalog != nil &&
		lastCatalog.Form != SkillCatalogFormRemoved &&
		lastCatalog.Digest == digest {
		// 成员与描述相同：绝不追加重复目录。
		return nil, nil
	}

	maxBytes := s.maxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultSkillCatalogMaxBytes
	}
	text, body, truncated := renderSkillCatalogText(catalogSkills, maxBytes)
	if strings.TrimSpace(text) == "" {
		// 仅包装本身就已超过上限；输出截断片段比什么都不输出更糟。
		return nil, nil
	}
	if truncated {
		logger.DebugCF("agent", "Skill catalog truncated to fit byte cap", map[string]any{
			"skills":    len(catalogSkills),
			"max_bytes": maxBytes,
		})
	}

	meta := protocoltypes.ContextMeta{
		Kind:     protocoltypes.ContextKindSkillCatalog,
		Form:     SkillCatalogFormCatalog,
		Digest:   digest,
		Sections: []protocoltypes.ContextSection{{Name: "skills", Text: body}},
		Changes: []protocoltypes.ContextChange{{
			Action: protocoltypes.ContextActionReplace,
			Scope:  SkillCatalogChangeScope,
			Digest: digest,
		}},
	}
	return []ContextContribution{{
		Text:     text,
		Meta:     meta,
		Required: true,
		Order:    SkillCatalogContextOrder,
	}}, nil
}

func skillCatalogRemovedContribution() ContextContribution {
	return ContextContribution{
		Text: `<system-reminder>
技能目录已清空：此前发布的技能名单不再有效，不要再引用旧名单中的技能名。如仍需技能，可先用 find_skills 搜索、再用 install_skill 安装，然后重试当前任务。
</system-reminder>`,
		Meta: protocoltypes.ContextMeta{
			Kind:   protocoltypes.ContextKindSkillCatalog,
			Form:   SkillCatalogFormRemoved,
			Digest: "",
			Changes: []protocoltypes.ContextChange{{
				Action: protocoltypes.ContextActionRemove,
				Scope:  SkillCatalogChangeScope,
			}},
		},
		Required: true,
		Order:    SkillCatalogContextOrder,
	}
}

// SkillCatalogDigest 返回稳定的目录摘要：对排序后的 "name\0description" 对做 sha256。
// 顺序、路径和来源被刻意排除，因此移动技能目录但不改变其可见摘要不会重新发布目录。
func SkillCatalogDigest(catalogSkills []skills.SkillInfo) string {
	normalized := make([]string, 0, len(catalogSkills))
	for _, skill := range catalogSkills {
		normalized = append(normalized, skill.Name+"\x00"+skill.Description)
	}
	slices.Sort(normalized)

	hasher := sha256.New()
	for _, entry := range normalized {
		hasher.Write([]byte(entry))
		hasher.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// filterSkillCatalogSkills 返回本次请求可见的技能。allowed 为空表示所有已安装技能可见；
// 否则名称会与 turn-profile 允许列表做大小写不敏感匹配。
func filterSkillCatalogSkills(all []skills.SkillInfo, allowed []string) []skills.SkillInfo {
	if len(all) == 0 {
		return nil
	}
	if len(allowed) == 0 {
		return append([]skills.SkillInfo(nil), all...)
	}
	allowedSet := cleanAllowedSet(allowed)
	if len(allowedSet) == 0 {
		return nil
	}
	filtered := make([]skills.SkillInfo, 0, len(all))
	for _, skill := range all {
		if _, ok := allowedSet[strings.ToLower(strings.TrimSpace(skill.Name))]; !ok {
			continue
		}
		filtered = append(filtered, skill)
	}
	return filtered
}

// lastSkillCatalogMeta 返回历史中最近的技能目录上下文消息，或 nil。
// 搜索从后往前，因此只有最新发布状态有意义。
func lastSkillCatalogMeta(history []providers.Message) *protocoltypes.ContextMeta {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if msg.Context == nil {
			continue
		}
		if msg.Context.Kind != protocoltypes.ContextKindSkillCatalog {
			continue
		}
		meta := *msg.Context
		return &meta
	}
	return nil
}

const (
	skillCatalogIntro = "技能是可复用的任务专用指令集。本次会话可用的技能如下："

	skillCatalogUsageHint = "用户点名技能或任务匹配技能描述时，先用 `skill` 工具加载完整指令（name 用目录中的准确技能名）。目录仅含摘要；加载前不得推断或执行技能指令。"

	skillCatalogTruncationNote = "（技能目录超过大小上限，仅显示前一部分；其余技能可用 find_skills 搜索。）"
)

var skillCatalogClosingTagRe = regexp.MustCompile(`(?i)</system-reminder>`)

// renderSkillCatalogLine 渲染一条目录项。名称与描述除空白规范化和安全转义外保持原样；
// 转义可防止描述提前关闭 reminder 块或开启围栏代码块。
func renderSkillCatalogLine(skill skills.SkillInfo) string {
	name := strings.Join(strings.Fields(skill.Name), " ")
	description := strings.Join(strings.Fields(skill.Description), " ")
	if strings.TrimSpace(description) == "" {
		return fmt.Sprintf("- `%s`", escapeSkillCatalogText(name))
	}
	return fmt.Sprintf("- `%s`：%s", escapeSkillCatalogText(name), escapeSkillCatalogText(description))
}

func escapeSkillCatalogText(text string) string {
	text = skillCatalogClosingTagRe.ReplaceAllString(text, `<\/system-reminder>`)
	// 字面量 ``` 会提前结束外层 Markdown 上下文，可能被误认为围栏块；
	// 转义围栏可让它保持惰性。
	text = strings.ReplaceAll(text, "```", "\\`\\`\\`")
	text = strings.ReplaceAll(text, "<available_skills>", "&lt;available_skills&gt;")
	text = strings.ReplaceAll(text, "</available_skills>", "&lt;/available_skills&gt;")
	text = strings.ReplaceAll(text, "<skill_content>", "&lt;skill_content&gt;")
	text = strings.ReplaceAll(text, "</skill_content>", "&lt;/skill_content&gt;")
	return text
}

func buildSkillCatalogText(lines []string, truncated bool) string {
	var b strings.Builder
	b.WriteString("<system-reminder>\n")
	b.WriteString(skillCatalogIntro)
	b.WriteString("\n\n<available_skills>\n")
	for _, line := range lines {
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if truncated {
		b.WriteString(skillCatalogTruncationNote)
		b.WriteByte('\n')
	}
	b.WriteString("</available_skills>\n\n")
	b.WriteString(skillCatalogUsageHint)
	b.WriteString("\n</system-reminder>")
	return b.String()
}

// renderSkillCatalogText 在 maxBytes 下渲染目录消息。条目按顺序加入直到将超上限；
// 随后消息会报告截断，而不是静默丢弃技能。返回的正文只是目录列表
// （用于 ContextMeta.Sections）。
func renderSkillCatalogText(catalogSkills []skills.SkillInfo, maxBytes int) (text, body string, truncated bool) {
	lines := make([]string, 0, len(catalogSkills))
	for _, skill := range catalogSkills {
		line := renderSkillCatalogLine(skill)
		if strings.TrimSpace(line) == "" {
			continue
		}
		candidate := append(append([]string(nil), lines...), line)
		if len(buildSkillCatalogText(candidate, false)) > maxBytes {
			truncated = true
			break
		}
		lines = candidate
	}

	if truncated {
		// 为截断提示留出空间，必要时丢弃尾部条目。
		for len(lines) > 0 && len(buildSkillCatalogText(lines, true)) > maxBytes {
			lines = lines[:len(lines)-1]
		}
		if len(buildSkillCatalogText(lines, true)) > maxBytes {
			truncated = false
		}
	}

	text = buildSkillCatalogText(lines, truncated)
	if len(text) > maxBytes {
		return "", "", true
	}

	body = strings.Join(lines, "\n")
	if truncated && body != "" {
		body += "\n" + skillCatalogTruncationNote
	}
	return text, body, truncated
}

// normalizeSkillCatalogMode 把未知/空值映射为默认 "context" 模式。
// 配置校验在加载时拒绝未知值；这里是运行时的防御性回退。
func normalizeSkillCatalogMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case config.SkillsCatalogModeSystem:
		return config.SkillsCatalogModeSystem
	default:
		return config.SkillsCatalogModeContext
	}
}

// WithSkillCatalogMode 选择技能目录投递方式："context"（默认，
// 持久上下文消息 + `skill` 工具）或 "system"（遗留 system prompt 片段，作为回退保留）。
// 未知值回退到 "context"。
func (cb *ContextBuilder) WithSkillCatalogMode(mode string) *ContextBuilder {
	if cb == nil {
		return cb
	}
	cb.skillCatalogMode = normalizeSkillCatalogMode(mode)
	if cb.skillCatalogSource != nil {
		cb.skillCatalogSource.SetMode(cb.skillCatalogMode)
	}
	cb.InvalidateCache()
	return cb
}

// SkillCatalogMode 返回生效的技能目录投递方式。
func (cb *ContextBuilder) SkillCatalogMode() string {
	if cb == nil {
		return config.SkillsCatalogModeContext
	}
	return normalizeSkillCatalogMode(cb.skillCatalogMode)
}

// systemSkillCatalogEnabled 报告该 builder 是否应渲染遗留的 system prompt 目录片段。
func (cb *ContextBuilder) systemSkillCatalogEnabled() bool {
	return cb.SkillCatalogMode() == config.SkillsCatalogModeSystem
}
