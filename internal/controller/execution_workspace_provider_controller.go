package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const (
	executionWorkspaceProviderFinalizer = "workspace.orka.ai/provider-protection"
	defaultProviderHeartbeatTimeout     = 2 * time.Minute
	defaultProviderHeartbeatFutureSkew  = 30 * time.Second
	defaultProviderHeartbeatCheck       = 30 * time.Second
)

// ExecutionWorkspaceProviderReconciler evaluates generic provider usability and
// prevents deletion while pools or concrete workspaces remain bound to a provider.
// Adapter-owned status fields are preserved.
type ExecutionWorkspaceProviderReconciler struct {
	client.Client
	APIReader        client.Reader
	RESTMapper       apimeta.RESTMapper
	HeartbeatTimeout time.Duration
	Now              func() time.Time
	CleanupOnly      bool
}

//nolint:gocyclo // Provider readiness and deletion protection are explicit fail-closed state gates.
func (r *ExecutionWorkspaceProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.Get(ctx, req.NamespacedName, provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.CleanupOnly && provider.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if !provider.DeletionTimestamp.IsZero() {
		blocked, err := r.providerHasReferences(ctx, provider)
		if err != nil {
			return ctrl.Result{}, err
		}
		if blocked {
			if err := r.patchProviderCondition(ctx, provider.Name, metav1.Condition{
				Type:               string(workspacev1alpha1.ConditionProviderReady),
				Status:             metav1.ConditionFalse,
				Reason:             "ReferencesRemain",
				Message:            "provider deletion is blocked by bound pools or workspaces",
				ObservedGeneration: provider.Generation,
			}); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: defaultProviderHeartbeatCheck}, nil
		}
		if controllerutil.ContainsFinalizer(provider, executionWorkspaceProviderFinalizer) {
			controllerutil.RemoveFinalizer(provider, executionWorkspaceProviderFinalizer)
			if err := r.Update(ctx, provider); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(provider, executionWorkspaceProviderFinalizer) {
		controllerutil.AddFinalizer(provider, executionWorkspaceProviderFinalizer)
		if err := r.Update(ctx, provider); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	timeout := r.HeartbeatTimeout
	if timeout <= 0 {
		timeout = defaultProviderHeartbeatTimeout
	}

	heartbeatFresh := false
	if provider.Status.LastHeartbeat != nil {
		heartbeatAge := now.Sub(provider.Status.LastHeartbeat.Time)
		heartbeatFresh = heartbeatAge >= -defaultProviderHeartbeatFutureSkew && heartbeatAge <= timeout
	}
	adapterObserved := provider.Status.ObservedGeneration == provider.Generation
	adapterIdentified := provider.Status.Adapter != nil && strings.TrimSpace(provider.Status.Adapter.Version) != ""
	contractsCompatible := slices.Contains(
		provider.Spec.RequiredContracts,
		workspacev1alpha1.ContractVersionV1,
	) && slices.Contains(
		provider.Status.SupportedContracts,
		workspacev1alpha1.ContractVersionV1,
	) && stringSetContainsAll(provider.Status.SupportedContracts, provider.Spec.RequiredContracts)
	parametersValid, parametersReason, parametersMessage, err := r.providerParametersClusterScoped(provider)
	if err != nil {
		return ctrl.Result{}, err
	}
	ready := heartbeatFresh && adapterObserved && adapterIdentified && contractsCompatible && parametersValid &&
		provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderActive

	conditions := []metav1.Condition{
		{
			Type:               string(workspacev1alpha1.ConditionProviderHeartbeat),
			Status:             conditionStatus(heartbeatFresh),
			Reason:             conditionReason(heartbeatFresh, string(workspacev1alpha1.ReasonHeartbeatExpired)),
			Message:            chooseMessage(heartbeatFresh, "provider heartbeat is fresh", "provider heartbeat is missing or expired"),
			ObservedGeneration: provider.Generation,
		},
		{
			Type:               string(workspacev1alpha1.ConditionProviderCompatible),
			Status:             conditionStatus(contractsCompatible),
			Reason:             conditionReason(contractsCompatible, string(workspacev1alpha1.ReasonIncompatibleContract)),
			Message:            chooseMessage(contractsCompatible, "provider supports all required contracts", "provider does not support every required contract"),
			ObservedGeneration: provider.Generation,
		},
	}
	readyReason := string(workspacev1alpha1.ReasonReady)
	readyMessage := "provider accepts new workspaces"
	if provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDraining {
		readyReason = string(workspacev1alpha1.ReasonProviderDraining)
		readyMessage = "provider is draining and rejects new workspaces"
	} else if provider.Spec.LifecycleState == workspacev1alpha1.ExecutionWorkspaceProviderDisabled {
		readyReason = string(workspacev1alpha1.ReasonProviderDisabled)
		readyMessage = "provider is disabled and permits cleanup only"
	} else if !heartbeatFresh {
		readyReason = string(workspacev1alpha1.ReasonHeartbeatExpired)
		readyMessage = "provider heartbeat is missing or expired"
	} else if !adapterObserved {
		readyReason = "AdapterObservationStale"
		readyMessage = "provider adapter has not observed the current spec generation"
	} else if !adapterIdentified {
		readyReason = "AdapterIdentityMissing"
		readyMessage = "provider adapter version identity is missing"
	} else if !contractsCompatible {
		readyReason = string(workspacev1alpha1.ReasonIncompatibleContract)
		readyMessage = "provider must require and advertise the workspace.orka.ai/v1 contract"
	} else if !parametersValid {
		readyReason = parametersReason
		readyMessage = parametersMessage
	}
	conditions = append(conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionProviderReady),
		Status:             conditionStatus(ready),
		Reason:             readyReason,
		Message:            readyMessage,
		ObservedGeneration: provider.Generation,
	})

	if err := r.patchProviderConditions(ctx, provider.Name, conditions); err != nil {
		log.Error(err, "failed to patch provider conditions")
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: defaultProviderHeartbeatCheck}, nil
}

func (r *ExecutionWorkspaceProviderReconciler) providerParametersClusterScoped(
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
) (bool, string, string, error) {
	if r.RESTMapper == nil {
		return false, "ParametersResolverUnavailable", "provider parameter REST mapping is unavailable", nil
	}
	ref := provider.Spec.ParametersRef
	mapping, err := r.RESTMapper.RESTMapping(schema.GroupKind{Group: ref.Group, Kind: ref.Kind})
	if err != nil {
		if apimeta.IsNoMatchError(err) {
			return false, "ParametersKindUnavailable", "provider parameter kind is not installed", nil
		}
		return false, "", "", fmt.Errorf("resolve provider parameter kind: %w", err)
	}
	if mapping.Scope.Name() != apimeta.RESTScopeNameRoot {
		return false, reasonParametersScopeInvalid, "provider parametersRef must reference a cluster-scoped object", nil
	}
	return true, "", "", nil
}

func (r *ExecutionWorkspaceProviderReconciler) providerHasReferences(
	ctx context.Context,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var pools workspacev1alpha1.ExecutionWorkspacePoolList
	if err := reader.List(ctx, &pools); err != nil {
		return false, fmt.Errorf("list workspace pools: %w", err)
	}
	for i := range pools.Items {
		if pools.Items[i].Spec.ProviderRef.Name == provider.Name {
			return true, nil
		}
	}
	var classes workspacev1alpha1.ExecutionWorkspaceClassList
	if err := reader.List(ctx, &classes); err != nil {
		return false, fmt.Errorf("list workspace classes: %w", err)
	}
	for i := range classes.Items {
		if classes.Items[i].Spec.ProviderRef != nil &&
			classes.Items[i].Spec.ProviderRef.Name == provider.Name {
			return true, nil
		}
	}

	var workspaces workspacev1alpha1.ExecutionWorkspaceList
	if err := reader.List(ctx, &workspaces); err != nil {
		return false, fmt.Errorf("list execution workspaces: %w", err)
	}
	for i := range workspaces.Items {
		binding := workspaces.Items[i].Spec.ProviderBinding
		if binding.Name == provider.Name && (binding.UID == "" || binding.UID == provider.UID) {
			return true, nil
		}
	}
	return false, nil
}

func (r *ExecutionWorkspaceProviderReconciler) patchProviderCondition(ctx context.Context, name string, condition metav1.Condition) error {
	return r.patchProviderConditions(ctx, name, []metav1.Condition{condition})
}

func (r *ExecutionWorkspaceProviderReconciler) patchProviderConditions(ctx context.Context, name string, conditions []metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
		if err := r.Get(ctx, types.NamespacedName{Name: name}, provider); err != nil {
			return err
		}
		before := provider.DeepCopy()
		for _, condition := range conditions {
			workspaceprovider.SetCondition(&provider.Status.Conditions, condition)
		}
		return r.Status().Patch(ctx, provider, client.MergeFrom(before))
	})
}

// SetupWithManager registers the generic provider usability controller.
func (r *ExecutionWorkspaceProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspaceProvider{}).
		Named("execution-workspace-provider-core").
		Complete(r)
}

func stringSetContainsAll(have, required []string) bool {
	for _, value := range required {
		if !slices.Contains(have, value) {
			return false
		}
	}
	return true
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func conditionReason(ok bool, failure string) string {
	if ok {
		return string(workspacev1alpha1.ReasonReady)
	}
	return failure
}

func chooseMessage(ok bool, success, failure string) string {
	if ok {
		return success
	}
	return failure
}
