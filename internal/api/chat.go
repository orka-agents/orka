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

type sseEmitter func(event, data string) error

type chatTurnCommitState struct {
	turnID            string
	persistedMessages int
	committedInput    int
	committedOutput   int
}

type toolResultSSE struct {
	id     string
	name   string
	result string
}

type chatToolBatch struct {
	response          *llm.CompletionResponse
	executor          *ToolExecutor
	messages          []llm.Message
	modelMessages     []llm.Message
	repetitionTracker map[string]int
	namespace         string
	sessionID         string
	usage             *ChatUsage
	turnState         *chatTurnCommitState
	emitSSE           sseEmitter
}

type chatToolBatchResult struct {
	messages      []llm.Message
	modelMessages []llm.Message
	toolCalls     []ToolCallInfo
	iterationBump int
}

type chatSSEStream struct {
	parentCtx      context.Context
	provider       llm.Provider
	messages       []llm.Message
	systemPrompt   string
	tools          []llm.Tool
	executor       *ToolExecutor
	sessionID      string
	turnID         string
	deadline       time.Time
	namespace      string
	model          string
	temperature    float64
	maxOutput      int
	persistedCount int
	span           trace.Span
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
}

// NewChatHandler creates a new ChatHandler.
func NewChatHandler(c client.Client, sm *controller.SessionManager, config ChatConfig, watchNamespace string, enforceNS bool, ss store.SessionStore, rs store.ResultStore, resolver *ProviderResolver, kubeClientOpt ...kubernetes.Interface) *ChatHandler {
	var kubeClient kubernetes.Interface
	if len(kubeClientOpt) > 0 {
		kubeClient = kubeClientOpt[0]
	}

	turnCommitter, _ := ss.(store.SessionTurnCommitter)

	return &ChatHandler{
		client:                    c,
		kubeClient:                kubeClient,
		sessionManager:            sm,
		config:                    config,
		semaphore:                 make(chan struct{}, config.MaxConcurrent),
		watchNamespace:            watchNamespace,
		enforceNamespaceIsolation: enforceNS,
		sessionStore:              ss,
		sessionTurnCommitter:      turnCommitter,
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

	turnID, turnDeadline, err := ch.reserveChatTurn(ctx, namespace, sessionID)
	if err != nil {
		return err
	}
	defer func() {
		if !sseMode {
			ch.releaseChatTurn(namespace, sessionID, turnID)
		}
	}()

	// Load session history only after reserving its revision for this turn.
	messages, err := ch.loadChatSession(ctx, namespace, sessionID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load chat session: %v", err))
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
		content, usage, toolCalls, err := ch.runToolLoop(ctx, provider, messages, systemPrompt, tools, executor, sessionID, namespace, model, temperature, maxTokens, persistedCount, nil, turnID)
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
	sseParentCtx := baggage.ContextWithBaggage(
		trace.ContextWithSpanContext(context.Background(), span.SpanContext()),
		baggage.FromContext(ctx),
	)

	stream := chatSSEStream{
		parentCtx:      sseParentCtx,
		provider:       sseProvider,
		messages:       sseMessages,
		systemPrompt:   sseSystemPrompt,
		tools:          sseTools,
		executor:       sseExecutor,
		sessionID:      sessionID,
		turnID:         turnID,
		deadline:       turnDeadline,
		namespace:      namespace,
		model:          model,
		temperature:    temperature,
		maxOutput:      maxTokens,
		persistedCount: persistedCount,
		span:           span,
	}
	streamErr := c.SendStreamWriter(func(w *bufio.Writer) {
		ch.writeChatSSEStream(w, stream)
	})
	if streamErr == nil {
		sseMode = true
	}
	return streamErr
}

func (ch *ChatHandler) reserveChatTurn(ctx context.Context, namespace, sessionID string) (string, time.Time, error) {
	if ch.sessionTurnCommitter == nil {
		return "", time.Time{}, fiber.NewError(fiber.StatusInternalServerError, "session store does not support atomic chat turns")
	}

	turnLifetime := ch.config.MaxDuration
	if turnLifetime <= 0 {
		turnLifetime = 5 * time.Minute
	}
	turnID := fmt.Sprintf("chat-turn-%s", generateChatID())
	now := time.Now()
	turnDeadline := now.Add(turnLifetime)
	err := ch.sessionTurnCommitter.AcquireChatTurn(ctx, &store.SessionRecord{
		Namespace:   namespace,
		Name:        sessionID,
		SessionType: "chat",
		CreatedAt:   now,
		UpdatedAt:   now,
	}, turnID, turnDeadline.Add(chatDurabilityTimeout))
	if errors.Is(err, store.ErrConflict) {
		return "", time.Time{}, fiber.NewError(fiber.StatusConflict, "chat session is busy")
	}
	if err != nil {
		return "", time.Time{}, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to reserve chat session: %v", err))
	}
	return turnID, turnDeadline, nil
}

func (ch *ChatHandler) writeChatSSEStream(w *bufio.Writer, stream chatSSEStream) {
	defer stream.span.End()
	defer func() { <-ch.semaphore }()
	defer ch.releaseChatTurn(stream.namespace, stream.sessionID, stream.turnID)

	sseCtx, sseCancel := context.WithDeadline(stream.parentCtx, stream.deadline)
	defer sseCancel()

	emitSSE := func(event, data string) error {
		if err := writeSSE(w, event, data); err != nil {
			sseCancel()
			return fmt.Errorf("failed to emit SSE %s event: %w", event, err)
		}
		return nil
	}

	statusData, _ := json.Marshal(map[string]string{
		"sessionId": stream.sessionID,
		"provider":  stream.provider.Name(),
		"model":     stream.model,
	})
	if err := emitSSE("status", string(statusData)); err != nil {
		stream.span.RecordError(err)
		stream.span.SetStatus(codes.Error, err.Error())
		return
	}

	_, usage, _, err := ch.runToolLoop(
		sseCtx,
		stream.provider,
		stream.messages,
		stream.systemPrompt,
		stream.tools,
		stream.executor,
		stream.sessionID,
		stream.namespace,
		stream.model,
		stream.temperature,
		stream.maxOutput,
		stream.persistedCount,
		emitSSE,
		stream.turnID,
	)
	if err != nil {
		stream.span.RecordError(err)
		stream.span.SetStatus(codes.Error, err.Error())
		errData, _ := json.Marshal(map[string]string{"error": err.Error()})
		_ = emitSSE("error", string(errData))
		return
	}

	doneData, _ := json.Marshal(map[string]any{"usage": usage})
	if err := emitSSE("done", string(doneData)); err != nil {
		stream.span.RecordError(err)
		stream.span.SetStatus(codes.Error, err.Error())
	}
}

func (ch *ChatHandler) releaseChatTurn(namespace, sessionID, turnID string) {
	if turnID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), chatDurabilityTimeout)
	defer cancel()
	if ch.sessionTurnCommitter == nil {
		return
	}
	if err := ch.sessionTurnCommitter.ReleaseChatTurn(ctx, namespace, sessionID, turnID); err != nil {
		chatLog.Error(err, "failed to release chat turn", "namespace", namespace, "sessionId", sessionID)
	}
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
	emitSSE sseEmitter,
	turnIDs ...string,
) (string, ChatUsage, []ToolCallInfo, error) {
	var usage ChatUsage
	var allToolCalls []ToolCallInfo
	repetitionTracker := make(map[string]int)
	start := time.Now()
	turnState := chatTurnCommitState{persistedMessages: persistedCount}
	if len(turnIDs) > 0 {
		turnState.turnID = turnIDs[0]
	}
	modelMessages := append([]llm.Message(nil), messages...)

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

		if content, hit, err := ch.handleIterationLimit(iterCtx, iteration, provider, messages, modelMessages, systemPrompt, model, namespace, sessionID, maxTokens, temperature, emitSSE, executor, &usage, &turnState, start); hit {
			setUsageSpanAttributes(iterSpan, usage)
			if err != nil {
				iterSpan.RecordError(err)
				iterSpan.SetStatus(codes.Error, err.Error())
			}
			iterSpan.End()
			return content, usage, allToolCalls, err
		}

		if iteration > 0 && iteration%5 == 0 {
			progressMessage := llm.Message{
				Role:    "user",
				Content: "[System: Progress check — summarize what you've done so far and what remains.]",
			}
			messages = append(messages, progressMessage)
			modelMessages = append(modelMessages, progressMessage)
		}

		if ch.config.MaxSessionSize > 0 {
			modelMessages = llm.TruncateMessages(modelMessages, ch.config.MaxSessionSize/4)
		}

		resp, updatedModelMessages, err := ch.callLLMWithRetry(iterCtx, provider, modelMessages, systemPrompt, model, tools, maxTokens, temperature)
		modelMessages = updatedModelMessages
		if err != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			err = fmt.Errorf("LLM completion failed: %w", err)
			iterSpan.RecordError(err)
			iterSpan.SetStatus(codes.Error, err.Error())
			iterSpan.End()
			return "", usage, allToolCalls, err
		}
		iterSpan.SetAttributes(attribute.Int("chat.tool_call_count", len(resp.ToolCalls)))
		usage.LLMCalls++
		usage.InputTokens += resp.InputTokens
		usage.OutputTokens += resp.OutputTokens

		if len(resp.ToolCalls) == 0 {
			// Check if any tasks created in this session are still running.
			// If so, re-prompt the LLM to keep waiting instead of ending the session.
			if executor.tasksCreated > 0 && ch.hasRunningTasks(iterCtx, namespace, sessionID) {
				progressMessages := []llm.Message{
					{Role: "assistant", Content: resp.Content},
					{Role: "user", Content: "[System: You have tasks still running. Do NOT stop. Call wait_for_task again for each running task until it reaches Succeeded or Failed, then call fetch_task_output to get the result.]"},
				}
				messages = append(messages, progressMessages...)
				modelMessages = append(modelMessages, progressMessages...)
				if err := ch.commitChatProgress(iterCtx, namespace, sessionID, messages, &turnState, usage); err != nil {
					usage.Duration = time.Since(start).Round(time.Millisecond).String()
					err = fmt.Errorf("failed to checkpoint chat progress: %w", err)
					iterSpan.RecordError(err)
					iterSpan.SetStatus(codes.Error, err.Error())
					iterSpan.End()
					return "", usage, allToolCalls, err
				}
				if emitSSE != nil && resp.Content != "" {
					msgData, _ := json.Marshal(map[string]string{"content": resp.Content})
					if err := emitSSE("message", string(msgData)); err != nil {
						usage.Duration = time.Since(start).Round(time.Millisecond).String()
						err = fmt.Errorf("failed to emit progress message: %w", err)
						iterSpan.RecordError(err)
						iterSpan.SetStatus(codes.Error, err.Error())
						iterSpan.End()
						return "", usage, allToolCalls, err
					}
				}
				iterSpan.End()
				continue
			}
			content, err := ch.handleFinalResponse(iterCtx, resp.Content, messages, namespace, sessionID, emitSSE, executor, &usage, &turnState, start)
			setUsageSpanAttributes(iterSpan, usage)
			if err != nil {
				iterSpan.RecordError(err)
				iterSpan.SetStatus(codes.Error, err.Error())
			}
			iterSpan.End()
			return content, usage, allToolCalls, err
		}

		if err := emitToolCallEvents(resp, emitSSE); err != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			iterSpan.RecordError(err)
			iterSpan.SetStatus(codes.Error, err.Error())
			iterSpan.End()
			return "", usage, allToolCalls, err
		}

		pendingTools := pendingToolCheckpointMessage(resp.ToolCalls)
		messages = append(messages, pendingTools)
		modelMessages = append(modelMessages, pendingTools)
		if err := ch.commitChatProgress(iterCtx, namespace, sessionID, messages, &turnState, usage); err != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			err = fmt.Errorf("failed to persist tool intent: %w", err)
			iterSpan.RecordError(err)
			iterSpan.SetStatus(codes.Error, err.Error())
			iterSpan.End()
			return "", usage, allToolCalls, err
		}

		batchResult, batchErr := ch.executeToolCalls(iterCtx, chatToolBatch{
			response:          resp,
			executor:          executor,
			messages:          messages,
			modelMessages:     modelMessages,
			repetitionTracker: repetitionTracker,
			namespace:         namespace,
			sessionID:         sessionID,
			usage:             &usage,
			turnState:         &turnState,
			emitSSE:           emitSSE,
		})
		messages = batchResult.messages
		modelMessages = batchResult.modelMessages
		allToolCalls = append(allToolCalls, batchResult.toolCalls...)
		if batchErr != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			iterSpan.RecordError(batchErr)
			iterSpan.SetStatus(codes.Error, batchErr.Error())
			iterSpan.End()
			return "", usage, allToolCalls, batchErr
		}
		iteration += batchResult.iterationBump
		iterSpan.End()
	}
}

