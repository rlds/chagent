// Chagent - 超轻量个人 AI 智能体

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// 重复工具调用守卫（移植自 DSH packages/guard/repeat-tool-reminder）。
//
// 守卫观察 agent 连续的工具调用。当同一工具以*相同*参数连续调用足够多次时，
// 它返回模型可见的提示：首次阈值给简短警告，后续阈值给详细报告
// （工具、连续次数、参数预览）。
//
// 守卫是提示性的：绝不阻塞调用、绝不改写参数、也绝不自行修改对话历史。
// 调用方决定把返回消息放在哪里（回合循环可以把它作为下一条用户输入追加）。
//
// 该类型不持有全局状态；单个守卫属于一个会话/回合，不得在 goroutine 之间共享。

// 默认重复守卫策略值，与 DSH 默认值对齐。
var (
	defaultLoopGuardThresholds = []int{3, 5, 8}
	defaultLoopGuardExcludes   = []string{"todo_write", "task_get", "task_update"}
)

// DefaultLoopGuardPreviewChars 限制详细提示中引用多少规范化参数 JSON。
// 检测始终使用完整字符串。
const DefaultLoopGuardPreviewChars = 500

// 链键中工具名与规范化参数之间的分隔符。
// 两部分都不可能出现 NUL 字节，因此不同调用不会冲突。
const loopGuardChainSeparator = "\x00"

// LoopGuardConfig 配置重复工具调用守卫。
//
// 零值语义：
//   - Thresholds：为空、含小于 2 的值或含重复值时回退到 [3, 5, 8]，而不是静默降级。
//   - Include：为空则跟踪所有工具；否则调用必须匹配至少一个 "*" 通配模式才会被跟踪。
//   - Exclude：nil 回退到默认排除项；非 nil 的空切片表示完全禁用排除。Exclude 始终优先于 Include。
//   - ArgumentsPreviewChars：<= 0 时回退到 DefaultLoopGuardPreviewChars。
type LoopGuardConfig struct {
	Enabled               bool
	Thresholds            []int
	Include               []string
	Exclude               []string
	ArgumentsPreviewChars int
}

// DefaultLoopGuardConfig 返回可用且启用的守卫策略：DSH 默认阈值、默认排除项与参数预览上限。
// 返回的切片是副本，调用方可以自由修改。
func DefaultLoopGuardConfig() LoopGuardConfig {
	return LoopGuardConfig{
		Enabled:               true,
		Thresholds:            append([]int(nil), defaultLoopGuardThresholds...),
		Include:               nil,
		Exclude:               append([]string(nil), defaultLoopGuardExcludes...),
		ArgumentsPreviewChars: DefaultLoopGuardPreviewChars,
	}
}

// normalized 校验并分离配置。非法策略会被修复为文档中的默认值，
// 而不是在调用时失败。
func (c LoopGuardConfig) normalized() LoopGuardConfig {
	out := c
	out.Thresholds = normalizeLoopGuardThresholds(c.Thresholds)
	out.Include = append([]string(nil), c.Include...)
	if c.Exclude == nil {
		out.Exclude = append([]string(nil), defaultLoopGuardExcludes...)
	} else {
		out.Exclude = append([]string(nil), c.Exclude...)
	}
	if out.ArgumentsPreviewChars <= 0 {
		out.ArgumentsPreviewChars = DefaultLoopGuardPreviewChars
	}
	return out
}

