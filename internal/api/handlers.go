/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/controller"
	"github.com/sozercan/mercan/internal/store"
)

// Handlers contains all API handlers
//
// builtinToolsList defines the built-in tools returned by list/get endpoints.
var builtinToolsList = []fiber.Map{
	{"name": "web_search", "builtin": true, "description": "Search the web"},
	{"name": "code_exec", "builtin": true, "description": "Execute code in sandbox"},
	{"name": "file_read", "builtin": true, "description": "Read files from workspace"},
}

// builtinToolsMap indexes built-in tools by name for single-tool lookup.
var builtinToolsMap = func() map[string]fiber.Map {
	m := make(map[string]fiber.Map, len(builtinToolsList))
	for _, t := range builtinToolsList {
		m[t["name"].(string)] = t
	}
	return m
}()

type Handlers struct {
	client         client.Client
	sessionManager *controller.SessionManager
	watchNamespace string
	resultStore    store.ResultStore
	sessionStore   store.SessionStore
}

// NewHandlers creates a new Handlers instance
func NewHandlers(c client.Client, sessionManager *controller.SessionManager, watchNamespace string, rs store.ResultStore, ss store.SessionStore) *Handlers {
	return &Handlers{
		client:         c,
		sessionManager: sessionManager,
		watchNamespace: watchNamespace,
		resultStore:    rs,
		sessionStore:   ss,
	}
}

// CreateAgentRequest is the request body for creating an agent
type CreateAgentRequest struct {
	Name      string                 `json:"name"`
	Namespace string                 `json:"namespace"`
	Spec      corev1alpha1.AgentSpec `json:"spec"`
}

// UpdateAgentRequest is the request body for updating an agent
type UpdateAgentRequest struct {
	Spec corev1alpha1.AgentSpec `json:"spec"`
}

// CreateTaskRequest is the request body for creating a task
type CreateTaskRequest struct {
	Name              string                         `json:"name"`
	Namespace         string                         `json:"namespace"`
	Type              corev1alpha1.TaskType          `json:"type"`
	Image             string                         `json:"image,omitempty"`
	Command           []string                       `json:"command,omitempty"`
	Args              []string                       `json:"args,omitempty"`
	Env               []corev1.EnvVar                `json:"env,omitempty"`
	Timeout           string                         `json:"timeout,omitempty"`
	Priority          *int32                         `json:"priority,omitempty"`
	RetryPolicy       *corev1alpha1.RetryPolicy      `json:"retryPolicy,omitempty"`
	WebhookURL        string                         `json:"webhookURL,omitempty"`
	SecretRef         *corev1alpha1.SecretReference  `json:"secretRef,omitempty"`
	SessionRef        *corev1alpha1.SessionReference `json:"sessionRef,omitempty"`
	AI                *corev1alpha1.AISpec           `json:"ai,omitempty"`
	AgentRef          *corev1alpha1.AgentReference   `json:"agentRef,omitempty"`
	Prompt            string                         `json:"prompt,omitempty"`
	AgentRuntime      *corev1alpha1.AgentRuntimeSpec `json:"agentRuntime,omitempty"`
	Schedule          string                         `json:"schedule,omitempty"`
	TimeZone          *string                        `json:"timeZone,omitempty"`
	ConcurrencyPolicy string                         `json:"concurrencyPolicy,omitempty"`
	Suspend           *bool                          `json:"suspend,omitempty"`
}

// ListResponse is a generic list response with pagination
type ListResponse struct {
	Items    any      `json:"items"`
	Metadata ListMeta `json:"metadata"`
}

// ListMeta contains pagination metadata
type ListMeta struct {
	Continue           string `json:"continue,omitempty"`
	RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
}

// Healthz handles health check requests
func (h *Handlers) Healthz(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"status": "ok"})
}

// Readyz handles readiness check requests
func (h *Handlers) Readyz(c fiber.Ctx) error {
	// Could add more sophisticated checks here
	return c.JSON(fiber.Map{"status": "ok"})
}

