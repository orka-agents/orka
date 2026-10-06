package controller

import (
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func acpReplyPlanningFixture(t *testing.T, linked bool) (*TaskReconciler, *corev1alpha1.Task, *corev1alpha1.Agent) {
	t.Helper()
	task, agent, ns := bindingTestTask(), bindingTestAgent(), bindingTestNamespace()
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude
	task.Spec.Prompt = ""
	task.Labels = map[string]string{gatewayruntime.TaskGatewayNameLabel: "chat", gatewayruntime.TaskGatewayBindingLabel: "room", gatewayruntime.TaskGatewayEventLabel: "gev-test"}
	task.Annotations = map[string]string{gatewayruntime.TaskGatewayEventAnnotation: "gev-test", gatewayruntime.TaskGatewayExternalEvent: "external", gatewayruntime.TaskGatewaySession: "session", gatewayruntime.TaskGatewayNameAnnotation: "chat", gatewayruntime.TaskGatewayBindingAnnotation: "room"}
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session", ThroughMessageID: "gateway:gev-test:user", PromptIncluded: true}
	task.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/" + string(ns.UID) + "/chat/gateway-uid", Subject: "sender", Groups: []string{"gateway:chat"}, Roles: []string{"gateway-sender"}}
	r, db := newBindingTestReconciler(t, task, ns)
	r.ACPRuntimeImages.Claude = r.ACPRuntimeImages.Codex
	require.NoError(t, gatewayv1alpha1.AddToScheme(r.Scheme))
	require.NoError(t, r.Create(t.Context(), &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", UID: "gateway-uid", Generation: 1}}))
	now := time.Now().UTC()
	event := store.GatewayEvent{ID: "gev-test", Namespace: "default", NamespaceUID: string(ns.UID), GatewayName: "chat", GatewayUID: "gateway-uid", GatewayGeneration: 1, BindingName: "room", AgentName: "agent", ExternalEventID: "external", ProtocolVersion: protocol.Version, EventType: "text", AccountID: "account", ContextID: "room", SenderID: "sender", Text: "hello", SessionName: "session", TaskName: task.Name, ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	_, _, err := db.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
	require.NoError(t, err)
	_, err = db.ClaimNextGatewayEvent(t.Context(), "default", "dispatch", now, time.Minute)
	require.NoError(t, err)
	if linked {
		require.NoError(t, db.MarkGatewayEventTaskCreated(t.Context(), "default", event.ID, task.Name, string(task.UID), "dispatch", now))
	}
	r.GatewayService = gatewayruntime.NewService(r.Client, db, db, db, gatewayruntime.DefaultConfig())
	r.MCPRegistry = tools.NewRegistry()
	r.MCPRegistry.Register(tools.NewReplyInConversationTool())
	return r, task, agent
}

func TestACPReplyProviderDefaults(t *testing.T) {
	for _, provider := range []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode} {
		t.Run(string(provider), func(t *testing.T) {
			r, task, agent := acpReplyPlanningFixture(t, true)
			agent.Spec.Runtime.Type = provider
			agent.Spec.SystemPrompt = nil
			r.ACPRuntimeImages.Copilot = r.ACPRuntimeImages.Codex
			r.ACPRuntimeImages.Opencode = r.ACPRuntimeImages.Codex
			if provider == corev1alpha1.AgentRuntimeOpencode {
				agent.Spec.Model = testOpenCodeModelConfig()
			}
			candidate, err := r.resolveAgentExecutionCandidate(t.Context(), task, agent)
			require.NoError(t, err)
			body, err := decodeAgentExecutionSnapshot(candidate.snapshotBody)
			require.NoError(t, err)
			require.Contains(t, body.MCPConfiguration.ToolPolicy.AllowedToolNames, aitools.GatewayReplyToolName)
			nativeRead := "Read"
			if provider == corev1alpha1.AgentRuntimeOpencode {
				nativeRead = "read"
			}
			require.Contains(t, body.MCPConfiguration.ToolPolicy.AllowedToolNames, nativeRead)
			require.Greater(t, len(body.MCPConfiguration.ToolPolicy.Tools), 1, "injection must not replace provider-native defaults")
			result, err, handled := r.ensureAgentExecutionBinding(t.Context(), task, agent)
			require.NoError(t, err)
			require.False(t, handled, "the injected snapshot must verify and permit dispatch")
			require.Zero(t, result.RequeueAfter)
			require.NotNil(t, task.Status.AgentExecutionBinding)
		})
	}
}

func TestACPReplyFrozenPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1alpha1.Task, *corev1alpha1.Agent)
		want   bool
	}{
		{"implicit", func(*corev1alpha1.Task, *corev1alpha1.Agent) {}, true},
		{"explicit empty", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			t.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{}}
		}, false},
		{"closed default", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			a.Spec.Runtime.DefaultAllowedTools = []string{"Read"}
		}, false},
		{"task precedence", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			a.Spec.Runtime.DefaultAllowedTools = []string{}
			t.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"Read", aitools.GatewayReplyToolName}}
		}, true},
		{"disallowed", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			t.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{aitools.GatewayReplyToolName}}
		}, false},
		{"forged", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			t.Spec.RequestedBy.Subject = "forged"
			t.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{aitools.GatewayReplyToolName}}
		}, false},
		{"read only", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			t.Annotations[labels.AnnotationAgentReadOnly] = "true"
		}, false},
		{"transaction", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			t.Spec.Transaction = &corev1alpha1.TaskTransaction{Context: map[string]string{"allowedTools": "[]"}}
		}, false},
		{"child", func(t *corev1alpha1.Task, a *corev1alpha1.Agent) {
			t.Labels[labels.LabelParentTask] = "parent"
			t.Annotations[labels.AnnotationDisableCoordinationToolInject] = "true"
			t.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{aitools.GatewayReplyToolName}}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, task, agent := acpReplyPlanningFixture(t, true)
			tc.mutate(task, agent)
			before := task.DeepCopy()
			candidate, err := r.resolveAgentExecutionCandidate(t.Context(), task, agent)
			require.NoError(t, err)
			require.Equal(t, before, task, "planning must not mutate Task policy")
			body, err := decodeAgentExecutionSnapshot(candidate.snapshotBody)
			require.NoError(t, err)
			require.NoError(t, body.MCPConfiguration.ValidateProfile(body.RuntimeProfile))
			found := false
			for _, d := range body.MCPConfiguration.ToolPolicy.Tools {
				if d.Name == aitools.GatewayReplyToolName {
					found = true
					require.Equal(t, harnessv2.MCPToolSourceBrokeredBuiltin, d.Source)
					require.Equal(t, harnessv2.MCPToolEffectConsequential, d.Effect)
				}
			}
			require.Equal(t, tc.want, found)
			if tc.name == "implicit" {
				require.Contains(t, body.MCPConfiguration.ToolPolicy.AllowedToolNames, "Read")
				require.Contains(t, body.MCPConfiguration.ToolPolicy.AllowedToolNames, "Bash")
			}
		})
	}
}

