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

// Task admission limits (--max-tasks-per-namespace and a coordinator's
// maxConcurrentChildren) admit Tasks through a slot ledger. A Task is a slot
// holder once it has been granted a slot, or once its own state shows it
// admitted (see taskAdmittedByWork and taskAdmissionRetryPending); a Task
// still waiting for a slot, or a recurring Task's scheduling parent, is not
// counted. A waiting Task joins the ledger with a resourceVersion-checked
// update of one shared Lease, so two contenders can never both take the last
// slot, and whenever a slot is free some contender takes it: waiting Tasks
// never block each other. Holders that run nothing yet, such as Tasks over a
// lowered limit, proceed one at a time, oldest first (taskAdmissionView).
// Holders free their slot by reaching a terminal phase or being deleted.
//
// Ledgers live only in the controller namespace, the trust boundary that also
// holds the controller-epoch Lease, and admission refuses every write to
// their name prefixes but the controller's
// (config/admission/acp_workspace_lease_protection.yaml), so a tenant that
// can write Leases there cannot forge or remove a grant. A grant
// counts only when it names a live, schedule-free Task of the scope by UID,
// and every admission, a granted Task's included, requires the running Tasks
// and the holders ahead of it to number fewer than the limit. A forged or
// stale grant can therefore only delay admission, never raise capacity above
// the limit.

const (
	// taskAdmissionLedgerAnnotation holds the granted Task UIDs, sorted, as a
	// JSON array.
	taskAdmissionLedgerAnnotation = "orka.ai/task-admission-slots"
	taskAdmissionNamespaceLedger  = labels.TaskAdmissionNamespaceLedgerNamePrefix
	taskAdmissionChildrenLedger   = labels.TaskAdmissionChildrenLedgerNamePrefix
	// taskAdmissionLedgerKindLabel marks ledgers for the reaper, which reads
	// the scope a ledger admits from its annotations.
	taskAdmissionLedgerKindLabel           = "orka.ai/task-admission-ledger"
	taskAdmissionLedgerKindNamespace       = "namespace"
	taskAdmissionLedgerKindChildren        = "children"
	taskAdmissionLedgerNamespaceAnnotation = "orka.ai/task-admission-namespace"
	taskAdmissionLedgerParentAnnotation    = "orka.ai/task-admission-parent"
	taskAdmissionLedgerParentUIDAnnotation = "orka.ai/task-admission-parent-uid"
	// taskAdmissionLedgerStaleAfter is how long a ledger with no live grant
	// is kept after its last grant.
	taskAdmissionLedgerStaleAfter   = time.Hour
	taskAdmissionLedgerReapInterval = 10 * time.Minute
)

// errTaskAdmissionLedgerUnavailable fails admission closed when no controller
// namespace is configured to hold admission ledgers.
var errTaskAdmissionLedgerUnavailable = errors.New(
	"task admission limits require a controller namespace to hold admission ledgers; no Task is admitted under a limit without one")

// MaxTaskAdmissionSlots bounds every admission limit. A ledger never holds
// more grants than its limit, each about 40 bytes of JSON, so this keeps a
// full ledger near 160KiB, well under the 256KiB Kubernetes allows for all of
// an object's annotations. --max-tasks-per-namespace is validated against it;
// a larger maxConcurrentChildren admits at most this many children at once.
const MaxTaskAdmissionSlots = 4096

// effectiveTaskAdmissionLimit clamps a configured limit to
// MaxTaskAdmissionSlots, the stricter direction.
func effectiveTaskAdmissionLimit(limit int32) int32 {
	return min(limit, MaxTaskAdmissionSlots)
}

// taskAdmissionScope is one limit: its ledger, its size, and how to list
// every Task it covers from the API server.
type taskAdmissionScope struct {
	ledger types.NamespacedName
	limit  int32
	// list returns the scope's Tasks. It is called after each ledger read,
	// so every grant the read returns belongs to a Task the list can see
	// unless that Task is gone.
	list func(context.Context) ([]corev1alpha1.Task, error)
	// labels and annotations identify the scope for the reaper.
	labels      map[string]string
	annotations map[string]string
}

