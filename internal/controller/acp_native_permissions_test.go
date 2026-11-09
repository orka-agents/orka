package controller

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func requesterDecisionsTask(task *corev1alpha1.Task) {
	if task.Spec.AgentRuntime == nil {
		task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{}
	}
	task.Spec.AgentRuntime.PermissionDecisions = corev1alpha1.PermissionDecisionsRequester
}

func TestTaskNativePermissionsFollowPermissionDecisions(t *testing.T) {
	require.Equal(t, harnessv2.NativePermissionsPreapproved, taskNativePermissions(nil))
	require.Equal(t, harnessv2.NativePermissionsPreapproved, taskNativePermissions(&corev1alpha1.AgentRuntimeSpec{}))
	require.Equal(t, harnessv2.NativePermissionsPreapproved, taskNativePermissions(
		&corev1alpha1.AgentRuntimeSpec{PermissionDecisions: corev1alpha1.PermissionDecisionsPolicy}))
	require.Equal(t, harnessv2.NativePermissionsAsk, taskNativePermissions(
		&corev1alpha1.AgentRuntimeSpec{PermissionDecisions: corev1alpha1.PermissionDecisionsRequester}))
}

func TestValidateFrozenNativePermissions(t *testing.T) {
	requester := &corev1alpha1.AgentRuntimeSpec{PermissionDecisions: corev1alpha1.PermissionDecisionsRequester}
	pool, external := corev1alpha1.AgentExecutionBackendRuntimePool, corev1alpha1.AgentExecutionBackendExternalEndpoint
	for _, test := range []struct {
		name     string
		backend  corev1alpha1.AgentExecutionBackend
		override *corev1alpha1.AgentRuntimeSpec
		mode     harnessv2.NativePermissionMode
		wantErr  bool
	}{
		{name: "policy pool session", backend: pool},
		{name: "requester pool session asks", backend: pool, override: requester, mode: harnessv2.NativePermissionsAsk},
		{name: "requester pool session that would pre-approve", backend: pool, override: requester, wantErr: true},
		{name: "policy pool session that would ask", backend: pool, mode: harnessv2.NativePermissionsAsk, wantErr: true},
		{name: "policy external session", backend: external},
		{name: "requester external session", backend: external, override: requester, mode: harnessv2.NativePermissionsAsk, wantErr: true},
		{name: "external session that would ask", backend: external, mode: harnessv2.NativePermissionsAsk, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateFrozenNativePermissions(test.backend, test.override, test.mode)
			require.Equal(t, test.wantErr, err != nil, "error = %v", err)
		})
	}
}

func TestRequesterTaskFreezesAnAskingLaunch(t *testing.T) {
	for _, test := range []struct {
		name string
		mode harnessv2.NativePermissionMode
	}{
		{name: "policy", mode: harnessv2.NativePermissionsPreapproved},
		{name: "requester", mode: harnessv2.NativePermissionsAsk},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			task := bindingTestTask()
			if test.mode == harnessv2.NativePermissionsAsk {
				requesterDecisionsTask(task)
			}
			reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
			_, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), bindingTestAgent())
			require.NoError(t, err)
			require.False(t, handled)
			bound := &corev1alpha1.Task{}
			require.NoError(t, reconciler.Get(ctx, client.ObjectKeyFromObject(task), bound))
			verified, err := reconciler.loadVerifiedBoundExecution(ctx, bound, bound.Status.AgentExecutionBinding)
			require.NoError(t, err)
			require.Equal(t, test.mode, verified.mcpConfiguration.NativePermissions)
		})
	}
}

// A RuntimeSession is bound to its launch mode through the MCP binding
// digest: a mode change recreates the session instead of reusing a provider
// launched the other way, and a pre-approved launch keeps its digest.
func TestRuntimeSessionBindingFollowsTheLaunchMode(t *testing.T) {
	preapproved := harnessv2.MCPPolicyConfiguration{
		ToolPolicyDigest: testControlDigestForDispatcher("tool"), ApprovalPolicyDigest: testControlDigestForDispatcher("approval"),
		MCPConfigurationDigest: testControlDigestForDispatcher("mcp"),
		ToolPolicy:             harnessv2.MCPToolPolicy{AllowBash: true, DescriptorDigest: testControlDigestForDispatcher("descriptors")},
	}
	ask := preapproved
	ask.NativePermissions = harnessv2.NativePermissionsAsk
	preapprovedDigest, err := acpDomainDigest("runtime-session-mcp-configuration", preapproved)
	require.NoError(t, err)
	askDigest, err := acpDomainDigest("runtime-session-mcp-configuration", ask)
	require.NoError(t, err)
	require.NotEqual(t, preapprovedDigest, askDigest)
	legacyDigest, err := acpDomainDigest("runtime-session-mcp-configuration", struct {
		ToolPolicyDigest       string                      `json:"toolPolicyDigest"`
		ApprovalPolicyDigest   string                      `json:"approvalPolicyDigest"`
		MCPConfigurationDigest string                      `json:"mcpConfigurationDigest"`
		ToolPolicy             harnessv2.MCPToolPolicy     `json:"toolPolicy"`
		ApprovalPolicy         harnessv2.MCPApprovalPolicy `json:"approvalPolicy"`
	}{
		ToolPolicyDigest: preapproved.ToolPolicyDigest, ApprovalPolicyDigest: preapproved.ApprovalPolicyDigest,
		MCPConfigurationDigest: preapproved.MCPConfigurationDigest, ToolPolicy: preapproved.ToolPolicy,
	})
	require.NoError(t, err)
	require.Equal(t, legacyDigest, preapprovedDigest, "a pre-approved launch must keep its existing binding digest")

	control := store.SessionControl{SessionUID: "session-uid", Availability: store.SessionAvailable}
	profile := harnessv2.ProfileDigest(acpSessionTestDigest("profile"))
	current := &ACPRuntimeSessionBinding{
		SessionUID: "session-uid", Generation: 3, ProfileDigest: profile, MCPDigest: preapprovedDigest,
		RuntimeInstanceID: "instance", SupervisorBootID: "boot",
	}
	reused, err := PlanACPRuntimeSession(control, current, profile, preapprovedDigest, "instance", "boot")
	require.NoError(t, err)
	require.False(t, reused.Recreate)
	switched, err := PlanACPRuntimeSession(control, current, profile, askDigest, "instance", "boot")
	require.NoError(t, err)
	require.True(t, switched.Recreate)
	require.True(t, switched.BootstrapRequired)
	require.Equal(t, uint64(4), switched.Binding.Generation)
	require.Equal(t, askDigest, switched.Binding.MCPDigest)
}

