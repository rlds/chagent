package mcpresources

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	picomcp "github.com/rlds/chagent/pkg/mcp"
	"github.com/rlds/chagent/pkg/tools"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

// -----------------------------------------------------------------------------
// fakes / helpers
// -----------------------------------------------------------------------------

type fakeManager struct {
	servers map[string]*picomcp.ServerConnection
}

func (m *fakeManager) GetServers() map[string]*picomcp.ServerConnection {
	out := make(map[string]*picomcp.ServerConnection, len(m.servers))
	for name, conn := range m.servers {
		out[name] = conn
	}
	return out
}

// compile-time check: the production manager satisfies the narrow interface.
var _ Manager = (*picomcp.Manager)(nil)

// newFakeSession connects a real in-memory MCP client session to an official
// SDK server configured by configure.
func newFakeSession(t *testing.T, opts *mcp.ServerOptions, configure func(*mcp.Server)) *mcp.ClientSession {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "fake-resource-server", Version: "0.0.1"}, opts)
	if configure != nil {
		configure(server)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("fake server Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("fake client Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func addTextResource(server *mcp.Server, uri, name, text string) {
	server.AddResource(
		&mcp.Resource{URI: uri, Name: name, MIMEType: "text/plain"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{
					{URI: uri, MIMEType: "text/plain", Text: text},
				},
			}, nil
		},
	)
}

func managerFromSessions(sessions map[string]*mcp.ClientSession) *fakeManager {
	servers := make(map[string]*picomcp.ServerConnection, len(sessions))
	for name, session := range sessions {
		servers[name] = &picomcp.ServerConnection{Name: name, Session: session}
	}
	return &fakeManager{servers: servers}
}

// -----------------------------------------------------------------------------
// metadata / registration
// -----------------------------------------------------------------------------

func TestToolMetadataAndRegister(t *testing.T) {
	mgr := managerFromSessions(nil)

	list := NewListResourcesTool(mgr)
	templates := NewListResourceTemplatesTool(mgr)
	read := NewReadResourceTool(mgr)

	wantNames := []string{ToolListResources, ToolListResourceTemplates, ToolReadResource}
	metas := []toolshared.PromptMetadata{list.PromptMetadata(), templates.PromptMetadata(), read.PromptMetadata()}
	descriptions := []string{list.Description(), templates.Description(), read.Description()}
	for i := range wantNames {
		meta := metas[i]
		if meta.Layer != "capability" || meta.Slot != "dsh" || meta.Source != "tool_registry:dsh" {
			t.Fatalf("tool[%d].PromptMetadata() = %+v, want capability/dsh/tool_registry:dsh", i, meta)
		}
		if !strings.Contains(descriptions[i], "MCP") {
			t.Fatalf("tool[%d].Description() = %q, want MCP description", i, descriptions[i])
		}
	}

	// read_mcp_resource must require uri in its schema.
	readParams := read.Parameters()
	required, _ := readParams["required"].([]string)
	if len(required) != 1 || required[0] != "uri" {
		t.Fatalf("read parameters required = %v, want [uri]", readParams["required"])
	}
	listParams := list.Parameters()
	if gotRequired, _ := listParams["required"].([]string); len(gotRequired) != 0 {
		t.Fatalf("list parameters required = %v, want none", listParams["required"])
	}

	// Register registers all three tools on a real registry.
	registry := tools.NewToolRegistry()
	Register(registry, mgr)
	for _, name := range wantNames {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("registry missing registered tool %q", name)
		}
	}
	// nil registry is a no-op, not a panic.
	Register(nil, mgr)
}

// -----------------------------------------------------------------------------
// list_mcp_resources
// -----------------------------------------------------------------------------

