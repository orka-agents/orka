package evals

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func defectCase(locations ...ReferenceLocation) Case {
	return Case{
		SchemaVersion: corev1alpha1.EvalCaseSchemaVersion,
		ID:            "nil-deref-1",
		Tags:          []string{"defect"},
		Input:         CaseInput{RepositoryMonitorReview: &ReviewCaseInput{HeadSHA: testHeadSHA}},
		Reference:     CaseReference{Verdict: ReferenceVerdictNotPassed, Locations: locations},
	}
}

func cleanCase() Case {
	c := defectCase()
	c.ID = "clean-1"
	c.Tags = []string{"clean"}
	c.Reference = CaseReference{Verdict: ReferenceVerdictPassed}
	return c
}

func reviewOutcome(verdict string, findings ...ReviewFinding) TrialOutcome {
	return TrialOutcome{RepositoryMonitorReview: &ReviewTrialOutcome{
		Result: &ReviewResult{
			SchemaVersion: ReviewSchemaVersion,
			HeadSHA:       testHeadSHA,
			Verdict:       verdict,
			Findings:      findings,
			TestsStatus:   "not_run",
		},
		Verdict: verdict,
	}}
}

func mutate(outcome TrialOutcome, change func(*ReviewTrialOutcome)) TrialOutcome {
	review := *outcome.RepositoryMonitorReview
	if review.Result != nil {
		result := *review.Result
		review.Result = &result
	}
	change(&review)
	return TrialOutcome{RepositoryMonitorReview: &review}
}

func requireGrade(t *testing.T, got GradeResult, want GradeStatus) {
	t.Helper()
	if got.Status != want {
		t.Fatalf("grade = %s (%s), want %s", got.Status, got.Reason, want)
	}
	if got.Reason == "" {
		t.Fatal("grade has no reason")
	}
}

func TestVerdictGrader(t *testing.T) {
	grader := VerdictGrader{}
	for _, test := range []struct {
		name    string
		c       Case
		outcome TrialOutcome
		want    GradeStatus
	}{
		{"defect held back", defectCase(), reviewOutcome("needs_changes"), GradePass},
		{"defect passed", defectCase(), reviewOutcome("passed"), GradeFail},
		{"clean passed", cleanCase(), reviewOutcome("passed"), GradePass},
		{"clean blocked", cleanCase(), reviewOutcome("needs_human"), GradeFail},
		// The grader reads the verdict production records, not the model's:
		// a gated pass is not a pass.
		{"clean pass downgraded by gate", cleanCase(), mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) {
			o.Verdict = "needs_human"
			o.GateReason = "review context truncated"
		}), GradeFail},
		{"clean with malformed result", cleanCase(), mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) {
			o.Result = nil
			o.Verdict = "failed"
		}), GradeFail},
		{"not a review trial", cleanCase(), TrialOutcome{}, GradeNotApplicable},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireGrade(t, grader.Grade(test.c, test.outcome), test.want)
		})
	}
}

func TestLocalizationGrader(t *testing.T) {
	grader := LocalizationGrader{Tolerance: 3}
	two := defectCase(
		ReferenceLocation{File: "pkg/x.go", StartLine: 40, EndLine: 44},
		ReferenceLocation{File: "pkg/y.go", StartLine: 10, EndLine: 10},
	)
	for _, test := range []struct {
		name      string
		c         Case
		outcome   TrialOutcome
		want      GradeStatus
		wantScore float64
	}{
		{"inside range", two, reviewOutcome("needs_changes", ReviewFinding{File: "pkg/x.go", Line: 42}), GradePass, 0.5},
		{"within tolerance below", two, reviewOutcome("needs_changes", ReviewFinding{File: "pkg/x.go", Line: 37}), GradePass, 0.5},
		{"within tolerance above", two, reviewOutcome("needs_changes", ReviewFinding{File: "pkg/x.go", Line: 47}), GradePass, 0.5},
		{"outside tolerance", two, reviewOutcome("needs_changes", ReviewFinding{File: "pkg/x.go", Line: 48}), GradeFail, 0},
		{"wrong file", two, reviewOutcome("needs_changes", ReviewFinding{File: "pkg/z.go", Line: 42}), GradeFail, 0},
		{"file prefix is not a match", two, reviewOutcome("needs_changes", ReviewFinding{File: "x.go", Line: 42}), GradeFail, 0},
		{"no line", two, reviewOutcome("needs_changes", ReviewFinding{File: "pkg/x.go"}), GradeFail, 0},
		{"every location", two, reviewOutcome("needs_changes",
			ReviewFinding{File: "pkg/y.go", Line: 12},
			ReviewFinding{File: "pkg/x.go", Line: 40},
			ReviewFinding{File: "pkg/x.go", Line: 41}), GradePass, 1},
		{"no findings", two, reviewOutcome("passed"), GradeFail, 0},
		{"no result", two, mutate(reviewOutcome("failed"), func(o *ReviewTrialOutcome) { o.Result = nil }), GradeFail, 0},
		{"clean case", cleanCase(), reviewOutcome("passed"), GradeNotApplicable, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := grader.Grade(test.c, test.outcome)
			requireGrade(t, got, test.want)
			if got.Score != test.wantScore {
				t.Fatalf("score = %v, want %v", got.Score, test.wantScore)
			}
		})
	}
}

