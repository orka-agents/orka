package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
)

// The Task CRD rule alone, with no admission webhook, must keep a soul-bound
// Task pausable while freezing its execution inputs, and keep the whole spec
// of an ACP-bound Task frozen.
func TestTaskImmutabilityRuleOnAPIServer(t *testing.T) {
	apiClient := newArchivedSessionAPIClient(t)
	ctx := t.Context()
	digest := agentcontext.Digest("bound")

	// An ACP binding with a valid shape, produced by the real binder.
	acpSource := bindingTestTask()
	binder, _ := newBindingTestReconciler(t, acpSource, bindingTestNamespace())
	if _, err, handled := binder.ensureAgentExecutionBinding(ctx, acpSource, bindingTestAgent()); err != nil || handled {
		t.Fatalf("bind: %v / %v", err, handled)
	}
	if err := binder.Get(ctx, client.ObjectKeyFromObject(acpSource), acpSource); err != nil {
		t.Fatal(err)
	}
	soulBinding := func(task *corev1alpha1.Task) *corev1alpha1.TaskSoulBinding {
		return &corev1alpha1.TaskSoulBinding{TaskGeneration: task.Generation, AgentUID: "agent-uid", AgentGeneration: 1,
			SoulDigest: digest, PromptDigest: digest, InputsDigest: digest}
	}
	create := func(name string, bind func(*corev1alpha1.Task)) *corev1alpha1.Task {
		t.Helper()
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "iterate",
				AgentRef: &corev1alpha1.AgentReference{Name: "agent"}},
		}
		if err := apiClient.Create(ctx, task); err != nil {
			t.Fatal(err)
		}
		bind(task)
		if err := apiClient.Status().Update(ctx, task); err != nil {
			t.Fatalf("record execution authority: %v", err)
		}
		return task
	}
	patch := func(task *corev1alpha1.Task, mutate func(*corev1alpha1.Task)) error {
		current := &corev1alpha1.Task{}
		if err := apiClient.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			t.Fatal(err)
		}
		base := current.DeepCopy()
		mutate(current)
		return apiClient.Patch(ctx, current, client.MergeFrom(base))
	}
	rejected := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "Task spec is immutable after execution authority is recorded")
	}
	suspend := func(value *bool) func(*corev1alpha1.Task) {
		return func(task *corev1alpha1.Task) { task.Spec.Suspend = value }
	}
	prompt := func(task *corev1alpha1.Task) { task.Spec.Prompt = "a different task" }
	acpBinding := func(task *corev1alpha1.Task) {
		task.Status.AgentExecutionBinding = acpSource.Status.AgentExecutionBinding.DeepCopy()
	}

	soulBound := create("soul-bound", func(task *corev1alpha1.Task) { task.Status.SoulBinding = soulBinding(task) })
	if err := patch(soulBound, suspend(new(true))); err != nil {
		t.Fatalf("soul-bound Task could not be paused: %v", err)
	}
	if err := patch(soulBound, suspend(nil)); err != nil {
		t.Fatalf("soul-bound Task could not be resumed: %v", err)
	}
	if err := patch(soulBound, prompt); !rejected(err) {
		t.Fatalf("soul-bound execution input changed: %v", err)
	}
	if err := patch(soulBound, func(task *corev1alpha1.Task) { suspend(new(true))(task); prompt(task) }); !rejected(err) {
		t.Fatalf("a control-field edit carried an input edit: %v", err)
	}

	acpBound := create("acp-bound", acpBinding)
	if err := patch(acpBound, suspend(new(true))); !rejected(err) {
		t.Fatalf("ACP-bound Task spec changed: %v", err)
	}
	mixed := create("mixed-bound", func(task *corev1alpha1.Task) {
		acpBinding(task)
		task.Status.SoulBinding = soulBinding(task)
	})
	if err := patch(mixed, suspend(new(true))); !rejected(err) {
		t.Fatalf("Task with an ACP binding changed its spec: %v", err)
	}

	unbound := create("unbound", func(*corev1alpha1.Task) {})
	if err := patch(unbound, prompt); err != nil {
		t.Fatalf("unbound Task spec could not change: %v", err)
	}
}
