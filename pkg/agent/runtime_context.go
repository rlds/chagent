package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

// RuntimeContextKind 是 P-08 运行时快照的持久化上下文类型。
const RuntimeContextKind = protocoltypes.ContextKindRuntimeContext

// BuildRuntimeSnapshot 渲染中文的 P-08 运行时快照。只包含可能发生变化的事实，
// 系统提示词保持稳定不变。
func BuildRuntimeSnapshot(
	channel, chatID, senderID, senderDisplayName, workspace, projectRoot string,
	includeSender bool,
	now time.Time,
) string {
	var b strings.Builder
	b.WriteString("<runtime_context>\n")
	fmt.Fprintf(&b, "当前时间：%s（%s）\n", now.Format(time.RFC3339), now.Location())
	fmt.Fprintf(&b, "平台：%s/%s\n", runtime.GOOS, runtime.GOARCH)
	if workspace != "" {
		if projectRoot != "" && projectRoot != workspace {
			fmt.Fprintf(&b, "工作区：%q（项目根：%q）\n", workspace, projectRoot)
		} else {
			fmt.Fprintf(&b, "工作区：%q\n", workspace)
		}
	}
	if channel != "" || chatID != "" {
		fmt.Fprintf(&b, "会话：通道=%s，会话=%s\n", channel, chatID)
	}
	if includeSender {
		if line := formatCurrentSenderLine(senderID, senderDisplayName); line != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	b.WriteString("</runtime_context>")
	return b.String()
}

// RuntimeSnapshotDigest 返回非时间类事实的稳定摘要。时间被刻意排除在外，
// 这样驱动可以按其配置的间隔刷新，而不会把时钟变化视为语义变化。
func RuntimeSnapshotDigest(channel, chatID, senderID, senderDisplayName, workspace, projectRoot string, includeSender bool) string {
	parts := []string{channel, chatID, workspace, projectRoot, fmt.Sprintf("include_sender=%t", includeSender)}
	if includeSender {
		parts = append(parts, senderID, senderDisplayName)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}
