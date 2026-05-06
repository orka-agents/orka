/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
)

// CreateContainerTaskTool creates a container-type Task CR.
type CreateContainerTaskTool struct {
	k8sClient client.Client
}

// NewCreateContainerTaskTool creates a container task tool for coordinator agents.
func NewCreateContainerTaskTool(k8sClient client.Client) *CreateContainerTaskTool {
	return &CreateContainerTaskTool{k8sClient: k8sClient}
}

func (t *CreateContainerTaskTool) Name() string { return "create_container_task" }

func (t *CreateContainerTaskTool) Description() string {
	return "Create a container task to run commands. Use when the user needs to run a shell command, build code, or execute a container image. Do NOT use for LLM reasoning. Repository-dependent commands such as tests, builds, or git inspection must include workspace.gitRepo."
}

func (t *CreateContainerTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":    map[string]any{"type": "string", "description": "Task name"},
			"image":   map[string]any{"type": "string", "description": "Container image to run. Leave empty to use the default worker image which includes common tools (kubectl, sh) and writes results to a ConfigMap. Only set a custom image if you need a specific runtime not in the default worker."},
			"command": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Command to execute"},
			"args":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Arguments to the command"},
			"workspace": map[string]any{
				"type":        "object",
				"description": "Git workspace for the command. Required when the command validates, builds, tests, or inspects repository files. Orka prepares /workspace before running the container and records workspace provenance in the result.",
				"properties": map[string]any{
					"gitRepo":      map[string]any{"type": "string", "description": "Git repository URL"},
					"branch":       map[string]any{"type": "string", "description": "Git branch to clone from (must exist). Omit to use the default branch."},
					"ref":          map[string]any{"type": "string", "description": "Exact git ref, commit SHA, or tag to checkout. Prefer this for validation."},
					"gitSecretRef": map[string]any{"type": "string", "description": "Optional Secret name containing git credentials. Omit for public repositories. Container tasks do not auto-discover git credentials."},
					"subPath":      map[string]any{"type": "string", "description": "Sub-path within the repo to run from"},
					"pushBranch":   map[string]any{"type": "string", "description": "Branch name to push command-produced changes to. Omit for read-only validation."},
				},
			},
			"prior_task": map[string]any{"type": "string", "description": "Optional prior task whose structured diff should be applied before running the container command."},
			"namespace":  map[string]any{"type": "string", "description": "Namespace"},
			"timeout":    map[string]any{"type": "string", "description": "Timeout duration, e.g. \"5m\""},
			"priority":   map[string]any{"type": "integer", "description": "Priority 0-1000"},
			"schedule":   map[string]any{"type": "string", "description": "Cron schedule for recurring tasks (e.g., '0 */6 * * *' for every 6 hours, '0 9 * * 1-5' for weekdays at 9am, '*/5 * * * *' for every 5 minutes). Leave empty for one-time tasks."},
		},
		"required": []string{"name"},
	})
}

func (t *CreateContainerTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return t.executeCoordination(ctx, args)
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	if limitErr := tc.CheckTaskLimit(); limitErr != nil {
		return ChatToolErrorResult(limitErr.Type, limitErr.Message, limitErr.Suggestion)
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	task := buildContainerTask(a)
	if err := validateContainerTaskWorkspace(task); err != nil {
		return ChatToolErrorResult(err.Type, err.Message, err.Suggestion)
	}
	task.ObjectMeta = metav1.ObjectMeta{
		Name:      tc.GenerateTaskName(),
		Namespace: namespace,
		Labels:    tc.TaskLabels(),
	}

	if d, errResult, ok := parseTimeoutArg(a); !ok {
		return errResult, nil
	} else if d > 0 {
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}

	if _, ok := a["priority"]; ok {
		p := int32(chatGetIntArg(a, "priority", 500))
		task.Spec.Priority = &p
	}

	schedule := chatGetStringArg(a, "schedule")
	if schedule != "" {
		task.Spec.Schedule = schedule
	}

	if err := tc.Client.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{
		"name":      task.Name,
		"namespace": task.Namespace,
		"phase":     "Pending",
		"message":   taskCreatedMsg(schedule),
	})
}

