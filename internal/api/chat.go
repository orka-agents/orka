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
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	chattools "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/tracing/genai"
)

var chatLog = logf.Log.WithName("chat-handler")

const (
	defaultNamespace      = "default"
	chatDurabilityTimeout = 10 * time.Second
)

const (
	chatStreamUnclaimed uint32 = iota
	chatStreamOwned
	chatStreamFinalized
)

// ChatConfig holds configuration for the chat handler.
type ChatConfig struct {
	Enabled                bool
	Provider               string
	Model                  string
	MaxIterations          int
	MaxDuration            time.Duration
	ToolTimeout            time.Duration
	MaxConcurrent          int
	MaxTasksPerTurn        int
	MaxSessionSize         int // bytes
	MaxPrematureEndRetries int // re-prompts when the model emits text without the GOAL_STATE sentinel
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
	kubeClient                kubernetes.Interface
	sessionManager            *controller.SessionManager
	config                    ChatConfig
	semaphore                 chan struct{}
	watchNamespace            string
	enforceNamespaceIsolation bool
	sessionStore              store.SessionStore
	sessionTurnCommitter      store.SessionTurnCommitter
	resultStore               store.ResultStore
	contextTokenAuthorization ContextTokenAuthorizationConfig
	cooldownTracker           *llm.CooldownTracker
	resolver                  *ProviderResolver
	activeChatTurnsMu         sync.Mutex
	activeChatTurns           map[chatTurnKey]*activeChatTurn
}

type chatTurnKey struct {
	namespace string
	sessionID string
}

type activeChatTurn struct {
	turnID     string
	cancel     context.CancelFunc
	done       chan struct{}
	expiresAt  time.Time
	cancellers int
}

type chatStreamRequest struct {
	parentCtx      context.Context
	turnCancelCtx  context.Context
	turnDeadline   time.Time
	finalizeTurn   func()
	provider       llm.Provider
	messages       []llm.Message
	systemPrompt   string
	tools          []llm.Tool
	executor       *ToolExecutor
	sessionID      string
	namespace      string
	model          string
	temperature    float64
	maxTokens      int
	persistedCount int
	turnID         string
	span           trace.Span
}

