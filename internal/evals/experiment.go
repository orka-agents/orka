package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"

	sigsyaml "sigs.k8s.io/yaml"
)

// ProtectedSetSchemaVersion and ProtectedSetConfigMapKey name the protected
// set declaration whose gate section freezes experiment thresholds.
const (
	ProtectedSetSchemaVersion = "orka.protectedSet.v1"
	ProtectedSetConfigMapKey  = "protectedSet.yaml"
)

// ExperimentGate is the protected set's gate for improvement experiments.
// Every field is required: a missing threshold fails experiments closed.
type ExperimentGate struct {
	MaxInfraErrorPercent        int32   `json:"maxInfraErrorPercent"`
	MaxCandidatesPerBaseline    int32   `json:"maxCandidatesPerBaseline"`
	MaxExperimentsPerHeldOutSet int32   `json:"maxExperimentsPerHeldOutSet"`
	MinTargetImprovementPoints  int32   `json:"minTargetImprovementPoints"`
	Significance                float64 `json:"significance"`
}

// ParseExperimentGate reads the gate section of an orka.protectedSet.v1
// declaration. Other sections belong to the protected set's own parser and
// are not interpreted here; the gate itself is decoded strictly.
func ParseExperimentGate(declaration []byte) (ExperimentGate, error) {
	var gate ExperimentGate
	var document struct {
		SchemaVersion string          `json:"schemaVersion"`
		Gate          json.RawMessage `json:"gate"`
	}
	raw, err := sigsyaml.YAMLToJSON(declaration)
	if err != nil {
		return gate, fmt.Errorf("decode protected set: %w", err)
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return gate, fmt.Errorf("decode protected set: %w", err)
	}
	if document.SchemaVersion != ProtectedSetSchemaVersion {
		return gate, fmt.Errorf("protected set schemaVersion must be %q", ProtectedSetSchemaVersion)
	}
	if len(document.Gate) == 0 || string(document.Gate) == "null" {
		return gate, errors.New("protected set has no gate")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(document.Gate, &fields); err != nil {
		return gate, fmt.Errorf("decode protected set gate: %w", err)
	}
	for _, name := range []string{"maxInfraErrorPercent", "maxCandidatesPerBaseline", "maxExperimentsPerHeldOutSet",
		"minTargetImprovementPoints", "significance"} {
		if value, ok := fields[name]; !ok || string(value) == "null" {
			return gate, fmt.Errorf("protected set gate.%s is required for experiments", name)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(document.Gate))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&gate); err != nil {
		return gate, fmt.Errorf("decode protected set gate: %w", err)
	}
	switch {
	case gate.MaxInfraErrorPercent < 0 || gate.MaxInfraErrorPercent > 100:
		return gate, errors.New("protected set gate.maxInfraErrorPercent must be between 0 and 100")
	case gate.MaxCandidatesPerBaseline < 1:
		return gate, errors.New("protected set gate.maxCandidatesPerBaseline must be at least 1")
	case gate.MaxExperimentsPerHeldOutSet < 1:
		return gate, errors.New("protected set gate.maxExperimentsPerHeldOutSet must be at least 1")
	case gate.MinTargetImprovementPoints < 0 || gate.MinTargetImprovementPoints > 100:
		return gate, errors.New("protected set gate.minTargetImprovementPoints must be between 0 and 100")
	case !(gate.Significance > 0 && gate.Significance <= 0.5):
		return gate, errors.New("protected set gate.significance must be greater than 0 and at most 0.5")
	}
	return gate, nil
}

// Case set splits the protected set assigns.
const (
	SplitTrain   = "train"
	SplitHeldOut = "heldOut"
)

// CaseSetSplits returns the split of every case ConfigMap the protected set
// lists, keyed namespace/name. Experiments run only on held-out sets, and a
// candidate Agent outside an experiment only on training sets, so which
// cases judge a candidate, and when they rotate, stay operator choices.
func CaseSetSplits(declaration []byte) (map[string]string, error) {
	var document struct {
		CaseSets []struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
			Split     string `json:"split"`
		} `json:"caseSets"`
	}
	raw, err := sigsyaml.YAMLToJSON(declaration)
	if err != nil {
		return nil, fmt.Errorf("decode protected set: %w", err)
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("decode protected set caseSets: %w", err)
	}
	splits := map[string]string{}
	for _, caseSet := range document.CaseSets {
		if (caseSet.Split == SplitTrain || caseSet.Split == SplitHeldOut) && caseSet.Namespace != "" && caseSet.Name != "" {
			key := caseSet.Namespace + "/" + caseSet.Name
			// A set listed twice must not take whichever split comes last:
			// a held-out set also listed as train would admit unpaired
			// candidates.
			if split, listed := splits[key]; listed && split != caseSet.Split {
				return nil, fmt.Errorf("protected set lists case set %s as both %s and %s", key, split, caseSet.Split)
			}
			splits[key] = caseSet.Split
		}
	}
	return splits, nil
}

