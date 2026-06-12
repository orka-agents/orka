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
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tools"
)

const queryTrue = "true"

// Handlers contains all API handlers
//
// builtinToolsList defines the built-in tools returned by list/get endpoints.
var builtinToolsList = []fiber.Map{
	builtinToolResponse(tools.NewWebSearchTool()),
	builtinToolResponse(tools.NewCodeExecTool()),
	builtinToolResponse(tools.NewFileReadTool()),
	builtinToolResponse(tools.NewWebFetchTool()),
	builtinToolResponse(tools.NewFileWriteTool()),
}

// builtinToolsMap indexes built-in tools by name for single-tool lookup.
var builtinToolsMap = func() map[string]fiber.Map {
	m := make(map[string]fiber.Map, len(builtinToolsList))
	for _, t := range builtinToolsList {
		m[t["name"].(string)] = t
	}
	return m
}()

func builtinToolResponse(tool tools.Tool) fiber.Map {
	var parameters any = fiber.Map{}
	if raw := tool.Parameters(); len(raw) > 0 {
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			parameters = parsed
		}
	}

	return fiber.Map{
		"name":        tool.Name(),
		"builtin":     true,
		"description": tool.Description(),
		"parameters":  parameters,
	}
}

func toolSpecHTTPURL(tool *corev1alpha1.Tool) string {
	if tool == nil || tool.Spec.HTTP == nil {
		return ""
	}
	return tool.Spec.HTTP.URL
}

type Handlers struct {
	client                    client.Client
	clientset                 kubernetes.Interface
	watchNamespace            string
	enforceNamespaceIsolation bool
	contextTokenAuthorization ContextTokenAuthorizationConfig
	resultStore               store.ResultStore
	sessionStore              store.SessionStore
	planStore                 store.PlanStore
	healthChecker             store.HealthChecker
	artifactStore             store.ArtifactStore
	memoryStore               store.MemoryStore
	memoryProposalStore       store.MemoryProposalStore
	securityStore             store.SecurityStore
	repositoryMonitorStore    store.RepositoryMonitorStore
	executionEventStore       store.ExecutionEventStore
	approvalDecisionMu        sync.Mutex
	eventStreamPollInterval   time.Duration
	eventStreamHeartbeatEvery time.Duration
}

// HandlersConfig holds configuration for creating Handlers.
type HandlersConfig struct {
	Client                    client.Client
	WatchNamespace            string
	EnforceNamespaceIsolation bool
	ContextTokenAuthorization ContextTokenAuthorizationConfig
	ResultStore               store.ResultStore
	SessionStore              store.SessionStore
	PlanStore                 store.PlanStore
	KubeClient                kubernetes.Interface
	HealthChecker             store.HealthChecker
	ArtifactStore             store.ArtifactStore
	MemoryStore               store.MemoryStore
	MemoryProposalStore       store.MemoryProposalStore
	SecurityStore             store.SecurityStore
	RepositoryMonitorStore    store.RepositoryMonitorStore
	ExecutionEventStore       store.ExecutionEventStore
}

// NewHandlers creates a new Handlers instance
func NewHandlers(cfg HandlersConfig) *Handlers {
	return &Handlers{
		client:                    cfg.Client,
		clientset:                 cfg.KubeClient,
		watchNamespace:            cfg.WatchNamespace,
		enforceNamespaceIsolation: cfg.EnforceNamespaceIsolation,
		contextTokenAuthorization: cfg.ContextTokenAuthorization,
		resultStore:               cfg.ResultStore,
		sessionStore:              cfg.SessionStore,
		planStore:                 cfg.PlanStore,
		healthChecker:             cfg.HealthChecker,
		artifactStore:             cfg.ArtifactStore,
		memoryStore:               cfg.MemoryStore,
		memoryProposalStore:       cfg.MemoryProposalStore,
		securityStore:             cfg.SecurityStore,
		repositoryMonitorStore:    cfg.RepositoryMonitorStore,
		executionEventStore:       cfg.ExecutionEventStore,
		eventStreamPollInterval:   defaultEventStreamPollInterval,
		eventStreamHeartbeatEvery: defaultEventStreamHeartbeatEvery,
	}
}

