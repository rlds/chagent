package config

import "encoding/json"

// loadConfig 在 DefaultConfig 之上解码一份规范的 v2 JSON 配置。目前恰好只支持
// 一种磁盘存档格式；这里不存在任何迁移分支。
func loadConfig(data []byte) (*Config, error) {
	cfg := DefaultConfig()

	// 预扫描 model_list，避免缺失的字段继承模板中同一切片索引的条目。
	var tmp Config
	if err := decodeJSONWithDiagnostics(data, &tmp, "config.json"); err != nil {
		return nil, err
	}
	if len(tmp.ModelList) > 0 {
		cfg.ModelList = nil
	}
	if err := decodeJSONWithDiagnostics(data, cfg, "config.json"); err != nil {
		return nil, err
	}
	return cfg, nil
}

func configObjectHasTopLevelField(data []byte, field string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	_, ok := raw[field]
	return ok
}

func configObjectHasField(data []byte, objectField, nestedField string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	objectData, ok := raw[objectField]
	if !ok {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(objectData, &object); err != nil {
		return false
	}
	_, ok = object[nestedField]
	return ok
}

// rawMCPExplicitlyDisabled 报告原始配置字节是否将 tools.mcp.enabled 显式设为
// JSON false。缺失该键与 JSON null 都视为“未提供”，这样仅配置 servers 的配置
// 仍会保留自动启用的行为。
func rawMCPExplicitlyDisabled(data []byte) bool {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false
	}
	toolsData, ok := top["tools"]
	if !ok {
		return false
	}
	var tools map[string]json.RawMessage
	if err := json.Unmarshal(toolsData, &tools); err != nil {
		return false
	}
	mcpData, ok := tools["mcp"]
	if !ok {
		return false
	}
	var mcp map[string]json.RawMessage
	if err := json.Unmarshal(mcpData, &mcp); err != nil {
		return false
	}
	enabledData, ok := mcp["enabled"]
	if !ok {
		return false
	}
	var enabled *bool
	if err := json.Unmarshal(enabledData, &enabled); err != nil || enabled == nil {
		return false
	}
	return !*enabled
}
