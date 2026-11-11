package openai_compat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers/common"
	"github.com/rlds/chagent/pkg/providers/messageutil"
	"github.com/rlds/chagent/pkg/providers/protocoltypes"
)

type (
	ToolCall               = protocoltypes.ToolCall
	FunctionCall           = protocoltypes.FunctionCall
	LLMResponse            = protocoltypes.LLMResponse
	StreamChunk            = protocoltypes.StreamChunk
	UsageInfo              = protocoltypes.UsageInfo
	Message                = protocoltypes.Message
	ToolDefinition         = protocoltypes.ToolDefinition
	ToolFunctionDefinition = protocoltypes.ToolFunctionDefinition
	ExtraContent           = protocoltypes.ExtraContent
	GoogleExtra            = protocoltypes.GoogleExtra
	ReasoningDetail        = protocoltypes.ReasoningDetail
)

type Provider struct {
	apiKey         string
	apiBase        string
	providerName   string
	maxTokensField string // max tokens 的字段名（例如 o1/glm 模型使用 "max_completion_tokens"）
	httpClient     *http.Client
	extraBody      map[string]any // 要注入请求体的附加字段
	customHeaders  map[string]string
	userAgent      string
}

type Option func(*Provider)

const (
	defaultRequestTimeout           = common.DefaultRequestTimeout
	defaultStreamingReadIdleTimeout = 5 * time.Minute
)

var stripModelPrefixProviders = map[string]struct{}{
	"litellm":     {},
	"nearai":      {},
	"venice":      {},
	"moonshot":    {},
	"nvidia":      {},
	"groq":        {},
	"ollama":      {},
	"deepseek":    {},
	"google":      {},
	"openrouter":  {},
	"siliconflow": {},
	"zhipu":       {},
	"mistral":     {},
	"vivgrid":     {},
	"minimax":     {},
	"novita":      {},
	"lmstudio":    {},
}

func WithMaxTokensField(maxTokensField string) Option {
	return func(p *Provider) {
		p.maxTokensField = maxTokensField
	}
}

func WithUserAgent(userAgent string) Option {
	return func(p *Provider) {
		p.userAgent = userAgent
	}
}

func WithRequestTimeout(timeout time.Duration) Option {
	return func(p *Provider) {
		if timeout > 0 {
			p.httpClient.Timeout = timeout
		}
	}
}

func WithExtraBody(extraBody map[string]any) Option {
	return func(p *Provider) {
		p.extraBody = extraBody
	}
}

func WithCustomHeaders(customHeaders map[string]string) Option {
	return func(p *Provider) {
		p.customHeaders = customHeaders
	}
}

func WithProviderName(providerName string) Option {
	return func(p *Provider) {
		p.providerName = strings.ToLower(strings.TrimSpace(providerName))
	}
}

func NewProvider(apiKey, apiBase, proxy string, opts ...Option) *Provider {
	p := &Provider{
		apiKey:     apiKey,
		apiBase:    strings.TrimRight(apiBase, "/"),
		httpClient: common.NewHTTPClient(proxy),
	}

	for _, opt := range opts {
		if opt != nil {
			opt(p)
		}
	}

	return p
}

func NewProviderWithMaxTokensField(apiKey, apiBase, proxy, maxTokensField string) *Provider {
	return NewProvider(apiKey, apiBase, proxy, WithMaxTokensField(maxTokensField))
}

func NewProviderWithMaxTokensFieldAndTimeout(
	apiKey, apiBase, proxy, maxTokensField string,
	requestTimeoutSeconds int,
) *Provider {
	return NewProvider(
		apiKey,
		apiBase,
		proxy,
		WithMaxTokensField(maxTokensField),
		WithRequestTimeout(time.Duration(requestTimeoutSeconds)*time.Second),
	)
}

