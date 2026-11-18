/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// A provider status write on a linked ExecutionWorkspace reconciles the
// waiting Task through the watch, with no Task event and no RequeueAfter
// poll, and status churn the gates ignore does not. A Task reaching Queued
// wakes the ACP dispatcher the same way.
func TestACPWorkspaceStatusChangeWakesLinkedTaskWithoutPolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true, BinaryAssetsDirectory: getFirstFoundEnvTestBinaryDir(),
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := environment.Stop(); err != nil {
			t.Errorf("stop wake API server: %v", err)
		}
	})
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: newTestScheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan reconcile.Request, 64)
	probe := ctrl.NewControllerManagedBy(mgr).Named("acp-task-wake-probe").
		// Task events never reach the probe: only the wake watches can.
		For(&corev1alpha1.Task{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(client.Object) bool { return false })))
	probe, err = registerACPTaskWakeWatches(ctx, mgr, probe)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Complete(reconcile.Func(func(_ context.Context, request reconcile.Request) (reconcile.Result, error) {
		requests <- request
		return reconcile.Result{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	managerDone := make(chan error, 1)
	go func() { managerDone <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-managerDone; err != nil {
			t.Errorf("manager: %v", err)
		}
	})
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache never synced")
	}
	dispatcher := &ACPDispatcher{Informers: mgr.GetCache()}
	defer dispatcher.registerDispatchWakeHandlers(ctx)()

	c := mgr.GetClient()
	const namespace = "acp-wake"
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatal(err)
	}
	workspace := validExecutionWorkspace(namespace, "acp-ws-wake", "class", "provider")
	workspace.Labels = map[string]string{workspacev1alpha1.ProviderControllerLabel: "sandbox.provider.example.com"}
	workspace.Annotations = map[string]string{acpExecutionWorkspacePoolAnnotation: "acp-ws-wake-pool"}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: "waiting",
			Labels: map[string]string{acpExecutionWorkspaceLinkLabel: workspace.Name},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "continue", AgentRef: &corev1alpha1.AgentReference{Name: "agent"},
		},
	}
	if err := c.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	want := types.NamespacedName{Namespace: namespace, Name: task.Name}
	expectWake := func(step string, within time.Duration) {
		t.Helper()
		deadline := time.After(within)
		for {
			select {
			case request := <-requests:
				if request.NamespacedName == want {
					return
				}
			case <-deadline:
				t.Fatalf("%s: the linked Task was not reconciled within %s", step, within)
			}
		}
	}
	expectQuiet := func(step string, within time.Duration) {
		t.Helper()
		select {
		case request := <-requests:
			t.Fatalf("%s: unexpected wakeup for %s", step, request.NamespacedName)
		case <-time.After(within):
		}
	}

	if err := c.Create(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	expectWake("workspace created", 10*time.Second)
	expectQuiet("after create", 300*time.Millisecond)

	// The provider acknowledges the current generation: well inside one 2s
	// gate poll, the watch alone reconciles the Task.
	updateStatus := func(mutate func(*workspacev1alpha1.ExecutionWorkspace)) {
		t.Helper()
		current := &workspacev1alpha1.ExecutionWorkspace{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
			t.Fatal(err)
		}
		mutate(current)
		if err := c.Status().Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}
	updateStatus(func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.ObservedGeneration = w.Generation
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
		meta.SetStatusCondition(&w.Status.Conditions, metav1.Condition{
			Type: string(workspacev1alpha1.ConditionWorkspaceProvisioned), Status: metav1.ConditionTrue,
			Reason: "Provisioned", Message: "provisioned", ObservedGeneration: w.Generation,
		})
	})
	expectWake("provider acknowledged", 1500*time.Millisecond)

	// Rewording a condition message changes nothing a gate reads.
	updateStatus(func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.Conditions[0].Message = "provisioned and healthy"
	})
	expectQuiet("condition message", 750*time.Millisecond)

	// A Task reaching Queued wakes the dispatcher before its next tick.
	select {
	case <-dispatcher.wakeChannel():
	default:
	}
	current := &corev1alpha1.Task{}
	if err := c.Get(ctx, want, current); err != nil {
		t.Fatal(err)
	}
	now := metav1.Now()
	current.Status.Phase = corev1alpha1.TaskPhasePending
	current.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: "prompt-1",
		RuntimePoolName: "acp-ws-wake-pool", RuntimePoolUID: "pool-uid", LastTransitionTime: &now,
	}
	if err := c.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	select {
	case <-dispatcher.wakeChannel():
	case <-time.After(5 * time.Second):
		t.Fatal("a newly queued Task did not wake the dispatcher")
	}
}
