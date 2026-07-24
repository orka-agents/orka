package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const (
	FakeWorkspaceControllerName = "fake.workspace.orka.ai/v1"
	fakeWorkspaceAdapterVersion = "0.1.0-dev"
	fakeProviderHeartbeatPeriod = 20 * time.Second
)

// FakeExecutionWorkspaceProviderReconciler is a status-only reference adapter
// used by envtest and development. It owns only providers with the fake controllerName.
type FakeExecutionWorkspaceProviderReconciler struct {
	client.Client
	Now func() time.Time
}

func (r *FakeExecutionWorkspaceProviderReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.Get(ctx, req.NamespacedName, provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if provider.Spec.ControllerName != FakeWorkspaceControllerName || !provider.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	before := provider.DeepCopy()
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.Adapter = &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: fakeWorkspaceAdapterVersion}
	provider.Status.Backend = &workspacev1alpha1.ExecutionWorkspaceBackendStatus{Version: "in-memory", APIVersions: []string{"fake.workspace.orka.ai/v1"}}
	provider.Status.SupportedContracts = []string{workspacev1alpha1.ContractVersionV1}
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeatureFiles,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureSuspend,
		workspacev1alpha1.WorkspaceFeatureServicePorts,
		workspacev1alpha1.WorkspaceFeaturePools,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	heartbeat := metav1.NewTime(now)
	provider.Status.LastHeartbeat = &heartbeat
	workspaceprovider.SetCondition(&provider.Status.Conditions, metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionProviderCompatible),
		Status:             metav1.ConditionTrue,
		Reason:             string(workspacev1alpha1.ReasonReady),
		Message:            "fake provider implements the required contract",
		ObservedGeneration: provider.Generation,
	})
	if err := r.Status().Patch(ctx, provider, client.MergeFrom(before)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: fakeProviderHeartbeatPeriod}, nil
}

func (r *FakeExecutionWorkspaceProviderReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspaceProvider{}).
		// Every reconcile writes a fresh LastHeartbeat. Without the generation filter that
		// status write feeds back as an update event and replaces the timed heartbeat with a
		// continuous reconcile loop; RequeueAfter still drives the heartbeat.
		WithEventFilter(predicate.And(
			workspaceprovider.ControllerNamePredicate(FakeWorkspaceControllerName),
			predicate.GenerationChangedPredicate{},
		)).
		Named("fake-execution-workspace-provider").
		Complete(r)
}

// FakeExecutionWorkspacePoolReconciler derives deterministic pool counts from
// concrete workspaces. It does not create provider-native resources.
type FakeExecutionWorkspacePoolReconciler struct {
	client.Client
}

func (r *FakeExecutionWorkspacePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	pool := &workspacev1alpha1.ExecutionWorkspacePool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owned, err := fakeProviderOwns(ctx, r.Client, pool.Spec.ProviderRef.Name)
	if err != nil || !owned {
		return ctrl.Result{}, err
	}
	allocated, suspended, err := r.countPoolWorkspaces(ctx, pool)
	if err != nil {
		return ctrl.Result{}, err
	}
	total := min(pool.Spec.Capacity.MinReady+allocated, pool.Spec.Capacity.MaxSize)
	// A downsize never evicts leased workspaces; total may temporarily exceed maxSize.
	total = max(total, allocated)
	available := max(total-allocated, 0)
	return ctrl.Result{RequeueAfter: fakeProviderHeartbeatPeriod}, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &workspacev1alpha1.ExecutionWorkspacePool{}
		if err := r.Get(ctx, req.NamespacedName, current); err != nil {
			return err
		}
		before := current.DeepCopy()
		current.Status.ObservedGeneration = current.Generation
		current.Status.Available = available
		current.Status.Allocated = allocated
		current.Status.Suspended = suspended
		current.Status.Total = total
		workspaceprovider.SetCondition(&current.Status.Conditions, metav1.Condition{
			Type:               string(workspacev1alpha1.ConditionPoolReady),
			Status:             metav1.ConditionTrue,
			Reason:             string(workspacev1alpha1.ReasonReady),
			Message:            "fake pool capacity is reconciled",
			ObservedGeneration: current.Generation,
		})
		admitted := allocated < current.Spec.Capacity.MaxSize
		workspaceprovider.SetCondition(&current.Status.Conditions, metav1.Condition{
			Type:               string(workspacev1alpha1.ConditionPoolAdmitted),
			Status:             conditionStatus(admitted),
			Reason:             conditionReason(admitted, string(workspacev1alpha1.ReasonCapacityUnavailable)),
			Message:            chooseMessage(admitted, "pool has allocation capacity", "pool capacity is exhausted"),
			ObservedGeneration: current.Generation,
		})
		return r.Status().Patch(ctx, current, client.MergeFrom(before))
	})
}

