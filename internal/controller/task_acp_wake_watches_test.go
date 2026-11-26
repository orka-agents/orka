/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func wakeTestTask(namespace, name string, mutate ...func(*corev1alpha1.Task)) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "p"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	for _, m := range mutate {
		m(task)
	}
	return task
}

func wakeTestSession(name string) func(*corev1alpha1.Task) {
	return func(task *corev1alpha1.Task) { task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: name} }
}

func wakeTestLabel(key, value string) func(*corev1alpha1.Task) {
	return func(task *corev1alpha1.Task) {
		if task.Labels == nil {
			task.Labels = map[string]string{}
		}
		task.Labels[key] = value
	}
}

func wakeTestSettled(task *corev1alpha1.Task) {
	task.Annotations = map[string]string{acpTaskWorkspaceSettledAnnotation: booleanTrueValue}
}

func wakeTestPhase(phase corev1alpha1.TaskPhase) func(*corev1alpha1.Task) {
	return func(task *corev1alpha1.Task) { task.Status.Phase = phase }
}

func wakeTestReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testACPWorkspaceScheme(t)).
		WithIndex(&corev1alpha1.Task{}, acpTaskSessionNameField, acpTaskSessionNameIndex).
		WithObjects(objects...).Build()
}

func wakeRequestNames(requests []reconcile.Request) []string {
	names := make([]string, 0, len(requests))
	for _, request := range requests {
		names = append(names, request.Namespace+"/"+request.Name)
	}
	return names
}

func wakeTestWorkspace() *workspacev1alpha1.ExecutionWorkspace {
	return &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team", Name: "acp-ws-session", UID: "ws-uid", Generation: 3,
			Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: "sandbox.provider.example.com"},
			Annotations: map[string]string{
				acpExecutionWorkspacePoolAnnotation:   "acp-ws-pool",
				acpWorkspaceResumeRequestedAnnotation: "2026-11-18T00:00:00Z resumer resumer-uid",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "owner", UID: "owner-uid",
			}},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			SessionRef: &workspacev1alpha1.ObjectIdentityReference{Name: "conversation", UID: "session-uid"},
			Attachment: &workspacev1alpha1.ExecutionWorkspaceAttachment{
				TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: "attached", UID: "attached-uid"},
				Epoch:   2,
			},
		},
	}
}

func TestACPWorkspaceTaskWakeRequestsTargetGatedTasks(t *testing.T) {
	workspace := wakeTestWorkspace()
	reader := wakeTestReader(t,
		// Linked and not yet settled: still gated (live or settling).
		wakeTestTask("team", "linked", wakeTestLabel(acpExecutionWorkspaceLinkLabel, workspace.Name)),
		wakeTestTask("team", "settling", wakeTestLabel(acpExecutionWorkspaceLinkLabel, workspace.Name),
			wakeTestPhase(corev1alpha1.TaskPhaseSucceeded)),
		// Linked but settled: its workspace gates are behind it.
		wakeTestTask("team", "settled", wakeTestLabel(acpExecutionWorkspaceLinkLabel, workspace.Name), wakeTestSettled,
			wakeTestPhase(corev1alpha1.TaskPhaseSucceeded)),
		// A live Session continuation not yet linked (queued behind a predecessor).
		wakeTestTask("team", "waiting", wakeTestSession("conversation")),
		// Finished Session history that never linked is not gated.
		wakeTestTask("team", "history", wakeTestSession("conversation"), wakeTestPhase(corev1alpha1.TaskPhaseFailed)),
		// Unrelated Session, other workspace, other namespace.
		wakeTestTask("team", "other-session", wakeTestSession("elsewhere")),
		wakeTestTask("team", "other-workspace", wakeTestLabel(acpExecutionWorkspaceLinkLabel, "acp-ws-other")),
		wakeTestTask("other", "linked", wakeTestLabel(acpExecutionWorkspaceLinkLabel, workspace.Name)),
		wakeTestTask("other", "waiting", wakeTestSession("conversation")),
	)
	got := wakeRequestNames(acpWorkspaceTaskWakeRequests(reader)(context.Background(), workspace))
	want := []string{"team/attached", "team/linked", "team/owner", "team/resumer", "team/settling", "team/waiting"}
	if !slices.Equal(got, want) {
		t.Fatalf("workspace wake requests = %v, want %v", got, want)
	}
}

func TestACPWorkspaceTaskWakeRequestsIgnoreForeignWorkspaces(t *testing.T) {
	workspace := wakeTestWorkspace()
	// Neither the ACP controller label nor the ACP pool link: not ours.
	delete(workspace.Annotations, acpExecutionWorkspacePoolAnnotation)
	reader := wakeTestReader(t, wakeTestTask("team", "linked", wakeTestLabel(acpExecutionWorkspaceLinkLabel, workspace.Name)))
	if got := acpWorkspaceTaskWakeRequests(reader)(context.Background(), workspace); len(got) != 0 {
		t.Fatalf("foreign workspace woke Tasks: %v", wakeRequestNames(got))
	}
	if got := acpWorkspaceTaskWakeRequests(reader)(context.Background(), &corev1alpha1.Task{}); len(got) != 0 {
		t.Fatalf("non-workspace object woke Tasks: %v", wakeRequestNames(got))
	}
}

