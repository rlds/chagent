// Chagent - 超轻量级个人 AI agent
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/rlds/chagent/pkg/channels"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/tools"
	"github.com/rlds/chagent/pkg/utils"
)

// 工具执行上报（execution.report）。
//
// 设计要点：
//   - 默认开启，只有 config.json 里的 `execution_report.enabled: false` 才关闭
//     （见 config.ExecutionReportConfig）；
//   - 上报发生在工具执行完成的**同一位置**，但不走 AfterTool hook：挂载任何
//     hook 都会关闭并发批次路径（pipeline_execute.go 的 toolHooksWouldIntercept），
//     用 hook 上报会带来真实性能回归；
//   - 上报是异步的：入队即返回，受有界队列保护，发送在独立 goroutine 中完成，
//     失败只记日志，绝不影响工具结果或主循环；
//   - 载荷只包含摘要：参数只上报键名/数量/摘要哈希，结果只上报长度/有界摘要哈希，
//     不上报完整结果或大参数明文；
//   - event_id 稳定且唯一（优先 session+tool_call_id 的哈希），便于 Server 幂等。

const (
	// executionReportQueueSize 是有界队列容量。队列满时丢弃并计数，绝不阻塞。
	executionReportQueueSize = 256
	// executionReportSendTimeout 是单条上报的发送超时。
	executionReportSendTimeout = 2 * time.Second
	// executionReportDedupTTL 是 event_id 去重窗口。
	executionReportDedupTTL = 10 * time.Minute
	// executionReportDedupMax 是去重表上限，超过后清理过期项。
	executionReportDedupMax = 1024
	// executionReportDigestBytes 是结果摘要参与哈希的最大字节数。
	executionReportDigestBytes = 8 * 1024
	// executionReportErrorMaxRunes 是 error 字段的最大长度。
	executionReportErrorMaxRunes = 500
)

// execution.report 载荷字段名（线上契约，改动需同步 Server）。
const (
	executionReportKeyEventID       = "event_id"
	executionReportKeyAgentNickname = "agent_nickname"
	executionReportKeySessionID     = "session_id"
	executionReportKeyChatID        = "chat_id"
	executionReportKeyUserID        = "user_id"
	executionReportKeyToolName      = "tool_name"
	executionReportKeyToolCallID    = "tool_call_id"
	executionReportKeyExecutionType = "execution_type"
	executionReportKeySkillName     = "skill_name"
	executionReportKeyMCPServer     = "mcp_server"
	executionReportKeyMCPTool       = "mcp_tool"
	executionReportKeyStatus        = "status"
	executionReportKeyStartedAt     = "started_at"
	executionReportKeyEndedAt       = "ended_at"
	executionReportKeyDurationMS    = "duration_ms"
	executionReportKeyTrigger       = "trigger"
	executionReportKeyError         = "error"
	executionReportKeyMetadata      = "metadata"
	executionReportKeyArguments     = "arguments"
	executionReportKeyResult        = "result"
)

// 上报的执行类型。
const (
	executionTypeSkill = "skill"
	executionTypeMCP   = "mcp"
	executionTypeTool  = "tool"
)

// 上报的执行状态。
const (
	executionStatusSuccess = "success"
	executionStatusError   = "error"
)

// toolExecutionReport 是调用点提供的、一次工具执行完成的事实集合。
// 它保持轻量：只携带引用与小标量，繁重的摘要计算在入队前有界完成。
type toolExecutionReport struct {
	ToolName   string
	ToolCallID string
	Arguments  map[string]any
	Result     *tools.ToolResult
	StartedAt  time.Time
	Duration   time.Duration
	Iteration  int
}

// executionReportJob 是队列中的一条待发送上报。
type executionReportJob struct {
	Channel string
	ChatID  string
	Payload map[string]any
}

// executionReportSink 是 execution.report 的异步发送器。
//
// 它由 AgentLoop 持有并在 Close 时停止。所有方法对 nil 接收者安全。
type executionReportSink struct {
	al    *AgentLoop
	queue chan executionReportJob
	done  chan struct{}

	closeOnce sync.Once

	dedupMu sync.Mutex
	dedup   map[string]time.Time

	sent    atomic.Uint64
	dropped atomic.Uint64
	failed  atomic.Uint64
}

