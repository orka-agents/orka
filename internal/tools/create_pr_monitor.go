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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/workerenv"
)

// CreatePRMonitorTool creates a scheduled AI task that monitors GitHub PRs.
type CreatePRMonitorTool struct{}

var prMonitorRequiredTools = []string{
	listPullRequestsToolName,
	checkPRReviewMarkerToolName,
	checkPullRequestCIToolName,
	reviewPullRequestToolName,
	postReviewCommentToolName,
}

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
				jsonSchemaDescriptionField: "GitHub repository URL to monitor.",
			},
			scheduleField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: cronScheduleDescription,
			},
			agentRefField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Agent name for the scheduled monitor task. The agent must have coordination enabled.",
			},
			providerRefField: map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional Provider CRD reference name for the scheduled AI task.",
			},
			"gitSecretRef": map[string]any{
				jsonSchemaTypeField:        jsonSchemaTypeString,
				jsonSchemaDescriptionField: "Optional Secret name containing git/GitHub credentials for private repositories. If omitted, Orka tries common git credential secret names.",
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
		jsonSchemaRequiredField: []string{nameField, repoURLField, scheduleField, agentRefField},
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
	if namespace == "" {
		return ChatToolErrorResult("invalid_arguments", "namespace is required", "Provide namespace or set ORKA_TASK_NAMESPACE")
	}
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	agentRef := chatGetStringArg(args, agentRefField)
	if agentRef == "" {
		return ChatToolErrorResult("invalid_arguments", "agent_ref is required", "Provide an Agent with coordination enabled")
	}
	repoURL := chatGetStringArg(args, repoURLField)
	if repoURL == "" {
		return ChatToolErrorResult("invalid_arguments", "repo_url is required", "Provide the GitHub repository URL to monitor")
	}
	if _, _, err := security.ParseGitHubRepositoryURL(repoURL); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("invalid repo_url %q: %v", repoURL, err), "Provide a GitHub repository URL such as https://github.com/owner/repo")
	}
	var agent corev1alpha1.Agent
	if err := tc.Client.Get(ctx, types.NamespacedName{Name: agentRef, Namespace: namespace}, &agent); err != nil {
		if apierrors.IsNotFound(err) {
			return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("agent %q not found in namespace %q", agentRef, namespace), "Create the Agent or provide a valid agent_ref")
		}
		return classifyChatK8sErr(err)
	}
	if agent.Spec.Coordination == nil || !agent.Spec.Coordination.Enabled {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("agent %q must have coordination enabled", agentRef), "Enable coordination on the Agent before creating a PR monitor")
	}
	if agent.Spec.Runtime != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("agent %q is a runtime Agent, but PR monitors require an AI Agent", agentRef), "Provide an AI Agent without spec.runtime for the scheduled PR monitor")
	}
	if agent.Spec.Coordination.Autonomous {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("agent %q must not have autonomous coordination enabled", agentRef), "Disable coordination.autonomous for the Agent before creating a scheduled PR monitor")
	}
	if reviewEvent := chatGetStringArg(args, "review_event"); reviewEvent != "" {
		if _, ok := normalizePRMonitorReviewEvent(reviewEvent); !ok {
			return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("invalid review_event %q", reviewEvent), "Use COMMENT, APPROVE, or REQUEST_CHANGES")
		}
	}

	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tc.GenerateTaskName(),
			Namespace: namespace,
			Labels:    tc.TaskLabels(),
			Annotations: map[string]string{
				labels.AnnotationPRMonitorName:                 monitorName,
				labels.AnnotationDisableCoordinationToolInject: trueStr,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAI,
			Prompt:   buildPRMonitorPrompt(args),
			Schedule: schedule,
		},
	}

	task.Spec.AgentRef = &corev1alpha1.AgentReference{Name: agentRef}
	task.Spec.AI = &corev1alpha1.AISpec{
		Tools: append([]string(nil), prMonitorRequiredTools...),
	}
	workspace := &corev1alpha1.WorkspaceConfig{GitRepo: repoURL}
	requestedGitSecretRef := chatGetStringArg(args, "gitSecretRef")
	secretRef, secretRefErr := resolveWorkspaceGitSecretRef(ctx, tc.Client, namespace, nil, requestedGitSecretRef)
	if secretRefErr == nil && secretRef != nil {
		workspace.GitSecretRef = secretRef
	}
	task.Spec.Workspace = workspace
	task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: workerenv.GitRepo, Value: repoURL})
	if providerName := chatGetStringArg(args, providerRefField); providerName != "" {
		task.Spec.AI.ProviderRef = &corev1alpha1.ProviderReference{Name: providerName}
	}

	tracing.StampTaskTraceContext(ctx, task)
	if result, ok := authorizeTaskCreate(ctx, tc, task); !ok {
		return result, nil
	}

	if result, err := validatePRMonitorGitSecretRef(ctx, tc.Client, namespace, requestedGitSecretRef, secretRef, secretRefErr); result != "" || err != nil {
		return result, err
	}
	workspace.GitSecretRef = secretRef

	if err := tc.Client.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{
		nameField:      task.Name,
		"monitor_name": monitorName,
		namespaceField: task.Namespace,
		scheduleField:  task.Spec.Schedule,
		messageField:   "PR monitor task created",
	})
}

func validatePRMonitorGitSecretRef(ctx context.Context, k8sClient client.Reader, namespace, requested string, secretRef *corev1.LocalObjectReference, secretRefErr error) (string, error) {
	if requested != "" {
		var secret corev1.Secret
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: requested, Namespace: namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("git secretRef %q not found in namespace %q", requested, namespace), "Create the Secret or provide a valid gitSecretRef")
			}
			return classifyChatK8sErr(err)
		}
	}
	if secretRefErr != nil {
		if requested != "" {
			return ChatToolErrorResult("invalid_arguments", secretRefErr.Error(), "Create the Secret or provide a valid gitSecretRef")
		}
		return classifyChatK8sErr(secretRefErr)
	}
	if secretRef == nil {
		return ChatToolErrorResult(
			"invalid_arguments",
			fmt.Sprintf("gitSecretRef is required for PR monitor GitHub access; no supported git credential Secret found in namespace %q", namespace),
			fmt.Sprintf("Provide gitSecretRef or create one of these Secrets in namespace %q: %s", namespace, strings.Join(gitCredentialSecretCandidates, ", ")),
		)
	}
	if err := validateGitCredentialSecret(ctx, k8sClient, namespace, secretRef.Name); err != nil {
		return ChatToolErrorResult("invalid_arguments", err.Error(), "Use a Secret with a non-empty token, password, or GITHUB_TOKEN key")
	}
	return "", nil
}

func normalizePRMonitorReviewEvent(value string) (string, bool) {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return reviewEventComment, true
	}
	switch value {
	case reviewEventComment, reviewEventApprove, reviewEventRequestChanges:
		return value, true
	default:
		return "", false
	}
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

	reviewEvent, _ := normalizePRMonitorReviewEvent(chatGetStringArg(args, "review_event"))

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
