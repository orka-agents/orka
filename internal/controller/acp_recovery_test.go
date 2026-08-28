package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2eventjournal "github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	kubestore "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	acpTestModel                      = "gpt-test"
	acpRecoveryOutcomeUnknownMessage  = "outcome unknown"
	acpRecoveryStatusSubresource      = "status"
	acpRecoveryRuntimeInstanceID      = "runtime-instance"
	acpRecoveryRuntimeSessionUID      = "runtime-session"
	acpRecoveryToolTitle              = "Inspect repository"
	acpRecoveryToolKind               = "read"
	acpRecoveryPromptCancelledMessage = "prompt cancelled"
	acpRecoveryPromptFailedMessage    = "prompt failed"
)

type missingRecoveryPromptAttemptStore struct {
	store.DurableControlStore
	calls atomic.Int32
}

func (s *missingRecoveryPromptAttemptStore) GetPromptAttempt(context.Context, string) (*store.PromptAttempt, error) {
	s.calls.Add(1)
	return nil, store.ErrNotFound
}

type failOnceRecoveryProjectionStore struct {
	store.DurableControlStore
	err   error
	calls atomic.Int32
}

func (s *failOnceRecoveryProjectionStore) EnqueueOutboxProjection(
	ctx context.Context,
	projection *store.OutboxProjection,
	fence store.ControllerEpochFence,
) (*store.OutboxProjection, error) {
	if s.calls.Add(1) == 1 {
		return nil, s.err
	}
	return s.DurableControlStore.EnqueueOutboxProjection(ctx, projection, fence)
}

func TestACPDispatcherRecoverySkipsTaskDeletedAfterCachedList(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	missingStore := &missingRecoveryPromptAttemptStore{DurableControlStore: fixture.controlStore}
	fixture.dispatcher.Store = missingStore
	getCalls := 0
	watchClient, ok := fixture.kubeClient.(client.WithWatch)
	if !ok {
		t.Fatal("recovery fake client does not implement client.WithWatch")
	}
	fixture.dispatcher.APIReader = interceptor.NewClient(watchClient, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isTask := obj.(*corev1alpha1.Task); !isTask {
				return c.Get(ctx, key, obj, opts...)
			}
			getCalls++
			if getCalls == 1 {
				return c.Get(ctx, key, obj, opts...)
			}
			return apierrors.NewNotFound(
				schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"},
				key.Name,
			)
		},
	})

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatalf("recoverStaleAttempts() error = %v, want deleted Task race ignored", err)
	}
	if getCalls != 2 {
		t.Fatalf("Task reads = %d, want initial refresh plus post-not-found recheck", getCalls)
	}
	if missingStore.calls.Load() != 1 {
		t.Fatalf("GetPromptAttempt calls = %d, want 1", missingStore.calls.Load())
	}
}

func TestACPDispatcherRecoveryFailsClosedWhenAttemptMissingForLiveTask(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	missingStore := &missingRecoveryPromptAttemptStore{DurableControlStore: fixture.controlStore}
	fixture.dispatcher.Store = missingStore

	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("recoverStaleAttempts() error = %v, want ErrNotFound for a still-live Task", err)
	}
	if missingStore.calls.Load() != 1 {
		t.Fatalf("GetPromptAttempt calls = %d, want 1", missingStore.calls.Load())
	}
}

func TestACPDispatcherRecoversPreSubmissionAttemptUnderNewEpoch(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionReserved || attempt.ControllerEpoch != fixture.fence.Epoch || attempt.RuntimeInstanceID != "" {
		t.Fatalf("recovered attempt = %#v", attempt)
	}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Execution == nil || task.Status.Execution.State != corev1alpha1.TaskExecutionStateReserved || task.Status.Execution.ControllerEpoch != fixture.fence.Epoch {
		t.Fatalf("recovered task = %#v", task.Status.Execution)
	}
}

func TestACPDispatcherMakesAcceptedOldEpochAttemptOutcomeUnknown(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionAccepted)
	defer fixture.close(t)

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown || attempt.OutcomeMarker == "" {
		t.Fatalf("recovered attempt = %#v", attempt)
	}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseFailed || task.Status.Execution == nil || task.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeOutcomeUnknown {
		t.Fatalf("recovered task status = %#v", task.Status)
	}
}

func TestACPDispatcherClosesRecoveredPromptAndOpenToolsAsOutcomeUnknown(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionAccepted)
	defer fixture.close(t)

	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "recovery-session"}
	if err := fixture.kubeClient.Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	task = configureRecoveryJournalIdentity(t, fixture)
	appendRecoveryOpenPrompt(t, fixture, task)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	listed, err := fixture.controlStore.ListExecutionEvents(fixture.ctx, store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{
		executionevents.ExecutionEventTypeModelRequestStarted,
		executionevents.ExecutionEventTypeToolCallStarted,
		executionevents.ExecutionEventTypeToolCallFailed,
		executionevents.ExecutionEventTypeModelRequestFailed,
	}
	if len(listed) != len(wantTypes) {
		t.Fatalf("recovered lifecycle events = %#v", listed)
	}
	for index, wantType := range wantTypes {
		if listed[index].Type != wantType {
			t.Fatalf("recovered lifecycle event %d type = %q, want %q", index, listed[index].Type, wantType)
		}
		if listed[index].SessionName != task.Spec.SessionRef.Name {
			t.Fatalf("recovered lifecycle event %d session = %q, want %q", index, listed[index].SessionName, task.Spec.SessionRef.Name)
		}
	}
	if listed[1].ToolCallID == "" || listed[2].ToolCallID != listed[1].ToolCallID ||
		listed[2].Summary != "Tool call outcome unknown" {
		t.Fatalf("recovered tool lifecycle = start %#v terminal %#v", listed[1], listed[2])
	}
	var toolClosure struct {
		ControllerSynthesized bool                `json:"controllerSynthesized"`
		Outcome               harnessv2.EventType `json:"outcome"`
	}
	if err := json.Unmarshal(listed[2].Content, &toolClosure); err != nil {
		t.Fatal(err)
	}
	if !toolClosure.ControllerSynthesized || toolClosure.Outcome != harnessv2.EventOutcomeUnknown {
		t.Fatalf("recovered tool closure content = %#v", toolClosure)
	}
	var promptClosure struct {
		ControllerSynthesized bool                `json:"controllerSynthesized"`
		TerminalEvent         harnessv2.EventType `json:"terminalEvent"`
	}
	if err := json.Unmarshal(listed[3].Content, &promptClosure); err != nil {
		t.Fatal(err)
	}
	if !promptClosure.ControllerSynthesized || promptClosure.TerminalEvent != harnessv2.EventOutcomeUnknown {
		t.Fatalf("recovered prompt closure content = %#v", promptClosure)
	}
	identity, ok := mappedPromptRecoveryIdentity(task)
	if !ok {
		t.Fatal("recovery identity missing")
	}
	if err := fixture.dispatcher.closeRecoveredPromptJournal(
		fixture.ctx, task, time.Now().UTC(), "duplicate recovery",
	); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := fixture.controlStore.ListExecutionEvents(fixture.ctx, store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 10,
	}); err != nil || len(duplicate) != len(wantTypes) {
		t.Fatalf("duplicate recovery events = %#v err=%v identity=%#v", duplicate, err, identity)
	}
}

func TestACPDispatcherRecoversMatchedJournaledNonSuccessTerminal(t *testing.T) {
	tests := []struct {
		name        string
		terminal    harnessv2.EventType
		wantState   store.PromptExecutionState
		wantTask    corev1alpha1.TaskExecutionState
		wantOutcome corev1alpha1.TaskExecutionOutcome
		wantPhase   corev1alpha1.TaskPhase
	}{
		{
			name: acpPromptOutcomeFailed, terminal: harnessv2.EventFailed, wantState: store.PromptExecutionFailed,
			wantTask: corev1alpha1.TaskExecutionStateFailed, wantOutcome: corev1alpha1.TaskExecutionOutcomeFailed,
			wantPhase: corev1alpha1.TaskPhaseFailed,
		},
		{
			name: acpCancelledOperation, terminal: harnessv2.EventCancelled, wantState: store.PromptExecutionCancelled,
			wantTask: corev1alpha1.TaskExecutionStateCancelled, wantOutcome: corev1alpha1.TaskExecutionOutcomeCancelled,
			wantPhase: corev1alpha1.TaskPhaseCancelled,
		},
		{
			name: acpRecoveryOutcomeUnknownMessage, terminal: harnessv2.EventOutcomeUnknown, wantState: store.PromptExecutionOutcomeUnknown,
			wantTask: corev1alpha1.TaskExecutionStateOutcomeUnknown, wantOutcome: corev1alpha1.TaskExecutionOutcomeOutcomeUnknown,
			wantPhase: corev1alpha1.TaskPhaseFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
			defer fixture.close(t)

			task := configureRecoveryJournalIdentity(t, fixture)
			appendRecoveryPromptTerminal(t, fixture, task, test.terminal, false)
			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != test.wantState {
				t.Fatalf("recovered attempt state = %s, want %s", attempt.ExecutionState, test.wantState)
			}
			updated := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.Phase != test.wantPhase || updated.Status.Execution == nil ||
				updated.Status.Execution.State != test.wantTask || updated.Status.Execution.Outcome != test.wantOutcome {
				t.Fatalf("recovered Task status = %#v", updated.Status)
			}
		})
	}
}

func TestACPDispatcherRecoversTimeoutReasonFromProvenCancellationSettlement(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)

	task := configureRecoveryJournalIdentity(t, fixture)
	appendRecoveryPromptSettlement(t, fixture, task, harnessv2.CancelReasonTaskTimeout)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionCancelled {
		t.Fatalf("recovered attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionCancelled)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseCancelled || updated.Status.Execution == nil ||
		updated.Status.Execution.State != corev1alpha1.TaskExecutionStateCancelled ||
		updated.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeCancelled ||
		updated.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(acpTaskTimeoutReason) ||
		updated.Status.Execution.Message != acpTaskTimeoutCancellationSettledMessage {
		t.Fatalf("recovered timeout cancellation status = %#v", updated.Status)
	}
}

func TestACPDispatcherRecoversTimeoutClassificationAfterAttemptTransition(t *testing.T) {
	for _, test := range []struct {
		name      string
		operation string
		message   string
	}{
		{
			name: "before acceptance", operation: "timeout-before-acceptance",
			message: "task deadline exceeded before prompt acceptance",
		},
		{
			name: "after acceptance", operation: "timeout-cancelled",
			message: acpTaskTimeoutCancellationSettledMessage,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
			defer fixture.close(t)

			if err := fixture.dispatcher.transitionAttemptToCancelled(
				fixture.ctx, fixture.attemptID, fixture.fence, test.operation, acpTaskTimeoutReason, test.message,
			); err != nil {
				t.Fatal(err)
			}
			attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != store.PromptExecutionCancelled ||
				attempt.TerminalReason != string(acpTaskTimeoutReason) || attempt.OutcomeMarker != test.message {
				t.Fatalf("terminal timeout attempt = %#v", attempt)
			}

			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			updated := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.Phase != corev1alpha1.TaskPhaseCancelled || updated.Status.Execution == nil ||
				updated.Status.Execution.State != corev1alpha1.TaskExecutionStateCancelled ||
				updated.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeCancelled ||
				updated.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(acpTaskTimeoutReason) ||
				updated.Status.Execution.Message != test.message {
				t.Fatalf("recovered timeout cancellation status = %#v", updated.Status)
			}
		})
	}
}

func TestACPDispatcherPatchRecoveredTerminalExecutionPreservesTerminalClassification(t *testing.T) {
	tests := []struct {
		name           string
		attemptState   store.PromptExecutionState
		attemptReason  string
		attemptMessage string
		taskState      corev1alpha1.TaskExecutionState
		taskOutcome    corev1alpha1.TaskExecutionOutcome
		latestReason   corev1alpha1.TaskExecutionReason
		latestMessage  string
		wantReason     corev1alpha1.TaskExecutionReason
		wantMessage    string
	}{
		{
			name: "durable runtime lost wins over restart marker", attemptState: store.PromptExecutionOutcomeUnknown,
			attemptReason: string(corev1alpha1.TaskExecutionReasonRuntimeLost), attemptMessage: "journaled prompt outcome is unknown",
			taskState: corev1alpha1.TaskExecutionStateOutcomeUnknown, taskOutcome: corev1alpha1.TaskExecutionOutcomeOutcomeUnknown,
			latestReason: acpControllerRestartRecoveredReason, latestMessage: "terminal ACP attempt recovered under the new controller epoch",
			wantReason: corev1alpha1.TaskExecutionReasonRuntimeLost, wantMessage: "journaled prompt outcome is unknown",
		},
		{
			name: "latest timeout wins over stale caller", attemptState: store.PromptExecutionCancelled,
			taskState: corev1alpha1.TaskExecutionStateCancelled, taskOutcome: corev1alpha1.TaskExecutionOutcomeCancelled,
			latestReason: acpTaskTimeoutReason, latestMessage: acpTaskTimeoutCancellationSettledMessage,
			wantReason: acpTaskTimeoutReason, wantMessage: acpTaskTimeoutCancellationSettledMessage,
		},
		{
			name: "legacy cancellation uses terminal default", attemptState: store.PromptExecutionCancelled,
			taskState: corev1alpha1.TaskExecutionStateCancelled, taskOutcome: corev1alpha1.TaskExecutionOutcomeCancelled,
			latestReason: acpControllerRestartRecoveredReason, latestMessage: "terminal ACP attempt recovered under the new controller epoch",
			wantReason: corev1alpha1.TaskExecutionReason(harnessV1ReasonCancelled), wantMessage: acpRecoveryPromptCancelledMessage,
		},
		{
			name: "legacy failure uses terminal default", attemptState: store.PromptExecutionFailed,
			taskState: corev1alpha1.TaskExecutionStateFailed, taskOutcome: corev1alpha1.TaskExecutionOutcomeFailed,
			wantReason: corev1alpha1.TaskExecutionReason(harnessV1ReasonFailed), wantMessage: acpRecoveryPromptFailedMessage,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			latest := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "terminal-recovery"},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
					State: test.taskState, Outcome: test.taskOutcome, ControllerEpoch: 1,
					Reason: test.latestReason, Message: test.latestMessage,
				}},
			}
			stale := latest.DeepCopy()
			stale.Status.Execution.Reason = corev1alpha1.TaskExecutionReason(harnessV1ReasonCancelled)
			stale.Status.Execution.Message = "stale caller classification"
			kubeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.Task{}).
				WithObjects(latest).
				Build()
			dispatcher := &ACPDispatcher{Client: kubeClient}
			attempt := &store.PromptAttempt{
				ExecutionState: test.attemptState, DeliveryState: store.PromptDeliveryNotRequested,
				TerminalReason: test.attemptReason, OutcomeMarker: test.attemptMessage,
			}
			if err := dispatcher.patchRecoveredTerminalExecution(context.Background(), stale, attempt, 2); err != nil {
				t.Fatal(err)
			}
			updated := &corev1alpha1.Task{}
			if err := kubeClient.Get(context.Background(), clientObjectKey(latest), updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.Execution == nil || updated.Status.Execution.ControllerEpoch != 2 ||
				updated.Status.Execution.Reason != test.wantReason || updated.Status.Execution.Message != test.wantMessage {
				t.Fatalf("recovered terminal execution = %#v, want epoch 2 reason %q message %q", updated.Status.Execution, test.wantReason, test.wantMessage)
			}
		})
	}
}

func TestACPDispatcherRecoversCompletedJournalTerminalOnlyWithExactResult(t *testing.T) {
	tests := []struct {
		name        string
		storeResult bool
	}{
		{name: "exact result persisted", storeResult: true},
		{name: "result missing", storeResult: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, store.PromptExecutionSettling)
			defer fixture.close(t)

			task := configureRecoveryJournalIdentity(t, fixture)
			const result = "exact recovered result"
			if test.storeResult {
				if err := fixture.controlStore.SaveResult(fixture.ctx, task.Namespace, task.Name, []byte(result)); err != nil {
					t.Fatal(err)
				}
			}
			appendRecoveryPromptTerminal(t, fixture, task, harnessv2.EventCompleted, false)
			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			updated := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
				t.Fatal(err)
			}
			if !test.storeResult {
				if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown || updated.Status.Execution == nil ||
					updated.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeOutcomeUnknown || updated.Status.ResultRef != nil {
					t.Fatalf("missing-result recovery = attempt %#v Task %#v", attempt, updated.Status)
				}
				return
			}
			if attempt.ExecutionState != store.PromptExecutionSucceeded || attempt.DeliveryState != store.PromptDeliveryConflict {
				t.Fatalf("completed recovery attempt = %#v", attempt)
			}
			if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || updated.Status.Execution == nil ||
				updated.Status.Execution.State != corev1alpha1.TaskExecutionStateSucceeded ||
				updated.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded ||
				updated.Status.Delivery == nil || updated.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeDeliveryConflict ||
				updated.Status.ResultRef == nil || !updated.Status.ResultRef.Available {
				t.Fatalf("completed recovery Task status = %#v", updated.Status)
			}
			persisted, err := fixture.controlStore.GetResult(fixture.ctx, task.Namespace, task.Name)
			if err != nil || string(persisted) != result {
				t.Fatalf("recovered result = %q, err=%v", persisted, err)
			}
		})
	}
}

