/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"slices"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	workspaceParametersRefField = "parametersRef"
	workspaceUIDField           = "uid"
)

func externalACPProvider(provider *workspacev1alpha1.ExecutionWorkspaceProvider) bool {
	return provider.Spec.ControllerName != acpWorkspaceProviderControllerName &&
		slices.Contains(provider.Spec.RequiredContracts, workspacev1alpha1.LifecycleContractV1)
}

// resolveExternalWorkspaceParameters keeps provider APIs out of Orka's scheme.
// Their installed schema and adapter validate native inputs; core freezes the
// functional spec and exact object revision before admitting a workload.
func resolveExternalWorkspaceParameters(ctx context.Context, reader client.Reader, mapper apimeta.RESTMapper,
	namespace string, ref *workspacev1alpha1.TypedObjectReference, scope apimeta.RESTScopeName,
) (*unstructured.Unstructured, error) {
	if ref == nil || ref.Group == "" || ref.Kind == "" || ref.Name == "" || mapper == nil {
		return nil, fmt.Errorf("provider parameters require a complete reference and REST mapper")
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: ref.Group, Kind: ref.Kind})
	if err != nil {
		return nil, fmt.Errorf("resolve provider parameter kind: %w", err)
	}
	if mapping.Scope.Name() != scope {
		return nil, fmt.Errorf("provider parameter reference has invalid scope")
	}
	if scope == apimeta.RESTScopeNameRoot {
		namespace = ""
	}
	object := &unstructured.Unstructured{}
	object.SetGroupVersionKind(mapping.GroupVersionKind)
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, object); err != nil {
		wrapped := fmt.Errorf("read provider parameters: %w", err)
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return nil, wrapped
		}
		return nil, markRetryableACPWorkspaceClassResolution(wrapped)
	}
	if object.GetDeletionTimestamp() != nil || object.GetUID() == "" || object.GetGeneration() < 1 {
		return nil, fmt.Errorf("provider parameters are deleting or have no immutable identity")
	}
	return object, nil
}

func externalWorkspaceParametersBinding(object *unstructured.Unstructured) (*workspacev1alpha1.ImmutableObjectBinding, error) {
	hash, err := workspaceprovider.ParametersProfileHash(object)
	if err != nil {
		return nil, err
	}
	return &workspacev1alpha1.ImmutableObjectBinding{Name: object.GetName(), UID: object.GetUID(), Generation: object.GetGeneration(), ProfileHash: hash}, nil
}

func validateExternalACPProvider(ctx context.Context, c client.Client, provider *workspacev1alpha1.ExecutionWorkspaceProvider) error {
	if !workspaceProviderNameSupportsRouting(provider.Name) {
		return fmt.Errorf("%s", messageProviderNameUnsupported)
	}
	if provider.Spec.ControllerName == acpWorkspaceControllerLabelValue {
		return fmt.Errorf("provider controllerName is reserved for the legacy ACP controller")
	}
	if len(provider.Spec.ControllerName) > 63 || len(validation.IsDNS1123Subdomain(provider.Spec.ControllerName)) != 0 {
		return fmt.Errorf("provider controllerName must be a DNS-compatible routing identity")
	}
	if !slices.Contains(provider.Spec.RequiredContracts, workspacev1alpha1.LifecycleContractV1) ||
		!slices.Contains(provider.Status.SupportedContracts, workspacev1alpha1.LifecycleContractV1) ||
		!slices.Contains(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureACPRuntime) {
		return fmt.Errorf("provider must advertise %s and acp.runtime.v2", workspacev1alpha1.LifecycleContractV1)
	}
	principal := provider.Spec.ServiceAccountRef
	if principal == nil || len(validation.IsDNS1123Label(principal.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(principal.Name)) != 0 {
		return fmt.Errorf("provider requires an installed serviceAccountRef")
	}
	review := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
		User:   "system:serviceaccount:" + principal.Namespace + ":" + principal.Name,
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + principal.Namespace, "system:authenticated"},
		ResourceAttributes: &authorizationv1.ResourceAttributes{Group: workspacev1alpha1.GroupVersion.Group,
			Resource: "executionworkspaceproviders", Name: provider.Name, Verb: "provider-status"},
	}}
	if err := c.Create(ctx, review); err != nil {
		return markRetryableACPWorkspaceClassResolution(fmt.Errorf("authorize provider status principal: %w", err))
	}
	if review.Status.EvaluationError != "" {
		return markRetryableACPWorkspaceClassResolution(fmt.Errorf("provider status authorization could not be evaluated"))
	}
	if !review.Status.Allowed || review.Status.Denied {
		return fmt.Errorf("provider ServiceAccount lacks provider-status authorization for this registration")
	}
	return nil
}