// handleIterationLimit checks if the iteration limit is reached and, if so,
// makes one final summary call and durably commits that response.
func (ch *ChatHandler) handleIterationLimit(
	ctx context.Context,
	iteration int,
	provider llm.Provider,
	messages, modelMessages []llm.Message,
	systemPrompt, model, namespace, sessionID string,
	limit int,
	temperature float64,
	emitSSE sseEmitter,
	executor *ToolExecutor,
	usage *ChatUsage,
	turnState *chatTurnCommitState,
	start time.Time,
) (string, bool, error) {
	if iteration < ch.config.MaxIterations {
		return "", false, nil
	}

	terminationMessage := llm.Message{
		Role:    "user",
		Content: "[System: You have reached the maximum number of iterations. Please provide a final summary of what you accomplished.]",
	}
	messages = append(messages, terminationMessage)
	modelMessages = append(modelMessages, terminationMessage)

	resp, err := provider.Complete(ctx, &llm.CompletionRequest{
		Model:        model,
		SystemPrompt: systemPrompt,
		Messages:     modelMessages,
		MaxTokens:    limit,
		Temperature:  temperature,
	})
	if err != nil {
		usage.Duration = time.Since(start).Round(time.Millisecond).String()
		return "", true, fmt.Errorf("final LLM completion after iteration limit failed: %w", err)
	}
	usage.LLMCalls++
	usage.InputTokens += resp.InputTokens
	usage.OutputTokens += resp.OutputTokens

	content, err := ch.handleFinalResponse(ctx, resp.Content, messages, namespace, sessionID, emitSSE, executor, usage, turnState, start)
	return content, true, err
}

