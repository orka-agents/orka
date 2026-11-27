/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/agentidentity"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerpkg "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/outboundaccess"
)

const (
	conditionReasonReady = "Ready"
)

type AgentReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Mode             executionmode.Mode
	ToolboxPolicy    ACPToolboxPolicy
	Identity         agentidentity.Config
	APIReader        client.Reader
	IdentityVerifier AgentIdentityVerifier

	issuerBackoff struct {
		sync.Mutex
		until time.Time
	}
	verifications struct {
		sync.Mutex
		byAgent map[types.NamespacedName]agentIdentityVerification
	}
}

type AgentIdentityVerifier interface {
	Verify(context.Context, *corev1alpha1.Agent) error
}

const (
	conditionTypeIdentityVerified = outboundaccess.AgentConditionIdentityVerified
	reasonIdentityDeclared        = "Declared"
)

const (
	agentSoulConfigMapDependenciesField = "agent.soulConfigMapDependencies"

	agentReasoningEffortNone   = "none"
	agentReasoningEffortLow    = "low"
	agentReasoningEffortMedium = "medium"
	agentReasoningEffortHigh   = "high"
	agentReasoningEffortXHigh  = "xhigh"
	agentReasoningEffortMax    = "max"
)

// +kubebuilder:rbac:groups=core.orka.ai,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentidentities/entra,verbs=bind
// +kubebuilder:rbac:groups=core.orka.ai,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=list
// +kubebuilder:rbac:groups=core.orka.ai,resources=providers,verbs=get
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools,verbs=get
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	agent := &corev1alpha1.Agent{}
	if err := r.Get(ctx, req.NamespacedName, agent); err != nil {
		if errors.IsNotFound(err) {
			r.forgetAgentIdentityVerification(req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Agent", "agent", agent.Name)

	if r.Mode != "" && agentOmitsBuiltInContract(agent) {
		if err := executionmode.DefaultBuiltInAgentContract(agent, r.Mode); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Update(ctx, agent); err != nil {
			return ctrl.Result{}, fmt.Errorf("persist built-in Agent contract: %w", err)
		}
		logger.Info("Stamped built-in Agent with the namespace execution mode", "agent", agent.Name, "contract", *agent.Spec.Runtime.ContractVersion)
	}

	validationErr := r.validateAgent(ctx, agent)
	claim, claimErr := r.reconcileIdentityClaim(ctx, agent)
	if validationErr == nil && claimErr != nil {
		validationErr = claimErr
	}
	identity := r.verifyAgentIdentity(ctx, agent, validationErr)
	if validationErr == nil && identity.err != nil {
		validationErr = identity.err
	}

	activeTasks, err := r.countActiveTasks(ctx, agent)
	if err != nil {
		logger.Error(err, "Failed to count active tasks")
	}

	if result, deleted := r.checkTTLExpiry(ctx, agent, activeTasks); deleted {
		return result, nil
	}

	result, err := r.updateStatus(ctx, agent, activeTasks, validationErr, claim, identity)
	if err == nil && identity.retryAfter > 0 && (result.RequeueAfter == 0 || result.RequeueAfter > identity.retryAfter) {
		result.RequeueAfter = identity.retryAfter
	}
	if err == nil && identity.declared && (result.RequeueAfter == 0 || result.RequeueAfter > agentIdentityReverifyInterval) {
		result.RequeueAfter = agentIdentityReverifyInterval
	}
	if err == nil && claim != nil && claim.Releasing && (result.RequeueAfter == 0 || result.RequeueAfter > agentIdentityReleaseRecheck) {
		result.RequeueAfter = agentIdentityReleaseRecheck
	}
	return result, err
}

func (r *AgentReconciler) validateAgent(ctx context.Context, agent *corev1alpha1.Agent) error {
	if err := r.validateAgentIdentity(ctx, agent); err != nil {
		return err
	}
	if agent.Spec.Runtime != nil && agent.Spec.ProviderRef != nil {
		return fmt.Errorf("runtime and providerRef are mutually exclusive")
	}

	if err := validateAgentModelLimits(agent.Spec.Model); err != nil {
		return err
	}

	if agent.Spec.Runtime == nil {
		if agent.Spec.ProviderRef == nil && (agent.Spec.Model == nil || agent.Spec.Model.Provider == "") {
			return fmt.Errorf("either providerRef or model.provider must be specified")
		}
	} else {
		if agent.Spec.Runtime.RuntimeRef != nil {
			if strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) == "" {
				return fmt.Errorf("runtimeRef.name is required")
			}
			if err := validateRuntimeRefAgentTaskRestrictions(nil, agent); err != nil {
				return err
			}
		}
		if err := validateACPAgentModelControls(agent.Spec.Runtime, agent.Spec.Model); err != nil {
			return err
		}
		if err := validateAgentRuntimeReasoningEffort(agent.Spec.Runtime); err != nil {
			return err
		}
		if err := validateBuiltInACPAgentCredentialSecretRef(agent); err != nil {
			return err
		}
		if err := r.ToolboxPolicy.ValidateAgentToolboxes(withEffectiveBuiltInContract(agent, r.Mode)); err != nil {
			return err
		}
		if err := r.ToolboxPolicy.ValidateAgentSkills(withEffectiveBuiltInContract(agent, r.Mode)); err != nil {
			return err
		}
		if err := validateAgentRuntimeCeiling(nil, agent); err != nil {
			return fmt.Errorf("runtime defaults exceed the ceiling: %w", err)
		}
	}

	if err := ValidateOpenCodeAgentSpec(agent); err != nil {
		return err
	}

	if err := r.validateProviderRef(ctx, agent); err != nil {
		return err
	}
	if err := r.validateRuntimeRef(ctx, agent); err != nil {
		return err
	}
	if err := r.validateSecretRef(ctx, agent); err != nil {
		return err
	}
	if err := r.validateTools(ctx, agent); err != nil {
		return err
	}
	if err := r.validateSkills(ctx, agent); err != nil {
		return err
	}
	if err := r.validateSystemPromptConfigMap(ctx, agent); err != nil {
		return err
	}
	if err := r.validateDefaultInstructions(ctx, agent); err != nil {
		return err
	}
	return r.validateCoordination(ctx, agent)
}

func validateAgentModelLimits(model *corev1alpha1.ModelConfig) error {
	if model == nil {
		return nil
	}
	if model.MaxTokens != nil && *model.MaxTokens < 0 {
		return fmt.Errorf("spec.model.maxTokens must not be negative")
	}
	if model.ContextWindow != nil && *model.ContextWindow < 1 {
		return fmt.Errorf("spec.model.contextWindow must be positive")
	}
	return nil
}

func isBuiltInACPProviderRuntime(runtimeType corev1alpha1.AgentRuntimeType) bool {
	switch runtimeType {
	case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode:
		return true
	default:
		return false
	}
}

func validateBuiltInACPAgentCredentialSecretRef(agent *corev1alpha1.Agent) error {
	if agent == nil || agent.Spec.Runtime == nil || agent.Spec.SecretRef == nil ||
		!isBuiltInACPProviderRuntime(agent.Spec.Runtime.Type) {
		return nil
	}
	return fmt.Errorf("built-in ACP runtime %q does not support agent secretRef; provider credentials are controller-managed", agent.Spec.Runtime.Type)
}

func validateAgentRuntimeReasoningEffort(runtimeCfg *corev1alpha1.AgentCLIRuntime) error {
	if runtimeCfg == nil {
		return nil
	}
	effort := strings.ToLower(strings.TrimSpace(runtimeCfg.DefaultReasoningEffort))
	if effort == "" {
		return nil
	}
	switch runtimeCfg.Type {
	case corev1alpha1.AgentRuntimeCodex:
		switch effort {
		case agentReasoningEffortNone, agentReasoningEffortLow, agentReasoningEffortMedium, agentReasoningEffortHigh,
			agentReasoningEffortXHigh, agentReasoningEffortMax:
			return nil
		default:
			return fmt.Errorf("codex runtime defaultReasoningEffort %q must be none, low, medium, high, xhigh, or max", effort)
		}
	case corev1alpha1.AgentRuntimeClaude:
		switch effort {
		case agentReasoningEffortLow, agentReasoningEffortMedium, agentReasoningEffortHigh,
			agentReasoningEffortXHigh, agentReasoningEffortMax:
			return nil
		default:
			return fmt.Errorf("claude runtime defaultReasoningEffort %q must be low, medium, high, xhigh, or max", effort)
		}
	case corev1alpha1.AgentRuntimeCopilot:
		return fmt.Errorf("copilot runtime does not support defaultReasoningEffort")
	default:
		if runtimeCfg.RuntimeRef != nil {
			return fmt.Errorf("runtimeRef custom runtimes do not support defaultReasoningEffort")
		}
		return fmt.Errorf("runtime %q does not support defaultReasoningEffort", runtimeCfg.Type)
	}
}

func (r *AgentReconciler) validateProviderRef(ctx context.Context, agent *corev1alpha1.Agent) error {
	if agent.Spec.ProviderRef == nil {
		return nil
	}
	provider := &corev1alpha1.Provider{}
	ns := agent.Namespace
	if agent.Spec.ProviderRef.Namespace != "" {
		ns = agent.Spec.ProviderRef.Namespace
	}
	key := client.ObjectKey{Name: agent.Spec.ProviderRef.Name, Namespace: ns}
	if err := r.Get(ctx, key, provider); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("referenced provider %q not found", agent.Spec.ProviderRef.Name)
		}
		return fmt.Errorf("failed to get provider %q: %w", agent.Spec.ProviderRef.Name, err)
	}
	if !provider.Status.Ready {
		return fmt.Errorf("referenced provider %q is not ready", agent.Spec.ProviderRef.Name)
	}
	return nil
}

