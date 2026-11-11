// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"strings"
	"unicode/utf8"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
)

// ToolResultPruneMarker 替换每个被移除的中间片段。
// 该文本模型可见，因此直白说明被丢弃的内容。
const ToolResultPruneMarker = "\n…[工具输出中段已裁剪]…\n"

// ToolResultPruneConfig 是确定性工具结果裁剪的字符预算策略，
// 对应 DSH 的 compaction-tool-result-pruner
// （packages/compaction/compaction-tool-result-pruner/src/config.ts）。
//
// 裁剪在 chagent 意义上无模型参与且可安全重放：它只改写将要发送给 provider 的
// 内存历史副本，绝不修改已持久化的会话历史。
type ToolResultPruneConfig struct {
	// ThresholdChars：工具结果文本超过该 Unicode 码点数时会被裁剪。
	// 零值回退到默认值。
	ThresholdChars int
	// HeadChars 是保留的最大前导 Unicode 码点数。
	HeadChars int
	// TailChars 是保留的最大尾部 Unicode 码点数。
	TailChars int
}

// DefaultToolResultPruneConfig 返回 DSH 对编码 agent 工具输出的低摩擦默认值：
// 8192/4096/1024 码点。
func DefaultToolResultPruneConfig() ToolResultPruneConfig {
	return ToolResultPruneConfig{
		ThresholdChars: 8192,
		HeadChars:      4096,
		TailChars:      1024,
	}
}

// normalizeToolResultPruneConfig 填充默认值，并拒绝“保留的头 + 标记 + 尾”
// 不会小于裁剪阈值的预算。DSH 在这种情况下会抛配置错误；
// 本纯函数回退到安全默认值，因此裁剪绝不会让 prompt 变大。
func normalizeToolResultPruneConfig(cfg ToolResultPruneConfig) ToolResultPruneConfig {
	defaults := DefaultToolResultPruneConfig()
	if cfg == (ToolResultPruneConfig{}) {
		return defaults
	}
	if cfg.ThresholdChars <= 0 {
		cfg.ThresholdChars = defaults.ThresholdChars
	}
	if cfg.HeadChars < 0 {
		cfg.HeadChars = 0
	}
	if cfg.TailChars < 0 {
		cfg.TailChars = 0
	}
	emitted := cfg.HeadChars + utf8.RuneCountInString(ToolResultPruneMarker) + cfg.TailChars
	if emitted > cfg.ThresholdChars {
		return defaults
	}
	return cfg
}

// PruneToolResults 返回历史的副本，其中每个超大的纯文本工具结果都被替换为
// 其头部、裁剪标记和尾部。
//
// 规则（等价于 DSH 的 pruneContent）：
//   - 只有 role=="tool" 且内容为纯文本的消息是候选；媒体、附件、SystemParts 和工具调用会让消息变为结构化，
//     保持原样；
//   - 只有内容按 Unicode 码点超过 ThresholdChars（> 阈值，不是 >=）的候选才会被裁剪；
//   - 文本按 rune 切分，因此保留边界绝不会切开 UTF-8 序列或代理对；
//   - 裁剪是幂等的：已带标记的内容会被跳过；
//   - 输入切片及其消息绝不修改。
//
// pruned 是被改写的消息数；savedChars 是被移除的 Unicode 码点总数。
func PruneToolResults(
	history []providers.Message,
	cfg ToolResultPruneConfig,
) (out []providers.Message, pruned int, savedChars int) {
	out = make([]providers.Message, len(history))
	copy(out, history)

	cfg = normalizeToolResultPruneConfig(cfg)
	markerRunes := utf8.RuneCountInString(ToolResultPruneMarker)

	for i, msg := range out {
		if !isPureTextToolResult(msg) {
			continue
		}
		if strings.Contains(msg.Content, ToolResultPruneMarker) {
			// 已裁剪过（或模型原样输出了标记）：保持不变，使重复裁剪过程成为空操作。
			continue
		}
		runes := []rune(msg.Content)
		if len(runes) <= cfg.ThresholdChars {
			continue
		}

		replacement := string(runes[:cfg.HeadChars]) +
			ToolResultPruneMarker +
			string(runes[len(runes)-cfg.TailChars:])

		savedChars += len(runes) - (cfg.HeadChars + markerRunes + cfg.TailChars)
		out[i].Content = replacement
		pruned++
	}

	return out, pruned, savedChars
}