// MetadataRequest holds Kubernetes-style metadata fields
type MetadataRequest struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func objectMetaFromRequest(name, namespace string, metadata MetadataRequest) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   namespace,
		Labels:      metadata.Labels,
		Annotations: metadata.Annotations,
	}
}

// resolveNamespace resolves the effective namespace for a request and enforces isolation if enabled.
// When watchNamespace is set: it's the only allowed namespace (explicit mismatches are rejected).
// Otherwise: explicit param > SA namespace from token > "default"
func (h *Handlers) resolveNamespace(c fiber.Ctx, explicit string) (string, error) {
	var ns string
	if h.watchNamespace != "" {
		if explicit != "" && explicit != h.watchNamespace {
			log.Info("namespace access denied: watchNamespace mismatch",
				"requestedNamespace", explicit,
				"allowedNamespace", h.watchNamespace,
				"ip", c.IP(),
			)
			return "", fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
		}
		ns = h.watchNamespace
	} else if explicit != "" {
		ns = explicit
	} else {
		ns = GetEffectiveNamespace(c, "")
	}

	// Enforce namespace isolation: user can only access their SA namespace
	if h.enforceNamespaceIsolation {
		ui := GetUserInfo(c)
		if ui != nil && ui.Namespace != "" && ns != ui.Namespace {
			log.Info("namespace access denied: isolation violation",
				"username", ui.Username,
				"userNamespace", ui.Namespace,
				"requestedNamespace", ns,
				"ip", c.IP(),
			)
			return "", fiber.NewError(fiber.StatusForbidden,
				fmt.Sprintf("namespace %q not allowed, restricted to %q", ns, ui.Namespace))
		}
	}

	c.Locals(resolvedNamespaceLocalKey, ns)
	return ns, nil
}

// CreateAgentRequest is the request body for creating an agent
type CreateAgentRequest struct {
	Name      string                 `json:"name"`
	Namespace string                 `json:"namespace"`
	Metadata  MetadataRequest        `json:"metadata"`
	Spec      corev1alpha1.AgentSpec `json:"spec"`
}

// UpdateAgentRequest is the request body for updating an agent
type UpdateAgentRequest struct {
	Spec corev1alpha1.AgentSpec `json:"spec"`
}

// CreateTaskRequest is the request body for creating a task
type CreateTaskRequest struct {
	Name              string                           `json:"name"`
	Namespace         string                           `json:"namespace"`
	Metadata          MetadataRequest                  `json:"metadata"`
	Spec              *corev1alpha1.TaskSpec           `json:"spec,omitempty"`
	Annotations       map[string]string                `json:"annotations,omitempty"`
	Type              corev1alpha1.TaskType            `json:"type"`
	Image             string                           `json:"image,omitempty"`
	Command           []string                         `json:"command,omitempty"`
	Args              []string                         `json:"args,omitempty"`
	Env               []corev1.EnvVar                  `json:"env,omitempty"`
	Timeout           string                           `json:"timeout,omitempty"`
	Priority          *int32                           `json:"priority,omitempty"`
	RetryPolicy       *corev1alpha1.RetryPolicy        `json:"retryPolicy,omitempty"`
	WebhookURL        string                           `json:"webhookURL,omitempty"`
	SecretRef         *corev1alpha1.SecretReference    `json:"secretRef,omitempty"`
	SessionRef        *corev1alpha1.SessionReference   `json:"sessionRef,omitempty"`
	AI                *corev1alpha1.AISpec             `json:"ai,omitempty"`
	AgentRef          *corev1alpha1.AgentReference     `json:"agentRef,omitempty"`
	Prompt            string                           `json:"prompt,omitempty"`
	AgentRuntime      *corev1alpha1.AgentRuntimeSpec   `json:"agentRuntime,omitempty"`
	Execution         *corev1alpha1.ExecutionSpec      `json:"execution,omitempty"`
	Workspace         *corev1alpha1.WorkspaceConfig    `json:"workspace,omitempty"`
	PriorTaskRef      *corev1alpha1.PriorTaskReference `json:"priorTaskRef,omitempty"`
	Schedule          string                           `json:"schedule,omitempty"`
	TimeZone          *string                          `json:"timeZone,omitempty"`
	ConcurrencyPolicy string                           `json:"concurrencyPolicy,omitempty"`
	Suspend           *bool                            `json:"suspend,omitempty"`
}

