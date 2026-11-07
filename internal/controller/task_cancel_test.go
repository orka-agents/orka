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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
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
			if err := reconciler.cancelRequestedAgentTask(ctx, current); err != nil {
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
		if err := r.cancelRequestedAgentTask(ctx, unguarded.DeepCopy()); err == nil || !strings.Contains(err.Error(), "epoch mutation guard") {
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

// Cancelling a class-backed Agent Task while its workspace attachment is
// pending, before any execution status exists, still settles the attachment.
// With artifact retirement enabled, completion and deletion recognize the
// Task as ended before an attempt and finish.
func TestCancelledTaskSettlesItsPendingClassWorkspaceAttachment(t *testing.T) {
	ctx := context.Background()
	r, task, workspaceKey, retirer := cancelTaskWithPendingClassWorkspace(t)
	key := client.ObjectKeyFromObject(task)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}

	// Completion retires artifact references and settles the workspace.
	settled := false
	for range 5 {
		if err := r.Get(ctx, key, task); err != nil {
			t.Fatal(err)
		}
		result, err := r.handleCompleted(ctx, task)
		if err != nil {
			t.Fatalf("handleCompleted: %v", err)
		}
		if result.RequeueAfter >= 30*time.Second {
			t.Fatalf("handleCompleted requeue = %v, want completion to proceed", result.RequeueAfter)
		}
		err = r.Get(ctx, workspaceKey, workspace)
		if apierrors.IsNotFound(err) || (err == nil && workspace.Spec.Attachment == nil) {
			settled = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !settled {
		t.Fatalf("workspace attachment = %+v, want it revoked", workspace.Spec.Attachment)
	}
	if len(retirer.calls) != 1 {
		t.Fatalf("artifact retirement calls = %d, want one", len(retirer.calls))
	}

	// Deletion reclaims the (absent) attempts and releases the finalizer.
	if err := r.Delete(ctx, task); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := r.Get(ctx, key, task); apierrors.IsNotFound(err) {
			return
		} else if err != nil {
			t.Fatal(err)
		}
		if _, err := r.handleDeletion(ctx, task); err != nil {
			t.Fatalf("handleDeletion: %v", err)
		}
	}
	t.Fatalf("finalizers = %v, want the deleted Task released", task.Finalizers)
}

// cancelTaskWithPendingClassWorkspace cancels a class-backed Agent Task whose
// workspace attachment is written but not yet acknowledged, with artifact
// retirement enabled, and returns it Cancelled.
func cancelTaskWithPendingClassWorkspace(t *testing.T) (*TaskReconciler, *corev1alpha1.Task, types.NamespacedName, *recordingIdentityRetirer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fixture := newACPClassFixture(t, RuntimeProviderBackendAgentSandbox)
	task := acpClassTestTask(func(task *corev1alpha1.Task) {
		task.Finalizers = []string{labels.TaskFinalizer}
	})
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	controls := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controls, "controller")
	done := make(chan error, 1)
	go func() { done <- epochs.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	retirer := &recordingIdentityRetirer{}
	r.DurableControlStore, r.ControllerEpochManager, r.ACPArtifactRetirer = controls, epochs, retirer

	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: acpTestSandboxPoolName, Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	workspaceName := acpClassWorkspaceName(task, binding)
	workspaceKey := types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, workspaceKey, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	admitTestACPWorkspace(t, r, workspace)
	// The attach pass writes the attachment; the adapter has not acknowledged it.
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || ready {
		t.Fatalf("attach pass = (%v, %v), want a pending attachment", ready, err)
	}
	if err := r.Get(ctx, workspaceKey, workspace); err != nil || workspace.Spec.Attachment == nil ||
		workspace.Spec.Attachment.TaskRef.UID != task.UID {
		t.Fatalf("workspace attachment = %+v, %v; want this Task's pending attachment", workspace.Spec.Attachment, err)
	}

	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	if err := r.Get(ctx, key, task); err != nil {
		t.Fatal(err)
	}
	if task.Labels[acpExecutionWorkspaceLinkLabel] != workspaceName || task.Status.Execution != nil {
		t.Fatalf("task link = %q, execution = %+v; want a linked Task without execution status",
			task.Labels[acpExecutionWorkspaceLinkLabel], task.Status.Execution)
	}
	task.Annotations[labels.AnnotationCancelRequested] = "cancelled by alice: wrong repository"
	if err := r.Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	task.Status.Phase = corev1alpha1.TaskPhasePending
	if err := r.Status().Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := r.cancelRequestedAgentTask(ctx, task); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := r.Get(ctx, key, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseCancelled || task.Status.Execution == nil ||
		task.Status.Execution.State != corev1alpha1.TaskExecutionStateCancelled || task.Status.Execution.Attempt != 0 ||
		!acpTaskRequiresAuthoritativeAttemptDiscovery(task) {
		t.Fatalf("status = %+v, want a Cancelled record cleanup recognizes as ended before an attempt", task.Status)
	}

	return r, task, workspaceKey, retirer
}

// A recorded cancel request stops a prepared publication before the
// controller writes the Cancelled phase.
func TestPublicationHonorsARecordedCancelRequest(t *testing.T) {
	running := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: testNS, UID: "running-uid"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	requested := running.DeepCopy()
	requested.Name, requested.UID = "requested", "requested-uid"
	requested.Annotations = map[string]string{labels.AnnotationCancelRequested: "cancelled by alice: wrong repository"}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(running, requested).Build()
	d := &ACPDispatcher{Client: c, APIReader: c}
	for task, want := range map[*corev1alpha1.Task]bool{running: false, requested: true} {
		got, err := d.taskCancellationRequested(context.Background(), task)
		if err != nil || got != want {
			t.Fatalf("%s: taskCancellationRequested() = %v, %v; want %v", task.Name, got, err, want)
		}
	}
}

// A write Task cancelled after its prompt succeeded but before publication
// pushed keeps who cancelled it and why.
func TestCancelTaskAfterExecutionKeepsTheCancelRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	controls := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controls, "controller")
	done := make(chan error, 1)
	go func() { done <- epochs.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	const request = "cancelled by alice: wrong repository"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "write", Namespace: testNS, UID: "write-uid",
			Annotations: map[string]string{labels.AnnotationCancelRequested: request},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-1",
		}},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task).WithStatusSubresource(task).Build()
	d := &ACPDispatcher{Client: c, APIReader: c, Store: controls, Epochs: epochs}
	delivery := corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateCancelledBeforePublish, Outcome: corev1alpha1.TaskDeliveryOutcomeCancelledBeforePublish,
	}
	if err := d.cancelTaskAfterExecution(ctx, task.DeepCopy(), delivery, "publication cancelled before push"); err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Task{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseCancelled || current.Status.Message != request {
		t.Fatalf("status = %s %q, want Cancelled with the cancel request", current.Status.Phase, current.Status.Message)
	}
}

