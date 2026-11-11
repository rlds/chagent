package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/rlds/chagent/pkg/logger"
)

// 通道类型常量 —— 所有通道类型名称的唯一事实来源。
const (
	ChannelPico       = "pico"
	ChannelPicoClient = "pico_client"
)

func initChannel() {
	registerSingletonChannel(ChannelPico)
	registerSingletonChannel(ChannelPicoClient)
}

// singletonRegistry 存储哪些通道类型是单例（只允许一个实例）。
// 如果某个通道类型是单例，应在它的 init() 中调用 registerSingletonChannel。
var singletonRegistry = make(map[string]struct{})

// registerSingletonChannel 将通道类型标记为单例（只允许一个实例）。
// 应从该通道类型的 init() 函数中调用。
func registerSingletonChannel(channelType string) {
	singletonRegistry[channelType] = struct{}{}
}

// IsSingletonChannel 返回该通道类型是否只允许一个实例。
func IsSingletonChannel(channelType string) bool {
	_, ok := singletonRegistry[channelType]
	return ok
}

// RawNode 以 JSON 字节形式存储原始配置数据，同时支持 JSON 和 YAML。
// 内部使用 json.RawMessage，因此 Decode 始终使用 json.Unmarshal，
// 从而正确遵循 json 结构体标签。
type RawNode json.RawMessage

// UnmarshalJSON 实现 json.Unmarshaler：存储原始 JSON 字节。
// 注意：在将 YAML 反序列化到 RawNode 字段时，yaml.Unmarshal 可能会调用此方法。
// 我们会检测输入是否看起来像 YAML（而非 JSON）并做相应处理。
func (r *RawNode) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "{}" || trimmed == "[]" {
		*r = nil
		return nil
	}

	// 如果输入看起来不像 JSON（以 {、[、"、数字、n、t、f 开头），
	// 则很可能是经由 yaml.Unmarshal 传入的 YAML 数据。
	// 尝试将其解析为 YAML 并转换为 JSON。
	if len(trimmed) > 0 {
		first := trimmed[0]
		if first != '{' && first != '[' && first != '"' && first != '-' &&
			!(first >= '0' && first <= '9') && first != 'n' && first != 't' && first != 'f' {
			// 看起来是 YAML 而非 JSON。按 YAML 解析并转换为 JSON。
			var v any
			if err := yaml.Unmarshal(data, &v); err != nil {
				return err
			}
			jsonData, err := json.Marshal(v)
			if err != nil {
				return err
			}
			*r = jsonData
			return nil
		}
	}

	*r = append((*r)[:0:0], data...)
	return nil
}

// MarshalJSON 实现 json.Marshaler：输出已存储的 JSON 字节。
func (r RawNode) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// UnmarshalYAML 实现 yaml.Unmarshaler：将 YAML 节点转换为 JSON 字节。
// 将传入的 YAML 值与现有数据合并，YAML 优先。
func (r *RawNode) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == 0 {
		//*r = nil
		return nil
	}
	var v1, v2 map[string]any
	if len(*r) > 0 {
		if err := json.Unmarshal(*r, &v1); err != nil {
			return err
		}
	}
	if err := value.Decode(&v2); err != nil {
		return err
	}
	v := mergeMap(v1, v2)
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	*r = data
	return nil
}

// mergeMap 深度合并两个 map[string]any。
// dst：基础 map
// src：覆盖 map（相同键会覆盖 dst，嵌套 map 会递归合并）
// 返回一个新 map，不修改原 map。
func mergeMap(dst, src map[string]any) map[string]any {
	// logger.Infof("mergeMap: dst: %v, src: %v", dst, src)
	// 创建结果 map，避免修改原 map
	result := make(map[string]any)

	// 复制基础 map 的全部内容
	for k, v := range dst {
		result[k] = v
	}

	// 合并覆盖 map
	for k, srcVal := range src {
		dstVal, exists := result[k]

		if !exists {
			// 基础 map 中不存在该键，直接添加
			result[k] = srcVal
			continue
		}

		// 两者都是 map → 递归合并
		dstMap, dstIsMap := toMap(dstVal)
		srcMap, srcIsMap := toMap(srcVal)

		if dstIsMap && srcIsMap {
			result[k] = mergeMap(dstMap, srcMap)
		} else {
			// 并非两者都是 map → 覆盖
			result[k] = srcVal
		}
	}

	return result
}

