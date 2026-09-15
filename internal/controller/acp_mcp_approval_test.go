package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storesqlite "github.com/orka-agents/orka/internal/store/sqlite"
)

type approvalTestGuard struct{ sync.Mutex }

func (g *approvalTestGuard) WithControllerEpochMutation(ctx context.Context, _ store.ControllerEpochFence, fn func(context.Context) error) error {
	g.Lock()
	defer g.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

type mcpApprovalFixture struct {
	t          *testing.T
	broker     *ACPMCPBroker
	request    harnessv2.MCPBrokerCallRequest
	profile    harnessv2.RuntimeProfile
	events     *storesqlite.Store
	active     atomic.Bool
	authorized atomic.Bool
	count      atomic.Int32
	reopen     func()
}

func newMCPApprovalFixture(t *testing.T) *mcpApprovalFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "approval.db")
	connection, err := storesqlite.NewDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	db := storesqlite.NewStore(connection, dbPath)
	epoch, err := db.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1, HolderID: "controller-a",
		RequestDigest: testControllerMCPDigest("epoch"), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	fence := store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
	request, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectConsequential)
	request.Authorization.ApprovalPolicy.RequiredTools = []string{request.Call.ToolName}
	request.Authorization.ApprovalPolicyDigest, _ = harnessv2.CanonicalMCPApprovalPolicyDigest(request.Authorization.ApprovalPolicy)
	profile.ApprovalPolicyDigest = request.Authorization.ApprovalPolicyDigest
	profile.ProviderKind = "agentkit"
	request.Metadata.RequestDigest, _ = harnessv2.CanonicalRequestDigest(request)
	f := &mcpApprovalFixture{t: t, request: request, profile: profile, events: db}
	f.active.Store(true)
	f.authorized.Store(true)
	f.broker = &ACPMCPBroker{
		Credentials: ACPMCPBrokerCredentialResolverFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
			if !f.authorized.Load() {
				return ACPMCPBrokerCredentials{}, errors.New("authority changed")
			}
			return ACPMCPBrokerCredentials{
				ControllerBearerToken: strings.Repeat("b", 32), CapabilitySecret: []byte(strings.Repeat("c", 32)),
				ExpectedFence: got.Metadata.Fence, RuntimeProfile: f.profile, ControllerFence: fence,
				Task: ACPMCPAuthenticatedTask{Name: "approval-task", Namespace: request.Namespace, UID: string(request.Metadata.TaskUID)},
			}, nil
		}),
		Prompts: ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error {
			if !f.active.Load() {
				return errors.New("prompt ended")
			}
			return nil
		}),
		Executor: ACPMCPToolExecutorFunc(func(_ context.Context, got harnessv2.MCPBrokerCallRequest, _ harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			f.count.Add(1)
			if string(got.Call.Arguments) != string(request.Call.Arguments) {
				return nil, errors.New("executed changed arguments")
			}
			return json.RawMessage(`{"workOrder":"simulated-1"}`), nil
		}),
		Effects: db, EpochMutations: &approvalTestGuard{}, ApprovalEvents: db,
		ApprovalSecrets: k8sfake.NewClientset(), ApprovalPollInterval: 5 * time.Millisecond,
	}
	f.reopen = func() {
		if err := connection.Close(); err != nil {
			t.Fatal(err)
		}
		connection, err = storesqlite.NewDB(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		f.events = storesqlite.NewStore(connection, dbPath)
		recovered := *f.broker
		recovered.Effects = f.events
		recovered.ApprovalEvents = f.events
		f.broker = &recovered
	}
	return f
}

func (f *mcpApprovalFixture) start(request harnessv2.MCPBrokerCallRequest) <-chan *httptest.ResponseRecorder {
	return f.startContext(f.t.Context(), request)
}

func (f *mcpApprovalFixture) startContext(ctx context.Context, request harnessv2.MCPBrokerCallRequest) <-chan *httptest.ResponseRecorder {
	f.t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- performMCPBrokerCall(f.t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.broker.ServeHTTP(w, r.WithContext(ctx))
		}), request, strings.Repeat("b", 32), []byte(strings.Repeat("c", 32)))
	}()
	return done
}

