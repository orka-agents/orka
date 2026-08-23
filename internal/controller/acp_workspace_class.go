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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

// acpWorkspaceProviderControllerName is the reserved adapter identity for the
// in-tree ACP RuntimePool execution-workspace adapter. Only
// ExecutionWorkspaceProvider objects carrying this controllerName may back
// class-selected ACP RuntimeSessions.
const acpWorkspaceProviderControllerName = "acp.workspace.orka.ai/runtime-pool"

// ACPWorkspaceClassDeletionPolicy freezes the class deletion dispositions that
// finalization must honor independently for each retained-data category.
type ACPWorkspaceClassDeletionPolicy struct {
	ProviderResources string
	PersistentVolumes string
	Checkpoints       string
}

// ACPWorkspaceClassBinding is the frozen controller-first class identity for
// one ACP execution-workspace binding. Every field is immutable snapshot
// input: live class or provider drift is rejected against these exact values
// instead of silently rebinding.
type ACPWorkspaceClassBinding struct {
	Name               string
	UID                string
	Generation         int64
	ProfileHash        string
	ProviderName       string
	ProviderUID        string
	ProviderGeneration int64
	// ProviderConfigUID pins the exact cluster-scoped RuntimeProviderConfig
	// that selected the physical backend: recreating the immutable config
	// under the same name must read as drift, never as a silent backend swap.
	ProviderConfigUID string
	// EffectiveOnDetach is the validated detach action for this Task: the
	// Task-requested value when the class allows it, otherwise the class
	// default. Only Delete is currently executable for ACP RuntimeSessions.
	EffectiveOnDetach string
	// DefaultOnDetach and AllowedOnDetach freeze the class lifecycle policy in
	// class order so the materialized ExecutionWorkspace carries the exact
	// class lifecycle and never drifts from it.
	DefaultOnDetach string
	AllowedOnDetach []string
	DetachTimeout   string
	IdleTimeout     string
	MaxLifetime     string
	DeletionPolicy  ACPWorkspaceClassDeletionPolicy
}

// Lifecycle rebuilds the exact class lifecycle frozen into this binding. The
// result must match the live class spec byte for byte, or the workspace core
// controller rejects the materialized workspace as policy drift.
func (c *ACPWorkspaceClassBinding) Lifecycle() (workspacev1alpha1.ExecutionWorkspaceLifecycle, error) {
	detachTimeout, err := time.ParseDuration(c.DetachTimeout)
	if err != nil {
		return workspacev1alpha1.ExecutionWorkspaceLifecycle{}, fmt.Errorf("frozen class detach timeout is invalid: %w", err)
	}
	lifecycle := workspacev1alpha1.ExecutionWorkspaceLifecycle{
		DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetach(c.DefaultOnDetach),
		DetachTimeout:   metav1.Duration{Duration: detachTimeout},
		DeletionPolicy: workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
			ProviderResources: workspacev1alpha1.WorkspaceDeletionAction(c.DeletionPolicy.ProviderResources),
			PersistentVolumes: workspacev1alpha1.WorkspaceDeletionAction(c.DeletionPolicy.PersistentVolumes),
			Checkpoints:       workspacev1alpha1.WorkspaceDeletionAction(c.DeletionPolicy.Checkpoints),
		},
	}
	for _, action := range c.AllowedOnDetach {
		lifecycle.AllowedOnDetach = append(lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetach(action))
	}
	if c.IdleTimeout != "" {
		idle, err := time.ParseDuration(c.IdleTimeout)
		if err != nil {
			return workspacev1alpha1.ExecutionWorkspaceLifecycle{}, fmt.Errorf("frozen class idle timeout is invalid: %w", err)
		}
		lifecycle.IdleTimeout = &metav1.Duration{Duration: idle}
	}
	if c.MaxLifetime != "" {
		maxLifetime, err := time.ParseDuration(c.MaxLifetime)
		if err != nil {
			return workspacev1alpha1.ExecutionWorkspaceLifecycle{}, fmt.Errorf("frozen class maximum lifetime is invalid: %w", err)
		}
		lifecycle.MaxLifetime = &metav1.Duration{Duration: maxLifetime}
	}
	return lifecycle, nil
}

