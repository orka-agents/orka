package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

func TestGatewayTerminalSoulUnboundRequiresPreExecutionConfigurationFailure(t *testing.T) {
	now := metav1.Now()
	for _, tc := range []struct {
		name     string
		mutate   func(*corev1alpha1.Task)
		unbound  bool
		jobOwner string
	}{
		{name: "unbound soul configuration failure", unbound: true},
		{name: "missing Task UID", mutate: func(task *corev1alpha1.Task) { task.UID = "" }},
		{name: "owned Job despite missing start status", jobOwner: "current"},
		{name: "unrelated Job with same owner name", jobOwner: "other-task-uid", unbound: true},
		{name: "unknown failure", mutate: func(task *corev1alpha1.Task) { task.Status.Message = "failed" }},
		{name: "agent task", mutate: func(task *corev1alpha1.Task) { task.Spec.Type = corev1alpha1.TaskTypeAgent }},
		{name: "cancelled", mutate: func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseCancelled }},
		{name: "no-soul success", mutate: func(task *corev1alpha1.Task) { task.Status.Phase = corev1alpha1.TaskPhaseSucceeded }},
		{name: "previous attempt", mutate: func(task *corev1alpha1.Task) { task.Status.Attempts = 1 }},
		{name: "start time", mutate: func(task *corev1alpha1.Task) { task.Status.StartTime = &now }},
		{name: "job name", mutate: func(task *corev1alpha1.Task) { task.Status.JobName = "job" }},
		{name: "job UID", mutate: func(task *corev1alpha1.Task) { task.Status.JobUID = "job-uid" }},
		{name: "iteration", mutate: func(task *corev1alpha1.Task) { task.Status.Iteration = 1 }},
		{name: "execution", mutate: func(task *corev1alpha1.Task) { task.Status.Execution = &corev1alpha1.TaskExecutionStatus{} }},
		{name: "outcome", mutate: func(task *corev1alpha1.Task) {
			task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{}
		}},
		{name: "result", mutate: func(task *corev1alpha1.Task) { task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true} }},
		{name: "bound before failure", mutate: func(task *corev1alpha1.Task) {
			task.Status.SoulBinding = &corev1alpha1.TaskSoulBinding{
				TaskGeneration: 1, AgentUID: "agent-uid", AgentGeneration: 1,
				SoulDigest: agentcontext.Digest("persona"), PromptDigest: agentcontext.Digest("role and persona"),
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service, ss, _ := newGatewayServiceFixture(t)
			setGatewayAgentNativeAI(t, service, true)
			accepted, err := service.AdmitEvent(ctx, "default", "chat", "Bearer inbound-token", gatewayEventBody(t, "soul-error", "user-1"))
			require.NoError(t, err)
			require.NoError(t, service.DispatchOnce(ctx))
			event, err := ss.GetGatewayEvent(ctx, "default", accepted.EventID)
			require.NoError(t, err)
			task := &corev1alpha1.Task{}
			require.NoError(t, service.Client.Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.TaskName}, task))
			task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed, Message: "AI soul configuration: soul content does not match its digest"}
			if tc.mutate != nil {
				tc.mutate(task)
			}
			if tc.jobOwner != "" {
				owner := metav1.NewControllerRef(task, corev1alpha1.GroupVersion.WithKind("Task"))
				if tc.jobOwner != "current" {
					owner.UID = types.UID(tc.jobOwner)
				}
				job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "unrecorded-job", Namespace: task.Namespace, OwnerReferences: []metav1.OwnerReference{*owner},
				}}
				// Only the uncached reader sees this Job. Labels are not authority.
				service.APIReader = fake.NewClientBuilder().WithScheme(service.Client.Scheme()).WithObjects(job).Build()
			}
			// Exercise projection directly for all status/type boundary cases.
			projected, err := service.projectTerminal(ctx, event, task)
			require.NoError(t, err)
			require.True(t, projected)
			messages, err := ss.LoadTranscript(ctx, event.Namespace, event.SessionName, 0)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			if tc.unbound {
				require.Equal(t, "true", messages[1].Metadata[store.SessionSoulUnboundMetadata])
			} else {
				require.NotContains(t, messages[1].Metadata, store.SessionSoulUnboundMetadata)
			}
			require.Equal(t, agentcontext.SessionDigest(task.Status.SoulBinding), messages[1].Metadata[store.SessionSoulDigestMetadata])
		})
	}
}

type failingGatewaySoulJobReader struct {
	client.Reader
	err error
}

func (r failingGatewaySoulJobReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if _, ok := list.(*batchv1.JobList); ok {
		return r.err
	}
	return r.Reader.List(ctx, list, options...)
}

func TestGatewaySoulUnboundJobReadFailureRetriesProjection(t *testing.T) {
	ctx := context.Background()
	service, ss, _ := newGatewayServiceFixture(t)
	setGatewayAgentNativeAI(t, service, true)
	accepted, err := service.AdmitEvent(ctx, "default", "chat", "Bearer inbound-token", gatewayEventBody(t, "soul-job-read", "user-1"))
	require.NoError(t, err)
	require.NoError(t, service.DispatchOnce(ctx))
	event, err := ss.GetGatewayEvent(ctx, "default", accepted.EventID)
	require.NoError(t, err)
	task := &corev1alpha1.Task{}
	require.NoError(t, service.Client.Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.TaskName}, task))
	task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed, Message: "AI soul configuration: invalid soul"}
	outage := errors.New("Job read unavailable")
	service.APIReader = failingGatewaySoulJobReader{Reader: service.Client, err: outage}
	projected, err := service.projectTerminal(ctx, event, task)
	require.ErrorIs(t, err, outage)
	require.False(t, projected)
	session, err := ss.GetSession(ctx, event.Namespace, event.SessionName)
	require.NoError(t, err)
	require.Equal(t, task.Name, session.ActiveTask)
	require.Len(t, session.Messages, 1, "an uncertain read must not pin absence or claim an unbound failure")
	service.APIReader = service.Client
	projected, err = service.projectTerminal(ctx, event, task)
	require.NoError(t, err)
	require.True(t, projected)
	messages, err := ss.LoadTranscript(ctx, event.Namespace, event.SessionName, 0)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	require.Equal(t, "true", messages[1].Metadata[store.SessionSoulUnboundMetadata])
}
