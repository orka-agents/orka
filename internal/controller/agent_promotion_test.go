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

	corev1 "k8s.io/api/core/v1"
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
		CandidateAgent:       &store.EvalAgentIdentity{Name: "candidate-agent", UID: "candidate-agent-uid", Generation: 1},
		Previous:             store.AgentPromotionValues{Model: "claude-haiku"},
		Target:               store.AgentPromotionValues{SystemPrompt: promotionTestPrompt, Model: "claude-sonnet"},
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
	if err != nil || len(log) != 2 || log[1].State != store.AgentPromotionFailed || !strings.Contains(log[1].StateReason, "restarted") {
		t.Fatalf("log = %+v, %v", log, err)
	}
	if after := env.getAgent("reviewer"); after.ResourceVersion != before.ResourceVersion {
		t.Fatal("recovery changed the Agent")
	}
}

// What gets promoted is read after the approval was checked, so it is
// pinned to the experiment's candidate: its identity and the prompt digest
// its arm ran with.
func TestPromotionPinsTheCandidateToItsExperiment(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)
	entry := env.experimentEntry()
	decision := entry.Decisions[0]
	candidate := env.getAgent("candidate-agent")

	values, err := env.reconciler.promotionTargetValues(ctx, candidate, entry.Outcome)
	if err != nil || values.SystemPrompt != promotionTestPrompt {
		t.Fatalf("promotionTargetValues() = %+v, %v", values, err)
	}
	edited := *entry.Outcome
	edited.Candidate.SystemPromptDigest = "sha256:other"
	_, err = env.reconciler.promotionTargetValues(ctx, candidate, &edited)
	refusedRollout(t, err, "not the one its experiment ran")

	binding := *decision.Binding
	binding.CandidateAgent.Generation++
	decision.Binding = &binding
	_, _, err = env.reconciler.promotionAgents(ctx, env.getRun("candidate"), &decision, "reviewer")
	refusedRollout(t, err, "changed after its experiment")
}

// A canary starts and widens on an approval, routing checks it, a
// promotion of the same approval ends it, and an abort ends one early.
func TestCanaryAnAgent(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)

	_, err := env.reconciler.CanaryAgent(ctx, evalTestNamespace, "reviewer", "d1", 0, promotionTestActor)
	refusedRollout(t, err, "percent must be 1 to 100")
	record, err := env.reconciler.CanaryAgent(ctx, evalTestNamespace, "reviewer", "d1", 10, promotionTestActor)
	if err != nil || record.State != store.AgentPromotionComplete || record.Percent != 10 {
		t.Fatalf("CanaryAgent() = %+v, %v", record, err)
	}
	rollout := env.getAgent("reviewer").Status.Rollout
	if rollout == nil || rollout.Candidate == nil || rollout.Candidate.Name != "candidate-agent" || rollout.Percent != 10 ||
		rollout.DecisionID != "d1" || rollout.RecordID != record.ID || env.getAgent("reviewer").Spec.SystemPrompt != nil {
		t.Fatalf("rollout = %+v", rollout)
	}
	widened, err := env.reconciler.CanaryAgent(ctx, evalTestNamespace, "reviewer", "d1", 50, promotionTestActor)
	if err != nil || env.getAgent("reviewer").Status.Rollout.Percent != 50 {
		t.Fatalf("widen = %+v, %v", widened, err)
	}
	candidate, err := env.reconciler.VerifyAgentRollout(ctx, env.getAgent("reviewer"))
	if err != nil || candidate.Name != "candidate-agent" {
		t.Fatalf("VerifyAgentRollout() = %v, %v", candidate, err)
	}
	_, err = env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	refusedRollout(t, err, "abort it before rolling back")

	// Promoting another approval while this canary runs is refused.
	stable := env.getAgent("reviewer")
	stable.Status.Rollout.DecisionID = "d-other"
	if err := env.client.Status().Update(ctx, stable); err != nil {
		t.Fatal(err)
	}
	_, err = env.reconciler.PromoteAgent(ctx, evalTestNamespace, "reviewer", "d1", promotionTestActor)
	refusedRollout(t, err, "has a canary of decision \"d-other\"")
	stable = env.getAgent("reviewer")
	stable.Status.Rollout.DecisionID = "d1"
	if err := env.client.Status().Update(ctx, stable); err != nil {
		t.Fatal(err)
	}

	promoted, err := env.reconciler.PromoteAgent(ctx, evalTestNamespace, "reviewer", "d1", promotionTestActor)
	if err != nil {
		t.Fatalf("PromoteAgent() error = %v", err)
	}
	if after := env.getAgent("reviewer").Status.Rollout; after == nil || after.Candidate != nil || after.RecordID != widened.ID {
		t.Fatalf("rollout after promotion = %+v (promotion %s)", after, promoted.ID)
	}
	_, err = env.reconciler.AbortAgentRollout(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	refusedRollout(t, err, "no canary to abort")
}