// acpResolvedWorkspaceClass carries the live-resolved class data consumed by
// the pure binding resolver. Binding is frozen; the remaining fields validate
// the Task request against class policy without entering the snapshot.
type acpResolvedWorkspaceClass struct {
	Binding                    ACPWorkspaceClassBinding
	Backend                    corev1alpha1.WorkspaceProvider
	Mode                       workspacev1alpha1.ExecutionWorkspaceMode
	AllowedReuseScopes         []workspacev1alpha1.WorkspaceReuseScope
	AllowedOnDetach            []workspacev1alpha1.WorkspaceOnDetach
	DefaultOnDetach            workspacev1alpha1.WorkspaceOnDetach
	SubstrateTemplateNamespace string
	SubstrateTemplateName      string
}

func taskRequestsWorkspaceClass(task *corev1alpha1.Task) bool {
	return task != nil && task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil &&
		task.Spec.Execution.Workspace.ClassRef != nil
}

// resolveACPWorkspaceClass resolves and pins Task.spec.execution.workspace.classRef
// against the live ExecutionWorkspaceClass, its provider, and the adapter-owned
// parameter objects. Every mismatch fails closed before any workspace or
// RuntimePool demand exists. The `use` authorization for the class is enforced
// at admission by the workspace-class-use webhook and policy; this resolver
// re-verifies object identity and policy, not caller authority.
//
//nolint:gocyclo // Every class-path rejection is audited in one place.
func (r *TaskReconciler) resolveACPWorkspaceClass(
	ctx context.Context,
	task *corev1alpha1.Task,
) (*acpResolvedWorkspaceClass, error) {
	if !taskRequestsWorkspaceClass(task) {
		return nil, nil
	}
	if !r.WorkspaceProviderAPIEnabled {
		return nil, fmt.Errorf("execution workspace classRef requires the workspace provider API")
	}
	className := strings.TrimSpace(task.Spec.Execution.Workspace.ClassRef.Name)
	if className == "" {
		return nil, fmt.Errorf("execution workspace classRef.name is required")
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: className}, class); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("execution workspace class %q does not exist in namespace %q", className, task.Namespace)
		}
		return nil, fmt.Errorf("resolve execution workspace class: %w", err)
	}
	if !class.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("execution workspace class %q is deleting", className)
	}
	if class.Spec.Mode != workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		return nil, fmt.Errorf("execution workspace class %q mode %q is not supported for ACP RuntimeSessions; only Interactive classes may back Task attachment", className, class.Spec.Mode)
	}
	if class.Spec.PoolRef != nil || class.Spec.ProviderRef == nil || class.Spec.ParametersRef == nil {
		return nil, fmt.Errorf("execution workspace class %q must use direct providerRef provisioning; pooled provisioning is not supported for ACP RuntimeSessions", className)
	}
	ready := apimeta.FindStatusCondition(class.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	if class.Status.ObservedGeneration != class.Generation ||
		ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != class.Generation {
		return nil, fmt.Errorf("execution workspace class %q is not ready at its current generation", className)
	}
	if strings.TrimSpace(class.Status.ProfileHash) == "" || class.Status.ProviderRef == nil ||
		strings.TrimSpace(class.Status.ProviderRef.Name) == "" {
		return nil, fmt.Errorf("execution workspace class %q has no pinned profile hash or resolved provider", className)
	}
	if class.Spec.ParametersRef.Group != acpworkspacev1alpha1.GroupVersion.Group ||
		class.Spec.ParametersRef.Kind != "RuntimeWorkspaceProfile" {
		return nil, fmt.Errorf(
			"execution workspace class %q parametersRef %s/%s is not an ACP RuntimeWorkspaceProfile",
			className, class.Spec.ParametersRef.Group, class.Spec.ParametersRef.Kind,
		)
	}

	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := reader.Get(ctx, types.NamespacedName{Name: class.Status.ProviderRef.Name}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("execution workspace provider %q does not exist", class.Status.ProviderRef.Name)
		}
		return nil, fmt.Errorf("resolve execution workspace provider: %w", err)
	}
	if !provider.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("execution workspace provider %q is deleting", provider.Name)
	}
	if provider.Spec.ControllerName != acpWorkspaceProviderControllerName {
		return nil, fmt.Errorf(
			"execution workspace provider %q controllerName %q is not the ACP RuntimePool adapter; ACP RuntimeSessions have no fallback execution path",
			provider.Name, provider.Spec.ControllerName,
		)
	}
	if provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderActive {
		return nil, fmt.Errorf("execution workspace provider %q is %s and rejects new ACP workspaces", provider.Name, provider.Spec.LifecycleState)
	}
	providerReady := workspaceprovider.FindCondition(provider.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
	if provider.Status.ObservedGeneration != provider.Generation ||
		providerReady == nil || providerReady.Status != metav1.ConditionTrue ||
		providerReady.ObservedGeneration != provider.Generation {
		return nil, fmt.Errorf("execution workspace provider %q is not ready at its current generation", provider.Name)
	}
	if provider.Spec.ParametersRef.Group != acpworkspacev1alpha1.GroupVersion.Group ||
		provider.Spec.ParametersRef.Kind != "RuntimeProviderConfig" {
		return nil, fmt.Errorf(
			"execution workspace provider %q parametersRef %s/%s is not an ACP RuntimeProviderConfig",
			provider.Name, provider.Spec.ParametersRef.Group, provider.Spec.ParametersRef.Kind,
		)
	}
	config := &acpworkspacev1alpha1.RuntimeProviderConfig{}
	if err := reader.Get(ctx, types.NamespacedName{Name: provider.Spec.ParametersRef.Name}, config); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("ACP runtime provider config %q does not exist", provider.Spec.ParametersRef.Name)
		}
		return nil, fmt.Errorf("resolve ACP runtime provider config: %w", err)
	}
	if !config.DeletionTimestamp.IsZero() {
		// The operator has withdrawn this configuration; freezing its identity
		// into a new Task would dispatch against it before the provider
		// advertisement heartbeat notices the deletion.
		return nil, fmt.Errorf("ACP runtime provider config %q is being deleted", config.Name)
	}
	var backend corev1alpha1.WorkspaceProvider
	switch config.Spec.Backend {
	case acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox:
		backend = corev1alpha1.WorkspaceProviderAgentSandbox
	case acpworkspacev1alpha1.RuntimeProviderBackendSubstrate:
		backend = corev1alpha1.WorkspaceProviderSubstrate
	default:
		return nil, fmt.Errorf("ACP runtime provider config %q backend %q is not supported", config.Name, config.Spec.Backend)
	}

	profile, profileSpec, err := r.resolveACPWorkspaceProfile(ctx, reader, class)
	if err != nil {
		return nil, err
	}
	resolvedHash, err := acpWorkspaceClassProfileHash(class, provider, profile)
	if err != nil {
		return nil, fmt.Errorf("recompute execution workspace class profile hash: %w", err)
	}
	if resolvedHash != class.Status.ProfileHash {
		return nil, fmt.Errorf(
			"execution workspace class %q resolved profile drifted from its pinned hash; create a new class",
			className,
		)
	}

	resolved := &acpResolvedWorkspaceClass{
		Binding: ACPWorkspaceClassBinding{
			Name:               class.Name,
			UID:                string(class.UID),
			Generation:         class.Generation,
			ProfileHash:        class.Status.ProfileHash,
			ProviderName:       provider.Name,
			ProviderUID:        string(provider.UID),
			ProviderGeneration: provider.Generation,
			ProviderConfigUID:  string(config.UID),
			DefaultOnDetach:    string(class.Spec.Lifecycle.DefaultOnDetach),
			AllowedOnDetach:    onDetachActionsToStrings(class.Spec.Lifecycle.AllowedOnDetach),
			DetachTimeout:      class.Spec.Lifecycle.DetachTimeout.Duration.String(),
			DeletionPolicy: ACPWorkspaceClassDeletionPolicy{
				ProviderResources: string(class.Spec.Lifecycle.DeletionPolicy.ProviderResources),
				PersistentVolumes: string(class.Spec.Lifecycle.DeletionPolicy.PersistentVolumes),
				Checkpoints:       string(class.Spec.Lifecycle.DeletionPolicy.Checkpoints),
			},
		},
		Backend:            backend,
		Mode:               class.Spec.Mode,
		AllowedReuseScopes: append([]workspacev1alpha1.WorkspaceReuseScope(nil), class.Spec.AllowedReuseScopes...),
		AllowedOnDetach:    append([]workspacev1alpha1.WorkspaceOnDetach(nil), class.Spec.Lifecycle.AllowedOnDetach...),
		DefaultOnDetach:    class.Spec.Lifecycle.DefaultOnDetach,
	}
	if class.Spec.Lifecycle.IdleTimeout != nil {
		resolved.Binding.IdleTimeout = class.Spec.Lifecycle.IdleTimeout.Duration.String()
	}
	if class.Spec.Lifecycle.MaxLifetime != nil {
		resolved.Binding.MaxLifetime = class.Spec.Lifecycle.MaxLifetime.Duration.String()
	}
	// Retained provider resources, volumes, and checkpoints require the
	// bounded-retention machinery; until it exists every category must delete.
	for category, action := range map[string]workspacev1alpha1.WorkspaceDeletionAction{
		"providerResources": class.Spec.Lifecycle.DeletionPolicy.ProviderResources,
		"persistentVolumes": class.Spec.Lifecycle.DeletionPolicy.PersistentVolumes,
		"checkpoints":       class.Spec.Lifecycle.DeletionPolicy.Checkpoints,
	} {
		if action != workspacev1alpha1.WorkspaceDeletionActionDelete {
			return nil, fmt.Errorf(
				"execution workspace class %q deletion policy retains %s; retained workspace data is not yet supported for ACP RuntimeSessions",
				className, category,
			)
		}
	}

	switch backend {
	case corev1alpha1.WorkspaceProviderSubstrate:
		if profileSpec.Substrate == nil || strings.TrimSpace(profileSpec.Substrate.TemplateRef.Name) == "" {
			return nil, fmt.Errorf(
				"ACP runtime workspace profile %q must name the operator-owned Substrate infrastructure ActorTemplate for backend substrate",
				class.Spec.ParametersRef.Name,
			)
		}
		templateName := strings.TrimSpace(profileSpec.Substrate.TemplateRef.Name)
		templateNamespace := strings.TrimSpace(profileSpec.Substrate.TemplateRef.Namespace)
		if templateNamespace == "" {
			templateNamespace = class.Namespace
		}
		if err := validateSubstrateWorkspaceTemplateReference(templateNamespace, templateName); err != nil {
			return nil, err
		}
		resolved.SubstrateTemplateNamespace = templateNamespace
		resolved.SubstrateTemplateName = templateName
	case corev1alpha1.WorkspaceProviderAgentSandbox:
		if profileSpec.Substrate != nil {
			return nil, fmt.Errorf(
				"ACP runtime workspace profile %q sets substrate inputs, but provider %q backend is agent-sandbox",
				class.Spec.ParametersRef.Name, provider.Name,
			)
		}
	}
	return resolved, nil
}