func applyFlatTaskRequest(spec *corev1alpha1.TaskSpec, req CreateTaskRequest) {
	if req.Type != "" {
		spec.Type = req.Type
	}
	if req.Image != "" {
		spec.Image = req.Image
	}
	if req.Command != nil {
		spec.Command = req.Command
	}
	if req.Args != nil {
		spec.Args = req.Args
	}
	if req.Env != nil {
		spec.Env = req.Env
	}
	if req.Priority != nil {
		spec.Priority = req.Priority
	}
	if req.RetryPolicy != nil {
		spec.RetryPolicy = req.RetryPolicy
	}
	if req.WebhookURL != "" {
		spec.WebhookURL = req.WebhookURL
	}
	if req.SecretRef != nil {
		spec.SecretRef = req.SecretRef
	}
	if req.SessionRef != nil {
		spec.SessionRef = req.SessionRef
	}
	if req.AI != nil {
		spec.AI = req.AI
	}
	if req.AgentRef != nil {
		spec.AgentRef = req.AgentRef
	}
	if req.Prompt != "" {
		spec.Prompt = req.Prompt
	}
	if req.AgentRuntime != nil {
		spec.AgentRuntime = req.AgentRuntime
	}
	if req.Execution != nil {
		spec.Execution = req.Execution.DeepCopy()
	}
	if req.Workspace != nil {
		spec.Workspace = req.Workspace
	}
	if req.PriorTaskRef != nil {
		spec.PriorTaskRef = req.PriorTaskRef
	}
	if req.Schedule != "" {
		spec.Schedule = req.Schedule
	}
	if req.TimeZone != nil {
		spec.TimeZone = req.TimeZone
	}
	if req.ConcurrencyPolicy != "" {
		spec.ConcurrencyPolicy = corev1alpha1.ConcurrencyPolicy(req.ConcurrencyPolicy)
	}
	if req.Suspend != nil {
		spec.Suspend = req.Suspend
	}
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
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Second)
	defer cancel()

	checks := fiber.Map{}

	// Verify database connectivity
	if h.healthChecker != nil {
		if err := h.healthChecker.HealthCheck(ctx); err != nil {
			checks["store"] = "unhealthy"
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"status": "not ready",
				"checks": checks,
			})
		}
		checks["store"] = "ok"
	}

	// Verify Kubernetes API connectivity
	if h.client != nil {
		var ns corev1.NamespaceList
		if err := h.client.List(ctx, &ns, client.Limit(1)); err != nil {
			checks["kubernetes"] = "unhealthy"
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"status": "not ready",
				"checks": checks,
			})
		}
		checks["kubernetes"] = "ok"
	}

	return c.JSON(fiber.Map{
		"status": "ok",
		"checks": checks,
	})
}

func rejectRequestedByTampering(body []byte) error {
	if len(body) == 0 {
		return nil
	}

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(body, &topLevel); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	for key := range topLevel {
		switch strings.ToLower(key) {
		case "requestedby":
			return fiber.NewError(fiber.StatusBadRequest, "requestedBy cannot be set by clients")
		case "transaction":
			return fiber.NewError(fiber.StatusBadRequest, "transaction cannot be set by clients")
		}
	}

	for topKey, specRaw := range topLevel {
		if strings.ToLower(topKey) != "spec" {
			continue
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(specRaw, &spec); err == nil {
			for key := range spec {
				switch strings.ToLower(key) {
				case "requestedby":
					return fiber.NewError(fiber.StatusBadRequest, "spec.requestedBy cannot be set by clients")
				case "transaction":
					return fiber.NewError(fiber.StatusBadRequest, "spec.transaction cannot be set by clients")
				}
			}
		}
	}

	return nil
}

func rejectReservedTaskAnnotations(annotations map[string]string) error {
	for key := range annotations {
		if strings.HasPrefix(key, "orka.ai/") {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("annotation %q is reserved", key))
		}
	}
	return nil
}

