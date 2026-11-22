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
	"slices"
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
	"go.opentelemetry.io/otel/trace"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	chattools "github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/tracing/genai"
)

const (
	apiFieldContent = "content"
	apiFieldName    = "name"
)

var chatLog = logf.Log.WithName("chat-handler")

const (
	defaultNamespace      = "default"
	chatDurabilityTimeout = 10 * time.Second
	chatRoleUser          = "user"
	chatError             = "error"
	chatProviderKey       = "provider"
	chatModelKey          = "model"
	chatSuccessKey        = "success"
)

const (
	chatStreamUnclaimed uint32 = iota
	chatStreamOwned
	chatStreamFinalized
)

type ChatConfig struct {
	Enabled                bool
	ConnectorsEnabled      bool
	DashboardEnabled       bool
	Provider               string
	Model                  string
	MaxIterations          int
	MaxDuration            time.Duration
	ToolTimeout            time.Duration
	MaxConcurrent          int
	MaxTasksPerTurn        int
	MaxSessionSize         int
	MaxPrematureEndRetries int
	RuntimeAvailability    ACPRuntimeAvailability
	ExecutionMode          executionmode.Mode
	LinkedAccounts         LinkedAccountsFactory
}

type LinkedAccountsFactory func(namespace string, requester *corev1alpha1.RequestedBy) chattools.LinkedAccountCredentials

type ACPRuntimeAvailability struct {
	Codex    bool
	Claude   bool
	Copilot  bool
	OpenCode bool
}

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

type ChatResponse struct {
	SessionID string         `json:"sessionId"`
	Message   string         `json:"message"`
	ToolCalls []ToolCallInfo `json:"toolCalls,omitempty"`
	Usage     ChatUsage      `json:"usage"`
}

type ToolCallInfo struct {
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
	Result json.RawMessage `json:"result"`
}

type ChatUsage struct {
	InputTokens  int    `json:"inputTokens"`
	OutputTokens int    `json:"outputTokens"`
	LLMCalls     int    `json:"llmCalls"`
	ToolCalls    int    `json:"toolCalls"`
	TasksCreated int    `json:"tasksCreated"`
	Duration     string `json:"duration"`
}

type SSEEvent struct {
	SessionID string `json:"sessionId"`
	Content   string `json:"content,omitempty"`
	ToolCall  any    `json:"toolCall,omitempty"`
	Error     any    `json:"error,omitempty"`
	Usage     any    `json:"usage,omitempty"`
}

type activeChatRequest struct {
	cancel             context.CancelFunc
	done               chan struct{}
	deleteRequested    bool
	deleteWaiters      int
	gate               bool
	keepCreatedSession bool
}

type activeChatHandle struct {
	cancelContext context.Context
	finish        func()
	turnID        string
	turnDeadline  time.Time
}

type activeChatReservation struct {
	cancelContext context.Context
	cancel        context.CancelFunc
	request       *activeChatRequest
	key           string
	once          sync.Once
}

