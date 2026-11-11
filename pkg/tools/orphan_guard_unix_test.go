//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestOrphanGuardKillsProcessGroupOnParentDeath simulates the gateway being
// SIGKILLed by closing the guard pipe (the kernel does the same when the parent
// dies): the target process group must be terminated promptly.
func TestOrphanGuardKillsProcessGroupOnParentDeath(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	})

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	stop, err := startOrphanGuard(pid)
	if err != nil {
		t.Fatalf("startOrphanGuard: %v", err)
	}
	// Simulate parent death: closing the pipe is what the kernel does on
	// SIGKILL; the watchdog must terminate the group.
	stop()

	select {
	case <-waited:
		// Reaped: the process group is gone.
	case <-time.After(5 * time.Second):
		t.Fatalf("child process %d still alive after guard fired", pid)
	}
}
