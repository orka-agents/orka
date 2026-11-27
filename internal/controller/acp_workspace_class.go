/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const acpWorkspaceSuspendDataOnly = "DataOnly"

const acpWorkspaceProviderControllerName = "acp.workspace.orka.ai/runtime-pool"

const acpWorkspaceControllerLabelValue = "runtime-pool.acp.workspace.orka.ai"

type ACPWorkspaceClassDeletionPolicy struct {
	ProviderResources string
	PersistentVolumes string
	Checkpoints       string
}

type ACPSandboxDurableVolume struct {
	StorageClassName string
	StorageClassUID  string
	AccessModes      []string
	Capacity         string
}

type ACPWorkspaceClassBinding struct {
	Name                     string
	UID                      string
	Generation               int64
	ProfileHash              string
	ProviderName             string
	ProviderUID              string
	ProviderGeneration       int64
	ControllerName           string
	LifecycleContractVersion string
	ProviderConfigRef        *workspacev1alpha1.TypedObjectReference
	ProviderConfigBinding    *workspacev1alpha1.ImmutableObjectBinding
	ParametersRef            *workspacev1alpha1.TypedObjectReference
	ParametersBinding        *workspacev1alpha1.ImmutableObjectBinding
	ProviderConfigUID        string
	EffectiveOnDetach        string
	SuspendMode              string
	SandboxVolume            *ACPSandboxDurableVolume
	MaxSuspendedWorkspaces   *int32
	SuspendAfterDetach       string
	DefaultOnDetach          string
	AllowedOnDetach          []string
	DetachTimeout            string
	IdleTimeout              string
	MaxLifetime              string
	DeletionPolicy           ACPWorkspaceClassDeletionPolicy
}

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

type retryableACPWorkspaceClassResolutionError struct{ err error }

func (e *retryableACPWorkspaceClassResolutionError) Error() string { return e.err.Error() }
func (e *retryableACPWorkspaceClassResolutionError) Unwrap() error { return e.err }

func markRetryableACPWorkspaceClassResolution(err error) error {
	if err == nil || isRetryableACPWorkspaceClassResolutionError(err) {
		return err
	}
	return &retryableACPWorkspaceClassResolutionError{err: err}
}

func isRetryableACPWorkspaceClassResolutionError(err error) bool {
	var retryable *retryableACPWorkspaceClassResolutionError
	return errors.As(err, &retryable)
}

func taskRequestsWorkspaceClass(task *corev1alpha1.Task) bool {
	return task != nil && task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil &&
		task.Spec.Execution.Workspace.ClassRef != nil
}

func mayResolveFrozenACPContinuation(
	task *corev1alpha1.Task,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	ready *metav1.Condition,
	frozenContinuation bool,
	requiredFeatures []workspacev1alpha1.ExecutionWorkspaceFeature,
) bool {
	if frozenContinuation && class.Status.ObservedGeneration == class.Generation && ready != nil &&
		ready.Status == metav1.ConditionFalse && ready.ObservedGeneration == class.Generation &&
		ready.Reason == string(workspacev1alpha1.ReasonProviderDraining) {
		return true
	}
	continuation := frozenContinuation && task != nil && task.Spec.Execution != nil &&
		task.Spec.Execution.Workspace != nil &&
		task.Spec.Execution.Workspace.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession &&
		class.Status.ObservedGeneration == class.Generation &&
		ready != nil && ready.Status == metav1.ConditionFalse &&
		ready.ObservedGeneration == class.Generation &&
		ready.Reason == reasonRequiredFeatures
	if !continuation {
		return false
	}
	if ready.Message == messageACPProfileInvalid {
		return true
	}
	return ready.Message == messageProviderFeaturesMissing &&
		!slices.Contains(requiredFeatures, workspacev1alpha1.WorkspaceFeatureSuspend)
}

