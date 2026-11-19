/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// limitTestReconciler returns a reconciler whose informer cache still shows
// peer before its admission, while the API server already has it admitted, as
// when another Task's reconcile admitted peer concurrently.
func limitTestReconciler(t *testing.T, task, peer *corev1alpha1.Task, objects ...client.Object) *TaskReconciler {
	t.Helper()
	scheme := newTestScheme()
	cachedPeer := peer.DeepCopy()
	cachedPeer.Status = corev1alpha1.TaskStatus{}
	r := newUnitReconciler(scheme, append([]client.Object{task, cachedPeer}, objects...)...)
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(append([]client.Object{task.DeepCopy(), peer.DeepCopy()}, objectCopies(objects)...)...).Build()
	return r
}

func objectCopies(objects []client.Object) []client.Object {
	copies := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		copies = append(copies, object.DeepCopyObject().(client.Object))
	}
	return copies
}

// limitTestTask is a Pending container Task.
func limitTestTask(name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid"), Finalizers: []string{labels.TaskFinalizer}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
}

// Parallel reconciles must not admit past the namespace limit: a Task the
// cache has not yet seen as Pending is counted from the API server.
func TestHandlePendingNamespaceLimitCountsConcurrentlyAdmittedTasks(t *testing.T) {
	task := limitTestTask("checking")
	peer := limitTestTask("admitted-concurrently")
	peer.Status.JobName = "admitted-concurrently-job"
	r := limitTestReconciler(t, task, peer)
	r.MaxTasksPerNamespace = 1

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("RequeueAfter = %v, want the namespace limit to hold the second Task", result.RequeueAfter)
	}
	assertNoJobsForTask(t, r, task)
}

// The same holds for a parent's maxConcurrentChildren.
func TestCoordinationChildLimitCountsConcurrentlyAdmittedSiblings(t *testing.T) {
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coordinator", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
			Enabled: true, MaxConcurrentChildren: 1,
		}},
	}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: parentAgent.Name}},
	}
	child := func(name string) *corev1alpha1.Task {
		task := limitTestTask(name)
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		task.Annotations = map[string]string{labels.AnnotationCoordinationDepth: "1"}
		return task
	}
	task, sibling := child("checking-child"), child("admitted-sibling")
	sibling.Status.JobName = "admitted-sibling-job"
	r := limitTestReconciler(t, task, sibling, parentAgent, parent)

	result, err, handled := r.validateCoordinationConstraints(context.Background(), task)
	if err != nil {
		t.Fatalf("validateCoordinationConstraints() error = %v", err)
	}
	if !handled || result.RequeueAfter != 10*time.Second {
		t.Fatalf("handled=%t RequeueAfter=%v, want the child limit to hold the second child", handled, result.RequeueAfter)
	}
}

// Two Pending Tasks waiting under a limit of one used to count each other
// and wait forever. Waiting Tasks no longer count: the first to take the slot
// proceeds and the other waits for it.
func TestHandlePendingNamespaceLimitAdmitsOneOfTwoWaitingTasks(t *testing.T) {
	first := limitTestTask("first-waiting")
	second := limitTestTask("second-waiting")
	r := newUnitReconciler(newTestScheme(), first, second)
	r.APIReader = r.Client
	r.MaxTasksPerNamespace = 1

	result, err := r.handlePending(context.Background(), first.DeepCopy())
	if err != nil {
		t.Fatalf("first handlePending() error = %v", err)
	}
	if result.RequeueAfter == 10*time.Second {
		t.Fatal("the first waiting Task was held although no Task held the slot")
	}
	result, err = r.handlePending(context.Background(), second.DeepCopy())
	if err != nil {
		t.Fatalf("second handlePending() error = %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Fatalf("second RequeueAfter = %v, want it to wait for the slot", result.RequeueAfter)
	}
	// Only the admitted Task created its Job.
	stored := &corev1alpha1.Task{}
	for _, task := range []*corev1alpha1.Task{first, second} {
		if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), stored); err != nil {
			t.Fatal(err)
		}
		if admitted := stored.Status.JobName != ""; admitted != (task == first) {
			t.Fatalf("%s JobName = %q, want only the first Task admitted", task.Name, stored.Status.JobName)
		}
	}
}

