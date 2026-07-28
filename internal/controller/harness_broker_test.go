package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

func TestHarnessBrokeredWriteReplayFindsCompletedResultAfterDefaultPage(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"data":{"dispatched":true}}`))
	}))
	defer toolServer.Close()

	task, agent, tool := harnessBrokerWriteTestObjects(toolServer.URL)
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := brokeredWriteFrame()

	_, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err == nil {
		t.Fatal("first handleHarnessBrokeredToolCall() error = nil, want approval pending")
	}
	approvalID := brokeredApprovalIDForTest(t, r, task)
	appendApprovalDecisionForTest(t, r, task, approvalID, events.ExecutionEventTypeApprovalApproved)
	appendHarnessBrokerLedgerFillers(t, r, task, 101)

	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil || result.Error != nil {
		t.Fatalf("approved handleHarnessBrokeredToolCall() = %#v, %v", result, err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions after approved call = %d, want 1", executions.Load())
	}

	result, err = r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil || result.Error != nil {
		t.Fatalf("replay handleHarnessBrokeredToolCall() = %#v, %v", result, err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions after replay = %d, want 1 (completed ledger result must prevent duplicate side effect)", executions.Load())
	}
}

func TestHarnessBrokeredWriteFindsUnresolvedStartedExecutionAfterMaxPage(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer toolServer.Close()

	task, agent, tool := harnessBrokerWriteTestObjects(toolServer.URL)
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := brokeredWriteFrame()

	_, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err == nil {
		t.Fatal("first handleHarnessBrokeredToolCall() error = nil, want approval pending")
	}
	approvalID := brokeredApprovalIDForTest(t, r, task)
	appendApprovalDecisionForTest(t, r, task, approvalID, events.ExecutionEventTypeApprovalApproved)
	appendHarnessBrokerLedgerFillers(t, r, task, store.MaxExecutionEventLimit+1)

	idempotencyKey := harness.ToolRequestIdempotencyKey(frame.RuntimeSessionID, frame.TurnID, frame.ToolCallID)
	argsDigest, err := approvals.TargetArgsDigest(frame.Content)
	if err != nil {
		t.Fatalf("TargetArgsDigest: %v", err)
	}
	started := harness.ToolCallResult{IdempotencyKey: idempotencyKey, Approved: true}
	if err := r.recordHarnessBrokeredToolEvent(
		context.Background(),
		task,
		frame,
		events.ExecutionEventTypeToolCallStarted,
		"brokered write tool execution started",
		brokeredToolEventContent(started, map[string]any{
			"targetArgsDigest": argsDigest,
			"executionState":   "started",
		}),
	); err != nil {
		t.Fatalf("record unresolved brokered execution: %v", err)
	}

	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "tool_execution_outcome_unknown" {
		t.Fatalf("result = %#v, want tool_execution_outcome_unknown", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("executions = %d, want fail-closed without duplicate side effect", executions.Load())
	}
}

func TestPreviousHarnessBrokeredToolResultFailsClosedAcrossPages(t *testing.T) {
	tests := []struct {
		name           string
		priorEntries   int
		recordedTool   string
		recordedDigest string
		wantCode       string
	}{
		{
			name:         "tool changed after default page",
			priorEntries: 101,
			recordedTool: "different_tool",
			wantCode:     "tool_call_id_reused",
		},
		{
			name:           "arguments changed after max page",
			priorEntries:   store.MaxExecutionEventLimit + 1,
			recordedTool:   "dispatch_work_order",
			recordedDigest: "sha256:different",
			wantCode:       "tool_call_arguments_changed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task, _, _ := harnessBrokerWriteTestObjects("http://tool.invalid")
			r := newUnitReconciler(newTestScheme(), task)
			frame := brokeredWriteFrame()
			idempotencyKey := harness.ToolRequestIdempotencyKey(frame.RuntimeSessionID, frame.TurnID, frame.ToolCallID)
			argsDigest, err := approvals.TargetArgsDigest(frame.Content)
			if err != nil {
				t.Fatalf("TargetArgsDigest: %v", err)
			}
			appendHarnessBrokerLedgerFillers(t, r, task, tt.priorEntries)

			recordedDigest := tt.recordedDigest
			if recordedDigest == "" {
				recordedDigest = argsDigest
			}
			recordedFrame := frame
			recordedFrame.ToolName = tt.recordedTool
			recorded := harness.ToolCallResult{IdempotencyKey: idempotencyKey, Approved: true}
			if err := r.recordHarnessBrokeredToolEvent(
				context.Background(),
				task,
				recordedFrame,
				events.ExecutionEventTypeToolCallCompleted,
				"brokered write tool call completed",
				brokeredToolEventContent(recorded, map[string]any{"targetArgsDigest": recordedDigest}),
			); err != nil {
				t.Fatalf("record completed brokered execution: %v", err)
			}

			result, ok, err := r.previousHarnessBrokeredToolResult(context.Background(), task, frame, idempotencyKey, argsDigest)
			if err != nil {
				t.Fatalf("previousHarnessBrokeredToolResult() error = %v", err)
			}
			if !ok || result.Error == nil || result.Error.Code != tt.wantCode {
				t.Fatalf("previousHarnessBrokeredToolResult() = (%#v, %t), want fail-closed code %q", result, ok, tt.wantCode)
			}
		})
	}
}

func TestPreviousHarnessBrokeredToolResultFailsClosedForEmptyReferencedFailure(t *testing.T) {
	task, _, _ := harnessBrokerWriteTestObjects("http://tool.invalid")
	r := newUnitReconciler(newTestScheme(), task)
	frame := brokeredWriteFrame()
	idempotencyKey := harness.ToolRequestIdempotencyKey(frame.RuntimeSessionID, frame.TurnID, frame.ToolCallID)
	argsDigest, err := approvals.TargetArgsDigest(frame.Content)
	if err != nil {
		t.Fatalf("TargetArgsDigest: %v", err)
	}

	completed := harness.ToolCallResult{IdempotencyKey: idempotencyKey, Approved: true}
	if err := r.recordHarnessBrokeredToolEvent(
		context.Background(),
		task,
		frame,
		events.ExecutionEventTypeToolCallCompleted,
		"brokered write tool call completed",
		brokeredToolEventContent(completed, map[string]any{
			"targetArgsDigest": argsDigest,
			"toolResult":       json.RawMessage(`{"success":true}`),
		}),
	); err != nil {
		t.Fatalf("record completed brokered execution: %v", err)
	}

	const emptyResultRef = "empty-failed-brokered-result"
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, emptyResultRef, []byte{}); err != nil {
		t.Fatalf("save empty brokered result: %v", err)
	}
	failed := harness.ToolCallResult{IdempotencyKey: idempotencyKey, Approved: true}
	if err := r.recordHarnessBrokeredToolEvent(
		context.Background(),
		task,
		frame,
		events.ExecutionEventTypeToolCallFailed,
		"brokered write tool call failed",
		brokeredToolEventContent(failed, map[string]any{
			"targetArgsDigest": argsDigest,
			"toolResultRef":    emptyResultRef,
		}),
	); err != nil {
		t.Fatalf("record failed brokered execution: %v", err)
	}

	result, ok, err := r.previousHarnessBrokeredToolResult(context.Background(), task, frame, idempotencyKey, argsDigest)
	if err != nil {
		t.Fatalf("previousHarnessBrokeredToolResult() error = %v", err)
	}
	if !ok || result.Error == nil || result.Error.Code != "tool_result_replay_unavailable" {
		t.Fatalf("previousHarnessBrokeredToolResult() = (%#v, %t), want fail-closed tool_result_replay_unavailable", result, ok)
	}
}

func TestHarnessBrokeredToolLedgerPaginationRejectsNonProgress(t *testing.T) {
	page := make([]store.ExecutionEvent, store.MaxExecutionEventLimit)
	for i := range page {
		page[i] = store.ExecutionEvent{Seq: int64(i + 1), ToolCallID: "other-call"}
	}

	var filters []store.ExecutionEventFilter
	ledger := &harnessBrokerLedgerListStore{
		ExecutionEventStore: store.NewFakeExecutionEventStore(),
		list: func(_ context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
			filters = append(filters, filter)
			return page, nil
		},
	}
	task, _, _ := harnessBrokerWriteTestObjects("http://tool.invalid")
	r := &TaskReconciler{ExecutionEventStore: ledger}
	frame := brokeredWriteFrame()
	idempotencyKey := harness.ToolRequestIdempotencyKey(frame.RuntimeSessionID, frame.TurnID, frame.ToolCallID)

	_, err := r.hasUnresolvedHarnessBrokeredToolExecution(context.Background(), task, frame, idempotencyKey, "sha256:args")
	if err == nil || !strings.Contains(err.Error(), "pagination did not advance") {
		t.Fatalf("hasUnresolvedHarnessBrokeredToolExecution() error = %v, want non-progress error", err)
	}
	if len(filters) != 2 {
		t.Fatalf("list calls = %d, want 2", len(filters))
	}
	if filters[0].AfterSeq != 0 || filters[0].Limit != store.MaxExecutionEventLimit {
		t.Fatalf("first filter = %#v, want AfterSeq=0 Limit=%d", filters[0], store.MaxExecutionEventLimit)
	}
	if filters[1].AfterSeq != int64(store.MaxExecutionEventLimit) || filters[1].Limit != store.MaxExecutionEventLimit {
		t.Fatalf("second filter = %#v, want AfterSeq=%d Limit=%d", filters[1], store.MaxExecutionEventLimit, store.MaxExecutionEventLimit)
	}
}

func TestHarnessBrokeredToolLedgerLookupPropagatesErrors(t *testing.T) {
	task, _, _ := harnessBrokerWriteTestObjects("http://tool.invalid")
	frame := brokeredWriteFrame()
	idempotencyKey := harness.ToolRequestIdempotencyKey(frame.RuntimeSessionID, frame.TurnID, frame.ToolCallID)

	t.Run("store error", func(t *testing.T) {
		wantErr := errors.New("ledger unavailable")
		ledger := &harnessBrokerLedgerListStore{
			ExecutionEventStore: store.NewFakeExecutionEventStore(),
			list: func(_ context.Context, _ store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
				return nil, wantErr
			},
		}
		r := &TaskReconciler{ExecutionEventStore: ledger}

		_, _, err := r.previousHarnessBrokeredToolResult(context.Background(), task, frame, idempotencyKey, "sha256:args")
		if !errors.Is(err, wantErr) {
			t.Fatalf("previousHarnessBrokeredToolResult() error = %v, want wrapped %v", err, wantErr)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		listCalled := false
		ledger := &harnessBrokerLedgerListStore{
			ExecutionEventStore: store.NewFakeExecutionEventStore(),
			list: func(_ context.Context, _ store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
				listCalled = true
				return nil, nil
			},
		}
		r := &TaskReconciler{ExecutionEventStore: ledger}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := r.hasUnresolvedHarnessBrokeredToolExecution(ctx, task, frame, idempotencyKey, "sha256:args")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("hasUnresolvedHarnessBrokeredToolExecution() error = %v, want context canceled", err)
		}
		if listCalled {
			t.Fatal("ListExecutionEvents called after context cancellation")
		}
	})
}

type harnessBrokerLedgerListStore struct {
	store.ExecutionEventStore
	list func(context.Context, store.ExecutionEventFilter) ([]store.ExecutionEvent, error)
}

func (s *harnessBrokerLedgerListStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	return s.list(ctx, filter)
}

func harnessBrokerWriteTestObjects(toolURL string) (*corev1alpha1.Task, *corev1alpha1.Agent, *corev1alpha1.Tool) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"dispatch_work_order"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch_work_order", Namespace: task.Namespace, ResourceVersion: "1"},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Dispatch technician",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolURL},
		},
	}
	return task, agent, tool
}

func appendHarnessBrokerLedgerFillers(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, count int) {
	t.Helper()
	for i := range count {
		content, err := json.Marshal(map[string]any{
			"brokered":       false,
			"fillerSequence": i,
		})
		if err != nil {
			t.Fatalf("marshal filler event %d: %v", i, err)
		}
		if _, err := r.ExecutionEventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace:  task.Namespace,
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   task.Name,
			TaskName:   task.Name,
			Type:       events.ExecutionEventTypeToolCallCompleted,
			ToolName:   "filler_tool",
			ToolCallID: fmt.Sprintf("filler-call-%d", i),
			Content:    content,
		}); err != nil {
			t.Fatalf("append filler event %d: %v", i, err)
		}
	}
}