func TestACPRuntimePoolTaskWakeRequestsTargetQueuedAndLinkedTasks(t *testing.T) {
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team", Name: "acp-ws-pool",
		Labels: map[string]string{acpExecutionWorkspaceLinkLabel: "acp-ws-session"},
	}}
	reader := wakeTestReader(t,
		wakeTestTask("team", "queued", wakeTestLabel(acpRuntimeTaskPoolLabel, pool.Name)),
		wakeTestTask("team", "done", wakeTestLabel(acpRuntimeTaskPoolLabel, pool.Name), wakeTestPhase(corev1alpha1.TaskPhaseSucceeded)),
		wakeTestTask("team", "attaching", wakeTestLabel(acpExecutionWorkspaceLinkLabel, "acp-ws-session")),
		wakeTestTask("team", "other-pool", wakeTestLabel(acpRuntimeTaskPoolLabel, "acp-other")),
		wakeTestTask("other", "queued", wakeTestLabel(acpRuntimeTaskPoolLabel, pool.Name)),
	)
	got := wakeRequestNames(acpRuntimePoolTaskWakeRequests(reader)(context.Background(), pool))
	if want := []string{"team/attaching", "team/queued"}; !slices.Equal(got, want) {
		t.Fatalf("pool wake requests = %v, want %v", got, want)
	}
}

func TestACPWorkspaceTaskWakePredicate(t *testing.T) {
	base := wakeTestWorkspace()
	base.Status = workspacev1alpha1.ExecutionWorkspaceStatus{
		ObservedGeneration: 3, State: workspacev1alpha1.ExecutionWorkspaceStateReady,
		Conditions: []metav1.Condition{{
			Type: string(workspacev1alpha1.ConditionWorkspaceAdmitted), Status: metav1.ConditionTrue,
			Reason: "Admitted", Message: "admitted", ObservedGeneration: 3, LastTransitionTime: metav1.Now(),
		}},
	}
	predicate := acpWorkspaceTaskWakePredicate()
	for _, tc := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
		want   bool
	}{
		{name: "resourceVersion only", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.ResourceVersion = "99" }},
		{name: "condition message and time", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Conditions[0].Message = "still admitted"
			w.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(time.Minute))
		}},
		{name: "endpoint refresh", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Endpoints = []workspacev1alpha1.ExecutionWorkspaceEndpoint{{Name: "api"}}
		}},
		{name: "attachment acknowledged", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.Status.AttachedEpoch = 2 }},
		{name: "provider observed generation", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.Status.ObservedGeneration = 4 }},
		{name: "state", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
		}},
		{name: "condition reason", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.Status.Conditions[0].Reason = "Readmitting" }},
		{name: "condition generation", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Conditions[0].ObservedGeneration = 4
		}},
		{name: "condition added", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Conditions = append(w.Status.Conditions, metav1.Condition{Type: "Attached", Status: metav1.ConditionTrue})
		}},
		{name: "spec generation", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) { w.Generation = 4 }},
		// Two live Tasks of one Session each rewrite the resume demand while
		// a suspension settles; waking on it would let them re-trigger each
		// other with no delay.
		{name: "resume demand rewritten by another waiting Task", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceResumeRequestedAnnotation] = "2026-11-18T00:00:01Z other other-uid"
		}},
		{name: "detach action rewritten", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceDetachActionAnnotation] = "Delete"
		}},
		{name: "demand rewritten with another annotation change", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceResumeRequestedAnnotation] = "2026-11-18T00:00:01Z other other-uid"
			w.Annotations[acpWorkspaceLastSettledTaskAnnotation] = "attached-uid 2"
		}},
		{name: "revocation stamp", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceRevocationStartedAnnotation] = "2 2026-11-18T00:00:00Z"
		}},
		{name: "deleting", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			now := metav1.Now()
			w.DeletionTimestamp = &now
		}},
		{name: "finalizer released", want: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Finalizers = []string{"provider.example.com/cleanup"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := base.DeepCopy()
			tc.mutate(updated)
			if got := predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: updated}); got != tc.want {
				t.Fatalf("Update = %v, want %v", got, tc.want)
			}
		})
	}
	if !predicate.Create(event.CreateEvent{Object: base}) || !predicate.Delete(event.DeleteEvent{Object: base}) {
		t.Fatal("workspace creates and deletes must wake gated Tasks")
	}
	if predicate.Generic(event.GenericEvent{Object: base}) {
		t.Fatal("generic workspace events must not wake Tasks")
	}
}

