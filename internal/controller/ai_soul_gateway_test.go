package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func newAISoulGatewayTask(t *testing.T, r *TaskReconciler, ss *sqlite.Store, suffix string) *corev1alpha1.Task {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	event := store.GatewayEvent{
		ID: "soul-" + suffix, Namespace: "default", NamespaceUID: "namespace-uid",
		GatewayUID: "gateway-uid", GatewayGeneration: 1, GatewayName: "chat",
		BindingName: "room", BindingUID: "binding-uid", ExternalEventID: "external-" + suffix,
		ProtocolVersion: "orka.gateway.v1", EventType: "text", AgentName: "agent",
		AccountID: "acct", ContextID: "room", SenderID: "sender", Text: "question",
		ReplyTarget: "room", SessionName: "soul-session", TaskName: "task-" + suffix,
		ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	_, _, err := ss.AdmitGatewayEvent(ctx, store.GatewayEventAdmission{Event: event, AppendUserMessage: true})
	require.NoError(t, err)
	claimed, err := ss.ClaimNextGatewayEvent(ctx, event.Namespace, "dispatcher", now, time.Minute)
	require.NoError(t, err)
	require.Equal(t, event.ID, claimed.ID)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: event.TaskName, Namespace: event.Namespace, UID: types.UID(event.TaskName + "-uid"), Generation: 1,
			Labels: map[string]string{
				gateway.TaskGatewayNameLabel: event.GatewayName, gateway.TaskGatewayBindingLabel: event.BindingName,
				gateway.TaskGatewayEventLabel: event.ID,
			},
			Annotations: map[string]string{
				gateway.TaskGatewayEventAnnotation: event.ID, gateway.TaskGatewayExternalEvent: event.ExternalEventID,
				gateway.TaskGatewaySession: event.SessionName, gateway.TaskGatewayNameAnnotation: event.GatewayName,
				gateway.TaskGatewayBindingAnnotation: event.BindingName,
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: event.AgentName},
			SessionRef: &corev1alpha1.SessionReference{Name: event.SessionName, MaxMessages: store.GatewayTranscriptMessageLimit,
				ThroughMessageID: store.GatewayUserMessageID(event.ID), PromptIncluded: true},
			RequestedBy: &corev1alpha1.RequestedBy{
				Issuer: "gateway.orka.ai/default/namespace-uid/chat/gateway-uid", Subject: event.SenderID,
				Groups: []string{"gateway:chat"}, Roles: []string{"gateway-sender"},
			},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	require.NoError(t, r.Create(ctx, task))
	require.NoError(t, ss.MarkGatewayEventTaskCreated(ctx, event.Namespace, event.ID, task.Name, string(task.UID), "dispatcher", now))
	require.NoError(t, r.SessionManager.AcquireLock(ctx, task))
	return task
}

func TestAIGatewaySoulRecoversAfterUnboundSourceFailures(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "soul"}, Data: map[string]string{"SOUL.md": "incorrect content"}}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{
			ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: cm.Name, Key: "SOUL.md"}, Digest: agentcontext.Digest("persona"),
		}},
	}
	r := newUnitReconciler(newTestScheme(), agent, cm)
	ss := r.SessionManager.store.(*sqlite.Store)
	service := gateway.NewService(r.Client, ss, ss, ss, gateway.DefaultConfig())
	for _, suffix := range []string{"first", "second"} {
		task := newAISoulGatewayTask(t, r, ss, suffix)
		_, err := r.createTaskJob(ctx, task, agent, nil)
		require.NoError(t, err)
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
		require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
		require.Contains(t, task.Status.Message, "soul content does not match its expected digest")
		require.Nil(t, task.Status.SoulBinding)
		require.Zero(t, task.Status.Attempts)
		require.Nil(t, task.Status.StartTime)
		require.Empty(t, task.Status.JobName)
		var jobs batchv1.JobList
		require.NoError(t, r.List(ctx, &jobs))
		require.Empty(t, jobs.Items, "source failure must precede model execution")
		require.NoError(t, service.ProjectTerminals(ctx))
	}

	cm.Data["SOUL.md"] = "persona"
	require.NoError(t, r.Update(ctx, cm))
	next := newAISoulGatewayTask(t, r, ss, "corrected")
	state, err := ss.ReadSessionSoul(ctx, next.Namespace, next.Spec.SessionRef.Name, next.Name, string(next.UID))
	require.NoError(t, err)
	require.False(t, state.Established, "pre-execution errors must not pin absence of a soul")
	require.Equal(t, 5, state.MessageCount, "canonical errors and user messages remain in the transcript")
	require.Equal(t, next.Spec.SessionRef.ThroughMessageID, state.FirstMessageID)
	messages, err := r.SessionManager.LoadTranscript(ctx, next)
	require.NoError(t, err)
	require.Len(t, messages, 5)
	require.Equal(t, "true", messages[1].Metadata[store.SessionSoulUnboundMetadata])
	require.Equal(t, "true", messages[3].Metadata[store.SessionSoulUnboundMetadata])

	_, err = r.createTaskJob(ctx, next, agent, nil)
	require.NoError(t, err)
	require.Equal(t, corev1alpha1.TaskPhaseRunning, next.Status.Phase)
	require.NotNil(t, next.Status.SoulBinding)
	require.EqualValues(t, 1, next.Status.Attempts)
	_, err = r.completeExecutedTask(ctx, next, corev1alpha1.TaskPhaseSucceeded, "")
	require.NoError(t, err)
	require.NoError(t, ss.SaveResult(ctx, next.Namespace, next.Name, []byte("answer")))
	require.NoError(t, service.ProjectTerminals(ctx))

	continued := newAISoulGatewayTask(t, r, ss, "continued")
	state, err = ss.ReadSessionSoul(ctx, continued.Namespace, continued.Spec.SessionRef.Name, continued.Name, string(continued.UID))
	require.NoError(t, err)
	require.True(t, state.Established)
	require.Equal(t, agentcontext.SessionDigest(next.Status.SoulBinding), state.Digest)
	for _, change := range []string{"soul", "role", "Agent generation", "Agent UID", "remove soul"} {
		t.Run(change, func(t *testing.T) {
			changed := agent.DeepCopy()
			switch change {
			case "soul":
				changed.Spec.Soul = &corev1alpha1.SoulSource{Inline: "different persona"}
			case "role":
				changed.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "different role"}
			case "Agent generation":
				changed.Generation++
			case "Agent UID":
				changed.UID = "replacement-agent"
			case "remove soul":
				changed.Spec.Soul = nil
			}
			_, err := r.prepareAISoul(ctx, continued, changed)
			require.Error(t, err)
			require.True(t, isPermanentAISoulConfigurationError(err))
			require.Nil(t, continued.Status.SoulBinding)
		})
	}
	_, err = r.prepareAISoul(ctx, continued, agent)
	require.NoError(t, err, "the established revision can continue")
}

