/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const evalTestGate = `schemaVersion: orka.protectedSet.v1
paths: [".github/**"]
gate:
  maxInfraErrorPercent: 10
  maxCandidatesPerBaseline: 2
  maxExperimentsPerHeldOutSet: 1
  minTargetImprovementPoints: 5
  significance: 0.05
`

// experimentEnv holds a baseline and a candidate run over three defect cases.
type experimentEnv struct {
	*evalRunTestEnv
}

func experimentCases() string {
	return evalTestCaseLine("defect-1", evals.ReferenceVerdictNotPassed, "defect") + "\n" +
		evalTestCaseLine("defect-2", evals.ReferenceVerdictNotPassed, "defect") + "\n" +
		evalTestCaseLine("defect-3", evals.ReferenceVerdictNotPassed, "defect") + "\n"
}

func experimentRuns(baselineTrials, candidateTrials int32) (*corev1alpha1.EvalRun, *corev1alpha1.EvalRun) {
	baseline := evalTestRun(baselineTrials, 10)
	baseline.Name, baseline.UID = "baseline", "baseline-uid"
	candidate := evalTestRun(candidateTrials, 10)
	candidate.Name, candidate.UID = "candidate", "candidate-uid"
	candidate.Spec.Subject.RepositoryMonitorReview.AgentRef = &corev1alpha1.EvalObjectReference{Name: "candidate-agent"}
	candidate.Spec.Experiment = &corev1alpha1.EvalExperiment{
		Hypothesis:     "The reviewer misses defects.",
		BaselineRunRef: corev1alpha1.EvalObjectReference{Name: "baseline"},
		Target:         corev1alpha1.EvalExperimentTarget{Grader: corev1alpha1.EvalGraderVerdict, Tag: "defect"},
	}
	return baseline, candidate
}

func newExperimentEnv(t *testing.T, baseline, candidate *corev1alpha1.EvalRun, gate string) *experimentEnv {
	t.Helper()
	objects := evalTestObjects(baseline, experimentCases())
	objects = append(objects, candidate, &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "candidate-agent", Namespace: evalTestNamespace, UID: "candidate-agent-uid", Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model:   &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-sonnet"},
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
		},
	})
	if gate != "" {
		objects = append(objects, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "protected-set", Namespace: "orka-system"},
			Data:       map[string]string{evals.ProtectedSetConfigMapKey: gate},
		})
	}
	env := newEvalRunTestEnv(t, objects...)
	env.reconciler.GateConfigMap = types.NamespacedName{Namespace: "orka-system", Name: "protected-set"}
	return &experimentEnv{env}
}

// baselineCaseSetDigest snapshots the baseline and returns its case set
// digest, which the candidate shares.
func (e *experimentEnv) baselineCaseSetDigest() string {
	e.t.Helper()
	e.reconcileRun("baseline") // finalizer
	e.reconcileRun("baseline") // snapshot
	snapshot := e.getRun("baseline").Status.Snapshot
	if snapshot == nil {
		e.t.Fatal("the baseline recorded no snapshot")
	}
	return snapshot.CaseSetDigest
}

// reserveEarlier records the attempt of an earlier candidate that no longer
// exists, against the given baseline and case set.
func (e *experimentEnv) reserveEarlier(baselineUID, caseSetDigest string) {
	e.t.Helper()
	if caseSetDigest == "" {
		caseSetDigest = "sha256:other-cases"
	}
	granted, err := e.store.ReserveEvalExperiment(context.Background(), &store.EvalExperimentRecord{
		SchemaVersion: store.EvalExperimentSchemaVersion, Namespace: "other-team", CandidateRun: "earlier",
		CandidateRunUID: "earlier-uid", BaselineRun: "baseline", BaselineRunUID: baselineUID, CaseSetDigest: caseSetDigest,
		GateDigest: "sha256:gate", Hypothesis: "earlier", ReservedAt: evalTestNow,
	}, store.EvalExperimentLimits{MaxPerBaseline: 10, MaxPerCaseSet: 10})
	if err != nil || !granted.Granted {
		e.t.Fatalf("reserve the earlier attempt: %+v, %v", granted, err)
	}
}

func (e *experimentEnv) reconcileRun(name string) ctrl.Result {
	e.t.Helper()
	result, err := e.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: evalTestNamespace, Name: name}})
	if err != nil {
		e.t.Fatalf("Reconcile(%s) error = %v", name, err)
	}
	return result
}