// NewChatHandler creates a new ChatHandler.
func NewChatHandler(c client.Client, sm *controller.SessionManager, config ChatConfig, watchNamespace string, enforceNS bool, ss store.SessionStore, rs store.ResultStore, resolver *ProviderResolver, kubeClientOpt ...kubernetes.Interface) *ChatHandler {
	var kubeClient kubernetes.Interface
	if len(kubeClientOpt) > 0 {
		kubeClient = kubeClientOpt[0]
	}

	handler := &ChatHandler{
		client:                    c,
		kubeClient:                kubeClient,
		sessionManager:            sm,
		config:                    config,
		semaphore:                 make(chan struct{}, config.MaxConcurrent),
		watchNamespace:            watchNamespace,
		enforceNamespaceIsolation: enforceNS,
		sessionStore:              ss,
		resultStore:               rs,
		cooldownTracker:           llm.NewCooldownTracker(),
		resolver:                  resolver,
		activeChatTurns:           make(map[chatTurnKey]*activeChatTurn),
	}
	if committer, ok := ss.(store.SessionTurnCommitter); ok {
		handler.sessionTurnCommitter = committer
	}
	return handler
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

	userInfo := GetUserInfo(c)
	var contextToken *ContextToken
	if userInfo != nil {
		contextToken = userInfo.ContextToken
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

	// Derive non-SSE work from the Fiber user context so chat.request,
	// tool-loop, and GenAI spans remain children of the API SERVER span.
	// SSE mode captures the span context before returning and seeds its
	// long-lived background context below.
	ctx, cancel := context.WithTimeout(c.Context(), ch.config.MaxDuration)
	defer cancel()

	// Resolve namespace from request or token
	namespace, err := ResolveNamespace(c, req.Namespace, ch.watchNamespace, ch.enforceNamespaceIsolation)
	if err != nil {
		return err
	}

	if blockedNamespaces[namespace] {
		return fiber.NewError(fiber.StatusForbidden, fmt.Sprintf("namespace %q is not allowed for chat", namespace))
	}
	if err := authorizeContextTokenAgentContext(c, ch.contextTokenAuthorization, "chat", namespace, req.AgentRef); err != nil {
		return err
	}

	// Resolve or create session ID
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("chat-%s", generateChatID())
	}

	// Resolve LLM provider
	provider, model, providerInfo, err := ch.resolver.ResolveWithInfo(ctx, ResolveOpts{
		ProviderName: req.Provider,
		Model:        req.Model,
		AgentRef:     req.AgentRef,
		Namespace:    namespace,
	})
	if err != nil {
		chatLog.Error(err, "failed to resolve provider")
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("failed to resolve provider: %v", err))
	}

	if err := authorizeContextTokenProviderUse(c, ch.contextTokenAuthorization, "chat", namespace, providerInfo, model); err != nil {
		return err
	}

	// Wrap provider with retry and fallback
	provider, err = ch.wrapWithRetryAndFallback(ctx, c, provider, req, namespace)
	if err != nil {
		return err
	}

	// Wrap provider with tracing
	provider = llm.NewTracingProvider(provider)

	// Start chat.request span
	tracer := tracing.Tracer("orka.chat")
	ctx, span := tracer.Start(ctx, "chat.request",
		trace.WithAttributes(
			attribute.String("session.id", sessionID),
			attribute.String(genai.AttrConversationID, sessionID),
			attribute.String("chat.provider", provider.Name()),
			attribute.String("chat.model", model),
		),
	)
	defer func() {
		if !sseMode {
			span.End()
		}
	}()

	// Build system prompt
	promptBuilder := NewSystemPromptBuilder(ch.client, namespace)
	systemPrompt, err := promptBuilder.BuildSystemPrompt(ctx, req.SystemPrompt, PromptModeFull)
	if err != nil {
		chatLog.Error(err, "failed to build system prompt")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to build system prompt")
	}

	turnID, turnDeadline, sessionCreated, turnCancelCtx, err := ch.beginChatTurn(ctx, namespace, sessionID)
	if err != nil {
		return err
	}
	stopTurnCancellation := context.AfterFunc(turnCancelCtx, cancel)
	defer stopTurnCancellation()
	finalizeTurn := sync.OnceFunc(func() {
		ch.releaseChatTurn(namespace, sessionID, turnID, sessionCreated)
		ch.finishActiveChatTurn(namespace, sessionID, turnID)
	})
	defer func() {
		if !sseMode {
			finalizeTurn()
		}
	}()

	// Load session history only after reserving its observed revision.
	messages, err := ch.loadReservedChatSession(ctx, namespace, sessionID)
	if err != nil {
		return err
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

	// Auto-route to dev-coordinator for issue/PR workflows.
	// Chat just creates the coordinator — the coordinator creates its own specialist agents.
	if req.AgentRef == "" && looksLikeIssueWorkflow(req.Message) {
		userContent = fmt.Sprintf("[System: This is an issue workflow. "+
			"You MUST create a dev-coordinator agent using create_agent with initialPrompt (one-shot pattern). "+
			"Set coordination.enabled=true, providerRef=\"copilot\", model.name=\"gpt-5.4\", maxDepth=3, maxConcurrentChildren=5. "+
			"CRITICAL: The dev-coordinator must NOT have a runtime — it must be a plain AI agent with providerRef. "+
			"The coordinator system prompt must instruct it to: "+
			"1) Use create_agent to create any specialist agents it needs (coder as copilot runtime, reviewers as copilot runtime with different models like claude-opus-4.6, gpt-5.4, gemini-3-pro). "+
			"Set allowedAgents to include the agents it creates. "+
			"2) Follow this adaptive workflow: "+
			"Phase 1 — ANALYZE: Read the issue and determine what phases are needed. "+
			"Phase 2 — PLAN & DESIGN: If needed, delegate design/planning to a coder agent, then review with multiple reviewer agents in parallel. "+
			"Phase 3 — CODE: Delegate implementation to the coder agent with a pushBranch. "+
			"Phase 4 — VALIDATE: Before review, determine the validation image and command from repository evidence such as CI workflows, toolchain files, Dockerfiles/devcontainers, Makefiles, and docs. Run validation with create_container_task on an immutable ref when available. If the environment cannot be determined confidently, report VALIDATION_CONFIG_BLOCKED. Allow up to 6 validation repair tasks before reporting VALIDATION_BLOCKED. "+
			"Phase 5 — REVIEW LOOP: Delegate parallel reviews to reviewer agents, then delegate coder repairs on the same branch until validation passes and all reviewers approve. Bound this to at most 8 review repair tasks. "+
			"Phase 6 — PR + CI LOOP: After validation passes and reviewers approve, create or update the PR, then call check_pull_request_ci once with wait_timeout=\"30m\" and poll_interval=\"30s\". If checks fail, delegate a focused CI repair task to the coder on the PR branch, then re-run validation and reviewers before re-checking CI. Bound this to at most 3 CI repair tasks; if the CI check times out while still pending, report CI_PENDING. "+
			"Phase 7 — APPROVE: Post final approval only after validation passes, reviewers approve, and CI is green. Prefer additional focused repair iterations over stopping early when reviewers identify concrete diff-backed security, correctness, or acceptance-criteria issues. Report VALIDATION_BLOCKED, REVIEW_BLOCKED, CI_BLOCKED, or CI_PENDING when a bounded loop is exhausted. Do not merge unless the user explicitly asks. "+
			"Use initialPrompt to pass the user's request so the coordinator starts immediately. "+
			"Include the gitRepo URL in the initialPrompt.]\n\n%s", req.Message)
	}
	messages = append(messages, llm.Message{
		Role:    "user",
		Content: userContent,
	})

	// Create tool executor (also creates the chat registry)
	executor := NewToolExecutor(ch.client, ch.sessionManager, namespace, sessionID, ch.watchNamespace, ch.enforceNamespaceIsolation, ch.config.MaxTasksPerTurn, ch.config.ToolTimeout, ch.resultStore, ch.kubeClient)
	executor.provider = providerInfo.Name
	executor.providerType = providerInfo.Type
	executor.SetTaskCreateAuthorizer(func(ctx context.Context, task *corev1alpha1.Task) error {
		return authorizeAndStampToolTaskCreate(ctx, ch.client, ch.kubeClient, contextToken, ch.contextTokenAuthorization, "chatToolCreateTask", userInfo, task)
	})
	executor.SetTaskDeleteAuthorizer(func(ctx context.Context, task *corev1alpha1.Task) error {
		return authorizeContextTokenTaskDeleteObject(ctx, ch.client, contextToken, ch.contextTokenAuthorization, "chatToolDeleteTask", task)
	})
	executor.SetAgentCreateAuthorizer(func(ctx context.Context, agent *corev1alpha1.Agent) error {
		return authorizeContextTokenToolAgentCreate(ctx, ch.client, contextToken, ch.contextTokenAuthorization, "chatToolCreateAgent", agent)
	})
	executor.SetAgentUpdateAuthorizer(func(ctx context.Context, agent *corev1alpha1.Agent) error {
		return authorizeContextTokenToolAgentUpdate(ctx, ch.client, contextToken, ch.contextTokenAuthorization, "chatToolUpdateAgent", agent)
	})
	executor.SetAgentDeleteAuthorizer(func(ctx context.Context, agent *corev1alpha1.Agent) error {
		return authorizeContextTokenToolAgentDelete(contextToken, ch.contextTokenAuthorization, "chatToolDeleteAgent", agent)
	})
	executor.SetSecretReadAuthorizer(func(ctx context.Context, namespace, secretName string) error {
		return authorizeContextTokenSecretRead(contextToken, ch.contextTokenAuthorization, "chatToolReadSecret", namespace, secretName)
	})

	// Build tools from the chat registry and restrict execution to the exposed set.
	tools := executor.registry.ToLLMTools(chattools.ChatToolNames())
	tools = filterCompletionToolsForContextToken(c, ch.contextTokenAuthorization, tools)
	if err := authorizeContextTokenToolUse(c, ch.contextTokenAuthorization, "chatTools", completionToolNames(tools)); err != nil {
		return err
	}
	executor.SetAllowedTools(tools)

	// Build completion request parameters
	var temperature float64
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
		content, usage, toolCalls, err := ch.runToolLoop(
			ctx, provider, messages, systemPrompt, tools, executor,
			sessionID, namespace, model, temperature, maxTokens, persistedCount, nil, turnID,
		)
		if err != nil {
			chatLog.Error(err, "tool loop error")
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("chat error: %v", err))
		}

		return c.JSON(ChatResponse{
			SessionID: sessionID,
			Message:   content,
			ToolCalls: toolCalls,
			Usage:     usage,
		})
	}

	streamErr := ch.sendChatStream(c, chatStreamRequest{
		parentCtx:      ctx,
		turnCancelCtx:  turnCancelCtx,
		turnDeadline:   turnDeadline,
		finalizeTurn:   finalizeTurn,
		provider:       provider,
		messages:       messages,
		systemPrompt:   systemPrompt,
		tools:          tools,
		executor:       executor,
		sessionID:      sessionID,
		namespace:      namespace,
		model:          model,
		temperature:    temperature,
		maxTokens:      maxTokens,
		persistedCount: persistedCount,
		turnID:         turnID,
		span:           span,
	})
	if streamErr == nil {
		sseMode = true
	}
	return streamErr
}

