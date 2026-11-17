package commands

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rlds/chagent/pkg/cron"
)

func newCronCommandExecutor(t *testing.T) (*Executor, *cron.CronService) {
	t.Helper()
	service := cron.NewCronService(filepath.Join(t.TempDir(), "jobs.json"), nil)
	rt := &Runtime{Cron: service}
	return NewExecutor(NewRegistry(BuiltinDefinitions()), rt), service
}

func runCronCommand(t *testing.T, ex *Executor, text string) string {
	t.Helper()
	var reply string
	result := ex.Execute(context.Background(), Request{
		Channel: "pico",
		ChatID:  "pico:user-1",
		Text:    text,
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if result.Err != nil {
		t.Fatalf("command %q error: %v", text, result.Err)
	}
	return reply
}

func TestCronCommand_AddListGetStopStartRemove(t *testing.T) {
	ex, service := newCronCommandExecutor(t)

	addReply := runCronCommand(t, ex, `/cron add name="每日巡检" every_seconds=3600 message="检查主机状态"`)
	if !strings.Contains(addReply, "定时任务已创建") {
		t.Fatalf("add reply = %q", addReply)
	}
	re := regexp.MustCompile(`ID：([^\s]+)`)
	match := re.FindStringSubmatch(addReply)
	if len(match) != 2 {
		t.Fatalf("cannot parse job id from %q", addReply)
	}
	id := match[1]

	listReply := runCronCommand(t, ex, "/cron list")
	if !strings.Contains(listReply, id) || !strings.Contains(listReply, "每日巡检") {
		t.Fatalf("list reply missing job: %q", listReply)
	}

	getReply := runCronCommand(t, ex, "/cron get "+id)
	if !strings.Contains(getReply, "检查主机状态") || !strings.Contains(getReply, "每 1h0m0s") {
		t.Fatalf("get reply = %q", getReply)
	}

	stopReply := runCronCommand(t, ex, "/cron stop "+id)
	if !strings.Contains(stopReply, "已停止") {
		t.Fatalf("stop reply = %q", stopReply)
	}
	job, ok := service.GetJob(id)
	if !ok || job.Enabled {
		t.Fatalf("job should be disabled: ok=%v job=%+v", ok, job)
	}

	startReply := runCronCommand(t, ex, "/cron start "+id)
	if !strings.Contains(startReply, "已开始") {
		t.Fatalf("start reply = %q", startReply)
	}
	job, _ = service.GetJob(id)
	if !job.Enabled || job.State.NextRunAtMS == nil {
		t.Fatalf("job should be enabled with next run: %+v", job)
	}

	deleteReply := runCronCommand(t, ex, "/cron delete "+id)
	if !strings.Contains(deleteReply, "已删除") {
		t.Fatalf("delete reply = %q", deleteReply)
	}
	if _, ok := service.GetJob(id); ok {
		t.Fatal("job should be deleted")
	}
}

func TestCronCommand_AddCronExpressionAndValidation(t *testing.T) {
	ex, _ := newCronCommandExecutor(t)

	reply := runCronCommand(t, ex, `/cron add name="每天九点" cron_expr="0 9 * * *" message="生成日报"`)
	if !strings.Contains(reply, "cron: 0 9 * * *") {
		t.Fatalf("add cron expression reply = %q", reply)
	}

	noMessage := runCronCommand(t, ex, "/cron add every_seconds=60")
	if !strings.Contains(noMessage, "缺少 message") {
		t.Fatalf("missing message reply = %q", noMessage)
	}

	noSchedule := runCronCommand(t, ex, `/cron add message="测试"`)
	if !strings.Contains(noSchedule, "必须指定") {
		t.Fatalf("missing schedule reply = %q", noSchedule)
	}

	command := runCronCommand(t, ex, `/cron add every_seconds=60 message="测试" command="df -h"`)
	if !strings.Contains(command, "不支持 shell command") {
		t.Fatalf("command rejection reply = %q", command)
	}
}

func TestCronCommand_ParseQuotedFields(t *testing.T) {
	fields := parseCronFields(`/cron add name="每日 巡检" message="检查 主机 状态" every_seconds=60`, 2)
	if fields["name"] != "每日 巡检" {
		t.Fatalf("name = %q", fields["name"])
	}
	if fields["message"] != "检查 主机 状态" {
		t.Fatalf("message = %q", fields["message"])
	}
	if fields["every_seconds"] != "60" {
		t.Fatalf("every_seconds = %q", fields["every_seconds"])
	}
}
