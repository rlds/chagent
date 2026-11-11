// Package constants 提供整个代码库共享的常量。
package constants

// internalChannels 定义用于内部通信的频道，
// 这些频道不应暴露给外部用户，也不应被记录为最后活跃频道。
var internalChannels = map[string]struct{}{
	"cli":      {},
	"system":   {},
	"subagent": {},
}

// IsInternalChannel 判断该频道是否为内部频道，是则返回 true。
func IsInternalChannel(channel string) bool {
	_, found := internalChannels[channel]
	return found
}
