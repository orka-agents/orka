package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const (
	executionWorkspaceClassFinalizer = "workspace.orka.ai/class-protection"
	classReadinessRequeue            = 30 * time.Second
	reasonParametersScopeInvalid     = "ParametersScopeInvalid"
	reasonProfileDrift               = "ProfileDrift"
	reasonProviderDeleting           = "ProviderDeleting"
	reasonRequiredFeatures           = "RequiredFeaturesUnavailable"
	reasonProviderBindingMismatch    = "ProviderBindingMismatch"
	reasonNamespacePolicyInvalid     = "NamespacePolicyInvalid"
	reasonClassNotReady              = "ClassNotReady"
	reasonPoolNotReady               = "PoolNotReady"
	reasonProviderNotReady           = "ProviderNotReady"
	messageProviderDisabled          = "provider is disabled"
)

var errInvalidProviderNamespaceSelector = errors.New("invalid provider namespace selector")

// ExecutionWorkspaceClassReconciler resolves provider/pool references and
// publishes provider-neutral class readiness. Authorization is performed for
// each Task/Tool caller by WorkspaceClassAuthorizer.
type ExecutionWorkspaceClassReconciler struct {
	client.Client
	APIReader   client.Reader
	RESTMapper  apimeta.RESTMapper
	CleanupOnly bool
}

func (r *ExecutionWorkspaceClassReconciler) classPolicyReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ExecutionWorkspaceClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := r.Get(ctx, req.NamespacedName, class); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !class.DeletionTimestamp.IsZero() {
		return r.reconcileClassDeletion(ctx, class)
	}
	if r.CleanupOnly {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(class, executionWorkspaceClassFinalizer) {
		controllerutil.AddFinalizer(class, executionWorkspaceClassFinalizer)
		if err := r.Update(ctx, class); err != nil {
			return ctrl.Result{}, err
		}
	}

	providerName, reason, message, err := r.resolveClassProvider(ctx, class)
	if err != nil {
		return ctrl.Result{}, err
	}
	ready := reason == string(workspacev1alpha1.ReasonReady)
	profileHash := class.Status.ProfileHash
	if ready {
		resolvedHash, err := r.resolvedClassProfileHash(ctx, class)
		if err != nil {
			return ctrl.Result{}, err
		}
		if profileHash != "" && profileHash != resolvedHash {
			ready = false
			reason = reasonProfileDrift
			message = "resolved provider profile or pool identity changed; create a new class"
		} else {
			profileHash = resolvedHash
		}
	}
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	condition := metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionClassReady),
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: class.Generation,
	}
	if err := r.patchClassStatus(ctx, req.NamespacedName, providerName, profileHash, condition); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: classReadinessRequeue}, nil
}

func (r *ExecutionWorkspaceClassReconciler) reconcileClassDeletion(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(class, executionWorkspaceClassFinalizer) {
		return ctrl.Result{}, nil
	}
	blocked, err := r.classHasBoundWorkspaces(ctx, class)
	if err != nil {
		return ctrl.Result{}, err
	}
	if blocked {
		providerName := ""
		if class.Status.ProviderRef != nil {
			providerName = class.Status.ProviderRef.Name
		}
		condition := metav1.Condition{
			Type:               string(workspacev1alpha1.ConditionClassReady),
			Status:             metav1.ConditionFalse,
			Reason:             "ReferencesRemain",
			Message:            "class deletion is blocked by workspaces bound to this class UID",
			ObservedGeneration: class.Generation,
		}
		if err := r.patchClassStatus(
			ctx,
			types.NamespacedName{Namespace: class.Namespace, Name: class.Name},
			providerName,
			class.Status.ProfileHash,
			condition,
		); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: classReadinessRequeue}, nil
	}
	controllerutil.RemoveFinalizer(class, executionWorkspaceClassFinalizer)
	if err := r.Update(ctx, class); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *ExecutionWorkspaceClassReconciler) classHasBoundWorkspaces(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	workspaces := &workspacev1alpha1.ExecutionWorkspaceList{}
	if err := reader.List(ctx, workspaces, client.InNamespace(class.Namespace)); err != nil {
		return false, fmt.Errorf("list workspaces bound to class: %w", err)
	}
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		if workspace.Spec.ClassBinding.Name == class.Name &&
			workspace.Spec.ClassBinding.UID == class.UID {
			return true, nil
		}
	}
	return false, nil
}