// toMap 安全地将任意值转换为 map[string]any。
func toMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// MarshalYAML 实现 yaml.ValueMarshaler：将存储的 JSON 转换回 YAML 兼容的值。
func (r RawNode) MarshalYAML() (any, error) {
	if len(r) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(r, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// Decode 使用 json.Unmarshal 将存储的数据反序列化到给定的目标结构体。
func (r *RawNode) Decode(target any) error {
	if len(*r) == 0 {
		return nil
	}
	return json.Unmarshal(*r, target)
}

// IsEmpty 返回节点是否尚未填充。
func (r *RawNode) IsEmpty() bool {
	return len(*r) == 0
}

// Channel 定义所有通道类型共享的公共字段。
// 通道特有的设置放入 Settings（仅限嵌套格式）。
// 设置结构体中，敏感字段应使用 SecureString/SecureStrings。
//
// Decode 会在内部保存设置指针；此后对被解码结构体的修改
// 会自动反映到 MarshalJSON/MarshalYAML。
//
// MarshalJSON 输出嵌套格式（公共字段位于顶层，设置位于子键）。
// MarshalYAML 仅输出安全字段（用于 .security.yml）。
//
// 标准 Go JSON/YAML 反序列化可以正确处理嵌套格式：
//   - JSON: {"enabled": true, "type": "telegram", "settings": {"base_url": "..."}}
//   - YAML: settings: {token: xxx}（用于 .security.yml）
//
//nolint:recvcheck
type Channel struct {
	name               string
	Enabled            bool                `json:"enabled"                 yaml:"-"`
	Type               string              `json:"type"                    yaml:"-"`
	AllowFrom          FlexibleStringSlice `json:"allow_from,omitempty"    yaml:"-"`
	ReasoningChannelID string              `json:"reasoning_channel_id"    yaml:"-"`
	GroupTrigger       GroupTriggerConfig  `json:"group_trigger,omitempty" yaml:"-"`
	Typing             TypingConfig        `json:"typing,omitempty"        yaml:"-"`
	Placeholder        PlaceholderConfig   `json:"placeholder,omitempty"   yaml:"-"`
	Settings           RawNode             `json:"settings,omitzero"       yaml:"settings,omitempty"`
	extend             any
}

// UnmarshalJSON 接受规范的 chess 扁平通道结构
// （{"enabled":true,"token":"..."}）以及嵌套的 "settings" 结构。额外的
// 顶层键会变为通道设置；不存在迁移层。
func (b *Channel) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var common struct {
		Enabled            bool                `json:"enabled"`
		Type               string              `json:"type"`
		AllowFrom          FlexibleStringSlice `json:"allow_from"`
		ReasoningChannelID string              `json:"reasoning_channel_id"`
		GroupTrigger       GroupTriggerConfig  `json:"group_trigger"`
		Typing             TypingConfig        `json:"typing"`
		Placeholder        PlaceholderConfig   `json:"placeholder"`
		Settings           json.RawMessage     `json:"settings"`
	}
	if err := json.Unmarshal(data, &common); err != nil {
		return err
	}
	settings := make(map[string]json.RawMessage)
	if len(common.Settings) > 0 {
		if err := json.Unmarshal(common.Settings, &settings); err != nil {
			return fmt.Errorf("channel settings: %w", err)
		}
	}
	for key, value := range raw {
		switch key {
		case "enabled", "type", "allow_from", "reasoning_channel_id", "group_trigger", "typing", "placeholder", "settings":
			continue
		default:
			settings[key] = value
		}
	}
	merged := json.RawMessage(nil)
	if len(settings) > 0 {
		encoded, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		merged = encoded
	}
	*b = Channel{
		Enabled:            common.Enabled,
		Type:               common.Type,
		AllowFrom:          common.AllowFrom,
		ReasoningChannelID: common.ReasoningChannelID,
		GroupTrigger:       common.GroupTrigger,
		Typing:             common.Typing,
		Placeholder:        common.Placeholder,
		Settings:           RawNode(merged),
	}
	return nil
}

// MarshalJSON 为 Channel 实现 json.Marshaler。
// 输出嵌套格式：公共字段位于顶层，通道特有字段位于 "settings"。
// 安全字段（SecureString/SecureStrings）会从 settings 输出中移除。
func (b Channel) MarshalJSON() ([]byte, error) {
	var settings RawNode
	if b.extend != nil {
		raw, err := json.Marshal(b.extend)
		if err != nil {
			return nil, err
		}
		raw = preserveExplicitDisabledStreaming(raw, b.Settings)
		settings = raw
	} else {
		settings = b.Settings
	}

	out := b
	out.Settings = settings

	// 使用类型别名绕过自定义 MarshalJSON（无限递归）
	type Alias Channel
	return json.Marshal((*Alias)(&out))
}

func preserveExplicitDisabledStreaming(settings, original RawNode) RawNode {
	if len(original) == 0 || len(settings) == 0 {
		return settings
	}

	var originalMap map[string]any
	if err := json.Unmarshal(original, &originalMap); err != nil {
		return settings
	}
	originalStreaming, ok := originalMap["streaming"].(map[string]any)
	if !ok || originalStreaming["enabled"] != false {
		return settings
	}

	var settingsMap map[string]any
	if err := json.Unmarshal(settings, &settingsMap); err != nil {
		return settings
	}
	if _, exists := settingsMap["streaming"]; exists {
		return settings
	}
	settingsMap["streaming"] = map[string]any{"enabled": false}

	data, err := json.Marshal(settingsMap)
	if err != nil {
		return settings
	}
	return data
}

// MarshalYAML 为 Channel 实现 yaml.ValueMarshaler。
// 仅在 Settings YAML 中输出安全字段（用于 .security.yml）。
// 如果调用过 Decode，则从存储的 extend 序列化（会反映任何
// 修改）；否则回退到根据通道 Type 解码 Settings
// 以提取安全字段。
func (b Channel) MarshalYAML() (any, error) {
	decoded, _ := b.GetDecoded()
	return struct {
		Settings any `json:"settings,omitzero" yaml:"settings,omitempty"`
	}{
		Settings: decoded,
	}, nil
}

// Name 返回通道名称。
func (b *Channel) Name() string {
	return b.name
}

// SetName 设置通道名称。
func (b *Channel) SetName(name string) {
	b.name = name
}

// SetSecretField 按字段名在 Settings JSON 中设置安全字段的值。
// 注意：此方法仅操作原始 Settings。如果已调用 Decode()，
// 应优先直接修改类型化结构体 —— MarshalJSON 会从 extend 序列化。
func (b *Channel) SetSecretField(fieldName string, value SecureString) {
	var m map[string]any
	if err := json.Unmarshal(b.Settings, &m); err != nil {
		return
	}
	m[fieldName] = value
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	b.Settings = data
}

// Decode 将 Settings 节点解码到给定的目标结构体，并在内部保存
// 该指针。此后对目标的修改会
// 自动反映到 MarshalJSON/MarshalYAML（无需显式调用 Encode）。
func (b *Channel) Decode(target any) error {
	if target == nil {
		return fmt.Errorf("target is nil")
	}
	if err := b.Settings.Decode(target); err != nil {
		return err
	}
	b.extend = target
	return nil
}

// GetDecoded 返回此前解码得到的设置结构体。
// 如果尚未调用 Decode，则使用通道 Type 的原型进行惰性解码。
// 如果解码失败则返回错误；同时仍会返回解码值（可能为 nil），
// 以便调用方区分“尚未解码”和“解码失败”。
func (b *Channel) GetDecoded() (any, error) {
	if b.extend == nil {
		// 回退到基于原型的创建
		if target := newChannelSettings(b.Type); target != nil {
			if err := b.Decode(target); err != nil {
				return nil, fmt.Errorf("channel %q failed to decode settings: %w", b.name, err)
			}
		}
	}
	return b.extend, nil
}

// UnmarshalYAML 为 Channel 实现 yaml.Unmarshaler。
// 将 YAML 节点合并到现有 Channel。
// 同时支持嵌套格式（settings: {...}）和扁平格式（token: xxx）。
func (b *Channel) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == 0 {
		return nil
	}

	type alias Channel
	a := alias(*b)
	err := value.Decode(&a)
	if err != nil {
		logger.Errorf("decode yaml error: %v", err)
		return err
	}

	*b = *(*Channel)(&a)

	if len(b.Settings) > 0 {
		b.extend = nil
	}

	return nil
}

