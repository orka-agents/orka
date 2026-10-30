/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{"image": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Container image to run. Leave empty to use the default worker image which includes common tools (kubectl, sh) and writes results to a ConfigMap. Only set a custom image if you need a specific runtime not in the default worker. Custom images cannot publish workspace changes: do not combine with workspace.pushBranch."},
		"command": map[string]any{jsonSchemaTypeField: jsonSchemaTypeArray, itemsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}, jsonSchemaDescriptionField: "Command to execute, replacing the image entrypoint. Omit it to run the image entrypoint with args."},
		"args":    map[string]any{jsonSchemaTypeField: jsonSchemaTypeArray, itemsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}, jsonSchemaDescriptionField: "Arguments to the command"}, workspaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaDescriptionField: "Git workspace for the command. Required when the command validates, builds, tests, or inspects repository files. Orka prepares /workspace before running the container and records workspace provenance in the result.", jsonSchemaPropertiesField: map[string]any{
			"gitRepo":                  map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Source Git repository URL"},
			branchField:                map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Source branch to clone from (must exist). Omit to use the default branch."},
			refField:                   map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Exact source git ref, commit SHA, or tag to checkout. Prefer this for validation."},
			readCredentialRefField:     map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional Secret name for clone/read credentials. Omit for public repositories."},
			"publicationGitRepo":       map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Publication repository URL for command-produced changes"},
			"publicationCredentialRef": map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional Secret name for publication credentials"},
			"subPath":                  map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Sub-path within the repo to run from"},
			"pushBranch":               map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Publication branch name. Omit for read-only validation. Requires publicationCredentialRef and the default worker image; not supported with a custom image."},
		},
		}, priorTaskField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Optional prior task whose structured diff should be applied before running the container command. If workspace is omitted, Orka copies the workspace from this prior task when available."}, namespaceField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: namespaceDescription}, timeoutField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: timeoutDescription}, priorityField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaDescriptionField: "Priority 0-1000", jsonSchemaMinimumField: 0, jsonSchemaMaximumField: 1000}, scheduleField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: cronScheduleDescription},
		envField: map[string]any{
			jsonSchemaTypeField:        jsonSchemaTypeArray,
			jsonSchemaDescriptionField: "value is visible in the Task: pass credentials only via valueFrom.secretKeyRef. ORKA_* and OTEL_EXPORTER_OTLP* are reserved.",
			jsonSchemaMaxItemsField:    maxContainerTaskEnvVars,
			itemsField: map[string]any{
				jsonSchemaTypeField: jsonSchemaTypeObject,
				jsonSchemaPropertiesField: map[string]any{
					nameField:  map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaMaxLengthField: maxContainerTaskEnvNameBytes},
					valueField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaMaxLengthField: maxContainerTaskEnvValueBytes},
					valueFromField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{
						secretKeyRefField: containerTaskSecretNameSchema(keyField),
					}},
				},
				jsonSchemaRequiredField:        []string{nameField},
				jsonSchemaAdditionalProperties: false,
			},
		},
		secretRefField: func() map[string]any {
			schema := containerTaskSecretNameSchema()
			schema[jsonSchemaDescriptionField] = "Secret mounted as files under /secrets/task"
			return schema
		}(),
		resourcesField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaDescriptionField: `e.g. {requests:{cpu:"500m",memory:"1Gi"},limits:{cpu:"2",memory:"2Gi"}}; default limits 1 CPU/2Gi`, jsonSchemaPropertiesField: map[string]any{
			resourceRequestsField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaAdditionalProperties: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}},
			resourceLimitsField:   map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaAdditionalProperties: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}},
		}},
		retryPolicyField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaDescriptionField: "Only for idempotent commands", jsonSchemaPropertiesField: map[string]any{
			maxRetriesField:        map[string]any{jsonSchemaTypeField: jsonSchemaTypeInteger, jsonSchemaMinimumField: 0, jsonSchemaMaximumField: maxContainerTaskRetries},
			backoffMultiplierField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeNumber, jsonSchemaMinimumField: minContainerTaskBackoffMultiplier, jsonSchemaMaximumField: maxContainerTaskBackoffMultiplier},
			initialDelayField:      map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "Go duration, at most 5m"},
		}},
		executionField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{
			runtimeClassNameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString, jsonSchemaDescriptionField: "e.g. gvisor; operator allowlist only"},
		}},
	},
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

	if r, ok := requireChatObjectArg(a, workspaceField); !ok {
		return r, nil
	}
	task := buildContainerTask(a)
	if r, ok := applyContainerTaskSpecArgs(ctx, tc, namespace, a, task); !ok {
		return r, nil
	}
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

	if p, errResult, ok := chatPriorityArg(a); !ok {
		return errResult, nil
	} else if p != nil {
		task.Spec.Priority = p
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
	sealTaskCreate(ctx, tc, task)

	tc.IncrementTasks()
	tc.RecordCreatedTask(task)
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

	if r, ok := requireChatObjectArg(a, workspaceField); !ok {
		return r, nil
	}
	task := buildContainerTask(a)
	if r, ok := applyContainerTaskSpecArgs(ctx, GetToolContext(ctx), namespace, a, task); !ok {
		return r, nil
	}
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
	if p, errResult, ok := chatPriorityArg(a); !ok {
		return errResult, nil
	} else if p != nil {
		task.Spec.Priority = p
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
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "Task",
			Name:       parentTask.Name,
			UID:        parentTask.UID,
			Controller: &isController,
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
	// The owned child inherits requestedBy; the seal binds it to the child's
	// UID so connector-backed execution can trust it, as delegate_task does.
	sealTaskCreate(ctx, GetToolContext(ctx), task)
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
	// Mirror the Job builder's validateContainerPublicationWorkspace gates
	// against the effective (post-inheritance) workspace. The tool schema never
	// exposes these fields, so their only source here is a prior_task workspace
	// copied from an agent Task; created Tasks carrying them would be rejected
	// by the Job builder after creation.
	if field := unsupportedContainerWorkspaceField(task.Spec.Workspace); field != "" {
		return &ChatToolError{
			Type: "unsupported_container_workspace_field",
			Message: fmt.Sprintf(
				"container tasks do not support %s, which was inherited from the prior_task workspace", field),
			Suggestion: "Provide an explicit workspace (gitRepo, ref/branch, readCredentialRef) for the container task instead of inheriting the agent task's clean-room publication workspace.",
		}
	}
	if task.Spec.Workspace != nil && strings.TrimSpace(task.Spec.Workspace.PushBranch) != "" {
		if strings.TrimSpace(task.Spec.Image) != "" {
			return &ChatToolError{
				Type:       "unsupported_custom_image_publication",
				Message:    "custom-image container tasks do not support workspace.pushBranch publication",
				Suggestion: "Omit image so the default worker image handles publication, or remove workspace.pushBranch to run the custom image without publication.",
			}
		}
		if task.Spec.Workspace.PublicationCredentialRef == nil || strings.TrimSpace(task.Spec.Workspace.PublicationCredentialRef.Name) == "" {
			return &ChatToolError{
				Type:       "missing_publication_credential",
				Message:    "container workspace.pushBranch requires an explicit workspace.publicationCredentialRef",
				Suggestion: "Provide a publicationCredentialRef with write access to publicationGitRepo. Do not reuse readCredentialRef implicitly for publication.",
			}
		}
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
		Suggestion: "Retry create_container_task with workspace.gitRepo, workspace.readCredentialRef when private, and workspace.ref or workspace.branch for the exact code under test. Alternatively provide prior_task for a task that already has a workspace.",
	}
}

// unsupportedContainerWorkspaceField returns the first workspace field the Job
// builder's validateContainerPublicationWorkspace rejects for container Tasks:
// clean-room publication and policy fields supported only on agent Tasks. Keep
// this list in sync with internal/controller/job_builder.go.
func unsupportedContainerWorkspaceField(workspace *corev1alpha1.WorkspaceConfig) string {
	switch {
	case workspace == nil:
		return ""
	case strings.TrimSpace(workspace.ExpectedRemoteSHA) != "":
		return "workspace.expectedRemoteSHA"
	case workspace.CreatePR:
		return "workspace.createPR"
	case workspace.MaxChangedFiles != nil:
		return "workspace.maxChangedFiles"
	case len(workspace.AllowedPaths) > 0:
		return "workspace.allowedPaths"
	case workspace.DenyRepositoryControlPaths:
		return "workspace.denyRepositoryControlPaths"
	case workspace.RejectBinaryFiles:
		return "workspace.rejectBinaryFiles"
	case workspace.RejectSecretLikeContent:
		return "workspace.rejectSecretLikeContent"
	default:
		return ""
	}
}

func containerTaskLooksRepoDependent(command, args []string) bool {
	var joinedBuilder strings.Builder
	for _, part := range command {
		joinedBuilder.WriteByte(' ')
		joinedBuilder.WriteString(part)
	}
	for _, part := range args {
		joinedBuilder.WriteByte(' ')
		joinedBuilder.WriteString(part)
	}
	joined := strings.ToLower(joinedBuilder.String())
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
			if branch := chatGetStringArg(wsMap, branchField); branch != "" {
				wsCfg.Branch = branch
			}
			if ref := chatGetStringArg(wsMap, refField); ref != "" {
				wsCfg.Ref = ref
			}
			if subPath := chatGetStringArg(wsMap, "subPath"); subPath != "" {
				wsCfg.SubPath = subPath
			}
			if readCredentialRef := chatGetStringArg(wsMap, readCredentialRefField); readCredentialRef != "" {
				wsCfg.ReadCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: readCredentialRef}
			}
			if publicationGitRepo := chatGetStringArg(wsMap, "publicationGitRepo"); publicationGitRepo != "" {
				wsCfg.PublicationGitRepo = publicationGitRepo
			}
			if publicationCredentialRef := chatGetStringArg(wsMap, "publicationCredentialRef"); publicationCredentialRef != "" {
				wsCfg.PublicationCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: publicationCredentialRef}
			}
			if pushBranch := chatGetStringArg(wsMap, "pushBranch"); pushBranch != "" {
				wsCfg.Intent = corev1alpha1.WorkspaceIntentWrite
				wsCfg.PushBranch = pushBranch
			}
			task.Spec.Workspace = wsCfg
		}
	}

	if priorTask := chatGetStringArg(a, priorTaskField); priorTask != "" {
		task.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{Name: strings.TrimSpace(priorTask)}
	}
	return task
}

