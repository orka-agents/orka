package controller

import (
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCreateTaskJobGatewayReplyDefersUntilDurableLink(t *testing.T) {
	scheme := newTestScheme()
	require.NoError(t, gatewayv1alpha1.AddToScheme(scheme))
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "gateway-task", Namespace: "default", UID: "task-uid", Labels: map[string]string{
		gatewayruntime.TaskGatewayNameLabel: "chat", gatewayruntime.TaskGatewayBindingLabel: "room", gatewayruntime.TaskGatewayEventLabel: "gev-test",
	}, Annotations: map[string]string{
		gatewayruntime.TaskGatewayEventAnnotation: "gev-test", gatewayruntime.TaskGatewayExternalEvent: "external", gatewayruntime.TaskGatewaySession: "session", gatewayruntime.TaskGatewayNameAnnotation: "chat", gatewayruntime.TaskGatewayBindingAnnotation: "room",
	}}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: "assistant"}, SessionRef: &corev1alpha1.SessionReference{Name: "session", ThroughMessageID: "gateway:gev-test:user", PromptIncluded: true}, RequestedBy: &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/ns-uid/chat/gateway-uid", Subject: "sender", Groups: []string{"gateway:chat"}, Roles: []string{"gateway-sender"}}}}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "default"}, Spec: corev1alpha1.AgentSpec{Model: &corev1alpha1.ModelConfig{Provider: "openai", Name: "test-model"}}}
	gateway := &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", UID: "gateway-uid", Generation: 1}, Status: gatewayv1alpha1.GatewayStatus{Ready: true, ObservedGeneration: 1, ObservedCapabilities: &gatewayv1alpha1.GatewayObservedCapabilities{ContractVersion: protocol.Version, Capabilities: gatewayv1alpha1.GatewayCapabilities{InterimDelivery: true}}}}
	r := newUnitReconciler(scheme, task, agent, gateway, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "ns-uid"}})
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	s := sqlite.NewStore(db, ":memory:")
	now := time.Now().UTC()
	event := store.GatewayEvent{ID: "gev-test", Namespace: "default", NamespaceUID: "ns-uid", GatewayName: "chat", GatewayUID: "gateway-uid", GatewayGeneration: 1, BindingName: "room", AgentName: "assistant", ExternalEventID: "external", ProtocolVersion: protocol.Version, EventType: "text", AccountID: "account", ContextID: "room", SenderID: "sender", Text: "hello", SessionName: "session", TaskName: "gateway-task", ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	_, _, err = s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
	require.NoError(t, err)
	_, err = s.ClaimNextGatewayEvent(t.Context(), "default", "dispatch", now, time.Minute)
	require.NoError(t, err)
	r.GatewayService = gatewayruntime.NewService(r.Client, s, s, s, gatewayruntime.DefaultConfig())
	result, err := r.createTaskJob(t.Context(), task, agent, nil)
	require.NoError(t, err)
	require.Equal(t, time.Second, result.RequeueAfter)
	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(t.Context(), jobs))
	require.Empty(t, jobs.Items)
	require.NotEqual(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
	require.NoError(t, s.MarkGatewayEventTaskCreated(t.Context(), "default", event.ID, task.Name, string(task.UID), "dispatch", now))
	// Admission availability is not a planning-time origin discriminator. Freeze
	// the tool into the real Job even if readiness/capability recovery is pending.
	gateway.Status.Ready = false
	gateway.Status.ObservedCapabilities = nil
	require.NoError(t, r.Update(t.Context(), gateway))
	_, err = r.createTaskJob(t.Context(), task, agent, nil)
	require.NoError(t, err)
	require.Equal(t, corev1alpha1.TaskPhaseRunning, task.Status.Phase)
	require.NoError(t, r.List(t.Context(), jobs, client.InNamespace("default")))
	require.Len(t, jobs.Items, 1)
	env, ok := findEnvVar(jobs.Items[0].Spec.Template.Spec.Containers[0].Env, workerenv.GatewayReplyEnabled)
	require.True(t, ok)
	require.Equal(t, "true", env.Value)
}

func TestJobBuilderGatewayReplyTrustedOptionsAndEnv(t *testing.T) {
	for _, eligible := range []bool{false, true} {
		t.Run(map[bool]string{false: "forged", true: "eligible"}[eligible], func(t *testing.T) {
			b := setupJobBuilder()
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "real-task-uid"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AI: &corev1alpha1.AISpec{Tools: []string{"reply_in_conversation"}}, Env: []corev1.EnvVar{{Name: workerenv.GatewayReplyEnabled, Value: "true"}, {Name: workerenv.TaskUID, Value: "spoof"}}}}
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{SecretRef: &corev1.LocalObjectReference{Name: "env-override"}}}
			job, err := b.BuildWithOptions(t.Context(), task, agent, nil, JobBuildOptions{GatewayReplyEligible: eligible})
			require.NoError(t, err)
			require.Len(t, job.Spec.Template.Spec.Containers[0].EnvFrom, 1, "Agent secret injection remains present")
			env := job.Spec.Template.Spec.Containers[0].Env
			values := map[string]string{}
			flags := 0
			for _, value := range env {
				values[value.Name] = value.Value
				if value.Name == workerenv.GatewayReplyEnabled {
					flags++
				}
			}
			require.Equal(t, 1, flags, "explicit controller env must shadow EnvFrom without duplicate ambiguity")
			parsed := workerenv.ParseAIWorkerEnv(func(name string) string { return values[name] })
			require.Equal(t, eligible, parsed.GatewayReplyEnabled)
			flag, ok := findEnvVar(env, workerenv.GatewayReplyEnabled)
			require.True(t, ok)
			require.Equal(t, eligible, flag.Value == "true")
			names, ok := findEnvVar(env, workerenv.AITools)
			require.True(t, ok)
			if eligible {
				require.Contains(t, workerenv.SplitCSV(names.Value), "reply_in_conversation")
			} else {
				require.NotContains(t, workerenv.SplitCSV(names.Value), "reply_in_conversation")
			}
			uid, ok := findEnvVar(env, workerenv.TaskUID)
			require.True(t, ok)
			require.Equal(t, "real-task-uid", uid.Value)
		})
	}
}