// callLLMWithRetry calls the LLM provider and retries once with truncated messages
// if the context is too long.
func (ch *ChatHandler) callLLMWithRetry(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt, model string,
	tools []llm.Tool,
	limit int,
	temperature float64,
) (*llm.CompletionResponse, []llm.Message, error) {
	resp, err := provider.Complete(ctx, &llm.CompletionRequest{
		Model:        model,
		SystemPrompt: systemPrompt,
		Messages:     messages,
		Tools:        tools,
		MaxTokens:    limit,
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
			MaxTokens:    limit,
			Temperature:  temperature,
		})
	}
	return resp, messages, err
}

func pendingToolCheckpointMessage(toolCalls []llm.ToolCall) llm.Message {
	payload, err := json.Marshal(toolCalls)
	if err != nil {
		payload = []byte(`[]`)
	}
	return llm.Message{
		Role: "user",
		Content: "[System: The following tool calls are durably pending, including their original arguments: " + string(payload) +
			". A matching assistant/tool result sequence immediately after this note means they completed. If results are absent, inspect existing side effects before retrying.]",
	}
}

func emitToolCallEvents(resp *llm.CompletionResponse, emitSSE sseEmitter) error {
	if emitSSE == nil {
		return nil
	}
	for _, tc := range resp.ToolCalls {
		tcData, err := json.Marshal(map[string]any{
			"id":   tc.ID,
			"name": tc.Name,
			"args": tc.Arguments,
		})
		if err != nil {
			return fmt.Errorf("failed to marshal tool_call event: %w", err)
		}
		if err := emitSSE("tool_call", string(tcData)); err != nil {
			return fmt.Errorf("failed to emit tool_call event: %w", err)
		}
	}
	return nil
}