const (
	envField                       = "env"
	valueField                     = "value"
	valueFromField                 = "valueFrom"
	secretKeyRefField              = "secretKeyRef"
	keyField                       = "key"
	resourcesField                 = "resources"
	resourceRequestsField          = "requests"
	resourceLimitsField            = "limits"
	retryPolicyField               = "retryPolicy"
	maxRetriesField                = "maxRetries"
	backoffMultiplierField         = "backoffMultiplier"
	initialDelayField              = "initialDelay"
	executionField                 = "execution"
	runtimeClassNameField          = "runtimeClassName"
	jsonSchemaMaxItemsField        = "maxItems"
	jsonSchemaAdditionalProperties = "additionalProperties"

	maxContainerTaskEnvVars       = 64
	maxContainerTaskEnvValueBytes = 32 << 10
	// maxContainerTaskEnvNameBytes keeps every NAME=value entry far below
	// Linux's 128 KiB limit on a single environment string.
	maxContainerTaskEnvNameBytes = 1 << 10
	// maxContainerTaskEnvTotalBytes keeps the Task object well inside the
	// API server's request limit (1.5 MiB by default on etcd).
	maxContainerTaskEnvTotalBytes     = 256 << 10
	maxContainerTaskRetries           = 5
	minContainerTaskBackoffMultiplier = 1
	maxContainerTaskBackoffMultiplier = 10
	maxContainerTaskInitialDelay      = 5 * time.Minute
)

