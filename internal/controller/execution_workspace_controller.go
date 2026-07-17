package controller

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
	CleanupOnly bool
}

func (r *ExecutionWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, req.NamespacedName, workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if r.CleanupOnly && workspace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	if err := r.projectWorkspaceToOwner(ctx, workspace); err != nil {
		return ctrl.Result{}, err
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return r.reconcileWorkspaceDeletion(ctx, workspace)
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
	admitted := reason == string(workspacev1alpha1.ReasonReady)
	condition := metav1.Condition{
		Type:               string(workspacev1alpha1.ConditionWorkspaceAdmitted),
		Status:             conditionStatus(admitted),
		Reason:             reason,
		Message:            message,
		ObservedGeneration: workspace.Generation,
	}
	if err := r.patchWorkspaceCondition(ctx, req.NamespacedName, condition); err != nil {
		return ctrl.Result{}, err
	}
	if !admitted {
		if err := r.quarantineWorkspace(ctx, workspace, message); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: workspaceRequeueInterval}, nil
}

func (r *ExecutionWorkspaceReconciler) validateWorkspaceBindings(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) (string, string, error) {
	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: workspace.Spec.ClassBinding.Name}, class); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "ClassNotFound", "bound workspace class does not exist", nil
		}
		return "", "", fmt.Errorf("get bound workspace class: %w", err)
	}
	if class.UID != workspace.Spec.ClassBinding.UID || class.Generation != workspace.Spec.ClassBinding.Generation {
		return "ClassBindingMismatch", "workspace class binding does not match the referenced class revision", nil
	}
	profileHash := class.Status.ProfileHash
	if profileHash == "" {
		var err error
		profileHash, err = workspaceprovider.ClassProfileHash(class.Spec)
		if err != nil {
			return "", "", fmt.Errorf("hash workspace class profile: %w", err)
		}
	}
	if profileHash != workspace.Spec.ClassBinding.ProfileHash {
		return "ClassProfileMismatch", "workspace class profile hash does not match the bound class", nil
	}
	if workspace.Spec.Mode != class.Spec.Mode || !reflect.DeepEqual(workspace.Spec.Lifecycle, class.Spec.Lifecycle) {
		return "ClassPolicyMismatch", "workspace mode or lifecycle does not match the bound class", nil
	}
	if workspace.Spec.SessionRef != nil && !slices.Contains(class.Spec.AllowedReuseScopes, workspacev1alpha1.WorkspaceReuseScopeSession) {
		return "ReuseScopeNotAllowed", "class does not allow Session reuse", nil
	}

	providerName, err := r.classProviderName(ctx, class)
	if err != nil {
		return "", "", err
	}
	if providerName == "" || providerName != workspace.Spec.ProviderBinding.Name {
		return "ProviderBindingMismatch", "workspace provider binding does not match the bound class", nil
	}
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.Get(ctx, types.NamespacedName{Name: providerName}, provider); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "ProviderNotFound", "bound workspace provider does not exist", nil
		}
		return "", "", fmt.Errorf("get bound workspace provider: %w", err)
	}
	if provider.UID != workspace.Spec.ProviderBinding.UID || provider.Generation < workspace.Spec.ProviderBinding.Generation {
		return "ProviderBindingMismatch", "workspace provider binding does not match the referenced provider identity", nil
	}
	if workspace.Status.State == "" && provider.Spec.LifecycleState != workspacev1alpha1.ExecutionWorkspaceProviderActive {
		return string(workspacev1alpha1.ReasonProviderDraining), "provider does not accept new workspaces", nil
	}
	return string(workspacev1alpha1.ReasonReady), "workspace bindings are valid", nil
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
	if err := r.Get(ctx, types.NamespacedName{Namespace: class.Namespace, Name: class.Spec.PoolRef.Name}, pool); err != nil {
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
		if current.Spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined && current.Labels[workspacev1alpha1.QuarantinedLabel] == scheduledRunLabelValue {
			return nil
		}
		before := current.DeepCopy()
		current.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
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
		return r.Status().Patch(ctx, workspace, client.MergeFrom(before))
	})
}

func (r *ExecutionWorkspaceReconciler) projectWorkspaceToOwner(
	ctx context.Context,
	workspace *workspacev1alpha1.ExecutionWorkspace,
) error {
	owner := metav1.GetControllerOf(workspace)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() {
		return nil
	}
	key := types.NamespacedName{Namespace: workspace.Namespace, Name: owner.Name}
	switch owner.Kind {
	case "Task":
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
			projection.State = string(workspace.Status.State)
			projection.AttachedEpoch = workspace.Status.AttachedEpoch
			projection.Conditions = append([]metav1.Condition(nil), workspace.Status.Conditions...)
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
				State:        string(workspace.Status.State),
				Conditions:   append([]metav1.Condition(nil), workspace.Status.Conditions...),
			}
			if len(workspace.Status.Endpoints) > 0 {
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
	if workspace == nil || !workspace.DeletionTimestamp.IsZero() || workspace.Spec.Attachment != nil {
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspace{}).
		Named("execution-workspace-core").
		Complete(r)
}
