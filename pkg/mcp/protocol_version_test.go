package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startFakeSDKServer starts an official Go SDK MCP server over an in-memory
// transport pair and returns the client-side transport. It mirrors the fake
// stdio server used by the independent probe under
// build/functest-20261030/verify/mcp-probe/, but is self-contained and runs
// in-process so the assertions are part of the normal test suite.
func startFakeSDKServer(
	t *testing.T,
	opts *mcp.ServerOptions,
	configure func(*mcp.Server),
) *mcp.InMemoryTransport {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "fake-sdk-mcp-server",
		Version: "0.0.1",
	}, opts)
	if configure != nil {
		configure(server)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	session, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("fake server Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return clientTransport
}

// captureInitializeParams returns ServerOptions that record the initialize
// params the server received (the client's request).
func captureInitializeParams() (*mcp.ServerOptions, <-chan *mcp.InitializeParams) {
	paramsCh := make(chan *mcp.InitializeParams, 1)
	opts := &mcp.ServerOptions{
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			params := req.Session.InitializeParams()
			if params == nil {
				return
			}
			select {
			case paramsCh <- params:
			default:
			}
		},
	}
	return opts, paramsCh
}

func waitInitializeParams(t *testing.T, ch <-chan *mcp.InitializeParams) *mcp.InitializeParams {
	t.Helper()
	select {
	case params := <-ch:
		return params
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the server to observe initialize params")
		return nil
	}
}

// TestClientRequestsLatestProtocolVersion asserts the client sends the SDK's
// latest protocol version (2026-06-30 via the local go-sdk fork) and that the
// negotiated version is accepted. It also locks the advertised client
// capabilities: roots{listChanged:true} on, sampling/elicitation off.
func TestClientRequestsLatestProtocolVersion(t *testing.T) {
	opts, paramsCh := captureInitializeParams()
	clientTransport := startFakeSDKServer(t, opts, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, _ := newMCPClient(0, "fake", nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect() error = %v", err)
	}
	defer session.Close()

	if got := LatestMCPProtocolVersion; got != "2026-06-30" {
		t.Fatalf("LatestMCPProtocolVersion = %q, want 2026-06-30", got)
	}
	if len(MCPProtocolVersions) != 5 || MCPProtocolVersions[0] != LatestMCPProtocolVersion {
		t.Fatalf("MCPProtocolVersions = %v, want 5 versions with latest first", MCPProtocolVersions)
	}

	params := waitInitializeParams(t, paramsCh)
	if params.ProtocolVersion != LatestMCPProtocolVersion {
		t.Fatalf("initialize requested protocolVersion = %q, want %q",
			params.ProtocolVersion, LatestMCPProtocolVersion)
	}
	if got := session.InitializeResult().ProtocolVersion; got != LatestMCPProtocolVersion {
		t.Fatalf("negotiated protocolVersion = %q, want %q", got, LatestMCPProtocolVersion)
	}

	caps := params.Capabilities
	if caps == nil {
		t.Fatal("initialize params had nil capabilities")
	}
	if caps.RootsV2 == nil || !caps.RootsV2.ListChanged {
		t.Fatalf("roots capability = %+v, want listChanged=true", caps.RootsV2)
	}
	if caps.Sampling != nil {
		t.Fatalf("sampling capability must not be advertised by default, got %+v", caps.Sampling)
	}
	if caps.Elicitation != nil {
		t.Fatalf("elicitation capability must not be advertised by default, got %+v", caps.Elicitation)
	}
}

// TestClientAcceptsOlderNegotiatedProtocolVersion asserts a server may
// negotiate down to any version the SDK supports and the connection stays
// usable (the client must not force the latest version).
func TestClientAcceptsOlderNegotiatedProtocolVersion(t *testing.T) {
	for _, downgraded := range []string{MCPProtocolVersion20251125, MCPProtocolVersion20250618, MCPProtocolVersion20241105} {
		downgraded := downgraded
		t.Run(downgraded, func(t *testing.T) {
			downgradeMiddleware := func(server *mcp.Server) {
				server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
					return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
						res, err := next(ctx, method, req)
						if err == nil && method == "initialize" {
							if initResult, ok := res.(*mcp.InitializeResult); ok {
								initResult.ProtocolVersion = downgraded
							}
						}
						return res, err
					}
				})
			}

			opts, paramsCh := captureInitializeParams()
			clientTransport := startFakeSDKServer(t, opts, downgradeMiddleware)

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			client, _ := newMCPClient(0, "fake", nil)
			session, err := client.Connect(ctx, clientTransport, nil)
			if err != nil {
				t.Fatalf("client.Connect() with server-negotiated %s error = %v", downgraded, err)
			}
			defer session.Close()

			if got := session.InitializeResult().ProtocolVersion; got != downgraded {
				t.Fatalf("negotiated protocolVersion = %q, want server-downgraded %q", got, downgraded)
			}
			if got := waitInitializeParams(t, paramsCh).ProtocolVersion; got != LatestMCPProtocolVersion {
				t.Fatalf("client requested %q, want latest %q before downgrade", got, LatestMCPProtocolVersion)
			}
		})
	}
}

// TestClientRejectsUnsupportedNegotiatedProtocolVersion asserts a version the
// SDK does not know is rejected instead of being silently accepted.
func TestClientRejectsUnsupportedNegotiatedProtocolVersion(t *testing.T) {
	const unsupported = "2027-01-01"
	configure := func(server *mcp.Server) {
		server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				res, err := next(ctx, method, req)
				if err == nil && method == "initialize" {
					if initResult, ok := res.(*mcp.InitializeResult); ok {
						initResult.ProtocolVersion = unsupported
					}
				}
				return res, err
			}
		})
	}

	clientTransport := startFakeSDKServer(t, nil, configure)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, _ := newMCPClient(0, "fake", nil)
	_, err := client.Connect(ctx, clientTransport, nil)
	if err == nil {
		t.Fatalf("client.Connect() accepted unsupported protocol version %q", unsupported)
	}
	if !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("client.Connect() error = %v, want it to mention unsupported protocol version", err)
	}
}
