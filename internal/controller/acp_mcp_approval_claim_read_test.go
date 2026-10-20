package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

type approvalClaimReadExecutor struct {
	ACPMCPToolExecutor
	validate func(context.Context) error
}

func (e approvalClaimReadExecutor) ValidateACPMCPTool(ctx context.Context, _ harnessv2.MCPBrokerCallRequest, _ harnessv2.MCPToolDescriptor) error {
	return e.validate(ctx)
}

func approvalClaimReadEffectID(t *testing.T, f *mcpApprovalFixture) string {
	t.Helper()
	identity := store.ExternalEffectIdentity{Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID)}
	id, err := identity.CanonicalID()
	require.NoError(t, err)
	return id
}

func configureApprovalClaimReadFault(t *testing.T, f *mcpApprovalFixture, source string) (*approvalRetryReadFault, <-chan struct{}) {
	t.Helper()
	fault := &approvalRetryReadFault{reads: make(chan struct{}, 2)}
	watchdogReads := make(chan struct{}, 2)
	id := approvalClaimReadEffectID(t, f)
	read := func(ctx context.Context) error {
		if !fault.active.Load() {
			return nil
		}
		effect, err := f.events.GetExternalEffect(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return acpMCPAuthorityReadError(err)
		}
		if effect.State != store.ExternalEffectInFlight {
			return nil
		}
		if source == "final prompt" {
			listed, err := approvals.ListEvents(ctx, f.events, f.request.Namespace, "approval-task")
			if err != nil {
				return acpMCPAuthorityReadError(err)
			}
			values := approvals.Derive(listed, time.Time{})
			if len(values) != 1 || values[0].ExecutionOutcome != "running" {
				return nil
			}
			// Preparation retains the review deadline; the authority
			// watchdog uses a five-second read.
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= 5*time.Second {
				select {
				case watchdogReads <- struct{}{}:
				default:
				}
			}
		}
		return acpMCPAuthorityReadError(fault.readError())
	}
	switch source {
	case "credentials":
		resolver := f.broker.Credentials
		f.broker.Credentials = ACPMCPBrokerCredentialResolverFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
			credentials, err := resolver.ResolveACPMCPBrokerCredentials(ctx, request)
			if err == nil {
				err = read(ctx)
			}
			return credentials, err
		})
	case "tool":
		f.broker.Executor = approvalClaimReadExecutor{ACPMCPToolExecutor: f.broker.Executor, validate: read}
	case "final prompt":
		authorizer := f.broker.Prompts
		f.broker.Prompts = ACPMCPPromptAuthorizerFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) error {
			if err := authorizer.AuthorizeACPMCPPrompt(ctx, request); err != nil {
				return err
			}
			return read(ctx)
		})
	default:
		t.Fatalf("unknown claimed approval read source %q", source)
	}
	return fault, watchdogReads
}

func TestMCPApprovalPreparationPreservesExecutionBudgetAndLease(t *testing.T) {
	f := newMCPApprovalFixture(t)
	original := f.broker.Executor
	type executionObservation struct {
		started  time.Time
		deadline time.Time
		effect   *store.ExternalEffect
	}
	observed := make(chan executionObservation, 1)
	f.broker.Executor = ACPMCPToolExecutorFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		started := time.Now().UTC()
		deadline, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("execution has no deadline")
		}
		effect, err := f.events.GetExternalEffect(ctx, approvalClaimReadEffectID(t, f))
		if err != nil {
			return nil, err
		}
		observed <- executionObservation{started: started, deadline: deadline, effect: effect}
		return original.ExecuteACPMCPTool(ctx, request, descriptor)
	})
	fault, _ := configureApprovalClaimReadFault(t, f, "tool")
	done := f.start(f.request)
	pending := f.pending()
	fault.active.Store(true)
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	claimed := requireApprovalClaimReadRetrying(t, f, fault, done, nil)
	require.NotNil(t, pending.ExpiresAt)
	require.NotNil(t, claimed.LeaseExpiresAt)
	require.WithinDuration(t, pending.ExpiresAt.Add(harnessv2.MCPApprovalExecutionTimeout+externalEffectLeaseSettlementMargin), *claimed.LeaseExpiresAt, time.Millisecond)

	// Hold the acknowledged claim before releasing its preparation reads.
	// The executor must still receive its full budget, not the time left
	// from a deadline installed before this outage.
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	readyAt := time.Now().UTC()
	fault.active.Store(false)
	result := awaitMCPApprovalResult(t, done)
	require.False(t, result.IsError)
	require.EqualValues(t, 1, f.count.Load())
	execution := <-observed
	require.False(t, execution.deadline.Before(readyAt.Add(harnessv2.MCPApprovalExecutionTimeout)))
	require.WithinDuration(t, execution.started.Add(harnessv2.MCPApprovalExecutionTimeout), execution.deadline, 100*time.Millisecond)
	require.Equal(t, claimed.Version, execution.effect.Version)
	require.Equal(t, claimed.LeaseOwner, execution.effect.LeaseOwner)
	require.Equal(t, claimed.LeaseExpiresAt, execution.effect.LeaseExpiresAt, "preparation must not renew or replace the claim")
	require.False(t, execution.effect.LeaseExpiresAt.Before(execution.deadline.Add(externalEffectLeaseSettlementMargin)))
}

