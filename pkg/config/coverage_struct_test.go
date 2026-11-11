package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func yamlMapping(t *testing.T, doc string) *yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
		t.Fatalf("yaml.Unmarshal(%q) error = %v", doc, err)
	}
	if len(node.Content) == 0 {
		t.Fatalf("yaml.Unmarshal(%q) produced no content", doc)
	}
	return node.Content[0]
}

func TestCoverageSkillsRegistriesAccessors(t *testing.T) {
	var nilRegistries *SkillsRegistriesConfig
	if _, ok := nilRegistries.Get("x"); ok {
		t.Fatal("nil registries Get() returned a value")
	}
	nilRegistries.Set("x", SkillRegistryConfig{}) // must not panic

	registries := SkillsRegistriesConfig{}
	if _, ok := registries.Get(""); ok {
		t.Fatal("empty name Get() returned a value")
	}
	if _, ok := registries.Get("missing"); ok {
		t.Fatal("missing name Get() returned a value")
	}
	registries.Set("", SkillRegistryConfig{}) // empty name ignored
	registries.Set("clawhub", SkillRegistryConfig{Enabled: true, BaseURL: "https://a"})
	if got, ok := registries.Get(" clawhub "); !ok || !got.Enabled || got.Name != "clawhub" {
		t.Fatalf("Get() = %+v, %v", got, ok)
	}
	registries.Set("clawhub", SkillRegistryConfig{Enabled: false, BaseURL: "https://b"})
	if len(registries) != 1 || registries[0].BaseURL != "https://b" {
		t.Fatalf("Set() replace = %+v", registries)
	}
	registries.Set("github", SkillRegistryConfig{Enabled: true})
	if len(registries) != 2 {
		t.Fatalf("Set() append len = %d", len(registries))
	}
	// nil entries are skipped by Get.
	withNil := append(registries, nil)
	if _, ok := withNil.Get("nothing"); ok {
		t.Fatal("Get() matched nil entry")
	}
}

func TestCoverageSkillsRegistriesJSON(t *testing.T) {
	// List form with no existing value replaces the whole list.
	var list SkillsRegistriesConfig
	if err := json.Unmarshal([]byte(`[{"name":"a","enabled":true},{"name":"b","base_url":"https://b"}]`), &list); err != nil {
		t.Fatalf("list form error = %v", err)
	}
	if len(list) != 2 || list[0].Name != "a" || !list[0].Enabled || list[1].BaseURL != "https://b" {
		t.Fatalf("list form = %+v", list)
	}

	// A second list-form decode merges into the existing entries.
	if err := json.Unmarshal([]byte(`[{"name":"a","base_url":"https://a2"}]`), &list); err != nil {
		t.Fatalf("merge list form error = %v", err)
	}
	if got, ok := list.Get("a"); !ok || got.BaseURL != "https://a2" || len(list) != 2 {
		t.Fatalf("merge list form = %+v", list)
	}

	// Map/legacy form into empty list is sorted by name.
	var legacy SkillsRegistriesConfig
	if err := json.Unmarshal([]byte(`{"b":{"enabled":true},"a":{"base_url":"https://a"}}`), &legacy); err != nil {
		t.Fatalf("legacy form error = %v", err)
	}
	if len(legacy) != 2 || legacy[0].Name != "a" || legacy[1].Name != "b" {
		t.Fatalf("legacy form = %+v", legacy)
	}

	// Legacy form merges into existing entries.
	if err := json.Unmarshal([]byte(`{"a":{"enabled":true},"c":{"enabled":true}}`), &legacy); err != nil {
		t.Fatalf("legacy merge error = %v", err)
	}
	if len(legacy) != 3 {
		t.Fatalf("legacy merge len = %d", len(legacy))
	}
	if a, ok := legacy.Get("a"); !ok || !a.Enabled || a.BaseURL != "https://a" {
		t.Fatalf("legacy merge a = %+v", a)
	}

	// Invalid inputs.
	var invalid SkillsRegistriesConfig
	if err := json.Unmarshal([]byte(`[{"name":123}]`), &invalid); err == nil {
		t.Fatal("list form with non-string name expected error")
	}
	var invalidLegacy SkillsRegistriesConfig
	if err := json.Unmarshal([]byte(`{"a":7}`), &invalidLegacy); err == nil {
		t.Fatal("legacy form with non-object value expected error")
	}

	// MarshalJSON: nil, empty-name and nil entries are skipped.
	if blob, err := (SkillsRegistriesConfig)(nil).MarshalJSON(); err != nil || string(blob) != "null" {
		t.Fatalf("nil MarshalJSON = %s, %v", blob, err)
	}
	blob, err := SkillsRegistriesConfig{
		nil,
		{Name: "", Enabled: true},
		{Name: "a", Enabled: true, BaseURL: "https://a"},
	}.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON error = %v", err)
	}
	var decoded map[string]SkillRegistryConfig
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("MarshalJSON output invalid: %v", err)
	}
	if len(decoded) != 1 || !decoded["a"].Enabled {
		t.Fatalf("MarshalJSON = %s", blob)
	}
}

