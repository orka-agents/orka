package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

type mcpApprovalRecoveryFixture struct {
	*mcpApprovalFixture
	ctx         context.Context
	kube        client.Client
	control     *storekube.Store
	dispatcher  *ACPDispatcher
	task        *corev1alpha1.Task
	fence       store.ControllerEpochFence
	stopEpoch   func()
	exactReads  atomic.Int32
	secretReads atomic.Int32
}

func newMCPApprovalRecoveryFixture(t *testing.T) *mcpApprovalRecoveryFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	f := &mcpApprovalRecoveryFixture{mcpApprovalFixture: newMCPApprovalFixture(t), ctx: ctx}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	f.task = &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.request.Namespace, Name: "approval-task", UID: types.UID(f.request.Metadata.TaskUID)},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	f.kube = withControllerEpochLeaseUIDs(t, fake.NewClientBuilder().WithScheme(scheme).WithObjects(f.task).
		WithStatusSubresource(&corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{}, &corev1alpha1.ExternalEffect{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
				switch object.(type) {
				case *corev1alpha1.ExternalEffect:
					f.exactReads.Add(1)
				case *corev1.Secret:
					f.secretReads.Add(1)
				}
				return c.Get(ctx, key, object, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.SecretList); ok {
					f.secretReads.Add(1)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build())
	var err error
	f.control, err = storekube.NewComposite(f.kube, "orka-system", f.events, storekube.WithAPIReader(f.kube))
	require.NoError(t, err)
	epochs, stop := startArchivedRecoveryEpoch(t, ctx, f.control, nil, "controller-a")
	f.stopEpoch = stop
	f.fence, err = epochs.CurrentFence(ctx)
	require.NoError(t, err)
	f.dispatcher = &ACPDispatcher{Client: f.kube, APIReader: f.kube, Store: f.control, EventStore: f.events, Epochs: epochs}
	f.broker.Effects, f.broker.EpochMutations = f.control, f.control
	f.broker.Prompts = DurableACPMCPPromptAuthorizer{Attempts: f.control, PromptLeases: &ACPMCPPromptLeaseRegistry{}}
	f.createRunningAttempt(t)
	return f
}

func (f *mcpApprovalRecoveryFixture) createRunningAttempt(t *testing.T) {
	t.Helper()
	attempt, err := f.control.CreatePromptAttempt(f.ctx, &store.PromptAttempt{
		Key:        mcpPromptLeaseKey(f.request.Namespace, f.request.Metadata),
		SessionUID: string(f.request.Authorization.RuntimeSessionUID), RuntimeInstanceID: string(f.request.Metadata.Fence.RuntimeInstanceID),
		RequestDigest: testControllerMCPDigest("prompt"), BindingDigest: testControllerMCPDigest("binding"), SnapshotDigest: testControllerMCPDigest("snapshot"),
	}, f.fence)
	require.NoError(t, err)
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning,
	} {
		attempt, err = f.control.TransitionPromptAttemptExecution(f.ctx, store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: f.fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: state, OperationID: string(state), OperationDigest: testControllerMCPDigest(string(state)),
		})
		require.NoError(t, err)
	}
}

func (f *mcpApprovalRecoveryFixture) restart(t *testing.T) {
	t.Helper()
	f.stopEpoch()
	epochs, stop := startArchivedRecoveryEpoch(t, f.ctx, f.control, nil, "controller-b")
	f.stopEpoch = stop
	f.dispatcher.Epochs = epochs
	var err error
	f.fence, err = epochs.CurrentFence(f.ctx)
	require.NoError(t, err)
	require.Greater(t, uint64(f.fence.Epoch), f.request.Metadata.Fence.ControllerEpoch)
	// Recovery has no executable Secret client and no remembered prompt lease.
	f.broker.ApprovalSecrets = nil
	f.broker.Prompts = DurableACPMCPPromptAuthorizer{Attempts: f.control, PromptLeases: &ACPMCPPromptLeaseRegistry{}}
}

