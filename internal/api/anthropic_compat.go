/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/llm"
)

var anthropicLog = logf.Log.WithName("anthropic-compat")

// AnthropicCompatHandler implements Anthropic-compatible /v1/messages endpoints.
// This allows Anthropic-compatible clients to use Orka as a custom provider.
type AnthropicCompatHandler struct {
	client                    client.Client
	watchNamespace            string
	enforceNamespaceIsolation bool
	config                    ChatConfig
	resolver                  *ProviderResolver
}

// NewAnthropicCompatHandler creates an Anthropic-compatible API handler.
func NewAnthropicCompatHandler(c client.Client, watchNamespace string, enforceNamespaceIsolation bool, config ChatConfig, resolver *ProviderResolver) *AnthropicCompatHandler {
	return &AnthropicCompatHandler{
		client:                    c,
		watchNamespace:            watchNamespace,
		enforceNamespaceIsolation: enforceNamespaceIsolation,
		config:                    config,
		resolver:                  resolver,
	}
}

// AnthropicRequest represents an incoming Anthropic Messages API request.
type AnthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []AnthropicMessage `json:"messages"`
	MaxTokens     int                `json:"max_tokens"`
	System        json.RawMessage    `json:"system,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	TopK          *int               `json:"top_k,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Metadata      *AnthropicMetadata `json:"metadata,omitempty"`
	Thinking      *AnthropicThinking `json:"thinking,omitempty"`
}

// AnthropicMessage is a single message in an Anthropic conversation.
// Content is json.RawMessage because it can be a string or []ContentBlock.
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthropicContentBlock represents a typed content block in Anthropic messages.
// The Type field determines which other fields are populated.
type AnthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Source    *ImageSource    `json:"source,omitempty"`
}

// ImageSource represents a base64-encoded image source in an Anthropic content block.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// AnthropicTool defines a tool available for the model to call.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicThinking configures extended thinking.
type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// AnthropicMetadata contains request metadata.
type AnthropicMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

// AnthropicResponse is the non-streaming response from the Anthropic Messages API.
type AnthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []AnthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        AnthropicUsage          `json:"usage"`
}

// AnthropicUsage contains token usage statistics.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicStreamEvent is a single SSE event in an Anthropic streaming response.
type AnthropicStreamEvent struct {
	Type         string                 `json:"type"`
	Message      *AnthropicResponse     `json:"message,omitempty"`
	Index        int                    `json:"index,omitempty"`
	ContentBlock *AnthropicContentBlock `json:"content_block,omitempty"`
	Delta        *AnthropicDelta        `json:"delta,omitempty"`
	Usage        *AnthropicUsage        `json:"usage,omitempty"`
}

// AnthropicDelta carries incremental updates within a streaming event.
type AnthropicDelta struct {
	Type         string  `json:"type,omitempty"`
	Text         string  `json:"text,omitempty"`
	PartialJSON  string  `json:"partial_json,omitempty"`
	Thinking     string  `json:"thinking,omitempty"`
	StopReason   *string `json:"stop_reason,omitempty"`
	StopSequence *string `json:"stop_sequence,omitempty"`
}

// AnthropicError is the standard error response format for the Anthropic API.
type AnthropicError struct {
	Type  string               `json:"type"`
	Error AnthropicErrorDetail `json:"error"`
}

// AnthropicErrorDetail contains the error type and message.
type AnthropicErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// parseAnthropicContent handles both string and []ContentBlock formats for message content.
func parseAnthropicContent(raw json.RawMessage) ([]AnthropicContentBlock, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []AnthropicContentBlock{
			{Type: "text", Text: s},
		}, nil
	}

	// Try []ContentBlock
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("content must be a string or array of content blocks: %w", err)
	}
	return blocks, nil
}

// parseAnthropicSystem handles both string and []ContentBlock formats for the system field.
func parseAnthropicSystem(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}

	// Try string first
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Try []ContentBlock and concatenate text blocks
	var blocks []AnthropicContentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", fmt.Errorf("system must be a string or array of content blocks: %w", err)
	}

	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), nil
}