// A recurring Task's scheduling parent stays Scheduled and runs nothing
// itself, so it never holds a slot: under a limit of one, both its own child
// run and an unrelated Task proceed.
func TestHandlePendingRecurringParentHoldsNoSlot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		labeled bool
	}{{name: "child run", labeled: true}, {name: "unrelated Task"}} {
		t.Run(tc.name, func(t *testing.T) {
			parent := limitTestTask("recurring-parent")
			parent.Spec.Schedule = "*/5 * * * *"
			parent.Status.Phase = corev1alpha1.TaskPhaseScheduled
			waiting := limitTestTask("run")
			if tc.labeled {
				waiting.Labels = map[string]string{labels.LabelParentTask: parent.Name}
			}
			r := newUnitReconciler(newTestScheme(), parent, waiting)
			r.APIReader = r.Client
			r.MaxTasksPerNamespace = 1

			result, err := r.handlePending(context.Background(), waiting.DeepCopy())
			if err != nil {
				t.Fatalf("handlePending() error = %v", err)
			}
			if result.RequeueAfter == 10*time.Second {
				t.Fatal("the recurring parent held the only namespace slot")
			}
			stored := &corev1alpha1.Task{}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(waiting), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.JobName == "" {
				t.Fatal("the waiting Task did not run")
			}
		})
	}
}

// Two reconciles of Tasks in one namespace can both read a worker
// RoleBinding with a stale RoleRef. When the other has already replaced it
// with the desired binding, this one must not delete that replacement out
// from under running workers.
func TestRecreateWorkerRoleBindingKeepsAConcurrentReplacement(t *testing.T) {
	ctx := context.Background()
	const namespace = "shared-ns"
	spec := workerRBACSpec{
		serviceAccountName: AIWorkerServiceAccount, clusterRoleName: DefaultAIWorkerClusterRoleName,
		roleBindingName: "orka-ai-worker-" + namespace,
	}
	replacement := workerRoleBinding(namespace, spec)
	replacement.UID = "replacement-uid"
	r := newUnitReconciler(newTestScheme(), replacement)
	// The fake client ignores delete preconditions; the API server refuses a
	// delete whose UID precondition does not match with a Conflict.
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Delete: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			deleteOptions := &client.DeleteOptions{}
			deleteOptions.ApplyOptions(options)
			if preconditions := deleteOptions.Preconditions; preconditions != nil && preconditions.UID != nil {
				current := &rbacv1.RoleBinding{}
				if err := delegate.Get(ctx, client.ObjectKeyFromObject(object), current); err == nil && current.UID != *preconditions.UID {
					return apierrors.NewConflict(rbacv1.Resource("rolebindings"), object.GetName(), errors.New("UID precondition failed"))
				}
			}
			return delegate.Delete(ctx, object, options...)
		},
	})
	staleRead := replacement.DeepCopy()
	staleRead.UID = "stale-uid"
	staleRead.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "stale-worker-role"}

	got, err := r.recreateWorkerRoleBinding(ctx, namespace, spec, staleRead, workerRoleBinding(namespace, spec))
	if err != nil {
		t.Fatal(err)
	}
	stored := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(replacement), stored); err != nil {
		t.Fatalf("the concurrent replacement was deleted: %v", err)
	}
	if stored.UID != replacement.UID || got.UID != replacement.UID {
		t.Fatalf("stored UID %s, returned UID %s; want the concurrent replacement %s kept", stored.UID, got.UID, replacement.UID)
	}
}

