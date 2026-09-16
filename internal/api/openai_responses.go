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
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	responsesStatusInProgress = "in_progress"
	responsesStatusIncomplete = "incomplete"
	responsesRoleAssistant    = "assistant"
	responsesRoleTool         = "tool"
	responsesFunctionToolType = "function"
	responsesServerError      = "server_error"
	responsesJSONNull         = "null"
	responsesMessage          = "message"
	responsesObject           = "response"
)

// ResponsesResponse contains a transient response, never a saved server object.
// Item IDs identify SSE items; function call IDs remain the provider's call IDs.
type ResponsesResponse struct {
	ID                string                `json:"id"`
	Object            string                `json:"object"`
	CreatedAt         int64                 `json:"created_at"`
	Status            string                `json:"status"`
	Model             string                `json:"model"`
	Output            []responsesOutputItem `json:"output"`
	Store             bool                  `json:"store"`
	Error             *responsesError       `json:"error"`
	IncompleteDetails *responsesIncomplete  `json:"incomplete_details"`
	Usage             *responsesUsage       `json:"usage"`
	ParallelToolCalls bool                  `json:"parallel_tool_calls"`
	ToolChoice        string                `json:"tool_choice"`
	Tools             []responsesFunction   `json:"tools"`
	Text              responsesTextConfig   `json:"text"`
}

