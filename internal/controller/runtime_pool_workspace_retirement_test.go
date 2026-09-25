package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWorkspaceSuspensionPreservesOriginalSessionCleanup(t *testing.T) {
	ctx := context.Background()
	pool := runtimePoolSandboxSuspendTestObject()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolSandboxSuspendTestReconciler(t, runtimePoolWorkspaceTestScheme(t), supervisor, pool)
	sandbox, _, pod, pvc := sandboxSuspendTestReachServing(t, r, pool, supervisor)
	current := runtimePoolTestGetPool(t, r, pool)
	tasks := make([]*corev1alpha1.Task, 0, 2)
	for i := range 2 {
		task := runtimePoolRetirementTask(t, &current, fmt.Sprintf("suspended-turn-%d", i+1))
		task.Status.Execution.RuntimeSessionGeneration = int64(i + 1)
		if err := r.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	sandboxSuspendTestSetIntent(t, r, pool, true)
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		t.Fatalf("drain calls = %d, want 1", supervisor.drainCalls)
	}
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
	runtimePoolReconcile(t, r, pool)
	runtimePoolReconcile(t, r, pool)
	current = runtimePoolTestGetPool(t, r, pool)
	if current.Status.ActiveInstance != nil || sandboxConsensualSuspendRecord(&current) == nil {
		t.Fatalf("suspension did not discard the original instance after recording provider consent: %#v", current.Status)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
		t.Fatal(err)
	}
	if sandbox.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeSuspended {
		t.Fatal("provider suspension was not requested")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("runtime retirement removed the preserved workspace PVC: %v", err)
	}
	dispatcher := &ACPDispatcher{Client: r.Client, APIReader: r.Client}
	for _, task := range tasks {
		if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
			t.Fatal(err)
		}
		if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
			t.Fatalf("provider suspension forgot original runtime custody without a cleanup receipt for %s", task.Name)
		}
		if ready, err := dispatcher.reconcileRecoveredRuntimeSession(ctx, task, task.UID, true, &sessionRuntimeCleanupFence{}); err != nil || !ready {
			t.Fatalf("Session cleanup after suspension = %v, %v", ready, err)
		}
	}
}

func TestWorkspaceSuspensionWaitsForExactDurableTaskCleanup(t *testing.T) {
	for _, test := range []string{"receipt write failure", "Task boot mismatch", "live descendant"} {
		t.Run(test, func(t *testing.T) {
			ctx := context.Background()
			pool := runtimePoolSandboxSuspendTestObject()
			supervisor := &fakeRuntimePoolSupervisorClient{}
			r := runtimePoolSandboxSuspendTestReconciler(t, runtimePoolWorkspaceTestScheme(t), supervisor, pool)
			sandbox, _, pod, _ := sandboxSuspendTestReachServing(t, r, pool, supervisor)
			current := runtimePoolTestGetPool(t, r, pool)
			task := runtimePoolRetirementTask(t, &current, "suspended-turn")
			if test == "Task boot mismatch" {
				task.Status.Execution.RuntimeSessionSupervisorBootID = "foreign-boot"
			}
			if err := r.Create(ctx, task); err != nil {
				t.Fatal(err)
			}
			sandboxSuspendTestSetIntent(t, r, pool, true)
			runtimePoolReconcile(t, r, pool)
			supervisor.probe = runtimePoolValidProbe(pool, &pod, "workspace-boot", true)
			if test == "live descendant" {
				supervisor.probe.Status.Pressure.LiveDescendants = 1
			}
			if test == "receipt write failure" {
				r.Client = &providerRetirementReceiptFailureClient{
					Client: r.Client, err: errors.New("injected workspace cleanup receipt failure"),
				}
			}
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
			if test == "receipt write failure" && (err == nil || !strings.Contains(err.Error(), "injected workspace cleanup receipt failure")) {
				t.Fatalf("suspension before durable Task cleanup = %v, want receipt failure", err)
			}
			if test == "Task boot mismatch" && !errors.Is(err, store.ErrConflict) {
				t.Fatalf("suspension with foreign Task boot = %v, want conflict", err)
			}
			if test == "live descendant" && err != nil {
				t.Fatal(err)
			}
			current = runtimePoolTestGetPool(t, r, pool)
			if current.Status.ActiveInstance == nil || sandboxConsensualSuspendRecord(&current) != nil {
				t.Fatal("unproved Task cleanup discarded runtime authority or committed suspension consent")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), sandbox); err != nil {
				t.Fatal(err)
			}
			if sandbox.Spec.OperatingMode == sandboxv1beta1.SandboxOperatingModeSuspended {
				t.Fatal("unproved Task cleanup suspended the provider")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
				t.Fatal(err)
			}
			if task.Status.Execution.RuntimeSessionCleanupDigest != "" {
				t.Fatal("unproved Task cleanup produced a receipt")
			}
		})
	}
}