func TestCoverageSkillRegistryConfigCodecs(t *testing.T) {
	// JSON decode: known fields, nested param, unknown keys folded into Param,
	// and "_auth_token" ignored.
	var reg SkillRegistryConfig
	if err := json.Unmarshal([]byte(`{
		"name":"clawhub",
		"enabled":true,
		"base_url":"https://x",
		"auth_token":"tok",
		"param":{"timeout":5},
		"extra_key":"value",
		"_auth_token":"ignored"
	}`), &reg); err != nil {
		t.Fatalf("UnmarshalJSON error = %v", err)
	}
	if reg.Name != "clawhub" || !reg.Enabled || reg.BaseURL != "https://x" || reg.AuthToken.String() != "tok" {
		t.Fatalf("UnmarshalJSON = %+v", reg)
	}
	if reg.Param["timeout"] != float64(5) || reg.Param["extra_key"] != "value" {
		t.Fatalf("Param = %+v", reg.Param)
	}
	if _, ok := reg.Param["_auth_token"]; ok {
		t.Fatal("_auth_token leaked into Param")
	}
	// Invalid shapes.
	if err := json.Unmarshal([]byte(`{"name":1}`), &SkillRegistryConfig{}); err == nil {
		t.Fatal("non-string name expected error")
	}
	if err := json.Unmarshal([]byte(`{"param":[1]}`), &SkillRegistryConfig{}); err == nil {
		t.Fatal("non-object param expected error")
	}
	if err := json.Unmarshal([]byte(`{"auth_token":5}`), &SkillRegistryConfig{}); err == nil {
		t.Fatal("non-string auth_token expected error")
	}

	// JSON encode: token, param filtering (empty/param/_private/reserved keys).
	reg = SkillRegistryConfig{
		Name: "clawhub", Enabled: true, BaseURL: "https://x",
		AuthToken: *NewSecureString("tok"),
		Param: map[string]any{
			"":         "skip-empty",
			"param":    "skip-reserved",
			"_private": "skip-underscore",
			"name":     "skip-existing",
			"timeout":  5,
		},
	}
	blob, err := reg.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON error = %v", err)
	}
	var encoded map[string]any
	if err := json.Unmarshal(blob, &encoded); err != nil {
		t.Fatalf("MarshalJSON output: %v", err)
	}
	if encoded["enabled"] != true || encoded["base_url"] != "https://x" {
		t.Fatalf("MarshalJSON base fields = %v", encoded)
	}
	if _, ok := encoded["auth_token"]; !ok {
		t.Fatalf("MarshalJSON missing auth_token: %v", encoded)
	}
	if _, ok := encoded[""]; ok {
		t.Fatal("empty param key encoded")
	}
	if _, ok := encoded["_private"]; ok {
		t.Fatal("underscore param encoded")
	}
	if encoded["name"] != "skip-existing" {
		t.Fatalf("reserved param overwrote field: %v", encoded["name"])
	}
	if encoded["timeout"] != float64(5) {
		t.Fatalf("timeout param = %v", encoded["timeout"])
	}

	// YAML decode: nested param + extra keys + clone behavior.
	existing := SkillRegistryConfig{Name: "a", Param: map[string]any{"keep": "yes"}}
	var fromYAML SkillRegistryConfig
	if err := yaml.Unmarshal([]byte("enabled: true\nparam:\n  timeout: 7\nextra: 9\n"), &fromYAML); err != nil {
		t.Fatalf("UnmarshalYAML error = %v", err)
	}
	if !fromYAML.Enabled || fromYAML.Param["timeout"] != 7 || fromYAML.Param["extra"] != 9 {
		t.Fatalf("UnmarshalYAML = %+v", fromYAML)
	}
	if existing.Param["keep"] != "yes" {
		t.Fatal("clone mutated original params")
	}
	existingNode := yamlMapping(t, "keep: last\nenabled: false\n")
	merged := &SkillRegistryConfig{Name: "a", Param: map[string]any{"prev": 1}}
	if err := merged.UnmarshalYAML(existingNode); err != nil {
		t.Fatalf("UnmarshalYAML(merge) error = %v", err)
	}
	if merged.Param["prev"] != 1 || merged.Param["keep"] != "last" {
		t.Fatalf("UnmarshalYAML(merge) Param = %+v", merged.Param)
	}
	if err := (&SkillRegistryConfig{}).UnmarshalYAML(&yaml.Node{Kind: yaml.ScalarNode, Value: "x"}); err == nil {
		t.Fatal("UnmarshalYAML(scalar) expected error")
	}

	// YAML encode.
	reg = SkillRegistryConfig{Name: "a", Enabled: true, AuthToken: *NewSecureString("tok"), Param: map[string]any{"x": 1}}
	out, err := reg.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML error = %v", err)
	}
	if _, err := yaml.Marshal(out); err != nil {
		t.Fatalf("MarshalYAML output: %v", err)
	}
}