func TestACPDispatcherRetriesResultReferenceForSucceededAttempt(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionSucceeded)
	defer fixture.close(t)

	if err := fixture.controlStore.SaveResult(fixture.ctx, "default", "task", []byte("recovered result")); err != nil {
		t.Fatal(err)
	}
	rawClient, ok := fixture.kubeClient.(client.WithWatch)
	if !ok {
		t.Fatal("recovery fake client does not implement client.WithWatch")
	}
	injectedErr := errors.New("injected result-reference patch failure")
	var resultReferencePatches atomic.Int32
	intercepted := interceptor.NewClient(rawClient, interceptor.Funcs{
		SubResourcePatch: func(
			ctx context.Context,
			c client.Client,
			subresource string,
			obj client.Object,
			patch client.Patch,
			opts ...client.SubResourcePatchOption,
		) error {
			task, isTask := obj.(*corev1alpha1.Task)
			if subresource == acpRecoveryStatusSubresource && isTask && task.Status.ResultRef != nil && task.Status.ResultRef.Available {
				if resultReferencePatches.Add(1) == 1 {
					return injectedErr
				}
			}
			return c.SubResource(subresource).Patch(ctx, obj, patch, opts...)
		},
	})
	fixture.dispatcher.Client = intercepted

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); !errors.Is(err, injectedErr) {
		t.Fatalf("first recovery error = %v, want injected result-reference failure", err)
	}
	committed, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if committed.ExecutionState != store.PromptExecutionSucceeded {
		t.Fatalf("attempt state after failed result-reference patch = %s, want %s", committed.ExecutionState, store.PromptExecutionSucceeded)
	}
	current := &corev1alpha1.Task{}
	if err := rawClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.ResultRef != nil {
		t.Fatalf("result reference unexpectedly persisted after injected failure: %#v", current.Status.ResultRef)
	}

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if err := rawClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, current); err != nil {
		t.Fatal(err)
	}
	if current.Status.ResultRef == nil || !current.Status.ResultRef.Available {
		t.Fatalf("recovered result reference = %#v, want available", current.Status.ResultRef)
	}
	if resultReferencePatches.Load() < 2 {
		t.Fatalf("result-reference patch attempts = %d, want at least 2", resultReferencePatches.Load())
	}
}

func TestACPDispatcherRecoversSettlingResultReceipt(t *testing.T) {
	tests := []struct {
		name            string
		storedResult    string
		hasStoredResult bool
	}{
		{name: "missing task result"},
		{name: "stale task result", storedResult: "stale result", hasStoredResult: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
			defer fixture.close(t)

			task := configureRecoveryJournalIdentity(t, fixture)
			appendRecoveryOpenPrompt(t, fixture, task)
			const receiptResult = "exact recovered result"
			if err := fixture.dispatcher.transitionAttemptToSettlingWithResult(
				fixture.ctx, task, fixture.attemptID, fixture.fence, []byte(receiptResult),
			); err != nil {
				t.Fatal(err)
			}
			task.Status.Execution.State = corev1alpha1.TaskExecutionStateSettling
			if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
				t.Fatal(err)
			}
			if test.hasStoredResult {
				if err := fixture.controlStore.SaveResult(
					fixture.ctx, task.Namespace, task.Name, []byte(test.storedResult),
				); err != nil {
					t.Fatal(err)
				}
			}

			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != store.PromptExecutionSucceeded {
				t.Fatalf("recovered attempt state = %s, want %s", attempt.ExecutionState, store.PromptExecutionSucceeded)
			}
			updated := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.Execution == nil || updated.Status.Execution.State != corev1alpha1.TaskExecutionStateSucceeded ||
				updated.Status.ResultRef == nil || !updated.Status.ResultRef.Available {
				t.Fatalf("recovered result status = %#v", updated.Status)
			}
			persistedResult, err := fixture.controlStore.GetResult(fixture.ctx, task.Namespace, task.Name)
			if err != nil || string(persistedResult) != receiptResult {
				t.Fatalf("recovered task result = %q, err=%v", persistedResult, err)
			}
			identity, ok := mappedPromptRecoveryIdentity(task)
			if !ok {
				t.Fatal("recovery identity is incomplete")
			}
			evidence, err := (v2eventjournal.Journal{
				EventStore: fixture.controlStore,
				MapContext: v2eventjournal.MapContext{
					Namespace: task.Namespace, TaskName: task.Name, StreamID: task.Name,
				},
				RecoveryIdentity: identity,
			}).FindPromptTerminal(fixture.ctx)
			if err != nil || evidence == nil || evidence.TerminalEvent != harnessv2.EventCompleted {
				t.Fatalf("recovered terminal evidence = %#v, err=%v", evidence, err)
			}
			listed, err := fixture.controlStore.ListExecutionEvents(fixture.ctx, store.ExecutionEventFilter{
				Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			wantTypes := []string{
				executionevents.ExecutionEventTypeModelRequestStarted,
				executionevents.ExecutionEventTypeToolCallStarted,
				executionevents.ExecutionEventTypeToolCallFailed,
				executionevents.ExecutionEventTypeModelRequestCompleted,
			}
			if len(listed) != len(wantTypes) {
				t.Fatalf("recovered settling lifecycle events = %#v", listed)
			}
			for index, wantType := range wantTypes {
				if listed[index].Type != wantType {
					t.Fatalf("recovered settling lifecycle event %d type = %q, want %q", index, listed[index].Type, wantType)
				}
			}
		})
	}
}

func TestACPDispatcherRestoredTaskPreservesJournaledCompletion(t *testing.T) {
	for _, target := range []store.PromptExecutionState{store.PromptExecutionRunning, store.PromptExecutionSettling} {
		t.Run(string(target), func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, target)
			defer fixture.close(t)

			task := configureRecoveryJournalIdentity(t, fixture)
			appendRecoveryPromptTerminal(t, fixture, task, harnessv2.EventCompleted, false)
			if err := fixture.controlStore.SaveResult(
				fixture.ctx, task.Namespace, task.Name, []byte("exact recovered result"),
			); err != nil {
				t.Fatal(err)
			}
			sourceUID, restoredUID := restoreACPRecoveryFixtureTask(t, fixture)
			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatal(err)
			}

			attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.Key.TaskUID != string(sourceUID) || attempt.ExecutionState != store.PromptExecutionSucceeded {
				t.Fatalf("reconciled source attempt = %#v", attempt)
			}
			restored := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(
				fixture.ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, restored,
			); err != nil {
				t.Fatal(err)
			}
			if restored.UID != restoredUID || restored.Status.Phase != corev1alpha1.TaskPhaseSucceeded ||
				restored.Status.Execution == nil || restored.Status.Execution.State != corev1alpha1.TaskExecutionStateSucceeded ||
				restored.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded ||
				restored.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason) ||
				restored.Status.ResultRef == nil || !restored.Status.ResultRef.Available {
				t.Fatalf("restored completed task status = %#v", restored.Status)
			}
			projectionID := standaloneTaskTerminalProjectionIDForUID(task.Namespace, sourceUID, 1)
			projection, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, projectionID)
			if err != nil {
				t.Fatal(err)
			}
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.TaskUID != string(sourceUID) || payload.Phase != corev1alpha1.TaskPhaseSucceeded ||
				payload.Execution.State != corev1alpha1.TaskExecutionStateSucceeded ||
				payload.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded {
				t.Fatalf("restored completion projection = %#v", payload)
			}
		})
	}
}

func TestACPDispatcherIgnoresJournaledTerminalForDifferentRuntimeIdentity(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)

	task := configureRecoveryJournalIdentity(t, fixture)
	appendRecoveryPromptTerminal(t, fixture, task, harnessv2.EventFailed, true)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown {
		t.Fatalf("mismatched terminal changed attempt to %s, want OutcomeUnknown", attempt.ExecutionState)
	}
}

func configureRecoveryJournalIdentity(t *testing.T, fixture *recoveryFixture) *corev1alpha1.Task {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	task.Status.Execution.RuntimeInstanceID = acpRecoveryRuntimeInstanceID
	task.Status.Execution.RuntimeSessionUID = acpRecoveryRuntimeSessionUID
	task.Status.Execution.RuntimeSessionGeneration = 1
	task.Status.Execution.RuntimeSessionSupervisorBootID = "supervisor-boot"
	if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	fixture.dispatcher.EventStore = fixture.controlStore
	return task
}

func appendRecoveryOpenPrompt(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task) {
	t.Helper()
	execution := task.Status.Execution
	now := time.Now().UTC()
	accepted := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion,
		Type:     harnessv2.EventAccepted,
		Identity: harnessv2.EventIdentity{
			RuntimeInstanceID:        harnessv2.RuntimeInstanceID(execution.RuntimeInstanceID),
			SupervisorBootID:         harnessv2.SupervisorBootID(execution.RuntimeSessionSupervisorBootID),
			RuntimeSessionUID:        harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
			RuntimeSessionGeneration: uint64(execution.RuntimeSessionGeneration),
			TaskUID:                  harnessv2.TaskUID(task.UID),
			TaskAttempt:              uint32(execution.Attempt),
			PromptID:                 harnessv2.PromptID(execution.PromptID),
			Sequence:                 1,
			RequestDigest:            harnessv2.RequestDigest(execution.RequestDigest),
			Timestamp:                now,
		},
		Accepted: &harnessv2.AcceptedEvent{
			AcceptedAt: now,
			Lease: harnessv2.PromptLease{
				Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
			},
			ACPVersion: harnessv2.ACPProfileV1,
		},
	}
	journal := v2eventjournal.Journal{
		EventStore: fixture.controlStore,
		MapContext: v2eventjournal.MapContext{
			Namespace: task.Namespace, TaskName: task.Name, StreamID: task.Name, SessionName: taskSessionName(task),
		},
	}
	state, err := journal.Open(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if appended, isNew, err := state.AppendPromptLifecycleIfNew(fixture.ctx, accepted); err != nil || !isNew || appended == nil {
		t.Fatalf("append recovery prompt acceptance = %#v new=%t err=%v", appended, isNew, err)
	}
	tool := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion,
		Type:     harnessv2.EventUpdate,
		Identity: accepted.Identity,
		Update: &harnessv2.UpdateEvent{
			Kind: harnessv2.UpdateToolCall,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: "recovery-tool", Title: acpRecoveryToolTitle, Kind: acpRecoveryToolKind,
				Status: harnessv2.ToolCallStatusInProgress,
			},
		},
	}
	tool.Identity.Sequence = 2
	tool.Identity.Timestamp = now.Add(time.Millisecond)
	if appended, isNew, err := state.AppendUpdateIfNew(fixture.ctx, tool); err != nil || !isNew || appended == nil {
		t.Fatalf("append recovery open tool = %#v new=%t err=%v", appended, isNew, err)
	}
}

func appendRecoveryPromptTerminal(
	t *testing.T,
	fixture *recoveryFixture,
	task *corev1alpha1.Task,
	eventType harnessv2.EventType,
	mismatchRuntime bool,
) {
	t.Helper()
	execution := task.Status.Execution
	bootID := execution.RuntimeSessionSupervisorBootID
	if mismatchRuntime {
		bootID = "different-supervisor-boot"
	}
	event := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion,
		Type:     eventType,
		Identity: harnessv2.EventIdentity{
			RuntimeInstanceID:        harnessv2.RuntimeInstanceID(execution.RuntimeInstanceID),
			SupervisorBootID:         harnessv2.SupervisorBootID(bootID),
			RuntimeSessionUID:        harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
			RuntimeSessionGeneration: uint64(execution.RuntimeSessionGeneration),
			TaskUID:                  harnessv2.TaskUID(task.UID),
			TaskAttempt:              uint32(execution.Attempt),
			PromptID:                 harnessv2.PromptID(execution.PromptID),
			Sequence:                 3,
			RequestDigest:            harnessv2.RequestDigest(execution.RequestDigest),
			Timestamp:                time.Now().UTC(),
		},
	}
	switch eventType {
	case harnessv2.EventCompleted:
		event.Completed = &harnessv2.CompletedEvent{
			StopReason: harnessv2.ACPStopReasonEndTurn,
			Result: harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{
				Type: harnessv2.ContentBlockText, Text: "exact recovered result",
			}}},
		}
	case harnessv2.EventCancelled:
		event.Cancelled = &harnessv2.CancelledEvent{StopReason: harnessv2.ACPStopReasonCancelled, Reason: acpCancelledOperation}
	case harnessv2.EventOutcomeUnknown:
		event.OutcomeUnknown = &harnessv2.OutcomeUnknownEvent{Code: "runtime_lost", Message: acpRecoveryOutcomeUnknownMessage}
	default:
		event.Failed = &harnessv2.FailedEvent{
			StopReason: harnessv2.ACPStopReasonRefusal, Code: "prompt_failed", Message: acpRecoveryPromptFailedMessage,
		}
	}
	mapped, err := v2eventjournal.MapPromptLifecycle(event, v2eventjournal.MapContext{
		Namespace: task.Namespace, TaskName: task.Name, StreamID: task.Name,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlStore.AppendExecutionEvent(fixture.ctx, mapped); err != nil {
		t.Fatal(err)
	}
}

func appendRecoveryPromptSettlement(
	t *testing.T,
	fixture *recoveryFixture,
	task *corev1alpha1.Task,
	cancellationReason harnessv2.CancelReason,
) {
	t.Helper()
	execution := task.Status.Execution
	now := time.Now().UTC()
	accepted := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion,
		Type:     harnessv2.EventAccepted,
		Identity: harnessv2.EventIdentity{
			RuntimeInstanceID:        harnessv2.RuntimeInstanceID(execution.RuntimeInstanceID),
			SupervisorBootID:         harnessv2.SupervisorBootID(execution.RuntimeSessionSupervisorBootID),
			RuntimeSessionUID:        harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
			RuntimeSessionGeneration: uint64(execution.RuntimeSessionGeneration),
			TaskUID:                  harnessv2.TaskUID(task.UID),
			TaskAttempt:              uint32(execution.Attempt),
			PromptID:                 harnessv2.PromptID(execution.PromptID),
			Sequence:                 1,
			RequestDigest:            harnessv2.RequestDigest(execution.RequestDigest),
			Timestamp:                now,
		},
		Accepted: &harnessv2.AcceptedEvent{
			AcceptedAt: now,
			Lease: harnessv2.PromptLease{
				Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute),
			},
			ACPVersion: harnessv2.ACPProfileV1,
		},
	}
	journal := v2eventjournal.Journal{
		EventStore: fixture.controlStore,
		MapContext: v2eventjournal.MapContext{
			Namespace: task.Namespace, TaskName: task.Name, StreamID: task.Name,
		},
	}
	state, err := journal.Open(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if appended, isNew, err := state.AppendPromptLifecycleIfNew(fixture.ctx, accepted); err != nil || !isNew || appended == nil {
		t.Fatalf("append recovery prompt acceptance = %#v new=%t err=%v", appended, isNew, err)
	}
	settlement := harnessv2.PromptSettlement{
		TerminalEvent: harnessv2.EventCancelled,
		Outcome:       harnessv2.PromptOutcomeCancelled,
		StopReason:    harnessv2.ACPStopReasonCancelled,
		SettledAt:     now.Add(time.Second),
	}
	if appended, isNew, err := state.AppendPromptSettlementIfNew(
		fixture.ctx, settlement, cancellationReason,
	); err != nil || !isNew || appended == nil {
		t.Fatalf("append recovery prompt settlement = %#v new=%t err=%v", appended, isNew, err)
	}
}