// A child pinned to a replaced parent fails terminally rather than being
// admitted under the Task that now holds the parent's name, whose children
// limit does not count it. Several such children cannot each run under a
// limit of one, and the new parent's own child is still admitted. A cache
// that still holds the earlier parent does not fail the new parent's child.
func TestCoordinationChildOfAReplacedParentIsNeverAdmitted(t *testing.T) {
	ctx := context.Background()
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coordinator", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
			Enabled: true, MaxConcurrentChildren: 1,
		}},
	}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-new"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: parentAgent.Name}},
	}
	child := func(name string, parentUID types.UID) *corev1alpha1.Task {
		task := limitTestTask(name)
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		task.Annotations = map[string]string{labels.AnnotationCoordinationDepth: "1"}
		isController := true
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parentUID, Controller: &isController,
		}}
		return task
	}
	stale := []*corev1alpha1.Task{child("old-a", "parent-old"), child("old-b", "parent-old"), child("old-c", "parent-old")}
	current := child("new-a", parent.UID)
	objects := make([]client.Object, 0, 3+len(stale))
	objects = append(objects, parentAgent, parent, current)
	for _, task := range stale {
		objects = append(objects, task)
	}
	r := newUnitReconciler(newTestScheme(), objects...)
	r.APIReader = r.Client

	for _, task := range stale {
		if _, err, handled := r.validateCoordinationConstraints(ctx, task.DeepCopy()); err != nil || !handled {
			t.Fatalf("%s: handled=%t err=%v, want a terminal failure", task.Name, handled, err)
		}
		stored := &corev1alpha1.Task{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(task), stored); err != nil {
			t.Fatal(err)
		}
		if stored.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(stored.Status.Message, "was replaced") {
			t.Fatalf("%s: phase=%s message=%q, want it failed as a child of a replaced parent", task.Name, stored.Status.Phase, stored.Status.Message)
		}
	}
	if _, err, handled := r.validateCoordinationConstraints(ctx, current.DeepCopy()); err != nil || handled {
		t.Fatalf("the new parent's child: handled=%t err=%v, want it admitted", handled, err)
	}

	// The cache still holds the earlier parent; the API server has the new
	// one. The new parent's child is confirmed against the API server and
	// admitted, not failed as a child of a replaced parent.
	cachedParent := parent.DeepCopy()
	cachedParent.UID = "parent-old"
	fresh := child("new-b", parent.UID)
	scheme := newTestScheme()
	r = newUnitReconciler(scheme, parentAgent.DeepCopy(), cachedParent, fresh.DeepCopy())
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(parentAgent.DeepCopy(), parent.DeepCopy(), fresh.DeepCopy()).Build()
	if _, err, handled := r.validateCoordinationConstraints(ctx, fresh.DeepCopy()); err != nil || handled {
		t.Fatalf("the new parent's child seen with a stale cached parent: handled=%t err=%v, want it admitted", handled, err)
	}
}

// limitTestRetryingTask is a Job Task backing off between attempts, as
// retryTask leaves it: Pending, its Job cleared, and the retry-pending
// condition the only sign it was admitted.
func limitTestRetryingTask(name string) *corev1alpha1.Task {
	task := limitTestTask(name)
	task.Spec.RetryPolicy = &corev1alpha1.RetryPolicy{InitialDelay: &metav1.Duration{Duration: time.Hour}}
	task.Status.Attempts = 1
	task.Status.Conditions = []metav1.Condition{{
		Type: ConditionTypeJobCreated, Status: metav1.ConditionFalse, LastTransitionTime: metav1.Now(),
		Reason: taskRetryPendingReason, Message: "waiting for the next retry attempt",
	}}
	return task
}

