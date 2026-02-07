/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
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
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/controller"
	"github.com/sozercan/mercan/internal/llm"
	"github.com/sozercan/mercan/internal/store"
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
	client         client.Client
	sessionManager *controller.SessionManager
	config         ChatConfig
	semaphore      chan struct{}
	watchNamespace string
	sessionStore   store.SessionStore
	resultStore    store.ResultStore
}

// NewChatHandler creates a new ChatHandler.
func NewChatHandler(c client.Client, sm *controller.SessionManager, config ChatConfig, watchNamespace string, ss store.SessionStore, rs store.ResultStore) *ChatHandler {
	return &ChatHandler{
		client:         c,
		sessionManager: sm,
		config:         config,
		semaphore:      make(chan struct{}, config.MaxConcurrent),
		watchNamespace: watchNamespace,
		sessionStore:   ss,
		resultStore:    rs,
	}
}

// blockedNamespaces that cannot be targeted by chat requests.
var blockedNamespaces = map[string]bool{
	"kube-system": true,
	"kube-public": true,
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
	defer func() { <-ch.semaphore }()

	ctx, cancel := context.WithTimeout(context.Background(), ch.config.MaxDuration)
	defer cancel()

	// Resolve namespace
	namespace := req.Namespace
	if namespace == "" {
		namespace = ch.watchNamespace
	}
	if namespace == "" {
		namespace = defaultNamespace
	}

	if blockedNamespaces[namespace] {
		return fiber.NewError(fiber.StatusForbidden, fmt.Sprintf("namespace %q is not allowed for chat", namespace))
	}

	// Resolve or create session ID
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("chat-%s", generateChatID())
	}

	// Resolve LLM provider
	provider, model, err := ch.resolveProvider(ctx, req, namespace)
	if err != nil {
		chatLog.Error(err, "failed to resolve provider")
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("failed to resolve provider: %v", err))
	}

	// Build system prompt
	promptBuilder := NewSystemPromptBuilder(ch.client, namespace)
	systemPrompt, err := promptBuilder.BuildSystemPrompt(ctx, req.SystemPrompt)
	if err != nil {
		chatLog.Error(err, "failed to build system prompt")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to build system prompt")
	}

	// Load session history
	messages, err := ch.loadChatSession(ctx, namespace, sessionID)
	if err != nil {
		chatLog.Info("no existing session, starting fresh", "sessionId", sessionID, "error", err)
		messages = []llm.Message{}
	}

	// Append user message
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: req.Message,
	})

	// Build tools
	tools := CoreTools()
	if ShouldLoadManagementTools(req.Message) {
		tools = append(tools, ManagementTools()...)
	}

	// Create tool executor
	executor := NewToolExecutor(ch.client, ch.sessionManager, namespace, sessionID, ch.watchNamespace, ch.config.MaxTasksPerTurn, ch.config.ToolTimeout, ch.resultStore)

	// Build completion request parameters
	temperature := 0.7
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = int(*req.MaxTokens)
	}

	// Check Accept header for response format
	accept := c.Get("Accept")
	if accept == "application/json" {
		// JSON mode: run tool loop, collect all content, return JSON
		content, usage, toolCalls, err := ch.runToolLoop(ctx, provider, messages, systemPrompt, tools, executor, sessionID, namespace, model, temperature, maxTokens, nil)
		if err != nil {
			chatLog.Error(err, "tool loop error")
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("chat error: %v", err))
		}

		return c.JSON(ChatResponse{
			SessionID: sessionID,
			Message:   content,
			ToolCalls: toolCalls,
			Usage:     usage,
		})
	}

	// SSE mode
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	// Capture values for the streaming closure (ctx from outer scope is cancelled
	// when HandleChat returns, so we create a new context inside the callback)
	sseProvider := provider
	sseMessages := messages
	sseSystemPrompt := systemPrompt
	sseTools := tools
	sseExecutor := executor

	return c.SendStreamWriter(func(w *bufio.Writer) {
		sseCtx, sseCancel := context.WithTimeout(context.Background(), ch.config.MaxDuration)
		defer sseCancel()

		emitSSE := func(event, data string) {
			_ = writeSSE(w, event, data)
		}

		// Emit status event
		statusData, _ := json.Marshal(map[string]string{
			"sessionId": sessionID,
			"provider":  sseProvider.Name(),
			"model":     model,
		})
		emitSSE("status", string(statusData))

		content, usage, _, err := ch.runToolLoop(sseCtx, sseProvider, sseMessages, sseSystemPrompt, sseTools, sseExecutor, sessionID, namespace, model, temperature, maxTokens, emitSSE)
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
	emitSSE func(event, data string),
) (string, ChatUsage, []ToolCallInfo, error) {
	var usage ChatUsage
	var allToolCalls []ToolCallInfo
	repetitionTracker := make(map[string]int)
	start := time.Now()

	for iteration := 0; ; iteration++ {
		// Check context cancellation
		select {
		case <-ctx.Done():
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
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

			resp, err := provider.Complete(ctx, &llm.CompletionRequest{
				Model:        model,
				SystemPrompt: systemPrompt,
				Messages:     messages,
				MaxTokens:    maxTokens,
				Temperature:  temperature,
			})
			if err != nil {
				usage.Duration = time.Since(start).Round(time.Millisecond).String()
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
			_ = ch.saveChatSession(ctx, namespace, sessionID, finalMessages, usage)
			return resp.Content, usage, allToolCalls, nil
		}

		// Inject progress assertion every 5 iterations
		if iteration > 0 && iteration%5 == 0 {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: Progress check — summarize what you've done so far and what remains.]",
			})
		}

		// Call LLM
		resp, err := provider.Complete(ctx, &llm.CompletionRequest{
			Model:        model,
			SystemPrompt: systemPrompt,
			Messages:     messages,
			Tools:        tools,
			MaxTokens:    maxTokens,
			Temperature:  temperature,
		})
		if err != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
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
				result, execErr := executor.Execute(ctx, tc)
				if execErr != nil {
					result = fmt.Sprintf(`{"success":false,"error":"%s"}`, execErr.Error())
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
			continue
		}

		// No tool calls — this is the final response
		if emitSSE != nil && resp.Content != "" {
			msgData, _ := json.Marshal(map[string]string{"content": resp.Content})
			emitSSE("message", string(msgData))
		}

		// Save session
		finalMessages := append(messages, llm.Message{Role: "assistant", Content: resp.Content})
		usage.Duration = time.Since(start).Round(time.Millisecond).String()
		usage.TasksCreated = executor.tasksCreated
		_ = ch.saveChatSession(ctx, namespace, sessionID, finalMessages, usage)

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
		llmMessages = append(llmMessages, llm.Message{
			Role:    msg.Role,
			Content: msg.Content,
			Name:    msg.Name,
		})
	}

	return llmMessages, nil
}

