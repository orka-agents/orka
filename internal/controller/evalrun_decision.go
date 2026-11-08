/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/store"
)

// CurrentEvalDecisionBinding recomputes what an approval of the candidate
// would bind to now, from the live monitor, both reviewer Agents and their
// resolved prompts, the cases, the graders and the gate, all read uncached.
// It fails when the cases are no longer listed as held out.
// An approval is current only while this equals the binding recorded with
// the experiment's outcome.
func (r *EvalRunReconciler) CurrentEvalDecisionBinding(ctx context.Context, candidate *corev1alpha1.EvalRun) (*store.EvalDecisionBinding, error) {
	reader := r.uncachedReader()
	subject, err := r.resolveEvalReviewSubject(ctx, reader, candidate)
	if err != nil {
		return nil, err
	}
	stableRun := candidate.DeepCopy()
	stableRun.Spec.Subject.RepositoryMonitorReview.AgentRef = nil
	stable, err := r.resolveEvalReviewSubject(ctx, reader, stableRun)
	if err != nil {
		return nil, err
	}
	cases, err := r.loadEvalCases(ctx, candidate)
	if err != nil {
		return nil, err
	}
	caseSetDigest, err := evals.CaseSetDigest(cases)
	if err != nil {
		return nil, err
	}
	graderSetDigest, err := r.graders().GraderSetDigest(candidate.Spec.Graders)
	if err != nil {
		return nil, err
	}
	_, gateDigest, splits, err := r.loadExperimentGate(ctx)
	if err != nil {
		return nil, err
	}
	// The cases must still be held out: cases moved to training, or no
	// longer listed, may have been read since the experiment ran.
	if ref := candidate.Spec.Cases.ConfigMapRef; ref == nil || splits[candidate.Namespace+"/"+ref.Name] != evals.SplitHeldOut {
		return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid,
			"the candidate's cases ConfigMap is no longer listed with split: heldOut in the protected set")
	}
	candidateSnapshot, stableSnapshot := subject.snapshot(), stable.snapshot()
	candidateDigest, err := evalCandidateDigest(candidateSnapshot)
	if err != nil {
		return nil, err
	}
	stableDigest, err := evalCandidateDigest(stableSnapshot)
	if err != nil {
		return nil, err
	}
	return &store.EvalDecisionBinding{
		CandidateAgent:       evalAgentIdentity(candidateSnapshot.ReviewerAgent),
		CandidateDigest:      candidateDigest,
		BaselineAgent:        evalAgentIdentity(stableSnapshot.ReviewerAgent),
		BaselineDigest:       stableDigest,
		MonitorUID:           string(candidateSnapshot.MonitorUID),
		MonitorGeneration:    candidateSnapshot.MonitorGeneration,
		PromptTemplateDigest: candidateSnapshot.PromptTemplateDigest,
		CaseSetDigest:        caseSetDigest,
		GraderSetDigest:      graderSetDigest,
		GateDigest:           gateDigest,
	}, nil
}
