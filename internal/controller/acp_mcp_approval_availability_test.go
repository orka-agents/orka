package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func TestMCPApprovalPendingCallSurvivesTransientAuthorityReads(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{name: "unavailable", err: errors.New("simulated read outage")},
		{name: "timeout", err: apierrors.NewTimeoutError("simulated read timeout", 1)},
	} {
		t.Run(failure.name, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			var unavailable atomic.Bool
			configureApprovalPromptReadFailure(t, f, &unavailable, failure.err)
			authorizer := f.broker.Prompts
			failedReads := make(chan struct{}, 2)
			f.broker.Prompts = ACPMCPPromptAuthorizerFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
				err := authorizer.AuthorizeACPMCPPrompt(ctx, request)
				if errors.Is(err, errACPMCPAuthorityUnavailable) {
					select {
					case failedReads <- struct{}{}:
					default:
					}
				}
				return err
			})
			done := f.start(f.request)
			pending := f.pending()
			unavailable.Store(true)
			// Wait for repeated watchdog reads, not just a delay that could
			// pass without encountering the outage.
			for range 2 {
				select {
				case <-failedReads:
				case response := <-done:
					t.Fatalf("pending call ended during a transient authority outage: HTTP %d", response.Code)
				case <-time.After(4 * time.Second):
					t.Fatal("pending approval did not retry the authority read")
				}
			}
			require.Zero(t, f.count.Load())
			unavailable.Store(false)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			result := awaitMCPApprovalResult(t, done)
			require.False(t, result.IsError)
			require.False(t, result.Replayed, "the original waiting HTTP call must finish without redelivery")
			require.EqualValues(t, 1, f.count.Load())
			f.reopen()
			replayed := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replayed.Replayed)
			require.JSONEq(t, string(result.Result), string(replayed.Result))
			require.EqualValues(t, 1, f.count.Load())
		})
	}
}

func TestMCPApprovalPendingCallStopsOnDefinitiveAuthorityLoss(t *testing.T) {
	f := newMCPApprovalFixture(t)
	var missing atomic.Bool
	configureApprovalPromptReadFailure(t, f, &missing, store.ErrNotFound)
	done := f.start(f.request)
	f.pending()
	missing.Store(true)
	result := awaitMCPApprovalResult(t, done)
	require.True(t, result.IsError)
	require.Contains(t, string(result.Result), acpApprovalCodeStale)
	require.Zero(t, f.count.Load())
	missing.Store(false)
	replayed := awaitMCPApprovalResult(t, f.start(f.request))
	require.True(t, replayed.Replayed)
	require.JSONEq(t, string(result.Result), string(replayed.Result))
	require.Zero(t, f.count.Load())
}

