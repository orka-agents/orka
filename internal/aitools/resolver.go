/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package aitools resolves the effective built-in and configured tool set exposed
// to AI workers and brokered agent runtimes. It is deliberately pure so
// authorization, child transaction checks, and runtime injection share the
// exact same decision.
package aitools

import (
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

var memoryToolNames = []string{
	"recall_memory",
	"remember",
	"propose_memory",
	"search_transcript",
}

var registeredCoordinationToolNames = []string{
	"delegate_task",
	"wait_for_tasks",
	"create_container_task",
	"cancel_task",
	"send_message",
	"check_messages",
	"create_pull_request",
	"check_pull_request_ci",
	"merge_pull_request",
	"auto_merge_pull_request",
	"review_pull_request",
	"post_review_comment",
	"check_pr_review_marker",
	"list_issues",
	"list_pull_requests",
	"get_issue",
	"comment_on_issue",
	"create_agent",
	"delete_agent",
	"update_plan",
	"recall_memory",
	"remember",
	"propose_memory",
	"search_transcript",
}

var implicitCoordinationToolNames = []string{
	"delegate_task",
	"wait_for_tasks",
	"create_container_task",
	"cancel_task",
	"send_message",
	"check_messages",
	"recall_memory",
	"remember",
	"propose_memory",
	"search_transcript",
	"create_pull_request",
	"list_pull_requests",
	"check_pr_review_marker",
	"check_pull_request_ci",
	"merge_pull_request",
	"auto_merge_pull_request",
	"review_pull_request",
	"post_review_comment",
	"create_agent",
	"delete_agent",
	"update_plan",
}

var childMessagingToolNames = []string{
	"send_message",
	"check_messages",
}

// MemoryToolNames returns the AI worker's always-on memory tool names.
func MemoryToolNames() []string {
	return append([]string(nil), memoryToolNames...)
}

// CoordinationToolNames returns the tools registered for coordinator workers.
func CoordinationToolNames() []string {
	return append([]string(nil), registeredCoordinationToolNames...)
}

// RegistersCoordinationTools reports whether the AI worker registers Orka's
// coordination tool implementations for task. Disabling implicit injection only
// narrows the selected tool list; AI child identity still enables the registry
// so explicitly selected coordination tools remain platform-provided.
func RegistersCoordinationTools(task *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	if !usesAIWorkerToolRegistry(task) {
		return false
	}
	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		return true
	}
	return task != nil && labels.ParentTaskName(task.Labels, task.Annotations) != ""
}

// IsImplicitTool reports whether name is injected by Orka rather than selected
// explicitly by an Agent or Task. Authorization uses it to distinguish platform
// tools from Tool CR references without broadening implicit capabilities.
func IsImplicitTool(task *corev1alpha1.Task, agent *corev1alpha1.Agent, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || !usesAIWorkerToolRegistry(task) {
		return false
	}
	disableImplicitTools := task != nil && task.Annotations[labels.AnnotationDisableCoordinationToolInject] == "true"
	if !disableImplicitTools {
		if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
			if containsTool(implicitCoordinationToolNames, name) || (agent.Spec.Coordination.Autonomous && name == "request_approval") {
				return true
			}
		}
		if task != nil && labels.ParentTaskName(task.Labels, task.Annotations) != "" && containsTool(childMessagingToolNames, name) {
			return true
		}
	}
	return task != nil && task.Spec.Type == corev1alpha1.TaskTypeAI && containsTool(memoryToolNames, name)
}

func containsTool(tools []string, name string) bool {
	return slices.Contains(tools, name)
}

func usesAIWorkerToolRegistry(task *corev1alpha1.Task) bool {
	return task == nil || task.Spec.Type == "" || task.Spec.Type == corev1alpha1.TaskTypeAI
}

// Resolve returns the ordered, de-duplicated configured and implicit tool set
// exposed for task. Container tasks do not expose AI tools. Agent tasks retain
// configured/brokered tools, while the in-process AI worker additionally gets
// its always-on memory tools.
func Resolve(task *corev1alpha1.Task, agent *corev1alpha1.Agent) []string {
	if task != nil && task.Spec.Type == corev1alpha1.TaskTypeContainer {
		return nil
	}

	tools := make([]string, 0)
	seen := make(map[string]struct{})
	appendTool := func(raw string) {
		name := strings.TrimSpace(raw)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		tools = append(tools, name)
	}
	appendTools := func(names []string) {
		for _, name := range names {
			appendTool(name)
		}
	}

	if agent != nil {
		for _, tool := range agent.Spec.Tools {
			if tool.Enabled != nil && !*tool.Enabled {
				continue
			}
			appendTool(tool.Name)
		}
	}
	if task != nil && task.Spec.AI != nil {
		appendTools(task.Spec.AI.Tools)
	}

	disableImplicitTools := task != nil && task.Annotations[labels.AnnotationDisableCoordinationToolInject] == "true"
	if usesAIWorkerToolRegistry(task) && agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled && !disableImplicitTools {
		appendTools(implicitCoordinationToolNames)
		if agent.Spec.Coordination.Autonomous {
			appendTool("request_approval")
		}
	}

	if usesAIWorkerToolRegistry(task) && task != nil && labels.ParentTaskName(task.Labels, task.Annotations) != "" && !disableImplicitTools {
		appendTools(childMessagingToolNames)
	}

	if task != nil && task.Spec.Type == corev1alpha1.TaskTypeAI {
		appendTools(memoryToolNames)
	}

	return tools
}