// A Job Task in retry backoff when the namespace limit is first enforced has
// no grant, and its cleared Job no longer shows it admitted. It still holds
// its slot: another Task is not admitted past the limit while it backs off,
// it takes its own grant before the delay returns, and the other Task is
// admitted once it finishes.
func TestHandlePendingRetryBackoffKeepsItsNamespaceSlot(t *testing.T) {
	ctx := context.Background()
	retrying, waiting := limitTestRetryingTask("retrying"), limitTestTask("waiting")
	r := newUnitReconciler(newTestScheme(), retrying, waiting)
	r.APIReader = r.Client
	r.MaxTasksPerNamespace = 1
	scope := r.namespaceAdmissionScope(waiting.Namespace, r.MaxTasksPerNamespace)
	stored := func(task *corev1alpha1.Task) *corev1alpha1.Task {
		t.Helper()
		current := &corev1alpha1.Task{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	holdsWaiting := func(when string) {
		t.Helper()
		result, err := r.handlePending(ctx, stored(waiting))
		if err != nil {
			t.Fatalf("%s: handlePending(waiting) error = %v", when, err)
		}
		if result.RequeueAfter != 10*time.Second || stored(waiting).Status.JobName != "" {
			t.Fatalf("%s: waiting Task RequeueAfter=%v JobName=%q, want it held by the limit", when, result.RequeueAfter, stored(waiting).Status.JobName)
		}
	}

	holdsWaiting("before the retrying Task reconciles")

	result, err := r.handlePending(ctx, stored(retrying))
	if err != nil {
		t.Fatalf("handlePending(retrying) error = %v", err)
	}
	if result.RequeueAfter <= 10*time.Second || stored(retrying).Status.JobName != "" {
		t.Fatalf("retrying Task RequeueAfter=%v JobName=%q, want it to keep backing off", result.RequeueAfter, stored(retrying).Status.JobName)
	}
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, scope.ledger, lease); err != nil {
		t.Fatalf("the retrying Task took no grant during its backoff: %v", err)
	}
	grants, err := decodeTaskAdmissionLedger(lease.Annotations[taskAdmissionLedgerAnnotation])
	if err != nil || len(grants) != 1 || grants[0] != retrying.UID {
		t.Fatalf("ledger grants = %v (%v), want only the retrying Task", grants, err)
	}

	holdsWaiting("while the retrying Task backs off")

	finished := stored(retrying)
	finished.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := r.Status().Update(ctx, finished); err != nil {
		t.Fatal(err)
	}
	if _, err := r.handlePending(ctx, stored(waiting)); err != nil {
		t.Fatalf("handlePending(waiting) after the retrying Task finished: %v", err)
	}
	if stored(waiting).Status.JobName == "" {
		t.Fatal("the waiting Task was not admitted after the retrying Task finished")
	}
}

// Tasks backing off when a limit is first enforced can outnumber it. They do
// not wait for each other: one takes the slot, the others wait for it, and a
// new Task waits for all of them.
func TestRetryBackoffTasksOverALimitTakeTheSlotInTurn(t *testing.T) {
	ctx := context.Background()
	first, second, fresh := limitTestRetryingTask("first"), limitTestRetryingTask("second"), limitTestTask("fresh")
	r := newUnitReconciler(newTestScheme(), first, second, fresh)
	r.APIReader = r.Client
	scope := r.namespaceAdmissionScope("default", 1)
	if granted, err := r.acquireTaskAdmissionSlot(ctx, fresh, scope); err != nil || granted {
		t.Fatalf("new Task: granted=%t err=%v, want it held while Tasks back off", granted, err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, first, scope); err != nil || !granted {
		t.Fatalf("first retrying Task: granted=%t err=%v, want the slot", granted, err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, second, scope); err != nil || granted {
		t.Fatalf("second retrying Task: granted=%t err=%v, want it to wait for the first", granted, err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, fresh, scope); err != nil || granted {
		t.Fatalf("new Task: granted=%t err=%v, want it held", granted, err)
	}
}

// A retrying child takes its slots in admission order, namespace before
// children. Waiting for its namespace slot, it takes no child slot, so two
// retrying siblings never each hold the slot the other waits for.
func TestRetryBackoffTakesNoChildSlotWhileWaitingForItsNamespaceSlot(t *testing.T) {
	ctx := context.Background()
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coordinator", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
			Enabled: true, MaxConcurrentChildren: 1,
		}},
	}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: parentAgent.Name}},
	}
	child := func(name string) *corev1alpha1.Task {
		task := limitTestRetryingTask(name)
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		task.Annotations = map[string]string{labels.AnnotationCoordinationDepth: "1"}
		isController := true
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parent.UID, Controller: &isController,
		}}
		return task
	}
	first, second := child("first"), child("second")
	r := newUnitReconciler(newTestScheme(), parentAgent, parent, first, second)
	r.APIReader = r.Client
	r.MaxTasksPerNamespace = 1
	namespace := r.namespaceAdmissionScope("default", r.MaxTasksPerNamespace)
	children := r.childrenAdmissionScope(parent, 1)
	writeAdmissionLedger(t, r.Client, namespace.ledger, first.UID)

	result, err := r.handlePending(ctx, second.DeepCopy())
	if err != nil || result.RequeueAfter <= 10*time.Second {
		t.Fatalf("second child: RequeueAfter=%v err=%v, want its retry delay", result.RequeueAfter, err)
	}
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, children.ledger, lease); !apierrors.IsNotFound(err) {
		t.Fatalf("the child waiting for its namespace slot took a child slot: %v %v", err, lease.Annotations)
	}
	if _, err := r.handlePending(ctx, first.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, first, children); err != nil || !granted {
		t.Fatalf("first child: child slot granted=%t err=%v, want it to hold both slots", granted, err)
	}
	if granted, err := r.acquireTaskAdmissionSlot(ctx, second, children); err != nil || granted {
		t.Fatalf("second child: child slot granted=%t err=%v, want it held", granted, err)
	}
}

// After both limits drop to 1, sibling A holds only a namespace grant and
// sibling B only a child grant: each needs the slot the other holds. A Task
// held in one scope gives up its grant in the other, so in either age order
// exactly one sibling is admitted and the other follows once it finishes.
// A running holder in either scope holds both siblings until it finishes.
func TestCrossScopePartialHoldersProceedOneAtATime(t *testing.T) {
	for _, running := range []string{"", taskAdmissionLedgerKindNamespace, taskAdmissionLedgerKindChildren} {
		for _, aOlder := range []bool{true, false} {
			name := fmt.Sprintf("running=%q/aOlder=%t", running, aOlder)
			t.Run(name, func(t *testing.T) {
				testCrossScopePartialHolders(t, running, aOlder)
			})
		}
	}
}

