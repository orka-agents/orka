/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/tools"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// builtinProxyTools are the tool names injected by the proxy for server-side execution.
var builtinProxyTools = []string{"web_search", "code_exec", "file_read", "file_write", "web_fetch"}

// injectOrkaTools appends Orka's built-in tools and namespace Tool CRDs to the completion request.
// Client-provided tools (if any) are preserved.
func injectOrkaTools(ctx context.Context, k8sClient client.Client, req *llm.CompletionRequest, namespace string) {
	builtinTools := tools.DefaultRegistry.ToLLMTools(builtinProxyTools)
	req.Tools = append(req.Tools, builtinTools...)

	// Load Tool CRDs from namespace for custom HTTP tools
	var toolList corev1alpha1.ToolList
	if err := k8sClient.List(ctx, &toolList, client.InNamespace(namespace)); err == nil {
		for _, t := range toolList.Items {
			if t.Spec.Parameters != nil {
				req.Tools = append(req.Tools, llm.Tool{
					Name:        t.Name,
					Description: t.Spec.Description,
					Parameters:  t.Spec.Parameters.Raw,
				})
			}
		}
	}
}

// executeToolCall executes a single tool call via the default registry with a timeout.
func executeToolCall(ctx context.Context, tc llm.ToolCall, timeout time.Duration) string {
	toolCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result, err := tools.DefaultRegistry.Execute(toolCtx, tc.Name, tc.Arguments)
	if err != nil {
		errResult, _ := json.Marshal(map[string]any{"success": false, "error": err.Error()})
		return string(errResult)
	}
	return result
}

// runNonStreamingToolLoop runs the agentic tool loop using non-streaming Complete() calls.
// It loops until the LLM produces a response with no tool calls, or limits are reached.
// Returns the final CompletionResponse and all intermediate content blocks.
func runNonStreamingToolLoop(
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
	model string,
	config ChatConfig,
) (*llm.CompletionResponse, error) {
	repetitionTracker := make(map[string]int)
	messages := make([]llm.Message, len(req.Messages))
	copy(messages, req.Messages)

	for iteration := 0; ; iteration++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			return &llm.CompletionResponse{
				Content:    "Request timed out during tool execution.",
				StopReason: "end_turn",
			}, nil
		default:
		}

		// Check iteration limit — do one final call without tools
		if iteration >= config.MaxIterations {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: You have reached the maximum number of iterations. Please provide a final summary of what you accomplished.]",
			})
			resp, err := provider.Complete(ctx, &llm.CompletionRequest{
				Model:        model,
				Messages:     messages,
				SystemPrompt: req.SystemPrompt,
				MaxTokens:    req.MaxTokens,
				Temperature:  req.Temperature,
			})
			if err != nil {
				return &llm.CompletionResponse{
					Content:    "Reached iteration limit.",
					StopReason: "end_turn",
				}, nil
			}
			return resp, nil
		}

		// Progress check every 5 iterations
		if iteration > 0 && iteration%5 == 0 {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: Progress check — summarize what you've done so far and what remains.]",
			})
		}

		// Truncate conversation if it exceeds the session size budget
		if config.MaxSessionSize > 0 {
			tokenBudget := config.MaxSessionSize / 4
			messages = llm.TruncateMessages(messages, tokenBudget)
		}

		// Call LLM with tools
		compReq := &llm.CompletionRequest{
			Model:        model,
			Messages:     messages,
			SystemPrompt: req.SystemPrompt,
			Tools:        req.Tools,
			MaxTokens:    req.MaxTokens,
			Temperature:  req.Temperature,
		}

		resp, err := provider.Complete(ctx, compReq)
		if err != nil && llm.IsContextTooLongErr(err) {
			tokenEstimate := 0
			for _, m := range messages {
				tokenEstimate += len(m.Content) / 4
			}
			messages = llm.TruncateMessages(messages, tokenEstimate/2)
			compReq.Messages = messages
			resp, err = provider.Complete(ctx, compReq)
		}
		if err != nil {
			return nil, fmt.Errorf("LLM completion failed: %w", err)
		}

		// No tool calls → final response
		if len(resp.ToolCalls) == 0 {
			return resp, nil
		}

		anthropicLog.Info("tool loop iteration",
			"iteration", iteration,
			"tool_calls", len(resp.ToolCalls),
		)

		// Append assistant message with tool calls
		messages = append(messages, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool and append results
		var repetitionWarning string
		for _, tc := range resp.ToolCalls {
			// Check repetition
			argsHash := hashArgs(tc.Name, tc.Arguments)
			repetitionTracker[argsHash]++
			if repetitionTracker[argsHash] >= 3 {
				repetitionWarning = fmt.Sprintf("[System: Warning — you have called %s with the same arguments %d times. Try a different approach.]", tc.Name, repetitionTracker[argsHash])
				iteration += 5
			}

			result := executeToolCall(ctx, tc, config.ToolTimeout)

			messages = append(messages, llm.Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    result,
			})
		}

		// Append repetition warning if triggered
		if repetitionWarning != "" {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: repetitionWarning,
			})
		}
	}
}
