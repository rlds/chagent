// Chagent - 超轻量级个人 AI agent
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package heartbeat

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/constants"
	"github.com/rlds/chagent/pkg/fileutil"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/state"
	"github.com/rlds/chagent/pkg/tools"
)

const (
	minIntervalMinutes     = 5
	defaultIntervalMinutes = 30
	userTasksMarker        = "Add your heartbeat tasks below this line:"
)

// HeartbeatHandler 是用于处理心跳的函数类型。
// 它返回一个 ToolResult，可用于表示异步操作。
// channel 与 chatID 均取自最近活跃的用户通道。
type HeartbeatHandler func(prompt, channel, chatID string) *tools.ToolResult

// HeartbeatService 管理周期性心跳检查
type HeartbeatService struct {
	workspace string
	bus       *bus.MessageBus
	state     *state.Manager
	handler   HeartbeatHandler
	interval  time.Duration
	enabled   bool
	mu        sync.RWMutex
	stopChan  chan struct{}
}

// NewHeartbeatService 创建一个新的心跳服务
func NewHeartbeatService(workspace string, intervalMinutes int, enabled bool) *HeartbeatService {
	// 应用最小间隔限制
	if intervalMinutes < minIntervalMinutes && intervalMinutes != 0 {
		intervalMinutes = minIntervalMinutes
	}

	if intervalMinutes == 0 {
		intervalMinutes = defaultIntervalMinutes
	}

	return &HeartbeatService{
		workspace: workspace,
		interval:  time.Duration(intervalMinutes) * time.Minute,
		enabled:   enabled,
		state:     state.NewManager(workspace),
	}
}

// SetBus 设置用于投递心跳结果的消息总线。
func (hs *HeartbeatService) SetBus(msgBus *bus.MessageBus) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.bus = msgBus
}

// SetHandler 设置心跳处理器。
func (hs *HeartbeatService) SetHandler(handler HeartbeatHandler) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.handler = handler
}

// Start 启动心跳服务
func (hs *HeartbeatService) Start() error {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	if hs.stopChan != nil {
		logger.InfoC("heartbeat", "Heartbeat service already running")
		return nil
	}

	if !hs.enabled {
		logger.InfoC("heartbeat", "Heartbeat service disabled")
		return nil
	}

	hs.stopChan = make(chan struct{})
	go hs.runLoop(hs.stopChan)

	logger.InfoCF("heartbeat", "Heartbeat service started", map[string]any{
		"interval_minutes": hs.interval.Minutes(),
	})

	return nil
}

// Stop 优雅地停止心跳服务
func (hs *HeartbeatService) Stop() {
	hs.mu.Lock()
	defer hs.mu.Unlock()

	if hs.stopChan == nil {
		return
	}

	logger.InfoC("heartbeat", "Stopping heartbeat service")
	close(hs.stopChan)
	hs.stopChan = nil
}

// IsRunning 返回服务是否正在运行
func (hs *HeartbeatService) IsRunning() bool {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	return hs.stopChan != nil
}

// runLoop 运行心跳定时器
func (hs *HeartbeatService) runLoop(stopChan chan struct{}) {
	ticker := time.NewTicker(hs.interval)
	defer ticker.Stop()

	// 在初始延迟后执行第一次心跳
	time.AfterFunc(time.Second, func() {
		hs.executeHeartbeat()
	})

	for {
		select {
		case <-stopChan:
			return
		case <-ticker.C:
			hs.executeHeartbeat()
		}
	}
}

// executeHeartbeat 执行一次心跳检查
func (hs *HeartbeatService) executeHeartbeat() {
	hs.mu.RLock()
	enabled := hs.enabled
	handler := hs.handler
	if !hs.enabled || hs.stopChan == nil {
		hs.mu.RUnlock()
		return
	}
	hs.mu.RUnlock()

	if !enabled {
		return
	}

	logger.DebugC("heartbeat", "Executing heartbeat")

	prompt := hs.buildPrompt()
	if prompt == "" {
		logger.InfoC("heartbeat", "No heartbeat prompt (HEARTBEAT.md empty or missing)")
		return
	}

	if handler == nil {
		hs.logErrorf("Heartbeat handler not configured")
		return
	}

	// 获取最近通道信息作为上下文
	lastChannel := hs.state.GetLastChannel()
	channel, chatID := hs.parseLastChannel(lastChannel)

	// 通道解析的调试日志
	hs.logInfof("Resolved channel: %s, chatID: %s (from lastChannel: %s)", channel, chatID, lastChannel)

	result := handler(prompt, channel, chatID)

	if result == nil {
		hs.logInfof("Heartbeat handler returned nil result")
		return
	}

	// 处理不同的结果类型
	if result.IsError {
		hs.logErrorf("Heartbeat error: %s", result.ForLLM)
		return
	}

	if result.Async {
		hs.logInfof("Async task started: %s", result.ForLLM)
		logger.InfoCF("heartbeat", "Async heartbeat task started",
			map[string]any{
				"message": result.ForLLM,
			})
		return
	}

	// 检查是否为静默
	if result.Silent {
		hs.logInfof("Heartbeat OK - silent")
		return
	}

	// 将结果发送给用户
	if result.ForUser != "" {
		hs.sendResponse(result.ForUser)
	} else if result.ForLLM != "" {
		hs.sendResponse(result.ForLLM)
	}

	hs.logInfof("Heartbeat completed: %s", result.ForLLM)
}