func (ch *ChatHandler) sendChatStream(c fiber.Ctx, req chatStreamRequest) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("Connection", "keep-alive")
	c.Set("X-Accel-Buffering", "no")

	// SendStreamWriter outlives the handler, so capture only trace and baggage
	// state from the recyclable Fiber request context.
	sseParentCtx := baggage.ContextWithBaggage(
		trace.ContextWithSpanContext(context.Background(), req.span.SpanContext()),
		baggage.FromContext(req.parentCtx),
	)
	var streamOwnership atomic.Uint32
	finalizeStream := sync.OnceFunc(func() {
		req.finalizeTurn()
		req.span.End()
		<-ch.semaphore
	})
	stopUnclaimedCancellation := context.AfterFunc(req.turnCancelCtx, func() {
		if streamOwnership.CompareAndSwap(chatStreamUnclaimed, chatStreamFinalized) {
			finalizeStream()
		}
	})
	streamWatchdog := time.AfterFunc(max(time.Until(req.turnDeadline), time.Duration(0)), func() {
		if streamOwnership.CompareAndSwap(chatStreamUnclaimed, chatStreamFinalized) {
			finalizeStream()
		}
	})

	streamErr := c.SendStreamWriter(func(w *bufio.Writer) {
		if !streamOwnership.CompareAndSwap(chatStreamUnclaimed, chatStreamOwned) {
			return
		}
		stopUnclaimedCancellation()
		streamWatchdog.Stop()
		defer finalizeStream()

		sseCtx, sseCancel := context.WithDeadline(sseParentCtx, req.turnDeadline)
		stopSSECancellation := context.AfterFunc(req.turnCancelCtx, sseCancel)
		defer stopSSECancellation()
		defer sseCancel()

		emitSSE := func(event, data string) {
			_ = writeSSE(w, event, data)
		}
		statusData, _ := json.Marshal(map[string]string{
			"sessionId": req.sessionID,
			"provider":  req.provider.Name(),
			"model":     req.model,
		})
		emitSSE("status", string(statusData))

		content, usage, _, err := ch.runToolLoop(
			sseCtx, req.provider, req.messages, req.systemPrompt, req.tools, req.executor,
			req.sessionID, req.namespace, req.model, req.temperature, req.maxTokens,
			req.persistedCount, emitSSE, req.turnID,
		)
		if err != nil {
			errData, _ := json.Marshal(map[string]string{"error": err.Error()})
			emitSSE("error", string(errData))
			return
		}

		_ = content // content already emitted via SSE
		doneData, _ := json.Marshal(map[string]any{"usage": usage})
		emitSSE("done", string(doneData))
	})
	if streamErr != nil {
		stopUnclaimedCancellation()
		streamWatchdog.Stop()
	}
	return streamErr
}