func (f *mcpApprovalFixture) pending() approvals.Approval {
	f.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		listed, err := approvals.ListEvents(f.t.Context(), f.events, f.request.Namespace, "approval-task")
		if err != nil {
			f.t.Fatal(err)
		}
		for _, value := range approvals.Derive(listed, time.Time{}) {
			if value.Status == approvals.StatusPending {
				return value
			}
		}
		select {
		case <-deadline:
			f.t.Fatal("no durable pending approval")
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *mcpApprovalFixture) decide(id, eventType string) {
	f.t.Helper()
	content, _ := json.Marshal(map[string]string{"approvalID": id, "taskUID": string(f.request.Metadata.TaskUID), "actor": "reviewer"})
	_, err := f.events.AppendExecutionEvent(f.t.Context(), &store.ExecutionEvent{
		Namespace: f.request.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: "approval-task", TaskName: "approval-task",
		Type: eventType, ToolCallID: id, Summary: "reviewer decision", Content: content,
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

func awaitMCPApprovalResult(t *testing.T, done <-chan *httptest.ResponseRecorder) harnessv2.MCPBrokerCallResponse {
	t.Helper()
	select {
	case response := <-done:
		if response.Code != http.StatusOK {
			t.Fatalf("broker response status=%d body=%s", response.Code, response.Body.String())
		}
		var result harnessv2.MCPBrokerCallResponse
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("approval call did not finish")
		return harnessv2.MCPBrokerCallResponse{}
	}
}

func TestMCPApprovalPersistsExactCallAndReplaysOnlyItsRealResult(t *testing.T) {
	f := newMCPApprovalFixture(t)
	done := f.start(f.request)
	pending := f.pending()
	if f.count.Load() != 0 || pending.ExecutionOutcome != "not_started" || pending.Binding == nil {
		t.Fatal("pending review executed or lost its exact call binding")
	}
	secrets, err := f.broker.ApprovalSecrets.CoreV1().Secrets(f.request.Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 1 || secrets.Items[0].Immutable == nil || !*secrets.Items[0].Immutable ||
		metav1.GetControllerOf(&secrets.Items[0]).UID != types.UID(f.request.Metadata.TaskUID) {
		t.Fatalf("original action was not stored in an immutable Task-owned Secret: %v", err)
	}
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	if result.IsError || result.Replayed || string(result.Result) != `{"workOrder":"simulated-1"}` || f.count.Load() != 1 {
		t.Fatalf("approved result=%s replayed=%t executions=%d", result.Result, result.Replayed, f.count.Load())
	}
	// Reopen SQLite and reconstruct the handler without any in-memory receipt.
	f.reopen()
	replay := awaitMCPApprovalResult(t, f.start(f.request))
	if !replay.Replayed || string(replay.Result) != string(result.Result) || f.count.Load() != 1 {
		t.Fatal("redelivery repeated the action instead of its committed result")
	}
	listed, _ := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	values := approvals.Derive(listed, time.Time{})
	if len(values) != 1 || values[0].DecisionActor != "reviewer" || values[0].ExecutionOutcome != "succeeded" {
		t.Fatalf("approval history does not include reviewer and outcome: %#v", values)
	}
}

func TestMCPApprovalDenialExpiryAndAuthorityLossNeverExecute(t *testing.T) {
	for _, test := range []struct {
		name, decision, code string
		revoke               bool
		expire               bool
	}{
		{name: "decline", decision: events.ExecutionEventTypeApprovalDeclined, code: "approval_declined"},
		{name: "explicit cancel", decision: events.ExecutionEventTypeApprovalCancelled, code: "approval_cancelled"},
		{name: "expiry", code: "approval_expired", expire: true},
		{name: "approval after policy revocation", decision: events.ExecutionEventTypeApprovalApproved, code: "approval_stale", revoke: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			if test.expire {
				f.broker.ApprovalWaitTimeout = 60 * time.Millisecond
			}
			done := f.start(f.request)
			pending := f.pending()
			if test.revoke {
				f.broker.EpochMutations.(*approvalTestGuard).Lock()
			}
			if test.decision != "" {
				f.decide(pending.ID, test.decision)
			}
			if test.revoke {
				f.authorized.Store(false)
				f.broker.EpochMutations.(*approvalTestGuard).Unlock()
			}
			result := awaitMCPApprovalResult(t, done)
			if !result.IsError || !strings.Contains(string(result.Result), test.code) || f.count.Load() != 0 {
				t.Fatalf("result=%s executions=%d", result.Result, f.count.Load())
			}
			if test.revoke {
				f.authorized.Store(true)
				f.reopen()
				replay := awaitMCPApprovalResult(t, f.start(f.request))
				if !replay.Replayed || string(replay.Result) != string(result.Result) || f.count.Load() != 0 {
					t.Fatal("restored authority revived a call that already returned a final denial")
				}
			}
		})
	}
}

func TestMCPApprovalChangedInputsCannotBorrowDecision(t *testing.T) {
	f := newMCPApprovalFixture(t)
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	awaitMCPApprovalResult(t, done)
	for _, mutate := range []func(*harnessv2.MCPBrokerCallRequest){
		func(r *harnessv2.MCPBrokerCallRequest) { r.Call.Arguments = json.RawMessage(`{"value":"different"}`) },
		func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.OperationID = "different-operation" },
		func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.RuntimeSessionGeneration++ },
	} {
		changed := f.request
		mutate(&changed)
		changed.Metadata.RequestDigest, _ = harnessv2.CanonicalRequestDigest(changed)
		response := performMCPBrokerCall(t, f.broker, changed, strings.Repeat("b", 32), []byte(strings.Repeat("c", 32)))
		if response.Code == http.StatusOK || f.count.Load() != 1 {
			t.Fatalf("altered call accepted: status=%d executions=%d", response.Code, f.count.Load())
		}
	}
}

type approvalLostReceiptStore struct{ store.ExternalEffectStore }

func (s approvalLostReceiptStore) TransitionExternalEffect(ctx context.Context, change store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	if change.NewState == store.ExternalEffectSucceeded {
		return nil, errors.New("simulated interrupted result commit")
	}
	return s.ExternalEffectStore.TransitionExternalEffect(ctx, change)
}

func TestMCPApprovalLostReceiptNeverRepeatsStartedAction(t *testing.T) {
	f := newMCPApprovalFixture(t)
	f.broker.Effects = approvalLostReceiptStore{f.broker.Effects}
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	first := awaitMCPApprovalResult(t, done)
	if !first.IsError || !strings.Contains(string(first.Result), "tool_outcome_unknown") || f.count.Load() != 1 {
		t.Fatalf("interrupted receipt result=%s count=%d", first.Result, f.count.Load())
	}
	for range 2 {
		result := awaitMCPApprovalResult(t, f.start(f.request))
		if !strings.Contains(string(result.Result), "tool_outcome_unknown") || f.count.Load() != 1 {
			t.Fatal("unknown approved action was repeated")
		}
	}
}

func TestMCPApprovalPendingRecoveryAndIndependentSessionProgress(t *testing.T) {
	f := newMCPApprovalFixture(t)
	done := f.start(f.request)
	pending := f.pending()
	// The approval waiter holds no global broker or controller epoch lock.
	other := f.request
	other.Call.CallID = "unrelated-read"
	other.Metadata.OperationID = "unrelated-operation"
	other.Metadata.Fence.RuntimeSessionUID = "unrelated-session"
	other.Authorization.RuntimeSessionUID = "unrelated-session"
	other.Authorization.ApprovalPolicy.RequiredTools = nil
	other.Authorization.ApprovalPolicyDigest, _ = harnessv2.CanonicalMCPApprovalPolicyDigest(other.Authorization.ApprovalPolicy)
	other.Metadata.RequestDigest, _ = harnessv2.CanonicalRequestDigest(other)
	separate := *f.broker
	separate.Credentials = ACPMCPBrokerCredentialResolverFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
		creds, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(ctx, request)
		creds.RuntimeProfile.ApprovalPolicyDigest = other.Authorization.ApprovalPolicyDigest
		return creds, err
	})
	response := performMCPBrokerCall(t, &separate, other, strings.Repeat("b", 32), []byte(strings.Repeat("c", 32)))
	if response.Code != http.StatusOK || f.count.Load() != 1 {
		t.Fatal("unrelated session could not progress during approval wait")
	}
	// Simulate exact redelivery while the original waiter still exists. Either
	// caller may claim, but neither may execute the other's in-flight action.
	recovered := f.start(f.request)
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	awaitMCPApprovalResult(t, done)
	awaitMCPApprovalResult(t, recovered)
	if f.count.Load() != 2 {
		t.Fatalf("duplicate pending delivery executions=%d, want one lookup and one approved action", f.count.Load())
	}
}

