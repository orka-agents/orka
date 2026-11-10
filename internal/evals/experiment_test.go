package evals

import (
	"math"
	"slices"
	"strings"
	"testing"
)

const testGateDeclaration = `schemaVersion: orka.protectedSet.v1
paths: [".github/**"]
gate:
  maxInfraErrorPercent: 10
  maxCandidatesPerBaseline: 3
  maxExperimentsPerHeldOutSet: 10
  minTargetImprovementPoints: 5
  significance: 0.05
`

func TestParseExperimentGate(t *testing.T) {
	gate, err := ParseExperimentGate([]byte(testGateDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	if gate != (ExperimentGate{10, 3, 10, 5, 0.05, 0}) {
		t.Fatalf("gate = %+v", gate)
	}
	if level := gate.TestLevel(); math.Abs(level-0.005) > 1e-12 {
		t.Fatalf("test level = %v", level)
	}
	first, err := gate.Digest()
	if err != nil {
		t.Fatal(err)
	}
	gate.MinTargetImprovementPoints = 6
	if second, _ := gate.Digest(); second == first {
		t.Fatal("the gate digest ignores a threshold")
	}

	for name, declaration := range map[string]string{
		"wrong schema":       strings.Replace(testGateDeclaration, "orka.protectedSet.v1", "orka.protectedSet.v2", 1),
		"no gate":            "schemaVersion: orka.protectedSet.v1\npaths: [a]\n",
		"proposer-only gate": "schemaVersion: orka.protectedSet.v1\ngate: {maxInfraErrorPercent: 10, maxCandidatesPerBaseline: 3}\n",
		"unknown gate field": strings.Replace(testGateDeclaration, "significance: 0.05", "significance: 0.05\n  extra: 1", 1),
		"null threshold":     strings.Replace(testGateDeclaration, "significance: 0.05", "significance: null", 1),
		"zero significance":  strings.Replace(testGateDeclaration, "significance: 0.05", "significance: 0", 1),
		"no experiments":     strings.Replace(testGateDeclaration, "maxExperimentsPerHeldOutSet: 10", "maxExperimentsPerHeldOutSet: 0", 1),
		"points too large":   strings.Replace(testGateDeclaration, "minTargetImprovementPoints: 5", "minTargetImprovementPoints: 101", 1),
		"negative guardrail tolerance": strings.Replace(testGateDeclaration, "significance: 0.05",
			"significance: 0.05\n  maxGuardrailDropPoints: -1", 1),
		"not yaml": "{",
	} {
		if _, err := ParseExperimentGate([]byte(declaration)); err == nil {
			t.Errorf("%s: ParseExperimentGate accepted an invalid gate", name)
		}
	}
}

func TestSignTestPValue(t *testing.T) {
	for _, test := range []struct {
		improved, regressed int
		want                float64
	}{
		{0, 0, 1},
		{0, 3, 1},
		{5, 0, 1.0 / 32},
		{8, 2, 56.0 / 1024},
		{1, 1, 0.75},
		{60, 0, math.Pow(2, -60)},
	} {
		got := SignTestPValue(test.improved, test.regressed)
		if math.Abs(got-test.want) > 1e-12*math.Max(1, test.want) && math.Abs(got-test.want)/test.want > 1e-9 {
			t.Errorf("SignTestPValue(%d, %d) = %v, want %v", test.improved, test.regressed, got, test.want)
		}
	}
}

// armTrials builds an arm whose trials pass grader "verdict" for the cases
// listed in passing and fail it otherwise; "contract" always passes.
func armTrials(caseIDs []string, trials int32, passing map[string]bool) []ArmTrial {
	var out []ArmTrial
	for _, id := range caseIDs {
		for trial := int32(1); trial <= trials; trial++ {
			verdict := GradeFail
			if passing[id] {
				verdict = GradePass
			}
			out = append(out, ArmTrial{CaseID: id, Trial: trial, Grades: map[string]GradeStatus{"verdict": verdict, "contract": GradePass}})
		}
	}
	return out
}

func experimentInput(caseIDs []string, baselinePassing, candidatePassing map[string]bool) ExperimentInput {
	gate := ExperimentGate{MaxInfraErrorPercent: 10, MaxCandidatesPerBaseline: 3, MaxExperimentsPerHeldOutSet: 1,
		MinTargetImprovementPoints: 5, Significance: 0.05}
	comparable := map[string]string{"caseSetDigest": "sha256:cases", "trialPlanDigest": "sha256:plan"}
	tags := map[string][]string{}
	for _, id := range caseIDs {
		tags[id] = []string{"defect"}
	}
	return ExperimentInput{
		Gate: gate, Grader: "verdict", Tag: "defect", Graders: []string{"verdict", "contract"}, CaseTags: tags,
		Baseline: ExperimentArm{Comparable: comparable, Planned: len(caseIDs) * 2,
			Trials: armTrials(caseIDs, 2, baselinePassing)},
		Candidate: ExperimentArm{Comparable: map[string]string{"caseSetDigest": "sha256:cases", "trialPlanDigest": "sha256:plan"},
			Planned: len(caseIDs) * 2, Trials: armTrials(caseIDs, 2, candidatePassing)},
	}
}

func cases(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "case-" + string(rune('a'+i))
	}
	return ids
}

func TestDecideExperimentPassesASignificantImprovement(t *testing.T) {
	ids := cases(4)
	in := experimentInput(ids, map[string]bool{}, map[string]bool{"case-a": true, "case-b": true, "case-c": true})
	verdict := DecideExperiment(in)
	if verdict.Result != ExperimentPassed {
		t.Fatalf("verdict = %+v", verdict)
	}
	// Six paired trials improved, none regressed: p = 2^-6.
	if verdict.Target.Improved != 6 || verdict.Target.Regressed != 0 || verdict.Target.PValue != 1.0/64 ||
		verdict.Target.ImprovementPoints != 75 {
		t.Fatalf("target = %+v", verdict.Target)
	}
}

func TestDecideExperimentFailures(t *testing.T) {
	ids := cases(4)
	improved := map[string]bool{"case-a": true, "case-b": true, "case-c": true}
	for name, test := range map[string]struct {
		mutate func(*ExperimentInput)
		want   string
	}{
		"too small": {func(in *ExperimentInput) { in.Gate.MinTargetImprovementPoints = 80 }, "less than the gate's 80"},
		"not significant": {func(in *ExperimentInput) { in.Gate.MaxExperimentsPerHeldOutSet = 10 },
			"above the per-experiment level 0.005"},
		"regression": {func(in *ExperimentInput) {
			for i := range in.Candidate.Trials {
				in.Candidate.Trials[i].Grades["contract"] = GradeFail
			}
		}, "contract regressed from 8/8 to 0/8"},
		"infra budget": {func(in *ExperimentInput) { in.Candidate.InfraError = 1 }, "the candidate run had 1 infra errors"},
		// A candidate review the target grader cannot judge counts as a
		// failure, not as a trial left out of the comparison.
		"ungraded candidate target": {func(in *ExperimentInput) {
			for i := range in.Candidate.Trials {
				if in.Candidate.Trials[i].CaseID != "case-a" {
					in.Candidate.Trials[i].Grades["verdict"] = GradeNotApplicable
				}
			}
		}, "the paired sign test p-value 0.25"},
		// A guardrail the candidate produced no grades for counts as failed.
		"guardrail without candidate results": {func(in *ExperimentInput) {
			for i := range in.Candidate.Trials {
				delete(in.Candidate.Trials[i].Grades, "contract")
			}
		}, "contract regressed from 8/8 to 0/8"},
		"no target trials": {func(in *ExperimentInput) { in.Tag = "clean" }, "no graded trials"},
	} {
		t.Run(name, func(t *testing.T) {
			in := experimentInput(ids, map[string]bool{}, improved)
			test.mutate(&in)
			verdict := DecideExperiment(in)
			if verdict.Result != ExperimentFailed || !strings.Contains(strings.Join(verdict.Reasons, "; "), test.want) {
				t.Fatalf("verdict = %+v, want a failure containing %q", verdict, test.want)
			}
		})
	}
}

func TestDecideExperimentIncomparable(t *testing.T) {
	in := experimentInput(cases(2), map[string]bool{}, map[string]bool{"case-a": true})
	in.Candidate.Comparable["caseSetDigest"] = "sha256:other"
	in.Candidate.Comparable["runtimeImageDigest"] = "sha256:image"
	verdict := DecideExperiment(in)
	if verdict.Result != ExperimentIncomparable || len(verdict.Reasons) != 2 {
		t.Fatalf("verdict = %+v", verdict)
	}
}

// An improvement exactly at the threshold passes: 2/10 to 7/10 is 50
// points, although the float difference is just below 50.
func TestDecideExperimentThresholdIsExact(t *testing.T) {
	ids := cases(10)
	baseline := map[string]bool{"case-a": true, "case-b": true}
	candidate := map[string]bool{"case-a": true, "case-b": true, "case-c": true, "case-d": true, "case-e": true,
		"case-f": true, "case-g": true}
	in := experimentInput(ids, baseline, candidate)
	in.Gate.MinTargetImprovementPoints = 50
	if verdict := DecideExperiment(in); verdict.Result != ExperimentPassed {
		t.Fatalf("verdict = %+v", verdict)
	}
	in.Gate.MinTargetImprovementPoints = 51
	if verdict := DecideExperiment(in); verdict.Result != ExperimentFailed {
		t.Fatalf("verdict above the threshold = %+v", verdict)
	}
}

func TestFormatDecimalFitsTheStatusField(t *testing.T) {
	for value, want := range map[float64]string{0.05: "0.05", 100: "100", 1e-40: "1e-40", 0.005: "0.005"} {
		if got := FormatDecimal(value); got != want || len(got) > 32 {
			t.Errorf("FormatDecimal(%v) = %q, want %q", value, got, want)
		}
	}
	if got := FormatDecimal(math.SmallestNonzeroFloat64); len(got) > 32 {
		t.Errorf("FormatDecimal(smallest) = %q is longer than the status field", got)
	}
}

// Pairs that are not graded on both sides, such as an infra error, do not
// count toward the sign test.
func TestDecideExperimentPairsOnlyGradedTrials(t *testing.T) {
	ids := cases(2)
	in := experimentInput(ids, map[string]bool{}, map[string]bool{"case-a": true, "case-b": true})
	in.Baseline.Trials[0].Infra = true
	in.Baseline.InfraError = 0
	verdict := DecideExperiment(in)
	if verdict.Target.Improved != 3 {
		t.Fatalf("target = %+v", verdict.Target)
	}
}

// An infra error in one arm drops that pair from the pass@1 rise too, so it
// cannot raise the other arm's rate on its own.
func TestDecideExperimentRiseCountsOnlyPairs(t *testing.T) {
	ids := cases(2)
	// Both arms pass case-a and fail case-b, so there is no real change.
	in := experimentInput(ids, map[string]bool{"case-a": true}, map[string]bool{"case-a": true})
	for i := range in.Candidate.Trials {
		if in.Candidate.Trials[i].CaseID == "case-b" {
			in.Candidate.Trials[i].Infra = true
		}
	}
	in.Gate.MaxInfraErrorPercent = 100
	verdict := DecideExperiment(in)
	if verdict.Target.BaselineTrials != 2 || verdict.Target.CandidateTrials != 2 || verdict.Target.ImprovementPoints != 0 {
		t.Fatalf("target = %+v, want only the two case-a pairs", verdict.Target)
	}
	if verdict.Result != ExperimentFailed {
		t.Fatalf("verdict = %+v, want no rise", verdict)
	}
}

// Held-out keys follow the reviewed commits only: reordering, renaming,
// retagging or rewording cases keeps them, and a different commit does not.
func TestHeldOutCaseKeys(t *testing.T) {
	reviewCase := func(id, head, title string, tags ...string) Case {
		return Case{ID: id, Tags: tags, Input: CaseInput{RepositoryMonitorReview: &ReviewCaseInput{
			RepoURL: "https://github.com/acme/fixtures", BaseSHA: "base", HeadSHA: head, Title: title}}}
	}
	first, err := HeldOutCaseKeys([]Case{reviewCase("a", "head-1", "one"), reviewCase("b", "head-2", "two")})
	if err != nil {
		t.Fatal(err)
	}
	relabeled, err := HeldOutCaseKeys([]Case{reviewCase("renamed", "head-2", "reworded", "defect"), reviewCase("x", "head-1", "one"),
		reviewCase("copy", "head-1", "again")})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || !slices.Equal(first, relabeled) || !slices.IsSorted(first) {
		t.Fatalf("keys = %v, relabeled = %v", first, relabeled)
	}
	for _, spelling := range []string{"https://GitHub.com/Acme/Fixtures.git/", "https://github.com:443/acme/fixtures",
		"http://github.com/acme//fixtures.git.git", "https://github.com/%61cme/fixtures"} {
		alias := reviewCase("a", "HEAD-1", "one")
		alias.Input.RepositoryMonitorReview.RepoURL = spelling
		aliased, _ := HeldOutCaseKeys([]Case{alias})
		if !slices.Contains(first, aliased[0]) {
			t.Errorf("repository URL alias %q makes a held-out case fresh", spelling)
		}
	}
	other, _ := HeldOutCaseKeys([]Case{reviewCase("a", "head-3", "one")})
	if slices.Contains(first, other[0]) {
		t.Fatal("a different commit shares a held-out key")
	}
	if _, err := HeldOutCaseKeys([]Case{{ID: "empty"}}); err == nil {
		t.Fatal("a case without input has a held-out key")
	}
}

// A case set listed under two splits is refused rather than taking the last.
func TestCaseSetSplitsRejectsConflictingSplits(t *testing.T) {
	conflicting := "caseSets:\n- {namespace: team-a, name: cases, split: heldOut}\n- {namespace: team-a, name: cases, split: train}\n"
	if _, err := CaseSetSplits([]byte(conflicting)); err == nil || !strings.Contains(err.Error(), "both heldOut and train") {
		t.Fatalf("CaseSetSplits() error = %v, want a conflict", err)
	}
	repeated := "caseSets:\n- {namespace: team-a, name: cases, split: heldOut}\n- {namespace: team-a, name: cases, split: heldOut}\n"
	splits, err := CaseSetSplits([]byte(repeated))
	if err != nil || splits["team-a/cases"] != SplitHeldOut {
		t.Fatalf("CaseSetSplits() = %v, %v", splits, err)
	}
}

// A fall reads as a change, rounded like the status field.
func TestDecideExperimentReportsARoundedChange(t *testing.T) {
	ids := cases(3)
	in := experimentInput(ids, map[string]bool{"case-a": true, "case-b": true}, map[string]bool{"case-a": true})
	verdict := DecideExperiment(in)
	reasons := strings.Join(verdict.Reasons, "; ")
	if !strings.Contains(reasons, "the target pass@1 changed by -33.3333 points") {
		t.Fatalf("reasons = %q", reasons)
	}
}

// A guardrail drop within the gate's tolerance passes; a larger one fails.
// Without a tolerance, any drop fails.
func TestDecideExperimentGuardrailTolerance(t *testing.T) {
	ids := cases(4)
	improved := map[string]bool{"case-a": true, "case-b": true, "case-c": true}
	// One of eight candidate trials fails the contract: a 12.5-point drop.
	oneBadTrial := func() ExperimentInput {
		in := experimentInput(ids, map[string]bool{}, improved)
		in.Candidate.Trials[0].Grades["contract"] = GradeFail
		return in
	}
	in := oneBadTrial()
	if verdict := DecideExperiment(in); verdict.Result != ExperimentFailed ||
		!strings.Contains(strings.Join(verdict.Reasons, "; "), "contract regressed from 8/8 to 7/8") {
		t.Fatalf("strict gate verdict = %+v", verdict)
	}
	in = oneBadTrial()
	in.Gate.MaxGuardrailDropPoints = 13
	if verdict := DecideExperiment(in); verdict.Result != ExperimentPassed {
		t.Fatalf("tolerant gate verdict = %+v", verdict)
	}
	in = oneBadTrial()
	in.Gate.MaxGuardrailDropPoints = 12
	if verdict := DecideExperiment(in); verdict.Result != ExperimentFailed ||
		!strings.Contains(strings.Join(verdict.Reasons, "; "), "more than the gate's 12 points") {
		t.Fatalf("too-small tolerance verdict = %+v", verdict)
	}
}

func TestParseExperimentGateGuardrailTolerance(t *testing.T) {
	gate, err := ParseExperimentGate([]byte(strings.Replace(testGateDeclaration, "significance: 0.05",
		"significance: 0.05\n  maxGuardrailDropPoints: 5", 1)))
	if err != nil || gate.MaxGuardrailDropPoints != 5 {
		t.Fatalf("gate = %+v, %v", gate, err)
	}
	strict, err := ParseExperimentGate([]byte(testGateDeclaration))
	if err != nil {
		t.Fatal(err)
	}
	strictDigest, _ := strict.Digest()
	tolerantDigest, _ := gate.Digest()
	if strictDigest == tolerantDigest {
		t.Fatal("the gate digest ignores the guardrail tolerance")
	}
}