func TestACPReplyExternalRegisteredOptIn(t *testing.T) {
	tools.DefaultRegistry.Register(tools.NewReplyInConversationTool())
	for _, eligible := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "gateway"}[eligible], func(t *testing.T) {
			policy := testAgentRuntimeMCPPolicy()
			policy.AllowedTools = []string{aitools.GatewayReplyToolName}
			f := newExternalACPDispatchFixtureWithPolicy(t, "external-reply", policy)
			_, task, _ := acpReplyPlanningFixture(t, true)
			task.ResourceVersion = ""
			task.Spec.AgentRef.Name = f.agent.Name
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{aitools.GatewayReplyToolName}}
			task.Spec.Workspace = nil
			ns := &corev1.Namespace{}
			require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Name: task.Namespace}, ns))
			task.Spec.RequestedBy.Issuer = "gateway.orka.ai/" + task.Namespace + "/" + string(ns.UID) + "/chat/gateway-uid"
			if eligible {
				require.NoError(t, gatewayv1alpha1.AddToScheme(f.client.Scheme()))
				require.NoError(t, f.client.Create(t.Context(), &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: task.Namespace, UID: "gateway-uid", Generation: 1}}))
				now := time.Now().UTC()
				event := store.GatewayEvent{ID: "gev-test", Namespace: task.Namespace, NamespaceUID: string(ns.UID), GatewayName: "chat", GatewayUID: "gateway-uid", GatewayGeneration: 1, BindingName: "room", AgentName: f.agent.Name, ExternalEventID: "external", ProtocolVersion: protocol.Version, EventType: "text", AccountID: "account", ContextID: "room", SenderID: "sender", Text: "hello", SessionName: "session", TaskName: task.Name, ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
				_, _, err := f.persistence.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
				require.NoError(t, err)
				_, err = f.persistence.ClaimNextGatewayEvent(t.Context(), task.Namespace, "dispatch", now, time.Minute)
				require.NoError(t, err)
				require.NoError(t, f.persistence.MarkGatewayEventTaskCreated(t.Context(), task.Namespace, event.ID, task.Name, string(task.UID), "dispatch", now))
			} else {
				task.Labels = nil
				task.Annotations = nil
				task.Spec.RequestedBy = nil
				task.Spec.SessionRef = nil
				task.Spec.Prompt = "ordinary"
			}
			f.reconciler.GatewayService = gatewayruntime.NewService(f.client, f.persistence, f.persistence, f.persistence, gatewayruntime.DefaultConfig())
			require.NoError(t, f.client.Create(t.Context(), task))
			candidate, err := f.reconciler.resolveAgentExecutionCandidate(t.Context(), task, f.agent)
			if !eligible {
				require.ErrorContains(t, err, "eligible gateway Task")
				require.Nil(t, candidate)
				_, err, _ = f.reconciler.ensureAgentExecutionBinding(t.Context(), task, f.agent)
				require.NoError(t, err)
				require.Nil(t, task.Status.AgentExecutionBinding)
			} else {
				require.NoError(t, err)
				body, err := decodeAgentExecutionSnapshot(candidate.snapshotBody)
				require.NoError(t, err)
				require.Equal(t, f.runtime.Spec.Capabilities.Profile.Digest, body.ProfileDigest)
				require.Equal(t, policy.AllowedTools, body.MCPConfiguration.ToolPolicy.AllowedToolNames)
				require.Equal(t, f.runtime.Status.ObservedCapabilities.MCPToolDescriptorDigest, body.MCPConfiguration.ToolPolicy.DescriptorDigest)
				require.Len(t, body.MCPConfiguration.ToolPolicy.Tools, 1)
			}
			require.Zero(t, f.createCalls.Load(), "candidate resolution must precede runtime Session creation")
		})
	}
}