// CreateTask creates a new task
func (h *Handlers) CreateTask(c fiber.Ctx) error {
	if err := rejectRequestedByTampering(c.Body()); err != nil {
		return err
	}

	var req CreateTaskRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	name := req.Name
	if name == "" {
		name = req.Metadata.Name
	}
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}

	annotations := req.Annotations
	if annotations == nil {
		annotations = req.Metadata.Annotations
	}
	if err := rejectReservedTaskAnnotations(annotations); err != nil {
		return err
	}

	explicitNamespace := req.Namespace
	if explicitNamespace == "" {
		explicitNamespace = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNamespace)
	if err != nil {
		return err
	}

	spec := corev1alpha1.TaskSpec{}
	if req.Spec != nil {
		spec = *req.Spec.DeepCopy()
	}
	applyFlatTaskRequest(&spec, req)
	// Server-owned audit fields are always stamped after authorization.
	spec.RequestedBy = nil
	spec.Transaction = nil
	if spec.Type == "" {
		return fiber.NewError(fiber.StatusBadRequest, "type is required")
	}
	objectMeta := objectMetaFromRequest(name, namespace, req.Metadata)
	objectMeta.Annotations = annotations
	task := &corev1alpha1.Task{
		ObjectMeta: objectMeta,
		Spec:       spec,
	}
	authReq := createTaskRequestFromTask(task)
	if req.Timeout != "" {
		authReq.Timeout = req.Timeout
	}
	if err := h.authorizeContextTokenTaskCreate(c, authReq, namespace); err != nil {
		return err
	}

	stampTaskRequesterFromUserInfo(task, GetUserInfo(c))

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
	explicitNS := c.Query("namespace", "")
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	opts := &client.ListOptions{}

	// Apply namespace filter with smart defaults
	namespace, err := h.resolveNamespace(c, explicitNS)
	if err != nil {
		return err
	}
	opts.Namespace = namespace
	if err := h.authorizeContextTokenAction(c, "listTasks", h.contextTokenAuthorization.TaskListScopes); err != nil {
		return err
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
	filteredList := false
	if h.contextTokenAuthorization.Enabled() {
		filtered := taskList.Items[:0]
		for i := range taskList.Items {
			allowed, err := h.contextTokenAllowsLoadedTask(c, "listTasks", &taskList.Items[i])
			if err != nil {
				return err
			}
			if allowed {
				filtered = append(filtered, taskList.Items[i])
			}
		}
		filteredList = len(filtered) != len(taskList.Items)
		taskList.Items = filtered
	}
	remainingItemCount := taskList.RemainingItemCount
	if filteredList {
		remainingItemCount = nil
	}

	response := ListResponse{
		Items: taskList.Items,
		Metadata: ListMeta{
			Continue:           taskList.Continue,
			RemainingItemCount: remainingItemCount,
		},
	}

	return c.JSON(response)
}

// GetTask gets a task by ID
func (h *Handlers) GetTask(c fiber.Ctx) error {
	id := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "getTask", namespace, id); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "getTask", task); err != nil {
		return err
	}

	// Build consistent response shape with optional plan data
	type planResponse struct {
		Summary      string `json:"summary"`
		ProgressPct  int    `json:"progressPct"`
		GoalComplete bool   `json:"goalComplete"`
		PlanDocument string `json:"planDocument,omitempty"`
		Iteration    int    `json:"iteration"`
	}
	type taskResponse struct {
		corev1alpha1.Task `json:",inline"`
		Plan              *planResponse `json:"plan,omitempty"`
	}

	resp := taskResponse{Task: *task}
	if h.planStore != nil && task.Status.Iteration > 0 {
		if plan, planErr := h.planStore.GetPlan(ctx, task.Namespace, task.Name); planErr == nil {
			resp.Plan = &planResponse{
				Summary:      plan.Summary,
				ProgressPct:  plan.ProgressPct,
				GoalComplete: plan.GoalComplete,
				PlanDocument: plan.PlanDocument,
				Iteration:    plan.Iteration,
			}
		}
	}

	return c.JSON(resp)
}

// DeleteTask deletes a task
func (h *Handlers) DeleteTask(c fiber.Ctx) error {
	id := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "deleteTask", h.contextTokenAuthorization.TaskDeleteScopes); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "deleteTask", task); err != nil {
		return err
	}

	if err := h.client.Delete(ctx, task); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete task: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetTaskLogs gets logs for a task