func TestACPDispatcherFailsRestoredPreSubmissionTaskWithoutReplayingUnderNewUID(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	sourceUID, restoredUID := restoreACPRecoveryFixtureTask(t, fixture)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}

	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Key.TaskUID != string(sourceUID) || attempt.ExecutionState != store.PromptExecutionFailed {
		t.Fatalf("source attempt = %#v", attempt)
	}
	targetAttemptID, err := (store.PromptAttemptKey{
		Namespace: "default", TaskUID: string(restoredUID), Attempt: 1, PromptID: attempt.Key.PromptID,
	}).CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, targetAttemptID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("target-incarnation attempt lookup error = %v, want ErrNotFound", err)
	}
	projection := assertRestoredTaskTerminalProjection(t, fixture, sourceUID, restoredUID,
		corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed, acpRestorePreSubmissionMessage)

	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.UID != restoredUID || task.Status.Phase != corev1alpha1.TaskPhaseFailed || task.Status.CompletionTime == nil ||
		task.Status.Execution == nil || task.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
		task.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeFailed ||
		task.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason) ||
		task.Status.Execution.ControllerEpoch != fixture.fence.Epoch {
		t.Fatalf("restored task status = %#v", task.Status)
	}

	version, completion, projectionVersion := attempt.Version, task.Status.CompletionTime.DeepCopy(), projection.Version
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Version != version {
		t.Fatalf("idempotent recovery changed source attempt version: got %d, want %d", attempt.Version, version)
	}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.CompletionTime == nil || !task.Status.CompletionTime.Equal(completion) {
		t.Fatalf("idempotent recovery changed completion time: got %v, want %v", task.Status.CompletionTime, completion)
	}
	projection = assertRestoredTaskTerminalProjection(t, fixture, sourceUID, restoredUID,
		corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed, acpRestorePreSubmissionMessage)
	if projection.Version != projectionVersion {
		t.Fatalf("idempotent recovery changed source projection version: got %d, want %d", projection.Version, projectionVersion)
	}
}

func TestACPDispatcherRestoredTaskRecoveryReconstructsProjectionAfterCommittedAttemptTransition(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	sourceUID, restoredUID := restoreACPRecoveryFixtureTask(t, fixture)
	injectedErr := errors.New("injected projection enqueue failure")
	failingStore := &failOnceRecoveryProjectionStore{DurableControlStore: fixture.controlStore, err: injectedErr}
	fixture.dispatcher.Store = failingStore
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); !errors.Is(err, injectedErr) {
		t.Fatalf("first recoverStaleAttempts() error = %v, want injected enqueue failure", err)
	}

	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionFailed ||
		attempt.TerminalReason != acpRestoreIdentityChangedReason || attempt.OutcomeMarker != acpRestorePreSubmissionMessage {
		t.Fatalf("committed source attempt = %#v", attempt)
	}
	committedVersion := attempt.Version
	projectionID := standaloneTaskTerminalProjectionIDForUID("default", sourceUID, 1)
	if _, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, projectionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("projection after injected failure lookup error = %v, want ErrNotFound", err)
	}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Execution == nil || task.Status.Execution.Reason == corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason) {
		t.Fatalf("Task was terminally patched before projection durability: %#v", task.Status.Execution)
	}

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err = fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Version != committedVersion {
		t.Fatalf("projection reconstruction changed source attempt version: got %d, want %d", attempt.Version, committedVersion)
	}
	assertRestoredTaskTerminalProjection(t, fixture, sourceUID, restoredUID,
		corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed, acpRestorePreSubmissionMessage)
}

func TestACPDispatcherMarksRestoredPostWriteTaskOutcomeUnknown(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionAccepted)
	defer fixture.close(t)

	sourceUID, restoredUID := restoreACPRecoveryFixtureTask(t, fixture)
	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionOutcomeUnknown ||
		attempt.TerminalReason != acpRestoreIdentityChangedReason || attempt.OutcomeMarker == "" {
		t.Fatalf("source attempt = %#v", attempt)
	}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: "default", Name: "task"}, task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Execution == nil || task.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown ||
		task.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeOutcomeUnknown ||
		task.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason) {
		t.Fatalf("restored task execution = %#v", task.Status.Execution)
	}
	assertRestoredTaskTerminalProjection(t, fixture, sourceUID, restoredUID,
		corev1alpha1.TaskExecutionStateOutcomeUnknown, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, acpRestorePostWriteMessage)
}

func TestACPDispatcherRestoredTaskRecordsSourceBoundCleanupWhenRuntimeIsAbsent(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionAccepted)
	defer fixture.close(t)

	sourceUID, _ := restoreACPRecoveryFixtureTask(t, fixture)
	task := &corev1alpha1.Task{}
	key := types.NamespacedName{Namespace: "default", Name: "task"}
	if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
		t.Fatal(err)
	}
	task.Status.Execution.RuntimePoolName = "absent-restored-pool"
	task.Status.Execution.RuntimeInstanceID = "source-runtime-instance"
	task.Status.Execution.RuntimeSessionUID = "source-runtime-session"
	task.Status.Execution.RuntimeSessionGeneration = 3
	task.Status.Execution.RuntimeSessionCleanupDigest = ""
	if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}

	if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
		t.Fatal(err)
	}
	wantDigest, err := taskScopedRuntimeSessionCleanupDigest(
		sourceUID, task.Status.Execution.Attempt, task.Status.Execution.RuntimeInstanceID,
		task.Status.Execution.RuntimeSessionUID, task.Status.Execution.RuntimeSessionGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.Execution.RuntimeSessionCleanupDigest != wantDigest || !taskScopedRuntimeSessionCleanupComplete(task) {
		t.Fatalf("restored cleanup receipt = %q, want source-bound %q", task.Status.Execution.RuntimeSessionCleanupDigest, wantDigest)
	}

	projector := &ACPOutboxProjector{
		Client: fixture.kubeClient, Store: fixture.controlStore, Epochs: fixture.dispatcher.Epochs,
		WorkerID: "restored-cleanup-projector", MaxAttempts: 3,
	}
	if err := projector.projectOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	projectionID := standaloneTaskTerminalProjectionIDForUID(task.Namespace, sourceUID, task.Status.Execution.Attempt)
	projection, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.State != store.OutboxProjectionDelivered {
		t.Fatalf("restored source projection state = %s, want Delivered", projection.State)
	}
	reconciler := &TaskReconciler{Client: fixture.kubeClient, DurableControlStore: fixture.controlStore}
	ready, err := reconciler.acpTaskDeletionReady(fixture.ctx, task)
	if err != nil || !ready {
		t.Fatalf("restored Task deletion readiness = %v, %v", ready, err)
	}
}

//nolint:gocyclo // The restored publication states intentionally share one complete recovery/idempotency/finalizer matrix.
func TestACPDispatcherRestoredTerminalExecutionSettlesNonterminalDeliveryAndPublication(t *testing.T) {
	tests := []struct {
		name                 string
		deliveryState        store.PromptDeliveryState
		publicationState     store.PublicationState
		wantDeliveryState    store.PromptDeliveryState
		wantPublicationState store.PublicationState
		wantPreparedReceipt  bool
		wantPhase            corev1alpha1.TaskPhase
	}{
		{
			name: "validating before publication creation", deliveryState: store.PromptDeliveryValidating,
			wantDeliveryState: store.PromptDeliveryConflict, wantPhase: corev1alpha1.TaskPhaseFailed,
		},
		{
			name: "validating after publication creation", deliveryState: store.PromptDeliveryValidating,
			publicationState: store.PublicationPreparing, wantDeliveryState: store.PromptDeliveryPublicationOutcomeUnknown,
			wantPublicationState: store.PublicationOutcomeUnknown, wantPhase: corev1alpha1.TaskPhaseFailed,
		},
		{
			name: "prepared before remote write", deliveryState: store.PromptDeliveryPrepared,
			publicationState: store.PublicationPrepared, wantDeliveryState: store.PromptDeliveryCancelledBeforePublish,
			wantPublicationState: store.PublicationCancelledBeforePublish, wantPreparedReceipt: true,
			wantPhase: corev1alpha1.TaskPhaseCancelled,
		},
		{
			name: "publishing with remote outcome unknown", deliveryState: store.PromptDeliveryPublishing,
			publicationState: store.PublicationPublishing, wantDeliveryState: store.PromptDeliveryPublicationOutcomeUnknown,
			wantPublicationState: store.PublicationOutcomeUnknown, wantPreparedReceipt: true,
			wantPhase: corev1alpha1.TaskPhaseFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newACPRecoveryFixture(t, store.PromptExecutionAccepted)
			defer fixture.close(t)

			attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			for _, next := range []store.PromptExecutionState{
				store.PromptExecutionRunning, store.PromptExecutionSettling, store.PromptExecutionSucceeded,
			} {
				attempt, err = fixture.controlStore.TransitionPromptAttemptExecution(fixture.ctx, store.PromptAttemptExecutionTransition{
					ID: attempt.ID, Fence: fixture.fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
					NewState: next, OperationID: "restore-terminal-" + string(next),
					OperationDigest: testControlDigestForDispatcher("restore-terminal-" + string(next)), UpdatedAt: time.Now().UTC(),
				})
				if err != nil {
					t.Fatalf("transition source execution to %s: %v", next, err)
				}
			}
			deliveryPath := []store.PromptDeliveryState{store.PromptDeliveryValidating}
			switch tt.deliveryState {
			case store.PromptDeliveryValidating:
			case store.PromptDeliveryPrepared:
				deliveryPath = append(deliveryPath, store.PromptDeliveryPreparing, store.PromptDeliveryPrepared)
			case store.PromptDeliveryPublishing:
				deliveryPath = append(deliveryPath, store.PromptDeliveryPreparing, store.PromptDeliveryPrepared)
				deliveryPath = append(deliveryPath, store.PromptDeliveryPublishing)
			default:
				t.Fatalf("unsupported delivery fixture state %s", tt.deliveryState)
			}
			for _, next := range deliveryPath {
				attempt, err = fixture.controlStore.TransitionPromptAttemptDelivery(fixture.ctx, store.PromptAttemptDeliveryTransition{
					ID: attempt.ID, Fence: fixture.fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
					NewState: next, OperationID: "restore-delivery-" + string(next),
					OperationDigest: testControlDigestForDispatcher("restore-delivery-" + string(next)), UpdatedAt: time.Now().UTC(),
				})
				if err != nil {
					t.Fatalf("transition source delivery to %s: %v", next, err)
				}
			}

			key := types.NamespacedName{Namespace: "default", Name: "task"}
			task := &corev1alpha1.Task{}
			if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
				t.Fatal(err)
			}
			task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite}
			if err := fixture.kubeClient.Update(fixture.ctx, task); err != nil {
				t.Fatal(err)
			}
			if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
				t.Fatal(err)
			}
			task.Status.Phase = corev1alpha1.TaskPhaseRunning
			task.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
			task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeSucceeded
			evidenceTransition := metav1.NewTime(time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC))
			task.Status.Delivery = restoredDeliveryEvidenceFixture(tt.deliveryState, "", evidenceTransition)
			publicationID := publicationIDForTask(task)
			task.Status.Delivery.PublicationID = publicationID
			var publication *store.Publication
			if tt.publicationState != "" {
				publication = createACPRecoveryPublication(t, fixture, task, tt.publicationState)
			}
			sourceDeliveryEvidence := task.Status.Delivery.DeepCopy()
			if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
				t.Fatal(err)
			}

			sourceUID, restoredUID := restoreACPRecoveryFixtureTask(t, fixture)
			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatal(err)
			}

			attempt, err = fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.ExecutionState != store.PromptExecutionSucceeded || attempt.DeliveryState != tt.wantDeliveryState {
				t.Fatalf("settled source PromptAttempt = %#v", attempt)
			}
			if tt.wantPublicationState == "" {
				if _, err := fixture.controlStore.GetPublication(fixture.ctx, publicationID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("restored recovery invented Publication %q: %v", publicationID, err)
				}
			} else {
				publication, err = fixture.controlStore.GetPublication(fixture.ctx, publicationID)
				if err != nil {
					t.Fatal(err)
				}
				if publication.TaskUID != string(sourceUID) || publication.State != tt.wantPublicationState ||
					(publication.PreparedReceipt != nil) != tt.wantPreparedReceipt {
					t.Fatalf("settled source Publication = %#v", publication)
				}
			}
			if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
				t.Fatal(err)
			}
			if task.UID != restoredUID || task.Status.Phase != tt.wantPhase || task.Status.Execution == nil ||
				task.Status.Execution.State != corev1alpha1.TaskExecutionStateSucceeded ||
				task.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded ||
				task.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason) ||
				task.Status.Delivery == nil || store.PromptDeliveryState(task.Status.Delivery.State) != tt.wantDeliveryState {
				t.Fatalf("settled restored Task status = %#v", task.Status)
			}
			assertRestoredDeliveryEvidence(t, task.Status.Delivery, sourceDeliveryEvidence, tt.wantDeliveryState)
			if task.Status.Delivery.LastTransitionTime.Equal(sourceDeliveryEvidence.LastTransitionTime) {
				t.Fatal("restored delivery settlement did not advance its transition time")
			}
			firstRestoredDelivery := task.Status.Delivery.DeepCopy()
			projectionID := standaloneTaskTerminalProjectionIDForUID(task.Namespace, sourceUID, task.Status.Execution.Attempt)
			projection, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, projectionID)
			if err != nil {
				t.Fatal(err)
			}
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.TaskUID != string(sourceUID) || payload.Phase != tt.wantPhase ||
				payload.Execution.State != corev1alpha1.TaskExecutionStateSucceeded || payload.Delivery == nil ||
				store.PromptDeliveryState(payload.Delivery.State) != tt.wantDeliveryState {
				t.Fatalf("settled source projection payload = %#v", payload)
			}
			assertRestoredDeliveryEvidence(t, payload.Delivery, sourceDeliveryEvidence, tt.wantDeliveryState)
			if !reflect.DeepEqual(payload.Delivery, task.Status.Delivery) {
				t.Fatalf("projection and restored Task delivery evidence differ: projection=%#v task=%#v", payload.Delivery, task.Status.Delivery)
			}

			attemptVersion, projectionVersion := attempt.Version, projection.Version
			publicationVersion := int64(0)
			if publication != nil {
				publicationVersion = publication.Version
			}
			if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
				t.Fatalf("idempotent restored recovery: %v", err)
			}
			attempt, err = fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantPublicationState == "" {
				if _, err := fixture.controlStore.GetPublication(fixture.ctx, publicationID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("idempotent recovery invented Publication %q: %v", publicationID, err)
				}
			} else {
				publication, err = fixture.controlStore.GetPublication(fixture.ctx, publicationID)
				if err != nil {
					t.Fatal(err)
				}
				if publication.Version != publicationVersion {
					t.Fatalf("idempotent recovery changed Publication version: got %d, want %d", publication.Version, publicationVersion)
				}
			}
			projection, err = fixture.controlStore.GetOutboxProjection(fixture.ctx, projectionID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.Version != attemptVersion || projection.Version != projectionVersion {
				t.Fatalf("idempotent recovery changed versions: attempt %d/%d projection %d/%d",
					attemptVersion, attempt.Version, projectionVersion, projection.Version)
			}
			if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(task.Status.Delivery, firstRestoredDelivery) {
				t.Fatalf("idempotent recovery changed restored delivery evidence: before=%#v after=%#v", firstRestoredDelivery, task.Status.Delivery)
			}

			projector := &ACPOutboxProjector{
				Client: fixture.kubeClient, Store: fixture.controlStore, Epochs: fixture.dispatcher.Epochs,
				WorkerID: "restored-publication-projector", MaxAttempts: 3,
			}
			if err := projector.projectOnce(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			reconciler := &TaskReconciler{Client: fixture.kubeClient, DurableControlStore: fixture.controlStore}
			ready, err := reconciler.acpTaskDeletionReady(fixture.ctx, task)
			if err != nil || !ready {
				t.Fatalf("settled restored Task deletion readiness = %v, %v", ready, err)
			}
		})
	}
}

