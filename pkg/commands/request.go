package commands

import (
	"context"
	"strings"
)

type Handler func(ctx context.Context, req Request, rt *Runtime) error

type Request struct {
	Channel  string
	ChatID   string
	SenderID string
	Text     string
	Reply    func(text string) error
}

const unavailableMsg = "当前上下文不可用该命令。"

var commandPrefixes = []string{"/", "!"}

// parseCommandName 接受 "/name"、"!name" 以及 Telegram 的 "/name@bot"，
// 然后规范化为小写命令名。
func parseCommandName(input string) (string, bool) {
	token := nthToken(input, 0)
	if token == "" {
		return "", false
	}

	name, ok := trimCommandPrefix(token)
	if !ok {
		return "", false
	}
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}
	name = normalizeCommandName(name)
	if name == "" {
		return "", false
	}
	return name, true
}

// CommandName 返回输入中的规范化命令名（若存在）。
func CommandName(input string) (string, bool) {
	return parseCommandName(input)
}

func trimCommandPrefix(token string) (string, bool) {
	for _, prefix := range commandPrefixes {
		if strings.HasPrefix(token, prefix) {
			return strings.TrimPrefix(token, prefix), true
		}
	}
	return "", false
}

// HasCommandPrefix 在输入以可识别的命令前缀（例如 "/" 或 "!"）开头时
// 返回 true。
func HasCommandPrefix(input string) bool {
	token := nthToken(input, 0)
	if token == "" {
		return false
	}
	_, ok := trimCommandPrefix(token)
	return ok
}

// nthToken 返回按空白切分输入后从 0 开始计数的分词。
func nthToken(input string, n int) string {
	parts := strings.Fields(strings.TrimSpace(input))
	if n >= len(parts) {
		return ""
	}
	return parts[n]
}

func normalizeCommandName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