func (e *experimentEnv) getRun(name string) *corev1alpha1.EvalRun {
	e.t.Helper()
	run := &corev1alpha1.EvalRun{}
	if err := e.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: name}, run); err != nil {
		e.t.Fatal(err)
	}
	return run
}

func (e *experimentEnv) tasksOf(name string) []corev1alpha1.Task {
	e.t.Helper()
	var list corev1alpha1.TaskList
	if err := e.client.List(context.Background(), &list, client.InNamespace(evalTestNamespace),
		client.MatchingLabels{labels.LabelEvalRun: name}); err != nil {
		e.t.Fatal(err)
	}
	return list.Items
}

// runToEnd starts a run, creates its trials and finishes every one with the
// given review verdict.
func (e *experimentEnv) runToEnd(name, verdict string) {
	e.t.Helper()
	e.reconcileRun(name) // finalizer
	e.reconcileRun(name) // snapshot
	e.reconcileRun(name) // trials
	tasks := e.tasksOf(name)
	if len(tasks) == 0 {
		e.t.Fatalf("%s created no trial Tasks: %+v", name, e.getRun(name).Status)
	}
	for i := range tasks {
		e.finishTask(&tasks[i], corev1alpha1.TaskPhaseSucceeded, evalTestReviewResult(verdict), true)
	}
	e.reconcileRun(name)
	if phase := e.getRun(name).Status.Phase; phase != corev1alpha1.EvalRunPhaseSucceeded {
		e.t.Fatalf("%s phase = %q: %+v", name, phase, e.getRun(name).Status.Conditions)
	}
}

func TestEvalExperimentPassesASignificantImprovement(t *testing.T) {
	baseline, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed) // the stable reviewer passes every defect
	env.runToEnd("candidate", "needs_changes")                     // the candidate holds them back

	// Trials review with the candidate Agent, not the monitor's reviewer.
	for _, task := range env.tasksOf("candidate") {
		if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "candidate-agent" {
			t.Fatalf("candidate trial Agent = %+v", task.Spec.AgentRef)
		}
	}
	env.reconcileRun("candidate")
	experiment := env.getRun("candidate").Status.Experiment
	if experiment == nil || experiment.Result != corev1alpha1.EvalExperimentPassed {
		t.Fatalf("experiment = %+v", experiment)
	}
	if experiment.BaselineRun.UID != "baseline-uid" || experiment.CandidateDigest == "" || experiment.GateDigest == "" ||
		experiment.Gate.Significance != "0.05" {
		t.Fatalf("experiment = %+v", experiment)
	}
	target := experiment.Target
	if target.BaselinePassedTrials != 0 || target.CandidatePassedTrials != 9 || target.ImprovedPairs != 9 ||
		target.RegressedPairs != 0 || target.ImprovementPoints != "100" || !strings.HasPrefix(target.PValue, "0.00195") {
		t.Fatalf("target = %+v", target)
	}
	// The decision is final: another reconcile keeps it.
	env.reconcileRun("candidate")
	if got := env.getRun("candidate").Status.Experiment.Result; got != corev1alpha1.EvalExperimentPassed {
		t.Fatalf("result after another reconcile = %q", got)
	}
	// The candidate's attempt is held in the store, where it outlives the run.
	reserved, err := env.store.GetEvalExperiment(context.Background(), evalTestNamespace, "candidate-uid")
	if err != nil || reserved.BaselineRunUID != "baseline-uid" || reserved.CaseSetDigest != env.getRun("candidate").Status.Snapshot.CaseSetDigest {
		t.Fatalf("reservation = %+v, %v", reserved, err)
	}
}

func TestEvalExperimentFailsWithoutImprovement(t *testing.T) {
	baseline, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", repositoryMonitorReviewVerdictPassed)
	env.reconcileRun("candidate")
	experiment := env.getRun("candidate").Status.Experiment
	if experiment.Result != corev1alpha1.EvalExperimentFailed ||
		!strings.Contains(strings.Join(experiment.Reasons, "; "), "rose by 0 points") {
		t.Fatalf("experiment = %+v", experiment)
	}
}

func TestEvalExperimentWaitsForTheBaseline(t *testing.T) {
	baseline, candidate := experimentRuns(1, 1)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.reconcileRun("baseline") // finalizer only: the baseline has not started
	env.runToEnd("candidate", "needs_changes")
	result := env.reconcileRun("candidate")
	if got := env.getRun("candidate").Status.Experiment.Result; got != corev1alpha1.EvalExperimentPending || result.RequeueAfter == 0 {
		t.Fatalf("result = %q, requeue = %v", got, result.RequeueAfter)
	}
}

