/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workspace/statusrules"
)

const (
	maxResultSize                        = 10 << 20 // 10MB
	defaultInternalTranscriptSearchLimit = 10
	maxInternalTranscriptSearchLimit     = 50
)

// InternalHandlers contains handlers for internal worker endpoints.
type InternalHandlers struct {
	k8sClient           client.Client
	apiReader           client.Reader
	resultStore         store.ResultStore
	sessionStore        store.SessionStore
	planStore           store.PlanStore
	messageStore        store.MessageStore
	artifactStore       store.ArtifactStore
	executionEventStore store.ExecutionEventStore
	gatewayEventStore   store.GatewayEventStore
	memoryStore         store.MemoryStore
	memoryProposalStore store.MemoryProposalStore
}

// InternalHandlersConfig holds optional configuration for internal handlers.
type InternalHandlersConfig struct {
	Client              client.Client
	APIReader           client.Reader
	MemoryStore         store.MemoryStore
	MemoryProposalStore store.MemoryProposalStore
	ExecutionEventStore store.ExecutionEventStore
	GatewayEventStore   store.GatewayEventStore
}

// NewInternalHandlers creates a new InternalHandlers instance.
func NewInternalHandlers(rs store.ResultStore, ss store.SessionStore, ps store.PlanStore, ms store.MessageStore, as store.ArtifactStore, configs ...InternalHandlersConfig) *InternalHandlers {
	h := &InternalHandlers{
		resultStore:   rs,
		sessionStore:  ss,
		planStore:     ps,
		messageStore:  ms,
		artifactStore: as,
	}
	if len(configs) > 0 {
		h.k8sClient = configs[0].Client
		h.apiReader = configs[0].APIReader
		h.memoryStore = configs[0].MemoryStore
		h.memoryProposalStore = configs[0].MemoryProposalStore
		h.executionEventStore = configs[0].ExecutionEventStore
		h.gatewayEventStore = configs[0].GatewayEventStore
	}
	return h
}

// SubmitResult handles POST /internal/v1/results/{namespace}/{taskName}.
// Workers call this to persist task results.
func (h *InternalHandlers) SubmitResult(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")

	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}

	if _, err := h.internalCallerAuthorizer().verifyTaskCaller(c, namespace, taskName); err != nil {
		return err
	}

	if h.resultStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "result storage not enabled")
	}

	// Read body with size limit
	body := c.Request().BodyStream()
	if body == nil {
		// Fiber may buffer the body; fall back to c.Body()
		data := c.Body()
		if len(data) == 0 {
			return fiber.NewError(fiber.StatusBadRequest, "empty request body")
		}
		if len(data) > maxResultSize {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "result exceeds 10MB limit")
		}
		ctx := c.Context()
		if err := h.resultStore.SaveResult(ctx, namespace, taskName, data); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save result: %v", err))
		}
		return c.SendStatus(fiber.StatusNoContent)
	}

	lr := io.LimitReader(body, int64(maxResultSize)+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to read request body: %v", err))
	}
	if len(data) > maxResultSize {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "result exceeds 10MB limit")
	}
	if len(data) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "empty request body")
	}

	ctx := c.Context()
	if err := h.resultStore.SaveResult(ctx, namespace, taskName, data); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save result: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// UpdateExecutionWorkspaceStatus handles
