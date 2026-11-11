package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/rlds/chagent/pkg/credential"
	"github.com/rlds/chagent/pkg/logger"
)

// FlexibleStringSlice 是一个 []string，同时也能接受 JSON 数字，
// 因此 allow_from 里既可以写 "123"，也可以写 123。
// 它还支持解析环境变量中以逗号分隔的字符串，
// 同时兼容英文逗号（,）和中文逗号（，）。
type FlexibleStringSlice []string

func (f *FlexibleStringSlice) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*f = nil
		return nil
	}

	// 为方便起见，也接受单个 JSON 字符串，例如：
	// "text": "Thinking..."
	var singleString string
	if err := json.Unmarshal(data, &singleString); err == nil {
		*f = FlexibleStringSlice{singleString}
		return nil
	}

	// 也接受单个 JSON 数字，以便与可能包含数字标识符的混合 allow_from
	// 载荷保持对称。
	var singleNumber float64
	if err := json.Unmarshal(data, &singleNumber); err == nil {
		*f = FlexibleStringSlice{fmt.Sprintf("%.0f", singleNumber)}
		return nil
	}

	// 先尝试 []string
	var ss []string
	if err := json.Unmarshal(data, &ss); err == nil {
		*f = ss
		return nil
	}

	// 再尝试 []interface{} 以处理混合类型
	var raw []any
	if err := json.Unmarshal(data, &raw); err != nil {
		var s string
		// 回退到兼容旧格式的字符串
		if err = json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = []string{s}
		return nil
	}

	result := make([]string, 0, len(raw))
	for _, v := range raw {
		switch val := v.(type) {
		case string:
			result = append(result, val)
		case float64:
			result = append(result, fmt.Sprintf("%.0f", val))
		default:
			result = append(result, fmt.Sprintf("%v", val))
		}
	}
	*f = result
	return nil
}

// UnmarshalText 实现 encoding.TextUnmarshaler，以支持环境变量解析。
// 它处理以英文逗号（,）和中文逗号（，）分隔的值。
func (f *FlexibleStringSlice) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*f = nil
		return nil
	}

	s := string(text)
	// 将中文逗号替换为英文逗号，然后拆分
	s = strings.ReplaceAll(s, "，", ",")
	parts := strings.Split(s, ",")

	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	*f = result
	return nil
}

const (
	notHere = `"[NOT_HERE]"`
)

// SecureStrings 是 SecureString 的切片
//
//nolint:recvcheck
type SecureStrings []*SecureString

// IsZero 在 SecureStrings 为 nil 或空时返回 true。
func (s SecureStrings) IsZero() bool {
	if !callerFromYaml() {
		return true
	}
	return len(s) == 0
}

// Values 返回解密/解析后的值
func (s *SecureStrings) Values() []string {
	if s == nil {
		return nil
	}
	keys := make([]string, len(*s))
	for i, k := range *s {
		keys[i] = k.String()
	}
	return unique(keys)
}

func SimpleSecureStrings(val ...string) SecureStrings {
	val = unique(val)
	vv := make(SecureStrings, len(val))
	for i, s := range val {
		vv[i] = NewSecureString(s)
	}
	return vv
}

// unique 返回一个去除重复元素后的新切片。
func unique[T comparable](input []T) []T {
	m := make(map[T]struct{})
	var result []T
	for _, v := range input {
		if _, ok := m[v]; !ok {
			m[v] = struct{}{}
			result = append(result, v)
		}
	}
	return result
}

func (s SecureStrings) MarshalJSON() ([]byte, error) {
	return []byte(notHere), nil
}

func (s *SecureStrings) UnmarshalJSON(value []byte) error {
	if string(value) == notHere {
		return nil
	}
	var v []*SecureString
	err := json.Unmarshal(value, &v)
	if err != nil {
		return err
	}
	// 过滤掉 SecureString.UnmarshalJSON 未做实际处理的元素
	// （例如 "[NOT_HERE]" 条目），只保留真正有值的条目。
	filtered := make(SecureStrings, 0, len(v))
	for _, ss := range v {
		if ss == nil {
			continue
		}
		if ss.resolved != "" || ss.raw != "" {
			filtered = append(filtered, ss)
		}
	}
	if len(filtered) == 0 {
		*s = nil
	} else {
		*s = filtered
	}
	return nil
}

// SecureString 表示可以被解密或解析的字符串值
//
//nolint:recvcheck
type SecureString struct {
	resolved string // String() 返回的解密/解析后的值
	raw      string // 持久化的原始值（enc://、file:// 或明文）
}