// containerTaskSecretNameSchema is a closed {name} object, plus any other
// required string fields such as a Secret key.
func containerTaskSecretNameSchema(fields ...string) map[string]any {
	properties := map[string]any{nameField: map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}}
	required := make([]string, 0, 1+len(fields))
	required = append(required, nameField)
	for _, field := range fields {
		properties[field] = map[string]any{jsonSchemaTypeField: jsonSchemaTypeString}
		required = append(required, field)
	}
	return map[string]any{
		jsonSchemaTypeField:            jsonSchemaTypeObject,
		jsonSchemaPropertiesField:      properties,
		jsonSchemaRequiredField:        required,
		jsonSchemaAdditionalProperties: false,
	}
}

// applyContainerTaskSpecArgs copies the container settings a coordinator may
// choose onto the Task. The schema advertises these limits; this enforces them,
// so nothing is created from a value the schema would have refused. Every
// Secret the Task would reference is authorized for the caller first.
func applyContainerTaskSpecArgs(ctx context.Context, tc *ToolContext, namespace string, a map[string]any, task *corev1alpha1.Task) (string, bool) {
	// A command whose first entry is blank names no executable: the container
	// fails to start. Models write [""] when told to leave command empty.
	if len(task.Spec.Command) > 0 && strings.TrimSpace(task.Spec.Command[0]) == "" {
		return containerTaskArgError("command[0] is empty, so the container would have nothing to run",
			"Omit command to run the image entrypoint, and put the script in args"), false
	}
	env, errResult, ok := parseContainerTaskEnvArg(a)
	if !ok {
		return errResult, false
	}
	task.Spec.Env = env

	secretRef, errResult, ok := parseContainerTaskSecretRefArg(a)
	if !ok {
		return errResult, false
	}
	task.Spec.SecretRef = secretRef

	resources, errResult, ok := parseContainerTaskResourcesArg(a)
	if !ok {
		return errResult, false
	}
	task.Spec.Resources = resources

	retryPolicy, errResult, ok := parseContainerTaskRetryPolicyArg(a)
	if !ok {
		return errResult, false
	}
	task.Spec.RetryPolicy = retryPolicy

	var allowed []string
	if tc != nil {
		allowed = tc.ContainerTaskRuntimeClasses
	}
	execution, errResult, ok := parseContainerTaskExecutionArg(a, allowed)
	if !ok {
		return errResult, false
	}
	task.Spec.Execution = execution
	return authorizeContainerTaskSecrets(ctx, tc, namespace, task)
}