// POST /internal/v1/tasks/{namespace}/{taskName}/execution-workspace/status.
func (h *InternalHandlers) UpdateExecutionWorkspaceStatus(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")

	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}
	if h.k8sClient == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "task status updates not enabled")
	}
	authorizer := h.internalCallerAuthorizer()
	if _, err := authorizer.verifyTaskCaller(c, namespace, taskName); err != nil {
		return err
	}

	var req statusrules.Update
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}
	statusForValidation := req.Status()
	if !statusrules.HasRequiredInboundFields(statusForValidation) {
		return fiber.NewError(fiber.StatusBadRequest, "provider, phase, and reason are required")
	}
	if !statusrules.ValidInboundStatus(statusForValidation) {
		return fiber.NewError(fiber.StatusBadRequest, "unsupported execution workspace status value")
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		status := req.Status()
		authorizedTask, err := authorizer.verifyTaskCaller(c, namespace, taskName)
		if err != nil {
			return err
		}
		task := &corev1alpha1.Task{}
		if err := h.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
			return err
		}
		if task.UID == "" || task.UID != authorizedTask.UID {
			return fiber.NewError(fiber.StatusForbidden, "task identity changed")
		}
		statusrules.PreserveReadyTelemetry(status, task.Status.ExecutionWorkspace)
		task.Status.ExecutionWorkspace = status
		return h.k8sClient.Status().Update(c.Context(), task)
	})
	if err != nil {
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			return fiberErr
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update execution workspace status: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// UploadArtifact handles POST /internal/v1/artifacts/{namespace}/{taskName}/{filename}.
// Workers call this to upload artifact files.
func (h *InternalHandlers) UploadArtifact(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")
	filename := c.Params("filename")

	if namespace == "" || taskName == "" || filename == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, taskName, and filename are required")
	}

	// Server-side filename validation (defense-in-depth)
	if len(filename) > 255 {
		return fiber.NewError(fiber.StatusBadRequest, "filename exceeds 255 character limit")
	}
	for _, r := range filename {
		if r < 0x20 || r == 0x7f {
			return fiber.NewError(fiber.StatusBadRequest, "filename contains invalid characters")
		}
	}
	if filename == "." || filename == ".." {
		return fiber.NewError(fiber.StatusBadRequest, "invalid filename")
	}

	if err := h.internalCallerAuthorizer().verifyArtifactUploadCaller(c, namespace, taskName); err != nil {
		return err
	}

	if h.artifactStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "artifact storage not enabled")
	}

	data := c.Body()
	if len(data) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "empty request body")
	}
	if len(data) > maxResultSize {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "artifact exceeds 10MB limit")
	}

	contentType := string(c.Request().Header.ContentType())
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	ctx := c.Context()
	if err := h.artifactStore.SaveArtifact(ctx, namespace, taskName, filename, contentType, data); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save artifact: %v", err))
	}

	return c.SendStatus(fiber.StatusCreated)
}

// GetSessionTranscript handles GET /internal/v1/sessions/{namespace}/{name}/transcript.
// Returns the session transcript as JSONL (one JSON object per line).
func (h *InternalHandlers) GetSessionTranscript(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	name, err := url.PathUnescape(c.Params("name"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "session name is invalid")
	}

	if namespace == "" || name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and name are required")
	}
	if h.sessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session storage not enabled")
	}

	authorizer := h.internalCallerAuthorizer()
	callerTask, err := authorizer.resolveActiveTaskCaller(c, namespace)
	if err != nil {
		return err
	}
	taskHint := strings.TrimSpace(c.Query("taskName", ""))
	if taskHint != "" && taskHint != callerTask.Name {
		return fiber.NewError(fiber.StatusForbidden, "task identity does not match caller")
	}

	ctx := c.Context()
	callerOwnsSession := callerTask.Spec.SessionRef != nil && callerTask.Spec.SessionRef.Name == name
	gatewayOwned := false
	var gatewayEvent *store.GatewayEvent
	if h.gatewayEventStore != nil {
		event, eventErr := h.gatewayEventStore.GetGatewayEventForTask(ctx, namespace, callerTask.Name, string(callerTask.UID))
		switch {
		case eventErr == nil:
			gatewayOwned = true
			gatewayEvent = event
			if strings.TrimSpace(event.SessionName) == "" || event.SessionName != name {
				return fiber.NewError(fiber.StatusForbidden, "task does not own this gateway session")
			}
		case errors.Is(eventErr, store.ErrNotFound):
		default:
			return fiber.NewError(fiber.StatusInternalServerError, "failed to load gateway transcript ownership")
		}
	}
	if !gatewayOwned && !callerOwnsSession {
		return fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this session")
	}

	sessionType, err := transcriptSessionType(ctx, h.sessionStore, namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to load session transcript policy")
	}
	if sessionType == store.SessionTypeGateway {
		if taskHint == "" {
			return fiber.NewError(fiber.StatusForbidden, "gateway session transcript requires authenticated task identity")
		}
		if h.gatewayEventStore == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "gateway transcript ownership lookup is unavailable")
		}
		if !gatewayOwned {
			return fiber.NewError(fiber.StatusForbidden, "task does not own this gateway session")
		}
	}

	maxMessages := 0
	throughMessageID := ""
	if gatewayOwned {
		maxMessages = store.GatewayTranscriptMessageLimit
		throughMessageID = store.GatewayUserMessageID(gatewayEvent.ID)
	} else if callerTask.Spec.SessionRef != nil && callerTask.Spec.SessionRef.Name == name {
		maxMessages = int(callerTask.Spec.SessionRef.MaxMessages)
		throughMessageID = callerTask.Spec.SessionRef.ThroughMessageID
	}

	var messages []store.SessionMessage
	if throughMessageID != "" {
		messages, err = h.sessionStore.LoadTranscriptThrough(ctx, namespace, name, throughMessageID, maxMessages)
	} else {
		messages, err = h.sessionStore.LoadTranscript(ctx, namespace, name, maxMessages)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load transcript: %v", err))
	}

	c.Set("Content-Type", "application/x-ndjson")

	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	for _, msg := range messages {
		if err := enc.Encode(msg); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode message: %v", err))
		}
	}

	return c.SendString(sb.String())
}

