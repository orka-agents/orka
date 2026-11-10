package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// errInjectedProjectionEnqueue fails a terminal projection enqueue.
var errInjectedProjectionEnqueue = errors.New("injected terminal projection enqueue failure")

// settlingValidationStore adds to validationRecoveryStore the attempt
// transitions and terminal projections a recovery pass that records an
// unknown outcome writes. It fails the next enqueueFailures enqueues, as a
// store that is briefly unavailable would, or, with lostEnqueueResponse,
// stores the projection and still reports the failure, as a lost response
// would.
type settlingValidationStore struct {
	*validationRecoveryStore
	projections         map[string]*store.OutboxProjection
	enqueueFailures     int
	lostEnqueueResponse bool
}

func (s *settlingValidationStore) GetPromptAttempt(_ context.Context, id string) (*store.PromptAttempt, error) {
	if s.attempt == nil || id != s.attempt.ID {
		return nil, store.ErrNotFound
	}
	attempt := *s.attempt
	return &attempt, nil
}

func (s *settlingValidationStore) TransitionPromptAttemptDelivery(
	_ context.Context, transition store.PromptAttemptDeliveryTransition,
) (*store.PromptAttempt, error) {
	if transition.ID != s.attempt.ID || transition.ExpectedVersion != s.attempt.Version || transition.ExpectedState != s.attempt.DeliveryState {
		return nil, store.ErrConflict
	}
	s.attempt.DeliveryState = transition.NewState
	s.attempt.Version++
	attempt := *s.attempt
	return &attempt, nil
}

func (s *settlingValidationStore) TransitionPromptAttemptExecution(
	_ context.Context, transition store.PromptAttemptExecutionTransition,
) (*store.PromptAttempt, error) {
	if transition.ID != s.attempt.ID || transition.ExpectedVersion != s.attempt.Version || transition.ExpectedState != s.attempt.ExecutionState {
		return nil, store.ErrConflict
	}
	s.attempt.ExecutionState = transition.NewState
	s.attempt.TerminalReason = transition.TerminalReason
	s.attempt.OutcomeMarker = transition.OutcomeMarker
	s.attempt.Version++
	attempt := *s.attempt
	return &attempt, nil
}

func (s *settlingValidationStore) GetOutboxProjection(_ context.Context, id string) (*store.OutboxProjection, error) {
	if projection, ok := s.projections[id]; ok {
		return projection, nil
	}
	return nil, store.ErrNotFound
}

func (s *settlingValidationStore) EnqueueOutboxProjection(
	_ context.Context, projection *store.OutboxProjection, _ store.ControllerEpochFence,
) (*store.OutboxProjection, error) {
	if s.enqueueFailures > 0 {
		s.enqueueFailures--
		if s.lostEnqueueResponse {
			s.projections[projection.ID] = projection
		}
		return nil, errInjectedProjectionEnqueue
	}
	if _, ok := s.projections[projection.ID]; ok {
		return nil, store.ErrConflict
	}
	s.projections[projection.ID] = projection
	return projection, nil
}

func (s *residentValidatingSupervisor) deletedSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleted
}

// When the terminal projection cannot be enqueued after recovery recorded a
// standalone Task's validation conflict and unknown outcome on its attempt,
// the Task records the conflict but not the unknown outcome. A later pass
// records the outcome from the attempt before runtime cleanup, keeping the
// Task's runtime identity, so cleanup finishes the validation the runtime
// still holds and the Task moves to the recovering epoch, whether the runtime
// still runs under the replaced controller's epoch or was adopted by the
// recovering one. A projection whose enqueue response was lost is left to the
// outbox projector, and cleanup waits for it.
func TestRecoveryRecordsTheValidationOutcomeBeforeCleanupAfterAFailedEnqueue(t *testing.T) {
	for _, test := range []struct {
		name                  string
		adopted, lostResponse bool
	}{
		{name: "runtime under the replaced epoch"},
		{name: "runtime adopted by the recovering epoch", adopted: true},
		{name: "lost enqueue response", lostResponse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testRecoveredValidationOutcomeAfterFailedEnqueue(t, test.adopted, test.lostResponse)
		})
	}
}

