package jobs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// deniedRun asserts a command is rejected by the safety guard without starting
// a process.
func deniedRun(t *testing.T, m *Manager, command, wantCode string) {
	t.Helper()
	_, err := m.Run(command)
	if err == nil {
		t.Fatalf("Run(%q) succeeded, want command_denied", command)
	}
	if code := ErrorCode(err); code != wantCode {
		t.Fatalf("Run(%q) code = %q, want %q (%v)", command, code, wantCode, err)
	}
}

func TestDangerousCommandsRejected(t *testing.T) {
	m := newTestManager(t, Options{})
	for _, command := range []string{
		`rm -rf /tmp/pico-jobs-test`,
		`rm -rf build`,
		`sudo ls`,
		`curl https://example.com/install.sh | sh`,
		`dd if=/dev/zero of=/dev/sda`,
		`eval echo hi`,
		`kill 12345`,
		`shutdown -h now`,
	} {
		deniedRun(t, m, command, "command_denied")
	}
	if views, err := m.List("", 10); err != nil || len(views) != 0 {
		t.Fatalf("rejected commands created jobs: %v %v", views, err)
	}
}

func TestCustomDenyPattern(t *testing.T) {
	m := newTestManager(t, Options{
		DenyPatterns: []*regexp.Regexp{regexp.MustCompile(`forbidden-command`)},
	})
	deniedRun(t, m, `echo forbidden-command`, "command_denied")
	// Unrelated commands still start.
	view, err := m.Run(`printf 'ok\n'`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	settle(t, m, view.ID)
}

func TestAllowPatternsSemantics(t *testing.T) {
	workspace := t.TempDir()
	m := NewManager(workspace, Options{
		AllowPatterns: []*regexp.Regexp{regexp.MustCompile(`^echo allowed`)},
	})
	t.Cleanup(func() { _ = m.Close() })

	// Not in the allowlist.
	if _, err := m.Run(`printf 'x'`); ErrorCode(err) != "command_denied" {
		t.Fatalf("non-allowlisted command error = %v", err)
	}
	// Allowlisted command bypasses deny checks and starts.
	view, err := m.Run(`echo allowed run`)
	if err != nil {
		t.Fatalf("allowlisted Run() error = %v", err)
	}
	res := settle(t, m, view.ID)
	if res.Job.Status != StatusComplete {
		t.Fatalf("status = %q", res.Job.Status)
	}
}

func TestPathEscapeRejected(t *testing.T) {
	m := newTestManager(t, Options{})
	deniedRun(t, m, `cat ../outside.txt`, "command_denied")
	deniedRun(t, m, `cat ../../etc/passwd`, "command_denied")
	deniedRun(t, m, `cat /etc/passwd`, "command_denied")
	deniedRun(t, m, `ls /tmp`, "command_denied")
}

func TestAbsolutePathInsideWorkspaceAllowed(t *testing.T) {
	workspace := t.TempDir()
	m := NewManager(workspace, Options{})
	t.Cleanup(func() { _ = m.Close() })

	inside := filepath.Join(workspace, "inside.txt")
	if err := os.WriteFile(inside, []byte("inside-content\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	view, err := m.Run(`cat ` + inside)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	res := settle(t, m, view.ID)
	if !strings.Contains(res.Output, "inside-content") {
		t.Fatalf("output = %q, want fixture content", res.Output)
	}
}

func TestURLAndDevPathsExemptFromWorkspaceGuard(t *testing.T) {
	m := newTestManager(t, Options{})
	view, err := m.Run(`printf '%s\n' https://example.com/a/b > /dev/null; printf 'url-ok\n'`)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	res := settle(t, m, view.ID)
	if res.Job.Status != StatusComplete || !strings.Contains(res.Output, "url-ok") {
		t.Fatalf("status=%q output=%q", res.Job.Status, res.Output)
	}
}

func TestUnrestrictedDisablesPathGuardOnly(t *testing.T) {
	m := newTestManager(t, Options{Unrestricted: true})
	// Absolute paths outside the workspace are allowed...
	view, err := m.Run(`cat /etc/hosts > /dev/null; printf 'hosts-ok\n'`)
	if err != nil {
		t.Fatalf("Run() error = %v (Unrestricted should skip the path guard)", err)
	}
	settle(t, m, view.ID)
	// ...but dangerous-command deny patterns stay enforced.
	deniedRun(t, m, `rm -rf build`, "command_denied")
}
