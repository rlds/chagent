package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/cron"
)

// CronRunClaim is returned by Client CronRunCoordinator after Server Claim.
type CronRunClaim struct {
	RunID         string `json:"run_id"`
	TaskID        string `json:"task_id"`
	SessionID     string `json:"session_id"`
	ProjectID     string `json:"project_id,omitempty"`
	OwnerType     string `json:"owner_type"`
	OwnerID       string `json:"owner_id"`
	AgentNickname string `json:"agent_nickname"`
}

type CronRunCoordinator struct {
	BaseURL string
	Token   string
	Agent   string
	Client  *http.Client
}

func NewCronRunCoordinator(baseURL, token, agent string) *CronRunCoordinator {
	return &CronRunCoordinator{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Token:   strings.TrimSpace(token),
		Agent:   strings.TrimSpace(agent),
		Client:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *CronRunCoordinator) enabled() bool {
	return c != nil && c.BaseURL != "" && c.Token != "" && c.Agent != ""
}

func (c *CronRunCoordinator) Claim(ctx context.Context, job *cron.CronJob) (*CronRunClaim, error) {
	if !c.enabled() {
		return nil, fmt.Errorf("cron coordinator is not configured")
	}
	target := job.Payload.Target
	req := map[string]any{
		"job_id":         job.ID,
		"job_name":       job.Name,
		"scheduled_at":   cronScheduledAt(job).Format(time.RFC3339Nano),
		"agent_nickname": c.Agent,
		"title":          "[Cron] " + job.Name,
	}
	if target != nil {
		req["owner_type"] = target.OwnerType
		req["owner_id"] = target.OwnerID
		req["project_id"] = target.ProjectID
	} else {
		req["legacy_to"] = job.Payload.To
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/cron/runs/claim", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Cron-Agent", c.Agent)
	httpReq.Header.Set("X-Cron-Token", c.Token)
	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cron claim failed: HTTP %d", resp.StatusCode)
	}
	var out CronRunClaim
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.RunID == "" || out.TaskID == "" || out.SessionID == "" {
		return nil, fmt.Errorf("cron claim returned incomplete result")
	}
	return &out, nil
}

func (c *CronRunCoordinator) Finish(ctx context.Context, runID, status string, result map[string]any, errText string, duration time.Duration) error {
	if !c.enabled() || runID == "" {
		return fmt.Errorf("cron coordinator is not configured")
	}
	body, _ := json.Marshal(map[string]any{
		"status":      status,
		"result":      result,
		"error":       errText,
		"duration_ms": duration.Milliseconds(),
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/cron/runs/"+runID+"/finish", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Cron-Agent", c.Agent)
	httpReq.Header.Set("X-Cron-Token", c.Token)
	resp, err := c.Client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("cron finish failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func cronScheduledAt(job *cron.CronJob) time.Time {
	if job != nil && job.State.NextRunAtMS != nil && *job.State.NextRunAtMS > 0 {
		return time.UnixMilli(*job.State.NextRunAtMS)
	}
	return time.Now()
}