// A PromptAttempt persisted before a lost status write leaves a Task without
// execution status, which recovery never visits. Cancelling it settles the
// attempt and binds it to the terminal execution.
func TestCancelRequestedAgentTaskSettlesAnUnprojectedAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	controls := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controls, "controller")
	done := make(chan error, 1)
	go func() { done <- epochs.Start(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}

	const request = "cancelled by alice: wrong repository"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unprojected", Namespace: testNS, UID: "unprojected-uid",
			Annotations: map[string]string{labels.AnnotationCancelRequested: request},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	key := store.PromptAttemptKey{Namespace: testNS, TaskUID: string(task.UID), Attempt: 1, PromptID: "prompt-unprojected-uid-1"}
	attemptID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controls.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: testControlDigestForDispatcher("unprojected-request"),
		ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested, CreatedAt: time.Now().UTC(),
	}), fence); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task).WithStatusSubresource(task).Build()
	guarded := &cancelGuardStore{DurableControlStore: controls}
	r := &TaskReconciler{Client: c, DurableControlStore: guarded, ControllerEpochManager: epochs}
	if err := r.cancelRequestedAgentTask(ctx, task.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if guarded.entered == 0 {
		t.Fatal("the status bound to the settled attempt was written outside the epoch guard")
	}

	attempt, err := controls.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionCancelled || attempt.OutcomeMarker != request {
		t.Fatalf("attempt = %s %q, want Cancelled with the cancel request", attempt.ExecutionState, attempt.OutcomeMarker)
	}
	current := &corev1alpha1.Task{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	execution := current.Status.Execution
	if current.Status.Phase != corev1alpha1.TaskPhaseCancelled || execution == nil ||
		execution.State != corev1alpha1.TaskExecutionStateCancelled || execution.Attempt != 1 || execution.PromptID != key.PromptID ||
		current.Status.Delivery == nil || current.Status.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested {
		t.Fatalf("status = %#v, want Cancelled bound to the settled attempt", current.Status)
	}
}

// A workspace status projection that keeps failing must not keep a
// cancelled class-backed Agent Task running: the Cancelled phase, which
// stops the dispatcher's run, is written first, and a later reconcile still
// cancels the Tasks it delegated.
func TestReconcileCancelsBeforeAFailingWorkspaceProjection(t *testing.T) {
	const request = "cancelled by alice: wrong repository"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "class-backed", Namespace: testNS, UID: "class-backed-uid",
			Finalizers:  []string{labels.TaskFinalizer},
			Labels:      map[string]string{acpExecutionWorkspaceLinkLabel: "acp-ws-class-backed"},
			Annotations: map[string]string{labels.AnnotationCancelRequested: request},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-1", RuntimePoolName: "acp-ws-class-backed",
			},
			ExecutionWorkspace: &corev1alpha1.ExecutionWorkspaceStatus{Phase: corev1alpha1.ExecutionWorkspacePhaseReady},
		},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, UID: "test-ns-uid"}}
	controller := true
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "delegated", Namespace: testNS, UID: "delegated-uid",
			Labels: map[string]string{labels.LabelParentTask: "class-backed"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "class-backed", UID: "class-backed-uid", Controller: &controller,
			}},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	r := newUnitReconciler(newTestScheme(), task, namespace, child)
	watched, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit reconciler client does not implement client.WithWatch")
	}
	workspaceReads := 0
	r.Client = interceptor.NewClient(watched, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isWorkspace := obj.(*workspacev1alpha1.ExecutionWorkspace); isWorkspace {
				workspaceReads++
				return errors.New("workspace read unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)}); err != nil {
		t.Fatalf("Reconcile() = %v, want the cancellation applied", err)
	}
	current := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseCancelled || current.Status.Message != request {
		t.Fatalf("status = %s %q, want Cancelled with the cancel request", current.Status.Phase, current.Status.Message)
	}
	if workspaceReads != 0 {
		t.Fatalf("workspace reads = %d, want the cancellation applied before the workspace projection", workspaceReads)
	}
	// The next reconcile reaches the failing projection, but cancels the
	// delegated child first.
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	delegated := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), delegated); err != nil {
		t.Fatal(err)
	}
	if _, requested := delegated.Annotations[labels.AnnotationCancelRequested]; !requested {
		t.Fatal("a failing workspace projection must not keep the delegated child running")
	}
}