func callerFromYaml() bool {
	_, file, _, ok := runtime.Caller(2)
	if ok {
		d := filepath.Dir(file)
		// 检查调用方是否来自 yaml.v
		if !strings.Contains(d, "yaml.v") {
			return false
		}
	}
	return true
}

// IsZero 在 SecureString 为空时返回 true
// 如果调用方不是 yaml，则直接返回 true，以避免序列化该字段
func (s SecureString) IsZero() bool {
	if !callerFromYaml() {
		return true
	}
	return s.resolved == ""
}

func NewSecureString(value string) *SecureString {
	s := &SecureString{}
	if err := s.fromRaw(value); err != nil {
		logger.Warn(fmt.Sprintf("NewSecureString.fromRaw error: %s", err))
	}
	return s
}

func (s *SecureString) String() string {
	if s == nil {
		return ""
	}
	return s.resolved
}

func (s *SecureString) Set(value string) *SecureString {
	s.resolved = value
	s.raw = ""
	return s
}

func (s SecureString) MarshalJSON() ([]byte, error) {
	return []byte(notHere), nil
}

func (s *SecureString) UnmarshalJSON(value []byte) error {
	if string(value) == notHere {
		return nil
	}
	var v string
	if err := json.Unmarshal(value, &v); err != nil {
		return err
	}
	return s.fromRaw(v)
}

func (s SecureString) MarshalYAML() (any, error) {
	// 如果 raw 已是引用形式（enc:// 或 file://），则保留原值
	if strings.HasPrefix(s.raw, credential.EncScheme) || strings.HasPrefix(s.raw, credential.FileScheme) {
		return s.raw, nil
	}
	// 如果 resolved 是引用形式（例如通过 Set 设置），则回写到 raw
	if strings.HasPrefix(s.resolved, credential.EncScheme) || strings.HasPrefix(s.resolved, credential.FileScheme) {
		s.raw = s.resolved
		return s.raw, nil
	}
	// 尝试加密 resolved 的值
	if passphrase := credential.PassphraseProvider(); passphrase != "" {
		encrypted, err := credential.Encrypt(passphrase, "", s.resolved)
		if err != nil {
			logger.Errorf("Encrypt error: %v", err)
			return nil, err
		}
		s.raw = encrypted
	} else {
		s.raw = s.resolved
	}
	return s.raw, nil
}

func (s *SecureString) UnmarshalYAML(value *yaml.Node) error {
	return s.fromRaw(value.Value)
}

func (s *SecureString) fromRaw(v string) error {
	s.raw = v
	vv, err := resolveKey(v)
	if err != nil {
		return err
	}
	s.resolved = vv
	return nil
}

var (
	secResolverMu sync.RWMutex
	secResolver   *credential.Resolver
)

func updateResolver(path string) {
	secResolverMu.Lock()
	defer secResolverMu.Unlock()
	secResolver = credential.NewResolver(path)
}

func resolveKey(v string) (string, error) {
	secResolverMu.RLock()
	resolver := secResolver
	secResolverMu.RUnlock()
	if resolver == nil {
		resolver = credential.NewResolver("")
	}
	if strings.HasPrefix(v, "enc://") || strings.HasPrefix(v, "file://") {
		decrypted, err := resolver.Resolve(v)
		if err != nil {
			logger.Errorf("Resolve error: %v", err)
			return "", err
		}
		return decrypted, nil
	}
	return v, nil
}

func (s *SecureString) UnmarshalText(text []byte) error {
	v := string(text)
	return s.fromRaw(v)
}

type SecureModelList []*ModelConfig

func (v *SecureModelList) UnmarshalYAML(value *yaml.Node) error {
	mm := make(map[string]*ModelConfig)
	if err := value.Decode(&mm); err != nil {
		logger.Errorf("Decode error: %v", err)
		return err
	}
	nameList := toNameIndex(*v)
	for i, m := range *v {
		sec := mm[nameList[i]]
		if sec == nil {
			sec = mm[m.ModelName]
		}
		if sec != nil {
			m.APIKeys = sec.APIKeys
		}
	}
	return nil
}

func (v SecureModelList) MarshalYAML() (any, error) {
	type onlySecureData struct {
		APIKeys SecureStrings `yaml:"api_keys,omitempty"`
	}
	mm := make(map[string]onlySecureData)
	nameList := toNameIndex(v)
	for i, m := range v {
		mm[nameList[i]] = onlySecureData{
			APIKeys: m.APIKeys,
		}
	}

	return mm, nil
}

