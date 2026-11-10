/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const promotionTestPrompt = "Hold back any change with a defect."

var promotionTestActor = store.EvalDecisionActor{Username: "alice", Groups: []string{"promoters"}}

// newPromotionEnv runs a passing experiment whose candidate belongs to the
// reviewer, and lets edit change the candidate first.
func newPromotionEnv(t *testing.T, edit func(*corev1alpha1.Agent)) *experimentEnv {
	t.Helper()
	baseline, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	agent := env.getAgent("candidate-agent")
	agent.Labels = map[string]string{labels.LabelCandidateOf: "reviewer"}
	agent.OwnerReferences = []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Agent", Name: "reviewer", UID: "agent-uid"}}
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: promotionTestPrompt}
	if edit != nil {
		edit(agent)
	}
	if err := env.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", "needs_changes")
	env.reconcileRun("candidate")
	if err := env.reconciler.RecoverAgentPromotions(context.Background()); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *experimentEnv) getAgent(name string) *corev1alpha1.Agent {
	e.t.Helper()
	agent := &corev1alpha1.Agent{}
	if err := e.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: name}, agent); err != nil {
		e.t.Fatal(err)
	}
	return agent
}

// decide appends a decision on the experiment.
func (e *experimentEnv) decide(id, decision string) {
	e.t.Helper()
	outcome := e.experimentEntry().Outcome
	if outcome == nil || outcome.Result != store.EvalExperimentResultPassed {
		e.t.Fatalf("outcome = %+v", outcome)
	}
	record := &store.EvalDecisionRecord{
		SchemaVersion: store.EvalDecisionSchemaVersion, ID: id, Namespace: evalTestNamespace,
		CandidateRun: "candidate", CandidateRunUID: "candidate-uid", Decision: decision, Reason: "reviewed",
		Actor: promotionTestActor, DecidedAt: e.now,
	}
	if decision == store.EvalDecisionApproved {
		record.Binding = outcome.Binding
	}
	if err := e.store.AppendEvalDecision(context.Background(), record); err != nil {
		e.t.Fatal(err)
	}
}

func refusedRollout(t *testing.T, err error, want string) {
	t.Helper()
	refusal, ok := errors.AsType[*evals.RolloutRefusal](err)
	if !ok || !strings.Contains(refusal.Reason, want) {
		t.Fatalf("error = %v, want a refusal containing %q", err, want)
	}
}

// An approved candidate's prompt and model are written onto the stable
// Agent, and a rollback writes back what the promotion replaced.
func TestPromoteAndRollBackAnAgent(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)

	record, err := env.reconciler.PromoteAgent(ctx, evalTestNamespace, "reviewer", "d1", promotionTestActor)
	if err != nil {
		t.Fatalf("PromoteAgent() error = %v", err)
	}
	if record.State != store.AgentPromotionComplete || record.Previous.Model != "claude-haiku" || record.Previous.SystemPrompt != "" ||
		record.Target != (store.AgentPromotionValues{SystemPrompt: promotionTestPrompt, Model: "claude-sonnet"}) ||
		record.CandidateAgent.Name != "candidate-agent" || record.AppliedDigest == "" || record.Actor.Username != "alice" {
		t.Fatalf("promotion = %+v", record)
	}
	stable := env.getAgent("reviewer")
	if stable.Spec.SystemPrompt == nil || stable.Spec.SystemPrompt.Inline != promotionTestPrompt || stable.Spec.Model.Name != "claude-sonnet" ||
		stable.Spec.Model.Provider != "anthropic" || stable.Annotations[labels.AnnotationPromotionRecord] != record.ID {
		t.Fatalf("promoted Agent = %+v", stable)
	}

	// The approval bound the reviewer as it was, so it cannot promote twice.
	_, err = env.reconciler.PromoteAgent(ctx, evalTestNamespace, "reviewer", "d1", promotionTestActor)
	if err == nil {
		t.Fatal("a second promotion of the same approval succeeded")
	}

	rollback, err := env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "regressed in production")
	if err != nil {
		t.Fatalf("RollbackAgent() error = %v", err)
	}
	if rollback.State != store.AgentPromotionComplete || rollback.PromotionID != record.ID || rollback.Target != record.Previous ||
		rollback.Reason != "regressed in production" {
		t.Fatalf("rollback = %+v", rollback)
	}
	stable = env.getAgent("reviewer")
	if stable.Spec.SystemPrompt != nil || stable.Spec.Model.Name != "claude-haiku" || stable.Annotations[labels.AnnotationPromotionRecord] != rollback.ID {
		t.Fatalf("rolled back Agent = %+v", stable)
	}
	_, err = env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	refusedRollout(t, err, "no promotion to roll back")

	log, err := env.reconciler.ListAgentPromotions(ctx, evalTestNamespace, "reviewer")
	if err != nil || len(log) != 2 || log[0].ID != record.ID || log[1].ID != rollback.ID {
		t.Fatalf("promotion log = %+v, %v", log, err)
	}
}