func TestAbortACanary(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)
	canary, err := env.reconciler.CanaryAgent(ctx, evalTestNamespace, "reviewer", "d1", 20, promotionTestActor)
	if err != nil {
		t.Fatal(err)
	}
	abort, err := env.reconciler.AbortAgentRollout(ctx, evalTestNamespace, "reviewer", promotionTestActor, "noisy reviews")
	if err != nil || abort.PromotionID != canary.ID || abort.Reason != "noisy reviews" {
		t.Fatalf("AbortAgentRollout() = %+v, %v", abort, err)
	}
	if rollout := env.getAgent("reviewer").Status.Rollout; rollout == nil || rollout.Candidate != nil || rollout.RecordID != abort.ID {
		t.Fatalf("rollout after abort = %+v", rollout)
	}
	_, err = env.reconciler.VerifyAgentRollout(ctx, env.getAgent("reviewer"))
	refusedRollout(t, err, "has no canary")
}

// A later rejection stops a running canary at the next routing check.
func TestVerifyAgentRolloutAfterARejection(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)
	if _, err := env.reconciler.CanaryAgent(ctx, evalTestNamespace, "reviewer", "d1", 30, promotionTestActor); err != nil {
		t.Fatal(err)
	}
	env.decide("d2", store.EvalDecisionRejected)
	_, err := env.reconciler.VerifyAgentRollout(ctx, env.getAgent("reviewer"))
	refusedRollout(t, err, "not the approval in force")
}

// Recovery settles an open canary from status.rollout, not the annotation.
func TestRecoverACanary(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	record := promotionTestRecord(env, "canary-1")
	record.Action, record.Percent = store.AgentPromotionCanary, 10
	if err := env.store.CreateAgentPromotion(ctx, record); err != nil {
		t.Fatal(err)
	}
	stable := env.getAgent("reviewer")
	stable.Status.Rollout = &corev1alpha1.AgentRolloutStatus{RecordID: "canary-1",
		Candidate: &corev1alpha1.EvalObjectIdentity{Name: "candidate-agent"}, Percent: 10}
	if err := env.client.Status().Update(ctx, stable); err != nil {
		t.Fatal(err)
	}
	env.reconciler.promotions = promotionGate{}
	if err := env.reconciler.RecoverAgentPromotions(ctx); err != nil {
		t.Fatal(err)
	}
	log, err := env.reconciler.ListAgentPromotions(ctx, evalTestNamespace, "reviewer")
	if err != nil || len(log) != 1 || log[0].State != store.AgentPromotionComplete || log[0].AppliedGeneration != stable.Generation {
		t.Fatalf("log = %+v, %v", log, err)
	}
}

// A reviewer whose prompt lives in a ConfigMap gets the reference back on
// rollback, never the ConfigMap's text, and a ConfigMap edited since the
// promotion refuses the rollback.
func TestRollbackKeepsAConfigMapPrompt(t *testing.T) {
	baseline, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	ctx := context.Background()
	if err := env.client.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-prompt", Namespace: evalTestNamespace},
		Data:       map[string]string{"prompt.md": "Private review instructions."},
	}); err != nil {
		t.Fatal(err)
	}
	stable := env.getAgent("reviewer")
	stable.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "reviewer-prompt", Key: "prompt.md"}}
	if err := env.client.Update(ctx, stable); err != nil {
		t.Fatal(err)
	}
	agent := env.getAgent("candidate-agent")
	agent.Labels = map[string]string{labels.LabelCandidateOf: "reviewer"}
	agent.OwnerReferences = []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Agent", Name: "reviewer", UID: "agent-uid"}}
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: promotionTestPrompt}
	if err := env.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", "needs_changes")
	env.reconcileRun("candidate")
	if err := env.reconciler.RecoverAgentPromotions(ctx); err != nil {
		t.Fatal(err)
	}
	env.decide("d1", store.EvalDecisionApproved)
	promotion, err := env.reconciler.PromoteAgent(ctx, evalTestNamespace, "reviewer", "d1", promotionTestActor)
	if err != nil {
		t.Fatalf("PromoteAgent() error = %v", err)
	}
	if ref := promotion.Previous.SystemPromptConfigMap; ref == nil || ref.Name != "reviewer-prompt" {
		t.Fatalf("previous = %+v", promotion.Previous)
	}
	if _, err := env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, ""); err != nil {
		t.Fatalf("RollbackAgent() error = %v", err)
	}
	prompt := env.getAgent("reviewer").Spec.SystemPrompt
	if prompt == nil || prompt.Inline != "" || prompt.ConfigMapRef == nil || prompt.ConfigMapRef.Name != "reviewer-prompt" {
		t.Fatalf("rolled back prompt = %+v", prompt)
	}
}

