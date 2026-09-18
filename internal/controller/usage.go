package controller

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func usageTaskSnapshot(task *corev1alpha1.Task) store.UsageTask {
	phase := string(task.Status.Phase)
	if phase == "" {
		phase = "Pending"
	}
	result := store.UsageTask{Namespace: task.Namespace, TaskUID: string(task.UID), TaskName: task.Name,
		Phase: phase, Runtime: string(task.Spec.Type), StartedAt: task.CreationTimestamp.Time}
	result.PhaseObservedAt = time.Now().UTC()
	if phase == "Pending" {
		result.PhaseObservedAt = task.CreationTimestamp.Time
	}
	if phase == "Running" && task.Status.StartTime != nil {
		result.PhaseObservedAt = task.Status.StartTime.Time
	}
	if task.Status.CompletionTime != nil {
		result.PhaseObservedAt = task.Status.CompletionTime.Time
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
