/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
)

const (
	evalRunReasonExperimentGateUnavailable = "ExperimentGateUnavailable"
	evalRunReasonExperimentInvalid         = "ExperimentInvalid"
	evalRunReasonCandidateBudgetSpent      = "CandidateBudgetSpent"
)

// prepareExperiment freezes a candidate run's experiment before its first
// trial: the gate, the baseline's identity and the candidate's digest. A
// missing gate or baseline, or a spent candidate budget, fails the run before
// any trial starts.
func (r *EvalRunReconciler) prepareExperiment(ctx context.Context, run *corev1alpha1.EvalRun, snapshot corev1alpha1.EvalRunSnapshot) (*corev1alpha1.EvalExperimentStatus, error) {
	experiment := run.Spec.Experiment
	gate, gateDigest, err := r.loadExperimentGate(ctx)
	if err != nil {
		return nil, err
	}
	if experiment.BaselineRunRef.Name == run.Name {
		return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid, "an EvalRun cannot be its own baseline")
	}
	baseline := &corev1alpha1.EvalRun{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: experiment.BaselineRunRef.Name}, baseline); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid, "baseline EvalRun %q was not found; create it first", experiment.BaselineRunRef.Name)
		}
		return nil, err
	}
	if baseline.Spec.Experiment != nil {
		return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid, "baseline EvalRun %q is itself an experiment candidate", baseline.Name)
	}
	if !baseline.DeletionTimestamp.IsZero() {
		return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid, "baseline EvalRun %q is being deleted", baseline.Name)
	}
	// Count the other candidates already paired with this baseline incarnation.
	var runs corev1alpha1.EvalRunList
	if err := r.List(ctx, &runs, client.InNamespace(run.Namespace)); err != nil {
		return nil, err
	}
	candidates := 0
	for i := range runs.Items {
		other := &runs.Items[i]
		if other.UID != run.UID && other.Status.Experiment != nil && other.Status.Experiment.BaselineRun != nil &&
			other.Status.Experiment.BaselineRun.UID == baseline.UID {
			candidates++
		}
	}
	if candidates >= int(gate.MaxCandidatesPerBaseline) {
		return nil, evalRunConfigErrorf(evalRunReasonCandidateBudgetSpent,
			"baseline EvalRun %q already has %d candidates, the gate's maxCandidatesPerBaseline", baseline.Name, candidates)
	}
	candidateDigest, err := evalCandidateDigest(snapshot)
	if err != nil {
		return nil, err
	}
	return &corev1alpha1.EvalExperimentStatus{
		Result:          corev1alpha1.EvalExperimentPending,
		BaselineRun:     &corev1alpha1.EvalObjectIdentity{Name: baseline.Name, UID: baseline.UID, Generation: baseline.Generation},
		CandidateDigest: candidateDigest,
		GateDigest:      gateDigest,
		Gate: &corev1alpha1.EvalExperimentGate{
			MaxInfraErrorPercent:        gate.MaxInfraErrorPercent,
			MaxCandidatesPerBaseline:    gate.MaxCandidatesPerBaseline,
			MaxExperimentsPerHeldOutSet: gate.MaxExperimentsPerHeldOutSet,
			MinTargetImprovementPoints:  gate.MinTargetImprovementPoints,
			Significance:                evals.FormatDecimal(gate.Significance),
		},
	}, nil
}

// loadExperimentGate reads the gate section of the protected-set ConfigMap.
// Without one, experiments fail closed.
func (r *EvalRunReconciler) loadExperimentGate(ctx context.Context) (evals.ExperimentGate, string, error) {
	if r.GateConfigMap.Name == "" {
		return evals.ExperimentGate{}, "", evalRunConfigErrorf(evalRunReasonExperimentGateUnavailable,
			"no experiment gate is configured; set --eval-gate-configmap to the protected-set ConfigMap")
	}
	configMap := &corev1.ConfigMap{}
	if err := r.uncachedReader().Get(ctx, r.GateConfigMap, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return evals.ExperimentGate{}, "", evalRunConfigErrorf(evalRunReasonExperimentGateUnavailable,
				"the experiment gate ConfigMap %s was not found", r.GateConfigMap)
		}
		return evals.ExperimentGate{}, "", err
	}
	gate, err := evals.ParseExperimentGate([]byte(configMap.Data[evals.ProtectedSetConfigMapKey]))
	if err != nil {
		return evals.ExperimentGate{}, "", evalRunConfigErrorf(evalRunReasonExperimentGateUnavailable,
			"the experiment gate in ConfigMap %s is invalid: %v", r.GateConfigMap, err)
	}
	digest, err := gate.Digest()
	if err != nil {
		return evals.ExperimentGate{}, "", err
	}
	return gate, digest, nil
}