func TestRollbackRefusesAnEditedPromptConfigMap(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	if err := env.client.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-prompt", Namespace: evalTestNamespace},
		Data:       map[string]string{"prompt.md": "Edited since."},
	}); err != nil {
		t.Fatal(err)
	}
	err := env.reconciler.checkPromptConfigMap(ctx, evalTestNamespace,
		&store.AgentPromotionConfigMapRef{Name: "reviewer-prompt", Key: "prompt.md"}, "Private review instructions.")
	refusedRollout(t, err, "no longer holds")
	err = env.reconciler.checkPromptConfigMap(ctx, evalTestNamespace, &store.AgentPromotionConfigMapRef{Name: "gone", Key: "prompt.md"}, "x")
	refusedRollout(t, err, "is gone")
}

// An action left applying by an ambiguous failure is settled by the next
// action on the Agent, without a restart.
func TestPromoteSettlesAnEarlierOpenRecord(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)
	stuck := promotionTestRecord(env, "stuck")
	if err := env.store.CreateAgentPromotion(ctx, stuck); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{Namespace: evalTestNamespace, ID: "stuck",
		From: store.AgentPromotionPending, To: store.AgentPromotionApplying, UpdatedAt: env.now}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.reconciler.PromoteAgent(ctx, evalTestNamespace, "reviewer", "d1", promotionTestActor); err != nil {
		t.Fatalf("PromoteAgent() error = %v", err)
	}
	log, err := env.reconciler.ListAgentPromotions(ctx, evalTestNamespace, "reviewer")
	if err != nil || len(log) != 2 || log[0].State != store.AgentPromotionFailed || log[1].State != store.AgentPromotionComplete {
		t.Fatalf("log = %+v, %v", log, err)
	}
}

// An Agent one generation on with exactly the target values committed the
// update, even when an editor removed the marker.
func TestRecoveryUsesValuesWhenTheMarkerIsGone(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	stable := env.getAgent("reviewer")
	record := promotionTestRecord(env, "applied")
	record.AgentGeneration = stable.Generation - 1
	record.Target = store.AgentPromotionValues{Model: "claude-haiku"}
	if err := env.store.CreateAgentPromotion(ctx, record); err != nil {
		t.Fatal(err)
	}
	env.reconciler.promotions = promotionGate{}
	if err := env.reconciler.RecoverAgentPromotions(ctx); err != nil {
		t.Fatal(err)
	}
	log, err := env.reconciler.ListAgentPromotions(ctx, evalTestNamespace, "reviewer")
	if err != nil || len(log) != 1 || log[0].State != store.AgentPromotionComplete {
		t.Fatalf("log = %+v, %v", log, err)
	}
}

// A routing decision made from an Agent read before an abort never reaches
// the candidate: verification reads the live rollout.
func TestVerifyAgentRolloutReadsTheLiveRollout(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	env.decide("d1", store.EvalDecisionApproved)
	if _, err := env.reconciler.CanaryAgent(ctx, evalTestNamespace, "reviewer", "d1", 30, promotionTestActor); err != nil {
		t.Fatal(err)
	}
	beforeAbort := env.getAgent("reviewer")
	if _, err := env.reconciler.AbortAgentRollout(ctx, evalTestNamespace, "reviewer", promotionTestActor, ""); err != nil {
		t.Fatal(err)
	}
	candidate, err := env.reconciler.VerifyAgentRollout(ctx, beforeAbort)
	if err != nil || candidate != nil {
		t.Fatalf("VerifyAgentRollout() after an abort = %v, %v", candidate, err)
	}
}