// TestRuntimeWatcherStopsARunOnACancelRequest covers a cancel request whose
// Cancelled status write keeps failing: the Task stays Running, but the
// runtime watcher sees the request itself and stops the run.
func TestRuntimeWatcherStopsARunOnACancelRequest(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "running", Namespace: "default", UID: "running-uid",
			Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(task).Build()
	dispatcher := &ACPDispatcher{APIReader: reader}
	ctx, cancelTest := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTest()
	runCtx, stopRun := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		dispatcher.watchTaskCancellation(runCtx, stopRun, client.ObjectKeyFromObject(task))
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the watcher kept a Running Task with a cancel request running")
	}
	if runCtx.Err() == nil {
		t.Fatal("the run's context was not cancelled")
	}
}

// TestCancelledTaskCancelsTheChildrenItControls covers delegated work: a
// cancelled parent asks its Pending and Running children to cancel, reaching
// children in other namespaces by the parent UID they record and work that
// outlived a finished child. It leaves finished children, children it does
// not control, and scheduled templates alone.
func TestCancelledTaskCancelsTheChildrenItControls(t *testing.T) {
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled},
	}
	controller := true
	child := func(name string, phase corev1alpha1.TaskPhase, controlled bool, schedule string) *corev1alpha1.Task {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{labels.LabelParentTask: "parent"}},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Schedule: schedule},
			Status:     corev1alpha1.TaskStatus{Phase: phase},
		}
		if controlled {
			task.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "parent", UID: "parent-uid", Controller: &controller,
			}}
		}
		return task
	}
	done := child("done", corev1alpha1.TaskPhaseSucceeded, true, "")
	done.UID = "done-uid"
	grandchild := child("grandchild", corev1alpha1.TaskPhaseRunning, false, "")
	grandchild.Labels[labels.LabelParentTask] = "done"
	grandchild.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "done", UID: "done-uid", Controller: &controller,
	}}
	elsewhere := func(name, parentUID string) *corev1alpha1.Task {
		task := child(name, corev1alpha1.TaskPhaseRunning, false, "")
		task.Namespace = "other"
		task.Annotations = map[string]string{labels.AnnotationParentTaskUID: parentUID}
		return task
	}
	objects := []client.Object{
		parent,
		child("running", corev1alpha1.TaskPhaseRunning, true, ""),
		child("pending", corev1alpha1.TaskPhasePending, true, ""),
		done,
		grandchild,
		child("unowned", corev1alpha1.TaskPhaseRunning, false, ""),
		child("template", corev1alpha1.TaskPhasePending, true, "*/5 * * * *"),
		elsewhere("delegated-elsewhere", "parent-uid"),
		elsewhere("namesake-elsewhere", "another-parent-uid"),
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(objects...).Build()
	// The cache has not seen the child in the other namespace yet; the API
	// server has.
	stale := interceptor.NewClient(c, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			if tasks, ok := list.(*corev1alpha1.TaskList); ok {
				kept := tasks.Items[:0]
				for _, task := range tasks.Items {
					if task.Namespace != "other" {
						kept = append(kept, task)
					}
				}
				tasks.Items = kept
			}
			return nil
		},
	})
	r := &TaskReconciler{Client: stale, APIReader: c}
	if err := r.cancelChildTasks(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[client.ObjectKey]bool{
		{Namespace: "default", Name: "running"}:           true,
		{Namespace: "default", Name: "pending"}:           true,
		{Namespace: "default", Name: "done"}:              false,
		{Namespace: "default", Name: "grandchild"}:        true,
		{Namespace: "default", Name: "unowned"}:           false,
		{Namespace: "default", Name: "template"}:          false,
		{Namespace: "other", Name: "delegated-elsewhere"}: true,
		{Namespace: "other", Name: "namesake-elsewhere"}:  false,
	} {
		got := &corev1alpha1.Task{}
		if err := c.Get(context.Background(), key, got); err != nil {
			t.Fatal(err)
		}
		if _, requested := got.Annotations[labels.AnnotationCancelRequested]; requested != want {
			t.Errorf("%s: cancel requested = %t, want %t", key, requested, want)
		}
	}
	// A second pass changes nothing.
	if err := r.cancelChildTasks(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
}

// TestInterruptedCancelStopsTheJobWhileAChildRefuses covers a parent that
// recovery recorded as Failed after a cancel request, with a child whose
// cancel keeps failing: the cascade still runs and reports the refusal for a
// retry, and the parent's own completion, its Job included, still proceeds.
func TestInterruptedCancelStopsTheJobWhileAChildRefuses(t *testing.T) {
	controller := true
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent", Namespace: testNS, UID: "parent-uid",
			Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed, JobName: "parent-job"},
	}
	owner := []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "parent", UID: "parent-uid", Controller: &controller,
	}}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: testNS, UID: "child-uid", OwnerReferences: owner,
			Labels: map[string]string{labels.LabelParentTask: "parent"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "parent-job", Namespace: testNS, OwnerReferences: owner},
		Status:     batchv1.JobStatus{Active: 1},
	}
	r := newUnitReconciler(newTestScheme(), parent, child, job)
	watched, ok := r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("unit reconciler client does not implement client.WithWatch")
	}
	childPatches := 0
	r.Client = interceptor.NewClient(watched, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if obj.GetName() == "child" {
				childPatches++
				return apierrors.NewForbidden(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), "child", errors.New("denied"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	if err := r.cascadeTaskCancellation(context.Background(), parent); err == nil {
		t.Fatal("a refused child cancel must be reported for a retry")
	}
	if childPatches == 0 {
		t.Fatal("a cancel-requested parent that ended Failed must still cancel its children")
	}
	if _, err := r.handleCompleted(context.Background(), parent); err != nil {
		t.Fatalf("handleCompleted() = %v, want the parent's own completion to proceed", err)
	}
	if err := watched.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("parent Job read = %v, want it deleted while the child refuses", err)
	}
}

// TestCancelReachesLocalChildrenWithNamespaceScopedTaskAccess covers the
// standard install, whose controller may list Tasks only in the namespaces
// it is bound in: a cluster-wide list is forbidden, yet the parent's own
// children are still cancelled, and with namespace isolation on no
// cluster-wide list is tried at all.
func TestCancelReachesLocalChildrenWithNamespaceScopedTaskAccess(t *testing.T) {
	controller := true
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: "default", Labels: map[string]string{labels.LabelParentTask: "parent"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "parent", UID: "parent-uid", Controller: &controller,
			}},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	for _, isolated := range []bool{false, true} {
		c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(parent.DeepCopy(), child.DeepCopy()).Build()
		clusterLists := 0
		scoped := interceptor.NewClient(c, interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				options := &client.ListOptions{}
				options.ApplyOptions(opts)
				if options.Namespace == "" {
					clusterLists++
					return apierrors.NewForbidden(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), "", errors.New("namespace-scoped"))
				}
				return c.List(ctx, list, opts...)
			},
		})
		r := &TaskReconciler{Client: scoped, EnforceNamespaceIsolation: isolated}
		if err := r.cancelChildTasks(context.Background(), parent); err != nil {
			t.Fatalf("isolation %t: cancelChildTasks() = %v, want a forbidden cluster-wide list tolerated", isolated, err)
		}
		got := &corev1alpha1.Task{}
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(child), got); err != nil {
			t.Fatal(err)
		}
		if _, requested := got.Annotations[labels.AnnotationCancelRequested]; !requested {
			t.Fatalf("isolation %t: the parent's own child was not cancelled", isolated)
		}
		if isolated && clusterLists != 0 {
			t.Fatalf("namespace isolation: %d cluster-wide lists, want none", clusterLists)
		}
	}
}