// authorizeContainerTaskSecrets checks every Secret the Task names against the
// caller before anything is created. Delivery still requires the operator's
// direct Secret mount opt-in; this decides only whether the caller may ask.
func authorizeContainerTaskSecrets(ctx context.Context, tc *ToolContext, namespace string, task *corev1alpha1.Task) (string, bool) {
	var names []string
	if task.Spec.SecretRef != nil {
		names = append(names, task.Spec.SecretRef.Name)
	}
	for _, envVar := range task.Spec.Env {
		if envVar.ValueFrom != nil && envVar.ValueFrom.SecretKeyRef != nil {
			names = append(names, envVar.ValueFrom.SecretKeyRef.Name)
		}
	}
	slices.Sort(names)
	for _, name := range slices.Compact(names) {
		if tc == nil || tc.AuthorizeSecretRead == nil {
			if tc != nil && tc.RequireSecretReadAuthorization {
				result, _ := ChatToolErrorResult("authorization_failed", fmt.Sprintf("Secret %s/%s requires a secret read authorizer", namespace, name), "Retry from a context that can authorize Secret access")
				return result, false
			}
			continue
		}
		if authzErr := tc.AuthorizeSecretRead(ctx, namespace, name); authzErr != nil {
			result, _ := ChatToolErrorResult(authzErr.Type, fmt.Sprintf("not authorized to reference Secret %s/%s: %s", namespace, name, authzErr.Message), authzErr.Suggestion)
			return result, false
		}
	}
	return "", true
}