type transcriptSessionTypeReader interface {
	GetSessionType(ctx context.Context, namespace, name string) (string, error)
}

func transcriptSessionType(ctx context.Context, sessionStore store.SessionStore, namespace, name string) (string, error) {
	if reader, ok := sessionStore.(transcriptSessionTypeReader); ok {
		return reader.GetSessionType(ctx, namespace, name)
	}
	session, err := sessionStore.GetSession(ctx, namespace, name)
	if err != nil {
		return "", err
	}
	return session.SessionType, nil
}

// SearchTranscript handles GET /internal/v1/sessions/{namespace}/search.
// It searches namespace-scoped session transcripts and returns compact snippets.
func (h *InternalHandlers) SearchTranscript(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if h.sessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session storage not enabled")
	}

	authorizer := h.internalCallerAuthorizer()
	callerTask, err := authorizer.resolveActiveTaskCaller(c, namespace)
	if err != nil {
		return err
	}
	allowedSessions, err := authorizer.coordinationTreeSessionNames(c.Context(), callerTask)
	if err != nil {
		return err
	}
	if h.gatewayEventStore != nil {
		_, eventErr := h.gatewayEventStore.GetGatewayEventForTask(c.Context(), namespace, callerTask.Name, string(callerTask.UID))
		switch {
		case eventErr == nil:
			// Gateway turns are authorized against an event-specific transcript
			// cutoff. Transcript search has no cutoff field, so fail closed
			// instead of searching the full canonical session.
			return fiber.NewError(fiber.StatusForbidden, "gateway session transcript search is unavailable")
		case errors.Is(eventErr, store.ErrNotFound):
		default:
			return fiber.NewError(fiber.StatusInternalServerError, "failed to load gateway transcript ownership")
		}
	}
	allowedSessions, err = searchableTranscriptSessionNames(c.Context(), h.sessionStore, namespace, allowedSessions)
	if err != nil {
		return err
	}

	sessionName := strings.TrimSpace(c.Query("sessionName", ""))
	excludeSessionName := strings.TrimSpace(c.Query("excludeSessionName", ""))
	if sessionName != "" {
		if _, ok := allowedSessions[sessionName]; !ok {
			return fiber.NewError(fiber.StatusForbidden, "caller is not authorized for this session")
		}
	}

	query := strings.TrimSpace(c.Query("query", ""))
	if query == "" {
		return fiber.NewError(fiber.StatusBadRequest, "query is required")
	}

	limit, err := parseOptionalLimit(c.Query("limit", ""))
	if err != nil {
		return err
	}
	maxSnippetLength, err := parseOptionalNonNegativeQueryInt(c.Query("maxSnippetLength", ""), "maxSnippetLength")
	if err != nil {
		return err
	}

	results, err := searchAuthorizedTranscriptResults(c.Context(), h.sessionStore, store.TranscriptSearchFilter{
		Namespace:          namespace,
		Query:              query,
		SessionName:        sessionName,
		ExcludeSessionName: excludeSessionName,
		Roles:              splitCSV(c.Query("roles", "")),
		Limit:              limit,
		MaxSnippetLength:   maxSnippetLength,
	}, allowedSessions)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to search transcript: %v", err))
	}
	if results == nil {
		results = []store.TranscriptSearchResult{}
	}
	return c.JSON(results)
}

func searchableTranscriptSessionNames(
	ctx context.Context,
	sessionStore store.SessionStore,
	namespace string,
	allowedSessions map[string]struct{},
) (map[string]struct{}, error) {
	searchable := make(map[string]struct{}, len(allowedSessions))
	for sessionName := range allowedSessions {
		sessionType, err := transcriptSessionType(ctx, sessionStore, namespace, sessionName)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to load session transcript policy")
		case sessionType == store.SessionTypeGateway:
			continue
		default:
			searchable[sessionName] = struct{}{}
		}
	}
	return searchable, nil
}