// TestCancelReachesGrandchildrenThroughACancelRequestedChild covers a child
// already asked to cancel whose own cascade never runs, as when its status
// write keeps failing: the cancelled ancestor still reaches its running
// grandchild.
func TestCancelReachesGrandchildrenThroughACancelRequestedChild(t *testing.T) {
	controller := true
	owner := func(name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: name, UID: uid, Controller: &controller}}
	}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: "default", UID: "child-uid", OwnerReferences: owner("parent", "parent-uid"),
			Labels:      map[string]string{labels.LabelParentTask: "parent"},
			Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled with its parent task parent"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	grandchild := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "grandchild", Namespace: "default", UID: "grandchild-uid", OwnerReferences: owner("child", "child-uid"),
			Labels: map[string]string{labels.LabelParentTask: "child"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(parent, child, grandchild).Build()
	if err := (&TaskReconciler{Client: c}).cancelChildTasks(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	got := &corev1alpha1.Task{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(grandchild), got); err != nil {
		t.Fatal(err)
	}
	if _, requested := got.Annotations[labels.AnnotationCancelRequested]; !requested {
		t.Fatal("a running grandchild under a cancel-requested child must be cancelled")
	}
}

// TestJobCreationSeesACancelTheCacheMissed covers a cancel request written
// after the cached copy was read: Job creation re-reads the Task and starts
// no workload.
func TestJobCreationSeesACancelTheCacheMissed(t *testing.T) {
	stale := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "native", Namespace: testNS, UID: "native-uid"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	live := stale.DeepCopy()
	live.Annotations = map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"}
	r := newUnitReconciler(newTestScheme(), live)
	result, err := r.createTaskJob(context.Background(), stale, nil, nil)
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("createTaskJob() = %+v, %v; want creation skipped and the cancel applied on the next pass", result, err)
	}
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("Jobs = %d, want none for a cancel-requested Task", len(jobs.Items))
	}
}

