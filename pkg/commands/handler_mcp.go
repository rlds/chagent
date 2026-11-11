package commands

import (
	"context"
	"fmt"
	"strings"
)

func listMCPServersHandler() Handler {
	return func(ctx context.Context, req Request, rt *Runtime) error {
		if rt == nil || rt.ListMCPServers == nil {
			return req.Reply(unavailableMsg)
		}

		servers := rt.ListMCPServers(ctx)
		if len(servers) == 0 {
			return req.Reply("未配置 MCP server")
		}

		header := "已配置 MCP Server："
		if rt.Config != nil && !rt.Config.Tools.IsToolEnabled("mcp") {
			header = "已配置 MCP Server（集成已禁用）："
		}

		lines := make([]string, 0, len(servers)*5+1)
		lines = append(lines, header)
		for idx, server := range servers {
			if idx > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, fmt.Sprintf("- `%s`", server.Name))
			lines = append(lines, fmt.Sprintf("  已启用：%s", yesNo(server.Enabled)))
			lines = append(lines, fmt.Sprintf("  延迟加载：%s", yesNo(server.Deferred)))
			lines = append(lines, fmt.Sprintf("  已连接：%s", yesNo(server.Connected)))
			if server.Connected {
				lines = append(lines, fmt.Sprintf("  活跃工具数：%d", server.ToolCount))
				continue
			}
			lines = append(lines, "  活跃工具数：不可用")
		}

		return req.Reply(strings.Join(lines, "\n"))
	}
}

func showMCPToolsHandler() Handler {
	return func(ctx context.Context, req Request, rt *Runtime) error {
		if rt == nil || rt.ListMCPTools == nil {
			return req.Reply(unavailableMsg)
		}

		serverName := nthToken(req.Text, 2)
		if serverName == "" {
			return req.Reply("用法：/show mcp <server>")
		}

		tools, err := rt.ListMCPTools(ctx, serverName)
		if err != nil {
			return req.Reply(err.Error())
		}
		if len(tools) == 0 {
			return req.Reply(fmt.Sprintf("MCP server「%s」没有活跃工具", serverName))
		}

		lines := make([]string, 0, len(tools)*6+1)
		lines = append(lines, fmt.Sprintf("`%s` 的活跃 MCP 工具：", serverName))
		for idx, tool := range tools {
			if idx > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, fmt.Sprintf("- `%s`", tool.Name))
			lines = append(lines, fmt.Sprintf("  描述：%s", tool.Description))
			if len(tool.Parameters) == 0 {
				lines = append(lines, "  参数：无")
				continue
			}

			lines = append(lines, "  参数：")
			for _, param := range tool.Parameters {
				line := fmt.Sprintf("    - `%s`", param.Name)
				if param.Type != "" {
					line += fmt.Sprintf("（%s", param.Type)
					if param.Required {
						line += "，必填"
					}
					line += "）"
				} else if param.Required {
					line += "（必填）"
				}
				if param.Description != "" {
					line += "：" + param.Description
				}
				lines = append(lines, line)
			}
		}

		return req.Reply(strings.Join(lines, "\n"))
	}
}

func yesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}