func TestProviderWorkspaceDrainPersistsOriginalSessionCleanup(t *testing.T) {
	for _, backend := range []string{"agent-sandbox", "substrate"} {
		for _, operation := range []string{"scale-down", "rollout", "suspend"} {
			if backend == "agent-sandbox" && operation == "suspend" {
				continue // Covered with exact PVC preservation above.
			}
			for _, failReceipt := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/receiptFailure=%t", backend, operation, failReceipt), func(t *testing.T) {
					testProviderWorkspaceDrainTaskCleanup(t, backend, operation, failReceipt)
				})
			}
		}
	}
}

func testProviderWorkspaceDrainTaskCleanup(t *testing.T, backend, operation string, failReceipt bool) {
	t.Helper()
	ctx := context.Background()
	supervisor := &fakeRuntimePoolSupervisorClient{}
	var r *RuntimePoolReconciler
	var pool *corev1alpha1.RuntimePool
	var pod corev1.Pod
	boot := "workspace-boot"
	if backend == "agent-sandbox" {
		pool = runtimePoolSandboxSuspendTestObject()
		r = runtimePoolSandboxSuspendTestReconciler(t, runtimePoolWorkspaceTestScheme(t), supervisor, pool)
		_, _, pod, _ = sandboxSuspendTestReachServing(t, r, pool, supervisor)
	} else {
		if operation == "suspend" {
			r, pool, supervisor, _ = newSubstrateSuspendTestReconciler(t)
		} else {
			r, pool = runtimePoolSubstrateTestReconciler(t, supervisor, newFakeSubstrateActorControl())
		}
		runtimePoolReconcile(t, r, pool)
		pod = substrateTestProbePod(pool)
		boot = "actor-boot"
		supervisor.probe = runtimePoolValidProbe(pool, &pod, boot, false)
		runtimePoolReconcile(t, r, pool)
	}
	current := runtimePoolTestGetPool(t, r, pool)
	if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("provider fixture was not serving: %s", current.Status.Message)
	}
	task := runtimePoolRetirementTask(t, &current, "provider-turn")
	if err := r.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	if operation == "rollout" {
		current.Spec.Runtime.Profile.Model = runtimePoolTestNextModel
		current.Spec.Runtime.Profile.ProxyCredentialScope = "model:" + runtimePoolTestNextModel
		runtimePoolTestRefreshProfileDigest(t, &current)
	} else {
		current.Spec.DesiredReplicas = 0
		if operation == "suspend" {
			current.Annotations[runtimePoolWorkspaceSuspendAnnotation] = booleanTrueValue
		}
	}
	current.Generation++
	if err := r.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, r, pool)
	if supervisor.drainCalls != 1 {
		state := runtimePoolTestGetPool(t, r, pool).Status
		t.Fatalf("provider fixture did not reach authenticated drain: calls=%d lifecycle=%s message=%s", supervisor.drainCalls, state.Lifecycle, state.Message)
	}
	supervisor.probe = runtimePoolValidProbe(pool, &pod, boot, true)
	if failReceipt {
		r.Client = &providerRetirementReceiptFailureClient{
			Client: r.Client, err: errors.New("injected provider retirement receipt failure"),
		}
	}
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)})
	if failReceipt {
		if err == nil || !strings.Contains(err.Error(), "injected provider retirement receipt failure") {
			t.Fatalf("provider retirement bypassed the Task receipt write: %v", err)
		}
		if current := runtimePoolTestGetPool(t, r, pool); current.Status.ActiveInstance == nil ||
			current.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleQuiescent {
			t.Fatal("failed receipt write advanced the retirement barrier or discarded its original instance")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
		t.Fatalf("authenticated %s %s reached its retirement barrier without an original Session cleanup receipt", backend, operation)
	}
	// The later provider operation must retain the original receipt, even
	// though the mutable pool now has a new generation and possibly profile.
	runtimePoolReconcile(t, r, pool)
	dispatcher := &ACPDispatcher{Client: r.Client, APIReader: r.Client}
	if ready, err := dispatcher.reconcileRecoveredRuntimeSession(ctx, task, task.UID, true, &sessionRuntimeCleanupFence{}); err != nil || !ready {
		t.Fatalf("original Session cleanup after provider retirement = %v, %v", ready, err)
	}
}

type providerRetirementReceiptFailureClient struct {
	client.Client
	err error
}

func (c *providerRetirementReceiptFailureClient) Status() client.SubResourceWriter {
	return &providerRetirementReceiptFailureStatus{SubResourceWriter: c.Client.Status(), err: c.err}
}

type providerRetirementReceiptFailureStatus struct {
	client.SubResourceWriter
	err error
}

func (s *providerRetirementReceiptFailureStatus) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, isTask := obj.(*corev1alpha1.Task); isTask {
		return s.err
	}
	return s.SubResourceWriter.Update(ctx, obj, opts...)
}
