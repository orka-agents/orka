/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/store"
)

const (
	evalRunReasonExperimentGateUnavailable = "ExperimentGateUnavailable"
	evalRunReasonExperimentInvalid         = "ExperimentInvalid"
	evalRunReasonCandidateBudgetSpent      = "CandidateBudgetSpent"
	evalRunReasonHeldOutBudgetSpent        = "HeldOutBudgetSpent"
)

// prepareExperiment freezes a candidate run's experiment before its first
// trial: the gate, the baseline's identity and the candidate's digest. A
// missing gate or baseline, or a spent candidate budget, fails the run before
// any trial starts.
func (r *EvalRunReconciler) prepareExperiment(ctx context.Context, run *corev1alpha1.EvalRun, observed *evalRunObservation) (*corev1alpha1.EvalExperimentStatus, error) {
	snapshot := observed.Snapshot
	experiment := run.Spec.Experiment
	gate, gateDigest, splits, err := r.loadExperimentGate(ctx)
	if err != nil {
		return nil, err
	}
	if ref := run.Spec.Cases.ConfigMapRef; ref == nil || splits[run.Namespace+"/"+ref.Name] != evals.SplitHeldOut {
		return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid,
			"an experiment must run on a held-out case set that the protected set's caseSets lists for namespace %s", run.Namespace)
	}
	if experiment.BaselineRunRef.Name == run.Name {
		return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid, "an EvalRun cannot be its own baseline")
	}
	// The baseline is read from the API, not the cache: a stale copy must
	// never stand in for a deleted or recreated baseline.
	baseline := &corev1alpha1.EvalRun{}
	if err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: experiment.BaselineRunRef.Name}, baseline); err != nil {
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
	if evalRunAgentOverride(baseline) {
		return nil, evalRunConfigErrorf(evalRunReasonExperimentInvalid,
			"baseline EvalRun %q sets agentRef; a baseline must review with the monitor's own reviewer", baseline.Name)
	}
	candidateDigest, err := evalCandidateDigest(snapshot)
	if err != nil {
		return nil, err
	}
	if err := r.reserveExperiment(ctx, run, baseline, observed, gate, gateDigest); err != nil {
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

// evalRunAgentOverride reports whether a run reviews with an Agent other than
// its monitor's reviewer.
func evalRunAgentOverride(run *corev1alpha1.EvalRun) bool {
	subject := run.Spec.Subject.RepositoryMonitorReview
	return subject != nil && subject.AgentRef != nil
}

// hasUndecidedCandidates reports whether a candidate still pairs, or may
// pair, with this run: one that froze this incarnation as its baseline and is
// Pending, or one that names it and has not prepared its experiment yet.
func (r *EvalRunReconciler) hasUndecidedCandidates(ctx context.Context, baseline *corev1alpha1.EvalRun) (bool, error) {
	// Read the API, not the cache, so a candidate created a moment ago is
	// never missed before the baseline is deleted.
	var runs corev1alpha1.EvalRunList
	if err := r.uncachedReader().List(ctx, &runs, client.InNamespace(baseline.Namespace)); err != nil {
		return false, err
	}
	for i := range runs.Items {
		candidate := &runs.Items[i]
		if candidate.Spec.Experiment == nil || candidate.UID == baseline.UID || !candidate.DeletionTimestamp.IsZero() {
			continue
		}
		status := candidate.Status.Experiment
		switch {
		case status != nil && status.BaselineRun != nil:
			if status.BaselineRun.UID == baseline.UID && status.Result == corev1alpha1.EvalExperimentPending {
				return true, nil
			}
		case candidate.Spec.Experiment.BaselineRunRef.Name == baseline.Name && !evalRunFinished(candidate.Status.Phase):
			return true, nil
		}
	}
	return false, nil
}

// refuseUnpairedHeldOutCandidate fails a run that reviews with a candidate
// Agent outside an experiment unless its cases ConfigMap is a training set
// the protected set lists. Anything else, a held-out set or an unlisted copy
// of one, would test a candidate against held-out evidence without spending
// an attempt. With no gate configured there are no case sets to protect; a
// configured gate that cannot be read fails closed.
func (r *EvalRunReconciler) refuseUnpairedHeldOutCandidate(ctx context.Context, run *corev1alpha1.EvalRun) error {
	if r.GateConfigMap.Name == "" {
		return nil
	}
	_, _, splits, err := r.loadExperimentGate(ctx)
	if err != nil {
		return err
	}
	if ref := run.Spec.Cases.ConfigMapRef; ref != nil && splits[run.Namespace+"/"+ref.Name] == evals.SplitTrain {
		return nil
	}
	return evalRunConfigErrorf(evalRunReasonExperimentInvalid,
		"a run that sets agentRef outside an experiment must use a training case set the protected set's caseSets lists; add spec.experiment for held-out cases")
}

// reserveExperiment spends one of the gate's attempts on this candidate
// before its first trial. Attempts are durable: deleting a candidate, or its
// TTL, never returns one, so a proposer cannot retry against the held-out
// cases until a candidate passes by chance.
func (r *EvalRunReconciler) reserveExperiment(ctx context.Context, run, baseline *corev1alpha1.EvalRun,
	observed *evalRunObservation, gate evals.ExperimentGate, gateDigest string) error {
	if r.ExperimentStore == nil {
		return evalRunConfigErrorf(evalRunReasonExperimentGateUnavailable, "no experiment store is configured")
	}
	caseKeys, err := evals.HeldOutCaseKeys(observed.Cases)
	if err != nil {
		return evalRunConfigErrorf(evalRunReasonExperimentInvalid, "%v", err)
	}
	record := &store.EvalExperimentRecord{
		SchemaVersion:   store.EvalExperimentSchemaVersion,
		Namespace:       run.Namespace,
		CandidateRun:    run.Name,
		CandidateRunUID: string(run.UID),
		BaselineRun:     baseline.Name,
		BaselineRunUID:  string(baseline.UID),
		CaseSetDigest:   observed.Snapshot.CaseSetDigest,
		CaseKeys:        caseKeys,
		GateDigest:      gateDigest,
		Hypothesis:      run.Spec.Experiment.Hypothesis,
		// The creation time keeps a retried reservation identical.
		ReservedAt: run.CreationTimestamp.UTC(),
	}
	if record.ReservedAt.IsZero() {
		record.ReservedAt = r.now().UTC()
	}
	reservation, err := r.ExperimentStore.ReserveEvalExperiment(ctx, record, store.EvalExperimentLimits{
		MaxPerBaseline: gate.MaxCandidatesPerBaseline,
		MaxPerCase:     gate.MaxExperimentsPerHeldOutSet,
	})
	if errors.Is(err, store.ErrDuplicateMismatch) {
		return evalRunConfigErrorf(evalRunReasonExperimentInvalid,
			"this candidate already reserved an attempt under a different gate or baseline; create a new candidate")
	}
	if err != nil {
		return err
	}
	switch {
	case reservation.Granted:
		return nil
	case reservation.BaselineAttempts >= gate.MaxCandidatesPerBaseline:
		return evalRunConfigErrorf(evalRunReasonCandidateBudgetSpent,
			"baseline EvalRun %q already spent %d candidate attempts, the gate's maxCandidatesPerBaseline; deleted candidates still count",
			baseline.Name, reservation.BaselineAttempts)
	default:
		return evalRunConfigErrorf(evalRunReasonHeldOutBudgetSpent,
			"a held-out case was already in %d experiments, the gate's maxExperimentsPerHeldOutSet; add fresh held-out cases",
			reservation.CaseAttempts)
	}
}

// loadExperimentGate reads the gate and the case set splits of the
// protected-set ConfigMap. Without them, experiments fail closed.
func (r *EvalRunReconciler) loadExperimentGate(ctx context.Context) (evals.ExperimentGate, string, map[string]string, error) {
	if r.GateConfigMap.Name == "" {
		return evals.ExperimentGate{}, "", nil, evalRunConfigErrorf(evalRunReasonExperimentGateUnavailable,
			"no experiment gate is configured; set --eval-gate-configmap to the protected-set ConfigMap")
	}
	configMap := &corev1.ConfigMap{}
	if err := r.uncachedReader().Get(ctx, r.GateConfigMap, configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return evals.ExperimentGate{}, "", nil, evalRunConfigErrorf(evalRunReasonExperimentGateUnavailable,
				"the experiment gate ConfigMap %s was not found", r.GateConfigMap)
		}
		return evals.ExperimentGate{}, "", nil, err
	}
	declaration := []byte(configMap.Data[evals.ProtectedSetConfigMapKey])
	gate, err := evals.ParseExperimentGate(declaration)
	if err == nil {
		var splits map[string]string
		if splits, err = evals.CaseSetSplits(declaration); err == nil {
			digest, digestErr := gate.Digest()
			return gate, digest, splits, digestErr
		}
	}
	return evals.ExperimentGate{}, "", nil, evalRunConfigErrorf(evalRunReasonExperimentGateUnavailable,
		"the experiment gate in ConfigMap %s is invalid: %v", r.GateConfigMap, err)
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
	baseline := &corev1alpha1.EvalRun{}
	err := r.uncachedReader().Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: status.BaselineRun.Name}, baseline)
	if run.Status.Phase != corev1alpha1.EvalRunPhaseSucceeded {
		// Keep the baseline's evidence when it finished; the outcome is final.
		var finished *corev1alpha1.EvalRun
		if err == nil && baseline.UID == status.BaselineRun.UID && evalRunFinished(baseline.Status.Phase) {
			finished = baseline
		} else if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, finished, nil, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{fmt.Sprintf("the candidate run ended %s", run.Status.Phase)}})
	}
	if apierrors.IsNotFound(err) || (err == nil && baseline.UID != status.BaselineRun.UID) {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, nil, nil, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{fmt.Sprintf("baseline EvalRun %q is gone or was recreated", status.BaselineRun.Name)}})
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if evalRunAgentOverride(baseline) {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, baseline, nil, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{fmt.Sprintf("baseline EvalRun %q sets agentRef", baseline.Name)}})
	}
	if !evalRunFinished(baseline.Status.Phase) {
		return ctrl.Result{RequeueAfter: evalRunRequeueInterval}, nil
	}
	if baseline.Status.Phase != corev1alpha1.EvalRunPhaseSucceeded || baseline.Status.Snapshot == nil {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, baseline, nil, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{fmt.Sprintf("the baseline run ended %s", baseline.Status.Phase)}})
	}
	input, trials, incomparable, err := r.experimentInput(ctx, run, baseline)
	if err != nil {
		return ctrl.Result{}, err
	}
	if incomparable != "" {
		return ctrl.Result{}, r.recordExperimentResult(ctx, run, baseline, trials, evals.ExperimentVerdict{Result: evals.ExperimentIncomparable,
			Reasons: []string{incomparable}})
	}
	return ctrl.Result{}, r.recordExperimentResult(ctx, run, baseline, trials, evals.DecideExperiment(input))
}