// HandleMessages handles POST /anthropic/v1/messages.
func (h *AnthropicCompatHandler) HandleMessages(c fiber.Ctx) error {
	start := time.Now()

	var req AnthropicRequest
	if err := c.Bind().JSON(&req); err != nil {
		return anthropicError(c, 400, "invalid_request_error", "invalid request body: "+err.Error())
	}

	if req.Model == "" {
		return anthropicError(c, 400, "invalid_request_error", "model is required")
	}
	if len(req.Messages) == 0 {
		return anthropicError(c, 400, "invalid_request_error", "messages is required and must be non-empty")
	}
	if req.MaxTokens <= 0 {
		return anthropicError(c, 400, "invalid_request_error", "max_tokens is required and must be greater than 0")
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.config.MaxDuration)
	defer cancel()

	namespace := GetEffectiveNamespace(c, "")
	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}
	if h.enforceNamespaceIsolation {
		ui := GetUserInfo(c)
		if ui != nil && ui.Namespace != "" && namespace != ui.Namespace {
			return anthropicError(c, 403, "permission_error", fmt.Sprintf("namespace %q not allowed", namespace))
		}
	}

	provider, model, err := h.resolver.Resolve(ctx, ResolveOpts{
		ModelStr:     req.Model,
		Namespace:    namespace,
		RequireModel: true,
	})
	if err != nil {
		anthropicLog.Error(err, "failed to resolve provider", "model", req.Model)
		return anthropicError(c, 400, "invalid_request_error", "failed to resolve provider: "+err.Error())
	}

	messages, err := convertAnthropicMessages(req.Messages)
	if err != nil {
		return anthropicError(c, 400, "invalid_request_error", "failed to convert messages: "+err.Error())
	}

	tools := convertAnthropicTools(req.Tools)

	systemPrompt, err := parseAnthropicSystem(req.System)
	if err != nil {
		return anthropicError(c, 400, "invalid_request_error", "failed to parse system prompt: "+err.Error())
	}

	compReq := &llm.CompletionRequest{
		Model:         model,
		Messages:      messages,
		SystemPrompt:  systemPrompt,
		MaxTokens:     req.MaxTokens,
		Tools:         tools,
		StopSequences: req.StopSequences,
	}
	if req.Temperature != nil {
		compReq.Temperature = *req.Temperature
	}

	// Only inject Orka tools and run the server-side agentic loop when explicitly opted in.
	// Without this header the endpoint behaves as a transparent proxy (like OpenAI compat).
	orkaToolsEnabled := c.Get("X-Orka-Tools") == "enabled"

	if orkaToolsEnabled {
		h.injectOrkaTools(ctx, compReq, namespace)
	}

	if req.Stream {
		if orkaToolsEnabled {
			return h.handleStreamingMessages(c, provider, compReq, model, 0)
		}
		return h.handleStreamingProxy(c, provider, compReq, model)
	}

	var resp *llm.CompletionResponse
	if orkaToolsEnabled {
		// Run the agentic tool loop (executes tools server-side until final text response)
		resp, err = h.runNonStreamingToolLoop(ctx, provider, compReq, model)
		if err != nil {
			anthropicLog.Error(err, "tool loop failed")
			return anthropicError(c, 500, "api_error", "completion failed: "+err.Error())
		}
	} else {
		// Transparent proxy: single LLM call, return response directly
		resp, err = provider.Complete(ctx, compReq)
		if err != nil {
			anthropicLog.Error(err, "completion failed")
			return anthropicError(c, 500, "api_error", "completion failed: "+err.Error())
		}
	}

	user := ""
	if ui := GetUserInfo(c); ui != nil {
		user = ui.Username
	}
	anthropicLog.Info("messages completed",
		"user", user,
		"model", model,
		"input_tokens", resp.InputTokens,
		"output_tokens", resp.OutputTokens,
		"stop_reason", resp.StopReason,
		"duration", time.Since(start).String(),
	)

	result := convertToAnthropicResponse(resp, model)
	return c.JSON(result)
}

// HandleListModels handles GET /anthropic/v1/models.
func (h *AnthropicCompatHandler) HandleListModels(c fiber.Ctx) error {
	ctx := c.Context()

	namespace := GetEffectiveNamespace(c, "")
	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	providerList := &corev1alpha1.ProviderList{}
	listOpts := []client.ListOption{}
	if namespace != "" {
		listOpts = append(listOpts, client.InNamespace(namespace))
	}
	if err := h.client.List(ctx, providerList, listOpts...); err != nil {
		anthropicLog.Error(err, "failed to list providers")
		return anthropicError(c, 500, "api_error", "failed to list providers")
	}

	now := time.Now().Unix()
	models := []OAIModel{}
	seen := make(map[string]bool)

	for _, p := range providerList.Items {
		if p.Spec.DefaultModel != "" {
			modelID := fmt.Sprintf("%s/%s", p.Name, p.Spec.DefaultModel)
			if !seen[modelID] {
				models = append(models, OAIModel{
					ID:      modelID,
					Object:  "model",
					Created: now,
					OwnedBy: string(p.Spec.Type),
				})
				seen[modelID] = true
			}
			if !seen[p.Spec.DefaultModel] {
				models = append(models, OAIModel{
					ID:      p.Spec.DefaultModel,
					Object:  "model",
					Created: now,
					OwnedBy: string(p.Spec.Type),
				})
				seen[p.Spec.DefaultModel] = true
			}
		}
	}

	return c.JSON(OAIModelList{
		Object: "list",
		Data:   models,
	})
}