// SettingsIsEmpty 返回 Settings 是否尚未填充。
func (b *Channel) SettingsIsEmpty() bool {
	return b.Settings.IsEmpty()
}

// CollectSensitiveValues 返回此 Channel 已解码设置（extend）中的
// 所有敏感字符串值。由安全过滤系统使用。
func (b Channel) CollectSensitiveValues() []string {
	if b.extend == nil {
		return nil
	}
	var values []string
	collectSensitive(reflect.ValueOf(b.extend), &values)
	return values
}

// ChannelsConfig 将通道名称映射到其 Channel 配置。
// 每个 Channel 都将完整的通道配置存储在 Settings 中，并处理
// JSON/YAML 序列化（自动移除/保留安全字段）。
//
//nolint:recvcheck
type ChannelsConfig map[string]*Channel

// UnmarshalYAML 为 ChannelsConfig 实现 yaml.Unmarshaler。
// 这确保在加载 security.yml 时，现有 Channel 实例会被
// 正确合并，而不是被新实例替换。
func (c *ChannelsConfig) UnmarshalYAML(value *yaml.Node) error {
	// 映射的 yaml.Node Content 包含交替的键值节点
	// 我们需要成对遍历它们
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("expected mapping node, got %v", value.Kind)
	}

	if *c == nil {
		*c = make(ChannelsConfig)
	}

	for i := 0; i < len(value.Content); i += 2 {
		if i+1 >= len(value.Content) {
			break
		}
		name := value.Content[i].Value
		node := value.Content[i+1]

		existingBC := (*c)[name]
		if existingBC != nil {
			// Channel 已存在 - 对其调用 UnmarshalYAML
			// 这会将 security.yml 设置合并到现有配置中
			if err := existingBC.UnmarshalYAML(node); err != nil {
				return err
			}
			// 确保名称已设置（此前可能为空）
			existingBC.SetName(name)
		} else {
			// 新通道 - 创建并反序列化
			newBC := &Channel{}
			if err := node.Decode(newBC); err != nil {
				return err
			}
			// 从 map 键设置通道名称
			newBC.SetName(name)
			(*c)[name] = newBC
		}
	}

	return nil
}

