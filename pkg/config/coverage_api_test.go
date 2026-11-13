package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestCoverageSimpleGetters 覆盖配置结构上的小取值函数与默认值分支。
func TestCoverageSimpleGetters(t *testing.T) {
	defaults := &AgentDefaults{}
	if got := defaults.GetMaxMediaSize(); got != DefaultMaxMediaSize {
		t.Fatalf("zero GetMaxMediaSize() = %d", got)
	}
	defaults.MaxMediaSize = 123
	if got := defaults.GetMaxMediaSize(); got != 123 {
		t.Fatalf("set GetMaxMediaSize() = %d", got)
	}
	defaults.ToolFeedback.Enabled = true
	defaults.ToolFeedback.SeparateMessages = true
	if !defaults.IsToolFeedbackEnabled() || !defaults.IsToolFeedbackSeparateMessagesEnabled() {
		t.Fatal("tool feedback flags not reported")
	}
	defaults.ModelName = "m1"
	if got := defaults.GetModelName(); got != "m1" {
		t.Fatalf("GetModelName() = %q", got)
	}

	for _, tc := range []struct {
		name string
		text []string
		want string
	}{
		{"empty", nil, "Thinking..."},
		{"single", []string{"稍等"}, "稍等"},
	} {
		t.Run("placeholder/"+tc.name, func(t *testing.T) {
			p := &PlaceholderConfig{Text: tc.text}
			if got := p.GetRandomText(); got != tc.want {
				t.Fatalf("GetRandomText() = %q, want %q", got, tc.want)
			}
		})
	}
	multi := &PlaceholderConfig{Text: []string{"a", "b"}}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		got := multi.GetRandomText()
		if got != "a" && got != "b" {
			t.Fatalf("GetRandomText() = %q, want a or b", got)
		}
		seen[got] = true
	}
	if len(seen) == 0 {
		t.Fatal("GetRandomText() never returned a candidate")
	}

	// MCP getters.
	var nilMCP *MCPConfig
	if nilMCP.Active() {
		t.Fatal("nil MCP Active() = true")
	}
	mcp := &MCPConfig{}
	if mcp.Active() {
		t.Fatal("empty MCP Active() = true")
	}
	mcp.Enabled = true
	if !mcp.Active() {
		t.Fatal("enabled MCP Active() = false")
	}
	mcp = &MCPConfig{Servers: map[string]MCPServerConfig{"s": {}}}
	if !mcp.Active() {
		t.Fatal("servers-only MCP Active() = false")
	}
	mcp.ExplicitDisabled = true
	if mcp.Active() {
		t.Fatal("explicitly disabled MCP Active() = true")
	}
	if got := (&MCPConfig{}).GetMaxInlineTextChars(); got != DefaultMCPMaxInlineTextChars {
		t.Fatalf("GetMaxInlineTextChars() = %d", got)
	}
	if got := (&MCPConfig{MaxInlineTextChars: 99}).GetMaxInlineTextChars(); got != 99 {
		t.Fatalf("GetMaxInlineTextChars(custom) = %d", got)
	}
	if got := (&MCPConfig{}).GetKeepAliveInterval(); got != DefaultMCPKeepAliveInterval {
		t.Fatalf("GetKeepAliveInterval() = %d", got)
	}
	if got := (&MCPConfig{KeepAliveInterval: -1}).GetKeepAliveInterval(); got != 0 {
		t.Fatalf("GetKeepAliveInterval(negative) = %d", got)
	}
	if got := (&MCPConfig{KeepAliveInterval: 7}).GetKeepAliveInterval(); got != 7 {
		t.Fatalf("GetKeepAliveInterval(custom) = %d", got)
	}

	// Read-file mode normalization.
	for _, tc := range []struct{ in, want string }{
		{"", ReadFileModeBytes},
		{"bytes", ReadFileModeBytes},
		{"LINES", ReadFileModeLines},
		{" lines ", ReadFileModeLines},
		{"bogus", ReadFileModeBytes},
	} {
		if got := (ReadFileToolConfig{Mode: tc.in}).EffectiveMode(); got != tc.want {
			t.Fatalf("EffectiveMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Skills catalog mode normalization.
	for _, tc := range []struct{ in, want string }{
		{"", SkillsCatalogModeContext},
		{"context", SkillsCatalogModeContext},
		{"SYSTEM", SkillsCatalogModeSystem},
		{" system ", SkillsCatalogModeSystem},
		{"bogus", SkillsCatalogModeContext},
	} {
		if got := (&SkillsToolsConfig{CatalogMode: tc.in}).EffectiveCatalogMode(); got != tc.want {
			t.Fatalf("EffectiveCatalogMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	var nilSkills *SkillsToolsConfig
	if got := nilSkills.EffectiveCatalogMode(); got != SkillsCatalogModeContext {
		t.Fatalf("nil EffectiveCatalogMode() = %q", got)
	}
	nilSkills.ApplyDefaults()
	if err := nilSkills.Validate(); err != nil {
		t.Fatalf("nil Skills Validate() = %v", err)
	}
	(&SkillsToolsConfig{}).ApplyDefaults()
	if err := (&SkillsToolsConfig{CatalogMode: "context"}).Validate(); err != nil {
		t.Fatalf("valid catalog mode rejected: %v", err)
	}
	err := (&SkillsToolsConfig{CatalogMode: "sometimes"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "tools.skills.catalog_mode") {
		t.Fatalf("invalid catalog mode error = %v", err)
	}

	// Sensitive-data filter accessors.
	tools := &ToolsConfig{}
	if tools.IsFilterSensitiveDataEnabled() {
		t.Fatal("filter enabled by default")
	}
	if got := tools.GetFilterMinLength(); got != 8 {
		t.Fatalf("GetFilterMinLength() = %d", got)
	}
	tools.FilterMinLength = 3
	if got := tools.GetFilterMinLength(); got != 3 {
		t.Fatalf("GetFilterMinLength(custom) = %d", got)
	}

	// FlexibleStringSlice: JSON shapes and text form.
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{"null", `null`, nil},
		{"string", `"one"`, []string{"one"}},
		{"number", `42`, []string{"42"}},
		{"strings", `["a","b"]`, []string{"a", "b"}},
		{"mixed", `["a",2,true]`, []string{"a", "2", "true"}},
	} {
		t.Run("flexible/"+tc.name, func(t *testing.T) {
			var got FlexibleStringSlice
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("UnmarshalJSON(%s) error = %v", tc.in, err)
			}
			if !reflect.DeepEqual([]string(got), tc.want) {
				t.Fatalf("UnmarshalJSON(%s) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
	var text FlexibleStringSlice
	if err := text.UnmarshalText([]byte(" a，b , ,c ")); err != nil {
		t.Fatalf("UnmarshalText() error = %v", err)
	}
	if !reflect.DeepEqual([]string(text), []string{"a", "b", "c"}) {
		t.Fatalf("UnmarshalText() = %v", text)
	}
	if err := text.UnmarshalText(nil); err != nil || text != nil {
		t.Fatalf("UnmarshalText(nil) = %v, %v", text, err)
	}
	var bad FlexibleStringSlice
	if err := bad.UnmarshalJSON([]byte(`{"x":1}`)); err == nil {
		t.Fatal("UnmarshalJSON(object) expected error")
	}
}

// TestCoverageModelAPIKeys 覆盖模型/搜索配置的 API key 访问器。
func TestCoverageModelAPIKeys(t *testing.T) {
	m := &ModelConfig{}
	if got := m.APIKey(); got != "" {
		t.Fatalf("empty APIKey() = %q", got)
	}
	m.SetAPIKey("k1")
	if got := m.APIKey(); got != "k1" {
		t.Fatalf("APIKey() = %q", got)
	}
	m.SetAPIKey("k2")
	if got := m.APIKey(); got != "k2" {
		t.Fatalf("APIKey() after Set existing = %q", got)
	}
	m.APIKeys = nil
	m.SetAPIKey("k3")
	if got := m.APIKey(); got != "k3" {
		t.Fatalf("APIKey() after Set empty = %q", got)
	}
	m.APIKeys = SimpleSecureStrings("a", "b")
	if got := m.APIKey(); got != "a" {
		t.Fatalf("APIKey() after multi-key list = %q", got)
	}
	if m.IsVirtual() {
		t.Fatal("IsVirtual() = true for normal model")
	}

	brave := &BraveConfig{}
	if brave.APIKey() != "" {
		t.Fatal("empty brave APIKey")
	}
	brave.SetAPIKey("b1")
	if brave.APIKey() != "b1" {
		t.Fatal("brave SetAPIKey")
	}
	brave.SetAPIKeys([]string{"b2", "b3"})
	if brave.APIKey() != "b2" {
		t.Fatal("brave SetAPIKeys")
	}

	tavily := &TavilyConfig{}
	if tavily.APIKey() != "" {
		t.Fatal("empty tavily APIKey")
	}
	tavily.SetAPIKey("t1")
	tavily.SetAPIKeys([]string{"t2", "t3"})
	if tavily.APIKey() != "t2" {
		t.Fatalf("tavily APIKey = %q", tavily.APIKey())
	}

	kagi := &KagiConfig{}
	if kagi.APIKey() != "" {
		t.Fatal("empty kagi APIKey")
	}
	kagi.SetAPIKey("k1")
	kagi.SetAPIKeys([]string{"k2"})
	if kagi.APIKey() != "k2" {
		t.Fatalf("kagi APIKey = %q", kagi.APIKey())
	}

	perplexity := &PerplexityConfig{}
	if perplexity.APIKey() != "" {
		t.Fatal("empty perplexity APIKey")
	}
	perplexity.SetAPIKey("p1")
	if perplexity.APIKey() != "p1" {
		t.Fatalf("perplexity APIKey = %q", perplexity.APIKey())
	}
}

// TestCoverageDSHGroup 覆盖 DSH 工具组开关分支。
func TestCoverageDSHGroup(t *testing.T) {
	var g DSHGroupConfig
	if !g.IsEnabled() {
		t.Fatal("zero DSH group should be enabled")
	}
	disabled := false
	g.Enabled = &disabled
	if g.IsEnabled() {
		t.Fatal("disabled DSH group reported enabled")
	}
	if g.IsJobsEnabled() || g.IsGoalEnabled() || g.IsFSSearchEnabled() ||
		g.IsMCPResourcesEnabled() || g.IsSessionQueryEnabled() || g.IsSubagentControlEnabled() {
		t.Fatal("disabled group reported an enabled tool family")
	}

	truthy := true
	g = DSHGroupConfig{Enabled: &truthy}
	if !g.IsEnabled() || !g.IsJobsEnabled() {
		t.Fatal("enabled group/tool family not reported")
	}
	off := false
	g.Jobs.Enabled = &off
	if g.IsJobsEnabled() {
		t.Fatal("explicitly disabled jobs family reported enabled")
	}
	if !g.IsGoalEnabled() {
		t.Fatal("goal family should stay enabled")
	}
	var nilTool DSHToolConfig
	if !nilTool.isEnabled() {
		t.Fatal("nil tool config should default enabled")
	}
}

// TestCoverageSecureStringAPI 覆盖 SecureString/SecureStrings 的编解码与零值语义。
func TestCoverageSecureStringAPI(t *testing.T) {
	var nilStr *SecureString
	if got := nilStr.String(); got != "" {
		t.Fatalf("nil String() = %q", got)
	}
	s := NewSecureString("plain")
	if s.String() != "plain" {
		t.Fatalf("String() = %q", s.String())
	}
	if s.Set("next").String() != "next" {
		t.Fatal("Set() did not update value")
	}
	blob, err := s.MarshalJSON()
	if err != nil || string(blob) != notHere {
		t.Fatalf("MarshalJSON() = %q, %v", blob, err)
	}
	if err := s.UnmarshalJSON([]byte(notHere)); err != nil {
		t.Fatalf("UnmarshalJSON(notHere) error = %v", err)
	}
	if err := s.UnmarshalJSON([]byte(`"from-json"`)); err != nil || s.String() != "from-json" {
		t.Fatalf("UnmarshalJSON(string) = %q, %v", s.String(), err)
	}
	if err := s.UnmarshalJSON([]byte(`123`)); err == nil {
		t.Fatal("UnmarshalJSON(number) expected error")
	}
	if err := s.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: "from-yaml"}); err != nil ||
		s.String() != "from-yaml" {
		t.Fatalf("UnmarshalYAML = %q, %v", s.String(), err)
	}
	if err := s.UnmarshalText([]byte("from-text")); err != nil || s.String() != "from-text" {
		t.Fatalf("UnmarshalText = %q, %v", s.String(), err)
	}
	// enc:// / file:// raw values pass through unchanged when no resolver target exists.
	if err := s.UnmarshalText([]byte("file://missing-secret")); err == nil {
		// A resolvable resolver may or may not exist; both outcomes are fine, but
		// raw must be preserved for later encryption.
		if s.raw == "" {
			t.Fatal("raw not preserved for encrypted value")
		}
	}

	yamlOut, err := s.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML(plain) error = %v", err)
	}
	if yamlOut != "file://missing-secret" && yamlOut != "from-text" {
		t.Fatalf("MarshalYAML(plain) = %v", yamlOut)
	}
	rawEnc := SecureString{raw: "enc://abcd"}
	if out, err := rawEnc.MarshalYAML(); err != nil || out != "enc://abcd" {
		t.Fatalf("MarshalYAML(raw enc) = %v, %v", out, err)
	}
	resolvedEnc := SecureString{resolved: "enc://efgh"}
	if out, err := resolvedEnc.MarshalYAML(); err != nil || out != "enc://efgh" {
		t.Fatalf("MarshalYAML(resolved enc) = %v, %v", out, err)
	}

	var nilList *SecureStrings
	if got := nilList.Values(); got != nil {
		t.Fatalf("nil Values() = %v", got)
	}
	list := SimpleSecureStrings("a", "a", "b")
	if got := list.Values(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Values() = %v", got)
	}
	if _, err := list.MarshalJSON(); err != nil {
		t.Fatalf("SecureStrings MarshalJSON error = %v", err)
	}
	if !list.IsZero() {
		t.Fatal("IsZero() should be true outside YAML caller")
	}
	if err := list.UnmarshalJSON([]byte(notHere)); err != nil {
		t.Fatalf("SecureStrings UnmarshalJSON(notHere) error = %v", err)
	}
	if err := list.UnmarshalJSON([]byte(`["x","x","y"]`)); err != nil {
		t.Fatalf("SecureStrings UnmarshalJSON(list) error = %v", err)
	}
	if got := list.Values(); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Fatalf("UnmarshalJSON list Values() = %v", got)
	}
}

// TestCoverageRawNode 覆盖 RawNode 的 JSON/YAML 合并语义。
func TestCoverageRawNode(t *testing.T) {
	var r RawNode
	if blob, err := r.MarshalJSON(); err != nil || string(blob) != "null" {
		t.Fatalf("empty MarshalJSON = %q, %v", blob, err)
	}
	r = RawNode(`{"a":1}`)
	if blob, err := r.MarshalJSON(); err != nil || string(blob) != `{"a":1}` {
		t.Fatalf("MarshalJSON = %q, %v", blob, err)
	}
	for _, in := range []string{`null`, `{}`, `[]`} {
		var got RawNode
		if err := json.Unmarshal([]byte(in), &got); err != nil || got != nil {
			t.Fatalf("UnmarshalJSON(%s) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{`{"a":1}`, `[1,2]`, `"text"`, `42`, `true`} {
		var got RawNode
		if err := json.Unmarshal([]byte(in), &got); err != nil {
			t.Fatalf("UnmarshalJSON(%s) error = %v", in, err)
		}
		if len(got) == 0 {
			t.Fatalf("UnmarshalJSON(%s) = empty", in)
		}
	}
	var fromYAML RawNode
	if err := json.Unmarshal([]byte(`{"a":1}`), &fromYAML); err != nil {
		t.Fatalf("seed RawNode error = %v", err)
	}
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte("a: 2\nb:\n  c: 3\n"), node); err != nil {
		t.Fatalf("yaml.Unmarshal error = %v", err)
	}
	if err := fromYAML.UnmarshalYAML(node.Content[0]); err != nil {
		t.Fatalf("UnmarshalYAML error = %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(fromYAML, &merged); err != nil {
		t.Fatalf("merged JSON error = %v", err)
	}
	if merged["a"] != float64(2) || merged["b"] == nil {
		t.Fatalf("merged = %v", merged)
	}
	var zeroKind RawNode
	zeroKind.UnmarshalYAML(&yaml.Node{})
	if zeroKind != nil {
		t.Fatalf("zero-kind node produced %q", zeroKind)
	}
}

// TestCoverageChannelAccessors 覆盖 Channel 名称/设置/敏感值访问器。
func TestCoverageChannelAccessors(t *testing.T) {
	c := &Channel{Type: "pico"}
	if c.Name() != "" || !c.SettingsIsEmpty() {
		t.Fatalf("zero channel name/settings = %q/%v", c.Name(), c.SettingsIsEmpty())
	}
	c.SetName("pico-main")
	if c.Name() != "pico-main" {
		t.Fatalf("SetName/Name = %q", c.Name())
	}
	if err := c.Decode(nil); err == nil {
		t.Fatal("Decode(nil) expected error")
	}
	if err := c.Decode(&PicoSettings{}); err != nil {
		t.Fatalf("Decode(PicoSettings) error = %v", err)
	}
	decoded, err := c.GetDecoded()
	if err != nil {
		t.Fatalf("GetDecoded() error = %v", err)
	}
	if _, ok := decoded.(*PicoSettings); !ok {
		t.Fatalf("GetDecoded() = %T, want *PicoSettings", decoded)
	}
	rawChannel := &Channel{Type: "pico", Settings: RawNode(`{}`)}
	rawChannel.SetSecretField("token", *NewSecureString("secret-token"))
	if _, err := rawChannel.GetDecoded(); err != nil {
		t.Fatalf("GetDecoded() after SetSecretField error = %v", err)
	}
	// SetSecretField serializes via SecureString.MarshalJSON, which stores the
	// [NOT_HERE] placeholder rather than the plaintext; the raw JSON must be updated.
	if !strings.Contains(string(rawChannel.Settings), "[NOT_HERE]") {
		t.Fatalf("SetSecretField did not write the placeholder: %s", rawChannel.Settings)
	}
	// Invalid raw settings are ignored without panicking.
	(&Channel{Settings: RawNode(`not-json`)}).SetSecretField("token", *NewSecureString("x"))
	plain := &Channel{Type: "pico", Settings: mustParseRawNode(`{"token":"direct"}`)}
	if _, err := plain.GetDecoded(); err != nil {
		t.Fatalf("GetDecoded() from settings error = %v", err)
	}
	if len(plain.CollectSensitiveValues()) == 0 {
		t.Fatal("CollectSensitiveValues() empty for direct token settings")
	}
	empty := &Channel{}
	if got := empty.CollectSensitiveValues(); got != nil {
		t.Fatalf("CollectSensitiveValues() without extend = %v", got)
	}
	if got := newChannelSettings("unknown-type"); got != nil {
		t.Fatalf("newChannelSettings(unknown) = %v", got)
	}
	if got := newChannelSettings("pico"); got == nil {
		t.Fatal("newChannelSettings(pico) = nil")
	}

	// ChannelsConfig accessors.
	var nilChannels ChannelsConfig
	if nilChannels.Get("x") != nil || nilChannels.GetByType("pico") != nil {
		t.Fatal("nil ChannelsConfig returned a channel")
	}
	channels := ChannelsConfig{
		"pico":  {Type: "pico", Enabled: false},
		"other": {Type: "slack", Enabled: true},
	}
	if channels.Get("pico") == nil || channels.GetByType("slack") == nil {
		t.Fatal("ChannelsConfig Get/GetByType failed")
	}
	if channels.SetEnabled("pico", true) != true || !channels["pico"].Enabled {
		t.Fatal("SetEnabled existing failed")
	}
	if channels.SetEnabled("missing", true) {
		t.Fatal("SetEnabled missing returned true")
	}

	// JSON/YAML decoding of the map.
	var fromJSON ChannelsConfig
	if err := json.Unmarshal([]byte(`{"pico":{"enabled":true,"type":"pico"}}`), &fromJSON); err != nil {
		t.Fatalf("ChannelsConfig UnmarshalJSON error = %v", err)
	}
	if fromJSON["pico"].Name() != "pico" || !fromJSON["pico"].Enabled {
		t.Fatalf("ChannelsConfig JSON decode = %+v", fromJSON["pico"])
	}
	var fromYAML ChannelsConfig
	node := &yaml.Node{}
	if err := yaml.Unmarshal([]byte("pico:\n  enabled: true\n  type: pico\n"), node); err != nil {
		t.Fatalf("yaml.Unmarshal error = %v", err)
	}
	if err := fromYAML.UnmarshalYAML(node.Content[0]); err != nil {
		t.Fatalf("ChannelsConfig UnmarshalYAML error = %v", err)
	}
	if fromYAML["pico"] == nil || fromYAML["pico"].Name() != "pico" {
		t.Fatalf("ChannelsConfig YAML decode = %+v", fromYAML["pico"])
	}
	var badChannels ChannelsConfig
	if err := badChannels.UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: "x"}); err == nil {
		t.Fatal("ChannelsConfig UnmarshalYAML(scalar) expected error")
	}
	if !IsSingletonChannel("pico") || IsSingletonChannel("definitely-not-singleton") {
		t.Fatal("IsSingletonChannel semantics wrong")
	}

	// Pico settings token helper.
	pico := &PicoSettings{}
	pico.SetToken("tok")
	if pico.Token.String() != "tok" {
		t.Fatalf("SetToken/Token = %q", pico.Token.String())
	}
}

// TestCoverageFilterSensitiveData 覆盖 FilterSensitiveData 的开关/长度/替换分支。
func TestCoverageFilterSensitiveData(t *testing.T) {
	var nilCfg *Config
	if got := nilCfg.FilterSensitiveData("secret"); got != "secret" {
		t.Fatal("nil config changed content")
	}
	cfg := &Config{}
	if got := cfg.FilterSensitiveData("abcdefgh-secret"); got != "abcdefgh-secret" {
		t.Fatal("disabled filter changed content")
	}
	cfg.Tools.FilterSensitiveData = true
	cfg.Tools.FilterMinLength = 100
	if got := cfg.FilterSensitiveData("short"); got != "short" {
		t.Fatal("short content changed despite min length")
	}

	// Install a long-enough sensitive value through a model API key.
	cfg2 := &Config{}
	cfg2.Tools.FilterSensitiveData = true
	cfg2.ModelList = []*ModelConfig{{ModelName: "m", Model: "m"}}
	cfg2.ModelList[0].SetAPIKey("super-secret-key-value")
	cfg2.sensitiveCache = nil
	content := "prefix super-secret-key-value suffix"
	if got := cfg2.FilterSensitiveData(content); strings.Contains(got, "super-secret-key-value") {
		t.Fatalf("sensitive value not filtered: %q", got)
	} else if !strings.Contains(got, "[FILTERED]") {
		t.Fatalf("filtered content lacks marker: %q", got)
	}
}
