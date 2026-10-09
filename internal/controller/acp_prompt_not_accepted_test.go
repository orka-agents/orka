package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2eventjournal "github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPromptNotAcceptedFailsOnlyProvenIdleResumeRejection(t *testing.T) {
	for _, test := range []struct {
		name        string
		accepted    bool
		contextErr  error
		change      func(*harnessv2.ClientError)
		wantUnknown bool
	}{
		{name: "idle resume rejected"},
		{name: "deadline after rejection", contextErr: context.DeadlineExceeded},
		{name: "already accepted", accepted: true, wantUnknown: true},
		{name: "generic poisoned session", wantUnknown: true, change: func(err *harnessv2.ClientError) { err.Code = harnessv2.ErrorCodeSessionPoisoned }},
		{name: "unvalidated envelope", wantUnknown: true, change: func(err *harnessv2.ClientError) { err.Kind = harnessv2.ClientErrorProtocol }},
		{name: "other operation", wantUnknown: true, change: func(err *harnessv2.ClientError) { err.Operation = "cancel_prompt" }},
		{name: "wrong status", wantUnknown: true, change: func(err *harnessv2.ClientError) { err.StatusCode = http.StatusInternalServerError }},
		{name: "retryable rejection", wantUnknown: true, change: func(err *harnessv2.ClientError) { err.Retryable = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			initialState := store.PromptExecutionPlanned
			if test.accepted {
				initialState = store.PromptExecutionRunning
			}
			fixture := newACPRecoveryFixture(t, initialState)
			defer fixture.close(t)
			task := configureRecoveryJournalIdentity(t, fixture)
			if !test.accepted {
				if err := fixture.dispatcher.transitionAttempt(fixture.ctx, fixture.attemptID, fixture.fence,
					store.PromptExecutionPlanned, store.PromptExecutionSubmitting, "submit", nil); err != nil {
					t.Fatal(err)
				}
			}
			journalState, err := (v2eventjournal.Journal{
				EventStore: fixture.controlStore, MapContext: mappedPromptRecoveryContext(task),
			}).Open(fixture.ctx)
			if err != nil {
				t.Fatal(err)
			}
			writeEvidence := harnessv2.RequestWriteEvidence{State: harnessv2.RequestWriteComplete, RequestBodyBytesRead: 100, WroteRequest: true}
			rejection := &harnessv2.ClientError{
				Operation: "start_prompt", Kind: harnessv2.ClientErrorHTTP, StatusCode: http.StatusConflict,
				Code: harnessv2.ErrorCodePromptNotAccepted, WriteEvidence: writeEvidence,
			}
			if test.change != nil {
				test.change(rejection)
			}
			if retryableUnsentPromptCanRequeue(test.accepted, harnessv2.PromptStreamSummary{WriteEvidence: writeEvidence}, test.contextErr, rejection) {
				t.Fatal("rejection authorized replay of a written HTTP request")
			}
			// A nil runtime client also proves the definitive rejection does not
			// issue a cancellation when the caller deadline races the response.
			if err := fixture.dispatcher.handlePromptStreamError(
				fixture.ctx, nil, nil, "runtime-session", task, fixture.attemptID, fixture.fence,
				harnessv2.Fence{}, journalState, test.accepted, writeEvidence, test.contextErr, rejection,
			); err != nil {
				t.Fatal(err)
			}
			completed := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, client.ObjectKeyFromObject(task), completed); err != nil {
				t.Fatal(err)
			}
			wantState, wantOutcome, wantAttempt := corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed, store.PromptExecutionFailed
			if test.wantUnknown {
				wantState, wantOutcome, wantAttempt = corev1alpha1.TaskExecutionStateOutcomeUnknown, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, store.PromptExecutionOutcomeUnknown
			}
			if completed.Status.Execution.State != wantState || completed.Status.Execution.Outcome != wantOutcome || completed.Status.Execution.Reason != "RuntimeLost" {
				t.Fatalf("execution = %#v, want %s/%s/RuntimeLost", completed.Status.Execution, wantState, wantOutcome)
			}
			attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != wantAttempt || attempt.TerminalReason != "RuntimeLost" || attempt.OutcomeMarker != completed.Status.Execution.Message {
				t.Fatalf("durable attempt classification differs from Task: %#v", attempt)
			}
		})
	}
}