// CreateTask creates a new task
func (h *Handlers) CreateTask(c fiber.Ctx) error {
	var req CreateTaskRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	// Validate required fields
	if req.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}
	if req.Type == "" {
		return fiber.NewError(fiber.StatusBadRequest, "type is required")
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = "default"
	}

	// Check namespace scope
	if h.watchNamespace != "" && h.watchNamespace != namespace {
		return fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: corev1alpha1.TaskSpec{
			Type:         req.Type,
			Image:        req.Image,
			Command:      req.Command,
			Args:         req.Args,
			Env:          req.Env,
			Priority:     req.Priority,
			RetryPolicy:  req.RetryPolicy,
			WebhookURL:   req.WebhookURL,
			SecretRef:    req.SecretRef,
			SessionRef:   req.SessionRef,
			AI:           req.AI,
			AgentRef:     req.AgentRef,
			Prompt:       req.Prompt,
			AgentRuntime: req.AgentRuntime,
			Schedule:     req.Schedule,
			Suspend:      req.Suspend,
		},
	}

	// Parse timeout if provided
	if req.Timeout != "" {
		duration, err := parseDuration(req.Timeout)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid timeout: %v", err))
		}
		task.Spec.Timeout = duration
	}

	if req.Schedule != "" {
		task.Spec.Schedule = req.Schedule
		task.Spec.TimeZone = req.TimeZone
		if req.ConcurrencyPolicy != "" {
			task.Spec.ConcurrencyPolicy = corev1alpha1.ConcurrencyPolicy(req.ConcurrencyPolicy)
		}
	}

	ctx := c.Context()
	if err := h.client.Create(ctx, task); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "task already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create task: %v", err))
	}

	return c.Status(fiber.StatusCreated).JSON(task)
}

// ListTasks lists tasks
func (h *Handlers) ListTasks(c fiber.Ctx) error {
	namespace := c.Query("namespace", "")
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	opts := &client.ListOptions{}

	// Apply namespace filter
	if namespace != "" {
		opts.Namespace = namespace
	} else if h.watchNamespace != "" {
		opts.Namespace = h.watchNamespace
	}

	// Apply pagination
	pagination, err := ParsePagination(limit, continueToken)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	opts.Limit = pagination.Limit
	opts.Continue = pagination.Continue

	taskList := &corev1alpha1.TaskList{}
	ctx := c.Context()
	if err := h.client.List(ctx, taskList, opts); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list tasks: %v", err))
	}

	response := ListResponse{
		Items: taskList.Items,
		Metadata: ListMeta{
			Continue:           taskList.Continue,
			RemainingItemCount: taskList.RemainingItemCount,
		},
	}

	return c.JSON(response)
}

// GetTask gets a task by ID
func (h *Handlers) GetTask(c fiber.Ctx) error {
	id := c.Params("id")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}

	return c.JSON(task)
}

// DeleteTask deletes a task
func (h *Handlers) DeleteTask(c fiber.Ctx) error {
	id := c.Params("id")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}

	if err := h.client.Delete(ctx, task); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete task: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetTaskLogs gets logs for a task
func (h *Handlers) GetTaskLogs(c fiber.Ctx) error {
	id := c.Params("id")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}

	// For completed tasks with results available, serve from ResultStore
	if task.Status.ResultRef != nil && task.Status.ResultRef.Available {
		data, err := h.resultStore.GetResult(ctx, namespace, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fiber.NewError(fiber.StatusNotFound, "logs not found in result store")
			}
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get logs: %v", err))
		}
		return c.JSON(fiber.Map{
			"logs": string(data),
		})
	}

	// For pending/scheduled tasks with no job yet
	if task.Status.JobName == "" {
		return fiber.NewError(fiber.StatusNotFound, "task is pending, no logs available yet")
	}

	// For running tasks, live log streaming is not available without a kubernetes clientset
	return c.JSON(fiber.Map{
		"message": "live log streaming not available",
		"jobName": task.Status.JobName,
	})
}

// GetTaskResult gets the result of a task
func (h *Handlers) GetTaskResult(c fiber.Ctx) error {
	id := c.Params("id")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}

	if task.Status.ResultRef == nil || !task.Status.ResultRef.Available {
		return fiber.NewError(fiber.StatusNotFound, "task has no result")
	}

	data, err := h.resultStore.GetResult(ctx, namespace, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "result not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get result: %v", err))
	}

	return c.JSON(fiber.Map{
		"result": string(data),
	})
}