// evalCandidateDigest digests the candidate's promotable fields as the
// snapshot froze them.
func evalCandidateDigest(snapshot corev1alpha1.EvalRunSnapshot) (string, error) {
	return evals.DigestJSON(struct {
		Reviewer           *corev1alpha1.EvalObjectIdentity `json:"reviewer"`
		SystemPromptDigest string                           `json:"systemPromptDigest"`
		Model              string                           `json:"model"`
		RuntimeImageDigest string                           `json:"runtimeImageDigest"`
	}{snapshot.ReviewerAgent, snapshot.ReviewerSystemPromptDigest, snapshot.Model, snapshot.RuntimeImageDigest})
}

// decideExperiment judges a finished candidate run against its baseline once
// the baseline has finished too. It requeues while the baseline runs.
func (r *EvalRunReconciler) decideExperiment(ctx context.Context, run *corev1alpha1.EvalRun) (ctrl.Result, error) {
	status := run.Status.Experiment
	if run.Status.Phase != corev1alpha1.EvalRunPhaseSucceeded {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{fmt.Sprintf("the candidate run ended %s", run.Status.Phase)}})
	}
	baseline := &corev1alpha1.EvalRun{}
	err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: status.BaselineRun.Name}, baseline)
	if apierrors.IsNotFound(err) || (err == nil && baseline.UID != status.BaselineRun.UID) {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{fmt.Sprintf("baseline EvalRun %q is gone or was recreated", status.BaselineRun.Name)}})
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !evalRunFinished(baseline.Status.Phase) {
		return ctrl.Result{RequeueAfter: evalRunRequeueInterval}, nil
	}
	if baseline.Status.Phase != corev1alpha1.EvalRunPhaseSucceeded || baseline.Status.Snapshot == nil {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{fmt.Sprintf("the baseline run ended %s", baseline.Status.Phase)}})
	}
	input, incomparable, err := r.experimentInput(ctx, run, baseline)
	if err != nil {
		return ctrl.Result{}, err
	}
	if incomparable != "" {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{incomparable}})
	}
	return ctrl.Result{}, r.recordExperimentResult(ctx, run, evals.DecideExperiment(input))
}

func (r *EvalRunReconciler) experimentInput(ctx context.Context, candidate, baseline *corev1alpha1.EvalRun) (evals.ExperimentInput, string, error) {
	gate := candidate.Status.Experiment.Gate
	significance, err := strconv.ParseFloat(gate.Significance, 64)
	if err != nil {
		return evals.ExperimentInput{}, "", fmt.Errorf("parse the frozen gate significance: %w", err)
	}
	in := evals.ExperimentInput{
		Gate: evals.ExperimentGate{
			MaxInfraErrorPercent:        gate.MaxInfraErrorPercent,
			MaxCandidatesPerBaseline:    gate.MaxCandidatesPerBaseline,
			MaxExperimentsPerHeldOutSet: gate.MaxExperimentsPerHeldOutSet,
			MinTargetImprovementPoints:  gate.MinTargetImprovementPoints,
			Significance:                significance,
		},
		Grader: string(candidate.Spec.Experiment.Target.Grader),
		Tag:    candidate.Spec.Experiment.Target.Tag,
	}
	for _, grader := range candidate.Spec.Graders {
		in.Graders = append(in.Graders, string(grader))
	}
	if in.Tag != "" {
		// Trial records carry no tags; read them from the cases, which must
		// still be the ones both runs used.
		cases, err := r.loadEvalCases(ctx, candidate)
		if err != nil {
			return in, "the cases ConfigMap can no longer be read to find tagged cases", nil
		}
		digest, err := evals.CaseSetDigest(cases)
		if err != nil {
			return in, "", err
		}
		if digest != candidate.Status.Snapshot.CaseSetDigest {
			return in, "the cases ConfigMap changed after the runs", nil
		}
		in.CaseTags = make(map[string][]string, len(cases))
		for _, c := range cases {
			in.CaseTags[c.ID] = c.Tags
		}
	}
	for _, arm := range []struct {
		run *corev1alpha1.EvalRun
		out *evals.ExperimentArm
	}{{baseline, &in.Baseline}, {candidate, &in.Candidate}} {
		records, err := r.TrialStore.ListEvalTrials(ctx, arm.run.Namespace, arm.run.Name, string(arm.run.UID))
		if err != nil {
			return in, "", err
		}
		arm.out.Comparable = evalExperimentComparable(arm.run)
		arm.out.Planned = int(arm.run.Status.Counts.Planned)
		arm.out.InfraError = int(arm.run.Status.Counts.InfraError)
		for _, record := range records {
			trial := evals.ArmTrial{CaseID: record.CaseID, Trial: record.Trial, Infra: record.InfraClass != "",
				Grades: make(map[string]evals.GradeStatus, len(record.Grades))}
			for _, grade := range record.Grades {
				trial.Grades[grade.Grader] = evals.GradeStatus(grade.Status)
			}
			arm.out.Trials = append(arm.out.Trials, trial)
		}
	}
	return in, "", nil
}

