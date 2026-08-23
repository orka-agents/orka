package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const (
	executionWorkspaceFinalizer = "workspace.orka.ai/finalizer"
	workspaceRequeueInterval    = 10 * time.Second
)

// ExecutionWorkspaceReconciler owns generic workspace identity, immutable
// binding validation, desired lifecycle, and finalization. It never writes
// provider-native fields or performs provider lifecycle calls directly.
type ExecutionWorkspaceReconciler struct {
	client.Client
	APIReader               client.Reader
	RESTMapper              apimeta.RESTMapper
	AdmissionLeaseNamespace string
	CleanupOnly             bool
}

func (r *ExecutionWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.CleanupOnly && workspace.DeletionTimestamp.IsZero() {
		// Cleanup-only mode admits nothing new, but the cleanup finalizer
		// must still be installed: a workspace created just before the API
		// was disabled would otherwise never gain it, retention would wait on
		// it forever, and neither idleTimeout nor maxLifetime could ever
		// reclaim the workspace and its pool.
		if !controllerutil.ContainsFinalizer(workspace, executionWorkspaceFinalizer) {
			controllerutil.AddFinalizer(workspace, executionWorkspaceFinalizer)
			if err := r.Update(ctx, workspace); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return r.reconcileDeletingWorkspace(ctx, workspace)
	}
	if !controllerutil.ContainsFinalizer(workspace, executionWorkspaceFinalizer) {
		controllerutil.AddFinalizer(workspace, executionWorkspaceFinalizer)
		if err := r.Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if dispositionFailed(workspace.Status.Disposition) {
		if err := r.quarantineWorkspace(ctx, workspace, "workspace cleanup disposition contains a failed category"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
	}

	reason, message, err := r.validateWorkspaceBindings(ctx, workspace)
	if err != nil {
		return ctrl.Result{}, err
	}
	return r.reconcileValidatedWorkspace(ctx, req.NamespacedName, workspace, reason, message)
}

func (r *ExecutionWorkspaceReconciler) reconcileDeletingWorkspace(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (ctrl.Result, error) {
	if workspaceProjectionTrusted(workspace) {
		if err := r.projectWorkspaceToOwner(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	return r.reconcileWorkspaceDeletion(ctx, workspace)
}

func (r *ExecutionWorkspaceReconciler) reconcileValidatedWorkspace(
	ctx context.Context,
	key types.NamespacedName,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	reason string,
	message string,
) (ctrl.Result, error) {
	admitted := reason == string(workspacev1alpha1.ReasonReady)
	maintenanceIntent := workspaceHasMaintenanceIntent(workspace) || workspaceNeedsAttachmentRevocation(workspace)
	switch {
	case admitted && !maintenanceIntent && !workspaceCurrentlyAdmittedByCore(workspace):
		return r.reconcileWorkspaceAdmission(ctx, key, workspace, message)
	case !admitted && !maintenanceIntent:
		return r.reconcileWorkspaceAdmissionDenial(ctx, key, workspace, reason, message)
	}

	condition := metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		Status:             conditionStatus(admitted),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: workspace.Generation,
	}
	if err := r.patchWorkspaceCondition(ctx, key, condition); err != nil {
		return ctrl.Result{}, err
	}
	if (admitted || maintenanceIntent) && workspaceProjectionTrusted(workspace) {
		if err := r.projectLatestWorkspaceToOwner(ctx, key); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
}

func (r *ExecutionWorkspaceReconciler) reconcileWorkspaceAdmission(
	ctx context.Context,
	key types.NamespacedName,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	message string,
) (ctrl.Result, error) {
	if retryAfter := workspaceCapacityAdmissionRetryAfter(workspace, time.Now().UTC()); retryAfter > 0 {
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}
	targetGeneration := workspace.Generation + 1
	if !workspaceCoreAdmissionConditionMatches(workspace, targetGeneration) {
		condition := metav1.Condition{
			Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
			Status:             metav1.ConditionTrue,
			Reason:             string(workspacev1alpha1.ReasonReady),
			Message:            message,
			ObservedGeneration: targetGeneration,
		}
		if err := r.patchWorkspaceCondition(ctx, key, condition); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.projectLatestWorkspaceToOwnerPendingAdmission(ctx, key); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	reason, denialMessage, err := r.patchWorkspaceCoreAdmission(ctx, workspace, targetGeneration)
	if err != nil {
		if errors.Is(err, errWorkspacePoolAdmissionContended) {
			if projectErr := r.projectLatestWorkspaceToOwnerPendingAdmission(ctx, key); projectErr != nil {
				return ctrl.Result{}, projectErr
			}
			return ctrl.Result{RequeueAfter: workspacePoolAdmissionContentionRequeue}, nil
		}
		if errors.Is(err, errWorkspacePoolCapacityUnavailable) {
			return r.reconcileWorkspaceCapacityDenial(ctx, key, workspace)
		}
		return ctrl.Result{}, err
	}
	if reason != string(workspacev1alpha1.ReasonReady) {
		return r.reconcileWorkspaceAdmissionDenial(ctx, key, workspace, reason, denialMessage)
	}
	if err := r.projectLatestWorkspaceToOwner(ctx, key); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func workspaceCapacityAdmissionRetryAfter(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	now time.Time,
) time.Duration {
	if workspace == nil {
		return 0
	}
	condition := workspaceprovider.FindCondition(
		workspace.Status.Conditions,
		string(workspacev1alpha1.ConditionWorkspaceAdmitted),
	)
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		condition.Reason != string(workspacev1alpha1.ReasonCapacityUnavailable) ||
		condition.ObservedGeneration != workspace.Generation || condition.LastTransitionTime.IsZero() {
		return 0
	}
	retryAt := condition.LastTransitionTime.Add(workspaceRequeueInterval)
	if !retryAt.After(now) {
		return 0
	}
	return retryAt.Sub(now)
}

func (r *ExecutionWorkspaceReconciler) reconcileWorkspaceCapacityDenial(
	ctx context.Context,
	key types.NamespacedName,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (ctrl.Result, error) {
	condition := metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		Status:             metav1.ConditionFalse,
		Reason:             string(workspacev1alpha1.ReasonCapacityUnavailable),
		Message:            "workspace pool has no admission capacity",
		ObservedGeneration: workspace.Generation,
	}
	if err := r.patchWorkspaceCondition(ctx, key, condition); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.projectLatestWorkspaceToOwner(ctx, key); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
}

func (r *ExecutionWorkspaceReconciler) reconcileWorkspaceAdmissionDenial(
	ctx context.Context,
	key types.NamespacedName,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	reason string,
	message string,
) (ctrl.Result, error) {
	if !workspaceAdmissionRetryable(reason) {
		// Invalidate the protected admitted generation with the quarantine spec
		// update before publishing any provider-visible denial status.
		if err := r.quarantineWorkspace(ctx, workspace, message); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
	}

	condition := metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: workspace.Generation,
	}
	if err := r.patchWorkspaceCondition(ctx, key, condition); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.projectLatestWorkspaceToOwner(ctx, key); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
}

func (r *ExecutionWorkspaceReconciler) workspacePolicyReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ExecutionWorkspaceReconciler) validateWorkspaceBindings(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (string, string, error) {
	// ExecutionWorkspace and coreAdmission ship in the same unreleased API version,
	// so there is no legacy marker backfill path. Missing markers are new allocations.
	previouslyAdmitted := workspaceHasCoreAdmission(workspace)
	class, reason, message, err := r.boundWorkspaceClass(ctx, workspace)
	if err != nil || reason != string(workspacev1alpha1.ReasonReady) {
		return reason, message, err
	}
	if reason, message = validateWorkspaceClassBinding(workspace, class, previouslyAdmitted); reason != string(workspacev1alpha1.ReasonReady) {
		return reason, message, nil
	}
	if !previouslyAdmitted {
		reason, message, err = r.validateNewWorkspaceClassAdmission(ctx, workspace, class)
		if err != nil || reason != string(workspacev1alpha1.ReasonReady) {
			return reason, message, err
		}
	}

	providerName := workspace.Spec.ProviderBinding.Name
	if !previouslyAdmitted {
		providerName, err = r.classProviderName(ctx, class)
		if err != nil {
			return "", "", err
		}
	}
	if !previouslyAdmitted && class.Spec.PoolRef != nil {
		reason, message, err = r.validateWorkspacePoolAdmission(ctx, class, providerName)
		if err != nil || reason != string(workspacev1alpha1.ReasonReady) {
			return reason, message, err
		}
	}
	return r.validateWorkspaceProviderBinding(ctx, workspace, providerName, !previouslyAdmitted)
}

func (r *ExecutionWorkspaceReconciler) boundWorkspaceClass(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (*workspacev1alpha1.ExecutionWorkspaceClass, string, string, error) {
	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Spec.ClassBinding.Name}
	if err := r.workspacePolicyReader().Get(ctx, key, class); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, "ClassNotFound", "bound workspace class does not exist", nil
		}
		return nil, "", "", fmt.Errorf("get bound workspace class: %w", err)
	}
	return class, string(workspacev1alpha1.ReasonReady), "", nil
}

func validateWorkspaceClassBinding(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	previouslyAdmitted bool,
) (string, string) {
	if class.UID != workspace.Spec.ClassBinding.UID || class.Generation != workspace.Spec.ClassBinding.Generation {
		return "ClassBindingMismatch", "workspace class binding does not match the referenced class revision"
	}
	if !class.DeletionTimestamp.IsZero() && !previouslyAdmitted {
		return "ClassDeleting", "bound workspace class is deleting and cannot admit new workspaces"
	}
	if class.Status.ProfileHash == "" {
		return reasonClassNotReady, "bound workspace class has no ready profile hash"
	}
	if class.Status.ProfileHash != workspace.Spec.ClassBinding.ProfileHash {
		return "ClassProfileMismatch", "workspace class profile hash does not match the bound class"
	}
	if workspace.Spec.Mode != class.Spec.Mode || !reflect.DeepEqual(workspace.Spec.Lifecycle, class.Spec.Lifecycle) {
		return "ClassPolicyMismatch", "workspace mode or lifecycle does not match the bound class"
	}
	reuseScope := workspacev1alpha1.WorkspaceReuseScopeNone
	if workspace.Spec.SessionRef != nil {
		reuseScope = workspacev1alpha1.WorkspaceReuseScopeSession
	}
	if !slices.Contains(class.Spec.AllowedReuseScopes, reuseScope) {
		return "ReuseScopeNotAllowed", fmt.Sprintf("class does not allow %s reuse", reuseScope)
	}
	return string(workspacev1alpha1.ReasonReady), "workspace class binding is valid"
}

func (r *ExecutionWorkspaceReconciler) validateNewWorkspaceClassAdmission(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (string, string, error) {
	if reason, message := workspaceClassAdmissionReadiness(class); reason != string(workspacev1alpha1.ReasonReady) {
		return reason, message, nil
	}
	classValidator := &ExecutionWorkspaceClassReconciler{
		Client: r.Client, APIReader: r.workspacePolicyReader(), RESTMapper: r.RESTMapper,
	}
	providerName, reason, message, err := classValidator.resolveClassProvider(ctx, class)
	if err != nil || reason != string(workspacev1alpha1.ReasonReady) {
		return reason, message, err
	}
	resolvedHash, err := classValidator.resolvedClassProfileHash(ctx, class)
	if err != nil {
		return "", "", fmt.Errorf("resolve live workspace class profile: %w", err)
	}
	if resolvedHash != workspace.Spec.ClassBinding.ProfileHash {
		return reasonProfileDrift, "live workspace class dependencies changed", nil
	}
	if providerName != workspace.Spec.ProviderBinding.Name {
		return reasonProviderBindingMismatch, "live class provider does not match workspace binding", nil
	}
	return string(workspacev1alpha1.ReasonReady), "workspace class accepts new allocations", nil
}

func workspaceClassAdmissionReadiness(class *workspacev1alpha1.ExecutionWorkspaceClass) (string, string) {
	readyCondition := workspaceprovider.FindCondition(
		class.Status.Conditions,
		string(workspacev1alpha1.ConditionClassReady),
	)
	classReady := class.Status.ObservedGeneration == class.Generation &&
		readyCondition != nil &&
		readyCondition.ObservedGeneration == class.Generation &&
		readyCondition.Status == metav1.ConditionTrue
	if classReady {
		return string(workspacev1alpha1.ReasonReady), "bound workspace class is ready"
	}

	reason := reasonClassNotReady
	message := "bound workspace class is not ready for new workspaces"
	if readyCondition != nil {
		if readyCondition.Reason != "" && readyCondition.Reason != string(workspacev1alpha1.ReasonReady) {
			reason = readyCondition.Reason
		}
		if readyCondition.Message != "" {
			message = readyCondition.Message
		}
	}
	return reason, message
}

func (r *ExecutionWorkspaceReconciler) validateWorkspacePoolAdmission(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	expectedProviderName string,
) (string, string, error) {
	pool := &workspacev1alpha1.ExecutionWorkspacePool{}
	key := types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.PoolRef.Name}
	if err := r.workspacePolicyReader().Get(ctx, key, pool); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "PoolNotFound", "bound workspace pool does not exist", nil
		}
		return "", "", fmt.Errorf("get bound workspace pool: %w", err)
	}
	return validateWorkspacePoolObject(pool, expectedProviderName)
}

func validateWorkspacePoolObject(
	pool *workspacev1alpha1.ExecutionWorkspacePool,
	expectedProviderName string,
) (string, string, error) {
	if !pool.DeletionTimestamp.IsZero() {
		return "PoolDeleting", "bound workspace pool is deleting", nil
	}
	if pool.Spec.ProviderRef.Name != expectedProviderName {
		return "PoolProviderMismatch", "bound workspace pool provider does not match the class", nil
	}
	ready := workspaceprovider.FindCondition(pool.Status.Conditions, string(workspacev1alpha1.ConditionPoolReady))
	admitted := workspaceprovider.FindCondition(pool.Status.Conditions, string(workspacev1alpha1.ConditionPoolAdmitted))
	if pool.Status.ObservedGeneration != pool.Generation ||
		ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != pool.Generation {
		return reasonPoolNotReady, "bound workspace pool readiness is stale or false", nil
	}
	if admitted == nil || admitted.Status != metav1.ConditionTrue || admitted.ObservedGeneration != pool.Generation {
		return string(workspacev1alpha1.ReasonCapacityUnavailable), "bound workspace pool has no admission capacity", nil
	}
	return string(workspacev1alpha1.ReasonReady), "workspace pool accepts new allocations", nil
}

func (r *ExecutionWorkspaceReconciler) validateWorkspaceProviderBinding(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	expectedProviderName string,
	requireActive bool,
) (string, string, error) {
	if expectedProviderName == "" || expectedProviderName != workspace.Spec.ProviderBinding.Name {
		return reasonProviderBindingMismatch, "workspace provider binding does not match the bound class", nil
	}
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.workspacePolicyReader().Get(ctx, types.NamespacedName{Name: expectedProviderName}, provider); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "ProviderNotFound", "bound workspace provider does not exist", nil
		}
		return "", "", fmt.Errorf("get bound workspace provider: %w", err)
	}
	if provider.UID != workspace.Spec.ProviderBinding.UID || provider.Generation < workspace.Spec.ProviderBinding.Generation {
		return reasonProviderBindingMismatch, "workspace provider binding does not match the referenced provider identity", nil
	}
	if requireActive {
		if !provider.DeletionTimestamp.IsZero() {
			return "ProviderDeleting", "provider is deleting and cannot admit new workspaces", nil
		}
		if provider.Generation != workspace.Spec.ProviderBinding.Generation {
			return reasonProviderBindingMismatch, "new workspace provider binding is stale", nil
		}
		if provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDisabled {
			return string(workspacev1alpha1.ReasonProviderDisabled), messageProviderDisabled, nil
		}
		if provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderActive {
			return string(workspacev1alpha1.ReasonProviderDraining), "provider does not accept new workspaces", nil
		}
		ready := workspaceprovider.FindCondition(
			provider.Status.Conditions,
			string(workspacev1alpha1.ConditionProviderReady),
		)
		if provider.Status.ObservedGeneration != provider.Generation ||
			ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != provider.Generation {
			return reasonProviderNotReady, "provider readiness is stale or false", nil
		}
		allowed, err := namespaceAllowedByWorkspaceProvider(
			ctx, r.workspacePolicyReader(), workspace.Namespace, provider,
		)
		if errors.Is(err, errInvalidProviderNamespaceSelector) {
			return reasonNamespacePolicyInvalid, "provider namespace usage policy is invalid", nil
		}
		if err != nil {
			return "", "", err
		}
		if !allowed {
			return "NamespaceNotAllowed", "provider usage policy does not allow this namespace", nil
		}
	}
	if provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDisabled {
		return string(workspacev1alpha1.ReasonProviderDisabled), messageProviderDisabled, nil
	}
	if requireActive && provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderActive {
		return string(workspacev1alpha1.ReasonProviderDraining), "provider does not accept new workspaces", nil
	}
	return string(workspacev1alpha1.ReasonReady), "workspace bindings are valid", nil
}

func workspaceHasCoreAdmission(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || workspace.Spec.CoreAdmission == nil {
		return false
	}
	admission := workspace.Spec.CoreAdmission
	return admission.ClassBinding == workspace.Spec.ClassBinding &&
		admission.ProviderBinding == workspace.Spec.ProviderBinding
}

func workspaceCoreAdmissionConditionMatches(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	generation int64,
) bool {
	if workspace == nil {
		return false
	}
	condition := workspaceprovider.FindCondition(
		workspace.Status.Conditions,
		string(workspacev1alpha1.ConditionWorkspaceAdmitted),
	)
	return condition != nil && condition.Status == metav1.ConditionTrue &&
		condition.Reason == string(workspacev1alpha1.ReasonReady) &&
		condition.ObservedGeneration == generation
}

func workspaceHasCoreAdmissionEvidence(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspaceHasCoreAdmission(workspace) &&
		workspaceCoreAdmissionConditionMatches(workspace, workspace.Spec.CoreAdmission.AdmittedGeneration)
}

func workspaceCurrentlyAdmittedByCore(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspaceHasCoreAdmissionEvidence(workspace) &&
		workspace.Spec.CoreAdmission.AdmittedGeneration == workspace.Generation
}

func (r *ExecutionWorkspaceReconciler) patchWorkspaceCoreAdmission(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	targetGeneration int64,
) (string, string, error) {
	if workspace == nil {
		return "", "", fmt.Errorf("workspace is required")
	}
	if workspace.Spec.CoreAdmission != nil && !workspaceHasCoreAdmission(workspace) {
		return "", "", fmt.Errorf("workspace core admission does not match immutable bindings")
	}
	patchCtx := ctx
	var releasePoolAdmission func() error
	var poolBinding *workspacev1alpha1.ImmutableObjectBinding
	if workspace.Spec.CoreAdmission == nil {
		criticalCtx, cancel := context.WithTimeout(ctx, workspacePoolAdmissionCriticalTimeout)
		defer cancel()
		patchCtx = criticalCtx
		var err error
		releasePoolAdmission, poolBinding, err = r.acquireWorkspacePoolAdmission(criticalCtx, workspace)
		if err != nil {
			return "", "", err
		}
		defer func() { _ = releasePoolAdmission() }()
	}
	reason, message, err := r.validateWorkspaceBindings(patchCtx, workspace)
	if err != nil || reason != string(workspacev1alpha1.ReasonReady) {
		return reason, message, err
	}
	before := workspace.DeepCopy()
	if workspace.Spec.CoreAdmission == nil {
		workspace.Spec.CoreAdmission = &workspacev1alpha1.ExecutionWorkspaceCoreAdmission{
			ClassBinding:    workspace.Spec.ClassBinding,
			ProviderBinding: workspace.Spec.ProviderBinding,
			PoolBinding:     poolBinding,
		}
	}
	// Updating coreAdmission is a spec change, so the API server advances the
	// workspace generation once. Publish the generation that adapters will observe.
	workspace.Spec.CoreAdmission.AdmittedGeneration = targetGeneration
	if err := r.Patch(patchCtx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return "", "", err
	}
	return string(workspacev1alpha1.ReasonReady), "workspace core admission is current", nil
}

func workspaceAdmissionRetryable(reason string) bool {
	return reason == reasonClassNotReady || reason == reasonPoolNotReady || reason == reasonProviderNotReady ||
		reason == string(workspacev1alpha1.ReasonCapacityUnavailable)
}

func workspaceProjectionTrusted(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	return workspaceCurrentlyAdmittedByCore(workspace) ||
		(workspace != nil && (!workspace.DeletionTimestamp.IsZero() || workspaceHasMaintenanceIntent(workspace)))
}

func workspaceProviderStatusTrustedForProjection(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspaceCurrentlyAdmittedByCore(workspace) {
		return workspace.Status.ObservedGeneration == workspace.Generation
	}
	return workspaceHasCoreAdmission(workspace) && workspace.Status.ObservedGeneration == workspace.Generation &&
		(!workspace.DeletionTimestamp.IsZero() || workspaceHasMaintenanceIntent(workspace))
}

func coreOwnedWorkspaceProjectionConditions(conditions []metav1.Condition) []metav1.Condition {
	result := make([]metav1.Condition, 0, 2)
	for _, condition := range conditions {
		switch condition.Type {
		case string(workspacev1alpha1.ConditionWorkspaceAdmitted),
			string(workspacev1alpha1.ConditionWorkspaceQuarantined):
			result = append(result, condition)
		}
	}
	return result
}

func workspaceNeedsAttachmentRevocation(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil || !workspaceHasCoreAdmission(workspace) || workspace.Spec.Attachment != nil {
		return false
	}
	if workspace.Status.AttachedEpoch > 0 {
		return true
	}
	return workspaceprovider.ConditionIsTrue(
		workspace.Status.Conditions,
		string(workspacev1alpha1.ConditionWorkspaceAttached),
	)
}

func workspaceHasMaintenanceIntent(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if workspace == nil {
		return false
	}
	return workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted ||
		workspace.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
}

func (r *ExecutionWorkspaceReconciler) classProviderName(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (string, error) {
	if class.Spec.ProviderRef != nil {
		return class.Spec.ProviderRef.Name, nil
	}
	if class.Spec.PoolRef == nil {
		return "", nil
	}
	pool := &workspacev1alpha1.ExecutionWorkspacePool{}
	if err := r.workspacePolicyReader().Get(ctx, types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.PoolRef.Name}, pool); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "", nil
		}
		return "", fmt.Errorf("get bound workspace pool: %w", err)
	}
	return pool.Spec.ProviderRef.Name, nil
}