func TestListResourcesAggregatesConnectedServers(t *testing.T) {
	sessionA := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://a-1.txt", "a-1", "hello a")
	})
	sessionB := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://b-1.txt", "b-1", "hello b")
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"zeta": sessionB, "alpha": sessionA})

	out := NewListResourcesTool(mgr).Execute(context.Background(), map[string]any{})
	if out == nil || out.IsError {
		t.Fatalf("unexpected result: %+v", out)
	}
	for _, want := range []string{"MCP server：alpha", "MCP server：zeta", "mem://a-1.txt", "mem://b-1.txt"} {
		if !strings.Contains(out.ForLLM, want) {
			t.Fatalf("ForLLM = %q, want it to contain %q", out.ForLLM, want)
		}
	}
	if strings.Index(out.ForLLM, "MCP server：alpha") > strings.Index(out.ForLLM, "MCP server：zeta") {
		t.Fatalf("servers not sorted in output: %q", out.ForLLM)
	}
}

func TestListResourcesSpecificServerAndUnknownServer(t *testing.T) {
	sessionA := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://a-1.txt", "a-1", "hello a")
	})
	sessionB := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://b-1.txt", "b-1", "hello b")
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"a": sessionA, "b": sessionB})

	out := NewListResourcesTool(mgr).Execute(context.Background(), map[string]any{"server": "a"})
	if out.IsError {
		t.Fatalf("unexpected error: %+v", out)
	}
	if strings.Contains(out.ForLLM, "mem://b-1.txt") {
		t.Fatalf("ForLLM = %q, want only server a resources", out.ForLLM)
	}

	out = NewListResourcesTool(mgr).Execute(context.Background(), map[string]any{"server": "missing"})
	if out == nil || !out.IsError {
		t.Fatalf("unknown server result = %+v, want error", out)
	}
	if !strings.Contains(out.ForLLM, "未连接") {
		t.Fatalf("error = %q, want it to mention 未连接", out.ForLLM)
	}
}

func TestListResourcesPagination(t *testing.T) {
	session := newFakeSession(t, &mcp.ServerOptions{PageSize: 1}, func(server *mcp.Server) {
		addTextResource(server, "mem://r1.txt", "r1", "one")
		addTextResource(server, "mem://r2.txt", "r2", "two")
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"page": session})
	tool := NewListResourcesTool(mgr)

	first := tool.Execute(context.Background(), map[string]any{})
	if first.IsError {
		t.Fatalf("first page error: %+v", first)
	}
	match := regexp.MustCompile(`next_cursor:\s*(\S+)`).FindStringSubmatch(first.ForLLM)
	if len(match) != 2 {
		t.Fatalf("first page = %q, want a next_cursor", first.ForLLM)
	}

	seen := first.ForLLM + "\n"
	second := tool.Execute(context.Background(), map[string]any{"server": "page", "cursor": match[1]})
	if second.IsError {
		t.Fatalf("second page error: %+v", second)
	}
	seen += second.ForLLM
	for _, uri := range []string{"mem://r1.txt", "mem://r2.txt"} {
		if !strings.Contains(seen, uri) {
			t.Fatalf("paged output missing %q: %q", uri, seen)
		}
	}
}

func TestListResourcesCursorRequiresServerWhenAmbiguous(t *testing.T) {
	sessionA := newFakeSession(t, nil, nil)
	sessionB := newFakeSession(t, nil, nil)
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"a": sessionA, "b": sessionB})

	out := NewListResourcesTool(mgr).Execute(context.Background(), map[string]any{"cursor": "c1"})
	if out == nil || !out.IsError {
		t.Fatalf("ambiguous cursor result = %+v, want error", out)
	}
	if !strings.Contains(out.ForLLM, "必须显式指定 `server`") {
		t.Fatalf("error = %q, want ambiguity diagnostic", out.ForLLM)
	}
}

func TestListResourcesNoConnectedServers(t *testing.T) {
	out := NewListResourcesTool(managerFromSessions(nil)).Execute(context.Background(), map[string]any{})
	if out == nil || !out.IsError || !strings.Contains(out.ForLLM, "没有已连接的 MCP server") {
		t.Fatalf("result = %+v, want no-connected-servers error", out)
	}

	// nil manager must not panic.
	nilOut := NewListResourcesTool(nil).Execute(context.Background(), map[string]any{})
	if nilOut == nil || !nilOut.IsError {
		t.Fatalf("nil manager result = %+v, want error", nilOut)
	}
}

