/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
)

type fakeRolloutVerifier struct {
	refuse string
	calls  int
}

func (f *fakeRolloutVerifier) VerifyAgentRollout(_ context.Context, agent *corev1alpha1.Agent) (*corev1alpha1.Agent, error) {
	f.calls++
	if f.refuse != "" {
		return nil, &evals.RolloutRefusal{Reason: f.refuse}
	}
	return &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent.Status.Rollout.Candidate.Name, Namespace: agent.Namespace}}, nil
}

func rolloutTestAgent(percent int32) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: "team-a", UID: "agent-uid", Generation: 2},
		Status: corev1alpha1.AgentStatus{Rollout: &corev1alpha1.AgentRolloutStatus{
			Candidate: &corev1alpha1.EvalObjectIdentity{Name: "reviewer-c-1", UID: "candidate-uid", Generation: 1},
			Percent:   percent, DecisionID: "d1", RecordID: "c1",
		}},
	}
}

func rolloutTestTask(name string, mutate func(*corev1alpha1.Task)) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "team-a", UID: types.UID(name + "-uid")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "reviewer"}, Prompt: "x"},
	}
	if mutate != nil {
		mutate(task)
	}
	return task
}

func newRolloutTestReconciler(t *testing.T, verifier AgentRolloutVerifier, objects ...client.Object) *TaskReconciler {
	t.Helper()
	kube := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&corev1alpha1.Agent{}, &corev1alpha1.Task{}).WithObjects(objects...).Build()
	return &TaskReconciler{Client: kube, RolloutVerifier: verifier}
}

func TestRouteAgentRollout(t *testing.T) {
	ctx := context.Background()
	t.Run("every standalone Task at 100 percent goes to the candidate", func(t *testing.T) {
		agent, task := rolloutTestAgent(100), rolloutTestTask("standalone", nil)
		verifier := &fakeRolloutVerifier{}
		r := newRolloutTestReconciler(t, verifier, agent, task)
		routed, err := r.routeAgentRollout(ctx, task, agent)
		if err != nil || routed.Name != "reviewer-c-1" || verifier.calls != 1 {
			t.Fatalf("routed = %v, %v (verifier calls %d)", routed, err, verifier.calls)
		}
		stored := &corev1alpha1.Task{}
		_ = r.Get(ctx, client.ObjectKeyFromObject(task), stored)
		if stored.Annotations[labels.AnnotationRolloutArm] != rolloutArmCandidate ||
			stored.Annotations[labels.AnnotationRolloutCandidate] != "reviewer-c-1" {
			t.Fatalf("Task annotations = %v", stored.Annotations)
		}
	})
	t.Run("EvalRun trials are never routed", func(t *testing.T) {
		agent := rolloutTestAgent(100)
		task := rolloutTestTask("trial", func(task *corev1alpha1.Task) {
			controller := true
			task.OwnerReferences = []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "EvalRun",
				Name: "run", UID: "run-uid", Controller: &controller}}
		})
		verifier := &fakeRolloutVerifier{}
		r := newRolloutTestReconciler(t, verifier, agent, task)
		if routed, err := r.routeAgentRollout(ctx, task, agent); err != nil || routed.Name != "reviewer" || verifier.calls != 0 {
			t.Fatalf("routed = %v, %v", routed, err)
		}
	})
	t.Run("a bound Task is not routed again", func(t *testing.T) {
		agent := rolloutTestAgent(100)
		task := rolloutTestTask("bound", func(task *corev1alpha1.Task) {
			task.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{}
		})
		r := newRolloutTestReconciler(t, &fakeRolloutVerifier{}, agent, task)
		if routed, _ := r.routeAgentRollout(ctx, task, agent); routed.Name != "reviewer" {
			t.Fatalf("routed = %v", routed)
		}
	})
	t.Run("a failed check runs the Task on the stable Agent and marks the canary invalid", func(t *testing.T) {
		agent, task := rolloutTestAgent(100), rolloutTestTask("refused", nil)
		r := newRolloutTestReconciler(t, &fakeRolloutVerifier{refuse: "the approval of evalrun/candidate is stale"}, agent, task)
		routed, err := r.routeAgentRollout(ctx, task, agent)
		if err != nil || routed.Name != "reviewer" {
			t.Fatalf("routed = %v, %v", routed, err)
		}
		stored := &corev1alpha1.Agent{}
		_ = r.Get(ctx, client.ObjectKeyFromObject(agent), stored)
		condition := meta.FindStatusCondition(stored.Status.Conditions, AgentConditionRolloutInvalid)
		if condition == nil || condition.Status != metav1.ConditionTrue || condition.Message != "the approval of evalrun/candidate is stale" {
			t.Fatalf("conditions = %+v", stored.Status.Conditions)
		}
	})
	t.Run("without a verifier every Task runs on the stable Agent", func(t *testing.T) {
		agent, task := rolloutTestAgent(100), rolloutTestTask("unverified", nil)
		r := newRolloutTestReconciler(t, nil, agent, task)
		if routed, err := r.routeAgentRollout(ctx, task, agent); err != nil || routed.Name != "reviewer" {
			t.Fatalf("routed = %v, %v", routed, err)
		}
	})
	t.Run("a Session keeps the arm its first routed Task took", func(t *testing.T) {
		agent := rolloutTestAgent(1)
		session := func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "chat", Create: true}
		}
		earlier := rolloutTestTask("first", session)
		earlier.Labels = map[string]string{labels.LabelRolloutSession: rolloutSessionLabel("team-a", "chat")}
		earlier.Annotations = map[string]string{labels.AnnotationRolloutArm: rolloutArmCandidate, labels.AnnotationRolloutCandidate: "reviewer-c-1"}
		next := rolloutTestTask("second", session)
		r := newRolloutTestReconciler(t, &fakeRolloutVerifier{}, agent, earlier, next)
		routed, err := r.routeAgentRollout(ctx, next, agent)
		if err != nil || routed.Name != "reviewer-c-1" {
			t.Fatalf("routed = %v, %v", routed, err)
		}
		stored := &corev1alpha1.Task{}
		_ = r.Get(ctx, client.ObjectKeyFromObject(next), stored)
		if stored.Labels[labels.LabelRolloutSession] != rolloutSessionLabel("team-a", "chat") {
			t.Fatalf("Task labels = %v", stored.Labels)
		}
	})
	t.Run("an unknown Session without a session store stays on the stable Agent", func(t *testing.T) {
		agent := rolloutTestAgent(100)
		task := rolloutTestTask("session", func(task *corev1alpha1.Task) {
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "chat", Create: true}
		})
		r := newRolloutTestReconciler(t, &fakeRolloutVerifier{}, agent, task)
		if routed, err := r.routeAgentRollout(ctx, task, agent); err != nil || routed.Name != "reviewer" {
			t.Fatalf("routed = %v, %v", routed, err)
		}
	})
}

// Buckets spread keys over 0..99, so a percent routes about that share.
func TestRolloutBucketSpread(t *testing.T) {
	candidate := 0
	for i := range 2000 {
		if bucketArm("task/"+string(rune('a'+i%26))+string(rune(i)), 25) == rolloutArmCandidate {
			candidate++
		}
	}
	if candidate < 400 || candidate > 600 {
		t.Fatalf("%d of 2000 keys took the candidate arm at 25%%", candidate)
	}
	if bucketArm("x", 0) != rolloutArmStable || bucketArm("x", 100) != rolloutArmCandidate {
		t.Fatal("0 and 100 percent are not absolute")
	}
}