// UnmarshalJSON 为 ChannelsConfig 实现 json.Unmarshaler。
// 在反序列化后从 map 键设置通道名称。
func (c *ChannelsConfig) UnmarshalJSON(data []byte) error {
	// 使用类型别名避免无限递归
	type channelsConfigAlias map[string]*Channel
	var raw channelsConfigAlias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if *c == nil {
		*c = make(ChannelsConfig)
	}

	for name, bc := range raw {
		if bc != nil {
			bc.SetName(name)
		}
		(*c)[name] = bc
	}

	return nil
}

// Get 返回给定通道名称（map 键）对应的 Channel；未找到时返回 nil。
func (c ChannelsConfig) Get(name string) *Channel {
	if c == nil {
		return nil
	}
	return c[name]
}

// GetByType 返回给定通道类型对应的 Channel；未找到时返回 nil。
func (c ChannelsConfig) GetByType(t string) *Channel {
	if c == nil {
		return nil
	}
	for _, bc := range c {
		if bc.Type == t {
			return bc
		}
	}
	return nil
}

// SetEnabled 设置指定名称通道的 Enabled 字段。
// 如果不存在该名称的通道，则返回 false。
func (c ChannelsConfig) SetEnabled(name string, enabled bool) bool {
	bc := c[name]
	if bc == nil {
		return false
	}
	bc.Enabled = enabled
	return true
}

