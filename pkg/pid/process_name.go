package pid

import "strings"

// agentProcessNames 是用于识别 Chagent 网关的可执行文件/comm 名称片段。
// 重命名后仅识别 "chagent"；旧进程名不再兼容，否则可能把正在运行的
// Chagent 网关误判为陈旧 PID，并在同一工作区启动第二个实例。
var agentProcessNames = []string{"chagent"}

// isChagentProcessName 报告给定的进程名（通常是 /proc/<pid>/comm
// 或可执行映像名）是否属于 Chagent 家族的网关二进制。
func isChagentProcessName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	for _, candidate := range agentProcessNames {
		if strings.Contains(name, candidate) {
			return true
		}
	}
	return false
}
