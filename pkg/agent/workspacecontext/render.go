package workspacecontext

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 帧标记。本包负责生成完整帧：指令正文永远无法关闭帧，
// 因为每个正文都用 escapedFrameClose 做了转义。
const (
	frameOpen         = "<system-reminder>"
	frameClose        = "</system-reminder>"
	escapedFrameClose = `<\/system-reminder>`
	blockSeparator    = "\n\n"
)

// 模型可见的中文提示文本（prompt 规范 §4.10.3 - §4.10.7）。
const (
	baselineIntro        = "以下为工作区指令，必须遵循。更具体的指令优先；与 system、developer 或用户直接指令冲突时，以后者为准。"
	baselineReplaceIntro = "以下完整工作区指令 baseline 会替代此前所有工作区指令 baseline。" +
		baselineIntro
	emptyReplacementText = "当前没有生效的工作区指令。"
	compactBaselineIntro = "以下为工作区指令，必须遵循。"

	baselineSectionPrefix   = "来自以下文件的指令："
	additionalSectionPrefix = "来自以下文件的新增指令："
	updatedSectionPrefix    = "来自以下文件的指令已更新："
	removedSectionPrefix    = "以下文件的指令已移除："

	removedBody         = "此前从该文件加载的指令不再适用。"
	updatedBodySentence = "该文件内容已变更；以下内容替代此前从该文件加载的指令。"

	projectScopeTemplate     = "以下指令适用于 `%s` 下的工作；更具体指令优先；与 system、developer 或用户直接指令冲突时以后者为准。"
	projectRootScopeSentence = "以下指令适用于项目根目录下的工作；更具体指令优先；与 system、developer 或用户直接指令冲突时以后者为准。"
	userGlobalScopeSentence  = "以下用户级指令适用于所有工作；更具体指令优先；与 system、developer 或用户直接指令冲突时以后者为准。"
)

// ChangeItem 是一条待渲染的增量条目。Content 仅用于 set 与
// replace 动作；移除动作只携带展示路径。
type ChangeItem struct {
	Action     string
	Scope      string
	Directory  string // 内部的项目相对目录，或用户作用域名称
	Path       string // 模型可见的展示路径
	Digest     string
	Content    string
	UserGlobal bool
}

// escapeFrameBody 转义结束帧标记，使指令内容永远不会提前终止帧。
// 所有模型可见的正文与提示都必须经过此函数。
func escapeFrameBody(text string) string {
	return strings.ReplaceAll(text, frameClose, escapedFrameClose)
}

// truncateUTF8 把 s 裁剪到最多 maxBytes 字节，且不切开 UTF-8 字符。
func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}

// renderBlock 是帧内的一个区块。
type renderBlock struct {
	header    string
	prefix    string // 增量入口的作用域句子；基线为空
	body      string // 已转义的内容；移除动作为空
	display   string
	scope     string
	digest    string
	action    string
	candidate *Candidate
}

// text 渲染区块正文（不含区块分隔符）。
func (b renderBlock) text() string {
	parts := make([]string, 0, 3)
	parts = append(parts, b.header)
	if b.prefix != "" {
		parts = append(parts, b.prefix)
	}
	if b.body != "" {
		parts = append(parts, b.body)
	}
	return strings.Join(parts, blockSeparator)
}

// size 是渲染后区块的字节长度。
func (b renderBlock) size() int { return len(b.text()) }

// truncateBody 只裁剪内容部分，使标题与作用域句子保持可见，
// 然后报告是否发生裁剪以及裁剪了多少字节。
func (b renderBlock) truncateBody(maxTotal int) (renderBlock, bool) {
	if b.body == "" {
		return b, false
	}
	overhead := len(b.header)
	if b.prefix != "" {
		overhead += len(blockSeparator) + len(b.prefix)
	}
	available := maxTotal - overhead - len(blockSeparator)
	if available <= 0 {
		return b, false
	}
	truncatedBody := truncateUTF8(b.body, available)
	if truncatedBody == b.body {
		return b, false
	}
	b.body = truncatedBody
	return b, true
}

func blocksText(blocks []renderBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.text())
	}
	return strings.Join(parts, blockSeparator)
}