func (ch *ChatHandler) beginChatTurn(
	ctx context.Context,
	namespace, sessionID string,
) (string, time.Time, bool, context.Context, error) {
	key := chatTurnKey{namespace: namespace, sessionID: sessionID}
	ch.activeChatTurnsMu.Lock()
	defer ch.activeChatTurnsMu.Unlock()
	if ch.activeChatTurns == nil {
		ch.activeChatTurns = make(map[chatTurnKey]*activeChatTurn)
	}
	if active := ch.activeChatTurns[key]; active != nil {
		if active.turnID == "" || active.cancellers > 0 || active.expiresAt.After(time.Now().UTC()) {
			return "", time.Time{}, false, nil, fiber.NewError(fiber.StatusConflict, "chat session is busy")
		}
		delete(ch.activeChatTurns, key)
		active.cancel()
		close(active.done)
	}

	turnID, turnDeadline, sessionCreated, err := ch.reserveChatTurn(ctx, namespace, sessionID)
	if err != nil {
		return "", time.Time{}, false, nil, err
	}
	turnCancelCtx, cancel := context.WithCancel(context.Background())
	ch.activeChatTurns[key] = &activeChatTurn{
		turnID:    turnID,
		cancel:    cancel,
		done:      make(chan struct{}),
		expiresAt: turnDeadline.Add(chatDurabilityTimeout),
	}
	return turnID, turnDeadline, sessionCreated, turnCancelCtx, nil
}

func (ch *ChatHandler) reserveChatTurn(
	ctx context.Context,
	namespace, sessionID string,
) (string, time.Time, bool, error) {
	if ch.sessionTurnCommitter == nil {
		return "", time.Time{}, false, fiber.NewError(
			fiber.StatusInternalServerError,
			"session store does not support atomic chat turns",
		)
	}

	turnLifetime := ch.config.MaxDuration
	if turnLifetime <= 0 {
		turnLifetime = 5 * time.Minute
	}
	turnID := fmt.Sprintf("chat-turn-%s", generateChatID())
	now := time.Now().UTC()
	turnDeadline := now.Add(turnLifetime)
	created, err := ch.sessionTurnCommitter.AcquireChatTurn(ctx, &store.SessionRecord{
		Namespace:   namespace,
		Name:        sessionID,
		SessionType: store.SessionTypeChat,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, turnID, turnDeadline.Add(chatDurabilityTimeout))
	if errors.Is(err, store.ErrGatewayOwnedSession) {
		return "", time.Time{}, false, fiber.NewError(fiber.StatusNotFound, "chat session not found")
	}
	if errors.Is(err, store.ErrConflict) {
		return "", time.Time{}, false, fiber.NewError(fiber.StatusConflict, "chat session is busy")
	}
	if err != nil {
		return "", time.Time{}, false, fiber.NewError(
			fiber.StatusInternalServerError,
			fmt.Sprintf("failed to reserve chat session: %v", err),
		)
	}
	return turnID, turnDeadline, created, nil
}

func (ch *ChatHandler) releaseChatTurn(namespace, sessionID, turnID string, deleteEmptyCreatedSession bool) {
	if ch.sessionTurnCommitter == nil || turnID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), chatDurabilityTimeout)
	defer cancel()
	if err := ch.sessionTurnCommitter.ReleaseChatTurn(
		ctx,
		namespace,
		sessionID,
		turnID,
		deleteEmptyCreatedSession,
	); err != nil {
		chatLog.Error(err, "failed to release chat turn", "namespace", namespace, "sessionId", sessionID)
	}
}

