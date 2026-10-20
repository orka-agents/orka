package controller

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

func TestMCPApprovalExpiresDuringSecretReload(t *testing.T) {
	f := newMCPApprovalFixture(t)
	f.broker.ApprovalWaitTimeout = time.Second
	reloadDeadline := make(chan time.Time, 1)
	reloadStarted := make(chan time.Time, 1)
	f.broker.ApprovalSecrets.(*k8sfake.Clientset).PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case deadline := <-reloadDeadline:
			reloadStarted <- time.Now().UTC()
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-t.Context().Done():
				return true, nil, t.Context().Err()
			}
		default:
		}
		return false, nil, nil
	})

	done := f.start(f.request)
	pending := f.pending()
	require.NotNil(t, pending.ExpiresAt)
	reloadDeadline <- *pending.ExpiresAt
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	select {
	case started := <-reloadStarted:
		require.True(t, started.Before(*pending.ExpiresAt), "the unchanged Secret read must cross the original approval deadline")
	default:
		t.Fatal("approved call did not reload its private input")
	}
	require.True(t, result.IsError)
	require.JSONEq(t, string(acpApprovalError(pending.ID, acpApprovalCodeExpired)), string(result.Result))
	require.Zero(t, f.count.Load())

	listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	require.NoError(t, err)
	values := approvals.Derive(listed, time.Time{})
	require.Len(t, values, 1)
	require.Equal(t, approvals.StatusApproved, values[0].Status, "expiry must preserve the earlier reviewer decision")
	require.Equal(t, "not_started", values[0].ExecutionOutcome)
	require.Equal(t, acpApprovalCodeExpired, values[0].ExecutionReason)

	identity := store.ExternalEffectIdentity{Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID)}
	id, err := identity.CanonicalID()
	require.NoError(t, err)
	effect, err := f.broker.Effects.GetExternalEffect(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectFailed, effect.State)
	require.JSONEq(t, string(result.Result), string(effect.Response))
	f.reopen()
	replay := awaitMCPApprovalResult(t, f.start(f.request))
	require.True(t, replay.Replayed && replay.IsError)
	require.JSONEq(t, string(result.Result), string(replay.Result))
	require.Zero(t, f.count.Load(), "expired approval must never execute on redelivery")
}