func (r *AgentReconciler) validateRuntimeRef(ctx context.Context, agent *corev1alpha1.Agent) error {
	if agent.Spec.Runtime == nil || agent.Spec.Runtime.RuntimeRef == nil {
		return nil
	}
	name := strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name)
	if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: agent.Namespace}, &corev1alpha1.AgentRuntime{}); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("referenced AgentRuntime %q not found", name)
		}
		return fmt.Errorf("failed to get AgentRuntime %q: %w", name, err)
	}
	return nil
}

func (r *AgentReconciler) agentsForAgentRuntime(ctx context.Context, obj client.Object) []reconcile.Request {
	agents := &corev1alpha1.AgentList{}
	if err := r.List(ctx, agents, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list Agents for AgentRuntime", "agentRuntime", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range agents.Items {
		agent := &agents.Items[i]
		if agent.Spec.Runtime != nil && agent.Spec.Runtime.RuntimeRef != nil &&
			strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) == obj.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
		}
	}
	return requests
}

func (r *AgentReconciler) agentsForProvider(ctx context.Context, obj client.Object) []reconcile.Request {
	agents := &corev1alpha1.AgentList{}
	if err := r.List(ctx, agents); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list Agents for Provider", "provider", obj.GetName())
		return nil
	}
	var requests []reconcile.Request
	for i := range agents.Items {
		agent := &agents.Items[i]
		ref := agent.Spec.ProviderRef
		if ref == nil || ref.Name != obj.GetName() {
			continue
		}
		namespace := ref.Namespace
		if namespace == "" {
			namespace = agent.Namespace
		}
		if namespace == obj.GetNamespace() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)})
		}
	}
	return requests
}

