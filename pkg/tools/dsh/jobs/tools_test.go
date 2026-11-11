package jobs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/tools"
	toolshared "github.com/rlds/chagent/pkg/tools/shared"
)

func TestToolIdentityAndMetadata(t *testing.T) {
	m := newTestManager(t, Options{})
	cases := []struct {
		tool toolshared.Tool
		name string
	}{
		{NewJobRunTool(m), "job_run"},
		{NewJobListTool(m), "job_list"},
		{NewJobOutputTool(m), "job_output"},
		{NewJobKillTool(m), "job_kill"},
	}
	for _, tc := range cases {
		if got := tc.tool.Name(); got != tc.name {
			t.Fatalf("Name() = %q, want %q", got, tc.name)
		}
		if desc := tc.tool.Description(); strings.TrimSpace(desc) == "" {
			t.Fatalf("%s Description() is empty", tc.name)
		}
		params := tc.tool.Parameters()
		if params["type"] != "object" {
			t.Fatalf("%s Parameters() type = %v", tc.name, params["type"])
		}
		provider, ok := tc.tool.(toolshared.PromptMetadataProvider)
		if !ok {
			t.Fatalf("%s does not implement PromptMetadataProvider", tc.name)
		}
		meta := provider.PromptMetadata()
		want := toolshared.PromptMetadata{Layer: "capability", Slot: "dsh", Source: "tool_registry:dsh"}
		if meta != want {
			t.Fatalf("%s PromptMetadata() = %+v, want %+v", tc.name, meta, want)
		}
	}
}

func TestToolRequiredParams(t *testing.T) {
	m := newTestManager(t, Options{})
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"job_run", map[string]any{}},
		{"job_output", map[string]any{}},
		{"job_kill", map[string]any{}},
	} {
		var tool toolshared.Tool
		switch tc.name {
		case "job_run":
			tool = NewJobRunTool(m)
		case "job_output":
			tool = NewJobOutputTool(m)
		case "job_kill":
			tool = NewJobKillTool(m)
		}
		res := tool.Execute(context.Background(), tc.args)
		if !res.IsError || !strings.Contains(res.ForLLM, "错误") {
			t.Fatalf("%s missing-args result = %+v, want error", tc.name, res)
		}
	}
}

func TestRegisterAndFullToolRoundTrip(t *testing.T) {
	requirePOSIX(t)
	registry := tools.NewToolRegistry()
	m := newTestManager(t, Options{})
	Register(registry, m)
	for _, name := range []string{"job_run", "job_list", "job_output", "job_kill"} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("tool %q not registered", name)
		}
	}

	runTool := NewJobRunTool(m)
	res := runTool.Execute(context.Background(), map[string]any{
		"command": `printf 'from-tool\n'; sleep 0.2`,
	})
	if res.IsError {
		t.Fatalf("job_run error: %s", res.ForLLM)
	}
	var runEnvelope jobRunEnvelope
	if err := json.Unmarshal([]byte(res.ForLLM), &runEnvelope); err != nil {
		t.Fatalf("job_run JSON = %q: %v", res.ForLLM, err)
	}
	if runEnvelope.Job == nil || runEnvelope.Job.ID == "" {
		t.Fatalf("job_run envelope = %+v", runEnvelope)
	}
	jobID := runEnvelope.Job.ID

	listRes := NewJobListTool(m).Execute(context.Background(), map[string]any{"limit": 5})
	if listRes.IsError {
		t.Fatalf("job_list error: %s", listRes.ForLLM)
	}
	var listEnvelope jobListEnvelope
	if err := json.Unmarshal([]byte(listRes.ForLLM), &listEnvelope); err != nil {
		t.Fatalf("job_list JSON = %q: %v", listRes.ForLLM, err)
	}
	if listEnvelope.Count != 1 || listEnvelope.Jobs[0].ID != jobID {
		t.Fatalf("job_list = %+v", listEnvelope)
	}

	outRes := NewJobOutputTool(m).Execute(context.Background(), map[string]any{
		"job_id":     jobID,
		"wait":       true,
		"timeout_ms": 5000,
	})
	if outRes.IsError {
		t.Fatalf("job_output error: %s", outRes.ForLLM)
	}
	var outEnvelope jobOutputEnvelope
	if err := json.Unmarshal([]byte(outRes.ForLLM), &outEnvelope); err != nil {
		t.Fatalf("job_output JSON = %q: %v", outRes.ForLLM, err)
	}
	if outEnvelope.Job.ID == "" || outEnvelope.Job.Status != StatusComplete {
		t.Fatalf("job_output job = %+v", outEnvelope.Job)
	}
	if !strings.Contains(outEnvelope.Output, "from-tool") {
		t.Fatalf("job_output output = %q", outEnvelope.Output)
	}

	killRes := NewJobKillTool(m).Execute(context.Background(), map[string]any{"job_id": jobID})
	if killRes.IsError {
		t.Fatalf("job_kill error: %s", killRes.ForLLM)
	}
	var killEnvelope jobKillEnvelope
	if err := json.Unmarshal([]byte(killRes.ForLLM), &killEnvelope); err != nil {
		t.Fatalf("job_kill JSON = %q: %v", killRes.ForLLM, err)
	}
	if killEnvelope.Job == nil || killEnvelope.Job.Status != StatusComplete {
		t.Fatalf("job_kill on finished job = %+v", killEnvelope)
	}
	if killEnvelope.Killed {
		t.Fatalf("Killed = true for an already completed job")
	}
}