func (h *Handlers) GetTaskLogs(c fiber.Ctx) error {
	id := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "getTaskLogs", namespace, id); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "getTaskLogs", task); err != nil {
		return err
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

	// For running tasks, stream logs from the pod if clientset is available
	if h.clientset == nil {
		return c.JSON(fiber.Map{
			"message": "live log streaming not available",
			"jobName": task.Status.JobName,
		})
	}

	// Find the pod for this job
	pods, err := h.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", task.Status.JobName),
	})
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list pods: %v", err))
	}
	if len(pods.Items) == 0 {
		return fiber.NewError(fiber.StatusNotFound, "no pods found for task job")
	}

	podName := pods.Items[0].Name
	follow := c.Query("follow") == queryTrue

	if follow {
		streamCtx, streamCancel := context.WithCancel(context.Background())
		stream, err := StreamPodLogs(streamCtx, h.clientset, namespace, podName, "worker")
		if err != nil {
			streamCancel()
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to stream logs: %v", err))
		}

		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")

		return c.SendStreamWriter(func(w *bufio.Writer) {
			defer streamCancel()
			defer func() { _ = stream.Close() }()
			scanner := bufio.NewScanner(stream)
			for scanner.Scan() {
				_, _ = fmt.Fprintf(w, "data: %s\n\n", scanner.Text())
				if err := w.Flush(); err != nil {
					return
				}
			}
		})
	}

	// Non-follow mode: return the last N lines
	var tailLines int64 = 100
	opts := &corev1.PodLogOptions{
		Container: "worker",
		TailLines: &tailLines,
	}
	req := h.clientset.CoreV1().Pods(namespace).GetLogs(podName, opts)
	logStream, err := req.Stream(ctx)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get logs: %v", err))
	}
	defer func() { _ = logStream.Close() }()

	logBytes, err := io.ReadAll(logStream)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to read logs: %v", err))
	}

	return c.JSON(fiber.Map{
		"logs": string(logBytes),
	})
}

// GetTaskResult gets the result of a task
func (h *Handlers) GetTaskResult(c fiber.Ctx) error {
	id := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "getTaskResult", namespace, id); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "getTaskResult", task); err != nil {
		return err
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

// GetTaskPlan gets the autonomous plan state for a task
func (h *Handlers) GetTaskPlan(c fiber.Ctx) error {
	id := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "getTaskPlan", namespace, id); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "getTaskPlan", task); err != nil {
		return err
	}

	if h.planStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "plan store not configured")
	}

	plan, planErr := h.planStore.GetPlan(ctx, task.Namespace, task.Name)
	if planErr != nil {
		if errors.Is(planErr, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "no plan found for this task")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get plan: %v", planErr))
	}

	return c.JSON(plan)
}

