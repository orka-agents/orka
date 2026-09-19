package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestAISoulPendingSpecChangeRetriesWithoutTerminalizing(t *testing.T) {
	for _, directPrepare := range []bool{false, true} {
		name := "Job preflight"
		if directPrepare {
			name = "binding CAS"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "old prompt"},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
			}
			current := task.DeepCopy()
			current.Generation = 2
			current.Spec.Prompt = "edited prompt"
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
				Spec:       corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "persona"}},
			}
			r := newUnitReconciler(newTestScheme(), current, agent)
			var err error
			if directPrepare {
				_, err = r.prepareAISoul(ctx, task, agent)
			} else {
				_, err = r.createTaskJob(ctx, task, agent, nil)
			}
			if !apierrors.IsConflict(err) || isPermanentAISoulConfigurationError(err) {
				t.Fatalf("concurrent edit was not a retryable conflict: %v", err)
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != corev1alpha1.TaskPhasePending || current.Status.SoulBinding != nil || current.Status.Attempts != 0 || current.Spec.Prompt != "edited prompt" {
				t.Fatal("stale preparation consumed or overwrote the edited Task")
			}
			var jobs batchv1.JobList
			if err := r.List(ctx, &jobs); err != nil || len(jobs.Items) != 0 {
				t.Fatal("stale preparation launched a Job")
			}
			prepared, err := r.prepareAISoul(ctx, current, agent)
			if err != nil || prepared.UserPrompt != "edited prompt" || prepared.Binding.TaskGeneration != 2 {
				t.Fatalf("fresh retry did not prepare the edited Task: %v", err)
			}
		})
	}
}

func TestAISoulJobPreparationUsesFreshBindingStatus(t *testing.T) {
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "prompt"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	stale := task.DeepCopy()
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec:       corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "persona"}},
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	if _, err := r.prepareAISoul(ctx, task, agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Soul = nil
	if _, err := r.createTaskJob(ctx, stale, agent, nil); err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Task{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed || current.Status.SoulBinding == nil {
		t.Fatal("fresh binding was ignored after soul removal")
	}
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs); err != nil || len(jobs.Items) != 0 {
		t.Fatal("a Job started after a previously bound soul was removed")
	}
}

func TestAISoulJobPreparationUsesFreshAttemptStatus(t *testing.T) {
	ctx := context.Background()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: "task-uid", Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, Prompt: "prompt"},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default", UID: "agent-uid", Generation: 1},
		Spec:       corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "persona"}},
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	if _, err := r.prepareAISoul(ctx, task, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	stale := task.DeepCopy()
	task.Status.Attempts = 2
	if err := r.Status().Update(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := r.createTaskJob(ctx, stale, agent, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Phase != corev1alpha1.TaskPhaseRunning || task.Status.Attempts != 3 || task.Status.SoulBinding == nil {
		t.Fatalf("Job launch did not preserve fresh binding and attempt status: phase=%s attempts=%d", task.Status.Phase, task.Status.Attempts)
	}
}