// buildRequestBody 为 Chat 和 ChatStream 构建公共请求体。
func (p *Provider) buildRequestBody(
	messages []Message, tools []ToolDefinition, model string, options map[string]any,
) map[string]any {
	model = normalizeModel(model, p.apiBase)

	requestBody := map[string]any{
		"model":    model,
		"messages": common.SerializeMessages(p.prepareMessagesForRequest(messages)),
	}

	// 当回退使用其他 provider（例如 DeepSeek）时，该 provider 不得注入 web_search_preview。
	nativeSearch, ok := options["native_search"].(bool)
	if !ok {
		// 若该选项存在但类型不是 bool，则记录警告并
		// 按 false 处理 —— 当调用方无法表达类型明确的意图时，
		// 不得注入 web_search_preview。
		if _, present := options["native_search"]; present {
			logger.WarnCF(
				"provider.openai_compat",
				"native_search option has unexpected type, ignoring",
				map[string]any{
					"type": fmt.Sprintf("%T", options["native_search"]),
				},
			)
		}
	}
	nativeSearch = nativeSearch && isNativeSearchHost(p.apiBase)
	if len(tools) > 0 || nativeSearch {
		requestBody["tools"] = buildToolsList(tools, nativeSearch)
		requestBody["tool_choice"] = "auto"
	}

	if maxTokens, ok := common.AsInt(options["max_tokens"]); ok {
		fieldName := p.maxTokensField
		if fieldName == "" {
			lowerModel := strings.ToLower(model)
			if strings.Contains(lowerModel, "glm") || strings.Contains(lowerModel, "o1") ||
				strings.Contains(lowerModel, "gpt-5") {
				fieldName = "max_completion_tokens"
			} else {
				fieldName = "max_tokens"
			}
		}
		requestBody[fieldName] = maxTokens
	}

	if temperature, ok := common.AsFloat(options["temperature"]); ok {
		lowerModel := strings.ToLower(model)
		if strings.Contains(lowerModel, "kimi") && strings.Contains(lowerModel, "k2") {
			requestBody["temperature"] = 1.0
		} else {
			requestBody["temperature"] = temperature
		}
	}

	// 提示缓存：传入稳定的缓存键，使 OpenAI 能将请求
	// 按相同键分桶，并在多次调用间复用前缀 KV 缓存。
	// 提示缓存仅受 OpenAI 原生端点支持。
	// 非 OpenAI 的 provider 会以 422 错误拒绝未知字段。
	if cacheKey, ok := options["prompt_cache_key"].(string); ok && cacheKey != "" {
		if supportsPromptCacheKey(p.apiBase) {
			requestBody["prompt_cache_key"] = cacheKey
		}
	}

	p.applyThinkingControl(requestBody, model, options)

	// 合并按 provider/模型配置的附加请求体字段。
	// 这些字段最后注入，因此会覆盖默认值。
	maps.Copy(requestBody, p.extraBody)

	return requestBody
}

func (p *Provider) applyThinkingControl(requestBody map[string]any, model string, options map[string]any) {
	level, ok := normalizedThinkingLevel(options)
	if !ok {
		return
	}

	if p.SupportsThinking() {
		p.applyDeepSeekThinkingControl(requestBody, level)
		return
	}

	if level != "off" {
		return
	}

	switch p.thinkingControlKind(model) {
	case "thinking_type":
		requestBody["thinking"] = map[string]any{"type": "disabled"}
	case "enable_thinking":
		requestBody["enable_thinking"] = false
	}
}

func (p *Provider) applyDeepSeekThinkingControl(requestBody map[string]any, level string) {
	switch level {
	case "off":
		requestBody["thinking"] = map[string]any{"type": "disabled"}
	case "low", "medium", "high":
		requestBody["thinking"] = map[string]any{"type": "enabled"}
		requestBody["reasoning_effort"] = "high"
	case "xhigh":
		requestBody["thinking"] = map[string]any{"type": "enabled"}
		requestBody["reasoning_effort"] = "max"
	case "adaptive":
		logger.WarnCF("provider.openai_compat",
			`DeepSeek does not support thinking_level="adaptive"; using provider default thinking behavior`,
			map[string]any{
				"provider":       p.providerName,
				"api_base":       p.apiBase,
				"thinking_level": level,
			},
		)
	}
}

func normalizedThinkingLevel(options map[string]any) (string, bool) {
	raw, ok := options["thinking_level"].(string)
	if !ok {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "low", "medium", "high", "xhigh", "adaptive":
		return strings.ToLower(strings.TrimSpace(raw)), true
	default:
		return "", false
	}
}

func (p *Provider) thinkingControlKind(model string) string {
	providerName := strings.ToLower(strings.TrimSpace(p.providerName))
	lowerModel := strings.ToLower(strings.TrimSpace(model))

	switch providerName {
	case "volcengine":
		return "thinking_type"
	case "zhipu", "zai":
		return "thinking_type"
	case "qwen", "qwen-portal", "qwen-intl", "qwen-international", "dashscope-intl", "qwen-us", "dashscope-us":
		return "enable_thinking"
	case "modelscope":
		if strings.Contains(lowerModel, "qwen") {
			return "enable_thinking"
		}
	}

	if providerName == "openai" || providerName == "" {
		if isVolcengineHost(p.apiBase) || strings.Contains(lowerModel, "doubao") {
			return "thinking_type"
		}
		if isDashScopeHost(p.apiBase) || strings.Contains(lowerModel, "qwen") {
			return "enable_thinking"
		}
	}

	return ""
}

