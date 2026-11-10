package controller

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/store"
)

func TestAggregateEvalRunPassAtKAndPassHatK(t *testing.T) {
	cases := []evals.Case{{ID: "a", Tags: []string{"defect"}}, {ID: "b", Tags: []string{"defect"}}, {ID: "c", Tags: []string{"clean"}}}
	record := func(caseID string, trial int32, outcome string, grades ...string) store.EvalTrialRecord {
		r := store.EvalTrialRecord{CaseID: caseID, Trial: trial, Outcome: outcome}
		for _, status := range grades {
			r.Grades = append(r.Grades, store.EvalTrialGrade{Grader: string(corev1alpha1.EvalGraderVerdict), Status: status})
		}
		if outcome == store.EvalTrialOutcomeInfraError {
			r.InfraClass = string(evals.InfraRuntimeLost)
		}
		return r
	}
	records := []store.EvalTrialRecord{
		// a: every graded trial passes; the infra error is excluded.
		record("a", 1, store.EvalTrialOutcomePass, "pass"),
		record("a", 2, store.EvalTrialOutcomeInfraError),
		record("a", 3, store.EvalTrialOutcomePass, "pass"),
		// b: one of three passes.
		record("b", 1, store.EvalTrialOutcomeFail, "fail"),
		record("b", 2, store.EvalTrialOutcomePass, "pass"),
		record("b", 3, store.EvalTrialOutcomeFail, "fail"),
		// c: never passes, so it is suspect.
		record("c", 1, store.EvalTrialOutcomeFail, "fail"),
		record("c", 2, store.EvalTrialOutcomeFail, "fail"),
		record("c", 3, store.EvalTrialOutcomeFail, "fail"),
	}
	got := aggregateEvalRun(cases, 3, []corev1alpha1.EvalGraderName{corev1alpha1.EvalGraderVerdict}, records)
	wantCounts := corev1alpha1.EvalRunCounts{Planned: 9, Completed: 9, Passed: 3, Failed: 5, InfraError: 1}
	if got.Counts != wantCounts {
		t.Fatalf("counts = %+v, want %+v", got.Counts, wantCounts)
	}
	want := []corev1alpha1.EvalGraderResult{
		{Grader: corev1alpha1.EvalGraderVerdict, Trials: 8, PassedTrials: 3, Cases: 3, CasesAnyPassed: 2, CasesAllPassed: 1},
		{Grader: corev1alpha1.EvalGraderVerdict, Tag: "clean", Trials: 3, PassedTrials: 0, Cases: 1},
		{Grader: corev1alpha1.EvalGraderVerdict, Tag: "defect", Trials: 5, PassedTrials: 3, Cases: 2, CasesAnyPassed: 2, CasesAllPassed: 1},
	}
	if len(got.Results) != len(want) {
		t.Fatalf("results = %+v", got.Results)
	}
	for i := range want {
		if got.Results[i] != want[i] {
			t.Fatalf("results[%d] = %+v, want %+v", i, got.Results[i], want[i])
		}
	}
	if len(got.SuspectCases) != 1 || got.SuspectCases[0] != "c" {
		t.Fatalf("suspect cases = %v", got.SuspectCases)
	}
}

func TestAggregateEvalRunNotApplicableAndUnfinishedCases(t *testing.T) {
	cases := []evals.Case{{ID: "a"}, {ID: "b"}}
	records := []store.EvalTrialRecord{
		{CaseID: "a", Trial: 1, Outcome: store.EvalTrialOutcomePass, Grades: []store.EvalTrialGrade{
			{Grader: string(corev1alpha1.EvalGraderLocalization), Status: string(evals.GradeNotApplicable)},
		}},
		// b has one of two trials: not yet suspect.
		{CaseID: "b", Trial: 1, Outcome: store.EvalTrialOutcomeFail, Grades: []store.EvalTrialGrade{
			{Grader: string(corev1alpha1.EvalGraderLocalization), Status: string(evals.GradeFail)},
		}},
	}
	got := aggregateEvalRun(cases, 2, []corev1alpha1.EvalGraderName{corev1alpha1.EvalGraderLocalization}, records)
	if r := got.Results[0]; r.Trials != 1 || r.Cases != 1 || r.PassedTrials != 0 {
		t.Fatalf("a not-applicable grade was counted: %+v", r)
	}
	if len(got.SuspectCases) != 0 {
		t.Fatalf("an unfinished case is suspect: %v", got.SuspectCases)
	}
}
