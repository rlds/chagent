package session

import (
	"testing"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/routing"
)

func TestAllocateRouteSession_PerPeerDM(t *testing.T) {
	allocation := AllocateRouteSession(AllocationInput{
		AgentID: "main",
		Context: bus.InboundContext{
			Channel:  "telegram",
			Account:  "default",
			ChatID:   "dm-123",
			ChatType: "direct",
			SenderID: "User123",
		},
		SessionPolicy: routing.SessionPolicy{
			Dimensions: []string{"sender"},
		},
	})

	if allocation.SessionKey == "" || !IsOpaqueSessionKey(allocation.SessionKey) {
		t.Fatalf("SessionKey = %q, want opaque session key", allocation.SessionKey)
	}
	if !containsAlias(allocation.SessionAliases, "agent:main:direct:user123") {
		t.Fatalf("SessionAliases = %v, want to contain agent:main:direct:user123", allocation.SessionAliases)
	}
	if allocation.MainSessionKey == "" || !IsOpaqueSessionKey(allocation.MainSessionKey) {
		t.Fatalf("MainSessionKey = %q, want opaque session key", allocation.MainSessionKey)
	}
	if len(allocation.MainAliases) != 1 || allocation.MainAliases[0] != "agent:main:main" {
		t.Fatalf("MainAliases = %v, want [agent:main:main]", allocation.MainAliases)
	}
	if allocation.Scope.Version != ScopeVersionV1 {
		t.Fatalf("Scope.Version = %d, want %d", allocation.Scope.Version, ScopeVersionV1)
	}
	if len(allocation.Scope.Dimensions) != 1 || allocation.Scope.Dimensions[0] != "sender" {
		t.Fatalf("Scope.Dimensions = %v, want [sender]", allocation.Scope.Dimensions)
	}
	if allocation.Scope.Values["sender"] != "user123" {
		t.Fatalf("Scope.Values[sender] = %q, want user123", allocation.Scope.Values["sender"])
	}
}

func TestAllocateRouteSession_GroupPeer(t *testing.T) {
	allocation := AllocateRouteSession(AllocationInput{
		AgentID: "main",
		Context: bus.InboundContext{
			Channel:  "slack",
			Account:  "workspace-a",
			ChatID:   "C001",
			ChatType: "channel",
			SenderID: "U001",
		},
		SessionPolicy: routing.SessionPolicy{
			Dimensions: []string{"chat"},
		},
	})

	if allocation.SessionKey == "" || !IsOpaqueSessionKey(allocation.SessionKey) {
		t.Fatalf("SessionKey = %q, want opaque session key", allocation.SessionKey)
	}
	if !containsAlias(allocation.SessionAliases, "agent:main:slack:channel:c001") {
		t.Fatalf("SessionAliases = %v, want to contain agent:main:slack:channel:c001", allocation.SessionAliases)
	}
	if allocation.MainSessionKey == "" || !IsOpaqueSessionKey(allocation.MainSessionKey) {
		t.Fatalf("MainSessionKey = %q, want opaque session key", allocation.MainSessionKey)
	}
	if len(allocation.MainAliases) != 1 || allocation.MainAliases[0] != "agent:main:main" {
		t.Fatalf("MainAliases = %v, want [agent:main:main]", allocation.MainAliases)
	}
	if len(allocation.Scope.Dimensions) != 1 || allocation.Scope.Dimensions[0] != "chat" {
		t.Fatalf("Scope.Dimensions = %v, want [chat]", allocation.Scope.Dimensions)
	}
	if allocation.Scope.Values["chat"] != "channel:c001" {
		t.Fatalf("Scope.Values[chat] = %q, want channel:c001", allocation.Scope.Values["chat"])
	}
}