func (v *SkillsRegistriesConfig) UnmarshalJSON(data []byte) error {
	var list []json.RawMessage
	if err := json.Unmarshal(data, &list); err == nil {
		decodedList := make([]*SkillRegistryConfig, 0, len(list))
		for _, item := range list {
			var nameOnly struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(item, &nameOnly); err != nil {
				return err
			}
			registry := cloneRegistryConfig(findRegistryConfigByName(*v, nameOnly.Name))
			if registry == nil {
				registry = &SkillRegistryConfig{Name: nameOnly.Name}
			}
			if err := json.Unmarshal(item, registry); err != nil {
				return err
			}
			decodedList = append(decodedList, registry)
		}
		if len(*v) > 0 {
			for _, registry := range decodedList {
				if registry == nil {
					continue
				}
				v.Set(registry.Name, *registry)
			}
			return nil
		}
		*v = decodedList
		return nil
	}

	legacy := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}

	if len(*v) == 0 {
		keys := make([]string, 0, len(legacy))
		for name := range legacy {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		decodedList := make([]*SkillRegistryConfig, 0, len(keys))
		for _, name := range keys {
			var registry SkillRegistryConfig
			if err := json.Unmarshal(legacy[name], &registry); err != nil {
				return err
			}
			registry.Name = name
			decodedList = append(decodedList, &registry)
		}
		*v = decodedList
		return nil
	}

	for _, name := range sortedRegistryNamesFromJSON(legacy) {
		registry := cloneRegistryConfig(findRegistryConfigByName(*v, name))
		if registry == nil {
			registry = &SkillRegistryConfig{Name: name}
		}
		if err := json.Unmarshal(legacy[name], registry); err != nil {
			return err
		}
		registry.Name = name
		v.Set(name, *registry)
	}
	return nil
}

func (v SkillsRegistriesConfig) MarshalJSON() ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	mm := make(map[string]SkillRegistryConfig, len(v))
	for _, registry := range v {
		if registry == nil || registry.Name == "" {
			continue
		}
		mm[registry.Name] = *registry
	}
	return json.Marshal(mm)
}

func (c *SkillRegistryConfig) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	params := cloneRegistryParams(c.Param)
	if params == nil {
		params = map[string]any{}
	}
	if value, ok := raw["name"]; ok {
		if err := json.Unmarshal(value, &c.Name); err != nil {
			return err
		}
	}
	if value, ok := raw["enabled"]; ok {
		if err := json.Unmarshal(value, &c.Enabled); err != nil {
			return err
		}
	}
	if value, ok := raw["base_url"]; ok {
		if err := json.Unmarshal(value, &c.BaseURL); err != nil {
			return err
		}
	}
	if value, ok := raw["auth_token"]; ok {
		if err := json.Unmarshal(value, &c.AuthToken); err != nil {
			return err
		}
	}
	if value, ok := raw["param"]; ok {
		var nested map[string]any
		if err := json.Unmarshal(value, &nested); err != nil {
			return err
		}
		for key, nestedValue := range nested {
			params[key] = nestedValue
		}
	}
	for key, value := range raw {
		switch key {
		case "name", "enabled", "base_url", "auth_token", "param":
			continue
		case "_auth_token":
			// UI/API 的影子密钥字段只应用于填充 SecureString，
			// 绝不能作为任意注册表参数被持久化。
			continue
		default:
			var decoded any
			if err := json.Unmarshal(value, &decoded); err != nil {
				return err
			}
			params[key] = decoded
		}
	}
	c.Param = params
	return nil
}

func (c SkillRegistryConfig) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"enabled":  c.Enabled,
		"base_url": c.BaseURL,
	}
	if c.AuthToken.String() != "" {
		m["auth_token"] = c.AuthToken
	}
	for key, value := range c.Param {
		if key == "" || key == "param" || strings.HasPrefix(key, "_") {
			continue
		}
		if _, exists := m[key]; exists {
			continue
		}
		m[key] = value
	}
	return json.Marshal(m)
}