func TestMCPApprovalExecutingCallStopsOnAuthorityReadFailure(t *testing.T) {
	f := newMCPApprovalFixture(t)
	var unavailable atomic.Bool
	configureApprovalPromptReadFailure(t, f, &unavailable, errors.New("simulated read outage"))
	started := make(chan struct{})
	f.broker.Executor = ACPMCPToolExecutorFunc(func(ctx context.Context, _ harnessv2.MCPBrokerCallRequest, _ harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		f.count.Add(1)
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("approved tool did not start")
	}
	unavailable.Store(true)
	result := awaitMCPApprovalResult(t, done)
	require.True(t, result.IsError)
	require.Contains(t, string(result.Result), acpApprovalCodeUnknown)
	require.EqualValues(t, 1, f.count.Load())
	unavailable.Store(false)
	f.reopen()
	replayed := awaitMCPApprovalResult(t, f.start(f.request))
	require.True(t, replayed.IsError)
	require.Contains(t, string(replayed.Result), acpApprovalCodeUnknown)
	require.EqualValues(t, 1, f.count.Load(), "an interrupted execution must never become retryable")
}

func TestMCPApprovalReadFailuresPreserveApprovedCall(t *testing.T) {
	for _, source := range []string{"secret", "tool", "prompt"} {
		for _, failure := range []struct {
			name string
			err  error
		}{
			{name: "unavailable", err: errors.New("simulated read outage")},
			{name: "timeout", err: apierrors.NewTimeoutError("simulated read timeout", 1)},
			{name: "missing", err: apierrors.NewNotFound(schema.GroupResource{Resource: source}, "approval")},
		} {
			t.Run(source+"/"+failure.name, func(t *testing.T) {
				f := newMCPApprovalFixture(t)
				var unavailable atomic.Bool
				failedReads := make(chan struct{}, 2)
				observeFailure := func() error {
					select {
					case failedReads <- struct{}{}:
					default:
					}
					return failure.err
				}
				switch source {
				case "secret":
					f.broker.ApprovalSecrets.(*k8sfake.Clientset).PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
						if unavailable.Load() {
							return true, nil, observeFailure()
						}
						return false, nil, nil
					})
				case "prompt":
					configureApprovalPromptReadFailure(t, f, &unavailable, failure.err)
					authorizer := f.broker.Prompts
					f.broker.Prompts = ACPMCPPromptAuthorizerFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
						err := authorizer.AuthorizeACPMCPPrompt(ctx, request)
						if err != nil && unavailable.Load() {
							_ = observeFailure()
						}
						return err
					})
				default:
					tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: f.request.Call.ToolName, Namespace: f.request.Namespace},
						Spec: corev1alpha1.ToolSpec{Description: "counted approval test tool", Parameters: &apiextensionsJSONForMCPTest,
							BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite}}
					descriptor, err := customACPMCPToolDescriptor(tool)
					require.NoError(t, err)
					f.request.Authorization.ToolPolicy.Tools = []harnessv2.MCPToolDescriptor{descriptor}
					f.request.Authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(f.request.Authorization.ToolPolicy.Tools)
					require.NoError(t, err)
					f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
					require.NoError(t, err)
					scheme := runtime.NewScheme()
					require.NoError(t, corev1alpha1.AddToScheme(scheme))
					reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool).WithInterceptorFuncs(interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
							if unavailable.Load() {
								return observeFailure()
							}
							return c.Get(ctx, key, object, opts...)
						},
					}).Build()
					f.broker.Executor = approvalLeaseValidationExecutor{ACPMCPToolExecutor: f.broker.Executor,
						validator: RegistryACPMCPToolExecutor{Reader: reader}}
				}
				done := f.start(f.request)
				pending := f.pending()
				unavailable.Store(true)
				f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
				missing := failure.name == "missing"
				if missing {
					result := awaitMCPApprovalResult(t, done)
					require.True(t, result.IsError)
					require.Contains(t, string(result.Result), "approval_stale")
				} else {
					requireApprovalReadRetrying(t, f, pending.ID, approvals.StatusApproved, done, failedReads)
				}
				require.Zero(t, f.count.Load())
				unavailable.Store(false)
				if missing {
					f.reopen()
					resumed := awaitMCPApprovalResult(t, f.start(f.request))
					require.True(t, resumed.IsError && resumed.Replayed)
					require.Contains(t, string(resumed.Result), "approval_stale")
					require.Zero(t, f.count.Load(), "restoring a verified missing input must not revive its denied action")
				} else {
					resumed := awaitMCPApprovalResult(t, done)
					require.False(t, resumed.IsError)
					require.False(t, resumed.Replayed, "recovery must finish the original HTTP call")
					require.JSONEq(t, `{"workOrder":"simulated-1"}`, string(resumed.Result))
					require.EqualValues(t, 1, f.count.Load())
					f.reopen()
					replay := performMCPBrokerCall(t, f.broker, f.request, strings.Repeat("b", 32), []byte(strings.Repeat("c", 32)))
					require.Equal(t, http.StatusOK, replay.Code)
					require.EqualValues(t, 1, f.count.Load())
				}
			})
		}
	}
}