//nolint:gocyclo // Readiness deliberately evaluates direct/pool, provider, feature, and namespace gates.
func (r *ExecutionWorkspaceClassReconciler) resolveClassProvider(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (string, string, string, error) {
	providerName := ""
	if class.Spec.ProviderRef != nil {
		providerName = class.Spec.ProviderRef.Name
		parameters, reason, message, err := r.resolveDirectParameters(ctx, class)
		if err != nil {
			return "", "", "", err
		}
		if parameters == nil {
			return providerName, reason, message, nil
		}
	} else if class.Spec.PoolRef != nil {
		pool := &workspacev1alpha1.ExecutionWorkspacePool{}
		if err := r.classPolicyReader().Get(ctx, types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.PoolRef.Name}, pool); err != nil {
			if client.IgnoreNotFound(err) == nil {
				return "", "PoolNotFound", "referenced workspace pool does not exist", nil
			}
			return "", "", "", fmt.Errorf("get workspace pool: %w", err)
		}
		providerName = pool.Spec.ProviderRef.Name
		if !pool.DeletionTimestamp.IsZero() {
			return providerName, "PoolDeleting", "referenced workspace pool is deleting", nil
		}
		poolReady := pool.Status.ObservedGeneration == pool.Generation &&
			workspaceprovider.ConditionIsTrue(
				pool.Status.Conditions, string(workspacev1alpha1.ConditionPoolReady),
			)
		if !poolReady {
			return providerName, reasonPoolNotReady, "referenced workspace pool is not ready", nil
		}
		parameters, reason, message, err := r.resolveNamespacedParameters(
			ctx, class.Namespace, &pool.Spec.ParametersRef,
		)
		if err != nil {
			return "", "", "", err
		}
		if parameters == nil {
			return providerName, reason, message, nil
		}
	}
	if providerName == "" {
		return "", "InvalidProvisioningSource", "class has no resolved provider", nil
	}

	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.classPolicyReader().Get(ctx, types.NamespacedName{Name: providerName}, provider); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return providerName, "ProviderNotFound", "referenced workspace provider does not exist", nil
		}
		return "", "", "", fmt.Errorf("get workspace provider: %w", err)
	}
	if !provider.DeletionTimestamp.IsZero() {
		return providerName, reasonProviderDeleting, "referenced workspace provider is deleting", nil
	}
	if provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderActive {
		reason := string(workspacev1alpha1.ReasonProviderDraining)
		message := "provider is draining and rejects new workspaces"
		if provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDisabled {
			reason = string(workspacev1alpha1.ReasonProviderDisabled)
			message = messageProviderDisabled
		}
		return providerName, reason, message, nil
	}
	if !workspaceprovider.ConditionIsTrue(provider.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady)) {
		return providerName, reasonProviderNotReady, "provider is not ready for new workspaces", nil
	}
	requiredFeatures := append(
		[]workspacev1alpha1.ExecutionWorkspaceFeature(nil), class.Spec.RequiredFeatures...,
	)
	if !slices.Contains(requiredFeatures, workspacev1alpha1.WorkspaceFeatureTLS) {
		requiredFeatures = append(requiredFeatures, workspacev1alpha1.WorkspaceFeatureTLS)
	}
	if class.Spec.Mode == workspacev1alpha1.ExecutionWorkspaceModeInteractive {
		for _, feature := range []workspacev1alpha1.ExecutionWorkspaceFeature{
			workspacev1alpha1.WorkspaceFeatureExec,
			workspacev1alpha1.WorkspaceFeatureReset,
		} {
			if !slices.Contains(requiredFeatures, feature) {
				requiredFeatures = append(requiredFeatures, feature)
			}
		}
	}
	if class.Spec.Mode == workspacev1alpha1.ExecutionWorkspaceModeService &&
		!slices.Contains(requiredFeatures, workspacev1alpha1.WorkspaceFeatureServicePorts) {
		requiredFeatures = append(requiredFeatures, workspacev1alpha1.WorkspaceFeatureServicePorts)
	}
	if class.Spec.PoolRef != nil &&
		!slices.Contains(requiredFeatures, workspacev1alpha1.WorkspaceFeaturePools) {
		requiredFeatures = append(requiredFeatures, workspacev1alpha1.WorkspaceFeaturePools)
	}
	if slices.Contains(
		class.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend,
	) && !slices.Contains(requiredFeatures, workspacev1alpha1.WorkspaceFeatureSuspend) {
		requiredFeatures = append(requiredFeatures, workspacev1alpha1.WorkspaceFeatureSuspend)
	}
	if !featureSetContainsAll(provider.Status.SupportedFeatures, requiredFeatures) {
		return providerName, reasonRequiredFeatures,
			"provider does not support every explicit or implied class feature", nil
	}
	if provider.Spec.ControllerName == acpWorkspaceProviderControllerName &&
		slices.Contains(requiredFeatures, workspacev1alpha1.WorkspaceFeatureSuspend) {
		// The reserved in-tree ACP adapter advertises suspend per PROVIDER,
		// but executing it requires the class profile to opt into a backend
		// DataOnly suspend policy; a class that allows or defaults to
		// Suspend without one would go Ready and then fail every Task that
		// relies on the advertised lifecycle.
		permits, err := r.acpClassProfilePermitsSuspend(ctx, class, provider)
		if err != nil {
			return "", "", "", err
		}
		if !permits {
			return providerName, reasonRequiredFeatures,
				"class permits Suspend, but its ACP RuntimeWorkspaceProfile does not opt into a DataOnly suspend policy", nil
		}
	}
	allowed, err := r.namespaceAllowedByProvider(ctx, class.Namespace, provider)
	if errors.Is(err, errInvalidProviderNamespaceSelector) {
		return providerName, reasonNamespacePolicyInvalid, "provider namespace usage policy is invalid", nil
	}
	if err != nil {
		return "", "", "", err
	}
	if !allowed {
		return providerName, "NamespaceNotAllowed", "provider usage policy does not allow this namespace", nil
	}
	return providerName, string(workspacev1alpha1.ReasonReady), "class references are ready", nil
}