func (r *FakeExecutionWorkspacePoolReconciler) countPoolWorkspaces(
	ctx context.Context,
	pool *workspacev1alpha1.ExecutionWorkspacePool,
) (int32, int32, error) {
	var workspaces workspacev1alpha1.ExecutionWorkspaceList
	if err := r.List(ctx, &workspaces, client.InNamespace(pool.Namespace)); err != nil {
		return 0, 0, err
	}
	var allocated, suspended int32
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		class := &workspacev1alpha1.ExecutionWorkspaceClass{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: workspace.Spec.ClassBinding.Name}, class); err != nil {
			continue
		}
		if class.Spec.PoolRef == nil || class.Spec.PoolRef.Name != pool.Name {
			continue
		}
		if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleted || workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateDeleting {
			continue
		}
		allocated++
		if workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateSuspended {
			suspended++
		}
	}
	return allocated, suspended, nil
}

func (r *FakeExecutionWorkspacePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspacePool{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("fake-execution-workspace-pool").
		Complete(r)
}

// FakeExecutionWorkspaceReconciler implements the generic lifecycle entirely in
// status and is intentionally free of provider-native branches.
type FakeExecutionWorkspaceReconciler struct {
	client.Client
}

func (r *FakeExecutionWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	owned, err := fakeProviderOwns(ctx, r.Client, workspace.Spec.ProviderBinding.Name)
	if err != nil || !owned {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &workspacev1alpha1.ExecutionWorkspace{}
		if err := r.Get(ctx, req.NamespacedName, current); err != nil {
			return err
		}
		before := current.DeepCopy()
		current.Status.ObservedGeneration = current.Generation
		current.Status.ExternalID = fmt.Sprintf("fake/%s/%s", current.Namespace, current.Name)
		current.Status.ProviderBinding = &workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus{
			ContractVersion:   workspacev1alpha1.ContractVersionV1,
			AdapterVersion:    fakeWorkspaceAdapterVersion,
			BackendAPIVersion: "fake.workspace.orka.ai/v1",
		}
		current.Status.Endpoints = nil
		current.Status.Disposition = nil

		switch current.Spec.DesiredState {
		case workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined:
			current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateQuarantined
			current.Status.AttachedEpoch = 0
		case workspacev1alpha1.ExecutionWorkspaceDesiredDeleted:
			current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateDeleted
			current.Status.AttachedEpoch = 0
			current.Status.Disposition = fakeDeletedDisposition(current.Spec.Lifecycle.DeletionPolicy)
		case workspacev1alpha1.ExecutionWorkspaceDesiredSuspended:
			current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
			current.Status.AttachedEpoch = 0
		default:
			if current.Spec.Attachment != nil {
				current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
				current.Status.AttachedEpoch = current.Spec.Attachment.Epoch
			} else {
				current.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
				current.Status.AttachedEpoch = 0
			}
		}
		if current.Spec.Mode == workspacev1alpha1.ExecutionWorkspaceModeService && current.Spec.Service != nil && current.Status.State != workspacev1alpha1.ExecutionWorkspaceStateDeleted {
			for _, port := range current.Spec.Service.Ports {
				scheme := "http"
				if port.Protocol == "HTTPS" {
					scheme = "https"
				}
				current.Status.Endpoints = append(current.Status.Endpoints, workspacev1alpha1.ExecutionWorkspaceEndpoint{
					Name:     port.Name,
					URL:      fmt.Sprintf("%s://%s.%s.svc:%d", scheme, current.Name, current.Namespace, port.Port),
					Protocol: port.Protocol,
				})
			}
		}
		dataReady := current.Status.State == workspacev1alpha1.ExecutionWorkspaceStateReady || current.Status.State == workspacev1alpha1.ExecutionWorkspaceStateAttached
		workspaceprovider.SetCondition(&current.Status.Conditions, metav1.Condition{
			Type:               string(workspacev1alpha1.ConditionWorkspaceDataPlaneReady),
			Status:             conditionStatus(dataReady),
			Reason:             conditionReason(dataReady, string(workspacev1alpha1.ReasonProgressing)),
			Message:            chooseMessage(dataReady, "fake workspace data plane is ready", "fake workspace data plane is not ready"),
			ObservedGeneration: current.Generation,
		})
		attached := current.Status.State == workspacev1alpha1.ExecutionWorkspaceStateAttached
		workspaceprovider.SetCondition(&current.Status.Conditions, metav1.Condition{
			Type:               string(workspacev1alpha1.ConditionWorkspaceAttached),
			Status:             conditionStatus(attached),
			Reason:             conditionReason(attached, string(workspacev1alpha1.ReasonAttachmentRevoked)),
			Message:            chooseMessage(attached, "attachment epoch is active", "no attachment epoch is active"),
			ObservedGeneration: current.Generation,
		})
		// Core writes its own conditions on this workspace. Optimistic locking turns a
		// concurrent write into a retryable conflict instead of silently replacing the
		// core-owned entries in the conditions array.
		return r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *FakeExecutionWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		Named("fake-execution-workspace").
		Complete(r)
}

func fakeProviderOwns(ctx context.Context, c client.Client, name string) (bool, error) {
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, provider); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return provider.Spec.ControllerName == FakeWorkspaceControllerName, nil
}

func fakeDeletedDisposition(policy workspacev1alpha1.ExecutionWorkspaceDeletionPolicy) *workspacev1alpha1.ExecutionWorkspaceDisposition {
	retainedOrDeleted := func(action workspacev1alpha1.WorkspaceDeletionAction) workspacev1alpha1.ExecutionWorkspaceDispositionState {
		if action == workspacev1alpha1.WorkspaceDeletionActionRetain {
			return workspacev1alpha1.DispositionRetained
		}
		return workspacev1alpha1.DispositionDeleted
	}
	disposition := &workspacev1alpha1.ExecutionWorkspaceDisposition{
		Compute:           workspacev1alpha1.DispositionDeleted,
		WorkspaceData:     workspacev1alpha1.DispositionDeleted,
		PersistentVolumes: retainedOrDeleted(policy.PersistentVolumes),
		Checkpoints:       retainedOrDeleted(policy.Checkpoints),
		ProviderResources: retainedOrDeleted(policy.ProviderResources),
	}
	setAccessDisposition(disposition, workspacev1alpha1.DispositionRevoked)
	setEphemeralDisposition(disposition, workspacev1alpha1.DispositionDeleted)
	return disposition
}

func setAccessDisposition(
	disposition *workspacev1alpha1.ExecutionWorkspaceDisposition,
	state workspacev1alpha1.ExecutionWorkspaceDispositionState,
) {
	disposition.AccessCredentials = state
}

func setEphemeralDisposition(
	disposition *workspacev1alpha1.ExecutionWorkspaceDisposition,
	state workspacev1alpha1.ExecutionWorkspaceDispositionState,
) {
	disposition.EphemeralSecrets = state
}
