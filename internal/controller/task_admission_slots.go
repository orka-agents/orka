/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	taskAdmissionLedgerAnnotation          = "orka.ai/task-admission-slots"
	taskAdmissionNamespaceLedger           = labels.TaskAdmissionNamespaceLedgerNamePrefix
	taskAdmissionChildrenLedger            = labels.TaskAdmissionChildrenLedgerNamePrefix
	taskAdmissionLedgerKindLabel           = "orka.ai/task-admission-ledger"
	taskAdmissionLedgerKindNamespace       = "namespace"
	taskAdmissionLedgerKindChildren        = "children"
	taskAdmissionLedgerNamespaceAnnotation = "orka.ai/task-admission-namespace"
	taskAdmissionLedgerParentAnnotation    = "orka.ai/task-admission-parent"
	taskAdmissionLedgerParentUIDAnnotation = "orka.ai/task-admission-parent-uid"
	taskAdmissionLedgerStaleAfter          = time.Hour
	taskAdmissionLedgerReapInterval        = 10 * time.Minute
)

var errTaskAdmissionLedgerUnavailable = errors.New(
	"task admission limits require a controller namespace to hold admission ledgers; no Task is admitted under a limit without one")

const MaxTaskAdmissionSlots = 4096

func effectiveTaskAdmissionLimit(limit int32) int32 {
	return min(limit, MaxTaskAdmissionSlots)
}

type taskAdmissionScope struct {
	ledger      types.NamespacedName
	limit       int32
	namespace   string
	list        func(context.Context) ([]corev1alpha1.Task, error)
	labels      map[string]string
	annotations map[string]string
}

func (r *TaskReconciler) namespaceAdmissionScope(namespace string, limit int32) taskAdmissionScope {
	return taskAdmissionScope{
		ledger: types.NamespacedName{
			Namespace: strings.TrimSpace(r.ControllerNamespace), Name: taskAdmissionNamespaceLedger + taskAdmissionLedgerDigest(namespace),
		},
		limit:     limit,
		namespace: namespace,
		list: func(ctx context.Context) ([]corev1alpha1.Task, error) {
			tasks := &corev1alpha1.TaskList{}
			err := uncachedReader(r.APIReader, r.Client).List(ctx, tasks, client.InNamespace(namespace))
			return tasks.Items, err
		},
		labels:      map[string]string{taskAdmissionLedgerKindLabel: taskAdmissionLedgerKindNamespace},
		annotations: map[string]string{taskAdmissionLedgerNamespaceAnnotation: namespace},
	}
}

func (r *TaskReconciler) childrenAdmissionScope(parent *corev1alpha1.Task, limit int32) taskAdmissionScope {
	return taskAdmissionScope{
		ledger: types.NamespacedName{
			Namespace: strings.TrimSpace(r.ControllerNamespace), Name: taskAdmissionChildrenLedgerName(parent.Namespace, parent.UID),
		},
		limit:     limit,
		namespace: parent.Namespace,
		list: func(ctx context.Context) ([]corev1alpha1.Task, error) {
			siblings := &corev1alpha1.TaskList{}
			if err := uncachedReader(r.APIReader, r.Client).List(ctx, siblings, client.InNamespace(parent.Namespace),
				client.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(parent.Name)}); err != nil {
				return nil, err
			}
			children := siblings.Items[:0]
			for i := range siblings.Items {
				if taskAdmissionChildOf(&siblings.Items[i], parent) {
					children = append(children, siblings.Items[i])
				}
			}
			return children, nil
		},
		labels: map[string]string{taskAdmissionLedgerKindLabel: taskAdmissionLedgerKindChildren},
		annotations: map[string]string{
			taskAdmissionLedgerNamespaceAnnotation: parent.Namespace,
			taskAdmissionLedgerParentAnnotation:    parent.Name,
			taskAdmissionLedgerParentUIDAnnotation: string(parent.UID),
		},
	}
}

func taskAdmissionChildOf(child, parent *corev1alpha1.Task) bool {
	pinned, ok := taskParentUIDPin(child, parent.Name)
	return !ok || pinned == parent.UID
}

