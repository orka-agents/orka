package controller

import (
	"context"
	"net/http"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2eventjournal "github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