func TestAllocateRouteSession_TelegramForumTopicsRemainIsolatedByDefault(t *testing.T) {
	first := AllocateRouteSession(AllocationInput{
		AgentID: "main",
		Context: bus.InboundContext{
			Channel:  "telegram",
			ChatID:   "-1001234567890",
			ChatType: "group",
			TopicID:  "42",
			SenderID: "7",
		},
		SessionPolicy: routing.SessionPolicy{
			Dimensions: []string{"chat"},
		},
	})
	second := AllocateRouteSession(AllocationInput{
		AgentID: "main",
		Context: bus.InboundContext{
			Channel:  "telegram",
			ChatID:   "-1001234567890",
			ChatType: "group",
			TopicID:  "99",
			SenderID: "7",
		},
		SessionPolicy: routing.SessionPolicy{
			Dimensions: []string{"chat"},
		},
	})

	if first.SessionKey == second.SessionKey {
		t.Fatalf("forum topics should not share default session key: %q", first.SessionKey)
	}
	if got := first.Scope.Values["chat"]; got != "group:-1001234567890/42" {
		t.Fatalf("first.Scope.Values[chat] = %q, want %q", got, "group:-1001234567890/42")
	}
	if got := second.Scope.Values["chat"]; got != "group:-1001234567890/99" {
		t.Fatalf("second.Scope.Values[chat] = %q, want %q", got, "group:-1001234567890/99")
	}
}

func TestAllocateRouteSession_PicoDirectAliasesIncludeLegacyChatKey(t *testing.T) {
	allocation := AllocateRouteSession(AllocationInput{
		AgentID: "main",
		Context: bus.InboundContext{
			Channel:  "pico",
			Account:  "default",
			ChatID:   "pico:session-123",
			ChatType: "direct",
			SenderID: "pico-user",
		},
		SessionPolicy: routing.SessionPolicy{
			Dimensions: []string{"sender"},
		},
	})

	if !containsAlias(allocation.SessionAliases, "agent:main:pico:direct:pico:session-123") {
		t.Fatalf("SessionAliases = %v, want pico legacy alias", allocation.SessionAliases)
	}
}

func TestBuildOpaqueSessionKey_IsStable(t *testing.T) {
	first := BuildOpaqueSessionKey("agent:main:direct:user123")
	second := BuildOpaqueSessionKey("agent:main:direct:user123")
	if first != second {
		t.Fatalf("BuildOpaqueSessionKey() mismatch: %q != %q", first, second)
	}
	if !IsOpaqueSessionKey(first) {
		t.Fatalf("expected opaque session key, got %q", first)
	}
}

func containsAlias(aliases []string, want string) bool {
	for _, alias := range aliases {
		if alias == want {
			return true
		}
	}
	return false
}

// TestAllocateRouteSession_PicoForcesChatDimension 回归 P0-2：
// 默认 dm_scope=per-peer（仅 sender）会让同一 Agent 的所有 pico 任务共享一个
// 会话，导致多任务被 steering 串行化。pico 必须强制加入 chat 维度（sessionID
// 已包含 user+project+task），不同任务得到不同 SessionKey。
func TestAllocateRouteSession_PicoForcesChatDimension(t *testing.T) {
	policy := routing.SessionPolicy{Dimensions: []string{"sender"}}
	a := AllocateRouteSession(AllocationInput{
		AgentID: "docs-curator-1012",
		Context: bus.InboundContext{
			Channel: "pico", ChatType: "direct", SenderID: "pico-user",
			ChatID: "pico:pico_user_diaryi_TBlfOe",
		},
		SessionPolicy: policy,
	})
	b := AllocateRouteSession(AllocationInput{
		AgentID: "docs-curator-1012",
		Context: bus.InboundContext{
			Channel: "pico", ChatType: "direct", SenderID: "pico-user",
			ChatID: "pico:pico_user_diaryi_TBlfOv",
		},
		SessionPolicy: policy,
	})
	if a.SessionKey == b.SessionKey {
		t.Fatalf("pico tasks share session key %q; they must be isolated per chat", a.SessionKey)
	}
	if !containsDimension(a.Scope.Dimensions, "chat") {
		t.Fatalf("pico scope dimensions = %v, want chat", a.Scope.Dimensions)
	}
}