// resolveACPWorkspaceProfile reads the class's RuntimeWorkspaceProfile both as
// the unstructured object hashed by the class controller and as the typed spec
// consumed by the resolver, so hash recomputation matches the pinned profile
// byte for byte.
func (r *TaskReconciler) resolveACPWorkspaceProfile(
	ctx context.Context,
	reader client.Reader,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (*unstructured.Unstructured, acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec, error) {
	profile := &unstructured.Unstructured{}
	profile.SetGroupVersionKind(acpworkspacev1alpha1.GroupVersion.WithKind("RuntimeWorkspaceProfile"))
	key := types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.ParametersRef.Name}
	if err := reader.Get(ctx, key, profile); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{}, fmt.Errorf(
				"ACP runtime workspace profile %q does not exist in namespace %q", class.Spec.ParametersRef.Name, class.Namespace,
			)
		}
		return nil, acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{}, fmt.Errorf("resolve ACP runtime workspace profile: %w", err)
	}
	if profile.GetDeletionTimestamp() != nil {
		return nil, acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{}, fmt.Errorf(
			"ACP runtime workspace profile %q is deleting", class.Spec.ParametersRef.Name,
		)
	}
	typed := &acpworkspacev1alpha1.RuntimeWorkspaceProfile{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(profile.Object, typed); err != nil {
		return nil, acpworkspacev1alpha1.RuntimeWorkspaceProfileSpec{}, fmt.Errorf("decode ACP runtime workspace profile: %w", err)
	}
	return profile, typed.Spec, nil
}