// executeToolCalls executes, checkpoints, and emits each tool result before
// starting the next tool in the batch. Each durable checkpoint contains only
// complete assistant/tool pairs, while all tool_call frames have already been
// flushed and the pending batch intent has already been committed.
func (ch *ChatHandler) executeToolCalls(ctx context.Context, batch chatToolBatch) (chatToolBatchResult, error) {
	result := chatToolBatchResult{
		messages:      batch.messages,
		modelMessages: batch.modelMessages,
		toolCalls:     make([]ToolCallInfo, 0, len(batch.response.ToolCalls)),
	}
	var repetitionWarning string

	for index, tc := range batch.response.ToolCalls {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("chat turn interrupted before tool %s: %w", tc.Name, err)
		}

		argsHash := hashArgs(tc.Name, tc.Arguments)
		batch.repetitionTracker[argsHash]++
		if batch.repetitionTracker[argsHash] >= 3 {
			repetitionWarning = fmt.Sprintf("[System: Warning — you have called %s with the same arguments %d times. Try a different approach.]", tc.Name, batch.repetitionTracker[argsHash])
			result.iterationBump += 5
		}

		intentContent := ""
		if index == 0 {
			intentContent = batch.response.Content
		}
		toolIntent := llm.Message{Role: "assistant", Content: intentContent, ToolCalls: []llm.ToolCall{tc}}
		result.messages = append(result.messages, toolIntent)
		result.modelMessages = append(result.modelMessages, toolIntent)

		toolResult, execErr := batch.executor.Execute(ctx, tc)
		if execErr != nil {
			errResult := map[string]any{"success": false, "error": execErr.Error()}
			if errJSON, jsonErr := json.Marshal(errResult); jsonErr == nil {
				toolResult = string(errJSON)
			} else {
				toolResult = `{"success":false,"error":"tool execution failed"}`
			}
		}

		resultMessage := llm.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    toolResult,
		}
		result.messages = append(result.messages, resultMessage)
		result.modelMessages = append(result.modelMessages, resultMessage)

		var argsAny any
		_ = json.Unmarshal(tc.Arguments, &argsAny)
		var resultAny any
		_ = json.Unmarshal([]byte(toolResult), &resultAny)
		result.toolCalls = append(result.toolCalls, ToolCallInfo{Name: tc.Name, Args: argsAny, Result: resultAny})
		batch.usage.ToolCalls++

		if index == len(batch.response.ToolCalls)-1 && repetitionWarning != "" {
			warning := llm.Message{Role: "user", Content: repetitionWarning}
			result.messages = append(result.messages, warning)
			result.modelMessages = append(result.modelMessages, warning)
		}

		// External side effects cannot be transacted with SQLite. Their intent is
		// already durable; checkpoint this result before exposing it or starting
		// the next side effect.
		durabilityCtx, durabilityCancel := context.WithTimeout(context.WithoutCancel(ctx), chatDurabilityTimeout)
		commitErr := ch.commitChatProgress(durabilityCtx, batch.namespace, batch.sessionID, result.messages, batch.turnState, *batch.usage)
		durabilityCancel()
		if commitErr != nil {
			return result, fmt.Errorf("failed to checkpoint executed tools after %s: %w", tc.Name, commitErr)
		}

		resultEvent := toolResultSSE{id: tc.ID, name: tc.Name, result: toolResult}
		if err := emitToolResultEvent(resultEvent, batch.emitSSE); err != nil {
			return result, err
		}
	}

	return result, nil
}