// ListSessions lists sessions
func (h *Handlers) ListSessions(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSessions", h.contextTokenAuthorization.SessionReadScopes); err != nil {
		return err
	}

	ctx := c.Context()
	sessions, err := h.sessionStore.ListSessions(ctx, namespace)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list sessions: %v", err))
	}

	items := make([]fiber.Map, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, fiber.Map{
			"id":           s.Name,
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getSession", h.contextTokenAuthorization.SessionReadScopes); err != nil {
		return err
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "deleteSession", h.contextTokenAuthorization.SessionWriteScopes); err != nil {
		return err
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listTools", h.contextTokenAuthorization.ToolReadScopes); err != nil {
		return err
	}
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	opts := &client.ListOptions{Namespace: namespace}

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
	toolItems := make([]fiber.Map, 0, len(toolList.Items)+len(builtinToolsList))
	for _, tool := range builtinToolsList {
		name, _ := tool["name"].(string)
		allowed, err := contextTokenAllowsToolMetadata(c, h.contextTokenAuthorization, "listTools", name)
		if err != nil {
			return err
		}
		if allowed {
			toolItems = append(toolItems, tool)
		}
	}

	for _, tool := range toolList.Items {
		allowed, err := contextTokenAllowsToolMetadata(c, h.contextTokenAuthorization, "listTools", tool.Name)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		toolItems = append(toolItems, fiber.Map{
			"name":        tool.Name,
			"namespace":   tool.Namespace,
			"builtin":     false,
			"description": tool.Spec.Description,
			"available":   tool.Status.Available,
			"url":         toolSpecHTTPURL(&tool),
		})
	}

	response := ListResponse{
		Items: toolItems,
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getTool", h.contextTokenAuthorization.ToolReadScopes); err != nil {
		return err
	}

	// Check if it's a built-in tool
	if builtin, ok := builtinToolsMap[name]; ok {
		if err := authorizeContextTokenToolMetadata(c, h.contextTokenAuthorization, "getTool", name); err != nil {
			return err
		}
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
	if err := authorizeContextTokenToolMetadata(c, h.contextTokenAuthorization, "getTool", tool.Name); err != nil {
		return err
	}

	return c.JSON(tool)
}

// ListAgents lists available agents
func (h *Handlers) ListAgents(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listAgents", h.contextTokenAuthorization.AgentReadScopes); err != nil {
		return err
	}
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	opts := &client.ListOptions{Namespace: namespace}

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
	if h.contextTokenAuthorization.Enabled() {
		filtered := agentList.Items[:0]
		for i := range agentList.Items {
			agent := &agentList.Items[i]
			allowed, err := contextTokenAllowsAgentContext(c, h.contextTokenAuthorization, "listAgents", agent.Namespace, agent.Name)
			if err != nil {
				return err
			}
			if allowed {
				filtered = append(filtered, *agent)
			}
		}
		agentList.Items = filtered
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getAgent", h.contextTokenAuthorization.AgentReadScopes); err != nil {
		return err
	}

	agent := &corev1alpha1.Agent{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "agent not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent: %v", err))
	}
	if err := authorizeContextTokenAgentContext(c, h.contextTokenAuthorization, "getAgent", agent.Namespace, agent.Name); err != nil {
		return err
	}

	return c.JSON(agent)
}

// CreateAgent creates a new agent
func (h *Handlers) CreateAgent(c fiber.Ctx) error {
	var req CreateAgentRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	// Support both flat format {"name":"x"} and Kubernetes-style {"metadata":{"name":"x"}}
	name := req.Name
	if name == "" {
		name = req.Metadata.Name
	}
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}

	// Resolve namespace from request or token
	explicitNS := req.Namespace
	if explicitNS == "" {
		explicitNS = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNS)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createAgent", h.contextTokenAuthorization.AgentWriteScopes); err != nil {
		return err
	}

	agent := &corev1alpha1.Agent{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata),
		Spec:       req.Spec,
	}
	if err := authorizeContextTokenAgentContext(c, h.contextTokenAuthorization, "createAgent", agent.Namespace, agent.Name); err != nil {
		return err
	}
	if err := authorizeContextTokenAgentSpec(c.Context(), h.client, contextTokenFromUserInfo(GetUserInfo(c)), h.contextTokenAuthorization, "createAgent", agent); err != nil {
		return err
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "updateAgent", h.contextTokenAuthorization.AgentWriteScopes); err != nil {
		return err
	}

	// Namespace isolation is resolved before parsing the body so unauthorized
	// namespace probes cannot learn whether the payload is syntactically valid.
	var req UpdateAgentRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	ctx := c.Context()
	token := contextTokenFromUserInfo(GetUserInfo(c))

	var agent *corev1alpha1.Agent
	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Agent{}
		if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, current); err != nil {
			return err
		}
		if err := authorizeContextTokenAgentContext(c, h.contextTokenAuthorization, "updateAgent", current.Namespace, current.Name); err != nil {
			return err
		}

		patchBase := current.DeepCopy()
		current.Spec = req.Spec
		if err := authorizeContextTokenAgentSpec(ctx, h.client, token, h.contextTokenAuthorization, "updateAgent", current); err != nil {
			return err
		}
		if err := h.client.Patch(ctx, current, client.MergeFromWithOptions(patchBase, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		agent = current
		return nil
	})
	if err != nil {
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) {
			return err
		}
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "agent not found")
		}
		if apierrors.IsConflict(err) {
			return fiber.NewError(fiber.StatusConflict, "agent was modified concurrently")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update agent: %v", err))
	}

	return c.JSON(agent)
}

