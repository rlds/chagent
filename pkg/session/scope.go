package session

// ScopeVersionV1 是第一个结构化会话作用域 schema 版本。
const ScopeVersionV1 = 1

// SessionScope 描述为一次回合选定的语义会话分区。
type SessionScope struct {
	Version    int               `json:"version"`
	AgentID    string            `json:"agent_id"`
	Channel    string            `json:"channel"`
	Account    string            `json:"account"`
	Dimensions []string          `json:"dimensions"`
	Values     map[string]string `json:"values"`
}

// CloneScope 返回 scope 的深拷贝。
func CloneScope(scope *SessionScope) *SessionScope {
	if scope == nil {
		return nil
	}
	cloned := *scope
	if len(scope.Dimensions) > 0 {
		cloned.Dimensions = append([]string(nil), scope.Dimensions...)
	}
	if len(scope.Values) > 0 {
		cloned.Values = make(map[string]string, len(scope.Values))
		for key, value := range scope.Values {
			cloned.Values[key] = value
		}
	}
	return &cloned
}
