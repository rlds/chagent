package commands

import (
	"context"
	"fmt"
	"strings"
)

// agentsHandler 返回一个供 /show agents 与 /list agents 共用的处理器。
func agentsHandler() Handler {
	return func(_ context.Context, req Request, rt *Runtime) error {
		if rt == nil || rt.ListAgentIDs == nil {
			return req.Reply(unavailableMsg)
		}
		ids := rt.ListAgentIDs()
		if len(ids) == 0 {
			return req.Reply("没有已注册的 agent")
		}
		return req.Reply(fmt.Sprintf("已注册 agent：%s", strings.Join(ids, ", ")))
	}
}