// DeleteAgent deletes an agent
func (h *Handlers) DeleteAgent(c fiber.Ctx) error {
	name := c.Params("name")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "deleteAgent", h.contextTokenAuthorization.AgentWriteScopes); err != nil {
		return err
	}

	agent := &corev1alpha1.Agent{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "agent not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get agent: %v", err))
	}
	if err := authorizeContextTokenAgentContext(c, h.contextTokenAuthorization, "deleteAgent", agent.Namespace, agent.Name); err != nil {
		return err
	}

	if err := h.client.Delete(ctx, agent); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete agent: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// CreateSkillRequest is the request body for creating a skill
type CreateSkillRequest struct {
	Name      string                 `json:"name"`
	Namespace string                 `json:"namespace"`
	Metadata  MetadataRequest        `json:"metadata"`
	Spec      corev1alpha1.SkillSpec `json:"spec"`
}

// UpdateSkillRequest is the request body for updating a skill
type UpdateSkillRequest struct {
	Spec corev1alpha1.SkillSpec `json:"spec"`
}

// ListSkills lists available skills
func (h *Handlers) ListSkills(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSkills", h.contextTokenAuthorization.SkillReadScopes); err != nil {
		return err
	}
	limit := c.Query("limit", "100")
	continueToken := c.Query("continue", "")

	opts := &client.ListOptions{Namespace: namespace}

	pagination, err := ParsePagination(limit, continueToken)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	opts.Limit = pagination.Limit
	opts.Continue = pagination.Continue

	skillList := &corev1alpha1.SkillList{}
	ctx := c.Context()
	if err := h.client.List(ctx, skillList, opts); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list skills: %v", err))
	}

	skills := make([]fiber.Map, 0, len(skillList.Items))
	for _, skill := range skillList.Items {
		skills = append(skills, fiber.Map{
			"name":        skill.Name,
			"namespace":   skill.Namespace,
			"displayName": skill.Spec.DisplayName,
			"description": skill.Spec.Description,
			"version":     skill.Spec.Version,
			"author":      skill.Spec.Author,
			"tags":        skill.Spec.Tags,
			"phase":       skill.Status.Phase,
		})
	}

	response := ListResponse{
		Items: skills,
		Metadata: ListMeta{
			Continue:           skillList.Continue,
			RemainingItemCount: skillList.RemainingItemCount,
		},
	}

	return c.JSON(response)
}

// GetSkill gets a skill by name
func (h *Handlers) GetSkill(c fiber.Ctx) error {
	name := c.Params("name")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getSkill", h.contextTokenAuthorization.SkillReadScopes); err != nil {
		return err
	}

	skill := &corev1alpha1.Skill{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, skill); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "skill not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get skill: %v", err))
	}

	return c.JSON(skill)
}

// GetSkillContent gets the raw content of a skill
func (h *Handlers) GetSkillContent(c fiber.Ctx) error {
	name := c.Params("name")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getSkillContent", h.contextTokenAuthorization.SkillReadScopes); err != nil {
		return err
	}

	skill := &corev1alpha1.Skill{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, skill); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "skill not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get skill: %v", err))
	}

	c.Set("Content-Type", "text/markdown")
	return c.SendString(skill.Spec.Content.Inline)
}

// CreateSkill creates a new skill
func (h *Handlers) CreateSkill(c fiber.Ctx) error {
	var req CreateSkillRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	name := req.Name
	if name == "" {
		name = req.Metadata.Name
	}
	if name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "name is required")
	}

	explicitNS := req.Namespace
	if explicitNS == "" {
		explicitNS = req.Metadata.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNS)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createSkill", h.contextTokenAuthorization.SkillWriteScopes); err != nil {
		return err
	}

	skill := &corev1alpha1.Skill{
		ObjectMeta: objectMetaFromRequest(name, namespace, req.Metadata),
		Spec:       req.Spec,
	}

	ctx := c.Context()
	if err := h.client.Create(ctx, skill); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "skill already exists")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to create skill: %v", err))
	}

	return c.Status(fiber.StatusCreated).JSON(skill)
}

// UpdateSkill updates an existing skill
func (h *Handlers) UpdateSkill(c fiber.Ctx) error {
	name := c.Params("name")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "updateSkill", h.contextTokenAuthorization.SkillWriteScopes); err != nil {
		return err
	}

	skill := &corev1alpha1.Skill{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, skill); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "skill not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get skill: %v", err))
	}

	var req UpdateSkillRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}

	skill.Spec = req.Spec
	if err := h.client.Update(ctx, skill); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to update skill: %v", err))
	}

	return c.JSON(skill)
}

