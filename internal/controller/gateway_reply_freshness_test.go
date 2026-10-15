package controller

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestCreateTaskJobGatewayReplyDefersStalePolicy(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.Task, *corev1alpha1.Agent)
	}{
		{"Task AI policy", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Generation++
			task.Spec.AI = &corev1alpha1.AISpec{Tools: []string{}}
		}},
		{"Task runtime policy", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Generation++
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"reply_in_conversation"}}
		}},
		{"Agent empty policy", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Generation++
			agent.Spec.Tools = []corev1alpha1.ToolReference{}
		}},
		{"Agent disabled policy", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Generation++
			agent.Spec.Tools = []corev1alpha1.ToolReference{{Name: "reply_in_conversation", Enabled: new(false)}}
		}},
		{"Agent replacement same generation", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.UID = "replacement-agent-uid"
			agent.Spec.Tools = []corev1alpha1.ToolReference{}
		}},
		{"Task provider reference", func(task *corev1alpha1.Task, _ *corev1alpha1.Agent) {
			task.Generation++
			task.Spec.AI = &corev1alpha1.AISpec{ProviderRef: &corev1alpha1.ProviderReference{Name: "new-provider"}}
		}},
		{"Agent provider reference", func(_ *corev1alpha1.Task, agent *corev1alpha1.Agent) {
			agent.Generation++
			agent.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: "new-provider"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, task, agent := newGatewayReplyFreshnessFixture(t)
			latestTask, latestAgent := task.DeepCopy(), agent.DeepCopy()
			test.mutate(latestTask, latestAgent)
			// Policy changed before the uncached Task read; cached resolution still
			// supplies the old granting Agent and its already-resolved provider.
			r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(latestTask, latestAgent).Build()
			resolvedAgent, err := r.resolveAgent(t.Context(), task)
			require.NoError(t, err)
			provider, err := r.resolveProvider(t.Context(), task, resolvedAgent)
			require.NoError(t, err)
			result, err := r.createTaskJob(t.Context(), task, resolvedAgent, provider)
			require.NoError(t, err)
			requireNoGatewayJobFrozen(t, r, task)
			require.Equal(t, time.Second, result.RequeueAfter)
		})
	}
}

func TestCreateTaskJobGatewayReplyRequiresLiveAgent(t *testing.T) {
	for _, state := range []string{"missing", "deleting", "unreadable", "unresolved", "missing reference"} {
		t.Run(state, func(t *testing.T) {
			r, task, agent := newGatewayReplyFreshnessFixture(t)
			latestAgent := agent.DeepCopy()
			objects := []client.Object{task.DeepCopy()}
			if state != "missing" {
				if state == "deleting" {
					latestAgent.DeletionTimestamp = new(metav1.Now())
					latestAgent.Finalizers = []string{"test.orka.ai/hold-deletion"}
				}
				objects = append(objects, latestAgent)
			}
			reader := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(objects...).Build()
			r.APIReader = reader
			readErr := errors.New("Agent API unavailable")
			if state == "unreadable" {
				r.APIReader = interceptor.NewClient(reader, interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1alpha1.Agent); ok {
							return readErr
						}
						return c.Get(ctx, key, obj, opts...)
					},
				})
			}
			if state == "unresolved" {
				agent = nil
			}
			if state == "missing reference" {
				task.Spec.AgentRef = nil
			}
			result, err := r.createTaskJob(t.Context(), task, agent, nil)
			if state == "missing" || state == "unreadable" {
				require.Error(t, err)
				if state == "unreadable" {
					require.ErrorIs(t, err, readErr)
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, time.Second, result.RequeueAfter)
			}
			requireNoGatewayJobFrozen(t, r, task)
		})
	}
}

func TestCreateTaskJobGatewayReplyDefersReplacedTask(t *testing.T) {
	r, task, agent := newGatewayReplyFreshnessFixture(t)
	r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(task.DeepCopy(), agent).Build()
	// The durable event belongs to the new Task; the caller resolved the old UID.
	task.UID = "old-task-uid"
	result, err := r.createTaskJob(t.Context(), task, agent, nil)
	require.NoError(t, err)
	requireNoGatewayJobFrozen(t, r, task)
	require.Equal(t, time.Second, result.RequeueAfter)
}

