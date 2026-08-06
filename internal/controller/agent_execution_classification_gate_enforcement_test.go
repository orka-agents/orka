package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestAgentExecutionClassificationGatedRunnableCancelsAndRestarts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	control := classifiedReadinessControl()
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentExecutionControl{}).
		WithObjects(control).
		Build()

	started := make(chan int, 4)
	stopped := make(chan int, 4)
	startCount := 0
	child := manager.RunnableFunc(func(ctx context.Context) error {
		startCount++
		instance := startCount
		started <- instance
		<-ctx.Done()
		stopped <- instance
		return ctx.Err()
	})
	const interval = 5 * time.Millisecond
	runnable := &AgentExecutionClassificationGatedRunnable{
		Gate:     &AgentExecutionClassificationGate{APIReader: cl},
		Runnable: child,
		Interval: interval,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- runnable.Start(ctx) }()

	requireClassificationGateEvent(t, started, 1, "first child start")
	updateClassificationStateForTest(t, cl, corev1alpha1.AgentExecutionClassificationOpen)
	requireClassificationGateEvent(t, stopped, 1, "first child cancellation")
	requireNoClassificationGateEvent(t, started, 10*interval, "child restart while classification is Open")

	updateClassificationStateForTest(t, cl, corev1alpha1.AgentExecutionClassificationSealed)
	requireClassificationGateEvent(t, started, 2, "second child start after reseal")

	cancel()
	requireClassificationGateEvent(t, stopped, 2, "second child cancellation")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("classification-gated runnable did not stop with its parent context")
	}
}

func TestTaskReconcilerWithholdsAgentTaskForOpenOrStaleClassification(t *testing.T) {
	for _, testCase := range classificationGateWithholdingCases() {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			control := classifiedReadinessControl()
			testCase.mutate(control)
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "withheld", Namespace: "default", UID: types.UID("withheld-uid")},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
			}
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.Task{}).
				WithObjects(control, task).
				Build()
			reconciler := &TaskReconciler{
				Client:                           cl,
				AgentExecutionClassificationGate: &AgentExecutionClassificationGate{APIReader: cl},
			}
			key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
			before := &corev1alpha1.Task{}
			if err := cl.Get(context.Background(), key, before); err != nil {
				t.Fatal(err)
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != time.Second {
				t.Fatalf("RequeueAfter = %s, want 1s", result.RequeueAfter)
			}
			after := &corev1alpha1.Task{}
			if err := cl.Get(context.Background(), key, after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("agent Task mutated while classification was %s\nbefore: %#v\nafter:  %#v", testCase.name, before, after)
			}
		})
	}
}

func TestRuntimePoolReconcilerWithholdsForOpenOrStaleClassification(t *testing.T) {
	for _, testCase := range classificationGateWithholdingCases() {
		t.Run(testCase.name, func(t *testing.T) {
			scheme := runtimePoolTestScheme(t)
			control := classifiedReadinessControl()
			testCase.mutate(control)
			pool := runtimePoolTestObject(1)
			reconciler := runtimePoolTestReconciler(t, scheme, nil, control, pool)
			reconciler.AgentExecutionClassificationGate = &AgentExecutionClassificationGate{APIReader: reconciler.Client}
			key := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}
			before := &corev1alpha1.RuntimePool{}
			if err := reconciler.Get(context.Background(), key, before); err != nil {
				t.Fatal(err)
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != time.Second {
				t.Fatalf("RequeueAfter = %s, want 1s", result.RequeueAfter)
			}
			after := &corev1alpha1.RuntimePool{}
			if err := reconciler.Get(context.Background(), key, after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("RuntimePool mutated while classification was %s\nbefore: %#v\nafter:  %#v", testCase.name, before, after)
			}
			deployments := &appsv1.DeploymentList{}
			if err := reconciler.List(context.Background(), deployments, client.InNamespace(pool.Namespace)); err != nil {
				t.Fatal(err)
			}
			if len(deployments.Items) != 0 {
				t.Fatalf("created %d RuntimePool Deployment(s) while classification was %s", len(deployments.Items), testCase.name)
			}
		})
	}
}

func classificationGateWithholdingCases() []struct {
	name   string
	mutate func(*corev1alpha1.AgentExecutionControl)
} {
	return []struct {
		name   string
		mutate func(*corev1alpha1.AgentExecutionControl)
	}{
		{
			name: "Open",
			mutate: func(control *corev1alpha1.AgentExecutionControl) {
				control.Status.Classification.State = corev1alpha1.AgentExecutionClassificationOpen
			},
		},
		{
			name: "stale seal",
			mutate: func(control *corev1alpha1.AgentExecutionControl) {
				control.Status.Classification.ControlGeneration--
			},
		},
	}
}

func updateClassificationStateForTest(
	t *testing.T,
	cl client.Client,
	state corev1alpha1.AgentExecutionClassificationState,
) {
	t.Helper()
	control := &corev1alpha1.AgentExecutionControl{}
	key := types.NamespacedName{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}
	if err := cl.Get(context.Background(), key, control); err != nil {
		t.Fatal(err)
	}
	control.Status.Classification.State = state
	if err := cl.Status().Update(context.Background(), control); err != nil {
		t.Fatalf("update classification state to %s: %v", state, err)
	}
}

func requireClassificationGateEvent(t *testing.T, events <-chan int, want int, description string) {
	t.Helper()
	select {
	case got := <-events:
		if got != want {
			t.Fatalf("%s instance = %d, want %d", description, got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func requireNoClassificationGateEvent(t *testing.T, events <-chan int, duration time.Duration, description string) {
	t.Helper()
	select {
	case got := <-events:
		t.Fatalf("unexpected %s: instance %d", description, got)
	case <-time.After(duration):
	}
}
