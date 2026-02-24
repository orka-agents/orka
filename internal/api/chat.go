/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/controller"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/store"
	chattools "github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/tracing"
)

var chatLog = logf.Log.WithName("chat-handler")

const defaultNamespace = "default"

// ChatConfig holds configuration for the chat handler.
type ChatConfig struct {
	Enabled         bool
	Provider        string
	Model           string
	MaxIterations   int
	MaxDuration     time.Duration
	ToolTimeout     time.Duration
	MaxConcurrent   int
	MaxTasksPerTurn int
	MaxSessionSize  int // bytes
}

// DefaultChatConfig returns a ChatConfig with default values.
func DefaultChatConfig() ChatConfig {
	return ChatConfig{
		Enabled:         true,
		MaxIterations:   20,
		MaxDuration:     5 * time.Minute,
		ToolTimeout:     60 * time.Second,
		MaxConcurrent:   10,
		MaxTasksPerTurn: 5,
		MaxSessionSize:  500 * 1024,
	}
}

// ChatRequest is the request body for POST /api/v1/chat.
type ChatRequest struct {
	Message      string   `json:"message"`
	SessionID    string   `json:"sessionId,omitempty"`
	Provider     string   `json:"provider,omitempty"`
	Model        string   `json:"model,omitempty"`
	Namespace    string   `json:"namespace,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"`
	MaxTokens    *int32   `json:"maxTokens,omitempty"`
	SystemPrompt string   `json:"systemPrompt,omitempty"`
	AgentRef     string   `json:"agentRef,omitempty"`
}

// ChatResponse is the response body for POST /api/v1/chat (JSON mode).
type ChatResponse struct {
	SessionID string         `json:"sessionId"`
	Message   string         `json:"message"`
	ToolCalls []ToolCallInfo `json:"toolCalls,omitempty"`
	Usage     ChatUsage      `json:"usage"`
}

// ToolCallInfo describes a tool invocation and its result.
type ToolCallInfo struct {
	Name   string `json:"name"`
	Args   any    `json:"args"`
	Result any    `json:"result"`
}

// ChatUsage holds usage statistics for a chat turn.
type ChatUsage struct {
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	LLMCalls     int    `json:"llmCalls"`
	ToolCalls    int    `json:"toolCalls"`
	TasksCreated int    `json:"tasksCreated"`
	Duration     string `json:"duration"`
}

// SSEEvent is the payload for server-sent events during streaming.
type SSEEvent struct {
	SessionID string `json:"sessionId"`
	Content   string `json:"content,omitempty"`
	ToolCall  any    `json:"toolCall,omitempty"`
	Error     any    `json:"error,omitempty"`
	Usage     any    `json:"usage,omitempty"`
}

// ChatHandler implements the orchestrator chat endpoints.
type ChatHandler struct {
	client                    client.Client
	sessionManager            *controller.SessionManager
	config                    ChatConfig
	semaphore                 chan struct{}
	watchNamespace            string
	enforceNamespaceIsolation bool
	sessionStore              store.SessionStore
	resultStore               store.ResultStore
	cooldownTracker           *llm.CooldownTracker
	resolver                  *ProviderResolver
}

// NewChatHandler creates a new ChatHandler.
func NewChatHandler(c client.Client, sm *controller.SessionManager, config ChatConfig, watchNamespace string, enforceNS bool, ss store.SessionStore, rs store.ResultStore, resolver *ProviderResolver) *ChatHandler {
	return &ChatHandler{
		client:                    c,
		sessionManager:            sm,
		config:                    config,
		semaphore:                 make(chan struct{}, config.MaxConcurrent),
		watchNamespace:            watchNamespace,
		enforceNamespaceIsolation: enforceNS,
		sessionStore:              ss,
		resultStore:               rs,
		cooldownTracker:           llm.NewCooldownTracker(),
		resolver:                  resolver,
	}
}

// blockedNamespaces that cannot be targeted by chat requests.
var blockedNamespaces = map[string]bool{
	"kube-system": true,
	"kube-public": true,
}

// chatParams holds the prepared parameters for a chat request.
type chatParams struct {
	ctx            context.Context
	cancel         context.CancelFunc
	provider       llm.Provider
	model          string
	messages       []llm.Message
	tools          []llm.Tool
	executor       *ToolExecutor
	sessionID      string
	namespace      string
	span           trace.Span
	systemPrompt   string
	temperature    float64
	maxTokens      int
	persistedCount int
}