// acpWorkspaceClassProfileHash recomputes the class profile hash with the same
// canonical inputs the workspace class controller pins, detecting provider or
// parameter drift behind an unchanged class generation.
func acpWorkspaceClassProfileHash(
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	parameters *unstructured.Unstructured,
) (string, error) {
	requiredContracts := append([]string(nil), provider.Spec.RequiredContracts...)
	slices.Sort(requiredContracts)
	providerIdentity := struct {
		UID               types.UID                              `json:"uid"`
		ControllerName    string                                 `json:"controllerName"`
		ParametersRef     workspacev1alpha1.TypedObjectReference `json:"parametersRef"`
		RequiredContracts []string                               `json:"requiredContracts"`
	}{
		UID:               provider.UID,
		ControllerName:    provider.Spec.ControllerName,
		ParametersRef:     provider.Spec.ParametersRef,
		RequiredContracts: requiredContracts,
	}
	resolved := struct {
		APIVersion string    `json:"apiVersion"`
		Kind       string    `json:"kind"`
		UID        types.UID `json:"uid"`
		Generation int64     `json:"generation"`
		Spec       any       `json:"spec,omitempty"`
	}{
		APIVersion: parameters.GetAPIVersion(),
		Kind:       parameters.GetKind(),
		UID:        parameters.GetUID(),
		Generation: parameters.GetGeneration(),
		Spec:       parameters.Object["spec"],
	}
	return workspaceprovider.ClassProfileHash(class.Spec, providerIdentity, resolved)
}