// experimentInput builds the gate's input. It returns the trial records it
// read, by run UID, so the outcome records the evidence the gate judged.
func (r *EvalRunReconciler) experimentInput(ctx context.Context, candidate, baseline *corev1alpha1.EvalRun) (evals.ExperimentInput, map[types.UID][]store.EvalTrialRecord, string, error) {
	gate := candidate.Status.Experiment.Gate
	significance, err := strconv.ParseFloat(gate.Significance, 64)
	if err != nil {
		return evals.ExperimentInput{}, nil, "", fmt.Errorf("parse the frozen gate significance: %w", err)
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
			return in, nil, "the cases ConfigMap can no longer be read to find tagged cases", nil
		}
		digest, err := evals.CaseSetDigest(cases)
		if err != nil {
			return in, nil, "", err
		}
		if digest != candidate.Status.Snapshot.CaseSetDigest {
			return in, nil, "the cases ConfigMap changed after the runs", nil
		}
		in.CaseTags = make(map[string][]string, len(cases))
		for _, c := range cases {
			in.CaseTags[c.ID] = c.Tags
		}
	}
	trials := make(map[types.UID][]store.EvalTrialRecord, 2)
	for _, arm := range []struct {
		run *corev1alpha1.EvalRun
		out *evals.ExperimentArm
	}{{baseline, &in.Baseline}, {candidate, &in.Candidate}} {
		records, err := r.TrialStore.ListEvalTrials(ctx, arm.run.Namespace, arm.run.Name, string(arm.run.UID))
		if err != nil {
			return in, nil, "", err
		}
		trials[arm.run.UID] = records
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
	return in, trials, "", nil
}