// -----------------------------------------------------------------------------
// list_mcp_resource_templates
// -----------------------------------------------------------------------------

func TestListResourceTemplates(t *testing.T) {
	session := newFakeSession(t, nil, func(server *mcp.Server) {
		server.AddResourceTemplate(
			&mcp.ResourceTemplate{URITemplate: "mem://items/{id}", Name: "item", MIMEType: "application/json"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{
					Contents: []*mcp.ResourceContents{{URI: "mem://items/1", MIMEType: "application/json", Text: `{"id":1}`}},
				}, nil
			},
		)
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"tpl": session})

	out := NewListResourceTemplatesTool(mgr).Execute(context.Background(), map[string]any{})
	if out == nil || out.IsError {
		t.Fatalf("unexpected result: %+v", out)
	}
	if !strings.Contains(out.ForLLM, "mem://items/{id}") || !strings.Contains(out.ForLLM, "uri_template") {
		t.Fatalf("ForLLM = %q, want template uri", out.ForLLM)
	}
}

// -----------------------------------------------------------------------------
// read_mcp_resource
// -----------------------------------------------------------------------------

func TestReadResourceTextInline(t *testing.T) {
	session := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://doc.txt", "doc", "inline text body")
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"srv": session})

	out := NewReadResourceTool(mgr).Execute(context.Background(), map[string]any{"uri": "mem://doc.txt"})
	if out == nil || out.IsError {
		t.Fatalf("unexpected result: %+v", out)
	}
	if !strings.Contains(out.ForLLM, "inline text body") {
		t.Fatalf("ForLLM = %q, want inline text", out.ForLLM)
	}
	if len(out.ArtifactTags) != 0 {
		t.Fatalf("ArtifactTags = %v, want none for inline text", out.ArtifactTags)
	}
}