func (r *AgentReconciler) validateSecretRef(ctx context.Context, agent *corev1alpha1.Agent) error {
	if agent.Spec.SecretRef == nil {
		return nil
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: agent.Spec.SecretRef.Name, Namespace: agent.Namespace}
	if err := r.Get(ctx, key, secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("referenced secret %q not found", agent.Spec.SecretRef.Name)
		}
		return fmt.Errorf("failed to get secret %q: %w", agent.Spec.SecretRef.Name, err)
	}
	return nil
}

func (r *AgentReconciler) validateTools(ctx context.Context, agent *corev1alpha1.Agent) error {
	for _, toolRef := range agent.Spec.Tools {
		if toolRef.Enabled != nil && !*toolRef.Enabled {
			continue
		}
		tool := &corev1alpha1.Tool{}
		key := client.ObjectKey{Name: toolRef.Name, Namespace: agent.Namespace}
		if err := r.Get(ctx, key, tool); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to check tool %q: %w", toolRef.Name, err)
		}
	}
	return nil
}

func (r *AgentReconciler) validateSkills(ctx context.Context, agent *corev1alpha1.Agent) error {
	for _, skillRef := range agent.Spec.Skills {
		switch {
		case skillRef.Name != "":
			skill := &corev1alpha1.Skill{}
			skillName := skillRef.Name
			key := client.ObjectKey{Name: skillName, Namespace: agent.Namespace}
			if err := r.Get(ctx, key, skill); err != nil {
				if errors.IsNotFound(err) {
					return fmt.Errorf("referenced Skill %q not found", skillName)
				}
				return fmt.Errorf("failed to get Skill %q: %w", skillName, err)
			}
		case skillRef.ConfigMapRef != nil:
			cm := &corev1.ConfigMap{}
			key := client.ObjectKey{Name: skillRef.ConfigMapRef.Name, Namespace: agent.Namespace}
			if err := r.Get(ctx, key, cm); err != nil {
				if errors.IsNotFound(err) {
					return fmt.Errorf("referenced skill ConfigMap %q not found", skillRef.ConfigMapRef.Name)
				}
				return fmt.Errorf("failed to get skill ConfigMap %q: %w", skillRef.ConfigMapRef.Name, err)
			}
			if _, ok := cm.Data[skillRef.ConfigMapRef.Key]; !ok {
				return fmt.Errorf("key %q not found in skill ConfigMap %q", skillRef.ConfigMapRef.Key, skillRef.ConfigMapRef.Name)
			}
		default:
			return fmt.Errorf("skill reference must set either name or configMapRef")
		}
	}
	return nil
}