func TestCreateTaskJobGatewayReplyFreshPolicy(t *testing.T) {
	for _, policy := range []string{"default grant", "Agent empty", "Agent disabled", "Task AI empty", "Task runtime deny", "transaction deny"} {
		t.Run(policy, func(t *testing.T) {
			r, task, agent := newGatewayReplyFreshnessFixture(t)
			switch policy {
			case "Agent empty":
				agent.Spec.Tools = []corev1alpha1.ToolReference{}
			case "Agent disabled":
				agent.Spec.Tools = []corev1alpha1.ToolReference{{Name: "reply_in_conversation", Enabled: new(false)}}
			case "Task AI empty":
				task.Spec.AI = &corev1alpha1.AISpec{Tools: []string{}}
			case "Task runtime deny":
				task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{"reply_in_conversation"}}
			case "transaction deny":
				task.Spec.Transaction = &corev1alpha1.TaskTransaction{Context: map[string]string{"allowedTools": "[]"}}
			}
			latestAgent := agent.DeepCopy()
			latestAgent.ResourceVersion = "2000" // Metadata/status changes do not invalidate the tuple.
			r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(task, latestAgent).Build()
			provider, err := r.resolveProvider(t.Context(), task, agent)
			require.NoError(t, err)
			_, err = r.createTaskJob(t.Context(), task, agent, provider)
			require.NoError(t, err)
			require.Equal(t, corev1alpha1.TaskPhaseRunning, task.Status.Phase)
			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(t.Context(), jobs))
			require.Len(t, jobs.Items, 1)
			requireGatewayJobReplyGrant(t, &jobs.Items[0], policy == "default grant")
		})
	}
}

func TestCreateTaskJobGatewayReplyRecordedJobRecovery(t *testing.T) {
	for _, recorded := range []bool{true, false} {
		t.Run(map[bool]string{true: "recorded", false: "unbound"}[recorded], func(t *testing.T) {
			r, task, agent := newGatewayReplyFreshnessFixture(t)
			job, err := r.JobBuilder.BuildWithOptions(t.Context(), task, agent, nil, JobBuildOptions{GatewayReplyEligible: true})
			require.NoError(t, err)
			job.UID = "frozen-job-uid"
			require.NoError(t, controllerutil.SetControllerReference(task, job, r.Scheme))
			require.NoError(t, r.Create(t.Context(), job))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(job), job))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(task), task))
			if recorded {
				task.Status.JobName, task.Status.JobUID = job.Name, string(job.UID)
				require.NoError(t, r.Status().Update(t.Context(), task))
			}
			latest := task.DeepCopy()
			latest.Generation++
			latest.Spec.AI = &corev1alpha1.AISpec{Tools: []string{}}
			// Even unavailable Agent state must not replan an existing Job.
			r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(latest, job).Build()
			_, err = r.createTaskJob(t.Context(), task, agent, nil)
			require.NoError(t, err)
			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(t.Context(), jobs))
			if recorded {
				require.Equal(t, corev1alpha1.TaskPhaseRunning, task.Status.Phase)
				require.Equal(t, string(job.UID), task.Status.JobUID)
				require.Len(t, jobs.Items, 1)
				require.Equal(t, job.Spec, jobs.Items[0].Spec, "frozen Job is unchanged")
				requireGatewayJobReplyGrant(t, &jobs.Items[0], true)
			} else {
				require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
				require.Contains(t, task.Status.Message, "no matching recorded UID")
				require.Empty(t, jobs.Items)
			}
		})
	}
}