// TestDeletingRestoredACPTaskReclaimsSourcePromptAttemptAndRemovesFinalizer
// isolates the Kubernetes-backed PromptAttempt reclamation/finalizer boundary.
// The recovery matrix above separately proves source-UID Publication settlement
// and projection continuity for write workspaces.
//
//nolint:gocyclo // The matrix keeps each crash-tail step and its exactly-once assertions visible together.
func TestDeletingRestoredACPTaskReclaimsSourcePromptAttemptAndRemovesFinalizer(t *testing.T) {
	tests := []struct {
		name     string
		delivery store.PromptDeliveryState
		phase    corev1alpha1.TaskPhase
	}{
		{name: "succeeded verified exact", delivery: store.PromptDeliveryVerifiedExact, phase: corev1alpha1.TaskPhaseSucceeded},
		{name: "succeeded cancelled before publish", delivery: store.PromptDeliveryCancelledBeforePublish, phase: corev1alpha1.TaskPhaseCancelled},
		{name: "succeeded publication outcome unknown", delivery: store.PromptDeliveryPublicationOutcomeUnknown, phase: corev1alpha1.TaskPhaseFailed},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			sourceUID := types.UID(fmt.Sprintf("55555555-5555-5555-5555-%012d", index+1))
			restoredUID := types.UID(fmt.Sprintf("66666666-6666-6666-6666-%012d", index+1))
			taskName := "restored-finalizer-" + strconv.Itoa(index+1)
			promptID := "prompt-" + string(sourceUID) + "-1"
			sourceTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: taskName, UID: sourceUID},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
			}
			reconciler := newUnitReconciler(newTestScheme(), sourceTask)
			base := withControllerEpochLeaseUIDs(t, reconciler.Client)
			var promptAttemptDeletes atomic.Int32
			var finalizerPatches atomic.Int32
			intercepted := interceptor.NewClient(base, interceptor.Funcs{
				Delete: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					if _, isPromptAttempt := object.(*corev1alpha1.PromptAttempt); isPromptAttempt {
						promptAttemptDeletes.Add(1)
					}
					return delegate.Delete(ctx, object, options...)
				},
				Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if _, isTask := object.(*corev1alpha1.Task); isTask {
						call := finalizerPatches.Add(1)
						if call == 1 {
							return errors.New("simulated crash after durable PromptAttempt reclamation")
						}
					}
					return delegate.Patch(ctx, object, patch, options...)
				},
			})
			reconciler.Client = intercepted
			reconciler.APIReader = intercepted

			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "restored-finalizer.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			persistence := sqlite.NewStore(db, "restored-finalizer")
			controlStore, err := kubestore.NewComposite(intercepted, "default", persistence, kubestore.WithAPIReader(intercepted))
			if err != nil {
				t.Fatal(err)
			}
			epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "restored-finalizer-"+strconv.Itoa(index+1))
			defer stopEpoch()
			fence, err := epochs.CurrentFence(ctx)
			if err != nil {
				t.Fatal(err)
			}
			reconciler.DurableControlStore = controlStore
			reconciler.ControllerEpochManager = epochs

			attemptKey := store.PromptAttemptKey{Namespace: sourceTask.Namespace, TaskUID: string(sourceUID), Attempt: 1, PromptID: promptID}
			attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
				Key: attemptKey, RequestDigest: testControlDigestForDispatcher("restored-finalizer-" + strconv.Itoa(index+1)),
			}), fence)
			if err != nil {
				t.Fatal(err)
			}
			for _, state := range []store.PromptExecutionState{
				store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
				store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning,
				store.PromptExecutionSettling, store.PromptExecutionSucceeded,
			} {
				attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, fence, attempt, state, nil)
			}
			attempt = transitionRestoredFinalizerDelivery(t, ctx, controlStore, fence, attempt, tt.delivery)

			transitionTime := metav1.NewTime(time.Date(2026, 8, 10, 14, index, 0, 0, time.UTC))
			execution := corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: promptID, RequestDigest: attempt.RequestDigest, LastTransitionTime: &transitionTime,
			}
			delivery := &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryState(tt.delivery), Outcome: corev1alpha1.TaskDeliveryOutcome(tt.delivery),
				LastTransitionTime: &transitionTime,
			}
			projectionPayload := taskTerminalProjection{
				Namespace: sourceTask.Namespace, Task: sourceTask.Name, TaskUID: string(sourceUID), Attempt: 1,
				Phase: tt.phase, Execution: execution, Delivery: delivery.DeepCopy(),
			}
			if err := enqueueDurableTaskTerminalProjectionForUID(ctx, controlStore, fence, sourceTask, sourceUID, projectionPayload); err != nil {
				t.Fatal(err)
			}
			projectionID := standaloneTaskTerminalProjectionIDForUID(sourceTask.Namespace, sourceUID, 1)
			claims, err := controlStore.ClaimOutboxProjections(ctx, store.ClaimOutboxProjectionsRequest{
				Fence: fence, WorkerID: "restored-finalizer", Limit: 1, LeaseDuration: time.Minute, Now: time.Now().UTC(),
			})
			if err != nil || len(claims) != 1 || claims[0].ID != projectionID {
				t.Fatalf("claim restored source projection: claims=%#v err=%v", claims, err)
			}
			projection, err := controlStore.CompleteOutboxProjection(ctx, store.CompleteOutboxProjectionRequest{
				ID: claims[0].ID, Fence: fence, ExpectedVersion: claims[0].Version, LeaseOwner: claims[0].LeaseOwner,
				OperationID: "deliver-restored-finalizer", OperationDigest: testControlDigestForDispatcher("deliver-restored-finalizer-" + strconv.Itoa(index+1)),
				NewState: store.OutboxProjectionDelivered, DeliveryDigest: testControlDigestForDispatcher("delivered-restored-finalizer-" + strconv.Itoa(index+1)),
				UpdatedAt: time.Now().UTC(),
			})
			if err != nil {
				t.Fatal(err)
			}

			if err := intercepted.Delete(ctx, sourceTask); err != nil {
				t.Fatal(err)
			}
			binding := testACPExecuteBindingForDispatcher()
			binding.Task.UID = sourceUID
			binding.Snapshot.ID = (store.AgentExecutionSnapshotKey{TaskUID: string(sourceUID), Digest: attempt.SnapshotDigest}).ID()
			binding.Snapshot.SchemaVersion = store.AgentExecutionSnapshotSchemaVersion
			restored := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: sourceTask.Namespace, Name: sourceTask.Name, UID: restoredUID,
					Finalizers: []string{labels.TaskFinalizer},
				},
				Spec: sourceTask.Spec,
			}
			if err := intercepted.Create(ctx, restored); err != nil {
				t.Fatal(err)
			}
			restored.Status = corev1alpha1.TaskStatus{
				Phase: tt.phase, Attempts: 1, AgentExecutionBinding: binding,
				Execution: &corev1alpha1.TaskExecutionStatus{
					State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
					Attempt: 1, PromptID: promptID, RequestDigest: attempt.RequestDigest, LastTransitionTime: &transitionTime,
				},
				Delivery: delivery.DeepCopy(),
			}
			if err := intercepted.Status().Update(ctx, restored); err != nil {
				t.Fatal(err)
			}
			if err := intercepted.Delete(ctx, restored); err != nil {
				t.Fatal(err)
			}
			key := types.NamespacedName{Namespace: restored.Namespace, Name: restored.Name}
			if err := intercepted.Get(ctx, key, restored); err != nil {
				t.Fatal(err)
			}
			if !acpTaskHasUnvalidatedSourceIdentity(restored) || restored.Status.Execution.RuntimePoolName != "" ||
				restored.Status.Execution.AgentRuntimeName != "" || restored.DeletionTimestamp.IsZero() {
				t.Fatalf("pre-recovery deleting restore did not exercise the unvalidated no-runtime selector: %#v", restored)
			}

			dispatcher := &ACPDispatcher{
				Client: intercepted, APIReader: intercepted, Store: controlStore, ResultStore: persistence, Epochs: epochs,
			}
			if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
				t.Fatalf("recover deleting restored Task: %v", err)
			}
			if err := intercepted.Get(ctx, key, restored); err != nil {
				t.Fatal(err)
			}
			if !acpTaskUsesRestoredSourceIdentity(restored) || restored.DeletionTimestamp.IsZero() ||
				restored.Status.Execution.RuntimePoolName != "" || restored.Status.Execution.AgentRuntimeName != "" {
				t.Fatalf("recovered deleting restore = %#v", restored)
			}

			if _, err := reconciler.handleDeletion(ctx, restored); err == nil || !strings.Contains(err.Error(), "simulated crash") {
				t.Fatalf("first handleDeletion() error = %v, want simulated finalizer-patch crash", err)
			}
			if got := promptAttemptDeletes.Load(); got != 1 {
				t.Fatalf("source PromptAttempt delete calls after crash = %d, want 1", got)
			}
			if _, err := controlStore.GetPromptAttempt(ctx, attempt.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("source PromptAttempt after crash = %v, want ErrNotFound", err)
			}
			if err := intercepted.Get(ctx, key, restored); err != nil {
				t.Fatal(err)
			}
			condition := apiMeta.FindStatusCondition(restored.Status.Conditions, "ACPPromptAttemptsReclaimed")
			if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "PromptAttemptsReclaimed" || condition.Message == "" {
				t.Fatalf("durable PromptAttempt reclamation receipt after crash = %#v", condition)
			}
			if !controllerutil.ContainsFinalizer(restored, labels.TaskFinalizer) || finalizerPatches.Load() != 1 {
				t.Fatalf("Task finalizer after crash = %#v, patch calls=%d", restored.Finalizers, finalizerPatches.Load())
			}
			assertNoRestoredFinalizerReclamationMarker(t, ctx, intercepted)
			storedProjection, err := controlStore.GetOutboxProjection(ctx, projectionID)
			if err != nil || storedProjection.Version != projection.Version || storedProjection.State != store.OutboxProjectionDelivered {
				t.Fatalf("source projection after crash = %#v err=%v, want unchanged delivered version %d", storedProjection, err, projection.Version)
			}

			missingStore := &missingRecoveryPromptAttemptStore{DurableControlStore: controlStore}
			dispatcher.Store = missingStore
			if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
				t.Fatalf("dispatcher restart after source attempt reclamation: %v", err)
			}
			if got := missingStore.calls.Load(); got != 0 {
				t.Fatalf("dispatcher restart read reclaimed source PromptAttempt %d times, want 0", got)
			}
			if _, err := controlStore.GetPromptAttempt(ctx, attempt.ID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("dispatcher restart recreated source PromptAttempt: %v", err)
			}

			if _, err := reconciler.handleDeletion(ctx, restored); err != nil {
				t.Fatalf("retry handleDeletion(): %v", err)
			}
			if got := promptAttemptDeletes.Load(); got != 1 {
				t.Fatalf("source PromptAttempt delete calls after retry = %d, want exactly 1", got)
			}
			if got := finalizerPatches.Load(); got != 2 {
				t.Fatalf("Task finalizer patch calls = %d, want failed call plus successful retry", got)
			}
			if err := intercepted.Get(ctx, key, &corev1alpha1.Task{}); !apierrors.IsNotFound(err) {
				t.Fatalf("restored Task after finalizer retry = %v, want NotFound", err)
			}
			assertNoRestoredFinalizerReclamationMarker(t, ctx, intercepted)
			storedProjection, err = controlStore.GetOutboxProjection(ctx, projectionID)
			if err != nil || storedProjection.Version != projection.Version || storedProjection.State != store.OutboxProjectionDelivered {
				t.Fatalf("source projection after retry = %#v err=%v, want unchanged delivered version %d", storedProjection, err, projection.Version)
			}
		})
	}
}

func transitionRestoredFinalizerDelivery(
	t *testing.T,
	ctx context.Context,
	controlStore store.PromptAttemptStore,
	fence store.ControllerEpochFence,
	attempt *store.PromptAttempt,
	target store.PromptDeliveryState,
) *store.PromptAttempt {
	t.Helper()
	path := []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryPreparing, store.PromptDeliveryPrepared}
	switch target {
	case store.PromptDeliveryVerifiedExact:
		path = append(path, store.PromptDeliveryPublishing, store.PromptDeliveryVerifying, target)
	case store.PromptDeliveryCancelledBeforePublish:
		path = append(path, target)
	case store.PromptDeliveryPublicationOutcomeUnknown:
		path = append(path, store.PromptDeliveryPublishing, target)
	default:
		t.Fatalf("unsupported restored finalizer delivery target %q", target)
	}
	for index, next := range path {
		updated, err := controlStore.TransitionPromptAttemptDelivery(ctx, store.PromptAttemptDeliveryTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
			NewState: next, OperationID: "restored-finalizer-delivery-" + strconv.Itoa(index),
			OperationDigest: testControlDigestForDispatcher("restored-finalizer-delivery-" + string(target) + "-" + strconv.Itoa(index)),
			UpdatedAt:       time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("transition restored finalizer delivery to %s: %v", next, err)
		}
		attempt = updated
	}
	return attempt
}

func assertNoRestoredFinalizerReclamationMarker(t *testing.T, ctx context.Context, kubeClient client.Client) {
	t.Helper()
	markers := &corev1.ConfigMapList{}
	if err := kubeClient.List(ctx, markers, client.InNamespace("default")); err != nil {
		t.Fatal(err)
	}
	for i := range markers.Items {
		if strings.HasPrefix(markers.Items[i].Name, "prompt-reclaim-") {
			t.Fatalf("PromptAttempt reclamation marker remained: %s", markers.Items[i].Name)
		}
	}
}