func isVolcengineHost(apiBase string) bool {
	host := normalizedHostname(apiBase)
	return host == "volcengine.com" || strings.HasSuffix(host, ".volcengine.com") ||
		host == "volces.com" || strings.HasSuffix(host, ".volces.com")
}

func isDashScopeHost(apiBase string) bool {
	host := normalizedHostname(apiBase)
	return host == "dashscope.aliyuncs.com" || strings.HasSuffix(host, ".dashscope.aliyuncs.com")
}

func normalizedHostname(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(parsed.Hostname()))
}

func (p *Provider) applyCustomHeaders(req *http.Request) {
	for k, v := range p.customHeaders {
		if strings.TrimSpace(k) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
}

func (p *Provider) SetProviderName(providerName string) {
	p.providerName = strings.ToLower(strings.TrimSpace(providerName))
}

func (p *Provider) SupportsThinking() bool {
	return strings.EqualFold(strings.TrimSpace(p.providerName), "deepseek") || isDeepSeekHost(p.apiBase)
}

func (p *Provider) prepareMessagesForRequest(messages []Message) []Message {
	if len(messages) == 0 {
		return nil
	}

	if p.requiresToolRoundReasoningReplay() {
		return filterReasoningReplayMessages(messages)
	}
	return stripReasoningMessages(messages)
}

func (p *Provider) requiresToolRoundReasoningReplay() bool {
	return p.providerName == "deepseek" ||
		p.providerName == "mimo" ||
		isDeepSeekHost(p.apiBase) ||
		isMiMoHost(p.apiBase)
}

func isDeepSeekHost(apiBase string) bool {
	parsed, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "deepseek.com" || strings.HasSuffix(host, ".deepseek.com")
}

func isMiMoHost(apiBase string) bool {
	parsed, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname()))
	return host == "xiaomimimo.com" || strings.HasSuffix(host, ".xiaomimimo.com")
}

func filterReasoningReplayMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	start := 0

	flush := func(end int) {
		if end <= start {
			return
		}
		out = append(out, filterReasoningReplayTurn(messages[start:end])...)
		start = end
	}

	for i := 1; i < len(messages); i++ {
		if messages[i].Role == "user" {
			flush(i)
		}
	}
	flush(len(messages))

	return out
}

func filterReasoningReplayTurn(messages []Message) []Message {
	hasToolInteraction := false
	for _, msg := range messages {
		if msg.Role == "tool" || (msg.Role == "assistant" && len(msg.ToolCalls) > 0) {
			hasToolInteraction = true
			break
		}
	}

	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if messageutil.IsTransientAssistantThoughtMessage(msg) {
			continue
		}

		cloned := msg
		// DeepSeek 和 MiMo 仅要求参与工具交互轮次的对话
		// 回放 reasoning_content。对于两条用户消息之间的普通
		// 助手轮次，推理轨迹在回放时会被忽略，
		// 因此我们在这里将其剥离。
		if cloned.Role == "assistant" && strings.TrimSpace(cloned.ReasoningContent) != "" && !hasToolInteraction {
			cloned.ReasoningContent = ""
		}
		if assistantMessageEmpty(cloned) {
			continue
		}
		out = append(out, cloned)
	}

	return out
}

func stripReasoningMessages(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if messageutil.IsTransientAssistantThoughtMessage(msg) {
			continue
		}

		cloned := msg
		cloned.ReasoningContent = ""
		if assistantMessageEmpty(cloned) {
			continue
		}
		out = append(out, cloned)
	}
	return out
}

func assistantMessageEmpty(msg Message) bool {
	return msg.Role == "assistant" &&
		strings.TrimSpace(msg.Content) == "" &&
		strings.TrimSpace(msg.ReasoningContent) == "" &&
		len(msg.ToolCalls) == 0 &&
		len(msg.Media) == 0 &&
		len(msg.Attachments) == 0 &&
		strings.TrimSpace(msg.ToolCallID) == ""
}

func (p *Provider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	if p.apiBase == "" {
		return nil, fmt.Errorf("API base not configured")
	}

	requestBody := p.buildRequestBody(messages, tools, model, options)

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.apiBase+"/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	p.applyCustomHeaders(req)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, common.HandleErrorResponse(resp, p.apiBase)
	}

	return common.ReadAndParseResponse(resp, p.apiBase)
}

