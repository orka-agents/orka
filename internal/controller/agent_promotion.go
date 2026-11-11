/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

// promotionGate holds back promote and rollback until open records left by
// an earlier controller process are settled, so recovery never races an
// action in flight.
type promotionGate struct {
	once  sync.Once
	ready chan struct{}
	// locks serialize rollout actions per Agent in this process. The API
	// server runs only on the leader, so no other process acts at the same
	// time, and an open record no lock holder owns is one an earlier
	// action left behind.
	locks sync.Map
}

func (g *promotionGate) lock(namespace, agent string) func() {
	value, _ := g.locks.LoadOrStore(namespace+"/"+agent, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (g *promotionGate) channel() chan struct{} {
	g.once.Do(func() { g.ready = make(chan struct{}) })
	return g.ready
}

func refuseRollout(format string, args ...any) error {
	return &evals.RolloutRefusal{Reason: fmt.Sprintf(format, args...)}
}

// RecoverAgentPromotions settles every open promotion record once, at
// startup, before any promote or rollback runs. A record whose ID the Agent
// carries in its promotion-record annotation committed its update, so it
// is completed; any other open record never reached the Agent and is
// failed, leaving the Agent as it is. No record stays open, so a restart
// cannot wedge rollouts on an Agent.
func (r *EvalRunReconciler) RecoverAgentPromotions(ctx context.Context) error {
	if r.ExperimentStore == nil {
		close(r.promotions.channel())
		return nil
	}
	open, err := r.ExperimentStore.ListOpenAgentPromotions(ctx)
	if err != nil {
		return fmt.Errorf("list open Agent promotions: %w", err)
	}
	for i := range open {
		if err := r.recoverAgentPromotion(ctx, &open[i]); err != nil {
			return err
		}
	}
	close(r.promotions.channel())
	return nil
}

// recoverAgentPromotionsUntilDone retries recovery until it succeeds or the
// manager stops; promote and rollback stay refused until then.
func (r *EvalRunReconciler) recoverAgentPromotionsUntilDone(ctx context.Context) error {
	for {
		err := r.RecoverAgentPromotions(ctx)
		if err == nil {
			return nil
		}
		logf.FromContext(ctx).Error(err, "Failed to settle open Agent promotion records; retrying")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Second):
		}
	}
}

func (r *EvalRunReconciler) recoverAgentPromotion(ctx context.Context, record *store.AgentPromotionRecord) error {
	agent := &corev1alpha1.Agent{}
	err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.Agent}, agent)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	committed := err == nil && agent.UID == types.UID(record.AgentUID) && agent.Annotations[labels.AnnotationPromotionRecord] == record.ID
	if !committed && err == nil && agent.UID == types.UID(record.AgentUID) && agent.Generation == record.AgentGeneration+1 {
		// The marker is an annotation an Agent editor could change. An Agent
		// one generation on that holds exactly the target values took this
		// update too.
		current, valuesErr := r.agentPromotionValues(ctx, agent)
		committed = valuesErr == nil && reflect.DeepEqual(current, record.Target)
	}
	log := logf.FromContext(ctx).WithValues("agent", record.Agent, "namespace", record.Namespace, "record", record.ID)
	if !committed {
		log.Info("Failing an Agent promotion record the controller restarted before applying")
		_, err := r.ExperimentStore.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{
			Namespace: record.Namespace, ID: record.ID, From: record.State, To: store.AgentPromotionFailed,
			Reason: "the controller restarted before the update reached the Agent; ask again", UpdatedAt: r.now(),
		})
		return err
	}
	if record.State == store.AgentPromotionPending {
		if _, err := r.ExperimentStore.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{
			Namespace: record.Namespace, ID: record.ID, From: store.AgentPromotionPending, To: store.AgentPromotionApplying,
			UpdatedAt: r.now(),
		}); err != nil {
			return err
		}
	}
	// The update committed; record what it wrote rather than reading values
	// a later edit may have changed.
	digest, err := agentPromotionDigest(record.Target)
	if err != nil {
		return err
	}
	log.Info("Completing an Agent promotion record whose update committed before a restart")
	reason := ""
	if agent.Annotations[labels.AnnotationPromotionRecord] != record.ID {
		// Said in the log so an audit can tell this from a marked commit.
		reason = "completed from the Agent's values: one generation on, it holds exactly the target; its promotion marker was missing"
	}
	_, err = r.ExperimentStore.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{
		Namespace: record.Namespace, ID: record.ID, From: store.AgentPromotionApplying, To: store.AgentPromotionComplete,
		AppliedGeneration: record.AgentGeneration + 1, AppliedDigest: digest, Reason: reason, UpdatedAt: r.now(),
	})
	return err
}