func TestMCPApprovalExecutionAndLeaseHonorEarlierDeadlines(t *testing.T) {
	for _, source := range []string{"task", "parent"} {
		t.Run(source, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			deadline := time.Now().UTC().Add(30 * time.Second)
			ctx := t.Context()
			if source == "task" {
				resolver := f.broker.Credentials
				f.broker.Credentials = ACPMCPBrokerCredentialResolverFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
					credentials, err := resolver.ResolveACPMCPBrokerCredentials(ctx, request)
					credentials.Task.Deadline = deadline
					return credentials, err
				})
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, deadline)
				defer cancel()
			}
			observed := make(chan time.Time, 1)
			original := f.broker.Executor
			f.broker.Executor = ACPMCPToolExecutorFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
				executionDeadline, _ := ctx.Deadline()
				observed <- executionDeadline
				return original.ExecuteACPMCPTool(ctx, request, descriptor)
			})
			fault, _ := configureApprovalClaimReadFault(t, f, "tool")
			done := f.startContext(ctx, f.request)
			pending := f.pending()
			fault.active.Store(true)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			claimed := requireApprovalClaimReadRetrying(t, f, fault, done, nil)
			require.NotNil(t, claimed.LeaseExpiresAt)
			require.WithinDuration(t, deadline.Add(externalEffectLeaseSettlementMargin), *claimed.LeaseExpiresAt, time.Millisecond)
			fault.active.Store(false)
			result := awaitMCPApprovalResult(t, done)
			require.False(t, result.IsError)
			require.WithinDuration(t, deadline, <-observed, time.Millisecond)
			require.EqualValues(t, 1, f.count.Load())
		})
	}
}

func requireApprovalClaimReadRetrying(t *testing.T, f *mcpApprovalFixture, fault *approvalRetryReadFault, done, duplicate <-chan *httptest.ResponseRecorder) *store.ExternalEffect {
	t.Helper()
	for range 2 {
		select {
		case <-fault.reads:
		case response := <-done:
			t.Fatalf("claim owner ended during a recoverable read outage: HTTP %d", response.Code)
		case response := <-duplicate:
			t.Fatalf("concurrent delivery ended during a recoverable read outage: HTTP %d", response.Code)
		case <-time.After(3 * time.Second):
			t.Fatal("acknowledged claim owner did not retry its preparation read")
		}
	}
	effect, err := f.events.GetExternalEffect(t.Context(), approvalClaimReadEffectID(t, f))
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectInFlight, effect.State)
	require.NotEmpty(t, effect.LeaseOwner)
	require.Zero(t, f.count.Load())
	return effect
}

