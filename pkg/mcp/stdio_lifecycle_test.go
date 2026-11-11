package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlds/chagent/pkg/config"
)

// stdioFixtureCommand returns a shell command that runs the repo's MCP stdio
// fixture through `exec`, so the pid captured in pidFile is the server pid.
// It skips the test when python3 or the fixture is unavailable.
func stdioFixtureCommand(t *testing.T, pidFile string) (string, []string, map[string]string) {
	t.Helper()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skipf("python3 not available: %v", err)
	}
	fixture, err := filepath.Abs(filepath.Join("..", "..", "scripts", "fixtures", "mcp_stdio_server.py"))
	if err != nil {
		t.Fatalf("resolve fixture path: %v", err)
	}
	if _, err := os.Stat(fixture); err != nil {
		t.Skipf("MCP stdio fixture not available: %v", err)
	}

	script := fmt.Sprintf("echo $$ > %q; exec %q %q", pidFile, python, fixture)
	return "sh", []string{"-c", script}, map[string]string{
		"CFGCAP_MCP_SERVER_NAME":  "lifecycle",
		"CFGCAP_MCP_ECHO_PREFIX":  "LIFECYCLE_ECHO",
		"CFGCAP_MCP_LOG_FILE":     filepath.Join(t.TempDir(), "fixture.log"),
		"CFGCAP_MCP_CONTROL_FILE": "",
	}
}

func waitPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for pid file %s", pidFile)
	return 0
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stdio MCP subprocess %d still alive after %s", pid, timeout)
}

func assertEchoCall(t *testing.T, ctx context.Context, session *mcp.ClientSession, text string) {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": text},
	})
	if err != nil {
		t.Fatalf("CallTool(echo) error = %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatalf("CallTool(echo) returned empty result: %+v", result)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(content.Text, text) {
		t.Fatalf("CallTool(echo) content = %+v, want it to contain %q", result.Content[0], text)
	}
}

// TestStdioSessionOutlivesConnectContext is the regression test for the live
// E2E finding: cancelling the context that triggered the stdio connection (a
// /reload request or a turn) must NOT kill the MCP server; the subprocess is
// bound to the session and is reaped by Close.
func TestStdioSessionOutlivesConnectContext(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "server.pid")
	command, args, env := stdioFixtureCommand(t, pidFile)

	ctx, cancel := context.WithCancel(context.Background())
	conn, err := connectServer(ctx, "lifecycle", config.MCPServerConfig{
		Enabled: true,
		Type:    "stdio",
		Command: command,
		Args:    args,
		Env:     env,
	}, 0, nil)
	if err != nil {
		cancel()
		t.Fatalf("connectServer() error = %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Session.Close()
		if pid := readPID(pidFile); pid > 0 && processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	pid := waitPID(t, pidFile)

	// The request context that opened the connection ends here (reload/turn end).
	cancel()
	time.Sleep(200 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatalf("stdio MCP subprocess %d exited when the connect context was canceled", pid)
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()
	assertEchoCall(t, callCtx, conn.Session, "after-context-cancel")

	// Session close must reap the subprocess (session-, not request-bound).
	if err := conn.Session.Close(); err != nil {
		t.Fatalf("session.Close() error = %v", err)
	}
	waitProcessGone(t, pid, 10*time.Second)
}

// TestManagerStdioSurvivesReloadContext covers the manager path used by
// /reload: LoadFromMCPConfig's context is canceled right after loading, yet the
// server must stay usable and only Manager.Close() may stop it.
func TestManagerStdioSurvivesReloadContext(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "server.pid")
	command, args, env := stdioFixtureCommand(t, pidFile)

	mgr := NewManager()
	t.Cleanup(func() {
		_ = mgr.Close()
		if pid := readPID(pidFile); pid > 0 && processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	mcpCfg := config.MCPConfig{
		ToolConfig: config.ToolConfig{Enabled: true},
		Servers: map[string]config.MCPServerConfig{
			"lifecycle": {
				Enabled: true,
				Type:    "stdio",
				Command: command,
				Args:    args,
				Env:     env,
			},
		},
	}
	if err := mgr.LoadFromMCPConfig(ctx, mcpCfg, t.TempDir()); err != nil {
		cancel()
		t.Fatalf("LoadFromMCPConfig() error = %v", err)
	}
	pid := waitPID(t, pidFile)
	cancel() // simulates the /reload HTTP request context ending

	time.Sleep(200 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatalf("stdio MCP subprocess %d exited when the reload context was canceled", pid)
	}

	callCtx, callCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer callCancel()
	result, err := mgr.CallTool(callCtx, "lifecycle", "echo", map[string]any{"text": "after-reload-context"})
	if err != nil {
		t.Fatalf("CallTool() after reload-context cancel error = %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatalf("CallTool() after reload-context cancel returned empty result: %+v", result)
	}

	if err := mgr.Close(); err != nil {
		t.Fatalf("Manager.Close() error = %v", err)
	}
	waitProcessGone(t, pid, 10*time.Second)
}

func readPID(pidFile string) int {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}