// Digest digests the gate's thresholds.
func (g ExperimentGate) Digest() (string, error) {
	return digestJSON(struct {
		MaxInfraErrorPercent        int32  `json:"maxInfraErrorPercent"`
		MaxCandidatesPerBaseline    int32  `json:"maxCandidatesPerBaseline"`
		MaxExperimentsPerHeldOutSet int32  `json:"maxExperimentsPerHeldOutSet"`
		MinTargetImprovementPoints  int32  `json:"minTargetImprovementPoints"`
		Significance                string `json:"significance"`
	}{g.MaxInfraErrorPercent, g.MaxCandidatesPerBaseline, g.MaxExperimentsPerHeldOutSet, g.MinTargetImprovementPoints,
		strconv.FormatFloat(g.Significance, 'g', -1, 64)})
}

// TestLevel is the significance a single experiment must meet: the gate's
// level shared across every experiment the held-out case set allows.
func (g ExperimentGate) TestLevel() float64 {
	return g.Significance / float64(g.MaxExperimentsPerHeldOutSet)
}

// SignTestPValue is the one-sided exact sign test for improved pairs out of
// improved plus regressed discordant pairs: the probability of at least that
// many improvements if each discordant pair were a fair coin. With no
// discordant pairs there is no evidence, so it is 1. It sums the binomial
// tail exactly; a run has at most EvalRunMaxCaseTrials trials, so the
// integers stay small.
func SignTestPValue(improved, regressed int) float64 {
	n := improved + regressed
	if n <= 0 || improved <= 0 {
		return 1
	}
	tail := new(big.Int)
	coefficient := big.NewInt(1) // binomial(n, 0)
	for k := 0; k <= n; k++ {
		if k >= improved {
			tail.Add(tail, coefficient)
		}
		// binomial(n, k+1) = binomial(n, k) * (n-k) / (k+1)
		coefficient.Mul(coefficient, big.NewInt(int64(n-k)))
		coefficient.Quo(coefficient, big.NewInt(int64(k+1)))
	}
	p, _ := new(big.Rat).SetFrac(tail, new(big.Int).Lsh(big.NewInt(1), uint(n))).Float64()
	return p
}

// ExperimentArm is what the gate reads of one arm of an experiment.
type ExperimentArm struct {
	// Comparable holds the snapshot fields both arms must share, by name.
	Comparable map[string]string
	Planned    int
	InfraError int
	Trials     []ArmTrial
}

// ArmTrial is one graded or infra-error trial of an arm.
type ArmTrial struct {
	CaseID string
	Trial  int32
	Infra  bool
	// Grades maps a grader name to its status for the trial.
	Grades map[string]GradeStatus
}

// ExperimentInput is everything the gate decides from.
type ExperimentInput struct {
	Gate      ExperimentGate
	Grader    string
	Tag       string
	Graders   []string
	CaseTags  map[string][]string
	Baseline  ExperimentArm
	Candidate ExperimentArm
}

// ExperimentVerdict is the gate's decision.
type ExperimentVerdict struct {
	// Result is Passed, Failed or Incomparable.
	Result  string
	Reasons []string
	Target  *TargetComparison
}

// TargetComparison compares the target result of the two arms.
type TargetComparison struct {
	BaselinePassed, BaselineTrials   int
	CandidatePassed, CandidateTrials int
	Improved, Regressed              int
	ImprovementPoints                float64
	PValue                           float64
}

// Experiment verdicts.
const (
	ExperimentPassed       = "Passed"
	ExperimentFailed       = "Failed"
	ExperimentIncomparable = "Incomparable"
)

// DecideExperiment applies the gate. Arms that differ in a comparable field
// are Incomparable. Otherwise the candidate passes only if neither arm spent
// more than the infra error budget, the target pass@1 rose by at least the
// minimum points with a one-sided exact sign test over paired trials at or
// below the per-experiment level, and no grader's overall pass@1 fell.
func DecideExperiment(in ExperimentInput) ExperimentVerdict {
	var differs []string
	for _, name := range sortedKeys(in.Baseline.Comparable, in.Candidate.Comparable) {
		if in.Baseline.Comparable[name] != in.Candidate.Comparable[name] {
			differs = append(differs, name+" differs between the baseline and candidate runs")
		}
	}
	if len(differs) > 0 {
		return ExperimentVerdict{Result: ExperimentIncomparable, Reasons: differs}
	}

	var failures []string
	for _, arm := range []struct {
		name string
		arm  ExperimentArm
	}{{"baseline", in.Baseline}, {"candidate", in.Candidate}} {
		if arm.arm.InfraError*100 > int(in.Gate.MaxInfraErrorPercent)*arm.arm.Planned {
			failures = append(failures, fmt.Sprintf("the %s run had %d infra errors in %d planned trials, more than the gate's %d%%",
				arm.name, arm.arm.InfraError, arm.arm.Planned, in.Gate.MaxInfraErrorPercent))
		}
	}

	target := compareTarget(in)
	switch {
	case target.BaselineTrials == 0 || target.CandidateTrials == 0:
		failures = append(failures, "the target has no graded trials in one of the runs")
	default:
		// Compare pass counts exactly: 100*(c/ct - b/bt) >= min.
		if 100*(int64(target.CandidatePassed)*int64(target.BaselineTrials)-int64(target.BaselinePassed)*int64(target.CandidateTrials)) <
			int64(in.Gate.MinTargetImprovementPoints)*int64(target.BaselineTrials)*int64(target.CandidateTrials) {
			failures = append(failures, fmt.Sprintf("the target pass@1 changed by %s points, less than the gate's %d",
				FormatDecimal(math.Round(target.ImprovementPoints*1e4)/1e4), in.Gate.MinTargetImprovementPoints))
		}
		if level := in.Gate.TestLevel(); target.PValue > level {
			failures = append(failures, fmt.Sprintf("the paired sign test p-value %s is above the per-experiment level %s",
				FormatDecimal(target.PValue), FormatDecimal(level)))
		}
	}

	for _, grader := range in.Graders {
		baseline, candidate := armGrades(in, grader, "")
		bp, bt := passRate(baseline)
		cp, ct := passRate(candidate)
		switch {
		case bt > 0 && ct == 0:
			// A guardrail the candidate never produced a result for cannot
			// show it did not regress.
			failures = append(failures, fmt.Sprintf("%s has %d graded baseline trials but no graded candidate trials", grader, bt))
		case bt > 0 && cp*bt < bp*ct:
			failures = append(failures, fmt.Sprintf("%s regressed from %d/%d to %d/%d", grader, bp, bt, cp, ct))
		}
	}
	if len(failures) > 0 {
		return ExperimentVerdict{Result: ExperimentFailed, Reasons: failures, Target: &target}
	}
	return ExperimentVerdict{Result: ExperimentPassed, Target: &target}
}