func TestAIGatewayNoSoulRemainsAbsenceOnly(t *testing.T) {
	ctx := context.Background()
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: "agent-uid", Generation: 1}}
	r := newUnitReconciler(newTestScheme(), agent)
	ss := r.SessionManager.store.(*sqlite.Store)
	service := gateway.NewService(r.Client, ss, ss, ss, gateway.DefaultConfig())
	task := newAISoulGatewayTask(t, r, ss, "no-soul")
	_, err := r.createTaskJob(ctx, task, agent, nil)
	require.NoError(t, err)
	_, err = r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "")
	require.NoError(t, err)
	require.NoError(t, ss.SaveResult(ctx, task.Namespace, task.Name, []byte("answer")))
	require.NoError(t, service.ProjectTerminals(ctx))
	next := newAISoulGatewayTask(t, r, ss, "no-soul-next")
	state, err := ss.ReadSessionSoul(ctx, next.Namespace, next.Spec.SessionRef.Name, next.Name, string(next.UID))
	require.NoError(t, err)
	require.True(t, state.Established)
	require.Empty(t, state.Digest)
	changed := agent.DeepCopy()
	changed.Generation++
	changed.UID = "replacement-agent"
	changed.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "changed legacy role"}
	prepared, err := r.prepareAISoul(ctx, next, changed)
	require.NoError(t, err, "no-soul identity must not newly freeze Agent or role revisions")
	require.Nil(t, prepared)
	changed.Spec.Soul = &corev1alpha1.SoulSource{Inline: "new persona"}
	_, err = r.prepareAISoul(ctx, next, changed)
	require.Error(t, err, "an executed no-soul turn still forbids later opt-in")
}

