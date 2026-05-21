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
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
)

const maxResultSize = 10 << 20 // 10MB

// InternalHandlers contains handlers for internal worker endpoints.
type InternalHandlers struct {
	k8sClient           client.Client
	resultStore         store.ResultStore
	sessionStore        store.SessionStore
	planStore           store.PlanStore
	messageStore        store.MessageStore
	artifactStore       store.ArtifactStore
	memoryStore         store.MemoryStore
	memoryProposalStore store.MemoryProposalStore
}

// InternalHandlersConfig holds optional configuration for internal handlers.
type InternalHandlersConfig struct {
	Client              client.Client
	MemoryStore         store.MemoryStore
	MemoryProposalStore store.MemoryProposalStore
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
		h.memoryStore = configs[0].MemoryStore
		h.memoryProposalStore = configs[0].MemoryProposalStore
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

	// Verify caller namespace matches the URL namespace
	if err := verifyCallerNamespace(c, namespace); err != nil {
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
	if err := verifyCallerNamespace(c, namespace); err != nil {
		return err
	}
	if h.k8sClient == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "task status updates not enabled")
	}

	var req executionWorkspaceStatusRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}
	status := req.status()
	if status.Provider == "" || status.Phase == "" || status.Reason == "" {
		return fiber.NewError(fiber.StatusBadRequest, "provider, phase, and reason are required")
	}
	if !validExecutionWorkspaceStatus(status) {
		return fiber.NewError(fiber.StatusBadRequest, "unsupported execution workspace status value")
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		task := &corev1alpha1.Task{}
		if err := h.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
			return err
		}
		if err := verifyCallerOwnsTaskWorker(c.Context(), h.k8sClient, GetUserInfo(c), task); err != nil {
			return err
		}
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

type executionWorkspaceStatusRequest struct {
	Provider      corev1alpha1.WorkspaceProvider           `json:"provider"`
	TemplateRef   *corev1alpha1.WorkspaceTemplateReference `json:"templateRef,omitempty"`
	Phase         corev1alpha1.ExecutionWorkspacePhase     `json:"phase"`
	Reason        corev1alpha1.ExecutionWorkspaceReason    `json:"reason"`
	ReusePolicy   corev1alpha1.WorkspaceReusePolicy        `json:"reusePolicy,omitempty"`
	CleanupPolicy corev1alpha1.WorkspaceCleanupPolicy      `json:"cleanupPolicy,omitempty"`
	Reused        bool                                     `json:"reused,omitempty"`
	Message       string                                   `json:"message,omitempty"`
	ObservedAt    *metav1.Time                             `json:"observedAt,omitempty"`
}

func (r executionWorkspaceStatusRequest) status() *corev1alpha1.ExecutionWorkspaceStatus {
	updateTime := r.ObservedAt
	if updateTime == nil {
		now := metav1.Now()
		updateTime = &now
	}
	return &corev1alpha1.ExecutionWorkspaceStatus{
		Provider:       r.Provider,
		TemplateRef:    r.TemplateRef,
		Phase:          r.Phase,
		Reason:         r.Reason,
		ReusePolicy:    r.ReusePolicy,
		CleanupPolicy:  r.CleanupPolicy,
		Reused:         r.Reused,
		Message:        sanitizeWorkspaceStatusMessage(r.Message),
		LastUpdateTime: updateTime,
	}
}

func validExecutionWorkspaceStatus(status *corev1alpha1.ExecutionWorkspaceStatus) bool {
	if status == nil {
		return false
	}
	switch status.Provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox, corev1alpha1.WorkspaceProviderSubstrate:
	default:
		return false
	}
	switch status.Phase {
	case corev1alpha1.ExecutionWorkspacePhasePending,
		corev1alpha1.ExecutionWorkspacePhaseReady,
		corev1alpha1.ExecutionWorkspacePhaseReleased,
		corev1alpha1.ExecutionWorkspacePhaseRetained,
		corev1alpha1.ExecutionWorkspacePhaseDeleted,
		corev1alpha1.ExecutionWorkspacePhaseFailed:
	default:
		return false
	}
	switch status.Reason {
	case corev1alpha1.ExecutionWorkspaceReasonPending,
		corev1alpha1.ExecutionWorkspaceReasonClaimed,
		corev1alpha1.ExecutionWorkspaceReasonReady,
		corev1alpha1.ExecutionWorkspaceReasonReleased,
		corev1alpha1.ExecutionWorkspaceReasonRetained,
		corev1alpha1.ExecutionWorkspaceReasonDeleted,
		corev1alpha1.ExecutionWorkspaceReasonValidationFailed,
		corev1alpha1.ExecutionWorkspaceReasonAttachmentLocked,
		corev1alpha1.ExecutionWorkspaceReasonClaimFailed,
		corev1alpha1.ExecutionWorkspaceReasonReadinessFailed,
		corev1alpha1.ExecutionWorkspaceReasonHandoffFailed,
		corev1alpha1.ExecutionWorkspaceReasonCommandFailed,
		corev1alpha1.ExecutionWorkspaceReasonSecretScrubFailed,
		corev1alpha1.ExecutionWorkspaceReasonCleanupFailed,
		corev1alpha1.ExecutionWorkspaceReasonStatusUpdateFailed:
	default:
		return false
	}
	switch status.ReusePolicy {
	case "", corev1alpha1.WorkspaceReusePolicyNone, corev1alpha1.WorkspaceReusePolicySession:
	default:
		return false
	}
	switch status.CleanupPolicy {
	case "", corev1alpha1.WorkspaceCleanupPolicyDelete, corev1alpha1.WorkspaceCleanupPolicyRetain:
	default:
		return false
	}
	return true
}

func sanitizeWorkspaceStatusMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 1024 {
		return message[:1024] + "...<truncated>"
	}
	return message
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

	if err := verifyCallerNamespace(c, namespace); err != nil {
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
	name := c.Params("name")

	if namespace == "" || name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and name are required")
	}

	if err := verifyCallerNamespace(c, namespace); err != nil {
		return err
	}

	if h.sessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session storage not enabled")
	}

	ctx := c.Context()
	messages, err := h.sessionStore.LoadTranscript(ctx, namespace, name, 0)
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

// SearchTranscript handles GET /internal/v1/sessions/{namespace}/search.
// It searches namespace-scoped session transcripts and returns compact snippets.
func (h *InternalHandlers) SearchTranscript(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}

	if err := verifyCallerNamespace(c, namespace); err != nil {
		return err
	}

	if h.sessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session storage not enabled")
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

	results, err := h.sessionStore.SearchTranscript(c.Context(), store.TranscriptSearchFilter{
		Namespace:          namespace,
		Query:              query,
		SessionName:        strings.TrimSpace(c.Query("sessionName", "")),
		ExcludeSessionName: strings.TrimSpace(c.Query("excludeSessionName", "")),
		Roles:              splitCSV(c.Query("roles", "")),
		Limit:              limit,
		MaxSnippetLength:   maxSnippetLength,
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to search transcript: %v", err))
	}
	if results == nil {
		results = []store.TranscriptSearchResult{}
	}
	return c.JSON(results)
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

	if err := verifyCallerNamespace(c, namespace); err != nil {
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

	if err := verifyCallerNamespace(c, namespace); err != nil {
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

// verifyCallerNamespace checks that the authenticated caller's SA namespace
// matches the target namespace in the URL path.
func verifyCallerNamespace(c fiber.Ctx, namespace string) error {
	userInfo := GetUserInfo(c)
	if userInfo == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}

	if userInfo.Namespace != "" && userInfo.Namespace != namespace {
		log.Info("cross-namespace access denied",
			"callerNamespace", userInfo.Namespace,
			"targetNamespace", namespace,
			"username", userInfo.Username,
			"ip", c.IP(),
		)
		return fiber.NewError(fiber.StatusForbidden, "cross-namespace access denied")
	}

	// SA usernames follow the format: system:serviceaccount:<namespace>:<name>
	parts := strings.Split(userInfo.Username, ":")
	if len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" { //nolint:goconst // "system" here is K8s SA prefix, not chat role
		if parts[2] != namespace {
			log.Info("cross-namespace access denied",
				"callerNamespace", parts[2],
				"targetNamespace", namespace,
				"username", userInfo.Username,
				"ip", c.IP(),
			)
			return fiber.NewError(fiber.StatusForbidden, "cross-namespace access denied")
		}
	}

	return nil
}

func verifyCallerOwnsTaskWorker(ctx context.Context, c client.Client, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if c == nil || userInfo == nil || task == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if userInfo.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "caller pod token required")
	}
	podName := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-name")
	podUID := firstUserExtra(userInfo, "authentication.kubernetes.io/pod-uid")
	if podName == "" || podUID == "" {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity required")
	}

	pod := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: podName}, pod); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "caller pod not found")
	}
	if string(pod.UID) != podUID {
		return fiber.NewError(fiber.StatusForbidden, "caller pod identity mismatch")
	}
	if pod.Labels[labels.LabelTask] != labels.SelectorValue(task.Name) {
		return fiber.NewError(fiber.StatusForbidden, "caller pod does not belong to task")
	}

	for _, owner := range pod.OwnerReferences {
		if owner.Kind != "Job" || owner.Name == "" {
			continue
		}
		job := &batchv1.Job{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: owner.Name}, job); err != nil {
			return fiber.NewError(fiber.StatusForbidden, "caller job not found")
		}
		if owner.UID != "" && owner.UID != job.UID {
			continue
		}
		for _, jobOwner := range job.OwnerReferences {
			if jobOwner.Kind == "Task" && jobOwner.UID == task.UID {
				return nil
			}
		}
	}
	return fiber.NewError(fiber.StatusForbidden, "caller is not a worker for this task")
}

func firstUserExtra(userInfo *UserInfo, key string) string {
	if userInfo == nil || len(userInfo.Extra) == 0 {
		return ""
	}
	values := userInfo.Extra[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// SendMessage handles POST /internal/v1/messages/{namespace}.
// Workers call this to send messages to sibling tasks.
func (h *InternalHandlers) SendMessage(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}

	if err := verifyCallerNamespace(c, namespace); err != nil {
		return err
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

	if err := verifyCallerNamespace(c, namespace); err != nil {
		return err
	}

	if h.messageStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "messaging not enabled")
	}

	parentTask := c.Query("parentTask")
	if parentTask == "" {
		return fiber.NewError(fiber.StatusBadRequest, "parentTask query parameter is required")
	}

	markRead := c.Query("markRead", "true") == "true"

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