// HandleChat handles POST /api/v1/chat.
func (ch *ChatHandler) HandleChat(c fiber.Ctx) error {
	var req ChatRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	if req.Message == "" {
		return fiber.NewError(fiber.StatusBadRequest, "message is required")
	}

	// Try to acquire semaphore (non-blocking)
	select {
	case ch.semaphore <- struct{}{}:
	default:
		c.Set("Retry-After", "5")
		return fiber.NewError(fiber.StatusTooManyRequests, "too many concurrent chat requests")
	}
	sseMode := false
	defer func() {
		if !sseMode {
			<-ch.semaphore
		}
	}()

	params, err := ch.buildChatParams(c, req)
	if err != nil {
		return err
	}
	defer params.cancel()
	defer params.span.End()

	// Route to JSON or SSE based on Accept header
	accept := c.Get("Accept")
	if accept == "application/json" {
		return ch.handleChatJSON(c, params)
	}

	sseMode = true
	return ch.handleChatSSE(c, params)
}

// buildChatParams resolves the namespace, provider, session, system prompt,
// tools, and executor needed for a chat turn.
func (ch *ChatHandler) buildChatParams(c fiber.Ctx, req ChatRequest) (*chatParams, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ch.config.MaxDuration)

	// Resolve namespace from request or token
	namespace, err := ResolveNamespace(c, req.Namespace, ch.watchNamespace, ch.enforceNamespaceIsolation)
	if err != nil {
		cancel()
		return nil, err
	}

	if blockedNamespaces[namespace] {
		cancel()
		return nil, fiber.NewError(fiber.StatusForbidden, fmt.Sprintf("namespace %q is not allowed for chat", namespace))
	}

	// Resolve or create session ID
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("chat-%s", generateChatID())
	}

	// Resolve LLM provider
	provider, model, err := ch.resolver.Resolve(ctx, ResolveOpts{
		ProviderName: req.Provider,
		Model:        req.Model,
		AgentRef:     req.AgentRef,
		Namespace:    namespace,
	})
	if err != nil {
		chatLog.Error(err, "failed to resolve provider")
		cancel()
		return nil, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("failed to resolve provider: %v", err))
	}

	// Wrap provider with retry and fallback
	provider = ch.wrapWithRetryAndFallback(ctx, provider, req, namespace)

	// Wrap provider with tracing
	provider = llm.NewTracingProvider(provider)

	// Start chat.request span
	tracer := tracing.Tracer("orka.chat")
	ctx, span := tracer.Start(ctx, "chat.request",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String("chat.provider", provider.Name()),
			attribute.String("chat.model", model),
		),
	)

	// Build system prompt
	promptBuilder := NewSystemPromptBuilder(ch.client, namespace)
	systemPrompt, err := promptBuilder.BuildSystemPrompt(ctx, req.SystemPrompt, PromptModeFull)
	if err != nil {
		chatLog.Error(err, "failed to build system prompt")
		span.End()
		cancel()
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to build system prompt")
	}

	// Load session history
	messages, err := ch.loadChatSession(ctx, namespace, sessionID)
	if err != nil {
		chatLog.Info("no existing session, starting fresh", "sessionId", sessionID, "error", err)
		messages = []llm.Message{}
	}
	persistedCount := len(messages)

	// Append user message — if an agentRef is set and the agent has a runtime,
	// prepend context so the LLM knows to use create_agent_task.
	userContent := req.Message
	if req.AgentRef != "" {
		agentObj := &corev1alpha1.Agent{}
		if err := ch.client.Get(ctx, types.NamespacedName{Name: req.AgentRef, Namespace: namespace}, agentObj); err == nil {
			if agentObj.Spec.Runtime != nil {
				userContent = fmt.Sprintf("[Using agent %q which has runtime %q — use create_agent_task with agent=%q for this request.]\n\n%s",
					req.AgentRef, agentObj.Spec.Runtime.Type, req.AgentRef, req.Message)
			}
		}
	}
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: userContent,
	})

	// Create tool executor (also creates the chat registry)
	executor := NewToolExecutor(ch.client, ch.sessionManager, namespace, sessionID, ch.watchNamespace, ch.enforceNamespaceIsolation, ch.config.MaxTasksPerTurn, ch.config.ToolTimeout, ch.resultStore)

	// Build tools from the chat registry
	tools := executor.registry.ToLLMTools(chattools.ChatToolNames())

	// Build completion request parameters
	var temperature float64
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = int(*req.MaxTokens)
	}

	return &chatParams{
		ctx:            ctx,
		cancel:         cancel,
		provider:       provider,
		model:          model,
		messages:       messages,
		tools:          tools,
		executor:       executor,
		sessionID:      sessionID,
		namespace:      namespace,
		span:           span,
		systemPrompt:   systemPrompt,
		temperature:    temperature,
		maxTokens:      maxTokens,
		persistedCount: persistedCount,
	}, nil
}