func TestCreateTaskJobGatewayReplyDoesNotRecreateObservedJob(t *testing.T) {
	r, task, agent := newGatewayReplyFreshnessFixture(t)
	job, err := r.JobBuilder.BuildWithOptions(t.Context(), task, agent, nil, JobBuildOptions{GatewayReplyEligible: true})
	require.NoError(t, err)
	job.UID = "recorded-job-uid"
	require.NoError(t, controllerutil.SetControllerReference(task, job, r.Scheme))
	require.NoError(t, r.Create(t.Context(), job))
	latest := task.DeepCopy()
	latest.Status.JobName, latest.Status.JobUID = job.Name, string(job.UID)
	latest.Generation++
	latest.Spec.AI = &corev1alpha1.AISpec{Tools: []string{}}
	reader := fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(latest, job).Build()
	observed := false
	r.APIReader = interceptor.NewClient(reader, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, obj, opts...)
			if _, ok := obj.(*batchv1.Job); ok && err == nil && !observed {
				observed = true
				// A previously frozen Job disappears after the preflight read.
				require.NoError(t, c.Delete(ctx, obj))
				require.NoError(t, r.Delete(ctx, job))
			}
			return err
		},
	})
	_, err = r.createTaskJob(t.Context(), task, agent, nil)
	require.NoError(t, err)
	require.True(t, observed)
	require.Equal(t, corev1alpha1.TaskPhaseFailed, task.Status.Phase)
	require.Contains(t, task.Status.Message, "disappeared during recovery")
	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(t.Context(), jobs))
	require.Empty(t, jobs.Items, "a skipped freshness check must never authorize a replacement execution")
}

func TestCreateTaskJobGatewayReplyOldJobUIDDoesNotBypassFreshness(t *testing.T) {
	for _, previousAttempt := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded Job missing", true: "previous attempt Job exists"}[previousAttempt], func(t *testing.T) {
			r, task, agent := newGatewayReplyFreshnessFixture(t)
			oldJob, err := r.JobBuilder.BuildWithOptions(t.Context(), task, agent, nil, JobBuildOptions{GatewayReplyEligible: true})
			require.NoError(t, err)
			oldJob.UID = "previous-job-uid"
			task.Status.JobName, task.Status.JobUID = oldJob.Name, string(oldJob.UID)
			if previousAttempt {
				task.Status.Attempts = 1
			}
			latestAgent := agent.DeepCopy()
			latestAgent.Generation++
			latestAgent.Spec.Tools = []corev1alpha1.ToolReference{}
			objects := []client.Object{task.DeepCopy(), latestAgent}
			if previousAttempt {
				objects = append(objects, oldJob)
			}
			r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(objects...).Build()
			before := task.Status.DeepCopy()
			result, err := r.createTaskJob(t.Context(), task, agent, nil)
			require.NoError(t, err)
			require.Equal(t, time.Second, result.RequeueAfter)
			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(t.Context(), jobs))
			require.Empty(t, jobs.Items, "old binding must not admit a stale new Job")
			require.Equal(t, *before, task.Status)
		})
	}
}

func TestCreateTaskJobNonGatewayAIFreshnessUnchanged(t *testing.T) {
	for _, referencedAgent := range []bool{true, false} {
		t.Run(map[bool]string{true: "stale Agent", false: "no Agent reference"}[referencedAgent], func(t *testing.T) {
			r, task, agent := newGatewayReplyFreshnessFixture(t)
			task.Spec.RequestedBy = nil
			task.Labels, task.Annotations = nil, nil
			if !referencedAgent {
				task.Spec.AgentRef, agent = nil, nil
			}
			latest := task.DeepCopy()
			latest.Generation++
			// No live Agent is available; non-gateway behavior must not gain a new fence.
			r.APIReader = fake.NewClientBuilder().WithScheme(r.Scheme).WithObjects(latest).Build()
			_, err := r.createTaskJob(t.Context(), task, agent, nil)
			require.NoError(t, err)
			require.Equal(t, corev1alpha1.TaskPhaseRunning, task.Status.Phase)
			jobs := &batchv1.JobList{}
			require.NoError(t, r.List(t.Context(), jobs))
			require.Len(t, jobs.Items, 1)
			requireGatewayJobReplyGrant(t, &jobs.Items[0], false)
		})
	}
}

func requireGatewayJobReplyGrant(t *testing.T, job *batchv1.Job, enabled bool) {
	t.Helper()
	env := job.Spec.Template.Spec.Containers[0].Env
	flag, ok := findEnvVar(env, workerenv.GatewayReplyEnabled)
	require.True(t, ok)
	require.Equal(t, enabled, flag.Value == "true")
	names, ok := findEnvVar(env, workerenv.AITools)
	require.True(t, ok)
	if enabled {
		require.Contains(t, workerenv.SplitCSV(names.Value), "reply_in_conversation")
	} else {
		require.NotContains(t, workerenv.SplitCSV(names.Value), "reply_in_conversation")
	}
}