func taskParentUIDPin(child *corev1alpha1.Task, parentName string) (types.UID, bool) {
	if owner := metav1.GetControllerOf(child); owner != nil && owner.Kind == taskResourceKind &&
		owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Name == parentName {
		return owner.UID, true
	}
	if pinned := types.UID(strings.TrimSpace(child.Annotations[labels.AnnotationParentTaskUID])); pinned != "" {
		return pinned, true
	}
	return "", false
}

func taskAdmissionChildrenLedgerName(namespace string, parentUID types.UID) string {
	return taskAdmissionChildrenLedger + taskAdmissionLedgerDigest(namespace+"/"+string(parentUID))
}

func taskAdmissionLedgerDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:20]
}

func taskAdmittedByWork(task *corev1alpha1.Task) bool {
	if task.Spec.Schedule != "" {
		return false
	}
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing:
		return true
	}
	return task.Status.AgentExecutionBinding != nil || strings.TrimSpace(task.Status.JobName) != "" || task.Status.Execution != nil
}

func taskAdmissionRetryPending(task *corev1alpha1.Task) bool {
	if task.Spec.Schedule != "" || task.Status.Phase != corev1alpha1.TaskPhasePending {
		return false
	}
	condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated)
	return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == taskRetryPendingReason
}

func taskAdmissionGrantEligible(task *corev1alpha1.Task) bool {
	return task.UID != "" && !task.Status.Phase.IsTerminal() && task.Spec.Schedule == ""
}

type taskAdmissionView struct {
	running                    int
	claimants, claimantsBefore int
	claimant                   bool
	valid                      []types.UID
	granted                    bool
}

func (v taskAdmissionView) admits(limit int32) bool {
	ahead := v.running + v.claimants
	if v.claimant {
		ahead = v.running + v.claimantsBefore
	}
	return int32(ahead) < limit //nolint:gosec // bounded by the listed Task count
}

func newTaskAdmissionView(task *corev1alpha1.Task, granted []types.UID, tasks []corev1alpha1.Task, jobOwners map[types.UID]struct{}) taskAdmissionView {
	eligible := make(map[types.UID]*corev1alpha1.Task, len(tasks))
	for i := range tasks {
		if taskAdmissionGrantEligible(&tasks[i]) {
			eligible[tasks[i].UID] = &tasks[i]
		}
	}
	view := taskAdmissionView{}
	grants := make(map[types.UID]struct{}, len(granted))
	for _, uid := range granted {
		if _, ok := eligible[uid]; !ok || slices.Contains(view.valid, uid) {
			continue
		}
		view.valid = append(view.valid, uid)
		grants[uid] = struct{}{}
	}
	self := task
	if listed, ok := eligible[task.UID]; ok {
		self = listed
		_, view.granted = grants[task.UID]
		_, ownsJob := jobOwners[task.UID]
		view.claimant = view.granted || taskAdmissionRetryPending(listed) || taskAdmittedByWork(listed) || ownsJob
	}
	for uid, candidate := range eligible {
		if uid == task.UID {
			continue
		}
		_, hasGrant := grants[uid]
		_, ownsJob := jobOwners[uid]
		switch {
		case taskAdmittedByWork(candidate) || ownsJob:
			view.running++
		case hasGrant || taskAdmissionRetryPending(candidate):
			view.claimants++
			if taskAdmissionOrderedBefore(candidate, self) {
				view.claimantsBefore++
			}
		}
	}
	return view
}

func taskAdmissionOrderedBefore(a, b *corev1alpha1.Task) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.UID < b.UID
}

func (r *TaskReconciler) acquireTaskAdmissionSlot(ctx context.Context, task *corev1alpha1.Task, scope taskAdmissionScope) (bool, error) {
	return r.taskAdmissionSlot(ctx, task, scope, true)
}

func (r *TaskReconciler) taskAdmissionSlotFree(ctx context.Context, task *corev1alpha1.Task, scope taskAdmissionScope) (bool, error) {
	return r.taskAdmissionSlot(ctx, task, scope, false)
}