func newExecutionReportSink(al *AgentLoop) *executionReportSink {
	sink := &executionReportSink{
		al:    al,
		queue: make(chan executionReportJob, executionReportQueueSize),
		done:  make(chan struct{}),
		dedup: make(map[string]time.Time),
	}
	go sink.run()
	return sink
}

// close 停止发送 goroutine。已入队但未发送的上报会被丢弃（尽力而为）。
func (s *executionReportSink) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.done)
	})
}

// accept 在去重窗口内按 event_id 去重：同一执行重复进入时只发送一次。
func (s *executionReportSink) accept(eventID string) bool {
	if s == nil {
		return false
	}
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return true
	}
	now := time.Now()
	s.dedupMu.Lock()
	defer s.dedupMu.Unlock()
	if _, exists := s.dedup[eventID]; exists {
		return false
	}
	if len(s.dedup) >= executionReportDedupMax {
		for key, at := range s.dedup {
			if now.Sub(at) > executionReportDedupTTL {
				delete(s.dedup, key)
			}
		}
		// 仍然超限时丢弃最旧的一半，保证内存有界。
		if len(s.dedup) >= executionReportDedupMax {
			cutoff := now.Add(-executionReportDedupTTL / 2)
			for key, at := range s.dedup {
				if at.Before(cutoff) {
					delete(s.dedup, key)
				}
			}
		}
	}
	s.dedup[eventID] = now
	return true
}

// enqueue 非阻塞入队。队列满时丢弃并记录，绝不阻塞调用方。
func (s *executionReportSink) enqueue(job executionReportJob) bool {
	if s == nil {
		return false
	}
	eventID := ""
	if job.Payload != nil {
		eventID, _ = job.Payload[executionReportKeyEventID].(string)
	}
	if !s.accept(eventID) {
		logger.DebugCF("agent", "Duplicate execution.report dropped", map[string]any{
			"event_id": eventID,
		})
		return false
	}

	select {
	case <-s.done:
		return false
	default:
	}

	select {
	case s.queue <- job:
		return true
	case <-s.done:
		return false
	default:
		dropped := s.dropped.Add(1)
		logger.WarnCF("agent", "execution.report queue full; dropping report", map[string]any{
			"event_id": eventID,
			"dropped":  dropped,
			"queue":    executionReportQueueSize,
		})
		return false
	}
}

func (s *executionReportSink) run() {
	for {
		select {
		case <-s.done:
			return
		case job := <-s.queue:
			s.deliver(job)
		}
	}
}

