/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPDispatcherSettlesCancelRequestedTaskBeforeRuntimePoolAdmission(t *testing.T) {
	runACPDispatcherPreAdmissionSettlementTest(t, func(task *corev1alpha1.Task) {
		if task.Annotations == nil {
			task.Annotations = map[string]string{}
		}
		task.Annotations[labels.AnnotationCancelRequested] = "cancelled by request"
	}, corev1alpha1.TaskExecutionReason("Cancelled"), "task cancelled by request before runtime admission", false)
}

func TestPromptCancelRequestedSendsUserRequestedAndEndsCancelled(t *testing.T) {
	fixture := newPromptStreamLifecycleFixture(t, harnessv2.PromptSettlement{
		TerminalEvent: harnessv2.EventCancelled, Outcome: harnessv2.PromptOutcomeCancelled,
		StopReason: harnessv2.ACPStopReasonCancelled, SettledAt: time.Now().UTC(),
	})
	if err := fixture.dispatcher.handlePromptStreamError(
		fixture.ctx, nil, fixture.runtimeClient, "runtime-session-1", fixture.task.DeepCopy(), fixture.attemptID,
		fixture.fence, fixture.runtimeFence, fixture.journalState, true, harnessv2.RequestWriteEvidence{},
		errACPTaskCancelRequested, context.Canceled, nil,
	); err != nil {
		t.Fatal(err)
	}
	if request := <-fixture.cancelRequests; request.Reason != harnessv2.CancelReasonUserRequested {
		t.Fatalf("cancel reason = %q, want %q", request.Reason, harnessv2.CancelReasonUserRequested)
	}
	completed := &corev1alpha1.Task{}
	if err := fixture.dispatcher.Client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.task), completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status.Execution == nil || completed.Status.Execution.State != corev1alpha1.TaskExecutionStateCancelled ||
		completed.Status.Execution.Reason != "Cancelled" {
		t.Fatalf("requested cancellation status = %#v", completed.Status.Execution)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionCancelled || attempt.OutcomeMarker != acpRequestedCancellationMessage {
		t.Fatalf("attempt = %s %q, want Cancelled %q", attempt.ExecutionState, attempt.OutcomeMarker, acpRequestedCancellationMessage)
	}
	fixture.assertTerminalLifecycle(t, harnessv2.EventCancelled, true)
}

func TestWatchTaskCancellationNamesARequestedCancel(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.Task)
		want   error
	}{
		{name: "requested", mutate: func(task *corev1alpha1.Task) {
			task.Annotations = map[string]string{labels.AnnotationCancelRequested: "cancelled by request"}
		}, want: errACPTaskCancelRequested},
		{name: "deleting", mutate: func(task *corev1alpha1.Task) {
			now := metav1.Now()
			task.DeletionTimestamp = &now
			task.Finalizers = []string{labels.TaskFinalizer}
		}, want: context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "watched", UID: types.UID("watched-uid")}}
			test.mutate(task)
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
			dispatcher := &ACPDispatcher{APIReader: reader}

			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan struct{})
			go func() {
				dispatcher.watchTaskCancellation(ctx, cancel, client.ObjectKeyFromObject(task))
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the watcher did not stop the runtime")
			}
			if cause := context.Cause(ctx); !errors.Is(cause, test.want) {
				t.Fatalf("cause = %v, want %v", cause, test.want)
			}
		})
	}
}