// A promotion whose Agent update committed but whose completion write
// failed is settled by the next action, so it can be rolled back without a
// restart.
func TestRollbackSettlesACommittedPromotionFirst(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	stable := env.getAgent("reviewer")
	record := promotionTestRecord(env, "committed")
	record.Previous = store.AgentPromotionValues{Model: "claude-haiku"}
	record.Target = store.AgentPromotionValues{SystemPrompt: promotionTestPrompt, Model: "claude-sonnet"}
	if err := env.store.CreateAgentPromotion(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{Namespace: evalTestNamespace, ID: "committed",
		From: store.AgentPromotionPending, To: store.AgentPromotionApplying, UpdatedAt: env.now}); err != nil {
		t.Fatal(err)
	}
	// The update reached the Agent; the completion write did not.
	stable.Spec = applyPromotableValues(stable.Spec, record.Target)
	stable.Annotations = map[string]string{labels.AnnotationPromotionRecord: "committed"}
	stable.Generation = record.AgentGeneration + 1
	if err := env.client.Update(ctx, stable); err != nil {
		t.Fatal(err)
	}
	rollback, err := env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	if err != nil || rollback.PromotionID != "committed" {
		t.Fatalf("RollbackAgent() = %+v, %v", rollback, err)
	}
	if prompt := env.getAgent("reviewer").Spec.SystemPrompt; prompt != nil {
		t.Fatalf("rolled back prompt = %+v", prompt)
	}
}

// completedPromotionRecord stores a completed promote or rollback.
func completedPromotionRecord(t *testing.T, env *experimentEnv, record *store.AgentPromotionRecord, generation int64) {
	t.Helper()
	ctx := context.Background()
	if err := env.store.CreateAgentPromotion(ctx, record); err != nil {
		t.Fatal(err)
	}
	digest, err := agentPromotionDigest(record.Target)
	if err != nil {
		t.Fatal(err)
	}
	for _, move := range [][2]string{{store.AgentPromotionPending, store.AgentPromotionApplying}, {store.AgentPromotionApplying, store.AgentPromotionComplete}} {
		if _, err := env.store.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{Namespace: evalTestNamespace, ID: record.ID,
			From: move[0], To: move[1], AppliedGeneration: generation, AppliedDigest: digest, UpdatedAt: env.now}); err != nil {
			t.Fatal(err)
		}
	}
}

// Rollbacks unwind stacked promotions newest first: after A to B and B to
// C, one rollback restores B and the next restores A.
func TestRollbackUnwindsStackedPromotions(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	a := store.AgentPromotionValues{Model: "claude-haiku"}
	b := store.AgentPromotionValues{SystemPrompt: "B", Model: "claude-haiku"}
	c := store.AgentPromotionValues{SystemPrompt: "C", Model: "claude-haiku"}
	stable := env.getAgent("reviewer")
	first := promotionTestRecord(env, "p1")
	first.Previous, first.Target = a, b
	completedPromotionRecord(t, env, first, stable.Generation)
	second := promotionTestRecord(env, "p2")
	second.Previous, second.Target = b, c
	completedPromotionRecord(t, env, second, stable.Generation)
	stable.Spec = applyPromotableValues(stable.Spec, c)
	if err := env.client.Update(ctx, stable); err != nil {
		t.Fatal(err)
	}

	rollback, err := env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	if err != nil || rollback.PromotionID != "p2" || env.getAgent("reviewer").Spec.SystemPrompt.Inline != "B" {
		t.Fatalf("first rollback = %+v, %v", rollback, err)
	}
	rollback, err = env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	if err != nil || rollback.PromotionID != "p1" || env.getAgent("reviewer").Spec.SystemPrompt != nil {
		t.Fatalf("second rollback = %+v, %v", rollback, err)
	}
	_, err = env.reconciler.RollbackAgent(ctx, evalTestNamespace, "reviewer", promotionTestActor, "")
	refusedRollout(t, err, "no promotion to roll back")
}

// An applying record whose ambiguous update may still commit is fenced:
// the Agent moves on, so that update can only fail, and the record fails.
func TestRecoveryFencesAnOutstandingUpdate(t *testing.T) {
	env := newPromotionEnv(t, nil)
	ctx := context.Background()
	record := promotionTestRecord(env, "outstanding")
	if err := env.store.CreateAgentPromotion(ctx, record); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{Namespace: evalTestNamespace, ID: "outstanding",
		From: store.AgentPromotionPending, To: store.AgentPromotionApplying, UpdatedAt: env.now}); err != nil {
		t.Fatal(err)
	}
	env.reconciler.promotions = promotionGate{}
	if err := env.reconciler.RecoverAgentPromotions(ctx); err != nil {
		t.Fatal(err)
	}
	stable := env.getAgent("reviewer")
	if stable.Annotations[labels.AnnotationPromotionFence] != "outstanding" || stable.ResourceVersion == record.AgentResourceVersion {
		t.Fatalf("Agent was not fenced: %+v", stable.ObjectMeta)
	}
	log, err := env.reconciler.ListAgentPromotions(ctx, evalTestNamespace, "reviewer")
	if err != nil || len(log) != 1 || log[0].State != store.AgentPromotionFailed {
		t.Fatalf("log = %+v, %v", log, err)
	}
}
