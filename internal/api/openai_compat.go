/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/llm"
)

var oaiLog = logf.Log.WithName("openai-compat")

// OpenAICompatHandler implements OpenAI-compatible /v1/chat/completions and /v1/models endpoints.
// This allows tools like OpenCode to use Mercan as a custom provider.
type OpenAICompatHandler struct {
	client         client.Client
	watchNamespace string
	config         ChatConfig
}

// NewOpenAICompatHandler creates an OpenAI-compatible API handler.
func NewOpenAICompatHandler(c client.Client, watchNamespace string, config ChatConfig) *OpenAICompatHandler {
	return &OpenAICompatHandler{
		client:         c,
		watchNamespace: watchNamespace,
		config:         config,
	}
}

// --- OpenAI API types ---

// OAIRequest is the OpenAI chat completion request format.
type OAIRequest struct {
	Model            string         `json:"model"`
	Messages         []OAIMessage   `json:"messages"`
	Tools            []OAITool      `json:"tools,omitempty"`
	Temperature      *float64       `json:"temperature,omitempty"`
	MaxTokens        *int           `json:"max_tokens,omitempty"`
	MaxCompTokens    *int           `json:"max_completion_tokens,omitempty"`
	Stream           bool           `json:"stream,omitempty"`
	StreamOptions    *StreamOptions `json:"stream_options,omitempty"`
	Stop             any            `json:"stop,omitempty"`
	TopP             *float64       `json:"top_p,omitempty"`
	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	N                *int           `json:"n,omitempty"`
	User             string         `json:"user,omitempty"`
}

// StreamOptions holds stream-specific options.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// OAIMessage represents an OpenAI chat message.
type OAIMessage struct {
	Role       string        `json:"role,omitempty"`
	Content    any           `json:"content"` // string or []ContentPart
	Name       string        `json:"name,omitempty"`
	ToolCalls  []OAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Refusal    *string       `json:"refusal,omitempty"`
}

// OAITool is an OpenAI tool definition.
type OAITool struct {
	Type     string         `json:"type"` // "function"
	Function OAIFunctionDef `json:"function"`
}

// OAIFunctionDef is an OpenAI function definition.
type OAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// OAIToolCall is an OpenAI tool call.
type OAIToolCall struct {
	Index    *int            `json:"index,omitempty"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"` // "function"
	Function OAIFunctionCall `json:"function"`
}

// OAIFunctionCall is a function call within a tool call.
type OAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OAIResponse is the OpenAI chat completion response.
type OAIResponse struct {
	ID                string      `json:"id"`
	Object            string      `json:"object"`
	Created           int64       `json:"created"`
	Model             string      `json:"model"`
	Choices           []OAIChoice `json:"choices"`
	Usage             *OAIUsage   `json:"usage,omitempty"`
	SystemFingerprint string      `json:"system_fingerprint,omitempty"`
}

