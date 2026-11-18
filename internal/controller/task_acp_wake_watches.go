/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// The Task controller's ACP workspace gates (core admission, the provider's
// attachment acknowledgement, revocation and detach settlement, a predecessor's
// release, RuntimePool replacement) wait on ExecutionWorkspace and RuntimePool
// objects that other controllers and the provider write. These watches wake
// exactly the Tasks whose gates read the changed object, so a handshake is
// noticed when it lands instead of on the next fallback poll. They only wake
// reconciles earlier: every gate still re-reads and re-verifies its inputs,
// and the RequeueAfter polls stay as the fallback for a missed event.

// acpTaskSessionNameIndex indexes Tasks by spec.sessionRef.name in the
// informer cache. The same name is a CRD selectable field, so uncached
// readers can use the identical field selector against the API server.
func acpTaskSessionNameIndex(object client.Object) []string {
	task, ok := object.(*corev1alpha1.Task)
	if !ok || task.Spec.SessionRef == nil {
		return nil
	}
	name := strings.TrimSpace(task.Spec.SessionRef.Name)
	if name == "" {
		return nil
	}
	return []string{name}
}

// registerACPTaskWakeWatches indexes Tasks by Session and adds the
// ExecutionWorkspace and RuntimePool watches to the Task controller. The
// workspace watch is registered only when its CRD is served, like the
// RuntimePool controller's.
func registerACPTaskWakeWatches(ctx context.Context, mgr manager.Manager, b *builder.Builder) (*builder.Builder, error) {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &corev1alpha1.Task{}, acpTaskSessionNameField, acpTaskSessionNameIndex); err != nil {
		return nil, err
	}
	reader := mgr.GetClient()
	_, err := mgr.GetRESTMapper().RESTMapping(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace").GroupKind(), workspacev1alpha1.GroupVersion.Version,
	)
	switch {
	case err == nil:
		b = b.Watches(
			&workspacev1alpha1.ExecutionWorkspace{},
			handler.EnqueueRequestsFromMapFunc(acpWorkspaceTaskWakeRequests(reader)),
			builder.WithPredicates(acpWorkspaceTaskWakePredicate()),
		)
	case !meta.IsNoMatchError(err):
		return nil, err
	}
	return b.Watches(
		&corev1alpha1.RuntimePool{},
		handler.EnqueueRequestsFromMapFunc(acpRuntimePoolTaskWakeRequests(reader)),
		builder.WithPredicates(acpRuntimePoolTaskWakePredicate()),
	), nil
}

// acpWorkspaceTaskWakeRequests maps an ExecutionWorkspace to the Tasks whose
// workspace gates read it: the attached Task, the Task whose pending resume
// or materialization demand it records, its owning per-Task Task, every
// linked Task that has not yet settled its detach action, and the live Tasks
// of its Session that may still be queued behind it before linking.
func acpWorkspaceTaskWakeRequests(reader client.Reader) handler.MapFunc {
	return func(ctx context.Context, object client.Object) []reconcile.Request {
		workspace, ok := object.(*workspacev1alpha1.ExecutionWorkspace)
		if !ok || !workspaceHasACPControllerOwnership(workspace) {
			return nil
		}
		namespace := workspace.Namespace
		requests := make(map[types.NamespacedName]struct{})
		add := func(name string) {
			if name = strings.TrimSpace(name); name != "" {
				requests[types.NamespacedName{Namespace: namespace, Name: name}] = struct{}{}
			}
		}
		if attachment := workspace.Spec.Attachment; attachment != nil {
			add(attachment.TaskRef.Name)
		}
		if fields := strings.Fields(workspace.Annotations[acpWorkspaceResumeRequestedAnnotation]); len(fields) >= 2 {
			add(fields[1])
		}
		for _, owner := range workspace.OwnerReferences {
			if owner.Kind == taskResourceKind && strings.HasPrefix(owner.APIVersion, corev1alpha1.GroupVersion.Group+"/") {
				add(owner.Name)
			}
		}
		addLinkedACPWorkspaceTasks(ctx, reader, namespace, workspace.Name, add)
		if session := workspace.Spec.SessionRef; session != nil && strings.TrimSpace(session.Name) != "" {
			tasks := &corev1alpha1.TaskList{}
			if err := reader.List(ctx, tasks, client.InNamespace(namespace),
				client.MatchingFields{acpTaskSessionNameField: strings.TrimSpace(session.Name)}); err != nil {
				logf.FromContext(ctx).V(1).Info("list Session Tasks for workspace wakeup", "workspace", workspace.Name, "error", err.Error())
			} else {
				for i := range tasks.Items {
					if acpTaskMayWaitOnWorkspace(&tasks.Items[i], workspace.Name) {
						add(tasks.Items[i].Name)
					}
				}
			}
		}
		return sortedTaskWakeRequests(requests)
	}
}