func TestACPReplyRestoredBindingDoesNotUpgrade(t *testing.T) {
	r, task, agent := acpReplyPlanningFixture(t, true)
	// Freeze the legacy policy, then make origin available. Restoration must
	// consume the existing snapshot, not rerun optional tool injection.
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"Read"}}
	require.NoError(t, r.Update(t.Context(), task))
	_, err, _ := r.ensureAgentExecutionBinding(t.Context(), task, agent)
	require.NoError(t, err)
	binding := task.Status.AgentExecutionBinding.DeepCopy()
	require.NotNil(t, binding)
	agent.Spec.Runtime.DefaultAllowedTools = []string{aitools.GatewayReplyToolName}
	r.GatewayService = nil
	result, err, handled := r.ensureAgentExecutionBinding(t.Context(), task, agent)
	require.NoError(t, err)
	require.False(t, handled, "restored bindings must not defer for newly unavailable origin resolution")
	require.Zero(t, result.RequeueAfter)
	require.Equal(t, binding, task.Status.AgentExecutionBinding)
	snapshot, err := r.AgentExecutionSnapshots.GetAgentExecutionSnapshot(t.Context(), store.AgentExecutionSnapshotKey{TaskUID: string(task.UID), Digest: binding.Snapshot.Digest})
	require.NoError(t, err)
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	require.NoError(t, err)
	require.NotContains(t, body.MCPConfiguration.ToolPolicy.AllowedToolNames, aitools.GatewayReplyToolName)
	for _, descriptor := range body.MCPConfiguration.ToolPolicy.Tools {
		require.NotEqual(t, aitools.GatewayReplyToolName, descriptor.Name)
	}
}

func TestACPReplyPendingLinkDefersFreeze(t *testing.T) {
	r, task, agent := acpReplyPlanningFixture(t, false)
	candidate, err := r.resolveAgentExecutionCandidate(t.Context(), task, agent)
	require.ErrorIs(t, err, store.ErrNotReady)
	require.Nil(t, candidate)
	result, err, handled := r.ensureAgentExecutionBinding(t.Context(), task, agent)
	require.NoError(t, err)
	require.True(t, handled)
	require.Positive(t, result.RequeueAfter)
	require.Nil(t, task.Status.AgentExecutionBinding)
}
