package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

type approvalTerminalProjectionOutageStore struct {
	store.TaskDataTransactionStore
	store.DeduplicatingExecutionEventStore
	unavailable atomic.Bool
	attempts    atomic.Int32
}

func (s *approvalTerminalProjectionOutageStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	var payload struct {
		ExecutionOutcome string `json:"executionOutcome"`
	}
	if event.Type == events.ExecutionEventTypeApprovalExecutionUpdated && json.Unmarshal(event.Content, &payload) == nil &&
		payload.ExecutionOutcome != "" && payload.ExecutionOutcome != "running" && s.unavailable.Load() {
		s.attempts.Add(1)
		return nil, false, errors.New("injected terminal approval projection outage")
	}
	return s.DeduplicatingExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, key)
}

func TestMCPApprovalTerminalReceiptSurvivesProjectionOutage(t *testing.T) {
	for _, tc := range []struct {
		name       string
		decision   string
		toolResult json.RawMessage
		state      store.ExternalEffectState
		outcome    string
		isError    bool
		executions int32
	}{
		{
			name: "success", decision: events.ExecutionEventTypeApprovalApproved,
			toolResult: json.RawMessage(`{ "workOrder":"simulated-1", "z":1e-7 }`),
			state:      store.ExternalEffectSucceeded, outcome: "succeeded", executions: 1,
		},
		{
			name: "tool_error", decision: events.ExecutionEventTypeApprovalApproved,
			toolResult: json.RawMessage(`{ "isError":true, "error":"simulated failure", "z":1e-7 }`),
			state:      store.ExternalEffectSucceeded, outcome: acpApprovalOutcomeFailed, isError: true, executions: 1,
		},
		{
			name: "denial", decision: events.ExecutionEventTypeApprovalDeclined,
			state: store.ExternalEffectFailed, outcome: "not_started", isError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			authorizer := f.broker.Prompts.(DurableACPMCPPromptAuthorizer)
			registerApprovalLease(t, authorizer.PromptLeases, f.ctx, f.request)
			original := f.broker.Executor
			f.broker.Executor = ACPMCPToolExecutorFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
				_, err := original.ExecuteACPMCPTool(ctx, request, descriptor)
				return tc.toolResult, err
			})
			projection := &approvalTerminalProjectionOutageStore{DeduplicatingExecutionEventStore: f.events, TaskDataTransactionStore: f.events}
			projection.unavailable.Store(true)
			f.broker.ApprovalEvents = projection
			f.dispatcher.EventStore = projection

			done := f.start(f.request)
			pending := f.pending()
			f.decide(pending.ID, tc.decision)
			first := awaitMCPApprovalResult(t, done)
			require.False(t, first.Replayed)
			require.Equal(t, tc.isError, first.IsError)
			require.Equal(t, tc.executions, f.count.Load())
			if tc.toolResult != nil {
				require.JSONEq(t, string(tc.toolResult), string(first.Result))
			} else {
				require.JSONEq(t, string(acpApprovalError(pending.ID, acpApprovalCodeDeclined)), string(first.Result))
			}
			require.EqualValues(t, 1, projection.attempts.Load())

			identity := store.ExternalEffectIdentity{
				Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
				AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
			}
			effect, err := f.control.GetExternalEffectByIdentity(f.ctx, identity)
			require.NoError(t, err)
			require.Equal(t, tc.state, effect.State)
			outcome, _, saved, err := acpMCPApprovalReceiptOutcome(effect, pending.ID)
			require.NoError(t, err)
			require.Equal(t, tc.outcome, outcome)
			require.Equal(t, first.Result, saved, "the HTTP response must be the durable canonical receipt")
			_, listed := f.approval(t)
			for _, event := range listed {
				if event.Type == events.ExecutionEventTypeApprovalExecutionUpdated {
					var payload struct {
						ExecutionOutcome string `json:"executionOutcome"`
					}
					require.NoError(t, json.Unmarshal(event.Content, &payload))
					require.Equal(t, "running", payload.ExecutionOutcome, "the terminal projection is still unavailable")
				}
			}

			// Exact redelivery must return the same receipt while projection is
			// still unavailable, without invoking the tool a second time.
			replayed := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replayed.Replayed)
			require.Equal(t, first.IsError, replayed.IsError)
			require.Equal(t, first.Result, replayed.Result)
			require.EqualValues(t, 2, projection.attempts.Load())
			require.Equal(t, tc.executions, f.count.Load())

			// A new controller repairs the event projection from the receipt,
			// with no executable Secret access or runtime redelivery.
			f.restart(t)
			require.EqualError(t, f.reconcile(t), "injected terminal approval projection outage")
			projection.unavailable.Store(false)
			require.NoError(t, f.reconcile(t))
			recovered, _ := f.approval(t)
			require.Equal(t, tc.outcome, recovered.ExecutionOutcome)
			require.NotEmpty(t, recovered.ExecutionReason)
			require.Equal(t, tc.executions, f.count.Load())
			require.Zero(t, f.secretReads.Load())
		})
	}
}