// ListSessions lists sessions
func (h *Handlers) ListSessions(c fiber.Ctx) error {
	namespace := c.Query("namespace", "")

	if namespace == "" && h.watchNamespace != "" {
		namespace = h.watchNamespace
	}
	if namespace == "" {
		namespace = "default"
	}

	ctx := c.Context()
	sessions, err := h.sessionStore.ListSessions(ctx, namespace)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list sessions: %v", err))
	}

	items := make([]fiber.Map, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, fiber.Map{
			"name":         s.Name,
			"namespace":    namespace,
			"sessionType":  s.SessionType,
			"messageCount": s.MessageCount,
			"inputTokens":  s.InputTokens,
			"outputTokens": s.OutputTokens,
			"activeTask":   s.ActiveTask,
			"createdAt":    s.CreatedAt.Format(time.RFC3339),
			"updatedAt":    s.UpdatedAt.Format(time.RFC3339),
		})
	}

	response := ListResponse{
		Items:    items,
		Metadata: ListMeta{},
	}

	return c.JSON(response)
}

// GetSession gets a session
func (h *Handlers) GetSession(c fiber.Ctx) error {
	id := c.Params("id")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	ctx := c.Context()
	session, err := h.sessionStore.GetSession(ctx, namespace, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}

	// Build JSONL transcript from messages for backward compatibility
	var transcript string
	if len(session.Messages) > 0 {
		lines := make([]string, 0, len(session.Messages))
		for _, msg := range session.Messages {
			b, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			lines = append(lines, string(b))
		}
		transcript = strings.Join(lines, "\n")
	}

	return c.JSON(fiber.Map{
		"name":         id,
		"namespace":    namespace,
		"transcript":   transcript,
		"messageCount": session.MessageCount,
		"inputTokens":  session.InputTokens,
		"outputTokens": session.OutputTokens,
		"activeTask":   session.ActiveTask,
		"createdAt":    session.CreatedAt.Format(time.RFC3339),
		"updatedAt":    session.UpdatedAt.Format(time.RFC3339),
	})
}

// DeleteSession deletes a session
func (h *Handlers) DeleteSession(c fiber.Ctx) error {
	id := c.Params("id")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	ctx := c.Context()
	if err := h.sessionStore.DeleteSession(ctx, namespace, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete session: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// ListTools lists available tools
func (h *Handlers) ListTools(c fiber.Ctx) error {
	namespace := c.Query("namespace", "")
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	opts := &client.ListOptions{}

	if namespace != "" {
		opts.Namespace = namespace
	} else if h.watchNamespace != "" {
		opts.Namespace = h.watchNamespace
	}

	// Apply pagination
	pagination, err := ParsePagination(limit, continueToken)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	opts.Limit = pagination.Limit
	opts.Continue = pagination.Continue

	toolList := &corev1alpha1.ToolList{}
	ctx := c.Context()
	if err := h.client.List(ctx, toolList, opts); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list tools: %v", err))
	}

	// Add built-in tools to the response
	tools := make([]fiber.Map, 0, len(toolList.Items)+len(builtinToolsList))
	tools = append(tools, builtinToolsList...)

	for _, tool := range toolList.Items {
		tools = append(tools, fiber.Map{
			"name":        tool.Name,
			"namespace":   tool.Namespace,
			"builtin":     false,
			"description": tool.Spec.Description,
			"available":   tool.Status.Available,
			"url":         tool.Spec.HTTP.URL,
		})
	}

	response := ListResponse{
		Items: tools,
		Metadata: ListMeta{
			Continue:           toolList.Continue,
			RemainingItemCount: toolList.RemainingItemCount,
		},
	}

	return c.JSON(response)
}

// GetTool gets a tool by name
func (h *Handlers) GetTool(c fiber.Ctx) error {
	name := c.Params("name")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	// Check if it's a built-in tool
	if builtin, ok := builtinToolsMap[name]; ok {
		return c.JSON(builtin)
	}

	// Look up Tool CRD
	tool := &corev1alpha1.Tool{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, tool); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "tool not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get tool: %v", err))
	}

	return c.JSON(tool)
}

// ListAgents lists available agents
func (h *Handlers) ListAgents(c fiber.Ctx) error {
	namespace := c.Query("namespace", "")
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	opts := &client.ListOptions{}

	if namespace != "" {
		opts.Namespace = namespace
	} else if h.watchNamespace != "" {
		opts.Namespace = h.watchNamespace
	}

	// Apply pagination
	pagination, err := ParsePagination(limit, continueToken)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	opts.Limit = pagination.Limit
	opts.Continue = pagination.Continue

	agentList := &corev1alpha1.AgentList{}
	ctx := c.Context()
	if err := h.client.List(ctx, agentList, opts); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list agents: %v", err))
	}

	response := ListResponse{
		Items: agentList.Items,
		Metadata: ListMeta{
			Continue:           agentList.Continue,
			RemainingItemCount: agentList.RemainingItemCount,
		},
	}

	return c.JSON(response)
}