// deliver 解析通道并发送一条上报。它只在 sink goroutine 中执行。
func (s *executionReportSink) deliver(job executionReportJob) {
	al := s.al
	if al == nil {
		return
	}
	cm := al.channelManager
	if cm == nil {
		return
	}
	if strings.TrimSpace(job.Channel) == "" {
		return
	}
	ch, ok := cm.GetChannel(job.Channel)
	if !ok || ch == nil {
		logger.DebugCF("agent", "execution.report skipped: channel not found", map[string]any{
			"channel": job.Channel,
		})
		return
	}
	sender, ok := ch.(channels.ExecutionReportSender)
	if !ok {
		logger.DebugCF("agent", "execution.report skipped: channel cannot report", map[string]any{
			"channel": job.Channel,
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), executionReportSendTimeout)
	defer cancel()
	if err := sender.SendExecutionReport(ctx, job.ChatID, job.Payload); err != nil {
		s.failed.Add(1)
		fields := map[string]any{
			"channel":  job.Channel,
			"chat_id":  job.ChatID,
			"failed":   s.failed.Load(),
			"event_id": job.Payload[executionReportKeyEventID],
			"error":    err.Error(),
		}
		// 未连接/通道未运行是正常状态（pico 未连接不得阻塞），降到 debug。
		if errors.Is(err, channels.ErrNotRunning) || errors.Is(err, channels.ErrSendFailed) {
			logger.DebugCF("agent", "execution.report not delivered", fields)
			return
		}
		logger.WarnCF("agent", "execution.report delivery failed", fields)
		return
	}
	s.sent.Add(1)
	logger.DebugCF("agent", "execution.report sent", map[string]any{
		"channel":  job.Channel,
		"chat_id":  job.ChatID,
		"event_id": job.Payload[executionReportKeyEventID],
		"sent":     s.sent.Load(),
	})
}

// reportToolExecution 是工具执行完成路径的唯一上报入口。
//
// 它必须被**非阻塞**调用：构建载荷（只做有界摘要）后立即入队返回。
// 配置显式关闭时连载荷都不构建。
func (al *AgentLoop) reportToolExecution(ts *turnState, ev toolExecutionReport) {
	if al == nil || ts == nil || ts.agent == nil {
		return
	}
	sink := al.executionReports
	if sink == nil {
		return
	}
	cfg := al.GetConfig()
	if !cfg.GetExecutionReport().IsEnabled() {
		return
	}
	payload := buildExecutionReportPayload(cfg, ts, ev)
	sink.enqueue(executionReportJob{
		Channel: ts.channel,
		ChatID:  ts.chatID,
		Payload: payload,
	})
}

// buildExecutionReportPayload 组装一条 execution.report 载荷。
// 它是纯函数（除了读取环境变量与时钟），便于单元测试。
func buildExecutionReportPayload(
	cfg *config.Config,
	ts *turnState,
	ev toolExecutionReport,
) map[string]any {
	startedAt := ev.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	duration := ev.Duration
	if duration < 0 {
		duration = 0
	}
	endedAt := startedAt.Add(duration)

	classification := classifyExecution(ts.agent, cfg, ev.ToolName, ev.Arguments)

	status := executionStatusSuccess
	if ev.Result != nil && ev.Result.IsError {
		status = executionStatusError
	}

	errorText := ""
	if ev.Result != nil && ev.Result.IsError {
		errorText = utils.Truncate(
			strings.TrimSpace(toolErrorSummary(ev.Result)),
			executionReportErrorMaxRunes,
		)
		if cfg != nil {
			errorText = cfg.FilterSensitiveData(errorText)
		}
	}

	metadata := map[string]any{
		"model":     strings.TrimSpace(ts.agent.Model),
		"turn":      ts.turnID,
		"iteration": ev.Iteration,
		"agent_id":  ts.agent.ID,
		"channel":   ts.channel,
	}

	argsKeys, argsCount, argsDigest := summarizeExecutionArguments(ev.Arguments)
	resultChars, resultDigest := summarizeExecutionResult(ev.Result)

	return map[string]any{
		executionReportKeyEventID:       executionReportEventID(ts.sessionKey, ev.ToolCallID),
		executionReportKeyAgentNickname: executionReportAgentNickname(ts.agent),
		executionReportKeySessionID:     ts.sessionKey,
		executionReportKeyChatID:        ts.chatID,
		executionReportKeyUserID:        ts.opts.Dispatch.SenderID(),
		executionReportKeyToolName:      ev.ToolName,
		executionReportKeyToolCallID:    ev.ToolCallID,
		executionReportKeyExecutionType: classification.ExecutionType,
		executionReportKeySkillName:     classification.SkillName,
		executionReportKeyMCPServer:     classification.MCPServer,
		executionReportKeyMCPTool:       classification.MCPTool,
		executionReportKeyStatus:        status,
		executionReportKeyStartedAt:     startedAt.UTC().Format(time.RFC3339Nano),
		executionReportKeyEndedAt:       endedAt.UTC().Format(time.RFC3339Nano),
		executionReportKeyDurationMS:    duration.Milliseconds(),
		executionReportKeyTrigger:       executionReportTrigger(ts),
		executionReportKeyError:         errorText,
		executionReportKeyMetadata:      metadata,
		executionReportKeyArguments: map[string]any{
			"keys":   argsKeys,
			"count":  argsCount,
			"digest": argsDigest,
		},
		executionReportKeyResult: map[string]any{
			"chars":  resultChars,
			"digest": resultDigest,
			"error":  ev.Result != nil && ev.Result.IsError,
		},
	}
}

// executionClassification 是一次工具执行的分类结果。
type executionClassification struct {
	ExecutionType string
	SkillName     string
	MCPServer     string
	MCPTool       string
}

// classifyExecution 把工具名分类为 skill / mcp / tool。
func classifyExecution(
	agent *AgentInstance,
	cfg *config.Config,
	toolName string,
	args map[string]any,
) executionClassification {
	name := strings.ToLower(strings.TrimSpace(toolName))
	switch name {
	case "skill", "use_skill":
		return executionClassification{
			ExecutionType: executionTypeSkill,
			SkillName:     skillNameFromArguments(args),
		}
	}
	if strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, "mcp_") {
		server, tool := resolveMCPOrigin(agent, cfg, toolName)
		return executionClassification{
			ExecutionType: executionTypeMCP,
			MCPServer:     server,
			MCPTool:       tool,
		}
	}
	return executionClassification{ExecutionType: executionTypeTool}
}