// handleChatJSON runs the tool loop and returns the result as a JSON response.
func (ch *ChatHandler) handleChatJSON(c fiber.Ctx, p *chatParams) error {
	content, usage, toolCalls, err := ch.runToolLoop(p.ctx, p.provider, p.messages, p.systemPrompt, p.tools, p.executor, p.sessionID, p.namespace, p.model, p.temperature, p.maxTokens, p.persistedCount, nil)
	if err != nil {
		chatLog.Error(err, "tool loop error")
		p.span.RecordError(err)
		p.span.SetStatus(codes.Error, err.Error())
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("chat error: %v", err))
	}

	return c.JSON(ChatResponse{
		SessionID: p.sessionID,
		Message:   content,
		ToolCalls: toolCalls,
		Usage:     usage,
	})
}

// handleChatSSE runs the tool loop inside a streaming response, emitting SSE events.
func (ch *ChatHandler) handleChatSSE(c fiber.Ctx, p *chatParams) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	return c.SendStreamWriter(func(w *bufio.Writer) {
		defer func() { <-ch.semaphore }()
		// Create a new context for the SSE closure (the outer ctx is cancelled
		// when HandleChat returns)
		sseCtx, sseCancel := context.WithTimeout(context.Background(), ch.config.MaxDuration)
		defer sseCancel()

		emitSSE := func(event, data string) {
			_ = writeSSE(w, event, data)
		}

		// Emit status event
		statusData, _ := json.Marshal(map[string]string{
			"sessionId": p.sessionID,
			"provider":  p.provider.Name(),
			"model":     p.model,
		})
		emitSSE("status", string(statusData))

		content, usage, _, err := ch.runToolLoop(sseCtx, p.provider, p.messages, p.systemPrompt, p.tools, p.executor, p.sessionID, p.namespace, p.model, p.temperature, p.maxTokens, p.persistedCount, emitSSE)
		if err != nil {
			errData, _ := json.Marshal(map[string]string{"error": err.Error()})
			emitSSE("error", string(errData))
		}

		_ = content // content already emitted via SSE

		// Emit done event
		doneData, _ := json.Marshal(map[string]any{"usage": usage})
		emitSSE("done", string(doneData))
	})
}

