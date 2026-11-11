package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requirePOSIX skips tests whose fixtures assume a POSIX shell.
func requirePOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
}

// newTestManager returns a manager over a temp workspace and closes it on
// cleanup.
func newTestManager(t *testing.T, opts Options) *Manager {
	t.Helper()
	m := NewManager(t.TempDir(), opts)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// waitFor polls cond until it is true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// settle waits for a job to leave running/stopping via job_output.
func settle(t *testing.T, m *Manager, id string) *OutputResult {
	t.Helper()
	res, err := m.Output(id, OutputOptions{Wait: true, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("Output(wait) error = %v", err)
	}
	if res.Job.Status == StatusRunning || res.Job.Status == StatusStopping {
		t.Fatalf("job %s did not settle: %+v", id, res.Job)
	}
	return res
}

func TestRunEchoOutputAndPersistence(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{})

	view, err := m.Run(`printf 'hello\n'`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if view.ID == "" || view.PID <= 0 || view.Status != StatusRunning {
		t.Fatalf("Run() view = %+v", view)
	}
	if !strings.Contains(view.Command, "hello") || view.LogPath == "" {
		t.Fatalf("Run() view lost command/log path: %+v", view)
	}

	res := settle(t, m, view.ID)
	if res.Job.Status != StatusComplete {
		t.Fatalf("status = %q, want completed (%+v)", res.Job.Status, res.Job)
	}
	if res.Job.ExitCode == nil || *res.Job.ExitCode != 0 {
		t.Fatalf("exit code = %v, want 0", res.Job.ExitCode)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Fatalf("output = %q, want hello", res.Output)
	}
	if res.NextOffset == 0 || res.NextOffset != res.TotalBytes {
		t.Fatalf("next_offset = %d, total = %d", res.NextOffset, res.TotalBytes)
	}

	// A second read from next_offset must be empty (nothing new).
	again, err := m.Output(view.ID, OutputOptions{Offset: res.NextOffset})
	if err != nil {
		t.Fatalf("Output(offset) error = %v", err)
	}
	if again.Output != "" || again.NextOffset != res.NextOffset {
		t.Fatalf("second read = %+v, want empty at same offset", again)
	}

	// Durable layout and permissions: <workspace>/.chagent/jobs/<id>/.
	jobDir := filepath.Dir(view.LogPath)
	if filepath.Dir(filepath.Dir(jobDir)) != filepath.Join(m.Workspace(), ".chagent") {
		t.Fatalf("job dir = %q, want under <workspace>/.chagent/jobs", jobDir)
	}
	for path, wantMode := range map[string]os.FileMode{
		jobDir:                             0o700,
		view.LogPath:                       0o600,
		filepath.Join(jobDir, jobMetaName): 0o600,
	} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("stat %s: %v", path, statErr)
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("%s mode = %o, want %o", path, got, wantMode)
		}
	}
}

func TestIncrementalOutputWhileRunning(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{})

	view, err := m.Run(`printf 'first\n'; sleep 0.6; printf 'second\n'`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var first *OutputResult
	if !waitFor(t, 5*time.Second, func() bool {
		res, outErr := m.Output(view.ID, OutputOptions{})
		if outErr != nil {
			return false
		}
		if strings.Contains(res.Output, "first") {
			first = res
			return true
		}
		return false
	}) {
		t.Fatalf("first chunk never appeared")
	}
	if first.Job.Status != StatusRunning {
		t.Fatalf("status = %q, want running while sleeping", first.Job.Status)
	}

	final := settle(t, m, view.ID)
	if final.Job.Status != StatusComplete || !strings.Contains(final.Output, "second") {
		t.Fatalf("final = %+v output=%q", final.Job, final.Output)
	}
	// Reading from the earlier offset must append the remaining chunk.
	rest, err := m.Output(view.ID, OutputOptions{Offset: first.NextOffset})
	if err != nil {
		t.Fatalf("Output(offset) error = %v", err)
	}
	if !strings.Contains(rest.Output, "second") {
		t.Fatalf("incremental read after offset %d = %q, want second", first.NextOffset, rest.Output)
	}
}

func TestTailLines(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{})
	view, err := m.Run(`printf 'a\nb\nc\nd\ne\n'`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	settle(t, m, view.ID)

	res, err := m.Output(view.ID, OutputOptions{TailLines: 2})
	if err != nil {
		t.Fatalf("Output(tail) error = %v", err)
	}
	if res.Output != "d\ne\n" {
		t.Fatalf("tail = %q, want d/e", res.Output)
	}
	if res.NextOffset != res.TotalBytes {
		t.Fatalf("next_offset = %d, want total %d", res.NextOffset, res.TotalBytes)
	}
}