func blocksSize(blocks []renderBlock) int {
	if len(blocks) == 0 {
		return 0
	}
	total := 0
	for _, block := range blocks {
		total += block.size()
	}
	return total + len(blockSeparator)*(len(blocks)-1)
}

// chainRender 是渲染一条受预算约束的链的结果。
type chainRender struct {
	text      string
	included  []renderBlock
	omitted   []renderBlock
	truncated []TruncatedFile
}

// renderChain 按每条链的字节预算进行渲染：
//  1. 整条链放得下 -> 全部保留；
//  2. 否则从最宽泛的文件开始（切片前端）整份丢弃，
//     直到剩余后缀放得下；
//  3. 如果最具体的文件仍放不下，则在 UTF-8 字符边界处截断其内容；
//  4. 如果连这样都放不下，则省略该文件。
//
// 被省略和被截断的文件会记录到预算提示中。
func renderChain(blocks []renderBlock, budget int) chainRender {
	if len(blocks) == 0 {
		return chainRender{}
	}
	if blocksSize(blocks) <= budget {
		return chainRender{text: blocksText(blocks), included: append([]renderBlock(nil), blocks...)}
	}

	result := chainRender{}
	remaining := append([]renderBlock(nil), blocks...)
	for len(remaining) > 1 && blocksSize(remaining) > budget {
		result.omitted = append(result.omitted, remaining[0])
		remaining = remaining[1:]
	}
	if len(remaining) > 1 {
		// 防御性处理：此处应当不可达，因为上面的循环以只剩一个元素
		// 或剩余后缀已放得下而结束。
		for len(remaining) > 1 {
			result.omitted = append(result.omitted, remaining[0])
			remaining = remaining[1:]
		}
	}
	if len(remaining) == 1 && blocksSize(remaining) > budget {
		last := remaining[0]
		truncatedBlock, truncated := last.truncateBody(budget)
		if truncated {
			result.truncated = append(result.truncated, TruncatedFile{
				Path:          last.display,
				OriginalBytes: len(last.body),
				KeptBytes:     len(truncatedBlock.body),
			})
			if truncatedBlock.body != "" {
				remaining = []renderBlock{truncatedBlock}
			} else {
				result.omitted = append(result.omitted, last)
				remaining = nil
			}
		} else {
			// 仅标题就放不下，或正文为空：省略该文件。
			result.omitted = append(result.omitted, last)
			remaining = nil
		}
	}
	result.included = remaining
	result.text = blocksText(remaining)
	return result
}

// budgetNotice 渲染 P-07 提示。没有省略或截断任何内容时为空。
func budgetNotice(maxBytes int, omitted []renderBlock, truncated []TruncatedFile) string {
	parts := make([]string, 0, 2)
	if len(omitted) > 0 {
		displays := make([]string, 0, len(omitted))
		for _, block := range omitted {
			displays = append(displays, block.display)
		}
		parts = append(parts, "已省略 "+strings.Join(displays, "、"))
	}
	for _, file := range truncated {
		parts = append(parts, fmt.Sprintf("已截断 %s，从 %d 字节截为 %d 字节", file.Path, file.OriginalBytes, file.KeptBytes))
	}
	if len(parts) == 0 {
		return ""
	}
	return escapeFrameBody(fmt.Sprintf("工作区指令预算 %d 字节：%s", maxBytes, strings.Join(parts, "；")))
}

// framePlan 描述一个待渲染的上下文帧。
type framePlan struct {
	intro         string
	compactIntro  string
	hasFiles      bool // 候选文件曾存在（即使全部被省略）
	userBlocks    []renderBlock
	projectBlocks []renderBlock
	emptyText     string // !hasFiles 时使用的完整帧（空替换）
}

