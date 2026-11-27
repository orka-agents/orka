/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/publisher"
	"github.com/orka-agents/orka/internal/store"
	orkatracing "github.com/orka-agents/orka/internal/tracing"
	"go.opentelemetry.io/otel/trace"
)

const (
	delegateRequestOperationField  = "operation"
	delegateRequestParentTaskField = "parentTask"
)

type DelegateTaskSubjectTokenResolver func(context.Context, *corev1alpha1.Task, string) (string, error)

type BrokeredDelegateTaskTransactionExchangeConfig struct {
	TTS                 contexttoken.TTSConfig
	Exchanger           contexttoken.Exchanger
	SubjectTokenType    string
	ChildScope          string
	ResolveSubjectToken DelegateTaskSubjectTokenResolver
}

type DelegateTaskTool struct {
	k8sClient                   client.Client
	brokeredTransactionExchange *BrokeredDelegateTaskTransactionExchangeConfig
}

type WorkspaceArgs struct {
	Intent                       string `json:"intent,omitempty"`
	GitRepo                      string `json:"gitRepo,omitempty"`
	Branch                       string `json:"branch,omitempty"`
	Ref                          string `json:"ref,omitempty"`
	ReadCredentialRef            string `json:"readCredentialRef,omitempty"`
	PublicationGitRepo           string `json:"publicationGitRepo,omitempty"`
	PublicationReadCredentialRef string `json:"publicationReadCredentialRef,omitempty"`
	PublicationCredentialRef     string `json:"publicationCredentialRef,omitempty"`
	ForgeCredentialRef           string `json:"forgeCredentialRef,omitempty"`
	PushBranch                   string `json:"pushBranch,omitempty"`
	PRBaseBranch                 string `json:"prBaseBranch,omitempty"`
	PRTitle                      string `json:"prTitle,omitempty"`
	PRBody                       string `json:"prBody,omitempty"`
	CreatePR                     bool   `json:"createPR,omitempty"`
}

func (w *WorkspaceArgs) UnmarshalJSON(data []byte) error {
	var wsMap map[string]any
	if err := json.Unmarshal(data, &wsMap); err != nil {
		return err
	}
	if wsErr := agentWorkspacePullRequestArgTypeError(wsMap); wsErr != nil {
		return wsErr
	}
	type workspaceArgs WorkspaceArgs
	return json.Unmarshal(data, (*workspaceArgs)(w))
}

type DelegateTaskArgs struct {
	Agent          string         `json:"agent"`
	Prompt         string         `json:"prompt"`
	Namespace      string         `json:"namespace,omitempty"`
	AgentNamespace string         `json:"agentNamespace,omitempty"`
	TaskNamespace  string         `json:"taskNamespace,omitempty"`
	Priority       *int32         `json:"priority,omitempty"`
	Timeout        string         `json:"timeout,omitempty"`
	Workspace      *WorkspaceArgs `json:"workspace,omitempty"`
	MaxTurns       *int32         `json:"maxTurns,omitempty"`
	AllowBash      *bool          `json:"allowBash,omitempty"`

	PriorTask string `json:"prior_task,omitempty"`

	Feedback string `json:"feedback,omitempty"`

	AutoRetry bool `json:"auto_retry,omitempty"`

	MaxRetries *int `json:"max_retries,omitempty"`
}

type DelegateTaskResult struct {
	TaskName      string `json:"taskName"`
	TaskUID       string `json:"taskUID,omitempty"`
	ParentTaskUID string `json:"parentTaskUID,omitempty"`
	Status        string `json:"status"`
}

func NewDelegateTaskTool(k8sClient client.Client) *DelegateTaskTool {
	return &DelegateTaskTool{
		k8sClient: k8sClient,
	}
}

func NewBrokeredDelegateTaskTool(
	k8sClient client.Client,
	config BrokeredDelegateTaskTransactionExchangeConfig,
) (*DelegateTaskTool, error) {
	if k8sClient == nil {
		return nil, fmt.Errorf("brokered delegate_task requires a Kubernetes client")
	}
	if config.TTS.Enabled() {
		if config.Exchanger == nil {
			return nil, fmt.Errorf("brokered delegate_task requires a transaction-token exchanger when TTS is enabled")
		}
		if config.ResolveSubjectToken == nil {
			return nil, fmt.Errorf("brokered delegate_task requires request-scoped subject-token resolution when TTS is enabled")
		}
	}
	copyConfig := config
	return &DelegateTaskTool{
		k8sClient:                   k8sClient,
		brokeredTransactionExchange: &copyConfig,
	}, nil
}

func RegisterBrokeredDelegateTaskTool(
	registry *Registry,
	k8sClient client.Client,
	config BrokeredDelegateTaskTransactionExchangeConfig,
) error {
	if registry == nil {
		return fmt.Errorf("brokered coordination tool registry is required")
	}
	tool, err := NewBrokeredDelegateTaskTool(k8sClient, config)
	if err != nil {
		return err
	}
	registry.Register(tool)
	return nil
}

func (t *DelegateTaskTool) Name() string {
	return delegateTaskToolName
}