func (f *mcpApprovalRecoveryFixture) seed(t *testing.T, state store.ExternalEffectState, prior string, executed bool, result json.RawMessage) (*acpMCPApprovalCall, *store.ExternalEffect) {
	t.Helper()
	descriptor, err := f.request.ValidateAt(time.Now().UTC())
	require.NoError(t, err)
	credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
	require.NoError(t, err)
	credentials.Task.SessionName, credentials.Task.AgentName = "approval-session", "approval-agent"
	call, _, err := f.broker.persistApprovalCall(f.ctx, f.request, descriptor, credentials.Task)
	require.NoError(t, err)
	effect, err := f.control.ReserveExternalEffect(f.ctx, store.ReserveExternalEffectRequest{
		Identity: store.ExternalEffectIdentity{
			Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
			AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
		},
		RequestDigest: call.RequestDigest, Fence: f.fence, CreatedAt: call.CreatedAt, ApprovalTaskUID: call.Task.UID,
	})
	require.NoError(t, err)
	require.NoError(t, f.broker.requestToolApproval(f.ctx, call))
	decision := events.ExecutionEventTypeApprovalApproved
	if state == store.ExternalEffectFailed {
		decision = events.ExecutionEventTypeApprovalDeclined
		result = acpApprovalError(call.ID, "approval_declined")
	}
	f.decide(call.ID, decision)
	if state == store.ExternalEffectPending {
		return call, effect
	}
	expires := time.Now().UTC().Add(5 * time.Minute)
	effect, err = f.control.TransitionExternalEffect(f.ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: f.fence, ExpectedVersion: effect.Version,
		ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectInFlight,
		RequestDigest: call.RequestDigest, LeaseOwner: "original-call-owner", LeaseExpiresAt: &expires,
	})
	require.NoError(t, err)
	if prior != "" {
		require.NoError(t, f.broker.approvalOutcome(f.ctx, call, prior, "Approved action started", nil))
	}
	if executed {
		_, err = f.broker.Executor.ExecuteACPMCPTool(f.ctx, f.request, descriptor)
		require.NoError(t, err)
	}
	if state == store.ExternalEffectInFlight {
		return call, effect
	}
	digest := ""
	if len(result) > 0 {
		result, err = canonicalMCPApprovalResult(result)
		require.NoError(t, err)
		digest = store.CanonicalBytesDigest(result)
	}
	effect, err = f.control.TransitionExternalEffect(f.ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: f.fence, ExpectedVersion: effect.Version,
		ExpectedState: store.ExternalEffectInFlight, NewState: state, RequestDigest: call.RequestDigest,
		ExpectedLeaseOwner: effect.LeaseOwner, Response: result, ResponseDigest: digest,
	})
	require.NoError(t, err)
	return call, effect
}

func (f *mcpApprovalRecoveryFixture) reconcile(t *testing.T) error {
	t.Helper()
	var tasks corev1alpha1.TaskList
	require.NoError(t, f.kube.List(f.ctx, &tasks))
	return f.dispatcher.reconcileExpiredExternalEffects(f.ctx, tasks.Items)
}