func TestPromptNotAcceptedDeferredSessionFinalizationAllowsContinuation(t *testing.T) {
	ctx := t.Context()
	controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "idle-resume-session.db"))
	defer closeStore()
	continuity := newACPSessionTestContinuity(t, controlStore, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "idle-resume-session")
	const taskUID = "task-idle-resume-session"
	const promptID = "prompt-idle-resume-session"
	turn, attempt := openACPSessionTurnForTest(t, continuity, controlStore, fence, control, taskUID, promptID, "continue the session")
	for _, next := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned, store.PromptExecutionSubmitting,
	} {
		operation := "idle-resume-" + string(next)
		var err error
		attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: next, OperationID: operation, OperationDigest: acpSessionTestDigest(operation), UpdatedAt: attempt.UpdatedAt.Add(time.Second),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "idle-resume-task", UID: types.UID(taskUID)},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "continue the session", SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: promptID,
				RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: turn.Lease.Key.LeaseGeneration, RequestDigest: attempt.RequestDigest,
			},
		},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	dispatcher := &ACPDispatcher{Client: kubeClient, Store: controlStore, Sessions: continuity}
	binding := ACPRuntimeSessionBinding{SessionUID: control.SessionUID, Generation: uint64(turn.Lease.Key.LeaseGeneration)}
	dispatcher.setRuntimeSessionBinding(binding)
	session := &acpTaskSession{Turn: turn, Binding: binding}
	// Match executeReservedTask's existing deferred terminal reconciliation.
	// The stream error branch need not duplicate Session finalization.
	run := func() (retErr error) {
		defer func() {
			if err := dispatcher.reconcileUnfinalizedTaskSession(ctx, task, fence, session, retErr); retErr == nil {
				retErr = err
			}
		}()
		return dispatcher.handlePromptStreamError(ctx, nil, nil, "runtime-session", task, attempt.ID, fence, harnessv2.Fence{}, nil,
			false, harnessv2.RequestWriteEvidence{State: harnessv2.RequestWriteComplete}, nil,
			&harnessv2.ClientError{Operation: "start_prompt", Kind: harnessv2.ClientErrorHTTP, StatusCode: http.StatusConflict, Code: harnessv2.ErrorCodePromptNotAccepted},
		)
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	finalized, err := controlStore.GetSessionTurn(ctx, turn.Turn.ID)
	if err != nil || finalized.State != store.SessionTurnFinalized {
		t.Fatalf("Session turn = %#v, error = %v", finalized, err)
	}
	projection, err := controlStore.GetOutboxProjection(ctx, finalized.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Execution.State != corev1alpha1.TaskExecutionStateFailed || payload.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeFailed || payload.Execution.Reason != "RuntimeLost" {
		t.Fatalf("Session finalization lost the definitive failure: %#v", payload.Execution)
	}
	if dispatcher.currentRuntimeSessionBinding(control.SessionUID) != nil {
		t.Fatal("retired runtime binding survived deferred Session finalization")
	}
	current, err := controlStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil || current.Lease != nil {
		t.Fatalf("Session lease was not released: control=%#v error=%v", current, err)
	}
	if _, err := continuity.AcquireMutationLease(ctx, ACPAcquireSessionLeaseRequest{
		Session: *current, Fence: fence, TaskUID: "next-task", Attempt: 1, PromptID: "next-prompt",
		PromptRequestDigest: acpSessionTestDigest("next-prompt"), AcquiredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("next continuation blocked after resume rejection: %v", err)
	}
}