// renderContextFrame 用两条独立的链预算渲染一个帧。合并后的帧上限为
// 2*maxBytes；超过该上限时回退为单一共享预算，最后回退为紧凑/仅标记输出。
func renderContextFrame(cfg Config, plan framePlan) Rendered {
	maxBytes := cfg.MaxBytes
	userChain := renderChain(plan.userBlocks, maxBytes)
	projectChain := renderChain(plan.projectBlocks, maxBytes)

	included := append(append([]renderBlock(nil), userChain.included...), projectChain.included...)
	omitted := append(append([]renderBlock(nil), userChain.omitted...), projectChain.omitted...)
	truncated := append(append([]TruncatedFile(nil), userChain.truncated...), projectChain.truncated...)

	if !plan.hasFiles {
		if plan.emptyText == "" {
			return Rendered{}
		}
		text := plan.emptyText
		if len(text) > 2*maxBytes {
			text = degradeFrame(plan, "", maxBytes)
		}
		return Rendered{Text: text}
	}

	limit := 2 * maxBytes
	notice := budgetNotice(maxBytes, omitted, truncated)
	text := assembleFrame(plan.intro, userChain.text, projectChain.text, notice)
	if len(text) > limit {
		// 回退：对完整的有序区块列表使用单一共享预算。预算会扣除
		// 帧开销（前言、提示、标记），使最终消息始终不超过 2*maxBytes。
		allBlocks := append(append([]renderBlock(nil), plan.userBlocks...), plan.projectBlocks...)
		for attempt := 0; attempt < 3; attempt++ {
			overhead := len(assembleFrame(plan.intro, "", "", notice))
			budget := limit - overhead - 1
			if budget < 1 {
				budget = 1
			}
			single := renderChain(allBlocks, budget)
			included = single.included
			omitted = single.omitted
			truncated = single.truncated
			notice = budgetNotice(maxBytes, omitted, truncated)
			text = assembleFrame(plan.intro, single.text, "", notice)
			if len(text) <= limit {
				break
			}
		}
		if len(text) > limit {
			// 最后手段：紧凑前言 / 仅标记的帧。不渲染任何内容，
			// 因此不记录任何变更已应用。
			included = nil
			text = degradeFrame(plan, notice, maxBytes)
		}
	}
	return Rendered{
		Text:      text,
		Changes:   changesFromBlocks(included),
		Included:  candidatesFromBlocks(included),
		Omitted:   displaysOfBlocks(omitted),
		Truncated: truncated,
	}
}

// assembleFrame 构建一个完整的 <system-reminder> 帧。
func assembleFrame(intro, userText, projectText, notice string) string {
	var builder strings.Builder
	builder.WriteString(frameOpen)
	builder.WriteString("\n")
	hasHeader := false
	if intro != "" {
		builder.WriteString(intro)
		builder.WriteString("\n")
		hasHeader = true
	}
	if notice != "" {
		if hasHeader {
			builder.WriteString("\n")
		}
		builder.WriteString(notice)
		builder.WriteString("\n")
		hasHeader = true
	}
	body := userText
	if projectText != "" {
		if body != "" {
			body += blockSeparator
		}
		body += projectText
	}
	if body != "" {
		if hasHeader {
			builder.WriteString("\n")
		}
		builder.WriteString(body)
		builder.WriteString("\n")
	}
	builder.WriteString(frameClose)
	return builder.String()
}

// degradeFrame 返回能放进 2*maxBytes 的最小可用帧：先是紧凑前言 + 提示，
// 再是紧凑前言，最后是仅标记。当连仅标记帧都超过上限时，不注入任何上下文
// （返回空字符串）。
func degradeFrame(plan framePlan, notice string, maxBytes int) string {
	limit := 2 * maxBytes
	candidates := []string{
		assembleFrame(plan.compactIntro, "", "", notice),
		assembleFrame(plan.compactIntro, "", "", ""),
		frameOpen + "\n" + frameClose,
	}
	for _, candidate := range candidates {
		if len(candidate) <= limit {
			return candidate
		}
	}
	return ""
}

func changesFromBlocks(blocks []renderBlock) []Change {
	if len(blocks) == 0 {
		return nil
	}
	changes := make([]Change, 0, len(blocks))
	for _, block := range blocks {
		changes = append(changes, Change{
			Action: block.action,
			Scope:  block.scope,
			Path:   block.display,
			Digest: block.digest,
		})
	}
	return changes
}

func candidatesFromBlocks(blocks []renderBlock) []Candidate {
	if len(blocks) == 0 {
		return nil
	}
	candidates := make([]Candidate, 0, len(blocks))
	for _, block := range blocks {
		if block.candidate != nil {
			candidates = append(candidates, *block.candidate)
		}
	}
	return candidates
}