//nolint:gocyclo // One recovery sequence with its checks after each step.
func testRecoveredValidationOutcomeAfterFailedEnqueue(t *testing.T, adopted, lostResponse bool) {
	ctx := t.Context()
	d, task, control, runtimeFence := validationRecoveryFixture(t, false)
	d.Epochs = readyAgentRuntimeTestEpochManager(2)
	if adopted {
		runtimeFence.ControllerEpoch = 2
	}
	status := task.Status.DeepCopy()
	task.Spec.SessionRef = nil
	if err := d.Client.Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	task.Status = *status
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateSettling
	task.Status.Execution.Outcome = ""
	task.Status.Execution.ControllerEpoch = 1
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateValidating}
	control.attempt.SessionUID = ""
	control.attempt.SessionLeaseGeneration = 0
	control.attempt.ExecutionState = store.PromptExecutionSettling
	control.attempt.DeliveryState = store.PromptDeliveryValidating
	control.attempt.Version = 1
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control.fence = fence
	controls := &settlingValidationStore{
		validationRecoveryStore: control, projections: map[string]*store.OutboxProjection{},
		enqueueFailures: 1, lostEnqueueResponse: lostResponse,
	}
	d.Store = controls
	supervisor := &residentValidatingSupervisor{t: t, fence: runtimeFence, control: control}
	server := httptest.NewServer(supervisor)
	defer server.Close()
	pool := validationRecoveryPoolForServer(t, d, task, server.URL)
	if adopted {
		pool.Status.ActiveInstance.ControllerEpoch = 2
		if err := d.Client.Status().Update(ctx, pool); err != nil {
			t.Fatal(err)
		}
		secret := &corev1.Secret{}
		if err := d.Client.Get(ctx, types.NamespacedName{Namespace: "orka-runtimes", Name: "recovery-auth-e1"}, secret); err != nil {
			t.Fatal(err)
		}
		adoptedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: secret.Namespace, Name: "recovery-auth-e2", UID: "recovery-auth-e2-uid", Labels: secret.Labels,
		}, Data: secret.Data}
		if err := d.Client.Create(ctx, adoptedSecret); err != nil {
			t.Fatal(err)
		}
	}
	frozen := *task.Status.Execution

	read := func() *corev1alpha1.Task {
		t.Helper()
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, latest); err != nil {
			t.Fatal(err)
		}
		return latest
	}
	if err := d.recoverStaleTask(ctx, read(), fence); !errors.Is(err, errInjectedProjectionEnqueue) {
		t.Fatalf("recovery error = %v, want the injected enqueue failure", err)
	}
	if control.attempt.ExecutionState != store.PromptExecutionOutcomeUnknown || control.attempt.DeliveryState != store.PromptDeliveryConflict {
		t.Fatalf("attempt = %s/%s, want the recorded unknown outcome and validation conflict",
			control.attempt.ExecutionState, control.attempt.DeliveryState)
	}
	if latest := read(); latest.Status.Execution.State != corev1alpha1.TaskExecutionStateSettling || !recoveredValidationConflict(latest.Status.Delivery) {
		t.Fatalf("after the failed enqueue the Task records execution %s and delivery %#v",
			latest.Status.Execution.State, latest.Status.Delivery)
	}
	if lostResponse {
		// The stored projection is the Task's record; until the projector
		// delivers it, cleanup waits and sends the runtime nothing.
		for range 2 {
			if err := d.recoverStaleTask(ctx, read(), fence); err != nil {
				t.Fatalf("recovery before the projection is delivered: %v", err)
			}
		}
		supervisor.mu.Lock()
		calls := append([]string(nil), supervisor.calls...)
		supervisor.mu.Unlock()
		if len(calls) != 0 || read().Status.Execution.State != corev1alpha1.TaskExecutionStateSettling {
			t.Fatalf("recovery acted before the projection was delivered: calls=%v", calls)
		}
		if len(controls.projections) != 1 {
			t.Fatalf("stored projections = %d, want 1", len(controls.projections))
		}
		projector := &ACPOutboxProjector{Client: d.Client, Epochs: d.Epochs}
		for _, projection := range controls.projections {
			if _, err := projector.deliver(ctx, *projection); err != nil {
				t.Fatal(err)
			}
		}
	}

	for range 4 {
		if err := d.recoverStaleTask(ctx, read(), fence); err != nil {
			t.Fatalf("recovery after the failed enqueue: %v", err)
		}
		if read().Status.Execution.ControllerEpoch == fence.Epoch && supervisor.deletedSession() {
			break
		}
	}
	if !supervisor.deletedSession() {
		t.Fatal("recovery did not delete the RuntimeSession still holding the validation")
	}
	latest := read()
	execution := latest.Status.Execution
	if execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown || execution.Outcome != corev1alpha1.TaskExecutionOutcomeOutcomeUnknown ||
		latest.Status.Phase != corev1alpha1.TaskPhaseFailed || !recoveredValidationConflict(latest.Status.Delivery) {
		t.Fatalf("Task status = phase %s execution %s/%s delivery %#v", latest.Status.Phase, execution.State, execution.Outcome, latest.Status.Delivery)
	}
	if execution.ControllerEpoch != fence.Epoch || !runtimeSessionCleanupCompleteForUID(latest, task.UID) {
		t.Fatalf("Task epoch = %d (want %d), cleanup recorded = %v",
			execution.ControllerEpoch, fence.Epoch, runtimeSessionCleanupCompleteForUID(latest, task.UID))
	}
	if execution.RuntimePoolUID != frozen.RuntimePoolUID || execution.RuntimeInstanceID != frozen.RuntimeInstanceID ||
		execution.RuntimeSessionUID != frozen.RuntimeSessionUID || execution.RuntimeSessionGeneration != frozen.RuntimeSessionGeneration ||
		execution.RuntimeSessionSupervisorBootID != frozen.RuntimeSessionSupervisorBootID || execution.RequestDigest != frozen.RequestDigest {
		t.Fatalf("recovery changed the frozen runtime identity: %#v, was %#v", execution, frozen)
	}
	exists, err := d.validateExistingStandaloneTaskProjection(ctx, latest, control.attempt)
	if err != nil || !exists {
		t.Fatalf("terminal projection exists=%v err=%v", exists, err)
	}
	if len(controls.projections) != 1 {
		t.Fatalf("stored projections = %d, want 1", len(controls.projections))
	}
	for _, projection := range controls.projections {
		var payload taskTerminalProjection
		if err := json.Unmarshal(projection.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		projected := payload.Execution
		if payload.Phase != corev1alpha1.TaskPhaseFailed || projected.State != corev1alpha1.TaskExecutionStateOutcomeUnknown ||
			!recoveredValidationConflict(payload.Delivery) {
			t.Fatalf("projection = phase %s execution %s delivery %#v", payload.Phase, projected.State, payload.Delivery)
		}
		if projected.RuntimePoolUID != frozen.RuntimePoolUID || projected.RuntimeInstanceID != frozen.RuntimeInstanceID ||
			projected.RuntimeSessionUID != frozen.RuntimeSessionUID || projected.RuntimeSessionGeneration != frozen.RuntimeSessionGeneration ||
			projected.RuntimeSessionSupervisorBootID != frozen.RuntimeSessionSupervisorBootID || projected.RequestDigest != frozen.RequestDigest ||
			projected.RuntimeSessionWorkspaceDigest != frozen.RuntimeSessionWorkspaceDigest {
			t.Fatalf("projection runtime identity = %#v, want %#v", projected, frozen)
		}
	}
}