func (ch *ChatHandler) finishActiveChatTurn(namespace, sessionID, turnID string) {
	key := chatTurnKey{namespace: namespace, sessionID: sessionID}

	ch.activeChatTurnsMu.Lock()
	active := ch.activeChatTurns[key]
	if active == nil || active.turnID != turnID {
		ch.activeChatTurnsMu.Unlock()
		return
	}
	cancel := active.cancel
	done := active.done
	if active.cancellers > 0 {
		active.turnID = ""
		active.cancel = nil
		active.done = nil
	} else {
		delete(ch.activeChatTurns, key)
	}
	ch.activeChatTurnsMu.Unlock()

	cancel()
	close(done)
}

func (ch *ChatHandler) startSessionCancellation(namespace, sessionID string) (<-chan struct{}, bool) {
	key := chatTurnKey{namespace: namespace, sessionID: sessionID}

	ch.activeChatTurnsMu.Lock()
	if ch.activeChatTurns == nil {
		ch.activeChatTurns = make(map[chatTurnKey]*activeChatTurn)
	}
	active := ch.activeChatTurns[key]
	if active == nil {
		active = &activeChatTurn{}
		ch.activeChatTurns[key] = active
	}
	active.cancellers++
	cancel := active.cancel
	done := active.done
	hasActiveTurn := active.turnID != ""
	ch.activeChatTurnsMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return done, hasActiveTurn
}

func (ch *ChatHandler) finishSessionCancellation(namespace, sessionID string) {
	key := chatTurnKey{namespace: namespace, sessionID: sessionID}

	ch.activeChatTurnsMu.Lock()
	defer ch.activeChatTurnsMu.Unlock()
	active := ch.activeChatTurns[key]
	if active == nil || active.cancellers == 0 {
		return
	}
	active.cancellers--
	if active.cancellers == 0 && active.turnID == "" {
		delete(ch.activeChatTurns, key)
	}
}

func (ch *ChatHandler) loadReservedChatSession(
	ctx context.Context,
	namespace, sessionID string,
) ([]llm.Message, error) {
	messages, err := ch.loadChatSession(ctx, namespace, sessionID)
	if errors.Is(err, store.ErrGatewayOwnedSession) {
		return nil, fiber.NewError(fiber.StatusNotFound, "chat session not found")
	}
	if err != nil {
		return nil, fiber.NewError(
			fiber.StatusInternalServerError,
			fmt.Sprintf("failed to load chat session: %v", err),
		)
	}
	return messages, nil
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
	turnID string,
) (string, ChatUsage, []ToolCallInfo, error) {
	var usage ChatUsage
	var allToolCalls []ToolCallInfo
	repetitionTracker := make(map[string]int)
	start := time.Now()

	for iteration := 0; ; iteration++ {
		iterTracer := tracing.Tracer("orka.chat")
		iterCtx, iterSpan := iterTracer.Start(ctx, "chat.tool_loop.iteration",
			trace.WithAttributes(
				attribute.Int("chat.iteration", iteration),
				attribute.String(tracing.AttrTenant, namespace),
				attribute.String(genai.AttrRequestModel, model),
			),
		)

		select {
		case <-iterCtx.Done():
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			err := fmt.Errorf("chat turn interrupted: %w", iterCtx.Err())
			iterSpan.RecordError(err)
			iterSpan.SetStatus(codes.Error, err.Error())
			iterSpan.End()
			return "", usage, allToolCalls, err
		default:
		}

		if content, hit, err := ch.handleIterationLimit(
			iterCtx, iteration, provider, messages, systemPrompt, model, namespace, sessionID,
			maxTokens, persistedCount, temperature, emitSSE, executor, &usage, turnID, start,
		); hit {
			setUsageSpanAttributes(iterSpan, usage)
			if err != nil {
				iterSpan.RecordError(err)
				iterSpan.SetStatus(codes.Error, err.Error())
			}
			iterSpan.End()
			return content, usage, allToolCalls, err
		}

		if iteration > 0 && iteration%5 == 0 {
			messages = append(messages, llm.Message{
				Role:    "user",
				Content: "[System: Progress check — summarize what you've done so far and what remains.]",
			})
		}

		if ch.config.MaxSessionSize > 0 {
			messages = llm.TruncateMessages(messages, ch.config.MaxSessionSize/4)
		}

		resp, updatedMsgs, err := ch.callLLMWithRetry(iterCtx, provider, messages, systemPrompt, model, tools, maxTokens, temperature)
		messages = updatedMsgs
		if err != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			iterSpan.RecordError(err)
			iterSpan.SetStatus(codes.Error, err.Error())
			iterSpan.End()
			return "", usage, allToolCalls, fmt.Errorf("LLM completion failed: %w", err)
		}
		iterSpan.SetAttributes(attribute.Int("chat.tool_call_count", len(resp.ToolCalls)))
		usage.LLMCalls++
		usage.InputTokens += resp.InputTokens
		usage.OutputTokens += resp.OutputTokens

		if len(resp.ToolCalls) == 0 {
			// Check if any tasks created in this session are still running.
			// If so, re-prompt the LLM to keep waiting instead of ending the session.
			if executor.tasksCreated > 0 && ch.hasRunningTasks(iterCtx, namespace, sessionID) {
				if emitSSE != nil && resp.Content != "" {
					msgData, _ := json.Marshal(map[string]string{"content": resp.Content})
					emitSSE("message", string(msgData))
				}
				messages = append(messages,
					llm.Message{Role: "assistant", Content: resp.Content},
					llm.Message{Role: "user", Content: "[System: You have tasks still running. Do NOT stop. Call wait_for_task again for each running task until it reaches Succeeded or Failed, then call fetch_task_output to get the result.]"},
				)
				// Don't increment iteration here — the for loop's post-statement handles it
				iterSpan.End()
				continue
			}
			content, err := ch.handleFinalResponse(
				iterCtx, resp.Content, messages, namespace, sessionID, persistedCount,
				emitSSE, executor, &usage, turnID, start,
			)
			setUsageSpanAttributes(iterSpan, usage)
			if err != nil {
				iterSpan.RecordError(err)
				iterSpan.SetStatus(codes.Error, err.Error())
			}
			iterSpan.End()
			return content, usage, allToolCalls, err
		}

		var newToolCalls []ToolCallInfo
		var iterBump int
		messages, newToolCalls, iterBump = ch.executeToolCalls(iterCtx, resp, executor, emitSSE, messages, repetitionTracker)
		allToolCalls = append(allToolCalls, newToolCalls...)
		usage.ToolCalls += len(newToolCalls)
		iteration += iterBump
		iterSpan.End()
	}
}