func TestExitCodeAndFailure(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{})

	failing, err := m.Run(`exit 7`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	failed := settle(t, m, failing.ID)
	if failed.Job.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", failed.Job.Status)
	}
	if failed.Job.ExitCode == nil || *failed.Job.ExitCode != 7 {
		t.Fatalf("exit code = %v, want 7", failed.Job.ExitCode)
	}

	missing, err := m.Run(`definitely-not-a-command-xyz`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	missingRes := settle(t, m, missing.ID)
	if missingRes.Job.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", missingRes.Job.Status)
	}
	if missingRes.Job.ExitCode == nil || *missingRes.Job.ExitCode == 0 {
		t.Fatalf("exit code = %v, want non-zero", missingRes.Job.ExitCode)
	}
}

func TestKillRunningJob(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{KillGrace: 500 * time.Millisecond})

	view, err := m.Run(`sleep 30`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	started := time.Now()
	killed, err := m.Kill(view.ID)
	if err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if killed.Status != StatusKilled {
		t.Fatalf("status = %q, want killed", killed.Status)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Kill took %v", elapsed)
	}

	// Killing a finished job is a safe no-op.
	again, err := m.Kill(view.ID)
	if err != nil {
		t.Fatalf("Kill(finished) error = %v", err)
	}
	if again.Status != StatusKilled {
		t.Fatalf("status after second Kill = %q, want killed", again.Status)
	}
}

func TestConcurrentJobLimit(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{MaxConcurrentJobs: 2})
	t.Cleanup(func() { _ = m.Close() })

	var ids []string
	for i := 0; i < 2; i++ {
		view, err := m.Run(`sleep 30`)
		if err != nil {
			t.Fatalf("Run(%d) error = %v", i, err)
		}
		ids = append(ids, view.ID)
	}
	_, err := m.Run(`sleep 30`)
	if err == nil {
		t.Fatal("Run() succeeded past MaxConcurrentJobs")
	}
	if code := ErrorCode(err); code != "too_many_jobs" {
		t.Fatalf("error code = %q, want too_many_jobs (%v)", code, err)
	}

	// Freeing one slot allows a new job.
	if _, err := m.Kill(ids[0]); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if _, err := m.Run(`printf 'ok\n'`); err != nil {
		t.Fatalf("Run() after kill error = %v", err)
	}
}

func TestCloseKillsAllJobsAndIsIdempotent(t *testing.T) {
	requirePOSIX(t)
	m := NewManager(t.TempDir(), Options{KillGrace: 500 * time.Millisecond})

	var ids []string
	for i := 0; i < 2; i++ {
		view, err := m.Run(`sleep 30`)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		ids = append(ids, view.ID)
	}
	started := time.Now()
	if err := m.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 8*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if !m.Closed() {
		t.Fatal("Closed() = false after Close")
	}
	if _, err := m.Run(`printf 'x'`); ErrorCode(err) != "manager_closed" {
		t.Fatalf("Run() after Close error = %v, want manager_closed", err)
	}

	views, err := m.List("", 10)
	if err != nil {
		t.Fatalf("List() after Close error = %v", err)
	}
	for _, id := range ids {
		found := false
		for _, v := range views {
			if v.ID == id {
				found = true
				if v.Status != StatusKilled {
					t.Fatalf("job %s status after Close = %q, want killed", id, v.Status)
				}
			}
		}
		if !found {
			t.Fatalf("job %s missing from List after Close", id)
		}
	}
}

func TestOutputTruncationCap(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{MaxOutputBytes: 64})
	view, err := m.Run(`seq 1 300`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	res := settle(t, m, view.ID)
	if res.Job.Status != StatusComplete {
		t.Fatalf("status = %q", res.Job.Status)
	}
	if !res.Job.Truncated {
		t.Fatal("Truncated = false, want true past MaxOutputBytes")
	}
	if res.TotalBytes > 64 {
		t.Fatalf("total bytes = %d, want <= 64", res.TotalBytes)
	}
	if len(res.Output) > 64 {
		t.Fatalf("returned output len = %d, want <= 64", len(res.Output))
	}
	if res.Job.OutputBytes > 64 {
		t.Fatalf("OutputBytes = %d, want <= 64", res.Job.OutputBytes)
	}
}

