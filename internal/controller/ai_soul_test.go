package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestAISoulBindingAndLiteralDelivery(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "a", UID: "agent-uid", Generation: 1}, Spec: corev1alpha1.AgentSpec{SystemPrompt: &corev1alpha1.PromptSource{Inline: "agent role"}, Soul: &corev1alpha1.SoulSource{Inline: "Literal $(VOICE_NOTE), $$ and 語."}}}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "t", UID: "task-uid", Generation: 1}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AI: &corev1alpha1.AISpec{SystemPrompt: "Task role"}, Env: []corev1.EnvVar{{Name: "VOICE_NOTE", Value: "untrusted"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, agent).Build()
	r := &TaskReconciler{Client: c, APIReader: c}
	prepared, err := r.prepareAISoul(context.Background(), task, agent)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status.SoulBinding == nil || !strings.HasPrefix(prepared.Prompt, "Task role") || strings.Contains(prepared.Prompt, "agent role") {
		t.Fatal("role precedence or binding failed")
	}
	if err := validatePreparedAISoul(task, agent, prepared); err != nil {
		t.Fatal(err)
	}
	env := NewJobBuilder(c).buildEnvVarsWithOptions(context.Background(), task, agent, nil, JobBuildOptions{AISoul: prepared})
	found := false
	for _, v := range env {
		if v.Name == workerenv.AISystemPrompt {
			found = true
			if v.Value != literalKubernetesPrompt(prepared.Prompt) || !strings.Contains(v.Value, "$$(VOICE_NOTE), $$$$") {
				t.Fatal("prompt is not literal-safe")
			}
		}
	}
	if !found {
		t.Fatal("missing prompt environment")
	}
	task.Status.Attempts = 1
	now := metav1.Now()
	task.Status.StartTime = &now
	if _, err := r.prepareAISoul(context.Background(), task, agent); err != nil {
		t.Fatalf("retry with an existing soul binding was rejected: %v", err)
	}
	agent.Spec.Soul.Inline = "replacement"
	if _, err := r.prepareAISoul(context.Background(), task, agent); err == nil {
		t.Fatal("retry accepted soul drift")
	}
	agent.Spec.Soul = nil
	if _, err := r.prepareAISoul(context.Background(), task, agent); err == nil {
		t.Fatal("retry accepted removal")
	}
	if task.Status.SoulBinding.PromptDigest != agentcontext.Digest(prepared.Prompt) {
		t.Fatal("stored prompt identity changed")
	}
}

type soulStateStore struct {
	store.SessionStore
	state store.SessionSoulState
}

func (s soulStateStore) ReadSessionSoul(context.Context, string, string, string, string) (store.SessionSoulState, error) {
	return s.state, nil
}

func TestAISoulSessionRevisionContract(t *testing.T) {
	binding := &corev1alpha1.TaskSoulBinding{TaskGeneration: 1, AgentUID: "agent", AgentGeneration: 1, SoulDigest: agentcontext.Digest("soul"), PromptDigest: agentcontext.Digest("role and soul")}
	digest := agentcontext.SessionDigest(binding)
	for _, test := range []struct {
		name      string
		state     store.SessionSoulState
		binding   *corev1alpha1.TaskSoulBinding
		append    bool
		wantError bool
	}{
		{name: "new canonical conversation", binding: binding, append: true},
		{name: "read-only cannot establish identity", binding: binding, wantError: true},
		{name: "same revision", state: store.SessionSoulState{Established: true, Digest: digest}, binding: binding},
		{name: "changed revision", state: store.SessionSoulState{Established: true, Digest: agentcontext.Digest("other")}, binding: binding, wantError: true},
		{name: "removal", state: store.SessionSoulState{Established: true, Digest: digest}, wantError: true},
		{name: "legacy cannot acquire soul", state: store.SessionSoulState{Established: true, MessageCount: 2}, binding: binding, wantError: true},
		{name: "legacy no-soul unchanged", state: store.SessionSoulState{Established: true, MessageCount: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager := NewSessionManager(soulStateStore{state: test.state})
			task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: "s", Append: test.append}}}
			err := manager.validateSoulContext(context.Background(), task, test.binding)
			if (err != nil) != test.wantError {
				t.Fatalf("want error=%v, got %v", test.wantError, err)
			}
		})
	}
}

func TestAISoulIntroductionAfterExecutionIsRejected(t *testing.T) {
	now := metav1.Now()
	for _, test := range []struct {
		name   string
		status corev1alpha1.TaskStatus
	}{
		{name: "retry after the old Job was cleared", status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1}},
		{name: "start time without attempt count", status: corev1alpha1.TaskStatus{StartTime: &now}},
		{name: "existing Job name", status: corev1alpha1.TaskStatus{JobName: "previous-job"}},
		{name: "existing Job identity", status: corev1alpha1.TaskStatus{JobUID: "previous-job-uid"}},
		{name: "autonomous iteration", status: corev1alpha1.TaskStatus{Iteration: 1}},
	} {
		for _, stale := range []bool{false, true} {
			name := test.name
			if stale {
				name += "/stale-reconcile-read"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				scheme := runtime.NewScheme()
				if err := corev1alpha1.AddToScheme(scheme); err != nil {
					t.Fatal(err)
				}
				agent := &corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "a", UID: "agent-uid", Generation: 2},
					Spec:       corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "new persona"}},
				}
				task := &corev1alpha1.Task{
					ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: "t", UID: "task-uid", Generation: 1},
					Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
					Status:     test.status,
				}
				c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task, agent).Build()
				r := &TaskReconciler{Client: c, APIReader: c}
				input := task.DeepCopy()
				if stale {
					input.Status = corev1alpha1.TaskStatus{}
				}
				if _, err := r.prepareAISoul(ctx, input, agent); err == nil || !strings.Contains(err.Error(), "cannot acquire a soul") {
					t.Fatalf("started Task acquired its first soul: %v", err)
				}
				stored := &corev1alpha1.Task{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(task), stored); err != nil {
					t.Fatal(err)
				}
				if stored.Status.SoulBinding != nil || input.Status.SoulBinding != nil {
					t.Fatal("rejected introduction persisted a soul binding")
				}

				// Unconfigured legacy retries keep their original behavior.
				agent.Spec.Soul = nil
				prepared, err := r.prepareAISoul(ctx, stored, agent)
				if err != nil || prepared != nil || stored.Status.SoulBinding != nil {
					t.Fatalf("legacy no-soul retry changed: prepared=%v, error=%v", prepared != nil, err)
				}
			})
		}
	}
}