func onDetachActionsToStrings(actions []workspacev1alpha1.WorkspaceOnDetach) []string {
	values := make([]string, 0, len(actions))
	for _, action := range actions {
		values = append(values, string(action))
	}
	return values
}

// effectiveACPWorkspaceOnDetach selects and validates the detach action for a
// class-backed Task request: the explicit Task value when the class allows it,
// otherwise the class default.
func effectiveACPWorkspaceOnDetach(
	requested corev1alpha1.WorkspaceOnDetachPolicy,
	resolved *acpResolvedWorkspaceClass,
) (workspacev1alpha1.WorkspaceOnDetach, error) {
	effective := resolved.DefaultOnDetach
	if requested != "" {
		effective = workspacev1alpha1.WorkspaceOnDetach(requested)
		allowed := slices.Contains(resolved.AllowedOnDetach, effective)
		if !allowed {
			return "", fmt.Errorf(
				"execution workspace onDetach %q is not allowed by class %q; allowed actions are %v",
				requested, resolved.Binding.Name, resolved.AllowedOnDetach,
			)
		}
	}
	if effective != workspacev1alpha1.WorkspaceOnDetachDelete {
		return "", fmt.Errorf(
			"execution workspace onDetach %q is not yet executable for ACP RuntimeSessions; data-only suspension arrives with the provider cold-resume implementations",
			effective,
		)
	}
	return effective, nil
}