// acpClassProfilePermitsSuspend reports whether the class's ACP
// RuntimeWorkspaceProfile opts into a backend DataOnly suspend policy - the
// per-class prerequisite for actually executing the provider-advertised
// suspend capability.
func (r *ExecutionWorkspaceClassReconciler) acpClassProfilePermitsSuspend(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
) (bool, error) {
	ref := class.Spec.ParametersRef
	if ref == nil || ref.Group != acpworkspacev1alpha1.GroupVersion.Group ||
		ref.Kind != acpWorkspaceProviderProfileKind {
		return false, nil
	}
	profile := &acpworkspacev1alpha1.RuntimeWorkspaceProfile{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: class.Namespace, Name: ref.Name}, profile); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	// The suspend policy must match the provider's SELECTED backend and be
	// executable as-is: a mode without a valid volume (or attached to the
	// other backend) would mark the class Ready while every Task selecting
	// it fails at resolution.
	config := &acpworkspacev1alpha1.RuntimeProviderConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: provider.Spec.ParametersRef.Name}, config); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	switch config.Spec.Backend {
	case acpworkspacev1alpha1.RuntimeProviderBackendSubstrate:
		substrate := profile.Spec.Substrate
		return substrate != nil && substrate.Suspend != nil &&
			substrate.Suspend.Mode == acpworkspacev1alpha1.SubstrateSuspendModeDataOnly, nil
	case acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox:
		sandbox := profile.Spec.AgentSandbox
		if sandbox == nil || sandbox.Suspend == nil || profile.Spec.Substrate != nil {
			// Simultaneous substrate inputs on an agent-sandbox backend are
			// rejected at resolution; readiness must not advertise them.
			return false, nil
		}
		// The SAME validators resolution runs decide readiness: the frozen
		// volume shape (mode, positive capacity, writable access modes) and
		// the pinned storage class (existing, Delete reclaim,
		// non-terminating, or a resolvable cluster default).
		volume, volumeErr := frozenACPSandboxDurableVolume(sandbox.Suspend, class.Spec.ParametersRef.Name)
		if volumeErr != nil {
			return false, nil
		}
		if _, classErr := validateDurableStorageClassReclaim(
			ctx, r.Client, volume.StorageClassName, class.Spec.ParametersRef.Name,
		); classErr != nil {
			var apiStatus apierrors.APIStatus
			if errors.As(classErr, &apiStatus) && !apierrors.IsNotFound(classErr) {
				// A wrapped live API failure (throttle, timeout) is
				// transient: surface it so the reconcile retries instead of
				// latching a false not-ready verdict.
				return false, classErr
			}
			return false, nil
		}
		return true, nil
	default:
		return false, nil
	}
}