// handleIterationLimit checks if the iteration limit is reached and if so,
// injects a termination prompt, makes a final LLM call, emits SSE, and saves the session.
func (ch *ChatHandler) handleIterationLimit(
	ctx context.Context,
	iteration int,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt, model, namespace, sessionID string,
	maxTokens, persistedCount int,
	temperature float64,
	emitSSE func(event, data string),
	executor *ToolExecutor,
	usage *ChatUsage,
	turnID string,
	start time.Time,
) (string, bool, error) {
	if iteration < ch.config.MaxIterations {
		return "", false, nil
	}

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
		return "", true, fmt.Errorf("final LLM completion after iteration limit failed: %w", err)
	}
	usage.LLMCalls++
	usage.InputTokens += resp.InputTokens
	usage.OutputTokens += resp.OutputTokens

	finalMessages := append(messages, llm.Message{Role: "assistant", Content: resp.Content})
	usage.Duration = time.Since(start).Round(time.Millisecond).String()
	usage.TasksCreated = executor.tasksCreated
	if err := ch.saveChatSession(
		ctx, namespace, sessionID, finalMessages, persistedCount, *usage, turnID,
	); err != nil {
		return "", true, fmt.Errorf("failed to commit chat turn: %w", err)
	}

	if emitSSE != nil && resp.Content != "" {
		msgData, _ := json.Marshal(map[string]string{"content": resp.Content})
		emitSSE("message", string(msgData))
	}

	return resp.Content, true, nil
}

