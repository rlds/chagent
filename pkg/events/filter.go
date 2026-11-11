package events

import "strings"

// Filter 决定事件是否应通过某个 EventChannel。
type Filter func(Event) bool

// ScopeFilter 将选中的非空字段与 Event.Scope 进行匹配。
type ScopeFilter struct {
	AgentID    string
	SessionKey string
	TurnID     string
	Channel    string
	ChatID     string
	MessageID  string
}

// MatchKind 匹配 kind 属于 kinds 的事件。kinds 为空时匹配所有事件。
func MatchKind(kinds ...Kind) Filter {
	if len(kinds) == 0 {
		return matchAll
	}

	allowed := make(map[Kind]struct{}, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = struct{}{}
	}

	return func(evt Event) bool {
		_, ok := allowed[evt.Kind]
		return ok
	}
}

// MatchKindPrefix 匹配 kind 以 prefix 开头的事件。
func MatchKindPrefix(prefix string) Filter {
	if prefix == "" {
		return matchAll
	}
	return func(evt Event) bool {
		return strings.HasPrefix(evt.Kind.String(), prefix)
	}
}

// MatchSource 匹配由 component 发出、且（可选）名称属于 names 的事件。
func MatchSource(component string, names ...string) Filter {
	if component == "" && len(names) == 0 {
		return matchAll
	}

	allowedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowedNames[name] = struct{}{}
	}

	return func(evt Event) bool {
		if component != "" && evt.Source.Component != component {
			return false
		}
		if len(allowedNames) == 0 {
			return true
		}
		_, ok := allowedNames[evt.Source.Name]
		return ok
	}
}

// MatchScope 匹配 Scope 包含所有非空过滤字段的事件。
func MatchScope(scope ScopeFilter) Filter {
	if scope == (ScopeFilter{}) {
		return matchAll
	}

	return func(evt Event) bool {
		return matchesString(scope.AgentID, evt.Scope.AgentID) &&
			matchesString(scope.SessionKey, evt.Scope.SessionKey) &&
			matchesString(scope.TurnID, evt.Scope.TurnID) &&
			matchesString(scope.Channel, evt.Scope.Channel) &&
			matchesString(scope.ChatID, evt.Scope.ChatID) &&
			matchesString(scope.MessageID, evt.Scope.MessageID)
	}
}

// And 组合多个过滤器，并在第一个不匹配处短路。
func And(filters ...Filter) Filter {
	if len(filters) == 0 {
		return matchAll
	}

	return func(evt Event) bool {
		for _, filter := range filters {
			if filter != nil && !filter(evt) {
				return false
			}
		}
		return true
	}
}

// Or 组合多个过滤器，并在第一个匹配处短路。
func Or(filters ...Filter) Filter {
	if len(filters) == 0 {
		return matchAll
	}

	return func(evt Event) bool {
		for _, filter := range filters {
			if filter == nil || filter(evt) {
				return true
			}
		}
		return false
	}
}

func matchAll(Event) bool {
	return true
}

func matchesString(want, got string) bool {
	return want == "" || want == got
}

func matchesFilters(filters []Filter, evt Event) bool {
	for _, filter := range filters {
		if filter != nil && !filter(evt) {
			return false
		}
	}
	return true
}