func (r *AgentReconciler) validateSystemPromptConfigMap(ctx context.Context, agent *corev1alpha1.Agent) error {
	if agent.Spec.SystemPrompt == nil || agent.Spec.SystemPrompt.ConfigMapRef == nil {
		return nil
	}
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{Name: agent.Spec.SystemPrompt.ConfigMapRef.Name, Namespace: agent.Namespace}
	if err := r.Get(ctx, key, cm); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("systemPrompt ConfigMap %q not found", agent.Spec.SystemPrompt.ConfigMapRef.Name)
		}
		return fmt.Errorf("failed to get systemPrompt ConfigMap %q: %w", agent.Spec.SystemPrompt.ConfigMapRef.Name, err)
	}
	if _, ok := cm.Data[agent.Spec.SystemPrompt.ConfigMapRef.Key]; !ok {
		return fmt.Errorf("key %q not found in systemPrompt ConfigMap %q", agent.Spec.SystemPrompt.ConfigMapRef.Key, agent.Spec.SystemPrompt.ConfigMapRef.Name)
	}
	return nil
}

func (r *AgentReconciler) validateCoordination(ctx context.Context, agent *corev1alpha1.Agent) error {
	if agent.Spec.Coordination == nil || !agent.Spec.Coordination.Enabled {
		return nil
	}
	for _, allowed := range agent.Spec.Coordination.AllowedAgents {
		ns := agent.Namespace
		if allowed.Namespace != "" {
			ns = allowed.Namespace
		}
		delegateAgent := &corev1alpha1.Agent{}
		key := client.ObjectKey{Name: allowed.Name, Namespace: ns}
		if err := r.Get(ctx, key, delegateAgent); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("coordination target agent %q not found", allowed.Name)
			}
			return fmt.Errorf("failed to check coordination target agent %q: %w", allowed.Name, err)
		}
	}
	for _, name := range agent.Spec.Coordination.DelegatedGatewayTools {
		tool := &corev1alpha1.Tool{}
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: agent.Namespace}, tool); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("delegated gateway tool %q not found", name)
			}
			return fmt.Errorf("failed to check delegated gateway tool %q: %w", name, err)
		}
		if tool.Spec.HTTP == nil || !tool.Spec.HTTP.GatewayTaskPass {
			return fmt.Errorf("delegated gateway tool %q is not a gateway task pass tool", name)
		}
	}
	return nil
}

func (r *AgentReconciler) countActiveTasks(ctx context.Context, agent *corev1alpha1.Agent) (int32, error) {
	taskList := &corev1alpha1.TaskList{}
	if err := r.List(ctx, taskList, client.InNamespace(agent.Namespace)); err != nil {
		return 0, err
	}

	var count int32
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if task.Spec.AgentRef != nil && task.Spec.AgentRef.Name == agent.Name {
			if !task.Status.Phase.IsTerminal() {
				count++
			}
		}
	}
	return count, nil
}