// DeleteSkill deletes a skill
func (h *Handlers) DeleteSkill(c fiber.Ctx) error {
	name := c.Params("name")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "deleteSkill", h.contextTokenAuthorization.SkillWriteScopes); err != nil {
		return err
	}

	skill := &corev1alpha1.Skill{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, skill); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "skill not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get skill: %v", err))
	}

	if err := h.client.Delete(ctx, skill); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to delete skill: %v", err))
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listSecrets", h.contextTokenAuthorization.SecretReadScopes()); err != nil {
		return err
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
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "getTaskChildren", namespace, taskName); err != nil {
		return err
	}

	ctx := c.Context()
	if h.contextTokenAuthorization.Enabled() {
		ui := GetUserInfo(c)
		if ui != nil && ui.AuthType == AuthTypeContextToken && ui.ContextToken != nil {
			parentTask := &corev1alpha1.Task{}
			if err := h.client.Get(ctx, types.NamespacedName{Name: taskName, Namespace: namespace}, parentTask); err != nil {
				if apierrors.IsNotFound(err) {
					return fiber.NewError(fiber.StatusNotFound, "task not found")
				}
				return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
			}
			if err := h.authorizeContextTokenLoadedTask(c, "getTaskChildren", parentTask); err != nil {
				return err
			}
		}
	}

	var taskList corev1alpha1.TaskList
	if err := h.client.List(ctx, &taskList,
		client.InNamespace(namespace),
		client.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(taskName)},
	); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list child tasks: %v", err))
	}
	if h.contextTokenAuthorization.Enabled() {
		filtered := taskList.Items[:0]
		for i := range taskList.Items {
			allowed, err := h.contextTokenAllowsLoadedTaskWithIdentity(c, "getTaskChildren", &taskList.Items[i], false)
			if err != nil {
				return err
			}
			if allowed {
				filtered = append(filtered, taskList.Items[i])
			}
		}
		taskList.Items = filtered
	}

	return c.JSON(ListResponse{
		Items:    taskList.Items,
		Metadata: ListMeta{},
	})
}

// ListTaskArtifacts lists artifacts for a task
func (h *Handlers) ListTaskArtifacts(c fiber.Ctx) error {
	id := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "listTaskArtifacts", namespace, id); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "listTaskArtifacts", task); err != nil {
		return err
	}

	if h.artifactStore == nil {
		return c.JSON(fiber.Map{"artifacts": []any{}})
	}

	artifacts, err := h.artifactStore.ListArtifacts(ctx, namespace, id)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list artifacts: %v", err))
	}

	if artifacts == nil {
		artifacts = []store.ArtifactMetadata{}
	}

	return c.JSON(fiber.Map{"artifacts": artifacts})
}

// DownloadTaskArtifact downloads a specific artifact file
func (h *Handlers) DownloadTaskArtifact(c fiber.Ctx) error {
	id := c.Params("id")
	filename := c.Params("filename")
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenTaskRead(c, "downloadTaskArtifact", namespace, id); err != nil {
		return err
	}

	task := &corev1alpha1.Task{}
	ctx := c.Context()
	if err := h.client.Get(ctx, types.NamespacedName{Name: id, Namespace: namespace}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
	}
	if err := h.authorizeContextTokenLoadedTask(c, "downloadTaskArtifact", task); err != nil {
		return err
	}

	if h.artifactStore == nil {
		return fiber.NewError(fiber.StatusNotFound, "artifact store not configured")
	}

	data, contentType, err := h.artifactStore.GetArtifact(ctx, namespace, id, filename)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "artifact not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get artifact: %v", err))
	}

	// Sanitize filename for Content-Disposition header
	safeFilename := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\r' || r == '\n' {
			return '_'
		}
		return r
	}, filename)
	c.Set("Content-Type", contentType)
	c.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, safeFilename))
	return c.Send(data)
}

// handleAuthValidate returns success if the request passes auth middleware
func (s *Server) handleAuthValidate(c fiber.Ctx) error {
	return c.JSON(fiber.Map{"authenticated": true})
}