func (r *EvalRunReconciler) awaitPromotionRecovery(_ context.Context) error {
	select {
	case <-r.promotions.channel():
		return nil
	default:
		return refuseRollout("the controller is still settling promotions from before it restarted; ask again")
	}
}

// beginAgentRolloutAction takes the Agent's rollout lock and settles any
// record an earlier action left open, so every check that follows sees the
// Agent's settled history.
func (r *EvalRunReconciler) beginAgentRolloutAction(ctx context.Context, namespace, agentName string) (func(), error) {
	unlock := r.promotions.lock(namespace, agentName)
	agent := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, agent); err != nil {
		unlock()
		if apierrors.IsNotFound(err) {
			return nil, refuseRollout("Agent %q does not exist in %s", agentName, namespace)
		}
		return nil, err
	}
	if err := r.settleOpenAgentPromotions(ctx, namespace, string(agent.UID)); err != nil {
		unlock()
		return nil, fmt.Errorf("settle earlier rollout actions on Agent %q: %w", agentName, err)
	}
	return unlock, nil
}

// settleOpenAgentPromotions settles the Agent's open records under its
// lock: an action that failed ambiguously leaves its record applying, and
// this settles it on the next action rather than at the next restart.
func (r *EvalRunReconciler) settleOpenAgentPromotions(ctx context.Context, namespace, agentUID string) error {
	records, err := r.ExperimentStore.ListAgentPromotions(ctx, namespace, agentUID)
	if err != nil {
		return err
	}
	for i := range records {
		if records[i].State == store.AgentPromotionPending || records[i].State == store.AgentPromotionApplying {
			if err := r.recoverAgentPromotion(ctx, &records[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// agentPromotionDigest digests an Agent's promotable values.
func agentPromotionDigest(values store.AgentPromotionValues) (string, error) {
	return evals.DigestJSON(values)
}

// agentPromotionValues resolves an Agent's promotable values.
func (r *EvalRunReconciler) agentPromotionValues(ctx context.Context, agent *corev1alpha1.Agent) (store.AgentPromotionValues, error) {
	values, _, err := r.resolvedPromotionValues(ctx, agent)
	return values, err
}

// resolvedPromotionValues resolves an Agent's promotable values once, with
// the digest of its prompt composed with its soul, as an EvalRun snapshot
// records it.
func (r *EvalRunReconciler) resolvedPromotionValues(ctx context.Context, agent *corev1alpha1.Agent) (store.AgentPromotionValues, string, error) {
	reader := r.uncachedReader()
	prompt, err := resolveACPSystemPrompt(ctx, reader, agent)
	if err != nil {
		return store.AgentPromotionValues{}, "", fmt.Errorf("resolve Agent %q's system prompt: %w", agent.Name, err)
	}
	soul, err := agentcontext.ResolveSoul(ctx, reader, agent)
	if err != nil {
		return store.AgentPromotionValues{}, "", fmt.Errorf("resolve Agent %q's soul: %w", agent.Name, err)
	}
	sum := sha256.Sum256([]byte(agentcontext.Compose(prompt, soul)))
	values := store.AgentPromotionValues{SystemPrompt: prompt, Model: evalAgentModelName(agent)}
	if source := agent.Spec.SystemPrompt; source != nil && source.ConfigMapRef != nil {
		values.SystemPromptConfigMap = &store.AgentPromotionConfigMapRef{Name: source.ConfigMapRef.Name, Key: source.ConfigMapRef.Key}
	}
	return values, evals.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// sameBehavior reports whether two sets of values give the Agent the same
// prompt text and model, wherever the text comes from.
func sameBehavior(a, b store.AgentPromotionValues) bool {
	return a.SystemPrompt == b.SystemPrompt && a.Model == b.Model
}

// withPromotableValues returns spec with its system prompt and model name
// replaced. A model left with no field set is dropped, so a spec that had no
// model compares equal after its model is cleared again.
func withPromotableValues(spec corev1alpha1.AgentSpec, prompt *corev1alpha1.PromptSource, model string) corev1alpha1.AgentSpec {
	out := *spec.DeepCopy()
	out.SystemPrompt = prompt.DeepCopy()
	if out.Model == nil {
		out.Model = &corev1alpha1.ModelConfig{}
	}
	out.Model.Name = model
	if equality.Semantic.DeepEqual(*out.Model, corev1alpha1.ModelConfig{}) {
		out.Model = nil
	}
	return out
}

// applyPromotableValues writes values onto an Agent spec. A prompt that came
// from a ConfigMap keeps its reference, so its text is not copied where
// every Agent reader could see it; any other prompt is written inline.
func applyPromotableValues(spec corev1alpha1.AgentSpec, values store.AgentPromotionValues) corev1alpha1.AgentSpec {
	var prompt *corev1alpha1.PromptSource
	switch {
	case values.SystemPromptConfigMap != nil:
		prompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{
			Name: values.SystemPromptConfigMap.Name, Key: values.SystemPromptConfigMap.Key,
		}}
	case values.SystemPrompt != "":
		prompt = &corev1alpha1.PromptSource{Inline: values.SystemPrompt}
	}
	return withPromotableValues(spec, prompt, values.Model)
}

// PromoteAgent writes an approved candidate's system prompt and model onto
// the stable Agent it was tested against. It refuses unless the decision is
// the latest for its experiment and approves, the experiment's bound tuple
// still matches, the candidate belongs to this Agent and differs from it
// only in promotable values, and those values change something.
func (r *EvalRunReconciler) PromoteAgent(ctx context.Context, namespace, agentName, decisionID string,
	actor store.EvalDecisionActor) (*store.AgentPromotionRecord, error) {
	if err := r.awaitPromotionRecovery(ctx); err != nil {
		return nil, err
	}
	if r.ExperimentStore == nil {
		return nil, refuseRollout("promotions need the eval experiment store")
	}
	unlock, err := r.beginAgentRolloutAction(ctx, namespace, agentName)
	if err != nil {
		return nil, err
	}
	defer unlock()
	decision, err := r.ExperimentStore.GetEvalDecision(ctx, namespace, decisionID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, refuseRollout("no decision %q in %s", decisionID, namespace)
	}
	if err != nil {
		return nil, err
	}
	run, outcome, err := r.promotionExperimentRun(ctx, decision)
	if err != nil {
		return nil, err
	}
	stable, candidate, err := r.promotionAgents(ctx, run, decision, agentName)
	if err != nil {
		return nil, err
	}
	previous, previousDigest, err := r.resolvedPromotionValues(ctx, stable)
	if err != nil {
		return nil, err
	}
	// The rollback target is what the baseline ran: a ConfigMap edit since
	// then changes the prompt without moving the Agent's generation.
	if previousDigest != outcome.Baseline.SystemPromptDigest {
		return nil, refuseRollout("Agent %q's system prompt is not the one its baseline ran; run the experiment again", stable.Name)
	}
	target, err := r.promotionTargetValues(ctx, candidate, outcome)
	if err != nil {
		return nil, err
	}
	if sameBehavior(previous, target) {
		return nil, refuseRollout("candidate Agent %q has the same system prompt and model as %q; there is nothing to promote",
			candidate.Name, stable.Name)
	}
	// reflect, not Semantic: an empty list and an omitted one differ, as a
	// tool list does to the runtime.
	if !reflect.DeepEqual(withPromotableValues(candidate.Spec, nil, ""), withPromotableValues(stable.Spec, nil, "")) {
		return nil, refuseRollout("candidate Agent %q differs from %q in more than its system prompt and model; only those are promoted",
			candidate.Name, stable.Name)
	}
	at := r.now()
	record := &store.AgentPromotionRecord{
		SchemaVersion: store.AgentPromotionSchemaVersion, ID: uuid.NewString(), Namespace: namespace,
		Agent: stable.Name, AgentUID: string(stable.UID), Action: store.AgentPromotionPromote, State: store.AgentPromotionPending,
		DecisionID: decision.ID, CandidateRun: decision.CandidateRun, CandidateRunUID: decision.CandidateRunUID,
		CandidateAgent: &store.EvalAgentIdentity{Name: candidate.Name, UID: string(candidate.UID), Generation: candidate.Generation},
		Previous:       previous, Target: target,
		AgentResourceVersion: stable.ResourceVersion, AgentGeneration: stable.Generation,
		Actor: actor, CreatedAt: at, UpdatedAt: at,
	}
	return r.applyAgentPromotion(ctx, stable, record, true)
}

// promotionExperimentRun reads the candidate EvalRun a decision names and
// checks the decision is still the approval in force for it, with a bound
// tuple that still matches.
func (r *EvalRunReconciler) promotionExperimentRun(ctx context.Context,
	decision *store.EvalDecisionRecord) (*corev1alpha1.EvalRun, *store.EvalExperimentOutcome, error) {
	page, err := r.ExperimentStore.ListEvalExperiments(ctx, decision.Namespace,
		store.EvalExperimentListOptions{CandidateRunUID: decision.CandidateRunUID, Limit: 1})
	if err != nil {
		return nil, nil, err
	}
	if len(page.Items) != 1 || len(page.Items[0].Decisions) == 0 || page.Items[0].Outcome == nil {
		return nil, nil, refuseRollout("decision %q has no experiment", decision.ID)
	}
	decisions := page.Items[0].Decisions
	if latest := decisions[len(decisions)-1]; latest.ID != decision.ID || latest.Decision != store.EvalDecisionApproved {
		return nil, nil, refuseRollout("decision %q is not the approval in force for evalrun/%s; the latest decision is %s %q",
			decision.ID, decision.CandidateRun, latest.Decision, latest.ID)
	}
	run := &corev1alpha1.EvalRun{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: decision.Namespace, Name: decision.CandidateRun}, run); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, refuseRollout("evalrun/%s is gone, so its approval cannot be checked; run the experiment again", decision.CandidateRun)
		}
		return nil, nil, err
	}
	if string(run.UID) != decision.CandidateRunUID {
		return nil, nil, refuseRollout("evalrun/%s was replaced after decision %q; run the experiment again", decision.CandidateRun, decision.ID)
	}
	current, err := r.CurrentEvalDecisionBinding(ctx, run)
	if err != nil {
		return nil, nil, refuseRollout("the approval of evalrun/%s cannot be checked: %v", run.Name, err)
	}
	if decision.Binding == nil || *current != *decision.Binding {
		return nil, nil, refuseRollout("the approval of evalrun/%s is stale: the Agents, cases, graders or gate changed since it was given; "+
			"see orka eval decisions %s", run.Name, run.Name)
	}
	return run, page.Items[0].Outcome, nil
}

// promotionAgents reads the stable and candidate Agents the bound tuple
// names and checks the stable Agent is the one being promoted and the
// candidate belongs to it.
func (r *EvalRunReconciler) promotionAgents(ctx context.Context, run *corev1alpha1.EvalRun, decision *store.EvalDecisionRecord,
	agentName string) (*corev1alpha1.Agent, *corev1alpha1.Agent, error) {
	binding := decision.Binding
	if binding.BaselineAgent.Name != agentName {
		return nil, nil, refuseRollout("decision %q approves a change to Agent %q, not %q", decision.ID, binding.BaselineAgent.Name, agentName)
	}
	stable := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: agentName}, stable); err != nil {
		return nil, nil, err
	}
	if string(stable.UID) != binding.BaselineAgent.UID || stable.Generation != binding.BaselineAgent.Generation {
		return nil, nil, refuseRollout("Agent %q changed after its baseline run; run the experiment again", agentName)
	}
	candidate := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: binding.CandidateAgent.Name}, candidate); err != nil {
		return nil, nil, err
	}
	// The approval was checked against an earlier read; this read is what
	// gets promoted, so it must be the candidate the experiment ran.
	if string(candidate.UID) != binding.CandidateAgent.UID || candidate.Generation != binding.CandidateAgent.Generation {
		return nil, nil, refuseRollout("candidate Agent %q changed after its experiment; run the experiment again", candidate.Name)
	}
	if candidate.Labels[labels.LabelCandidateOf] != labels.SelectorValue(stable.Name) || !ownedByAgent(candidate, stable) {
		return nil, nil, refuseRollout("Agent %q is not a candidate of %q: it needs the %s label and an owner reference to it",
			candidate.Name, stable.Name, labels.LabelCandidateOf)
	}
	return stable, candidate, nil
}