type ChatHandler struct {
	client                    client.Client
	apiReader                 client.Reader
	kubeClient                kubernetes.Interface
	sessionManager            *controller.SessionManager
	config                    ChatConfig
	semaphore                 chan struct{}
	watchNamespace            string
	enforceNamespaceIsolation bool
	sessionStore              store.SessionStore
	sessionTurnCommitter      store.SessionTurnCommitter
	resultStore               store.ResultStore
	gatewayEventStore         store.GatewayEventStore
	contextTokenAuthorization ContextTokenAuthorizationConfig
	cooldownTracker           *llm.CooldownTracker
	resolver                  *ProviderResolver
	activeChatsMu             sync.Mutex
	activeChats               map[string]*activeChatRequest
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
	providerName   string
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

func NewChatHandler(c client.Client, apiReader client.Reader, sm *controller.SessionManager, config ChatConfig, watchNamespace string, enforceNS bool, ss store.SessionStore, rs store.ResultStore, resolver *ProviderResolver, kubeClientOpt ...kubernetes.Interface) *ChatHandler {
	var kubeClient kubernetes.Interface
	if len(kubeClientOpt) > 0 {
		kubeClient = kubeClientOpt[0]
	}

	handler := &ChatHandler{
		client:                    c,
		apiReader:                 apiReader,
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
		activeChats:               make(map[string]*activeChatRequest),
	}
	if committer, ok := ss.(store.SessionTurnCommitter); ok {
		handler.sessionTurnCommitter = committer
	}
	return handler
}

func (ch *ChatHandler) contextTokenAuthorizationReader() client.Reader {
	return uncachedReaderOr(ch.apiReader, ch.client)
}

var blockedNamespaces = map[string]bool{
	"kube-system": true,
	"kube-public": true,
}

//nolint:gocyclo // Auth, provider setup, JSON/SSE streaming, cancellation, and Session fencing share one request boundary.
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

	ctx, cancel := context.WithTimeout(c.Context(), ch.config.MaxDuration)
	defer cancel()

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
	if req.AgentRef != "" {
		if err := authorizeKubernetesResourceAction(ctx, ch.kubeClient, userInfo, namespace, "get", corev1alpha1.GroupVersion.Group, "agents", req.AgentRef); err != nil {
			return err
		}
	}

	sessionID := resolveChatSessionID(req.SessionID)
	ctx = usageRequestContext(ctx, ch.resultStore, uncachedReaderOr(ch.apiReader, ch.client), namespace, sessionID)
	if req.SessionID != "" {
		for _, verb := range []string{"get", "update"} {
			if err := authorizeKubernetesResourceAction(ctx, ch.kubeClient, userInfo, namespace, verb, corev1alpha1.GroupVersion.Group, "sessions", sessionID); err != nil {
				return err
			}
		}
		if err := ch.ahpSessionVisible(ctx, userInfo, namespace, sessionID); err != nil {
			return err
		}
	}
	reservation, err := ch.reserveActiveChat(namespace, sessionID)
	if err != nil {
		return chatSessionLockError(err)
	}
	var activeChat *activeChatHandle
	stopRequestCancellation := context.AfterFunc(reservation.cancelContext, cancel)
	defer stopRequestCancellation()
	defer func() {
		if activeChat == nil {
			ch.finishActiveChatReservation(reservation, nil)
		} else if !sseMode {
			activeChat.finish()
		}
	}()

	provider, model, providerInfo, err := ch.resolver.ResolveWithInfo(ctx, ResolveOpts{
		ProviderName: req.Provider,
		Model:        req.Model,
		AgentRef:     req.AgentRef,
		Namespace:    namespace,
		AuthorizeProviderReference: func(provider ProviderResolutionInfo) error {
			return authorizeContextTokenProviderReference(c, ch.contextTokenAuthorization, "chatProviderReference", namespace, provider)
		},
		AuthorizeProviderUse: func(provider ProviderResolutionInfo, model string) error {
			if err := authorizeContextTokenProviderUse(c, ch.contextTokenAuthorization, "chat", namespace, provider, model); err != nil {
				return err
			}
			return authorizeChatProviderUse(c, ch.kubeClient, "chat", provider)
		},
		RequireExplicitProvider: requestRequiresExplicitProvider(c, ch.contextTokenAuthorization),
	})
	if err != nil {
		if ferr, ok := err.(*fiber.Error); ok && ferr.Code == fiber.StatusForbidden {
			return err
		}
		chatLog.Error(err, "failed to resolve provider")
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("failed to resolve provider: %v", err))
	}

	provider, err = ch.wrapWithRetryAndFallback(ctx, c, provider, req, namespace)
	if err != nil {
		return err
	}

	provider = llm.NewTracingProvider(provider)

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

	discoveryClient := newExternalToolClient(ch.client, ch.kubeClient, userInfo, namespace, ch.watchNamespace, ch.enforceNamespaceIsolation, ch.gatewayEventStore)
	promptBuilder := NewSystemPromptBuilder(externalToolDiscoveryClient{Client: discoveryClient}, namespace, ch.config.RuntimeAvailability)
	promptBuilder.SetChatProvider(providerInfo.Name)
	chatToolNames := chattools.ChatToolNames()
	chatToolStubs := make([]llm.Tool, 0, len(chatToolNames))
	for _, name := range chatToolNames {
		chatToolStubs = append(chatToolStubs, llm.Tool{Name: name})
	}
	if !ch.config.ConnectorsEnabled {
		chatToolStubs = filterCompletionToolsExcluding(chatToolStubs, chattools.ListConnectionsToolName)
	}
	offeredChatTools := completionToolNames(filterCompletionToolsForContextToken(c, ch.contextTokenAuthorization, chatToolStubs))
	if len(offeredChatTools) < len(chatToolStubs) {
		promptBuilder.SetAvailableChatTools(offeredChatTools)
	}
	systemPrompt, err := promptBuilder.BuildSystemPrompt(ctx, req.SystemPrompt)
	if err != nil {
		chatLog.Error(err, "failed to build system prompt")
		return fiber.NewError(fiber.StatusInternalServerError, "failed to build system prompt")
	}
	activeChat, err = ch.activateReservedChat(ctx, reservation, namespace, sessionID)
	if err != nil {
		return err
	}
	turnID := activeChat.turnID

	messages, err := ch.loadReservedChatSession(ctx, namespace, sessionID)
	if err != nil {
		return err
	}
	persistedCount := len(messages)

	userContent := req.Message
	if req.AgentRef != "" {
		agentObj := &corev1alpha1.Agent{}
		if err := ch.client.Get(ctx, types.NamespacedName{Name: req.AgentRef, Namespace: namespace}, agentObj); err == nil {
			switch {
			case agentObj.Spec.Runtime != nil && slices.Contains(offeredChatTools, "create_agent_task"):
				userContent = fmt.Sprintf("[Using agent %q which has runtime %q — use create_agent_task with agent=%q for this request.]\n\n%s",
					req.AgentRef, agentRuntimeLabel(agentObj.Spec.Runtime), req.AgentRef, req.Message)
			case agentObj.Spec.Runtime == nil && slices.Contains(offeredChatTools, "create_ai_task"):
				userContent = fmt.Sprintf("[Using agent %q which has no runtime — use create_ai_task with agentRef=%q for this request.]\n\n%s",
					req.AgentRef, req.AgentRef, req.Message)
			default:
				userContent = fmt.Sprintf("[Using agent %q. The tool that runs it is not available in this conversation.]\n\n%s",
					req.AgentRef, req.Message)
			}
		}
	}

	messages = append(messages, llm.Message{
		Role:    chatRoleUser,
		Content: userContent,
	})

	executor := NewToolExecutor(ch.client, ch.sessionManager, namespace, sessionID, ch.watchNamespace, ch.enforceNamespaceIsolation, ch.config.MaxTasksPerTurn, ch.config.ToolTimeout, ch.resultStore, ch.kubeClient)
	executor.userInfo = userInfo
	executor.gatewayEventStore = ch.gatewayEventStore
	executor.requester = requesterFromUserInfo(userInfo)
	executor.linkedAccounts = scopedLinkedAccounts(ch.config.LinkedAccounts, namespace, executor.requester, userInfo, ch.contextTokenAuthorization)
	executor.authorizeConnectorRead = connectorReadToolAuthorizer(userInfo, ch.contextTokenAuthorization, ch.config.ConnectorsEnabled)
	executor.dashboardEnabled = ch.config.DashboardEnabled
	executor.chatSessionEpoch = chatSessionEpoch(ctx, ch.sessionStore, namespace, sessionID)
	executor.createdTasks = chattools.NewCreatedTasks()
	executor.SetExecutionMode(ch.config.ExecutionMode)
	executor.provider = providerInfo.Name
	executor.providerNamespace = providerInfo.Namespace
	executor.providerType = providerInfo.Type
	authorizationReader := ch.contextTokenAuthorizationReader()
	executor.SetPolicyReader(authorizationReader)
	executor.authorizeAgentReference = func(_ context.Context, agent *corev1alpha1.Agent) error {
		return authorizeContextTokenAgentTaskReference(contextToken, ch.contextTokenAuthorization, agent.Namespace, agent.Name)
	}
	executor.SetTaskCreateAuthorizer(func(ctx context.Context, task *corev1alpha1.Task) error {
		return authorizeAndStampToolTaskCreate(ctx, authorizationReader, ch.kubeClient, contextToken, ch.contextTokenAuthorization, "chatToolCreateTask", userInfo, task)
	})
	executor.SetTaskDeleteAuthorizer(func(ctx context.Context, task *corev1alpha1.Task) error {
		return authorizeContextTokenTaskDeleteObject(ctx, authorizationReader, contextToken, ch.contextTokenAuthorization, "chatToolDeleteTask", task)
	})
	executor.SetAgentCreateAuthorizer(func(ctx context.Context, agent *corev1alpha1.Agent) error {
		if err := authorizeContextTokenToolAgentCreate(ctx, authorizationReader, contextToken, ch.contextTokenAuthorization, "chatToolCreateAgent", agent); err != nil {
			return err
		}
		return authorizeToolAgentCreateProviderUse(ctx, ch.kubeClient, userInfo, agent)
	})
	executor.SetAgentUpdateAuthorizer(func(ctx context.Context, agent *corev1alpha1.Agent) error {
		if err := authorizeContextTokenToolAgentUpdate(ctx, authorizationReader, contextToken, ch.contextTokenAuthorization, "chatToolUpdateAgent", agent); err != nil {
			return err
		}
		return authorizeToolAgentUpdateProviderUse(ctx, authorizationReader, ch.kubeClient, userInfo, agent)
	})
	executor.SetAgentDeleteAuthorizer(func(ctx context.Context, agent *corev1alpha1.Agent) error {
		return authorizeContextTokenToolAgentDelete(contextToken, ch.contextTokenAuthorization, "chatToolDeleteAgent", agent)
	})
	executor.SetSecretReadAuthorizer(func(ctx context.Context, namespace, secretName string) error {
		return authorizeContextTokenSecretRead(contextToken, ch.contextTokenAuthorization, "chatToolReadSecret", namespace, secretName)
	})

	tools := executor.registry.ToLLMTools(chattools.ChatToolNames())
	if !ch.config.ConnectorsEnabled {
		tools = filterCompletionToolsExcluding(tools, chattools.ListConnectionsToolName)
	}
	tools = filterCompletionToolsForContextToken(c, ch.contextTokenAuthorization, tools)
	if err := authorizeContextTokenToolUse(c, ch.contextTokenAuthorization, "chatTools", completionToolNames(tools)); err != nil {
		return err
	}
	executor.SetAllowedTools(tools)

	var temperature float64
	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	maxTokens := 4096
	if req.MaxTokens != nil {
		maxTokens = int(*req.MaxTokens)
	}

	accept := c.Get("Accept")
	if accept == "application/json" {
		content, usage, toolCalls, err := ch.runToolLoop(
			ctx, provider, messages, systemPrompt, tools, executor,
			sessionID, namespace, model, temperature, maxTokens, persistedCount, nil, turnID,
		)
		if err != nil {
			chatLog.Error(err, "tool loop error")
			tracing.RecordSpanError(span, err)
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
		turnCancelCtx:  activeChat.cancelContext,
		turnDeadline:   activeChat.turnDeadline,
		finalizeTurn:   activeChat.finish,
		provider:       provider,
		providerName:   providerInfo.Name,
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

	sseParentCtx := baggage.ContextWithBaggage(
		trace.ContextWithSpanContext(context.Background(), req.span.SpanContext()),
		baggage.FromContext(req.parentCtx),
	)
	sseParentCtx = llm.CopyUsageRecorder(sseParentCtx, req.parentCtx)
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
		providerName := req.providerName
		if providerName == "" {
			providerName = req.provider.Name()
		}
		statusData, _ := json.Marshal(map[string]string{
			"sessionId":     req.sessionID,
			chatProviderKey: providerName,
			chatModelKey:    req.model,
		})
		emitSSE("status", string(statusData))

		content, usage, _, err := ch.runToolLoop(
			sseCtx, req.provider, req.messages, req.systemPrompt, req.tools, req.executor,
			req.sessionID, req.namespace, req.model, req.temperature, req.maxTokens,
			req.persistedCount, emitSSE, req.turnID,
		)
		if err != nil {
			errData, _ := json.Marshal(map[string]string{chatError: err.Error()})
			emitSSE(chatError, string(errData))
			return
		}

		_ = content
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
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(turnDeadline) {
		turnDeadline = deadline.UTC()
	}
	created, err := ch.sessionTurnCommitter.AcquireChatTurn(ctx, store.AcquireChatTurnRequest{
		Session: &store.SessionRecord{
			Namespace:   namespace,
			Name:        sessionID,
			SessionType: store.SessionTypeChat,
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		TurnID:    turnID,
		Now:       now,
		ExpiresAt: turnDeadline.Add(chatDurabilityTimeout),
	})
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

func resolveChatSessionID(requested string) string {
	if requested != "" {
		return requested
	}
	return fmt.Sprintf("chat-%s", generateChatID())
}

func chatSessionLockError(err error) error {
	switch {
	case errors.Is(err, store.ErrGatewayOwnedSession), errors.Is(err, store.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, "chat session not found")
	case errors.Is(err, store.ErrConflict):
		return fiber.NewError(fiber.StatusConflict, "chat session is active or its deleted name is reserved")
	default:
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to lock chat session: %v", err))
	}
}

func (ch *ChatHandler) reserveActiveChat(namespace, sessionID string) (*activeChatReservation, error) {
	return ch.reserveActiveChatAs(namespace, sessionID, false)
}

func (ch *ChatHandler) reserveActiveChatAs(namespace, sessionID string, gate bool) (*activeChatReservation, error) {
	cancelContext, cancel := context.WithCancel(context.Background())
	request := &activeChatRequest{cancel: cancel, done: make(chan struct{}), gate: gate}
	reservation := &activeChatReservation{
		cancelContext: cancelContext,
		cancel:        cancel,
		request:       request,
		key:           activeChatKey(namespace, sessionID),
	}
	ch.activeChatsMu.Lock()
	if ch.activeChats == nil {
		ch.activeChats = make(map[string]*activeChatRequest)
	}
	if _, exists := ch.activeChats[reservation.key]; exists {
		ch.activeChatsMu.Unlock()
		cancel()
		return nil, fmt.Errorf("%w: chat session %s/%s already has an active request", store.ErrConflict, namespace, sessionID)
	}
	ch.activeChats[reservation.key] = request
	ch.activeChatsMu.Unlock()
	return reservation, nil
}

func (ch *ChatHandler) finishActiveChatReservation(reservation *activeChatReservation, release func()) {
	if reservation == nil {
		return
	}
	reservation.once.Do(func() {
		reservation.cancel()
		if release != nil {
			release()
		}
		ch.activeChatsMu.Lock()
		if ch.activeChats[reservation.key] == reservation.request && reservation.request.deleteWaiters == 0 {
			delete(ch.activeChats, reservation.key)
		}
		close(reservation.request.done)
		ch.activeChatsMu.Unlock()
	})
}

func (ch *ChatHandler) activateReservedChat(
	ctx context.Context,
	reservation *activeChatReservation,
	namespace, sessionID string,
) (*activeChatHandle, error) {
	acquireCtx, acquireCancel := context.WithCancel(ctx)
	stopAcquireCancellation := context.AfterFunc(reservation.cancelContext, acquireCancel)
	turnID, turnDeadline, created, turnCancelCtx, err := ch.beginChatTurn(acquireCtx, namespace, sessionID)
	stopAcquireCancellation()
	acquireCancel()
	if err != nil {
		return nil, err
	}
	stopTurnCancellation := context.AfterFunc(turnCancelCtx, reservation.cancel)
	release := func() {
		stopTurnCancellation()
		ch.activeChatsMu.Lock()
		keep := reservation.request.keepCreatedSession
		ch.activeChatsMu.Unlock()
		ch.releaseChatTurn(namespace, sessionID, turnID, created && !keep)
		ch.finishActiveChatTurn(namespace, sessionID, turnID)
	}
	return &activeChatHandle{
		cancelContext: reservation.cancelContext,
		finish:        func() { ch.finishActiveChatReservation(reservation, release) },
		turnID:        turnID,
		turnDeadline:  turnDeadline,
	}, nil
}

func activeChatKey(namespace, sessionID string) string {
	return namespace + "\x00" + sessionID
}

func (ch *ChatHandler) cancelActiveChat(namespace, sessionID string) (*activeChatRequest, bool) {
	key := activeChatKey(namespace, sessionID)
	ch.activeChatsMu.Lock()
	request := ch.activeChats[key]
	if request == nil {
		ch.activeChatsMu.Unlock()
		return nil, false
	}
	request.deleteRequested = true
	request.deleteWaiters++
	ch.activeChatsMu.Unlock()
	request.cancel()
	return request, true
}

func (ch *ChatHandler) deleteChatSession(ctx context.Context, namespace, sessionID string) error {
	if ch.sessionManager != nil {
		return ch.sessionManager.DeleteSession(ctx, namespace, sessionID)
	}
	return ch.sessionStore.DeleteSession(ctx, namespace, sessionID, time.Now().UTC())
}

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
	if persistedCount < 0 || persistedCount > len(messages) {
		return "", usage, nil, fmt.Errorf("invalid persisted message count %d for %d messages", persistedCount, len(messages))
	}
	turnMessages := slices.Clone(messages[persistedCount:])
	appendTurnMessages := func(newMessages ...llm.Message) {
		turnMessages = append(turnMessages, newMessages...)
		messages = append(messages, newMessages...)
	}
	repetitionTracker := make(map[string]int)
	start := time.Now()
	taskClient := newExternalToolClient(executor.client, executor.kubeClient, executor.userInfo, namespace, executor.watchNamespace, executor.enforceNamespaceIsolation, executor.gatewayEventStore)

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
			tracing.RecordSpanError(iterSpan, err)
			iterSpan.End()
			return "", usage, allToolCalls, err
		default:
		}

		if content, hit, err := ch.handleIterationLimit(
			iterCtx, iteration, provider, messages, turnMessages, systemPrompt, model, namespace, sessionID,
			maxTokens, persistedCount, temperature, emitSSE, executor, &usage, turnID, start,
		); hit {
			setUsageSpanAttributes(iterSpan, usage)
			if err != nil {
				tracing.RecordSpanError(iterSpan, err)
			}
			iterSpan.End()
			return content, usage, allToolCalls, err
		}

		if iteration > 0 && iteration%5 == 0 {
			appendTurnMessages(llm.Message{
				Role:    chatRoleUser,
				Content: "[System: Progress check — summarize what you've done so far and what remains.]",
			})
		}

		sent := ch.sessionView(messages)
		resp, shortened, retried, err := ch.callLLMWithRetry(iterCtx, provider, sent, systemPrompt, model, tools, maxTokens, temperature)
		if retried {
			messages = shortened
		}
		if err != nil {
			usage.Duration = time.Since(start).Round(time.Millisecond).String()
			tracing.RecordSpanError(iterSpan, err)
			iterSpan.End()
			return "", usage, allToolCalls, fmt.Errorf("LLM completion failed: %w", err)
		}
		iterSpan.SetAttributes(attribute.Int("chat.tool_call_count", len(resp.ToolCalls)))
		usage.LLMCalls++
		usage.InputTokens += resp.PromptTokens()
		usage.OutputTokens += resp.OutputTokens

		if len(resp.ToolCalls) == 0 {
			if executor.tasksCreated > 0 && hasRunningTasks(iterCtx, taskClient, namespace, sessionID) {
				if emitSSE != nil && resp.Content != "" {
					msgData, _ := json.Marshal(map[string]string{apiFieldContent: resp.Content})
					emitSSE("message", string(msgData))
				}
				appendTurnMessages(
					llm.Message{Role: chatRoleAssistant, Content: resp.Content},
					llm.Message{Role: chatRoleUser, Content: "[System: You have tasks still running. Do NOT stop. Call wait_for_task again for each running task until it reaches Succeeded or Failed, then call fetch_task_output to get the result.]"},
				)
				iterSpan.End()
				continue
			}
			content, err := ch.handleFinalResponse(
				iterCtx, resp.Content, turnMessages, namespace, sessionID, persistedCount,
				emitSSE, executor, &usage, turnID, start,
			)
			setUsageSpanAttributes(iterSpan, usage)
			if err != nil {
				tracing.RecordSpanError(iterSpan, err)
			}
			iterSpan.End()
			return content, usage, allToolCalls, err
		}

		toolMessages, newToolCalls, iterBump := ch.executeToolCalls(iterCtx, resp, executor, emitSSE, repetitionTracker)
		appendTurnMessages(toolMessages...)
		allToolCalls = append(allToolCalls, newToolCalls...)
		usage.ToolCalls += len(newToolCalls)
		iteration += iterBump
		iterSpan.End()
	}
}