// evalExperimentComparable lists what both arms must share: everything that
// shapes a trial except the candidate's promotable fields (the reviewer
// Agent, its resolved system prompt and its model).
func evalExperimentComparable(run *corev1alpha1.EvalRun) map[string]string {
	snapshot := run.Status.Snapshot
	return map[string]string{
		"case set":             snapshot.CaseSetDigest,
		"grader set":           snapshot.GraderSetDigest,
		"trial plan":           snapshot.TrialPlanDigest,
		"monitor identity":     string(snapshot.MonitorUID) + "/" + strconv.FormatInt(snapshot.MonitorGeneration, 10),
		"review prompt":        snapshot.PromptTemplateDigest,
		"runtime image":        snapshot.RuntimeImageDigest,
		"subject":              string(run.Spec.Subject.Type) + "/" + run.Spec.Subject.RepositoryMonitorReview.MonitorRef.Name,
		"trial timeout":        evalRunTrialTimeout(run).String(),
		"maxConcurrentTrials":  strconv.Itoa(evalRunMaxConcurrentTrials(run)),
		"maxInfraErrorPercent": strconv.Itoa(int(evalRunMaxInfraErrorPercent(run))),
	}
}

// recordExperimentResult stores the experiment's outcome, then reports the
// stored one in status. The store keeps the first outcome, so a decision
// repeated after a failed status update reports what was recorded, and the
// record outlives both runs. baseline is nil when it could not be read, and
// trials holds the records the gate judged, by run UID, when it ran.
func (r *EvalRunReconciler) recordExperimentResult(ctx context.Context, run, baseline *corev1alpha1.EvalRun,
	trials map[types.UID][]store.EvalTrialRecord, verdict evals.ExperimentVerdict) error {
	if r.ExperimentStore == nil {
		return errors.New("the eval experiment store is not configured")
	}
	outcome, err := r.experimentOutcome(ctx, run, baseline, trials, verdict)
	if err != nil {
		return err
	}
	stored, err := r.ExperimentStore.RecordEvalExperimentOutcome(ctx, outcome)
	if err != nil {
		return fmt.Errorf("record the experiment outcome: %w", err)
	}
	status := run.Status.Experiment
	status.Result = corev1alpha1.EvalExperimentResult(stored.Result)
	decidedAt := metav1.NewTime(stored.DecidedAt)
	status.DecidedAt = &decidedAt
	status.Reasons = stored.Reasons
	status.Target = nil
	if target := stored.Target; target != nil {
		status.Target = &corev1alpha1.EvalExperimentTargetResult{
			BaselinePassedTrials:  target.BaselinePassedTrials,
			BaselineTrials:        target.BaselineTrials,
			CandidatePassedTrials: target.CandidatePassedTrials,
			CandidateTrials:       target.CandidateTrials,
			ImprovedPairs:         target.ImprovedPairs,
			RegressedPairs:        target.RegressedPairs,
			ImprovementPoints:     target.ImprovementPoints,
			PValue:                target.PValue,
		}
	}
	return r.Status().Update(ctx, run)
}