// acpRuntimePoolTaskWakeRequests maps a RuntimePool to the live Tasks queued
// on it (which rebind when it is replaced or stops admitting) and to the Tasks
// linked to its execution workspace, whose readiness gate reads the pool.
func acpRuntimePoolTaskWakeRequests(reader client.Reader) handler.MapFunc {
	return func(ctx context.Context, object client.Object) []reconcile.Request {
		pool, ok := object.(*corev1alpha1.RuntimePool)
		if !ok {
			return nil
		}
		namespace := pool.Namespace
		requests := make(map[types.NamespacedName]struct{})
		add := func(name string) {
			if name = strings.TrimSpace(name); name != "" {
				requests[types.NamespacedName{Namespace: namespace, Name: name}] = struct{}{}
			}
		}
		tasks := &corev1alpha1.TaskList{}
		if err := reader.List(ctx, tasks, client.InNamespace(namespace),
			client.MatchingLabels{acpRuntimeTaskPoolLabel: pool.Name}); err != nil {
			logf.FromContext(ctx).V(1).Info("list queued Tasks for RuntimePool wakeup", "runtimePool", pool.Name, "error", err.Error())
		} else {
			for i := range tasks.Items {
				if task := &tasks.Items[i]; !task.Status.Phase.IsTerminal() || !task.DeletionTimestamp.IsZero() {
					add(task.Name)
				}
			}
		}
		if workspaceName := strings.TrimSpace(pool.Labels[acpExecutionWorkspaceLinkLabel]); workspaceName != "" {
			addLinkedACPWorkspaceTasks(ctx, reader, namespace, workspaceName, add)
		}
		return sortedTaskWakeRequests(requests)
	}
}

// addLinkedACPWorkspaceTasks adds every Task carrying the workspace link
// whose detach action has not settled. A settled Task's workspace gates are
// all behind it, so a long-lived Session workspace does not wake its history.
func addLinkedACPWorkspaceTasks(ctx context.Context, reader client.Reader, namespace, workspaceName string, add func(string)) {
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, client.InNamespace(namespace),
		client.MatchingLabels{acpExecutionWorkspaceLinkLabel: workspaceName}); err != nil {
		logf.FromContext(ctx).V(1).Info("list linked Tasks for workspace wakeup", "workspace", workspaceName, "error", err.Error())
		return
	}
	for i := range tasks.Items {
		if tasks.Items[i].Annotations[acpTaskWorkspaceSettledAnnotation] == "" {
			add(tasks.Items[i].Name)
		}
	}
}

// acpTaskMayWaitOnWorkspace reports whether a Session Task can still be
// gated by the named workspace: it has not settled, and it is live, deleting,
// or linked to that workspace and settling.
func acpTaskMayWaitOnWorkspace(task *corev1alpha1.Task, workspaceName string) bool {
	if task.Annotations[acpTaskWorkspaceSettledAnnotation] != "" {
		return false
	}
	return !task.Status.Phase.IsTerminal() || !task.DeletionTimestamp.IsZero() ||
		task.Labels[acpExecutionWorkspaceLinkLabel] == workspaceName
}

func sortedTaskWakeRequests(set map[types.NamespacedName]struct{}) []reconcile.Request {
	if len(set) == 0 {
		return nil
	}
	keys := slices.SortedFunc(maps.Keys(set), func(a, b types.NamespacedName) int {
		return strings.Compare(a.String(), b.String())
	})
	requests := make([]reconcile.Request, 0, len(keys))
	for _, key := range keys {
		requests = append(requests, reconcile.Request{NamespacedName: key})
	}
	return requests
}

// acpWorkspaceTaskWakePredicate passes creates and deletes, and updates that
// change something a Task workspace gate reads. Pure resourceVersion or
// managedFields churn, condition message rewording, and endpoint or
// connection-secret refreshes do not wake Tasks.
func acpWorkspaceTaskWakePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldWorkspace, okOld := e.ObjectOld.(*workspacev1alpha1.ExecutionWorkspace)
			newWorkspace, okNew := e.ObjectNew.(*workspacev1alpha1.ExecutionWorkspace)
			if !okOld || !okNew {
				return false
			}
			return acpWorkspaceTaskGateChanged(oldWorkspace, newWorkspace)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// acpWorkspaceTaskWrittenDemandAnnotations are workspace annotations that
