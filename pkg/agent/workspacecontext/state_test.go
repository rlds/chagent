package workspacecontext

import (
	"testing"

	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

func contextMessage(kind string, baseline bool, identity string, changes ...Change) providers.Message {
	return providers.Message{
		Role:    "user",
		Content: "context",
		Context: &ContextMeta{
			Kind:             kind,
			Form:             protocoltypes.ContextFormReminder,
			Baseline:         baseline,
			BaselineIdentity: identity,
			Changes:          changes,
		},
	}
}

func TestScanVisibleReplay(t *testing.T) {
	rootScope := scopeKey(".", "AGENTS.md")
	nestedScope := scopeKey("packages/app", "AGENTS.md")
	history := []providers.Message{
		{Role: "user", Content: "真实的用户消息"},
		contextMessage(ContextKindWorkspaceInstructions, true, "identity-1",
			Change{Action: ChangeActionSet, Scope: rootScope, Path: "AGENTS.md", Digest: "d1"},
			Change{Action: ChangeActionSet, Scope: nestedScope, Path: "packages/app/AGENTS.md", Digest: "d2"},
		),
		contextMessage(ContextKindWorkspaceInstructions, false, "",
			Change{Action: ChangeActionReplace, Scope: rootScope, Path: "AGENTS.md", Digest: "d1b"},
		),
	}
	state := ScanVisible(history)
	if !state.Seen {
		t.Fatal("Seen = false, want true")
	}
	if state.Baseline == nil || state.Baseline.Identity != "identity-1" {
		t.Fatalf("baseline = %+v, want identity-1", state.Baseline)
	}
	if got := state.Scopes[rootScope].Digest; got != "d1b" {
		t.Fatalf("root scope digest = %q, want d1b (last write wins)", got)
	}
	if got := state.Scopes[nestedScope].Digest; got != "d2" {
		t.Fatalf("nested scope digest = %q, want d2", got)
	}
}

func TestScanVisibleRemove(t *testing.T) {
	scope := scopeKey("packages/app", "AGENTS.md")
	history := []providers.Message{
		contextMessage(ContextKindWorkspaceInstructions, true, "id",
			Change{Action: ChangeActionSet, Scope: scope, Path: "packages/app/AGENTS.md", Digest: "d1"},
		),
		contextMessage(ContextKindWorkspaceInstructions, false, "",
			Change{Action: ChangeActionRemove, Scope: scope, Path: "packages/app/AGENTS.md"},
		),
	}
	state := ScanVisible(history)
	if _, exists := state.Scopes[scope]; exists {
		t.Fatalf("scope %q was not removed: %+v", scope, state.Scopes)
	}
}

func TestScanVisibleLatestBaseline(t *testing.T) {
	history := []providers.Message{
		contextMessage(ContextKindWorkspaceInstructions, true, "old"),
		contextMessage(ContextKindWorkspaceInstructions, false, ""),
		contextMessage(ContextKindWorkspaceInstructions, true, "new"),
	}
	state := ScanVisible(history)
	if state.Baseline == nil || state.Baseline.Identity != "new" {
		t.Fatalf("baseline = %+v, want new", state.Baseline)
	}
	if state.Baseline.Seq != 2 {
		t.Fatalf("baseline seq = %d, want 2", state.Baseline.Seq)
	}
}

func TestScanVisibleIgnoresOtherKinds(t *testing.T) {
	history := []providers.Message{
		contextMessage("runtime-context", true, "runtime"),
		contextMessage("task-state", false, "", Change{Action: ChangeActionSet, Scope: "x", Path: "x"}),
		{Role: "user", Content: "普通消息"},
	}
	state := ScanVisible(history)
	if state.Seen {
		t.Fatal("Seen = true for non-workspace context")
	}
	if state.Baseline != nil || len(state.Scopes) != 0 {
		t.Fatalf("state = %+v, want empty", state)
	}
}

func TestScanVisibleEmptyHistory(t *testing.T) {
	state := ScanVisible(nil)
	if state.Seen || state.Baseline != nil || len(state.Scopes) != 0 {
		t.Fatalf("empty history state = %+v", state)
	}
}

func TestBaselineIdentityChangesWithConfig(t *testing.T) {
	workspace := "/tmp/ws"
	projectRoot := "/tmp/ws"
	base := DefaultConfig()
	identity := BaselineIdentity(base, projectRoot, workspace)
	if identity == "" || len(identity) != 64 {
		t.Fatalf("identity = %q, want sha256 hex", identity)
	}
	if again := BaselineIdentity(base, projectRoot, workspace); again != identity {
		t.Fatalf("identity not stable: %q vs %q", identity, again)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "max bytes", mutate: func(c *Config) { c.MaxBytes = 1024 }},
		{name: "max source bytes", mutate: func(c *Config) { c.MaxSourceBytes = 1024 }},
		{name: "instruction files", mutate: func(c *Config) { c.InstructionFiles = []string{"OTHER.md"} }},
		{name: "local files", mutate: func(c *Config) { c.LocalInstructionFiles = []string{"OTHER.local.md"} }},
		{name: "markers", mutate: func(c *Config) { c.ProjectRootMarkers = []string{".hg"} }},
		{name: "follow symlinks", mutate: func(c *Config) { c.FollowSymlinks = true }},
		{name: "user global", mutate: func(c *Config) {
			c.UserGlobal = true
			c.ChagentHome = "/home/u/.chagent"
			c.AgentsHome = "/home/u/.agents"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := DefaultConfig()
			tc.mutate(&changed)
			if got := BaselineIdentity(changed, projectRoot, workspace); got == identity {
				t.Fatalf("identity did not change after %s", tc.name)
			}
		})
	}
}

func TestBaselineIdentityStableAcrossTurns(t *testing.T) {
	cfg := DefaultConfig()
	first := BaselineIdentity(cfg, "/a/ws", "/a/ws")
	// The identity uses a workspace-relative project root, so moving the whole
	// workspace must not invalidate the baseline.
	second := BaselineIdentity(cfg, "/b/ws", "/b/ws")
	if first != second {
		t.Fatalf("identity changed when the workspace moved: %q vs %q", first, second)
	}
	// A different relative project root does change it.
	nested := BaselineIdentity(cfg, "/b", "/b/ws")
	if nested == second {
		t.Fatal("identity did not change when the relative project root changed")
	}
	// The exclude path participates in the identity.
	withoutExclude := BaselineIdentityWithExclude(cfg, "/b/ws", "/b/ws", "")
	withExclude := BaselineIdentityWithExclude(cfg, "/b/ws", "/b/ws", "/b/ws/AGENTS.md")
	if withoutExclude == withExclude {
		t.Fatal("identity did not change when excludePath changed")
	}
}