func compareTarget(in ExperimentInput) TargetComparison {
	var target TargetComparison
	baseline, candidate := armGrades(in, in.Grader, in.Tag)
	// Only pairs graded in both arms count, for the rise as for the sign
	// test: an infra error in one arm drops its pair, so it cannot raise
	// that arm's rate by removing a trial the other arm still counts.
	for key := range baseline {
		if _, paired := candidate[key]; !paired {
			delete(baseline, key)
		}
	}
	for key := range candidate {
		if _, paired := baseline[key]; !paired {
			delete(candidate, key)
		}
	}
	target.BaselinePassed, target.BaselineTrials = passRate(baseline)
	target.CandidatePassed, target.CandidateTrials = passRate(candidate)
	if target.BaselineTrials > 0 && target.CandidateTrials > 0 {
		target.ImprovementPoints = 100 * (float64(target.CandidatePassed)/float64(target.CandidateTrials) -
			float64(target.BaselinePassed)/float64(target.BaselineTrials))
	}
	for key, status := range candidate {
		before, paired := baseline[key]
		switch {
		case !paired || before == status:
		case status == GradePass:
			target.Improved++
		default:
			target.Regressed++
		}
	}
	target.PValue = SignTestPValue(target.Improved, target.Regressed)
	return target
}

type trialKey struct {
	caseID string
	trial  int32
}

// armGrades returns each arm's pass or fail per trial for grader on the
// tag's cases. Infra errors are left out of both arms. A candidate trial that
// ran but has no pass or fail where the baseline's same trial does, such as a
// malformed review an honesty grader cannot judge, counts as a failure: a
// candidate must not drop unfavorable trials out of the comparison.
func armGrades(in ExperimentInput, grader, tag string) (baseline, candidate map[trialKey]GradeStatus) {
	baseline = map[trialKey]GradeStatus{}
	for _, trial := range in.Baseline.Trials {
		if status, ok := gradedStatus(trial, grader, tag, in.CaseTags); ok {
			baseline[trialKey{trial.CaseID, trial.Trial}] = status
		}
	}
	candidate = map[trialKey]GradeStatus{}
	for _, trial := range in.Candidate.Trials {
		key := trialKey{trial.CaseID, trial.Trial}
		if status, ok := gradedStatus(trial, grader, tag, in.CaseTags); ok {
			candidate[key] = status
		} else if _, graded := baseline[key]; graded && !trial.Infra {
			candidate[key] = GradeFail
		}
	}
	return baseline, candidate
}

// gradedStatus is a trial's pass or fail for grader on the tag's cases;
// infra errors and notApplicable grades are not graded.
func gradedStatus(trial ArmTrial, grader, tag string, caseTags map[string][]string) (GradeStatus, bool) {
	if trial.Infra || (tag != "" && !hasTag(caseTags[trial.CaseID], tag)) {
		return "", false
	}
	status := trial.Grades[grader]
	return status, status == GradePass || status == GradeFail
}

func passRate(grades map[trialKey]GradeStatus) (passed, graded int) {
	for _, status := range grades {
		graded++
		if status == GradePass {
			passed++
		}
	}
	return passed, graded
}

func hasTag(tags []string, tag string) bool {
	return slices.Contains(tags, tag)
}

func sortedKeys(maps ...map[string]string) []string {
	seen := map[string]struct{}{}
	var keys []string
	for _, m := range maps {
		for key := range m {
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// FormatDecimal renders a gate number for status and reasons, in the
// shortest form that reads back exactly. Very small or large values use an
// exponent, so every accepted gate value fits the status field.
func FormatDecimal(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