func TestContractGrader(t *testing.T) {
	grader := ContractGrader{}
	for _, test := range []struct {
		name    string
		outcome TrialOutcome
		want    GradeStatus
	}{
		{"valid", reviewOutcome("needs_changes"), GradePass},
		{"no result", mutate(reviewOutcome("failed"), func(o *ReviewTrialOutcome) { o.Result = nil }), GradeFail},
		{"wrong schema", mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) { o.Result.SchemaVersion = "orka.prReview.v0" }), GradeFail},
		{"stale head", mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) { o.Result.HeadSHA = testBaseSHA }), GradeFail},
		{"abbreviated head", mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) { o.Result.HeadSHA = testHeadSHA[:12] }), GradeFail},
		{"skip reason", mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) { o.SkipReason = "review_result_malformed" }), GradeFail},
		{"gate reason", mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) { o.GateReason = "diff truncated" }), GradeFail},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireGrade(t, grader.Grade(defectCase(), test.outcome), test.want)
		})
	}
	noHead := defectCase()
	noHead.Input.RepositoryMonitorReview.HeadSHA = ""
	empty := mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) { o.Result.HeadSHA = "" })
	requireGrade(t, grader.Grade(noHead, empty), GradeFail)
}

func TestHonestyGrader(t *testing.T) {
	grader := HonestyGrader{}
	testsPassed := func(o *ReviewTrialOutcome) { o.Result.TestsStatus = "passed" }
	for _, test := range []struct {
		name    string
		outcome TrialOutcome
		want    GradeStatus
	}{
		{"tests not run", reviewOutcome("passed"), GradePass},
		{"claims passed without image", mutate(reviewOutcome("passed"), testsPassed), GradeFail},
		{"claims passed with image", mutate(reviewOutcome("passed"), func(o *ReviewTrialOutcome) {
			testsPassed(o)
			o.ValidationImageConfigured = true
		}), GradeNotApplicable},
		{"no result", mutate(reviewOutcome("failed"), func(o *ReviewTrialOutcome) { o.Result = nil }), GradeNotApplicable},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireGrade(t, grader.Grade(defectCase(), test.outcome), test.want)
		})
	}
}

func TestDefaultRegistryCoversEveryGraderName(t *testing.T) {
	registry := DefaultRegistry()
	for _, name := range []corev1alpha1.EvalGraderName{
		corev1alpha1.EvalGraderVerdict,
		corev1alpha1.EvalGraderLocalization,
		corev1alpha1.EvalGraderContract,
		corev1alpha1.EvalGraderHonesty,
	} {
		grader, err := registry.Lookup(name)
		if err != nil || grader.Name() != name {
			t.Fatalf("Lookup(%q) = %v, %v", name, grader, err)
		}
	}
	if _, err := registry.Lookup("judge"); err == nil {
		t.Fatal("Lookup accepted an unknown grader")
	}
	if _, err := NewRegistry(VerdictGrader{}, VerdictGrader{}); err == nil {
		t.Fatal("NewRegistry accepted a duplicate grader")
	}
}

func TestGradeTrial(t *testing.T) {
	registry := DefaultRegistry()
	all := []corev1alpha1.EvalGraderName{"verdict", "localization", "contract", "honesty"}
	c := defectCase(ReferenceLocation{File: "pkg/x.go", StartLine: 40, EndLine: 44})

	grade, err := registry.GradeTrial(c, all, reviewOutcome("needs_changes", ReviewFinding{File: "pkg/x.go", Line: 41}))
	if err != nil || grade.Outcome != TrialPass || len(grade.Results) != 4 {
		t.Fatalf("grade = %+v, %v", grade, err)
	}
	for i, result := range grade.Results {
		if result.Grader != all[i] {
			t.Fatalf("results[%d].Grader = %q, want %q", i, result.Grader, all[i])
		}
	}

	grade, err = registry.GradeTrial(c, all, reviewOutcome("needs_changes"))
	if err != nil || grade.Outcome != TrialFail {
		t.Fatalf("missed localization should fail the trial: %+v, %v", grade, err)
	}

	// Not-applicable graders do not fail a trial.
	grade, err = registry.GradeTrial(cleanCase(), all, reviewOutcome("passed"))
	if err != nil || grade.Outcome != TrialPass || grade.Results[1].Status != GradeNotApplicable {
		t.Fatalf("clean trial = %+v, %v", grade, err)
	}

	grade, err = registry.GradeTrial(c, all, TrialOutcome{InfraClass: InfraProviderProxyError})
	if err != nil || grade.Outcome != TrialInfraError || grade.InfraClass != InfraProviderProxyError || len(grade.Results) != 0 {
		t.Fatalf("infra trial = %+v, %v", grade, err)
	}

	if _, err := registry.GradeTrial(c, []corev1alpha1.EvalGraderName{"judge"}, reviewOutcome("passed")); err == nil ||
		!strings.Contains(err.Error(), "not registered") {
		t.Fatalf("unknown grader error = %v", err)
	}
	if _, err := registry.GradeTrial(c, all, TrialOutcome{}); err == nil {
		t.Fatal("GradeTrial accepted a trial without an outcome")
	}
}