func acpWorkspaceResolutionRequiredFeatures(
	task *corev1alpha1.Task,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	frozenContinuationReady bool,
) []workspacev1alpha1.ExecutionWorkspaceFeature {
	required := executionWorkspaceClassRequiredFeatures(class)
	if task != nil && task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil &&
		task.Spec.Execution.Workspace.RestoreFrom != nil && !slices.Contains(required, workspacev1alpha1.WorkspaceFeatureRestore) {
		required = append(required, workspacev1alpha1.WorkspaceFeatureRestore)
	}
	if !frozenContinuationReady || task == nil || task.Spec.Execution == nil ||
		task.Spec.Execution.Workspace == nil ||
		task.Spec.Execution.Workspace.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
		return required
	}
	effective := class.Spec.Lifecycle.DefaultOnDetach
	if requested := task.Spec.Execution.Workspace.OnDetach; requested != "" {
		effective = workspacev1alpha1.WorkspaceOnDetach(requested)
	}
	if effective != workspacev1alpha1.WorkspaceOnDetachDelete ||
		slices.Contains(class.Spec.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureSuspend) {
		return required
	}
	return slices.DeleteFunc(required, func(feature workspacev1alpha1.ExecutionWorkspaceFeature) bool {
		return feature == workspacev1alpha1.WorkspaceFeatureSuspend
	})
}

func (r *TaskReconciler) frozenACPContinuationExists(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	workspaceSessionUID string,
) (exists, readyWithoutResume bool, err error) {
	if !frozenACPContinuationRequestEligible(task, class, workspaceSessionUID) {
		return false, false, nil
	}
	reuse, slot, sessionUID, _, err := resolveACPWorkspaceSessionScope(task, workspaceSessionUID)
	if err != nil {
		return false, false, err
	}
	probe := &ACPRuntimeWorkspaceBinding{
		ReusePolicy:   reuse,
		WorkspaceSlot: slot,
		SessionUID:    sessionUID,
		Class:         &ACPWorkspaceClassBinding{UID: string(class.UID)},
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	workspaceName := acpClassWorkspaceName(task, probe)
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}
		return false, false, markRetryableACPWorkspaceClassResolution(fmt.Errorf(
			"resolve existing execution workspace for class continuation: %w", err,
		))
	}
	if !frozenACPContinuationWorkspaceMatches(workspace, task, class, sessionUID, slot) {
		return false, false, nil
	}
	poolName := strings.TrimSpace(workspace.Annotations[acpExecutionWorkspacePoolAnnotation])
	if poolName == "" {
		return false, false, nil
	}
	pool := &corev1alpha1.RuntimePool{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: poolName}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			if workspace.Annotations[acpWorkspaceResumedLineageAnnotation] == booleanTrueValue {
				return false, false, fmt.Errorf(
					"%w: resumed workspace %s is missing its linked RuntimePool %s",
					errACPWorkspaceBindingConflict, workspace.Name, poolName,
				)
			}
			return false, false, nil
		}
		return false, false, markRetryableACPWorkspaceClassResolution(fmt.Errorf(
			"resolve linked RuntimePool for class continuation: %w", err,
		))
	}
	if !frozenACPContinuationPoolMatches(pool, workspace) {
		return false, false, nil
	}
	return true, frozenACPContinuationReadyWithoutResume(workspace), nil
}

func frozenACPContinuationReadyWithoutResume(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceRevocationStartedAnnotation]) != "" {
		return false
	}
	return workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateReady ||
		workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateAttached
}

func frozenACPContinuationRequestEligible(
	task *corev1alpha1.Task,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	workspaceSessionUID string,
) bool {
	return task != nil && task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil &&
		task.Spec.Execution.Workspace.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession &&
		strings.TrimSpace(workspaceSessionUID) != "" && class != nil && class.UID != "" &&
		class.Status.ProviderRef != nil && strings.TrimSpace(class.Status.ProviderRef.Name) != "" &&
		strings.TrimSpace(class.Status.ProfileHash) != ""
}

func frozenACPContinuationWorkspaceMatches(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	sessionUID, slot string,
) bool {
	return workspace != nil && workspace.UID != "" && workspace.DeletionTimestamp.IsZero() &&
		workspace.Labels[workspacev1alpha1.ProviderControllerLabel] != "" &&
		workspace.Spec.Mode == workspacev1alpha1.ExecutionWorkspaceModeInteractive &&
		workspace.Spec.ClassBinding.Name == class.Name && workspace.Spec.ClassBinding.UID == class.UID &&
		workspace.Spec.ClassBinding.Generation == class.Generation &&
		workspace.Spec.ClassBinding.ProfileHash == class.Status.ProfileHash &&
		workspace.Spec.ProviderBinding.Name == class.Status.ProviderRef.Name &&
		workspace.Spec.SessionRef != nil && task.Spec.SessionRef != nil &&
		workspace.Spec.SessionRef.Name == strings.TrimSpace(task.Spec.SessionRef.Name) &&
		string(workspace.Spec.SessionRef.UID) == sessionUID && workspace.Spec.Slot == slot
}

