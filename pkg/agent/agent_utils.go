// Chagent - 超轻量个人 AI 智能体

package agent

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/bus"
	"github.com/rlds/chagent/pkg/commands"
	"github.com/rlds/chagent/pkg/config"
	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/session"
	"github.com/rlds/chagent/pkg/utils"
)

func outboundContextFromInbound(
	inbound *bus.InboundContext,
	channel, chatID, replyToMessageID string,
) bus.InboundContext {
	if inbound == nil {
		return bus.NewOutboundContext(channel, chatID, replyToMessageID)
	}

	outboundCtx := *cloneInboundContext(inbound)
	if outboundCtx.Channel == "" {
		outboundCtx.Channel = channel
	}
	if outboundCtx.ChatID == "" {
		outboundCtx.ChatID = chatID
	}
	if outboundCtx.ReplyToMessageID == "" {
		outboundCtx.ReplyToMessageID = replyToMessageID
	}
	return outboundCtx
}

func outboundScopeFromSessionScope(scope *session.SessionScope) *bus.OutboundScope {
	if scope == nil {
		return nil
	}
	outboundScope := &bus.OutboundScope{
		Version: scope.Version,
		AgentID: scope.AgentID,
		Channel: scope.Channel,
		Account: scope.Account,
	}
	if len(scope.Dimensions) > 0 {
		outboundScope.Dimensions = append([]string(nil), scope.Dimensions...)
	}
	if len(scope.Values) > 0 {
		outboundScope.Values = make(map[string]string, len(scope.Values))
		for key, value := range scope.Values {
			outboundScope.Values[key] = value
		}
	}
	return outboundScope
}

func outboundTurnMetadata(
	agentID, sessionKey string,
	scope *session.SessionScope,
) (string, string, *bus.OutboundScope) {
	return agentID, sessionKey, outboundScopeFromSessionScope(scope)
}

func outboundMessageForTurn(ts *turnState, content string) bus.OutboundMessage {
	agentID, sessionKey, scope := outboundTurnMetadata(ts.agent.ID, ts.sessionKey, ts.opts.Dispatch.SessionScope)
	msg := bus.OutboundMessage{
		Channel: ts.channel,
		ChatID:  ts.chatID,
		Context: outboundContextFromInbound(
			ts.opts.Dispatch.InboundContext,
			ts.channel,
			ts.chatID,
			ts.opts.Dispatch.ReplyToMessageID(),
		),
		AgentID:    agentID,
		SessionKey: sessionKey,
		Scope:      scope,
		Content:    content,
	}
	// 把本回合最后一次 LLM 调用的用量信息传递到出站消息
	if usage := ts.GetLastUsage(); usage != nil {
		msg.Usage = usage
	}
	return msg
}

func markFinalOutbound(msg *bus.OutboundMessage) {
	if msg == nil {
		return
	}
	if msg.Context.Raw == nil {
		msg.Context.Raw = make(map[string]string, 1)
	}
	msg.Context.Raw[metadataKeyOutboundKind] = outboundKindFinal
}

type outboundTurnMessageOptions struct {
	kind      string
	modelName string
	raw       map[string]string
}

func outboundMessageForTurnWithOptions(
	ts *turnState,
	content string,
	opts outboundTurnMessageOptions,
) bus.OutboundMessage {
	msg := outboundMessageForTurn(ts, content)
	trimmedKind := strings.TrimSpace(opts.kind)
	trimmedModelName := strings.TrimSpace(opts.modelName)
	rawCount := len(opts.raw)
	if trimmedKind != "" {
		rawCount++
	}
	if trimmedModelName != "" {
		rawCount++
	}
	if rawCount == 0 {
		return msg
	}

	if msg.Context.Raw == nil {
		msg.Context.Raw = make(map[string]string, rawCount)
	}
	if trimmedKind != "" {
		msg.Context.Raw[metadataKeyMessageKind] = trimmedKind
	}
	if trimmedModelName != "" {
		msg.Context.Raw["model_name"] = trimmedModelName
	}
	for key, value := range opts.raw {
		if strings.TrimSpace(key) == "" {
			continue
		}
		msg.Context.Raw[key] = value
	}
	return msg
}