func createACPRecoveryPublication(
	t *testing.T,
	fixture *recoveryFixture,
	task *corev1alpha1.Task,
	target store.PublicationState,
) *store.Publication {
	t.Helper()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	publicationID := publicationIDForTask(task)
	targetRepositoryID := "github.com/orka/target"
	targetRef := "refs/heads/restore"
	baseline := store.RemoteRefState{Absent: true}
	claimID, err := store.CanonicalBranchClaimID(targetRepositoryID, targetRef)
	if err != nil {
		t.Fatal(err)
	}
	claimDigest, err := branchClaimRequestDigest(
		targetRepositoryID, targetRef, store.BranchClaimOwnerTask, string(task.UID), baseline, publicationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := fixture.controlStore.CreateBranchClaim(fixture.ctx, &store.BranchClaim{
		ID: claimID, RepositoryID: targetRepositoryID, Ref: targetRef,
		OwnerKind: store.BranchClaimOwnerTask, OwnerUID: string(task.UID), Generation: 1,
		LastVerified: baseline, RequestDigest: claimDigest, CreatedAt: now,
	}, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := fixture.controlStore.CreatePublication(fixture.ctx, &store.Publication{
		ID: publicationID, Namespace: task.Namespace, Generation: 1,
		TaskUID: string(task.UID), Attempt: int64(task.Status.Execution.Attempt), PromptID: task.Status.Execution.PromptID,
		BranchClaimID: claim.ID, BranchClaimGeneration: claim.Generation,
		SourceRepositoryID: "github.com/orka/source", SourceRef: "refs/heads/main", SourceBaselineSHA: strings.Repeat("1", 40),
		TargetRepositoryID: targetRepositoryID, TargetRef: targetRef, Baseline: baseline,
		ArtifactID: "restore-artifact", ArtifactDigest: testControlDigestForDispatcher("restore-artifact"), ArtifactSizeBytes: 1,
		ArtifactMediaType: "application/vnd.orka.workspace-delta.v1+tar", PublicationCredentialRef: "secret/default/publisher#token",
		CommitIdentity: "Orka <orka@example.invalid>", CommitMessage: "restore publication fixture", CommitTimestamp: now,
		RequestDigest: testControlDigestForDispatcher("restore-publication"), CreatedAt: now,
	}, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	if target == store.PublicationPreparing {
		return publication
	}
	prepared := &store.PreparedPublicationReceipt{
		OperationID: "restore-prepare", RequestDigest: testControlDigestForDispatcher("restore-prepare"),
		TreeSHA: strings.Repeat("2", 40), CommitSHA: strings.Repeat("3", 40),
		ManifestDigest: testControlDigestForDispatcher("restore-manifest"), RelativeRoot: ".",
		BundleArtifactID: "restore-bundle", BundleDigest: testControlDigestForDispatcher("restore-bundle"),
		BundleSizeBytes: 1, BundleMediaType: store.PreparedBundleMediaType,
		BundleRef: "refs/orka/publications/" + strings.Repeat("4", 64), PreparedAt: now.Add(time.Minute),
	}
	publication, err = fixture.controlStore.TransitionPublication(fixture.ctx, store.PublicationTransition{
		ID: publication.ID, Fence: fixture.fence, ExpectedVersion: publication.Version, ExpectedGeneration: publication.Generation,
		ExpectedState: publication.State, NewState: store.PublicationPrepared,
		OperationID: prepared.OperationID, OperationDigest: prepared.RequestDigest, PreparedReceipt: prepared, UpdatedAt: prepared.PreparedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if target == store.PublicationPublishing {
		publication, err = fixture.controlStore.TransitionPublication(fixture.ctx, store.PublicationTransition{
			ID: publication.ID, Fence: fixture.fence, ExpectedVersion: publication.Version, ExpectedGeneration: publication.Generation,
			ExpectedState: publication.State, NewState: store.PublicationPublishing,
			OperationID: "restore-publishing", OperationDigest: testControlDigestForDispatcher("restore-publishing"), UpdatedAt: now.Add(2 * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if publication.State != target {
		t.Fatalf("publication fixture state = %s, want %s", publication.State, target)
	}
	return publication
}

func assertRestoredTaskTerminalProjection(
	t *testing.T,
	fixture *recoveryFixture,
	sourceUID types.UID,
	restoredUID types.UID,
	state corev1alpha1.TaskExecutionState,
	outcome corev1alpha1.TaskExecutionOutcome,
	message string,
) *store.OutboxProjection {
	t.Helper()
	projectionID := standaloneTaskTerminalProjectionIDForUID("default", sourceUID, 1)
	projection, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.AggregateKind != "Task" || projection.AggregateID != string(sourceUID) ||
		projection.ProjectionKind != "TaskTerminalStatus" {
		t.Fatalf("source projection metadata = %#v", projection)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TaskUID != string(sourceUID) || payload.Attempt != 1 || payload.Phase != corev1alpha1.TaskPhaseFailed ||
		payload.Execution.State != state || payload.Execution.Outcome != outcome ||
		payload.Execution.Reason != corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason) ||
		payload.Message != message {
		t.Fatalf("source projection payload = %#v", payload)
	}
	targetProjectionID := standaloneTaskTerminalProjectionIDForUID("default", restoredUID, 1)
	if _, err := fixture.controlStore.GetOutboxProjection(fixture.ctx, targetProjectionID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("target-incarnation projection lookup error = %v, want ErrNotFound", err)
	}
	return projection
}

func restoredDeliveryEvidenceFixture(
	state store.PromptDeliveryState,
	outcome corev1alpha1.TaskDeliveryOutcome,
	transition metav1.Time,
) *corev1alpha1.TaskDeliveryStatus {
	remoteBefore := strings.Repeat("2", 40)
	return &corev1alpha1.TaskDeliveryStatus{
		State:                 corev1alpha1.TaskDeliveryState(state),
		Outcome:               outcome,
		Reason:                corev1alpha1.TaskDeliveryReason("RestoreEvidencePreserved"),
		PublicationID:         "publication-restore-evidence",
		SourceRepository:      &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/orka/source"},
		PublicationRepository: &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/orka/target"},
		Branch:                "restore-evidence",
		StartingSHA:           strings.Repeat("1", 40),
		RemoteBeforeSHA:       &remoteBefore,
		TreeSHA:               strings.Repeat("3", 40),
		ExpectedCommitSHA:     strings.Repeat("4", 40),
		VerifiedRemoteSHA:     strings.Repeat("5", 40),
		SupersedingRemoteSHA:  strings.Repeat("6", 40),
		ArtifactDigest:        testControlDigestForDispatcher("restore-delivery-evidence"),
		PRReceipt: &corev1alpha1.TaskPullRequestReceipt{
			ID: "restore-evidence-pr", Number: 42, URL: "https://github.com/orka/target/pull/42", State: "open",
		},
		Message:            "bounded restore delivery evidence",
		LastTransitionTime: &transition,
	}
}

func assertRestoredDeliveryEvidence(
	t *testing.T,
	got *corev1alpha1.TaskDeliveryStatus,
	wantEvidence *corev1alpha1.TaskDeliveryStatus,
	terminalState store.PromptDeliveryState,
) {
	t.Helper()
	if got == nil || wantEvidence == nil || got.LastTransitionTime == nil {
		t.Fatalf("restored delivery evidence is incomplete: got=%#v want=%#v", got, wantEvidence)
	}
	want := wantEvidence.DeepCopy()
	want.State = corev1alpha1.TaskDeliveryState(terminalState)
	want.Outcome = corev1alpha1.TaskDeliveryOutcome(terminalState)
	want.LastTransitionTime = got.LastTransitionTime
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("restored delivery evidence = %#v, want %#v", got, want)
	}
}

func TestACPDispatcherRejectsCorruptRestoredSourceAttemptBinding(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	restoreACPRecoveryFixtureTask(t, fixture)
	task := markRestoredACPRecoveryTaskDeleting(t, fixture, func(task *corev1alpha1.Task) {
		task.Status.Execution.RuntimePoolName = ""
		task.Status.Execution.AgentRuntimeName = ""
		task.Status.AgentExecutionBinding.Snapshot.Digest = testControlDigestForDispatcher("corrupt-restore-snapshot")
	})
	if !acpTaskHasUnvalidatedSourceIdentity(task) || task.DeletionTimestamp.IsZero() {
		t.Fatalf("corrupt restored Task did not exercise deleting recovery selector: %#v", task)
	}
	before, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}

	err = fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("recoverStaleAttempts() error = %v, want ErrConflict", err)
	}
	attempt, getErr := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if attempt.ExecutionState != store.PromptExecutionPlanned || attempt.Version != before.Version {
		t.Fatalf("corrupt restore mutated source attempt: before=%#v after=%#v", before, attempt)
	}
}

func TestACPDispatcherRejectsDeletingRestoredTaskWithMissingExecutionBeforeRuntimeSelector(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionPlanned)
	defer fixture.close(t)

	restoreACPRecoveryFixtureTask(t, fixture)
	task := markRestoredACPRecoveryTaskDeleting(t, fixture, func(task *corev1alpha1.Task) {
		task.Status.Execution = nil
	})
	if !acpTaskHasUnvalidatedSourceIdentity(task) || task.DeletionTimestamp.IsZero() {
		t.Fatalf("missing-execution restored Task did not exercise deleting recovery selector: %#v", task)
	}
	missingStore := &missingRecoveryPromptAttemptStore{DurableControlStore: fixture.controlStore}
	fixture.dispatcher.Store = missingStore

	err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx)
	if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "execution status is missing") {
		t.Fatalf("recoverStaleAttempts() error = %v, want missing-execution ErrConflict", err)
	}
	if got := missingStore.calls.Load(); got != 0 {
		t.Fatalf("missing-execution restore read source PromptAttempt %d times, want 0", got)
	}
	attempt, getErr := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if attempt.ExecutionState != store.PromptExecutionPlanned {
		t.Fatalf("missing-execution restore mutated source attempt to %s", attempt.ExecutionState)
	}
}

func restoreACPRecoveryFixtureTask(t *testing.T, fixture *recoveryFixture) (types.UID, types.UID) {
	t.Helper()
	task := &corev1alpha1.Task{}
	key := types.NamespacedName{Namespace: "default", Name: "task"}
	if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
		t.Fatal(err)
	}
	sourceUID := task.UID
	restoredUID := types.UID("44444444-4444-4444-4444-444444444444")
	task.UID = restoredUID
	task.ResourceVersion = ""
	binding := *task.Status.AgentExecutionBinding
	binding.Task.UID = sourceUID
	task.Status.AgentExecutionBinding = &binding
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	fixture.kubeClient = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	fixture.dispatcher.Client = fixture.kubeClient
	fixture.dispatcher.APIReader = fixture.kubeClient
	return sourceUID, restoredUID
}

func markRestoredACPRecoveryTaskDeleting(
	t *testing.T,
	fixture *recoveryFixture,
	mutateStatus func(*corev1alpha1.Task),
) *corev1alpha1.Task {
	t.Helper()
	key := types.NamespacedName{Namespace: "default", Name: "task"}
	task := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
		t.Fatal(err)
	}
	controllerutil.AddFinalizer(task, labels.TaskFinalizer)
	if err := fixture.kubeClient.Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
		t.Fatal(err)
	}
	mutateStatus(task)
	if err := fixture.kubeClient.Status().Update(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Delete(fixture.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := fixture.kubeClient.Get(fixture.ctx, key, task); err != nil {
		t.Fatal(err)
	}
	if task.DeletionTimestamp.IsZero() {
		t.Fatal("restored Task deletion timestamp is missing")
	}
	return task
}

//nolint:gocyclo // This intentionally exercises the full production recovery boundary in one scenario.
func TestACPDispatcherRecoveryReusesExistingTaskScopedTerminalProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "task-projection.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	oldEpochs, stopOld := startACPRecoveryEpochManager(t, ctx, controlStore, "controller-old-task-projection")
	oldFence, err := oldEpochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}

	uid := types.UID("77777777-7777-7777-7777-777777777777")
	promptID := "prompt-" + string(uid) + "-1"
	fixedTransition := metav1.NewTime(time.Date(2026, 7, 25, 12, 30, 0, 0, time.UTC))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task-projection", UID: uid},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "test"},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1, Message: "ACP task completed",
			AgentExecutionBinding: testACPExecuteBindingForDispatcher(),
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: promptID, RuntimePoolName: "pool", RuntimePoolUID: "pool-uid", ControllerEpoch: oldFence.Epoch,
				RequestDigest: testControlDigestForDispatcher("task-projection-request"), LastTransitionTime: &fixedTransition,
			},
			Delivery: restoredDeliveryEvidenceFixture(
				store.PromptDeliveryReadValidated,
				corev1alpha1.TaskDeliveryOutcomeReadValidated,
				fixedTransition,
			),
		},
	}
	task.Status.AgentExecutionBinding.Task.UID = uid
	key := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: promptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		Key: key, RequestDigest: task.Status.Execution.RequestDigest,
	}), oldFence)
	if err != nil {
		t.Fatal(err)
	}
	attempt = completeACPAttemptExecutionForTest(t, controlStore, oldFence, attempt, false)
	for _, next := range []store.PromptDeliveryState{store.PromptDeliveryValidating, store.PromptDeliveryReadValidated} {
		op := "task-projection-delivery-" + string(next)
		attempt, err = controlStore.TransitionPromptAttemptDelivery(ctx, store.PromptAttemptDeliveryTransition{
			ID: attempt.ID, Fence: oldFence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
			NewState: next, OperationID: op, OperationDigest: testControlDigestForDispatcher(op), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	oldDispatcher := &ACPDispatcher{Store: controlStore, Epochs: oldEpochs}
	payload := taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
		Phase: corev1alpha1.TaskPhaseSucceeded, Message: "ACP task completed",
		Execution: *task.Status.Execution.DeepCopy(),
		Delivery:  task.Status.Delivery.DeepCopy(),
	}
	if err := oldDispatcher.enqueueStandaloneTaskProjection(ctx, task, payload); err != nil {
		t.Fatal(err)
	}
	projectionID := standaloneTaskTerminalProjectionID(task, 1)

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sourceClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	projector := &ACPOutboxProjector{Client: sourceClient, Store: controlStore, Epochs: oldEpochs, WorkerID: "task-projection-test"}
	if err := projector.projectOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sourceTerminal := &corev1alpha1.Task{}
	if err := sourceClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, sourceTerminal); err != nil {
		t.Fatal(err)
	}
	original, err := controlStore.GetOutboxProjection(ctx, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	if original.State != store.OutboxProjectionDelivered || original.DeliveryDigest == "" || original.DeliveredAt == nil {
		t.Fatalf("projection was not delivered before restart: %#v", original)
	}
	restoredUID := types.UID("88888888-8888-8888-8888-888888888888")
	restoredTask := sourceTerminal.DeepCopy()
	restoredTask.UID = restoredUID
	restoredTask.ResourceVersion = ""
	binding := *restoredTask.Status.AgentExecutionBinding
	binding.Task.UID = uid
	restoredTask.Status.AgentExecutionBinding = &binding
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(restoredTask).Build()
	stopOld()
	newEpochs, stopNew := startACPRecoveryEpochManager(t, ctx, controlStore, "controller-new-task-projection")
	defer stopNew()
	newFence, err := newEpochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newFence.Epoch <= oldFence.Epoch {
		t.Fatalf("new epoch = %d, want > %d", newFence.Epoch, oldFence.Epoch)
	}
	dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore, Epochs: newEpochs}
	if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
		t.Fatal(err)
	}
	recoveredTask := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, recoveredTask); err != nil {
		t.Fatal(err)
	}
	if recoveredTask.UID != restoredUID || recoveredTask.Status.Phase != corev1alpha1.TaskPhaseSucceeded ||
		recoveredTask.Status.Execution == nil || recoveredTask.Status.Execution.ControllerEpoch != newFence.Epoch ||
		recoveredTask.Status.Execution.State != corev1alpha1.TaskExecutionStateSucceeded ||
		recoveredTask.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeSucceeded ||
		recoveredTask.Status.Execution.Reason != corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason) ||
		recoveredTask.Status.Delivery == nil || recoveredTask.Status.Delivery.State != corev1alpha1.TaskDeliveryStateReadValidated {
		t.Fatalf("restored terminal Task status = %#v", recoveredTask.Status)
	}
	assertRestoredDeliveryEvidence(t, recoveredTask.Status.Delivery, task.Status.Delivery, store.PromptDeliveryReadValidated)
	firstRecoveredDelivery := recoveredTask.Status.Delivery.DeepCopy()
	if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
		t.Fatalf("same-epoch recovery retry: %v", err)
	}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, recoveredTask); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recoveredTask.Status.Delivery, firstRecoveredDelivery) {
		t.Fatalf("same-epoch recovery changed restored delivery evidence: before=%#v after=%#v", firstRecoveredDelivery, recoveredTask.Status.Delivery)
	}
	recovered, err := controlStore.GetOutboxProjection(ctx, projectionID)
	if err != nil {
		t.Fatal(err)
	}
	deliveredAtMatches := recovered.DeliveredAt != nil && original.DeliveredAt != nil && recovered.DeliveredAt.Equal(*original.DeliveredAt)
	if string(recovered.Payload) != string(original.Payload) || recovered.PayloadDigest != original.PayloadDigest ||
		!recovered.InitialAvailableAt.Equal(original.InitialAvailableAt) || !recovered.AvailableAt.Equal(original.AvailableAt) ||
		recovered.State != original.State || recovered.Version != original.Version || recovered.Attempts != original.Attempts ||
		recovered.DeliveryDigest != original.DeliveryDigest || !deliveredAtMatches || recovered.ControllerEpoch != original.ControllerEpoch {
		t.Fatalf("recovery changed existing task projection: before=%#v after=%#v", original, recovered)
	}
}

func TestValidateRestoredSourceTerminalProjectionRejectsForgedOutcome(t *testing.T) {
	sourceUID := types.UID("99999999-9999-9999-9999-999999999999")
	restoredUID := types.UID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	requestDigest := testControlDigestForDispatcher("forged-source-projection-request")
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "forged-source-projection", UID: restoredUID},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Reason: corev1alpha1.TaskExecutionReason(acpRestoreIdentityChangedReason), Attempt: 1,
				PromptID: "prompt-final", RequestDigest: requestDigest,
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
			},
			AgentExecutionBinding: testACPExecuteBindingForDispatcher(),
		},
	}
	task.Status.AgentExecutionBinding.Task.UID = sourceUID
	attempt := &store.PromptAttempt{
		Key: store.PromptAttemptKey{
			Namespace: task.Namespace, TaskUID: string(sourceUID), Attempt: 1, PromptID: task.Status.Execution.PromptID,
		},
		RequestDigest: requestDigest, ExecutionState: store.PromptExecutionSucceeded,
		DeliveryState: store.PromptDeliveryVerifiedExact,
	}
	payload, err := json.Marshal(taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(sourceUID), Attempt: 1,
		Phase: corev1alpha1.TaskPhaseSucceeded,
		Execution: corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeFailed,
			Attempt: 1, PromptID: task.Status.Execution.PromptID, RequestDigest: requestDigest,
		},
		Delivery: task.Status.Delivery.DeepCopy(),
	})
	if err != nil {
		t.Fatal(err)
	}
	projection := &store.OutboxProjection{
		ID:            standaloneTaskTerminalProjectionIDForUID(task.Namespace, sourceUID, 1),
		AggregateKind: taskResourceKind, AggregateID: string(sourceUID), ProjectionKind: taskTerminalProjectionKind,
		Payload: payload,
	}

	if err := validateRestoredSourceTerminalProjection(task, sourceUID, attempt, projection); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("validateRestoredSourceTerminalProjection() error = %v, want ErrConflict", err)
	}
}