func (ch *ChatHandler) handleIterationLimit(
	ctx context.Context,
	iteration int,
	provider llm.Provider,
	messages, turnMessages []llm.Message,
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

	terminationPrompt := llm.Message{
		Role:    chatRoleUser,
		Content: "[System: You have reached the maximum number of iterations. Please provide a final summary of what you accomplished.]",
	}
	messages = append(messages, terminationPrompt)
	turnMessages = append(turnMessages, terminationPrompt)

	resp, err := provider.Complete(ctx, &llm.CompletionRequest{
		Model:              model,
		SystemPrompt:       systemPrompt,
		SystemPromptStable: chatPromptStable(systemPrompt),
		Messages:           ch.sessionView(messages),
		MaxTokens:          maxTokens,
		Temperature:        temperature,
	})
	if err != nil {
		usage.Duration = time.Since(start).Round(time.Millisecond).String()
		return "", true, fmt.Errorf("final LLM completion after iteration limit failed: %w", err)
	}
	usage.LLMCalls++
	usage.InputTokens += resp.PromptTokens()
	usage.OutputTokens += resp.OutputTokens

	finalMessages := append(turnMessages, llm.Message{Role: chatRoleAssistant, Content: resp.Content})
	usage.Duration = time.Since(start).Round(time.Millisecond).String()
	usage.TasksCreated = executor.tasksCreated
	if err := ch.saveChatSession(
		ctx, namespace, sessionID, finalMessages, persistedCount, *usage, turnID,
	); err != nil {
		return "", true, fmt.Errorf("failed to commit chat turn: %w", err)
	}

	if emitSSE != nil && resp.Content != "" {
		msgData, _ := json.Marshal(map[string]string{apiFieldContent: resp.Content})
		emitSSE("message", string(msgData))
	}

	return resp.Content, true, nil
}