func emitToolResultEvent(result toolResultSSE, emitSSE sseEmitter) error {
	if emitSSE == nil {
		return nil
	}
	data, err := json.Marshal(map[string]any{
		"id":     result.id,
		"name":   result.name,
		"result": json.RawMessage(result.result),
	})
	if err != nil {
		return fmt.Errorf("failed to marshal tool_result event: %w", err)
	}
	if err := emitSSE("tool_result", string(data)); err != nil {
		return fmt.Errorf("failed to emit tool_result event: %w", err)
	}
	return nil
}

// handleFinalResponse durably commits the completed turn before exposing its
// final response as a successful result.
func (ch *ChatHandler) handleFinalResponse(
	ctx context.Context,
	content string,
	messages []llm.Message,
	namespace, sessionID string,
	emitSSE sseEmitter,
	executor *ToolExecutor,
	usage *ChatUsage,
	turnState *chatTurnCommitState,
	start time.Time,
) (string, error) {
	finalMessages := append(messages, llm.Message{Role: "assistant", Content: content})
	usage.Duration = time.Since(start).Round(time.Millisecond).String()
	usage.TasksCreated = executor.tasksCreated
	if err := ch.commitChatProgress(ctx, namespace, sessionID, finalMessages, turnState, *usage); err != nil {
		return "", fmt.Errorf("failed to commit chat turn: %w", err)
	}

	if emitSSE != nil && content != "" {
		msgData, _ := json.Marshal(map[string]string{"content": content})
		if err := emitSSE("message", string(msgData)); err != nil {
			return "", fmt.Errorf("failed to emit final message: %w", err)
		}
	}

	return content, nil
}

