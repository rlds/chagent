package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rlds/chagent/pkg/agent"
	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/netbind"
	"github.com/rlds/chagent/pkg/providers"
)

// --- stubs ---------------------------------------------------------------

type stubProvider struct {
	content string
	err     error
	calls   int
}

func (s *stubProvider) Chat(
	_ context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &providers.LLMResponse{Content: s.content, FinishReason: "stop"}, nil
}

func (s *stubProvider) GetDefaultModel() string { return "stub" }

type statefulStubProvider struct {
	stubProvider
	closed bool
}

func (s *statefulStubProvider) Close() { s.closed = true }

// --- small helpers -------------------------------------------------------

func waitGatewayReady(t *testing.T, workspace string, timeout time.Duration) (host string, port int, ok bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	pidPath := filepath.Join(workspace, ".chagent.pid")
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidPath); err == nil {
			var info struct {
				Host string `json:"host"`
				Port int    `json:"port"`
			}
			if json.Unmarshal(data, &info) == nil && info.Host != "" && info.Port > 0 &&
				gatewayHealthOK(info.Host, info.Port) {
				return info.Host, info.Port, true
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return "", 0, false
}

func startRunGateway(
	t *testing.T,
	debug, allowEmpty bool,
	home, configPath string,
) (done chan error, sig chan os.Signal) {
	t.Helper()
	done = make(chan error, 1)
	sig = make(chan os.Signal, 1)
	go func() { done <- runGateway(debug, home, configPath, allowEmpty, sig) }()
	return done, sig
}

func stopRunGateway(t *testing.T, done chan error, sig chan os.Signal, timeout time.Duration) {
	t.Helper()
	select {
	case sig <- syscall.SIGTERM:
	case <-time.After(2 * time.Second):
		t.Log("gateway did not accept SIGTERM in time")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runGateway returned error on shutdown: %v", err)
		}
	case <-time.After(timeout):
		t.Fatalf("gateway did not exit within %s", timeout)
	}
}

