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

func limitTestTask(name string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid"), Finalizers: []string{labels.TaskFinalizer}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
}

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

	cachedParent := parent.DeepCopy()
	cachedParent.UID = "parent-old"
	fresh := child("new-b", parent.UID)
	staleMatch := child("old-d", "parent-old")
	scheme := newTestScheme()
	r = newUnitReconciler(scheme, parentAgent.DeepCopy(), cachedParent, fresh.DeepCopy(), staleMatch.DeepCopy())
	r.APIReader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(parentAgent.DeepCopy(), parent.DeepCopy(), fresh.DeepCopy(), staleMatch.DeepCopy()).Build()
	if _, err, handled := r.validateCoordinationConstraints(ctx, fresh.DeepCopy()); err != nil || handled {
		t.Fatalf("the new parent's child seen with a stale cached parent: handled=%t err=%v, want it admitted", handled, err)
	}
	if _, err, handled := r.validateCoordinationConstraints(ctx, staleMatch.DeepCopy()); err != nil || !handled {
		t.Fatalf("stale child matching stale cached parent: handled=%t err=%v, want a terminal failure", handled, err)
	}
	stored := &corev1alpha1.Task{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(staleMatch), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != corev1alpha1.TaskPhaseFailed || !strings.Contains(stored.Status.Message, "was replaced") {
		t.Fatalf("stale child matching stale cached parent: phase=%s message=%q, want it failed as replaced", stored.Status.Phase, stored.Status.Message)
	}
}

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
