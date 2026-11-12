package config

import "strings"

// ModelCapability 是模型上下文窗口与输出预留的已知能力。
// 仅登记确认过的模型；未登记模型继续使用全局配置/保守启发式。
type ModelCapability struct {
	ContextWindow int
	MaxTokens     int
}

// knownModelCapabilities 记录当前默认模型的真实能力。
//
// 依据：
//   - glm-5.3：智谱官方上下文窗口 1,048,576，最大输出 131,072；
//     早期运行日志按旧的 524288 假设显示 524000，本表已修正为 1M；
//   - deepseek-v4.1-flash：model-relay 调用 #7371 成功处理
//     prompt 235406 + max_tokens 32768；DeepSeek 当前上下文窗口按 1,048,576 登记。
var knownModelCapabilities = map[string]ModelCapability{
	"glm-5.3":             {ContextWindow: 1048576, MaxTokens: 131072},
	"deepseek-v4.1-flash": {ContextWindow: 1048576, MaxTokens: 32768},
}

// LookupModelCapability 返回模型已知能力。名称可以是别名，允许带 provider 前缀。
func LookupModelCapability(name string) (ModelCapability, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return ModelCapability{}, false
	}
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	if cap, ok := knownModelCapabilities[n]; ok {
		return cap, true
	}
	// 兼容管理端别名后缀（如 glm-5.3-admin）。
	if i := strings.Index(n, "-admin"); i > 0 {
		if cap, ok := knownModelCapabilities[n[:i]]; ok {
			return cap, true
		}
	}
	return ModelCapability{}, false
}