func frozenACPContinuationPoolMatches(
	pool *corev1alpha1.RuntimePool,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) bool {
	if pool == nil || workspace == nil || !pool.DeletionTimestamp.IsZero() || pool.Spec.ExecutionWorkspace == nil {
		return false
	}
	if pool.Spec.ExecutionWorkspace.WorkspaceRef != nil {
		ref := pool.Spec.ExecutionWorkspace.WorkspaceRef
		return pool.Namespace == workspace.Namespace && ref.Name == workspace.Name && ref.UID == workspace.UID &&
			workspace.Spec.CoreAdmission != nil &&
			workspace.Spec.CoreAdmission.ProviderBinding == workspace.Spec.ProviderBinding &&
			workspace.Spec.CoreAdmission.ClassBinding == workspace.Spec.ClassBinding &&
			string(pool.Spec.ExecutionWorkspace.Provider) == workspace.Labels[workspacev1alpha1.ProviderControllerLabel] &&
			pool.Spec.ExecutionWorkspace.BindingDigest != ""
	}
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing ||
		pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting ||
		pool.Status.ObservedGeneration != pool.Generation {
		return false
	}
	backend := strings.TrimSpace(workspace.Annotations[acpWorkspaceBackendAnnotation])
	return pool.Labels[acpExecutionWorkspaceLinkLabel] == workspace.Name &&
		pool.Labels[acpRuntimeWorkspaceProviderLabel] == backend &&
		pool.Annotations[acpExecutionWorkspaceUIDAnnotation] == string(workspace.UID) &&
		string(pool.Spec.ExecutionWorkspace.Provider) == backend &&
		strings.TrimSpace(pool.Spec.ExecutionWorkspace.BindingDigest) != ""
}

func (r *TaskReconciler) resolveACPWorkspaceClass(
	ctx context.Context,
	task *corev1alpha1.Task,
) (*acpResolvedWorkspaceClass, error) {
	return r.resolveACPWorkspaceClassWithSessionUID(ctx, task, "")
}