type responsesError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responsesIncomplete struct {
	Reason string `json:"reason"`
}
type responsesUsage struct {
	InputTokens         int            `json:"input_tokens"`
	OutputTokens        int            `json:"output_tokens"`
	TotalTokens         int            `json:"total_tokens"`
	InputTokensDetails  map[string]int `json:"input_tokens_details"`
	OutputTokensDetails map[string]int `json:"output_tokens_details"`
}
type responsesOutputItem struct {
	ID        string                `json:"id"`
	Type      string                `json:"type"`
	Status    string                `json:"status"`
	Role      string                `json:"role,omitempty"`
	Content   []responsesOutputText `json:"content,omitempty"`
	CallID    string                `json:"call_id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Arguments *string               `json:"arguments,omitempty"`
}
type responsesOutputText struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
	Logprobs    []any  `json:"logprobs"`
}

func newResponsesResponse(req *ResponsesRequest, model string) *ResponsesResponse {
	text := responsesTextConfig{Format: responsesTextFormat{Type: oaiContentTypeText}}
	if req.Text != nil {
		text = *req.Text
	}
	responseTools := append([]responsesFunction{}, req.Tools...)
	return &ResponsesResponse{ID: "resp_" + generateChatID(), Object: responsesObject, CreatedAt: time.Now().Unix(), Status: responsesStatusInProgress, Model: model, Output: []responsesOutputItem{}, ParallelToolCalls: true, ToolChoice: "auto", Tools: responseTools, Text: text}
}

func newResponsesMessage() responsesOutputItem {
	return responsesOutputItem{ID: "msg_" + generateChatID(), Type: responsesMessage, Status: responsesStatusInProgress, Role: responsesRoleAssistant, Content: []responsesOutputText{{Type: "output_text", Annotations: []any{}, Logprobs: []any{}}}}
}

func newResponsesFunction(call llm.ToolCall) (responsesOutputItem, error) {
	if call.ID == "" || call.Name == "" || !validJSONObject(call.Arguments) {
		return responsesOutputItem{}, fmt.Errorf("provider returned an invalid function call")
	}
	args := string(call.Arguments)
	return responsesOutputItem{ID: "fc_" + generateChatID(), Type: finishReasonFunctionCall, Status: completionStatusCompleted, CallID: call.ID, Name: call.Name, Arguments: &args}, nil
}

// HandleResponses implements POST /openai/v1/responses independently of the
// Chat Completions wire format, sharing provider policy and coordinator tools.
func (h *OpenAICompatHandler) HandleResponses(c fiber.Ctx) error {
	req, comp, invalid := parseResponsesRequest(c.Body())
	if invalid != nil {
		return c.Status(fiber.StatusBadRequest).JSON(OAIError{Error: *invalid})
	}
	ctx, cancel := context.WithTimeout(c.Context(), h.config.MaxDuration)
	defer cancel()
	namespace, err := ResolveNamespace(c, c.Query("namespace", ""), h.watchNamespace, h.enforceNamespaceIsolation)
	if err != nil {
		return openAIContextTokenAuthorizationError(c, err)
	}
	provider, model, info, err := h.resolveCompatProvider(c, ctx, req.Model, namespace)
	if err != nil {
		if ferr, ok := err.(*fiber.Error); ok && ferr.Code == fiber.StatusForbidden {
			return openAIContextTokenAuthorizationError(c, err)
		}
		return c.Status(fiber.StatusBadRequest).JSON(OAIError{Error: *responsesInvalid("model", "failed to resolve provider: "+err.Error())})
	}
	if provider.Name() == "anthropic" && comp.ResponseFormat != nil && comp.ResponseFormat.Type != oaiContentTypeText {
		return c.Status(fiber.StatusBadRequest).JSON(OAIError{Error: *responsesInvalid("text.format", "structured output requires an OpenAI-compatible provider")})
	}
	provider = llm.NewTracingProvider(provider)
	comp.Model = model
	coordinator, err := prepareCompatCoordinatorTools(c, comp, compatCoordinatorSetup{Namespace: namespace, ToolUseAction: "openAITools", AuthorizationConfig: h.contextTokenAuthorization})
	if err != nil {
		return openAIContextTokenAuthorizationError(c, err)
	}
	var toolCtx *tools.ToolContext
	if coordinator {
		toolCtx = h.responsesToolContext(c, namespace, info)
		if comp.ResponseFormat != nil && comp.ResponseFormat.Type != oaiContentTypeText {
			comp.SystemPrompt += "\n\nFor this request, the final response must use the requested JSON format. Do not include a goal-state sentinel or any prose outside the JSON value."
		}
	}
	response := newResponsesResponse(req, model)
	if coordinator {
		// Report the exposed tool set, not the discarded client function definitions.
		response.Tools = make([]responsesFunction, 0, len(comp.Tools))
		for _, tool := range comp.Tools {
			response.Tools = append(response.Tools, responsesFunction{Type: responsesFunctionToolType, Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, Strict: tool.Strict})
		}
	}
	if req.Stream {
		return h.streamResponses(c, ctx, provider, comp, response, coordinator, toolCtx)
	}
	var completion *llm.CompletionResponse
	if coordinator {
		completion, err = runNonStreamingToolLoop(ctx, provider, comp, model, h.responsesLoopConfig(comp), toolCtx, toolLoopOptions{requireFinalCompletion: true, allowEmptyTokenBudget: true})
		completion = stripGoalStateSentinelFromResponse(completion)
	} else {
		completion, err = provider.Complete(ctx, comp)
	}
	if err != nil || ctx.Err() != nil {
		return responsesProviderError(c)
	}
	if err := response.setCompletion(completion); err != nil {
		return responsesProviderError(c)
	}
	return c.JSON(response)
}

func (h *OpenAICompatHandler) responsesToolContext(c fiber.Ctx, namespace string, info ProviderResolutionInfo) *tools.ToolContext {
	user := GetUserInfo(c)
	var token *ContextToken
	if user != nil {
		token = user.ContextToken
	}
	return newCompatProxyToolContext(compatProxyToolContextConfig{
		Client: h.client, AuthorizationReader: h.apiReader, KubeClient: h.kubeClient,
		Namespace: namespace, Provider: info, WatchNamespace: h.watchNamespace,
		EnforceNamespaceIsolation: h.enforceNamespaceIsolation, ResultStore: h.resultStore,
		GatewayEventStore: h.gatewayEventStore,
		GenerateTaskName:  func() string { return fmt.Sprintf("proxy-%s", generateChatID()) },
		Profile:           openAICompatProxyToolContextProfile, AuthContext: token,
		AuthorizationConfig: h.contextTokenAuthorization, UserInfo: user,
	})
}

func responsesProviderError(c fiber.Ctx) error {
	// Provider errors may contain upstream request details; expose a stable message.
	return c.Status(fiber.StatusBadGateway).JSON(OAIError{Error: OAIErrorDetail{Type: responsesServerError, Message: "provider failed to produce a valid Responses completion"}})
}

func (r *ResponsesResponse) setOutcome(completion *llm.CompletionResponse) error {
	switch llm.NormalizeCompletionOutcome(completion) {
	case llm.CompletionOutcomeCompleted, llm.CompletionOutcomeToolCalls:
		r.Status = completionStatusCompleted
	case llm.CompletionOutcomeIncomplete:
		if len(completion.ToolCalls) != 0 {
			return fmt.Errorf("incomplete function call")
		}
		reason := "max_output_tokens"
		if completion.StopReason != "max_tokens" && completion.StopReason != "length" {
			return fmt.Errorf("unsupported incomplete outcome")
		}
		r.Status = responsesStatusIncomplete
		r.IncompleteDetails = &responsesIncomplete{Reason: reason}
	default:
		return fmt.Errorf("invalid completion outcome")
	}
	r.Usage = &responsesUsage{InputTokens: completion.InputTokens, OutputTokens: completion.OutputTokens, TotalTokens: completion.InputTokens + completion.OutputTokens, InputTokensDetails: map[string]int{"cached_tokens": 0, "cache_write_tokens": 0}, OutputTokensDetails: map[string]int{"reasoning_tokens": 0}}
	return nil
}

func (r *ResponsesResponse) setCompletion(completion *llm.CompletionResponse) error {
	if err := r.setOutcome(completion); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, output := range responsesCompletionItems(completion) {
		if output.ToolCall == nil {
			if output.Content != "" {
				item := newResponsesMessage()
				item.Status = r.Status
				item.Content[0].Text = output.Content
				r.Output = append(r.Output, item)
			}
			continue
		}
		call := *output.ToolCall
		item, err := newResponsesFunction(call)
		if err != nil {
			return err
		}
		if seen[call.ID] {
			return fmt.Errorf("duplicate function call ID")
		}
		seen[call.ID] = true
		r.Output = append(r.Output, item)
	}
	if r.Status != responsesStatusIncomplete && len(r.Output) == 0 && strings.TrimSpace(completion.Content) == "" {
		return fmt.Errorf("empty completion")
	}
	return nil
}

// Providers without ordered output retain the compat text-then-calls shape.
func responsesCompletionItems(completion *llm.CompletionResponse) []llm.AssistantOutputItem {
	if completion.OutputItems != nil {
		return completion.OutputItems
	}
	items := make([]llm.AssistantOutputItem, 0, len(completion.ToolCalls)+1)
	if completion.Content != "" {
		items = append(items, llm.AssistantOutputItem{Content: completion.Content})
	}
	for _, call := range completion.ToolCalls {
		items = append(items, llm.AssistantOutputItem{ToolCall: &call})
	}
	return items
}

func validJSONObject(data []byte) bool {
	var obj map[string]any
	return json.Unmarshal(data, &obj) == nil && obj != nil
}