// ChatStream 通过 OpenAI 兼容的 SSE 实现流式输出（stream: true）。
// onChunk 在每个文本增量时接收目前已累积的文本。
func (p *Provider) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(accumulated string),
) (*LLMResponse, error) {
	return p.ChatStreamEvents(
		ctx,
		messages,
		tools,
		model,
		options,
		func(chunk StreamChunk) {
			if onChunk != nil && strings.TrimSpace(chunk.Content) != "" {
				onChunk(chunk.Content)
			}
		},
	)
}

func (p *Provider) ChatStreamEvents(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(StreamChunk),
) (*LLMResponse, error) {
	if p.apiBase == "" {
		return nil, fmt.Errorf("API base not configured")
	}

	requestBody := p.buildRequestBody(messages, tools, model, options)
	requestBody["stream"] = true

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", p.apiBase+"/chat/completions", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if p.userAgent != "" {
		req.Header.Set("User-Agent", p.userAgent)
	}
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	p.applyCustomHeaders(req)

	// 流式请求使用不带 Timeout 的客户端 —— http.Client.Timeout 覆盖
	// 整个请求生命周期（包括读取响应体），会中断长时间流。
	// 上下文取消仍可作为兜底保护。
	streamClient := &http.Client{Transport: p.httpClient.Transport}
	resp, err := streamClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, common.HandleErrorResponse(resp, p.apiBase)
	}

	return parseStreamResponse(ctx, withStreamingReadIdleTimeout(resp.Body, defaultStreamingReadIdleTimeout), onChunk)
}

func withStreamingReadIdleTimeout(body io.ReadCloser, timeout time.Duration) io.ReadCloser {
	if body == nil || timeout <= 0 {
		return body
	}
	return &streamingReadIdleTimeoutBody{
		body:    body,
		timeout: timeout,
	}
}

type streamingReadIdleTimeoutBody struct {
	body    io.ReadCloser
	timeout time.Duration
}

func (b *streamingReadIdleTimeoutBody) Read(p []byte) (int, error) {
	timedOut := make(chan struct{})
	timer := time.AfterFunc(b.timeout, func() {
		close(timedOut)
		_ = b.body.Close()
	})
	n, err := b.body.Read(p)
	if !timer.Stop() {
		<-timedOut
		return n, fmt.Errorf("stream idle timeout after %s", b.timeout)
	}
	return n, err
}

func (b *streamingReadIdleTimeoutBody) Close() error {
	return b.body.Close()
}