// callLLMWithRetry calls the LLM provider and retries once with truncated messages
// if the context is too long.
func (ch *ChatHandler) callLLMWithRetry(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt, model string,
	tools []llm.Tool,
	maxTokens int,
	temperature float64,
) (*llm.CompletionResponse, []llm.Message, error) {
	resp, err := provider.Complete(ctx, &llm.CompletionRequest{
		Model:        model,
		SystemPrompt: systemPrompt,
		Messages:     messages,
		Tools:        tools,
		MaxTokens:    maxTokens,
		Temperature:  temperature,
	})
	if err != nil && llm.IsContextTooLongErr(err) {
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
	return resp, messages, err
}

// executeToolCalls iterates over tool calls from the LLM response, emits SSE events,
// executes each tool, tracks repetitions, and appends results to messages.
func (ch *ChatHandler) executeToolCalls(
	ctx context.Context,
	resp *llm.CompletionResponse,
	executor *ToolExecutor,
	emitSSE func(event, data string),
	messages []llm.Message,
	repetitionTracker map[string]int,
) ([]llm.Message, []ToolCallInfo, int) {
	messages = append(messages, llm.Message{
		Role:      "assistant",
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	})

	toolCalls := make([]ToolCallInfo, 0, len(resp.ToolCalls))
	var iterationBump int
	var repetitionWarning string

	for _, tc := range resp.ToolCalls {
		if emitSSE != nil {
			tcData, _ := json.Marshal(map[string]any{
				"id":   tc.ID,
				"name": tc.Name,
				"args": tc.Arguments,
			})
			emitSSE("tool_call", string(tcData))
		}

		argsHash := hashArgs(tc.Name, tc.Arguments)
		repetitionTracker[argsHash]++
		if repetitionTracker[argsHash] >= 3 {
			repetitionWarning = fmt.Sprintf("[System: Warning — you have called %s with the same arguments %d times. Try a different approach.]", tc.Name, repetitionTracker[argsHash])
			iterationBump += 5
		}

		result, execErr := executor.Execute(ctx, tc)
		if execErr != nil {
			errResult := map[string]any{"success": false, "error": execErr.Error()}
			if errJSON, jsonErr := json.Marshal(errResult); jsonErr == nil {
				result = string(errJSON)
			} else {
				result = `{"success":false,"error":"tool execution failed"}`
			}
		}

		if emitSSE != nil {
			trData, _ := json.Marshal(map[string]any{
				"id":     tc.ID,
				"name":   tc.Name,
				"result": json.RawMessage(result),
			})
			emitSSE("tool_result", string(trData))
		}

		messages = append(messages, llm.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    result,
		})

		var argsAny any
		_ = json.Unmarshal(tc.Arguments, &argsAny)
		var resultAny any
		_ = json.Unmarshal([]byte(result), &resultAny)
		toolCalls = append(toolCalls, ToolCallInfo{
			Name:   tc.Name,
			Args:   argsAny,
			Result: resultAny,
		})
	}

	if repetitionWarning != "" {
		messages = append(messages, llm.Message{
			Role:    "user",
			Content: repetitionWarning,
		})
	}

	return messages, toolCalls, iterationBump
}

// handleFinalResponse emits the final SSE message and saves the chat session.
func (ch *ChatHandler) handleFinalResponse(
	ctx context.Context,
	content string,
	messages []llm.Message,
	namespace, sessionID string,
	persistedCount int,
	emitSSE func(event, data string),
	executor *ToolExecutor,
	usage *ChatUsage,
	turnID string,
	start time.Time,
) (string, error) {
	finalMessages := append(messages, llm.Message{Role: "assistant", Content: content})
	usage.Duration = time.Since(start).Round(time.Millisecond).String()
	usage.TasksCreated = executor.tasksCreated
	if err := ch.saveChatSession(
		ctx, namespace, sessionID, finalMessages, persistedCount, *usage, turnID,
	); err != nil {
		return "", fmt.Errorf("failed to commit chat turn: %w", err)
	}

	if emitSSE != nil && content != "" {
		msgData, _ := json.Marshal(map[string]string{"content": content})
		emitSSE("message", string(msgData))
	}

	return content, nil
}

func setUsageSpanAttributes(span trace.Span, usage ChatUsage) {
	span.SetAttributes(
		attribute.Int("chat.llm_calls", usage.LLMCalls),
		attribute.Int("chat.input_tokens", usage.InputTokens),
		attribute.Int("chat.output_tokens", usage.OutputTokens),
	)
}

