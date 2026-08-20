/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace/statusrules"
)

// defaultWorkspaceSlotName mirrors the API default for
// Task.spec.execution.workspace.workspaceSlot.
const defaultWorkspaceSlotName = "default"

// ACPRuntimeWorkspaceBinding is the resolved, canonical execution-workspace
// binding for one ACP RuntimePool. It carries no provider-native identifiers
// and no secrets; it is frozen into the immutable execution snapshot and
// recomputed exactly during snapshot verification.
type ACPRuntimeWorkspaceBinding struct {
	Provider      corev1alpha1.WorkspaceProvider
	ReusePolicy   corev1alpha1.WorkspaceReusePolicy
	CleanupPolicy corev1alpha1.WorkspaceCleanupPolicy
	WorkspaceSlot string
	// SessionKey scopes the physical workspace to one logical RuntimeSession:
	// the continued session reference under reusePolicy session, or the exact
	// Task UID otherwise.
	SessionKey string
	// TemplateNamespace and TemplateName reference the operator-owned Substrate
	// infrastructure ActorTemplate. They are set exactly when Provider is
	// substrate: agent-sandbox workloads run only controller-rendered templates.
	TemplateNamespace string
	TemplateName      string
	// BindingDigest is the canonical digest over the fields above. It is part
	// of the RuntimePool identity.
	BindingDigest string
}

// resolveACPWorkspaceBinding distills Task.spec.execution.workspace into the
// canonical ACP workspace binding. It is pure: the same frozen Task inputs and
// default provider always produce the same binding, so snapshot verification
// can recompute it exactly. Unsupported provider capabilities fail closed here,
// before any workspace or RuntimePool demand exists.
//
//nolint:gocyclo // Every unsupported-capability rejection is audited in one place.
func resolveACPWorkspaceBinding(
	task *corev1alpha1.Task,
	defaultProvider corev1alpha1.WorkspaceProvider,
) (*ACPRuntimeWorkspaceBinding, error) {
	if task == nil || task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil {
		return nil, nil
	}
	ws := task.Spec.Execution.Workspace
	if ws.ClassRef != nil {
		return nil, fmt.Errorf("execution workspace classRef requires controller-first Task workspace integration")
	}
	if !ws.Enabled {
		return nil, nil
	}
	if task.UID == "" {
		return nil, fmt.Errorf("task UID is required for an execution workspace binding")
	}
	provider := resolveWorkspaceProvider(ws, defaultProvider)
	templateNamespace := ""
	templateName := ""
	switch provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox:
		if ws.TemplateRef != nil {
			return nil, fmt.Errorf(
				"execution workspace templateRef selects a legacy worker-path sandbox template; ACP RuntimeSessions run only in controller-rendered sandbox templates, so templateRef must be omitted",
			)
		}
	case corev1alpha1.WorkspaceProviderSubstrate:
		// Substrate templates carry operator-owned infrastructure (worker pool
		// placement, runsc build, snapshot location) that the controller cannot
		// invent, so an explicit base template reference is required. The
		// controller renders its own derived runtime template from it; the
		// referenced template's containers never execute ACP work.
		if ws.TemplateRef == nil || strings.TrimSpace(ws.TemplateRef.Name) == "" {
			return nil, fmt.Errorf("execution workspace provider substrate requires templateRef.name naming the operator-owned infrastructure ActorTemplate")
		}
		templateName = strings.TrimSpace(ws.TemplateRef.Name)
		templateNamespace = strings.TrimSpace(ws.TemplateRef.Namespace)
		if templateNamespace == "" {
			templateNamespace = task.Namespace
		}
	default:
		return nil, fmt.Errorf(
			"execution workspace provider %q does not support ACP RuntimeSessions; there is no fallback execution path",
			provider,
		)
	}
	if ws.PoolRef != nil || ws.Boot || ws.Snapshot != nil || ws.Hibernation != nil {
		return nil, fmt.Errorf("execution workspace boot, poolRef, snapshot, and hibernation options are not supported for ACP RuntimeSessions")
	}
	if ws.OnDetach != "" {
		return nil, fmt.Errorf("execution workspace onDetach is not supported for ACP RuntimeSessions yet")
	}
	reuse := ws.ReusePolicy
	if reuse == "" {
		reuse = corev1alpha1.WorkspaceReusePolicyNone
	}
	cleanup := ws.CleanupPolicy
	if cleanup == "" {
		cleanup = corev1alpha1.WorkspaceCleanupPolicyDelete
	}
	if cleanup != corev1alpha1.WorkspaceCleanupPolicyDelete {
		return nil, fmt.Errorf(
			"execution workspace cleanupPolicy %q is not supported for ACP RuntimeSessions; the sandbox workspace is always deleted after authenticated drain",
			cleanup,
		)
	}
	slot := strings.TrimSpace(ws.WorkspaceSlot)
	if slot == "" {
		slot = defaultWorkspaceSlotName
	}
	sessionKey := ""
	switch reuse {
	case corev1alpha1.WorkspaceReusePolicyNone:
		sessionKey = "task:" + string(task.UID)
	case corev1alpha1.WorkspaceReusePolicySession:
		// Sessions and RuntimePools are both namespace-local, so the session
		// name alone scopes the workspace within the Task namespace.
		if task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) == "" {
			return nil, fmt.Errorf("execution workspace reusePolicy session requires spec.sessionRef.name")
		}
		sessionKey = "session:" + strings.TrimSpace(task.Spec.SessionRef.Name)
	default:
		return nil, fmt.Errorf("execution workspace reusePolicy %q is not supported", reuse)
	}
	binding := &ACPRuntimeWorkspaceBinding{
		Provider: provider, ReusePolicy: reuse, CleanupPolicy: cleanup,
		WorkspaceSlot: slot, SessionKey: sessionKey,
		TemplateNamespace: templateNamespace, TemplateName: templateName,
	}
	digest, err := acpWorkspaceBindingDigest(binding)
	if err != nil {
		return nil, err
	}
	binding.BindingDigest = digest
	return binding, nil
}