// GetAgent gets an agent by name
func (h *Handlers) GetAgent(c fiber.Ctx) error {
	name := c.Params("name")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	agent := &corev1alpha1.Agent{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "agent not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent: %v", err))
	}

	return c.JSON(agent)
}

// CreateAgent creates a new agent
func (h *Handlers) CreateAgent(c fiber.Ctx) error {
	var req CreateAgentRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	if req.Name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = "default"
	}

	if h.watchNamespace != "" && h.watchNamespace != namespace {
		return fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: req.Spec,
	}

	ctx := c.Context()
	if err := h.client.Create(ctx, agent); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "agent already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create agent: %v", err))
	}

	return c.Status(fiber.StatusCreated).JSON(agent)
}

// UpdateAgent updates an existing agent
func (h *Handlers) UpdateAgent(c fiber.Ctx) error {
	name := c.Params("name")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	agent := &corev1alpha1.Agent{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "agent not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent: %v", err))
	}

	var req UpdateAgentRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	agent.Spec = req.Spec
	if err := h.client.Update(ctx, agent); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update agent: %v", err))
	}

	return c.JSON(agent)
}

// DeleteAgent deletes an agent
func (h *Handlers) DeleteAgent(c fiber.Ctx) error {
	name := c.Params("name")
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	agent := &corev1alpha1.Agent{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "agent not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent: %v", err))
	}

	if err := h.client.Delete(ctx, agent); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete agent: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// SecretNameResponse is a minimal representation of a Secret for dropdown lists
type SecretNameResponse struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Type      string `json:"type"`
}

// ListSecretNames lists secret names in a namespace (metadata only, no data)
func (h *Handlers) ListSecretNames(c fiber.Ctx) error {
	namespace := c.Query("namespace", "default")

	if h.watchNamespace != "" {
		namespace = h.watchNamespace
	}

	secretList := &corev1.SecretList{}
	ctx := c.Context()
	opts := &client.ListOptions{Namespace: namespace}
	if err := h.client.List(ctx, secretList, opts); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list secrets: %v", err))
	}

	// Return only names and types, never secret data
	names := make([]SecretNameResponse, 0, len(secretList.Items))
	for _, s := range secretList.Items {
		if s.Type == corev1.SecretTypeServiceAccountToken || s.Type == "kubernetes.io/service-account-token" {
			continue
		}
		names = append(names, SecretNameResponse{
			Name:      s.Name,
			Namespace: s.Namespace,
			Type:      string(s.Type),
		})
	}

	return c.JSON(fiber.Map{"items": names})
}

// StreamPodLogs streams logs from a pod
func StreamPodLogs(ctx context.Context, clientset kubernetes.Interface, namespace, podName, containerName string) (io.ReadCloser, error) {
	opts := &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
	}

	req := clientset.CoreV1().Pods(namespace).GetLogs(podName, opts)
	return req.Stream(ctx)
}

// parseDuration parses a duration string
func parseDuration(s string) (*metav1.Duration, error) {
	// Handle common formats like "300s", "5m", "1h"
	duration, err := time.ParseDuration(s)
	if err != nil {
		return nil, err
	}
	return &metav1.Duration{Duration: duration}, nil
}

// GetTaskChildren returns child tasks for a given parent task
func (h *Handlers) GetTaskChildren(c fiber.Ctx) error {
	taskName := c.Params("id")
	namespace := c.Query("namespace", h.watchNamespace)
	if namespace == "" {
		namespace = "default"
	}

	var taskList corev1alpha1.TaskList
	if err := h.client.List(c.Context(), &taskList,
		client.InNamespace(namespace),
		client.MatchingLabels{"mercan.ai/parent-task": taskName},
	); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list child tasks: %v", err))
	}

	return c.JSON(ListResponse{
		Items:    taskList.Items,
		Metadata: ListMeta{},
	})
}

// Helper to read lines from a reader
func readLines(r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			ch <- scanner.Text()
		}
	}()
	return ch
}