func (r *ExecutionWorkspaceReconciler) reconcileWorkspaceDeletion(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (ctrl.Result, error) {
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredDeleted {
		before := workspace.DeepCopy()
		workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
		workspace.Spec.Attachment = nil
		if err := r.Patch(ctx, workspace, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if workspace.Status.State != workspacev1alpha1.ExecutionWorkspaceStateDeleted {
		return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
	}
	if workspace.Status.ObservedGeneration != workspace.Generation {
		return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
	}
	var dispositionErr error
	if workspace.Spec.Mode == workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		dispositionErr = workspaceprovider.ValidateInteractiveDeletedDisposition(
			workspace.Status.Disposition,
			workspace.Spec.Lifecycle.DeletionPolicy,
		)
	} else {
		dispositionErr = workspaceprovider.ValidateDeletedDisposition(
			workspace.Status.Disposition,
			workspace.Spec.Lifecycle.DeletionPolicy,
		)
	}
	if dispositionErr != nil {
		log.FromContext(ctx).Info(
			"waiting for policy-compliant terminal workspace cleanup disposition",
			"workspace", client.ObjectKeyFromObject(workspace),
			"error", dispositionErr,
		)
		return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
	}
	if err := r.projectWorkspaceToOwnerWithTerminalDeletion(ctx, workspace, true); err != nil {
		return ctrl.Result{}, err
	}
	if controllerutil.ContainsFinalizer(workspace, executionWorkspaceFinalizer) {
		controllerutil.RemoveFinalizer(workspace, executionWorkspaceFinalizer)
		if err := r.Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *ExecutionWorkspaceReconciler) quarantineWorkspace(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	message string,
) error {
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Name}
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &workspacev1alpha1.ExecutionWorkspace{}
		if err := r.Get(ctx, key, current); err != nil {
			return err
		}
		desiredQuarantined := current.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
		desiredDeleted := current.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
		if (desiredQuarantined || desiredDeleted) && current.Labels[workspacev1alpha1.QuarantinedLabel] == scheduledRunLabelValue {
			return nil
		}
		before := current.DeepCopy()
		if !desiredDeleted {
			current.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
		}
		current.Spec.Attachment = nil
		if current.Labels == nil {
			current.Labels = map[string]string{}
		}
		current.Labels[workspacev1alpha1.QuarantinedLabel] = scheduledRunLabelValue
		return r.Patch(ctx, current, client.MergeFrom(before))
	}); err != nil {
		return err
	}
	return r.patchWorkspaceCondition(ctx, key, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionWorkspaceQuarantined),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonQuarantined),
		Message:            message,
		ObservedGeneration: workspace.Generation,
	})
}

