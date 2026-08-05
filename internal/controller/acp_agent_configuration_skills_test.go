package controller

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const unsupportedACPAgentSkillsMessage = "built-in ACP runtimes do not support Agent.spec.skills; refusing to omit declared skills"

func TestPlanACPRuntimeRejectsDeclaredAgentSkills(t *testing.T) {
	task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent}}
	for _, runtimeType := range []corev1alpha1.AgentRuntimeType{
		corev1alpha1.AgentRuntimeCodex,
		corev1alpha1.AgentRuntimeClaude,
		corev1alpha1.AgentRuntimeCopilot,
	} {
		t.Run(string(runtimeType), func(t *testing.T) {
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{UID: types.UID("agent-uid"), Generation: 1},
				Spec: corev1alpha1.AgentSpec{
					Model:   &corev1alpha1.ModelConfig{Name: "model"},
					Runtime: &corev1alpha1.AgentCLIRuntime{Type: runtimeType},
					Skills:  []corev1alpha1.SkillReference{{Name: "required-skill"}},
				},
			}

			_, err := PlanACPRuntime(task, agent, ACPRuntimeImages{})
			if err == nil {
				t.Fatal("PlanACPRuntime() succeeded and silently omitted Agent.spec.skills")
			}
			if err.Error() != unsupportedACPAgentSkillsMessage {
				t.Fatalf("PlanACPRuntime() error = %q, want %q", err, unsupportedACPAgentSkillsMessage)
			}
			if !isPermanentACPAgentConfigurationError(err) {
				t.Fatalf("PlanACPRuntime() error = %v, want permanent ACP configuration error", err)
			}
		})
	}
}

func TestQueueACPRuntimeTaskRejectsAgentSkillsBeforePoolDemand(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "task-with-skills",
			UID:        types.UID("11111111-2222-3333-4444-555555555555"),
			Generation: 1,
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "use the required skill"},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "default",
			Name:       "agent-with-skills",
			UID:        types.UID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"),
			Generation: 1,
		},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Name: acpTestModel},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex},
			Skills:  []corev1alpha1.SkillReference{{Name: "required-skill"}},
		},
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).
		WithObjects(task).
		Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sqlite.NewStore(db, "test")
	epochs := NewControllerEpochManager(controlStore, "controller-test")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	t.Cleanup(func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("controller epoch manager shutdown: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := epochs.CurrentFence(ctx); err != nil {
		t.Fatal(err)
	}

	reconciler := &TaskReconciler{
		Client: kubeClient, Scheme: scheme, DurableControlStore: controlStore,
		ControllerEpochManager: epochs, ACPRuntimeEnabled: true, ACPRuntimeNamespace: "orka-runtimes",
		ACPRuntimeImages: ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)},
	}
	if _, err := reconciler.queueACPRuntimeTask(ctx, task.DeepCopy(), agent); err != nil {
		t.Fatalf("queueACPRuntimeTask() error = %v, want terminal InvalidRuntimeProfile status", err)
	}

	var pools corev1alpha1.RuntimePoolList
	if err := kubeClient.List(ctx, &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 0 {
		t.Fatalf("skills rejection unexpectedly created RuntimePools: %#v", pools.Items)
	}
	failed := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status.Phase != corev1alpha1.TaskPhaseFailed || failed.Status.Message != unsupportedACPAgentSkillsMessage ||
		failed.Status.Attempts != 0 || failed.Status.Execution == nil ||
		failed.Status.Execution.State != corev1alpha1.TaskExecutionStateFailed ||
		failed.Status.Execution.Reason != corev1alpha1.TaskExecutionReason("InvalidRuntimeProfile") ||
		failed.Status.Execution.Attempt != 0 || failed.Status.Execution.PromptID != "" || failed.Status.Execution.RuntimePoolName != "" ||
		failed.Status.Delivery == nil || failed.Status.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested ||
		failed.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeNotRequested {
		t.Fatalf("skills rejection status: %#v", failed.Status)
	}
	if failed.Labels[acpRuntimeTaskPoolLabel] != "" || failed.Annotations[acpRuntimeQueuedAtAnnotation] != "" {
		t.Fatalf("skills rejection recorded queue metadata: labels=%#v annotations=%#v", failed.Labels, failed.Annotations)
	}
	attemptID, err := (store.PromptAttemptKey{
		Namespace: task.Namespace,
		TaskUID:   string(task.UID),
		Attempt:   1,
		PromptID:  "prompt-" + string(task.UID) + "-1",
	}).CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controlStore.GetPromptAttempt(ctx, attemptID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("skills rejection durable attempt error = %v, want not found", err)
	}
}