// experimentOutcome builds the durable record of a decided experiment.
func (r *EvalRunReconciler) experimentOutcome(ctx context.Context, run, baseline *corev1alpha1.EvalRun,
	trials map[types.UID][]store.EvalTrialRecord, verdict evals.ExperimentVerdict) (*store.EvalExperimentOutcome, error) {
	status := run.Status.Experiment
	outcome := &store.EvalExperimentOutcome{
		SchemaVersion:   store.EvalExperimentOutcomeSchemaVersion,
		Namespace:       run.Namespace,
		CandidateRun:    run.Name,
		CandidateRunUID: string(run.UID),
		Result:          verdict.Result,
		Reasons:         verdict.Reasons,
		DecidedAt:       r.now().UTC().Truncate(time.Second),
	}
	if len(outcome.Reasons) > evalExperimentMaxReasons {
		outcome.Reasons = append(outcome.Reasons[:evalExperimentMaxReasons-1], "more reasons omitted")
	}
	for i := range outcome.Reasons {
		outcome.Reasons[i] = evalRunConditionMessage(outcome.Reasons[i])
	}
	if target := verdict.Target; target != nil {
		outcome.Target = &store.EvalExperimentTargetOutcome{
			Grader:                string(run.Spec.Experiment.Target.Grader),
			Tag:                   run.Spec.Experiment.Target.Tag,
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
	var err error
	if outcome.Candidate, err = r.experimentArm(ctx, run, trials); err != nil {
		return nil, err
	}
	if baseline != nil && baseline.UID == status.BaselineRun.UID {
		if outcome.Baseline, err = r.experimentArm(ctx, baseline, trials); err != nil {
			return nil, err
		}
	} else {
		outcome.Baseline = store.EvalExperimentArmOutcome{Run: status.BaselineRun.Name, RunUID: string(status.BaselineRun.UID)}
	}
	if verdict.Result == evals.ExperimentIncomparable {
		return outcome, nil
	}
	if baseline == nil || baseline.Status.Snapshot == nil || run.Status.Snapshot == nil {
		return nil, errors.New("a compared experiment needs both run snapshots")
	}
	baselineDigest, err := evalCandidateDigest(*baseline.Status.Snapshot)
	if err != nil {
		return nil, err
	}
	snapshot := run.Status.Snapshot
	outcome.Binding = &store.EvalDecisionBinding{
		CandidateAgent:       evalAgentIdentity(snapshot.ReviewerAgent),
		CandidateDigest:      status.CandidateDigest,
		BaselineAgent:        evalAgentIdentity(baseline.Status.Snapshot.ReviewerAgent),
		BaselineDigest:       baselineDigest,
		MonitorUID:           string(snapshot.MonitorUID),
		MonitorGeneration:    snapshot.MonitorGeneration,
		PromptTemplateDigest: snapshot.PromptTemplateDigest,
		CaseSetDigest:        snapshot.CaseSetDigest,
		GraderSetDigest:      snapshot.GraderSetDigest,
		GateDigest:           status.GateDigest,
	}
	return outcome, nil
}

// experimentArm summarizes one run for the outcome: its reviewer and
// snapshot, counts, token use and each trial's grader statuses, never review
// text. It uses the records the gate judged when trials has them.
func (r *EvalRunReconciler) experimentArm(ctx context.Context, run *corev1alpha1.EvalRun,
	trials map[types.UID][]store.EvalTrialRecord) (store.EvalExperimentArmOutcome, error) {
	counts := run.Status.Counts
	arm := store.EvalExperimentArmOutcome{
		Run: run.Name, RunUID: string(run.UID),
		Planned: counts.Planned, Completed: counts.Completed, Passed: counts.Passed, Failed: counts.Failed, InfraError: counts.InfraError,
	}
	if snapshot := run.Status.Snapshot; snapshot != nil {
		arm.Agent = evalAgentIdentity(snapshot.ReviewerAgent)
		arm.Model = snapshot.Model
		arm.SystemPromptDigest = snapshot.ReviewerSystemPromptDigest
		arm.RuntimeImageDigest = snapshot.RuntimeImageDigest
		arm.MonitorUID, arm.MonitorGeneration = string(snapshot.MonitorUID), snapshot.MonitorGeneration
		arm.PromptTemplateDigest = snapshot.PromptTemplateDigest
		arm.CaseSetDigest, arm.GraderSetDigest = snapshot.CaseSetDigest, snapshot.GraderSetDigest
		arm.TrialPlanDigest = snapshot.TrialPlanDigest
	}
	records, judged := trials[run.UID]
	if !judged {
		var err error
		if records, err = r.TrialStore.ListEvalTrials(ctx, run.Namespace, run.Name, string(run.UID)); err != nil {
			return arm, err
		}
	}
	for _, record := range records {
		arm.InputTokens = addEvalTokens(arm.InputTokens, record.InputTokens)
		arm.OutputTokens = addEvalTokens(arm.OutputTokens, record.OutputTokens)
		trial := store.EvalExperimentTrialOutcome{CaseID: record.CaseID, Trial: record.Trial, Outcome: record.Outcome, InfraClass: record.InfraClass}
		for _, grade := range record.Grades {
			trial.Grades = append(trial.Grades, store.EvalExperimentGrade{Grader: grade.Grader, Status: grade.Status})
		}
		arm.Trials = append(arm.Trials, trial)
	}
	return arm, nil
}

// addEvalTokens adds a trial's reported count, keeping the total unset until
// some trial reports one.
func addEvalTokens(total, reported *int64) *int64 {
	if reported == nil {
		return total
	}
	sum := *reported
	if total != nil {
		sum += *total
	}
	return &sum
}

func evalAgentIdentity(identity *corev1alpha1.EvalObjectIdentity) store.EvalAgentIdentity {
	if identity == nil {
		return store.EvalAgentIdentity{}
	}
	return store.EvalAgentIdentity{Name: identity.Name, UID: string(identity.UID), Generation: identity.Generation}
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