// normalizeLoopGuardThresholds 返回升序、去重的阈值。
// 任何非法输入（空列表、小于 2 的值或重复值）都会选择默认列表，
// 保证守卫不会静默停止触发或乱序触发。
func normalizeLoopGuardThresholds(in []int) []int {
	fallback := func() []int { return append([]int(nil), defaultLoopGuardThresholds...) }
	if len(in) == 0 {
		return fallback()
	}
	seen := make(map[int]struct{}, len(in))
	out := make([]int, 0, len(in))
	for _, value := range in {
		if value < 2 {
			return fallback()
		}
		if _, duplicate := seen[value]; duplicate {
			return fallback()
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Ints(out)
	return out
}

// repeatToolGuard 是单个会话的连续重复链：最近一次被跟踪调用的身份键及其当前连续次数。
type repeatToolGuard struct {
	cfg      LoopGuardConfig
	chainKey string
	count    int
}

// newRepeatToolGuard 从 cfg 构建守卫，并按 LoopGuardConfig 的零值语义修复非法策略值。
// 守卫从空链开始。
func newRepeatToolGuard(cfg LoopGuardConfig) *repeatToolGuard {
	return &repeatToolGuard{cfg: cfg.normalized()}
}

// Reset 清空连续重复链。调用方在新用户输入开始（或任何让上一次连续变得无关的事件）时调用，
// 使重复计数不会跨越无关工作。
func (g *repeatToolGuard) Reset() {
	if g == nil {
		return
	}
	g.chainKey = ""
	g.count = 0
}

// Observe 记录一次工具调用，并在调用恰好落在配置阈值上时返回要注入的提示；否则返回 nil。
//
// 被 Include/Exclude 排除的调用是透明的：它既不计入也不重置链。身份不同的被跟踪调用会把链重置为 1。
// 规范化参数 JSON 每次调用只计算一次；无法序列化的值会退化为空参数字符串而不是 panic。
func (g *repeatToolGuard) Observe(toolName string, args map[string]any) *providers.Message {
	if g == nil || !g.cfg.Enabled || !g.tracks(toolName) {
		return nil
	}
	canonical := canonicalLoopGuardArguments(args)
	key := toolName + loopGuardChainSeparator + canonical
	if key == g.chainKey && g.chainKey != "" {
		g.count++
	} else {
		g.chainKey = key
		g.count = 1
	}

	kind, hit := g.thresholdKind(g.count)
	if !hit {
		return nil
	}
	text := ""
	if kind == loopGuardReminderBrief {
		text = loopGuardBriefReminder
	} else {
		text = loopGuardDetailedReminder(toolName, g.count, loopGuardPreview(canonical, g.cfg.ArgumentsPreviewChars))
	}
	return &providers.Message{
		Role:    "user",
		Content: text,
		Context: &protocoltypes.ContextMeta{
			Kind:   protocoltypes.ContextKindLoopGuard,
			Form:   protocoltypes.ContextFormReminder,
			Digest: loopGuardDigest(toolName, g.count, key),
		},
	}
}

// loopGuardReminderKind 区分首次阈值的温和措辞与后续阈值的详细报告。
type loopGuardReminderKind int

const (
	loopGuardReminderBrief loopGuardReminderKind = iota
	loopGuardReminderDetailed
)

// thresholdKind 报告连续次数选中了哪种提示（如果有）。只有恰好命中阈值才产生提示，
// 因此阈值为 [3,5,8] 时，连续 6 次调用会在 3 和 5 时警告，在 6 时保持沉默。
func (g *repeatToolGuard) thresholdKind(count int) (loopGuardReminderKind, bool) {
	thresholds := g.cfg.Thresholds
	if len(thresholds) == 0 || count != thresholds[0] {
		for _, threshold := range thresholds[1:] {
			if count == threshold {
				return loopGuardReminderDetailed, true
			}
		}
		return loopGuardReminderBrief, false
	}
	return loopGuardReminderBrief, true
}

// tracks 报告本次调用是否参与该链。Exclude 优先于 Include；空的 Include 跟踪所有工具。
func (g *repeatToolGuard) tracks(toolName string) bool {
	for _, pattern := range g.cfg.Exclude {
		if matchToolPattern(pattern, toolName) {
			return false
		}
	}
	if len(g.cfg.Include) == 0 {
		return true
	}
	for _, pattern := range g.cfg.Include {
		if matchToolPattern(pattern, toolName) {
			return true
		}
	}
	return false
}

// matchToolPattern 用锚定的 "*" 通配模式匹配工具名（其余字符按字面量处理）。
// 常见精确匹配场景下刻意做到零分配，且对畸形模式绝不 panic。
func matchToolPattern(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	rest := name[len(parts[0]):]
	last := parts[len(parts)-1]
	if last != "" {
		if !strings.HasSuffix(rest, last) {
			return false
		}
		rest = rest[:len(rest)-len(last)]
	}
	for _, middle := range parts[1 : len(parts)-1] {
		if middle == "" {
			continue
		}
		index := strings.Index(rest, middle)
		if index < 0 {
			return false
		}
		rest = rest[index+len(middle):]
	}
	return true
}

// canonicalLoopGuardArguments 把参数渲染成稳定的身份字符串。
// encoding/json 会在每一层对 map 键排序，因此仅构造顺序不同的两次调用会产生相同字符串。
// 序列化失败（func/chan/NaN 值）退化为 "" 而不是 panic。
func canonicalLoopGuardArguments(args map[string]any) string {
	if args == nil {
		return ""
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// loopGuardPreview 对规范化参数做头部截断，最多保留 limit 个 rune，并报告省略了多少 rune。
// 检测始终使用未截断字符串；只有模型可见的引用被限制长度。
func loopGuardPreview(canonical string, limit int) string {
	if limit <= 0 {
		limit = DefaultLoopGuardPreviewChars
	}
	total := utf8.RuneCountInString(canonical)
	if total <= limit {
		return canonical
	}
	byteIndex := len(canonical)
	seen := 0
	for index := range canonical {
		if seen == limit {
			byteIndex = index
			break
		}
		seen++
	}
	return canonical[:byteIndex] + fmt.Sprintf("…（另有 %d 字符）", total-limit)
}

// loopGuardDigest 从工具名、连续次数和链身份推导稳定摘要。
// 它对 replay/压缩是确定性的，且足够短以保持持久元数据很小。
func loopGuardDigest(toolName string, count int, chainKey string) string {
	sum := sha256.Sum256([]byte(toolName + loopGuardChainSeparator + strconv.Itoa(count) + loopGuardChainSeparator + chainKey))
	return "loop-guard:" + hex.EncodeToString(sum[:8])
}

// loopGuardBriefReminder 是首次阈值的警告。它故意不点名具体工具：
// 模型自己的上下文中已有该调用，通用指令能让提示保持简短。
const loopGuardBriefReminder = "检测到重复工具调用：同一工具以相同参数反复调用。" +
	"分析上次调用结果：任务未完成则更换方法或参数，禁止相同参数重复调用；信息充分时立即结束任务。"

// loopGuardDetailedReminder 是后续阈值的报告。它点名工具、连续次数和有界参数预览，
// 使模型能识别重复调用，同时提示本身不会无限增长。
func loopGuardDetailedReminder(toolName string, count int, preview string) string {
	return fmt.Sprintf(
		"检测到重复工具调用且无进展：\n- 工具：%s\n- 连续调用次数：%d\n- 参数：%s\n"+
			"禁止以相同参数重复调用该工具；检查上次结果后更换动作或参数；证据充分时立即结束任务。",
		toolName, count, preview,
	)
}