func (r *ExecutionWorkspaceReconciler) patchWorkspaceCondition(
	ctx context.Context,
	key types.NamespacedName,
	condition metav1.Condition,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		workspace := &workspacev1alpha1.ExecutionWorkspace{}
		if err := r.Get(ctx, key, workspace); err != nil {
			return err
		}
		before := workspace.DeepCopy()
		workspaceprovider.SetCondition(&workspace.Status.Conditions, condition)
		// A merge patch replaces the whole conditions array and carries no precondition, so
		// without the optimistic lock a stale core write silently erases adapter-owned
		// conditions and RetryOnConflict never sees the race.
		return r.Status().Patch(ctx, workspace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *ExecutionWorkspaceReconciler) projectLatestWorkspaceToOwnerPendingAdmission(
	ctx context.Context,
	key types.NamespacedName,
) error {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.workspacePolicyReader().Get(ctx, key, workspace); err != nil {
		return client.IgnoreNotFound(err)
	}
	conditions := workspace.Status.Conditions[:0]
	for _, condition := range workspace.Status.Conditions {
		if condition.Type != string(workspacev1alpha1.ConditionWorkspaceAdmitted) {
			conditions = append(conditions, condition)
		}
	}
	workspace.Status.Conditions = conditions
	return r.projectWorkspaceToOwner(ctx, workspace)
}

func (r *ExecutionWorkspaceReconciler) projectLatestWorkspaceToOwner(
	ctx context.Context,
	key types.NamespacedName,
) error {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.workspacePolicyReader().Get(ctx, key, workspace); err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.projectWorkspaceToOwner(ctx, workspace)
}

func (r *ExecutionWorkspaceReconciler) projectWorkspaceToOwner(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	return r.projectWorkspaceToOwnerWithTerminalDeletion(ctx, workspace, false)
}

func (r *ExecutionWorkspaceReconciler) projectWorkspaceToOwnerWithTerminalDeletion(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
	forceTerminalDeleted bool,
) error {
	owner := metav1.GetControllerOf(workspace)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return nil
	}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: owner.Name}
	state := workspace.Status.State
	attachedEpoch := workspace.Status.AttachedEpoch
	conditions := append([]metav1.Condition(nil), workspace.Status.Conditions...)
	trustedProviderStatus := workspaceProviderStatusTrustedForProjection(workspace)
	if !trustedProviderStatus {
		if !workspaceHasCoreAdmission(workspace) {
			attachedEpoch = 0
		}
		conditions = coreOwnedWorkspaceProjectionConditions(workspace.Status.Conditions)
		if forceTerminalDeleted {
			state = workspacev1alpha1.ExecutionWorkspaceStateDeleted
		} else {
			switch workspace.Spec.DesiredState {
			case workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined:
				state = workspacev1alpha1.ExecutionWorkspaceStateQuarantined
			case workspacev1alpha1.ExecutionWorkspaceDesiredDeleted:
				state = workspacev1alpha1.ExecutionWorkspaceStateDeleting
			default:
				state = ""
			}
		}
	}
	switch owner.Kind {
	case taskResourceKind:
		return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			task := &corev1alpha1.Task{}
			if err := r.Get(ctx, key, task); err != nil {
				return client.IgnoreNotFound(err)
			}
			if owner.UID != "" && task.UID != owner.UID {
				return nil
			}
			before := task.DeepCopy()
			projection := task.Status.ExecutionWorkspace
			if projection == nil {
				projection = &corev1alpha1.ExecutionWorkspaceStatus{}
			}
			projection.ClassRef = &corev1alpha1.WorkspaceClassReference{Name: workspace.Spec.ClassBinding.Name}
			projection.WorkspaceRef = &corev1alpha1.WorkspaceObjectReference{Name: workspace.Name, UID: string(workspace.UID)}
			projection.State = string(state)
			projection.AttachedEpoch = max(projection.AttachedEpoch, workspace.Spec.AttachmentEpoch, attachedEpoch)
			projection.Conditions = append([]metav1.Condition(nil), conditions...)
			task.Status.ExecutionWorkspace = projection
			return r.Status().Patch(ctx, task, client.MergeFrom(before))
		})
	case "Tool":
		return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			tool := &corev1alpha1.Tool{}
			if err := r.Get(ctx, key, tool); err != nil {
				return client.IgnoreNotFound(err)
			}
			if owner.UID != "" && tool.UID != owner.UID {
				return nil
			}
			before := tool.DeepCopy()
			tool.Status.Workspace = &corev1alpha1.ToolWorkspaceStatus{
				ClassRef:     corev1alpha1.WorkspaceClassReference{Name: workspace.Spec.ClassBinding.Name},
				WorkspaceRef: &corev1alpha1.WorkspaceObjectReference{Name: workspace.Name, UID: string(workspace.UID)},
				State:        string(state),
				Conditions:   append([]metav1.Condition(nil), conditions...),
			}
			tool.Status.Endpoint = ""
			if trustedProviderStatus && workspace.DeletionTimestamp.IsZero() &&
				!workspaceHasMaintenanceIntent(workspace) &&
				workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateReady &&
				len(workspace.Status.Endpoints) > 0 {
				tool.Status.Endpoint = workspace.Status.Endpoints[0].URL
			}
			return r.Status().Patch(ctx, tool, client.MergeFrom(before))
		})
	default:
		return nil
	}
}

