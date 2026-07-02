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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tracing"
)

// CreateContainerTaskTool creates a container-type Task CR.
type CreateContainerTaskTool struct {
	k8sClient client.Client
}

// NewCreateContainerTaskTool creates a container task tool for coordinator agents.
func NewCreateContainerTaskTool(k8sClient client.Client) *CreateContainerTaskTool {
	return &CreateContainerTaskTool{k8sClient: k8sClient}
}

func (t *CreateContainerTaskTool) Name() string { return createContainerTaskToolName }

func (t *CreateContainerTaskTool) Description() string {
	return "Create a container task to run commands. Use when the user needs to run a shell command, build code, or execute a container image. Do NOT use for LLM reasoning. Repository-dependent commands such as tests, builds, or git inspection must include workspace.gitRepo."
}

func (t *CreateContainerTaskTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: taskNameDescription}, "image": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Container image to run. Leave empty to use the default worker image which includes common tools (kubectl, sh) and writes results to a ConfigMap. Only set a custom image if you need a specific runtime not in the default worker."},
		"command": map[string]any{jsonSchemaTypeField: jsonSchemaTypeArray, itemsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}, jsonSchemaDescriptionField: "Command to execute"},
		"args":    map[string]any{jsonSchemaTypeField: jsonSchemaTypeArray, itemsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}, jsonSchemaDescriptionField: "Arguments to the command"}, workspaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaDescriptionField: "Git workspace for the command. Required when the command validates, builds, tests, or inspects repository files. Orka prepares /workspace before running the container and records workspace provenance in the result.", jsonSchemaPropertiesField: map[string]any{
			"gitRepo":      map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Git repository URL"},
			"branch":       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Git branch to clone from (must exist). Omit to use the default branch."},
			"ref":          map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Exact git ref, commit SHA, or tag to checkout. Prefer this for validation."},
			"gitSecretRef": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional Secret name containing git credentials. Omit for public repositories. Container tasks do not auto-discover git credentials."},
			"subPath":      map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Sub-path within the repo to run from"},
			"pushBranch":   map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Branch name to push command-produced changes to. Omit for read-only validation."},
		},
		}, priorTaskField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional prior task whose structured diff should be applied before running the container command. If workspace is omitted, Orka copies the workspace from this prior task when available."}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, timeoutField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: timeoutDescription}, priorityField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Priority 0-1000"}, scheduleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: cronScheduleDescription},
	}, jsonSchemaRequiredField: []string{nameField},
	})
}

