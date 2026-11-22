package health

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapabilitiesEndpointUsesProvider(t *testing.T) {
	s := NewServer("127.0.0.1", 0, "")
	s.SetCapabilityProvider(func() any {
		return map[string]any{"tools": []string{"team_member_task_list"}, "pid": 123}
	})
	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()
	s.server.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["pid"] != float64(123) {
		t.Fatalf("body=%v", body)
	}
}
