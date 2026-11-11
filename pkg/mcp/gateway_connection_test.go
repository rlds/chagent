//go:build integration

package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/config"
)

// 本测试使用 chagent 的实际 MCP 客户端，精确复现 agent 连接 MCP Gateway 的行为。
// 运行方式：
//   go test -tags=integration ./pkg/mcp -run TestGatewayConnection -v -timeout 120s
//
// 测试结论（2026-07-14 验证）：
//   本地直连：HTTP/SSE 均成功（169 工具）
//   线上域名：HTTP 报 "session not found"，SSE 报 "404 Not Found"
//
// 根本原因：线上 ELB 负载均衡器将同一 MCP 会话的不同请求路由到不同后端实例，
// 而 MCP 协议是有状态的（session 存储在单个实例内存中），导致跨实例 session 丢失。
//   - Streamable HTTP：initialize（实例A）成功创建 session，notifications/initialized
//     被路由到实例B，找不到 session
//   - SSE：GET 建立连接（实例A），POST 到 endpoint 被路由到实例B，返回 404
//
// 解决方案：配置 ELB 会话粘性（Session Sticky）或使用单实例部署。

const (
	apiToken = "439e77b7c8b6892aee14c0c6dffca388"
)

// 用户提供的请求头（与 agent 配置一致）
func gatewayHeaders() map[string]string {
	return map[string]string{
		"A-agentname":  "weichat",
		"A-department": "",
		"A-email":      "diaryi@10.me",
		"A-userid":     "id-20260506164248-fl4jhSvM",
		"A-username":   "diaryi",
		"api-token":    apiToken,
	}
}

// connectAndListTools 连接 MCP 服务器并尝试列出工具，返回工具数量和错误。
func connectAndListTools(t *testing.T, name, transport, url string) (int, error) {
	t.Helper()

	cfg := config.MCPServerConfig{
		Enabled: true,
		Type:    transport,
		URL:     url,
		Headers: gatewayHeaders(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mgr := NewManager()
	defer mgr.Close()

	// 连接服务器
	if err := mgr.ConnectServer(ctx, name, cfg); err != nil {
		return 0, err
	}

	// 获取发现的工具
	tools := mgr.GetAllTools()[name]
	t.Logf("[%s] transport=%s url=%s 发现工具数: %d", name, transport, url, len(tools))

	// 打印前5个工具名
	for i, tool := range tools {
		if i >= 5 {
			t.Logf("[%s] ... 还有 %d 个工具", name, len(tools)-5)
			break
		}
		if tool != nil {
			t.Logf("[%s] 工具: %s", name, tool.Name)
		}
	}

	return len(tools), nil
}

// TestGatewayConnection_HttpLocal 测试本地 Streamable HTTP 连接
func TestGatewayConnection_HttpLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	count, err := connectAndListTools(t, "local-http", "http", "http://127.0.0.1:8081/api/v1/mcp")
	if err != nil {
		t.Fatalf("本地 HTTP 连接失败: %v", err)
	}
	if count == 0 {
		t.Fatal("本地 HTTP 连接成功但未发现工具")
	}
	t.Logf("✓ 本地 HTTP 连接成功，发现 %d 个工具", count)
}

// TestGatewayConnection_HttpRemote 测试线上 Streamable HTTP 连接
func TestGatewayConnection_HttpRemote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	count, err := connectAndListTools(t, "remote-http", "http", "https://mcagent.100.me/api/v1/mcp")
	if err != nil {
		t.Fatalf("线上 HTTP 连接失败: %v", err)
	}
	if count == 0 {
		t.Fatal("线上 HTTP 连接成功但未发现工具")
	}
	t.Logf("✓ 线上 HTTP 连接成功，发现 %d 个工具", count)
}

// TestGatewayConnection_SSELocal 测试本地 SSE 连接
func TestGatewayConnection_SSELocal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	count, err := connectAndListTools(t, "local-sse", "sse", "http://127.0.0.1:8081/api/v1/mcp/sse")
	if err != nil {
		t.Fatalf("本地 SSE 连接失败: %v", err)
	}
	if count == 0 {
		t.Fatal("本地 SSE 连接成功但未发现工具")
	}
	t.Logf("✓ 本地 SSE 连接成功，发现 %d 个工具", count)
}