func (ch *ChatHandler) commitChatProgress(
	ctx context.Context,
	namespace, sessionID string,
	messages []llm.Message,
	turnState *chatTurnCommitState,
	usage ChatUsage,
) error {
	incrIn := usage.InputTokens - turnState.committedInput
	incrOut := usage.OutputTokens - turnState.committedOutput
	if incrIn < 0 || incrOut < 0 {
		return fmt.Errorf("chat usage moved backwards while committing progress")
	}
	if err := ch.saveChatSession(ctx, namespace, sessionID, messages, turnState.persistedMessages, ChatUsage{
		InputTokens:  incrIn,
		OutputTokens: incrOut,
	}, turnState.turnID); err != nil {
		return err
	}
	turnState.persistedMessages = len(messages)
	turnState.committedInput = usage.InputTokens
	turnState.committedOutput = usage.OutputTokens
	return nil
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

// saveChatSession atomically commits the new transcript messages and their usage.
func (ch *ChatHandler) saveChatSession(ctx context.Context, namespace, sessionID string, messages []llm.Message, persistedCount int, usage ChatUsage, turnIDs ...string) error {
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

	session := &store.SessionRecord{
		Namespace:   namespace,
		Name:        sessionID,
		SessionType: "chat",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	turnID := ""
	if len(turnIDs) > 0 {
		turnID = turnIDs[0]
	}
	if ch.sessionTurnCommitter == nil {
		return fmt.Errorf("session store does not support atomic chat turns")
	}
	return ch.sessionTurnCommitter.CommitSessionTurn(ctx, session, turnID, persistedCount, storeMessages, usage.InputTokens, usage.OutputTokens)
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
	_, err = ch.sessionStore.GetSession(ctx, namespace, sessionID)
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
		phase := taskList.Items[i].Status.Phase
		if phase == corev1alpha1.TaskPhasePending || phase == corev1alpha1.TaskPhaseRunning {
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