// A rollback never overwrites an edit made after the promotion.
func TestRollbackRefusesAnEditedAgent(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)
	if _, err := env.reconciler.PromoteAgent(ctx, evalTestNamespace, "reviewer", "d1", promotionTestActor); err != nil {
		t.Fatal(err)
	}
	stable := env.getAgent("reviewer")
	stable.Spec.SystemPrompt.Inline = "Edited by hand."
	if err := env.client.Update(ctx, stable); err != nil {
		t.Fatal(err)
	}
	_, err := env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	refusedRollout(t, err, "changed after promotion")
	if env.getAgent("reviewer").Spec.SystemPrompt.Inline != "Edited by hand." {
		t.Fatal("the rollback overwrote the edit")
	}
}

func TestPromoteRefusals(t *testing.T) {
	ctx := context.Background()
	promote := func(env *experimentEnv, agent, decision string) error {
		_, err := env.reconciler.PromoteAgent(ctx, evalTestNamespace, agent, decision, promotionTestActor)
		return err
	}
	t.Run("unknown decision", func(t *testing.T) {
		env := newPromotionEnv(t, nil)
		refusedRollout(t, promote(env, "reviewer", "nope"), "no decision")
	})
	t.Run("a later rejection", func(t *testing.T) {
		env := newPromotionEnv(t, nil)
		env.decide("d1", store.EvalDecisionApproved)
		env.decide("d2", store.EvalDecisionRejected)
		refusedRollout(t, promote(env, "reviewer", "d1"), "not the approval in force")
	})
	t.Run("another Agent", func(t *testing.T) {
		env := newPromotionEnv(t, nil)
		env.decide("d1", store.EvalDecisionApproved)
		refusedRollout(t, promote(env, "candidate-agent", "d1"), "approves a change to Agent \"reviewer\"")
	})
	t.Run("stale approval", func(t *testing.T) {
		env := newPromotionEnv(t, nil)
		env.decide("d1", store.EvalDecisionApproved)
		candidate := env.getAgent("candidate-agent")
		candidate.Spec.SystemPrompt.Inline = "Something no one tested."
		candidate.Generation++
		if err := env.client.Update(ctx, candidate); err != nil {
			t.Fatal(err)
		}
		refusedRollout(t, promote(env, "reviewer", "d1"), "is stale")
	})
	t.Run("candidate run gone", func(t *testing.T) {
		env := newPromotionEnv(t, nil)
		env.decide("d1", store.EvalDecisionApproved)
		run := env.getRun("candidate")
		run.Finalizers = nil
		if err := env.client.Update(ctx, run); err != nil {
			t.Fatal(err)
		}
		if err := env.client.Delete(ctx, run); err != nil {
			t.Fatal(err)
		}
		refusedRollout(t, promote(env, "reviewer", "d1"), "is gone")
	})
	t.Run("not a candidate of the Agent", func(t *testing.T) {
		env := newPromotionEnv(t, func(a *corev1alpha1.Agent) { a.OwnerReferences = nil })
		env.decide("d1", store.EvalDecisionApproved)
		refusedRollout(t, promote(env, "reviewer", "d1"), "is not a candidate")
	})
	t.Run("differs beyond promotable values", func(t *testing.T) {
		temperature := 0.1
		env := newPromotionEnv(t, func(a *corev1alpha1.Agent) { a.Spec.Model.Temperature = &temperature })
		env.decide("d1", store.EvalDecisionApproved)
		refusedRollout(t, promote(env, "reviewer", "d1"), "more than its system prompt and model")
	})
	t.Run("another action in progress", func(t *testing.T) {
		env := newPromotionEnv(t, nil)
		env.decide("d1", store.EvalDecisionApproved)
		open := promotionTestRecord(env, "open")
		if err := env.store.CreateAgentPromotion(ctx, open); err != nil {
			t.Fatal(err)
		}
		refusedRollout(t, promote(env, "reviewer", "d1"), "in progress")
	})
	t.Run("before recovery", func(t *testing.T) {
		env := newPromotionEnv(t, nil)
		env.reconciler.promotions = promotionGate{}
		short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		defer cancel()
		_, err := env.reconciler.PromoteAgent(short, evalTestNamespace, "reviewer", "d1", promotionTestActor)
		refusedRollout(t, err, "still settling")
	})
}