func parseContainerTaskSecretRefArg(a map[string]any) (*corev1alpha1.SecretReference, string, bool) {
	raw, ok := a[secretRefField]
	if !ok || raw == nil {
		return nil, "", true
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, containerTaskArgError("secretRef must be an object", `Pass secretRef as {"name":"my-secret"}`), false
	}
	for key := range values {
		switch key {
		case nameField:
		case namespaceField:
			return nil, containerTaskArgError("secretRef.namespace is not supported: the Secret must be in the Task's namespace", "Omit secretRef.namespace"), false
		default:
			return nil, containerTaskArgError(fmt.Sprintf("secretRef has unsupported field %q", key), "secretRef takes only name"), false
		}
	}
	name, errResult, ok := containerTaskSecretName(values, "secretRef")
	if !ok {
		return nil, errResult, false
	}
	return &corev1alpha1.SecretReference{Name: name}, "", true
}

// containerTaskSecretName reads a required Secret name at field.
func containerTaskSecretName(values map[string]any, field string) (string, string, bool) {
	name, _ := values[nameField].(string)
	if name == "" {
		return "", containerTaskArgError(field+".name is required", "Name a Secret in the Task's namespace"), false
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", containerTaskArgError(fmt.Sprintf("%s.name %q is not a valid Secret name: %s", field, name, strings.Join(errs, "; ")), "Use the Secret's exact name"), false
	}
	return name, "", true
}

// parseContainerTaskEnvValueFrom accepts only a Secret key. Other sources read
// Pod metadata the worker already has, or objects the tool cannot authorize.
func parseContainerTaskEnvValueFrom(raw any, field string) (*corev1.EnvVarSource, string, bool) {
	values, ok := raw.(map[string]any)
	if !ok {
		return nil, containerTaskArgError(field+" must be an object", `Pass {"secretKeyRef":{"name":"my-secret","key":"token"}}`), false
	}
	for key := range values {
		if key != secretKeyRefField {
			return nil, containerTaskArgError(fmt.Sprintf("%s.%s is not supported: only %s.secretKeyRef is accepted", field, key, field), "Use a literal value or a Secret key"), false
		}
	}
	ref, ok := values[secretKeyRefField].(map[string]any)
	if !ok {
		return nil, containerTaskArgError(field+".secretKeyRef is required", `Pass {"secretKeyRef":{"name":"my-secret","key":"token"}}`), false
	}
	refField := field + ".secretKeyRef"
	for key := range ref {
		if key != nameField && key != keyField {
			return nil, containerTaskArgError(fmt.Sprintf("%s has unsupported field %q", refField, key), "secretKeyRef takes only name and key"), false
		}
	}
	name, errResult, ok := containerTaskSecretName(ref, refField)
	if !ok {
		return nil, errResult, false
	}
	key, _ := ref[keyField].(string)
	if key == "" {
		return nil, containerTaskArgError(refField+".key is required", "Name the key inside the Secret"), false
	}
	if errs := validation.IsConfigMapKey(key); len(errs) > 0 {
		return nil, containerTaskArgError(fmt.Sprintf("%s.key %q is not a valid Secret key: %s", refField, key, strings.Join(errs, "; ")), "Use the key's exact name"), false
	}
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name},
		Key:                  key,
	}}, "", true
}

