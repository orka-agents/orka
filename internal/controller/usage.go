package controller

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func usageTaskSnapshot(task *corev1alpha1.Task) store.UsageTask {
	phase := task.Status.Phase
	if phase == "" {
		phase = corev1alpha1.TaskPhasePending
	}
	result := store.UsageTask{Namespace: task.Namespace, TaskUID: string(task.UID), TaskName: task.Name,
		Phase: string(phase), PhaseAttempt: task.Status.Attempts, Runtime: string(task.Spec.Type), StartedAt: task.CreationTimestamp.Time}
	result.PhaseObservedAt = time.Now().UTC()
	if phase == corev1alpha1.TaskPhasePending {
		result.PhaseObservedAt = task.CreationTimestamp.Time
		if retry := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated); retry != nil &&
			retry.Status == metav1.ConditionFalse && retry.Reason == taskRetryPendingReason && !retry.LastTransitionTime.IsZero() {
			result.PhaseObservedAt = retry.LastTransitionTime.Time
		}
	}
	if phase == corev1alpha1.TaskPhaseRunning && task.Status.StartTime != nil {
		result.PhaseObservedAt = task.Status.StartTime.Time
	}
	// A delayed Finalizing snapshot must retain its transition time rather
	// than appear newer than an already-recorded terminal snapshot.
	if outcome := task.Status.ExecutionOutcome; phase == corev1alpha1.TaskPhaseFinalizing && outcome != nil && !outcome.RecordedAt.IsZero() {
		result.PhaseObservedAt = outcome.RecordedAt.Time
	}
	if task.Status.CompletionTime != nil {
		result.PhaseObservedAt = task.Status.CompletionTime.Time
	}
	// Deleting an unfinished Task cancels its retained accounting lifecycle.
	// Otherwise a snapshot can pin its cohort forever after the Task disappears.
	if !task.DeletionTimestamp.IsZero() && phase != corev1alpha1.TaskPhaseSucceeded && phase != corev1alpha1.TaskPhaseFailed && phase != corev1alpha1.TaskPhaseCancelled {
		result.Phase = string(corev1alpha1.TaskPhaseCancelled)
		result.PhaseObservedAt = task.DeletionTimestamp.Time
		if outcome := task.Status.ExecutionOutcome; outcome != nil {
			result.Phase = string(outcome.Phase)
			if !outcome.RecordedAt.IsZero() {
				result.PhaseObservedAt = outcome.RecordedAt.Time
			}
		}
	}
	if task.Spec.SessionRef != nil {
		result.SessionName = task.Spec.SessionRef.Name
	}
	if owner := metav1.GetControllerOf(task); owner != nil && owner.Kind == taskResourceKind && owner.APIVersion == corev1alpha1.GroupVersion.String() {
		result.ParentTaskUID = string(owner.UID)
	}
	if owner, ok := gateway.TaskOwner(task); ok {
		result.GatewayOwner = &store.UsageGatewayOwner{Namespace: owner.GatewayNamespace, NamespaceUID: owner.NamespaceUID,
			Name: owner.GatewayName, UID: owner.GatewayUID}
	}
	return result
}

func (r *TaskReconciler) retainUsageTask(ctx context.Context, task *corev1alpha1.Task) error {
	usageStore, ok := r.ExecutionEventStore.(store.UsageStore)
	if !ok || task.UID == "" || task.Spec.Schedule != "" {
		return nil
	}
	uid, err := usage.NamespaceUIDForObject(ctx, uncachedReader(r.APIReader, r.Client), task)
	if err != nil {
		return err
	}
	snapshot := usageTaskSnapshot(task)
	snapshot.NamespaceUID = uid
	return usageStore.RegisterUsageTask(ctx, snapshot)
}

func (r *RepositoryMonitorReconciler) prepareMonitorUsageWork(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, repository, kind string, number int64) (string, error) {
	usageStore, ok := r.Store.(store.UsageStore)
	if !ok || monitor.UID == "" {
		return "", nil
	}
	id := store.UsageWorkID(monitor.Namespace, string(monitor.UID), repository, kind, number)
	uid, err := usage.NamespaceUIDForObject(ctx, uncachedReader(r.APIReader, r.Client), monitor)
	if err != nil {
		return "", err
	}
	err = usageStore.RegisterUsageWork(ctx, store.UsageWorkRequest{ID: id, Namespace: monitor.Namespace, NamespaceUID: uid, MonitorName: monitor.Name,
		MonitorUID: string(monitor.UID), Repository: repository, Kind: kind, Number: number, StartedAt: time.Now().UTC()})
	return id, err
}

func (r *RepositoryMonitorReconciler) retainMonitorUsageTask(ctx context.Context, task *corev1alpha1.Task, workID, role string, prNumber int64) error {
	usageStore, ok := r.Store.(store.UsageStore)
	if !ok || workID == "" || task.UID == "" {
		return nil
	}
	snapshot := usageTaskSnapshot(task)
	snapshot.WorkID, snapshot.Role, snapshot.PRNumber = workID, role, prNumber
	return usageStore.RegisterUsageTask(ctx, snapshot)
}
