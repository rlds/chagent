package common

import (
	"fmt"
	"strings"
)

const (
	ToolSchemaTransformOff    = ""
	ToolSchemaTransformSimple = "simple"
)

// NormalizeToolSchemaTransform 将面向用户的别名解析为规范的转换模式。
// 空值和显式的 "off" 类取值都会禁用 schema 转换。
func NormalizeToolSchemaTransform(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "off", "none", "native":
		return ToolSchemaTransformOff, nil
	case "simple", "basic", "strict", "flat":
		return ToolSchemaTransformSimple, nil
	default:
		return "", fmt.Errorf("unsupported tool_schema_transform %q (supported: off, simple)", raw)
	}
}

// TransformToolDefinitions 克隆工具定义，并对函数参数 schema 应用所配置的
// schema 转换。当转换为关闭状态时，原样返回原始切片。
func TransformToolDefinitions(tools []ToolDefinition, transform string) ([]ToolDefinition, error) {
	transform, err := NormalizeToolSchemaTransform(transform)
	if err != nil {
		return nil, err
	}
	if transform == ToolSchemaTransformOff || len(tools) == 0 {
		return tools, nil
	}

	out := make([]ToolDefinition, len(tools))
	for i, tool := range tools {
		out[i] = tool
		if tool.Type != "function" {
			continue
		}
		out[i].Function = tool.Function
		out[i].Function.Parameters = transformToolSchema(tool.Function.Parameters, transform)
	}

	return out, nil
}

func transformToolSchema(schema map[string]any, transform string) map[string]any {
	switch transform {
	case ToolSchemaTransformSimple:
		return SanitizeSchemaForGoogle(schema)
	default:
		return cloneGeminiSchemaMap(schema)
	}
}