//nolint:gocyclo // Every class-path rejection is audited in one place.
func (r *TaskReconciler) resolveACPWorkspaceClassWithSessionUID(
	ctx context.Context,
	task *corev1alpha1.Task,
	workspaceSessionUID string,
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
	reader := uncachedReader(r.APIReader, r.Client)

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
	frozenContinuation, frozenContinuationReady, err := r.frozenACPContinuationExists(ctx, reader, task, class, workspaceSessionUID)
	if err != nil {
		return nil, err
	}
	requiredFeatures := acpWorkspaceResolutionRequiredFeatures(task, class, frozenContinuationReady)
	ready := apimeta.FindStatusCondition(class.Status.Conditions, string(workspacev1alpha1.ConditionClassReady))
	readyAtCurrentGeneration := class.Status.ObservedGeneration == class.Generation &&
		ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == class.Generation
	if !readyAtCurrentGeneration && !mayResolveFrozenACPContinuation(task, class, ready, frozenContinuation, requiredFeatures) {
		return nil, fmt.Errorf("execution workspace class %q is not ready at its current generation", className)
	}
	if strings.TrimSpace(class.Status.ProfileHash) == "" || class.Status.ProviderRef == nil ||
		strings.TrimSpace(class.Status.ProviderRef.Name) == "" {
		return nil, fmt.Errorf("execution workspace class %q has no pinned profile hash or resolved provider", className)
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
	if provider.Spec.ControllerName == acpWorkspaceProviderControllerName {
		return nil, fmt.Errorf("legacy execution workspace provider must retire under its original controller before upgrade")
	}
	return r.resolveExternalACPWorkspaceClass(ctx, reader, task, class, provider, workspaceSessionUID, frozenContinuation, requiredFeatures)
}

var errACPWorkspacePlanningTransient = errors.New("transient execution workspace planning failure")

func (r *TaskReconciler) enforceACPWorkspaceSuspendQuota(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	resolved *acpResolvedWorkspaceClass,
) error {
	if resolved.Binding.MaxSuspendedWorkspaces == nil {
		return nil
	}
	prospective := resolved.DefaultOnDetach
	if requested := task.Spec.Execution.Workspace.OnDetach; requested != "" {
		prospective = workspacev1alpha1.WorkspaceOnDetach(requested)
	}
	if prospective != workspacev1alpha1.WorkspaceOnDetachSuspend {
		return nil
	}
	sessionUID := ""
	if task.Spec.SessionRef != nil && strings.TrimSpace(task.Spec.SessionRef.Name) != "" &&
		r.DurableControlStore != nil && r.SessionManager != nil && r.ControllerEpochManager != nil {
		resolvedUID, sessionErr := r.planACPWorkspaceSessionUID(ctx, task)
		if sessionErr != nil {
			if permanentACPWorkspaceSessionPlanningError(sessionErr) {
				return sessionErr
			}
			return fmt.Errorf("%w: %v", errACPWorkspacePlanningTransient, sessionErr)
		}
		sessionUID = strings.TrimSpace(resolvedUID)
	}
	suspended, err := countSuspendedClassWorkspaces(ctx, reader, task.Namespace, class.UID,
		func(candidate *workspacev1alpha1.ExecutionWorkspace) bool {
			return sessionUID != "" && candidate.Spec.SessionRef != nil &&
				string(candidate.Spec.SessionRef.UID) == sessionUID
		})
	if err != nil {
		return fmt.Errorf("%w: %v", errACPWorkspacePlanningTransient, err)
	}
	if suspended >= int(*resolved.Binding.MaxSuspendedWorkspaces) {
		continuationReady, continuationErr := r.readySessionWorkspaceAwaitingSuspendQuota(
			ctx, reader, task, resolved, sessionUID,
		)
		if continuationErr != nil {
			return continuationErr
		}
		if continuationReady {
			return nil
		}
		remediation := "delete or resume a suspended workspace"
		if slices.Contains(resolved.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachDelete) {
			remediation += ", or request onDetach Delete"
		}
		return fmt.Errorf(
			"execution workspace class %q retention cap of %d suspended workspaces is exhausted; %s",
			class.Name, *resolved.Binding.MaxSuspendedWorkspaces, remediation,
		)
	}
	return nil
}

func (r *TaskReconciler) readySessionWorkspaceAwaitingSuspendQuota(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	resolved *acpResolvedWorkspaceClass,
	sessionUID string,
) (bool, error) {
	if strings.TrimSpace(sessionUID) == "" ||
		task.Spec.Execution.Workspace.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
		return false, nil
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, sessionUID, resolved)
	if err != nil {
		return false, err
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	key := types.NamespacedName{
		Namespace: task.Namespace,
		Name:      acpClassWorkspaceName(task, binding),
	}
	if err := reader.Get(ctx, key, workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("%w: read quota-blocked session workspace: %v", errACPWorkspacePlanningTransient, err)
	}
	if !workspace.DeletionTimestamp.IsZero() || workspace.Spec.Attachment != nil ||
		workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateReady ||
		workspace.Spec.SessionRef == nil || string(workspace.Spec.SessionRef.UID) != sessionUID ||
		workspace.Annotations[acpWorkspaceDetachActionAnnotation] != string(workspacev1alpha1.WorkspaceOnDetachSuspend) ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceDurableSessionCommittedAnnotation]) == "" ||
		!runtimePoolWorkspaceSuspendableAnnotationPresent(workspace) {
		return false, nil
	}
	if err := verifyACPClassWorkspace(
		workspace, task, binding, workspace.Annotations[acpExecutionWorkspacePoolAnnotation],
	); err != nil {
		return false, nil
	}
	return true, nil
}

type workspaceProviderProfileIdentity struct {
	UID               types.UID                              `json:"uid"`
	ControllerName    string                                 `json:"controllerName"`
	ParametersRef     workspacev1alpha1.TypedObjectReference `json:"parametersRef"`
	RequiredContracts []string                               `json:"requiredContracts"`
}