// runToolLoop executes the agentic tool loop until the LLM produces a final text response.
func (ch *ChatHandler) runToolLoop(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt string,
	tools []llm.Tool,
	executor *ToolExecutor,
	sessionID, namespace, model string,
	temperature float64,
	maxTokens int,
	persistedCount int,
	emitSSE func(event, data string),
) (string, ChatUsage, []ToolCallInfo, error) {
	var usage ChatUsage
	var allToolCalls []ToolCallInfo
	repetitionTracker := make(map[string]int)
	start := time.Now()

	for iteration := 0; ; iteration++ {
		// Start a span for this iteration
		iterTracer := tracing.Tracer("orka.chat")
		iterCtx, iterSpan := iterTracer.Start(ctx, "chat.tool_loop.iteration",
			trace.WithAttributes(
				attribute.Int("chat.iteration", iteration),
			),
		)

		// Check context cancellation
		select {
		case <-iterCtx.Done():
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			iterSpan.End()
			return "I ran out of time. Here's what I accomplished so far.", usage, allToolCalls, nil
		default:
		}

		// Check iteration limit
		if iteration >= ch.config.MaxIterations {
			// Inject graceful termination message and do one final call
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: You have reached the maximum number of iterations. Please provide a final summary of what you accomplished.]",
			})

			resp, err := provider.Complete(iterCtx, &llm.CompletionRequest{
				Model:        model,
				SystemPrompt: systemPrompt,
				Messages:     messages,
				MaxTokens:    maxTokens,
				Temperature:  temperature,
			})
			if err != nil {
				usage.Duration = time.Since(start).Round(time.Millisecond).String()
				iterSpan.End()
				return "Reached iteration limit.", usage, allToolCalls, nil
			}
			usage.LLMCalls++
			usage.InputTokens += resp.InputTokens
			usage.OutputTokens += resp.OutputTokens

			if emitSSE != nil && resp.Content != "" {
				msgData, _ := json.Marshal(map[string]string{"content": resp.Content})
				emitSSE("message", string(msgData))
			}

			// Save session
			finalMessages := append(messages, llm.Message{Role: "assistant", Content: resp.Content})
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			usage.TasksCreated = executor.tasksCreated
			_ = ch.saveChatSession(iterCtx, namespace, sessionID, finalMessages, persistedCount, usage)
			if err := ch.sessionStore.UpdateTokenCounts(iterCtx, namespace, sessionID, usage.InputTokens, usage.OutputTokens); err != nil {
				chatLog.Error(err, "failed to update token counts")
			}
			iterSpan.SetAttributes(
				attribute.Int("chat.llm_calls", usage.LLMCalls),
				attribute.Int("chat.input_tokens", usage.InputTokens),
				attribute.Int("chat.output_tokens", usage.OutputTokens),
			)
			iterSpan.End()
			return resp.Content, usage, allToolCalls, nil
		}

		// Inject progress assertion every 5 iterations
		if iteration > 0 && iteration%5 == 0 {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: Progress check — summarize what you've done so far and what remains.]",
			})
		}

		// Truncate conversation if it exceeds the session size budget
		if ch.config.MaxSessionSize > 0 {
			tokenBudget := ch.config.MaxSessionSize / 4 // bytes to approximate tokens
			messages = llm.TruncateMessages(messages, tokenBudget)
		}

		// Call LLM
		resp, err := provider.Complete(iterCtx, &llm.CompletionRequest{
			Model:        model,
			SystemPrompt: systemPrompt,
			Messages:     messages,
			Tools:        tools,
			MaxTokens:    maxTokens,
			Temperature:  temperature,
		})
		if err != nil && llm.IsContextTooLongErr(err) {
			// Halve the messages and retry once
			tokenEstimate := 0
			for _, m := range messages {
				tokenEstimate += len(m.Content) / 4
			}
			messages = llm.TruncateMessages(messages, tokenEstimate/2)
			resp, err = provider.Complete(ctx, &llm.CompletionRequest{
				Model:        model,
				SystemPrompt: systemPrompt,
				Messages:     messages,
				Tools:        tools,
				MaxTokens:    maxTokens,
				Temperature:  temperature,
			})
		}
		if err != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			iterSpan.RecordError(err)
			iterSpan.SetStatus(codes.Error, err.Error())
			iterSpan.End()
			return "", usage, allToolCalls, fmt.Errorf("LLM completion failed: %w", err)
		}
		usage.LLMCalls++
		usage.InputTokens += resp.InputTokens
		usage.OutputTokens += resp.OutputTokens

		// If response has tool calls, execute them
		if len(resp.ToolCalls) > 0 {
			// Append assistant message with tool calls
			messages = append(messages, llm.Message{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			var repetitionWarning string
			for _, tc := range resp.ToolCalls {
				// Emit tool_call SSE event
				if emitSSE != nil {
					tcData, _ := json.Marshal(map[string]any{
						"id":   tc.ID,
						"name": tc.Name,
						"args": tc.Arguments,
					})
					emitSSE("tool_call", string(tcData))
				}

				// Check repetition (defer warning until after all tool results)
				argsHash := hashArgs(tc.Name, tc.Arguments)
				repetitionTracker[argsHash]++
				if repetitionTracker[argsHash] >= 3 {
					repetitionWarning = fmt.Sprintf("[System: Warning — you have called %s with the same arguments %d times. Try a different approach.]", tc.Name, repetitionTracker[argsHash])
					iteration += 5
				}

				// Execute tool
				result, execErr := executor.Execute(iterCtx, tc)
				if execErr != nil {
					errResult := map[string]any{"success": false, "error": execErr.Error()}
					if errJSON, jsonErr := json.Marshal(errResult); jsonErr == nil {
						result = string(errJSON)
					} else {
						result = `{"success":false,"error":"tool execution failed"}`
					}
				}

				// Emit tool_result SSE event
				if emitSSE != nil {
					trData, _ := json.Marshal(map[string]any{
						"id":     tc.ID,
						"name":   tc.Name,
						"result": json.RawMessage(result),
					})
					emitSSE("tool_result", string(trData))
				}

				// Append tool result message
				messages = append(messages, llm.Message{
					Role:       "tool",
					ToolCallID: tc.ID,
					Name:       tc.Name,
					Content:    result,
				})

				// Track for response
				var argsAny any
				_ = json.Unmarshal(tc.Arguments, &argsAny)
				var resultAny any
				_ = json.Unmarshal([]byte(result), &resultAny)
				allToolCalls = append(allToolCalls, ToolCallInfo{
					Name:   tc.Name,
					Args:   argsAny,
					Result: resultAny,
				})

				usage.ToolCalls++
			}

			// Append repetition warning after all tool results
			if repetitionWarning != "" {
				messages = append(messages, llm.Message{
					Role:    "user",
					Content: repetitionWarning,
				})
			}

			// Continue loop for next LLM call
			iterSpan.End()
			continue
		}

		// This is the final response
		if emitSSE != nil && resp.Content != "" {
			msgData, _ := json.Marshal(map[string]string{"content": resp.Content})
			emitSSE("message", string(msgData))
		}

		// Save session
		finalMessages := append(messages, llm.Message{Role: "assistant", Content: resp.Content})
		usage.Duration = time.Since(start).Round(time.Millisecond).String()
		usage.TasksCreated = executor.tasksCreated
		_ = ch.saveChatSession(iterCtx, namespace, sessionID, finalMessages, persistedCount, usage)
		if err := ch.sessionStore.UpdateTokenCounts(iterCtx, namespace, sessionID, usage.InputTokens, usage.OutputTokens); err != nil {
			chatLog.Error(err, "failed to update token counts")
		}

		iterSpan.SetAttributes(
			attribute.Int("chat.llm_calls", usage.LLMCalls),
			attribute.Int("chat.input_tokens", usage.InputTokens),
			attribute.Int("chat.output_tokens", usage.OutputTokens),
		)
		iterSpan.End()
		return resp.Content, usage, allToolCalls, nil
	}
}