// promotionTargetValues resolves the candidate's promotable values once and
// checks that the prompt is the one its experiment arm ran with: a
// ConfigMap-backed prompt can change without the Agent's generation moving.
func (r *EvalRunReconciler) promotionTargetValues(ctx context.Context, candidate *corev1alpha1.Agent,
	outcome *store.EvalExperimentOutcome) (store.AgentPromotionValues, error) {
	values, digest, err := r.resolvedPromotionValues(ctx, candidate)
	if err != nil {
		return store.AgentPromotionValues{}, err
	}
	if digest != outcome.Candidate.SystemPromptDigest {
		return store.AgentPromotionValues{}, refuseRollout(
			"candidate Agent %q's system prompt is not the one its experiment ran; run the experiment again", candidate.Name)
	}
	return values, nil
}

// RollbackAgent writes back the values the Agent's latest promotion
// replaced. It is a compare-and-swap: the live Agent must still hold exactly
// what that promotion wrote, so an edit made since is never overwritten.
func (r *EvalRunReconciler) RollbackAgent(ctx context.Context, namespace, agentName string,
	actor store.EvalDecisionActor, reason string) (*store.AgentPromotionRecord, error) {
	if err := r.awaitPromotionRecovery(ctx); err != nil {
		return nil, err
	}
	if r.ExperimentStore == nil {
		return nil, refuseRollout("rollbacks need the eval experiment store")
	}
	unlock, err := r.beginAgentRolloutAction(ctx, namespace, agentName)
	if err != nil {
		return nil, err
	}
	defer unlock()
	stable := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, stable); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refuseRollout("Agent %q does not exist in %s", agentName, namespace)
		}
		return nil, err
	}
	records, err := r.ExperimentStore.ListAgentPromotions(ctx, namespace, string(stable.UID))
	if err != nil {
		return nil, err
	}
	var last *store.AgentPromotionRecord
	for i := range records {
		if records[i].State == store.AgentPromotionComplete {
			last = &records[i]
		}
	}
	if last == nil || last.Action != store.AgentPromotionPromote {
		return nil, refuseRollout("Agent %q has no promotion to roll back", agentName)
	}
	current, err := r.agentPromotionValues(ctx, stable)
	if err != nil {
		return nil, err
	}
	digest, err := agentPromotionDigest(current)
	if err != nil {
		return nil, err
	}
	if stable.Generation != last.AppliedGeneration || digest != last.AppliedDigest {
		return nil, refuseRollout("Agent %q changed after promotion %s; roll it back by hand rather than overwrite that edit",
			agentName, last.ID)
	}
	at := r.now()
	record := &store.AgentPromotionRecord{
		SchemaVersion: store.AgentPromotionSchemaVersion, ID: uuid.NewString(), Namespace: namespace,
		Agent: stable.Name, AgentUID: string(stable.UID), Action: store.AgentPromotionRollback, State: store.AgentPromotionPending,
		PromotionID: last.ID, Previous: last.Target, Target: last.Previous,
		AgentResourceVersion: stable.ResourceVersion, AgentGeneration: stable.Generation,
		Actor: actor, Reason: strings.TrimSpace(reason), CreatedAt: at, UpdatedAt: at,
	}
	return r.applyAgentPromotion(ctx, stable, record, false)
}