func latestUserContent(messages []providers.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if content := strings.TrimSpace(msg.Content); content != "" {
			return content
		}
	}
	return ""
}

func toolFeedbackExplanationFromResponse(
	response *providers.LLMResponse,
	messages []providers.Message,
) string {
	if response == nil {
		return ""
	}
	explanation := strings.TrimSpace(response.Content)
	if explanation == "" {
		explanation = toolFeedbackExplanationFromToolCalls(response.ToolCalls)
	}
	if explanation == "" {
		explanation = toolFeedbackExplanationFromMessages(messages)
	}
	return explanation
}

func toolFeedbackExplanationFromToolCalls(toolCalls []providers.ToolCall) string {
	for _, tc := range toolCalls {
		if tc.ExtraContent == nil {
			continue
		}
		if explanation := strings.TrimSpace(tc.ExtraContent.ToolFeedbackExplanation); explanation != "" {
			return explanation
		}
	}
	return ""
}

func toolFeedbackExplanationForToolCall(
	response *providers.LLMResponse,
	toolCall providers.ToolCall,
	messages []providers.Message,
) string {
	if toolCall.ExtraContent != nil {
		if explanation := strings.TrimSpace(toolCall.ExtraContent.ToolFeedbackExplanation); explanation != "" {
			return explanation
		}
	}
	if response == nil {
		return toolFeedbackExplanationFromMessages(messages)
	}

	explanation := strings.TrimSpace(response.Content)
	if explanation == "" {
		explanation = toolFeedbackExplanationFromMessages(messages)
	}
	return explanation
}

func toolFeedbackExplanationFromMessages(messages []providers.Message) string {
	explanation := latestUserContent(messages)
	if explanation != "" {
		return utils.ToolFeedbackContinuationHint + ": " + explanation
	}
	return ""
}

func toolFeedbackArgsPreview(args map[string]any, maxLen int) string {
	argsJSON := utils.FormatArgsJSON(args, true, false)
	return utils.Truncate(argsJSON, maxLen)
}

func shouldPublishToolFeedback(cfg *config.Config, ts *turnState) bool {
	if ts == nil || ts.channel == "" || ts.opts.SuppressToolFeedback {
		return false
	}
	return cfg != nil && cfg.Agents.Defaults.IsToolFeedbackEnabled()
}

func cloneEventArguments(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}

	cloned := make(map[string]any, len(args))
	for k, v := range args {
		cloned[k] = v
	}
	return cloned
}

func hookDeniedToolContent(prefix, reason string) string {
	if reason == "" {
		return prefix
	}
	return prefix + ": " + reason
}

func appendEventContextFields(fields map[string]any, turnCtx *TurnContext) {
	if turnCtx == nil {
		return
	}

	if inbound := turnCtx.Inbound; inbound != nil {
		if inbound.Channel != "" {
			fields["inbound_channel"] = inbound.Channel
		}
		if inbound.Account != "" {
			fields["inbound_account"] = inbound.Account
		}
		if inbound.ChatID != "" {
			fields["inbound_chat_id"] = inbound.ChatID
		}
		if inbound.ChatType != "" {
			fields["inbound_chat_type"] = inbound.ChatType
		}
		if inbound.TopicID != "" {
			fields["inbound_topic_id"] = inbound.TopicID
		}
		if inbound.SpaceType != "" {
			fields["inbound_space_type"] = inbound.SpaceType
		}
		if inbound.SpaceID != "" {
			fields["inbound_space_id"] = inbound.SpaceID
		}
		if inbound.SenderID != "" {
			fields["inbound_sender_id"] = inbound.SenderID
		}
		if inbound.Mentioned {
			fields["inbound_mentioned"] = true
		}
	}

	if route := turnCtx.Route; route != nil {
		if route.AgentID != "" {
			fields["route_agent_id"] = route.AgentID
		}
		if route.Channel != "" {
			fields["route_channel"] = route.Channel
		}
		if route.AccountID != "" {
			fields["route_account_id"] = route.AccountID
		}
		if route.MatchedBy != "" {
			fields["route_matched_by"] = route.MatchedBy
		}
		if len(route.SessionPolicy.Dimensions) > 0 {
			fields["route_dimensions"] = strings.Join(route.SessionPolicy.Dimensions, ",")
		}
		if count := len(route.SessionPolicy.IdentityLinks); count > 0 {
			fields["route_identity_link_count"] = count
		}
	}

	if scope := turnCtx.Scope; scope != nil {
		if scope.Version > 0 {
			fields["scope_version"] = scope.Version
		}
		if scope.AgentID != "" {
			fields["scope_agent_id"] = scope.AgentID
		}
		if scope.Channel != "" {
			fields["scope_channel"] = scope.Channel
		}
		if scope.Account != "" {
			fields["scope_account"] = scope.Account
		}
		if len(scope.Dimensions) > 0 {
			fields["scope_dimensions"] = strings.Join(scope.Dimensions, ",")
		}
		for dim, value := range scope.Values {
			if dim == "" || value == "" {
				continue
			}
			fields["scope_"+dim] = value
		}
	}
}