func TestMCPApprovalClaimReadRecoveryKeepsOneExecutionOwner(t *testing.T) {
	for _, source := range []string{"credentials", "tool", "final prompt"} {
		t.Run(source, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			fault, watchdogReads := configureApprovalClaimReadFault(t, f, source)
			secondInputRead := make(chan struct{})
			var inputReads atomic.Int32
			f.broker.ApprovalSecrets.(*k8sfake.Clientset).PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
				if inputReads.Add(1) == 2 {
					close(secondInputRead)
				}
				return false, nil, nil
			})
			done := f.start(f.request)
			pending := f.pending()
			duplicate := f.start(f.request)
			select {
			case <-secondInputRead:
			case <-time.After(3 * time.Second):
				t.Fatal("concurrent delivery did not pass initial admission")
			}
			fault.active.Store(true)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			claimed := requireApprovalClaimReadRetrying(t, f, fault, done, duplicate)
			if source == "final prompt" {
				// Keep the outage through multiple watchdog polls. A strict
				// execution watcher must not cancel preparation on unreadability.
				for range 2 {
					select {
					case <-watchdogReads:
					case response := <-done:
						t.Fatalf("authority watchdog ended unstarted preparation: HTTP %d", response.Code)
					case response := <-duplicate:
						t.Fatalf("concurrent waiter ended during preparation: HTTP %d", response.Code)
					case <-time.After(4 * time.Second):
						t.Fatal("authority watchdog did not observe the outage")
					}
				}
			}
			current, err := f.events.GetExternalEffect(t.Context(), claimed.ID)
			require.NoError(t, err)
			require.Equal(t, claimed, current, "preparation must not renew, replace, or reclaim the receipt lease")
			fault.active.Store(false)
			result := awaitMCPApprovalResult(t, done)
			joined := awaitMCPApprovalResult(t, duplicate)
			require.False(t, result.IsError || joined.IsError)
			require.NotEqual(t, result.Replayed, joined.Replayed, "one delivery must execute and the other must join its receipt")
			require.JSONEq(t, string(result.Result), string(joined.Result))
			require.EqualValues(t, 1, f.count.Load())
			f.reopen()
			replay := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replay.Replayed)
			require.JSONEq(t, string(result.Result), string(replay.Result))
			require.EqualValues(t, 1, f.count.Load())
		})
	}
}

func TestMCPApprovalClaimReadRecoveryRechecksAuthorityAndExpiry(t *testing.T) {
	for _, change := range []string{"approval expiry", "task expiry", "credential drift", "prompt revoked"} {
		t.Run(change, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			fault, _ := configureApprovalClaimReadFault(t, f, "final prompt")
			if change == "approval expiry" {
				f.broker.ApprovalWaitTimeout = time.Second
			}
			if change == "task expiry" {
				deadline := time.Now().UTC().Add(time.Second)
				resolver := f.broker.Credentials
				f.broker.Credentials = ACPMCPBrokerCredentialResolverFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (ACPMCPBrokerCredentials, error) {
					credentials, err := resolver.ResolveACPMCPBrokerCredentials(ctx, request)
					credentials.Task.Deadline = deadline
					return credentials, err
				})
			}
			done := f.start(f.request)
			pending := f.pending()
			fault.active.Store(true)
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			requireApprovalClaimReadRetrying(t, f, fault, done, nil)
			code := acpApprovalCodeExpired
			switch change {
			case "credential drift":
				// Let an existing guarded read return the outage before
				// recovery, so the next iteration must refresh credentials.
				guard := f.broker.EpochMutations.(*approvalTestGuard)
				guard.Lock()
				f.authorized.Store(false)
				fault.active.Store(false)
				guard.Unlock()
				code = acpApprovalCodeStale
			case "prompt revoked":
				f.active.Store(false)
				code = acpApprovalCodeStale
			}
			result := awaitMCPApprovalResult(t, done)
			require.True(t, result.IsError)
			require.JSONEq(t, string(acpApprovalError(pending.ID, code)), string(result.Result))
			require.Zero(t, f.count.Load())
			effect, err := f.events.GetExternalEffect(t.Context(), approvalClaimReadEffectID(t, f))
			require.NoError(t, err)
			require.Equal(t, store.ExternalEffectFailed, effect.State)
			require.JSONEq(t, string(result.Result), string(effect.Response))
			fault.active.Store(false)
			f.authorized.Store(true)
			f.active.Store(true)
			f.reopen()
			replay := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replay.IsError && replay.Replayed)
			require.JSONEq(t, string(result.Result), string(replay.Result))
			require.Zero(t, f.count.Load(), "restored authority must not revive a denied call")
		})
	}
}