type lostGatewayJobStatusClient struct {
	client.Client
	err error
}

func (c lostGatewayJobStatusClient) Status() client.SubResourceWriter {
	return lostGatewayJobStatusWriter{SubResourceWriter: c.Client.Status(), err: c.err}
}

type lostGatewayJobStatusWriter struct {
	client.SubResourceWriter
	err error
}

func (w lostGatewayJobStatusWriter) Update(ctx context.Context, object client.Object, options ...client.SubResourceUpdateOption) error {
	if task, ok := object.(*corev1alpha1.Task); ok && task.Status.Phase == corev1alpha1.TaskPhaseRunning {
		return w.err
	}
	return w.SubResourceWriter.Update(ctx, object, options...)
}

func TestAIGatewaySoulDoesNotIgnoreJobAfterLostStartStatus(t *testing.T) {
	ctx := context.Background()
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: "agent-uid", Generation: 1}}
	r := newUnitReconciler(newTestScheme(), agent)
	ss := r.SessionManager.store.(*sqlite.Store)
	baseClient := r.Client
	service := gateway.NewService(baseClient, ss, ss, ss, gateway.DefaultConfig())
	task := newAISoulGatewayTask(t, r, ss, "lost-start-status")
	outage := errors.New("start status write unavailable")
	r.Client = lostGatewayJobStatusClient{Client: baseClient, err: outage}
	_, err := r.createTaskJob(ctx, task, agent, nil)
	require.ErrorIs(t, err, outage)
	r.Client = baseClient
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
	require.Equal(t, corev1alpha1.TaskPhasePending, task.Status.Phase)
	require.Zero(t, task.Status.Attempts)
	require.Nil(t, task.Status.StartTime)
	require.Empty(t, task.Status.JobName)
	require.Nil(t, task.Status.SoulBinding)
	var jobs batchv1.JobList
	require.NoError(t, r.List(ctx, &jobs))
	require.Len(t, jobs.Items, 1, "the no-soul Job can execute despite the missing start status")
	require.True(t, metav1.IsControlledBy(&jobs.Items[0], task))

	agent.Spec.Soul = &corev1alpha1.SoulSource{Inline: "invalid\x00"}
	_, err = r.createTaskJob(ctx, task, agent, nil)
	require.NoError(t, err)
	require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
	require.Contains(t, task.Status.Message, "AI soul configuration: ")
	require.NoError(t, service.ProjectTerminals(ctx))
	messages, err := ss.LoadTranscript(ctx, task.Namespace, task.Spec.SessionRef.Name, 0)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.NotContains(t, messages[1].Metadata, store.SessionSoulUnboundMetadata)
	next := newAISoulGatewayTask(t, r, ss, "after-lost-start-status")
	agent.Spec.Soul.Inline = "valid persona"
	_, err = r.prepareAISoul(ctx, next, agent)
	require.Error(t, err, "a previously launched no-soul Job must still prevent later soul introduction")
	require.True(t, isPermanentAISoulConfigurationError(err))
}