// skillNameFromArguments 从 skill / use_skill 的参数里取技能名。
func skillNameFromArguments(args map[string]any) string {
	for _, key := range []string{"name", "skill", "skill_name"} {
		value, ok := args[key]
		if !ok {
			continue
		}
		if name, ok := value.(string); ok {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// mcpOriginNamer 由 pkg/tools/integration.MCPTool 实现，提供未规范化的来源名。
type mcpOriginNamer interface {
	MCPServerName() string
	MCPToolName() string
}

// resolveMCPOrigin 解析 MCP 工具的服务器名与原始工具名。
//
// 优先从注册表里的 MCPTool 包装器直接读取（权威）；包装器不可得时退化为
// 名称解析：
//  1. `mcp__<server>__<tool>`（双下划线分隔）直接切分；
//  2. `mcp_<sanitized server>_<sanitized tool>` 用已配置的 server 名做最长前缀匹配；
//  3. 都不匹配时，mcp_tool 取去掉 `mcp_` 前缀后的剩余部分，server 留空。
func resolveMCPOrigin(agent *AgentInstance, cfg *config.Config, toolName string) (string, string) {
	if agent != nil && agent.Tools != nil {
		if tool, ok := agent.Tools.Get(toolName); ok {
			if namer, ok := tool.(mcpOriginNamer); ok {
				return strings.TrimSpace(namer.MCPServerName()), strings.TrimSpace(namer.MCPToolName())
			}
		}
	}

	register := func(toolName string) string {
		trimmed := strings.ToLower(strings.TrimSpace(toolName))
		trimmed = strings.TrimPrefix(trimmed, "mcp__")
		trimmed = strings.TrimPrefix(trimmed, "mcp_")
		return trimmed
	}
	rest := register(toolName)
	if rest == "" {
		return "", ""
	}
	if idx := strings.Index(rest, "__"); idx > 0 {
		return rest[:idx], strings.TrimPrefix(rest[idx+2:], "_")
	}

	if cfg != nil {
		prefix := strings.ToLower(strings.TrimSpace(toolName))
		bestServer := ""
		bestLength := 0
		for server := range cfg.Tools.MCP.Servers {
			sanitized := sanitizeExecutionIdentifier(server)
			if sanitized == "" {
				continue
			}
			candidate := "mcp_" + sanitized + "_"
			if strings.HasPrefix(prefix, candidate) && len(candidate) > bestLength {
				bestServer = server
				bestLength = len(candidate)
			}
		}
		if bestServer != "" {
			tool := rest[len(sanitizeExecutionIdentifier(bestServer)):]
			return bestServer, strings.TrimPrefix(tool, "_")
		}
	}
	return "", rest
}

// sanitizeExecutionIdentifier 复刻 pkg/tools/integration 的工具名规范化，
// 仅用于按“已配置的 server 名”匹配扁平化后的工具名。
func sanitizeExecutionIdentifier(raw string) string {
	const maxLen = 64
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(s))
	prevUnderscore := false
	for _, r := range s {
		isAllowed := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-'
		if !isAllowed {
			if !prevUnderscore {
				b.WriteRune('_')
				prevUnderscore = true
			}
			continue
		}
		if r == '_' {
			if prevUnderscore {
				continue
			}
			prevUnderscore = true
		} else {
			prevUnderscore = false
		}
		b.WriteRune(r)
	}
	result := strings.Trim(b.String(), "_")
	if result == "" {
		return ""
	}
	if len(result) > maxLen {
		result = result[:maxLen]
	}
	return result
}

// executionReportEventID 生成稳定且唯一的 event_id。
//
// 优先用 session + tool_call_id：同一次工具调用的任意重试/重放都得到同一个
// event_id，Server 可以幂等去重。tool_call_id 缺失时退回 UUID（仍然唯一，
// 但不可幂等）。
func executionReportEventID(sessionKey, toolCallID string) string {
	callID := strings.TrimSpace(toolCallID)
	if callID == "" {
		return "exec-" + uuid.NewString()
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(sessionKey) + "|" + callID))
	return "exec-" + hex.EncodeToString(sum[:])[:24]
}

// executionReportAgentNickname 返回上报用的 agent 昵称。
//
// 优先级：gateway --name（CHAGENT_AGENT_NAME 环境变量）→ agent 配置名 →
// agent ID → "default"。与 pico 的 token 派生使用同一个名字，便于 Server 关联。
func executionReportAgentNickname(agent *AgentInstance) string {
	if name := strings.TrimSpace(os.Getenv(config.AgentNameEnv)); name != "" {
		return config.ResolveAgentName(name)
	}
	if agent != nil {
		if name := strings.TrimSpace(agent.Name); name != "" {
			return name
		}
		if id := strings.TrimSpace(agent.ID); id != "" {
			return id
		}
	}
	return config.ResolveAgentName("")
}

// executionReportTrigger 返回本次执行的触发来源（human/autonomy/scheduler/...）。
func executionReportTrigger(ts *turnState) string {
	if ts == nil {
		return "human"
	}
	trigger := strings.ToLower(strings.TrimSpace(ts.opts.TurnSource))
	if trigger == "" {
		return "human"
	}
	return trigger
}

// summarizeExecutionArguments 返回参数的键名、数量与摘要哈希。
//
// 只暴露键名与 "键:类型:规模" 的哈希，任何参数值（尤其 write_file 的正文）
// 都不会出现在载荷里。
func summarizeExecutionArguments(args map[string]any) ([]string, int, string) {
	if len(args) == 0 {
		return nil, 0, ""
	}
	keys := make([]string, 0, len(args))
	lines := make([]string, 0, len(args))
	for key, value := range args {
		keys = append(keys, key)
		lines = append(lines, fmt.Sprintf("%s:%s:%d", key, executionArgumentKind(value), executionArgumentSize(value)))
	}
	sort.Strings(keys)
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return keys, len(args), hex.EncodeToString(sum[:])[:32]
}

func executionArgumentKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "bool"
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "other"
	}
}

func executionArgumentSize(value any) int {
	switch typed := value.(type) {
	case nil:
		return 0
	case string:
		return len(typed)
	case map[string]any:
		return len(typed)
	case []any:
		return len(typed)
	default:
		return 0
	}
}

// summarizeExecutionResult 返回结果长度与有界摘要哈希。
//
// 哈希只覆盖结果的前 executionReportDigestBytes 字节，加上总长度，避免对大
// 工具输出做全量哈希而拖慢主循环；它只用于关联/去重，不是内容指纹。
func summarizeExecutionResult(result *tools.ToolResult) (int, string) {
	if result == nil {
		return 0, ""
	}
	content := result.ContentForLLM()
	total := len(content)
	if total == 0 {
		return 0, ""
	}
	sample := content
	truncated := false
	if total > executionReportDigestBytes {
		sample = content[:executionReportDigestBytes]
		truncated = true
	}
	sum := sha256.Sum256([]byte(sample))
	digest := hex.EncodeToString(sum[:])[:32]
	if truncated {
		digest += fmt.Sprintf(":prefix%dof%d", executionReportDigestBytes, total)
	}
	return total, digest
}