func (r *ExecutionWorkspaceClassReconciler) resolveDirectParameters(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (*unstructured.Unstructured, string, string, error) {
	if class.Spec.ParametersRef == nil {
		return nil, "ParametersRefMissing", "direct class parametersRef is required", nil
	}
	return r.resolveNamespacedParameters(ctx, class.Namespace, class.Spec.ParametersRef)
}

func (r *ExecutionWorkspaceClassReconciler) resolveNamespacedParameters(
	ctx context.Context,
	namespace string,
	ref *workspacev1alpha1.TypedObjectReference,
) (*unstructured.Unstructured, string, string, error) {
	if ref == nil {
		return nil, "ParametersRefMissing", "workspace parametersRef is required", nil
	}
	if r.RESTMapper == nil {
		return nil, "ParametersResolverUnavailable", "provider parameter REST mapping is unavailable", nil
	}
	mapping, err := r.RESTMapper.RESTMapping(schema.GroupKind{Group: ref.Group, Kind: ref.Kind})
	if err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil, "ParametersKindUnavailable", "provider parameter kind is not installed", nil
		}
		return nil, "", "", fmt.Errorf("resolve provider parameter kind: %w", err)
	}
	if mapping.Scope.Name() != apimeta.RESTScopeNameNamespace {
		return nil, reasonParametersScopeInvalid, "workspace parametersRef must reference a namespaced object", nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	parameters := &unstructured.Unstructured{}
	parameters.SetGroupVersionKind(mapping.GroupVersionKind)
	key := types.NamespacedName{Namespace: namespace, Name: ref.Name}
	if err := reader.Get(ctx, key, parameters); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "ParametersNotFound", "referenced provider parameters do not exist", nil
		}
		if apierrors.IsForbidden(err) {
			return nil, "ParametersAccessDenied",
				"controller lacks adapter-aggregated read access to provider parameters", nil
		}
		return nil, "", "", fmt.Errorf("get provider parameters: %w", err)
	}
	if parameters.GetDeletionTimestamp() != nil {
		return nil, "ParametersDeleting", "referenced provider parameters are deleting", nil
	}
	return parameters, string(workspacev1alpha1.ReasonReady), "provider parameters are available", nil
}

