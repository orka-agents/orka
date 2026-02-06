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
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
	"github.com/sozercan/mercan/internal/controller"
	"github.com/sozercan/mercan/internal/llm"
)

// ToolExecutor executes orchestrator LLM tool calls by creating and managing
// Kubernetes resources (Tasks, Agents, Tools, Sessions).
type ToolExecutor struct {
	client         client.Client
	sessionManager *controller.SessionManager
	namespace      string
	sessionID      string
	taskSeq        atomic.Int32
	tasksCreated   int
	maxTasks       int
	toolTimeout    time.Duration
	watchNamespace string
}

// NewToolExecutor creates a new ToolExecutor.
func NewToolExecutor(c client.Client, sm *controller.SessionManager, namespace, sessionID, watchNamespace string, maxTasks int, toolTimeout time.Duration) *ToolExecutor {
	return &ToolExecutor{
		client:         c,
		sessionManager: sm,
		namespace:      namespace,
		sessionID:      sessionID,
		maxTasks:       maxTasks,
		toolTimeout:    toolTimeout,
		watchNamespace: watchNamespace,
	}
}

// ToolResult represents the result of a tool execution.
type ToolResult struct {
	Success    bool   `json:"success"`
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	ErrorType  string `json:"errorType,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// Execute dispatches a tool call to the appropriate handler and returns
// the JSON-serialized result.
func (e *ToolExecutor) Execute(ctx context.Context, toolCall llm.ToolCall) (string, error) {
	var args map[string]any
	if err := json.Unmarshal(toolCall.Arguments, &args); err != nil {
		result := toolError("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
		return marshalResult(result)
	}

	toolCtx, cancel := context.WithTimeout(ctx, e.toolTimeout)
	defer cancel()

	var result ToolResult
	switch toolCall.Name {
	case "create_ai_task":
		result = e.executeCreateAITask(toolCtx, args)
	case "create_container_task":
		result = e.executeCreateContainerTask(toolCtx, args)
	case "create_agent_task":
		result = e.executeCreateAgentTask(toolCtx, args)
	case "check_task_progress":
		result = e.executeCheckTaskProgress(toolCtx, args)
	case "fetch_task_output":
		result = e.executeFetchTaskOutput(toolCtx, args)
	case "wait_for_task":
		result = e.executeWaitForTask(toolCtx, args)
	case "cancel_task":
		result = e.executeCancelTask(toolCtx, args)
	case "list_agents":
		result = e.executeListAgents(toolCtx, args)
	case "list_tools":
		result = e.executeListTools(toolCtx, args)
	case "list_tasks":
		result = e.executeListTasks(toolCtx, args)
	case "create_agent":
		result = e.executeCreateAgent(toolCtx, args)
	case "update_agent":
		result = e.executeUpdateAgent(toolCtx, args)
	case "delete_agent":
		result = e.executeDeleteAgent(toolCtx, args)
	case "create_tool":
		result = e.executeCreateTool(toolCtx, args)
	case "delete_tool":
		result = e.executeDeleteTool(toolCtx, args)
	case "delete_session":
		result = e.executeDeleteSession(toolCtx, args)
	default:
		result = toolError("unknown_tool", fmt.Sprintf("unknown tool: %s", toolCall.Name), "Use one of the available tools")
	}

	return marshalResult(result)
}

// ---- Task creation helpers ----

func (e *ToolExecutor) generateTaskName() string {
	seq := e.taskSeq.Add(1)
	prefix := e.sessionID
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return fmt.Sprintf("chat-%s-%d", prefix, seq)
}

func (e *ToolExecutor) taskLabels() map[string]string {
	return map[string]string{
		"mercan.ai/created-by":   "orchestrator",
		"mercan.ai/chat-session": e.sessionID,
	}
}

func (e *ToolExecutor) checkTaskLimit() *ToolResult {
	if e.tasksCreated >= e.maxTasks {
		r := toolError("limit_reached", fmt.Sprintf("task creation limit reached (max %d per turn)", e.maxTasks), "Wait for existing tasks to complete before creating new ones")
		return &r
	}
	return nil
}

func (e *ToolExecutor) checkNamespaceScope(namespace string) *ToolResult {
	if e.watchNamespace != "" && namespace != e.watchNamespace {
		r := toolError("permission_denied", fmt.Sprintf("cannot create resources in namespace %q, restricted to %q", namespace, e.watchNamespace), "Use the allowed namespace")
		return &r
	}
	return nil
}

func (e *ToolExecutor) executeCreateAITask(ctx context.Context, args map[string]any) ToolResult {
	if limit := e.checkTaskLimit(); limit != nil {
		return *limit
	}

	prompt := getStringArg(args, "prompt")
	if prompt == "" {
		return toolError("invalid_arguments", "prompt is required", "Provide a prompt for the AI task")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)
	if ns := e.checkNamespaceScope(namespace); ns != nil {
		return *ns
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.generateTaskName(),
			Namespace: namespace,
			Labels:    e.taskLabels(),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAI,
			Prompt: prompt,
		},
	}

	if agentRef := getStringArg(args, "agentRef"); agentRef != "" {
		task.Spec.AgentRef = &corev1alpha1.AgentReference{Name: agentRef}
	}

	// Set provider reference (defaults to "default")
	providerName := getStringArgDefault(args, "providerRef", "default")
	if task.Spec.AI == nil {
		task.Spec.AI = &corev1alpha1.AISpec{}
	}
	task.Spec.AI.ProviderRef = &corev1alpha1.ProviderReference{Name: providerName}

	if timeoutStr := getStringArg(args, "timeout"); timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return toolError("invalid_arguments", fmt.Sprintf("invalid timeout: %v", err), "Use Go duration format (e.g., 30s, 5m)")
		}
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}

	if priority, ok := args["priority"]; ok {
		p := int32(getIntArg(args, "priority", int(priority.(float64))))
		task.Spec.Priority = &p
	}

	if sessionRef := getStringArg(args, "sessionRef"); sessionRef != "" {
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: sessionRef}
	}

	if err := e.client.Create(ctx, task); err != nil {
		return classifyK8sError(err)
	}

	e.tasksCreated++
	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":      task.Name,
			"namespace": task.Namespace,
			"phase":     "Pending",
			"message":   "Task created",
		},
	}
}

func (e *ToolExecutor) executeCreateContainerTask(ctx context.Context, args map[string]any) ToolResult {
	if limit := e.checkTaskLimit(); limit != nil {
		return *limit
	}

	image := getStringArg(args, "image")
	if image == "" {
		return toolError("invalid_arguments", "image is required", "Provide a container image")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)
	if ns := e.checkNamespaceScope(namespace); ns != nil {
		return *ns
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.generateTaskName(),
			Namespace: namespace,
			Labels:    e.taskLabels(),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   image,
			Command: getStringSliceArg(args, "command"),
			Args:    getStringSliceArg(args, "args"),
		},
	}

	if timeoutStr := getStringArg(args, "timeout"); timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return toolError("invalid_arguments", fmt.Sprintf("invalid timeout: %v", err), "Use Go duration format (e.g., 30s, 5m)")
		}
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}

	if _, ok := args["priority"]; ok {
		p := int32(getIntArg(args, "priority", 500))
		task.Spec.Priority = &p
	}

	if err := e.client.Create(ctx, task); err != nil {
		return classifyK8sError(err)
	}

	e.tasksCreated++
	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":      task.Name,
			"namespace": task.Namespace,
			"phase":     "Pending",
			"message":   "Task created",
		},
	}
}

func (e *ToolExecutor) executeCreateAgentTask(ctx context.Context, args map[string]any) ToolResult {
	if limit := e.checkTaskLimit(); limit != nil {
		return *limit
	}

	prompt := getStringArg(args, "prompt")
	if prompt == "" {
		return toolError("invalid_arguments", "prompt is required", "Provide a prompt for the agent task")
	}

	agentRef := getStringArg(args, "agentRef")
	if agentRef == "" {
		return toolError("invalid_arguments", "agentRef is required", "Provide an agent reference for the agent task")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)
	if ns := e.checkNamespaceScope(namespace); ns != nil {
		return *ns
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      e.generateTaskName(),
			Namespace: namespace,
			Labels:    e.taskLabels(),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:   corev1alpha1.TaskTypeAgent,
			Prompt: prompt,
			AgentRef: &corev1alpha1.AgentReference{
				Name: agentRef,
			},
		},
	}

	if timeoutStr := getStringArg(args, "timeout"); timeoutStr != "" {
		d, err := time.ParseDuration(timeoutStr)
		if err != nil {
			return toolError("invalid_arguments", fmt.Sprintf("invalid timeout: %v", err), "Use Go duration format (e.g., 30s, 5m)")
		}
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}

	// Agent runtime overrides
	var agentRuntime *corev1alpha1.AgentRuntimeSpec

	if maxTurns, ok := args["maxTurns"]; ok {
		if agentRuntime == nil {
			agentRuntime = &corev1alpha1.AgentRuntimeSpec{}
		}
		mt := int32(maxTurns.(float64))
		agentRuntime.MaxTurns = &mt
	}

	if ws, ok := args["workspace"]; ok {
		if wsMap, ok := ws.(map[string]any); ok {
			if agentRuntime == nil {
				agentRuntime = &corev1alpha1.AgentRuntimeSpec{}
			}
			wsCfg := &corev1alpha1.WorkspaceConfig{}
			if gitRepo := getStringArg(wsMap, "gitRepo"); gitRepo != "" {
				wsCfg.GitRepo = gitRepo
			}
			if branch := getStringArg(wsMap, "branch"); branch != "" {
				wsCfg.Branch = branch
			}
			if subPath := getStringArg(wsMap, "subPath"); subPath != "" {
				wsCfg.SubPath = subPath
			}
			agentRuntime.Workspace = wsCfg
		}
	}

	task.Spec.AgentRuntime = agentRuntime

	if err := e.client.Create(ctx, task); err != nil {
		return classifyK8sError(err)
	}

	e.tasksCreated++
	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":      task.Name,
			"namespace": task.Namespace,
			"phase":     "Pending",
			"message":   "Task created",
		},
	}
}

// ---- Query tools ----

func (e *ToolExecutor) executeCheckTaskProgress(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide the task name")
	}
	namespace := getStringArgDefault(args, "namespace", e.namespace)

	task := &corev1alpha1.Task{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
		return classifyK8sError(err)
	}

	data := map[string]any{
		"name":      task.Name,
		"namespace": task.Namespace,
		"phase":     string(task.Status.Phase),
		"message":   task.Status.Message,
	}

	if task.Status.StartTime != nil {
		duration := time.Since(task.Status.StartTime.Time)
		data["duration"] = duration.Round(time.Second).String()
	}

	if len(task.Status.Conditions) > 0 {
		conditions := make([]map[string]string, 0, len(task.Status.Conditions))
		for _, c := range task.Status.Conditions {
			conditions = append(conditions, map[string]string{
				"type":    c.Type,
				"status":  string(c.Status),
				"reason":  c.Reason,
				"message": c.Message,
			})
		}
		data["conditions"] = conditions
	}

	return ToolResult{Success: true, Data: data}
}

func (e *ToolExecutor) executeFetchTaskOutput(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide the task name")
	}
	namespace := getStringArgDefault(args, "namespace", e.namespace)

	task := &corev1alpha1.Task{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
		return classifyK8sError(err)
	}

	if task.Status.ResultRef == nil {
		return toolError("not_found", "task has no result yet", "Wait for the task to complete, then try again")
	}

	cm := &corev1.ConfigMap{}
	if err := e.client.Get(ctx, types.NamespacedName{
		Name:      task.Status.ResultRef.ConfigMapName,
		Namespace: namespace,
	}, cm); err != nil {
		return classifyK8sError(err)
	}

	key := task.Status.ResultRef.Key
	if key == "" {
		key = "result"
	}
	result := cm.Data[key]

	const maxLen = 2048
	if len(result) > maxLen {
		result = result[:maxLen] + fmt.Sprintf(" [truncated, full output: %d chars]", len(cm.Data[key]))
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":   task.Name,
			"phase":  string(task.Status.Phase),
			"output": result,
		},
	}
}

func (e *ToolExecutor) executeWaitForTask(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide the task name")
	}
	namespace := getStringArgDefault(args, "namespace", e.namespace)
	timeout := min(getIntArg(args, "timeout", 30), 60)

	// Check current status first
	task := &corev1alpha1.Task{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
		return classifyK8sError(err)
	}

	if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || task.Status.Phase == corev1alpha1.TaskPhaseFailed {
		return e.taskStatusResult(task)
	}

	// Poll every 2 seconds until timeout
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return e.taskTimeoutResult(task)
		case <-ticker.C:
			if time.Now().After(deadline) {
				// Re-fetch latest status before returning
				_ = e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task)
				return e.taskTimeoutResult(task)
			}

			if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
				return classifyK8sError(err)
			}

			if task.Status.Phase == corev1alpha1.TaskPhaseSucceeded || task.Status.Phase == corev1alpha1.TaskPhaseFailed {
				return e.taskStatusResult(task)
			}
		}
	}
}

func (e *ToolExecutor) taskStatusResult(task *corev1alpha1.Task) ToolResult {
	data := map[string]any{
		"name":    task.Name,
		"phase":   string(task.Status.Phase),
		"message": task.Status.Message,
	}
	if task.Status.StartTime != nil {
		elapsed := time.Since(task.Status.StartTime.Time)
		if task.Status.CompletionTime != nil {
			elapsed = task.Status.CompletionTime.Sub(task.Status.StartTime.Time)
		}
		data["elapsed"] = elapsed.Round(time.Second).String()
	}
	return ToolResult{Success: true, Data: data}
}

func (e *ToolExecutor) taskTimeoutResult(task *corev1alpha1.Task) ToolResult {
	data := map[string]any{
		"name":    task.Name,
		"phase":   string(task.Status.Phase),
		"message": "Task is still running. Call wait_for_task again to continue waiting, or do other work in the meantime.",
	}
	if task.Status.StartTime != nil {
		data["elapsed"] = time.Since(task.Status.StartTime.Time).Round(time.Second).String()
	}
	return ToolResult{Success: true, Data: data}
}

func (e *ToolExecutor) executeCancelTask(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide the task name")
	}
	namespace := getStringArgDefault(args, "namespace", e.namespace)

	task := &corev1alpha1.Task{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, task); err != nil {
		return classifyK8sError(err)
	}

	if err := e.client.Delete(ctx, task); err != nil {
		return classifyK8sError(err)
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":    task.Name,
			"message": "Task cancelled and deleted",
		},
	}
}

// ---- Discovery tools ----

func (e *ToolExecutor) executeListAgents(ctx context.Context, args map[string]any) ToolResult {
	namespace := getStringArgDefault(args, "namespace", e.namespace)

	agentList := &corev1alpha1.AgentList{}
	if err := e.client.List(ctx, agentList, client.InNamespace(namespace)); err != nil {
		return classifyK8sError(err)
	}

	agents := make([]map[string]any, 0, len(agentList.Items))
	for _, agent := range agentList.Items {
		info := map[string]any{
			"name": agent.Name,
		}

		if agent.Spec.Model != nil {
			info["model"] = fmt.Sprintf("%s/%s", agent.Spec.Model.Provider, agent.Spec.Model.Name)
		}

		if len(agent.Spec.Tools) > 0 {
			toolNames := make([]string, 0, len(agent.Spec.Tools))
			for _, t := range agent.Spec.Tools {
				toolNames = append(toolNames, t.Name)
			}
			info["tools"] = toolNames
		}

		if agent.Spec.Runtime != nil {
			info["runtime"] = string(agent.Spec.Runtime.Type)
		}

		if desc, ok := agent.Annotations["description"]; ok {
			info["description"] = desc
		}

		agents = append(agents, info)
	}

	return ToolResult{Success: true, Data: agents}
}

func (e *ToolExecutor) executeListTools(ctx context.Context, args map[string]any) ToolResult {
	namespace := getStringArgDefault(args, "namespace", e.namespace)

	toolList := &corev1alpha1.ToolList{}
	if err := e.client.List(ctx, toolList, client.InNamespace(namespace)); err != nil {
		return classifyK8sError(err)
	}

	tools := make([]map[string]any, 0, len(toolList.Items))
	for _, tool := range toolList.Items {
		tools = append(tools, map[string]any{
			"name":        tool.Name,
			"description": tool.Spec.Description,
			"builtin":     false,
		})
	}

	return ToolResult{Success: true, Data: tools}
}

func (e *ToolExecutor) executeListTasks(ctx context.Context, args map[string]any) ToolResult {
	namespace := getStringArgDefault(args, "namespace", e.namespace)
	statusFilter := getStringArg(args, "status")
	limit := getIntArg(args, "limit", 20)

	taskList := &corev1alpha1.TaskList{}
	if err := e.client.List(ctx, taskList, client.InNamespace(namespace)); err != nil {
		return classifyK8sError(err)
	}

	tasks := make([]map[string]any, 0)
	for _, task := range taskList.Items {
		if statusFilter != "" && !strings.EqualFold(string(task.Status.Phase), statusFilter) {
			continue
		}

		age := time.Since(task.CreationTimestamp.Time).Round(time.Second).String()

		tasks = append(tasks, map[string]any{
			"name":  task.Name,
			"phase": string(task.Status.Phase),
			"type":  string(task.Spec.Type),
			"age":   age,
		})

		if len(tasks) >= limit {
			break
		}
	}

	return ToolResult{Success: true, Data: tasks}
}

// ---- Management tools ----

func (e *ToolExecutor) executeCreateAgent(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide a name for the agent")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)
	if ns := e.checkNamespaceScope(namespace); ns != nil {
		return *ns
	}

	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1alpha1.AgentSpec{},
	}

	// Model configuration
	if modelObj, ok := args["model"]; ok {
		switch m := modelObj.(type) {
		case map[string]any:
			agent.Spec.Model = &corev1alpha1.ModelConfig{
				Name:     getStringArg(m, "name"),
				Provider: getStringArg(m, "provider"),
			}
			if temp, ok := m["temperature"]; ok {
				if t, ok := temp.(float64); ok {
					agent.Spec.Model.Temperature = &t
				}
			}
		case string:
			// Support "provider/model" format
			parts := strings.SplitN(m, "/", 2)
			if len(parts) == 2 {
				agent.Spec.Model = &corev1alpha1.ModelConfig{
					Provider: parts[0],
					Name:     parts[1],
				}
			} else {
				agent.Spec.Model = &corev1alpha1.ModelConfig{
					Name: m,
				}
			}
		}
	}

	// Set providerRef (defaults to "default")
	providerRefName := getStringArgDefault(args, "providerRef", "default")
	agent.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: providerRefName}

	if systemPrompt := getStringArg(args, "systemPrompt"); systemPrompt != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{
			Inline: systemPrompt,
		}
	}

	if toolNames := getStringSliceArg(args, "tools"); len(toolNames) > 0 {
		for _, t := range toolNames {
			agent.Spec.Tools = append(agent.Spec.Tools, corev1alpha1.ToolReference{Name: t})
		}
	}

	if rt, ok := args["runtime"]; ok {
		if rtMap, ok := rt.(map[string]any); ok {
			runtimeType := getStringArg(rtMap, "type")
			if runtimeType != "" {
				agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{
					Type: corev1alpha1.AgentRuntimeType(runtimeType),
				}
			}
		}
	}

	if err := e.client.Create(ctx, agent); err != nil {
		return classifyK8sError(err)
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":      agent.Name,
			"namespace": agent.Namespace,
			"message":   "Agent created",
		},
	}
}

func (e *ToolExecutor) executeUpdateAgent(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide the agent name")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)

	agent := &corev1alpha1.Agent{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		return classifyK8sError(err)
	}

	// Update specified fields
	if modelProvider := getStringArg(args, "model"); modelProvider != "" {
		parts := strings.SplitN(modelProvider, "/", 2)
		if agent.Spec.Model == nil {
			agent.Spec.Model = &corev1alpha1.ModelConfig{}
		}
		if len(parts) == 2 {
			agent.Spec.Model.Provider = parts[0]
			agent.Spec.Model.Name = parts[1]
		} else {
			agent.Spec.Model.Name = modelProvider
		}
	}

	if systemPrompt := getStringArg(args, "systemPrompt"); systemPrompt != "" {
		agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{
			Inline: systemPrompt,
		}
	}

	if toolNames := getStringSliceArg(args, "tools"); len(toolNames) > 0 {
		agent.Spec.Tools = nil
		for _, t := range toolNames {
			agent.Spec.Tools = append(agent.Spec.Tools, corev1alpha1.ToolReference{Name: t})
		}
	}

	// Re-fetch before update to avoid conflicts
	latest := &corev1alpha1.Agent{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, latest); err != nil {
		return classifyK8sError(err)
	}
	agent.ResourceVersion = latest.ResourceVersion

	if err := e.client.Update(ctx, agent); err != nil {
		return classifyK8sError(err)
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":    agent.Name,
			"message": "Agent updated",
		},
	}
}

func (e *ToolExecutor) executeDeleteAgent(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide the agent name")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)

	agent := &corev1alpha1.Agent{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
		return classifyK8sError(err)
	}

	if err := e.client.Delete(ctx, agent); err != nil {
		return classifyK8sError(err)
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":    agent.Name,
			"message": "Agent deleted",
		},
	}
}

func (e *ToolExecutor) executeCreateTool(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide a name for the tool")
	}

	description := getStringArg(args, "description")
	if description == "" {
		return toolError("invalid_arguments", "description is required", "Provide a description for the tool")
	}

	url := getStringArg(args, "url")
	if url == "" {
		return toolError("invalid_arguments", "url is required", "Provide the HTTP endpoint URL")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)
	if ns := e.checkNamespaceScope(namespace); ns != nil {
		return *ns
	}

	method := getStringArgDefault(args, "method", "POST")

	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1alpha1.ToolSpec{
			Description: description,
			HTTP: corev1alpha1.HTTPExecution{
				URL:    url,
				Method: method,
			},
		},
	}

	if err := e.client.Create(ctx, tool); err != nil {
		return classifyK8sError(err)
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":      tool.Name,
			"namespace": tool.Namespace,
			"message":   "Tool created",
		},
	}
}

func (e *ToolExecutor) executeDeleteTool(ctx context.Context, args map[string]any) ToolResult {
	name := getStringArg(args, "name")
	if name == "" {
		return toolError("invalid_arguments", "name is required", "Provide the tool name")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)

	tool := &corev1alpha1.Tool{}
	if err := e.client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, tool); err != nil {
		return classifyK8sError(err)
	}

	if err := e.client.Delete(ctx, tool); err != nil {
		return classifyK8sError(err)
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"name":    tool.Name,
			"message": "Tool deleted",
		},
	}
}

func (e *ToolExecutor) executeDeleteSession(ctx context.Context, args map[string]any) ToolResult {
	sessionID := getStringArg(args, "sessionId")
	if sessionID == "" {
		return toolError("invalid_arguments", "sessionId is required", "Provide the session ID")
	}

	namespace := getStringArgDefault(args, "namespace", e.namespace)

	if err := e.sessionManager.DeleteSession(ctx, namespace, sessionID); err != nil {
		return classifyK8sError(err)
	}

	return ToolResult{
		Success: true,
		Data: map[string]any{
			"sessionId": sessionID,
			"message":   "Session deleted",
		},
	}
}

// ---- Error handling helpers ----

func toolError(errType, message, suggestion string) ToolResult {
	return ToolResult{
		Success:    false,
		Error:      message,
		ErrorType:  errType,
		Suggestion: suggestion,
	}
}

func classifyK8sError(err error) ToolResult {
	if apierrors.IsNotFound(err) {
		return toolError("not_found", err.Error(), "Check the resource name and namespace")
	}
	if apierrors.IsAlreadyExists(err) {
		return toolError("already_exists", err.Error(), "Use a different name or delete the existing resource first")
	}
	if apierrors.IsForbidden(err) {
		return toolError("permission_denied", err.Error(), "Check RBAC permissions")
	}
	return toolError("internal_error", err.Error(), "")
}

func marshalResult(result ToolResult) (string, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tool result: %w", err)
	}
	return string(b), nil
}

// ---- Argument extraction helpers ----

func getStringArg(args map[string]any, key string) string {
	v, ok := args[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func getStringArgDefault(args map[string]any, key, defaultVal string) string {
	v := getStringArg(args, key)
	if v == "" {
		return defaultVal
	}
	return v
}

func getIntArg(args map[string]any, key string, defaultVal int) int {
	v, ok := args[key]
	if !ok {
		return defaultVal
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return defaultVal
	}
}

func getStringSliceArg(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			result = append(result, s)
		} else {
			result = append(result, fmt.Sprintf("%v", item))
		}
	}
	return result
}
