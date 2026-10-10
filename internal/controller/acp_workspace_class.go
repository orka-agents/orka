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

// acpWorkspaceProviderControllerName is the reserved adapter identity for the
// in-tree ACP RuntimePool execution-workspace adapter. Only
// ExecutionWorkspaceProvider objects carrying this controllerName may back
// class-selected ACP RuntimeSessions.
const acpWorkspaceSuspendDataOnly = "DataOnly"

const acpWorkspaceProviderControllerName = "acp.workspace.orka.ai/runtime-pool"

// acpWorkspaceControllerLabelValue is the label-safe encoding of the reserved
// controllerName (label values cannot contain '/'); it marks ACP-owned
// ExecutionWorkspaces for the adapter and the retention reconciler.
const acpWorkspaceControllerLabelValue = "runtime-pool.acp.workspace.orka.ai"

// ACPWorkspaceClassDeletionPolicy freezes the class deletion dispositions that
// finalization must honor independently for each retained-data category.
type ACPWorkspaceClassDeletionPolicy struct {
	ProviderResources string
	PersistentVolumes string
	Checkpoints       string
}

// ACPSandboxDurableVolume is the frozen durable workspace PVC shape for a
// suspend-capable agent-sandbox class binding.
type ACPSandboxDurableVolume struct {
	StorageClassName string
	// StorageClassUID pins the exact StorageClass whose Delete reclaim
	// semantics were validated at class resolution; provisioning reverifies
	// it so a same-name replacement class cannot silently retain volumes.
	StorageClassUID string
	AccessModes     []string
	Capacity        string
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
	// External bindings pin the installed lifecycle contract and both opaque
	// provider-owned parameter objects. Empty ControllerName identifies only
	// the legacy snapshot format retained for migration.
	ControllerName           string
	LifecycleContractVersion string
	ProviderConfigRef        *workspacev1alpha1.TypedObjectReference
	ProviderConfigBinding    *workspacev1alpha1.ImmutableObjectBinding
	ParametersRef            *workspacev1alpha1.TypedObjectReference
	ParametersBinding        *workspacev1alpha1.ImmutableObjectBinding
	// ProviderConfigUID pins the exact cluster-scoped RuntimeProviderConfig
	// that selected the physical backend: recreating the immutable config
	// under the same name must read as drift, never as a silent backend swap.
	ProviderConfigUID string
	// EffectiveOnDetach is the validated detach action for this Task: the
	// Task-requested value when the class allows it, otherwise the class
	// default. Delete is always executable; Suspend is executable only for
	// session-reused workspaces whose backend profile permits DataOnly
	// suspension.
	EffectiveOnDetach string
	// SuspendMode freezes the operator-permitted suspension scope from the
	// class profile. Empty means suspension is not permitted; DataOnly is the
	// only supported value.
	SuspendMode string
	// SandboxVolume freezes the durable workspace PVC shape for
	// suspend-capable agent-sandbox classes. It is nil for every other class.
	SandboxVolume *ACPSandboxDurableVolume
	// MaxSuspendedWorkspaces freezes the class retention cap. Nil means
	// per-workspace age must be bounded by idleTimeout or maxLifetime.
	MaxSuspendedWorkspaces *int32
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

// mayResolveFrozenACPContinuation permits an established session to continue
// through a current-generation class condition that is stricter than its Task
// binding. This covers a frozen RuntimePool volume after its StorageClass is
// retired and a Delete-bound continuation after the provider withdraws the
// class's implied Suspend feature. Every provider, profile-hash, and frozen-
// binding fence below still runs.
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

// acpWorkspaceResolutionRequiredFeatures derives the provider capabilities the
// Task will freeze into its class binding. A Delete-bound continuation can
// omit the class's implied Suspend feature only when its existing workspace
// is already running and needs no provider resume operation.
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

// frozenACPContinuationExists proves that the planned Session UID already
// owns the deterministic class workspace and its exact UID-linked RuntimePool.
// It separately reports whether the workspace is already running, because a
// suspended or settling workspace still needs the provider's Suspend feature
// to resume. A planned Session UID alone is not continuation evidence because
// every new session-reused Task resolves one before class readiness is checked.
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

// resolveACPWorkspaceClass resolves and pins Task.spec.execution.workspace.classRef
// against the live ExecutionWorkspaceClass, its provider, and the adapter-owned
// parameter objects. Every mismatch fails closed before any workspace or
// RuntimePool demand exists. The `use` authorization for the class is enforced
// at admission by the workspace-class-use webhook and policy; this resolver
// re-verifies object identity and policy, not caller authority.
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

// errACPWorkspacePlanningTransient marks workspace-plan resolution failures
// caused by transient reads (uncached quota lists, durable session-store
// lookups): the Task must requeue instead of being permanently rejected by a
// brief API-server or control-store outage.
var errACPWorkspacePlanningTransient = errors.New("transient execution workspace planning failure")

// enforceACPWorkspaceSuspendQuota rejects a Task whose prospective Suspend
// detach action would exceed the class retention cap. Settlement re-checks
// the live count so a race between admissions still cannot exceed the cap.
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
	// A continuation resuming its own suspended session workspace frees the
	// slot it occupies: that workspace never counts against admission, or the
	// Task that would resume it could never reach ensureACPClassWorkspace.
	// The exclusion matches the immutable Session UID, never the reusable
	// name: a Session recreated under the same name resolves a different UID
	// and creates a different workspace, so the old incarnation's suspended
	// workspace still consumes the cap. Session reuse currently admits only
	// the default workspace slot in resolveACPWorkspaceSessionScope, so the UID
	// identifies the only reusable workspace this Task can resume.
	sessionUID := ""
	if task.Spec.SessionRef != nil && strings.TrimSpace(task.Spec.SessionRef.Name) != "" &&
		r.DurableControlStore != nil && r.SessionManager != nil && r.ControllerEpochManager != nil {
		// Without the durable session stores (validation-only resolution) the
		// exclusion is simply skipped: counting the own workspace is stricter,
		// never looser.
		resolvedUID, sessionErr := r.planACPWorkspaceSessionUID(ctx, task)
		if sessionErr != nil {
			if permanentACPWorkspaceSessionPlanningError(sessionErr) {
				// A nonexistent create:false Session or failed stored-Session
				// validation is terminal: the binding stage classifies these
				// permanent, and marking them transient here would requeue
				// the Task forever instead of surfacing the validation
				// failure.
				return sessionErr
			}
			// A store-read outage stays retryable: the primary binding
			// resolution re-runs this lookup with full classification, and
			// here it only shapes the quota exclusion.
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

// readySessionWorkspaceAwaitingSuspendQuota reports whether this Task can
// reuse its exact session workspace while the class cap is full. The existing
// Ready workspace has already consumed the only materialization for this
// session and cannot suspend until a slot opens, so admitting its continuation
// creates no additional retained workspace.
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
	// Retention bounds gate new class resolution. Frozen snapshots admitted by
	// older controllers remain executable so an upgrade cannot wedge a Task
	// whose immutable binding predates that requirement.
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
		// Only the all-Delete lifecycle is executable: class admission
		// rejects retained policies, and settlement destroys the workspace
		// and its pool. A snapshot frozen by a newer controller with Retain
		// semantics must fail closed here after a rollback rather than begin
		// destructive cleanup under a retention contract this version cannot
		// honor.
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
	return nil
}