// acpWorkspaceBindingDigest canonically digests the binding identity fields.
func acpWorkspaceBindingDigest(binding *ACPRuntimeWorkspaceBinding) (string, error) {
	if binding == nil {
		return "", fmt.Errorf("execution workspace binding is required")
	}
	return acpDomainDigest("execution-workspace-binding", map[string]string{
		"provider":          string(binding.Provider),
		"reusePolicy":       string(binding.ReusePolicy),
		"cleanupPolicy":     string(binding.CleanupPolicy),
		"workspaceSlot":     binding.WorkspaceSlot,
		"sessionKey":        binding.SessionKey,
		"templateNamespace": binding.TemplateNamespace,
		"templateName":      binding.TemplateName,
	})
}

// applyACPWorkspaceBindingToPlan folds a resolved workspace binding into the
// RuntimePool identity so workspace-backed sessions never share a plain pool.
func applyACPWorkspaceBindingToPlan(plan ACPRuntimePlan, binding *ACPRuntimeWorkspaceBinding) (ACPRuntimePlan, error) {
	if binding == nil {
		return plan, nil
	}
	if strings.TrimSpace(binding.BindingDigest) == "" {
		return ACPRuntimePlan{}, fmt.Errorf("execution workspace binding digest is required")
	}
	identity, err := acpDomainDigest("runtime-pool-identity", map[string]string{
		"profileDigest": string(plan.Digest), "runtimeImage": plan.Image,
		"workspaceBindingDigest": binding.BindingDigest,
	})
	if err != nil {
		return ACPRuntimePlan{}, err
	}
	workspace := *binding
	plan.PoolName = acpWorkspaceRuntimePoolName(plan.Profile.ProviderKind, harnessv2.ProfileDigest(identity))
	plan.Workspace = &workspace
	return plan, nil
}

func acpWorkspaceRuntimePoolName(providerKind string, digest harnessv2.ProfileDigest) string {
	hexDigest := strings.TrimPrefix(string(digest), "sha256:")
	return fmt.Sprintf("acp-ws-%s-%s", providerKind, hexDigest[:16])
}