func (r *AgentReconciler) updateStatus(ctx context.Context, agent *corev1alpha1.Agent, activeTasks int32, validationErr error, claim *corev1alpha1.AgentIdentityClaim, identity agentIdentityState) (ctrl.Result, error) {
	now := metav1.Now()

	agent.Status.ActiveTasks = activeTasks
	agent.Status.Ready = validationErr == nil
	if activeTasks > 0 {
		agent.Status.LastUsed = &now
	}

	condition := metav1.Condition{
		Type:               conditionReasonReady,
		LastTransitionTime: now,
		ObservedGeneration: agent.Generation,
	}

	if validationErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = reasonValidationFailed
		condition.Message = validationErr.Error()
	} else {
		condition.Status = metav1.ConditionTrue
		condition.Reason = reasonValidationSucceeded
		condition.Message = "Agent configuration is valid"
	}

	meta.SetStatusCondition(&agent.Status.Conditions, condition)
	setAgentIdentityCondition(agent, now, identity)
	observedClaim := agent.Status.IdentityClaim.DeepCopy()
	agent.Status.IdentityClaim = claim.DeepCopy()

	activeCount := activeTasks
	lastUsed := agent.Status.LastUsed
	decidedUID, decidedGeneration := agent.UID, agent.Generation
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := r.Get(ctx, types.NamespacedName{Name: agent.Name, Namespace: agent.Namespace}, agent); err != nil {
			return err
		}
		if agent.UID != decidedUID || agent.Generation != decidedGeneration {
			return fmt.Errorf("agent changed while its status was being decided")
		}
		agent.Status.ActiveTasks = activeCount
		agent.Status.Ready = validationErr == nil
		if lastUsed != nil && (agent.Status.LastUsed == nil || agent.Status.LastUsed.Before(lastUsed)) {
			agent.Status.LastUsed = lastUsed
		}
		meta.SetStatusCondition(&agent.Status.Conditions, condition)
		setAgentIdentityCondition(agent, now, identity)
		if !apiequality.Semantic.DeepEqual(agent.Status.IdentityClaim, observedClaim) &&
			!apiequality.Semantic.DeepEqual(agent.Status.IdentityClaim, claim) {
			return fmt.Errorf("identity claim changed while it was being decided")
		}
		agent.Status.IdentityClaim = claim.DeepCopy()
		return r.Status().Update(ctx, agent)
	}); err != nil {
		return ctrl.Result{}, err
	}

	if agent.Spec.TTLAfterLastTask != nil {
		if activeTasks == 0 && agent.Status.LastUsed != nil {
			ttl := agent.Spec.TTLAfterLastTask.Duration
			elapsed := time.Since(agent.Status.LastUsed.Time)
			if remaining := ttl - elapsed; remaining > 0 {
				return ctrl.Result{RequeueAfter: remaining}, nil
			}
		} else if activeTasks > 0 {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

	if validationErr != nil {
		return ctrl.Result{RequeueAfter: agentValidationRecheckInterval}, nil
	}

	return ctrl.Result{}, nil
}

const agentValidationRecheckInterval = 30 * time.Second

func (r *AgentReconciler) checkTTLExpiry(ctx context.Context, agent *corev1alpha1.Agent, activeTasks int32) (ctrl.Result, bool) {
	if agent.Spec.TTLAfterLastTask == nil || agent.Spec.TTLAfterLastTask.Duration <= 0 {
		return ctrl.Result{}, false
	}
	if activeTasks > 0 {
		return ctrl.Result{}, false
	}

	if agent.Status.LastUsed == nil {
		agent.Status.LastUsed = &metav1.Time{Time: agent.CreationTimestamp.Time}
	}

	elapsed := time.Since(agent.Status.LastUsed.Time)
	ttl := agent.Spec.TTLAfterLastTask.Duration
	if elapsed < ttl {
		return ctrl.Result{}, false
	}

	logger := log.FromContext(ctx)
	logger.Info("TTL expired, deleting agent", "agent", agent.Name, "ttl", ttl, "lastUsed", agent.Status.LastUsed.Time)
	if err := r.Delete(ctx, agent); err != nil {
		logger.Error(err, "Failed to delete expired agent")
		return ctrl.Result{RequeueAfter: time.Second}, false //nolint:staticcheck
	}
	return ctrl.Result{}, true
}

func agentValidatesDefaultInstructions(agent *corev1alpha1.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.Spec.Runtime == nil {
		return true
	}
	if agent.Spec.Runtime.RuntimeRef != nil || agent.BuiltInContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return false
	}
	switch agent.Spec.Runtime.Type {
	case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode:
		return true
	default:
		return false
	}
}

