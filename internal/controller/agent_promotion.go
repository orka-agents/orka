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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	committed := err == nil && agent.UID == types.UID(record.AgentUID) && agentCarriesRolloutRecord(agent, record)
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
	// a later edit may have changed. A canary or abort writes status only.
	transition := store.AgentPromotionTransition{
		Namespace: record.Namespace, ID: record.ID, From: store.AgentPromotionApplying, To: store.AgentPromotionComplete,
		AppliedGeneration: record.AgentGeneration, UpdatedAt: r.now(),
	}
	if writesAgentSpec(record.Action) {
		digest, err := agentPromotionDigest(record.Target)
		if err != nil {
			return err
		}
		transition.AppliedGeneration, transition.AppliedDigest = record.AgentGeneration+1, digest
	}
	log.Info("Completing an Agent promotion record whose update committed before a restart")
	_, err = r.ExperimentStore.TransitionAgentPromotion(ctx, transition)
	return err
}

// writesAgentSpec reports whether an action changes the Agent's spec;
// canary and abort change only its status.
func writesAgentSpec(action string) bool {
	return action == store.AgentPromotionPromote || action == store.AgentPromotionRollback
}

// agentCarriesRolloutRecord reports whether the Agent shows that record's
// write committed: promote and rollback mark the spec update with an
// annotation, canary and abort the status update with status.rollout.
func agentCarriesRolloutRecord(agent *corev1alpha1.Agent, record *store.AgentPromotionRecord) bool {
	if writesAgentSpec(record.Action) {
		return agent.Annotations[labels.AnnotationPromotionRecord] == record.ID
	}
	return agent.Status.Rollout != nil && agent.Status.Rollout.RecordID == record.ID
}

func (r *EvalRunReconciler) awaitPromotionRecovery(ctx context.Context) error {
	select {
	case <-r.promotions.channel():
		return nil
	case <-ctx.Done():
		return refuseRollout("the controller is still settling promotions from before it restarted; ask again")
	}
}

// agentPromotionDigest digests an Agent's promotable values.
func agentPromotionDigest(values store.AgentPromotionValues) (string, error) {
	return evals.DigestJSON(values)
}