func TestCoverageSkillRegistryDecodeParam(t *testing.T) {
	var nilCfg *SkillRegistryConfig
	if err := nilCfg.DecodeParam(&struct{}{}); err != nil {
		t.Fatalf("nil DecodeParam error = %v", err)
	}
	if err := (&SkillRegistryConfig{}).DecodeParam(&struct{}{}); err != nil {
		t.Fatalf("empty DecodeParam error = %v", err)
	}
	var target struct {
		Timeout int `json:"timeout"`
	}
	if err := (&SkillRegistryConfig{Param: map[string]any{"timeout": 5}}).DecodeParam(&target); err != nil {
		t.Fatalf("DecodeParam error = %v", err)
	}
	if target.Timeout != 5 {
		t.Fatalf("DecodeParam target = %+v", target)
	}
	badParam := &SkillRegistryConfig{Param: map[string]any{"fn": func() {}}}
	if err := badParam.DecodeParam(&target); err == nil {
		t.Fatal("DecodeParam with marshal failure expected error")
	}
	if err := (&SkillRegistryConfig{Param: map[string]any{"timeout": "not-int"}}).DecodeParam(&target); err == nil {
		t.Fatal("DecodeParam with type mismatch expected error")
	}
}

func TestCoverageSkillsRegistriesYAML(t *testing.T) {
	// Empty target: decoded map becomes a name-sorted list.
	var empty SkillsRegistriesConfig
	if err := empty.UnmarshalYAML(yamlMapping(t, "b:\n  enabled: true\na:\n  base_url: https://a\n")); err != nil {
		t.Fatalf("UnmarshalYAML(empty) error = %v", err)
	}
	if len(empty) != 2 || empty[0].Name != "a" || empty[1].Name != "b" {
		t.Fatalf("UnmarshalYAML(empty) = %+v", empty)
	}

	// Existing target: merge by name and preserve previous params.
	var merged SkillsRegistriesConfig
	merged.Set("a", SkillRegistryConfig{Name: "a", Enabled: true, Param: map[string]any{"keep": 1}})
	if err := merged.UnmarshalYAML(yamlMapping(t, "a:\n  base_url: https://a2\nc:\n  enabled: true\n")); err != nil {
		t.Fatalf("UnmarshalYAML(merge) error = %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("UnmarshalYAML(merge) len = %d", len(merged))
	}
	if a, ok := merged.Get("a"); !ok || a.BaseURL != "https://a2" || !a.Enabled || a.Param["keep"] != 1 {
		t.Fatalf("UnmarshalYAML(merge) a = %+v", a)
	}

	// Nil node decodes to an empty map; empty/blank names are skipped.
	if err := (&SkillsRegistriesConfig{}).UnmarshalYAML(nil); err != nil {
		t.Fatalf("UnmarshalYAML(nil) error = %v", err)
	}
	var skipped SkillsRegistriesConfig
	if err := skipped.UnmarshalYAML(yamlMapping(t, "a: {enabled: true}\n")); err != nil {
		t.Fatalf("UnmarshalYAML(single) error = %v", err)
	}
	if len(skipped) != 1 {
		t.Fatalf("UnmarshalYAML(single) = %+v", skipped)
	}
	if err := (&SkillsRegistriesConfig{}).UnmarshalYAML(yamlMapping(t, "a: 7\n")); err == nil {
		t.Fatal("UnmarshalYAML(non-mapping value) expected error")
	}

	// YAML encode: map keyed by registry name, skipping nil/blank entries.
	out, err := SkillsRegistriesConfig{
		nil,
		{Name: "", Enabled: true},
		{Name: "a", Enabled: true, AuthToken: *NewSecureString("tok")},
	}.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML error = %v", err)
	}
	blob, err := yaml.Marshal(out)
	if err != nil {
		t.Fatalf("MarshalYAML output: %v", err)
	}
	if !strings.Contains(string(blob), "a:") || !strings.Contains(string(blob), "auth_token") {
		t.Fatalf("MarshalYAML = %s", blob)
	}
	if strings.Contains(string(blob), "true:") {
		t.Fatalf("MarshalYAML encoded a blank-name entry: %s", blob)
	}
}