func (f *mcpApprovalRecoveryFixture) approval(t *testing.T) (approvals.Approval, []store.ExecutionEvent) {
	t.Helper()
	listed, err := approvals.ListEvents(f.ctx, f.events, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	// This is the same Task-UID-filtered derivation used by ListTaskApprovals.
	listed = approvals.FilterEventsForTaskUID(listed, string(f.task.UID))
	values := approvals.Derive(listed, time.Time{})
	require.Len(t, values, 1)
	return values[0], listed
}

func TestMCPApprovalRecoveryProjectsPersistedExecutionAfterRestart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    store.ExternalEffectState
		prior    string
		executed bool
		result   json.RawMessage
		want     string
	}{
		{"claimed_before_start_event", store.ExternalEffectInFlight, "", false, nil, "unknown"},
		{"started_without_receipt", store.ExternalEffectInFlight, "running", true, nil, "unknown"},
		{"completed_before_outcome_event", store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"workOrder":"simulated-1"}`), "succeeded"},
		{"completed_nested_numbers", store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"z":[1.0,1e2,1e-7,9007199254740993,{"b":"<&>","a":2.5}],"isError":false}`), "succeeded"},
		{"completed_tool_error", store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"isError":true,"error":"simulated failure"}`), "failed"},
		{"denied_before_outcome_event", store.ExternalEffectFailed, "", false, nil, "not_started"},
		{"unknown_before_outcome_event", store.ExternalEffectOutcomeUnknown, "running", true, nil, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			call, effect := f.seed(t, tc.state, tc.prior, tc.executed, tc.result)
			f.restart(t)
			count := f.count.Load()
			// Real stale-epoch credential resolution and empty local lease state
			// both reject redelivery before it can replay the persisted receipt.
			resolver := KubernetesACPMCPBrokerCredentialResolver{Reader: f.kube, Epochs: f.dispatcher.Epochs}
			_, err := resolver.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
			require.EqualError(t, err, "MCP request uses a stale controller epoch")
			require.ErrorIs(t, f.broker.Prompts.AuthorizeACPMCPPrompt(f.ctx, f.request), errACPMCPPromptLeaseInactive)
			replay := performMCPBrokerCall(t, f.broker, f.request, strings.Repeat("b", 32), []byte(strings.Repeat("c", 32)))
			require.Equal(t, http.StatusForbidden, replay.Code)
			require.NoError(t, f.reconcile(t))
			approval, listed := f.approval(t)
			require.Equal(t, tc.want, approval.ExecutionOutcome)
			require.Equal(t, call.RequestDigest, approval.Binding.RequestDigest)
			require.Equal(t, "approval-session", listed[len(listed)-1].SessionName)
			require.Equal(t, "approval-agent", listed[len(listed)-1].AgentName)
			require.NotContains(t, string(listed[len(listed)-1].Content), "workOrder")
			persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			if tc.state == store.ExternalEffectInFlight {
				require.Equal(t, store.ExternalEffectOutcomeUnknown, persisted.State)
				require.Equal(t, effect.Version+1, persisted.Version)
			} else {
				require.Equal(t, effect.State, persisted.State)
				require.Equal(t, effect.Version, persisted.Version)
			}
			exactReads := f.exactReads.Load()
			require.NoError(t, f.reconcile(t))
			_, again := f.approval(t)
			require.Len(t, again, len(listed), "repeated recovery must not append duplicate outcomes")
			require.Equal(t, exactReads, f.exactReads.Load(), "correct projections need no exact effect read")
			require.Zero(t, f.secretReads.Load())
			require.Equal(t, count, f.count.Load(), "recovery must never execute or repeat the action")
		})
	}
}

func TestMCPApprovalRecoveryPreservesRedactedCallIdentity(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	for i, state := range []store.ExternalEffectState{store.ExternalEffectSucceeded, store.ExternalEffectPending} {
		f.request.Call.CallID = fmt.Sprintf("https://tools.example/call?request=%d#step", i)
		f.request.Metadata.OperationID = harnessv2.OperationID(fmt.Sprintf("operation-%d", i))
		var err error
		f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
		require.NoError(t, err)
		executed := state == store.ExternalEffectSucceeded
		prior := ""
		if executed {
			prior = "running"
		}
		f.seed(t, state, prior, executed, json.RawMessage(`{"workOrder":"simulated-1"}`))
	}
	listed, err := approvals.ListEvents(f.ctx, f.events, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	before := approvals.Derive(listed, time.Time{})
	require.Len(t, before, 2)
	require.Equal(t, before[0].ToolCallID, before[1].ToolCallID, "distinct calls have identical redacted display text")
	require.NotEqual(t, before[0].ID, before[1].ID)
	require.NotEqual(t, before[0].Binding.CallIDDigest, before[1].Binding.CallIDDigest)
	for _, event := range listed {
		require.NotContains(t, string(event.Content), "?request=")
		require.NotContains(t, string(event.Content), "#step")
	}
	f.restart(t)
	require.NoError(t, f.reconcile(t))
	listed, err = approvals.ListEvents(f.ctx, f.events, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	after := approvals.Derive(listed, time.Time{})
	require.Len(t, after, 2)
	require.Equal(t, before[0].ID, after[0].ID)
	require.Equal(t, "succeeded", after[0].ExecutionOutcome)
	require.Equal(t, before[1].ID, after[1].ID)
	require.Equal(t, "not_started", after[1].ExecutionOutcome)
	require.Equal(t, acpApprovalCodeStale, after[1].ExecutionReason)
	require.EqualValues(t, 1, f.count.Load(), "recovery must neither repeat the completed call nor start the pending call")
	require.Zero(t, f.secretReads.Load(), "recovery must not read the original executable requests")
}

func TestMCPApprovalRecoveryValidatesPersistedCallBinding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		legacy  bool
		mutate  func(*approvals.CallBinding)
		recover bool
	}{
		{name: "digest", recover: true},
		{name: "missing_digest", mutate: func(b *approvals.CallBinding) { b.CallIDDigest = "" }},
		{name: "malformed_digest", mutate: func(b *approvals.CallBinding) { b.CallIDDigest = "sha256:abc" }},
		{name: "noncanonical_digest", mutate: func(b *approvals.CallBinding) { b.CallIDDigest = strings.ToUpper(b.CallIDDigest) }},
		{name: "different_digest", mutate: func(b *approvals.CallBinding) { b.CallIDDigest = testControllerMCPDigest("another call") }},
		{name: "different_attempt", mutate: func(b *approvals.CallBinding) { b.TaskAttempt++ }},
		{name: "different_prompt", mutate: func(b *approvals.CallBinding) { b.PromptID = "another-prompt" }},
		{name: "legacy_without_digest", legacy: true, recover: true},
		{name: "legacy_malformed_digest", legacy: true, mutate: func(b *approvals.CallBinding) { b.CallIDDigest = "sha256:abc" }},
		{name: "legacy_with_digest", legacy: true, mutate: func(b *approvals.CallBinding) { b.CallIDDigest = testControllerMCPDigest("another call") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			call, _ := f.seed(t, store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
			_, listed := f.approval(t)
			approvalID := call.ID
			if tc.legacy {
				approvalID = store.CanonicalControlID("acp-tool-approval", f.task.Namespace, string(f.task.UID),
					fmt.Sprint(f.request.Metadata.TaskAttempt), string(f.request.Metadata.PromptID), f.request.Call.CallID)
			}
			// Rebuild the saved history to model old or damaged event bindings.
			// The completed effect and its immutable request digest stay intact.
			require.NoError(t, f.events.DeleteExecutionEvents(f.ctx, f.task.Namespace, events.ExecutionEventStreamTypeTask, f.task.Name))
			for _, event := range listed {
				var payload map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(event.Content, &payload))
				payload["approvalID"], _ = json.Marshal(approvalID)
				if event.Type == events.ExecutionEventTypeApprovalRequested {
					var binding approvals.CallBinding
					require.NoError(t, json.Unmarshal(payload["binding"], &binding))
					if tc.legacy {
						binding.CallIDDigest = ""
					}
					if tc.mutate != nil {
						tc.mutate(&binding)
					}
					payload["binding"], _ = json.Marshal(binding)
				}
				var err error
				event.Content, err = json.Marshal(payload)
				require.NoError(t, err)
				event.ToolCallID = approvalID
				_, err = f.events.AppendExecutionEvent(f.ctx, &event)
				require.NoError(t, err)
			}
			before, _ := f.approval(t)
			require.Equal(t, "running", before.ExecutionOutcome)
			f.restart(t)
			require.NoError(t, f.reconcile(t))
			after, afterEvents := f.approval(t)
			require.Equal(t, approvalID, after.ID)
			if tc.recover {
				require.Equal(t, "succeeded", after.ExecutionOutcome)
			} else {
				require.Equal(t, before, after, "invalid bindings must not inherit the saved receipt")
				require.Len(t, afterEvents, len(listed))
			}
			require.EqualValues(t, 1, f.count.Load())
			require.Zero(t, f.secretReads.Load())
		})
	}
}

type approvalLostOutcomeEventStore struct {
	store.DeduplicatingExecutionEventStore
	lost atomic.Bool
}

func (s *approvalLostOutcomeEventStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	var payload struct {
		ExecutionOutcome string `json:"executionOutcome"`
	}
	if event.Type == events.ExecutionEventTypeApprovalExecutionUpdated && json.Unmarshal(event.Content, &payload) == nil &&
		payload.ExecutionOutcome != "" && payload.ExecutionOutcome != "running" && !s.lost.Swap(true) {
		return nil, false, errors.New("injected crash after receipt commit")
	}
	return s.DeduplicatingExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, key)
}

func TestMCPApprovalRecoveryAfterBrokerReceiptCommit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision string
		state    store.ExternalEffectState
		outcome  string
		count    int32
	}{
		{"tool_error", events.ExecutionEventTypeApprovalApproved, store.ExternalEffectSucceeded, "failed", 1},
		{"denial", events.ExecutionEventTypeApprovalDeclined, store.ExternalEffectFailed, "not_started", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			authorizer := f.broker.Prompts.(DurableACPMCPPromptAuthorizer)
			registerApprovalLease(t, authorizer.PromptLeases, f.ctx, f.request)
			original := f.broker.Executor
			f.broker.Executor = ACPMCPToolExecutorFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
				_, err := original.ExecuteACPMCPTool(ctx, request, descriptor)
				return json.RawMessage(`{ "isError":true, "z":1e-7, "error":"simulated failure" }`), err
			})
			lost := &approvalLostOutcomeEventStore{DeduplicatingExecutionEventStore: f.events}
			f.broker.ApprovalEvents = lost
			done := f.start(f.request)
			pending := f.pending()
			f.decide(pending.ID, tc.decision)
			select {
			case response := <-done:
				require.Equal(t, http.StatusServiceUnavailable, response.Code)
			case <-f.ctx.Done():
				t.Fatal("broker did not return after losing the final outcome event")
			}
			require.True(t, lost.lost.Load())
			require.Equal(t, tc.count, f.count.Load())
			identity := store.ExternalEffectIdentity{
				Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
				AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
			}
			effect, err := f.control.GetExternalEffectByIdentity(f.ctx, identity)
			require.NoError(t, err)
			require.Equal(t, tc.state, effect.State)
			outcome, _, _, err := acpMCPApprovalReceiptOutcome(effect, pending.ID)
			require.NoError(t, err, "the broker's committed receipt must survive the Kubernetes JSON round-trip")
			require.Equal(t, tc.outcome, outcome)
			f.restart(t)
			require.NoError(t, f.reconcile(t))
			approval, _ := f.approval(t)
			require.Equal(t, tc.outcome, approval.ExecutionOutcome)
			require.NotEmpty(t, approval.ExecutionReason)
			require.Equal(t, tc.count, f.count.Load())
		})
	}
}

type approvalCancelDuringEffectReadStore struct {
	store.ExternalEffectStore
	cancel      context.CancelFunc
	interrupted atomic.Bool
}

func (s *approvalCancelDuringEffectReadStore) GetExternalEffect(ctx context.Context, id string) (*store.ExternalEffect, error) {
	if !s.interrupted.Swap(true) {
		// The first broker effect read follows its pending decision poll.
		// Cancel at that storage boundary, before it has a replacement value.
		s.cancel()
		return nil, ctx.Err()
	}
	return s.ExternalEffectStore.GetExternalEffect(ctx, id)
}

func TestMCPApprovalPostPollCancellationPersistsUnstartedReceipt(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	leaseCtx, cancelLease := context.WithCancel(f.ctx)
	t.Cleanup(cancelLease)
	ctx, cancelRequest := context.WithCancel(f.ctx)
	t.Cleanup(cancelRequest)
	authorizer := f.broker.Prompts.(DurableACPMCPPromptAuthorizer)
	registerApprovalLease(t, authorizer.PromptLeases, leaseCtx, f.request)
	storage := &approvalCancelDuringEffectReadStore{ExternalEffectStore: f.control, cancel: func() {
		cancelLease()
		cancelRequest()
	}}
	f.broker.Effects = storage
	result := awaitMCPApprovalResult(t, f.startContext(ctx, f.request))
	require.True(t, storage.interrupted.Load())
	require.True(t, result.IsError)
	require.Contains(t, string(result.Result), `"code":"approval_stale"`)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Zero(t, f.count.Load())
	approval, _ := f.approval(t)
	require.Equal(t, approvals.StatusCancelled, approval.Status)
	require.Equal(t, "not_started", approval.ExecutionOutcome)
	require.Equal(t, acpApprovalCodeStale, approval.ExecutionReason)
	effect, err := f.control.GetExternalEffectByIdentity(f.ctx, store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
	})
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectFailed, effect.State)
	outcome, reason, saved, err := acpMCPApprovalReceiptOutcome(effect, approval.ID)
	require.NoError(t, err)
	require.Equal(t, "not_started", outcome)
	require.Equal(t, acpApprovalCodeStale, reason)
	require.Equal(t, result.Result, saved)
}

type approvalRecoveryEventStore struct {
	store.DeduplicatingExecutionEventStore
	lists                 atomic.Int32
	sequences             atomic.Int32
	sequenceBatches       atomic.Int32
	sequenceStreams       atomic.Int64
	appends               atomic.Int32
	failNext              atomic.Bool
	failNextSequenceBatch atomic.Bool
	afterAppend           func()
}

func (s *approvalRecoveryEventStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	s.lists.Add(1)
	return s.DeduplicatingExecutionEventStore.ListExecutionEvents(ctx, filter)
}

func (s *approvalRecoveryEventStore) GetLatestExecutionEventSeq(ctx context.Context, namespace, streamType, streamID string) (int64, error) {
	s.sequences.Add(1)
	return s.DeduplicatingExecutionEventStore.GetLatestExecutionEventSeq(ctx, namespace, streamType, streamID)
}

func (s *approvalRecoveryEventStore) GetLatestExecutionEventSeqs(ctx context.Context, namespace, streamType string, streamIDs []string) (map[string]int64, error) {
	s.sequenceBatches.Add(1)
	s.sequenceStreams.Add(int64(len(streamIDs)))
	if s.failNextSequenceBatch.Swap(false) {
		return nil, errors.New("injected approval sequence outage")
	}
	return s.DeduplicatingExecutionEventStore.GetLatestExecutionEventSeqs(ctx, namespace, streamType, streamIDs)
}

func (s *approvalRecoveryEventStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	s.appends.Add(1)
	if s.failNext.Swap(false) {
		return nil, false, errors.New("injected approval projection outage")
	}
	persisted, appended, err := s.DeduplicatingExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, key)
	if err == nil && appended && s.afterAppend != nil {
		s.afterAppend()
	}
	return persisted, appended, err
}

func TestMCPApprovalRecoveryOnlyReadsOwningTaskAndSkipsUnchangedHistory(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	_, effect := f.seed(t, store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
	for i := range 30 {
		other := f.task.DeepCopy()
		other.Name, other.UID, other.ResourceVersion = fmt.Sprintf("other-task-%d", i), types.UID(fmt.Sprintf("other-uid-%d", i)), ""
		require.NoError(t, f.kube.Create(f.ctx, other))
	}
	// Automatic calls retain the same execution identity kind but have no
	// approval discovery hint, even after a terminal receipt has been saved.
	_, err := runExternalEffect(f.ctx, f.control, f.fence, store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: effect.Identity.AggregateID, OperationID: "automatic-operation",
	}, "automatic-lookup", func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"available":true}`), nil
	})
	require.NoError(t, err)

	observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
	f.dispatcher.EventStore = observed
	require.NoError(t, f.reconcile(t))
	require.Zero(t, observed.sequences.Load(), "recovery must not read sequences one Task at a time")
	require.EqualValues(t, 1, observed.sequenceBatches.Load())
	require.EqualValues(t, 1, observed.sequenceStreams.Load(), "only the owning Task belongs in the sequence batch")
	require.EqualValues(t, 2, observed.lists.Load(), "read the owning history once and recheck it under the epoch guard")
	require.EqualValues(t, 1, observed.appends.Load())
	// The first follow-up observes recovery's own append. Later scans must
	// skip full histories while the receipt and event sequence stay unchanged.
	require.NoError(t, f.reconcile(t))
	lists, exactReads := observed.lists.Load(), f.exactReads.Load()
	for range 3 {
		require.NoError(t, f.reconcile(t))
	}
	require.Equal(t, lists, observed.lists.Load())
	require.Equal(t, exactReads, f.exactReads.Load())
	require.Len(t, f.dispatcher.approvalRecovery, 1)
	require.Zero(t, observed.sequences.Load())
	persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, effect, persisted, "recovery must not change the original execution identity or receipt")

	require.NoError(t, f.kube.Delete(f.ctx, f.task))
	batches := observed.sequenceBatches.Load()
	require.NoError(t, f.reconcile(t))
	require.Equal(t, batches, observed.sequenceBatches.Load(), "historical effects for deleted Tasks need no event query")
	require.Empty(t, f.dispatcher.approvalRecovery)
}