func requireApprovalReadUnavailable(t *testing.T, f *mcpApprovalFixture, id, status string, done <-chan *httptest.ResponseRecorder) {
	t.Helper()
	select {
	case response := <-done:
		require.Equal(t, http.StatusServiceUnavailable, response.Code)
		require.NotContains(t, response.Body.String(), "simulated")
	case <-time.After(5 * time.Second):
		t.Fatal("unavailable authority did not return a bounded failure")
	}
	requireApprovalUnclaimed(t, f, id, status)
}

func requireApprovalReadRetrying(t *testing.T, f *mcpApprovalFixture, id, status string, done <-chan *httptest.ResponseRecorder, failedReads <-chan struct{}) {
	t.Helper()
	for range 2 {
		select {
		case <-failedReads:
		case response := <-done:
			t.Fatalf("original approval call ended during a recoverable read outage: HTTP %d", response.Code)
		case <-time.After(3 * time.Second):
			t.Fatal("unclaimed approval did not retry the failed read")
		}
	}
	select {
	case response := <-done:
		t.Fatalf("original approval call ended before read recovery: HTTP %d", response.Code)
	default:
	}
	requireApprovalUnclaimed(t, f, id, status)
}

func requireApprovalUnclaimed(t *testing.T, f *mcpApprovalFixture, id, status string) {
	t.Helper()
	listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	require.NoError(t, err)
	values := approvals.Derive(listed, time.Time{})
	require.Len(t, values, 1)
	require.Equal(t, id, values[0].ID)
	require.Equal(t, status, values[0].Status)
	require.Equal(t, "not_started", values[0].ExecutionOutcome)
	identity := store.ExternalEffectIdentity{Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID)}
	effectID, err := identity.CanonicalID()
	require.NoError(t, err)
	effect, err := f.events.GetExternalEffect(t.Context(), effectID)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectPending, effect.State)
	require.Zero(t, f.count.Load())
}

type approvalUnavailablePromptStore struct {
	store.PromptAttemptStore
	unavailable *atomic.Bool
	err         error
}

func (s approvalUnavailablePromptStore) GetPromptAttempt(ctx context.Context, id string) (*store.PromptAttempt, error) {
	if s.unavailable.Load() {
		return nil, s.err
	}
	return s.PromptAttemptStore.GetPromptAttempt(ctx, id)
}

func configureApprovalPromptReadFailure(t *testing.T, f *mcpApprovalFixture, unavailable *atomic.Bool, err error) {
	t.Helper()
	attempt := &store.PromptAttempt{
		Key:        mcpPromptLeaseKey(f.request.Namespace, f.request.Metadata),
		SessionUID: string(f.request.Authorization.RuntimeSessionUID), RuntimeInstanceID: string(f.request.Metadata.Fence.RuntimeInstanceID),
		ControllerEpoch: int64(f.request.Metadata.Fence.ControllerEpoch), ExecutionState: store.PromptExecutionRunning,
	}
	registry := &ACPMCPPromptLeaseRegistry{}
	registerApprovalLease(t, registry, t.Context(), f.request)
	f.broker.Prompts = DurableACPMCPPromptAuthorizer{PromptLeases: registry,
		Attempts: approvalUnavailablePromptStore{PromptAttemptStore: staticPromptAttemptStore{attempt: attempt}, unavailable: unavailable, err: err}}
}

func TestMCPApprovalInterruptedDuringReadOutagePreservesReview(t *testing.T) {
	f := newMCPApprovalFixture(t)
	var unavailable atomic.Bool
	configureApprovalPromptReadFailure(t, f, &unavailable, errors.New("simulated prompt read outage"))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := f.startContext(ctx, f.request)
	pending := f.pending()
	unavailable.Store(true)
	cancel()
	requireApprovalReadUnavailable(t, f, pending.ID, approvals.StatusPending, done)
	unavailable.Store(false)
	f.reopen()
	done = f.start(f.request)
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	require.False(t, result.IsError)
	require.EqualValues(t, 1, f.count.Load())
}

