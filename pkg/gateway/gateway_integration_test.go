// Chagent - 超轻量级个人 AI agent

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// 这些测试在进程内驱动 runGateway 的完整生命周期（启动 → 健康 → reload/热重载 → SIGTERM 关闭），
// 为 pkg/gateway 覆盖率补齐真实主路径，并把「/reload 后 SIGTERM 必须干净退出」的 P0 回归锁在 gateway 层。
// 注意：共享全局 logger/pid 状态，测试不使用 t.Parallel()，每个用例独立 TempDir。

const gatewayTestReadyTimeout = 20 * time.Second

type gatewayTestProc struct {
	home       string
	workspace  string
	configPath string
	sig        chan os.Signal
	done       chan error
	stopped    bool
}

func writeGatewayTestConfig(t *testing.T, dir string, mutate func(cfg map[string]any)) (home, workspace, configPath string) {
	t.Helper()
	home = filepath.Join(dir, "home")
	workspace = filepath.Join(dir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	cfg := map[string]any{
		"version": 2,
		"model_list": []any{
			map[string]any{
				"model_name": "fake",
				"model":      "fake",
				"api_base":   "http://127.0.0.1:1",
				"api_key":    "test-key",
			},
		},
		"agents": map[string]any{
			"defaults": map[string]any{
				"workspace": workspace,
			},
		},
		"gateway": map[string]any{
			"host":      "127.0.0.1",
			"port":      0,
			"log_level": "info",
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	configPath = filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return home, workspace, configPath
}

func startGatewayTestProc(t *testing.T, home, configPath string) *gatewayTestProc {
	t.Helper()
	proc := &gatewayTestProc{
		home:       home,
		configPath: configPath,
		sig:        make(chan os.Signal, 1),
		done:       make(chan error, 1),
	}
	go func() {
		proc.done <- runGateway(false, home, configPath, false, proc.sig)
	}()
	t.Cleanup(func() {
		if proc.stopped {
			return
		}
		select {
		case proc.sig <- syscall.SIGTERM:
		case <-time.After(2 * time.Second):
		}
		select {
		case <-proc.done:
		case <-time.After(10 * time.Second):
			t.Logf("cleanup: gateway goroutine did not exit after SIGTERM")
		}
	})
	return proc
}

func (p *gatewayTestProc) pidPath() string {
	return filepath.Join(p.workspace, ".chagent.pid")
}

func (p *gatewayTestProc) readPID(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile(p.pidPath())
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	var info map[string]any
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("decode pid file: %v", err)
	}
	return info
}

func (p *gatewayTestProc) waitReady(t *testing.T) (host string, port int) {
	t.Helper()
	deadline := time.Now().Add(gatewayTestReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-p.done:
			t.Fatalf("gateway exited before ready: %v", err)
		default:
		}
		if data, err := os.ReadFile(p.pidPath()); err == nil {
			var info struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			}
			if json.Unmarshal(data, &info) == nil && info.Host != "" && info.Port > 0 {
				if gatewayHealthOK(info.Host, info.Port) {
					p.workspace = filepath.Dir(p.pidPath())
					return info.Host, info.Port
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("gateway did not become ready within %s", gatewayTestReadyTimeout)
	return "", 0
}

func (p *gatewayTestProc) stopAndWait(t *testing.T, timeout time.Duration) time.Duration {
	t.Helper()
	p.stopped = true
	started := time.Now()
	p.sig <- syscall.SIGTERM
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("gateway shutdown returned error: %v", err)
		}
		return time.Since(started)
	case <-time.After(timeout):
		t.Fatalf("gateway did not exit within %s after SIGTERM", timeout)
		return 0
	}
}

func gatewayHealthOK(host string, port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/health", host, port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func gatewayHealthUnreachable(host string, port int) bool {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/health", host, port))
	if err != nil {
		return true
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return false
}

func gatewayPostReload(t *testing.T, host string, port int, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s:%d/reload", host, port), nil)
	if err != nil {
		t.Fatalf("new reload request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /reload: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func waitLogContains(t *testing.T, path string, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), needle) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("log %s did not contain %q within %s", path, needle, timeout)
}

func rewriteGatewayTestLogLevel(t *testing.T, configPath, level string, extra func(cfg map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	gateway, ok := cfg["gateway"].(map[string]any)
	if !ok {
		gateway = map[string]any{}
		cfg["gateway"] = gateway
	}
	gateway["log_level"] = level
	if extra != nil {
		extra(cfg)
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(configPath, out, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func newGatewayTestProc(t *testing.T, mutate func(cfg map[string]any)) *gatewayTestProc {
	t.Helper()
	dir := t.TempDir()
	home, workspace, configPath := writeGatewayTestConfig(t, dir, mutate)
	proc := startGatewayTestProc(t, home, configPath)
	proc.workspace = workspace
	return proc
}

// TestGatewayLifecycleStartAndCleanShutdown 覆盖最常用主路径：
// 加载配置 → 打开监听 → 写 pid → health 就绪 → SIGTERM → 5s 内退出并删除 pid。
func TestGatewayLifecycleStartAndCleanShutdown(t *testing.T) {
	proc := newGatewayTestProc(t, nil)
	host, port := proc.waitReady(t)

	if !gatewayHealthOK(host, port) {
		t.Fatalf("/health not ready at %s:%d", host, port)
	}
	if _, err := os.Stat(proc.pidPath()); err != nil {
		t.Fatalf("pid file missing: %v", err)
	} else {
		if mode := fileMode(t, proc.pidPath()); mode != 0o600 {
			t.Fatalf("pid file mode = %o, want 0600", mode)
		}
	}

	elapsed := proc.stopAndWait(t, 5*time.Second)
	if elapsed > 5*time.Second {
		t.Fatalf("shutdown took %s, want <=5s", elapsed)
	}
	if _, err := os.Stat(proc.pidPath()); !os.IsNotExist(err) {
		t.Fatalf("pid file still exists after shutdown (err=%v)", err)
	}
	if !gatewayHealthUnreachable(host, port) {
		t.Fatalf("health endpoint still reachable after shutdown")
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// TestGatewayReloadThenCleanShutdown 锁定 P0 回归：一次成功 /reload 后
// SIGTERM 仍须在 5s 内完成关闭并删除 pid（修复前会永久挂死）。
func TestGatewayReloadThenCleanShutdown(t *testing.T) {
	proc := newGatewayTestProc(t, nil)
	host, port := proc.waitReady(t)
	info := proc.readPID(t)
	token, _ := info["token"].(string)
	if token == "" {
		t.Fatal("pid file token is empty")
	}

	// 计划内修改配置（无害字段），随后通过 /reload 热更新。
	rewriteGatewayTestLogLevel(t, proc.configPath, "debug", nil)
	status, body := gatewayPostReload(t, host, port, token)
	if status != http.StatusOK {
		t.Fatalf("/reload status=%d body=%q", status, body)
	}
	if !gatewayHealthOK(host, port) {
		t.Fatal("/health not OK after reload")
	}
	// 给 reload 一点时间在 gateway 主循环内完成，避免与 SIGTERM 竞争造成假失败。
	time.Sleep(150 * time.Millisecond)

	elapsed := proc.stopAndWait(t, 5*time.Second)
	if elapsed > 5*time.Second {
		t.Fatalf("shutdown after reload took %s, want <=5s", elapsed)
	}
	if _, err := os.Stat(proc.pidPath()); !os.IsNotExist(err) {
		t.Fatalf("pid file still exists after shutdown following reload (err=%v)", err)
	}
	if !gatewayHealthUnreachable(host, port) {
		t.Fatal("health endpoint still reachable after shutdown following reload")
	}
}

// TestGatewayReloadInvalidConfigKeepsServing 覆盖 manual reload 失败分支：
// 坏配置 reload 必须被拒绝、旧服务保持健康、随后仍能干净关闭。
func TestGatewayReloadInvalidConfigKeepsServing(t *testing.T) {
	proc := newGatewayTestProc(t, nil)
	host, port := proc.waitReady(t)
	token, _ := proc.readPID(t)["token"].(string)

	if err := os.WriteFile(proc.configPath, []byte("{invalid-json"), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	// /reload 只负责把重载请求入队，返回 200；实际加载失败在主循环内记录并保留旧配置。
	status, body := gatewayPostReload(t, host, port, token)
	if status != http.StatusOK {
		t.Fatalf("/reload trigger status=%d body=%q, want 200 (async trigger)", status, body)
	}
	waitLogContains(t, filepath.Join(proc.home, "logs", "gateway.log"), "Error loading config for manual reload", 10*time.Second)
	if !gatewayHealthOK(host, port) {
		t.Fatal("gateway health lost after rejected reload")
	}
	proc.stopAndWait(t, 5*time.Second)
}

// TestGatewayHotReloadLifecycle 覆盖 hot_reload watcher 路径：
// 修改 config 后自动 reload，随后 SIGTERM 干净退出。
func TestGatewayHotReloadLifecycle(t *testing.T) {
	proc := newGatewayTestProc(t, func(cfg map[string]any) {
		gateway := cfg["gateway"].(map[string]any)
		gateway["hot_reload"] = true
	})
	host, port := proc.waitReady(t)

	rewriteGatewayTestLogLevel(t, proc.configPath, "debug", nil)
	waitLogContains(t, filepath.Join(proc.home, "logs", "gateway.log"), "Config file changed", 20*time.Second)
	if !gatewayHealthOK(host, port) {
		t.Fatal("gateway health lost after hot reload")
	}
	proc.stopAndWait(t, 5*time.Second)
}

// TestGatewayAllowEmptyStartupLimitedMode 覆盖 allowEmptyStartup 的受限启动路径
// （无可用模型仍启动，仅对话不可用），扩大启动分支覆盖率。
func TestGatewayAllowEmptyStartupLimitedMode(t *testing.T) {
	dir := t.TempDir()
	home, workspace, configPath := writeGatewayTestConfig(t, dir, func(cfg map[string]any) {
		cfg["model_list"] = []any{}
	})
	_ = workspace

	sig := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() {
		done <- runGateway(false, home, configPath, true, sig)
	}()
	pidPath := filepath.Join(workspace, ".chagent.pid")
	deadline := time.Now().Add(gatewayTestReadyTimeout)
	ready := false
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("limited-mode gateway exited before ready: %v", err)
		default:
		}
		if data, err := os.ReadFile(pidPath); err == nil {
			var info struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			}
			if json.Unmarshal(data, &info) == nil && info.Host != "" && info.Port > 0 && gatewayHealthOK(info.Host, info.Port) {
				ready = true
				sig <- syscall.SIGTERM
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatal("limited-mode gateway did not become ready")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("limited-mode gateway shutdown error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("limited-mode gateway did not exit within 5s")
	}
}