func (ch *ChatHandler) sessionView(messages []llm.Message) []llm.Message {
	if ch.config.MaxSessionSize > 0 {
		return llm.TruncateMessages(messages, ch.config.MaxSessionSize/4)
	}
	return messages
}

func (ch *ChatHandler) callLLMWithRetry(
	ctx context.Context,
	provider llm.Provider,
	messages []llm.Message,
	systemPrompt, model string,
	tools []llm.Tool,
	maxTokens int,
	temperature float64,
) (*llm.CompletionResponse, []llm.Message, bool, error) {
	resp, err := provider.Complete(ctx, &llm.CompletionRequest{
		Model:              model,
		SystemPrompt:       systemPrompt,
		SystemPromptStable: chatPromptStable(systemPrompt),
		Messages:           messages,
		Tools:              tools,
		MaxTokens:          maxTokens,
		Temperature:        temperature,
	})
	if err != nil && llm.IsContextTooLongErr(err) {
		tokenEstimate := 0
		for _, m := range messages {
			tokenEstimate += len(m.Content) / 4
		}
		messages = llm.TruncateMessages(messages, tokenEstimate/2)
		resp, err = provider.Complete(ctx, &llm.CompletionRequest{
			Model:              model,
			SystemPrompt:       systemPrompt,
			SystemPromptStable: chatPromptStable(systemPrompt),
			Messages:           messages,
			Tools:              tools,
			MaxTokens:          maxTokens,
			Temperature:        temperature,
		})
		return resp, messages, true, err
	}
	return resp, messages, false, err
}