// agentPromotionValues resolves an Agent's promotable values.
func (r *EvalRunReconciler) agentPromotionValues(ctx context.Context, agent *corev1alpha1.Agent) (store.AgentPromotionValues, error) {
	prompt, err := resolveACPSystemPrompt(ctx, r.uncachedReader(), agent)
	if err != nil {
		return store.AgentPromotionValues{}, fmt.Errorf("resolve Agent %q's system prompt: %w", agent.Name, err)
	}
	return store.AgentPromotionValues{SystemPrompt: prompt, Model: evalAgentModelName(agent)}, nil
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

// applyPromotableValues writes values onto an Agent spec: the prompt inline,
// so the Agent no longer depends on a ConfigMap no arm was tested against.
func applyPromotableValues(spec corev1alpha1.AgentSpec, values store.AgentPromotionValues) corev1alpha1.AgentSpec {
	var prompt *corev1alpha1.PromptSource
	if values.SystemPrompt != "" {
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
	approved, err := r.approvedCandidate(ctx, namespace, agentName, decisionID)
	if err != nil {
		return nil, err
	}
	if rollout := approved.stable.Status.Rollout; rollout != nil && rollout.Candidate != nil && rollout.DecisionID != decisionID {
		return nil, refuseRollout("Agent %q has a canary of decision %q; promote that decision or abort the canary first",
			agentName, rollout.DecisionID)
	}
	record := approved.record(store.AgentPromotionPromote, actor, r.now())
	complete, err := r.applyAgentRolloutAction(ctx, approved.stable, record, true, r.writePromotedSpec)
	if err != nil {
		return nil, err
	}
	// The promoted values are now the Agent's own, so a canary of them ends.
	// Routing re-verifies every canary, so a failure here only leaves a
	// rollout that no longer matches its approval and routes nothing.
	if err := r.clearAgentRollout(ctx, approved.stable, ""); err != nil {
		logf.FromContext(ctx).Error(err, "Failed to clear the canary after a promotion", "agent", agentName)
	}
	return complete, nil
}

// approvedRollout is an approval checked against the live cluster: the
// stable Agent it changes, the candidate it carries out, and both sets of
// promotable values.
type approvedRollout struct {
	decision          *store.EvalDecisionRecord
	stable, candidate *corev1alpha1.Agent
	previous, target  store.AgentPromotionValues
}

func (a *approvedRollout) record(action string, actor store.EvalDecisionActor, at time.Time) *store.AgentPromotionRecord {
	return &store.AgentPromotionRecord{
		SchemaVersion: store.AgentPromotionSchemaVersion, ID: uuid.NewString(), Namespace: a.stable.Namespace,
		Agent: a.stable.Name, AgentUID: string(a.stable.UID), Action: action, State: store.AgentPromotionPending,
		DecisionID: a.decision.ID, CandidateRun: a.decision.CandidateRun, CandidateRunUID: a.decision.CandidateRunUID,
		CandidateAgent: &store.EvalAgentIdentity{Name: a.candidate.Name, UID: string(a.candidate.UID), Generation: a.candidate.Generation},
		Previous:       a.previous, Target: a.target,
		AgentResourceVersion: a.stable.ResourceVersion, AgentGeneration: a.stable.Generation,
		Actor: actor, CreatedAt: at, UpdatedAt: at,
	}
}

// approvedCandidate checks an approval can be carried out on the Agent now:
// the decision is the latest for its experiment and approves, the bound
// tuple still matches, the Agent is the unchanged baseline, and the
// candidate is the experiment's own, belongs to the Agent, and differs from
// it in its system prompt or model and in nothing else.
func (r *EvalRunReconciler) approvedCandidate(ctx context.Context, namespace, agentName, decisionID string) (*approvedRollout, error) {
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
	previous, err := r.agentPromotionValues(ctx, stable)
	if err != nil {
		return nil, err
	}
	target, err := r.promotionTargetValues(ctx, candidate, outcome)
	if err != nil {
		return nil, err
	}
	if previous == target {
		return nil, refuseRollout("candidate Agent %q has the same system prompt and model as %q; there is nothing to roll out",
			candidate.Name, stable.Name)
	}
	if !equality.Semantic.DeepEqual(withPromotableValues(candidate.Spec, nil, ""), withPromotableValues(stable.Spec, nil, "")) {
		return nil, refuseRollout("candidate Agent %q differs from %q in more than its system prompt and model; only those are rolled out",
			candidate.Name, stable.Name)
	}
	return &approvedRollout{decision: decision, stable: stable, candidate: candidate, previous: previous, target: target}, nil
}

// CanaryAgent starts a canary, or changes its share: percent of new
// Sessions and standalone Tasks that name the Agent run on the approved
// candidate instead. It checks the approval as a promotion does; routing
// checks it again before every Task it sends to the candidate.
func (r *EvalRunReconciler) CanaryAgent(ctx context.Context, namespace, agentName, decisionID string, percent int32,
	actor store.EvalDecisionActor) (*store.AgentPromotionRecord, error) {
	if err := r.awaitPromotionRecovery(ctx); err != nil {
		return nil, err
	}
	if r.ExperimentStore == nil {
		return nil, refuseRollout("canaries need the eval experiment store")
	}
	if percent < 1 || percent > 100 {
		return nil, refuseRollout("percent must be 1 to 100")
	}
	approved, err := r.approvedCandidate(ctx, namespace, agentName, decisionID)
	if err != nil {
		return nil, err
	}
	record := approved.record(store.AgentPromotionCanary, actor, r.now())
	record.Percent = percent
	return r.applyAgentRolloutAction(ctx, approved.stable, record, true,
		func(ctx context.Context, record *store.AgentPromotionRecord) (int64, string, error) {
			return r.writeAgentRollout(ctx, approved.stable, &corev1alpha1.AgentRolloutStatus{
				Candidate: &corev1alpha1.EvalObjectIdentity{
					Name: approved.candidate.Name, UID: approved.candidate.UID, Generation: approved.candidate.Generation,
				},
				Percent: percent, DecisionID: record.DecisionID,
				CandidateRun: &corev1alpha1.EvalObjectIdentity{Name: record.CandidateRun, UID: types.UID(record.CandidateRunUID)},
				RecordID:     record.ID,
			})
		})
}

// AbortAgentRollout ends the Agent's canary: new Tasks run on the stable
// Agent again. A Session that started on the candidate cannot continue and
// needs a new Session.
func (r *EvalRunReconciler) AbortAgentRollout(ctx context.Context, namespace, agentName string, actor store.EvalDecisionActor,
	reason string) (*store.AgentPromotionRecord, error) {
	if err := r.awaitPromotionRecovery(ctx); err != nil {
		return nil, err
	}
	if r.ExperimentStore == nil {
		return nil, refuseRollout("canaries need the eval experiment store")
	}
	stable := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, stable); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, refuseRollout("Agent %q does not exist in %s", agentName, namespace)
		}
		return nil, err
	}
	rollout := stable.Status.Rollout
	if rollout == nil || rollout.Candidate == nil {
		return nil, refuseRollout("Agent %q has no canary to abort", agentName)
	}
	at := r.now()
	record := &store.AgentPromotionRecord{
		SchemaVersion: store.AgentPromotionSchemaVersion, ID: uuid.NewString(), Namespace: namespace,
		Agent: stable.Name, AgentUID: string(stable.UID), Action: store.AgentPromotionAbort, State: store.AgentPromotionPending,
		DecisionID: rollout.DecisionID, PromotionID: rollout.RecordID,
		AgentResourceVersion: stable.ResourceVersion, AgentGeneration: stable.Generation,
		Actor: actor, Reason: strings.TrimSpace(reason), CreatedAt: at, UpdatedAt: at,
	}
	if record.PromotionID == "" {
		record.PromotionID = "unrecorded"
	}
	return r.applyAgentRolloutAction(ctx, stable, record, false,
		func(ctx context.Context, record *store.AgentPromotionRecord) (int64, string, error) {
			return r.writeAgentRollout(ctx, stable, &corev1alpha1.AgentRolloutStatus{RecordID: record.ID})
		})
}