// validateSingletonChannels 检查单例通道类型最多只有一个
// 启用的实例。如果某个单例类型有多个启用的通道，则返回错误。
func validateSingletonChannels(channels ChannelsConfig) error {
	typeCount := make(map[string]int)
	typeNames := make(map[string][]string)
	for name, bc := range channels {
		if !bc.Enabled {
			continue
		}
		t := bc.Type
		if t == "" {
			t = name
		}
		if IsSingletonChannel(t) {
			typeCount[t]++
			typeNames[t] = append(typeNames[t], name)
		}
	}
	for t, count := range typeCount {
		if count > 1 {
			return fmt.Errorf(
				"channel type %q is singleton and does not support multiple instances, found %d enabled instances: %v",
				t,
				count,
				typeNames[t],
			)
		}
	}
	return nil
}

// BaseFieldNames 是属于 Channel 而非通道特有设置的 JSON 键。
var BaseFieldNames = map[string]struct{}{
	"enabled":              {},
	"type":                 {},
	"allow_from":           {},
	"reasoning_channel_id": {},
	"group_trigger":        {},
	"typing":               {},
	"placeholder":          {},
}

// ─── 内部辅助函数 ───

// extractSecureFieldNames 使用反射查找 SecureString 或 SecureStrings 类型的
// 导出字段，并返回它们的 JSON 字段名。
func extractSecureFieldNames(target any) map[string]struct{} {
	v := reflect.ValueOf(target)
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	names := make(map[string]struct{})
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		ft := f.Type
		if ft == reflect.TypeOf(SecureString{}) || ft == reflect.TypeOf(&SecureString{}) ||
			ft == reflect.TypeOf(SecureStrings{}) || ft == reflect.TypeOf(&SecureStrings{}) {
			jsonTag := f.Tag.Get("json")
			name := strings.Split(jsonTag, ",")[0]
			if name == "" || name == "-" {
				name = f.Name
			}
			names[name] = struct{}{}
		}
	}
	return names
}

// mergeRawJSON 在原始字节层面合并两个 JSON 对象（扁平键值）。
// overlay 中的值会覆盖 base 中的值。
func mergeRawJSON(base, overlay RawNode) (RawNode, error) {
	var baseMap, overlayMap map[string]any
	if len(base) > 0 {
		if err := json.Unmarshal(base, &baseMap); err != nil {
			return base, err
		}
	}
	if len(overlay) > 0 {
		if err := json.Unmarshal(overlay, &overlayMap); err != nil {
			return base, err
		}
	}
	if baseMap == nil {
		baseMap = make(map[string]any)
	}
	for k, v := range overlayMap {
		baseMap[k] = v
	}
	data, err := json.Marshal(baseMap)
	if err != nil {
		return base, err
	}
	return RawNode(data), nil
}

// removeSecureFields 从原始 JSON 中移除安全字段。
// 如果 secureFields 为 nil 或为空，则原样返回原始节点。
func removeSecureFields(r RawNode, secureFields map[string]struct{}) RawNode {
	if len(r) == 0 || len(secureFields) == 0 {
		return r
	}
	var m map[string]any
	if err := json.Unmarshal(r, &m); err != nil {
		return r
	}
	for name := range secureFields {
		delete(m, name)
	}
	data, err := json.Marshal(m)
	if err != nil {
		return r
	}
	return RawNode(data)
}

