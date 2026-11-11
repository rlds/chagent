package commands

import (
	"context"
	"strings"
)

func btwCommand() Definition {
	return Definition{
		Name:        "btw",
		Description: "在不改变会话历史的情况下提一个旁支问题",
		Usage:       "/btw <question>",
		Handler: func(ctx context.Context, req Request, rt *Runtime) error {
			const emptyAnswerMsg = "模型返回了空响应，可能是模型服务故障或触发了 token 上限。"

			if rt == nil || rt.AskSideQuestion == nil {
				return req.Reply(unavailableMsg)
			}

			question := sideQuestionText(req.Text)
			if question == "" {
				return req.Reply("用法：/btw <question>")
			}

			answer, err := rt.AskSideQuestion(ctx, question)
			if err != nil {
				return req.Reply(err.Error())
			}
			if strings.TrimSpace(answer) == "" {
				return req.Reply(emptyAnswerMsg)
			}

			return req.Reply(answer)
		},
	}
}

func sideQuestionText(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	parts := strings.Fields(input)
	if len(parts) < 2 {
		return ""
	}
	if !strings.HasPrefix(input, parts[0]) {
		return ""
	}
	return strings.TrimSpace(input[len(parts[0]):])
}
