// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package providers

import (
	"fmt"

	"github.com/rlds/chagent/pkg/config"
)

// CreateProvider 根据配置创建 provider。
// 它使用 model_list 配置（新格式）来创建 provider。
// 旧版 providers 配置会在加载配置时自动转换为 model_list。
// 返回 provider、要使用的模型 ID 以及可能出现的错误。
func CreateProvider(cfg *config.Config) (LLMProvider, string, error) {
	model := cfg.Agents.Defaults.GetModelName()

	// 此时必须已配置 model_list
	if len(cfg.ModelList) == 0 {
		return nil, "", fmt.Errorf("no providers configured. Please add entries to model_list in your config")
	}

	// 从 model_list 获取模型配置
	modelCfg, err := cfg.GetModelConfig(model)
	if err != nil {
		return nil, "", fmt.Errorf("model %q not found in model_list: %w", model, err)
	}

	// 若模型配置中未设置工作区，则注入全局工作区
	if modelCfg.Workspace == "" {
		modelCfg.Workspace = cfg.WorkspacePath()
	}

	// 使用工厂创建 provider
	provider, modelID, err := CreateProviderFromConfig(modelCfg)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create provider for model %q: %w", model, err)
	}

	return provider, modelID, nil
}