type approvalUnverifiedReceiptStore struct {
	store.ExternalEffectStore
}

func (s approvalUnverifiedReceiptStore) ReserveExternalEffect(ctx context.Context, request store.ReserveExternalEffectRequest) (*store.ExternalEffect, error) {
	effect, err := s.ExternalEffectStore.ReserveExternalEffect(ctx, request)
	if err == nil && effect.State == store.ExternalEffectSucceeded {
		changed := *effect
		changed.ResponseDigest = store.CanonicalBytesDigest([]byte("different receipt"))
		return &changed, nil
	}
	return effect, err
}

func TestMCPApprovalReplayRejectsUnverifiedReceiptBeforeProjection(t *testing.T) {
	f := newMCPApprovalFixture(t)
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	awaitMCPApprovalResult(t, done)

	projection := &approvalTerminalProjectionOutageStore{DeduplicatingExecutionEventStore: f.events, TaskDataTransactionStore: f.events}
	projection.unavailable.Store(true)
	f.broker.ApprovalEvents = projection
	f.broker.Effects = approvalUnverifiedReceiptStore{ExternalEffectStore: f.broker.Effects}
	response := performMCPBrokerCall(t, f.broker, f.request, strings.Repeat("b", 32), []byte(strings.Repeat("c", 32)))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.Zero(t, projection.attempts.Load(), "an unverified receipt must not be projected or returned")
	require.EqualValues(t, 1, f.count.Load(), "an unverifiable receipt must never authorize another execution")
}

type approvalAppendOutageStore struct {
	store.TaskDataTransactionStore
	store.DeduplicatingExecutionEventStore
	attempts atomic.Int32
}

func (s *approvalAppendOutageStore) AppendExecutionEventIfAbsent(context.Context, *store.ExecutionEvent, string) (*store.ExecutionEvent, bool, error) {
	s.attempts.Add(1)
	return nil, false, errors.New("injected event store outage")
}

func TestMCPApprovalReplaysTerminalReceiptDuringEventStoreOutage(t *testing.T) {
	f := newMCPApprovalFixture(t)
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	first := awaitMCPApprovalResult(t, done)
	require.False(t, first.Replayed)

	// Exact redelivery of a completed call must return the durable receipt
	// even when no event can be appended; recovery repairs projection later.
	outage := &approvalAppendOutageStore{DeduplicatingExecutionEventStore: f.events, TaskDataTransactionStore: f.events}
	f.broker.ApprovalEvents = outage
	replayed := awaitMCPApprovalResult(t, f.start(f.request))
	require.True(t, replayed.Replayed)
	require.Equal(t, first.Result, replayed.Result)
	require.EqualValues(t, 1, f.count.Load())
	require.EqualValues(t, 1, outage.attempts.Load(), "only the best-effort outcome projection may touch the event store")
}