// filterSecureFields 仅保留原始 JSON 中的安全字段。
// 如果 secureFields 为 nil 或为空，则返回 nil（以便 omitzero/omitempty 省略它）。
func filterSecureFields(r RawNode, secureFields map[string]struct{}) RawNode {
	if len(r) == 0 || len(secureFields) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(r, &m); err != nil {
		return nil
	}
	secureMap := make(map[string]any)
	for name := range secureFields {
		if val, ok := m[name]; ok {
			secureMap[name] = val
		}
	}
	if len(secureMap) == 0 {
		return nil
	}
	data, err := json.Marshal(secureMap)
	if err != nil {
		return nil
	}
	return data
}

// channelSettingsFactory 将通道类型映射到对应 Settings 结构体的
// 零值原型。InitChannelList 使用 reflect.New 创建
// 新实例，避免重复的闭包样板代码。
var channelSettingsMu sync.RWMutex

var channelSettingsFactory = map[string]any{
	ChannelPico:       (PicoSettings{}),
	ChannelPicoClient: (PicoClientSettings{}),
}

// RegisterChannelSettings 为自定义通道类型注册设置结构体原型。
// 外部包（通过 channels.RegisterFactory 注册的树外通道）会在 init() 中
// 调用此函数，使其通道类型能够通过配置校验（isValidChannelType），
// 并让 settings 块解码到正确的结构体（newChannelSettings）。原型必须是
// 结构体值，例如 RegisterChannelSettings("my_channel", MyChannelSettings{})。
func RegisterChannelSettings(channelType string, prototype any) {
	channelSettingsMu.Lock()
	defer channelSettingsMu.Unlock()
	channelSettingsFactory[channelType] = prototype
}

// newChannelSettings 为给定通道类型创建一个新的零值指针。
// 如果该类型未注册，则返回 nil。
func newChannelSettings(channelType string) any {
	channelSettingsMu.RLock()
	proto, ok := channelSettingsFactory[channelType]
	channelSettingsMu.RUnlock()
	if !ok {
		return nil
	}
	return reflect.New(reflect.TypeOf(proto)).Interface()
}

// isValidChannelType 返回通道类型是否为已知且已注册的类型。
func isValidChannelType(channelType string) bool {
	channelSettingsMu.RLock()
	_, ok := channelSettingsFactory[channelType]
	channelSettingsMu.RUnlock()
	return ok
}

// InitChannelList 校验并初始化 ChannelsConfig 中的所有通道。
// 它执行三个步骤：
//  1. 校验每个通道的 Type 非空
//  2. 校验单例约束
//  3. 根据 Type 将 Settings 解码到正确的类型化结构体，
//     使 b.extend 包含实际设置（例如 PicoSettings）
//
// 调用此方法后，调用方可以通过 Decode() 安全地使用 b.extend，
// 而无需重新解析原始 Settings。
func InitChannelList(channels ChannelsConfig) error {
	// 步骤 1 和 3：校验类型并解码到类型化设置
	for name, bc := range channels {
		if bc == nil {
			delete(channels, name)
			continue
		}
		// 确保从 map 键设置通道名称
		bc.SetName(name)
		// 如果未显式设置 Type，则从 map 键推断
		if bc.Type == "" {
			bc.Type = name
		}
		if !isValidChannelType(bc.Type) {
			return fmt.Errorf("channel %q has unknown type %q", name, bc.Type)
		}
		// 解码到正确的类型化设置
		if target := newChannelSettings(bc.Type); target != nil {
			if err := bc.Decode(target); err != nil {
				return fmt.Errorf("channel %q failed to decode settings: %w", name, err)
			}
			if err := validateChannelStreamingConfig(name, target); err != nil {
				return err
			}
		}
	}

	// 步骤 2：校验单例约束
	if err := validateSingletonChannels(channels); err != nil {
		return err
	}

	return nil
}

func validateChannelStreamingConfig(channelName string, target any) error {
	settings, ok := target.(*PicoSettings)
	if !ok {
		return nil
	}
	streaming := settings.Streaming

	if streaming.ThrottleSeconds < 0 {
		return fmt.Errorf("channel %q streaming.throttle_seconds must be >= 0", channelName)
	}
	if streaming.MinGrowthChars < 0 {
		return fmt.Errorf("channel %q streaming.min_growth_chars must be >= 0", channelName)
	}
	return nil
}