// convertAnthropicMessages converts Anthropic messages to internal llm.Message format.
func convertAnthropicMessages(msgs []AnthropicMessage) ([]llm.Message, error) {
	var messages []llm.Message

	for _, m := range msgs {
		blocks, err := parseAnthropicContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("failed to parse content for role %q: %w", m.Role, err)
		}

		switch m.Role {
		case "user":
			// Separate tool_result blocks from other content
			var textParts []string
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					resultContent := ""
					if b.Content != nil {
						// tool_result content can be string or []ContentBlock
						var s string
						if json.Unmarshal(b.Content, &s) == nil {
							resultContent = s
						} else {
							var innerBlocks []AnthropicContentBlock
							if json.Unmarshal(b.Content, &innerBlocks) == nil {
								var parts []string
								for _, ib := range innerBlocks {
									if ib.Type == "text" {
										parts = append(parts, ib.Text)
									}
								}
								resultContent = strings.Join(parts, "\n")
							}
						}
					}
					messages = append(messages, llm.Message{
						Role:       "tool",
						ToolCallID: b.ToolUseID,
						Content:    resultContent,
					})
				case "text":
					textParts = append(textParts, b.Text)
				}
			}
			if len(textParts) > 0 {
				messages = append(messages, llm.Message{
					Role:    "user",
					Content: strings.Join(textParts, "\n"),
				})
			}

		case "assistant":
			msg := llm.Message{Role: "assistant"}
			var textParts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					textParts = append(textParts, b.Text)
				case "tool_use":
					msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
						ID:        b.ID,
						Name:      b.Name,
						Arguments: b.Input,
					})
				}
			}
			if len(textParts) > 0 {
				msg.Content = strings.Join(textParts, "\n")
			}
			messages = append(messages, msg)

		default:
			// Pass through unknown roles
			var textParts []string
			for _, b := range blocks {
				if b.Type == "text" {
					textParts = append(textParts, b.Text)
				}
			}
			messages = append(messages, llm.Message{
				Role:    m.Role,
				Content: strings.Join(textParts, "\n"),
			})
		}
	}

	return messages, nil
}

// convertAnthropicTools converts Anthropic tool definitions to internal llm.Tool format.
func convertAnthropicTools(tools []AnthropicTool) []llm.Tool {
	if len(tools) == 0 {
		return nil
	}

	result := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		result = append(result, llm.Tool{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	return result
}

// convertToAnthropicResponse converts an internal CompletionResponse to Anthropic format.
func convertToAnthropicResponse(resp *llm.CompletionResponse, model string) AnthropicResponse {
	id := "msg_" + uuid.New().String()

	var content []AnthropicContentBlock
	if resp.Content != "" {
		content = append(content, AnthropicContentBlock{
			Type: "text",
			Text: resp.Content,
		})
	}
	for _, tc := range resp.ToolCalls {
		content = append(content, AnthropicContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: tc.Arguments,
		})
	}

	stopReason := mapAnthropicStopReason(resp.StopReason)

	return AnthropicResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Content:    content,
		Model:      model,
		StopReason: &stopReason,
		Usage: AnthropicUsage{
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
		},
	}
}

// mapAnthropicStopReason maps internal stop reasons to Anthropic stop_reason values.
func mapAnthropicStopReason(reason string) string {
	switch strings.ToLower(reason) {
	case "stop", "end_turn", "":
		return "end_turn"
	case "tool_calls", "tool_use":
		return "tool_use"
	case "max_tokens", "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

// anthropicError returns an error in Anthropic API format.
func anthropicError(c fiber.Ctx, status int, errType, message string) error {
	return c.Status(status).JSON(AnthropicError{
		Type: "error",
		Error: AnthropicErrorDetail{
			Type:    errType,
			Message: message,
		},
	})
}
