// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// normalizeSingularAPIKeys 把 model_list[*].api_key（v2/chess 的单数形式）
// 改写为 api_keys，以免该密钥在加载时被静默丢弃。已有的
// api_keys 条目会被保留；仅当该单数密钥尚未存在时才会追加它。
// 返回的字节仅在内存中使用。
func normalizeSingularAPIKeys(data []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return data, nil // 让常规加载器报告语法错误
	}
	modelListRaw, ok := top["model_list"]
	if !ok || len(modelListRaw) == 0 {
		return data, nil
	}

	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(modelListRaw, &entries); err != nil {
		return data, nil
	}

	changed := false
	for _, entry := range entries {
		if _, hasPlural := entry["api_keys"]; hasPlural {
			return nil, fmt.Errorf("model_list api_keys is not supported; use the singular api_key")
		}
		singularRaw, hasSingular := entry["api_key"]
		if !hasSingular {
			continue
		}
		var singular string
		if err := json.Unmarshal(singularRaw, &singular); err != nil {
			continue
		}
		singular = strings.TrimSpace(singular)
		if singular == "" {
			delete(entry, "api_key")
			changed = true
			continue
		}

		var keys []string
		if keysRaw, hasKeys := entry["api_keys"]; hasKeys {
			if err := json.Unmarshal(keysRaw, &keys); err != nil {
				keys = nil
			}
		}
		seen := make(map[string]struct{}, len(keys)+1)
		merged := make([]string, 0, len(keys)+1)
		for _, key := range keys {
			trimmed := strings.TrimSpace(key)
			if trimmed == "" {
				continue
			}
			if _, dup := seen[trimmed]; dup {
				continue
			}
			seen[trimmed] = struct{}{}
			merged = append(merged, trimmed)
		}
		if _, dup := seen[singular]; !dup {
			merged = append(merged, singular)
		}

		encoded, err := json.Marshal(merged)
		if err != nil {
			continue
		}
		entry["api_keys"] = encoded
		delete(entry, "api_key")
		changed = true
	}

	if !changed {
		return data, nil
	}

	encodedEntries, err := json.Marshal(entries)
	if err != nil {
		return data, nil
	}
	top["model_list"] = encodedEntries
	encoded, err := json.Marshal(top)
	if err != nil {
		return data, nil
	}
	return encoded, nil
}

// inferDefaultModel 在配置中 agents.defaults.model_name 为空时
// （chess 可能会清空它）从 model_list 中填充该值：优先选择第一个已启用的条目，
// 否则选择第一个条目。model_list 为空时返回错误。
func inferDefaultModel(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	if strings.TrimSpace(cfg.Agents.Defaults.ModelName) != "" {
		return nil
	}
	if len(cfg.ModelList) == 0 {
		// 加载过程不能失败：gateway 会在启动时报告缺失的模型
		// （或以 --allow-empty 受限模式启动）。chess 始终会
		// 提供 model_list。
		return nil
	}
	pick := cfg.ModelList[0]
	for _, model := range cfg.ModelList {
		if model != nil && model.Enabled {
			pick = model
			break
		}
	}
	if pick != nil && strings.TrimSpace(pick.ModelName) != "" {
		cfg.Agents.Defaults.ModelName = pick.ModelName
	}
	return nil
}

// inferDefaultProvider 在 agents.defaults.provider 为空时，
// 依据所选模型配置推导并填充它：优先使用显式的 model_list[*].provider，
// 其次是模型标识符中的提供商前缀（"openai/gpt-4" → "openai"），
// 最后对纯模型 id 使用 "openai"。
func inferDefaultProvider(cfg *Config) {
	if cfg == nil || strings.TrimSpace(cfg.Agents.Defaults.Provider) != "" {
		return
	}
	modelName := strings.TrimSpace(cfg.Agents.Defaults.ModelName)
	for _, model := range cfg.ModelList {
		if model == nil || model.ModelName != modelName {
			continue
		}
		if provider := strings.TrimSpace(model.Provider); provider != "" {
			cfg.Agents.Defaults.Provider = provider
			return
		}
		if protocol := providerPrefixFromModel(model.Model); protocol != "" {
			cfg.Agents.Defaults.Provider = protocol
			return
		}
		cfg.Agents.Defaults.Provider = "openai"
		return
	}
}

// providerPrefixFromModel 返回模型标识符的提供商前缀，
// 例如 "openai/gpt-4" → "openai"，"nvidia/z-ai/glm-5.1" → "nvidia"。
// 纯模型 id 返回 ""，以便调用方回退到默认协议。
func providerPrefixFromModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	prefix, rest, found := strings.Cut(model, "/")
	if !found || prefix == "" || rest == "" {
		return ""
	}
	return prefix
}

// hasExplicitWorkspace 报告已加载的配置/环境是否显式提供了工作区。
// 返回 false 时会应用回退链。
func hasExplicitWorkspace(data []byte) bool {
	if strings.TrimSpace(os.Getenv("CHAGENT_AGENTS_DEFAULTS_WORKSPACE")) != "" {
		return true
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false
	}
	agentsRaw, ok := top["agents"]
	if !ok {
		return false
	}
	var agents map[string]json.RawMessage
	if err := json.Unmarshal(agentsRaw, &agents); err != nil {
		return false
	}
	defaultsRaw, ok := agents["defaults"]
	if !ok {
		return false
	}
	var defaults map[string]json.RawMessage
	if err := json.Unmarshal(defaultsRaw, &defaults); err != nil {
		return false
	}
	workspaceRaw, ok := defaults["workspace"]
	if !ok {
		return false
	}
	var workspace string
	if err := json.Unmarshal(workspaceRaw, &workspace); err != nil {
		return false
	}
	return strings.TrimSpace(workspace) != ""
}

// defaultWorkspaceChain 在配置省略工作区时解析工作区：
// CHAGENT_WORKSPACE → CHAGENT_CONFIG 所在目录（/ 已加载的配置文件所在目录）
// → 进程当前工作目录 → $HOME/.chagent/workspace。
func defaultWorkspaceChain(configPath string) string {
	if ws := strings.TrimSpace(os.Getenv(EnvWorkspace)); ws != "" {
		return ws
	}

	configDir := ""
	if envConfig := strings.TrimSpace(os.Getenv(EnvConfig)); envConfig != "" {
		configDir = filepath.Dir(envConfig)
	} else if strings.TrimSpace(configPath) != "" {
		configDir = filepath.Dir(configPath)
	}
	if configDir != "" {
		return configDir
	}

	if cwd, err := os.Getwd(); err == nil && strings.TrimSpace(cwd) != "" {
		return cwd
	}

	return filepath.Join(GetHome(), ".chagent-workspace")
}