// TestCancelReachesGrandchildrenThroughADeletingChild covers a child being
// deleted whose finalizer keeps it readable: it is not patched, but its
// running grandchild is still cancelled.
func TestCancelReachesGrandchildrenThroughADeletingChild(t *testing.T) {
	controller := true
	owner := func(name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: name, UID: uid, Controller: &controller}}
	}
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled},
	}
	deleting := metav1.Now()
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: "default", UID: "child-uid", OwnerReferences: owner("parent", "parent-uid"),
			Labels: map[string]string{labels.LabelParentTask: "parent"}, DeletionTimestamp: &deleting, Finalizers: []string{labels.TaskFinalizer},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	grandchild := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "grandchild", Namespace: "default", UID: "grandchild-uid", OwnerReferences: owner("child", "child-uid"),
			Labels: map[string]string{labels.LabelParentTask: "child"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(parent, child, grandchild).Build()
	if err := (&TaskReconciler{Client: c}).cancelChildTasks(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"child": false, "grandchild": true} {
		got := &corev1alpha1.Task{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: name}, got); err != nil {
			t.Fatal(err)
		}
		if _, requested := got.Annotations[labels.AnnotationCancelRequested]; requested != want {
			t.Errorf("%s: cancel requested = %t, want %t", name, requested, want)
		}
	}
}