func (r *ExecutionWorkspaceClassReconciler) resolvedClassProfileHash(
	ctx context.Context,
	class *workspacev1alpha1.ExecutionWorkspaceClass,
) (string, error) {
	providerName := ""
	if class.Spec.ProviderRef != nil {
		providerName = class.Spec.ProviderRef.Name
	} else {
		pool := &workspacev1alpha1.ExecutionWorkspacePool{}
		key := types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.PoolRef.Name}
		if err := r.classPolicyReader().Get(ctx, key, pool); err != nil {
			return "", fmt.Errorf("get resolved workspace pool provider: %w", err)
		}
		providerName = pool.Spec.ProviderRef.Name
	}
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.classPolicyReader().Get(ctx, types.NamespacedName{Name: providerName}, provider); err != nil {
		return "", fmt.Errorf("get resolved workspace provider identity: %w", err)
	}
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
	if class.Spec.ProviderRef != nil {
		parameters, _, _, err := r.resolveDirectParameters(ctx, class)
		if err != nil {
			return "", err
		}
		if parameters == nil {
			return "", fmt.Errorf("direct provider parameters are not ready")
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
	pool := &workspacev1alpha1.ExecutionWorkspacePool{}
	key := types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.PoolRef.Name}
	if err := r.classPolicyReader().Get(ctx, key, pool); err != nil {
		return "", fmt.Errorf("get resolved workspace pool: %w", err)
	}
	parameters, _, _, err := r.resolveNamespacedParameters(ctx, class.Namespace, &pool.Spec.ParametersRef)
	if err != nil {
		return "", err
	}
	if parameters == nil {
		return "", fmt.Errorf("pool parameters are not ready")
	}
	resolved := struct {
		UID           types.UID                                `json:"uid"`
		ProviderRef   workspacev1alpha1.ClusterObjectReference `json:"providerRef"`
		ParametersRef workspacev1alpha1.TypedObjectReference   `json:"parametersRef"`
		ParameterUID  types.UID                                `json:"parameterUID"`
		Generation    int64                                    `json:"parameterGeneration"`
		Spec          any                                      `json:"parameterSpec,omitempty"`
	}{
		UID:           pool.UID,
		ProviderRef:   pool.Spec.ProviderRef,
		ParametersRef: pool.Spec.ParametersRef,
		ParameterUID:  parameters.GetUID(),
		Generation:    parameters.GetGeneration(),
		Spec:          parameters.Object["spec"],
	}
	return workspaceprovider.ClassProfileHash(class.Spec, providerIdentity, resolved)
}

func (r *ExecutionWorkspaceClassReconciler) namespaceAllowedByProvider(
	ctx context.Context,
	namespace string,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
) (bool, error) {
	return namespaceAllowedByWorkspaceProvider(ctx, r.classPolicyReader(), namespace, provider)
}

func namespaceAllowedByWorkspaceProvider(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
) (bool, error) {
	if provider.Spec.UsagePolicy == nil || provider.Spec.UsagePolicy.AllowedNamespaceSelector == nil {
		return true, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(provider.Spec.UsagePolicy.AllowedNamespaceSelector)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errInvalidProviderNamespaceSelector, err)
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, ns); err != nil {
		return false, fmt.Errorf("get namespace for provider policy: %w", err)
	}
	return selector.Matches(labels.Set(ns.Labels)), nil
}

func (r *ExecutionWorkspaceClassReconciler) patchClassStatus(
	ctx context.Context,
	key types.NamespacedName,
	providerName string,
	profileHash string,
	condition metav1.Condition,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		class := &workspacev1alpha1.ExecutionWorkspaceClass{}
		if err := r.Get(ctx, key, class); err != nil {
			return err
		}
		before := class.DeepCopy()
		class.Status.ObservedGeneration = class.Generation
		if profileHash != "" && class.Status.ProfileHash == "" {
			class.Status.ProfileHash = profileHash
		}
		if providerName == "" {
			class.Status.ProviderRef = nil
		} else {
			class.Status.ProviderRef = &workspacev1alpha1.ClusterObjectReference{Name: providerName}
		}
		workspaceprovider.SetCondition(&class.Status.Conditions, condition)
		return r.Status().Patch(ctx, class, client.MergeFrom(before))
	})
}

// SetupWithManager registers the class reference/readiness controller.
func (r *ExecutionWorkspaceClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspaceClass{}).
		Named("execution-workspace-class-core").
		Complete(r)
}

func featureSetContainsAll(have, required []workspacev1alpha1.ExecutionWorkspaceFeature) bool {
	for _, feature := range required {
		if !slices.Contains(have, feature) {
			return false
		}
	}
	return true
}