// projectACPExecutionWorkspaceStatus advances the provider-neutral Task
// workspace projection alongside the ACP execution state machine:
// Pending -> Ready once the prompt is running in the workspace-backed pool,
// and Pending/Ready -> Released once the attempt settles terminally. It never
// writes provider-native identifiers and never overrides a Failed projection.
func (r *TaskReconciler) projectACPExecutionWorkspaceStatus(ctx context.Context, task *corev1alpha1.Task) error {
	current := task.Status.ExecutionWorkspace
	if task.Spec.Type != corev1alpha1.TaskTypeAgent || current == nil || task.Status.Execution == nil || !taskManagedByACP(task) {
		return nil
	}
	update := statusrules.Update{
		Provider:      current.Provider,
		ReusePolicy:   current.ReusePolicy,
		CleanupPolicy: current.CleanupPolicy,
		Reused:        current.Reused,
	}
	switch {
	case taskExecutionStateTerminal(task.Status.Execution.State):
		if current.Phase != corev1alpha1.ExecutionWorkspacePhasePending && current.Phase != corev1alpha1.ExecutionWorkspacePhaseReady {
			return nil
		}
		update.Phase = corev1alpha1.ExecutionWorkspacePhaseReleased
		update.Reason = corev1alpha1.ExecutionWorkspaceReasonReleased
		update.Message = "RuntimeSession attempt settled; this Task's workspace demand is released"
	case task.Status.Execution.State == corev1alpha1.TaskExecutionStateRunning ||
		task.Status.Execution.State == corev1alpha1.TaskExecutionStateSettling:
		if current.Phase != corev1alpha1.ExecutionWorkspacePhasePending {
			return nil
		}
		update.Phase = corev1alpha1.ExecutionWorkspacePhaseReady
		update.Reason = corev1alpha1.ExecutionWorkspaceReasonReady
		update.Message = "RuntimeSession is executing in a workspace-provider-backed RuntimePool"
	default:
		return nil
	}
	next := update.Status()
	statusrules.PreserveReadyTelemetry(next, current)
	base := task.DeepCopy()
	task.Status.ExecutionWorkspace = next
	return r.Status().Patch(ctx, task, client.MergeFrom(base))
}

// validateACPWorkspaceBindingValues re-verifies a frozen snapshot workspace
// binding without consulting live cluster state.
func validateACPWorkspaceBindingValues(binding *ACPRuntimeWorkspaceBinding) error {
	if binding == nil {
		return nil
	}
	switch binding.Provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox:
		if binding.TemplateNamespace != "" || binding.TemplateName != "" {
			return fmt.Errorf("frozen agent-sandbox execution workspace binding must not carry a template reference")
		}
	case corev1alpha1.WorkspaceProviderSubstrate:
		if strings.TrimSpace(binding.TemplateNamespace) == "" || strings.TrimSpace(binding.TemplateName) == "" {
			return fmt.Errorf("frozen substrate execution workspace binding is missing the infrastructure template reference")
		}
	default:
		return fmt.Errorf("frozen execution workspace provider %q is not supported", binding.Provider)
	}
	if binding.CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyDelete {
		return fmt.Errorf("frozen execution workspace cleanup policy %q is not supported", binding.CleanupPolicy)
	}
	if binding.ReusePolicy != corev1alpha1.WorkspaceReusePolicyNone && binding.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
		return fmt.Errorf("frozen execution workspace reuse policy %q is not supported", binding.ReusePolicy)
	}
	if strings.TrimSpace(binding.WorkspaceSlot) == "" || strings.TrimSpace(binding.SessionKey) == "" {
		return fmt.Errorf("frozen execution workspace binding is incomplete")
	}
	digest, err := acpWorkspaceBindingDigest(binding)
	if err != nil {
		return err
	}
	if digest != binding.BindingDigest {
		return fmt.Errorf("frozen execution workspace binding digest does not match its canonical identity")
	}
	return nil
}