func inferMediaType(filename, contentType string) string {
	ct := strings.ToLower(contentType)
	fn := strings.ToLower(filename)

	// SVG 是图像 MIME 类型，但 Telegram SendPhoto 等仅支持位图的投递端点
	// 会拒绝它。因此把它当作文件/文档处理。
	if strings.HasPrefix(ct, "image/svg") || filepath.Ext(fn) == ".svg" {
		return "file"
	}

	if strings.HasPrefix(ct, "image/") {
		return "image"
	}
	if strings.HasPrefix(ct, "audio/") || ct == "application/ogg" {
		return "audio"
	}
	if strings.HasPrefix(ct, "video/") {
		return "video"
	}

	// 回退：根据扩展名推断
	ext := filepath.Ext(fn)
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return "image"
	case ".mp3", ".wav", ".ogg", ".m4a", ".flac", ".aac", ".wma", ".opus":
		return "audio"
	case ".mp4", ".avi", ".mov", ".webm", ".mkv":
		return "video"
	}

	return "file"
}

func normalizedInboundContext(msg bus.InboundMessage) bus.InboundContext {
	return bus.NormalizeInboundMessage(msg).Context
}

func resolveScopeKey(routeSessionKey, msgSessionKey string) string {
	if isExplicitSessionKey(msgSessionKey) {
		return msgSessionKey
	}
	return routeSessionKey
}

func isExplicitSessionKey(sessionKey string) bool {
	return session.IsExplicitSessionKey(sessionKey)
}

func buildSessionAliases(canonicalKey string, keys ...string) []string {
	if len(keys) == 0 {
		return nil
	}
	aliases := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	canonicalKey = strings.TrimSpace(canonicalKey)
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || key == canonicalKey {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		aliases = append(aliases, key)
	}
	if len(aliases) == 0 {
		return nil
	}
	return aliases
}