func TestMCPApprovalReadOutageAfterExecutionNeverReplaysAction(t *testing.T) {
	f := newMCPApprovalFixture(t)
	var unavailable atomic.Bool
	configureApprovalPromptReadFailure(t, f, &unavailable, errors.New("simulated post-execution read outage"))
	executor := f.broker.Executor
	f.broker.Executor = ACPMCPToolExecutorFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		result, err := executor.ExecuteACPMCPTool(ctx, request, descriptor)
		unavailable.Store(true)
		return result, err
	})
	done := f.start(f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	require.True(t, result.IsError)
	require.Contains(t, string(result.Result), "tool_outcome_unknown")
	require.EqualValues(t, 1, f.count.Load())
	unavailable.Store(false)
	f.reopen()
	replay := awaitMCPApprovalResult(t, f.start(f.request))
	require.True(t, replay.IsError)
	require.Contains(t, string(replay.Result), "tool_outcome_unknown")
	require.EqualValues(t, 1, f.count.Load())
}

type approvalUnavailableEpochGuard struct {
	store.ControllerEpochMutationStore
	unavailable *atomic.Bool
	err         error
	onRead      func()
}

func (g approvalUnavailableEpochGuard) WithControllerEpochMutation(ctx context.Context, fence store.ControllerEpochFence, access func(context.Context) error) error {
	if g.unavailable.Load() {
		if g.onRead != nil {
			g.onRead()
		}
		return g.err
	}
	return g.ControllerEpochMutationStore.WithControllerEpochMutation(ctx, fence, access)
}

func TestMCPApprovalEpochGuardFailurePreservesApprovedCall(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{name: "unavailable", err: errors.New("simulated epoch read outage")},
		{name: "contention", err: errors.Join(store.ErrControllerEpochMutationContention, store.ConflictErrorf("simulated epoch lock contention"))},
	} {
		t.Run(failure.name, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			var unavailable atomic.Bool
			failedReads := make(chan struct{}, 2)
			f.broker.EpochMutations = approvalUnavailableEpochGuard{
				ControllerEpochMutationStore: f.broker.EpochMutations, unavailable: &unavailable, err: failure.err,
				onRead: func() {
					select {
					case failedReads <- struct{}{}:
					default:
					}
				},
			}
			done := f.start(f.request)
			pending := f.pending()
			unavailable.Store(true)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			requireApprovalReadRetrying(t, f, pending.ID, approvals.StatusApproved, done, failedReads)
			unavailable.Store(false)
			result := awaitMCPApprovalResult(t, done)
			require.False(t, result.IsError)
			require.False(t, result.Replayed)
			require.EqualValues(t, 1, f.count.Load())
		})
	}
}

func TestMCPApprovalEpochGuardDefinitiveFailureDeniesApprovedCall(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{name: "stale fence", err: store.ConflictErrorf("controller epoch holder changed")},
		{name: "invalid fence", err: store.ValidationErrorf("controller epoch fence is invalid")},
		{name: "missing authority", err: store.ErrNotFound},
	} {
		t.Run(failure.name, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			f.broker.ApprovalWaitTimeout = time.Second
			var unavailable atomic.Bool
			var rejectedReads atomic.Int32
			f.broker.EpochMutations = approvalUnavailableEpochGuard{
				ControllerEpochMutationStore: f.broker.EpochMutations, unavailable: &unavailable, err: failure.err,
				onRead: func() { rejectedReads.Add(1) },
			}
			done := f.start(f.request)
			pending := f.pending()
			unavailable.Store(true)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)

			result := awaitMCPApprovalResult(t, done)
			require.True(t, result.IsError)
			require.Contains(t, string(result.Result), acpApprovalCodeStale)
			require.NotContains(t, string(result.Result), acpApprovalCodeExpired)
			require.EqualValues(t, 1, rejectedReads.Load(), "definitive authority loss must not enter the read retry loop")
			require.Zero(t, f.count.Load())

			// Once the readable fixture store records the denial, restored
			// acquisition cannot revive the same approved call after reopen.
			unavailable.Store(false)
			f.reopen()
			replay := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replay.Replayed)
			require.JSONEq(t, string(result.Result), string(replay.Result))
			require.Zero(t, f.count.Load())
		})
	}
}