//nolint:gocyclo // The restart, drain, retry, receipt, and projection assertions stay in one scenario.
func TestRecoveredTaskScopedRuntimeSessionCleanupRetriesBeforeEpochAdvance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "recovered-cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "recovered-cleanup")
	defer stopEpoch()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := harnessProfileForTest()
	profile.Model = acpTestModel
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	var deleteCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.CapabilitiesPath:
			writeDispatcherJSON(w, harnessv2.CapabilitiesResponse{
				Protocol: harnessv2.ProtocolVersion, Transport: "http+ndjson", ACPVersion: harnessv2.ACPProfileV1,
				RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				AdapterDigests: profile.AdapterDigests, Limits: harnessv2.DefaultProtocolLimits(),
				Provider:            harnessv2.ProviderCapabilities{ProviderKinds: []string{profile.ProviderKind}, Models: []string{profile.Model}, SupportsCancel: true, SupportsPermissions: true, SupportsTools: true},
				WorkspaceGovernance: harnessv2.StrictWorkspaceGovernanceCapabilities(), SupportsDrain: true,
			})
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath:
			now := time.Now().UTC()
			writeDispatcherJSON(w, harnessv2.StatusResponse{
				Protocol: harnessv2.ProtocolVersion,
				Fence: harnessv2.Fence{
					RuntimeInstanceID: "pod-uid.boot-id", SupervisorBootID: "boot-id", ControllerEpoch: uint64(fence.Epoch),
					RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: profileDigest,
					ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				},
				Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true},
				Sessions: []harnessv2.RuntimeSessionStatus{{
					RuntimeSessionID: "runtime-session-recovered-cleanup", RuntimeSessionUID: "session-recovered-cleanup",
					Generation: 1, State: harnessv2.RuntimeSessionStateIdle, LastTransitionAt: now,
				}},
				Pressure: harnessv2.PressureMetadata{ResidentSessions: 1}, Timestamp: now,
			})
		case r.Method == http.MethodDelete:
			var request harnessv2.DeleteRuntimeSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode delete: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if deleteCalls.Add(1) == 1 {
				writeDispatcherJSONStatus(w, http.StatusInternalServerError, harnessv2.ErrorResponse{
					Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeSessionPoisoned,
					Message: "runtime descendant cleanup could not be proven", Retryable: false,
				})
				return
			}
			writeDispatcherJSON(w, harnessv2.DeleteRuntimeSessionResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh}, State: harnessv2.RuntimeSessionStateDeleted,
				Tombstone: testDeleteTombstone(request, time.Now().UTC()),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "recovered-cleanup", UID: types.UID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseCancelled, Attempts: 1,
			AgentExecutionBinding: testACPExecuteBindingForDispatcher(),
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateCancelled, Outcome: corev1alpha1.TaskExecutionOutcomeCancelled,
				Attempt: 1, PromptID: "prompt-recovered-cleanup", RuntimePoolName: "pool", RuntimePoolUID: "pool-uid",
				RuntimeInstanceID: "pod-uid.boot-id", RuntimeSessionUID: "session-recovered-cleanup", RuntimeSessionGeneration: 1,
				ControllerEpoch: fence.Epoch, RequestDigest: testControlDigestForDispatcher("recovered-cleanup"),
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested},
		},
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "pool", UID: types.UID("pool-uid"), Generation: 1},
		Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
			Image:   "docker.io/example/acp@sha256:" + strings.Repeat("a", 64),
			Profile: RuntimePoolProfileFromPlan(ACPRuntimePlan{Profile: profile, Digest: profileDigest}),
		}},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: parsed.Host, PodUID: "pod-uid", BootID: "boot-id",
			RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID, ControllerEpoch: fence.Epoch - 1,
			ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(profileDigest), ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
		}},
	}
	// The stale-epoch cleanup runs while the active instance is at fence.Epoch-1
	// and then again after the test advances it to fence.Epoch; both Secrets
	// coexist during that overlap, exactly as during a graceful replacement.
	newSecret := func(epoch int64) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: fmt.Sprintf("pool-auth-e%d", epoch), Labels: map[string]string{
				runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
			}},
			Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))},
		}
	}
	oldSecret := newSecret(fence.Epoch - 1)
	currentSecret := newSecret(fence.Epoch)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, pool, oldSecret, currentSecret).Build()
	attemptKey := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{Key: attemptKey, RequestDigest: task.Status.Execution.RequestDigest}), fence)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning, store.PromptExecutionCancelled,
	} {
		op := "recovered-cleanup-" + string(next)
		attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: next, OperationID: op, OperationDigest: testControlDigestForDispatcher(op), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	gate := NewACPAdmissionGate()
	gate.Close("planned drain", time.Now().UTC())
	dispatcher := &ACPDispatcher{
		Client: kubeClient, APIReader: kubeClient, Store: controlStore, Epochs: epochs, AdmissionGate: gate,
		sem: make(chan struct{}, 1), active: make(map[types.UID]struct{}),
	}
	if complete, err := dispatcher.cleanupRecoveredTaskScopedRuntimeSession(ctx, task); err != nil || complete {
		t.Fatalf("stale-epoch cleanup = complete:%v err:%v, want pending without error", complete, err)
	}
	if got := deleteCalls.Load(); got != 0 {
		t.Fatalf("stale-epoch cleanup issued %d DELETE calls, want 0", got)
	}
	currentPool := &corev1alpha1.RuntimePool{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, currentPool); err != nil {
		t.Fatal(err)
	}
	currentPool.Status.ActiveInstance.ControllerEpoch = fence.Epoch
	if err := kubeClient.Update(ctx, currentPool); err != nil {
		t.Fatal(err)
	}
	if complete, err := dispatcher.cleanupRecoveredTaskScopedRuntimeSession(ctx, task); err == nil || complete {
		t.Fatalf("first cleanup = complete:%v err:%v, want incomplete error", complete, err)
	}
	if err := dispatcher.dispatchOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var recoveredTask corev1alpha1.Task
	projectionReady := false
	projectionID := standaloneTaskTerminalProjectionID(task, 1)
	for range 100 {
		if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, &recoveredTask); err != nil {
			t.Fatal(err)
		}
		if _, err := controlStore.GetOutboxProjection(ctx, projectionID); err == nil {
			projectionReady = true
		} else if !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		if deleteCalls.Load() == 2 && taskScopedRuntimeSessionCleanupComplete(&recoveredTask) && projectionReady {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := deleteCalls.Load(); got != 2 || !taskScopedRuntimeSessionCleanupComplete(&recoveredTask) || !projectionReady {
		t.Fatalf("dispatcher retry = DELETE calls:%d cleanupComplete:%v projectionReady:%v", got, taskScopedRuntimeSessionCleanupComplete(&recoveredTask), projectionReady)
	}
}

func TestRecoveredWriteSessionPublicationCleanupByRuntimeState(t *testing.T) {
	for _, test := range []struct {
		name              string
		state             harnessv2.RuntimeSessionState
		omitTaskDelivery  bool
		prepareOperations []string
		finalOperations   []string
	}{
		{
			name: "publication prepared", state: harnessv2.RuntimeSessionStatePublicationPrepared,
			prepareOperations: []string{"finalize"}, finalOperations: []string{"finalize", "delete"},
		},
		{
			name: "publication prepared before Task delivery projection", state: harnessv2.RuntimeSessionStatePublicationPrepared,
			omitTaskDelivery: true, prepareOperations: []string{"finalize"}, finalOperations: []string{"finalize", "delete"},
		},
		{name: "finalizing", state: harnessv2.RuntimeSessionStateFinalizing, finalOperations: []string{"delete"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRecoveredWriteCleanupFixture(t, recoveredWriteCleanupOptions{state: test.state})
			if test.omitTaskDelivery {
				fixture.task.Status.Delivery = nil
			}
			prepared, err := fixture.dispatcher.prepareRecoveredTaskScopedRuntimeSessionForSettlement(fixture.ctx, fixture.task.DeepCopy())
			if err != nil || !prepared {
				t.Fatalf("prepare for settlement = ready:%v err:%v, want ready", prepared, err)
			}
			operations, _, deletes := fixture.trace.snapshot()
			if fmt.Sprint(operations) != fmt.Sprint(test.prepareOperations) {
				t.Fatalf("pre-settlement operations = %v, want %v", operations, test.prepareOperations)
			}
			if len(deletes) != 0 {
				t.Fatalf("pre-settlement delete requests = %d, want 0", len(deletes))
			}
			complete, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, fixture.task.DeepCopy())
			if err != nil || !complete {
				t.Fatalf("cleanup = complete:%v err:%v, want complete", complete, err)
			}
			operations, finalizations, deletes := fixture.trace.snapshot()
			if fmt.Sprint(operations) != fmt.Sprint(test.finalOperations) {
				t.Fatalf("cleanup operations = %v, want %v", operations, test.finalOperations)
			}
			if len(deletes) != 1 {
				t.Fatalf("delete requests = %d, want 1", len(deletes))
			}
			if test.state == harnessv2.RuntimeSessionStatePublicationPrepared {
				if len(finalizations) != 1 {
					t.Fatalf("publication finalization requests = %d, want 1", len(finalizations))
				}
				request := finalizations[0]
				if request.WorkspaceDeltaID != harnessv2.WorkspaceDeltaID("delta-"+fixture.task.Status.Execution.PromptID) ||
					request.PublicationID != publicationIDForTaskUID(fixture.task, fixture.controlUID) ||
					request.PublicationGeneration != uint64(fixture.publication.Generation) ||
					request.PublicationVersion != uint64(fixture.publication.Version) ||
					request.TerminalState != harnessv2.PublicationTerminalVerifiedExact || request.TerminalReceiptDigest == "" {
					t.Fatalf("recovered publication finalization request = %#v", request)
				}
			} else if len(finalizations) != 0 {
				t.Fatalf("finalizing RuntimeSession was finalized again: %#v", finalizations)
			}
			fixture.assertCleanupReceipt(t)
		})
	}
}

//nolint:gocyclo // The two rows intentionally prove the full finalize/delete crash tail and retry boundary.
func TestRestoredWriteSessionCleanupUsesFrozenSourceUID(t *testing.T) {
	sourceUID := types.UID("cdcdcdcd-cdcd-cdcd-cdcd-cdcdcdcdcdcd")
	restoredUID := types.UID("abababab-abab-abab-abab-abababababab")
	for _, test := range []struct {
		name                   string
		omitPublication        bool
		abortFirstDelete       bool
		omitDeletedFromStatus  bool
		wantFirstComplete      bool
		wantFirstError         bool
		wantTerminalState      harnessv2.PublicationTerminalState
		setAbandonmentDelivery bool
	}{
		{
			name: "durable source publication", wantFirstComplete: true,
			wantTerminalState: harnessv2.PublicationTerminalVerifiedExact,
		},
		{
			name:            "source-bound abandonment survives lost delete response",
			omitPublication: true, abortFirstDelete: true, omitDeletedFromStatus: true,
			wantFirstError: true, wantTerminalState: harnessv2.PublicationTerminalDeliveryConflict,
			setAbandonmentDelivery: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRecoveredWriteCleanupFixture(t, recoveredWriteCleanupOptions{
				state: harnessv2.RuntimeSessionStatePublicationPrepared, sourceTaskUID: sourceUID,
				omitPublication: test.omitPublication, abortFirstDeleteResponse: test.abortFirstDelete,
				omitDeletedSessionFromStatus: test.omitDeletedFromStatus,
			})
			if fixture.task.UID != restoredUID || fixture.controlUID != sourceUID || fixture.task.Status.AgentExecutionBinding == nil ||
				fixture.task.Status.AgentExecutionBinding.Task.UID != sourceUID {
				t.Fatalf("restored fixture identity = uid:%q control:%q binding:%#v", fixture.task.UID, fixture.controlUID, fixture.task.Status.AgentExecutionBinding)
			}
			if test.setAbandonmentDelivery {
				fixture.task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
					State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
					Reason: "RestorePublicationMissing", Message: "source publication was not durably created",
				}
			}

			complete, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSessionForUID(
				fixture.ctx, fixture.task.DeepCopy(), sourceUID,
			)
			if complete != test.wantFirstComplete || (err != nil) != test.wantFirstError {
				t.Fatalf("first cleanup = complete:%v err:%v", complete, err)
			}
			var retryTask corev1alpha1.Task
			if getErr := fixture.kubeClient.Get(
				fixture.ctx, types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name}, &retryTask,
			); getErr != nil {
				t.Fatal(getErr)
			}
			if test.wantFirstError && retryTask.Status.Execution.RuntimeSessionCleanupDigest != "" {
				t.Fatalf("cleanup receipt persisted before delete response-loss reconciliation: %q", retryTask.Status.Execution.RuntimeSessionCleanupDigest)
			}
			complete, err = fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSessionForUID(fixture.ctx, &retryTask, sourceUID)
			if err != nil || !complete {
				t.Fatalf("idempotent cleanup retry = complete:%v err:%v", complete, err)
			}

			operations, finalizations, deletes := fixture.trace.snapshot()
			if fmt.Sprint(operations) != "[finalize delete]" || len(finalizations) != 1 || len(deletes) != 1 {
				t.Fatalf("restored cleanup operations = %v finalizations=%d deletes=%d", operations, len(finalizations), len(deletes))
			}
			finalizeRequest := finalizations[0]
			deleteRequest := deletes[0]
			if finalizeRequest.Metadata.TaskUID != harnessv2.TaskUID(sourceUID) ||
				deleteRequest.Metadata.TaskUID != harnessv2.TaskUID(sourceUID) ||
				finalizeRequest.Metadata.TaskUID == harnessv2.TaskUID(restoredUID) ||
				deleteRequest.Metadata.TaskUID == harnessv2.TaskUID(restoredUID) ||
				finalizeRequest.Metadata.TaskAttempt != 1 || deleteRequest.Metadata.TaskAttempt != 1 ||
				finalizeRequest.Metadata.PromptID != harnessv2.PromptID(fixture.task.Status.Execution.PromptID) ||
				deleteRequest.Metadata.PromptID != "" || finalizeRequest.Metadata.RequestDigest == "" ||
				deleteRequest.Metadata.RequestDigest == "" {
				t.Fatalf("restored cleanup mutation metadata = finalize:%#v delete:%#v", finalizeRequest.Metadata, deleteRequest.Metadata)
			}
			wantPublicationID := publicationIDForTaskUID(fixture.task, sourceUID)
			if finalizeRequest.PublicationID != wantPublicationID ||
				finalizeRequest.PublicationID == publicationIDForTask(fixture.task) ||
				finalizeRequest.TerminalState != test.wantTerminalState {
				t.Fatalf("restored finalization identity = %#v, want publication %q state %q", finalizeRequest, wantPublicationID, test.wantTerminalState)
			}
			if test.omitPublication {
				want, finalizationErr := runtimeSessionDeltaAbandonmentFinalizationForTaskUID(
					fixture.task, sourceUID, harnessv2.WorkspaceDeltaID("delta-"+fixture.task.Status.Execution.PromptID), *fixture.task.Status.Delivery,
				)
				if finalizationErr != nil {
					t.Fatal(finalizationErr)
				}
				wrong, finalizationErr := runtimeSessionDeltaAbandonmentFinalizationForTaskUID(
					fixture.task, restoredUID, harnessv2.WorkspaceDeltaID("delta-"+fixture.task.Status.Execution.PromptID), *fixture.task.Status.Delivery,
				)
				if finalizationErr != nil {
					t.Fatal(finalizationErr)
				}
				if finalizeRequest.TerminalReceiptDigest != want.TerminalReceiptDigest ||
					finalizeRequest.TerminalReceiptDigest == wrong.TerminalReceiptDigest {
					t.Fatalf("abandonment receipt digest = %q, want source %q and not restored %q", finalizeRequest.TerminalReceiptDigest, want.TerminalReceiptDigest, wrong.TerminalReceiptDigest)
				}
			}
			fixture.assertCleanupReceipt(t)
			var recovered corev1alpha1.Task
			if getErr := fixture.kubeClient.Get(
				fixture.ctx, types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name}, &recovered,
			); getErr != nil {
				t.Fatal(getErr)
			}
			if recovered.UID != restoredUID || recovered.Status.AgentExecutionBinding == nil ||
				recovered.Status.AgentExecutionBinding.Task.UID != sourceUID {
				t.Fatalf("cleanup changed restored/source identity: uid=%q binding=%#v", recovered.UID, recovered.Status.AgentExecutionBinding)
			}
		})
	}
}