// evalExperimentComparable lists what both arms must share: everything that
// shapes a trial except the candidate's promotable fields (the reviewer
// Agent, its resolved system prompt and its model).
func evalExperimentComparable(run *corev1alpha1.EvalRun) map[string]string {
	snapshot := run.Status.Snapshot
	graders := make([]string, 0, len(run.Spec.Graders))
	for _, grader := range run.Spec.Graders {
		graders = append(graders, string(grader))
	}
	return map[string]string{
		"case set":             snapshot.CaseSetDigest,
		"grader set":           snapshot.GraderSetDigest,
		"trial plan":           snapshot.TrialPlanDigest,
		"monitor identity":     string(snapshot.MonitorUID) + "/" + strconv.FormatInt(snapshot.MonitorGeneration, 10),
		"review prompt":        snapshot.PromptTemplateDigest,
		"runtime image":        snapshot.RuntimeImageDigest,
		"subject":              string(run.Spec.Subject.Type) + "/" + run.Spec.Subject.RepositoryMonitorReview.MonitorRef.Name,
		"graders":              strings.Join(graders, ","),
		"maxInfraErrorPercent": strconv.Itoa(int(evalRunMaxInfraErrorPercent(run))),
	}
}

func (r *EvalRunReconciler) recordExperimentResult(ctx context.Context, run *corev1alpha1.EvalRun, verdict evals.ExperimentVerdict) error {
	status := run.Status.Experiment
	status.Result = corev1alpha1.EvalExperimentResult(verdict.Result)
	status.Reasons = verdict.Reasons
	if len(status.Reasons) > evalExperimentMaxReasons {
		status.Reasons = append(status.Reasons[:evalExperimentMaxReasons-1], "more reasons omitted")
	}
	for i := range status.Reasons {
		status.Reasons[i] = evalRunConditionMessage(status.Reasons[i])
	}
	if target := verdict.Target; target != nil {
		status.Target = &corev1alpha1.EvalExperimentTargetResult{
			BaselinePassedTrials:  int32(target.BaselinePassed),
			BaselineTrials:        int32(target.BaselineTrials),
			CandidatePassedTrials: int32(target.CandidatePassed),
			CandidateTrials:       int32(target.CandidateTrials),
			ImprovedPairs:         int32(target.Improved),
			RegressedPairs:        int32(target.Regressed),
			ImprovementPoints:     evals.FormatDecimal(roundTo(target.ImprovementPoints, 4)),
			PValue:                strconv.FormatFloat(target.PValue, 'g', 6, 64),
		}
	}
	return r.Status().Update(ctx, run)
}

// evalExperimentMaxReasons matches the status reasons list bound.
const evalExperimentMaxReasons = 32

func roundTo(value float64, places int) float64 {
	scale := 1.0
	for range places {
		scale *= 10
	}
	if value < 0 {
		return -float64(int64(-value*scale+0.5)) / scale
	}
	return float64(int64(value*scale+0.5)) / scale
}