// Runs with different trial plans cannot be compared.
func TestEvalExperimentIncomparableWhenThePlansDiffer(t *testing.T) {
	baseline, candidate := experimentRuns(2, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", "needs_changes")
	env.reconcileRun("candidate")
	experiment := env.getRun("candidate").Status.Experiment
	if experiment.Result != corev1alpha1.EvalExperimentIncomparable ||
		!strings.Contains(strings.Join(experiment.Reasons, "; "), "trial plan differs") {
		t.Fatalf("experiment = %+v", experiment)
	}
}

func TestEvalExperimentIncomparableWhenTheBaselineIsGone(t *testing.T) {
	baseline, candidate := experimentRuns(1, 1)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", "needs_changes")
	run := env.getRun("baseline")
	run.Finalizers = nil
	if err := env.client.Update(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := env.client.Delete(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	env.reconcileRun("candidate")
	if got := env.getRun("candidate").Status.Experiment.Result; got != corev1alpha1.EvalExperimentIncomparable {
		t.Fatalf("result = %q", got)
	}
}

// A candidate that cannot be paired fails before any trial starts.
func TestEvalExperimentFailsClosedBeforeTrials(t *testing.T) {
	for name, test := range map[string]struct {
		gate   string
		mutate func(baseline, candidate *corev1alpha1.EvalRun)
		seed   func(*experimentEnv)
		reason string
	}{
		"no gate":   {reason: evalRunReasonExperimentGateUnavailable},
		"bad gate":  {gate: strings.Replace(evalTestGate, "significance: 0.05", "significance: 0", 1), reason: evalRunReasonExperimentGateUnavailable},
		"no target": {gate: "schemaVersion: orka.protectedSet.v1\ngate: {maxInfraErrorPercent: 10, maxCandidatesPerBaseline: 3}\n", reason: evalRunReasonExperimentGateUnavailable},
		"missing baseline": {gate: evalTestGate, mutate: func(_, candidate *corev1alpha1.EvalRun) {
			candidate.Spec.Experiment.BaselineRunRef.Name = "nowhere"
		}, reason: evalRunReasonExperimentInvalid},
		"baseline is a candidate": {gate: evalTestGate, mutate: func(baseline, _ *corev1alpha1.EvalRun) {
			baseline.Spec.Experiment = &corev1alpha1.EvalExperiment{Hypothesis: "x",
				BaselineRunRef: corev1alpha1.EvalObjectReference{Name: "other"},
				Target:         corev1alpha1.EvalExperimentTarget{Grader: corev1alpha1.EvalGraderVerdict}}
		}, reason: evalRunReasonExperimentInvalid},
		"baseline sets agentRef": {gate: evalTestGate, mutate: func(baseline, _ *corev1alpha1.EvalRun) {
			baseline.Spec.Subject.RepositoryMonitorReview.AgentRef = &corev1alpha1.EvalObjectReference{Name: "candidate-agent"}
		}, reason: evalRunReasonExperimentInvalid},
		// An earlier candidate's attempt still counts after that candidate
		// was deleted: only its reservation remains.
		"candidate budget spent": {gate: strings.Replace(evalTestGate, "maxCandidatesPerBaseline: 2", "maxCandidatesPerBaseline: 1", 1),
			seed: func(env *experimentEnv) { env.reserveEarlier("baseline-uid", "") }, reason: evalRunReasonCandidateBudgetSpent},
		"held-out budget spent": {gate: evalTestGate,
			seed:   func(env *experimentEnv) { env.reserveEarlier("other-baseline-uid", env.baselineCaseSetDigest()) },
			reason: evalRunReasonHeldOutBudgetSpent},
	} {
		t.Run(name, func(t *testing.T) {
			baseline, candidate := experimentRuns(1, 1)
			if test.mutate != nil {
				test.mutate(baseline, candidate)
			}
			env := newExperimentEnv(t, baseline, candidate, test.gate)
			if test.seed != nil {
				test.seed(env)
			}
			env.reconcileRun("candidate")
			env.reconcileRun("candidate")
			run := env.getRun("candidate")
			if run.Status.Phase != corev1alpha1.EvalRunPhaseFailed || evalCompletedCondition(run).Reason != test.reason {
				t.Fatalf("phase = %q condition = %+v", run.Status.Phase, evalCompletedCondition(run))
			}
			if len(env.tasksOf("candidate")) != 0 || run.Status.Experiment != nil {
				t.Fatal("a candidate that cannot be paired started trials or recorded an experiment")
			}
		})
	}
}
