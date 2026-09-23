package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

type approvalRegistryPreparationContextKey struct{}

type approvalRegistryPreparationExecutor struct {
	RegistryACPMCPToolExecutor
}

func (e approvalRegistryPreparationExecutor) prepareACPMCPTool(ctx context.Context, request harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (acpMCPPreparedToolCall, error) {
	// Distinguish the real Registry's preparation reads from descriptor
	// validation, which also reads the Tool before and after preparation.
	ctx = context.WithValue(ctx, approvalRegistryPreparationContextKey{}, true)
	return e.RegistryACPMCPToolExecutor.prepareACPMCPTool(ctx, request, descriptor)
}

type approvalRegistryHTTPTransport func(*http.Request) (*http.Response, error)

func (transport approvalRegistryHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func configureApprovalRegistryPreparation(t *testing.T, f *mcpApprovalFixture, source string, read func(context.Context) error, invoke func(context.Context) error) {
	t.Helper()
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: f.request.Call.ToolName, Namespace: f.request.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description: "approved Registry execution budget test", Parameters: &apiextensionsJSONForMCPTest,
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			HTTP: &corev1alpha1.HTTPExecution{
				URL: "https://tool.example.test/approved", Method: http.MethodPost,
				Timeout: &metav1.Duration{Duration: harnessv2.MCPApprovalExecutionTimeout - time.Second},
			},
		},
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "approval-task", Namespace: f.request.Namespace, UID: types.UID(f.request.Metadata.TaskUID),
	}}
	descriptor, err := customACPMCPToolDescriptor(tool)
	require.NoError(t, err)
	f.request.Authorization.ToolPolicy.Tools = []harnessv2.MCPToolDescriptor{descriptor}
	f.request.Authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(f.request.Authorization.ToolPolicy.Tools)
	require.NoError(t, err)
	f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool, task).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			_, toolRead := object.(*corev1alpha1.Tool)
			_, taskRead := object.(*corev1alpha1.Task)
			if ctx.Value(approvalRegistryPreparationContextKey{}) == true &&
				((source == "tool" && toolRead) || (source == "task" && taskRead)) {
				if err := read(ctx); err != nil {
					return err
				}
			}
			return c.Get(ctx, key, object, opts...)
		},
	}).Build()
	f.broker.Executor = approvalRegistryPreparationExecutor{RegistryACPMCPToolExecutor{
		Reader: reader, KubeClient: k8sfake.NewClientset(),
		HTTPClient: &http.Client{Transport: approvalRegistryHTTPTransport(func(request *http.Request) (*http.Response, error) {
			f.count.Add(1)
			if invoke != nil {
				if err := invoke(request.Context()); err != nil {
					return nil, err
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"workOrder":"simulated-1"}`)), Request: request,
			}, nil
		})},
	}}
}

func TestMCPApprovalRegistryPreparationPreservesHTTPExecutionBudget(t *testing.T) {
	for _, source := range []string{"tool", "task"} {
		t.Run(source, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			effectID := approvalClaimReadEffectID(t, f)
			type preparationObservation struct {
				readyAt time.Time
				effect  *store.ExternalEffect
			}
			type executionObservation struct {
				started, deadline time.Time
				effect            *store.ExternalEffect
			}
			prepared := make(chan preparationObservation, 1)
			executed := make(chan executionObservation, 1)
			configureApprovalRegistryPreparation(t, f, source, func(ctx context.Context) error {
				effect, err := f.events.GetExternalEffect(ctx, effectID)
				if err != nil {
					return err
				}
				// Delay the actual Registry read after its claim. The public
				// HTTP deadline must not include this preparation time.
				timer := time.NewTimer(300 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
				}
				prepared <- preparationObservation{readyAt: time.Now().UTC(), effect: effect}
				return nil
			}, func(ctx context.Context) error {
				started := time.Now().UTC()
				deadline, ok := ctx.Deadline()
				if !ok {
					return errors.New("approved HTTP request has no execution deadline")
				}
				effect, err := f.events.GetExternalEffect(ctx, effectID)
				if err != nil {
					return err
				}
				executed <- executionObservation{started: started, deadline: deadline, effect: effect}
				return nil
			})
			done := f.start(f.request)
			pending := f.pending()
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			result := awaitMCPApprovalResult(t, done)
			require.False(t, result.IsError)
			require.False(t, result.Replayed)
			require.JSONEq(t, `{"workOrder":"simulated-1"}`, string(result.Result))
			require.EqualValues(t, 1, f.count.Load())
			require.Len(t, prepared, 1, "the real Registry preparation read must be delayed")
			require.Len(t, executed, 1)
			preparation, execution := <-prepared, <-executed
			require.False(t, execution.deadline.Before(preparation.readyAt.Add(harnessv2.MCPApprovalExecutionTimeout-time.Second)),
				"Registry preparation must not spend the configured HTTP execution budget")
			require.WithinDuration(t, execution.started.Add(harnessv2.MCPApprovalExecutionTimeout-time.Second), execution.deadline, 100*time.Millisecond)
			require.Equal(t, store.ExternalEffectInFlight, preparation.effect.State)
			require.Equal(t, preparation.effect.Version, execution.effect.Version)
			require.Equal(t, preparation.effect.LeaseOwner, execution.effect.LeaseOwner)
			require.Equal(t, preparation.effect.LeaseExpiresAt, execution.effect.LeaseExpiresAt, "preparation must not renew the claim")
			require.NotNil(t, execution.effect.LeaseExpiresAt)
			require.False(t, execution.effect.LeaseExpiresAt.Before(execution.deadline.Add(externalEffectLeaseSettlementMargin)))

			// Verify a durable receipt, then replay from a reopened store.
			f.reopen()
			receipt, err := f.events.GetExternalEffect(t.Context(), effectID)
			require.NoError(t, err)
			require.Equal(t, store.ExternalEffectSucceeded, receipt.State)
			require.EqualValues(t, 1, receipt.Attempts)
			require.Equal(t, string(result.Result), string(receipt.Response))
			require.Equal(t, store.CanonicalBytesDigest(result.Result), receipt.ResponseDigest)
			replay := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replay.Replayed)
			require.Equal(t, string(result.Result), string(replay.Result))
			require.EqualValues(t, 1, f.count.Load(), "a saved receipt must not execute the HTTP request again")
		})
	}
}

func TestMCPApprovalRegistryPreparationExpiryNeverInvokesHTTP(t *testing.T) {
	for _, source := range []string{"tool", "task"} {
		for _, bound := range []string{"approval", "task"} {
			t.Run(source+"/"+bound, func(t *testing.T) {
				f := newMCPApprovalFixture(t)
				entered := make(chan struct{}, 1)
				configureApprovalRegistryPreparation(t, f, source, func(ctx context.Context) error {
					entered <- struct{}{}
					<-ctx.Done()
					return ctx.Err()
				}, nil)
				if bound == "approval" {
					f.broker.ApprovalWaitTimeout = time.Second
				} else {
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
				f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
				select {
				case <-entered:
				case <-done:
					t.Fatal("approval ended before entering the delayed Registry read")
				case <-time.After(3 * time.Second):
					t.Fatal("approved call did not reach Registry preparation")
				}
				result := awaitMCPApprovalResult(t, done)
				require.True(t, result.IsError)
				require.Contains(t, string(result.Result), "approval_expired")
				require.Zero(t, f.count.Load(), "expiry during preparation must prevent HTTP invocation")
				receipt, err := f.events.GetExternalEffect(t.Context(), approvalClaimReadEffectID(t, f))
				require.NoError(t, err)
				require.Equal(t, store.ExternalEffectFailed, receipt.State)
				require.EqualValues(t, 1, receipt.Attempts)
				require.Equal(t, string(result.Result), string(receipt.Response))
			})
		}
	}
}

func TestMCPApprovalRegistryPreparationReadRecoveryCompletesOriginalCall(t *testing.T) {
	for _, source := range []string{"tool", "task"} {
		t.Run(source, func(t *testing.T) {
			f := newMCPApprovalFixture(t)
			effectID := approvalClaimReadEffectID(t, f)
			var unavailable atomic.Bool
			unavailable.Store(true)
			failedReads := make(chan struct{}, 2)
			executed := make(chan *store.ExternalEffect, 1)
			configureApprovalRegistryPreparation(t, f, source, func(context.Context) error {
				if !unavailable.Load() {
					return nil
				}
				select {
				case failedReads <- struct{}{}:
				default:
				}
				return apierrors.NewServiceUnavailable("simulated Registry preparation read outage")
			}, func(ctx context.Context) error {
				effect, err := f.events.GetExternalEffect(ctx, effectID)
				if err == nil {
					executed <- effect
				}
				return err
			})
			done := f.start(f.request)
			pending := f.pending()
			f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
			for range 2 {
				select {
				case <-failedReads:
				case <-done:
					t.Fatal("original approval call ended during a recoverable Registry preparation read outage")
				case <-time.After(3 * time.Second):
					t.Fatal("Registry preparation did not retry its failed read")
				}
			}
			claimed, err := f.events.GetExternalEffect(t.Context(), effectID)
			require.NoError(t, err)
			require.Equal(t, store.ExternalEffectInFlight, claimed.State)
			require.EqualValues(t, 1, claimed.Attempts)
			require.Zero(t, f.count.Load())
			select {
			case <-done:
				t.Fatal("original approval call ended before Registry read recovery")
			default:
			}
			unavailable.Store(false)
			result := awaitMCPApprovalResult(t, done)
			require.False(t, result.IsError)
			require.False(t, result.Replayed, "recovery must complete the original broker request")
			require.JSONEq(t, `{"workOrder":"simulated-1"}`, string(result.Result))
			require.EqualValues(t, 1, f.count.Load())
			require.Len(t, executed, 1)
			execution := <-executed
			require.Equal(t, claimed.Version, execution.Version)
			require.Equal(t, claimed.LeaseOwner, execution.LeaseOwner)
			require.Equal(t, claimed.LeaseExpiresAt, execution.LeaseExpiresAt, "read recovery must not renew the claim")

			f.reopen()
			receipt, err := f.events.GetExternalEffect(t.Context(), effectID)
			require.NoError(t, err)
			require.Equal(t, store.ExternalEffectSucceeded, receipt.State)
			require.EqualValues(t, 1, receipt.Attempts)
			require.Equal(t, store.CanonicalBytesDigest(result.Result), receipt.ResponseDigest)
			replay := awaitMCPApprovalResult(t, f.start(f.request))
			require.True(t, replay.Replayed)
			require.Equal(t, string(result.Result), string(replay.Result))
			require.EqualValues(t, 1, f.count.Load())
		})
	}
}
