package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2eventjournal "github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
)

func TestACPDispatcherFailureStatusOmitsRuntimeDiagnostics(t *testing.T) {
	const canary = "fixture-failure-status-canary"
	for _, test := range []struct {
		name         string
		historyTitle string
		code         string
		message      string
		wantDetail   bool
	}{
		{
			name: "split diagnostic", historyTitle: "pw", code: "d", message: canary,
		},
		{
			name: "URL-empty code", historyTitle: "pwd", code: "?query=fixture-value", message: canary,
		},
		{
			name: "benign diagnostic stays in journal", code: "provider_upstream_error",
			message: "provider quota exhausted", wantDetail: true,
		},
	} {
		for _, recoverTerminal := range []bool{false, true} {
			mode := "live"
			if recoverTerminal {
				mode = "recovery"
			}
			t.Run(test.name+"/"+mode, func(t *testing.T) {
				fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
				defer fixture.close(t)
				task := configureRecoveryJournalIdentity(t, fixture)
				terminal, mapped := appendFailureStatusJournal(t, fixture, task, test.historyTitle, test.code, test.message)
				if strings.Contains(mapped.Summary+string(mapped.Content), canary) {
					t.Fatal("journal exposed the split diagnostic canary")
				}
				if test.wantDetail && !strings.Contains(mapped.Summary, test.message) {
					t.Fatal("journal lost the benign runtime diagnostic")
				}
				if recoverTerminal {
					if err := fixture.dispatcher.recoverStaleAttempts(fixture.ctx); err != nil {
						t.Fatal(err)
					}
				} else {
					for range 2 {
						if err := fixture.dispatcher.finishNonSuccess(
							fixture.ctx, task.DeepCopy(), fixture.attemptID, fixture.fence, nil, terminal,
						); err != nil {
							t.Fatal(err)
						}
					}
				}
				assertSafeFailureStatusProjection(t, fixture, task)
			})
		}
	}
}

func TestACPDispatcherSyntheticFailureUsesSafeStatus(t *testing.T) {
	fixture := newACPRecoveryFixture(t, store.PromptExecutionRunning)
	defer fixture.close(t)
	task := configureRecoveryJournalIdentity(t, fixture)
	if err := fixture.dispatcher.finishNonSuccess(
		fixture.ctx, task, fixture.attemptID, fixture.fence, nil, harnessv2.Event{Type: harnessv2.EventFailed},
	); err != nil {
		t.Fatal(err)
	}
	assertSafeFailureStatusProjection(t, fixture, task)
}

func appendFailureStatusJournal(
	t *testing.T,
	fixture *recoveryFixture,
	task *corev1alpha1.Task,
	historyTitle, code, message string,
) (harnessv2.Event, *store.ExecutionEvent) {
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
			Lease:      harnessv2.PromptLease{Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
			ACPVersion: harnessv2.ACPProfileV1,
		},
	}
	state, err := (v2eventjournal.Journal{
		EventStore: fixture.controlStore, MapContext: mappedPromptRecoveryContext(task),
	}).Open(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := state.AppendPromptLifecycleIfNew(fixture.ctx, accepted); err != nil {
		t.Fatal(err)
	}
	if historyTitle != "" {
		update := harnessv2.Event{
			Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventUpdate, Identity: accepted.Identity,
			Update: &harnessv2.UpdateEvent{
				Kind: harnessv2.UpdateToolCallUpdate,
				ToolCall: &harnessv2.ToolCallUpdate{
					ToolCallID: "failure-status-history", Title: historyTitle, Kind: "shell",
					Status: harnessv2.ToolCallStatusCompleted,
				},
			},
		}
		update.Identity.Sequence = 2
		if _, _, err := state.AppendUpdateIfNew(fixture.ctx, update); err != nil {
			t.Fatal(err)
		}
	}
	terminal := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventFailed, Identity: accepted.Identity,
		Failed: &harnessv2.FailedEvent{StopReason: harnessv2.ACPStopReasonRefusal, Code: code, Message: message},
	}
	terminal.Identity.Sequence = 3
	mapped, isNew, err := state.AppendPromptLifecycleIfNew(fixture.ctx, terminal)
	if err != nil || !isNew || mapped == nil {
		t.Fatalf("append failure diagnostic: new=%t, err=%v", isNew, err)
	}
	return terminal, mapped
}

func assertSafeFailureStatusProjection(t *testing.T, fixture *recoveryFixture, task *corev1alpha1.Task) {
	t.Helper()
	const wantMessage = "prompt failed"
	attempt, err := fixture.controlStore.GetPromptAttempt(fixture.ctx, fixture.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ExecutionState != store.PromptExecutionFailed || attempt.TerminalReason != "PromptFailed" ||
		attempt.OutcomeMarker != wantMessage {
		t.Error("durable failure classification contains runtime diagnostic text or changed failure state")
	}
	projection, err := fixture.controlStore.GetOutboxProjection(
		fixture.ctx, standaloneTaskTerminalProjectionID(task, task.Status.Execution.Attempt),
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Message != wantMessage || payload.Execution.Message != wantMessage {
		t.Error("durable Task projection contains runtime diagnostic text")
	}
	projector := ACPOutboxProjector{
		Client: fixture.kubeClient, Store: fixture.controlStore, Epochs: fixture.dispatcher.Epochs, WorkerID: "failure-status-test",
	}
	if err := projector.projectOnce(fixture.ctx); err != nil {
		t.Fatal(err)
	}
	updated := &corev1alpha1.Task{}
	if err := fixture.kubeClient.Get(fixture.ctx, clientObjectKey(task), updated); err != nil {
		t.Fatal(err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed || updated.Status.Execution == nil ||
		updated.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
		updated.Status.Execution.Outcome != corev1alpha1.TaskExecutionOutcomeFailed ||
		updated.Status.Execution.Reason != "PromptFailed" || updated.Status.Message != wantMessage ||
		updated.Status.Execution.Message != wantMessage {
		t.Error("Task failure status contains runtime diagnostic text or changed failure classification")
	}
	reconciler := TaskReconciler{Client: fixture.kubeClient, ExecutionEventStore: fixture.controlStore}
	if !reconciler.recordTerminalTaskLifecycleEventIfMissing(fixture.ctx, updated) {
		t.Fatal("record terminal Task event")
	}
	listed, err := fixture.controlStore.ListExecutionEvents(fixture.ctx, store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name,
		EventTypes: []string{events.ExecutionEventTypeTaskFailed}, Limit: 10,
	})
	if err != nil || len(listed) != 1 {
		t.Fatalf("terminal Task event count = %d, err=%v", len(listed), err)
	}
	if listed[0].Summary != wantMessage {
		t.Error("TaskFailed event reintroduced runtime diagnostic text")
	}
}