func (t *CreateContainerTaskTool) executeCoordination(ctx context.Context, args json.RawMessage) (string, error) {
	if t.k8sClient == nil {
		return ChatToolErrorResult("internal_error", "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	namespace := chatGetStringArgDefault(a, "namespace", os.Getenv("ORKA_TASK_NAMESPACE"))
	if namespace == "" {
		namespace = defaultNamespace
	}
	parentName := os.Getenv("ORKA_TASK_NAME")
	if parentName == "" {
		return ChatToolErrorResult("internal_error", "ORKA_TASK_NAME is required for coordinator container tasks", "")
	}

	parentTask := &corev1alpha1.Task{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: parentName, Namespace: namespace}, parentTask); err != nil {
		return "", fmt.Errorf("failed to get parent task: %w", err)
	}

	task := buildContainerTask(a)
	if err := validateContainerTaskWorkspace(task); err != nil {
		return ChatToolErrorResult(err.Type, err.Message, err.Suggestion)
	}
	task.ObjectMeta = metav1.ObjectMeta{
		GenerateName: parentName + "-child-",
		Namespace:    namespace,
		Labels: map[string]string{
			labels.LabelParentTask:  labels.SelectorValue(parentName),
			labels.LabelCoordinator: trueStr,
		},
		Annotations: map[string]string{
			labels.AnnotationParentTaskName: parentName,
		},
	}
	if parentTask.Spec.Priority != nil {
		task.Spec.Priority = parentTask.Spec.Priority
	}
	if _, ok := a["priority"]; ok {
		p := int32(chatGetIntArg(a, "priority", 500))
		task.Spec.Priority = &p
	}
	if d, errResult, ok := parseTimeoutArg(a); !ok {
		return errResult, nil
	} else if d > 0 {
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}
	if schedule := chatGetStringArg(a, "schedule"); schedule != "" {
		task.Spec.Schedule = schedule
	}
	if parentTask.UID != "" {
		blockOwnerDeletion := true
		isController := true
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         corev1alpha1.GroupVersion.String(),
			Kind:               "Task",
			Name:               parentTask.Name,
			UID:                parentTask.UID,
			Controller:         &isController,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}}
	}

	if err := t.k8sClient.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}
	return ChatToolSuccess(map[string]any{
		"name":      task.Name,
		"namespace": task.Namespace,
		"phase":     "Pending",
		"message":   taskCreatedMsg(task.Spec.Schedule),
	})
}

func validateContainerTaskWorkspace(task *corev1alpha1.Task) *ChatToolError {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeContainer {
		return nil
	}
	if task.Spec.Workspace != nil && task.Spec.Workspace.GitRepo != "" {
		return nil
	}
	if !containerTaskLooksRepoDependent(task.Spec.Command, task.Spec.Args) {
		return nil
	}
	return &ChatToolError{
		Type:       "missing_workspace",
		Message:    "container command appears to validate or inspect repository files, but no workspace.gitRepo was provided",
		Suggestion: "Retry create_container_task with workspace.gitRepo, workspace.gitSecretRef when private, and workspace.ref or workspace.branch for the exact code under test.",
	}
}

func containerTaskLooksRepoDependent(command, args []string) bool {
	parts := make([]string, 0, len(command)+len(args))
	parts = append(parts, command...)
	parts = append(parts, args...)
	joined := strings.ToLower(strings.Join(parts, " "))
	patterns := []string{
		"go test", "go vet", "go build", "go list", "go mod",
		"npm test", "npm run", "pnpm ", "yarn ",
		"make", "pytest", "cargo test", "mvn test", "gradle",
		"git diff", "git status", "git log",
		"go.mod", "package.json", "pyproject.toml", "cargo.toml",
		"makefile", ".github/workflows",
	}
	for _, p := range patterns {
		if strings.Contains(joined, p) {
			return true
		}
	}
	return false
}

func buildContainerTask(a map[string]any) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   chatGetStringArg(a, "image"),
			Command: chatGetStringSliceArg(a, "command"),
			Args:    chatGetStringSliceArg(a, "args"),
		},
	}

	if ws, ok := a["workspace"]; ok {
		if wsMap, ok := ws.(map[string]any); ok {
			wsCfg := &corev1alpha1.WorkspaceConfig{}
			if gitRepo := chatGetStringArg(wsMap, "gitRepo"); gitRepo != "" {
				wsCfg.GitRepo = gitRepo
			}
			if branch := chatGetStringArg(wsMap, "branch"); branch != "" {
				wsCfg.Branch = branch
			}
			if ref := chatGetStringArg(wsMap, "ref"); ref != "" {
				wsCfg.Ref = ref
			}
			if subPath := chatGetStringArg(wsMap, "subPath"); subPath != "" {
				wsCfg.SubPath = subPath
			}
			if pushBranch := chatGetStringArg(wsMap, "pushBranch"); pushBranch != "" {
				wsCfg.PushBranch = pushBranch
			}
			if gitSecretRef := chatGetStringArg(wsMap, "gitSecretRef"); gitSecretRef != "" {
				wsCfg.GitSecretRef = &corev1.LocalObjectReference{Name: gitSecretRef}
			}
			task.Spec.Workspace = wsCfg
		}
	}

	if priorTask := chatGetStringArg(a, "prior_task"); priorTask != "" {
		task.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{Name: strings.TrimSpace(priorTask)}
	}
	return task
}