// buildPrompt 根据 HEARTBEAT.md 构建心跳提示词
func (hs *HeartbeatService) buildPrompt() string {
	heartbeatPath := filepath.Join(hs.workspace, "HEARTBEAT.md")

	data, err := os.ReadFile(heartbeatPath)
	if err != nil {
		if os.IsNotExist(err) {
			hs.createDefaultHeartbeatTemplate()
			return ""
		}
		hs.logErrorf("Error reading HEARTBEAT.md: %v", err)
		return ""
	}

	content := string(data)
	if !heartbeatHasUserTasks(content) {
		return ""
	}

	now := time.Now().Format("2006-01-02 15:04:05")
	return fmt.Sprintf(`# 心跳检查

当前时间：%s

定时心跳检查：请使用可用工具/技能执行以下任务，直到全部处理完成。如果没有什么需要关注，请仅回复：HEARTBEAT_OK

%s
`, now, content)
}

// createDefaultHeartbeatTemplate 创建默认的 HEARTBEAT.md 文件
func (hs *HeartbeatService) createDefaultHeartbeatTemplate() {
	heartbeatPath := filepath.Join(hs.workspace, "HEARTBEAT.md")

	defaultContent := `# 心跳检查清单

心跳服务需要定期检查的任务。

## 说明

- 执行下面所有任务，不要跳过任何一项。
- 简单任务：直接回复。长任务：使用 spawn 工具（异步执行，结果会自动发送给用户），然后继续处理剩余任务。
- 仅当所有任务都完成且没有任何需要关注的事项时，才回复：HEARTBEAT_OK

## 示例

- 检查未读消息
- 查看日历上的近期日程
- 检查设备状态（如 MaixCam）

---

Add your heartbeat tasks below this line:
`

	if err := fileutil.WriteFileAtomic(heartbeatPath, []byte(defaultContent), 0o644); err != nil {
		hs.logErrorf("Failed to create default HEARTBEAT.md: %v", err)
	} else {
		hs.logInfof("Created default HEARTBEAT.md template")
	}
}

func heartbeatHasUserTasks(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}

	markerIdx := strings.Index(content, userTasksMarker)
	if markerIdx < 0 {
		return true
	}

	tasksSection := content[markerIdx+len(userTasksMarker):]
	for _, line := range strings.Split(tasksSection, "\n") {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" {
			continue
		}
		if strings.HasPrefix(trimmedLine, "#") {
			continue
		}
		return true
	}

	return false
}

// sendResponse 将心跳响应发送到最近的通道
func (hs *HeartbeatService) sendResponse(response string) {
	hs.mu.RLock()
	msgBus := hs.bus
	hs.mu.RUnlock()

	if msgBus == nil {
		hs.logInfof("No message bus configured, heartbeat result not sent")
		return
	}

	// 从状态中获取最近的通道
	lastChannel := hs.state.GetLastChannel()
	if lastChannel == "" {
		hs.logInfof("No last channel recorded, heartbeat result not sent")
		return
	}

	platform, userID := hs.parseLastChannel(lastChannel)

	// 跳过无法接收消息的内部通道
	if platform == "" || userID == "" {
		return
	}

	pubCtx, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pubCancel()
	msgBus.PublishOutbound(pubCtx, bus.OutboundMessage{
		Context: bus.NewOutboundContext(platform, userID, ""),
		Content: response,
	})

	hs.logInfof("Heartbeat result sent to %s", platform)
}

// parseLastChannel 将最近通道字符串解析为 platform 和 userID。
// 对于无效通道或内部通道，返回空字符串。
func (hs *HeartbeatService) parseLastChannel(lastChannel string) (platform, userID string) {
	if lastChannel == "" {
		return "", ""
	}

	// 解析通道格式："platform:user_id"（例如 "telegram:123456"）
	parts := strings.SplitN(lastChannel, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		hs.logErrorf("Invalid last channel format: %s", lastChannel)
		return "", ""
	}

	platform, userID = parts[0], parts[1]

	// 跳过内部通道
	if constants.IsInternalChannel(platform) {
		hs.logInfof("Skipping internal channel: %s", platform)
		return "", ""
	}

	return platform, userID
}

// logInfof 向心跳日志记录一条信息级消息
func (hs *HeartbeatService) logInfof(format string, args ...any) {
	hs.logf("INFO", format, args...)
}

// logErrorf 向心跳日志记录一条错误级消息
func (hs *HeartbeatService) logErrorf(format string, args ...any) {
	hs.logf("ERROR", format, args...)
}

// logf 向心跳日志文件写入一条消息
func (hs *HeartbeatService) logf(level, format string, args ...any) {
	logFile := filepath.Join(hs.workspace, "heartbeat.log")
	f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fmt.Fprintf(f, "[%s] [%s] %s\n", timestamp, level, fmt.Sprintf(format, args...))
}