// TestGatewayConnection_SSERemote 测试线上 SSE 连接
func TestGatewayConnection_SSERemote(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	count, err := connectAndListTools(t, "remote-sse", "sse", "https://mcagent.100.me/api/v1/mcp/sse")
	if err != nil {
		t.Fatalf("线上 SSE 连接失败: %v", err)
	}
	if count == 0 {
		t.Fatal("线上 SSE 连接成功但未发现工具")
	}
	t.Logf("✓ 线上 SSE 连接成功，发现 %d 个工具", count)
}

// TestGatewayConnection_Compare 对比本地和线上的连接行为
func TestGatewayConnection_Compare(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	tests := []struct {
		name      string
		transport string
		localURL  string
		remoteURL string
	}{
		{"HTTP_Streamable", "http", "http://127.0.0.1:8081/api/v1/mcp", "https://mcagent.100.me/api/v1/mcp"},
		{"SSE_Legacy", "sse", "http://127.0.0.1:8081/api/v1/mcp/sse", "https://mcagent.100.me/api/v1/mcp/sse"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Logf("=== 对比测试: %s ===", tt.name)

			// 本地连接
			localCount, localErr := connectAndListTools(t, "local-"+strings.ToLower(tt.name), tt.transport, tt.localURL)
			if localErr != nil {
				t.Errorf("本地 %s 连接失败: %v", tt.name, localErr)
			} else {
				t.Logf("本地 %s: 成功 (%d 工具)", tt.name, localCount)
			}

			// 线上连接
			remoteCount, remoteErr := connectAndListTools(t, "remote-"+strings.ToLower(tt.name), tt.transport, tt.remoteURL)
			if remoteErr != nil {
				t.Errorf("线上 %s 连接失败: %v", tt.name, remoteErr)
			} else {
				t.Logf("线上 %s: 成功 (%d 工具)", tt.name, remoteCount)
			}

			// 如果本地成功但线上失败，输出详细诊断
			if localErr == nil && remoteErr != nil {
				t.Logf("⚠ 问题确认: 本地正常但线上异常")
				t.Logf("⚠ 本地 URL: %s", tt.localURL)
				t.Logf("⚠ 线上 URL: %s", tt.remoteURL)
				t.Logf("⚠ 错误信息: %v", remoteErr)
			}

			// 如果都成功，对比工具数
			if localErr == nil && remoteErr == nil {
				if localCount != remoteCount {
					t.Logf("⚠ 工具数不一致: 本地 %d vs 线上 %d", localCount, remoteCount)
				} else {
					t.Logf("✓ 工具数一致: %d", localCount)
				}
			}
		})
	}
}

// TestGatewayConnection_CallTool 测试调用工具（如果连接成功）
func TestGatewayConnection_CallTool(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	targets := []struct {
		name      string
		transport string
		url       string
	}{
		{"local", "http", "http://127.0.0.1:8081/api/v1/mcp"},
		{"remote", "http", "https://mcagent.100.me/api/v1/mcp"},
	}

	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			cfg := config.MCPServerConfig{
				Enabled: true,
				Type:    target.transport,
				URL:     target.url,
				Headers: gatewayHeaders(),
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			mgr := NewManager()
			defer mgr.Close()

			if err := mgr.ConnectServer(ctx, target.name, cfg); err != nil {
				t.Fatalf("连接失败: %v", err)
			}

			tools := mgr.GetAllTools()[target.name]
			if len(tools) == 0 {
				t.Skip("无可用工具")
			}

			// 尝试调用第一个工具
			tool := tools[0]
			if tool == nil {
				t.Skip("工具为空")
			}

			t.Logf("尝试调用工具: %s", tool.Name)
			result, err := mgr.CallTool(ctx, target.name, tool.Name, map[string]any{})
			if err != nil {
				t.Errorf("调用工具 %s 失败: %v", tool.Name, err)
				return
			}

			text := joinTextContents(result)
			if len(text) > 200 {
				text = text[:200] + "..."
			}
			t.Logf("工具 %s 调用成功，返回: %s", tool.Name, text)
		})
	}
}