func TestACPRuntimePoolTaskWakePredicate(t *testing.T) {
	observed := metav1.Now()
	base := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "pool", Generation: 2},
		Status: corev1alpha1.RuntimePoolStatus{
			ObservedGeneration: 2, Lifecycle: corev1alpha1.RuntimePoolLifecycleServing,
			AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
			ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
				PodName: "runtime-0", PodUID: "pod-uid", BootID: "boot-1", LastObservedTime: &observed,
			},
		},
	}
	predicate := acpRuntimePoolTaskWakePredicate()
	for _, tc := range []struct {
		name   string
		mutate func(*corev1alpha1.RuntimePool)
		want   bool
	}{
		{name: "capacity accounting", mutate: func(p *corev1alpha1.RuntimePool) {
			p.Status.Capacity.RunningPrompts = 1
			p.Status.Capacity.Reservations = []corev1alpha1.RuntimePoolCapacityReservationStatus{{TaskUID: "t"}}
		}},
		{name: "message", mutate: func(p *corev1alpha1.RuntimePool) { p.Status.Message = "serving" }},
		{name: "lifecycle", want: true, mutate: func(p *corev1alpha1.RuntimePool) {
			p.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleStarting
		}},
		{name: "admission", want: true, mutate: func(p *corev1alpha1.RuntimePool) {
			p.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
		}},
		{name: "observed generation", want: true, mutate: func(p *corev1alpha1.RuntimePool) { p.Status.ObservedGeneration = 3 }},
		{name: "health probe refreshed the observation time", mutate: func(p *corev1alpha1.RuntimePool) {
			later := metav1.NewTime(observed.Add(time.Minute))
			p.Status.ActiveInstance.LastObservedTime = &later
		}},
		{name: "active instance rebooted", want: true, mutate: func(p *corev1alpha1.RuntimePool) {
			p.Status.ActiveInstance.BootID = "boot-2"
		}},
		{name: "active instance retired", want: true, mutate: func(p *corev1alpha1.RuntimePool) {
			p.Status.ActiveInstance = nil
		}},
		{name: "replaced", want: true, mutate: func(p *corev1alpha1.RuntimePool) { p.UID = "replacement" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated := base.DeepCopy()
			tc.mutate(updated)
			if got := predicate.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: updated}); got != tc.want {
				t.Fatalf("Update = %v, want %v", got, tc.want)
			}
		})
	}
}

// The binding status write returns a newer resourceVersion than the cached
// copy the reconcile started from. The first-pass workspace link (an
// optimistic-lock patch) must start from the written object, not conflict on
// the stale copy; a later pass whose cached copy still predates the binding
// adopts the bound object too.
func TestExecutionBindingLeavesTaskReadyForOptimisticWorkspaceLink(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace(), agent.DeepCopy())
	cached := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), cached); err != nil {
		t.Fatal(err)
	}

	first := cached.DeepCopy()
	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, first, agent); err != nil || handled {
		t.Fatalf("binding = handled=%v err=%v", handled, err)
	}
	if first.Status.AgentExecutionBinding == nil || first.ResourceVersion == cached.ResourceVersion {
		t.Fatalf("binding left the stale Task copy (rv %s, binding %v)", first.ResourceVersion, first.Status.AgentExecutionBinding)
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{
		Namespace: task.Namespace, Name: "acp-ws-first-pass", UID: "ws-uid",
	}}
	if err := reconciler.linkTaskToACPWorkspace(ctx, first, workspace); err != nil {
		t.Fatalf("first-pass workspace link: %v", err)
	}

	lagging := cached.DeepCopy()
	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, lagging, agent); err != nil || handled {
		t.Fatalf("lagging binding pass = handled=%v err=%v", handled, err)
	}
	if lagging.Status.AgentExecutionBinding == nil || lagging.ResourceVersion != first.ResourceVersion ||
		lagging.Labels[acpExecutionWorkspaceLinkLabel] != workspace.Name {
		t.Fatalf("lagging pass did not adopt the bound Task (rv %s want %s, labels %v)",
			lagging.ResourceVersion, first.ResourceVersion, lagging.Labels)
	}
}

// A cached copy that predates the binding while the API server already shows
// the Task past its pending stage is never adopted into the pending pass: the
// pass requeues and lifecycle handling re-enters once the cache catches up.
func TestExecutionBindingRefusesToAdoptAnAdvancedTask(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := bindingTestTask()
	task.Status.Phase = corev1alpha1.TaskPhasePending
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace(), agent.DeepCopy())
	cached := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), cached); err != nil {
		t.Fatal(err)
	}
	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, cached.DeepCopy(), agent); err != nil || handled {
		t.Fatalf("binding = handled=%v err=%v", handled, err)
	}
	advanced := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), advanced); err != nil {
		t.Fatal(err)
	}
	advanced.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	if err := reconciler.Status().Update(ctx, advanced); err != nil {
		t.Fatal(err)
	}

	lagging := cached.DeepCopy()
	result, err, handled := reconciler.ensureAgentExecutionBinding(ctx, lagging, agent)
	if err != nil || !handled || result.RequeueAfter <= 0 {
		t.Fatalf("lagging pass on an advanced Task = result=%+v handled=%v err=%v, want a requeue", result, handled, err)
	}
	if lagging.Status.AgentExecutionBinding != nil || lagging.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("the pending pass adopted the advanced Task: phase %q binding %v", lagging.Status.Phase, lagging.Status.AgentExecutionBinding)
	}
}
