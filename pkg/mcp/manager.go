package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rlds/chagent/pkg/config"
	runtimeevents "github.com/rlds/chagent/pkg/events"
	"github.com/rlds/chagent/pkg/logger"
)

// headerTransport 是一个 http.RoundTripper，用于给请求添加自定义 header
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func expandHomeCommandPath(command string) string {
	if command == "" || command[0] != '~' {
		return command
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return command
	}
	if command == "~" {
		return home
	}
	if strings.HasPrefix(command, "~/") || strings.HasPrefix(command, "~\\") {
		return filepath.Join(home, command[2:])
	}
	return command
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 克隆请求，避免修改原始请求
	req = req.Clone(req.Context())

	// 添加自定义 header
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}

	// MCP SSE transport 的 POST 请求需要 Accept header
	// 参见：https://modelcontextprotocol.io/specification/2024-11-05/basic/transports
	if req.Method == "POST" {
		req.Header.Set("Accept", "application/json, text/event-stream")
	}

	// 使用基础 transport
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// loadEnvFile 从 .env 格式的文件中加载环境变量
// 每行格式应为：KEY=value
// 以 # 开头的行是注释
// 空行会被忽略
func loadEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open env file: %w", err)
	}
	defer file.Close()

	envVars := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		// 跳过空行和注释
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 解析 KEY=value
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid format at line %d: %s", lineNum, line)
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if key == "" {
			return nil, fmt.Errorf("invalid format at line %d: empty key", lineNum)
		}

		// 如果值被引号包裹，则去掉外层引号
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		envVars[key] = value
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading env file: %w", err)
	}

	return envVars, nil
}

// ServerConnection 表示与一个 MCP server 的连接
type ServerConnection struct {
	Name        string
	Config      config.MCPServerConfig
	Client      *mcp.Client
	Session     *mcp.ClientSession
	Tools       []*mcp.Tool
	reconnectMu sync.Mutex
}

// Manager 管理多个 MCP server 连接
type Manager struct {
	servers       map[string]*ServerConnection
	runtimeEvents runtimeevents.Bus
	mu            sync.RWMutex
	closed        atomic.Bool       // 由 bool 改为 atomic.Bool，以避免 TOCTOU 竞态
	wg            sync.WaitGroup    // 跟踪进行中的 CallTool 调用
	mcpConfig     *config.MCPConfig // 保存 MCP 配置，供 keepalive 设置使用
	// toolListChanged 在服务器的 tools/list_changed 通知处理完毕、
	// 且缓存的工具列表已刷新之后被调用。
	toolListChanged func(serverName string, tools []*mcp.Tool)
}

// 内置（vendored）的 Go SDK fork（v1.6.1）所支持的 MCP 协议版本。客户端在
// initialize 握手时请求 LatestMCPProtocolVersion；服务器可以协商降到本列表中的
// 任意旧版本，连接仍可正常使用。
//
// 注意：这些值与 SDK 中未导出的 protocolVersion* 常量（及其
// latestProtocolVersion）保持一致；在此重新声明是为了便于审计和打日志。
const (
	MCPProtocolVersion20241105 = "2024-11-05"
	MCPProtocolVersion20250326 = "2025-03-26"
	MCPProtocolVersion20250618 = "2025-06-18"
	MCPProtocolVersion20251125 = "2025-11-25"
	MCPProtocolVersion20260630 = "2026-06-30"

	// LatestMCPProtocolVersion 是客户端在 initialize 握手中请求的版本
	// （即 SDK 支持的最新版本）。本地 go-sdk fork 已原生支持 2026-06-30
	// （参见 third_party/mcp-go-sdk/PATCH.md）；服务器仍可协商降级。
	LatestMCPProtocolVersion = MCPProtocolVersion20260630
)

// MCPProtocolVersions 列出本客户端可协商的所有协议版本
// （最新的在前），供文档和可观测性使用。
var MCPProtocolVersions = []string{
	MCPProtocolVersion20260630,
	MCPProtocolVersion20251125,
	MCPProtocolVersion20250618,
	MCPProtocolVersion20250326,
	MCPProtocolVersion20241105,
}