func (ch *ChatHandler) executeToolCalls(
	ctx context.Context,
	resp *llm.CompletionResponse,
	executor *ToolExecutor,
	emitSSE func(event, data string),
	repetitionTracker map[string]int,
) ([]llm.Message, []ToolCallInfo, int) {
	messages := []llm.Message{{
		Role:      chatRoleAssistant,
		Content:   resp.Content,
		ToolCalls: resp.ToolCalls,
	}}

	toolCalls := make([]ToolCallInfo, 0, len(resp.ToolCalls))
	var iterationBump int
	var repetitionWarning string

	for _, tc := range resp.ToolCalls {
		if emitSSE != nil {
			tcData, _ := json.Marshal(map[string]any{
				"id":         tc.ID,
				apiFieldName: tc.Name,
				"args":       tc.Arguments,
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
			errResult := map[string]any{chatSuccessKey: false, chatError: execErr.Error()}
			if errJSON, jsonErr := json.Marshal(errResult); jsonErr == nil {
				result = string(errJSON)
			} else {
				result = `{"success":false,"error":"tool execution failed"}`
			}
		}

		if emitSSE != nil {
			trData, _ := json.Marshal(map[string]any{
				"id":         tc.ID,
				apiFieldName: tc.Name,
				"result":     json.RawMessage(result),
			})
			emitSSE("tool_result", string(trData))
		}

		messages = append(messages, llm.Message{
			Role:       chatRoleTool,
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    result,
		})

		toolCalls = append(toolCalls, ToolCallInfo{
			Name:   tc.Name,
			Args:   tc.Arguments,
			Result: json.RawMessage(result),
		})
	}

	if repetitionWarning != "" {
		messages = append(messages, llm.Message{
			Role:    chatRoleUser,
			Content: repetitionWarning,
		})
	}

	return messages, toolCalls, iterationBump
}

func (ch *ChatHandler) handleFinalResponse(
	ctx context.Context,
	content string,
	turnMessages []llm.Message,
	namespace, sessionID string,
	persistedCount int,
	emitSSE func(event, data string),
	executor *ToolExecutor,
	usage *ChatUsage,
	turnID string,
	start time.Time,
) (string, error) {
	finalMessages := append(turnMessages, llm.Message{Role: chatRoleAssistant, Content: content})
	usage.Duration = time.Since(start).Round(time.Millisecond).String()
	usage.TasksCreated = executor.tasksCreated
	if err := ch.saveChatSession(
		ctx, namespace, sessionID, finalMessages, persistedCount, *usage, turnID,
	); err != nil {
		return "", fmt.Errorf("failed to commit chat turn: %w", err)
	}

	if emitSSE != nil && content != "" {
		msgData, _ := json.Marshal(map[string]string{apiFieldContent: content})
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

func (ch *ChatHandler) saveChatSession(
	ctx context.Context,
	namespace, sessionID string,
	newMessages []llm.Message,
	persistedCount int,
	usage ChatUsage,
	turnID string,
) error {
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

	durabilityCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), chatDurabilityTimeout)
	defer cancel()
	return ch.sessionTurnCommitter.CommitSessionTurn(durabilityCtx, store.CommitSessionTurnRequest{
		Session: &store.SessionRecord{
			Namespace:   namespace,
			Name:        sessionID,
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		},
		TurnID:               turnID,
		ExpectedMessageCount: persistedCount,
		Messages:             storeMessages,
		InputTokens:          usage.InputTokens,
		OutputTokens:         usage.OutputTokens,
		Now:                  time.Now().UTC(),
	})
}

func (ch *ChatHandler) HandleChatConfig(c fiber.Ctx) error {
	toolNames := chattools.ChatToolNames()
	if !ch.config.ConnectorsEnabled || !contextTokenAllowsConnectorRead(GetUserInfo(c), ch.contextTokenAuthorization) {
		toolNames = slices.DeleteFunc(slices.Clone(toolNames), func(name string) bool { return name == chattools.ListConnectionsToolName })
	}

	requireExplicitProvider := requestRequiresExplicitProvider(c, ch.contextTokenAuthorization)
	provider, model := ch.config.Provider, ch.config.Model
	if requireExplicitProvider {
		provider, model = "", ""
	}
	return c.JSON(fiber.Map{
		"enabled":                 ch.config.Enabled,
		chatProviderKey:           provider,
		chatModelKey:              model,
		"requireExplicitProvider": requireExplicitProvider,
		"maxIterations":           ch.config.MaxIterations,
		"maxDuration":             ch.config.MaxDuration.String(),
		"maxTasksPerTurn":         ch.config.MaxTasksPerTurn,
		"maxConcurrent":           ch.config.MaxConcurrent,
		"availableTools":          toolNames,
		"connectorsEnabled":       ch.config.ConnectorsEnabled,
	})
}

func (ch *ChatHandler) ahpSessionVisible(ctx context.Context, ui *UserInfo, namespace, sessionID string) error {
	if ch.client == nil {
		return nil
	}
	hidden, err := ahpSessionHiddenIn(ctx, ch.client, ui, namespace, sessionID)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to look up the session's owner: %v", err))
	}
	if hidden {
		return fiber.NewError(fiber.StatusNotFound, "chat session not found")
	}
	return nil
}

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
	if err := ch.ahpSessionVisible(ctx, GetUserInfo(c), namespace, sessionID); err != nil {
		return err
	}
	activeCancelled, err := ch.cancelAndWaitForActiveChat(ctx, namespace, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotReady) {
			return fiber.NewError(fiber.StatusConflict, "chat cancellation is still in progress")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to cancel active chat: %v", err))
	}
	if activeCancelled {
		defer ch.clearChatDeletionGate(namespace, sessionID)
	}
	sessionType, err := transcriptSessionType(ctx, ch.sessionStore, namespace, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if activeCancelled {
				return c.SendStatus(fiber.StatusNoContent)
			}
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

	if err := ch.deleteChatSession(ctx, namespace, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return c.SendStatus(fiber.StatusNoContent)
		}
		if errors.Is(err, store.ErrGatewayOwnedSession) {
			return fiber.NewError(fiber.StatusNotFound, "chat session not found")
		}
		if errors.Is(err, store.ErrConflict) {
			return fiber.NewError(fiber.StatusConflict, "chat session has active or unsettled work")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to cancel session: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

func (ch *ChatHandler) cancelAndWaitForActiveChat(ctx context.Context, namespace, sessionID string) (bool, error) {
	request, active := ch.cancelActiveChat(namespace, sessionID)
	if !active {
		return false, nil
	}
	timer := time.NewTimer(chatDurabilityTimeout)
	defer timer.Stop()
	select {
	case <-request.done:
		return true, nil
	case <-timer.C:
		ch.abandonChatDeletionWaiter(namespace, sessionID, request)
		return true, fmt.Errorf("%w: active chat did not stop before cancellation deadline", store.ErrNotReady)
	case <-ctx.Done():
		ch.abandonChatDeletionWaiter(namespace, sessionID, request)
		return true, ctx.Err()
	}
}

func (ch *ChatHandler) stopActiveChat(ctx context.Context, namespace, sessionID string) (bool, func(), error) {
	key := activeChatKey(namespace, sessionID)
	for range 3 {
		ch.activeChatsMu.Lock()
		request := ch.activeChats[key]
		if request != nil {
			request.deleteWaiters++
			request.keepCreatedSession = true
		}
		ch.activeChatsMu.Unlock()
		if request == nil {
			reservation, err := ch.reserveActiveChatAs(namespace, sessionID, true)
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			if err != nil {
				return false, nil, err
			}
			return false, func() { ch.finishActiveChatReservation(reservation, nil) }, nil
		}
		release := func() { ch.abandonChatDeletionWaiter(namespace, sessionID, request) }
		if request.gate {
			return false, release, nil
		}
		running := true
		select {
		case <-request.done:
			running = false
		default:
			request.cancel()
		}
		timer := time.NewTimer(chatDurabilityTimeout)
		select {
		case <-request.done:
			timer.Stop()
			return running, release, nil
		case <-timer.C:
			release()
			return true, nil, fmt.Errorf("%w: active chat did not stop before the cancellation deadline", store.ErrNotReady)
		case <-ctx.Done():
			timer.Stop()
			release()
			return true, nil, ctx.Err()
		}
	}
	return false, nil, fmt.Errorf("%w: chat session %s/%s kept starting requests", store.ErrNotReady, namespace, sessionID)
}

func (ch *ChatHandler) clearChatDeletionGate(namespace, sessionID string) {
	key := activeChatKey(namespace, sessionID)
	ch.activeChatsMu.Lock()
	if request := ch.activeChats[key]; request != nil && request.deleteWaiters > 0 {
		request.deleteWaiters--
		if request.deleteWaiters == 0 {
			request.deleteRequested = false
			delete(ch.activeChats, key)
		}
	}
	ch.activeChatsMu.Unlock()
}

func (ch *ChatHandler) abandonChatDeletionWaiter(namespace, sessionID string, request *activeChatRequest) {
	key := activeChatKey(namespace, sessionID)
	ch.activeChatsMu.Lock()
	defer ch.activeChatsMu.Unlock()
	if ch.activeChats[key] != request || request.deleteWaiters == 0 {
		return
	}
	request.deleteWaiters--
	if request.deleteWaiters != 0 {
		return
	}
	request.deleteRequested = false
	select {
	case <-request.done:
		delete(ch.activeChats, key)
	default:
	}
}

func (ch *ChatHandler) wrapWithRetryAndFallback(ctx context.Context, c fiber.Ctx, provider llm.Provider, req ChatRequest, namespace string) (llm.Provider, error) {
	var resultProvider llm.Provider = llm.NewRetryProvider(provider)

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
		if err := authorizeContextTokenProviderReference(c, ch.contextTokenAuthorization, "chatFallbackProviderReference", namespace, ProviderResolutionInfo{Name: fb.ProviderRef, Namespace: namespace}); err != nil {
			return resultProvider, err
		}
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
		if err := authorizeChatProviderUse(c, ch.kubeClient, "chat", fbProviderInfo); err != nil {
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
			fbConfig.AzureDeploymentName = fbProviderCRD.Spec.Azure.DeploymentName
		}

		fbProvider, err := llm.NewProvider(string(fbProviderCRD.Spec.Type), fbConfig)
		if err != nil {
			continue
		}

		fallbacks = append(fallbacks, llm.FallbackEntry{
			Provider: llm.NewRetryProvider(fbProvider),
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

func chatSessionEpoch(ctx context.Context, sessions store.SessionStore, namespace, sessionID string) string {
	reader, ok := sessions.(store.SessionCreationReader)
	if !ok {
		return ""
	}
	createdAt, err := reader.GetSessionCreatedAt(ctx, namespace, sessionID)
	if err != nil {
		logf.FromContext(ctx).Info("chat Session incarnation unavailable; its Tasks cannot act for the requester through an Agent identity",
			"namespace", namespace, "session", sessionID, "error", err.Error())
		return ""
	}
	return labels.SessionEpoch(createdAt)
}

func generateChatID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashArgs(name string, args json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write(args)
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

func writeSSE(w *bufio.Writer, event, data string) error {
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if err != nil {
		return err
	}
	return w.Flush()
}

func hasRunningTasks(ctx context.Context, c client.Client, namespace, sessionID string) bool {
	var taskList corev1alpha1.TaskList
	if err := c.List(ctx, &taskList,
		client.InNamespace(namespace),
		client.MatchingLabels{labels.LabelChatSession: sessionID},
	); err != nil {
		return false
	}
	for i := range taskList.Items {
		phase := taskList.Items[i].Status.Phase
		if !phase.IsTerminal() && phase != corev1alpha1.TaskPhaseScheduled {
			return true
		}
	}
	return false
}

func agentRuntimeLabel(runtime *corev1alpha1.AgentCLIRuntime) string {
	switch {
	case runtime.Type != "":
		return string(runtime.Type)
	case runtime.RuntimeRef != nil:
		return "external AgentRuntime " + runtime.RuntimeRef.Name
	default:
		return "unspecified"
	}
}
