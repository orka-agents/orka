/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

// CreatePRMonitorTool creates a scheduled AI task that monitors GitHub PRs.
type CreatePRMonitorTool struct{}

func (t *CreatePRMonitorTool) Name() string { return createPRMonitorToolName }

func (t *CreatePRMonitorTool) Description() string {
	return "Create a scheduled pull request monitor task. The monitor lists open PRs, checks review markers, and reviews PRs that have not been reviewed for their current head SHA."
}

func (t *CreatePRMonitorTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		jsonSchemaTypeField: jsonSchemaTypeObject,
		jsonSchemaPropertiesField: map[string]any{
			nameField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Human-readable monitor name. The created task receives an Orka-generated task name.",
			},
			namespaceField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: namespaceDescription,
			},
			repoURLField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "GitHub repository URL to monitor. If omitted, the monitor relies on ORKA_GIT_REPO at execution time.",
			},
			scheduleField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: cronScheduleDescription,
			},
			agentRefField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional Agent name for the scheduled monitor task. The agent should have PR review tools available.",
			},
			providerRefField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional Provider CRD reference name for the scheduled AI task.",
			},
			perPageField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeInteger,
				jsonSchemaDescriptionField: "Maximum open PRs to scan per run. Defaults to 30, maximum 100.",
			},
			"review_event": map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Review event to post after analysis: COMMENT, APPROVE, or REQUEST_CHANGES. Defaults to COMMENT.",
			},
			promptField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional additional instructions appended to the generated PR monitor prompt.",
			},
		},
		jsonSchemaRequiredField: []string{nameField, scheduleField},
	})
}

func (t *CreatePRMonitorTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var args map[string]any
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	if limitErr := tc.CheckTaskLimit(); limitErr != nil {
		return ChatToolErrorResult(limitErr.Type, limitErr.Message, limitErr.Suggestion)
	}

	monitorName := chatGetStringArg(args, nameField)
	if monitorName == "" {
		return ChatToolErrorResult("invalid_arguments", "name is required", "Provide a name for the PR monitor")
	}

	schedule := chatGetStringArg(args, scheduleField)
	if schedule == "" {
		return ChatToolErrorResult("invalid_arguments", "schedule is required", "Provide a cron schedule for the PR monitor")
	}

	namespace := chatGetStringArgDefault(args, namespaceField, tc.Namespace)
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tc.GenerateTaskName(),
			Namespace: namespace,
			Labels:    tc.TaskLabels(),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			Prompt:   buildPRMonitorPrompt(args),
			Schedule: schedule,
		},
	}

	if agentRef := chatGetStringArg(args, agentRefField); agentRef != "" {
		task.Spec.AgentRef = &corev1alpha1.AgentReference{Name: agentRef}
	}

	if providerName := chatGetStringArg(args, providerRefField); providerName != "" {
		task.Spec.AI = &corev1alpha1.AISpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: providerName},
		}
	}

	if err := tc.Client.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{
		nameField:      task.Name,
		namespaceField: task.Namespace,
		scheduleField:  task.Spec.Schedule,
		messageField:   "PR monitor task created",
	})
}

func buildPRMonitorPrompt(args map[string]any) string {
	repoURL := chatGetStringArg(args, repoURLField)
	perPage := chatGetIntArg(args, perPageField, 30)
	if perPage <= 0 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}

	reviewEvent := strings.ToUpper(strings.TrimSpace(chatGetStringArg(args, "review_event")))
	if reviewEvent == "" {
		reviewEvent = reviewEventComment
	}

	var b strings.Builder
	b.WriteString("You are an automated pull request monitor.\n\n")
	b.WriteString("On each scheduled run:\n")
	repoArgText := ""
	if repoURL != "" {
		repoArgText = fmt.Sprintf(" and repo_url %q", repoURL)
	}

	b.WriteString("1. Call list_pull_requests to list open PRs")
	if repoURL != "" {
		b.WriteString(fmt.Sprintf(" with repo_url %q", repoURL))
	}
	b.WriteString(fmt.Sprintf(" and per_page %d.\n", perPage))
	b.WriteString("2. For each non-draft PR, call check_pr_review_marker with pr_number")
	b.WriteString(repoArgText)
	b.WriteString(". Omit head_sha unless you already know it; the tool fetches the current head SHA and returns the marker to use. Skip PRs that already have a marker for the same head SHA.\n")
	b.WriteString("3. For unreviewed PR heads, call check_pull_request_ci with pr_number")
	b.WriteString(repoArgText)
	b.WriteString(" to verify CI status before reviewing. Do not review PRs with failing CI unless explicitly instructed, and skip or wait for pending CI as appropriate.\n")
	b.WriteString("4. For unreviewed PR heads with acceptable CI, call review_pull_request with pr_number")
	b.WriteString(repoArgText)
	b.WriteString(" to fetch the diff, then analyze correctness, tests, security, and maintainability.\n")
	b.WriteString("5. Post exactly one GitHub review with post_review_comment using pr_number")
	b.WriteString(repoArgText)
	b.WriteString(". Include the marker returned by check_pr_review_marker in the review body so future runs skip the same head SHA.\n")
	b.WriteString(fmt.Sprintf("6. Use review event %s unless the analysis clearly requires REQUEST_CHANGES.\n", reviewEvent))
	b.WriteString("Be conservative: do not approve changes you have not fully reviewed, and do not post duplicate reviews.\n")

	if extra := strings.TrimSpace(chatGetStringArg(args, promptField)); extra != "" {
		b.WriteString("\nAdditional instructions:\n")
		b.WriteString(extra)
		b.WriteString("\n")
	}
	return b.String()
}