// isPureTextToolResult 报告 msg 是否为整个负载就是纯文本 Content 字符串的工具结果。
func isPureTextToolResult(msg providers.Message) bool {
	if msg.Role != "tool" {
		return false
	}
	if len(msg.Media) > 0 || len(msg.Attachments) > 0 || len(msg.SystemParts) > 0 || len(msg.ToolCalls) > 0 {
		return false
	}
	return true
}

// SpillPruneMarkerPrefix 是工具结果被移除的中部持久化到 spill 文件时所用标记的稳定、模型可见前缀。
// 完整标记为 "\n…[工具输出中段已裁剪；完整输出：<path>]…\n"；
// 它在 ToolResultPruneMarker 基础上扩展了一个模型可读取或 grep 的定位符。
const SpillPruneMarkerPrefix = "…[工具输出中段已裁剪；完整输出："

// spillPruneMarker 为已溢出的工具结果渲染省略通知。
func spillPruneMarker(path string) string {
	return "\n" + SpillPruneMarkerPrefix + path + "]…\n"
}

// isPrunedToolResultContent 报告内容是否已携带任一裁剪标记，
// 使重复裁剪过程成为空操作。
func isPrunedToolResultContent(content string) bool {
	return strings.Contains(content, ToolResultPruneMarker) ||
		strings.Contains(content, SpillPruneMarkerPrefix)
}

// PruneToolResultsWithSpill 是 PruneToolResults 加一个 spill 存储：在丢弃工具结果中部之前，
// 先通过 spill.Save 持久化其完整原始文本，替换标记会指明存储文件，
// 使模型能读回或 grep 完整输出。
//
// 降级契约（与 spill-local 对齐）：如果溢出失败，消息完全保持 inline，pruned/spilled 都不推进——
// 绝不宣传不存在的文件。nil spill 存储回退到 PruneToolResults，即没有定位符的旧标记。
//
// spilled 统计完整文本被保存的消息数；pruned 统计被改写的消息数（这里总是等于 spilled，
// 分开保留供调用方/日志使用）；savedChars 统计从请求中移除的 Unicode 码点数。
func PruneToolResultsWithSpill(
	history []providers.Message,
	cfg ToolResultPruneConfig,
	spill *SpillStore,
	sessionKey string,
) (out []providers.Message, pruned int, savedChars int, spilled int) {
	if spill == nil {
		out, pruned, savedChars = PruneToolResults(history, cfg)
		return out, pruned, savedChars, 0
	}

	out = make([]providers.Message, len(history))
	copy(out, history)

	cfg = normalizeToolResultPruneConfig(cfg)

	for i, msg := range out {
		if !isPureTextToolResult(msg) {
			continue
		}
		if isPrunedToolResultContent(msg.Content) {
			continue
		}
		runes := []rune(msg.Content)
		if len(runes) <= cfg.ThresholdChars {
			continue
		}

		// spill 路径按内容寻址，因此其长度（以及标记长度）在写入前已知。
		// 如果定位符不会缩小 prompt（路径很长而内容只是略微超限），
		// 就保持结果 inline，不创建无引用的文件。
		label := "tool-" + msg.ToolCallID
		markerRunes := 0
		if candidate := spill.pathFor(sessionKey, label, msg.Content); candidate != "" {
			markerRunes = utf8.RuneCountInString(spillPruneMarker(candidate))
			if cfg.HeadChars+markerRunes+cfg.TailChars >= len(runes) {
				continue
			}
		}

		path, err := spill.Save(sessionKey, label, msg.Content)
		if err != nil {
			logger.WarnCF("agent", "Tool result spill failed; keeping full content inline", map[string]any{
				"session_key":   sessionKey,
				"tool_call_id":  msg.ToolCallID,
				"content_chars": len(runes),
				"error":         err.Error(),
			})
			continue
		}

		marker := spillPruneMarker(path)
		markerRunes = utf8.RuneCountInString(marker)
		if cfg.HeadChars+markerRunes+cfg.TailChars >= len(runes) {
			// 防御性检查：Save 使用的是上面估算过的路径，因此正常情况下不会触发；
			// 若触发也绝不让 prompt 变大。
			continue
		}

		replacement := string(runes[:cfg.HeadChars]) +
			marker +
			string(runes[len(runes)-cfg.TailChars:])

		savedChars += len(runes) - (cfg.HeadChars + markerRunes + cfg.TailChars)
		out[i].Content = replacement
		pruned++
		spilled++
	}

	return out, pruned, savedChars, spilled
}
