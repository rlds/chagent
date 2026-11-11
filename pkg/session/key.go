package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/rlds/chagent/pkg/routing"
)

const (
	sessionKeyV1Prefix          = "sk_v1_"
	legacyAgentSessionKeyPrefix = "agent:"
)

type ParsedLegacySessionKey struct {
	AgentID string
	Rest    string
}

// BuildOpaqueSessionKey 根据规范别名字符串返回稳定的不透明会话键。
// 出于兼容性和迁移目的，该别名仍可通过元数据获取。
func BuildOpaqueSessionKey(alias string) string {
	normalized := strings.TrimSpace(strings.ToLower(alias))
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return sessionKeyV1Prefix + hex.EncodeToString(sum[:])
}

// IsOpaqueSessionKey 在键匹配当前不透明会话键格式时返回 true。
func IsOpaqueSessionKey(key string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), sessionKeyV1Prefix)
}

func IsLegacyAgentSessionKey(key string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), legacyAgentSessionKeyPrefix)
}

func IsExplicitSessionKey(key string) bool {
	return IsOpaqueSessionKey(key) || IsLegacyAgentSessionKey(key)
}

func ParseLegacyAgentSessionKey(sessionKey string) *ParsedLegacySessionKey {
	raw := strings.TrimSpace(sessionKey)
	if raw == "" {
		return nil
	}
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) < 3 || parts[0] != "agent" {
		return nil
	}
	agentID := strings.TrimSpace(parts[1])
	rest := parts[2]
	if agentID == "" || rest == "" {
		return nil
	}
	return &ParsedLegacySessionKey{AgentID: agentID, Rest: rest}
}

// ResolveAgentID 返回会话关联的路由 agent ID。它优先使用结构化会话作用域
// 元数据，并在不可用时回退到旧式按 agent 划分的会话键以保持兼容。
func ResolveAgentID(store any, sessionKey string) string {
	if scopeReader, ok := store.(interface {
		GetSessionScope(sessionKey string) *SessionScope
	}); ok {
		scope := scopeReader.GetSessionScope(sessionKey)
		if scope != nil && strings.TrimSpace(scope.AgentID) != "" {
			return routing.NormalizeAgentID(scope.AgentID)
		}
	}

	if parsed := ParseLegacyAgentSessionKey(sessionKey); parsed != nil {
		return routing.NormalizeAgentID(parsed.AgentID)
	}

	return ""
}

func BuildLegacyMainAlias(agentID string) string {
	return fmt.Sprintf("agent:%s:main", routing.NormalizeAgentID(agentID))
}

// BuildMainSessionKey 返回某个 agent 的规范不透明主会话键。
// 对应旧式别名仍可通过 BuildLegacyMainAlias 获取，用于兼容和迁移逻辑。
func BuildMainSessionKey(agentID string) string {
	return BuildOpaqueSessionKey(BuildLegacyMainAlias(agentID))
}

func BuildLegacyDirectAliases(agentID, channel, account, peerID string) []string {
	agentID = routing.NormalizeAgentID(agentID)
	channel = normalizeLegacyChannel(channel)
	account = routing.NormalizeAccountID(account)
	peerID = strings.ToLower(strings.TrimSpace(peerID))
	if peerID == "" {
		return nil
	}
	return []string{
		fmt.Sprintf("agent:%s:direct:%s", agentID, peerID),
		fmt.Sprintf("agent:%s:%s:direct:%s", agentID, channel, peerID),
		fmt.Sprintf("agent:%s:%s:%s:direct:%s", agentID, channel, account, peerID),
	}
}

func BuildLegacyPeerAlias(agentID, channel, peerKind, peerID string) string {
	agentID = routing.NormalizeAgentID(agentID)
	channel = normalizeLegacyChannel(channel)
	peerKind = strings.ToLower(strings.TrimSpace(peerKind))
	if peerKind == "" {
		peerKind = "unknown"
	}
	peerID = strings.ToLower(strings.TrimSpace(peerID))
	if peerID == "" {
		peerID = "unknown"
	}
	return fmt.Sprintf("agent:%s:%s:%s:%s", agentID, channel, peerKind, peerID)
}

// CanonicalSessionIdentityID 在可能时利用 identity_links 折叠身份，
// 然后返回规范化的全小写标识符。
func CanonicalSessionIdentityID(channel, rawID string, identityLinks map[string][]string) string {
	normalizedID := strings.TrimSpace(rawID)
	if normalizedID == "" {
		return ""
	}
	if linked := resolveLinkedPeerID(identityLinks, channel, normalizedID); linked != "" {
		normalizedID = linked
	}
	return strings.ToLower(normalizedID)
}

func normalizeLegacyChannel(channel string) string {
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel == "" {
		return "unknown"
	}
	return channel
}

func resolveLinkedPeerID(identityLinks map[string][]string, channel, peerID string) string {
	if len(identityLinks) == 0 {
		return ""
	}
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return ""
	}

	candidates := make(map[string]bool)
	rawCandidate := strings.ToLower(peerID)
	if rawCandidate != "" {
		candidates[rawCandidate] = true
	}
	channel = strings.ToLower(strings.TrimSpace(channel))
	if channel != "" {
		candidates[fmt.Sprintf("%s:%s", channel, rawCandidate)] = true
	}
	if idx := strings.Index(rawCandidate, ":"); idx > 0 && idx < len(rawCandidate)-1 {
		candidates[rawCandidate[idx+1:]] = true
	}

	for canonical, ids := range identityLinks {
		canonicalName := strings.TrimSpace(canonical)
		if canonicalName == "" {
			continue
		}
		for _, id := range ids {
			normalized := strings.ToLower(strings.TrimSpace(id))
			if normalized != "" && candidates[normalized] {
				return canonicalName
			}
		}
	}
	return ""
}

// CanonicalScopeSignature 返回作用域的稳定序列化表示。
func CanonicalScopeSignature(scope SessionScope) string {
	parts := []string{
		fmt.Sprintf("v=%d", scope.Version),
		fmt.Sprintf("agent=%s", strings.TrimSpace(strings.ToLower(scope.AgentID))),
		fmt.Sprintf("channel=%s", strings.TrimSpace(strings.ToLower(scope.Channel))),
		fmt.Sprintf("account=%s", strings.TrimSpace(strings.ToLower(scope.Account))),
	}
	for _, dimension := range scope.Dimensions {
		dimension = strings.TrimSpace(strings.ToLower(dimension))
		if dimension == "" {
			continue
		}
		value := strings.TrimSpace(strings.ToLower(scope.Values[dimension]))
		parts = append(parts, fmt.Sprintf("%s=%s", dimension, value))
	}
	return strings.Join(parts, "|")
}

// BuildSessionKey 返回结构化会话作用域对应的当前不透明键。
func BuildSessionKey(scope SessionScope) string {
	return BuildOpaqueSessionKey(CanonicalScopeSignature(scope))
}