func searchAuthorizedTranscriptResults(
	ctx context.Context,
	sessionStore store.SessionStore,
	filter store.TranscriptSearchFilter,
	allowedSessions map[string]struct{},
) ([]store.TranscriptSearchResult, error) {
	if filter.SessionName != "" {
		return sessionStore.SearchTranscript(ctx, filter)
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultInternalTranscriptSearchLimit
	}
	if limit > maxInternalTranscriptSearchLimit {
		limit = maxInternalTranscriptSearchLimit
	}
	sessionNames := make([]string, 0, len(allowedSessions))
	for sessionName := range allowedSessions {
		if sessionName != filter.ExcludeSessionName {
			sessionNames = append(sessionNames, sessionName)
		}
	}
	sort.Strings(sessionNames)

	if len(sessionNames) == 0 {
		return []store.TranscriptSearchResult{}, nil
	}
	filter.SessionNames = sessionNames
	filter.ExcludeSessionName = ""
	filter.Limit = limit
	return sessionStore.SearchTranscript(ctx, filter)
}

func parseOptionalNonNegativeQueryInt(raw, name string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fiber.NewError(fiber.StatusBadRequest, "invalid "+name)
	}
	return value, nil
}

// SubmitPlan handles POST /internal/v1/plans/{namespace}/{taskName}.
// Workers call this to persist autonomous plan state.
func (h *InternalHandlers) SubmitPlan(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")

	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}

	if _, err := h.internalCallerAuthorizer().verifyTaskCaller(c, namespace, taskName); err != nil {
		return err
	}

	if h.planStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "plan storage not enabled")
	}

	var plan struct {
		Summary      string `json:"summary"`
		ProgressPct  int    `json:"progress_pct"`
		GoalComplete bool   `json:"goal_complete"`
		PlanDocument string `json:"plan_document"`
	}
	if err := c.Bind().JSON(&plan); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	planState := &store.PlanState{
		TaskName:     taskName,
		Namespace:    namespace,
		Summary:      plan.Summary,
		ProgressPct:  plan.ProgressPct,
		GoalComplete: plan.GoalComplete,
		PlanDocument: plan.PlanDocument,
	}

	ctx := c.Context()
	if err := h.planStore.SavePlan(ctx, namespace, taskName, planState); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save plan: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetPlan handles GET /internal/v1/plans/{namespace}/{taskName}.
// Workers call this to load the current plan state at startup.
func (h *InternalHandlers) GetPlan(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")

	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}

	if _, err := h.internalCallerAuthorizer().verifyTaskCaller(c, namespace, taskName); err != nil {
		return err
	}

	if h.planStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "plan storage not enabled")
	}

	ctx := c.Context()
	plan, err := h.planStore.GetPlan(ctx, namespace, taskName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "plan not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get plan: %v", err))
	}

	return c.JSON(plan)
}

// SendMessage handles POST /internal/v1/messages/{namespace}.
// Workers call this to send messages to sibling tasks.
func (h *InternalHandlers) SendMessage(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}

	if h.messageStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "messaging not enabled")
	}

	var req struct {
		FromTask   string `json:"fromTask"`
		ToTask     string `json:"toTask"`
		ParentTask string `json:"parentTask"`
		Content    string `json:"content"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}

	if req.FromTask == "" || req.ToTask == "" || req.Content == "" || req.ParentTask == "" {
		return fiber.NewError(fiber.StatusBadRequest, "fromTask, toTask, parentTask, and content are required")
	}
	if err := h.internalCallerAuthorizer().verifyMessageSender(
		c,
		namespace,
		req.FromTask,
		req.ToTask,
		req.ParentTask,
	); err != nil {
		return err
	}

	msg := &store.Message{
		Namespace:  namespace,
		FromTask:   req.FromTask,
		ToTask:     req.ToTask,
		ParentTask: req.ParentTask,
		Content:    req.Content,
	}

	ctx := c.Context()
	if err := h.messageStore.SendMessage(ctx, msg); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to send message: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetMessages handles GET /internal/v1/messages/{namespace}/{taskName}.
// Workers call this to check for messages from sibling tasks.
func (h *InternalHandlers) GetMessages(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")

	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}

	if h.messageStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "messaging not enabled")
	}

	parentTask := c.Query("parentTask")
	if parentTask == "" {
		return fiber.NewError(fiber.StatusBadRequest, "parentTask query parameter is required")
	}
	if err := h.internalCallerAuthorizer().verifyMessageInbox(c, namespace, taskName, parentTask); err != nil {
		return err
	}

	markRead := c.Query("markRead", queryTrue) == queryTrue

	ctx := c.Context()
	messages, err := h.messageStore.GetMessages(ctx, namespace, taskName, parentTask, markRead)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get messages: %v", err))
	}

	if messages == nil {
		messages = []store.Message{}
	}

	return c.JSON(messages)
}