func TestWaitTimeoutLeavesJobRunning(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{})
	view, err := m.Run(`sleep 30`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	res, err := m.Output(view.ID, OutputOptions{Wait: true, Timeout: 150 * time.Millisecond})
	if err != nil {
		t.Fatalf("Output(wait) error = %v", err)
	}
	if !res.WaitedOut {
		t.Fatalf("WaitedOut = false, want true (result %+v)", res)
	}
	if res.Job.Status != StatusRunning {
		t.Fatalf("status = %q, want running after timeout", res.Job.Status)
	}
}

func TestRunTimeoutMarksTimedOut(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{KillGrace: 300 * time.Millisecond})
	view, err := m.RunWithTimeout(`sleep 30`, 150*time.Millisecond)
	if err != nil {
		t.Fatalf("RunWithTimeout() error = %v", err)
	}
	res := settle(t, m, view.ID)
	if res.Job.Status != StatusKilled {
		t.Fatalf("status = %q, want killed after timeout", res.Job.Status)
	}
	if !res.Job.TimedOut {
		t.Fatalf("TimedOut = false, want true (%+v)", res.Job)
	}
}

func TestListFilterLimitAndPruning(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{MaxRetainedJobs: 2})
	for _, cmd := range []string{`printf 'a'`, `printf 'b'`, `printf 'c'`, `printf 'd'`} {
		view, err := m.Run(cmd)
		if err != nil {
			t.Fatalf("Run(%s) error = %v", cmd, err)
		}
		settle(t, m, view.ID)
	}
	views, err := m.List("", 10)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("retained jobs = %d, want 2", len(views))
	}
	for _, v := range views {
		if v.Status != StatusComplete {
			t.Fatalf("retained job status = %q", v.Status)
		}
	}
	completed, err := m.List(StatusComplete, 1)
	if err != nil {
		t.Fatalf("List(completed) error = %v", err)
	}
	if len(completed) != 1 {
		t.Fatalf("List(completed,1) = %d, want 1", len(completed))
	}
	running, err := m.List(StatusRunning, 10)
	if err != nil {
		t.Fatalf("List(running) error = %v", err)
	}
	if len(running) != 0 {
		t.Fatalf("List(running) = %d, want 0", len(running))
	}
	if _, err := m.List("bogus", 10); ErrorCode(err) != "invalid_status" {
		t.Fatalf("List(bogus) error = %v", err)
	}
}

func TestManagerErrors(t *testing.T) {
	m := newTestManager(t, Options{})
	if _, err := m.Run("   "); ErrorCode(err) != "invalid_command" {
		t.Fatalf("Run(empty) error = %v", err)
	}
	if _, err := m.Output("nope", OutputOptions{}); ErrorCode(err) != "job_not_found" {
		t.Fatalf("Output(unknown) error = %v", err)
	}
	if _, err := m.Kill("nope"); ErrorCode(err) != "job_not_found" {
		t.Fatalf("Kill(unknown) error = %v", err)
	}
	if _, err := m.Output("nope", OutputOptions{Offset: -1}); ErrorCode(err) != "invalid_output_request" {
		t.Fatalf("Output(offset<0) error = %v", err)
	}
	if _, err := m.Output("nope", OutputOptions{TailLines: MaxTailLines + 1}); ErrorCode(err) != "invalid_output_request" {
		t.Fatalf("Output(tail too big) error = %v", err)
	}
	if _, err := m.Output("nope", OutputOptions{TailLines: -1}); ErrorCode(err) != "invalid_output_request" {
		t.Fatalf("Output(tail<0) error = %v", err)
	}

	unavailable := NewManager("", Options{})
	_, err := unavailable.Run(`printf 'x'`)
	if ErrorCode(err) != "manager_unavailable" {
		t.Fatalf("Run on empty workspace error = %v", err)
	}
	var je *Error
	if !errors.As(err, &je) || je.Message == "" {
		t.Fatalf("error = %v, want descriptive *Error", err)
	}
}

func TestOutputReadsAfterJobSettles(t *testing.T) {
	requirePOSIX(t)
	m := newTestManager(t, Options{})
	view, err := m.Run(`printf 'done\n'`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	settle(t, m, view.ID)
	res, err := m.Output(view.ID, OutputOptions{Wait: true, Timeout: time.Second})
	if err != nil {
		t.Fatalf("Output() error = %v", err)
	}
	if res.WaitedOut {
		t.Fatalf("WaitedOut = true for a settled job")
	}
	if res.Job.FinishedAt == "" {
		t.Fatalf("FinishedAt empty after settle: %+v", res.Job)
	}
}