// writeAgentRollout replaces the Agent's status.rollout and clears
// RolloutInvalid, refusing when the Agent changed since the action read it.
func (r *EvalRunReconciler) writeAgentRollout(ctx context.Context, stable *corev1alpha1.Agent,
	rollout *corev1alpha1.AgentRolloutStatus) (int64, string, error) {
	current := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(stable), current); err != nil {
		return 0, "", err
	}
	if current.UID != stable.UID || current.Generation != stable.Generation {
		return 0, "", apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("agents").GroupResource(), stable.Name,
			errors.New("the Agent changed since the rollout action read it"))
	}
	current.Status.Rollout = rollout
	meta.RemoveStatusCondition(&current.Status.Conditions, AgentConditionRolloutInvalid)
	if err := r.Status().Update(ctx, current); err != nil {
		return 0, "", err
	}
	return current.Generation, "", nil
}

// clearAgentRollout drops the Agent's canary, keeping recordID.
func (r *EvalRunReconciler) clearAgentRollout(ctx context.Context, stable *corev1alpha1.Agent, recordID string) error {
	current := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, client.ObjectKeyFromObject(stable), current); err != nil {
		return err
	}
	if current.Status.Rollout == nil {
		return nil
	}
	if recordID == "" {
		recordID = current.Status.Rollout.RecordID
	}
	current.Status.Rollout = &corev1alpha1.AgentRolloutStatus{RecordID: recordID}
	meta.RemoveStatusCondition(&current.Status.Conditions, AgentConditionRolloutInvalid)
	return r.Status().Update(ctx, current)
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
	reader := r.uncachedReader()
	prompt, err := resolveACPSystemPrompt(ctx, reader, candidate)
	if err != nil {
		return store.AgentPromotionValues{}, fmt.Errorf("resolve Agent %q's system prompt: %w", candidate.Name, err)
	}
	soul, err := agentcontext.ResolveSoul(ctx, reader, candidate)
	if err != nil {
		return store.AgentPromotionValues{}, fmt.Errorf("resolve Agent %q's soul: %w", candidate.Name, err)
	}
	sum := sha256.Sum256([]byte(agentcontext.Compose(prompt, soul)))
	if evals.DigestPrefix+hex.EncodeToString(sum[:]) != outcome.Candidate.SystemPromptDigest {
		return store.AgentPromotionValues{}, refuseRollout(
			"candidate Agent %q's system prompt is not the one its experiment ran; run the experiment again", candidate.Name)
	}
	return store.AgentPromotionValues{SystemPrompt: prompt, Model: evalAgentModelName(candidate)}, nil
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
	if rollout := stable.Status.Rollout; rollout != nil && rollout.Candidate != nil {
		return nil, refuseRollout("Agent %q has a canary running; abort it before rolling back", agentName)
	}
	var last *store.AgentPromotionRecord
	for i := range records {
		if records[i].State == store.AgentPromotionComplete && writesAgentSpec(records[i].Action) {
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
	return r.applyAgentRolloutAction(ctx, stable, record, false, r.writePromotedSpec)
}

// rolloutWrite applies a record's change to the Agent, marked with the
// record, and returns the generation and digest it produced.
type rolloutWrite func(context.Context, *store.AgentPromotionRecord) (int64, string, error)

// applyAgentRolloutAction runs a record through its durable steps: insert
// it pending, move it to applying (for an approval, in the same transaction
// that checks its decision is still the approval in force), write the Agent
// with the record's marker, then complete it with what the write produced.
func (r *EvalRunReconciler) applyAgentRolloutAction(ctx context.Context, stable *corev1alpha1.Agent, record *store.AgentPromotionRecord,
	requireApproval bool, write rolloutWrite) (*store.AgentPromotionRecord, error) {
	if err := r.ExperimentStore.CreateAgentPromotion(ctx, record); err != nil {
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

	generation, digest, err := write(ctx, record)
	if err != nil {
		if apierrors.IsConflict(err) || apierrors.IsInvalid(err) || apierrors.IsForbidden(err) {
			fail("the Agent update was refused: " + err.Error())
			return nil, refuseRollout("Agent %q changed while it was being updated; ask again", stable.Name)
		}
		// The write may have committed. Leave the record applying: the
		// next controller start settles it from the marker.
		return nil, fmt.Errorf("update Agent %q: %w", stable.Name, err)
	}
	complete, err := r.ExperimentStore.TransitionAgentPromotion(ctx, store.AgentPromotionTransition{
		Namespace: record.Namespace, ID: record.ID, From: store.AgentPromotionApplying, To: store.AgentPromotionComplete,
		AppliedGeneration: generation, AppliedDigest: digest, UpdatedAt: r.now(),
	})
	if err != nil {
		return nil, err
	}
	logf.FromContext(ctx).Info("Applied an Agent rollout action", "agent", stable.Name, "action", record.Action,
		"record", record.ID, "generation", generation)
	return complete, nil
}

// writePromotedSpec writes a promote or rollback's target values onto the
// Agent with a resourceVersion precondition and the record's marker.
func (r *EvalRunReconciler) writePromotedSpec(ctx context.Context, record *store.AgentPromotionRecord) (int64, string, error) {
	stable := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.Agent}, stable); err != nil {
		return 0, "", err
	}
	if string(stable.UID) != record.AgentUID || stable.ResourceVersion != record.AgentResourceVersion {
		return 0, "", apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("agents").GroupResource(), record.Agent,
			errors.New("the Agent changed since the rollout action read it"))
	}
	updated := stable.DeepCopy()
	updated.Spec = applyPromotableValues(stable.Spec, record.Target)
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[labels.AnnotationPromotionRecord] = record.ID
	if err := r.Update(ctx, updated); err != nil {
		return 0, "", err
	}
	digest, err := agentPromotionDigest(record.Target)
	return updated.Generation, digest, err
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

// AgentRollout returns the Agent's canary, if any, and the message of its
// RolloutInvalid condition.
func (r *EvalRunReconciler) AgentRollout(ctx context.Context, namespace, agentName string) (*corev1alpha1.AgentRolloutStatus, string, error) {
	agent := &corev1alpha1.Agent{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: agentName}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", refuseRollout("Agent %q does not exist in %s", agentName, namespace)
		}
		return nil, "", err
	}
	invalid := ""
	if condition := meta.FindStatusCondition(agent.Status.Conditions, AgentConditionRolloutInvalid); condition != nil &&
		condition.Status == metav1.ConditionTrue {
		invalid = condition.Message
	}
	return agent.Status.Rollout, invalid, nil
}
