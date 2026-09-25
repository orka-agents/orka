package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
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

func newKubernetesMCPApprovalLeaseFixture(t *testing.T) (*mcpApprovalFixture, *storekube.Store, store.ControllerEpochFence, *ACPMCPPromptLeaseRegistry) {
	t.Helper()
	f := newMCPApprovalFixture(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.request.Namespace, Name: "approval-task", UID: types.UID(f.request.Metadata.TaskUID)},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	kube := withControllerEpochLeaseUIDs(t, fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).
		WithStatusSubresource(&corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{}, &corev1alpha1.ExternalEffect{}).Build())
	control, err := storekube.NewComposite(kube, "orka-system", f.events, storekube.WithAPIReader(kube))
	require.NoError(t, err)
	epoch, err := control.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		NewEpoch: 1, HolderID: "controller-a", RequestDigest: testControllerMCPDigest("epoch"),
	})
	require.NoError(t, err)
	fence := store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
	attempt, err := control.CreatePromptAttempt(t.Context(), &store.PromptAttempt{
		Key:        mcpPromptLeaseKey(f.request.Namespace, f.request.Metadata),
		SessionUID: string(f.request.Authorization.RuntimeSessionUID), RuntimeInstanceID: string(f.request.Metadata.Fence.RuntimeInstanceID),
		RequestDigest: testControllerMCPDigest("prompt"), BindingDigest: testControllerMCPDigest("binding"), SnapshotDigest: testControllerMCPDigest("snapshot"),
	}, fence)
	require.NoError(t, err)
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning,
	} {
		attempt, err = control.TransitionPromptAttemptExecution(t.Context(), store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: state, OperationID: string(state), OperationDigest: testControllerMCPDigest(string(state)),
		})
		require.NoError(t, err)
	}
	registry := &ACPMCPPromptLeaseRegistry{}
	f.broker.Prompts = DurableACPMCPPromptAuthorizer{Attempts: control, PromptLeases: registry}
	f.broker.Effects, f.broker.EpochMutations = control, control
	return f, control, fence, registry
}

func startFiberMCPApproval(t *testing.T, f *mcpApprovalFixture) <-chan *httptest.ResponseRecorder {
	t.Helper()
	app := fiber.New()
	app.Post(harnessv2.MCPBrokerCallPath, adaptor.HTTPHandler(f.broker))
	body, err := json.Marshal(f.request)
	require.NoError(t, err)
	token, err := harnessv2.SignOperationCapability([]byte(strings.Repeat("c", 32)), harnessv2.ClaimsForMutation(f.request.Metadata))
	require.NoError(t, err)
	incoming := httptest.NewRequest(http.MethodPost, harnessv2.MCPBrokerCallPath, bytes.NewReader(body))
	incoming.Header.Set("Content-Type", "application/json")
	incoming.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 32))
	incoming.Header.Set(harnessv2.OperationCapabilityHeader, token)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		response, err := app.Test(incoming, fiber.TestConfig{Timeout: 5 * time.Second})
		if err != nil {
			recorder.WriteHeader(http.StatusBadGateway)
			_, _ = recorder.WriteString(err.Error())
		} else {
			recorder.WriteHeader(response.StatusCode)
			_, _ = io.Copy(recorder, response.Body)
			_ = response.Body.Close()
		}
		done <- recorder
	}()
	return done
}