var connectServerFunc = connectServer

// ManagerOption 用于配置 MCP manager。
type ManagerOption func(*Manager)

// WithRuntimeEvents 注入用于 MCP 观测的运行时事件总线。
func WithRuntimeEvents(eventBus runtimeevents.Bus) ManagerOption {
	return func(m *Manager) {
		m.runtimeEvents = eventBus
	}
}

// WithToolListChangedHandler 注册一个回调：当 manager 响应
// notifications/tools/list_changed 通知、刷新了服务器的缓存工具列表之后会调用它。
// 该回调在 manager 锁之外执行；agent 层可用它重新注册 MCP 工具包装器。
func WithToolListChangedHandler(handler func(serverName string, tools []*mcp.Tool)) ManagerOption {
	return func(m *Manager) {
		m.mu.Lock()
		m.toolListChanged = handler
		m.mu.Unlock()
	}
}

// SetToolListChangedHandler 在构造完成之后更新工具列表变更回调
// （agent 层文档化的接入点）。
func (m *Manager) SetToolListChangedHandler(handler func(serverName string, tools []*mcp.Tool)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.toolListChanged = handler
	m.mu.Unlock()
}

// ServerEventPayload 描述 MCP server 连接事件。
type ServerEventPayload struct {
	Server    string `json:"server"`
	Type      string `json:"type,omitempty"`
	URL       string `json:"url,omitempty"`
	Command   string `json:"command,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolCount int    `json:"tool_count,omitempty"`
	Error     string `json:"error,omitempty"`
}

// NewManager 创建一个新的 MCP manager
func NewManager(opts ...ManagerOption) *Manager {
	m := &Manager{
		servers: make(map[string]*ServerConnection),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// LoadFromConfig 从配置中加载 MCP server
func (m *Manager) LoadFromConfig(ctx context.Context, cfg *config.Config) error {
	return m.LoadFromMCPConfig(ctx, cfg.Tools.MCP, cfg.WorkspacePath())
}

// LoadFromMCPConfig 从 MCP 配置和工作区路径加载 MCP server。
// 这是依赖最小的版本，不需要完整的 Config 对象。
func (m *Manager) LoadFromMCPConfig(
	ctx context.Context,
	mcpCfg config.MCPConfig,
	workspacePath string,
) error {
	// 保存 MCP 配置，供 keepalive 设置使用
	m.mu.Lock()
	m.mcpConfig = &mcpCfg
	m.mu.Unlock()

	// 只要配置了 server 就足以启用 MCP（参见 config.MCPConfig.Active）：
	// 旧的 mcp.enabled 开关是可选的，因此仅配置了 server 的配置不能在这里
	// 被当作已禁用而提前返回。
	if !mcpCfg.Active() {
		logger.InfoCF("mcp", "MCP integration is disabled", nil)
		return nil
	}

	if len(mcpCfg.Servers) == 0 {
		logger.InfoCF("mcp", "No MCP servers configured", nil)
		return nil
	}

	logger.InfoCF("mcp", "Initializing MCP servers",
		map[string]any{
			"count": len(mcpCfg.Servers),
		})

	var wg sync.WaitGroup
	errs := make(chan error, len(mcpCfg.Servers))
	enabledCount := 0

	for name, serverCfg := range mcpCfg.Servers {
		if !serverCfg.Enabled {
			logger.DebugCF("mcp", "Skipping disabled server",
				map[string]any{
					"server": name,
				})
			continue
		}

		enabledCount++
		wg.Add(1)
		go func(name string, serverCfg config.MCPServerConfig, workspace string) {
			defer wg.Done()

			// 将相对的 envFile 路径解析为相对于工作区的路径
			if serverCfg.EnvFile != "" && !filepath.IsAbs(serverCfg.EnvFile) {
				if workspace == "" {
					err := fmt.Errorf(
						"workspace path is empty while resolving relative envFile %q for server %s",
						serverCfg.EnvFile,
						name,
					)
					logger.ErrorCF("mcp", "Invalid MCP server configuration",
						map[string]any{
							"server":   name,
							"env_file": serverCfg.EnvFile,
							"error":    err.Error(),
						})
					errs <- err
					return
				}
				serverCfg.EnvFile = filepath.Join(workspace, serverCfg.EnvFile)
			}

			if err := m.ConnectServer(ctx, name, serverCfg); err != nil {
				logger.ErrorCF("mcp", "Failed to connect to MCP server",
					map[string]any{
						"server": name,
						"error":  err.Error(),
					})
				errs <- fmt.Errorf("failed to connect to server %s: %w", name, err)
			}
		}(name, serverCfg, workspacePath)
	}

	wg.Wait()
	close(errs)

	// 收集错误
	var allErrors []error
	for err := range errs {
		allErrors = append(allErrors, err)
	}

	connectedCount := len(m.GetServers())

	// 如果所有已启用的 server 都连接失败，则返回聚合错误
	if enabledCount > 0 && connectedCount == 0 {
		logger.ErrorCF("mcp", "All MCP servers failed to connect",
			map[string]any{
				"failed": len(allErrors),
				"total":  enabledCount,
			})
		return errors.Join(allErrors...)
	}

	if len(allErrors) > 0 {
		logger.WarnCF("mcp", "Some MCP servers failed to connect",
			map[string]any{
				"failed":    len(allErrors),
				"connected": connectedCount,
				"total":     enabledCount,
			})
		// 如果部分 server 连接成功，就不要整体失败
	}

	logger.InfoCF("mcp", "MCP server initialization complete",
		map[string]any{
			"connected": connectedCount,
			"total":     enabledCount,
		})

	return nil
}

// ConnectServer 连接单个 MCP server
func (m *Manager) ConnectServer(
	ctx context.Context,
	name string,
	cfg config.MCPServerConfig,
) error {
	// 从配置中获取 keepalive 间隔
	keepaliveInterval := 0
	if m.mcpConfig != nil {
		keepaliveInterval = m.mcpConfig.GetKeepAliveInterval()
	}

	m.publishServerEvent(runtimeevents.KindMCPServerConnecting, name, cfg, 0, nil)
	conn, err := connectServerFunc(ctx, name, cfg, keepaliveInterval, m.handleToolListChanged)
	if err != nil {
		m.publishServerEvent(runtimeevents.KindMCPServerFailed, name, cfg, 0, err)
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed.Load() {
		_ = conn.Session.Close()
		m.publishServerEvent(runtimeevents.KindMCPServerFailed, name, cfg, 0, fmt.Errorf("manager is closed"))
		return fmt.Errorf("manager is closed")
	}

	m.servers[name] = conn
	for _, tool := range conn.Tools {
		toolName := ""
		if tool != nil {
			toolName = tool.Name
		}
		m.publishToolDiscovered(name, cfg, toolName)
	}
	m.publishServerEvent(runtimeevents.KindMCPServerConnected, name, cfg, len(conn.Tools), nil)
	return nil
}

// newMCPClient 构建用于单个 server 连接的 SDK 客户端。
//
// 客户端在 initialize 时请求 SDK 的最新协议版本
// （go-sdk v1.6.1 下 LatestMCPProtocolVersion = 2025-11-25）；服务器可以
// 协商降到 MCPProtocolVersions 中的任意旧版本。
//
// 能力声明沿用 SDK 默认值，即声明 roots 且 listChanged=true。未设置
// sampling 或 elicitation 处理器，因此不会声明这些能力（chagent 没有
// elicitation 表单所需的交互 UI，也不会向 MCP server 暴露 sampling）。
func newMCPClient(
	keepaliveInterval int,
	serverName string,
	onToolListChanged func(serverName string, tools []*mcp.Tool),
) (*mcp.Client, *atomic.Pointer[mcp.ClientSession]) {
	var clientOpts *mcp.ClientOptions
	if keepaliveInterval > 0 {
		clientOpts = &mcp.ClientOptions{
			KeepAlive: time.Duration(keepaliveInterval) * time.Second,
		}
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "chagent",
		Version: "1.0.0",
	}, clientOpts)
	if onToolListChanged == nil {
		return client, nil
	}

	// 异步刷新：通知处理器运行在 session 的读取循环上，如果在这里同步发起
	// tools/list，可能会在同一个循环上阻塞消息处理（以及响应）。
	sessionPtr := &atomic.Pointer[mcp.ClientSession]{}
	clientOpts.ToolListChangedHandler = func(_ context.Context, _ *mcp.ToolListChangedRequest) {
		go refreshToolsAfterListChanged(serverName, sessionPtr, onToolListChanged)
	}
	return mcp.NewClient(&mcp.Implementation{
		Name:    "chagent",
		Version: "1.0.0",
	}, clientOpts), sessionPtr
}

// mcpToolListRefreshTimeout 限定由一条 notifications/tools/list_changed
// 消息触发的一次 tools/list 刷新的最长耗时。
const mcpToolListRefreshTimeout = 15 * time.Second

// refreshToolsAfterListChanged 重新列出某个 server 的工具，并把最新的
// 列表传给 manager 的回调。
func refreshToolsAfterListChanged(
	serverName string,
	sessionPtr *atomic.Pointer[mcp.ClientSession],
	callback func(serverName string, tools []*mcp.Tool),
) {
	session := sessionPtr.Load()
	if session == nil {
		return
	}
	initResult := session.InitializeResult()
	if initResult == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), mcpToolListRefreshTimeout)
	defer cancel()
	tools, err := listServerTools(ctx, serverName, session, initResult)
	if err != nil {
		logger.WarnCF("mcp", "Failed to refresh MCP tool list after list_changed notification",
			map[string]any{"server": serverName, "error": err.Error()})
		return
	}
	callback(serverName, tools)
}

// buildTransport 为单个 server 配置构建客户端 transport。
// 测试可以覆盖 buildTransportFunc，替换为内存 transport。
func buildTransport(ctx context.Context, name string, cfg config.MCPServerConfig) (mcp.Transport, error) {
	var transport mcp.Transport
	transportType := config.EffectiveMCPTransportType(cfg)
	if transportType == "" {
		return nil, fmt.Errorf("either URL or command must be provided")
	}

	switch transportType {
	case "sse":
		// 旧版 HTTP+SSE transport：仅属于 2024-11-05 规范。后续协议版本
		// （2025-03-26 起）改用 Streamable HTTP；这里保留用于兼容旧 server。
		// GET /sse 获取端点，然后 POST 到消息端点
		if cfg.URL == "" {
			return nil, fmt.Errorf("URL is required for SSE transport")
		}

		logger.DebugCF("mcp", "Using legacy SSE transport (2024-11-05 only)",
			map[string]any{
				"server": name,
				"url":    cfg.URL,
			})

		sseTransport := &mcp.SSEClientTransport{
			Endpoint: cfg.URL,
		}

		// 如提供了自定义 header，则添加
		if len(cfg.Headers) > 0 {
			// 创建一个带 header 注入 transport 的自定义 HTTP 客户端
			sseTransport.HTTPClient = &http.Client{
				Transport: &headerTransport{
					base:    http.DefaultTransport,
					headers: cfg.Headers,
				},
			}
			logger.DebugCF("mcp", "Added custom HTTP headers",
				map[string]any{
					"server":       name,
					"header_count": len(cfg.Headers),
				})
		}

		transport = sseTransport

	case "http":
		// Streamable HTTP transport（2025-03-26 引入，适用于 2025-03-26 /
		// 2025-06-18 / 2025-11-25）。直接 POST 到端点。
		if cfg.URL == "" {
			return nil, fmt.Errorf("URL is required for HTTP transport")
		}

		logger.DebugCF("mcp", "Using streamable HTTP transport (2025-03-26+)",
			map[string]any{
				"server": name,
				"url":    cfg.URL,
			})

		httpTransport := &mcp.StreamableClientTransport{
			Endpoint:             cfg.URL,
			DisableStandaloneSSE: true,
		}

		// 如提供了自定义 header，则添加
		if len(cfg.Headers) > 0 {
			// 创建一个带 header 注入 transport 的自定义 HTTP 客户端
			httpTransport.HTTPClient = &http.Client{
				Transport: &headerTransport{
					base:    http.DefaultTransport,
					headers: cfg.Headers,
				},
			}
			logger.DebugCF("mcp", "Added custom HTTP headers",
				map[string]any{
					"server":       name,
					"header_count": len(cfg.Headers),
				})
		}

		transport = httpTransport
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("command is required for stdio transport")
		}
		logger.DebugCF("mcp", "Using stdio transport",
			map[string]any{
				"server":  name,
				"command": cfg.Command,
			})
		// 把子进程的生命周期绑定到 MCP session，而不是触发本次连接的
		// 请求 context：reload/turn 的 context 会在请求/轮次结束时被取消，
		// 否则会杀掉一个仍在运行的 stdio MCP server
		// （参见 isolatedCommandTransport.Cancel / Close）。
		procCtx, procCancel := context.WithCancel(context.WithoutCancel(ctx))
		cmd := exec.CommandContext(procCtx, expandHomeCommandPath(cfg.Command), cfg.Args...)

		// 按正确的覆盖语义构建环境变量
		// 使用 map 确保配置中的变量覆盖文件中的变量
		envMap := make(map[string]string)

		// 先以父进程的环境变量为基础
		for _, e := range cmd.Environ() {
			if idx := strings.Index(e, "="); idx > 0 {
				envMap[e[:idx]] = e[idx+1:]
			}
		}

		// 如指定了 envFile，则从该文件加载环境变量
		if cfg.EnvFile != "" {
			envVars, err := loadEnvFile(cfg.EnvFile)
			if err != nil {
				procCancel()
				return nil, fmt.Errorf("failed to load env file %s: %w", cfg.EnvFile, err)
			}
			for k, v := range envVars {
				envMap[k] = v
			}
			logger.DebugCF("mcp", "Loaded environment variables from file",
				map[string]any{
					"server":    name,
					"envFile":   cfg.EnvFile,
					"var_count": len(envVars),
				})
		}

		// 配置中的环境变量覆盖文件中的同名变量
		for k, v := range cfg.Env {
			envMap[k] = v
		}

		// 把 map 转换为切片
		env := make([]string, 0, len(envMap))
		for k, v := range envMap {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
		transport = &isolatedCommandTransport{Command: cmd, Cancel: procCancel}
	default:
		return nil, fmt.Errorf(
			"unsupported transport type: %s (supported: stdio, sse, http, streamable-http)",
			transportType,
		)
	}
	if transport == nil {
		return nil, fmt.Errorf("unsupported transport type: %s", transportType)
	}
	return transport, nil
}

var buildTransportFunc = buildTransport

func connectServer(
	ctx context.Context,
	name string,
	cfg config.MCPServerConfig,
	keepaliveInterval int,
	onToolListChanged func(serverName string, tools []*mcp.Tool),
) (*ServerConnection, error) {
	logger.InfoCF("mcp", "Connecting to MCP server",
		map[string]any{
			"server":     name,
			"command":    cfg.Command,
			"args_count": len(cfg.Args),
		})

	if keepaliveInterval > 0 {
		logger.DebugCF("mcp", "Enable keepalive for MCP client",
			map[string]any{
				"server":             name,
				"keepalive_interval": keepaliveInterval,
			})
	}

	// 创建客户端（initialize 时请求最新的 MCP 协议版本）
	client, toolListSession := newMCPClient(keepaliveInterval, name, onToolListChanged)

	transport, err := buildTransportFunc(ctx, name, cfg)
	if err != nil {
		return nil, err
	}

	// 连接 server
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	if toolListSession != nil {
		toolListSession.Store(session)
	}

	// 获取 server 信息
	initResult := session.InitializeResult()
	logger.InfoCF("mcp", "Connected to MCP server",
		map[string]any{
			"server":        name,
			"serverName":    initResult.ServerInfo.Name,
			"serverVersion": initResult.ServerInfo.Version,
			"protocol":      initResult.ProtocolVersion,
			"transport":     config.EffectiveMCPTransportType(cfg),
		})

	// 如果支持，则列出可用的工具
	tools, err := listServerTools(ctx, name, session, initResult)
	if err != nil {
		_ = session.Close()
		return nil, err
	}

	return &ServerConnection{
		Name:    name,
		Config:  cfg,
		Client:  client,
		Session: session,
		Tools:   tools,
	}, nil
}

// GetServers 返回所有已连接的 server
func (m *Manager) GetServers() map[string]*ServerConnection {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*ServerConnection, len(m.servers))
	for k, v := range m.servers {
		result[k] = v
	}
	return result
}

// GetServer 返回指定的 server 连接
func (m *Manager) GetServer(name string) (*ServerConnection, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	conn, ok := m.servers[name]
	return conn, ok
}

// CallTool 在指定的 server 上调用工具
func (m *Manager) CallTool(
	ctx context.Context,
	serverName, toolName string,
	arguments map[string]any,
) (*mcp.CallToolResult, error) {
	// 加锁前先检查是否已关闭（快速路径）
	if m.closed.Load() {
		return nil, fmt.Errorf("manager is closed")
	}

	m.mu.RLock()
	// 加锁后再检查一次，防止 TOCTOU 竞态
	if m.closed.Load() {
		m.mu.RUnlock()
		return nil, fmt.Errorf("manager is closed")
	}
	conn, ok := m.servers[serverName]
	if ok {
		m.wg.Add(1) // 持锁期间加入 WaitGroup
	}
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	defer m.wg.Done()

	params := &mcp.CallToolParams{
		Name:      toolName,
		Arguments: arguments,
	}

	result, err := conn.Session.CallTool(ctx, params)
	if err != nil {
		if shouldReconnectCallError(err) {
			logger.WarnCF("mcp", "MCP server session was lost during tool call, reconnecting",
				map[string]any{
					"server": serverName,
					"tool":   toolName,
					"error":  err.Error(),
				})

			reconnectedConn, reconnectErr := m.reconnectServer(ctx, serverName, conn)
			if reconnectErr != nil {
				return nil, fmt.Errorf("failed to recover lost MCP session: %w", reconnectErr)
			}

			result, err = reconnectedConn.Session.CallTool(ctx, params)
			if err == nil {
				return result, nil
			}
		}

		return nil, fmt.Errorf("failed to call tool: %w", err)
	}

	return result, nil
}

// handleToolListChanged 更新某个 server 缓存的工具列表，并通知可选的
// agent 层回调。
func (m *Manager) handleToolListChanged(serverName string, tools []*mcp.Tool) {
	if m == nil {
		return
	}

	m.mu.Lock()
	conn, ok := m.servers[serverName]
	if !ok {
		m.mu.Unlock()
		return
	}
	cfg := conn.Config
	conn.Tools = tools
	callback := m.toolListChanged
	m.mu.Unlock()

	for _, tool := range tools {
		if tool != nil {
			m.publishToolDiscovered(serverName, cfg, tool.Name)
		}
	}
	logger.InfoCF("mcp", "Refreshed MCP tool list after tools/list_changed notification",
		map[string]any{
			"server":     serverName,
			"tool_count": len(tools),
		})
	if callback != nil {
		callback(serverName, tools)
	}
}

func listServerTools(
	ctx context.Context,
	name string,
	session *mcp.ClientSession,
	initResult *mcp.InitializeResult,
) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	if initResult.Capabilities.Tools == nil {
		return tools, nil
	}

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			logger.WarnCF("mcp", "Error listing tool",
				map[string]any{
					"server": name,
					"error":  err.Error(),
				})
			continue
		}
		tools = append(tools, tool)
	}

	logger.InfoCF("mcp", "Listed tools from MCP server",
		map[string]any{
			"server":    name,
			"toolCount": len(tools),
		})

	return tools, nil
}

func shouldReconnectCallError(err error) bool {
	if err == nil {
		return false
	}

	// 检查已知的 session 错误
	if errors.Is(err, mcp.ErrSessionMissing) {
		return true
	}

	errMsg := strings.ToLower(err.Error())

	// 检查 session 丢失错误
	if strings.Contains(errMsg, mcp.ErrSessionMissing.Error()) {
		return true
	}

	// 检查连接已关闭错误
	if strings.Contains(errMsg, "connection closed") {
		return true
	}

	// 检查 EOF 错误（对端重置连接）
	if strings.Contains(errMsg, "eof") {
		return true
	}

	// 检查其他与连接相关的错误
	connectionErrors := []string{
		"connection reset",
		"broken pipe",
		"network connection",
		"connection refused",
		"no such connection",
	}

	for _, connErr := range connectionErrors {
		if strings.Contains(errMsg, connErr) {
			return true
		}
	}

	return false
}

func (m *Manager) reconnectServer(
	ctx context.Context,
	serverName string,
	staleConn *ServerConnection,
) (*ServerConnection, error) {
	if staleConn == nil {
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	staleConn.reconnectMu.Lock()
	defer staleConn.reconnectMu.Unlock()

	if m.closed.Load() {
		return nil, fmt.Errorf("manager is closed")
	}

	m.mu.RLock()
	currentConn, ok := m.servers[serverName]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("server %s not found", serverName)
	}
	if currentConn != staleConn {
		return currentConn, nil
	}

	// 从配置中获取 keepalive 间隔
	keepaliveInterval := 0
	if m.mcpConfig != nil {
		keepaliveInterval = m.mcpConfig.GetKeepAliveInterval()
	}

	freshConn, err := connectServerFunc(ctx, serverName, staleConn.Config, keepaliveInterval, m.handleToolListChanged)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		_ = freshConn.Session.Close()
		return nil, fmt.Errorf("manager is closed")
	}

	currentConn, ok = m.servers[serverName]
	if !ok {
		m.mu.Unlock()
		_ = freshConn.Session.Close()
		return nil, fmt.Errorf("server %s not found", serverName)
	}

	if currentConn == staleConn {
		m.servers[serverName] = freshConn
		staleToClose := staleConn
		m.mu.Unlock()
		_ = staleToClose.Session.Close()
		return freshConn, nil
	}

	m.mu.Unlock()
	_ = freshConn.Session.Close()
	return currentConn, nil
}

// Close 关闭所有 server 连接
func (m *Manager) Close() error {
	// 用 Swap 原子地设置 closed=true 并取出旧值
	// 这样可避免与 CallTool 的 closed 检查产生 TOCTOU 竞态
	if m.closed.Swap(true) {
		return nil // 已经关闭
	}

	// 关闭 session 之前先等待所有进行中的 CallTool 调用结束
	// closed=true 设置之后，新的 CallTool 无法开始（它们会先检查 closed）
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()

	logger.InfoCF("mcp", "Closing all MCP server connections",
		map[string]any{
			"count": len(m.servers),
		})

	var errs []error
	for name, conn := range m.servers {
		if err := conn.Session.Close(); err != nil {
			logger.ErrorCF("mcp", "Failed to close server connection",
				map[string]any{
					"server": name,
					"error":  err.Error(),
				})
			errs = append(errs, fmt.Errorf("server %s: %w", name, err))
		}
	}

	m.servers = make(map[string]*ServerConnection)

	if len(errs) > 0 {
		return fmt.Errorf("failed to close %d server(s): %w", len(errs), errors.Join(errs...))
	}

	return nil
}

// GetAllTools 返回所有已连接 server 的全部工具
func (m *Manager) GetAllTools() map[string][]*mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string][]*mcp.Tool)
	for name, conn := range m.servers {
		if len(conn.Tools) > 0 {
			result[name] = conn.Tools
		}
	}
	return result
}
