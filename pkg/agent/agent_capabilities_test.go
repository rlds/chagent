package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rlds/chagent/pkg/config"
)

func TestCapabilitySnapshot_ReportsRealWorkspaceAndPaths(t *testing.T) {
	al, cfg, _, _, cleanup := newTestAgentLoop(t)
	defer cleanup()
	defer al.Close()
	skillDir := filepath.Join(cfg.Agents.Defaults.Workspace, "skills", "demo-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: demo-skill\ndescription: demo skill\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	if len(snap.Skills) != 1 || snap.Skills[0].Name != "demo-skill" {
		t.Fatalf("skills=%+v", snap.Skills)
	}
}