func TestACPMCPApprovalLeaseEndsBeforePromptCleanup(t *testing.T) {
	for _, end := range []string{"cancelled", "expired"} {
		t.Run(end, func(t *testing.T) {
			f, control, fence, registry := newKubernetesMCPApprovalLeaseFixture(t)
			leaseCtx, cancelLease := context.WithCancel(t.Context())
			t.Cleanup(cancelLease)
			if end == "expired" {
				f.request.Authorization.ExpiresAt = time.Now().UTC().Add(time.Second)
				f.request.Metadata.ExpiresAt = f.request.Authorization.ExpiresAt
				var err error
				f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
				require.NoError(t, err)
			}
			registerApprovalLease(t, registry, leaseCtx, f.request)
			done := startFiberMCPApproval(t, f)
			pending := f.pending()
			// Keep the production attempt Running and its registration present,
			// as while output flushing or remote cancellation has not returned.
			// The epoch guard makes the decision race deterministic without
			// replacing the production control-store interlock with a fake mutex.
			require.NoError(t, control.WithControllerEpochMutation(t.Context(), fence, func(context.Context) error {
				if end == "cancelled" {
					cancelLease()
				} else {
					time.Sleep(time.Until(f.request.Authorization.ExpiresAt))
				}
				f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
				return nil
			}))
			result := awaitMCPApprovalResult(t, done)
			require.True(t, result.IsError)
			require.Contains(t, string(result.Result), `"code":"approval_stale"`)
			require.Zero(t, f.count.Load())
			id, err := mcpPromptLeaseKey(f.request.Namespace, f.request.Metadata).CanonicalID()
			require.NoError(t, err)
			attempt, err := control.GetPromptAttempt(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, store.PromptExecutionRunning, attempt.ExecutionState)
		})
	}
}

func TestACPMCPApprovalHeldCallUsesRenewedLease(t *testing.T) {
	f, _, _, registry := newKubernetesMCPApprovalLeaseFixture(t)
	f.request.Authorization.ExpiresAt = time.Now().UTC().Add(time.Second)
	f.request.Metadata.ExpiresAt = f.request.Authorization.ExpiresAt
	var err error
	f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
	require.NoError(t, err)
	entry := registerApprovalLease(t, registry, t.Context(), f.request)
	done := startFiberMCPApproval(t, f)
	pending := f.pending()
	require.NoError(t, entry.renew(approvalLeaseRenewal(f.request, time.Now().UTC().Add(3*time.Minute))))
	time.Sleep(time.Until(f.request.Authorization.ExpiresAt))
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	require.False(t, result.IsError)
	require.EqualValues(t, 1, f.count.Load())
	require.JSONEq(t, `{"workOrder":"simulated-1"}`, string(result.Result))
}

type approvalLeaseValidationExecutor struct {
	ACPMCPToolExecutor
	validator RegistryACPMCPToolExecutor
}

func (e approvalLeaseValidationExecutor) ValidateACPMCPTool(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) error {
	return e.validator.ValidateACPMCPTool(ctx, request, descriptor)
}

func TestACPMCPApprovalRechecksLeaseAfterFinalToolRead(t *testing.T) {
	f, control, _, registry := newKubernetesMCPApprovalLeaseFixture(t)
	leaseCtx, cancelLease := context.WithCancel(t.Context())
	t.Cleanup(cancelLease)
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: f.request.Call.ToolName, Namespace: f.request.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description: "counted approval test tool", Parameters: &apiextensionsJSONForMCPTest,
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
		},
	}
	descriptor, err := customACPMCPToolDescriptor(tool)
	require.NoError(t, err)
	f.request.Authorization.ToolPolicy.Tools = []harnessv2.MCPToolDescriptor{descriptor}
	f.request.Authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(f.request.Authorization.ToolPolicy.Tools)
	require.NoError(t, err)
	f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
	require.NoError(t, err)
	registerApprovalLease(t, registry, leaseCtx, f.request)
	identity := store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
	}
	effectID, err := identity.CanonicalID()
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	var finalRead atomic.Bool
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			effect, err := control.GetExternalEffect(ctx, effectID)
			if err != nil {
				return err
			}
			if effect.State == store.ExternalEffectInFlight {
				// The broker has already authorized this start. End the prompt
				// lease during its final Kubernetes Tool read, before the
				// asynchronous authority watcher can observe cancellation.
				finalRead.Store(true)
				cancelLease()
			}
			return c.Get(ctx, key, object, opts...)
		},
	}).Build()
	f.broker.Executor = approvalLeaseValidationExecutor{
		ACPMCPToolExecutor: f.broker.Executor, validator: RegistryACPMCPToolExecutor{Reader: reader},
	}
	done := startFiberMCPApproval(t, f)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	require.True(t, finalRead.Load(), "the cancellation must occur after the execution claim")
	require.True(t, result.IsError)
	require.Contains(t, string(result.Result), `"code":"approval_stale"`)
	require.Zero(t, f.count.Load())
	effect, err := control.GetExternalEffect(t.Context(), effectID)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectFailed, effect.State, "a call that never entered the executor has a definitive result")
	listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	require.NoError(t, err)
	values := approvals.Derive(listed, time.Time{})
	require.Len(t, values, 1)
	require.Equal(t, "not_started", values[0].ExecutionOutcome)
}