func (t *DelegateTaskTool) Description() string {
	return "Delegate a task to another agent. Creates a child Task CR that will be picked up by the specified agent. Supports iterative workflows via prior_task (applies previous diff) and feedback (prepends review feedback to prompt)."
}

func (t *DelegateTaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"agent": {
				"type": "string",
				"description": "Name of the agent to delegate to"
			},
			"prompt": {
				"type": "string",
				"description": "The task prompt for the agent"
			},
			"namespace": {
				"type": "string",
				"description": "Legacy namespace for both child task and agent lookup. Prefer taskNamespace/agentNamespace for cross-namespace delegation."
			},
			"agentNamespace": {
				"type": "string",
				"description": "Namespace containing the target Agent. Defaults to taskNamespace/current namespace."
			},
			"taskNamespace": {
				"type": "string",
				"description": "Namespace where the child Task should be created. Defaults to the parent task namespace."
			},
			"priority": {
				"type": "integer",
				"description": "Priority 0-1000 (defaults to parent priority)"
			},
			"timeout": {
				"type": "string",
				"description": "Task timeout duration, e.g. \"20m\""
			},
			"workspace": {
				"type": "object",
				"description": "Git workspace configuration for agent runtime tasks",
				"properties": {
					"gitRepo": {
						"type": "string",
						"description": "Git repository URL as a credential-free HTTPS URL, e.g. https://github.com/owner/repo (GitHub SSH roots are converted automatically). Required for write intent."
					},
					"branch": {
						"type": "string",
						"description": "Git branch name (short name or refs/heads/... ref). Omit with ref to resolve and freeze the repository's advertised default branch."
					},
					"ref": {
						"type": "string",
						"description": "Exact source selector: a full commit SHA, refs/heads/... branch, refs/tags/... tag, or short ref name. Other refs/ namespaces are rejected."
					},
					"intent": {
						"type": "string",
						"enum": ["read", "write"],
						"description": "Workspace intent. Defaults to read; publication fields require write."
					},
					"readCredentialRef": {
						"type": "string",
						"description": "Optional Secret name for clone/read credentials. Omit to auto-discover a read credential when available. Requires gitRepo."
					},
					"publicationGitRepo": {
						"type": "string",
						"description": "Publication repository URL for write Tasks as a credential-free HTTPS URL."
					},
					"publicationReadCredentialRef": {
						"type": "string",
						"description": "Optional Secret name for target-repository preflight and verification credentials."
					},
					"publicationCredentialRef": {
						"type": "string",
						"description": "Secret name for target-repository write credentials. Required for write intent."
					},
					"forgeCredentialRef": {
						"type": "string",
						"description": "Secret name for forge API credentials used to reconcile pull requests. Required when createPR is true."
					},
					"pushBranch": {
						"type": "string",
						"description": "Publication branch name for a write Task."
					},
					"prBaseBranch": {
						"type": "string",
						"description": "Pull request base branch. Required when createPR is true."
					},
					"prTitle": {
						"type": "string",
						"maxLength": ` + strconv.Itoa(publisher.MaxPullRequestTitleLength) + `,
						"description": "` + workspacePRTitleDescription + `"
					},
					"prBody": {
						"type": "string",
						"maxLength": ` + strconv.Itoa(publisher.MaxPullRequestBodyLength) + `,
						"description": "` + workspacePRBodyDescription + `"
					},
					"createPR": {
						"type": "boolean",
						"description": "Reconcile a pull request after publication. Requires prBaseBranch and forgeCredentialRef."
					}
				}
			},
			"maxTurns": {
				"type": "integer",
				"description": "Maximum number of turns for the agent"
			},
			"allowBash": {
				"type": "` + jsonSchemaTypeBoolean + `",
				"description": "Whether to allow bash execution in the agent"
			},
			"prior_task": {
				"type": "string",
				"description": "Name of a previously completed task whose diff should be applied to the workspace before this task starts. Used for iterative workflows."
			},
			"feedback": {
				"type": "string",
				"description": "Review feedback to prepend to the task prompt. Used with prior_task for iterative code review workflows."
			},
			"auto_retry": {
				"type": "` + jsonSchemaTypeBoolean + `",
				"description": "Include structured retry metadata in failure reports. The coordinator decides whether to retry — wait_for_tasks does not auto-retry."
			},
			"max_retries": {
				"type": "integer",
				"description": "Maximum retry budget for coordinator reference (default: 2). Only used when auto_retry is true."
			}
		},
		"required": ["agent", "prompt"]
	}`)
}

type delegationContext struct {
	args         DelegateTaskArgs
	parentName   string
	currentDepth int
	namespace    string
	agentNS      string
	parentTask   *corev1alpha1.Task
	targetAgent  *corev1alpha1.Agent
	priority     *int32
}

func delegatedAgentAllowed(agentName, agentNamespace, taskNamespace, allowedAgents string) bool {
	agentName = strings.TrimSpace(agentName)
	agentNamespace = strings.TrimSpace(agentNamespace)
	taskNamespace = strings.TrimSpace(taskNamespace)
	for allowed := range strings.SplitSeq(allowedAgents, ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed == "" {
			continue
		}
		parts := strings.SplitN(allowed, "/", 2)
		if len(parts) == 2 {
			if strings.TrimSpace(parts[0]) == agentNamespace && strings.TrimSpace(parts[1]) == agentName {
				return true
			}
			continue
		}
		if allowed == agentName && (agentNamespace == "" || agentNamespace == taskNamespace) {
			return true
		}
	}
	return false
}

func namespacedDelegateAgent(namespace, name string) string {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

func validateDelegateNamespaceScope(toolCtx *ToolContext, resourceKind, namespace string) error {
	if toolCtx == nil {
		return nil
	}
	namespace = strings.TrimSpace(namespace)
	if watchNamespace := strings.TrimSpace(toolCtx.WatchNamespace); watchNamespace != "" && namespace != watchNamespace {
		return fmt.Errorf("%s namespace %q is outside watched namespace %q", resourceKind, namespace, watchNamespace)
	}
	if toolCtx.EnforceNamespaceIsolation {
		requestNamespace := strings.TrimSpace(toolCtx.Namespace)
		if namespace != requestNamespace {
			return fmt.Errorf("%s namespace %q is outside request namespace %q", resourceKind, namespace, requestNamespace)
		}
	}
	return nil
}

//nolint:gocyclo // Delegation keeps worker and authenticated broker policy checks auditable in one path.
func (t *DelegateTaskTool) parseDelegateArgs(ctx context.Context, args json.RawMessage) (*delegationContext, error) {
	parentName := os.Getenv(envOrkaTaskName)
	parentNamespace := os.Getenv(envOrkaTaskNamespace)
	depthStr := os.Getenv(envOrkaCoordinationDepth)
	allowedAgents := os.Getenv(envOrkaCoordinationAllowedAgents)
	maxDepthStr := os.Getenv(envOrkaCoordinationMaxDepth)
	toolCtx := GetToolContext(ctx)
	policyReader := toolPolicyReader(ctx, t.k8sClient)
	brokered := toolCtx != nil && toolCtx.Brokered
	if brokered {
		parentName = strings.TrimSpace(toolCtx.TaskID)
		parentNamespace = strings.TrimSpace(toolCtx.Namespace)
		depthStr = ""
		allowedAgents = ""
		maxDepthStr = ""
		if parentName == "" || parentNamespace == "" {
			return nil, fmt.Errorf("brokered delegation requires request-scoped task and namespace context")
		}
	}

	var delegateArgs DelegateTaskArgs
	if err := json.Unmarshal(args, &delegateArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if delegateArgs.Agent == "" {
		return nil, fmt.Errorf("agent is required")
	}
	if delegateArgs.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	taskNS := strings.TrimSpace(delegateArgs.TaskNamespace)
	agentNS := strings.TrimSpace(delegateArgs.AgentNamespace)
	legacyNS := strings.TrimSpace(delegateArgs.Namespace)
	if taskNS == "" {
		if legacyNS != "" && agentNS == "" {
			taskNS = legacyNS
		} else {
			taskNS = parentNamespace
		}
	}
	if taskNS == "" {
		taskNS = defaultNamespace
	}
	if agentNS == "" {
		if legacyNS != "" {
			agentNS = legacyNS
		} else {
			agentNS = taskNS
		}
	}

	if err := validateDelegateNamespaceScope(toolCtx, "child Task", taskNS); err != nil {
		return nil, err
	}
	if err := validateDelegateNamespaceScope(toolCtx, "target Agent", agentNS); err != nil {
		return nil, err
	}

	parentTask := &corev1alpha1.Task{}
	if parentName != "" {
		parentLookupNS := parentNamespace
		if parentLookupNS == "" {
			parentLookupNS = taskNS
		}
		if err := policyReader.Get(ctx, types.NamespacedName{Name: parentName, Namespace: parentLookupNS}, parentTask); err != nil {
			return nil, fmt.Errorf("failed to get parent task: %w", err)
		}
	}

	if brokered {
		authenticatedUID := strings.TrimSpace(toolCtx.TaskUID)
		if authenticatedUID == "" || string(parentTask.UID) != authenticatedUID {
			return nil, fmt.Errorf("brokered delegation parent Task identity changed")
		}
		depthStr = strings.TrimSpace(parentTask.Annotations[labels.AnnotationCoordinationDepth])
		if depthStr == "" {
			depthStr = "0"
		}
		if parentTask.Spec.AgentRef == nil || strings.TrimSpace(parentTask.Spec.AgentRef.Name) == "" {
			return nil, fmt.Errorf("brokered delegation requires a parent Task with agentRef")
		}
		parentAgentNamespace := parentTask.Namespace
		if value := strings.TrimSpace(parentTask.Spec.AgentRef.Namespace); value != "" {
			parentAgentNamespace = value
		}
		parentAgent := &corev1alpha1.Agent{}
		if err := policyReader.Get(ctx, types.NamespacedName{
			Name: parentTask.Spec.AgentRef.Name, Namespace: parentAgentNamespace,
		}, parentAgent); err != nil {
			return nil, fmt.Errorf("failed to get parent agent: %w", err)
		}
		coordination := parentAgent.Spec.Coordination
		if coordination == nil || !coordination.Enabled {
			return nil, fmt.Errorf("parent agent does not have coordination enabled")
		}
		if coordination.MaxDepth > 0 {
			maxDepthStr = strconv.FormatInt(int64(coordination.MaxDepth), 10)
		}
		allowed := make([]string, 0, len(coordination.AllowedAgents))
		for _, agent := range coordination.AllowedAgents {
			name := strings.TrimSpace(agent.Name)
			if name == "" {
				continue
			}
			if namespace := strings.TrimSpace(agent.Namespace); namespace != "" {
				allowed = append(allowed, namespacedDelegateAgent(namespace, name))
			} else {
				allowed = append(allowed, name)
			}
		}
		allowedAgents = strings.Join(allowed, ",")
	}

	currentDepth := 0
	if depthStr != "" {
		var err error
		currentDepth, err = strconv.Atoi(depthStr)
		if err != nil {
			return nil, fmt.Errorf("invalid coordination depth %q: %w", depthStr, err)
		}
	}

	maxDepth := 3
	if maxDepthStr != "" {
		var err error
		maxDepth, err = strconv.Atoi(maxDepthStr)
		if err != nil {
			return nil, fmt.Errorf("invalid max coordination depth %q: %w", maxDepthStr, err)
		}
	}

	if currentDepth+1 > maxDepth {
		return nil, fmt.Errorf("coordination depth exceeded: current depth %d, max depth %d", currentDepth, maxDepth)
	}

	if (brokered || allowedAgents != "") && !delegatedAgentAllowed(delegateArgs.Agent, agentNS, taskNS, allowedAgents) {
		return nil, fmt.Errorf("agent %q is not in the allowed agents list", namespacedDelegateAgent(agentNS, delegateArgs.Agent))
	}

	var priority *int32
	if delegateArgs.Priority != nil {
		priority = delegateArgs.Priority
	} else if parentTask.Spec.Priority != nil {
		priority = parentTask.Spec.Priority
	}

	targetAgent := &corev1alpha1.Agent{}
	if err := policyReader.Get(ctx, types.NamespacedName{
		Name: delegateArgs.Agent, Namespace: agentNS,
	}, targetAgent); err != nil {
		return nil, fmt.Errorf("failed to get agent %q: %w", namespacedDelegateAgent(agentNS, delegateArgs.Agent), err)
	}

	return &delegationContext{
		args:         delegateArgs,
		parentName:   parentName,
		currentDepth: currentDepth,
		namespace:    taskNS,
		agentNS:      agentNS,
		parentTask:   parentTask,
		targetAgent:  targetAgent,
		priority:     priority,
	}, nil
}

func (t *DelegateTaskTool) buildDelegatedTask(ctx context.Context, dc *delegationContext) (*corev1alpha1.Task, error) {
	taskType := corev1alpha1.TaskTypeAI
	if dc.targetAgent.Spec.Runtime != nil {
		taskType = corev1alpha1.TaskTypeAgent
	}

	childTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: dc.parentName + "-child-",
			Namespace:    dc.namespace,
			Labels: map[string]string{
				labels.LabelParentTask:     labels.SelectorValue(dc.parentName),
				labels.LabelCoordinator:    trueStr,
				labels.LabelDelegatedAgent: dc.args.Agent,
			},
			Annotations: map[string]string{
				labels.AnnotationCoordinationDepth: strconv.Itoa(dc.currentDepth + 1),
				labels.AnnotationParentTaskName:    dc.parentName,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: taskType,
			AgentRef: &corev1alpha1.AgentReference{
				Name: dc.args.Agent,
			},
			Prompt:   dc.args.Prompt,
			Priority: dc.priority,
		},
	}
	if dc.agentNS != "" && dc.agentNS != dc.namespace {
		childTask.Spec.AgentRef.Namespace = dc.agentNS
	}

	if dc.args.Timeout != "" {
		timeout, err := time.ParseDuration(dc.args.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout %q: %w", dc.args.Timeout, err)
		}
		if timeout > 0 {
			childTask.Spec.Timeout = &metav1.Duration{Duration: timeout}
		}
	}

	if dc.args.AutoRetry {
		childTask.Annotations[labels.AnnotationAutoRetry] = trueStr
		maxRetries := 2
		if dc.args.MaxRetries != nil && *dc.args.MaxRetries >= 0 {
			maxRetries = *dc.args.MaxRetries
		}
		childTask.Annotations[labels.AnnotationMaxRetries] = strconv.Itoa(maxRetries)
		childTask.Annotations[labels.AnnotationRetryCount] = "0"
		childTask.Annotations[labels.AnnotationOriginalPrompt] = dc.args.Prompt
	}

	if taskType == corev1alpha1.TaskTypeAgent {
		if err := t.applyAgentRuntimeConfig(ctx, childTask, dc); err != nil {
			return nil, err
		}
	}

	if dc.args.Feedback != "" {
		childTask.Spec.Prompt = fmt.Sprintf("FEEDBACK FROM REVIEW:\n%s\n\nTASK:\n%s", dc.args.Feedback, childTask.Spec.Prompt)
	}
	if taskType == corev1alpha1.TaskTypeAI {
		childTask.Spec.AI = &corev1alpha1.AISpec{Prompt: childTask.Spec.Prompt}
	}

	if dc.args.PriorTask != "" {
		t.applyPriorTaskConfig(ctx, childTask, dc)
	}

	inheritTaskProvenance(childTask, dc.parentTask)
	if toolCtx := GetToolContext(ctx); toolCtx != nil && toolCtx.Brokered {
		if dc.parentTask.UID == "" {
			return nil, fmt.Errorf("brokered delegation requires an immutable parent Task UID")
		}
		childTask.Annotations[labels.AnnotationParentTaskUID] = string(dc.parentTask.UID)
		if dc.parentTask.Namespace != "" {
			childTask.Annotations[labels.AnnotationParentTaskNamespace] = dc.parentTask.Namespace
		}
		if toolCtx.ExternalEffects != nil || strings.TrimSpace(toolCtx.SessionID) != "" || strings.TrimSpace(toolCtx.OperationID) != "" {
			effectID, err := brokeredDelegationEffectID(toolCtx)
			if err != nil {
				return nil, err
			}
			childTask.Annotations[labels.AnnotationDelegationEffectID] = effectID
		}
	}

	if dc.parentTask.UID != "" && dc.parentTask.Namespace == dc.namespace {
		isController := true
		childTask.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       taskKindString,
				Name:       dc.parentTask.Name,
				UID:        dc.parentTask.UID,
				Controller: &isController,
			},
		}
	}

	return childTask, nil
}

func (t *DelegateTaskTool) applyAgentRuntimeConfig(ctx context.Context, childTask *corev1alpha1.Task, dc *delegationContext) error {
	childTask.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}

	if dc.args.Workspace != nil {
		intent := corev1alpha1.WorkspaceIntent(strings.ToLower(strings.TrimSpace(dc.args.Workspace.Intent)))
		if intent == "" {
			intent = corev1alpha1.WorkspaceIntentRead
		}
		if intent != corev1alpha1.WorkspaceIntentRead && intent != corev1alpha1.WorkspaceIntentWrite {
			return fmt.Errorf("workspace intent must be read or write")
		}
		gitRepo, repoErr := canonicalAgentWorkspaceRepositoryArg("gitRepo", dc.args.Workspace.GitRepo)
		if repoErr != nil {
			return fmt.Errorf("%s (%s)", repoErr.Message, repoErr.Suggestion)
		}
		publicationGitRepo, repoErr := canonicalAgentWorkspaceRepositoryArg("publicationGitRepo", dc.args.Workspace.PublicationGitRepo)
		if repoErr != nil {
			return fmt.Errorf("%s (%s)", repoErr.Message, repoErr.Suggestion)
		}
		workspace := &corev1alpha1.WorkspaceConfig{
			Intent:             intent,
			GitRepo:            gitRepo,
			Branch:             dc.args.Workspace.Branch,
			Ref:                dc.args.Workspace.Ref,
			PublicationGitRepo: publicationGitRepo,
			PushBranch:         dc.args.Workspace.PushBranch,
			PRBaseBranch:       dc.args.Workspace.PRBaseBranch,
			PRTitle:            dc.args.Workspace.PRTitle,
			PRBody:             dc.args.Workspace.PRBody,
			CreatePR:           dc.args.Workspace.CreatePR,
		}
		if workspaceRequestsPublication(workspace) {
			workspace.Intent = corev1alpha1.WorkspaceIntentWrite
		}
		readCredential := strings.TrimSpace(dc.args.Workspace.ReadCredentialRef)
		publicationReadCredential := strings.TrimSpace(dc.args.Workspace.PublicationReadCredentialRef)
		publicationCredential := strings.TrimSpace(dc.args.Workspace.PublicationCredentialRef)
		forgeCredential := strings.TrimSpace(dc.args.Workspace.ForgeCredentialRef)
		if wsErr := agentWorkspaceArgError(workspace, readCredential, publicationReadCredential, publicationCredential, forgeCredential); wsErr != nil {
			return fmt.Errorf("%s (%s)", wsErr.Message, wsErr.Suggestion)
		}
		if strings.TrimSpace(workspace.GitRepo) != "" {
			readRef, err := resolveWorkspaceCredentialRef(ctx, t.k8sClient, dc.namespace, dc.targetAgent, readCredential)
			if err != nil {
				return err
			}
			workspace.ReadCredentialRef = readRef
		}
		if publicationReadCredential != "" {
			workspace.PublicationReadCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: publicationReadCredential}
		}
		if publicationCredential != "" {
			workspace.PublicationCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: publicationCredential}
		}
		if forgeCredential != "" {
			workspace.ForgeCredentialRef = &corev1alpha1.WorkspaceCredentialReference{Name: forgeCredential}
		}
		childTask.Spec.Workspace = workspace
	}

	if dc.args.MaxTurns != nil {
		childTask.Spec.AgentRuntime.MaxTurns = dc.args.MaxTurns
	}
	if dc.args.AllowBash != nil {
		childTask.Spec.AgentRuntime.AllowBash = dc.args.AllowBash
	}
	if err := materializeRuntimeRefAllowedTools(ctx, toolPolicyReader(ctx, t.k8sClient), childTask, dc.targetAgent); err != nil {
		return err
	}
	if dc.args.PriorTask != "" && dc.targetAgent.Spec.Runtime.RuntimeRef != nil &&
		strings.TrimSpace(dc.targetAgent.Spec.Runtime.RuntimeRef.Name) != "" {
		return fmt.Errorf("runtimeRef custom runtimes do not support priorTaskRef workspace handoff")
	}
	return nil
}

func (t *DelegateTaskTool) applyPriorTaskConfig(ctx context.Context, childTask *corev1alpha1.Task, dc *delegationContext) {
	childTask.Spec.PriorTaskRef = &corev1alpha1.PriorTaskReference{
		Name:      dc.args.PriorTask,
		Namespace: dc.namespace,
	}

	priorTask := &corev1alpha1.Task{}
	if err := t.k8sClient.Get(ctx, types.NamespacedName{Name: dc.args.PriorTask, Namespace: dc.namespace}, priorTask); err == nil {
		if dc.args.Workspace == nil {
			if priorWorkspace := taskWorkspace(priorTask); priorWorkspace != nil {
				childTask.Spec.Workspace = priorWorkspace.DeepCopy()
			}
		}

		iteration := 1
		if iterStr, ok := priorTask.Labels[labels.LabelIteration]; ok {
			if iter, err := strconv.Atoi(iterStr); err == nil {
				iteration = iter + 1
			}
		}
		childTask.Labels[labels.LabelIteration] = strconv.Itoa(iteration)

		if group, ok := priorTask.Labels[labels.LabelIterationGroup]; ok {
			childTask.Labels[labels.LabelIterationGroup] = group
		} else {
			childTask.Labels[labels.LabelIterationGroup] = string(priorTask.UID)
		}
	}
}

func (t *DelegateTaskTool) shouldPrepareChildTransactionToken(
	ctx context.Context,
	parentTask *corev1alpha1.Task,
) (bool, error) {
	toolCtx := GetToolContext(ctx)
	if toolCtx == nil || !toolCtx.Brokered {
		return shouldPrepareChildTransactionToken(parentTask)
	}
	if parentTask == nil || parentTask.Spec.Transaction == nil {
		return false, nil
	}
	if t.brokeredTransactionExchange == nil {
		return false, fmt.Errorf("brokered delegate_task transaction-token exchange configuration is not registered")
	}
	return t.brokeredTransactionExchange.TTS.Enabled(), nil
}

func (t *DelegateTaskTool) prepareChildTransactionToken(
	ctx context.Context,
	parentTask, childTask *corev1alpha1.Task,
	operation, agent string,
) error {
	toolCtx := GetToolContext(ctx)
	if toolCtx == nil || !toolCtx.Brokered {
		return prepareChildTransactionToken(ctx, t.k8sClient, parentTask, childTask, operation, agent)
	}
	return t.prepareBrokeredChildTransactionToken(ctx, parentTask, childTask, operation, agent)
}

func (t *DelegateTaskTool) prepareBrokeredChildTransactionToken(
	ctx context.Context,
	parentTask, childTask *corev1alpha1.Task,
	operation, agent string,
) error {
	config := t.brokeredTransactionExchange
	if config == nil {
		return fmt.Errorf("brokered delegate_task transaction-token exchange configuration is not registered")
	}
	if !config.TTS.Enabled() || parentTask == nil || parentTask.Spec.Transaction == nil {
		return nil
	}
	if parentTask.UID == "" {
		return fmt.Errorf("parent task UID is required for child transaction token exchange")
	}
	if err := requireSameNamespaceChildTokenExchange(parentTask, childTask); err != nil {
		return err
	}
	if config.Exchanger == nil {
		return fmt.Errorf("brokered delegate_task transaction-token exchanger is not configured")
	}
	if config.ResolveSubjectToken == nil {
		return fmt.Errorf("brokered delegate_task request-scoped subject-token resolver is not configured")
	}

	scope := strings.TrimSpace(config.ChildScope)
	if scope == "" {
		return fmt.Errorf("context-token child scope is required when brokered TTS is enabled for child task tokens")
	}
	if err := validateChildTransactionScope(parentTask, scope); err != nil {
		return err
	}
	subjectToken, err := config.ResolveSubjectToken(ctx, parentTask, config.TTS.TokenSource)
	if err != nil {
		return fmt.Errorf("resolving brokered child transaction subject token: %w", err)
	}
	if strings.TrimSpace(subjectToken) == "" {
		return fmt.Errorf("resolving brokered child transaction subject token: resolved token is empty")
	}
	subjectTokenType := strings.TrimSpace(config.SubjectTokenType)
	if subjectTokenType == "" {
		subjectTokenType = contexttoken.SubjectTokenTypeForSource(config.TTS.TokenSource)
	}

	requestDetails := map[string]any{
		delegateRequestOperationField:  operation,
		delegateRequestParentTaskField: parentTask.Name,
		namespaceField:                 childTask.Namespace,
	}
	if agent != "" {
		requestDetails["agent"] = agent
	}
	if parentTask.Spec.Transaction.ID != "" {
		requestDetails["txn"] = parentTask.Spec.Transaction.ID
	}
	token, err := config.Exchanger.Exchange(ctx, contexttoken.ExchangeRequest{
		SubjectToken:     subjectToken,
		SubjectTokenType: subjectTokenType,
		Scope:            scope,
		RequestedTTL:     config.TTS.ChildTokenTTL,
		RequestDetails:   requestDetails,
	})
	if err != nil {
		return fmt.Errorf("exchanging child transaction token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("exchanging child transaction token: TTS returned an empty token")
	}

	secretName, err := childTransactionTokenSecretName(parentTask.Name)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretName,
			Namespace:       childTask.Namespace,
			OwnerReferences: childTokenSecretOwnerReferences(parentTask, childTask),
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(parentTask.Name),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: parentTask.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}
	if err := t.k8sClient.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating child transaction token secret: %w", err)
	}
	stampChildTransactionScope(childTask, scope)
	if childTask.Annotations == nil {
		childTask.Annotations = map[string]string{}
	}
	childTask.Annotations[labels.AnnotationTransactionTokenSecret] = secretName
	return nil
}

func (t *DelegateTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	dc, err := t.parseDelegateArgs(ctx, args)
	if err != nil {
		return "", err
	}

	childTask, err := t.buildDelegatedTask(ctx, dc)
	if err != nil {
		return "", err
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(orkatracing.DelegateAttributes(dc.parentName, "")...)
	orkatracing.StampTaskTraceContext(ctx, childTask)
	if err := validateChildTaskAgainstParentTransaction(ctx, toolPolicyReader(ctx, t.k8sClient), dc.parentTask, childTask, dc.args.Agent); err != nil {
		return "", err
	}

	childTokenExchangeEnabled, err := t.shouldPrepareChildTransactionToken(ctx, dc.parentTask)
	if err != nil {
		return "", err
	}
	if childTokenExchangeEnabled {
		markChildTransactionTokenPending(childTask)
		if err := t.prepareChildTransactionToken(ctx, dc.parentTask, childTask, "delegateTask", dc.args.Agent); err != nil {
			return "", err
		}
	}

	parentReader := toolPolicyReader(ctx, t.k8sClient)
	if cancelled, err := delegationParentCancelled(ctx, parentReader, dc.parentTask); err != nil || cancelled {
		if childTokenExchangeEnabled {
			cleanupChildTransactionTokenSecret(ctx, t.k8sClient, childTask)
		}
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("parent task %s is being cancelled; it can delegate no more work", dc.parentName)
	}
	if err := t.k8sClient.Create(ctx, childTask); err != nil {
		if childTokenExchangeEnabled {
			cleanupChildTransactionTokenSecret(ctx, t.k8sClient, childTask)
		}
		if childCreateOutcomeUnknown(err) {
			go func() {
				_ = cancelChildrenAfterUnknownCreate(ctx, parentReader, t.k8sClient, dc.parentTask, childTask)
			}()
		}
		return "", fmt.Errorf("failed to create child task: %w", err)
	}
	span.SetAttributes(orkatracing.DelegateAttributes("", childTask.Name)...)
	sealTaskCreate(ctx, GetToolContext(ctx), childTask)

	if childTokenExchangeEnabled {
		if err := adoptChildTransactionTokenSecret(ctx, t.k8sClient, childTask); err != nil {
			cleanupChildTaskAfterTokenAdoptionFailure(ctx, t.k8sClient, childTask)
			return "", err
		}
		if err := patchPreparedChildTransactionToken(ctx, t.k8sClient, childTask); err != nil {
			cleanupChildTaskAfterTokenAdoptionFailure(ctx, t.k8sClient, childTask)
			return "", err
		}
	}

	if err := ensureLateChildCancelled(ctx, parentReader, t.k8sClient, dc.parentTask, childTask); err != nil {
		logf.FromContext(ctx).Error(err, "failed to cancel the child of a cancelled parent task", "child", childTask.Name)
	}

	result := DelegateTaskResult{
		TaskName:      childTask.Name,
		TaskUID:       string(childTask.UID),
		ParentTaskUID: string(dc.parentTask.UID),
		Status:        GitHubPullRequestStatusCreated,
	}

	output, err := json.Marshal(result)
	if err != nil {
		return "", err
	}

	return string(output), nil
}

func brokeredDelegationEffectID(toolCtx *ToolContext) (string, error) {
	if toolCtx == nil || !toolCtx.Brokered {
		return "", nil
	}
	identity := store.ExternalEffectIdentity{
		Kind:        "acp-mcp-tool",
		Namespace:   strings.TrimSpace(toolCtx.Namespace),
		AggregateID: strings.TrimSpace(toolCtx.SessionID),
		OperationID: strings.TrimSpace(toolCtx.OperationID),
	}
	id, err := identity.CanonicalID()
	if err != nil {
		return "", fmt.Errorf("bind brokered delegation to its durable effect: %w", err)
	}
	return id, nil
}

func markChildTransactionTokenPending(childTask *corev1alpha1.Task) {
	if childTask.Annotations == nil {
		childTask.Annotations = map[string]string{}
	}
	childTask.Annotations[labels.AnnotationTransactionTokenPending] = trueStr
	childTask.Annotations[labels.AnnotationTransactionTokenPendingSince] = time.Now().Format(time.RFC3339Nano)
}

func patchPreparedChildTransactionToken(ctx context.Context, k8sClient client.Client, childTask *corev1alpha1.Task) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				labels.AnnotationTransactionTokenPending:      nil,
				labels.AnnotationTransactionTokenPendingSince: nil,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("encoding child task transaction token metadata patch: %w", err)
	}
	target := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      childTask.Name,
			Namespace: childTask.Namespace,
		},
	}
	if err := k8sClient.Patch(ctx, target, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return fmt.Errorf("updating child task transaction token metadata: %w", err)
	}
	if childTask.Annotations != nil {
		delete(childTask.Annotations, labels.AnnotationTransactionTokenPending)
		delete(childTask.Annotations, labels.AnnotationTransactionTokenPendingSince)
	}
	return nil
}

var _ Tool = (*DelegateTaskTool)(nil)

const lateChildCheckTimeout = 30 * time.Second

var unknownCreateBackoff = wait.Backoff{Steps: 6, Duration: 250 * time.Millisecond, Factor: 2, Cap: 5 * time.Second}

func childCreateOutcomeUnknown(err error) bool {
	var status apierrors.APIStatus
	if !errors.As(err, &status) {
		return true
	}
	return apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err)
}

func cancelChildrenAfterUnknownCreate(ctx context.Context, reader client.Reader, writer client.Client, parent, child *corev1alpha1.Task) error {
	if parent == nil || parent.UID == "" {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lateChildCheckTimeout)
	defer cancel()
	effect := child.Annotations[labels.AnnotationDelegationEffectID]
	var last error
	err := wait.ExponentialBackoffWithContext(checkCtx, unknownCreateBackoff, func(ctx context.Context) (bool, error) {
		cancelled, err := delegationParentCancelled(ctx, reader, parent)
		if err != nil {
			last = err
			return false, nil
		}
		if !cancelled {
			return false, nil
		}
		var tasks corev1alpha1.TaskList
		if err := reader.List(ctx, &tasks, client.InNamespace(child.Namespace),
			client.MatchingLabels{labels.LabelParentTask: child.Labels[labels.LabelParentTask]}); err != nil {
			last = err
			return false, nil
		}
		found := false
		for i := range tasks.Items {
			candidate := &tasks.Items[i]
			if candidate.Annotations[labels.AnnotationParentTaskUID] != string(parent.UID) ||
				(effect != "" && candidate.Annotations[labels.AnnotationDelegationEffectID] != effect) {
				continue
			}
			if err := cancelChildOfCancelledParent(ctx, reader, writer, parent, candidate); err != nil {
				last = err
				continue
			}
			found = found || effect != ""
		}
		return found, nil
	})
	if err != nil && last != nil {
		return last
	}
	return nil
}

func ensureLateChildCancelled(ctx context.Context, reader client.Reader, writer client.Client, parent, child *corev1alpha1.Task) error {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lateChildCheckTimeout)
	defer cancel()
	backoff := wait.Backoff{Steps: 6, Duration: 250 * time.Millisecond, Factor: 2, Cap: 5 * time.Second}
	return retry.OnError(backoff, func(err error) bool { return checkCtx.Err() == nil }, func() error {
		return cancelChildOfCancelledParent(checkCtx, reader, writer, parent, child)
	})
}

func delegationParentCancelled(ctx context.Context, reader client.Reader, parent *corev1alpha1.Task) (bool, error) {
	if parent == nil || parent.Name == "" || parent.UID == "" {
		return false, nil
	}
	live := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: parent.Namespace, Name: parent.Name}, live); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("re-read the parent task: %w", err)
	}
	if live.UID != parent.UID || !live.DeletionTimestamp.IsZero() || live.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return true, nil
	}
	_, requested := live.Annotations[labels.AnnotationCancelRequested]
	return requested, nil
}

func cancelChildOfCancelledParent(ctx context.Context, reader client.Reader, writer client.Client, parent, child *corev1alpha1.Task) error {
	cancelled, err := delegationParentCancelled(ctx, reader, parent)
	if err != nil || !cancelled {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		live := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(child), live); err != nil {
			return client.IgnoreNotFound(err)
		}
		if live.UID != child.UID || !live.DeletionTimestamp.IsZero() {
			return nil
		}
		if _, requested := live.Annotations[labels.AnnotationCancelRequested]; requested {
			return nil
		}
		if phase := live.Status.Phase; phase != "" && phase != corev1alpha1.TaskPhasePending && phase != corev1alpha1.TaskPhaseRunning {
			return nil
		}
		base := live.DeepCopy()
		if live.Annotations == nil {
			live.Annotations = map[string]string{}
		}
		live.Annotations[labels.AnnotationCancelRequested] = "cancelled with its parent task " + parent.Name
		if err := writer.Patch(ctx, live, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	})
}