func externalACPProfilePolicy(profile *unstructured.Unstructured, class *workspacev1alpha1.ExecutionWorkspaceClass) (string, *int32, error) {
	mode, _, err := unstructured.NestedString(profile.Object, "spec", "suspend", "mode")
	if err != nil || (mode != "" && mode != "DataOnly") {
		return "", nil, fmt.Errorf("profile suspension must preserve data only")
	}
	var cap *int32
	raw, exists, err := unstructured.NestedInt64(profile.Object, "spec", "retention", "maxSuspendedWorkspaces")
	if err != nil || (exists && (raw < 0 || raw > 2147483647)) {
		return "", nil, fmt.Errorf("profile retention cap is invalid")
	}
	if exists {
		value := int32(raw)
		cap = &value
	}
	suspend := slices.Contains(class.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	if suspend && (mode == "" || !slices.Contains(class.Spec.AllowedReuseScopes, workspacev1alpha1.WorkspaceReuseScopeSession)) {
		return "", nil, fmt.Errorf("suspension requires a DataOnly profile and Session reuse")
	}
	if suspend && class.Spec.Lifecycle.MaxLifetime == nil && (class.Spec.Lifecycle.IdleTimeout == nil || cap != nil) {
		return "", nil, fmt.Errorf("suspended workspace lifetime must be bounded")
	}
	if cap != nil && *cap == 0 && class.Spec.Lifecycle.DefaultOnDetach == workspacev1alpha1.WorkspaceOnDetachSuspend {
		return "", nil, fmt.Errorf("default suspension cannot use a zero retention cap")
	}
	return mode, cap, nil
}

func externalACPClassProfileHash(class *workspacev1alpha1.ExecutionWorkspaceClass, provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	config, parameters *unstructured.Unstructured,
) (string, error) {
	required := append([]string(nil), provider.Spec.RequiredContracts...)
	slices.Sort(required)
	configBinding, err := externalWorkspaceParametersBinding(config)
	if err != nil {
		return "", err
	}
	parameterBinding, err := externalWorkspaceParametersBinding(parameters)
	if err != nil {
		return "", err
	}
	return workspaceprovider.ClassProfileHash(class.Spec, map[string]any{
		workspaceUIDField: provider.UID, "controllerName": provider.Spec.ControllerName, workspaceParametersRefField: provider.Spec.ParametersRef,
		"serviceAccountRef": provider.Spec.ServiceAccountRef, "requiredContracts": required,
		"lifecycleContractVersion": workspacev1alpha1.LifecycleContractV1, "configBinding": configBinding,
	}, map[string]any{workspaceParametersRefField: class.Spec.ParametersRef, "binding": parameterBinding})
}

//nolint:gocyclo // Preserve each independent class, capability, authorization and continuation fence.
func (r *TaskReconciler) resolveExternalACPWorkspaceClass(ctx context.Context, reader client.Reader,
	task *corev1alpha1.Task, class *workspacev1alpha1.ExecutionWorkspaceClass,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider, workspaceSessionUID string, continuation bool,
	required []workspacev1alpha1.ExecutionWorkspaceFeature,
) (*acpResolvedWorkspaceClass, error) {
	if task.Spec.Execution.Workspace.RestoreFrom != nil && !slices.Contains(required, workspacev1alpha1.WorkspaceFeatureCheckpoint) {
		required = append(slices.Clone(required), workspacev1alpha1.WorkspaceFeatureCheckpoint)
	}
	providerGeneration := provider.Generation
	if continuation {
		reuse, slot, sessionUID, _, err := resolveACPWorkspaceSessionScope(task, workspaceSessionUID)
		if err != nil {
			return nil, err
		}
		probe := &ACPRuntimeWorkspaceBinding{ReusePolicy: reuse, WorkspaceSlot: slot, SessionUID: sessionUID, Class: &ACPWorkspaceClassBinding{UID: string(class.UID)}}
		existing := &workspacev1alpha1.ExecutionWorkspace{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, probe)}, existing); err != nil {
			return nil, err
		}
		if existing.Spec.ProviderBinding.UID != provider.UID || existing.Spec.ProviderBinding.Name != provider.Name ||
			existing.Labels[workspacev1alpha1.ProviderControllerLabel] != provider.Spec.ControllerName {
			return nil, fmt.Errorf("continuation workspace belongs to a different provider installation")
		}
		providerGeneration = existing.Spec.ProviderBinding.Generation
	}
	if provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDisabled ||
		(provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderActive && !continuation) {
		return nil, fmt.Errorf("execution workspace provider %q is %s and rejects this workspace identity", provider.Name, provider.Spec.LifecycleState)
	}
	if provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderActive && provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderDraining {
		return nil, fmt.Errorf("provider lifecycle state is invalid")
	}
	if err := validateExternalACPProvider(ctx, r.Client, provider); err != nil {
		return nil, err
	}
	ready := workspaceprovider.FindCondition(provider.Status.Conditions, string(workspacev1alpha1.ConditionProviderReady))
	drainingReady := continuation && provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDraining && ready != nil && ready.Reason == string(workspacev1alpha1.ReasonProviderDraining)
	if drainingReady {
		for _, conditionType := range []workspacev1alpha1.ExecutionWorkspaceConditionType{workspacev1alpha1.ConditionProviderHeartbeat, workspacev1alpha1.ConditionProviderCompatible} {
			condition := workspaceprovider.FindCondition(provider.Status.Conditions, string(conditionType))
			if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != provider.Generation {
				drainingReady = false
			}
		}
	}
	if provider.Status.ObservedGeneration != provider.Generation || ready == nil || ready.ObservedGeneration != provider.Generation ||
		(ready.Status != metav1.ConditionTrue && !drainingReady) {
		return nil, fmt.Errorf("execution workspace provider %q is not ready at its current generation", provider.Name)
	}
	if !featureSetContainsAll(provider.Status.SupportedFeatures, required) {
		return nil, fmt.Errorf("provider does not support every required feature")
	}
	allowed, err := namespaceAllowedByWorkspaceProvider(ctx, reader, class.Namespace, provider)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, fmt.Errorf("provider usage policy rejects this namespace")
	}
	mapper := r.RESTMapper()
	config, err := resolveExternalWorkspaceParameters(ctx, reader, mapper, "", &provider.Spec.ParametersRef, apimeta.RESTScopeNameRoot)
	if err != nil {
		return nil, err
	}
	if provider.Status.PinnedParametersUID != string(config.GetUID()) {
		return nil, fmt.Errorf("provider parameters do not match its protected UID pin")
	}
	profile, err := resolveExternalWorkspaceParameters(ctx, reader, mapper, class.Namespace, class.Spec.ParametersRef, apimeta.RESTScopeNameNamespace)
	if err != nil {
		return nil, err
	}
	hash, err := externalACPClassProfileHash(class, provider, config, profile)
	if err != nil {
		return nil, err
	}
	if hash != class.Status.ProfileHash {
		return nil, fmt.Errorf("class profile drifted from its pinned hash; create a new class")
	}
	mode, cap, err := externalACPProfilePolicy(profile, class)
	if err != nil {
		return nil, err
	}
	configBinding, err := externalWorkspaceParametersBinding(config)
	if err != nil {
		return nil, err
	}
	parametersBinding, err := externalWorkspaceParametersBinding(profile)
	if err != nil {
		return nil, err
	}
	resolved := &acpResolvedWorkspaceClass{Backend: corev1alpha1.WorkspaceProvider(provider.Spec.ControllerName), Mode: class.Spec.Mode,
		AllowedReuseScopes: slices.Clone(class.Spec.AllowedReuseScopes), AllowedOnDetach: slices.Clone(class.Spec.Lifecycle.AllowedOnDetach), DefaultOnDetach: class.Spec.Lifecycle.DefaultOnDetach,
		Binding: ACPWorkspaceClassBinding{Name: class.Name, UID: string(class.UID), Generation: class.Generation, ProfileHash: hash,
			ProviderName: provider.Name, ProviderUID: string(provider.UID), ProviderGeneration: providerGeneration, ProviderConfigUID: string(config.GetUID()),
			ControllerName: provider.Spec.ControllerName, LifecycleContractVersion: workspacev1alpha1.LifecycleContractV1,
			ProviderConfigRef: provider.Spec.ParametersRef.DeepCopy(), ProviderConfigBinding: configBinding,
			ParametersRef: class.Spec.ParametersRef.DeepCopy(), ParametersBinding: parametersBinding,
			SuspendMode: mode, MaxSuspendedWorkspaces: cap, DefaultOnDetach: string(class.Spec.Lifecycle.DefaultOnDetach),
			AllowedOnDetach: onDetachActionsToStrings(class.Spec.Lifecycle.AllowedOnDetach), DetachTimeout: class.Spec.Lifecycle.DetachTimeout.Duration.String(),
			DeletionPolicy: ACPWorkspaceClassDeletionPolicy{ProviderResources: string(class.Spec.Lifecycle.DeletionPolicy.ProviderResources), PersistentVolumes: string(class.Spec.Lifecycle.DeletionPolicy.PersistentVolumes), Checkpoints: string(class.Spec.Lifecycle.DeletionPolicy.Checkpoints)},
		}}
	if class.Spec.Lifecycle.IdleTimeout != nil {
		resolved.Binding.IdleTimeout = class.Spec.Lifecycle.IdleTimeout.Duration.String()
	}
	if class.Spec.Lifecycle.MaxLifetime != nil {
		resolved.Binding.MaxLifetime = class.Spec.Lifecycle.MaxLifetime.Duration.String()
	}
	if resolved.Binding.DeletionPolicy.ProviderResources != string(workspacev1alpha1.WorkspaceDeletionActionDelete) || resolved.Binding.DeletionPolicy.PersistentVolumes != string(workspacev1alpha1.WorkspaceDeletionActionDelete) || resolved.Binding.DeletionPolicy.Checkpoints != string(workspacev1alpha1.WorkspaceDeletionActionDelete) {
		return nil, fmt.Errorf("ACP workspace deletion policy must delete every category")
	}
	if err := r.enforceACPWorkspaceSuspendQuota(ctx, reader, task, class, resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

// Checkpoint use is authorized against the live caller by Task admission. A
// fresh destination also rechecks the public artifact identity before creation;
// it need not read the source workspace, which may already have been deleted.
func (r *TaskReconciler) validateACPWorkspaceRestoreCheckpoint(ctx context.Context, task *corev1alpha1.Task, binding *ACPRuntimeWorkspaceBinding) error {
	if binding == nil || binding.RestoreFrom == nil || binding.Class == nil || binding.Class.ControllerName == "" {
		return nil
	}
	if err := validateACPWorkspaceRestoreReference(binding); err != nil {
		return err
	}
	checkpoint := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{}
	ref := binding.RestoreFrom
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: ref.Name}, checkpoint); err != nil {
		wrapped := fmt.Errorf("resolve restore checkpoint: %w", err)
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			return wrapped
		}
		return markRetryableACPWorkspaceClassResolution(wrapped)
	}
	class := binding.Class
	expectedClass := workspacev1alpha1.ImmutableObjectBinding{Name: class.Name, UID: types.UID(class.UID), Generation: class.Generation, ProfileHash: class.ProfileHash}
	ready := workspaceprovider.FindCondition(checkpoint.Status.Conditions, "Ready")
	if string(checkpoint.UID) != ref.UID || checkpoint.Status.Digest != ref.Digest || !checkpoint.DeletionTimestamp.IsZero() ||
		checkpoint.Status.Phase != "Ready" || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != checkpoint.Generation ||
		checkpoint.Status.ClassBinding == nil || *checkpoint.Status.ClassBinding != expectedClass {
		return fmt.Errorf("restoreFrom does not identify a current Ready checkpoint at the exact UID, digest, and class binding")
	}
	return nil
}