func TestBuildAgentRuntimeMCPConfigurationBoundsApprovalReadTimeout(t *testing.T) {
	for _, provider := range []string{"agentkit", "foundry"} {
		for _, test := range []struct {
			name     string
			timeout  *metav1.Duration
			required []string
			reject   bool
		}{
			{name: "default", required: []string{"long_read"}},
			{name: "below bound", timeout: &metav1.Duration{Duration: harnessv2.MCPApprovalExecutionTimeout - time.Second}, required: []string{"long_read"}},
			{name: "at bound", timeout: &metav1.Duration{Duration: harnessv2.MCPApprovalExecutionTimeout}, required: []string{"long_read"}, reject: true},
			{name: "over bound", timeout: &metav1.Duration{Duration: harnessv2.MCPApprovalExecutionTimeout + time.Second}, required: []string{"long_read"}, reject: true},
			{name: "ungated long read", timeout: &metav1.Duration{Duration: 10 * time.Minute}, required: []string{}},
			{name: "another tool requires approval", timeout: &metav1.Duration{Duration: 10 * time.Minute}, required: []string{"bounded_read"}},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				longRead := &corev1alpha1.Tool{
					ObjectMeta: metav1.ObjectMeta{Name: "long_read", Namespace: "default", UID: "long-read-uid", Generation: 1},
					Spec: corev1alpha1.ToolSpec{Description: "Read inventory", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
						HTTP: &corev1alpha1.HTTPExecution{URL: "https://tools.example/read", Method: "GET", Timeout: test.timeout}},
				}
				boundedRead := longRead.DeepCopy()
				boundedRead.Name, boundedRead.UID = "bounded_read", "bounded-read-uid"
				boundedRead.Spec.HTTP.Timeout = &metav1.Duration{Duration: time.Second}
				scheme := runtime.NewScheme()
				require.NoError(t, corev1alpha1.AddToScheme(scheme))
				reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(longRead, boundedRead).Build()
				policy := &corev1alpha1.AgentRuntimeMCPPolicySpec{
					AllowedTools: []string{"bounded_read", "long_read"}, DisallowedTools: []string{}, ApprovalRequiredTools: test.required,
				}
				external := &corev1alpha1.AgentRuntime{
					ObjectMeta: metav1.ObjectMeta{Name: "read-runtime", Namespace: "default"},
					Spec:       corev1alpha1.AgentRuntimeRegistrySpec{Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{MCPPolicy: policy}},
				}
				_, profile := testMCPBrokerRequest(t, harnessv2.MCPToolEffectReadOnly)
				profile.ProviderKind = provider
				var err error
				profile.ToolPolicyDigest, err = harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedTools, policy.DisallowedTools, policy.AllowBash)
				require.NoError(t, err)
				profile.ApprovalPolicyDigest, err = harnessv2.CanonicalMCPApprovalPolicyDigest(agentRuntimeMCPApprovalPolicy(policy))
				require.NoError(t, err)
				profile.MCPConfigurationDigest, err = harnessv2.CanonicalMCPConfigurationDigest(policy.AllowedTools)
				require.NoError(t, err)
				configuration, err := buildAgentRuntimeMCPConfigurationWithRegistry(t.Context(), reader, external, profile, tools.NewRegistry())
				if test.reject {
					require.ErrorContains(t, err, "must be less than the approval-required call duration")
					require.True(t, isPermanentACPAgentConfigurationError(err))
					return
				}
				require.NoError(t, err)
				descriptor, ok := configuration.ToolPolicy.Descriptor(longRead.Name)
				require.True(t, ok)
				require.Equal(t, harnessv2.MCPToolEffectReadOnly, descriptor.Effect)
				require.NoError(t, configuration.ValidateProfile(profile))
			})
		}
	}
}

func TestMCPApprovalReadToolTimeoutDriftNeverExecutes(t *testing.T) {
	f := newMCPApprovalFixture(t)
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: f.request.Call.ToolName, Namespace: f.request.Namespace, UID: "read-tool-uid", Generation: 1},
		Spec: corev1alpha1.ToolSpec{Description: "Read inventory", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP: &corev1alpha1.HTTPExecution{URL: "https://tools.example/read", Method: "GET", Timeout: &metav1.Duration{Duration: harnessv2.MCPApprovalExecutionTimeout - time.Second}}},
	}
	descriptor, err := customACPMCPToolDescriptor(tool)
	require.NoError(t, err)
	f.request.Authorization.ToolPolicy.Tools = []harnessv2.MCPToolDescriptor{descriptor}
	f.request.Authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(f.request.Authorization.ToolPolicy.Tools)
	require.NoError(t, err)
	f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tool).Build()
	validator := RegistryACPMCPToolExecutor{Reader: reader}
	f.broker.Executor = approvalLeaseValidationExecutor{ACPMCPToolExecutor: f.broker.Executor, validator: validator}

	done := f.start(f.request)
	pending := f.pending()
	current := &corev1alpha1.Tool{}
	require.NoError(t, reader.Get(t.Context(), client.ObjectKeyFromObject(tool), current))
	current.Spec.HTTP.Timeout.Duration += time.Second
	current.Generation++
	require.NoError(t, reader.Update(t.Context(), current))
	require.ErrorContains(t, validator.ValidateACPMCPTool(t.Context(), f.request, descriptor), "changed after prompt authorization")
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)
	require.True(t, result.IsError)
	require.JSONEq(t, string(acpApprovalError(pending.ID, acpApprovalCodeStale)), string(result.Result))
	require.Zero(t, f.count.Load())
	f.reopen()
	replay := awaitMCPApprovalResult(t, f.start(f.request))
	require.True(t, replay.IsError && replay.Replayed)
	require.JSONEq(t, string(result.Result), string(replay.Result))
	require.Zero(t, f.count.Load())
}
