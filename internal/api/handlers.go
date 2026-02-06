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
)

// Handlers contains all API handlers
type Handlers struct {
	client         client.Client
	sessionManager *controller.SessionManager
	watchNamespace string
}

// NewHandlers creates a new Handlers instance
func NewHandlers(c client.Client, sessionManager *controller.SessionManager, watchNamespace string) *Handlers {
	return &Handlers{
		client:         c,
		sessionManager: sessionManager,
		watchNamespace: watchNamespace,
	}
}

// CreateTaskRequest is the request body for creating a task
type CreateTaskRequest struct {
	Name         string                         `json:"name"`
	Namespace    string                         `json:"namespace"`
	Type         corev1alpha1.TaskType          `json:"type"`
	Image        string                         `json:"image,omitempty"`
	Command      []string                       `json:"command,omitempty"`
	Args         []string                       `json:"args,omitempty"`
	Env          []corev1.EnvVar                `json:"env,omitempty"`
	Timeout      string                         `json:"timeout,omitempty"`
	Priority     *int32                         `json:"priority,omitempty"`
	RetryPolicy  *corev1alpha1.RetryPolicy      `json:"retryPolicy,omitempty"`
	WebhookURL   string                         `json:"webhookURL,omitempty"`
	SecretRef    *corev1alpha1.SecretReference  `json:"secretRef,omitempty"`
	SessionRef   *corev1alpha1.SessionReference `json:"sessionRef,omitempty"`
	AI           *corev1alpha1.AISpec           `json:"ai,omitempty"`
	AgentRef     *corev1alpha1.AgentReference   `json:"agentRef,omitempty"`
	Prompt       string                         `json:"prompt,omitempty"`
	AgentRuntime *corev1alpha1.AgentRuntimeSpec `json:"agentRuntime,omitempty"`
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

	if task.Status.JobName == "" {
		return fiber.NewError(fiber.StatusNotFound, "task has no associated job")
	}

	// Get pod logs
	// Note: This requires a kubernetes clientset, which we'd need to inject
	// For now, return a placeholder
	return c.JSON(fiber.Map{
		"message": "Log streaming requires kubernetes clientset",
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

	if task.Status.ResultRef == nil {
		return fiber.NewError(fiber.StatusNotFound, "task has no result")
	}

	// Get the result ConfigMap
	resultCM := &corev1.ConfigMap{}
	if err := h.client.Get(ctx, types.NamespacedName{
		Name:      task.Status.ResultRef.ConfigMapName,
		Namespace: namespace,
	}, resultCM); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "result not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get result: %v", err))
	}

	key := task.Status.ResultRef.Key
	if key == "" {
		key = "result"
	}

	result, ok := resultCM.Data[key]
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "result key not found in ConfigMap")
	}

	return c.JSON(fiber.Map{
		"result": result,
	})
}

// ListSessions lists sessions
func (h *Handlers) ListSessions(c fiber.Ctx) error {
	namespace := c.Query("namespace", "")
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	if namespace == "" && h.watchNamespace != "" {
		namespace = h.watchNamespace
	}
	if namespace == "" {
		namespace = "default"
	}

	opts := &client.ListOptions{
		Namespace: namespace,
	}

	// Apply pagination
	pagination, err := ParsePagination(limit, continueToken)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	opts.Limit = pagination.Limit
	opts.Continue = pagination.Continue

	// List session ConfigMaps
	cmList := &corev1.ConfigMapList{}
	ctx := c.Context()
	if err := h.client.List(ctx, cmList, opts, client.MatchingLabels{
		"mercan.ai/session": "true",
	}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list sessions: %v", err))
	}

	// Convert to session response
	sessions := make([]fiber.Map, 0, len(cmList.Items))
	for _, cm := range cmList.Items {
		sessionName := strings.TrimPrefix(cm.Name, "session-")
		sessions = append(sessions, fiber.Map{
			"name":         sessionName,
			"namespace":    cm.Namespace,
			"messageCount": cm.Annotations["mercan.ai/message-count"],
			"inputTokens":  cm.Annotations["mercan.ai/input-tokens"],
			"outputTokens": cm.Annotations["mercan.ai/output-tokens"],
			"activeTask":   cm.Annotations["mercan.ai/active-task"],
			"createdAt":    cm.Annotations["mercan.ai/created-at"],
			"updatedAt":    cm.Annotations["mercan.ai/updated-at"],
		})
	}

	response := ListResponse{
		Items: sessions,
		Metadata: ListMeta{
			Continue:           cmList.Continue,
			RemainingItemCount: cmList.RemainingItemCount,
		},
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
	cm, err := h.sessionManager.GetSession(ctx, namespace, id)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get session: %v", err))
	}

	return c.JSON(fiber.Map{
		"name":         id,
		"namespace":    cm.Namespace,
		"transcript":   cm.Data["transcript.jsonl"],
		"messageCount": cm.Annotations["mercan.ai/message-count"],
		"inputTokens":  cm.Annotations["mercan.ai/input-tokens"],
		"outputTokens": cm.Annotations["mercan.ai/output-tokens"],
		"activeTask":   cm.Annotations["mercan.ai/active-task"],
		"createdAt":    cm.Annotations["mercan.ai/created-at"],
		"updatedAt":    cm.Annotations["mercan.ai/updated-at"],
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
	if err := h.sessionManager.DeleteSession(ctx, namespace, id); err != nil {
		if apierrors.IsNotFound(err) {
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
	builtinTools := []fiber.Map{
		{"name": "web_search", "builtin": true, "description": "Search the web"},
		{"name": "code_exec", "builtin": true, "description": "Execute code in sandbox"},
		{"name": "file_read", "builtin": true, "description": "Read files from workspace"},
	}

	tools := make([]fiber.Map, 0, len(toolList.Items)+len(builtinTools))
	tools = append(tools, builtinTools...)

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
	builtinTools := map[string]fiber.Map{
		"web_search": {"name": "web_search", "builtin": true, "description": "Search the web"},
		"code_exec":  {"name": "code_exec", "builtin": true, "description": "Execute code in sandbox"},
		"file_read":  {"name": "file_read", "builtin": true, "description": "Read files from workspace"},
	}

	if builtin, ok := builtinTools[name]; ok {
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