func (r *TaskReconciler) taskAdmissionSlot(ctx context.Context, task *corev1alpha1.Task, scope taskAdmissionScope, write bool) (bool, error) {
	if task == nil || task.UID == "" {
		return false, fmt.Errorf("task identity is required for admission")
	}
	if scope.limit <= 0 {
		return true, nil
	}
	if scope.ledger.Namespace == "" {
		return false, errTaskAdmissionLedgerUnavailable
	}
	scope.limit = effectiveTaskAdmissionLimit(scope.limit)
	reader := uncachedReader(r.APIReader, r.Client)
	granted := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		granted = false
		lease := &coordinationv1.Lease{}
		exists := true
		if err := reader.Get(ctx, scope.ledger, lease); apierrors.IsNotFound(err) {
			exists = false
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: scope.ledger.Namespace, Name: scope.ledger.Name}}
		} else if err != nil {
			return err
		}
		entries, err := decodeTaskAdmissionLedger(lease.Annotations[taskAdmissionLedgerAnnotation])
		if err != nil {
			return fmt.Errorf("admission ledger %s: %w", scope.ledger, err)
		}
		tasks, err := scope.list(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(tasks, func(candidate corev1alpha1.Task) bool {
			return candidate.UID == task.UID && taskAdmissionGrantEligible(&candidate)
		}) {
			return nil
		}
		jobOwners, err := r.taskAdmissionJobOwners(ctx, scope.namespace)
		if err != nil {
			return err
		}
		view := newTaskAdmissionView(task, entries, tasks, jobOwners)
		if !view.admits(scope.limit) {
			return nil
		}
		if view.granted || !write {
			granted = true
			return nil
		}
		next := append([]types.UID{task.UID}, view.valid...)
		slices.Sort(next)
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[taskAdmissionLedgerAnnotation] = string(encoded)
		maps.Copy(lease.Annotations, scope.annotations)
		if len(scope.labels) > 0 {
			if lease.Labels == nil {
				lease.Labels = map[string]string{}
			}
			maps.Copy(lease.Labels, scope.labels)
		}
		renewed := metav1.NewMicroTime(time.Now())
		lease.Spec.RenewTime = &renewed
		if exists {
			if err := r.Update(ctx, lease); err != nil {
				return err
			}
		} else if err := r.Create(ctx, lease); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return apierrors.NewConflict(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, lease.Name, err)
			}
			return err
		}
		granted = true
		return nil
	})
	return granted, err
}

func (r *TaskReconciler) taskAdmissionJobOwners(ctx context.Context, namespace string) (map[types.UID]struct{}, error) {
	jobs := &batchv1.JobList{}
	if err := uncachedReader(r.APIReader, r.Client).List(ctx, jobs, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	owners := map[types.UID]struct{}{}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		owner := metav1.GetControllerOf(job)
		if owner == nil || owner.Kind != taskResourceKind || owner.APIVersion != corev1alpha1.GroupVersion.String() ||
			job.DeletionTimestamp != nil || taskAdmissionJobFinished(job) {
			continue
		}
		owners[owner.UID] = struct{}{}
	}
	return owners, nil
}

func taskAdmissionJobFinished(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *TaskReconciler) releaseTaskAdmissionSlot(ctx context.Context, task *corev1alpha1.Task, scope taskAdmissionScope) error {
	if task == nil || task.UID == "" || scope.limit <= 0 || scope.ledger.Namespace == "" || taskAdmittedByWork(task) {
		return nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lease := &coordinationv1.Lease{}
		if err := reader.Get(ctx, scope.ledger, lease); err != nil {
			return client.IgnoreNotFound(err)
		}
		entries, err := decodeTaskAdmissionLedger(lease.Annotations[taskAdmissionLedgerAnnotation])
		if err != nil {
			return fmt.Errorf("admission ledger %s: %w", scope.ledger, err)
		}
		if !slices.Contains(entries, task.UID) {
			return nil
		}
		next := slices.DeleteFunc(slices.Clone(entries), func(uid types.UID) bool { return uid == task.UID })
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		lease.Annotations[taskAdmissionLedgerAnnotation] = string(encoded)
		return r.Update(ctx, lease)
	})
}