func TestToolArgValidation(t *testing.T) {
	m := newTestManager(t, Options{})
	cases := []struct {
		name string
		tool toolshared.Tool
		args map[string]any
	}{
		{"job_run command type", NewJobRunTool(m), map[string]any{"command": 42}},
		{"job_run negative timeout", NewJobRunTool(m), map[string]any{"command": "printf x", "timeout_ms": -1}},
		{"job_list limit", NewJobListTool(m), map[string]any{"limit": -1}},
		{"job_list status type", NewJobListTool(m), map[string]any{"status": 1}},
		{"job_output offset", NewJobOutputTool(m), map[string]any{"job_id": "x", "offset": -1}},
		{"job_output tail", NewJobOutputTool(m), map[string]any{"job_id": "x", "tail_lines": -1}},
		{"job_output wait type", NewJobOutputTool(m), map[string]any{"job_id": "x", "wait": "yes"}},
		{"job_output timeout", NewJobOutputTool(m), map[string]any{"job_id": "x", "timeout_ms": -5}},
		{"job_output max_bytes", NewJobOutputTool(m), map[string]any{"job_id": "x", "max_bytes": 0}},
		{"job_kill id type", NewJobKillTool(m), map[string]any{"job_id": 7}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := tc.tool.Execute(context.Background(), tc.args)
			if !res.IsError {
				t.Fatalf("res = %+v, want error", res)
			}
		})
	}
}

func TestToolUnknownJobAndUnavailableManager(t *testing.T) {
	m := newTestManager(t, Options{})
	out := NewJobOutputTool(m).Execute(context.Background(), map[string]any{"job_id": "missing"})
	if !out.IsError || !strings.Contains(out.ForLLM, "job_not_found") {
		t.Fatalf("job_output(unknown) = %+v", out)
	}
	kill := NewJobKillTool(m).Execute(context.Background(), map[string]any{"job_id": "missing"})
	if !kill.IsError || !strings.Contains(kill.ForLLM, "job_not_found") {
		t.Fatalf("job_kill(unknown) = %+v", kill)
	}

	unavailable := NewManager("", Options{})
	run := NewJobRunTool(unavailable).Execute(context.Background(), map[string]any{"command": "printf x"})
	if !run.IsError || !strings.Contains(run.ForLLM, "manager_unavailable") {
		t.Fatalf("job_run on unavailable manager = %+v", run)
	}
}

func TestToolJobRunTimeoutArg(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{KillGrace: 300 * time.Millisecond})
	res := NewJobRunTool(m).Execute(context.Background(), map[string]any{
		"command":    `sleep 30`,
		"timeout_ms": 120,
	})
	if res.IsError {
		t.Fatalf("job_run error: %s", res.ForLLM)
	}
	var envelope jobRunEnvelope
	if err := json.Unmarshal([]byte(res.ForLLM), &envelope); err != nil {
		t.Fatalf("job_run JSON = %q: %v", res.ForLLM, err)
	}
	out := settle(t, m, envelope.Job.ID)
	if out.Job.Status != StatusKilled || !out.Job.TimedOut {
		t.Fatalf("timeout job = %+v (timed_out=%v)", out.Job, out.Job.TimedOut)
	}
}

func TestNewManagerWithTools(t *testing.T) {
	m, run, list, output, kill := NewManagerWithTools(t.TempDir(), Options{})
	t.Cleanup(func() { _ = m.Close() })
	if run == nil || list == nil || output == nil || kill == nil || m == nil {
		t.Fatalf("NewManagerWithTools returned nil")
	}
	if run.Name() != "job_run" || list.Name() != "job_list" ||
		output.Name() != "job_output" || kill.Name() != "job_kill" {
		t.Fatalf("unexpected tool names: %s %s %s %s",
			run.Name(), list.Name(), output.Name(), kill.Name())
	}
}

func TestRegisterNilSafety(t *testing.T) {
	Register(nil, NewManager(t.TempDir(), Options{}))
	Register(tools.NewToolRegistry(), nil)
}
