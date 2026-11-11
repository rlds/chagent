// Chagent - 超轻量级个人 AI agent
// 灵感来自并基于 nanobot：https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 Chagent contributors

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/rlds/chagent/pkg/logger"
	"github.com/rlds/chagent/pkg/providers"
	"github.com/rlds/chagent/pkg/utils"
)

// ToolLoopConfig 配置工具执行循环。
type ToolLoopConfig struct {
	Provider      providers.LLMProvider
	Model         string
	Tools         *ToolRegistry
	MaxIterations int
	LLMOptions    map[string]any

	// MediaResolver 在每次 LLM 调用前解析消息中的 media:// 引用。
	// 该字段是可选的，主要用于子代理的旧版回退执行，
	// 使子代理能够复用与主循环相同的多模态媒体处理。
	MediaResolver func(messages []providers.Message) []providers.Message
}

// ToolLoopResult 包含运行工具循环得到的结果。
type ToolLoopResult struct {
	Content    string
	Iterations int
}

// RunToolLoop 执行 LLM + 工具调用的迭代循环。
// 这是核心的代理逻辑，主代理和子代理都可以复用。
func RunToolLoop(
	ctx context.Context,
	config ToolLoopConfig,
	messages []providers.Message,
	channel, chatID string,
) (*ToolLoopResult, error) {
	iteration := 0
	var finalContent string

	for iteration < config.MaxIterations {
		iteration++

		logger.DebugCF("toolloop", "LLM iteration",
			map[string]any{
				"iteration": iteration,
				"max":       config.MaxIterations,
			})

		// 1. 构建工具定义
		var providerToolDefs []providers.ToolDefinition
		if config.Tools != nil {
			providerToolDefs = config.Tools.ToProviderDefs()
		}

		// 2. 设置默认的 LLM 选项
		llmOpts := config.LLMOptions
		if llmOpts == nil {
			llmOpts = map[string]any{}
		}

		// 3. 解析 media:// 引用并调用 LLM。
		// 像 load_image 这样的工具会在其结果消息中产生 media:// 引用。
		// 若缺少这一步，LLM 收到的将是原始的 "media://uuid" 字符串，
		// 而不是 base64 编码的图像 data URL。
		//
		// 我们单独构造 callMessages 切片，以便：
		//   (a) 解析器的输出仅用于这次 LLM 调用，
		//   (b) 原始 messages 切片保留未解析的引用供后续迭代使用 ——
		//       解析器虽然是幂等的，但基于原始切片操作可避免重复编码问题。
		//
		// 在第 1 次迭代时，初始用户消息通常不含 media:// 引用
		// （它们来自纯文本），因此这一步实际上没有影响；
		// 从第 2 次迭代开始，工具结果可能包含媒体引用，它才真正起作用。
		callMessages := messages
		if config.MediaResolver != nil && iteration > 1 {
			callMessages = config.MediaResolver(messages)
		}
		response, err := config.Provider.Chat(ctx, callMessages, providerToolDefs, config.Model, llmOpts)
		if err != nil {
			logger.ErrorCF("toolloop", "LLM call failed",
				map[string]any{
					"iteration": iteration,
					"error":     err.Error(),
				})
			return nil, fmt.Errorf("LLM call failed: %w", err)
		}

		// 4. 若没有工具调用，则流程结束
		if len(response.ToolCalls) == 0 {
			finalContent = response.Content
			logger.InfoCF("toolloop", "LLM response without tool calls (direct answer)",
				map[string]any{
					"iteration":     iteration,
					"content_chars": len(finalContent),
				})
			break
		}

		normalizedToolCalls := make([]providers.ToolCall, 0, len(response.ToolCalls))
		for _, tc := range response.ToolCalls {
			normalizedToolCalls = append(normalizedToolCalls, providers.NormalizeToolCall(tc))
		}

		// 5. 记录工具调用
		toolNames := make([]string, 0, len(normalizedToolCalls))
		for _, tc := range normalizedToolCalls {
			toolNames = append(toolNames, tc.Name)
		}
		logger.InfoCF("toolloop", "LLM requested tool calls",
			map[string]any{
				"tools":     toolNames,
				"count":     len(normalizedToolCalls),
				"iteration": iteration,
			})

		// 6. 构建携带工具调用的 assistant 消息
		assistantMsg := providers.Message{
			Role:    "assistant",
			Content: response.Content,
		}
		for _, tc := range normalizedToolCalls {
			argumentsJSON, err := json.Marshal(tc.Arguments)
			if err != nil {
				logger.Warnf("toolloop: failed to marshal tool call arguments for %s: %v", tc.Name, err)
				argumentsJSON = []byte("{}")
			}
			assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, providers.ToolCall{
				ID:        tc.ID,
				Type:      "function",
				Name:      tc.Name,
				Arguments: tc.Arguments,
				Function: &providers.FunctionCall{
					Name:      tc.Name,
					Arguments: string(argumentsJSON),
				},
			})
		}
		messages = append(messages, assistantMsg)

		// 7. 并行执行工具调用
		type indexedResult struct {
			result *ToolResult
			tc     providers.ToolCall
		}

		results := make([]indexedResult, len(normalizedToolCalls))
		var wg sync.WaitGroup

		for i, tc := range normalizedToolCalls {
			results[i].tc = tc

			wg.Add(1)
			go func(idx int, tc providers.ToolCall) {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						logger.ErrorCF("toolloop", "tool execution goroutine panic recovered",
							map[string]any{
								"tool":  tc.Name,
								"panic": fmt.Sprintf("%v", r),
								"stack": string(debug.Stack()),
							})
						results[idx].result = ErrorResult(fmt.Sprintf("工具 %s 内部 panic", tc.Name))
					}
				}()

				argsJSON, _ := json.Marshal(tc.Arguments)
				argsPreview := utils.Truncate(string(argsJSON), 200)
				logger.InfoCF("toolloop", fmt.Sprintf("Tool call: %s(%s)", tc.Name, argsPreview),
					map[string]any{
						"tool":      tc.Name,
						"iteration": iteration,
					})

				var toolResult *ToolResult
				if config.Tools != nil {
					toolResult = config.Tools.ExecuteWithContext(ctx, tc.Name, tc.Arguments, channel, chatID, nil)
				} else {
					toolResult = ErrorResult("没有可用工具")
				}
				results[idx].result = toolResult
			}(i, tc)
		}
		wg.Wait()

		// 按原始顺序追加结果
		for _, r := range results {
			contentForLLM := r.result.ContentForLLM()

			toolMsg := providers.Message{
				Role:       "tool",
				Content:    contentForLLM,
				ToolCallID: r.tc.ID,
			}
			if len(r.result.Media) > 0 && !r.result.ResponseHandled {
				toolMsg.Media = append(toolMsg.Media, r.result.Media...)
			}
			messages = append(messages, toolMsg)
		}
	}

	return &ToolLoopResult{
		Content:    finalContent,
		Iterations: iteration,
	}, nil
}