func ensureSessionMetadata(store session.SessionStore, key string, scope *session.SessionScope, aliases []string) {
	if key == "" || scope == nil {
		return
	}
	metaStore, ok := store.(interface {
		EnsureSessionMetadata(sessionKey string, scope *session.SessionScope, aliases []string)
	})
	if !ok {
		return
	}
	metaStore.EnsureSessionMetadata(key, scope, aliases)
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func formatMessagesForLog(messages []providers.Message) string {
	if len(messages) == 0 {
		return "[]"
	}

	var sb strings.Builder
	sb.WriteString("[\n")
	for i, msg := range messages {
		fmt.Fprintf(&sb, "  [%d] Role: %s\n", i, msg.Role)
		if len(msg.ToolCalls) > 0 {
			sb.WriteString("  ToolCalls:\n")
			for _, tc := range msg.ToolCalls {
				fmt.Fprintf(&sb, "    - ID: %s, Type: %s, Name: %s\n", tc.ID, tc.Type, tc.Name)
				if tc.Function != nil {
					fmt.Fprintf(
						&sb,
						"      Arguments: %s\n",
						utils.Truncate(tc.Function.Arguments, 200),
					)
				}
			}
		}
		if msg.Content != "" {
			content := utils.Truncate(msg.Content, 200)
			fmt.Fprintf(&sb, "  Content: %s\n", content)
		}
		if msg.ToolCallID != "" {
			fmt.Fprintf(&sb, "  ToolCallID: %s\n", msg.ToolCallID)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("]")
	return sb.String()
}

func formatToolsForLog(toolDefs []providers.ToolDefinition) string {
	if len(toolDefs) == 0 {
		return "[]"
	}

	var sb strings.Builder
	sb.WriteString("[\n")
	for i, tool := range toolDefs {
		fmt.Fprintf(&sb, "  [%d] Type: %s, Name: %s\n", i, tool.Type, tool.Function.Name)
		fmt.Fprintf(&sb, "      Description: %s\n", tool.Function.Description)
		if len(tool.Function.Parameters) > 0 {
			fmt.Fprintf(
				&sb,
				"      Parameters: %s\n",
				utils.Truncate(fmt.Sprintf("%v", tool.Function.Parameters), 200),
			)
		}
	}
	sb.WriteString("]")
	return sb.String()
}

func activeSkillNames(agent *AgentInstance, opts processOptions) []string {
	if agent == nil {
		return nil
	}
	if turnProfileSkillsOff(opts.TurnProfile) {
		return nil
	}

	combined := make([]string, 0, len(agent.SkillsFilter)+len(opts.ForcedSkills))
	combined = append(combined, agent.SkillsFilter...)
	combined = append(combined, opts.ForcedSkills...)
	if len(combined) == 0 {
		return nil
	}

	var resolved []string
	seen := make(map[string]struct{}, len(combined))
	for _, name := range combined {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if agent.ContextBuilder != nil {
			if canonical, ok := agent.ContextBuilder.ResolveSkillName(name); ok {
				name = canonical
			}
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		resolved = append(resolved, name)
	}

	if turnProfileCustomSkills(opts.TurnProfile) {
		return filterNamesByTurnProfile(resolved, opts.TurnProfile.AllowedSkills)
	}
	return resolved
}

func sideQuestionResponseContent(response *providers.LLMResponse) string {
	if response == nil {
		return ""
	}
	if strings.TrimSpace(response.Content) != "" {
		return response.Content
	}
	return responseReasoningContent(response)
}

func responseReasoningContent(response *providers.LLMResponse) string {
	if response == nil {
		return ""
	}
	if strings.TrimSpace(response.Reasoning) != "" {
		return response.Reasoning
	}
	if strings.TrimSpace(response.ReasoningContent) != "" {
		return response.ReasoningContent
	}
	return ""
}

// execResponseUsage 从 LLMResponse 中安全地提取 Usage，若为 nil 则返回 nil。
func execResponseUsage(response *providers.LLMResponse) *providers.UsageInfo {
	if response == nil {
		return nil
	}
	return response.Usage
}

func shallowCloneLLMOptions(opts map[string]any) map[string]any {
	clone := make(map[string]any, len(opts))
	maps.Copy(clone, opts)
	return clone
}

func hasMediaRefs(messages []providers.Message) bool {
	for _, msg := range messages {
		if len(msg.Media) > 0 {
			return true
		}
	}
	return false
}

func sideQuestionModelName(agent *AgentInstance, usedLight bool) string {
	if usedLight && len(agent.LightCandidates) > 0 {
		if name := resolvedCandidateModelName(agent.LightCandidates, ""); name != "" {
			return name
		}
	}
	return agent.Model
}

func modelNameFromIdentityKey(identityKey string) string {
	identityKey = strings.TrimSpace(identityKey)
	if identityKey == "" {
		return ""
	}
	parts := strings.SplitN(identityKey, "/", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return identityKey
}

func modelAliasFromCandidateIdentityKey(identityKey string) string {
	const prefix = "model_name:"
	if !strings.HasPrefix(identityKey, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(identityKey, prefix))
}

func closeProviderIfStateful(provider providers.LLMProvider) {
	if stateful, ok := provider.(providers.StatefulProvider); ok {
		stateful.Close()
	}
}

// activeRequestsInc 原子地递增活跃请求计数。
func (al *AgentLoop) activeRequestsInc() {
	al.activeReqMu.Lock()
	al.activeReqCount++
	al.activeReqMu.Unlock()
}

// activeRequestsDec 原子地递减活跃请求计数，
// 并在计数归零时唤醒任何阻塞在 waitForActiveRequests 中的 goroutine。
func (al *AgentLoop) activeRequestsDec() {
	al.activeReqMu.Lock()
	al.activeReqCount--
	if al.activeReqCount == 0 {
		al.activeReqCond.Broadcast()
	}
	al.activeReqMu.Unlock()
}

func (al *AgentLoop) waitForActiveRequests(ctx context.Context, timeout time.Duration) bool {
	al.activeReqMu.Lock()
	if al.activeReqCount == 0 {
		al.activeReqMu.Unlock()
		return true
	}

	// 在超时或 context 取消时唤醒阻塞的 Wait() 调用方。
	var timedOut bool
	if timeout > 0 {
		time.AfterFunc(timeout, func() {
			al.activeReqMu.Lock()
			timedOut = true
			al.activeReqCond.Broadcast()
			al.activeReqMu.Unlock()
		})
	}
	go func() {
		<-ctx.Done()
		al.activeReqMu.Lock()
		al.activeReqCond.Broadcast()
		al.activeReqMu.Unlock()
	}()

	for al.activeReqCount > 0 && !timedOut && ctx.Err() == nil {
		al.activeReqCond.Wait()
	}
	result := al.activeReqCount == 0
	al.activeReqMu.Unlock()
	return result
}

func (al *AgentLoop) closeReloadedProvider(ctx context.Context, provider providers.StatefulProvider) {
	waitCtx := ctx
	if waitCtx == nil {
		waitCtx = context.Background()
	}

	drained := al.waitForActiveRequests(waitCtx, providerReloadGracePeriod)
	if !drained {
		fields := map[string]any{"grace_period": providerReloadGracePeriod.String()}
		if err := waitCtx.Err(); err != nil {
			fields["error"] = err.Error()
			logger.WarnCF("agent", "Provider reload interrupted while waiting for in-flight requests", fields)
		} else {
			logger.WarnCF("agent", "Provider reload grace period expired with in-flight requests still running", fields)
		}
	}

	provider.Close()
}

func makePendingTurnID(sessionKey string, seq uint64) string {
	return pendingTurnPrefix + sessionKey + "-" + fmt.Sprintf("%d", seq)
}

func commandsUnavailableSkillMessage() string {
	return "当前上下文不支持技能选择。"
}

func buildUseCommandHelp(agent *AgentInstance) string {
	if agent == nil || agent.ContextBuilder == nil {
		return "用法：/use <skill> [message]"
	}

	names := agent.ContextBuilder.ListSkillNames()
	if len(names) == 0 {
		return "用法：/use <skill> [message]\n未安装任何技能。"
	}

	return fmt.Sprintf(
		"用法：/use <skill> [message]\n\n已安装技能：\n- %s\n\n使用 /use <skill> 可将技能应用到下一条消息，或使用 /use <skill> <message> 立即强制应用。",
		strings.Join(names, "\n- "),
	)
}

func mapCommandError(result commands.ExecuteResult) string {
	if result.Command == "" {
		return fmt.Sprintf("命令执行失败：%v", result.Err)
	}
	return fmt.Sprintf("执行 /%s 失败：%v", result.Command, result.Err)
}

func isNativeSearchProvider(p providers.LLMProvider) bool {
	if ns, ok := p.(providers.NativeSearchCapable); ok {
		return ns.SupportsNativeSearch()
	}
	return false
}

func filterClientWebSearch(tools []providers.ToolDefinition) []providers.ToolDefinition {
	result := make([]providers.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if strings.EqualFold(t.Function.Name, "web_search") {
			continue
		}
		result = append(result, t)
	}
	return result
}

func extractProvider(registry *AgentRegistry) (providers.LLMProvider, bool) {
	if registry == nil {
		return nil, false
	}
	// 取任意一个 agent 以访问 provider
	defaultAgent := registry.GetDefaultAgent()
	if defaultAgent == nil {
		return nil, false
	}
	return defaultAgent.Provider, true
}