func testCrossScopePartialHolders(t *testing.T, running string, aOlder bool) {
	ctx := context.Background()
	parentAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "coordinator", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
			Enabled: true, MaxConcurrentChildren: 1,
		}},
	}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: parentAgent.Name}},
	}
	base := time.Now().Add(-time.Hour)
	child := func(name string, age time.Duration) *corev1alpha1.Task {
		task := limitTestTask(name)
		task.CreationTimestamp = metav1.NewTime(base.Add(-age))
		task.Labels = map[string]string{labels.LabelParentTask: parent.Name}
		task.Annotations = map[string]string{labels.AnnotationCoordinationDepth: "1"}
		isController := true
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parent.UID, Controller: &isController,
		}}
		return task
	}
	ageA, ageB := time.Minute, 2*time.Minute
	if aOlder {
		ageA, ageB = ageB, ageA
	}
	a, b := child("a", ageA), child("b", ageB)
	objects := []client.Object{parentAgent, parent, a, b}
	namespaceGrants, childGrants := []types.UID{a.UID}, []types.UID{b.UID}
	r := newUnitReconciler(newTestScheme())
	r.MaxTasksPerNamespace = 1
	var holder *corev1alpha1.Task
	switch running {
	case taskAdmissionLedgerKindNamespace:
		holder = limitTestTask("running-elsewhere")
		namespaceGrants = append(namespaceGrants, holder.UID)
	case taskAdmissionLedgerKindChildren:
		// A running sibling fills the child slot; the namespace has room for
		// it and one more.
		holder = child("running-sibling", 3*time.Minute)
		namespaceGrants = append(namespaceGrants, holder.UID)
		childGrants = append(childGrants, holder.UID)
		r.MaxTasksPerNamespace = 2
	}
	if holder != nil {
		holder.Status.Phase = corev1alpha1.TaskPhaseRunning
		holder.Status.JobName = holder.Name + "-job"
		objects = append(objects, holder)
	}
	limit := r.MaxTasksPerNamespace
	r = newUnitReconciler(newTestScheme(), objects...)
	r.APIReader = r.Client
	r.MaxTasksPerNamespace = limit
	writeAdmissionLedger(t, r.Client, r.namespaceAdmissionScope("default", 1).ledger, namespaceGrants...)
	writeAdmissionLedger(t, r.Client, r.childrenAdmissionScope(parent, 1).ledger, childGrants...)

	stored := func(task *corev1alpha1.Task) *corev1alpha1.Task {
		t.Helper()
		current := &corev1alpha1.Task{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		return current
	}
	admitted := func(task *corev1alpha1.Task) bool {
		current := stored(task)
		return current.Status.JobName != "" && !current.Status.Phase.IsTerminal()
	}
	// reconcile runs every waiting sibling a few times and returns the
	// admitted ones; never more than one runs at once.
	reconcile := func(when string) []*corev1alpha1.Task {
		t.Helper()
		for range 3 {
			for _, task := range []*corev1alpha1.Task{a, b} {
				current := stored(task)
				if current.Status.JobName != "" || current.Status.Phase.IsTerminal() {
					continue
				}
				if _, err := r.handlePending(ctx, current); err != nil {
					t.Fatalf("%s: handlePending(%s): %v", when, task.Name, err)
				}
				if admitted(a) && admitted(b) {
					t.Fatalf("%s: both siblings admitted under limits of one", when)
				}
			}
		}
		var out []*corev1alpha1.Task
		for _, task := range []*corev1alpha1.Task{a, b} {
			if admitted(task) {
				out = append(out, task)
			}
		}
		return out
	}
	finish := func(task *corev1alpha1.Task) {
		t.Helper()
		current := stored(task)
		current.Status.Phase = corev1alpha1.TaskPhaseSucceeded
		if err := r.Status().Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}

	if holder != nil {
		if got := reconcile("while a holder runs"); len(got) != 0 {
			t.Fatalf("while a holder runs, admitted %s", got[0].Name)
		}
		finish(holder)
	}
	first := reconcile("after the limits dropped")
	if len(first) != 1 {
		t.Fatalf("after the limits dropped, %d siblings admitted, want exactly one", len(first))
	}
	finish(first[0])
	second := reconcile("after the first sibling finished")
	if len(second) != 1 || second[0] == first[0] {
		t.Fatalf("after %s finished, admitted %d siblings, want the other one", first[0].Name, len(second))
	}
}