func (r *AgentReconciler) validateDefaultInstructions(ctx context.Context, agent *corev1alpha1.Agent) error {
	if err := validateSoulRuntime(agent); err != nil {
		return err
	}
	soul, err := agentcontext.ResolveSoul(ctx, r.Client, agent)
	if err != nil {
		return err
	}
	if !agentValidatesDefaultInstructions(agent) || (soul == nil && agent.Spec.SystemPrompt == nil) {
		return nil
	}
	if agent.Spec.Runtime == nil && soul == nil {
		jobBuilder := &JobBuilder{Client: r.Client}
		task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI}}
		return jobBuilder.validateContainerDeliveredPromptSize(ctx, task, agent)
	}
	role, err := resolveACPSystemPrompt(ctx, r.Client, agent)
	if err != nil {
		return err
	}
	prompt := agentcontext.Compose(role, soul)
	if agent.Spec.Runtime == nil {
		jobBuilder := &JobBuilder{Client: r.Client}
		task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI}}
		return jobBuilder.validateContainerDeliveredPromptSize(ctx, task, nil, literalKubernetesPrompt(prompt))
	}
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	configuration, err := buildACPAgentSessionConfiguration(task, agent, prompt)
	if err != nil {
		return err
	}
	return validateACPProviderSystemPrompt(string(agent.Spec.Runtime.Type), configuration)
}

func agentSoulConfigMapDependencyIndex(object client.Object) []string {
	agent, ok := object.(*corev1alpha1.Agent)
	if !ok || agent == nil {
		return nil
	}
	names := make([]string, 0, 2)
	if agent.Spec.Soul != nil {
		if ref := agent.Spec.Soul.ConfigMapRef; ref != nil && ref.Name != "" {
			names = append(names, ref.Name)
		}
	}
	if agent.Spec.SystemPrompt != nil {
		if ref := agent.Spec.SystemPrompt.ConfigMapRef; ref != nil && ref.Name != "" && !slices.Contains(names, ref.Name) {
			names = append(names, ref.Name)
		}
	}
	return names
}

func (r *AgentReconciler) agentsForSoulConfigMap(ctx context.Context, object client.Object) []reconcile.Request {
	configMap, ok := object.(*corev1.ConfigMap)
	if !ok || configMap == nil || configMap.Namespace == "" || configMap.Name == "" {
		return nil
	}
	var agents corev1alpha1.AgentList
	if err := r.List(ctx, &agents, client.InNamespace(configMap.Namespace), client.MatchingFields{
		agentSoulConfigMapDependenciesField: configMap.Name,
	}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list Agents referencing ConfigMap",
			"namespace", configMap.Namespace, "configMap", configMap.Name)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(agents.Items))
	for i := range agents.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&agents.Items[i])})
	}
	return requests
}

func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1alpha1.Agent{}, agentSoulConfigMapDependenciesField, agentSoulConfigMapDependencyIndex); err != nil {
		return fmt.Errorf("index agent soul ConfigMap references: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Agent{}).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.agentsForSoulConfigMap)).
		Watches(&corev1alpha1.Task{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []reconcile.Request {
				task, ok := obj.(*corev1alpha1.Task)
				if !ok || task.Spec.AgentRef == nil {
					return nil
				}
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      task.Spec.AgentRef.Name,
						Namespace: task.Namespace,
					},
				}}
			})).
		Watches(&corev1alpha1.AgentRuntime{}, handler.EnqueueRequestsFromMapFunc(r.agentsForAgentRuntime),
			builder.WithPredicates(predicate.Funcs{UpdateFunc: func(event.UpdateEvent) bool { return false }})).
		Watches(&corev1alpha1.Provider{}, handler.EnqueueRequestsFromMapFunc(r.agentsForProvider)).
		Watches(&corev1alpha1.Agent{}, handler.EnqueueRequestsFromMapFunc(r.agentsSharingIdentity)).
		Named("agent").
		WithOptions(controllerpkg.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