func TestReadResourceLargeTextPersistedAsArtifact(t *testing.T) {
	workspace := t.TempDir()
	large := strings.Repeat("abcdefghij", 100) // 1000 chars
	session := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://large.txt", "large", large)
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"srv": session})

	tool := NewReadResourceTool(mgr, WithWorkspace(workspace), WithMaxInlineTextRunes(40))
	out := tool.Execute(context.Background(), map[string]any{"uri": "mem://large.txt"})
	if out == nil || out.IsError {
		t.Fatalf("unexpected result: %+v", out)
	}
	if strings.Contains(out.ForLLM, large) {
		t.Fatal("large text must not be inlined into model context")
	}
	if len(out.ArtifactTags) != 1 || !strings.HasPrefix(out.ArtifactTags[0], "[file:") {
		t.Fatalf("ArtifactTags = %v, want one file tag", out.ArtifactTags)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(out.ArtifactTags[0], "[file:"), "]")
	if !strings.Contains(path, filepath.Join(".artifacts", "mcp-resources")) {
		t.Fatalf("artifact path = %q, want under .artifacts/mcp-resources", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(data) != large {
		t.Fatalf("artifact content mismatch: got %d bytes, want %d", len(data), len(large))
	}
}

func TestReadResourceBinaryPersistedAsArtifact(t *testing.T) {
	workspace := t.TempDir()
	payload := []byte{0x00, 0x01, 0x02, 0xfe, 0xff}
	session := newFakeSession(t, nil, func(server *mcp.Server) {
		server.AddResource(
			&mcp.Resource{URI: "mem://blob.bin", Name: "blob", MIMEType: "application/octet-stream"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{
					Contents: []*mcp.ResourceContents{
						{URI: "mem://blob.bin", MIMEType: "application/octet-stream", Blob: payload},
					},
				}, nil
			},
		)
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"srv": session})

	out := NewReadResourceTool(mgr, WithWorkspace(workspace)).Execute(context.Background(), map[string]any{"uri": "mem://blob.bin"})
	if out == nil || out.IsError {
		t.Fatalf("unexpected result: %+v", out)
	}
	if !strings.Contains(out.ForLLM, "二进制资源") || !strings.Contains(out.ForLLM, "已保存至") {
		t.Fatalf("ForLLM = %q, want binary artifact note", out.ForLLM)
	}
	if len(out.ArtifactTags) != 1 {
		t.Fatalf("ArtifactTags = %v, want one file tag", out.ArtifactTags)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(out.ArtifactTags[0], "[file:"), "]")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if string(data) != string(payload) {
		t.Fatalf("artifact bytes = %v, want %v", data, payload)
	}
}

func TestReadResourceOversizeBinaryRejected(t *testing.T) {
	payload := []byte(strings.Repeat("x", 32))
	session := newFakeSession(t, nil, func(server *mcp.Server) {
		server.AddResource(
			&mcp.Resource{URI: "mem://big.bin", Name: "big"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{
					Contents: []*mcp.ResourceContents{{URI: "mem://big.bin", Blob: payload}},
				}, nil
			},
		)
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"srv": session})

	out := NewReadResourceTool(mgr, WithMaxReadBytes(8)).Execute(context.Background(), map[string]any{"uri": "mem://big.bin"})
	if out == nil || !out.IsError {
		t.Fatalf("result = %+v, want oversize error", out)
	}
	if !strings.Contains(out.ForLLM, "超过配置上限") {
		t.Fatalf("error = %q, want limit diagnostic", out.ForLLM)
	}
}

func TestReadResourceServerAmbiguityAndValidation(t *testing.T) {
	sessionA := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://a.txt", "a", "A")
	})
	sessionB := newFakeSession(t, nil, func(server *mcp.Server) {
		addTextResource(server, "mem://b.txt", "b", "B")
	})
	mgr := managerFromSessions(map[string]*mcp.ClientSession{"a": sessionA, "b": sessionB})
	tool := NewReadResourceTool(mgr)

	// Missing uri.
	out := tool.Execute(context.Background(), map[string]any{})
	if out == nil || !out.IsError || !strings.Contains(out.ForLLM, "非空的 `uri`") {
		t.Fatalf("missing uri result = %+v, want uri validation error", out)
	}

	// Ambiguous server.
	out = tool.Execute(context.Background(), map[string]any{"uri": "mem://a.txt"})
	if out == nil || !out.IsError || !strings.Contains(out.ForLLM, "必须显式指定 `server`") {
		t.Fatalf("ambiguous server result = %+v, want ambiguity error", out)
	}

	// Explicit server works.
	out = tool.Execute(context.Background(), map[string]any{"server": "a", "uri": "mem://a.txt"})
	if out == nil || out.IsError || !strings.Contains(out.ForLLM, "A") {
		t.Fatalf("explicit server result = %+v, want server a content", out)
	}
}

func TestReadResourceSessionAndServerErrors(t *testing.T) {
	// Session missing.
	mgr := managerFromSessions(nil)
	mgr.servers["broken"] = &picomcp.ServerConnection{Name: "broken"}
	out := NewReadResourceTool(mgr).Execute(context.Background(), map[string]any{
		"server": "broken", "uri": "mem://x",
	})
	if out == nil || !out.IsError || !strings.Contains(out.ForLLM, "没有活动会话") {
		t.Fatalf("nil session result = %+v, want session error", out)
	}

	// Server-side read error.
	session := newFakeSession(t, nil, func(server *mcp.Server) {
		server.AddResource(
			&mcp.Resource{URI: "mem://err.txt", Name: "err"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return nil, os.ErrPermission
			},
		)
	})
	mgr = managerFromSessions(map[string]*mcp.ClientSession{"srv": session})
	out = NewReadResourceTool(mgr).Execute(context.Background(), map[string]any{"uri": "mem://err.txt"})
	if out == nil || !out.IsError || !strings.Contains(out.ForLLM, "读取 resource") {
		t.Fatalf("server error result = %+v, want read error", out)
	}
}