// loadChatSession loads chat session messages from the session store.
func (ch *ChatHandler) loadChatSession(ctx context.Context, namespace, sessionID string) ([]llm.Message, error) {
	messages, err := ch.sessionStore.LoadTranscript(ctx, namespace, sessionID, 0)
	if err != nil {
		return nil, err
	}

	if len(messages) == 0 {
		return nil, nil
	}

	// Convert store.SessionMessage to llm.Message
	llmMessages := make([]llm.Message, 0, len(messages))
	for _, msg := range messages {
		m := llm.Message{
			Role:       msg.Role,
			Content:    msg.Content,
			Name:       msg.Name,
			ToolCallID: msg.ToolCallID,
		}
		if msg.ToolCalls != nil {
			if data, err := json.Marshal(msg.ToolCalls); err == nil {
				var toolCalls []llm.ToolCall
				if json.Unmarshal(data, &toolCalls) == nil {
					m.ToolCalls = toolCalls
				}
			}
		}
		llmMessages = append(llmMessages, m)
	}

	return llmMessages, nil
}

// saveChatSession saves chat session messages to the session store.
func (ch *ChatHandler) saveChatSession(ctx context.Context, namespace, sessionID string, messages []llm.Message, persistedCount int, _ ChatUsage) error {
	// Check if session exists
	_, err := ch.sessionStore.GetSession(ctx, namespace, sessionID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("failed to get session: %w", err)
		}
		// Create new session
		now := time.Now()
		session := &store.SessionRecord{
			Namespace:   namespace,
			Name:        sessionID,
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := ch.sessionStore.CreateSession(ctx, session); err != nil {
			return fmt.Errorf("failed to create session: %w", err)
		}
	}

	// Only append messages that haven't been persisted yet
	newMessages := messages[persistedCount:]
	if len(newMessages) == 0 {
		return nil
	}

	// Convert llm.Message to store.SessionMessage
	storeMessages := make([]store.SessionMessage, 0, len(newMessages))
	now := time.Now()
	for _, msg := range newMessages {
		sm := store.SessionMessage{
			Role:       msg.Role,
			Content:    msg.Content,
			Name:       msg.Name,
			ToolCallID: msg.ToolCallID,
			Timestamp:  now,
		}
		if len(msg.ToolCalls) > 0 {
			sm.ToolCalls = msg.ToolCalls
		}
		storeMessages = append(storeMessages, sm)
	}

	return ch.sessionStore.AppendMessages(ctx, namespace, sessionID, storeMessages)
}