func containerTaskArgError(message, suggestion string) string {
	result, _ := ChatToolErrorResult(invalidArgumentsErrorType, message, suggestion)
	return result
}

func parseContainerTaskEnvArg(a map[string]any) ([]corev1.EnvVar, string, bool) {
	raw, ok := a[envField]
	if !ok || raw == nil {
		return nil, "", true
	}
	items, ok := raw.([]any)
	if !ok {
		r := containerTaskArgError("env must be an array of {name, value} objects", `Pass env as [{"name":"MODE","value":"smoke"}]`)
		return nil, r, false
	}
	if len(items) > maxContainerTaskEnvVars {
		r := containerTaskArgError(fmt.Sprintf("env has %d entries; at most %d are allowed", len(items), maxContainerTaskEnvVars), "Pass fewer variables, or bake static configuration into the image")
		return nil, r, false
	}
	env := make([]corev1.EnvVar, 0, len(items))
	seen := make(map[string]bool, len(items))
	total := 0
	for i, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			r := containerTaskArgError(fmt.Sprintf("env[%d] must be an object with name and value", i), `Pass each entry as {"name":"MODE","value":"smoke"}`)
			return nil, r, false
		}
		for key := range entry {
			if key != nameField && key != valueField && key != valueFromField {
				r := containerTaskArgError(fmt.Sprintf("env[%d] has unsupported field %q", i, key), "Each env entry takes name and either value or valueFrom")
				return nil, r, false
			}
		}
		name, _ := entry[nameField].(string)
		if name == "" {
			r := containerTaskArgError(fmt.Sprintf("env[%d].name is required", i), "Give every env entry a name")
			return nil, r, false
		}
		if len(name) > maxContainerTaskEnvNameBytes {
			r := containerTaskArgError(fmt.Sprintf("env[%d].name is %d bytes; at most %d are allowed", i, len(name), maxContainerTaskEnvNameBytes), "Use a shorter variable name")
			return nil, r, false
		}
		if errs := validation.IsEnvVarName(name); len(errs) > 0 {
			r := containerTaskArgError(fmt.Sprintf("env[%d].name %q is not a valid environment variable name: %s", i, name, strings.Join(errs, "; ")), "Use letters, digits, '_', '-', or '.', not starting with a digit")
			return nil, r, false
		}
		if reservedContainerTaskEnvName(name) {
			result, _ := ChatToolErrorResult("reserved_env_name", fmt.Sprintf("env[%d].name %q is reserved for the controller", i, name), "Rename the variable: names starting with ORKA_ or OTEL_EXPORTER_OTLP, and OTEL_RESOURCE_ATTRIBUTES, are set by Orka")
			return nil, result, false
		}
		if seen[name] {
			r := containerTaskArgError(fmt.Sprintf("env[%d].name %q is duplicated", i, name), "Set each variable once")
			return nil, r, false
		}
		seen[name] = true
		if total += len(name); total > maxContainerTaskEnvTotalBytes {
			r := containerTaskArgError(fmt.Sprintf("env is more than %d bytes in total", maxContainerTaskEnvTotalBytes), "Pass less through env: read large inputs from a file in the image or workspace")
			return nil, r, false
		}
		if source, ok := entry[valueFromField]; ok && source != nil {
			if _, hasValue := entry[valueField]; hasValue {
				r := containerTaskArgError(fmt.Sprintf("env[%d] sets both value and valueFrom", i), "Set either a literal value or valueFrom.secretKeyRef")
				return nil, r, false
			}
			valueFrom, errResult, ok := parseContainerTaskEnvValueFrom(source, fmt.Sprintf("env[%d].valueFrom", i))
			if !ok {
				return nil, errResult, false
			}
			env = append(env, corev1.EnvVar{Name: name, ValueFrom: valueFrom})
			continue
		}
		value := chatGetStringArg(entry, valueField)
		if len(value) > maxContainerTaskEnvValueBytes {
			r := containerTaskArgError(fmt.Sprintf("env[%d].value is %d bytes; at most %d are allowed", i, len(value), maxContainerTaskEnvValueBytes), "Write large inputs to a file in the image or workspace instead")
			return nil, r, false
		}
		if strings.ContainsRune(value, 0) {
			r := containerTaskArgError(fmt.Sprintf("env[%d].value contains a NUL byte", i), "Environment values cannot hold NUL; encode binary data, for example with base64")
			return nil, r, false
		}
		if total += len(value); total > maxContainerTaskEnvTotalBytes {
			r := containerTaskArgError(fmt.Sprintf("env is more than %d bytes in total", maxContainerTaskEnvTotalBytes), "Pass less through env: read large inputs from a file in the image or workspace")
			return nil, r, false
		}
		env = append(env, corev1.EnvVar{Name: name, Value: value})
	}
	if len(env) == 0 {
		return nil, "", true
	}
	return env, "", true
}