func TestMCPApprovalRecoveryBatchesHistoricalTaskSequences(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	_, effect := f.seed(t, store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
	tasks := make([]corev1alpha1.Task, 1, 65)
	tasks[0] = *f.task
	effects := map[string]acpMCPApprovalEffect{
		effect.ID: {ExternalEffect: *effect, taskUID: f.task.UID},
	}
	for i := range 64 {
		other := f.task.DeepCopy()
		other.Name, other.UID, other.ResourceVersion = fmt.Sprintf("retained-task-%d", i), types.UID(fmt.Sprintf("retained-uid-%d", i)), ""
		if i%2 == 0 {
			other.Namespace = "another-namespace"
		}
		tasks = append(tasks, *other)
		retained := *effect
		retained.Identity.Namespace = other.Namespace
		retained.Identity.AggregateID = fmt.Sprintf("retained-session-%d", i)
		retained.Identity.OperationID = fmt.Sprintf("retained-operation-%d", i)
		var err error
		retained.ID, err = retained.Identity.CanonicalID()
		require.NoError(t, err)
		// A crash before ApprovalRequested can leave a retained denial without
		// a review event. These owners must not cause individual sequence reads.
		retained.State = store.ExternalEffectFailed
		retained.Response = acpMCPAbandonedPendingReceipt(&retained)
		retained.ResponseDigest = store.CanonicalBytesDigest(retained.Response)
		effects[retained.ID] = acpMCPApprovalEffect{ExternalEffect: retained, taskUID: other.UID}
	}
	observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
	f.dispatcher.EventStore = observed
	reconcile := func() {
		t.Helper()
		require.NoError(t, f.dispatcher.reconcileMCPApprovalExecutions(f.ctx, f.fence, tasks, effects))
	}
	reconcile()
	require.EqualValues(t, 2, observed.sequenceBatches.Load(), "one batch per namespace, independent of Task count")
	require.EqualValues(t, len(tasks), observed.sequenceStreams.Load())
	// Observe the first projection's append, then verify unchanged histories
	// stay cached while every scan checks late events in namespace batches.
	reconcile()
	lists := observed.lists.Load()
	for range 3 {
		reconcile()
	}
	require.Equal(t, lists, observed.lists.Load())
	require.EqualValues(t, 10, observed.sequenceBatches.Load())
	require.EqualValues(t, 5*len(tasks), observed.sequenceStreams.Load())
	require.Zero(t, observed.sequences.Load())
	require.Len(t, f.dispatcher.approvalRecovery, len(tasks))
	approval, _ := f.approval(t)
	require.Equal(t, "succeeded", approval.ExecutionOutcome)
	require.EqualValues(t, 1, f.count.Load())
}

func TestMCPApprovalRecoveryRetriesFailedSequenceBatch(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, _ := f.seed(t, store.ExternalEffectSucceeded, "", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
	observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
	f.dispatcher.EventStore = observed
	require.NoError(t, f.reconcile(t))
	require.NoError(t, f.reconcile(t))
	lists, appends := observed.lists.Load(), observed.appends.Load()
	require.NoError(t, f.broker.approvalOutcome(f.ctx, call, "running", "Late start event", nil))
	observed.failNextSequenceBatch.Store(true)
	require.EqualError(t, f.reconcile(t), "injected approval sequence outage")
	require.Equal(t, lists, observed.lists.Load(), "failed sequence reads must not project histories")
	require.Equal(t, appends, observed.appends.Load())
	stale, _ := f.approval(t)
	require.Equal(t, "running", stale.ExecutionOutcome)
	require.NoError(t, f.reconcile(t))
	repaired, _ := f.approval(t)
	require.Equal(t, "succeeded", repaired.ExecutionOutcome)
	require.Equal(t, appends+1, observed.appends.Load())
	require.Zero(t, observed.sequences.Load())
	require.EqualValues(t, 1, f.count.Load())
}

func TestMCPApprovalRecoveryCacheDoesNotHideEventRacingWithProjection(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, _ := f.seed(t, store.ExternalEffectSucceeded, "", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
	var injected atomic.Bool
	observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
	observed.afterAppend = func() {
		if injected.CompareAndSwap(false, true) {
			require.NoError(t, f.broker.approvalOutcome(f.ctx, call, "running", "Late start event", nil))
		}
	}
	f.dispatcher.EventStore = observed
	require.NoError(t, f.reconcile(t))
	stale, _ := f.approval(t)
	require.Equal(t, "running", stale.ExecutionOutcome)
	require.NoError(t, f.reconcile(t))
	repaired, _ := f.approval(t)
	require.Equal(t, "succeeded", repaired.ExecutionOutcome)
	require.EqualValues(t, 2, observed.appends.Load())
	require.EqualValues(t, 1, f.count.Load())
}

func TestMCPApprovalRecoveryRetriesFailedAndSupersededProjection(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	call, effect := f.seed(t, store.ExternalEffectInFlight, "", true, nil)
	f.restart(t)
	observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
	observed.failNext.Store(true)
	f.dispatcher.EventStore = observed
	require.EqualError(t, f.reconcile(t), "injected approval projection outage")
	persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectOutcomeUnknown, persisted.State)
	before, _ := f.approval(t)
	require.Equal(t, "not_started", before.ExecutionOutcome)
	require.NoError(t, f.reconcile(t), "retry must release and reacquire the production epoch guard")
	approval, listed := f.approval(t)
	require.Equal(t, "unknown", approval.ExecutionOutcome)
	require.EqualValues(t, 2, observed.appends.Load())
	require.NoError(t, f.reconcile(t))
	require.EqualValues(t, 2, observed.appends.Load())

	// Model a delayed start-event append from before the crash. The same
	// effect version must still be able to repair this newer stale projection.
	require.NoError(t, f.broker.approvalOutcome(f.ctx, call, "running", "Approved action started", nil))
	late, _ := f.approval(t)
	require.Equal(t, "running", late.ExecutionOutcome)
	require.NoError(t, f.reconcile(t))
	repaired, repairedEvents := f.approval(t)
	require.Equal(t, "unknown", repaired.ExecutionOutcome)
	require.Len(t, repairedEvents, len(listed)+2)
	require.EqualValues(t, 3, observed.appends.Load())
	require.NoError(t, f.reconcile(t))
	require.EqualValues(t, 3, observed.appends.Load())
	require.EqualValues(t, 1, f.count.Load())
}

func TestMCPApprovalRecoveryPreservesCurrentLeaseAndPendingCalls(t *testing.T) {
	for _, state := range []store.ExternalEffectState{store.ExternalEffectPending, store.ExternalEffectInFlight} {
		t.Run(string(state), func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			_, effect := f.seed(t, state, "", false, nil)
			observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
			f.dispatcher.EventStore = observed
			_, before := f.approval(t)
			reads := f.exactReads.Load()
			require.NoError(t, f.reconcile(t))
			require.Equal(t, reads, f.exactReads.Load())
			_, after := f.approval(t)
			require.Equal(t, before, after)
			persisted, err := f.control.GetExternalEffectByIdentity(f.ctx, effect.Identity)
			require.NoError(t, err)
			require.Equal(t, effect.State, persisted.State)
			require.Equal(t, effect.Version, persisted.Version)
			if state == store.ExternalEffectPending {
				require.EqualValues(t, 1, observed.lists.Load(), "pending reviews need targeted expiry and prompt-liveness checks")
			} else {
				require.Zero(t, observed.lists.Load())
			}
			require.Zero(t, observed.sequences.Load())
			require.Zero(t, observed.sequenceBatches.Load(), "uncached Pending reviews and current live leases need no sequence query")
			require.Zero(t, f.count.Load())
		})
	}
}

func TestMCPApprovalRecoveryDoesNotAttachEvidenceToReusedTaskName(t *testing.T) {
	f := newMCPApprovalRecoveryFixture(t)
	_, _ = f.seed(t, store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
	f.restart(t)
	_, before := f.approval(t)
	require.NoError(t, f.kube.Delete(f.ctx, f.task))
	replacement := f.task.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "replacement-task-uid", ""
	require.NoError(t, f.kube.Create(f.ctx, replacement))
	require.NoError(t, f.reconcile(t))
	listed, err := approvals.ListEvents(f.ctx, f.events, replacement.Namespace, replacement.Name)
	require.NoError(t, err)
	require.Len(t, listed, len(before))
	require.Empty(t, approvals.Derive(approvals.FilterEventsForTaskUID(listed, string(replacement.UID)), time.Time{}))
}

func TestMCPApprovalRecoverySkipsMismatchedEffectsAndEmptyNamespaces(t *testing.T) {
	for _, mismatch := range []string{"no_effects", "other_namespace", "other_request", "other_operation", "other_task_hint", "unlabelled", "other_kind"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newMCPApprovalRecoveryFixture(t)
			_, effect := f.seed(t, store.ExternalEffectSucceeded, "running", true, json.RawMessage(`{"workOrder":"simulated-1"}`))
			observed := &approvalRecoveryEventStore{DeduplicatingExecutionEventStore: f.events}
			f.dispatcher.EventStore = observed
			candidate := acpMCPApprovalEffect{ExternalEffect: *effect, taskUID: f.task.UID}
			effects := map[string]acpMCPApprovalEffect{effect.ID: candidate}
			switch mismatch {
			case "no_effects":
				clear(effects)
			case "other_namespace":
				candidate.Identity.Namespace = "other"
				effects[effect.ID] = candidate
			case "other_request":
				candidate.RequestDigest = testControllerMCPDigest("other request")
				effects[effect.ID] = candidate
			case "other_operation":
				candidate.Identity.OperationID = "other-operation"
				effects[effect.ID] = candidate
			case "other_task_hint":
				candidate.taskUID = "another-task-uid"
				effects[effect.ID] = candidate
			case "unlabelled":
				candidate.taskUID = ""
				effects[effect.ID] = candidate
			case "other_kind":
				candidate.Identity.Kind = "workspace.prepare"
				effects[effect.ID] = candidate
			}
			require.NoError(t, f.dispatcher.reconcileMCPApprovalExecutions(f.ctx, f.fence, []corev1alpha1.Task{*f.task}, effects))
			require.Zero(t, observed.appends.Load())
			require.Zero(t, observed.sequences.Load())
			if mismatch != "other_request" && mismatch != "other_operation" {
				require.Zero(t, observed.lists.Load(), "no candidate effects means no per-Task event reads")
				require.Zero(t, observed.sequenceBatches.Load())
			}
		})
	}
}