func TestMCPApprovalTransportRecoveryPreservesPendingAndRenewedLease(t *testing.T) {
	f := newMCPApprovalFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := f.startContext(ctx, f.request)
	pending := f.pending()
	cancel()
	select {
	case response := <-done:
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("interrupted transport returned status %d", response.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted approval waiter did not stop")
	}
	f.reopen()
	if recovered := f.pending(); recovered.ID != pending.ID || f.count.Load() != 0 {
		t.Fatal("transport recovery lost the pending approval or executed it")
	}
	// The transport lease can renew without changing the stored operation.
	renewed := f.request
	renewed.Lease.Generation++
	renewed.Authorization.LeaseGeneration = renewed.Lease.Generation
	renewed.Lease.IssuedAt = time.Now().UTC()
	renewed.Lease.ExpiresAt = renewed.Lease.IssuedAt.Add(2 * time.Minute)
	renewed.Authorization.ExpiresAt = renewed.Lease.IssuedAt.Add(time.Minute)
	renewed.Metadata.ExpiresAt = renewed.Lease.IssuedAt.Add(time.Minute)
	renewed.Metadata.RequestDigest, _ = harnessv2.CanonicalRequestDigest(renewed)
	done = f.start(renewed)
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	if result.IsError || f.count.Load() != 1 {
		t.Fatalf("original call did not resume once: result=%s count=%d", result.Result, f.count.Load())
	}
}

func TestMCPApprovalExpiredInFlightLeaseCannotBeReclaimed(t *testing.T) {
	f := newMCPApprovalFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := f.startContext(ctx, f.request)
	pending := f.pending()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("approval waiter did not stop")
	}
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	identity := store.ExternalEffectIdentity{
		Kind: "acp-mcp-tool", Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
	}
	id, _ := identity.CanonicalID()
	effect, err := f.events.GetExternalEffect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(t.Context(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-2 * time.Minute)
	expired := started.Add(time.Minute)
	claimed, err := f.events.TransitionExternalEffect(t.Context(), store.ExternalEffectTransition{
		ID: id, Fence: credentials.ControllerFence, ExpectedVersion: effect.Version,
		ExpectedState: store.ExternalEffectPending, NewState: store.ExternalEffectInFlight,
		RequestDigest: effect.RequestDigest, LeaseOwner: "interrupted-controller", LeaseExpiresAt: &expired, UpdatedAt: started,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash after the action reached the simulator but before a receipt.
	f.count.Store(1)
	f.reopen()
	result := awaitMCPApprovalResult(t, f.start(f.request))
	current, err := f.events.GetExternalEffect(t.Context(), id)
	if err != nil || !result.IsError || !strings.Contains(string(result.Result), "tool_outcome_unknown") ||
		f.count.Load() != 1 || current.Attempts != claimed.Attempts || current.LeaseOwner != claimed.LeaseOwner {
		t.Fatalf("expired execution lease was reclaimed: count=%d err=%v", f.count.Load(), err)
	}
}

func TestMCPApprovalWrappedOperationHasSafePreview(t *testing.T) {
	f := newMCPApprovalFixture(t)
	request := f.request
	request.Call.ToolName = "call-tool"
	request.Call.Arguments = json.RawMessage(`{"name":"create_work_order","inputs":{"asset":"pump-7","access_token":"private-test-value","nested":{"password":"private-test-password"}}}`)
	descriptor := request.Authorization.ToolPolicy.Tools[0]
	descriptor.Name = request.Call.ToolName
	call, _, err := f.broker.persistApprovalCall(t.Context(), request, descriptor, ACPMCPAuthenticatedTask{
		Name: "approval-task", Namespace: request.Namespace, UID: string(request.Metadata.TaskUID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.broker.requestToolApproval(t.Context(), call); err != nil {
		t.Fatal(err)
	}
	preview := f.pending()
	if preview.Action != "Execute create_work_order via call-tool" ||
		!strings.Contains(string(preview.TargetArgsPreview), "pump-7") ||
		strings.Contains(string(preview.TargetArgsPreview), "private-test-") || f.count.Load() != 0 {
		t.Fatal("wrapped operation or safe input preview is incorrect")
	}
	stored, _, err := f.broker.loadApprovalCall(t.Context(), call)
	if err != nil || string(stored.Request.Call.Arguments) != string(request.Call.Arguments) {
		t.Fatal("private executable inputs were replaced with the sanitized preview")
	}
}

func TestMCPApprovalExecutionFailureRemainsDistinctFromDecline(t *testing.T) {
	f := newMCPApprovalFixture(t)
	f.broker.Executor = ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		f.count.Add(1)
		return json.RawMessage(`{"isError":true,"code":"tool_execution_failed","error":"MCP tool execution failed"}`), nil
	})
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	if !result.IsError || !strings.Contains(string(result.Result), "tool_execution_failed") || f.count.Load() != 1 {
		t.Fatal("approved execution failure was confused with review denial")
	}
	f.reopen()
	replay := awaitMCPApprovalResult(t, f.start(f.request))
	listed, _ := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	values := approvals.Derive(listed, time.Time{})
	if !replay.Replayed || f.count.Load() != 1 || len(values) != 1 || values[0].Status != approvals.StatusApproved || values[0].ExecutionOutcome != "failed" {
		t.Fatal("approved tool error lost its decision, outcome, or replay receipt")
	}
}

type approvalObservedEffectStore struct {
	store.ExternalEffectStore
	reads chan struct{}
}

func (s approvalObservedEffectStore) GetExternalEffect(ctx context.Context, id string) (*store.ExternalEffect, error) {
	effect, err := s.ExternalEffectStore.GetExternalEffect(ctx, id)
	if err == nil {
		select {
		case s.reads <- struct{}{}:
		default:
		}
	}
	return effect, err
}

func TestMCPApprovalPendingPollsDeferFullRevalidationUntilDecision(t *testing.T) {
	f := newMCPApprovalFixture(t)
	resolver := f.broker.Credentials
	var resolutions atomic.Int32
	f.broker.Credentials = ACPMCPBrokerCredentialResolverFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
		resolutions.Add(1)
		return resolver.ResolveACPMCPBrokerCredentials(ctx, request)
	})
	reads := make(chan struct{}, 3)
	f.broker.Effects = approvalObservedEffectStore{ExternalEffectStore: f.broker.Effects, reads: reads}
	done := f.start(f.request)
	pending := f.pending()
	for range 3 {
		select {
		case <-reads:
		case <-time.After(5 * time.Second):
			t.Fatal("pending approval did not continue polling")
		}
	}
	if got := resolutions.Load(); got != 1 {
		t.Fatalf("pending approval resolved credentials %d times, want only initial authentication", got)
	}
	// A pending review does not authorize execution. The full check must still
	// reject authority changed during the wait before any tool can start.
	f.authorized.Store(false)
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	if !result.IsError || !strings.Contains(string(result.Result), "approval_stale") || f.count.Load() != 0 || resolutions.Load() < 2 {
		t.Fatal("approved call did not revalidate changed authority before execution")
	}
}

func TestMCPApprovalPendingPromptRevocationStopsWait(t *testing.T) {
	f := newMCPApprovalFixture(t)
	done := f.start(f.request)
	pending := f.pending()
	f.active.Store(false)

	result := awaitMCPApprovalResult(t, done)
	if !result.IsError || !strings.Contains(string(result.Result), "approval_stale") || f.count.Load() != 0 {
		t.Fatal("revoked prompt kept its pending wait or started the tool")
	}
	listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	if err != nil {
		t.Fatal(err)
	}
	values := approvals.Derive(listed, time.Time{})
	if len(values) != 1 || values[0].ID != pending.ID || values[0].Status != approvals.StatusCancelled || values[0].ExecutionOutcome != "not_started" {
		t.Fatal("revoked pending approval lost its cancellation evidence")
	}
}

type approvalCancelDuringRevocationStore struct {
	store.DeduplicatingExecutionEventStore
	cancel    context.CancelFunc
	cancelled atomic.Bool
}

func (s *approvalCancelDuringRevocationStore) AppendExecutionEventIfAbsent(ctx context.Context, event *store.ExecutionEvent, key string) (*store.ExecutionEvent, bool, error) {
	if event.Type == events.ExecutionEventTypeApprovalCancelled {
		// The prompt-authority watcher can cancel the original call after
		// revalidation rejects it but before the denial evidence is saved.
		s.cancelled.Store(true)
		s.cancel()
	}
	return s.DeduplicatingExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, key)
}

func TestMCPApprovalRevocationSettlesWhenCallerContextEnds(t *testing.T) {
	f := newMCPApprovalFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	storage := &approvalCancelDuringRevocationStore{DeduplicatingExecutionEventStore: f.events, cancel: cancel}
	f.broker.ApprovalEvents = storage
	done := f.startContext(ctx, f.request)
	pending := f.pending()
	f.authorized.Store(false)
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)

	result := awaitMCPApprovalResult(t, done)
	if !storage.cancelled.Load() || !result.IsError || !strings.Contains(string(result.Result), "approval_stale") || f.count.Load() != 0 {
		t.Fatal("caller cancellation lost the definitive unstarted result")
	}
	listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	if err != nil {
		t.Fatal(err)
	}
	values := approvals.Derive(listed, time.Time{})
	if len(values) != 1 || values[0].Status != approvals.StatusApproved || values[0].ExecutionOutcome != "not_started" {
		t.Fatal("caller cancellation lost the saved review decision or denial outcome")
	}
}