func (t *CreateContainerTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	// Route to the coordinator path when the chat-executor hooks are absent.
	// Worker-side ToolContexts only set Client/Namespace/Tenant/TaskID and
	// leave the chat-only function fields nil; calling them would panic.
	if tc == nil || tc.CheckTaskLimit == nil || tc.GenerateTaskName == nil || tc.TaskLabels == nil || tc.IncrementTasks == nil {
		return t.executeCoordination(ctx, args)
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	if limitErr := tc.CheckTaskLimit(); limitErr != nil {
		return ChatToolErrorResult(limitErr.Type, limitErr.Message, limitErr.Suggestion)
	}

	namespace := chatGetStringArgDefault(a, namespaceField, tc.Namespace)
	if r, ok := checkChatNamespaceScope(tc, namespace); !ok {
		return r, nil
	}

	task := buildContainerTask(a)
	if err := applyContainerPriorTaskWorkspace(ctx, tc.Client, namespace, task); err != nil {
		return classifyChatK8sErr(err)
	}
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

	if _, ok := a[priorityField]; ok {
		p := int32(chatGetIntArg(a, priorityField, 500))
		task.Spec.Priority = &p
	}

	schedule := chatGetStringArg(a, scheduleField)
	if schedule != "" {
		task.Spec.Schedule = schedule
	}

	if result, ok := authorizeTaskCreate(ctx, tc, task); !ok {
		return result, nil
	}
	tracing.StampTaskTraceContext(ctx, task)
	if err := tc.Client.Create(ctx, task); err != nil {
		return classifyChatK8sErr(err)
	}

	tc.IncrementTasks()
	return ChatToolSuccess(map[string]any{nameField: task.Name, namespaceField: task.Namespace, phaseField: taskPhasePendingString, messageField: taskCreatedMsg(schedule)})
}

func (t *CreateContainerTaskTool) executeCoordination(ctx context.Context, args json.RawMessage) (string, error) {
	if t.k8sClient == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	namespace := chatGetStringArgDefault(a, namespaceField, os.Getenv(envOrkaTaskNamespace))
	if namespace == "" {
		namespace = defaultNamespace
	}
	parentName := os.Getenv(envOrkaTaskName)
	if parentName == "" {
		return ChatToolErrorResult(internalErrorType, fmt.Sprintf("%s is required for coordinator container tasks", envOrkaTaskName), "")
	}

	parentTask := &corev1alpha1.Task{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: parentName, Namespace: namespace}, parentTask); err != nil {
		return "", fmt.Errorf("failed to get parent task: %w", err)
	}

	task := buildContainerTask(a)
	if err := applyContainerPriorTaskWorkspace(ctx, t.k8sClient, namespace, task); err != nil {
		return classifyChatK8sErr(err)
	}
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
	inheritTaskProvenance(task, parentTask)
	if _, ok := a[priorityField]; ok {
		p := int32(chatGetIntArg(a, priorityField, 500))
		task.Spec.Priority = &p
	}
	if d, errResult, ok := parseTimeoutArg(a); !ok {
		return errResult, nil
	} else if d > 0 {
		task.Spec.Timeout = &metav1.Duration{Duration: d}
	}
	if schedule := chatGetStringArg(a, scheduleField); schedule != "" {
		task.Spec.Schedule = schedule
	}
	if parentTask.UID != "" {
		isController := true
		blockOwnerDeletion := true
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         corev1alpha1.GroupVersion.String(),
			Kind:               "Task",
			Name:               parentTask.Name,
			UID:                parentTask.UID,
			Controller:         &isController,
			BlockOwnerDeletion: &blockOwnerDeletion,
		}}
	}
	if err := validateChildTaskAgainstParentTransaction(ctx, t.k8sClient, parentTask, task, ""); err != nil {
		return "", err
	}
	tracing.StampTaskTraceContext(ctx, task)

	childTokenExchangeEnabled, err := shouldPrepareChildTransactionToken(parentTask)
	if err != nil {
		return "", err
	}
	if childTokenExchangeEnabled {
		if task.Spec.Schedule != "" {
			return ChatToolErrorResult("unsupported_schedule", "scheduled child container tasks cannot inherit delegated transaction tokens", "Create an immediate child container task, or create scheduled work from a task that does not need delegated child tokens.")
		}
		markChildTransactionTokenPending(task)
		if err := prepareChildTransactionToken(ctx, t.k8sClient, parentTask, task, "createContainerTask", ""); err != nil {
			return "", err
		}
	}
	if err := t.k8sClient.Create(ctx, task); err != nil {
		if childTokenExchangeEnabled {
			cleanupChildTransactionTokenSecret(ctx, t.k8sClient, task)
		}
		return classifyChatK8sErr(err)
	}
	if childTokenExchangeEnabled {
		if err := adoptChildTransactionTokenSecret(ctx, t.k8sClient, task); err != nil {
			cleanupChildTaskAfterTokenAdoptionFailure(ctx, t.k8sClient, task)
			return classifyChatK8sErr(err)
		}
		if err := patchPreparedChildTransactionToken(ctx, t.k8sClient, task); err != nil {
			cleanupChildTaskAfterTokenAdoptionFailure(ctx, t.k8sClient, task)
			return classifyChatK8sErr(err)
		}
	}
	return ChatToolSuccess(map[string]any{nameField: task.Name, namespaceField: task.Namespace, phaseField: taskPhasePendingString, messageField: taskCreatedMsg(task.Spec.Schedule)})
}

func applyContainerPriorTaskWorkspace(ctx context.Context, k8sClient client.Reader, namespace string, task *corev1alpha1.Task) error {
	if task == nil || task.Spec.PriorTaskRef == nil {
		return nil
	}
	task.Spec.PriorTaskRef.Name = strings.TrimSpace(task.Spec.PriorTaskRef.Name)
	if task.Spec.PriorTaskRef.Name == "" {
		task.Spec.PriorTaskRef = nil
		return nil
	}
	if task.Spec.PriorTaskRef.Namespace == "" {
		task.Spec.PriorTaskRef.Namespace = namespace
	}
	if task.Spec.Workspace != nil || k8sClient == nil {
		return nil
	}

	priorTask := &corev1alpha1.Task{}
	key := types.NamespacedName{Name: task.Spec.PriorTaskRef.Name, Namespace: task.Spec.PriorTaskRef.Namespace}
	if err := k8sClient.Get(ctx, key, priorTask); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if priorWorkspace := taskWorkspace(priorTask); priorWorkspace != nil {
		task.Spec.Workspace = priorWorkspace.DeepCopy()
	}
	return nil
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
		Message:    "container command appears to validate or inspect repository files, but no workspace.gitRepo was provided or inherited",
		Suggestion: "Retry create_container_task with workspace.gitRepo, workspace.gitSecretRef when private, and workspace.ref or workspace.branch for the exact code under test. Alternatively provide prior_task for a task that already has a workspace.",
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

	if ws, ok := a[workspaceField]; ok {
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

	if priorTask := chatGetStringArg(a, priorTaskField); priorTask != "" {
		task.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{Name: strings.TrimSpace(priorTask)}
	}
	return task
}
