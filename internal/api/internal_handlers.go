/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/sozercan/orka/internal/store"
)

const maxResultSize = 10 << 20 // 10MB

// InternalHandlers contains handlers for internal worker endpoints.
type InternalHandlers struct {
	resultStore   store.ResultStore
	sessionStore  store.SessionStore
	planStore     store.PlanStore
	messageStore  store.MessageStore
	artifactStore store.ArtifactStore
}

// NewInternalHandlers creates a new InternalHandlers instance.
func NewInternalHandlers(rs store.ResultStore, ss store.SessionStore, ps store.PlanStore, ms store.MessageStore, as store.ArtifactStore) *InternalHandlers {
	return &InternalHandlers{
		resultStore:   rs,
		sessionStore:  ss,
		planStore:     ps,
		messageStore:  ms,
		artifactStore: as,
	}
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

// UploadArtifact handles POST /internal/v1/artifacts/{namespace}/{taskName}/{filename}.
// Workers call this to upload artifact files.
func (h *InternalHandlers) UploadArtifact(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")
	filename := c.Params("filename")

	if namespace == "" || taskName == "" || filename == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace, taskName, and filename are required")
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

// SendMessage handles POST /internal/v1/messages/{namespace}.
// Workers call this to send messages to sibling tasks.
func (h *InternalHandlers) SendMessage(c fiber.Ctx) error {
	if h.messageStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "messaging not enabled")
	}

	namespace := c.Params("namespace")
	if namespace == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}

	if err := verifyCallerNamespace(c, namespace); err != nil {
		return err
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
	if h.messageStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "messaging not enabled")
	}

	namespace := c.Params("namespace")
	taskName := c.Params("taskName")

	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
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