// A Session turn whose launch mode differs from its live runtime session's
// recreates the runtime session from the Session transcript.
func TestSessionTurnWithAnotherLaunchModeRecreatesTheRuntimeSession(t *testing.T) {
	ctx := context.Background()
	controlStore, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "mode-switch.db"))
	defer closeStore()
	continuity, _ := newBootstrapTrackingSessionContinuity(t, controlStore, "mode-switch-session-uid")
	control := ensureACPSessionForTest(t, continuity, fence, "mode-switch")
	appendBootstrapHistoryForTest(t, controlStore, control)
	configuration := harnessv2.MCPPolicyConfiguration{
		ToolPolicyDigest: testControlDigestForDispatcher("tool"), ApprovalPolicyDigest: testControlDigestForDispatcher("approval"),
		MCPConfigurationDigest: testControlDigestForDispatcher("mcp"),
	}
	preapprovedDigest, err := acpDomainDigest("runtime-session-mcp-configuration", configuration)
	require.NoError(t, err)
	configuration.NativePermissions = harnessv2.NativePermissionsAsk
	askDigest, err := acpDomainDigest("runtime-session-mcp-configuration", configuration)
	require.NoError(t, err)

	profileDigest := harnessv2.ProfileDigest(testControlDigestForDispatcher("mode-switch-profile"))
	task := newBootstrapSessionTaskForTest(
		t, controlStore, fence, control, "mode-switch-task", types.UID("99999999-9999-9999-9999-999999999999"),
	)
	dispatcher := &ACPDispatcher{
		Store: controlStore, Sessions: continuity, runtimeSessions: make(map[string]ACPRuntimeSessionBinding),
	}
	dispatcher.setRuntimeSessionBinding(ACPRuntimeSessionBinding{
		SessionUID: control.SessionUID, Generation: 1, ProfileDigest: profileDigest, MCPDigest: preapprovedDigest,
		RuntimeInstanceID: "runtime", SupervisorBootID: "boot",
	})
	session, err := dispatcher.prepareTaskSession(
		ctx, task, fence, profileDigest, askDigest, "runtime", "boot", acpSessionLineageIdentity{}, false,
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	require.False(t, session.Reused, "a pre-approved runtime session must never serve an ask turn")
	require.Equal(t, uint64(2), session.Binding.Generation)
	require.Equal(t, askDigest, session.Binding.MCPDigest)
	require.NotNil(t, session.Bootstrap)
	require.EqualValues(t, 2, session.Bootstrap.MessageCount)
}

func TestACPDispatcherLaunchesRequesterTasksOnlyToAsk(t *testing.T) {
	for _, test := range []struct {
		name       string
		capability bool
	}{
		{name: "runtime that can ask", capability: true},
		{name: "runtime that cannot ask"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			var mu sync.Mutex
			var creates, prompts []harnessv2.NativePermissionMode
			fixture := newTaskScopedCreateConflictFixtureForRuntime(t, ctx, "requester-ask", types.UID("88888888-8888-8888-8888-888888888888"),
				corev1alpha1.AgentRuntimeCodex,
				func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
					return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{
						nativePermissionAsk: test.capability,
						onPrompt: func(request harnessv2.StartPromptRequest) {
							mu.Lock()
							defer mu.Unlock()
							prompts = append(prompts, request.MCPAuthorization.NativePermissions)
						},
					}, func(request harnessv2.CreateRuntimeSessionRequest) {
						mu.Lock()
						defer mu.Unlock()
						creates = append(creates, request.MCPConfiguration.NativePermissions)
					})
				}, requesterDecisionsTask)
			defer fixture.stop()
			fixture.dispatcher.EventStore = nil
			dispatchQueuedTask(ctx, t, fixture.dispatcher, fixture.task.DeepCopy())
			current := fixture.currentTask(t, ctx)
			mu.Lock()
			defer mu.Unlock()
			if !test.capability {
				require.Equal(t, corev1alpha1.TaskPhaseFailed, current.Status.Phase)
				require.Equal(t, corev1alpha1.TaskExecutionReason("PermissionDecisionsUnsupported"), current.Status.Execution.Reason)
				require.Empty(t, creates, "a runtime that cannot ask must never receive the session")
				require.Empty(t, prompts)
				return
			}
			require.Equal(t, []harnessv2.NativePermissionMode{harnessv2.NativePermissionsAsk}, creates)
			require.Equal(t, []harnessv2.NativePermissionMode{harnessv2.NativePermissionsAsk}, prompts)
		})
	}
}
