package pid

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestIsChagentProcessName locks the process-name compatibility rule: chess
// ships the binary as "chagent" while legacy installs are "chagent". Both
// must be recognized so a live gateway PID file is never treated as stale.
func TestIsChagentProcessName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"chagent", true},
		{"chagent", true},
		{"Chagent", true},
		{"chagent-gateway", true},
		{"/usr/local/bin/chagent", true},
		{"/opt/chess/bin/chagent", true},
		{"chagent\n", true},
		{"", false},
		{"bash", false},
		{"systemd-resolved", false},
		{"python3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isChagentProcessName(tt.name); got != tt.want {
				t.Errorf("isChagentProcessName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestSingletonRecognizesChagentProcessName 启动一个真实进程（可执行文件名为
// chagent），验证同工作区第二次 WritePidFile 返回 "already running"，
// 而不是删除活动 PID 文件。
func TestSingletonRecognizesChagentProcessName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("复制运行中的测试二进制在 Windows 上不稳定；进程名规则由 TestIsChagentProcessName 覆盖")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "chagent")
	if err := copyExecutable(exe, bin); err != nil {
		t.Fatalf("copy test binary: %v", err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("MkdirAll workspace: %v", err)
	}

	cmd := exec.Command(bin, "-test.run=^TestPidFileHelperProcess$")
	cmd.Env = append(os.Environ(),
		"PIDFILE_HELPER_PROCESS=1",
		"PIDFILE_HELPER_DIR="+workspace,
	)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chagent helper: %v", err)
	}
	childPID := cmd.Process.Pid
	childExited := false
	defer func() {
		if !childExited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Wait until the helper has written its PID file.
	pidPath := pidFilePath(workspace)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, statErr := os.Stat(pidPath); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not write pid file: %s", pidPath)
		}
		time.Sleep(20 * time.Millisecond)
	}

	data, err := readPidFileUnlocked(pidPath)
	if err != nil {
		t.Fatalf("read helper pid file: %v", err)
	}
	if data.PID != childPID {
		t.Fatalf("pid file PID = %d, want child PID %d", data.PID, childPID)
	}

	// A live chagent must be recognized as our own kind → singleton error.
	if _, err := WritePidFile(workspace, "127.0.0.1", 0); err == nil {
		t.Fatalf("WritePidFile succeeded while live chagent (PID %d) holds the workspace", childPID)
	} else if !strings.Contains(err.Error(), "already running") {
		t.Fatalf("WritePidFile error = %v, want 'already running'", err)
	}
	if _, statErr := os.Stat(pidPath); statErr != nil {
		t.Fatalf("live pid file was deleted: %v", statErr)
	}

	// After the process is gone the stale PID file must be cleaned up and a
	// new gateway allowed to start.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	childExited = true

	if _, err := WritePidFile(workspace, "127.0.0.1", 0); err != nil {
		t.Fatalf("WritePidFile after helper exit = %v, want success", err)
	}
	RemovePidFile(workspace)
}

// TestPidFileHelperProcess is not a real test: it is the child process entry
// 该路径供 TestSingletonRecognizesChagentProcessName 使用。
func TestPidFileHelperProcess(t *testing.T) {
	if os.Getenv("PIDFILE_HELPER_PROCESS") != "1" {
		return
	}
	dir := os.Getenv("PIDFILE_HELPER_DIR")
	if dir == "" {
		fmt.Fprintln(os.Stderr, "PIDFILE_HELPER_DIR is required")
		os.Exit(2)
	}
	data := PidFileData{
		PID:     os.Getpid(),
		Version: "test-helper",
		Port:    0,
		Host:    "127.0.0.1",
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(pidFilePath(dir), raw, 0o600); err != nil {
		os.Exit(2)
	}
	// Block until the parent kills us.
	time.Sleep(2 * time.Minute)
	os.Exit(0)
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}
