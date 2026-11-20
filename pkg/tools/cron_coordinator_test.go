package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/cron"
)

func TestCronTool_ExecuteJobUsesCoordinatorClaimAndFinish(t *testing.T) {
	var finished bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/claim"):
			if got := r.Header.Get("X-Cron-Agent"); got != "daily-rounds" {
				t.Fatalf("agent header=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"run_id": "run-1", "task_id": "TBlfkm",
				"session_id": "pico_user_alice_TBlfkm",
				"owner_type": "user", "owner_id": "alice", "agent_nickname": "daily-rounds",
			})
		case strings.HasSuffix(r.URL.Path, "/finish"):
			finished = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"code":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	executor := &stubJobExecutor{response: "巡检完成"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())
	tool.SetCoordinator(NewCronRunCoordinator(server.URL, "test-token", "daily-rounds"))

	job := &cron.CronJob{ID: "job-1", Name: "巡检"}
	job.Payload.Target = &cron.CronTarget{OwnerType: "user", OwnerID: "alice", Channel: "pico"}
	job.Payload.Message = "执行巡检"

	got := tool.ExecuteJob(context.Background(), job)
	if got != "ok" {
		t.Fatalf("ExecuteJob() = %q", got)
	}
	if executor.lastChatID != "pico:pico_user_alice_TBlfkm" {
		t.Fatalf("chatID=%q", executor.lastChatID)
	}
	if executor.lastKey != "agent:cron-run-1" {
		t.Fatalf("sessionKey=%q", executor.lastKey)
	}
	if !finished {
		t.Fatal("finish was not called")
	}
}

func TestCronTool_ExecuteJobCoordinatorClaimFailureIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "claim unavailable", http.StatusBadGateway)
	}))
	defer server.Close()
	executor := &stubJobExecutor{response: "should not run"}
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())
	tool.SetCoordinator(NewCronRunCoordinator(server.URL, "test-token", "daily-rounds"))
	job := &cron.CronJob{ID: "job-2", Name: "巡检"}
	job.Payload.Target = &cron.CronTarget{OwnerType: "user", OwnerID: "alice", Channel: "pico"}
	job.Payload.Message = "执行巡检"

	got := tool.ExecuteJob(context.Background(), job)
	if !strings.Contains(got, "cron claim 失败") {
		t.Fatalf("ExecuteJob() = %q", got)
	}
	if executor.lastPrompt != "" {
		t.Fatal("agent must not run when claim fails")
	}
}