// gatewayConfigFile builds a config file from a JSON object (already parsed).
func writeGatewayConfigJSON(t *testing.T, path string, cfg map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func baseGatewayConfig(workspace string) map[string]any {
	return map[string]any{
		"version": 2,
		"model_list": []any{
			map[string]any{
				"model_name": "fake",
				"model":      "fake",
				"provider":   "openai",
				"api_base":   "http://127.0.0.1:1",
				"api_key":    "test-key",
			},
		},
		"agents": map[string]any{
			"defaults": map[string]any{"workspace": workspace},
		},
		"gateway": map[string]any{"host": "127.0.0.1", "port": 0, "log_level": "info"},
	}
}

// --- unit tests ----------------------------------------------------------

func TestStartupBlockedProviderBehaviour(t *testing.T) {
	t.Parallel()
	provider := &startupBlockedProvider{reason: "limited mode reason"}
	if got := provider.GetDefaultModel(); got != "" {
		t.Fatalf("GetDefaultModel = %q, want empty", got)
	}
	if _, err := provider.Chat(context.Background(), nil, nil, "m", nil); err == nil ||
		!strings.Contains(err.Error(), "limited mode reason") {
		t.Fatalf("Chat error = %v, want limited-mode reason", err)
	}
}

func TestStartupInfoIntAllNumericTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   any
		want int
		ok   bool
	}{
		{int(7), 7, true},
		{int32(8), 8, true},
		{int64(9), 9, true},
		{float32(10), 10, true},
		{float64(11), 11, true},
		{"12", 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		got, ok := startupInfoInt(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("startupInfoInt(%#v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPublishGatewayEventEdgeCases(t *testing.T) {
	// nil agent loop must be a no-op.
	publishGatewayEvent(nil, runtimeevents.KindGatewayStart, time.Time{}, nil)

	busCh := bus.NewMessageBus()
	_ = busCh
	eventBus := runtimeevents.NewBus()
	t.Cleanup(func() { _ = eventBus.Close() })
	al := agent.NewAgentLoop(
		config.DefaultConfig(),
		bus.NewMessageBus(),
		&startupBlockedProvider{reason: "not used"},
		agent.WithRuntimeEvents(eventBus),
	)
	t.Cleanup(al.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sub, ch, err := eventBus.Channel().OfKind(runtimeevents.KindGatewayReloadFailed).SubscribeChan(
		ctx, runtimeevents.SubscribeOptions{Name: "gateway-coverage", Buffer: 4},
	)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Close() })

	publishGatewayEvent(al, runtimeevents.KindGatewayReloadFailed, time.Time{}, context.DeadlineExceeded)
	evt := receiveGatewayRuntimeEvent(t, ch)
	if evt.Severity != runtimeevents.SeverityError {
		t.Fatalf("severity = %q, want error", evt.Severity)
	}
	if evt.Attrs["error"] == nil {
		t.Fatalf("attrs missing error: %#v", evt.Attrs)
	}
	if _, ok := evt.Attrs["duration_ms"]; ok {
		t.Fatalf("zero startedAt should not emit duration_ms: %#v", evt.Attrs)
	}
}

func TestGetFileModTimeAndSizeMissingPath(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "nope", "config.json")
	if got := getFileModTime(missing); !got.IsZero() {
		t.Fatalf("getFileModTime(missing) = %v, want zero", got)
	}
	if got := getFileSize(missing); got != 0 {
		t.Fatalf("getFileSize(missing) = %d, want 0", got)
	}
}

func TestOpenGatewayListenersErrorPaths(t *testing.T) {
	t.Parallel()
	if _, _, err := openGatewayListeners("bad host!/", 0); err == nil {
		t.Fatal("expected BuildPlan error for invalid host")
	}

	if _, _, err := openGatewayListeners("127.0.0.1,", 0); err == nil {
		t.Fatal("expected BuildPlan error for empty host-list entry")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	if _, _, err := openGatewayListeners("127.0.0.1", port); err == nil {
		t.Fatalf("expected port-in-use error for occupied port %d", port)
	}
}

func TestSetupCronToolExecConfigError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Tools.Exec.EnableDenyPatterns = true
	cfg.Tools.Exec.CustomDenyPatterns = []string{"("} // invalid regexp
	msgBus := bus.NewMessageBus()
	al := agent.NewAgentLoop(cfg, msgBus, &startupBlockedProvider{reason: "not used"})
	t.Cleanup(al.Close)

	_, err := setupCronTool(al, msgBus, cfg.WorkspacePath(), false, time.Minute, cfg)
	if err == nil || !strings.Contains(err.Error(), "CronTool initialization") {
		t.Fatalf("setupCronTool error = %v, want CronTool initialization error", err)
	}
}

func TestSetupAndStartServicesCronStartError(t *testing.T) {
	cfg := config.DefaultConfig()
	workspace := t.TempDir()
	cfg.Agents.Defaults.Workspace = workspace
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = 0
	// A regular file where the cron store directory is expected makes
	// CronService.Start fail after successful CronTool creation.
	if err := os.WriteFile(filepath.Join(workspace, "cron"), []byte("not-a-dir"), 0o600); err != nil {
		t.Fatalf("write cron file: %v", err)
	}
	msgBus := bus.NewMessageBus()
	al := agent.NewAgentLoop(cfg, msgBus, &startupBlockedProvider{reason: "not used"})
	t.Cleanup(al.Close)

	_, err := setupAndStartServices(cfg, al, msgBus, "token",
		netbind.OpenResult{ProbeHost: "127.0.0.1", Port: "0"}, 0)
	if err == nil || !strings.Contains(err.Error(), "error starting cron service") {
		t.Fatalf("setupAndStartServices error = %v, want cron start error", err)
	}
}

func TestGatewayPicoChannelLifecycle(t *testing.T) {
	// Enabling the pico channel in config exercises the "channels enabled"
	// startup branch and the pico channel start/stop path inside runGateway.
	proc := newGatewayTestProc(t, func(cfg map[string]any) {
		cfg["channels"] = map[string]any{
			"pico": map[string]any{"enabled": true, "token": "coverage-token"},
		}
	})
	host, port := proc.waitReady(t)
	if !gatewayHealthOK(host, port) {
		t.Fatal("gateway health not OK with pico channel enabled")
	}
	token, _ := proc.readPID(t)["token"].(string)
	if status, body := gatewayPostReload(t, host, port, token); status != http.StatusOK {
		t.Fatalf("/reload with pico channel status=%d body=%q", status, body)
	}
	if !gatewayHealthOK(host, port) {
		t.Fatal("gateway health lost after reload with pico channel enabled")
	}
	proc.stopAndWait(t, 5*time.Second)
}

func TestShutdownGatewayClosesStatefulProvider(t *testing.T) {
	msgBus := bus.NewMessageBus()
	provider := &statefulStubProvider{}
	al := agent.NewAgentLoop(config.DefaultConfig(), msgBus, provider)
	msgBus.SetEventPublisher(al.RuntimeEventBus())
	shutdownGateway(&services{}, al, provider, msgBus, true)
	if !provider.closed {
		t.Fatal("stateful provider Close() was not called during full shutdown")
	}
}

func TestRestartServicesCronErrorBranches(t *testing.T) {
	// cron start error
	cfg := config.DefaultConfig()
	workspace := t.TempDir()
	cfg.Agents.Defaults.Workspace = workspace
	if err := os.WriteFile(filepath.Join(workspace, "cron"), []byte("not-a-dir"), 0o600); err != nil {
		t.Fatalf("write cron file: %v", err)
	}
	msgBus := bus.NewMessageBus()
	al := agent.NewAgentLoop(cfg, msgBus, &startupBlockedProvider{reason: "not used"})
	t.Cleanup(al.Close)
	err := restartServices(al, &services{}, msgBus)
	if err == nil || !strings.Contains(err.Error(), "error restarting cron service") {
		t.Fatalf("restartServices error = %v, want cron restart error", err)
	}

	// cron tool initialization error
	cfg2 := config.DefaultConfig()
	cfg2.Agents.Defaults.Workspace = t.TempDir()
	cfg2.Tools.Exec.EnableDenyPatterns = true
	cfg2.Tools.Exec.CustomDenyPatterns = []string{"("}
	msgBus2 := bus.NewMessageBus()
	al2 := agent.NewAgentLoop(cfg2, msgBus2, &startupBlockedProvider{reason: "not used"})
	t.Cleanup(al2.Close)
	err = restartServices(al2, &services{}, msgBus2)
	if err == nil || !strings.Contains(err.Error(), "error restarting cron service") {
		t.Fatalf("restartServices error = %v, want cron tool init error", err)
	}
}

func TestCreateHeartbeatHandlerBranches(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "stub"
	t.Setenv("CHAGENT_HOME", t.TempDir())
	msgBus := bus.NewMessageBus()

	// Error branch: provider fails → handler surfaces a tool error and uses the
	// default cli/direct channel when channel/chatID are empty.
	failing := &stubProvider{err: errors.New("stub provider failure")}
	al := agent.NewAgentLoop(cfg, msgBus, failing)
	t.Cleanup(al.Close)
	handler := createHeartbeatHandler(al)
	res := handler("check", "", "")
	if res == nil || !res.IsError || !strings.Contains(res.ForLLM, "Heartbeat error") {
		t.Fatalf("handler result = %#v, want heartbeat error", res)
	}

	// HEARTBEAT_OK branch: a successful provider reply short-circuits to a silent OK.
	msgBus2 := bus.NewMessageBus()
	okProvider := &stubProvider{content: "HEARTBEAT_OK"}
	alOK := agent.NewAgentLoop(cfg, msgBus2, okProvider)
	t.Cleanup(alOK.Close)
	res = createHeartbeatHandler(alOK)("HEARTBEAT_OK", "cli", "direct")
	if res == nil || !res.Silent {
		t.Fatalf("handler result = %#v, want silent heartbeat result", res)
	}
}

// --- in-process runGateway startup/reload error-path tests -----------------

// TestGatewayDebugModeStartsAndStops covers the debug startup branches
// (forced DEBUG log level and the "Debug mode enabled" startup notice).
func TestGatewayDebugModeStartsAndStops(t *testing.T) {
	dir := t.TempDir()
	home, workspace, configPath := writeGatewayTestConfig(t, dir, nil)
	done, sig := startRunGateway(t, true, false, home, configPath)
	host, port, ok := waitGatewayReady(t, workspace, gatewayTestReadyTimeout)
	if !ok {
		select {
		case err := <-done:
			t.Fatalf("debug gateway exited before ready: %v", err)
		default:
			t.Fatal("debug gateway did not become ready")
		}
	}
	if !gatewayHealthOK(host, port) {
		t.Fatal("debug gateway health not OK")
	}
	stopRunGateway(t, done, sig, 5*time.Second)
}

// TestGatewayPanicLogInitFailure covers the early startup failure when the
// panic-log directory cannot be created (home path is a regular file).
func TestGatewayPanicLogInitFailure(t *testing.T) {
	dir := t.TempDir()
	homeFile := filepath.Join(dir, "home-is-a-file")
	if err := os.WriteFile(homeFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("write home file: %v", err)
	}
	_, _, configPath := writeGatewayTestConfig(t, dir, nil)
	sig := make(chan os.Signal, 1)
	err := runGateway(false, homeFile, configPath, false, sig)
	if err == nil || !strings.Contains(err.Error(), "error initializing panic log") {
		t.Fatalf("runGateway error = %v, want panic-log init error", err)
	}
}

// TestGatewayFileLoggingDegradesWhenLogFileUnwritable covers the degraded
// logging path: the gateway must keep running when gateway.log cannot be
// opened (here it is a directory) instead of failing or panicking.
func TestGatewayFileLoggingDegradesWhenLogFileUnwritable(t *testing.T) {
	dir := t.TempDir()
	home, workspace, configPath := writeGatewayTestConfig(t, dir, nil)
	if err := os.MkdirAll(filepath.Join(home, logPath, logFile), 0o755); err != nil {
		t.Fatalf("mkdir log-file-as-dir: %v", err)
	}
	done, sig := startRunGateway(t, false, false, home, configPath)
	host, port, ok := waitGatewayReady(t, workspace, gatewayTestReadyTimeout)
	if !ok {
		select {
		case err := <-done:
			t.Fatalf("gateway exited when file logging failed: %v", err)
		default:
			t.Fatal("gateway did not become ready with degraded logging")
		}
	}
	if !gatewayHealthOK(host, port) {
		t.Fatal("gateway health not OK with degraded logging")
	}
	stopRunGateway(t, done, sig, 5*time.Second)
}

// TestGatewayListenerPortConflict covers the listener-open failure branch.
func TestGatewayListenerPortConflict(t *testing.T) {
	if _, _, err := openGatewayListeners("127.0.0.1,", 0); err == nil {
		t.Fatal("expected BuildPlan error for empty host-list entry")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	occupied := listener.Addr().(*net.TCPAddr).Port

	dir := t.TempDir()
	home, _, configPath := writeGatewayTestConfig(t, dir, func(cfg map[string]any) {
		cfg["gateway"] = map[string]any{"host": "127.0.0.1", "port": occupied, "log_level": "info"}
	})
	sig := make(chan os.Signal, 1)
	err = runGateway(false, home, configPath, false, sig)
	if err == nil || !strings.Contains(err.Error(), "error opening gateway listeners") {
		t.Fatalf("runGateway error = %v, want listener error for occupied port %d", err, occupied)
	}
}

// TestGatewayProviderCreationFailure covers provider creation failure during
// startup (unsupported provider protocol).
func TestGatewayProviderCreationFailure(t *testing.T) {
	dir := t.TempDir()
	home, _, configPath := writeGatewayTestConfig(t, dir, func(cfg map[string]any) {
		cfg["model_list"] = []any{
			map[string]any{"model_name": "fake", "model": "fake", "provider": "bedrock", "api_key": "k"},
		}
	})
	sig := make(chan os.Signal, 1)
	err := runGateway(false, home, configPath, false, sig)
	if err == nil || !strings.Contains(err.Error(), "error creating provider") {
		t.Fatalf("runGateway error = %v, want provider creation error", err)
	}
}

// TestGatewayServiceSetupFailure covers a setupAndStartServices failure during
// startup: the cron store path is a regular file, so CronService.Start fails.
func TestGatewayServiceSetupFailure(t *testing.T) {
	dir := t.TempDir()
	home, workspace, configPath := writeGatewayTestConfig(t, dir, nil)
	if err := os.WriteFile(filepath.Join(workspace, "cron"), []byte("not-a-dir"), 0o600); err != nil {
		t.Fatalf("write cron file: %v", err)
	}
	sig := make(chan os.Signal, 1)
	err := runGateway(false, home, configPath, false, sig)
	if err == nil || !strings.Contains(err.Error(), "error starting cron service") {
		t.Fatalf("runGateway error = %v, want cron service start error", err)
	}
}

// TestGatewayPidWriteFailureClosesListeners covers the pid-file write failure
// path: listeners are opened first, the pid write fails (read-only workspace)
// and the already-opened listeners are closed again before returning.
func TestGatewayPidWriteFailureClosesListeners(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
	dir := t.TempDir()
	home, workspace, configPath := writeGatewayTestConfig(t, dir, nil)
	if err := os.Chmod(workspace, 0o555); err != nil {
		t.Fatalf("chmod workspace: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(workspace, 0o755) })

	sig := make(chan os.Signal, 1)
	err := runGateway(false, home, configPath, false, sig)
	if err == nil || !strings.Contains(err.Error(), "singleton check failed") {
		t.Fatalf("runGateway error = %v, want singleton/pid-write failure", err)
	}
	if !strings.Contains(err.Error(), "failed to write pid file") {
		t.Fatalf("runGateway error = %v, want pid write detail", err)
	}
}

// TestGatewayManualReloadProviderFailure covers manual reload failure after the
// config was accepted: provider creation fails, old services are restarted and
// the gateway keeps serving.
func TestGatewayManualReloadProviderFailure(t *testing.T) {
	proc := newGatewayTestProc(t, nil)
	host, port := proc.waitReady(t)
	token, _ := proc.readPID(t)["token"].(string)

	rewriteGatewayTestLogLevel(t, proc.configPath, "info", func(cfg map[string]any) {
		cfg["model_list"] = []any{
			map[string]any{"model_name": "fake", "model": "fake", "provider": "bedrock", "api_key": "k"},
		}
	})
	status, body := gatewayPostReload(t, host, port, token)
	if status != 200 {
		t.Fatalf("/reload trigger status=%d body=%q", status, body)
	}
	waitLogContains(t, filepath.Join(proc.home, "logs", "gateway.log"), "Manual reload failed", 15*time.Second)
	if !gatewayHealthOK(host, port) {
		t.Fatal("gateway health lost after failed manual reload")
	}
	proc.stopAndWait(t, 5*time.Second)
}

// TestGatewayHotReloadWatcherInvalidConfig covers the polling watcher's config
// load error path.
func TestGatewayHotReloadWatcherInvalidConfig(t *testing.T) {
	proc := newGatewayTestProc(t, func(cfg map[string]any) {
		cfg["gateway"].(map[string]any)["hot_reload"] = true
	})
	proc.waitReady(t)
	valid, err := os.ReadFile(proc.configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(proc.configPath, []byte("{invalid-json"), 0o600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	waitLogContains(t, filepath.Join(proc.home, "logs", "gateway.log"), "Error loading new config", 20*time.Second)
	// Restore a valid config so the watcher stops retrying and shutdown is clean.
	if err := os.WriteFile(proc.configPath, valid, 0o600); err != nil {
		t.Fatalf("restore config: %v", err)
	}
	proc.stopAndWait(t, 5*time.Second)
}

// TestGatewayHotReloadWatcherProviderFailure covers the watcher-triggered
// reload failure branch in the main loop.
func TestGatewayHotReloadWatcherProviderFailure(t *testing.T) {
	proc := newGatewayTestProc(t, func(cfg map[string]any) {
		cfg["gateway"].(map[string]any)["hot_reload"] = true
	})
	proc.waitReady(t)
	rewriteGatewayTestLogLevel(t, proc.configPath, "info", func(cfg map[string]any) {
		cfg["model_list"] = []any{
			map[string]any{"model_name": "fake", "model": "fake", "provider": "bedrock", "api_key": "k"},
		}
	})
	waitLogContains(t, filepath.Join(proc.home, "logs", "gateway.log"), "Config reload failed", 25*time.Second)
	proc.stopAndWait(t, 5*time.Second)
}

// TestGatewayConcurrentReloadCalls covers the reloadTrigger in-progress error
// path (a second /reload while the first is still being processed).
func TestGatewayConcurrentReloadCalls(t *testing.T) {
	proc := newGatewayTestProc(t, nil)
	host, port := proc.waitReady(t)
	token, _ := proc.readPID(t)["token"].(string)

	sawInProgress := false
	for attempt := 0; attempt < 20 && !sawInProgress; attempt++ {
		status1, body1 := gatewayPostReload(t, host, port, token)
		status2, body2 := gatewayPostReload(t, host, port, token)
		if status1 != 200 {
			t.Fatalf("first /reload status=%d body=%q", status1, body1)
		}
		if status2 == 500 && strings.Contains(body2, "reload already in progress") {
			sawInProgress = true
		}
	}
	if !sawInProgress {
		t.Skip("could not observe an overlapping /reload window deterministically")
	}
	if !gatewayHealthOK(host, port) {
		t.Fatal("gateway health lost after concurrent reload calls")
	}
	proc.stopAndWait(t, 5*time.Second)
}

// TestSetupAndStartServicesCronToolError covers setupCronTool returning an
// error inside setupAndStartServices (invalid exec deny pattern).
func TestSetupAndStartServicesCronToolError(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Gateway.Host = "127.0.0.1"
	cfg.Gateway.Port = 0
	cfg.Tools.Exec.EnableDenyPatterns = true
	cfg.Tools.Exec.CustomDenyPatterns = []string{"("}
	msgBus := bus.NewMessageBus()
	al := agent.NewAgentLoop(cfg, msgBus, &startupBlockedProvider{reason: "not used"})
	t.Cleanup(al.Close)

	_, err := setupAndStartServices(cfg, al, msgBus, "token",
		netbind.OpenResult{ProbeHost: "127.0.0.1", Port: "0"}, 0)
	if err == nil || !strings.Contains(err.Error(), "error setting up cron service") {
		t.Fatalf("setupAndStartServices error = %v, want cron setup error", err)
	}
}

// TestGatewayHotReloadWatcherDebugLog covers the watcher's debug-only
// "config file change detected" branch.
func TestGatewayHotReloadWatcherDebugLog(t *testing.T) {
	dir := t.TempDir()
	home, workspace, configPath := writeGatewayTestConfig(t, dir, func(cfg map[string]any) {
		cfg["gateway"].(map[string]any)["hot_reload"] = true
	})
	done, sig := startRunGateway(t, true, false, home, configPath)
	if _, _, ok := waitGatewayReady(t, workspace, gatewayTestReadyTimeout); !ok {
		select {
		case err := <-done:
			t.Fatalf("debug hot-reload gateway exited: %v", err)
		default:
			t.Fatal("debug hot-reload gateway did not become ready")
		}
	}
	rewriteGatewayTestLogLevel(t, configPath, "debug", nil)
	waitLogContains(t, filepath.Join(home, "logs", "gateway.log"), "Config file change detected", 20*time.Second)
	stopRunGateway(t, done, sig, 5*time.Second)
}

// TestCreateHeartbeatHandlerNonOKResponse covers the non-HEARTBEAT_OK success
// branch (the response is forwarded as a silent tool result).
func TestCreateHeartbeatHandlerNonOKResponse(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "stub"
	t.Setenv("CHAGENT_HOME", t.TempDir())
	msgBus := bus.NewMessageBus()
	al := agent.NewAgentLoop(cfg, msgBus, &stubProvider{content: "TASK_PROGRESS_NOTE"})
	t.Cleanup(al.Close)
	res := createHeartbeatHandler(al)("check", "cli", "direct")
	if res == nil || !res.Silent || !strings.Contains(res.ForLLM, "TASK_PROGRESS_NOTE") {
		t.Fatalf("handler result = %#v, want silent forwarded response", res)
	}
}
