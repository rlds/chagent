// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package config

import (
	"reflect"
	"strings"
	"sync"
)

// SensitiveDataCache 缓存用于过滤敏感数据的 strings.Replacer。
// 通过 sync.Once 在首次访问时计算一次。
type SensitiveDataCache struct {
	replacer *strings.Replacer
	once     sync.Once
}

// SensitiveDataReplacer 返回用于过滤敏感数据的 strings.Replacer。
// 它通过 sync.Once 在首次访问时计算一次。
func (sec *Config) SensitiveDataReplacer() *strings.Replacer {
	sec.initSensitiveCache()
	return sec.sensitiveCache.replacer
}

// initSensitiveCache 初始化敏感数据缓存（若尚未初始化）。
func (sec *Config) initSensitiveCache() {
	if sec.sensitiveCache == nil {
		sec.sensitiveCache = &SensitiveDataCache{}
	}
	sec.sensitiveCache.once.Do(func() {
		values := sec.collectSensitiveValues()
		if len(values) == 0 {
			sec.sensitiveCache.replacer = strings.NewReplacer()
			return
		}

		// 为 strings.Replacer 构建 old/new 对
		var pairs []string
		for _, v := range values {
			if len(v) > 3 {
				pairs = append(pairs, v, "[FILTERED]")
			}
		}
		if len(pairs) == 0 {
			sec.sensitiveCache.replacer = strings.NewReplacer()
			return
		}
		sec.sensitiveCache.replacer = strings.NewReplacer(pairs...)
	})
}

// collectSensitiveValues 使用反射从 SecurityConfig 中收集所有敏感字符串。
func (sec *Config) collectSensitiveValues() []string {
	var values []string
	collectSensitive(reflect.ValueOf(sec), &values)
	return values
}

// collectSensitive 递归遍历值并收集 SecureString/SecureStrings 的值。
func collectSensitive(v reflect.Value, values *[]string) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}

	t := v.Type()

	// Channel：使用 CollectSensitiveValues() 方法
	if t == reflect.TypeOf(Channel{}) {
		if method := v.MethodByName("CollectSensitiveValues"); method.IsValid() {
			results := method.Call(nil)
			if len(results) > 0 {
				if vals, ok := results[0].Interface().([]string); ok {
					*values = append(*values, vals...)
				}
			}
		}
		return
	}

	// SecureString：通过 String() 方法收集（该方法定义在 *SecureString 上）
	if t == reflect.TypeOf(SecureString{}) {
		// 创建一个新指针，使其可寻址以便调用方法
		ptr := reflect.New(t)
		ptr.Elem().Set(v)
		result := ptr.MethodByName("String").Call(nil)
		if len(result) > 0 {
			if s := result[0].String(); s != "" {
				*values = append(*values, s)
			}
		}
		return
	}

	// SecureStrings（[]*SecureString）：遍历并收集每个元素
	if t == reflect.TypeOf(SecureStrings{}) {
		for i := 0; i < v.Len(); i++ {
			elem := v.Index(i)
			for elem.Kind() == reflect.Ptr || elem.Kind() == reflect.Interface {
				if elem.IsNil() {
					elem = reflect.Value{}
					break
				}
				elem = elem.Elem()
			}
			if elem.IsValid() && elem.Type() == reflect.TypeOf(SecureString{}) {
				result := elem.Addr().MethodByName("String").Call(nil)
				if len(result) > 0 {
					if s := result[0].String(); s != "" {
						*values = append(*values, s)
					}
				}
			}
		}
		return
	}

	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			collectSensitive(v.Field(i), values)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			collectSensitive(v.Index(i), values)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			collectSensitive(v.MapIndex(key), values)
		}
	}
}