// OAIChoice is a choice in the response.
type OAIChoice struct {
	Index        int         `json:"index"`
	Message      *OAIMessage `json:"message,omitempty"`
	Delta        *OAIMessage `json:"delta,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

// OAIUsage contains token usage statistics.
type OAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OAIModel is the model object for /v1/models.
type OAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// OAIModelList is the response for GET /v1/models.
type OAIModelList struct {
	Object string     `json:"object"`
	Data   []OAIModel `json:"data"`
}

// OAIError is the OpenAI error response format.
type OAIError struct {
	Error OAIErrorDetail `json:"error"`
}

// OAIErrorDetail is the error detail.
type OAIErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// HandleChatCompletions handles POST /v1/chat/completions.
func (h *OpenAICompatHandler) HandleChatCompletions(c fiber.Ctx) error {
	var req OAIRequest
	if err := c.Bind().JSON(&req); err != nil {
		return c.Status(400).JSON(OAIError{Error: OAIErrorDetail{
			Message: "invalid request body: " + err.Error(),
			Type:    "invalid_request_error",
		}})
	}

	if len(req.Messages) == 0 {
		return c.Status(400).JSON(OAIError{Error: OAIErrorDetail{
			Message: "messages is required and must be non-empty",
			Type:    "invalid_request_error",
		}})
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.config.MaxDuration)
	defer cancel()

	namespace := h.watchNamespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	// Resolve provider and model from the request model field.
	// Supports "provider/model" format (e.g., "anthropic/claude-sonnet-4") or plain model name.
	provider, model, err := h.resolveProviderFromModel(ctx, req.Model, namespace)
	if err != nil {
		oaiLog.Error(err, "failed to resolve provider", "model", req.Model)
		return c.Status(400).JSON(OAIError{Error: OAIErrorDetail{
			Message: "failed to resolve provider: " + err.Error(),
			Type:    "invalid_request_error",
		}})
	}

	// Convert OpenAI messages to internal format
	messages, systemPrompt := convertOAIMessages(req.Messages)

	// Convert OpenAI tools to internal format
	tools := convertOAITools(req.Tools)

	// Build completion request
	compReq := &llm.CompletionRequest{
		Model:        model,
		Messages:     messages,
		SystemPrompt: systemPrompt,
		Tools:        tools,
	}

	if req.Temperature != nil {
		compReq.Temperature = *req.Temperature
	}

	maxTokens := 0
	if req.MaxCompTokens != nil {
		maxTokens = *req.MaxCompTokens
	} else if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	if maxTokens > 0 {
		compReq.MaxTokens = maxTokens
	}

	// Convert stop sequences
	if req.Stop != nil {
		switch v := req.Stop.(type) {
		case string:
			compReq.StopSequences = []string{v}
		case []any:
			for _, s := range v {
				if str, ok := s.(string); ok {
					compReq.StopSequences = append(compReq.StopSequences, str)
				}
			}
		}
	}

	completionID := fmt.Sprintf("chatcmpl-%s", generateChatID())
	now := time.Now().Unix()

	if req.Stream {
		return h.handleStreamingCompletion(c, ctx, provider, compReq, completionID, model, now, req.StreamOptions)
	}

	return h.handleNonStreamingCompletion(c, ctx, provider, compReq, completionID, model, now)
}

// handleNonStreamingCompletion handles a non-streaming chat completion request.
func (h *OpenAICompatHandler) handleNonStreamingCompletion(
	c fiber.Ctx,
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
	completionID, model string,
	created int64,
) error {
	resp, err := provider.Complete(ctx, req)
	if err != nil {
		oaiLog.Error(err, "completion failed")
		return c.Status(500).JSON(OAIError{Error: OAIErrorDetail{
			Message: "completion failed: " + err.Error(),
			Type:    "server_error",
		}})
	}

	finishReason := mapFinishReason(resp.StopReason)

	msg := &OAIMessage{
		Role:    "assistant",
		Content: resp.Content,
	}

	// Convert tool calls
	if len(resp.ToolCalls) > 0 {
		msg.ToolCalls = make([]OAIToolCall, 0, len(resp.ToolCalls))
		for i, tc := range resp.ToolCalls {
			idx := i
			msg.ToolCalls = append(msg.ToolCalls, OAIToolCall{
				Index: &idx,
				ID:    tc.ID,
				Type:  "function",
				Function: OAIFunctionCall{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
			})
		}
		if finishReason == "stop" {
			finishReason = "tool_calls"
		}
	}

	return c.JSON(OAIResponse{
		ID:      completionID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []OAIChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: &finishReason,
		}},
		Usage: &OAIUsage{
			PromptTokens:     resp.InputTokens,
			CompletionTokens: resp.OutputTokens,
			TotalTokens:      resp.InputTokens + resp.OutputTokens,
		},
	})
}

// handleStreamingCompletion handles a streaming chat completion request.
func (h *OpenAICompatHandler) handleStreamingCompletion(
	c fiber.Ctx,
	ctx context.Context,
	provider llm.Provider,
	req *llm.CompletionRequest,
	completionID, model string,
	created int64,
	streamOpts *StreamOptions,
) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	// Capture for closure
	capturedProvider := provider
	capturedReq := req

	return c.SendStreamWriter(func(w *bufio.Writer) {
		streamCtx, streamCancel := context.WithTimeout(context.Background(), h.config.MaxDuration)
		defer streamCancel()

		streamCh, err := capturedProvider.Stream(streamCtx, capturedReq)
		if err != nil {
			// Try non-streaming fallback via Complete
			resp, completeErr := capturedProvider.Complete(streamCtx, capturedReq)
			if completeErr != nil {
				errChunk := OAIResponse{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []OAIChoice{{
						Index: 0,
						Delta: &OAIMessage{Role: "assistant", Content: "Error: " + completeErr.Error()},
					}},
				}
				writeStreamChunk(w, errChunk)
				writeStreamDone(w)
				return
			}

			// Send role chunk
			roleChunk := OAIResponse{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []OAIChoice{{
					Index: 0,
					Delta: &OAIMessage{Role: "assistant"},
				}},
			}
			writeStreamChunk(w, roleChunk)

			// Send content
			if resp.Content != "" {
				contentChunk := OAIResponse{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []OAIChoice{{
						Index: 0,
						Delta: &OAIMessage{Content: resp.Content},
					}},
				}
				writeStreamChunk(w, contentChunk)
			}

			// Send tool calls if any
			for i, tc := range resp.ToolCalls {
				idx := i
				tcChunk := OAIResponse{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []OAIChoice{{
						Index: 0,
						Delta: &OAIMessage{
							ToolCalls: []OAIToolCall{{
								Index: &idx,
								ID:    tc.ID,
								Type:  "function",
								Function: OAIFunctionCall{
									Name:      tc.Name,
									Arguments: string(tc.Arguments),
								},
							}},
						},
					}},
				}
				writeStreamChunk(w, tcChunk)
			}

			// Send finish
			finishReason := mapFinishReason(resp.StopReason)
			if len(resp.ToolCalls) > 0 {
				finishReason = "tool_calls"
			}
			finishChunk := OAIResponse{
				ID:      completionID,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   model,
				Choices: []OAIChoice{{
					Index:        0,
					Delta:        &OAIMessage{},
					FinishReason: &finishReason,
				}},
			}
			writeStreamChunk(w, finishChunk)

			// Send usage if requested
			if streamOpts != nil && streamOpts.IncludeUsage {
				usageChunk := OAIResponse{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []OAIChoice{},
					Usage: &OAIUsage{
						PromptTokens:     resp.InputTokens,
						CompletionTokens: resp.OutputTokens,
						TotalTokens:      resp.InputTokens + resp.OutputTokens,
					},
				}
				writeStreamChunk(w, usageChunk)
			}

			writeStreamDone(w)
			return
		}

		// Send initial role chunk
		roleChunk := OAIResponse{
			ID:      completionID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []OAIChoice{{
				Index: 0,
				Delta: &OAIMessage{Role: "assistant"},
			}},
		}
		writeStreamChunk(w, roleChunk)

		toolCallIndex := 0
		for chunk := range streamCh {
			if chunk.Error != nil {
				oaiLog.Error(chunk.Error, "stream error")
				break
			}

			if chunk.Content != "" {
				contentChunk := OAIResponse{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []OAIChoice{{
						Index: 0,
						Delta: &OAIMessage{Content: chunk.Content},
					}},
				}
				writeStreamChunk(w, contentChunk)
			}

			if chunk.ToolCall != nil {
				tc := chunk.ToolCall
				idx := toolCallIndex
				tcChunk := OAIResponse{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []OAIChoice{{
						Index: 0,
						Delta: &OAIMessage{
							ToolCalls: []OAIToolCall{{
								Index: &idx,
								ID:    tc.ID,
								Type:  "function",
								Function: OAIFunctionCall{
									Name:      tc.Name,
									Arguments: string(tc.Arguments),
								},
							}},
						},
					}},
				}
				writeStreamChunk(w, tcChunk)
				toolCallIndex++
			}

			if chunk.Done {
				finishReason := mapFinishReason(chunk.StopReason)
				finishChunk := OAIResponse{
					ID:      completionID,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   model,
					Choices: []OAIChoice{{
						Index:        0,
						Delta:        &OAIMessage{},
						FinishReason: &finishReason,
					}},
				}
				writeStreamChunk(w, finishChunk)
			}
		}

		writeStreamDone(w)
	})
}

// HandleListModels handles GET /v1/models.
func (h *OpenAICompatHandler) HandleListModels(c fiber.Ctx) error {
	ctx := c.Context()

	namespace := h.watchNamespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	// List Provider CRDs to build a model list
	providerList := &corev1alpha1.ProviderList{}
	listOpts := []client.ListOption{}
	if namespace != "" {
		listOpts = append(listOpts, client.InNamespace(namespace))
	}
	if err := h.client.List(ctx, providerList, listOpts...); err != nil {
		oaiLog.Error(err, "failed to list providers")
		return c.Status(500).JSON(OAIError{Error: OAIErrorDetail{
			Message: "failed to list providers",
			Type:    "server_error",
		}})
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
			// Also add the plain model name
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

// resolveProviderFromModel resolves the LLM provider from the model string.
// Supports "provider-name/model-name" format or plain "model-name" (uses default provider).
func (h *OpenAICompatHandler) resolveProviderFromModel(ctx context.Context, modelStr, namespace string) (llm.Provider, string, error) {
	var providerName, model string

	// Check for "provider/model" format
	if idx := strings.Index(modelStr, "/"); idx > 0 {
		providerName = modelStr[:idx]
		model = modelStr[idx+1:]
	} else {
		model = modelStr
	}

	// Try to resolve provider CRD
	var providerCRD *corev1alpha1.Provider

	if providerName != "" {
		p := &corev1alpha1.Provider{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: providerName, Namespace: namespace}, p); err == nil {
			providerCRD = p
		}
	}

	// Fall back to config provider, then "default"
	if providerCRD == nil && h.config.Provider != "" {
		p := &corev1alpha1.Provider{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: h.config.Provider, Namespace: namespace}, p); err == nil {
			providerCRD = p
		}
	}

	if providerCRD == nil {
		p := &corev1alpha1.Provider{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: "default", Namespace: namespace}, p); err != nil {
			return nil, "", fmt.Errorf("no provider %q found and no 'default' Provider CRD exists", providerName)
		}
		providerCRD = p
	}

	// Resolve API key from secret
	secretName := providerCRD.Spec.SecretRef.Name
	secretKey := providerCRD.Spec.SecretRef.Key
	if secretKey == "" {
		secretKey = "api-key"
	}

	secret := &corev1.Secret{}
	if err := h.client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: providerCRD.Namespace}, secret); err != nil {
		return nil, "", fmt.Errorf("failed to get provider secret %q: %w", secretName, err)
	}
	apiKeyBytes, ok := secret.Data[secretKey]
	if !ok {
		return nil, "", fmt.Errorf("secret %q has no key %q", secretName, secretKey)
	}

	// Use provider's default model if none specified
	if model == "" {
		model = providerCRD.Spec.DefaultModel
	}
	if model == "" {
		model = h.config.Model
	}
	if model == "" {
		return nil, "", fmt.Errorf("no model specified and no default model configured")
	}

	providerConfig := llm.ProviderConfig{
		APIKey:       string(apiKeyBytes),
		BaseURL:      providerCRD.Spec.BaseURL,
		ProviderType: string(providerCRD.Spec.Type),
	}
	if providerCRD.Spec.Azure != nil {
		providerConfig.AzureAPIVersion = providerCRD.Spec.Azure.APIVersion
	}

	provider, err := llm.NewProvider(string(providerCRD.Spec.Type), providerConfig)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create LLM provider: %w", err)
	}

	return provider, model, nil
}

// convertOAIMessages converts OpenAI messages to internal llm.Message format.
// Extracts the system prompt from system messages.
func convertOAIMessages(msgs []OAIMessage) ([]llm.Message, string) {
	var systemPrompt string
	var messages []llm.Message

	for _, m := range msgs {
		if m.Role == "system" {
			systemPrompt = extractContent(m.Content)
			continue
		}

		msg := llm.Message{
			Role:       m.Role,
			Content:    extractContent(m.Content),
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}

		// Convert tool calls
		if len(m.ToolCalls) > 0 {
			msg.ToolCalls = make([]llm.ToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, llm.ToolCall{
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: json.RawMessage(tc.Function.Arguments),
				})
			}
		}

		messages = append(messages, msg)
	}

	return messages, systemPrompt
}

// extractContent extracts string content from an OAI message content field.
// Content can be a string or an array of content parts.
func extractContent(content any) string {
	if content == nil {
		return ""
	}

	switch v := content.(type) {
	case string:
		return v
	case []any:
		// Array of content parts — extract text parts
		var parts []string
		for _, part := range v {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		// Try JSON marshal/unmarshal for other types
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		var s string
		if json.Unmarshal(b, &s) == nil {
			return s
		}
		return string(b)
	}
}

// convertOAITools converts OpenAI tool definitions to internal llm.Tool format.
func convertOAITools(tools []OAITool) []llm.Tool {
	if len(tools) == 0 {
		return nil
	}

	result := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		result = append(result, llm.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return result
}

// mapFinishReason maps internal stop reasons to OpenAI finish_reason values.
func mapFinishReason(reason string) string {
	switch strings.ToLower(reason) {
	case "end_turn", "stop", "":
		return "stop"
	case "tool_use", "tool_calls":
		return "tool_calls"
	case "max_tokens", "length":
		return "length"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}

// writeStreamChunk writes a single SSE chunk in OpenAI streaming format.
func writeStreamChunk(w *bufio.Writer, chunk OAIResponse) {
	data, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.Flush()
}

// writeStreamDone writes the final [DONE] marker for OpenAI streaming.
func writeStreamDone(w *bufio.Writer) {
	fmt.Fprintf(w, "data: [DONE]\n\n")
	w.Flush()
}
