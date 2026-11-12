package config

import "testing"

func TestLookupModelCapability_KnownDefaults(t *testing.T) {
	cases := []struct {
		name string
		ctx  int
		out  int
	}{
		{"glm-5.3", 1048576, 131072},
		{"glm-5.3-admin", 1048576, 131072},
		{"provider/glm-5.3", 1048576, 131072},
		{"deepseek-v4.1-flash", 1048576, 32768},
		{"unknown-model", 0, 0},
	}
	for _, tc := range cases {
		cap, ok := LookupModelCapability(tc.name)
		if tc.ctx == 0 {
			if ok || cap != (ModelCapability{}) {
				t.Fatalf("%s should be unknown, got %+v ok=%v", tc.name, cap, ok)
			}
			continue
		}
		if !ok {
			t.Fatalf("%s capability missing", tc.name)
		}
		if cap.ContextWindow != tc.ctx || cap.MaxTokens != tc.out {
			t.Fatalf("%s = %+v, want ctx=%d out=%d", tc.name, cap, tc.ctx, tc.out)
		}
	}
}
