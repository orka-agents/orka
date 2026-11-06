/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// cancelGuardStore records whether a write ran inside the epoch guard.
type cancelGuardStore struct {
	store.DurableControlStore
	entered int
}

func (s *cancelGuardStore) WithControllerEpochMutation(ctx context.Context, _ store.ControllerEpochFence, fn func(context.Context) error) error {
	s.entered++
	return fn(ctx)
}

func TestRequestedAgentTaskCancellation(t *testing.T) {
	agent := func(mutate func(*corev1alpha1.Task)) *corev1alpha1.Task {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{labels.AnnotationCancelRequested: "stop"}},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
			Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
		}
		mutate(task)
		return task
	}
	for _, tc := range []struct {
		name    string
		task    *corev1alpha1.Task
		message string
		ok      bool
	}{
		{name: "running", task: agent(func(*corev1alpha1.Task) {}), message: "stop", ok: true},
		{name: "pending", task: agent(func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhasePending }), message: "stop", ok: true},
		{name: "empty message", task: agent(func(t *corev1alpha1.Task) {
			t.Annotations[labels.AnnotationCancelRequested] = " "
		}), message: "cancellation requested", ok: true},
		{name: "no request", task: agent(func(t *corev1alpha1.Task) { t.Annotations = nil })},
		{name: "succeeded", task: agent(func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhaseSucceeded })},
		{name: "already cancelled", task: agent(func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhaseCancelled })},
		{name: "native", task: agent(func(t *corev1alpha1.Task) { t.Spec.Type = corev1alpha1.TaskTypeAI })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, ok := requestedAgentTaskCancellation(tc.task)
			if message != tc.message || ok != tc.ok {
				t.Fatalf("requestedAgentTaskCancellation() = %q, %v; want %q, %v", message, ok, tc.message, tc.ok)
			}
		})
	}
}

func TestCancelRequestedAgentTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := sqlite.NewStore(db, "test")
	controls := &cancelGuardStore{DurableControlStore: base}
	epochs := NewControllerEpochManager(base, "controller")
	done := make(chan error, 1)
	go func() { done <- epochs.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}

	task := func(name string, mutate func(*corev1alpha1.Task)) *corev1alpha1.Task {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: testNS, UID: types.UID(name + "-uid"),
				Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by alice: wrong repository"},
			},
			Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
			Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
		}
		mutate(task)
		return task
	}
	running := task("running", func(t *corev1alpha1.Task) {
		t.Status.Execution = &corev1alpha1.TaskExecutionStatus{ControllerEpoch: fence.Epoch, Attempt: 1, PromptID: "prompt-1"}
	})
	queued := task("queued", func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhasePending })
	finished := task("finished", func(t *corev1alpha1.Task) { t.Status.Phase = corev1alpha1.TaskPhaseSucceeded })
	objects := []client.Object{running, queued, finished}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objects...).WithStatusSubresource(objects...).Build()

	reconciler := &TaskReconciler{Client: c, DurableControlStore: controls, ControllerEpochManager: epochs}
	for name, want := range map[string]corev1alpha1.TaskPhase{
		"running":  corev1alpha1.TaskPhaseCancelled,
		"queued":   corev1alpha1.TaskPhaseCancelled,
		"finished": corev1alpha1.TaskPhaseSucceeded,
	} {
		t.Run(name, func(t *testing.T) {
			current := &corev1alpha1.Task{}
			key := types.NamespacedName{Namespace: testNS, Name: name}
			if err := c.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.cancelRequestedAgentTask(ctx, current); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, key, current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != want {
				t.Fatalf("phase = %s, want %s", current.Status.Phase, want)
			}
			if guarded := controls.entered > 0; guarded != (name == "running") {
				t.Fatalf("epoch guard entered = %d; want it only for the Task with ACP execution", controls.entered)
			}
			controls.entered = 0
			if want == corev1alpha1.TaskPhaseCancelled &&
				(current.Status.Message != "cancelled by alice: wrong repository" || current.Status.CompletionTime == nil) {
				t.Fatalf("status = %+v, want the request's message and a completion time", current.Status)
			}
		})
	}

	t.Run("an ACP Task without the epoch guard stays running", func(t *testing.T) {
		unguarded := task("unguarded", func(t *corev1alpha1.Task) {
			t.Status.Execution = &corev1alpha1.TaskExecutionStatus{ControllerEpoch: fence.Epoch}
		})
		c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(unguarded).WithStatusSubresource(unguarded).Build()
		r := &TaskReconciler{Client: c}
		if _, err := r.cancelRequestedAgentTask(ctx, unguarded.DeepCopy()); err == nil || !strings.Contains(err.Error(), "epoch mutation guard") {
			t.Fatalf("error = %v, want the epoch guard refusal", err)
		}
		current := &corev1alpha1.Task{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(unguarded), current); err != nil {
			t.Fatal(err)
		}
		if current.Status.Phase != corev1alpha1.TaskPhaseRunning {
			t.Fatalf("phase = %s, want Running", current.Status.Phase)
		}
	})
}