func decodeTaskAdmissionLedger(raw string) ([]types.UID, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var entries []types.UID
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("decode granted Task slots: %w", err)
	}
	return entries, nil
}

func (r *TaskReconciler) reapTaskAdmissionLedgers(ctx context.Context, now time.Time) error {
	controllerNamespace := strings.TrimSpace(r.ControllerNamespace)
	if controllerNamespace == "" {
		return nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	ledgers := &coordinationv1.LeaseList{}
	if err := reader.List(ctx, ledgers, client.InNamespace(controllerNamespace), client.HasLabels{taskAdmissionLedgerKindLabel}); err != nil {
		return err
	}
	for i := range ledgers.Items {
		ledger := &ledgers.Items[i]
		reap, err := r.taskAdmissionLedgerReapable(ctx, reader, ledger, now)
		if err != nil {
			return err
		}
		if !reap {
			continue
		}
		uid, resourceVersion := ledger.UID, ledger.ResourceVersion
		if err := r.Delete(ctx, ledger, client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}); err != nil &&
			!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}

//nolint:gocyclo // Each ledger kind's identity and liveness checks are kept together.
func (r *TaskReconciler) taskAdmissionLedgerReapable(
	ctx context.Context,
	reader client.Reader,
	ledger *coordinationv1.Lease,
	now time.Time,
) (bool, error) {
	namespace := strings.TrimSpace(ledger.Annotations[taskAdmissionLedgerNamespaceAnnotation])
	if namespace == "" {
		return false, nil
	}
	entries, err := decodeTaskAdmissionLedger(ledger.Annotations[taskAdmissionLedgerAnnotation])
	if err != nil {
		return false, nil
	}
	options := []client.ListOption{client.InNamespace(namespace)}
	var parentGone bool
	switch ledger.Labels[taskAdmissionLedgerKindLabel] {
	case taskAdmissionLedgerKindNamespace:
		if ledger.Name != taskAdmissionNamespaceLedger+taskAdmissionLedgerDigest(namespace) {
			return false, nil
		}
	case taskAdmissionLedgerKindChildren:
		parentName := strings.TrimSpace(ledger.Annotations[taskAdmissionLedgerParentAnnotation])
		parentUID := types.UID(strings.TrimSpace(ledger.Annotations[taskAdmissionLedgerParentUIDAnnotation]))
		if parentName == "" || parentUID == "" || ledger.Name != taskAdmissionChildrenLedgerName(namespace, parentUID) {
			return false, nil
		}
		options = append(options, client.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(parentName)})
		parent := &corev1alpha1.Task{}
		switch err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: parentName}, parent); {
		case apierrors.IsNotFound(err):
			parentGone = true
		case err != nil:
			return false, err
		default:
			parentGone = parent.UID != parentUID || parent.Status.Phase.IsTerminal()
		}
	default:
		return false, nil
	}
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, options...); err != nil {
		return false, err
	}
	for i := range tasks.Items {
		if slices.Contains(entries, tasks.Items[i].UID) && !tasks.Items[i].Status.Phase.IsTerminal() {
			return false, nil
		}
	}
	if parentGone {
		return true, nil
	}
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &corev1.Namespace{}); apierrors.IsNotFound(err) {
		return true, nil
	}
	renewed := ledger.CreationTimestamp.Time
	if ledger.Spec.RenewTime != nil {
		renewed = ledger.Spec.RenewTime.Time
	}
	return now.Sub(renewed) >= taskAdmissionLedgerStaleAfter, nil
}

type taskAdmissionLedgerReaper struct {
	reconciler *TaskReconciler
	interval   time.Duration
}

func (*taskAdmissionLedgerReaper) NeedLeaderElection() bool { return true }

func (p *taskAdmissionLedgerReaper) Start(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.reconciler.reapTaskAdmissionLedgers(ctx, time.Now()); err != nil && ctx.Err() == nil {
				logf.FromContext(ctx).Error(err, "reap task admission ledgers")
			}
		}
	}
}