// reservedContainerTaskEnvName reports names the job builder sets or reserves:
// Orka's own ORKA_* settings and the OpenTelemetry exporter configuration.
func reservedContainerTaskEnvName(name string) bool {
	return strings.HasPrefix(name, "ORKA_") || strings.HasPrefix(name, "OTEL_EXPORTER_OTLP") || name == "OTEL_RESOURCE_ATTRIBUTES"
}

// validateContainerTaskResourceShape fails on an unknown key under resources
// and on an empty or null quantity, both of which the shared parser drops. Any
// resources replace the Task's defaults, so a dropped entry would silently run
// without the limit the caller meant to set.
func validateContainerTaskResourceShape(a map[string]any) (string, bool) {
	resources, ok := a[resourcesField].(map[string]any)
	if !ok {
		return "", true
	}
	for key := range resources {
		if key != resourceRequestsField && key != resourceLimitsField {
			r := containerTaskArgError(fmt.Sprintf("resources has unsupported field %q", key), "resources takes requests and limits")
			return r, false
		}
	}
	for _, key := range []string{resourceRequestsField, resourceLimitsField} {
		values, ok := resources[key].(map[string]any)
		if !ok {
			continue
		}
		for name, raw := range values {
			if text, isString := raw.(string); raw == nil || (isString && strings.TrimSpace(text) == "") {
				r := containerTaskArgError(fmt.Sprintf("resources.%s.%s must be a Kubernetes quantity", key, name), "Omit the field instead of leaving it empty")
				return r, false
			}
		}
	}
	return "", true
}

func parseContainerTaskResourcesArg(a map[string]any) (corev1.ResourceRequirements, string, bool) {
	if errResult, ok := validateContainerTaskResourceShape(a); !ok {
		return corev1.ResourceRequirements{}, errResult, false
	}
	resources, errResult, ok := parseResourceRequirementsArg(a)
	if !ok {
		return resources, errResult, false
	}
	for _, list := range []struct {
		key    string
		values corev1.ResourceList
	}{{resourceRequestsField, resources.Requests}, {resourceLimitsField, resources.Limits}} {
		for name, quantity := range list.values {
			if quantity.Sign() < 0 {
				r := containerTaskArgError(fmt.Sprintf("resources.%s.%s must not be negative", list.key, name), "Use a positive Kubernetes quantity such as 500m or 1Gi")
				return resources, r, false
			}
		}
	}
	for name, request := range resources.Requests {
		if limit, ok := resources.Limits[name]; ok && request.Cmp(limit) > 0 {
			r := containerTaskArgError(fmt.Sprintf("resources.requests.%s (%s) exceeds resources.limits.%s (%s)", name, request.String(), name, limit.String()), "Set each request at or below its limit")
			return resources, r, false
		}
	}
	return resources, "", true
}