func TestMCPApprovalReceiptOutcomeValidatesDenialAndCompletedReceipts(t *testing.T) {
	const approvalID = "approval-under-test"
	for _, code := range []string{"approval_declined", "approval_expired", "approval_cancelled", "approval_stale"} {
		t.Run(code, func(t *testing.T) {
			response := acpApprovalError(approvalID, code)
			effect := &store.ExternalEffect{State: store.ExternalEffectFailed, Response: response, ResponseDigest: store.CanonicalBytesDigest(response)}
			outcome, reason, result, err := acpMCPApprovalReceiptOutcome(effect, approvalID)
			require.NoError(t, err)
			require.Equal(t, "not_started", outcome)
			require.Equal(t, code, reason)
			require.JSONEq(t, string(response), string(result))
			_, _, _, err = acpMCPApprovalReceiptOutcome(effect, "another-approval")
			require.Error(t, err)
			// A tool may itself return an error with a denial-shaped code. Its
			// Succeeded ledger state proves execution, not a broker denial.
			effect.State = store.ExternalEffectSucceeded
			outcome, _, _, err = acpMCPApprovalReceiptOutcome(effect, approvalID)
			require.NoError(t, err)
			require.Equal(t, "failed", outcome)
		})
	}
}

func TestMCPApprovalReceiptOutcomeCanonicalizesStoredJSON(t *testing.T) {
	canonical := json.RawMessage(`{"a":[1,100,9007199254740993],"isError":false,"z":0.0000001}`)
	for _, stored := range []json.RawMessage{
		canonical,
		json.RawMessage(`{ "z":1e-7, "isError":false, "a":[1.0,1e2,9007199254740993] }`),
	} {
		effect := &store.ExternalEffect{
			State: store.ExternalEffectSucceeded, Response: stored, ResponseDigest: store.CanonicalBytesDigest(canonical),
		}
		outcome, _, result, err := acpMCPApprovalReceiptOutcome(effect, "approval-under-test")
		require.NoError(t, err)
		require.Equal(t, "succeeded", outcome)
		require.Equal(t, canonical, result)
	}

	// A digest for a different JSON value never validates just because both
	// encodings are well formed or have the same fields.
	changed := &store.ExternalEffect{
		State: store.ExternalEffectSucceeded, Response: json.RawMessage(`{"a":[1,100,9007199254740992],"isError":false,"z":1e-7}`),
		ResponseDigest: store.CanonicalBytesDigest(canonical),
	}
	_, _, result, err := acpMCPApprovalReceiptOutcome(changed, "approval-under-test")
	require.Error(t, err)
	require.Empty(t, result)
}

func TestMCPApprovalRecoveryRejectsUnverifiableReceipt(t *testing.T) {
	for _, response := range []json.RawMessage{
		nil, json.RawMessage(`{`), json.RawMessage(`{"code":"approval_declined","approvalID":"approval-under-test"}`),
		acpApprovalError("another-approval", "approval_declined"), acpApprovalError("approval-under-test", "tool_outcome_unknown"),
	} {
		effect := &store.ExternalEffect{
			State: store.ExternalEffectFailed, Response: response, ResponseDigest: store.CanonicalBytesDigest(response),
		}
		outcome, _, result := mcpApprovalRecoveredOutcome(effect, "approval-under-test")
		require.Equal(t, "unknown", outcome)
		require.Empty(t, result)
	}
	response := json.RawMessage(`{"workOrder":"simulated-1"}`)
	effect := &store.ExternalEffect{State: store.ExternalEffectSucceeded, Response: response, ResponseDigest: testControllerMCPDigest("wrong receipt")}
	outcome, _, result := mcpApprovalRecoveredOutcome(effect, "approval-under-test")
	require.Equal(t, "unknown", outcome)
	require.Empty(t, result)
}