// acpWorkspaceReuseScopeAllowed reports whether the class permits the reuse
// scope implied by the Task's reusePolicy.
func acpWorkspaceReuseScopeAllowed(reuse corev1alpha1.WorkspaceReusePolicy, resolved *acpResolvedWorkspaceClass) bool {
	scope := workspacev1alpha1.WorkspaceReuseScopeNone
	if reuse == corev1alpha1.WorkspaceReusePolicySession {
		scope = workspacev1alpha1.WorkspaceReuseScopeSession
	}
	return slices.Contains(resolved.AllowedReuseScopes, scope)
}

// validateACPWorkspaceClassBindingValues re-verifies a frozen class binding
// without consulting live cluster state.
func validateACPWorkspaceClassBindingValues(class *ACPWorkspaceClassBinding) error {
	if class == nil {
		return nil
	}
	if strings.TrimSpace(class.Name) == "" || strings.TrimSpace(class.UID) == "" || class.Generation < 1 {
		return fmt.Errorf("frozen execution workspace class binding is missing its immutable class identity")
	}
	if !strings.HasPrefix(class.ProfileHash, "sha256:") || len(class.ProfileHash) != len("sha256:")+64 {
		return fmt.Errorf("frozen execution workspace class binding carries an invalid profile hash")
	}
	if strings.TrimSpace(class.ProviderName) == "" || strings.TrimSpace(class.ProviderUID) == "" || class.ProviderGeneration < 1 {
		return fmt.Errorf("frozen execution workspace class binding is missing its immutable provider identity")
	}
	if class.EffectiveOnDetach != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
		return fmt.Errorf("frozen execution workspace class binding detach action %q is not executable", class.EffectiveOnDetach)
	}
	if class.DefaultOnDetach != string(workspacev1alpha1.WorkspaceOnDetachDelete) &&
		class.DefaultOnDetach != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
		return fmt.Errorf("frozen execution workspace class binding default detach action %q is invalid", class.DefaultOnDetach)
	}
	if len(class.AllowedOnDetach) == 0 {
		return fmt.Errorf("frozen execution workspace class binding allows no detach actions")
	}
	for _, action := range class.AllowedOnDetach {
		if action != string(workspacev1alpha1.WorkspaceOnDetachDelete) &&
			action != string(workspacev1alpha1.WorkspaceOnDetachSuspend) {
			return fmt.Errorf("frozen execution workspace class binding allowed detach action %q is invalid", action)
		}
	}
	if _, err := time.ParseDuration(class.DetachTimeout); err != nil {
		return fmt.Errorf("frozen execution workspace class binding detach timeout is invalid: %w", err)
	}
	for _, timeout := range []string{class.IdleTimeout, class.MaxLifetime} {
		if timeout == "" {
			continue
		}
		if _, err := time.ParseDuration(timeout); err != nil {
			return fmt.Errorf("frozen execution workspace class binding lifetime policy is invalid: %w", err)
		}
	}
	for _, action := range []string{
		class.DeletionPolicy.ProviderResources,
		class.DeletionPolicy.PersistentVolumes,
		class.DeletionPolicy.Checkpoints,
	} {
		if action != string(workspacev1alpha1.WorkspaceDeletionActionDelete) &&
			action != string(workspacev1alpha1.WorkspaceDeletionActionRetain) {
			return fmt.Errorf("frozen execution workspace class binding deletion policy action %q is invalid", action)
		}
	}
	return nil
}
