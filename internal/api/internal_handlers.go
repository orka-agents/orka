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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/harness"
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
	k8sClient               client.Client
	apiReader               client.Reader
	resultStore             store.ResultStore
	sessionStore            store.SessionStore
	planStore               store.PlanStore
	messageStore            store.MessageStore
	artifactStore           store.ArtifactStore
	executionEventStore     store.ExecutionEventStore
	gatewayEventStore       store.GatewayEventStore
	memoryStore             store.MemoryStore
	memoryProposalStore     store.MemoryProposalStore
	taskProvenanceProtected bool
}

// InternalHandlersConfig holds optional configuration for internal handlers.
type InternalHandlersConfig struct {
	Client              client.Client
	APIReader           client.Reader
	MemoryStore         store.MemoryStore
	MemoryProposalStore store.MemoryProposalStore
	ExecutionEventStore store.ExecutionEventStore
	GatewayEventStore   store.GatewayEventStore
	// TaskProvenanceProtected permits cross-task coordination only when the
	// Task provenance admission webhook protects coordination ancestry.
	TaskProvenanceProtected bool
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
		h.taskProvenanceProtected = configs[0].TaskProvenanceProtected
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

	authorizer := h.internalCallerAuthorizer()
	authorizedTask, err := authorizer.verifyTaskCaller(c, namespace, taskName)
	if err != nil {
		return err
	}

	if h.resultStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "result storage not enabled")
	}

	// Finish the bounded body read before reserving the store writer. A slow
	// upload must not hold up Task deletion or preserve an earlier authorization.
	body := c.Request().BodyStream()
	var data []byte
	if body == nil {
		data = c.Body()
	} else {
		data, err = io.ReadAll(io.LimitReader(body, int64(maxResultSize)+1))
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to read request body: %v", err))
		}
	}
	if len(data) > maxResultSize {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "result exceeds 10MB limit")
	}
	if len(data) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "empty request body")
	}

	if err := withInternalTaskDataTransaction(c, h.resultStore, taskName, func(context.Context) error {
		return authorizer.revalidateTaskCaller(c, authorizedTask)
	}, func(ctx context.Context) error {
		return h.resultStore.SaveResult(ctx, namespace, taskName, data)
	}); err != nil {
		return err
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
		task, err := authorizer.verifyTaskCaller(c, namespace, taskName)
		if err != nil {
			return err
		}
		// Update the exact authorized version. A concurrent controller change
		// must conflict and repeat authorization before the worker can write.
		statusrules.PreserveReadyTelemetry(status, task.Status.ExecutionWorkspace)
		if previous := task.Status.ExecutionWorkspace; previous != nil {
			// Workers report legacy provider status; attachment authority and
			// finalization state remain owned by the controller.
			status.ClassRef = previous.ClassRef
			status.WorkspaceRef = previous.WorkspaceRef
			status.State = previous.State
			status.AttachedEpoch = previous.AttachedEpoch
			status.Conditions = previous.Conditions
		}
		task.Status.ExecutionWorkspace = status
		return h.k8sClient.Status().Update(c.Context(), task)
	})
	if err != nil {
		if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
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

	var authorizedWorker *corev1alpha1.Task
	if c.Get(artifactcap.CapabilityHeader) == "" {
		var err error
		authorizedWorker, err = h.internalCallerAuthorizer().verifyTaskCaller(c, namespace, taskName)
		if err != nil {
			return err
		}
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

	var verifyHarnessAttempt func(context.Context) error
	if err := withInternalTaskDataTransaction(c, h.artifactStore, taskName, func(ctx context.Context) error {
		if authorizedWorker != nil {
			return h.internalCallerAuthorizer().revalidateTaskCaller(c, authorizedWorker)
		}
		var err error
		verifyHarnessAttempt, err = h.prepareHarnessV1ArtifactUpload(ctx, c, harness.ArtifactUpload{
			Namespace: namespace, TaskName: taskName, Filename: filename, ContentType: contentType, Data: data,
		})
		return err
	}, func(ctx context.Context) error {
		if verifyHarnessAttempt != nil {
			if err := verifyHarnessAttempt(ctx); err != nil {
				return err
			}
		}
		return h.artifactStore.SaveArtifact(ctx, namespace, taskName, filename, contentType, data)
	}); err != nil {
		return err
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

	var messages []store.SessionMessage
	var callerTask *corev1alpha1.Task
	if err := withInternalTaskDataTransaction(c, h.sessionStore, "", func(context.Context) error {
		var err error
		callerTask, err = h.internalCallerAuthorizer().resolveActiveTaskCaller(c, namespace)
		return err
	}, func(ctx context.Context) error {
		taskHint := strings.TrimSpace(c.Query("taskName", ""))
		if taskHint != "" && taskHint != callerTask.Name {
			return fiber.NewError(fiber.StatusForbidden, "task identity does not match caller")
		}

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
		return nil
	}); err != nil {
		return err
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

	var results []store.TranscriptSearchResult
	var callerTask *corev1alpha1.Task
	var allowedSessions map[string][]corev1alpha1.SessionReference
	if err := withInternalTaskDataTransaction(c, h.sessionStore, "", func(ctx context.Context) error {
		authorizer := h.internalCallerAuthorizer()
		var err error
		callerTask, err = authorizer.resolveActiveTaskCaller(c, namespace)
		if err != nil {
			return err
		}
		allowedSessions, err = authorizer.coordinationTreeSessionReferences(ctx, callerTask)
		return err
	}, func(ctx context.Context) error {
		sessionName := strings.TrimSpace(c.Query("sessionName", ""))
		excludeSessionName := strings.TrimSpace(c.Query("excludeSessionName", ""))
		if err := authorizeTaskTranscriptSearch(ctx, h.sessionStore, h.gatewayEventStore, callerTask, sessionName, allowedSessions); err != nil {
			return err
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

		results, err = searchAuthorizedTranscriptResults(ctx, h.sessionStore, store.TranscriptSearchFilter{
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
		return nil
	}); err != nil {
		return err
	}
	if results == nil {
		results = []store.TranscriptSearchResult{}
	}
	return c.JSON(results)
}

func searchAuthorizedTranscriptResults(
	ctx context.Context,
	sessionStore store.SessionStore,
	filter store.TranscriptSearchFilter,
	allowedSessions map[string][]corev1alpha1.SessionReference,
) ([]store.TranscriptSearchResult, error) {
	if filter.SessionName != "" {
		filter.HistoryBounds = transcriptSearchHistoryBounds(filter.SessionName, allowedSessions[filter.SessionName])
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
	for _, sessionName := range sessionNames {
		filter.HistoryBounds = append(filter.HistoryBounds, transcriptSearchHistoryBounds(sessionName, allowedSessions[sessionName])...)
	}
	return sessionStore.SearchTranscript(ctx, filter)
}

func transcriptSearchHistoryBounds(sessionName string, refs []corev1alpha1.SessionReference) []store.TranscriptSearchHistoryBound {
	var bounds []store.TranscriptSearchHistoryBound
	for _, ref := range refs {
		if ref.MaxMessages != 0 || ref.ThroughMessageID != "" {
			bounds = append(bounds, store.TranscriptSearchHistoryBound{
				SessionName: sessionName, MaxMessages: int(ref.MaxMessages), ThroughMessageID: ref.ThroughMessageID,
			})
		}
	}
	return bounds
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

	authorizer := h.internalCallerAuthorizer()
	authorizedTask, err := authorizer.verifyTaskCaller(c, namespace, taskName)
	if err != nil {
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

	if err := withInternalTaskDataTransaction(c, h.planStore, taskName, func(context.Context) error {
		return authorizer.revalidateTaskCaller(c, authorizedTask)
	}, func(ctx context.Context) error {
		return h.planStore.SavePlan(ctx, namespace, taskName, planState)
	}); err != nil {
		return err
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

	if h.planStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "plan storage not enabled")
	}

	var plan *store.PlanState
	if err := withInternalTaskDataTransaction(c, h.planStore, taskName, func(context.Context) error {
		_, err := h.internalCallerAuthorizer().verifyTaskCaller(c, namespace, taskName)
		return err
	}, func(ctx context.Context) error {
		var err error
		plan, err = h.planStore.GetPlan(ctx, namespace, taskName)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fiber.NewError(fiber.StatusNotFound, "plan not found")
			}
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get plan: %v", err))
		}
		return nil
	}); err != nil {
		return err
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
	msg := &store.Message{
		Namespace:  namespace,
		FromTask:   req.FromTask,
		ToTask:     req.ToTask,
		ParentTask: req.ParentTask,
		Content:    req.Content,
	}

	if err := withInternalTaskDataTransaction(c, h.messageStore, "", func(context.Context) error {
		return h.internalCallerAuthorizer().verifyMessageSender(c, namespace, req.FromTask, req.ToTask, req.ParentTask)
	}, func(ctx context.Context) error {
		return h.messageStore.SendMessage(ctx, msg)
	}); err != nil {
		return err
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
	markRead := c.Query("markRead", queryTrue) == queryTrue
	var messages []store.Message
	if err := withInternalTaskDataTransaction(c, h.messageStore, "", func(context.Context) error {
		return h.internalCallerAuthorizer().verifyMessageInbox(c, namespace, taskName, parentTask)
	}, func(ctx context.Context) error {
		var err error
		messages, err = h.messageStore.GetMessages(ctx, namespace, taskName, parentTask, markRead)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get messages: %v", err))
		}
		return nil
	}); err != nil {
		return err
	}

	if messages == nil {
		messages = []store.Message{}
	}

	return c.JSON(messages)
}