func TestRequestedCancellationMessageReadsTheLiveTask(t *testing.T) {
	requested := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "requested", Namespace: testNS, UID: "requested-uid",
		Annotations: map[string]string{labels.AnnotationCancelRequested: " cancelled by alice: wrong repository "},
	}}
	plain := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: testNS, UID: "plain-uid"}}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(requested, plain).Build()
	d := &ACPDispatcher{Client: c, APIReader: c}
	// The dispatcher's copy predates the request.
	stale := requested.DeepCopy()
	stale.Annotations = nil
	if got := d.requestedCancellationMessage(context.Background(), stale); got != "cancelled by alice: wrong repository" {
		t.Fatalf("message = %q, want the live request", got)
	}
	if got := d.requestedCancellationMessage(context.Background(), plain); got != "" {
		t.Fatalf("message = %q, want none without a request", got)
	}
	replaced := requested.DeepCopy()
	replaced.UID = "older-uid"
	if got := d.requestedCancellationMessage(context.Background(), replaced); got != "" {
		t.Fatalf("message = %q, want none for a replaced Task", got)
	}
}

func TestACPMCPBrokerRefusesCallsOnceCancellationIsRequested(t *testing.T) {
	request, _ := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
	fence := store.ControllerEpochFence{Epoch: int64(request.Metadata.Fence.ControllerEpoch)}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: acpDispatcherTestTaskName, Namespace: request.Namespace, UID: types.UID(request.Metadata.TaskUID)},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, Attempt: int32(request.Metadata.TaskAttempt),
			PromptID: string(request.Metadata.PromptID), RuntimeInstanceID: string(request.Metadata.Fence.RuntimeInstanceID),
			RuntimeSessionUID:        string(request.Authorization.RuntimeSessionUID),
			RuntimeSessionGeneration: int64(request.Authorization.SessionGeneration), ControllerEpoch: fence.Epoch,
		}},
	}
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task).Build()
	if _, _, err := findACPMCPTaskExecution(context.Background(), reader, request, fence); err != nil {
		t.Fatalf("running Task: %v", err)
	}

	// The Task is still Running: the controller has not reconciled the request.
	task.Annotations = map[string]string{labels.AnnotationCancelRequested: "cancelled by alice: wrong repository"}
	reader = fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task).Build()
	if _, _, err := findACPMCPTaskExecution(context.Background(), reader, request, fence); !errors.Is(err, errACPMCPTaskCancelled) {
		t.Fatalf("error = %v, want the cancelled-Task refusal", err)
	}
}

// Cancelling a standalone Agent Task while its RuntimeSession is being created
// aborts the create call. The run settles as Cancelled with the request's
// message instead of a RuntimeSessionStartFailed failure.
func TestACPDispatcherSettlesCancellationDuringRuntimeSessionCreate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const request = "cancelled by alice: wrong repository"
	var fixture *taskScopedCreateConflictFixture
	createStarted := make(chan struct{}, 1)
	fixture = newTaskScopedCreateConflictFixture(t, ctx, "cancel-during-create", types.UID("99999999-1111-2222-3333-444444444444"),
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, kubeClient *client.Client) *httptest.Server {
			runtimeServer := newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{})
			t.Cleanup(runtimeServer.Close)
			return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/v2/runtime-sessions/") ||
					strings.Count(r.URL.Path, "/") != 3 {
					runtimeServer.Config.Handler.ServeHTTP(w, r)
					return
				}
				// Record the cancel request as the API does and the Cancelled
				// phase as the controller does, then hold the create until the
				// dispatcher's cancellation watcher aborts it.
				if err := cancelTaskForTest(r.Context(), *kubeClient, fixture.task, request); err != nil {
					t.Errorf("cancel the Task: %v", err)
				}
				select {
				case createStarted <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
				case <-ctx.Done():
				}
			}))
		})
	defer fixture.stop()

	dispatchQueuedTask(ctx, t, fixture.dispatcher, fixture.task.DeepCopy())
	select {
	case <-createStarted:
	default:
		t.Fatal("the RuntimeSession create was never sent")
	}
	current := fixture.currentTask(t, ctx)
	if current.Status.Phase != corev1alpha1.TaskPhaseCancelled || current.Status.Execution == nil ||
		current.Status.Execution.State != corev1alpha1.TaskExecutionStateCancelled ||
		current.Status.Execution.Reason != "Cancelled" {
		t.Fatalf("status = %#v, want a Cancelled run", current.Status)
	}
	if current.Status.Message != request || current.Status.Execution.Message != request {
		t.Fatalf("messages = %q / %q, want the cancel request", current.Status.Message, current.Status.Execution.Message)
	}
	attempt, err := fixture.dispatcher.Store.GetPromptAttempt(ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionCancelled || attempt.OutcomeMarker != request {
		t.Fatalf("attempt = %s %q, want Cancelled with the cancel request", attempt.ExecutionState, attempt.OutcomeMarker)
	}
}

func cancelTaskForTest(ctx context.Context, c client.Client, task *corev1alpha1.Task, message string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &corev1alpha1.Task{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			return err
		}
		if current.Annotations[labels.AnnotationCancelRequested] != message {
			if current.Annotations == nil {
				current.Annotations = map[string]string{}
			}
			current.Annotations[labels.AnnotationCancelRequested] = message
			if err := c.Update(ctx, current); err != nil {
				return err
			}
		}
		current.Status.Phase = corev1alpha1.TaskPhaseCancelled
		current.Status.Message = message
		return c.Status().Update(ctx, current)
	})
}