func parseContainerTaskRetryPolicyArg(a map[string]any) (*corev1alpha1.RetryPolicy, string, bool) {
	raw, ok := a[retryPolicyField]
	if !ok || raw == nil {
		return nil, "", true
	}
	values, ok := raw.(map[string]any)
	if !ok {
		r := containerTaskArgError("retryPolicy must be an object", `Pass retryPolicy as {"maxRetries":2,"initialDelay":"30s"}`)
		return nil, r, false
	}
	for key := range values {
		switch key {
		case maxRetriesField, backoffMultiplierField, initialDelayField:
		default:
			r := containerTaskArgError(fmt.Sprintf("retryPolicy has unsupported field %q", key), "retryPolicy takes maxRetries, backoffMultiplier, and initialDelay")
			return nil, r, false
		}
	}
	policy := &corev1alpha1.RetryPolicy{}
	if value, ok := values[maxRetriesField]; ok && value != nil {
		retries, err := chatParseIntArg(value)
		if err != nil || retries < 0 || retries > maxContainerTaskRetries {
			r := containerTaskArgError(fmt.Sprintf("retryPolicy.maxRetries must be a whole number from 0 to %d", maxContainerTaskRetries), "Retry only idempotent commands, and only a few times")
			return nil, r, false
		}
		policy.MaxRetries = int32(retries)
	}
	if value, ok := values[backoffMultiplierField]; ok && value != nil {
		multiplier, isNumber := value.(float64)
		if !isNumber || math.IsNaN(multiplier) || multiplier < minContainerTaskBackoffMultiplier || multiplier > maxContainerTaskBackoffMultiplier {
			r := containerTaskArgError(fmt.Sprintf("retryPolicy.backoffMultiplier must be a number from %d to %d", minContainerTaskBackoffMultiplier, maxContainerTaskBackoffMultiplier), "Omit it to use 2")
			return nil, r, false
		}
		policy.BackoffMultiplier = multiplier
	}
	if text := strings.TrimSpace(chatGetStringArg(values, initialDelayField)); text != "" {
		delay, err := time.ParseDuration(text)
		if err != nil || delay <= 0 || delay > maxContainerTaskInitialDelay {
			r := containerTaskArgError(fmt.Sprintf("retryPolicy.initialDelay must be a positive Go duration up to %s", maxContainerTaskInitialDelay), "Use a duration such as 10s or 1m")
			return nil, r, false
		}
		policy.InitialDelay = &metav1.Duration{Duration: delay}
	}
	return policy, "", true
}

func parseContainerTaskExecutionArg(a map[string]any, allowed []string) (*corev1alpha1.ExecutionSpec, string, bool) {
	raw, ok := a[executionField]
	if !ok || raw == nil {
		return nil, "", true
	}
	values, ok := raw.(map[string]any)
	if !ok {
		r := containerTaskArgError("execution must be an object", `Pass execution as {"runtimeClassName":"gvisor"}`)
		return nil, r, false
	}
	for key := range values {
		if key != runtimeClassNameField {
			r := containerTaskArgError(fmt.Sprintf("execution.%s is not supported: only execution.runtimeClassName can be set", key), "Node placement is configured by the operator")
			return nil, r, false
		}
	}
	name := strings.TrimSpace(chatGetStringArg(values, runtimeClassNameField))
	if name == "" {
		return nil, "", true
	}
	if !slices.Contains(allowed, name) {
		message := fmt.Sprintf("execution.runtimeClassName %q is not allowed for container tasks", name)
		suggestion := "Omit execution.runtimeClassName: the operator has not allowed any runtime classes for container tasks"
		if len(allowed) > 0 {
			suggestion = "Use one of the allowed runtime classes: " + strings.Join(allowed, ", ")
		}
		result, _ := ChatToolErrorResult("runtime_class_not_allowed", message, suggestion)
		return nil, result, false
	}
	return &corev1alpha1.ExecutionSpec{RuntimeClassName: name}, "", true
}