// loadChatSession loads chat session messages from the session store.
func (ch *ChatHandler) loadChatSession(ctx context.Context, namespace, sessionID string) ([]llm.Message, error) {
	sessionType, err := transcriptSessionType(ctx, ch.sessionStore, namespace, sessionID)
	if err != nil {
		return nil, err
	}
	if sessionType == store.SessionTypeGateway {
		return nil, store.ErrGatewayOwnedSession
	}
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

// saveChatSession atomically commits new transcript messages and their usage.
func (ch *ChatHandler) saveChatSession(
	ctx context.Context,
	namespace, sessionID string,
	messages []llm.Message,
	persistedCount int,
	usage ChatUsage,
	turnID string,
) error {
	if persistedCount < 0 || persistedCount > len(messages) {
		return fmt.Errorf("invalid persisted message count %d for %d messages", persistedCount, len(messages))
	}

	newMessages := messages[persistedCount:]
	if len(newMessages) == 0 {
		if usage.InputTokens != 0 || usage.OutputTokens != 0 {
			return fmt.Errorf("cannot commit token usage without new transcript messages")
		}
		return nil
	}

	storeMessages := make([]store.SessionMessage, 0, len(newMessages))
	now := time.Now().UTC()
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

	if ch.sessionTurnCommitter == nil {
		return fmt.Errorf("session store does not support atomic chat turns")
	}

	// The provider and tool loop remain bound to the request/turn context, but
	// once a final response exists its atomic persistence owns the durability
	// handoff. Detach cancellation here and bound the commit to the same grace
	// window added to the chat-turn lease.
	durabilityCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatDurabilityTimeout)
	defer cancel()
	return ch.sessionTurnCommitter.CommitSessionTurn(
		durabilityCtx,
		&store.SessionRecord{
			Namespace:   namespace,
			Name:        sessionID,
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		turnID,
		persistedCount,
		storeMessages,
		usage.InputTokens,
		usage.OutputTokens,
	)
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

	namespace, err := ResolveNamespace(c, c.Query("namespace", ""), ch.watchNamespace, ch.enforceNamespaceIsolation)
	if err != nil {
		return err
	}
	if err := authorizeContextTokenActionWithConfig(c, ch.contextTokenAuthorization, "cancelChat", ch.contextTokenAuthorization.SessionWriteScopes); err != nil {
		return err
	}

	ctx := c.Context()
	sessionType, err := transcriptSessionType(ctx, ch.sessionStore, namespace, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "chat session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session type: %v", err))
	}
	if sessionType == store.SessionTypeGateway {
		return fiber.NewError(fiber.StatusNotFound, "chat session not found")
	}
	done, active := ch.startSessionCancellation(namespace, sessionID)
	defer ch.finishSessionCancellation(namespace, sessionID)
	if active {
		waitCtx, waitCancel := context.WithTimeout(c.Context(), chatDurabilityTimeout)
		defer waitCancel()
		select {
		case <-done:
		case <-waitCtx.Done():
			return fiber.NewError(fiber.StatusGatewayTimeout, "timed out waiting for active chat turn to stop")
		}
	}

	// Delete the session to cancel it
	if err := ch.sessionStore.DeleteSession(ctx, namespace, sessionID); err != nil {
		if errors.Is(err, store.ErrGatewayOwnedSession) {
			return fiber.NewError(fiber.StatusNotFound, "chat session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to cancel session: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// wrapWithRetryAndFallback wraps a provider with retry logic and adds fallback
// providers if the agent has them configured.
func (ch *ChatHandler) wrapWithRetryAndFallback(ctx context.Context, c fiber.Ctx, provider llm.Provider, req ChatRequest, namespace string) (llm.Provider, error) {
	var resultProvider llm.Provider = llm.NewRetryProvider(provider, 0)

	if req.AgentRef == "" {
		return resultProvider, nil
	}

	agent := &corev1alpha1.Agent{}
	if err := ch.client.Get(ctx, types.NamespacedName{Name: req.AgentRef, Namespace: namespace}, agent); err != nil {
		return resultProvider, nil
	}
	if agent.Spec.Model == nil || len(agent.Spec.Model.Fallbacks) == 0 {
		return resultProvider, nil
	}

	fallbacks := make([]llm.FallbackEntry, 0, len(agent.Spec.Model.Fallbacks))
	for _, fb := range agent.Spec.Model.Fallbacks {
		fbProviderCRD, err := ch.resolver.LookupProvider(ctx, fb.ProviderRef, namespace)
		if err != nil {
			continue
		}

		fbProviderInfo := providerResolutionInfo(fbProviderCRD)
		fbModel := strings.TrimSpace(fb.Model)
		if fbModel == "" {
			fbModel = fbProviderCRD.Spec.DefaultModel
		}
		if err := authorizeContextTokenProviderUse(c, ch.contextTokenAuthorization, "chatFallback", namespace, fbProviderInfo, fbModel); err != nil {
			return resultProvider, err
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
			Model:    fbModel,
		})
	}

	if len(fallbacks) > 0 {
		fp := llm.NewFallbackProvider(resultProvider, fallbacks)
		fp.SetCooldownTracker(ch.cooldownTracker)
		resultProvider = fp
	}

	return resultProvider, nil
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

// hasRunningTasks checks if any tasks created by this chat session are still running.
func (ch *ChatHandler) hasRunningTasks(ctx context.Context, namespace, sessionID string) bool {
	var taskList corev1alpha1.TaskList
	if err := ch.client.List(ctx, &taskList,
		client.InNamespace(namespace),
		client.MatchingLabels{labels.LabelChatSession: sessionID},
	); err != nil {
		return false
	}
	for i := range taskList.Items {
		switch taskList.Items[i].Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
			continue
		case corev1alpha1.TaskPhaseScheduled:
			// A recurring parent stays Scheduled by design and never completes.
			continue
		default:
			return true
		}
	}
	return false
}

// looksLikeIssueWorkflow checks if the user message contains patterns suggesting
// a GitHub issue implementation workflow (fix, implement, create PR).
// Uses word-boundary matching to avoid false positives from substrings.
func looksLikeIssueWorkflow(message string) bool {
	lower := strings.ToLower(message)
	// Must mention a GitHub issue
	hasIssue := strings.Contains(lower, "/issues/") ||
		strings.Contains(lower, "issue #") ||
		strings.Contains(lower, "issue#")
	if !hasIssue {
		return false
	}
	// Must suggest implementation work — use multi-word phrases or word boundaries
	// to avoid false positives (e.g., "pr" in "problem", "fix" in "prefix")
	actionPhrases := []string{"pick up", "work on", "pull request", "create a pr", "open a pr", "make a pr"}
	for _, phrase := range actionPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	// Single-word actions need word boundary checks
	actionWordsPattern := regexp.MustCompile(`\b(fix|implement|resolve|address)\b`)
	return actionWordsPattern.MatchString(lower)
}
