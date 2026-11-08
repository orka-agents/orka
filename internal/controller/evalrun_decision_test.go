/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/store"
)

// experimentEntry returns the stored experiment of the candidate run.
func (e *experimentEnv) experimentEntry() store.EvalExperimentEntry {
	e.t.Helper()
	page, err := e.store.ListEvalExperiments(context.Background(), evalTestNamespace, store.EvalExperimentListOptions{CandidateRunUID: "candidate-uid"})
	if err != nil || len(page.Items) != 1 {
		e.t.Fatalf("experiments = %+v, %v", page, err)
	}
	return page.Items[0]
}

// A decided experiment's outcome is stored with what an approval binds to,
// and the same decision repeated later reports the stored outcome. The
// outcome outlives both runs.
func TestEvalExperimentOutcomeIsStoredOnceAndOutlivesTheRuns(t *testing.T) {
	baseline, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", "needs_changes")
	env.reconcileRun("candidate")
	decided := env.getRun("candidate")
	outcome := env.experimentEntry().Outcome
	if outcome == nil || outcome.Result != store.EvalExperimentResultPassed || outcome.Binding == nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if !outcome.DecidedAt.Equal(decided.Status.Experiment.DecidedAt.Time) || outcome.Target.CandidatePassedTrials != 9 {
		t.Fatalf("outcome = %+v, status = %+v", outcome, decided.Status.Experiment)
	}
	binding := *outcome.Binding
	if binding.CandidateAgent != (store.EvalAgentIdentity{Name: "candidate-agent", UID: "candidate-agent-uid", Generation: 1}) ||
		binding.BaselineAgent != (store.EvalAgentIdentity{Name: "reviewer", UID: "agent-uid", Generation: 2}) ||
		binding.CandidateDigest != decided.Status.Experiment.CandidateDigest || binding.BaselineDigest == "" ||
		binding.CaseSetDigest != decided.Status.Snapshot.CaseSetDigest || binding.GateDigest != decided.Status.Experiment.GateDigest ||
		binding.MonitorUID != "monitor-uid" || binding.MonitorGeneration != 3 {
		t.Fatalf("binding = %+v", binding)
	}
	checkOutcomeEvidence(t, outcome, decided)

	// A decision repeated after a lost status update reports the stored one.
	decided.Status.Experiment.Result = corev1alpha1.EvalExperimentPending
	decided.Status.Experiment.DecidedAt = nil
	if err := env.client.Status().Update(context.Background(), decided); err != nil {
		t.Fatal(err)
	}
	env.reconciler.Now = func() time.Time { return env.now.Add(time.Hour) }
	env.reconcileRun("candidate")
	if again := env.getRun("candidate").Status.Experiment; again.Result != corev1alpha1.EvalExperimentPassed || !again.DecidedAt.Time.Equal(outcome.DecidedAt) {
		t.Fatalf("repeated decision = %+v", again)
	}

	for _, name := range []string{"candidate", "baseline"} {
		run := env.getRun(name)
		run.Finalizers = nil
		if err := env.client.Update(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if err := env.client.Delete(context.Background(), run); err != nil {
			t.Fatal(err)
		}
	}
	if kept := env.experimentEntry().Outcome; kept == nil || *kept.Binding != binding {
		t.Fatalf("outcome after deletion = %+v", kept)
	}
}

// checkOutcomeEvidence checks the target and both arms of a compared outcome.
func checkOutcomeEvidence(t *testing.T, outcome *store.EvalExperimentOutcome, decided *corev1alpha1.EvalRun) {
	t.Helper()
	if outcome.Target.Grader != "verdict" || outcome.Target.Tag != "defect" ||
		outcome.Candidate.CaseSetDigest != decided.Status.Snapshot.CaseSetDigest || outcome.Baseline.GraderSetDigest == "" ||
		outcome.Baseline.TrialPlanDigest == "" || outcome.Baseline.MonitorUID != "monitor-uid" {
		t.Fatalf("outcome target and snapshots = %+v / %+v / %+v", outcome.Target, outcome.Candidate, outcome.Baseline)
	}
	if outcome.Candidate.Agent.Name != "candidate-agent" || len(outcome.Candidate.Trials) != 9 || len(outcome.Baseline.Trials) != 9 ||
		len(outcome.Candidate.Trials[0].Grades) == 0 || outcome.Baseline.RunUID != "baseline-uid" {
		t.Fatalf("arms = %+v / %+v", outcome.Candidate, outcome.Baseline)
	}
}

// The live binding matches the outcome until a bound element changes.
func TestCurrentEvalDecisionBindingTracksTheLiveConfiguration(t *testing.T) {
	baseline, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", "needs_changes")
	env.reconcileRun("candidate")
	recorded := *env.experimentEntry().Outcome.Binding
	current, err := env.reconciler.CurrentEvalDecisionBinding(context.Background(), env.getRun("candidate"))
	if err != nil || *current != recorded {
		t.Fatalf("current binding = %+v, %v; recorded %+v", current, err, recorded)
	}

	// Cases moved out of the held-out split cannot back an approval.
	gate := &corev1.ConfigMap{}
	gateKey := types.NamespacedName{Namespace: "orka-system", Name: "protected-set"}
	if err := env.client.Get(context.Background(), gateKey, gate); err != nil {
		t.Fatal(err)
	}
	original := gate.Data[evals.ProtectedSetConfigMapKey]
	gate.Data[evals.ProtectedSetConfigMapKey] = strings.Replace(original, "split: heldOut", "split: train", 1)
	if err := env.client.Update(context.Background(), gate); err != nil {
		t.Fatal(err)
	}
	if _, err := env.reconciler.CurrentEvalDecisionBinding(context.Background(), env.getRun("candidate")); err == nil ||
		!strings.Contains(err.Error(), "no longer listed with split: heldOut") {
		t.Fatalf("binding with training cases: %v", err)
	}
	gate.Data[evals.ProtectedSetConfigMapKey] = original
	if err := env.client.Update(context.Background(), gate); err != nil {
		t.Fatal(err)
	}

	for name, edit := range map[string]func(*corev1alpha1.Agent){
		"candidate":       func(a *corev1alpha1.Agent) { a.Spec.Model.Name = "claude-opus" },
		"stable reviewer": func(a *corev1alpha1.Agent) { a.Spec.Model.Name = "claude-opus" },
	} {
		t.Run(name, func(t *testing.T) {
			agentName := "candidate-agent"
			if name == "stable reviewer" {
				agentName = "reviewer"
			}
			agent := &corev1alpha1.Agent{}
			key := types.NamespacedName{Namespace: evalTestNamespace, Name: agentName}
			if err := env.client.Get(context.Background(), key, agent); err != nil {
				t.Fatal(err)
			}
			original := agent.DeepCopy()
			edit(agent)
			if err := env.client.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			current, err := env.reconciler.CurrentEvalDecisionBinding(context.Background(), env.getRun("candidate"))
			if err != nil || *current == recorded {
				t.Fatalf("binding after editing %s = %+v, %v", agentName, current, err)
			}
			if err := env.client.Get(context.Background(), key, agent); err != nil {
				t.Fatal(err)
			}
			agent.Spec = original.Spec
			if err := env.client.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Token totals stay unset until a trial reports a count, so an arm whose
// runtime reports none is not mistaken for one that used none.
func TestAddEvalTokens(t *testing.T) {
	five, seven := int64(5), int64(7)
	if got := addEvalTokens(nil, nil); got != nil {
		t.Fatalf("unreported = %v", *got)
	}
	total := addEvalTokens(nil, &five)
	total = addEvalTokens(total, nil)
	total = addEvalTokens(total, &seven)
	if total == nil || *total != 12 {
		t.Fatalf("total = %v", total)
	}
}

// A candidate that did not succeed is Incomparable, and its outcome still
// keeps the finished baseline's evidence.
func TestEvalExperimentOutcomeKeepsTheBaselineWhenTheCandidateFails(t *testing.T) {
	baseline, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, baseline, candidate, evalTestGate)
	env.runToEnd("baseline", repositoryMonitorReviewVerdictPassed)
	env.runToEnd("candidate", "needs_changes")
	run := env.getRun("candidate")
	run.Status.Phase = corev1alpha1.EvalRunPhaseFailed
	if err := env.client.Status().Update(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	env.reconcileRun("candidate")
	outcome := env.experimentEntry().Outcome
	if outcome == nil || outcome.Result != store.EvalExperimentResultIncomparable || outcome.Binding != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Baseline.Agent.Name != "reviewer" || len(outcome.Baseline.Trials) != 9 || outcome.Baseline.CaseSetDigest == "" {
		t.Fatalf("baseline arm = %+v", outcome.Baseline)
	}
}