func newWorkspaceProviderProfileIdentity(provider *workspacev1alpha1.ExecutionWorkspaceProvider) workspaceProviderProfileIdentity {
	requiredContracts := append([]string(nil), provider.Spec.RequiredContracts...)
	slices.Sort(requiredContracts)
	return workspaceProviderProfileIdentity{
		UID:               provider.UID,
		ControllerName:    provider.Spec.ControllerName,
		ParametersRef:     provider.Spec.ParametersRef,
		RequiredContracts: requiredContracts,
	}
}

func acpWorkspaceClassProfileHash(
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	parameters *unstructured.Unstructured,
) (string, error) {
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
	return workspaceprovider.ClassProfileHash(class.Spec, newWorkspaceProviderProfileIdentity(provider), resolved)
}

func onDetachActionsToStrings(actions []workspacev1alpha1.WorkspaceOnDetach) []string {
	values := make([]string, 0, len(actions))
	for _, action := range actions {
		values = append(values, string(action))
	}
	return values
}

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
	switch effective {
	case workspacev1alpha1.WorkspaceOnDetachDelete:
	case workspacev1alpha1.WorkspaceOnDetachSuspend:
		if resolved.Binding.SuspendMode != acpWorkspaceSuspendDataOnly {
			return "", fmt.Errorf(
				"execution workspace onDetach Suspend requires a class whose profile permits DataOnly suspension; class %q does not",
				resolved.Binding.Name,
			)
		}
	default:
		return "", fmt.Errorf(
			"execution workspace onDetach %q is not executable for ACP RuntimeSessions",
			effective,
		)
	}
	return effective, nil
}

func acpWorkspaceReuseScopeAllowed(reuse corev1alpha1.WorkspaceReusePolicy, resolved *acpResolvedWorkspaceClass) bool {
	scope := workspacev1alpha1.WorkspaceReuseScopeNone
	if reuse == corev1alpha1.WorkspaceReusePolicySession {
		scope = workspacev1alpha1.WorkspaceReuseScopeSession
	}
	return slices.Contains(resolved.AllowedReuseScopes, scope)
}

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
	if class.ControllerName != "" {
		if len(class.ControllerName) > 63 || len(validation.IsDNS1123Subdomain(class.ControllerName)) != 0 || class.LifecycleContractVersion != workspacev1alpha1.LifecycleContractV1 {
			return fmt.Errorf("frozen external workspace controller or lifecycle contract is invalid")
		}
		for _, pair := range []struct {
			ref     *workspacev1alpha1.TypedObjectReference
			binding *workspacev1alpha1.ImmutableObjectBinding
		}{{class.ProviderConfigRef, class.ProviderConfigBinding}, {class.ParametersRef, class.ParametersBinding}} {
			if pair.ref == nil || pair.binding == nil || pair.ref.Group == "" || pair.ref.Kind == "" ||
				pair.ref.Name != pair.binding.Name || pair.binding.UID == "" || pair.binding.Generation < 1 || !validSHA256Digest(pair.binding.ProfileHash) {
				return fmt.Errorf("frozen external workspace parameters lack an exact immutable binding")
			}
		}
		if class.ProviderConfigUID != string(class.ProviderConfigBinding.UID) || class.SandboxVolume != nil {
			return fmt.Errorf("frozen external workspace config pin or parameters are inconsistent")
		}
	}
	return validateACPWorkspaceClassLifecycleValues(class)
}