// failingUsageStore refuses to register usage, as during a storage outage.
type failingUsageStore struct{ *sqlite.Store }

func (failingUsageStore) RegisterUsageTask(context.Context, store.UsageTask) error {
	return store.ErrUnavailable
}

// TestUsageOutageStillCancelsDelegatedWork covers a cancelled Task whose
// usage registration keeps failing: the reconcile fails early, but its
// running child is still asked to cancel.
func TestUsageOutageStillCancelsDelegatedWork(t *testing.T) {
	controller := true
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "parent", Namespace: testNS, UID: "parent-uid",
			Annotations: map[string]string{labels.AnnotationCancelRequested: "cancelled by alice"},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child", Namespace: testNS, UID: "child-uid", Labels: map[string]string{labels.LabelParentTask: "parent"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "parent", UID: "parent-uid", Controller: &controller,
			}},
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testNS, UID: "test-ns-uid"}}
	r := newUnitReconciler(newTestScheme(), parent, child, namespace)
	r.ExecutionEventStore = failingUsageStore{r.ExecutionEventStore.(*sqlite.Store)}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(parent)})
	got := &corev1alpha1.Task{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), got); err != nil {
		t.Fatal(err)
	}
	if _, requested := got.Annotations[labels.AnnotationCancelRequested]; !requested {
		t.Fatal("a usage storage outage must not keep the delegated child running")
	}
}