func TestRecoveredWriteSessionDeleteResponseLossUsesStatusAbsenceReceipt(t *testing.T) {
	fixture := newRecoveredWriteCleanupFixture(t, recoveredWriteCleanupOptions{
		state: harnessv2.RuntimeSessionStateFinalizing, abortFirstDeleteResponse: true, omitDeletedSessionFromStatus: true,
	})
	complete, err := fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, fixture.task.DeepCopy())
	if err == nil || complete {
		t.Fatalf("first cleanup after lost DELETE response = complete:%v err:%v, want incomplete error", complete, err)
	}
	var afterLoss corev1alpha1.Task
	if err := fixture.kubeClient.Get(fixture.ctx, types.NamespacedName{Namespace: fixture.task.Namespace, Name: fixture.task.Name}, &afterLoss); err != nil {
		t.Fatal(err)
	}
	if afterLoss.Status.Execution == nil || afterLoss.Status.Execution.RuntimeSessionCleanupDigest != "" {
		t.Fatalf("cleanup receipt was persisted before deletion was proven: %#v", afterLoss.Status.Execution)
	}
	complete, err = fixture.dispatcher.cleanupRecoveredTaskScopedRuntimeSession(fixture.ctx, &afterLoss)
	if err != nil || !complete {
		t.Fatalf("status-absence recovery = complete:%v err:%v, want complete", complete, err)
	}
	operations, finalizations, deletes := fixture.trace.snapshot()
	if fmt.Sprint(operations) != "[delete]" || len(finalizations) != 0 || len(deletes) != 1 {
		t.Fatalf("response-loss recovery operations = %v finalizations=%d deletes=%d", operations, len(finalizations), len(deletes))
	}
	fixture.assertCleanupReceipt(t)
}

func TestACPDispatcherRecoveryAcceptsDeadLetteredStandaloneProjection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "dead-letter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "dead-letter")
	defer stopEpoch()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "dead-letter", UID: types.UID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
			Attempt: 1, PromptID: "prompt-dead-letter",
		}, Delivery: &corev1alpha1.TaskDeliveryStatus{
			State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeReadValidated,
		}},
	}
	attempt := &store.PromptAttempt{
		Key:            store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID},
		ExecutionState: store.PromptExecutionSucceeded, DeliveryState: store.PromptDeliveryReadValidated,
	}
	dispatcher := &ACPDispatcher{Store: controlStore, Epochs: epochs}
	if err := dispatcher.enqueueStandaloneTaskProjection(ctx, task, taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
		Phase: corev1alpha1.TaskPhaseSucceeded, Message: "ACP task completed",
		Execution: *task.Status.Execution.DeepCopy(), Delivery: task.Status.Delivery.DeepCopy(),
	}); err != nil {
		t.Fatal(err)
	}
	claimed, err := controlStore.ClaimOutboxProjections(ctx, store.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "dead-letter-worker", Limit: 1, LeaseDuration: time.Minute, Now: time.Now().UTC(),
	})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim projection: count=%d err=%v", len(claimed), err)
	}
	if _, err := controlStore.CompleteOutboxProjection(ctx, store.CompleteOutboxProjectionRequest{
		ID: claimed[0].ID, Fence: fence, ExpectedVersion: claimed[0].Version, LeaseOwner: "dead-letter-worker",
		OperationID: "dead-letter", OperationDigest: testControlDigestForDispatcher("dead-letter"),
		NewState: store.OutboxProjectionDeadLetter, LastError: "delivery attempts exhausted", UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	exists, err := dispatcher.validateExistingStandaloneTaskProjection(ctx, task, attempt)
	if err != nil || !exists {
		t.Fatalf("dead-letter projection validation = exists:%v err:%v", exists, err)
	}
}

func TestACPDispatcherRecoveryCreatesMissingStandaloneNonSuccessProjection(t *testing.T) {
	tests := []struct {
		name       string
		state      store.PromptExecutionState
		phase      corev1alpha1.TaskPhase
		outcome    corev1alpha1.TaskExecutionOutcome
		sessionRef bool
	}{
		{name: "failed", state: store.PromptExecutionFailed, phase: corev1alpha1.TaskPhaseFailed, outcome: corev1alpha1.TaskExecutionOutcomeFailed},
		{name: "cancelled", state: store.PromptExecutionCancelled, phase: corev1alpha1.TaskPhaseCancelled, outcome: corev1alpha1.TaskExecutionOutcomeCancelled},
		{name: "cancelled before session binding", state: store.PromptExecutionCancelled, phase: corev1alpha1.TaskPhaseCancelled, outcome: corev1alpha1.TaskExecutionOutcomeCancelled, sessionRef: true},
		{name: "outcome unknown", state: store.PromptExecutionOutcomeUnknown, phase: corev1alpha1.TaskPhaseFailed, outcome: corev1alpha1.TaskExecutionOutcomeOutcomeUnknown},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "missing-terminal.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			controlStore := sqlite.NewStore(db, "test")
			epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "missing-terminal-"+tc.name)
			defer stopEpoch()
			uid := types.UID(fmt.Sprintf("88888888-8888-8888-8888-%012d", i+1))
			promptID := "prompt-" + string(uid) + "-1"
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "missing-" + tc.name, UID: uid},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
					Execution: &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionState(tc.state), Outcome: tc.outcome, Attempt: 1, PromptID: promptID},
					Delivery:  &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested},
				},
			}
			if tc.sessionRef {
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "unbound-session", Create: true, Append: true}
			}
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
			dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: controlStore, Epochs: epochs}
			attempt := &store.PromptAttempt{
				Key:            store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: promptID},
				ExecutionState: tc.state, DeliveryState: store.PromptDeliveryNotRequested,
			}
			if err := dispatcher.recoverMissingStandaloneTerminalProjection(ctx, task, attempt); err != nil {
				t.Fatal(err)
			}
			projection, err := controlStore.GetOutboxProjection(ctx, standaloneTaskTerminalProjectionID(task, 1))
			if err != nil {
				t.Fatal(err)
			}
			var payload taskTerminalProjection
			if err := json.Unmarshal(projection.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Phase != tc.phase || payload.Execution.State != corev1alpha1.TaskExecutionState(tc.state) || payload.Execution.Outcome != tc.outcome || payload.Delivery == nil || payload.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested {
				t.Fatalf("terminal projection payload = %#v", payload)
			}
			updated := &corev1alpha1.Task{}
			if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, updated); err != nil {
				t.Fatal(err)
			}
			if updated.Status.Phase != tc.phase || updated.Status.Execution == nil || updated.Status.Execution.Outcome != tc.outcome {
				t.Fatalf("recovered Task status = %#v", updated.Status)
			}
		})
	}
}

func TestRecoveredTerminalDeliveryStatusPreservesTaskEvidence(t *testing.T) {
	transition := metav1.NewTime(time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC))
	task := &corev1alpha1.Task{Status: corev1alpha1.TaskStatus{Delivery: &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeReadValidated,
		StartingSHA: "740310bf8ecfbce4963628a51b6a11e26d7ee7be", LastTransitionTime: &transition,
	}}}
	attempt := &store.PromptAttempt{DeliveryState: store.PromptDeliveryReadValidated}
	status, err := (&ACPDispatcher{}).recoveredTerminalDeliveryStatus(context.Background(), task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if status.StartingSHA != task.Status.Delivery.StartingSHA || status.LastTransitionTime == nil || !status.LastTransitionTime.Equal(&transition) {
		t.Fatalf("recovered delivery evidence = %#v, want %#v", status, task.Status.Delivery)
	}
	if status == task.Status.Delivery {
		t.Fatal("recovered delivery status aliased Task status")
	}
}