func validateACPWorkspaceClassLifecycleValues(class *ACPWorkspaceClassBinding) error {
	switch class.EffectiveOnDetach {
	case string(workspacev1alpha1.WorkspaceOnDetachDelete):
	case string(workspacev1alpha1.WorkspaceOnDetachSuspend):
		if class.SuspendMode != acpWorkspaceSuspendDataOnly {
			return fmt.Errorf("frozen execution workspace class binding permits Suspend without a DataOnly suspension policy")
		}
	default:
		return fmt.Errorf("frozen execution workspace class binding detach action %q is not executable", class.EffectiveOnDetach)
	}
	if class.SuspendMode != "" && class.SuspendMode != acpWorkspaceSuspendDataOnly {
		return fmt.Errorf("frozen execution workspace class binding suspension mode %q is not supported", class.SuspendMode)
	}
	if class.MaxSuspendedWorkspaces != nil && *class.MaxSuspendedWorkspaces < 0 {
		return fmt.Errorf("frozen execution workspace class binding retention cap is negative")
	}
	if class.SuspendAfterDetach != "" {
		if _, err := parseACPWorkspaceSuspendAfterDetach(class.SuspendAfterDetach); err != nil {
			return fmt.Errorf("frozen execution workspace class binding warm-detach window is invalid: %w", err)
		}
		if class.SuspendMode != acpWorkspaceSuspendDataOnly ||
			!slices.Contains(class.AllowedOnDetach, string(workspacev1alpha1.WorkspaceOnDetachSuspend)) {
			return fmt.Errorf("frozen execution workspace class binding warm-detach window requires a DataOnly Suspend policy")
		}
	}
	if class.SandboxVolume != nil {
		if class.SuspendMode != acpWorkspaceSuspendDataOnly {
			return fmt.Errorf("frozen execution workspace class binding carries a durable volume without a DataOnly suspension policy")
		}
		if _, err := resource.ParseQuantity(class.SandboxVolume.Capacity); err != nil {
			return fmt.Errorf("frozen execution workspace class binding durable volume capacity is invalid: %w", err)
		}
		if len(class.SandboxVolume.AccessModes) == 0 {
			return fmt.Errorf("frozen execution workspace class binding durable volume has no access modes")
		}
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
	if !slices.Contains(class.AllowedOnDetach, class.DefaultOnDetach) {
		return fmt.Errorf("frozen execution workspace class binding default detach action %q is not allowed", class.DefaultOnDetach)
	}
	if !slices.Contains(class.AllowedOnDetach, class.EffectiveOnDetach) {
		return fmt.Errorf("frozen execution workspace class binding effective detach action %q is not allowed", class.EffectiveOnDetach)
	}
	if err := validateACPWorkspaceClassTimeouts(class); err != nil {
		return err
	}
	for _, action := range []string{
		class.DeletionPolicy.ProviderResources,
		class.DeletionPolicy.PersistentVolumes,
		class.DeletionPolicy.Checkpoints,
	} {
		if action != string(workspacev1alpha1.WorkspaceDeletionActionDelete) {
			return fmt.Errorf("frozen execution workspace class binding deletion policy action %q is not executable; only Delete is supported", action)
		}
	}
	return nil
}

func validateACPWorkspaceClassTimeouts(class *ACPWorkspaceClassBinding) error {
	detachTimeout, err := time.ParseDuration(class.DetachTimeout)
	if err != nil {
		return fmt.Errorf("frozen execution workspace class binding detach timeout is invalid: %w", err)
	}
	if detachTimeout <= 0 {
		return fmt.Errorf("frozen execution workspace class binding detach timeout must be positive")
	}
	var idleTimeout time.Duration
	if class.IdleTimeout != "" {
		idleTimeout, err = time.ParseDuration(class.IdleTimeout)
		if err != nil {
			return fmt.Errorf("frozen execution workspace class binding idle timeout is invalid: %w", err)
		}
		if idleTimeout <= 0 {
			return fmt.Errorf("frozen execution workspace class binding idle timeout must be positive")
		}
	}
	var maxLifetime time.Duration
	if class.MaxLifetime != "" {
		maxLifetime, err = time.ParseDuration(class.MaxLifetime)
		if err != nil {
			return fmt.Errorf("frozen execution workspace class binding maximum lifetime is invalid: %w", err)
		}
		if maxLifetime <= 0 {
			return fmt.Errorf("frozen execution workspace class binding maximum lifetime must be positive")
		}
	}
	if class.IdleTimeout != "" && class.MaxLifetime != "" && maxLifetime < idleTimeout {
		return fmt.Errorf("frozen execution workspace class binding maximum lifetime must be greater than or equal to idle timeout")
	}
	if class.SuspendAfterDetach != "" {
		window, err := parseACPWorkspaceSuspendAfterDetach(class.SuspendAfterDetach)
		if err != nil {
			return fmt.Errorf("frozen execution workspace class binding warm-detach window is invalid: %w", err)
		}
		if err := validateACPWorkspaceSuspendAfterDetachBounds(window, idleTimeout, maxLifetime); err != nil {
			return fmt.Errorf("frozen execution workspace class binding warm-detach window is invalid: %w", err)
		}
	}
	return nil
}
