/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package aitools resolves the effective built-in and configured tool set exposed
// to AI workers. It is deliberately pure so authorization and runtime injection
// can share the exact same decision.
package aitools

import (
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

var coordinationToolNames = []string{
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

// Resolve returns the ordered, de-duplicated AI tool set exposed for task. It
// combines enabled Agent tools, Task tools, implicit coordination/autonomous
// tools, child messaging tools, and the AI worker's always-on memory tools.
func Resolve(task *corev1alpha1.Task, agent *corev1alpha1.Agent) []string {
	if task != nil && task.Spec.Type != "" && task.Spec.Type != corev1alpha1.TaskTypeAI {
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
	if agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled && !disableImplicitTools {
		appendTools(coordinationToolNames)
		if agent.Spec.Coordination.Autonomous {
			appendTool("request_approval")
		}
	}

	if task != nil && labels.ParentTaskName(task.Labels, task.Annotations) != "" && !disableImplicitTools {
		appendTools(childMessagingToolNames)
	}

	if task != nil && task.Spec.Type == corev1alpha1.TaskTypeAI {
		appendTools(memoryToolNames)
	}

	return tools
}
