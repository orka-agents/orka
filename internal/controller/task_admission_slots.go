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
	"fmt"
	"slices"
	"strings"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// Task admission limits (--max-tasks-per-namespace and a coordinator's
// maxConcurrentChildren) admit Tasks through a slot ledger. A Task is a slot
// holder once it has been granted a slot, or once its own state shows it
// admitted (bound, its Job created, or Scheduled, Running, or Finalizing); a
// Task still waiting for a slot is not counted. A waiting Task joins the
// ledger with a resourceVersion-checked update of one shared Lease, so two
// contenders can never both take the last slot, and whenever a slot is free
// some contender takes it: waiting Tasks never block each other. Holders free
// their slot by reaching a terminal phase or being deleted.

const (
	// taskAdmissionLedgerAnnotation holds the granted Task UIDs, sorted, as a
	// JSON array.
	taskAdmissionLedgerAnnotation = "orka.ai/task-admission-slots"
	taskAdmissionNamespaceLedger  = "orka-task-admission-"
	taskAdmissionChildrenLedger   = "orka-child-admission-"
)

// taskAdmissionScope is one limit: its ledger, its size, and how to list
// every Task it covers from the API server.
type taskAdmissionScope struct {
	ledger types.NamespacedName
	// owner, when set, owns the ledger so it is collected with it.
	owner *corev1alpha1.Task
	limit int32
	// list returns the scope's Tasks. It is called after each ledger read,
	// so every grant the read returns belongs to a Task the list can see
	// unless that Task is gone.
	list func(context.Context) ([]corev1alpha1.Task, error)
}

// namespaceAdmissionScope is the namespace Task limit. Its ledger lives in the
// controller namespace when one is configured, where Task authors cannot edit
// the admin-set quota's grants.
func (r *TaskReconciler) namespaceAdmissionScope(namespace string, limit int32) taskAdmissionScope {
	ledgerNamespace := strings.TrimSpace(r.ControllerNamespace)
	if ledgerNamespace == "" {
		ledgerNamespace = namespace
	}
	return taskAdmissionScope{
		ledger: types.NamespacedName{Namespace: ledgerNamespace, Name: taskAdmissionNamespaceLedger + taskAdmissionLedgerDigest(namespace)},
		limit:  limit,
		list: func(ctx context.Context) ([]corev1alpha1.Task, error) {
			tasks := &corev1alpha1.TaskList{}
			err := uncachedReader(r.APIReader, r.Client).List(ctx, tasks, client.InNamespace(namespace))
			return tasks.Items, err
		},
	}
}

// childrenAdmissionScope is a coordinator's maxConcurrentChildren. Its ledger
// is owned by the parent Task, in the parent's namespace.
func (r *TaskReconciler) childrenAdmissionScope(parent *corev1alpha1.Task, limit int32) taskAdmissionScope {
	return taskAdmissionScope{
		ledger: types.NamespacedName{Namespace: parent.Namespace, Name: taskAdmissionChildrenLedger + taskAdmissionLedgerDigest(string(parent.UID))},
		owner:  parent, limit: limit,
		list: func(ctx context.Context) ([]corev1alpha1.Task, error) {
			siblings := &corev1alpha1.TaskList{}
			err := uncachedReader(r.APIReader, r.Client).List(ctx, siblings, client.InNamespace(parent.Namespace),
				client.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(parent.Name)})
			return siblings.Items, err
		},
	}
}

func taskAdmissionLedgerDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:20]
}

// taskAdmittedByState reports a Task whose own state shows it past admission,
// such as one admitted before the ledger existed.
func taskAdmittedByState(task *corev1alpha1.Task) bool {
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseScheduled, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing:
		return true
	}
	return task.Status.AgentExecutionBinding != nil || strings.TrimSpace(task.Status.JobName) != "" || task.Status.Execution != nil
}

// taskAdmissionHolders returns the slot holders other than task: granted
// entries still live, and live Tasks admitted by state. A Task is live until
// it is terminal or gone.
func taskAdmissionHolders(task *corev1alpha1.Task, granted []types.UID, tasks []corev1alpha1.Task) (holders map[types.UID]struct{}, live map[types.UID]struct{}) {
	live = make(map[types.UID]struct{}, len(tasks))
	holders = map[types.UID]struct{}{}
	for i := range tasks {
		candidate := &tasks[i]
		if candidate.UID == "" || candidate.Status.Phase.IsTerminal() {
			continue
		}
		live[candidate.UID] = struct{}{}
		if candidate.UID != task.UID && taskAdmittedByState(candidate) {
			holders[candidate.UID] = struct{}{}
		}
	}
	for _, uid := range granted {
		if _, ok := live[uid]; ok && uid != task.UID {
			holders[uid] = struct{}{}
		}
	}
	return holders, live
}

// acquireTaskAdmissionSlot reports whether task holds one of the scope's
// slots, granting it one if a slot is free.
func (r *TaskReconciler) acquireTaskAdmissionSlot(ctx context.Context, task *corev1alpha1.Task, scope taskAdmissionScope) (bool, error) {
	if task == nil || task.UID == "" {
		return false, fmt.Errorf("task identity is required for admission")
	}
	if scope.limit <= 0 {
		return true, nil
	}
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
		if slices.Contains(entries, task.UID) {
			granted = true
			return nil
		}
		tasks, err := scope.list(ctx)
		if err != nil {
			return err
		}
		holders, live := taskAdmissionHolders(task, entries, tasks)
		if int32(len(holders)) >= scope.limit { //nolint:gosec // bounded by the listed Task count
			return nil
		}
		next := []types.UID{task.UID}
		for _, uid := range entries {
			if _, ok := live[uid]; ok {
				next = append(next, uid)
			}
		}
		slices.Sort(next)
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if lease.Annotations == nil {
			lease.Annotations = map[string]string{}
		}
		lease.Annotations[taskAdmissionLedgerAnnotation] = string(encoded)
		if scope.owner != nil && !exists {
			scheme := r.Scheme
			if scheme == nil {
				scheme = r.Client.Scheme()
			}
			if err := controllerutil.SetOwnerReference(scope.owner, lease, scheme); err != nil {
				return err
			}
		}
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