// waiting Tasks themselves rewrite: the resume/materialization demand record
// and the frozen detach action. No Task gate waits for another Task to change
// them, and two live Tasks of one Session can each rewrite them in turn while
// a suspension settles. Waking on them would let those Tasks re-trigger each
// other without any delay; their changes are left to the fallback poll and
// to the spec and status changes that accompany every real handshake.
var acpWorkspaceTaskWrittenDemandAnnotations = []string{
	acpWorkspaceResumeRequestedAnnotation,
	acpWorkspaceDetachActionAnnotation,
}

func acpWorkspaceTaskGateChanged(oldWorkspace, newWorkspace *workspacev1alpha1.ExecutionWorkspace) bool {
	if objectMetadataGateChanged(oldWorkspace, newWorkspace, acpWorkspaceTaskWrittenDemandAnnotations...) {
		return true
	}
	oldStatus, newStatus := &oldWorkspace.Status, &newWorkspace.Status
	return oldStatus.State != newStatus.State ||
		oldStatus.ObservedGeneration != newStatus.ObservedGeneration ||
		oldStatus.AttachedEpoch != newStatus.AttachedEpoch ||
		!reflect.DeepEqual(oldStatus.Allocation, newStatus.Allocation) ||
		!reflect.DeepEqual(oldStatus.Disposition, newStatus.Disposition) ||
		conditionGatesChanged(oldStatus.Conditions, newStatus.Conditions)
}

// acpRuntimePoolTaskWakePredicate passes creates and deletes, and updates to
// a pool's identity, lifecycle, admission, or active instance. Capacity
// accounting churn (reservations, prompt counts) and health-probe timestamp
// refreshes do not wake Tasks; the dispatcher owns capacity admission.
func acpRuntimePoolTaskWakePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPool, okOld := e.ObjectOld.(*corev1alpha1.RuntimePool)
			newPool, okNew := e.ObjectNew.(*corev1alpha1.RuntimePool)
			if !okOld || !okNew {
				return false
			}
			return acpRuntimePoolTaskGateChanged(oldPool, newPool)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func acpRuntimePoolTaskGateChanged(oldPool, newPool *corev1alpha1.RuntimePool) bool {
	if objectMetadataGateChanged(oldPool, newPool) {
		return true
	}
	oldStatus, newStatus := &oldPool.Status, &newPool.Status
	return oldStatus.Lifecycle != newStatus.Lifecycle ||
		oldStatus.AdmissionState != newStatus.AdmissionState ||
		oldStatus.ObservedGeneration != newStatus.ObservedGeneration ||
		!reflect.DeepEqual(activeInstanceIdentity(oldStatus.ActiveInstance), activeInstanceIdentity(newStatus.ActiveInstance))
}

// activeInstanceIdentity drops LastObservedTime, which every successful
// health probe refreshes; the instance and fence fields remain compared.
func activeInstanceIdentity(instance *corev1alpha1.RuntimePoolActiveInstanceStatus) *corev1alpha1.RuntimePoolActiveInstanceStatus {
	if instance == nil {
		return nil
	}
	identity := instance.DeepCopy()
	identity.LastObservedTime = nil
	return identity
}

// annotationsEqualIgnoring reports whether the two annotation maps are equal
// once the ignored keys are removed from both.
func annotationsEqualIgnoring(oldAnnotations, newAnnotations map[string]string, ignored []string) bool {
	relevant := func(annotations map[string]string) map[string]string {
		filtered := make(map[string]string, len(annotations))
		for key, value := range annotations {
			if !slices.Contains(ignored, key) {
				filtered[key] = value
			}
		}
		return filtered
	}
	return maps.Equal(relevant(oldAnnotations), relevant(newAnnotations))
}

func objectMetadataGateChanged(oldObject, newObject client.Object, ignoredAnnotations ...string) bool {
	return oldObject.GetUID() != newObject.GetUID() ||
		oldObject.GetGeneration() != newObject.GetGeneration() ||
		oldObject.GetDeletionTimestamp().IsZero() != newObject.GetDeletionTimestamp().IsZero() ||
		!slices.Equal(oldObject.GetFinalizers(), newObject.GetFinalizers()) ||
		!maps.Equal(oldObject.GetLabels(), newObject.GetLabels()) ||
		!annotationsEqualIgnoring(oldObject.GetAnnotations(), newObject.GetAnnotations(), ignoredAnnotations)
}

// conditionGatesChanged compares the condition fields gates decide on: type,
// status, reason, and observed generation.
func conditionGatesChanged(oldConditions, newConditions []metav1.Condition) bool {
	if len(oldConditions) != len(newConditions) {
		return true
	}
	for i := range newConditions {
		previous := meta.FindStatusCondition(oldConditions, newConditions[i].Type)
		if previous == nil || previous.Status != newConditions[i].Status ||
			previous.Reason != newConditions[i].Reason ||
			previous.ObservedGeneration != newConditions[i].ObservedGeneration {
			return true
		}
	}
	return false
}