// HandleChatConfig handles GET /api/v1/chat/config.
func (ch *ChatHandler) HandleChatConfig(c fiber.Ctx) error {
	toolNames := chattools.ChatToolNames()

	return c.JSON(fiber.Map{
		"enabled":         ch.config.Enabled,
		"provider":        ch.config.Provider,
		"model":           ch.config.Model,
		"maxIterations":   ch.config.MaxIterations,
		"maxDuration":     ch.config.MaxDuration.String(),
		"maxTasksPerTurn": ch.config.MaxTasksPerTurn,
		"maxConcurrent":   ch.config.MaxConcurrent,
		"availableTools":  toolNames,
	})
}

// HandleCancelChat handles DELETE /api/v1/chat/{sessionId}.
func (ch *ChatHandler) HandleCancelChat(c fiber.Ctx) error {
	sessionID := c.Params("sessionId")
	if sessionID == "" {
		return fiber.NewError(fiber.StatusBadRequest, "sessionId is required")
	}

	namespace := GetEffectiveNamespace(c, c.Query("namespace", ""))
	if ch.watchNamespace != "" {
		namespace = ch.watchNamespace
	}
	if ch.enforceNamespaceIsolation {
		ui := GetUserInfo(c)
		if ui != nil && ui.Namespace != "" && namespace != ui.Namespace {
			return fiber.NewError(fiber.StatusForbidden, fmt.Sprintf("namespace %q not allowed", namespace))
		}
	}

	ctx := c.Context()
	_, err := ch.sessionStore.GetSession(ctx, namespace, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "chat session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}

	// Delete the session to cancel it
	if err := ch.sessionStore.DeleteSession(ctx, namespace, sessionID); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to cancel session: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// wrapWithRetryAndFallback wraps a provider with retry logic and adds fallback
// providers if the agent has them configured.
func (ch *ChatHandler) wrapWithRetryAndFallback(ctx context.Context, provider llm.Provider, req ChatRequest, namespace string) llm.Provider {
	var resultProvider llm.Provider = llm.NewRetryProvider(provider, 0)

	if req.AgentRef == "" {
		return resultProvider
	}

	agent := &corev1alpha1.Agent{}
	if err := ch.client.Get(ctx, types.NamespacedName{Name: req.AgentRef, Namespace: namespace}, agent); err != nil {
		return resultProvider
	}
	if agent.Spec.Model == nil || len(agent.Spec.Model.Fallbacks) == 0 {
		return resultProvider
	}

	fallbacks := make([]llm.FallbackEntry, 0, len(agent.Spec.Model.Fallbacks))
	for _, fb := range agent.Spec.Model.Fallbacks {
		fbProviderCRD, err := ch.resolver.LookupProvider(ctx, fb.ProviderRef, namespace)
		if err != nil {
			continue
		}

		fbAPIKey, err := ch.resolver.ResolveAPIKey(ctx, fbProviderCRD)
		if err != nil {
			continue
		}

		fbConfig := llm.ProviderConfig{
			APIKey:       fbAPIKey,
			BaseURL:      fbProviderCRD.Spec.BaseURL,
			ProviderType: string(fbProviderCRD.Spec.Type),
		}
		if fbProviderCRD.Spec.Azure != nil {
			fbConfig.AzureAPIVersion = fbProviderCRD.Spec.Azure.APIVersion
		}

		fbProvider, err := llm.NewProvider(string(fbProviderCRD.Spec.Type), fbConfig)
		if err != nil {
			continue
		}

		fallbacks = append(fallbacks, llm.FallbackEntry{
			Provider: llm.NewRetryProvider(fbProvider, 0),
			Model:    fb.Model,
		})
	}

	if len(fallbacks) > 0 {
		fp := llm.NewFallbackProvider(resultProvider, fallbacks)
		fp.SetCooldownTracker(ch.cooldownTracker)
		resultProvider = fp
	}

	return resultProvider
}

// generateChatID returns 8 random hex characters.
func generateChatID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// hashArgs creates a short hash of tool name + arguments for repetition detection.
func hashArgs(name string, args json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write(args)
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// writeSSE writes a server-sent event to the writer.
func writeSSE(w *bufio.Writer, event, data string) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if err != nil {
		return err
	}
	return w.Flush()
}