func (c *SkillRegistryConfig) UnmarshalYAML(value *yaml.Node) error {
	var raw map[string]any
	if err := value.Decode(&raw); err != nil {
		return err
	}
	params := cloneRegistryParams(c.Param)
	if params == nil {
		params = map[string]any{}
	}
	if nested, ok := raw["param"].(map[string]any); ok {
		for k, v := range nested {
			params[k] = v
		}
	}
	for key, v := range raw {
		switch key {
		case "name":
			if s, ok := v.(string); ok {
				c.Name = s
			}
		case "enabled":
			if b, ok := v.(bool); ok {
				c.Enabled = b
			}
		case "base_url":
			if s, ok := v.(string); ok {
				c.BaseURL = s
			}
		case "auth_token":
			data, err := yaml.Marshal(v)
			if err != nil {
				return err
			}
			if err := yaml.Unmarshal(data, &c.AuthToken); err != nil {
				return err
			}
		case "_auth_token":
			// UI/API 的影子密钥字段只应用于填充 SecureString，
			// 绝不能作为任意注册表参数被持久化。
			continue
		case "param":
			continue
		default:
			params[key] = v
		}
	}
	c.Param = params
	return nil
}

func (c SkillRegistryConfig) MarshalYAML() (any, error) {
	m := map[string]any{
		"enabled":  c.Enabled,
		"base_url": c.BaseURL,
	}
	if c.AuthToken.String() != "" {
		m["auth_token"] = c.AuthToken
	}
	keys := make([]string, 0, len(c.Param))
	for key := range c.Param {
		if key == "" || key == "param" || strings.HasPrefix(key, "_") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, exists := m[key]; exists {
			continue
		}
		m[key] = c.Param[key]
	}
	return m, nil
}

func (v *SkillsRegistriesConfig) UnmarshalYAML(value *yaml.Node) error {
	decoded, err := decodeRegistryNodesFromYAML(value, nil)
	if err != nil {
		logger.Errorf("Decode error: %v", err)
		return err
	}
	if len(*v) == 0 {
		keys := make([]string, 0, len(decoded))
		for name := range decoded {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		list := make([]*SkillRegistryConfig, 0, len(keys))
		for _, name := range keys {
			registry := decoded[name]
			if registry == nil {
				continue
			}
			list = append(list, registry)
		}
		*v = list
		return nil
	}
	decoded, err = decodeRegistryNodesFromYAML(value, *v)
	if err != nil {
		logger.Errorf("Decode error: %v", err)
		return err
	}
	for _, name := range sortedRegistryNames(decoded) {
		registry := decoded[name]
		if registry == nil {
			continue
		}
		v.Set(name, *registry)
	}
	return nil
}

func decodeRegistryNodesFromYAML(
	value *yaml.Node,
	existing SkillsRegistriesConfig,
) (map[string]*SkillRegistryConfig, error) {
	decoded := make(map[string]*SkillRegistryConfig)
	if value == nil {
		return decoded, nil
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		nameNode := value.Content[i]
		registryNode := value.Content[i+1]
		if nameNode == nil || registryNode == nil {
			continue
		}
		name := strings.TrimSpace(nameNode.Value)
		if name == "" {
			continue
		}
		registry := cloneRegistryConfig(findRegistryConfigByName(existing, name))
		if registry == nil {
			registry = &SkillRegistryConfig{Name: name}
		}
		if err := registryNode.Decode(registry); err != nil {
			return nil, err
		}
		registry.Name = name
		decoded[name] = registry
	}
	return decoded, nil
}

func cloneRegistryParams(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	cloned := make(map[string]any, len(src))
	for key, value := range src {
		cloned[key] = value
	}
	return cloned
}

func cloneRegistryConfig(src *SkillRegistryConfig) *SkillRegistryConfig {
	if src == nil {
		return nil
	}
	cloned := *src
	cloned.Param = cloneRegistryParams(src.Param)
	return &cloned
}

func findRegistryConfigByName(registries SkillsRegistriesConfig, name string) *SkillRegistryConfig {
	for _, registry := range registries {
		if registry == nil || registry.Name != name {
			continue
		}
		return registry
	}
	return nil
}

func sortedRegistryNames(mm map[string]*SkillRegistryConfig) []string {
	keys := make([]string, 0, len(mm))
	for name := range mm {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

func sortedRegistryNamesFromJSON(mm map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(mm))
	for name := range mm {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys
}

func (v SkillsRegistriesConfig) MarshalYAML() (any, error) {
	type onlySecureRegistryData struct {
		AuthToken SecureString `yaml:"auth_token,omitempty"`
	}
	mm := make(map[string]onlySecureRegistryData)
	for _, registry := range v {
		if registry == nil || registry.Name == "" {
			continue
		}
		if registry.AuthToken.String() == "" {
			continue
		}
		mm[registry.Name] = onlySecureRegistryData{
			AuthToken: registry.AuthToken,
		}
	}

	return mm, nil
}
