package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlds/chagent/pkg/config"
)

func fakeEchoTool(name string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: "fake echo tool",
		InputSchema: map[string]any{"type": "object"},
	}
}

func fakeEchoHandler(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
}

// TestManagerRefreshesToolsOnListChangedNotification covers MCP
// notifications/tools/list_changed: the manager refreshes the cached tool list
// and reports it through the configured callback (agent-side re-registration
// hook).
func TestManagerRefreshesToolsOnListChangedNotification(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-list-changed", Version: "test"}, nil)
	server.AddTool(fakeEchoTool("echo"), fakeEchoHandler)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("fake server Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	previousBuildTransport := buildTransportFunc
	buildTransportFunc = func(context.Context, string, config.MCPServerConfig) (mcp.Transport, error) {
		return clientTransport, nil
	}
	t.Cleanup(func() { buildTransportFunc = previousBuildTransport })

	changed := make(chan []*mcp.Tool, 1)
	mgr := NewManager(WithToolListChangedHandler(func(serverName string, tools []*mcp.Tool) {
		if serverName != "fake" {
			return
		}
		select {
		case changed <- tools:
		default:
		}
	}))
	t.Cleanup(func() { _ = mgr.Close() })

	mcpCfg := config.MCPConfig{
		ToolConfig: config.ToolConfig{Enabled: true},
		Servers: map[string]config.MCPServerConfig{
			"fake": {Enabled: true, Type: "stdio", Command: "fake-in-memory"},
		},
	}
	if err := mgr.LoadFromMCPConfig(ctx, mcpCfg, t.TempDir()); err != nil {
		t.Fatalf("LoadFromMCPConfig() error = %v", err)
	}
	if got := len(mgr.GetAllTools()["fake"]); got != 1 {
		t.Fatalf("initial cached tools = %d, want 1", got)
	}

	// Server adds a tool; the SDK sends notifications/tools/list_changed.
	server.AddTool(fakeEchoTool("echo2"), fakeEchoHandler)

	select {
	case tools := <-changed:
		names := make(map[string]struct{}, len(tools))
		for _, tool := range tools {
			if tool != nil {
				names[tool.Name] = struct{}{}
			}
		}
		if _, ok := names["echo2"]; !ok {
			t.Fatalf("callback tools = %v, want refreshed list containing echo2", names)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tools/list_changed callback")
	}

	// Manager cache must be updated too (not only the callback payload).
	tools := mgr.GetAllTools()["fake"]
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool != nil {
			names[tool.Name] = struct{}{}
		}
	}
	if _, ok := names["echo2"]; !ok {
		t.Fatalf("manager cached tools = %v, want it to contain echo2 after refresh", names)
	}
}