func displaysOfBlocks(blocks []renderBlock) []string {
	if len(blocks) == 0 {
		return nil
	}
	displays := make([]string, 0, len(blocks))
	for _, block := range blocks {
		displays = append(displays, block.display)
	}
	return displays
}

// RenderBaseline 渲染完整的工作区指令基线。
//
// 空文件集不会产生消息，除非 replace 为 true：替换基线必须告知模型
// 此前的指令不再生效。预算造成的省略/截断会在帧内报告（P-07）。
func RenderBaseline(files []Candidate, cfg Config, replace bool) Rendered {
	cfg = cfg.normalize()
	if len(files) == 0 {
		if !replace {
			return Rendered{}
		}
		return renderContextFrame(cfg, framePlan{
			hasFiles:  false,
			emptyText: frameOpen + "\n" + baselineReplaceIntro + emptyReplacementText + "\n" + frameClose,
		})
	}

	intro := baselineIntro
	emptyText := ""
	if replace {
		intro = baselineReplaceIntro
		emptyText = frameOpen + "\n" + baselineReplaceIntro + emptyReplacementText + "\n" + frameClose
	}
	plan := framePlan{
		intro:        intro,
		compactIntro: compactBaselineIntro,
		hasFiles:     true,
		emptyText:    emptyText,
	}
	for _, candidate := range files {
		ensureDigests(&candidate)
		candidateCopy := candidate
		block := renderBlock{
			header:    baselineSectionPrefix + candidate.DisplayPath,
			body:      escapeFrameBody(trimForRender(candidate.Content)),
			display:   candidate.DisplayPath,
			scope:     candidate.Scope,
			digest:    candidate.Digest,
			action:    ChangeActionSet,
			candidate: &candidateCopy,
		}
		if candidate.UserGlobal {
			plan.userBlocks = append(plan.userBlocks, block)
		} else {
			plan.projectBlocks = append(plan.projectBlocks, block)
		}
	}
	return renderContextFrame(cfg, plan)
}

// RenderChanges 渲染一个增量帧，包含新增/更新/移除三个分区。
// 每条链内部保持顺序；移除项不携带正文。
func RenderChanges(items []ChangeItem, cfg Config) Rendered {
	cfg = cfg.normalize()
	if len(items) == 0 {
		return Rendered{}
	}
	plan := framePlan{hasFiles: true}
	for _, item := range items {
		candidate := Candidate{
			DisplayPath:   item.Path,
			Directory:     item.Directory,
			Scope:         item.Scope,
			Content:       item.Content,
			Digest:        item.Digest,
			TrimmedDigest: TrimmedDigestString(item.Content),
			UserGlobal:    item.UserGlobal,
		}
		block := renderBlock{
			display:   item.Path,
			scope:     item.Scope,
			digest:    item.Digest,
			action:    item.Action,
			candidate: &candidate,
		}
		switch item.Action {
		case ChangeActionSet:
			block.header = additionalSectionPrefix + item.Path
			block.body = escapeFrameBody(trimForRender(item.Content))
			block.prefix = scopeSentence(item)
		case ChangeActionReplace:
			block.header = updatedSectionPrefix + item.Path
			block.body = escapeFrameBody(trimForRender(item.Content))
			block.prefix = updatedBodySentence
		case ChangeActionRemove:
			block.header = removedSectionPrefix + item.Path
			block.body = removedBody
		default:
			continue
		}
		if item.UserGlobal {
			plan.userBlocks = append(plan.userBlocks, block)
		} else {
			plan.projectBlocks = append(plan.projectBlocks, block)
		}
	}
	if len(plan.userBlocks) == 0 && len(plan.projectBlocks) == 0 {
		return Rendered{}
	}
	return renderContextFrame(cfg, plan)
}

func scopeSentence(item ChangeItem) string {
	if item.UserGlobal {
		return userGlobalScopeSentence
	}
	directory := projectDirectoryDisplay(item.Directory)
	if directory == "." {
		return projectRootScopeSentence
	}
	return fmt.Sprintf(projectScopeTemplate, directory)
}