// saveChatSession saves chat session messages to the session store.
func (ch *ChatHandler) saveChatSession(ctx context.Context, namespace, sessionID string, messages []llm.Message, usage ChatUsage) error {
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

	// Convert llm.Message to store.SessionMessage
	storeMessages := make([]store.SessionMessage, 0, len(messages))
	now := time.Now()
	for _, msg := range messages {
		storeMessages = append(storeMessages, store.SessionMessage{
			Role:      msg.Role,
			Content:   msg.Content,
			Name:      msg.Name,
			Timestamp: now,
		})
	}

	return ch.sessionStore.AppendMessages(ctx, namespace, sessionID, storeMessages)
}

// HandleChatConfig handles GET /api/v1/chat/config.
func (ch *ChatHandler) HandleChatConfig(c fiber.Ctx) error {
	tools := CoreTools()
	tools = append(tools, ManagementTools()...)
	toolNames := make([]string, 0, len(tools))
	for _, t := range tools {
		toolNames = append(toolNames, t.Name)
	}

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

	namespace := c.Query("namespace", defaultNamespace)
	if ch.watchNamespace != "" {
		namespace = ch.watchNamespace
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

// resolveProvider resolves the LLM provider from the request, agent, or config.
func (ch *ChatHandler) resolveProvider(ctx context.Context, req ChatRequest, namespace string) (llm.Provider, string, error) {
	var providerType corev1alpha1.ProviderType
	var apiKey string
	var baseURL string
	var model string
	var providerCRD *corev1alpha1.Provider

	if req.AgentRef != "" {
		// Look up Agent CRD
		agent := &corev1alpha1.Agent{}
		if err := ch.client.Get(ctx, types.NamespacedName{Name: req.AgentRef, Namespace: namespace}, agent); err != nil {
			return nil, "", fmt.Errorf("agent %q not found: %w", req.AgentRef, err)
		}

		// Get model from agent
		if agent.Spec.Model != nil {
			if agent.Spec.Model.Name != "" {
				model = agent.Spec.Model.Name
			}
		}

		// Get provider from agent
		if agent.Spec.ProviderRef != nil {
			p, err := ch.lookupProvider(ctx, agent.Spec.ProviderRef.Name, namespace)
			if err != nil {
				return nil, "", err
			}
			providerCRD = p
		}
	}

	if providerCRD == nil && req.Provider != "" {
		p, err := ch.lookupProvider(ctx, req.Provider, namespace)
		if err != nil {
			return nil, "", err
		}
		providerCRD = p
	}

	if providerCRD == nil && ch.config.Provider != "" {
		p, err := ch.lookupProvider(ctx, ch.config.Provider, namespace)
		if err != nil {
			return nil, "", err
		}
		providerCRD = p
	}

	if providerCRD == nil {
		p, err := ch.lookupProvider(ctx, "default", namespace)
		if err != nil {
			return nil, "", fmt.Errorf("no provider configured and no 'default' Provider CRD found: %w", err)
		}
		providerCRD = p
	}

	providerType = providerCRD.Spec.Type
	baseURL = providerCRD.Spec.BaseURL

	// Resolve API key from secret
	secretName := providerCRD.Spec.SecretRef.Name
	secretKey := providerCRD.Spec.SecretRef.Key
	if secretKey == "" {
		secretKey = "api-key"
	}

	secret := &corev1.Secret{}
	if err := ch.client.Get(ctx, types.NamespacedName{Name: secretName, Namespace: providerCRD.Namespace}, secret); err != nil {
		return nil, "", fmt.Errorf("failed to get provider secret %q: %w", secretName, err)
	}
	apiKeyBytes, ok := secret.Data[secretKey]
	if !ok {
		return nil, "", fmt.Errorf("secret %q has no key %q", secretName, secretKey)
	}
	apiKey = string(apiKeyBytes)

	// Model resolution priority: req.Model > agent model > provider default > config model
	if req.Model != "" {
		model = req.Model
	}
	if model == "" {
		model = providerCRD.Spec.DefaultModel
	}
	if model == "" {
		model = ch.config.Model
	}

	provider, err := llm.NewProvider(string(providerType), llm.ProviderConfig{
		APIKey:  apiKey,
		BaseURL: baseURL,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to create LLM provider: %w", err)
	}

	return provider, model, nil
}

// lookupProvider fetches a Provider CRD by name and namespace.
func (ch *ChatHandler) lookupProvider(ctx context.Context, name, namespace string) (*corev1alpha1.Provider, error) {
	p := &corev1alpha1.Provider{}
	if err := ch.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, p); err != nil {
		return nil, fmt.Errorf("provider %q not found in namespace %q: %w", name, namespace, err)
	}
	return p, nil
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

// truncateTranscript keeps the most recent lines that fit within maxSize bytes.
func truncateTranscript(transcript string, maxSize int) string {
	lines := strings.Split(transcript, "\n")
	var kept []string
	size := 0
	for i := len(lines) - 1; i >= 0; i-- {
		lineSize := len(lines[i]) + 1 // +1 for newline
		if size+lineSize > maxSize {
			break
		}
		size += lineSize
		kept = append([]string{lines[i]}, kept...)
	}
	return strings.Join(kept, "\n")
}

// writeSSE writes a server-sent event to the writer.
func writeSSE(w *bufio.Writer, event, data string) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if err != nil {
		return err
	}
	return w.Flush()
}