func startApprovalLeaseRenewalLoop(t *testing.T, ctx context.Context, cancelRuntime context.CancelFunc, runtimeClient *harnessv2.Client, request harnessv2.MCPBrokerCallRequest, entry *acpMCPPromptLease) <-chan struct{} {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: request.Namespace, Name: "renew-approval", UID: types.UID(request.Metadata.TaskUID)},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			Attempt: int32(request.Metadata.TaskAttempt), PromptID: string(request.Metadata.PromptID),
		}},
	}
	admitted := make(chan struct{})
	close(admitted)
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&ACPDispatcher{}).renewPromptLeaseLoop(
			ctx, admitted, cancelRuntime, runtimeClient, "renew-approval-session", task, request.Metadata.Fence,
			request.Lease, request.Authorization, harnessv2.DefaultProtocolLimits(), entry,
		)
	}()
	return done
}

func TestRenewPromptLeaseLoopUpdatesOnlyConfirmedApprovalAuthority(t *testing.T) {
	for _, test := range []struct {
		name  string
		delay time.Duration
	}{
		{name: "confirmed before expiry"},
		{name: "confirmed after expiry", delay: 2 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				request := approvalLeaseCall(t)
				request.Authorization.ExpiresAt = time.Now().UTC().Add(2 * time.Second)
				registry := &ACPMCPPromptLeaseRegistry{}
				leaseCtx, stopLease := context.WithCancel(t.Context())
				defer stopLease()
				entry := registerApprovalLease(t, registry, leaseCtx, request)
				var calls, cancelled atomic.Int32
				runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error { return nil }, func(outbound *http.Request) (*http.Response, error) {
					calls.Add(1)
					var renewal harnessv2.RenewPromptLeaseRequest
					require.NoError(t, json.NewDecoder(outbound.Body).Decode(&renewal))
					time.Sleep(test.delay)
					response := httptest.NewRecorder()
					writeDispatcherJSONStatus(response, http.StatusOK, harnessv2.PromptLeaseResponse{
						Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
						Lease: renewal.Lease,
					})
					return response.Result(), nil
				})
				done := startApprovalLeaseRenewalLoop(t, leaseCtx, func() { cancelled.Add(1); stopLease() }, runtimeClient, request, entry)
				time.Sleep(3 * time.Second)
				synctest.Wait()
				require.EqualValues(t, 1, calls.Load())
				if test.delay == 0 {
					require.Zero(t, cancelled.Load())
					require.NoError(t, registry.authorize(request))
				} else {
					require.EqualValues(t, 1, cancelled.Load())
					require.ErrorIs(t, registry.authorize(request), errACPMCPPromptLeaseInactive)
				}
				stopLease()
				synctest.Wait()
				<-done
			})
		})
	}
}

func TestRenewPromptLeaseLoopDoesNotCancelCompletedStreamDuringRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		request := approvalLeaseCall(t)
		request.Authorization.ExpiresAt = time.Now().UTC().Add(2 * time.Second)
		registry := &ACPMCPPromptLeaseRegistry{}
		leaseCtx, stopLease := context.WithCancel(t.Context())
		defer stopLease()
		entry := registerApprovalLease(t, registry, leaseCtx, request)
		entered := make(chan struct{})
		runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error { return nil }, func(outbound *http.Request) (*http.Response, error) {
			close(entered)
			<-outbound.Context().Done()
			return nil, outbound.Context().Err()
		})
		var cancelled atomic.Bool
		done := startApprovalLeaseRenewalLoop(t, leaseCtx, func() { cancelled.Store(true) }, runtimeClient, request, entry)
		<-entered
		// StreamPrompt received its terminal result while the renewal RPC was
		// still pending. stopLease must only end renewal and broker authority.
		stopLease()
		synctest.Wait()
		<-done
		require.False(t, cancelled.Load(), "completed prompt must retain its runtime context for result delivery")
		require.ErrorIs(t, registry.authorize(request), errACPMCPPromptLeaseInactive)
	})
}
