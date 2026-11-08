package evals

import (
	"math"
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
	if gate != (ExperimentGate{10, 3, 10, 5, 0.05}) {
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
		"not yaml":           "{",
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
		"infra budget":     {func(in *ExperimentInput) { in.Candidate.InfraError = 1 }, "the candidate run had 1 infra errors"},
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
