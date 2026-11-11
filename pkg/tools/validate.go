package tools

import (
	"fmt"
	"math"
)

// validateToolArgs 根据类 JSON Schema 的 map 校验 args。
// schema 预期的可选键包括："properties"、"required"、"additionalProperties"。
func validateToolArgs(schema map[string]any, args map[string]any) error {
	if len(schema) == 0 {
		return nil
	}

	if args == nil {
		args = map[string]any{}
	}

	if err := checkRequired(schema, args); err != nil {
		return err
	}

	propsRaw, ok := schema["properties"]
	if !ok {
		return nil // 未定义 properties —— 接受任意 args
	}

	props, ok := propsRaw.(map[string]any)
	if !ok {
		return nil
	}

	additional := allowsAdditional(schema)

	for key, val := range args {
		propSchemaRaw, known := props[key]
		if !known {
			if !additional {
				return fmt.Errorf("unexpected property %q", key)
			}
			continue
		}
		propSchema, ok := propSchemaRaw.(map[string]any)
		if !ok {
			continue // 没有正确的 schema map 就无法校验
		}
		if err := checkType(key, val, propSchema); err != nil {
			return err
		}
	}

	return nil
}

// checkRequired 校验 schema["required"] 中列出的每个字段都存在于 args 中。
func checkRequired(schema map[string]any, args map[string]any) error {
	reqRaw, ok := schema["required"]
	if !ok {
		return nil
	}

	var required []string

	switch r := reqRaw.(type) {
	case []string:
		required = r
	case []any:
		for _, v := range r {
			s, ok := v.(string)
			if ok {
				required = append(required, s)
			}
		}
	default:
		return nil
	}

	for _, field := range required {
		if _, present := args[field]; !present {
			return fmt.Errorf("missing required property %q", field)
		}
	}
	return nil
}

// 当 schema 显式把 "additionalProperties" 设为 true，或者该键不存在时，
// allowsAdditional 返回 true（默认：拒绝多余属性）。
func allowsAdditional(schema map[string]any) bool {
	v, ok := schema["additionalProperties"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// checkType 校验 val 是否匹配 propSchema 中声明的 JSON Schema 类型。
func checkType(key string, val any, propSchema map[string]any) error {
	typeRaw, ok := propSchema["type"]
	if !ok {
		return nil // 无类型约束
	}
	typeName, ok := typeRaw.(string)
	if !ok {
		return nil
	}

	switch typeName {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("property %q: expected string, got %T", key, val)
		}
	case "integer":
		switch v := val.(type) {
		case float64:
			if v != math.Trunc(v) {
				return fmt.Errorf("property %q: expected integer, got float64 with fractional part", key)
			}
		case int:
			// ok
		case int64:
			// ok
		default:
			return fmt.Errorf("property %q: expected integer, got %T", key, val)
		}
	case "number":
		switch val.(type) {
		case float64, int, int64:
			// ok
		default:
			return fmt.Errorf("property %q: expected number, got %T", key, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("property %q: expected boolean, got %T", key, val)
		}
	case "array":
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("property %q: expected array, got %T", key, val)
		}
		if err := checkArrayItems(key, arr, propSchema); err != nil {
			return err
		}
	case "object":
		obj, ok := val.(map[string]any)
		if !ok {
			return fmt.Errorf("property %q: expected object, got %T", key, val)
		}
		if err := validateToolArgs(propSchema, obj); err != nil {
			return fmt.Errorf("property %q: %w", key, err)
		}
	}

	if err := checkEnum(key, val, propSchema); err != nil {
		return err
	}

	return nil
}

// checkArrayItems 根据 "items" 子 schema 校验 arr 的每个元素。
func checkArrayItems(key string, arr []any, propSchema map[string]any) error {
	itemsRaw, ok := propSchema["items"]
	if !ok {
		return nil
	}
	itemSchema, ok := itemsRaw.(map[string]any)
	if !ok {
		return nil
	}
	for i, elem := range arr {
		elemKey := fmt.Sprintf("%s[%d]", key, i)
		if err := checkType(elemKey, elem, itemSchema); err != nil {
			return err
		}
	}
	return nil
}

// checkEnum 校验 val 是 propSchema 中允许的枚举值之一。
func checkEnum(key string, val any, propSchema map[string]any) error {
	enumRaw, ok := propSchema["enum"]
	if !ok {
		return nil
	}

	switch ev := enumRaw.(type) {
	case []any:
		for _, allowed := range ev {
			if val == allowed {
				return nil
			}
		}
	case []string:
		s, ok := val.(string)
		if ok {
			for _, allowed := range ev {
				if s == allowed {
					return nil
				}
			}
		}
	default:
		return nil // 未知的 enum 格式，跳过
	}

	return fmt.Errorf("property %q: value %v is not in enum", key, val)
}
