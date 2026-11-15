package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

func TestAISoulConfigurationErrorClassification(t *testing.T) {
	invalidSource := agentcontext.ValidateSource(&corev1alpha1.SoulSource{Inline: "\x00"})
	for _, tc := range []struct {
		name      string
		err       error
		permanent bool
	}{
		{name: "invalid source", err: invalidSource, permanent: true},
		{name: "pinned identity", err: invalidAISoulConfiguration("configuration drift"), permanent: true},
		{name: "wrapped identity", err: fmt.Errorf("prepare: %w", invalidAISoulConfiguration("configuration drift")), permanent: true},
		{name: "invalid role source", err: permanentACPAgentConfiguration(errors.New("missing role source")), permanent: true},
		{name: "API unavailable", err: apierrors.NewServiceUnavailable("temporary API failure")},
		{name: "store unavailable", err: errors.New("temporary store failure")},
		{name: "context timeout", err: context.DeadlineExceeded},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPermanentAISoulConfigurationError(tc.err); got != tc.permanent {
				t.Fatalf("permanent = %v, want %v", got, tc.permanent)
			}
		})
	}
}

type failingAISoulReader struct {
	client.Reader
	err error
}

func (r failingAISoulReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*corev1.ConfigMap); ok {
		return r.err
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type failingAISoulStateReader struct {
	store.SessionStore
	err error
}

func (s failingAISoulStateReader) ReadSessionSoul(context.Context, string, string, string, string) (store.SessionSoulState, error) {
	return store.SessionSoulState{}, s.err
}

type failingAISoulStatusClient struct {
	client.Client
	err error
}

func (c failingAISoulStatusClient) Status() client.SubResourceWriter {
	return failingAISoulStatusWriter{SubResourceWriter: c.Client.Status(), err: c.err}
}

type failingAISoulStatusWriter struct {
	client.SubResourceWriter
	err error
}

func (s failingAISoulStatusWriter) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return s.err
}

func TestCreateTaskJobRetriesOperationalAISoulErrors(t *testing.T) {
	for _, failure := range []string{"ConfigMap read", "role ConfigMap read", "Session read", "status patch"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default", UID: "task-uid", Generation: 1},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "test prompt"},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default", UID: "agent-uid", Generation: 1},
				Spec:       corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "persona"}},
			}
			r := newUnitReconciler(newTestScheme(), task, agent)
			baseClient := r.Client
			outage := apierrors.NewServiceUnavailable("temporary dependency failure")
			switch failure {
			case "ConfigMap read":
				agent.Spec.Soul = &corev1alpha1.SoulSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "soul", Key: "SOUL.md"}, Digest: agentcontext.Digest("persona")}
				r.APIReader = failingAISoulReader{Reader: baseClient, err: outage}
			case "role ConfigMap read":
				agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "role", Key: "prompt"}}
				r.APIReader = failingAISoulReader{Reader: baseClient, err: outage}
			case "Session read":
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "session", Append: true}
				if err := baseClient.Update(ctx, task); err != nil {
					t.Fatal(err)
				}
				r.SessionManager = NewSessionManager(failingAISoulStateReader{SessionStore: r.SessionManager.store, err: outage})
			case "status patch":
				r.Client = failingAISoulStatusClient{Client: baseClient, err: outage}
				r.APIReader = baseClient
			}

			if _, err := r.createTaskJob(ctx, task, agent, nil); !errors.Is(err, outage) {
				t.Fatalf("operational failure was not returned for reconciliation retry: %v", err)
			}
			stored := &corev1alpha1.Task{}
			if err := baseClient.Get(ctx, client.ObjectKeyFromObject(task), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.Phase != corev1alpha1.TaskPhasePending || stored.Status.Attempts != 0 || stored.Status.CompletionTime != nil || stored.Status.SoulBinding != nil {
				t.Fatal("operational outage consumed or bound the Task")
			}
			var jobs batchv1.JobList
			if err := baseClient.List(ctx, &jobs); err != nil {
				t.Fatal(err)
			}
			if len(jobs.Items) != 0 {
				t.Fatal("a Job was launched without resolved soul configuration")
			}
		})
	}
}

func TestCreateTaskJobFailsPermanentAISoulConfiguration(t *testing.T) {
	for _, failure := range []string{"invalid soul text", "missing soul source", "missing role source"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: "default", UID: "task-uid", Generation: 1},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "default", UID: "agent-uid", Generation: 1},
				Spec:       corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "persona"}},
			}
			switch failure {
			case "invalid soul text":
				agent.Spec.Soul.Inline = "\x00"
			case "missing soul source":
				agent.Spec.Soul = &corev1alpha1.SoulSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "missing", Key: "SOUL.md"}, Digest: agentcontext.Digest("persona")}
			case "missing role source":
				agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "missing", Key: "prompt"}}
			}
			r := newUnitReconciler(newTestScheme(), task, agent)
			if _, err := r.createTaskJob(ctx, task, agent, nil); err != nil {
				t.Fatal(err)
			}
			stored := &corev1alpha1.Task{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(task), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.Phase != corev1alpha1.TaskPhaseFailed || stored.Status.Attempts != 0 || stored.Status.SoulBinding != nil {
				t.Fatal("permanent source failure was not terminalized before execution")
			}
		})
	}
}