// WorkspaceReusable reports whether a workspace is safe for selection by a new Task.
func WorkspaceReusable(workspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if !workspaceCurrentlyAdmittedByCore(workspace) || workspace.Status.ObservedGeneration != workspace.Generation ||
		!workspace.DeletionTimestamp.IsZero() || workspace.Spec.Attachment != nil {
		return false
	}
	if workspace.Labels[workspacev1alpha1.QuarantinedLabel] == scheduledRunLabelValue ||
		workspaceprovider.ConditionIsTrue(workspace.Status.Conditions, string(workspacev1alpha1.ConditionWorkspaceQuarantined)) {
		return false
	}
	return workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateReady ||
		workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended
}

func dispositionFailed(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) bool {
	if disposition == nil {
		return false
	}
	return disposition.Compute == workspacev1alpha1.DispositionFailed ||
		disposition.AccessCredentials == workspacev1alpha1.DispositionFailed ||
		disposition.EphemeralSecrets == workspacev1alpha1.DispositionFailed ||
		disposition.WorkspaceData == workspacev1alpha1.DispositionFailed ||
		disposition.PersistentVolumes == workspacev1alpha1.DispositionFailed ||
		disposition.Checkpoints == workspacev1alpha1.DispositionFailed ||
		disposition.ProviderResources == workspacev1alpha1.DispositionFailed
}

// SetupWithManager registers the generic concrete workspace coordinator.
func (r *ExecutionWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.RESTMapper == nil {
		r.RESTMapper = mgr.GetRESTMapper()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		Named("execution-workspace-core").
		Complete(r)
}