func requireNoGatewayJobFrozen(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) {
	t.Helper()
	jobs := &batchv1.JobList{}
	require.NoError(t, r.List(t.Context(), jobs))
	for _, job := range jobs.Items {
		env := job.Spec.Template.Spec.Containers[0].Env
		flag, _ := findEnvVar(env, workerenv.GatewayReplyEnabled)
		names, _ := findEnvVar(env, workerenv.AITools)
		require.NotEqual(t, "true", flag.Value, "stale policy must not freeze the reply grant")
		require.NotContains(t, workerenv.SplitCSV(names.Value), "reply_in_conversation")
	}
	require.Empty(t, jobs.Items, "stale inputs must defer the whole Job, not silently remove the tool")
	require.Empty(t, task.Status.JobName)
	require.Empty(t, task.Status.JobUID)
	require.Equal(t, corev1alpha1.TaskPhasePending, task.Status.Phase)
	require.Zero(t, task.Status.Attempts)
}

func newGatewayReplyFreshnessFixture(t *testing.T) (*TaskReconciler, *corev1alpha1.Task, *corev1alpha1.Agent) {
	t.Helper()
	scheme := newTestScheme()
	require.NoError(t, gatewayv1alpha1.AddToScheme(scheme))
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gateway-task", Namespace: "default", UID: "task-uid", Generation: 1,
			Labels: map[string]string{
				gatewayruntime.TaskGatewayNameLabel: "chat", gatewayruntime.TaskGatewayBindingLabel: "room", gatewayruntime.TaskGatewayEventLabel: "gev-test",
			},
			Annotations: map[string]string{
				gatewayruntime.TaskGatewayEventAnnotation: "gev-test", gatewayruntime.TaskGatewayExternalEvent: "external", gatewayruntime.TaskGatewaySession: "session",
				gatewayruntime.TaskGatewayNameAnnotation: "chat", gatewayruntime.TaskGatewayBindingAnnotation: "room",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: "assistant"},
			SessionRef:  &corev1alpha1.SessionReference{Name: "session", ThroughMessageID: "gateway:gev-test:user", PromptIncluded: true},
			RequestedBy: &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/ns-uid/chat/gateway-uid", Subject: "sender", Groups: []string{"gateway:chat"}, Roles: []string{"gateway-sender"}},
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:       &corev1alpha1.ModelConfig{Provider: "openai", Name: "test-model"},
			ProviderRef: &corev1alpha1.ProviderReference{Name: "original-provider"},
		},
	}
	provider := &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: "original-provider", Namespace: "default", UID: "provider-uid"},
		Spec:       corev1alpha1.ProviderSpec{Type: corev1alpha1.ProviderTypeOpenAI},
		Status:     corev1alpha1.ProviderStatus{Ready: true},
	}
	gateway := &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", UID: "gateway-uid", Generation: 1}}
	r := newUnitReconciler(scheme, task, agent, provider, gateway, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "ns-uid"}})
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	s := sqlite.NewStore(db, ":memory:")
	now := time.Now().UTC()
	event := store.GatewayEvent{
		ID: "gev-test", Namespace: "default", NamespaceUID: "ns-uid", GatewayName: "chat", GatewayUID: "gateway-uid", GatewayGeneration: 1,
		BindingName: "room", AgentName: "assistant", ExternalEventID: "external", ProtocolVersion: protocol.Version, EventType: "text",
		AccountID: "account", ContextID: "room", SenderID: "sender", Text: "hello", SessionName: "session", TaskName: "gateway-task",
		ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}
	_, _, err = s.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
	require.NoError(t, err)
	_, err = s.ClaimNextGatewayEvent(t.Context(), "default", "dispatch", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, s.MarkGatewayEventTaskCreated(t.Context(), "default", event.ID, task.Name, string(task.UID), "dispatch", now))
	r.GatewayService = gatewayruntime.NewService(r.Client, s, s, s, gatewayruntime.DefaultConfig())
	return r, task, agent
}