func TestCoverageSecureModelListYAML(t *testing.T) {
	list := SecureModelList{
		{ModelName: "m", Model: "m", APIKeys: SimpleSecureStrings("k0")},
		{ModelName: "m", Model: "m", APIKeys: SimpleSecureStrings("k1")},
	}
	out, err := list.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML error = %v", err)
	}
	blob, err := yaml.Marshal(out)
	if err != nil {
		t.Fatalf("MarshalYAML encode error = %v", err)
	}
	if !strings.Contains(string(blob), "m:0") || !strings.Contains(string(blob), "m:1") {
		t.Fatalf("MarshalYAML = %s", blob)
	}

	target := SecureModelList{
		{ModelName: "m", Model: "m"},
		{ModelName: "m", Model: "m"},
	}
	node := yamlMapping(t, "\"m:0\":\n  api_keys: [\"k0\"]\n\"m:1\":\n  api_keys: [\"k1\"]\n")
	if err := target.UnmarshalYAML(node); err != nil {
		t.Fatalf("UnmarshalYAML error = %v", err)
	}
	if got := target[0].APIKeys.Values(); len(got) != 1 || got[0] != "k0" {
		t.Fatalf("target[0] APIKeys = %v", got)
	}
	if got := target[1].APIKeys.Values(); len(got) != 1 || got[0] != "k1" {
		t.Fatalf("target[1] APIKeys = %v", got)
	}

	// Fallback keyed by plain model name when the indexed key is absent.
	plain := SecureModelList{{ModelName: "solo", Model: "solo"}}
	if err := plain.UnmarshalYAML(yamlMapping(t, "solo:\n  api_keys: [\"k\"]\n")); err != nil {
		t.Fatalf("UnmarshalYAML(plain) error = %v", err)
	}
	if got := plain[0].APIKeys.Values(); len(got) != 1 || got[0] != "k" {
		t.Fatalf("plain fallback APIKeys = %v", got)
	}
	if err := plain.UnmarshalYAML(yamlMapping(t, "solo: 7\n")); err == nil {
		t.Fatal("UnmarshalYAML(non-mapping) expected error")
	}
}