func promotionTestRecord(env *experimentEnv, id string) *store.AgentPromotionRecord {
	stable := env.getAgent("reviewer")
	return &store.AgentPromotionRecord{
		SchemaVersion: store.AgentPromotionSchemaVersion, ID: id, Namespace: evalTestNamespace, Agent: "reviewer",
		AgentUID: string(stable.UID), Action: store.AgentPromotionPromote, State: store.AgentPromotionPending,
		DecisionID: "d1", CandidateRun: "candidate", CandidateRunUID: "candidate-uid",
		CandidateAgent: &store.EvalAgentIdentity{Name: "candidate-agent", UID: "candidate-agent-uid", Generation: 1},
		Previous:       store.AgentPromotionValues{Model: "claude-haiku"},
		Target:         store.AgentPromotionValues{SystemPrompt: promotionTestPrompt, Model: "claude-sonnet"},
		AgentResourceVersion: stable.ResourceVersion, AgentGeneration: stable.Generation,
		Actor: promotionTestActor, CreatedAt: env.now, UpdatedAt: env.now,
	}
}

// Recovery completes an open record whose update reached the Agent and
// fails one whose update did not, without touching the Agent.
func TestRecoverAgentPromotions(t *testing.T) {
	ctx := context.Background()
	env := newPromotionEnv(t, nil)
	committed := promotionTestRecord(env, "committed")
	if err := env.store.CreateAgentPromotion(ctx, committed); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{Namespace: evalTestNamespace, ID: "committed",
		From: store.AgentPromotionPending, To: store.AgentPromotionApplying, UpdatedAt: env.now}); err != nil {
		t.Fatal(err)
	}
	stable := env.getAgent("reviewer")
	stable.Annotations = map[string]string{labels.AnnotationPromotionRecord: "committed"}
	if err := env.client.Update(ctx, stable); err != nil {
		t.Fatal(err)
	}
	env.reconciler.promotions = promotionGate{}
	if err := env.reconciler.RecoverAgentPromotions(ctx); err != nil {
		t.Fatal(err)
	}
	log, err := env.reconciler.ListAgentPromotions(ctx, evalTestNamespace, "reviewer")
	if err != nil || len(log) != 1 || log[0].State != store.AgentPromotionComplete || log[0].AppliedGeneration != stable.Generation+1 {
		t.Fatalf("log = %+v, %v", log, err)
	}

	lost := promotionTestRecord(env, "lost")
	if err := env.store.CreateAgentPromotion(ctx, lost); err != nil {
		t.Fatal(err)
	}
	before := env.getAgent("reviewer")
	env.reconciler.promotions = promotionGate{}
	if err := env.reconciler.RecoverAgentPromotions(ctx); err != nil {
		t.Fatal(err)
	}
	log, err = env.reconciler.ListAgentPromotions(ctx, evalTestNamespace, "reviewer")
	if err != nil || len(log) != 2 || log[1].State != store.AgentPromotionFailed || !strings.Contains(log[1].Reason, "restarted") {
		t.Fatalf("log = %+v, %v", log, err)
	}
	if after := env.getAgent("reviewer"); after.ResourceVersion != before.ResourceVersion {
		t.Fatal("recovery changed the Agent")
	}
}