// parseStreamResponse 解析 OpenAI 兼容的 SSE 流。
func parseStreamResponse(
	ctx context.Context,
	reader io.Reader,
	onChunk func(StreamChunk),
) (*LLMResponse, error) {
	var textContent strings.Builder
	var reasoningContent strings.Builder
	var reasoning strings.Builder
	var reasoningDetails []ReasoningDetail
	var finishReason string
	var usage *UsageInfo
	var responseModel string

	// 工具调用组装：OpenAI 以增量 delta 形式流式返回工具调用
	type toolAccum struct {
		id       string
		name     string
		argsJSON strings.Builder
	}
	activeTools := map[int]*toolAccum{}

	processEvent := func(data string) error {
		if strings.TrimSpace(data) == "" {
			return nil
		}
		if strings.TrimSpace(data) == "[DONE]" {
			return io.EOF
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string            `json:"content"`
					ReasoningContent string            `json:"reasoning_content"`
					Reasoning        string            `json:"reasoning"`
					ReasoningDetails []ReasoningDetail `json:"reasoning_details"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function *struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *common.RawUsageInfo `json:"usage"`
			Model string               `json:"model"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("failed to decode stream event: %w", err)
		}

		if chunk.Usage != nil {
			usage = chunk.Usage.ToUsageInfo()
		}
		if chunk.Model != "" {
			responseModel = chunk.Model
		}

		if len(chunk.Choices) == 0 {
			return nil
		}

		choice := chunk.Choices[0]

		if choice.Delta.ReasoningContent != "" {
			reasoningContent.WriteString(choice.Delta.ReasoningContent)
			if onChunk != nil {
				onChunk(StreamChunk{ReasoningContent: reasoningContent.String(), Model: responseModel})
			}
		}
		if choice.Delta.Reasoning != "" {
			reasoning.WriteString(choice.Delta.Reasoning)
			if onChunk != nil {
				onChunk(StreamChunk{ReasoningContent: reasoning.String(), Model: responseModel})
			}
		}
		if len(choice.Delta.ReasoningDetails) > 0 {
			reasoningDetails = append(reasoningDetails, choice.Delta.ReasoningDetails...)
		}
		// 在推理内容之后累积文本内容，以便 provider 在同一事件中
		// 同时发送两个字段时，UI 可以先显示思考过程。
		if choice.Delta.Content != "" {
			textContent.WriteString(choice.Delta.Content)
			if onChunk != nil {
				onChunk(StreamChunk{Content: textContent.String(), Model: responseModel})
			}
		}

		// 累积工具调用增量
		for _, tc := range choice.Delta.ToolCalls {
			acc, ok := activeTools[tc.Index]
			if !ok {
				acc = &toolAccum{}
				activeTools[tc.Index] = acc
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.argsJSON.WriteString(tc.Function.Arguments)
				}
			}
		}

		if choice.FinishReason != nil {
			finishReason = *choice.FinishReason
		}

		return nil
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 初始 1MB，最大 10MB
	var eventData strings.Builder
	for scanner.Scan() {
		// 在各数据块之间检查上下文是否已取消
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line := scanner.Text()
		if line == "" {
			err := processEvent(eventData.String())
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			eventData.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimPrefix(line, "data:")
		data = strings.TrimPrefix(data, " ")
		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}
		eventData.WriteString(data)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("streaming read error: %w", err)
	}
	if eventData.Len() > 0 {
		err := processEvent(eventData.String())
		if err != nil && err != io.EOF {
			return nil, err
		}
	}

	// 从累积的增量中组装工具调用
	var toolCalls []ToolCall
	for i := 0; i < len(activeTools); i++ {
		acc, ok := activeTools[i]
		if !ok {
			continue
		}
		args := make(map[string]any)
		raw := acc.argsJSON.String()
		if raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				logger.WarnCF("openai_compat", "stream: failed to decode tool call arguments", map[string]any{
					"tool":  acc.name,
					"error": err.Error(),
				})
				args["raw"] = raw
			}
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        acc.id,
			Name:      acc.name,
			Arguments: args,
		})
	}

	if finishReason == "" {
		finishReason = "stop"
	}

	return &LLMResponse{
		Content:          textContent.String(),
		ReasoningContent: reasoningContent.String(),
		Reasoning:        reasoning.String(),
		ReasoningDetails: reasoningDetails,
		ToolCalls:        toolCalls,
		FinishReason:     finishReason,
		Usage:            usage,
		Model:            responseModel,
	}, nil
}

func normalizeModel(model, apiBase string) string {
	before, after, ok := strings.Cut(model, "/")
	if !ok {
		return model
	}

	if strings.Contains(strings.ToLower(apiBase), "openrouter.ai") {
		return model
	}

	prefix := strings.ToLower(before)
	if _, ok := stripModelPrefixProviders[prefix]; ok {
		return after
	}

	return model
}

func buildToolsList(tools []ToolDefinition, nativeSearch bool) []any {
	result := make([]any, 0, len(tools)+1)
	for _, t := range tools {
		if nativeSearch && strings.EqualFold(t.Function.Name, "web_search") {
			continue
		}
		result = append(result, t)
	}
	if nativeSearch {
		result = append(result, map[string]any{"type": "web_search_preview"})
	}
	return result
}

func (p *Provider) SupportsNativeSearch() bool {
	return isNativeSearchHost(p.apiBase)
}

// isNativeOpenAIOrAzureEndpoint 报告给定 API base 是否指向
// OpenAI 自有 API 或 Azure OpenAI 部署。
func isNativeOpenAIOrAzureEndpoint(apiBase string) bool {
	u, err := url.Parse(apiBase)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "api.openai.com" || strings.HasSuffix(host, ".openai.azure.com")
}

func isNativeSearchHost(apiBase string) bool {
	return isNativeOpenAIOrAzureEndpoint(apiBase)
}

// supportsPromptCacheKey 报告给定 API base 是否已知支持
// prompt_cache_key 请求字段。目前仅 OpenAI 自有 API 和 Azure OpenAI 支持。
// 其他所有 OpenAI 兼容 provider（Mistral、Gemini、DeepSeek、Groq 等）
// 都会以 422 错误拒绝未知字段。
func supportsPromptCacheKey(apiBase string) bool {
	return isNativeOpenAIOrAzureEndpoint(apiBase)
}