// namespaceAdmissionScope is the namespace Task limit.
func (r *TaskReconciler) namespaceAdmissionScope(namespace string, limit int32) taskAdmissionScope {
	return taskAdmissionScope{
		ledger: types.NamespacedName{
			Namespace: strings.TrimSpace(r.ControllerNamespace), Name: taskAdmissionNamespaceLedger + taskAdmissionLedgerDigest(namespace),
		},
		limit: limit,
		list: func(ctx context.Context) ([]corev1alpha1.Task, error) {
			tasks := &corev1alpha1.TaskList{}
			err := uncachedReader(r.APIReader, r.Client).List(ctx, tasks, client.InNamespace(namespace))
			return tasks.Items, err
		},
		labels:      map[string]string{taskAdmissionLedgerKindLabel: taskAdmissionLedgerKindNamespace},
		annotations: map[string]string{taskAdmissionLedgerNamespaceAnnotation: namespace},
	}
}

// childrenAdmissionScope is a coordinator's maxConcurrentChildren.
func (r *TaskReconciler) childrenAdmissionScope(parent *corev1alpha1.Task, limit int32) taskAdmissionScope {
	return taskAdmissionScope{
		ledger: types.NamespacedName{
			Namespace: strings.TrimSpace(r.ControllerNamespace), Name: taskAdmissionChildrenLedgerName(parent.Namespace, parent.UID),
		},
		limit: limit,
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

// taskAdmissionChildOf reports whether a Task labelled as parent's child
// belongs to this incarnation of parent. A child pins its parent's UID with
// its controller owner reference or, across namespaces, the parent UID
// annotation, as delegatedBy reads them; a child pinned to another UID
// belongs to an earlier Task of the same name and is not counted. A child
// with no pin at all is counted: counting it can only delay admission.
func taskAdmissionChildOf(child, parent *corev1alpha1.Task) bool {
	pinned, ok := taskParentUIDPin(child, parent.Name)
	return !ok || pinned == parent.UID
}

// taskParentUIDPin returns the parent UID a child pins for the parent named
// parentName: its controller owner reference to that Task or, failing that,
// the parent UID annotation.
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

// A Task's own state shows it admitted, such as one admitted before the
// ledger existed, when it is past admission and running work
// (taskAdmittedByWork) or Pending in retry backoff between Job attempts
// (taskAdmissionRetryPending). A recurring Task's scheduling parent never
// holds a slot: it bypasses admission, stays Scheduled, and runs nothing
// itself; each of its runs is a separate child Task that takes its own slot.
//
// taskAdmittedByWork reports a Task Running or Finalizing, or Pending with its
// execution bound, its Job created, or an ACP attempt.
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

// taskAdmissionRetryPending reports a Pending Task between Job attempts:
// retryTask cleared its Job, and the retry-pending condition is the only sign
// it was admitted. It still holds its slot, so a Task admitted before the
// ledger existed is not overtaken while it backs off.
func taskAdmissionRetryPending(task *corev1alpha1.Task) bool {
	if task.Spec.Schedule != "" || task.Status.Phase != corev1alpha1.TaskPhasePending {
		return false
	}
	condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated)
	return condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == taskRetryPendingReason
}

// taskAdmissionGrantEligible reports a listed Task a grant may name: live
// (not terminal) and not a recurring Task's scheduling parent.
func taskAdmissionGrantEligible(task *corev1alpha1.Task) bool {
	return task.UID != "" && !task.Status.Phase.IsTerminal() && task.Spec.Schedule == ""
}

// taskAdmissionView is what a scope's ledger and Task list say about one
// Task's admission.
type taskAdmissionView struct {
	// running counts the other Tasks of the scope running work
	// (taskAdmittedByWork).
	running int
	// claimants counts the other Tasks entitled to a slot that run nothing
	// yet: those with a valid grant, and those backing off between retries;
	// claimantsBefore counts the ones ordered before task.
	claimants, claimantsBefore int
	// claimant reports whether task itself is such a Task.
	claimant bool
	// valid holds the grants that still count, and granted whether one of
	// them is task's.
	valid   []types.UID
	granted bool
}

// admits reports whether task may proceed under limit. A claimant waits only
// for the running Tasks and the claimants ordered before it, so claimants
// over a lowered limit proceed one at a time in a fixed order instead of
// each waiting for all the others; any other Task waits for every running
// Task and claimant. Either way the running Tasks and the claimants allowed
// to proceed never number more than limit.
func (v taskAdmissionView) admits(limit int32) bool {
	ahead := v.running + v.claimants
	if v.claimant {
		ahead = v.running + v.claimantsBefore
	}
	return int32(ahead) < limit //nolint:gosec // bounded by the listed Task count
}

// newTaskAdmissionView reads task's admission from the scope's grants and
// Tasks. A grant counts only when it names, by UID, a listed Task of the
// scope that is grant-eligible. Claimants are ordered oldest first
// (taskAdmissionOrderedBefore), the same in every scope, so a Task ahead in
// one ledger is ahead in all of them and two Tasks never each hold a slot
// the other waits for. No grant is dropped: a claimant not yet elected keeps
// its place and proceeds once those ahead of it finish.
func newTaskAdmissionView(task *corev1alpha1.Task, granted []types.UID, tasks []corev1alpha1.Task) taskAdmissionView {
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
		view.claimant = view.granted || taskAdmissionRetryPending(listed)
	}
	for uid, candidate := range eligible {
		if uid == task.UID {
			continue
		}
		_, hasGrant := grants[uid]
		switch {
		case taskAdmittedByWork(candidate):
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

// taskAdmissionOrderedBefore orders claimants oldest first, by creation time
// and then UID, a total order every scope and reconcile agrees on.
func taskAdmissionOrderedBefore(a, b *corev1alpha1.Task) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.UID < b.UID
}

// acquireTaskAdmissionSlot reports whether task holds one of the scope's
// slots, granting it one if a slot is free (see taskAdmissionView.admits). A
// grant nobody made can never admit a Task past the limit, and a Task the
// scope does not list as grant-eligible is never admitted.
func (r *TaskReconciler) acquireTaskAdmissionSlot(ctx context.Context, task *corev1alpha1.Task, scope taskAdmissionScope) (bool, error) {
	return r.taskAdmissionSlot(ctx, task, scope, true)
}

// taskAdmissionSlotFree reports whether acquireTaskAdmissionSlot would admit
// task now, without writing a grant.
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
		// A grant only counts for a Task the scope lists, so one written for
		// any other Task would be discarded by the next request and let it
		// run past the limit.
		if !slices.ContainsFunc(tasks, func(candidate corev1alpha1.Task) bool {
			return candidate.UID == task.UID && taskAdmissionGrantEligible(&candidate)
		}) {
			return nil
		}
		view := newTaskAdmissionView(task, entries, tasks)
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
			// The read's resourceVersion makes this a compare-and-swap: a
			// concurrent grant conflicts and the slots are counted again.
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

// releaseTaskAdmissionSlot removes task's grant from the scope's ledger, if
// it has one. A Task held in one scope releases its grant in the others, so
// no waiting Task holds a slot another Task waits for: a Task never holds a
// grant in one ledger while waiting in another, and no two Tasks can each
// wait for the other. A Task running work keeps its grants; removing a grant
// only frees capacity the waiting Task is not using.
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
		// The read's resourceVersion makes this a compare-and-swap.
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

// reapTaskAdmissionLedgers deletes admission ledgers that no longer admit
// anything: none of their grants belongs to a live Task of their scope, and
// their namespace or coordinator is gone or finished, or none was granted for
// taskAdmissionLedgerStaleAfter. A deletion is conditional on the ledger's UID
// and resourceVersion, so a grant made meanwhile keeps it.
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

// taskAdmissionLedgerReaper reaps admission ledgers on the leader.
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
