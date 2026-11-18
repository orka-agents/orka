package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
)

func TestAISoulDriftRetiresLostStatusJobBeforeTerminalizing(t *testing.T) {
	for _, drift := range []string{"Agent", "ConfigMap", "removed soul"} {
		t.Run(drift, func(t *testing.T) {
			ctx := context.Background()
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "persona", Namespace: "default"}, Data: map[string]string{"SOUL.md": "persona"}}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
				Spec: corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{
					ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: cm.Name, Key: "SOUL.md"}, Digest: agentcontext.Digest("persona"),
				}},
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
			}
			r := newUnitReconciler(newTestScheme(), task, agent, cm)
			base := r.Client
			r.Client = lostGatewayJobStatusClient{Client: base, err: context.DeadlineExceeded}
			_, err := r.createTaskJob(ctx, task, agent, nil)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			r.Client = base
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
			require.NotNil(t, task.Status.SoulBinding)
			require.Equal(t, corev1alpha1.TaskPhasePending, task.Status.Phase)
			require.Empty(t, task.Status.JobUID)
			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(ctx, jobs))
			require.Len(t, jobs.Items, 1)
			job := &jobs.Items[0]
			job.UID = "lost-status-job-uid"
			job.Finalizers = []string{"test.orka.ai/hold"}
			require.NoError(t, r.Update(ctx, job))
			switch drift {
			case "Agent":
				agent.Generation++
				agent.Spec.Soul = &corev1alpha1.SoulSource{Inline: "replacement"}
			case "ConfigMap":
				cm.Data["SOUL.md"] = "replacement"
				require.NoError(t, r.Update(ctx, cm))
			case "removed soul":
				agent.Spec.Soul = nil
			}
			for range 2 {
				result, err := r.createTaskJob(ctx, task, agent, nil)
				require.NoError(t, err)
				require.Equal(t, time.Second, result.RequeueAfter)
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(task), task))
				require.Equal(t, corev1alpha1.TaskPhasePending, task.Status.Phase)
				require.Nil(t, task.Status.CompletionTime)
				require.True(t, taskJobIdentityRejected(task), "replay must be fenced before deletion")
			}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(job), job))
			require.False(t, job.DeletionTimestamp.IsZero())
			job.Finalizers = nil
			require.NoError(t, r.Update(ctx, job))
			_, err = r.createTaskJob(ctx, task, agent, nil)
			require.NoError(t, err)
			require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
			require.True(t, strings.HasPrefix(task.Status.Message, "AI soul configuration:"))
			require.NoError(t, r.List(ctx, jobs))
			require.Empty(t, jobs.Items, "retired execution must not be recreated")
		})
	}
}