// applyAgentPromotion runs a record through its durable steps: insert it
// pending, move it to applying (for a promotion, in the same transaction
// that checks its decision is still the approval in force), update the
// Agent with a resourceVersion precondition and the record's marker, then
// complete it with what the update produced.
func (r *EvalRunReconciler) applyAgentPromotion(ctx context.Context, stable *corev1alpha1.Agent, record *store.AgentPromotionRecord,
	requireApproval bool) (*store.AgentPromotionRecord, error) {
	if err := r.ExperimentStore.CreateAgentPromotion(ctx, record); err != nil {
		if errors.Is(err, store.ErrValidation) {
			return nil, refuseRollout("the rollout record was refused: %v", err)
		}
		if errors.Is(err, store.ErrConflict) {
			return nil, refuseRollout("another rollout action on Agent %q is in progress", stable.Name)
		}
		return nil, err
	}
	fail := func(reason string) {
		if _, err := r.ExperimentStore.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{
			Namespace: record.Namespace, ID: record.ID, From: record.State, To: store.AgentPromotionFailed, Reason: reason, UpdatedAt: r.now(),
		}); err != nil {
			logf.FromContext(ctx).Error(err, "Failed to close an Agent promotion record", "record", record.ID)
		}
	}
	applying, err := r.ExperimentStore.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{
		Namespace: record.Namespace, ID: record.ID, From: store.AgentPromotionPending, To: store.AgentPromotionApplying,
		RequireLatestApproval: requireApproval, UpdatedAt: r.now(),
	})
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			fail(err.Error())
			return nil, refuseRollout("decision %q stopped being the approval in force", record.DecisionID)
		}
		return nil, err
	}
	record = applying

	if ref := record.Target.SystemPromptConfigMap; ref != nil {
		if err := r.checkPromptConfigMap(ctx, record.Namespace, ref, record.Target.SystemPrompt); err != nil {
			fail(err.Error())
			return nil, err
		}
	}
	updated := stable.DeepCopy()
	updated.Spec = applyPromotableValues(stable.Spec, record.Target)
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[labels.AnnotationPromotionRecord] = record.ID
	if err := r.Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			fail("the Agent update was refused: " + err.Error())
			return nil, refuseRollout("Agent %q changed while it was being updated; ask again", stable.Name)
		}
		// The update may have committed. Leave the record applying: the
		// next controller start settles it from the marker.
		return nil, fmt.Errorf("update Agent %q: %w", stable.Name, err)
	}
	digest, err := agentPromotionDigest(record.Target)
	if err != nil {
		return nil, err
	}
	complete, err := r.ExperimentStore.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{
		Namespace: record.Namespace, ID: record.ID, From: store.AgentPromotionApplying, To: store.AgentPromotionComplete,
		AppliedGeneration: updated.Generation, AppliedDigest: digest, UpdatedAt: r.now(),
	})
	if err != nil {
		return nil, err
	}
	logf.FromContext(ctx).Info("Applied an Agent rollout action", "agent", stable.Name, "action", record.Action,
		"record", record.ID, "generation", updated.Generation)
	return complete, nil
}

// ListAgentPromotions returns an Agent's promotion log, oldest first.
func (r *EvalRunReconciler) ListAgentPromotions(ctx context.Context, namespace, agentName string) ([]store.AgentPromotionRecord, error) {
	if r.ExperimentStore == nil {
		return nil, refuseRollout("promotions need the eval experiment store")
	}
	agent := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refuseRollout("Agent %q does not exist in %s", agentName, namespace)
		}
		return nil, err
	}
	return r.ExperimentStore.ListAgentPromotions(ctx, namespace, string(agent.UID))
}

// checkPromptConfigMap refuses writing a ConfigMap reference whose
// ConfigMap no longer holds the prompt the action checked.
func (r *EvalRunReconciler) checkPromptConfigMap(ctx context.Context, namespace string, ref *store.AgentPromotionConfigMapRef,
	want string) error {
	configMap := &corev1.ConfigMap{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return refuseRollout("ConfigMap %q holding the system prompt is gone; restore it or roll back by hand", ref.Name)
		}
		return err
	}
	if value, ok := configMap.Data[ref.Key]; !ok || value != want {
		return refuseRollout("ConfigMap %q key %q no longer holds the system prompt this action checked; roll back by hand", ref.Name, ref.Key)
	}
	return nil
}
