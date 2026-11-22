package agent

import (
	"context"
	"testing"

	"github.com/rlds/chagent/pkg/config"
)

func TestCapabilitySnapshot_ReportsRealWorkspaceAndPaths(t *testing.T) {
	al, cfg, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()
	defer al.Close()
	cfg.Tools.AllowReadPaths = []string{"/tmp/read"}
	cfg.Tools.AllowWritePaths = []string{"/tmp/write"}
	cfg.Tools.MCP = config.MCPConfig{}
	snap := al.CapabilitySnapshot(context.Background())
	if snap.PID <= 0 || snap.Workspace == "" || snap.Model == "" {
		t.Fatalf("incomplete snapshot: %+v", snap)
	}
	if len(snap.ReadPaths) != 1 || snap.ReadPaths[0] != "/tmp/read" {
		t.Fatalf("read paths=%v", snap.ReadPaths)
	}
	if len(snap.WritePaths) != 1 || snap.WritePaths[0] != "/tmp/write" {
		t.Fatalf("write paths=%v", snap.WritePaths)
	}
	if len(snap.Tools) == 0 {
		t.Fatal("expected builtin tools in snapshot")
	}
}