//nolint:gocyclo // The restart-boundary assertions intentionally stay in one end-to-end scenario.
func TestACPDispatcherRecoveryResumesCommittedSessionTurnFinalization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	sqliteStore := sqlite.NewStore(db, "test")

	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	taskUID := types.UID("44444444-4444-4444-4444-444444444444")
	promptID := "prompt-" + string(taskUID) + "-1"
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "session-crash", UID: taskUID,
			Labels: map[string]string{acpRuntimeTaskPoolLabel: "pool"},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "persist this failed turn",
			SessionRef: &corev1alpha1.SessionReference{Name: "session-crash", Create: true, Append: true},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
			AgentExecutionBinding: testACPExecuteBindingForDispatcher(),
		},
	}
	rawClient := withControllerEpochLeaseUIDs(t, fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&corev1alpha1.Task{}, &corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{},
			&corev1alpha1.RuntimeSessionControl{}, &corev1alpha1.BranchClaim{}, &corev1alpha1.Publication{},
			&corev1alpha1.ExternalEffect{},
		).
		WithObjects(task).
		Build())
	controlStore, err := kubestore.NewComposite(rawClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	oldEpochs, stopOldEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "controller-old")
	oldFence, err := oldEpochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	continuity := newACPRecoveryContinuity(t, controlStore, sqliteStore, "session-crash-uid")
	control, err := continuity.EnsureSession(ctx, ACPEnsureSessionRequest{
		Namespace: "default", SessionName: "session-crash", SessionType: "task",
		ExpectedSessionUID: "session-crash-uid", Fence: oldFence, CreatedAt: time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptKey := store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(taskUID), Attempt: 1, PromptID: promptID}
	attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{Key: attemptKey, RequestDigest: testControlDigestForDispatcher("session-crash")}), oldFence)
	if err != nil {
		t.Fatal(err)
	}
	attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, store.PromptExecutionReserved, nil)
	lease, err := continuity.AcquireMutationLease(ctx, ACPAcquireSessionLeaseRequest{
		Session: *control, Fence: oldFence, TaskUID: string(taskUID), Attempt: 1, PromptID: promptID,
		PromptRequestDigest: attempt.RequestDigest, AcquiredAt: time.Date(2026, 7, 25, 12, 1, 0, 0, time.UTC),
		NamespaceUID: "default-namespace-uid", RuntimeIdentity: "codex",
		ConfigDigest: testControlDigestForDispatcher("session-crash-profile"),
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, store.PromptExecutionSessionStarting, lease)
	turn, err := continuity.OpenTurn(ctx, ACPOpenSessionTurnRequest{
		Lease: *lease, Fence: oldFence, PromptAttemptID: attempt.ID,
		PromptRequestDigest: attempt.RequestDigest, UserPrompt: task.Spec.Prompt,
		OpenedAt: time.Date(2026, 7, 25, 12, 2, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionPlanned, store.PromptExecutionSubmitting, store.PromptExecutionAccepted,
		store.PromptExecutionRunning, store.PromptExecutionFailed,
	} {
		attempt = transitionACPRecoveryAttempt(t, ctx, controlStore, oldFence, attempt, state, nil)
	}
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateFailed, Outcome: corev1alpha1.TaskExecutionOutcomeFailed,
		Attempt: 1, PromptID: promptID, RuntimePoolName: "pool", RuntimeInstanceID: "runtime-old",
		RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: lease.Key.LeaseGeneration,
		ControllerEpoch: oldFence.Epoch, RequestDigest: attempt.RequestDigest,
	}
	if err := rawClient.Status().Update(ctx, task); err != nil {
		t.Fatal(err)
	}

	failSessionStatusOnce := true
	failingClient := interceptor.NewClient(rawClient, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failSessionStatusOnce {
				if _, isSession := obj.(*corev1alpha1.RuntimeSessionControl); isSession {
					failSessionStatusOnce = false
					return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated controller crash"))
				}
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	failingStore, err := kubestore.NewComposite(failingClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	failingContinuity := newACPRecoveryContinuity(t, failingStore, sqliteStore, control.SessionUID)
	projectionPayload, err := json.Marshal(taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: 1,
		Phase: corev1alpha1.TaskPhaseFailed,
		Execution: corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateFailed, Outcome: corev1alpha1.TaskExecutionOutcomeFailed,
			Attempt: 1, PromptID: promptID, Reason: "PromptFailed", Message: "prompt failed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalizedAt := time.Date(2026, 7, 25, 12, 3, 0, 0, time.UTC)
	_, err = failingContinuity.FinalizeOutcomeMarker(ctx, ACPFinalizeOutcomeMarkerRequest{
		SessionTurn: *turn, Fence: oldFence, Kind: "Failed", Reason: "prompt failed",
		Projection: ACPFinalizationProjection{
			ProjectionKind: "TaskTerminalStatus", Payload: projectionPayload, AvailableAt: finalizedAt,
		},
		FinalizedAt: finalizedAt,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("partial finalization error = %v, want simulated conflict", err)
	}
	committedTurn, err := sqliteStore.GetSessionTurn(ctx, turn.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if committedTurn.State != store.SessionTurnFinalized || committedTurn.FinalizedAt == nil {
		t.Fatalf("SQLite turn was not finalized before crash: %#v", committedTurn)
	}
	deferred, err := sqliteStore.GetOutboxProjection(ctx, committedTurn.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	if deferred.AvailableAt.Year() != 9999 {
		t.Fatalf("outbox was activated before Kubernetes completion: %s", deferred.AvailableAt)
	}

	stopOldEpoch()
	restartedStore, err := kubestore.NewComposite(rawClient, "orka-system", sqliteStore)
	if err != nil {
		t.Fatal(err)
	}
	newEpochs, stopNewEpoch := startACPRecoveryEpochManager(t, ctx, restartedStore, "controller-new")
	defer stopNewEpoch()
	newContinuity := newACPRecoveryContinuity(t, restartedStore, sqliteStore, control.SessionUID)
	dispatcher := &ACPDispatcher{
		Client: rawClient, APIReader: rawClient, Store: restartedStore, ResultStore: sqliteStore,
		Epochs: newEpochs, Sessions: newContinuity,
	}
	if err := dispatcher.recoverStaleAttempts(ctx); err != nil {
		t.Fatal(err)
	}
	if !dispatcher.finalizedSessionTurnKnown(task.UID, turn.Turn.ID) {
		t.Fatal("successful recovered finalization tail was not indexed for subsequent dispatcher scans")
	}

	recoveredTurn, err := sqliteStore.GetSessionTurn(ctx, turn.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredTurn.FinalizationDigest != committedTurn.FinalizationDigest || recoveredTurn.FinalizedAt == nil ||
		!recoveredTurn.FinalizedAt.Equal(*committedTurn.FinalizedAt) ||
		!recoveredTurn.ProjectionAvailableAt.Equal(committedTurn.ProjectionAvailableAt) {
		t.Fatalf("recovery changed persisted finalization identity: before=%#v after=%#v", committedTurn, recoveredTurn)
	}
	recoveredControl, err := restartedStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredControl.Lease != nil || recoveredControl.LastOperationID != "finalize:"+turn.Turn.ID || recoveredControl.LastOperationDigest != committedTurn.FinalizationDigest {
		t.Fatalf("SessionControl finalization tail was not completed: %#v", recoveredControl)
	}
	activated, err := sqliteStore.GetOutboxProjection(ctx, committedTurn.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	if !activated.AvailableAt.Equal(committedTurn.ProjectionAvailableAt) || activated.PayloadDigest != committedTurn.ProjectionDigest {
		t.Fatalf("outbox activation did not reuse persisted receipt: %#v", activated)
	}
	transcript, err := sqliteStore.LoadTranscript(ctx, control.Namespace, control.SessionName, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 2 {
		t.Fatalf("recovery duplicated transcript entries: %d", len(transcript))
	}
}

type recoveredWriteCleanupOptions struct {
	state                        harnessv2.RuntimeSessionState
	abortFirstDeleteResponse     bool
	omitDeletedSessionFromStatus bool
	sourceTaskUID                types.UID
	omitPublication              bool
}

type recoveredWriteCleanupTrace struct {
	mu            sync.Mutex
	operations    []string
	finalizations []harnessv2.FinalizeRuntimeSessionPublicationRequest
	deletes       []harnessv2.DeleteRuntimeSessionRequest
	finalized     bool
	deleted       bool
}

func (t *recoveredWriteCleanupTrace) snapshot() (
	[]string,
	[]harnessv2.FinalizeRuntimeSessionPublicationRequest,
	[]harnessv2.DeleteRuntimeSessionRequest,
) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.operations...),
		append([]harnessv2.FinalizeRuntimeSessionPublicationRequest(nil), t.finalizations...),
		append([]harnessv2.DeleteRuntimeSessionRequest(nil), t.deletes...)
}

type recoveredWriteCleanupFixture struct {
	ctx         context.Context
	task        *corev1alpha1.Task
	controlUID  types.UID
	publication *store.Publication
	kubeClient  client.Client
	dispatcher  *ACPDispatcher
	trace       *recoveredWriteCleanupTrace
}

func (f *recoveredWriteCleanupFixture) assertCleanupReceipt(t *testing.T) {
	t.Helper()
	var recovered corev1alpha1.Task
	if err := f.kubeClient.Get(f.ctx, types.NamespacedName{Namespace: f.task.Namespace, Name: f.task.Name}, &recovered); err != nil {
		t.Fatal(err)
	}
	if recovered.Status.Execution == nil {
		t.Fatal("recovered task execution status is missing")
	}
	wantDigest, err := taskScopedRuntimeSessionCleanupDigest(
		f.controlUID, recovered.Status.Execution.Attempt, recovered.Status.Execution.RuntimeInstanceID,
		recovered.Status.Execution.RuntimeSessionUID, recovered.Status.Execution.RuntimeSessionGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status.Execution.RuntimeSessionCleanupDigest != wantDigest ||
		!taskScopedRuntimeSessionCleanupCompleteForUID(&recovered, f.controlUID) {
		t.Fatalf("write SessionRef cleanup receipt = %q, want %q", recovered.Status.Execution.RuntimeSessionCleanupDigest, wantDigest)
	}
}

type recoveredPublicationStore struct {
	store.DurableControlStore
	publication    *store.Publication
	sessionControl *store.SessionControl
}

func (s *recoveredPublicationStore) GetSessionControl(ctx context.Context, namespace, sessionName string) (*store.SessionControl, error) {
	if s.sessionControl != nil && namespace == s.sessionControl.Namespace && sessionName == s.sessionControl.SessionName {
		copy := *s.sessionControl
		return &copy, nil
	}
	return s.DurableControlStore.GetSessionControl(ctx, namespace, sessionName)
}

func (s *recoveredPublicationStore) GetPublication(ctx context.Context, id string) (*store.Publication, error) {
	if s.publication != nil && id == s.publication.ID {
		copy := *s.publication
		return &copy, nil
	}
	return s.DurableControlStore.GetPublication(ctx, id)
}

//nolint:gocyclo // This fixture keeps the authenticated recovery boundary explicit and auditable.
func newRecoveredWriteCleanupFixture(t *testing.T, options recoveredWriteCleanupOptions) *recoveredWriteCleanupFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "recovered-write-cleanup.db"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	t.Cleanup(cancel)
	controlStore := sqlite.NewStore(db, "test")
	epochs, stopEpoch := startACPRecoveryEpochManager(t, ctx, controlStore, "recovered-write-cleanup")
	t.Cleanup(stopEpoch)
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := harnessProfileForTest()
	profile.Model = acpTestModel
	profile.WorkspaceIntent = harnessv2.WorkspaceIntentWrite
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	trace := &recoveredWriteCleanupTrace{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.CapabilitiesPath:
			writeDispatcherJSON(w, harnessv2.CapabilitiesResponse{
				Protocol: harnessv2.ProtocolVersion, Transport: "http+ndjson", ACPVersion: harnessv2.ACPProfileV1,
				RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				AdapterDigests: profile.AdapterDigests, Limits: harnessv2.DefaultProtocolLimits(),
				Provider: harnessv2.ProviderCapabilities{
					ProviderKinds: []string{profile.ProviderKind}, Models: []string{profile.Model},
					SupportsCancel: true, SupportsPermissions: true, SupportsTools: true,
				},
				WorkspaceGovernance:             harnessv2.StrictWorkspaceGovernanceCapabilities(),
				SupportsDrain:                   true,
				SupportsPublicationFinalization: true,
			})
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath:
			now := time.Now().UTC()
			trace.mu.Lock()
			deleted := trace.deleted
			finalized := trace.finalized
			trace.mu.Unlock()
			sessions := []harnessv2.RuntimeSessionStatus(nil)
			resident := uint32(0)
			if !deleted || !options.omitDeletedSessionFromStatus {
				state := options.state
				if finalized && state == harnessv2.RuntimeSessionStatePublicationPrepared {
					state = harnessv2.RuntimeSessionStateFinalizing
				}
				sessions = []harnessv2.RuntimeSessionStatus{{
					RuntimeSessionID: "runtime-session-recovered-write", RuntimeSessionUID: "session-recovered-write",
					Generation: 1, State: state,
					ReservedForFinalization: state == harnessv2.RuntimeSessionStateFinalizing,
					LastTransitionAt:        now,
				}}
				resident = 1
			}
			writeDispatcherJSON(w, harnessv2.StatusResponse{
				Protocol: harnessv2.ProtocolVersion,
				Fence: harnessv2.Fence{
					RuntimeInstanceID: "pod-uid.boot-id", SupervisorBootID: "boot-id", ControllerEpoch: uint64(fence.Epoch),
					RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: profileDigest,
					ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				},
				Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true},
				Sessions: sessions, Pressure: harnessv2.PressureMetadata{ResidentSessions: resident}, Timestamp: now,
			})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/publication-finalization"):
			var request harnessv2.FinalizeRuntimeSessionPublicationRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode recovered publication finalization: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			trace.mu.Lock()
			trace.operations = append(trace.operations, "finalize")
			trace.finalizations = append(trace.finalizations, request)
			trace.finalized = true
			trace.mu.Unlock()
			now := time.Now().UTC()
			writeDispatcherJSON(w, harnessv2.FinalizeRuntimeSessionPublicationResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				Session: harnessv2.RuntimeSessionDescriptor{
					RuntimeSessionID:  harnessv2.RuntimeSessionID(runtimeSessionID(request.Metadata.Fence)),
					RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, Generation: request.Metadata.Fence.RuntimeSessionGeneration,
					RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID, SupervisorBootID: request.Metadata.Fence.SupervisorBootID,
					RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest, State: harnessv2.RuntimeSessionStateFinalizing,
					ProviderSessionID: "provider-session",
					WorkspaceBaseline: harnessv2.WorkspaceBaseline{
						RepositoryIdentity: "github.com/orka-agents/orka", Revision: strings.Repeat("1", 40),
						TreeDigest: "sha256:" + strings.Repeat("2", 64),
					},
					CreatedAt: now.Add(-time.Minute), LastTransitionAt: now,
				},
				Finalization: harnessv2.PublicationFinalizationReceipt{
					WorkspaceDeltaID: request.WorkspaceDeltaID, PublicationID: request.PublicationID,
					PublicationGeneration: request.PublicationGeneration, PublicationVersion: request.PublicationVersion,
					TerminalState: request.TerminalState, TerminalReceiptDigest: request.TerminalReceiptDigest, AppliedAt: now,
				},
			})
		case r.Method == http.MethodDelete:
			var request harnessv2.DeleteRuntimeSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode recovered delete: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			trace.mu.Lock()
			trace.operations = append(trace.operations, "delete")
			trace.deletes = append(trace.deletes, request)
			deleteCall := len(trace.deletes)
			trace.deleted = true
			trace.mu.Unlock()
			if options.abortFirstDeleteResponse && deleteCall == 1 {
				panic(http.ErrAbortHandler)
			}
			writeDispatcherJSON(w, harnessv2.DeleteRuntimeSessionResponse{
				Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				State:     harnessv2.RuntimeSessionStateDeleted,
				Tombstone: testDeleteTombstone(request, time.Now().UTC()),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "recovered-write", UID: types.UID("abababab-abab-abab-abab-abababababab")},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAgent,
			SessionRef: &corev1alpha1.SessionReference{Name: "recovered-write", Create: true, Append: true},
			Workspace:  &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: "prompt-recovered-write", RuntimePoolName: "pool", RuntimePoolUID: "pool-uid",
				RuntimeInstanceID: "pod-uid.boot-id", RuntimeSessionUID: "session-recovered-write", RuntimeSessionGeneration: 1,
				ControllerEpoch: fence.Epoch, RequestDigest: testControlDigestForDispatcher("recovered-write"),
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
				StartingSHA: strings.Repeat("1", 40), ExpectedCommitSHA: strings.Repeat("3", 40), VerifiedRemoteSHA: strings.Repeat("3", 40),
			},
		},
	}
	controlUID := task.UID
	if options.sourceTaskUID != "" {
		controlUID = options.sourceTaskUID
		binding := testACPExecuteBindingForDispatcher()
		binding.Task.UID = controlUID
		task.Status.AgentExecutionBinding = binding
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "pool", UID: types.UID("pool-uid"), Generation: 1},
		Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
			Image:   "docker.io/example/acp@sha256:" + strings.Repeat("a", 64),
			Profile: RuntimePoolProfileFromPlan(ACPRuntimePlan{Profile: profile, Digest: profileDigest}),
		}},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: parsed.Host, PodUID: "pod-uid", BootID: "boot-id",
			RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID, ControllerEpoch: fence.Epoch,
			ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(profileDigest), ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: fmt.Sprintf("pool-auth-e%d", pool.Status.ActiveInstance.ControllerEpoch), Labels: map[string]string{
			runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
		}},
		Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, pool, secret).Build()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	var publication *store.Publication
	if !options.omitPublication {
		publication = &store.Publication{
			ID: publicationIDForTaskUID(task, controlUID), Namespace: task.Namespace, Generation: 1, Version: 7,
			TaskUID: string(controlUID), Attempt: 1, PromptID: task.Status.Execution.PromptID,
			State: store.PublicationVerifiedExact,
			PreparedReceipt: &store.PreparedPublicationReceipt{
				OperationID: "prepare", RequestDigest: testControlDigestForDispatcher("recovered-prepare"),
				TreeSHA: strings.Repeat("2", 40), CommitSHA: strings.Repeat("3", 40),
				ManifestDigest: "sha256:" + strings.Repeat("4", 64), PreparedAt: now.Add(-2 * time.Minute),
			},
			PublishReceipt: &store.PublishOperationReceipt{
				OperationID: "publish", RequestDigest: testControlDigestForDispatcher("recovered-publish"),
				ExpectedCommitSHA: strings.Repeat("3", 40), PublishedAt: now.Add(-time.Minute),
			},
			VerificationReceipt: &store.PublicationVerificationReceipt{
				OperationID: "verify", RequestDigest: testControlDigestForDispatcher("recovered-verify"),
				Outcome: store.PublicationVerifiedExact, ExpectedCommitSHA: strings.Repeat("3", 40),
				ObservedRemote: store.RemoteRefState{SHA: strings.Repeat("3", 40)}, VerifiedAt: now,
			},
		}
	}
	publicationStore := &recoveredPublicationStore{DurableControlStore: controlStore, publication: publication}
	dispatcher := &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: publicationStore, Epochs: epochs}
	return &recoveredWriteCleanupFixture{
		ctx: ctx, task: task, controlUID: controlUID, publication: publication,
		kubeClient: kubeClient, dispatcher: dispatcher, trace: trace,
	}
}

func transitionACPRecoveryAttempt(
	t *testing.T,
	ctx context.Context,
	controlStore store.PromptAttemptStore,
	fence store.ControllerEpochFence,
	attempt *store.PromptAttempt,
	to store.PromptExecutionState,
	lease *ACPSessionLease,
) *store.PromptAttempt {
	t.Helper()
	transition := store.PromptAttemptExecutionTransition{
		ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
		NewState: to, OperationID: "recovery-" + string(to) + "-" + strconv.FormatInt(attempt.Version, 10),
		OperationDigest: testControlDigestForDispatcher("recovery-" + string(to) + "-" + strconv.FormatInt(attempt.Version, 10)),
		UpdatedAt:       time.Now().UTC(),
	}
	if lease != nil {
		transition.SessionUID = lease.Key.SessionUID
		transition.SessionLeaseGeneration = lease.Key.LeaseGeneration
		transition.RuntimeInstanceID = "runtime-old"
	}
	updated, err := controlStore.TransitionPromptAttemptExecution(ctx, transition)
	if err != nil {
		t.Fatalf("transition PromptAttempt to %s: %v", to, err)
	}
	return updated
}

func newACPRecoveryContinuity(t *testing.T, controls store.DurableControlStore, transcripts store.SessionStore, sessionUID string) *ACPSessionContinuity {
	t.Helper()
	lineages, _ := transcripts.(store.SessionLineageStore)
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controls, Transcripts: transcripts,
		Publications: controls, BranchClaims: controls,
		Lineages:      lineages,
		NewSessionUID: func() (string, error) { return sessionUID, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return continuity
}

func startACPRecoveryEpochManager(t *testing.T, ctx context.Context, epochStore store.ControllerEpochStore, holder string) (*ControllerEpochManager, func()) {
	t.Helper()
	manager := NewControllerEpochManager(epochStore, holder)
	epochCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- manager.Start(epochCtx) }()
	if _, err := manager.CurrentFence(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	return manager, func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop epoch manager: %v", err)
		}
	}
}

type recoveryFixture struct {
	ctx          context.Context
	cancel       context.CancelFunc
	db           *sql.DB
	epochCancel  context.CancelFunc
	epochDone    chan error
	fence        store.ControllerEpochFence
	controlStore *sqlite.Store
	kubeClient   client.Client
	dispatcher   *ACPDispatcher
	attemptID    string
}

func newACPRecoveryFixture(t *testing.T, target store.PromptExecutionState) *recoveryFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	controlStore := sqlite.NewStore(db, "test")
	first := NewControllerEpochManager(controlStore, "controller-old")
	oldCtx, stopOld := context.WithCancel(context.Background())
	oldDone := make(chan error, 1)
	go func() { oldDone <- first.Start(oldCtx) }()
	oldFence, err := first.CurrentFence(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}

	uid := types.UID("33333333-3333-3333-3333-333333333333")
	promptID := "prompt-" + string(uid) + "-1"
	key := store.PromptAttemptKey{Namespace: "default", TaskUID: string(uid), Attempt: 1, PromptID: promptID}
	attemptID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: testControlDigestForDispatcher("recovery"),
	}), oldFence)
	if err != nil {
		t.Fatal(err)
	}
	path := []store.PromptExecutionState{store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned}
	if target == store.PromptExecutionAccepted || target == store.PromptExecutionRunning ||
		target == store.PromptExecutionSettling || target == store.PromptExecutionSucceeded {
		path = append(path, store.PromptExecutionSubmitting, store.PromptExecutionAccepted)
	}
	if target == store.PromptExecutionRunning || target == store.PromptExecutionSettling || target == store.PromptExecutionSucceeded {
		path = append(path, store.PromptExecutionRunning)
	}
	if target == store.PromptExecutionSettling || target == store.PromptExecutionSucceeded {
		path = append(path, store.PromptExecutionSettling)
	}
	if target == store.PromptExecutionSucceeded {
		path = append(path, store.PromptExecutionSucceeded)
	}
	for _, next := range path {
		if attempt.ExecutionState == target {
			break
		}
		attempt, err = controlStore.TransitionPromptAttemptExecution(ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: oldFence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState, NewState: next,
			OperationID: "recover-" + string(next), OperationDigest: testControlDigestForDispatcher("recover-" + string(next)), UpdatedAt: time.Now().UTC(),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	stopOld()
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}

	second := NewControllerEpochManager(controlStore, "controller-new")
	epochCtx, epochCancel := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- second.Start(epochCtx) }()
	fence, err := second.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "task", UID: uid, Labels: map[string]string{acpRuntimeTaskPoolLabel: "pool"}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "test"},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
			AgentExecutionBinding: testACPExecuteBindingForDispatcher(), Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionState(target), Attempt: 1, PromptID: promptID, RuntimePoolName: "pool",
				ControllerEpoch: oldFence.Epoch, RequestDigest: attempt.RequestDigest,
			}},
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	return &recoveryFixture{
		ctx: ctx, cancel: cancel, db: db, epochCancel: epochCancel, epochDone: epochDone, fence: fence,
		controlStore: controlStore, kubeClient: kubeClient,
		dispatcher: &ACPDispatcher{Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore, Epochs: second},
		attemptID:  attemptID,
	}
}

func (f *recoveryFixture) close(t *testing.T) {
	t.Helper()
	f.epochCancel()
	if err := <-f.epochDone; err != nil {
		t.Fatal(err)
	}
	f.cancel()
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveredTerminalDeliveryStatusUsesTaskReceiptWithoutPublication(t *testing.T) {
	storeValue, _, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "recovery-no-publication.db"))
	defer closeStore()
	dispatcher := &ACPDispatcher{Store: storeValue}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "no-publication", UID: types.UID("no-publication-uid")},
		Status: corev1alpha1.TaskStatus{
			Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-no-publication"},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
				Reason: "BranchMoved", Message: "branch moved before publication",
			},
		},
	}
	attempt := &store.PromptAttempt{DeliveryState: store.PromptDeliveryConflict}
	status, err := dispatcher.recoveredTerminalDeliveryStatus(context.Background(), task, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if status.Outcome != corev1alpha1.TaskDeliveryOutcomeDeliveryConflict || status.Reason != task.Status.Delivery.Reason {
		t.Fatalf("status = %#v", status)
	}
}
