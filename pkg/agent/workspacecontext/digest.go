package workspacecontext

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// TrimmedDigest 返回去除首尾空白后内容的 sha256 十六进制摘要。
// 它是同一候选组内候选的去重键：
// 仅在首尾空白上不同的文件会被视为重复。
func TrimmedDigest(content []byte) string {
	return digestOfBytes(bytes.TrimSpace(content))
}

// TrimmedDigestString 是 TrimmedDigest 的字符串版本。
func TrimmedDigestString(content string) string {
	return TrimmedDigest([]byte(content))
}

// ContentDigest 返回原始内容的 sha256 十六进制摘要。它被用作
// 持久的 ContextChange.Digest：摘要变化意味着渲染出的
// 指令正文发生了变化。
func ContentDigest(content []byte) string {
	return digestOfBytes(content)
}

// ContentDigestString 是 ContentDigest 的字符串版本。
func ContentDigestString(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func digestOfBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ensureDigests 在 Candidate.Digest/TrimmedDigest 缺失时
// 根据 Content 填充它们。它从不重新读取文件系统，因此
// 对缓存或手工构造的候选调用是安全的。
func ensureDigests(c *Candidate) {
	if c.Digest == "" {
		c.Digest = ContentDigestString(c.Content)
	}
	if c.TrimmedDigest == "" {
		c.TrimmedDigest = TrimmedDigestString(c.Content)
	}
}

// trimForRender 移除末尾换行，使渲染出的 section 始终
// 恰好以渲染器添加的分隔符结尾。内容本身（用于摘要的
// 字节）不受影响。
func trimForRender(content string) string {
	return strings.TrimRight(content, "\r\n")
}
