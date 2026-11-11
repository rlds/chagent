//go:build !windows

package jobs

import (
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestKillTerminatesProcessGroup proves the SIGTERM path reaches children of
// the shell wrapper, not just the wrapper: the backgrounded `sleep` must be
// gone after job_kill.
func TestKillTerminatesProcessGroup(t *testing.T) {
	m := newTestManager(t, Options{KillGrace: 500 * time.Millisecond})
	view, err := m.Run(`sleep 30 & echo "child:$!"; wait`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var childPID int
	if !waitFor(t, 5*time.Second, func() bool {
		res, outErr := m.Output(view.ID, OutputOptions{})
		if outErr != nil {
			return false
		}
		for _, line := range strings.Split(res.Output, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "child:") {
				continue
			}
			pid := strings.TrimSpace(strings.TrimPrefix(line, "child:"))
			for _, c := range pid {
				if c < '0' || c > '9' {
					return false
				}
			}
			if pid == "" {
				return false
			}
			childPID = atoi(pid)
			return true
		}
		return false
	}) {
		t.Fatal("job never reported its child pid")
	}

	if err := syscall.Kill(childPID, 0); err != nil {
		t.Fatalf("child %d is not alive before kill: %v", childPID, err)
	}
	killed, err := m.Kill(view.ID)
	if err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if killed.Status != StatusKilled {
		t.Fatalf("status = %q, want killed", killed.Status)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err != nil {
			return // child is gone: the process group was terminated
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d survived job_kill (process group not terminated)", childPID)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
