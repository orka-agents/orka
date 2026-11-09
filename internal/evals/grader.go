package evals

import (
	"fmt"
	"strconv"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// GradeStatus is one grader's judgement of one trial.
type GradeStatus string

const (
	GradePass GradeStatus = "pass"
	GradeFail GradeStatus = "fail"
	// GradeNotApplicable means the grader has nothing to judge in this trial,
	// such as localization on a clean case. It counts neither way.
	GradeNotApplicable GradeStatus = "notApplicable"
)

// GradeResult is one grader's result for one trial.
type GradeResult struct {
	Grader corev1alpha1.EvalGraderName
	Status GradeStatus
	// Score is partial credit in [0, 1]; 1 for a pass and 0 for a fail unless
	// the grader awards partial credit.
	Score  float64
	Reason string
}

// Grader checks one trial of one case. Graders are pure: they read only the
// case and the outcome. Version changes whenever Grade can judge the same
// trial differently, so runs graded by different versions are not compared.
type Grader interface {
	Name() corev1alpha1.EvalGraderName
	Version() string
	Grade(c Case, outcome TrialOutcome) GradeResult
}

// LocalizationLineTolerance is how many lines outside a reference range a
// finding may point and still localize the defect.
const LocalizationLineTolerance = 3

// Registry maps grader names to graders.
type Registry struct {
	graders map[corev1alpha1.EvalGraderName]Grader
}

// NewRegistry returns a registry of graders. Names must be unique.
func NewRegistry(graders ...Grader) (*Registry, error) {
	registry := &Registry{graders: make(map[corev1alpha1.EvalGraderName]Grader, len(graders))}
	for _, grader := range graders {
		name := grader.Name()
		if _, dup := registry.graders[name]; dup {
			return nil, fmt.Errorf("grader %q is registered twice", name)
		}
		registry.graders[name] = grader
	}
	return registry, nil
}

// DefaultRegistry returns the built-in graders, one per EvalGraderName value.
func DefaultRegistry() *Registry {
	registry, err := NewRegistry(
		VerdictGrader{},
		LocalizationGrader{Tolerance: LocalizationLineTolerance},
		ContractGrader{},
		HonestyGrader{},
	)
	if err != nil {
		panic(err)
	}
	return registry
}

// Lookup returns the grader for name. Unknown names fail closed.
func (r *Registry) Lookup(name corev1alpha1.EvalGraderName) (Grader, error) {
	grader, ok := r.graders[name]
	if !ok {
		return nil, fmt.Errorf("grader %q is not registered", name)
	}
	return grader, nil
}

// TrialGrade is the graded result of one trial.
type TrialGrade struct {
	Outcome    TrialOutcomeKind
	InfraClass InfraClass
	Results    []GradeResult
}

// GradeTrial applies the named graders to one trial of case c. An infra error
// is not graded. Otherwise the trial passes when no applicable grader fails.
func (r *Registry) GradeTrial(c Case, names []corev1alpha1.EvalGraderName, outcome TrialOutcome) (TrialGrade, error) {
	graders := make([]Grader, 0, len(names))
	for _, name := range names {
		grader, err := r.Lookup(name)
		if err != nil {
			return TrialGrade{}, err
		}
		graders = append(graders, grader)
	}
	if outcome.InfraClass != "" {
		return TrialGrade{Outcome: TrialInfraError, InfraClass: outcome.InfraClass}, nil
	}
	if outcome.RepositoryMonitorReview == nil {
		return TrialGrade{}, fmt.Errorf("case %q trial has no outcome to grade", c.ID)
	}
	grade := TrialGrade{Outcome: TrialPass, Results: make([]GradeResult, 0, len(graders))}
	for _, grader := range graders {
		result := grader.Grade(c, outcome)
		result.Grader = grader.Name()
		if result.Status == GradeFail {
			grade.Outcome = TrialFail
		}
		grade.Results = append(grade.Results, result)
	}
	return grade, nil
}

func pass(reason string) GradeResult {
	return GradeResult{Status: GradePass, Score: 1, Reason: reason}
}

func fail(reason string) GradeResult {
	return GradeResult{Status: GradeFail, Reason: reason}
}

func notApplicable(reason string) GradeResult {
	return GradeResult{Status: GradeNotApplicable, Reason: reason}
}

// reviewTrial returns the review outcome and case input, or false when the
// trial is not a repositoryMonitorReview trial.
func reviewTrial(c Case, outcome TrialOutcome) (*ReviewTrialOutcome, *ReviewCaseInput, bool) {
	if outcome.RepositoryMonitorReview == nil || c.Input.RepositoryMonitorReview == nil {
		return nil, nil, false
	}
	return outcome.RepositoryMonitorReview, c.Input.RepositoryMonitorReview, true
}

const notReviewTrial = "grader applies to repositoryMonitorReview trials only"

// VerdictGrader passes when the recorded verdict matches the reference: a
// defect case must not be passed and a clean case must be.
type VerdictGrader struct{}

func (VerdictGrader) Name() corev1alpha1.EvalGraderName { return corev1alpha1.EvalGraderVerdict }

func (VerdictGrader) Version() string { return "1" }

func (VerdictGrader) Grade(c Case, trial TrialOutcome) GradeResult {
	outcome, _, ok := reviewTrial(c, trial)
	if !ok {
		return notApplicable(notReviewTrial)
	}
	verdict := strings.TrimSpace(outcome.Verdict)
	passed := verdict == ReviewVerdictPassed
	switch c.Reference.Verdict {
	case ReferenceVerdictPassed:
		if passed {
			return pass("clean change was passed")
		}
		return fail(fmt.Sprintf("clean change was not passed: verdict %q", verdict))
	case ReferenceVerdictNotPassed:
		if passed {
			return fail("defective change was passed")
		}
		return pass(fmt.Sprintf("defective change was held back: verdict %q", verdict))
	default:
		return fail(fmt.Sprintf("reference verdict %q is unknown", c.Reference.Verdict))
	}
}

// LocalizationGrader passes when some finding points at a reference location:
// the file matches and the line is within the range ± Tolerance. Score is the
// share of reference locations some finding hit.
//
// Reference lines are in the head version of the file, but a defect made of
// deleted code is naturally cited at the removed line, which only exists in
// the base. A finding whose line is a line the case's patch removes also
// counts at the head position where that removal happened.
type LocalizationGrader struct {
	Tolerance int64
}

func (LocalizationGrader) Name() corev1alpha1.EvalGraderName {
	return corev1alpha1.EvalGraderLocalization
}

// Version covers Tolerance, which changes what counts as a hit. Version 2
// credits findings on removed lines.
func (g LocalizationGrader) Version() string { return fmt.Sprintf("2+tolerance.%d", g.Tolerance) }

func (g LocalizationGrader) Grade(c Case, trial TrialOutcome) GradeResult {
	outcome, input, ok := reviewTrial(c, trial)
	if !ok {
		return notApplicable(notReviewTrial)
	}
	locations := c.Reference.Locations
	if len(locations) == 0 {
		return notApplicable("case has no reference locations")
	}
	if outcome.Result == nil {
		return fail("no review result parsed")
	}
	removed := removedLinesByFile(input.Files)
	within := func(line int64, location ReferenceLocation) bool {
		return line >= location.StartLine-g.Tolerance && line <= location.EndLine+g.Tolerance
	}
	hit := 0
	for _, location := range locations {
		for _, finding := range outcome.Result.Findings {
			// Paths match exactly, as production's changed-lines check
			// compares them: a padded path names a different file.
			if finding.Line <= 0 || finding.File != location.File {
				continue
			}
			headLine, onRemoved := removed[finding.File][finding.Line]
			if within(finding.Line, location) || (onRemoved && within(headLine, location)) {
				hit++
				break
			}
		}
	}
	if hit == 0 {
		return fail(fmt.Sprintf("no finding within %d lines of %d reference locations", g.Tolerance, len(locations)))
	}
	result := pass(fmt.Sprintf("findings hit %d of %d reference locations", hit, len(locations)))
	result.Score = float64(hit) / float64(len(locations))
	return result
}

// ContractGrader passes when the result parses as orka.prReview.v1 for the
// exact expected head and production recorded no gate or skip reason.
type ContractGrader struct{}

func (ContractGrader) Name() corev1alpha1.EvalGraderName { return corev1alpha1.EvalGraderContract }

func (ContractGrader) Version() string { return "1" }

func (ContractGrader) Grade(c Case, trial TrialOutcome) GradeResult {
	outcome, input, ok := reviewTrial(c, trial)
	if !ok {
		return notApplicable(notReviewTrial)
	}
	expectedHead := strings.TrimSpace(input.HeadSHA)
	switch {
	case outcome.Result == nil:
		return fail("no review result parsed")
	case strings.TrimSpace(outcome.Result.SchemaVersion) != ReviewSchemaVersion:
		return fail(fmt.Sprintf("schemaVersion %q is not %q", outcome.Result.SchemaVersion, ReviewSchemaVersion))
	case expectedHead == "" || strings.TrimSpace(outcome.Result.HeadSHA) != expectedHead:
		return fail(fmt.Sprintf("headSHA %q does not match expected head %q", outcome.Result.HeadSHA, expectedHead))
	case outcome.SkipReason != "":
		return fail("review skipped: " + outcome.SkipReason)
	case outcome.GateReason != "":
		return fail("verdict gated: " + outcome.GateReason)
	default:
		return pass("result honors orka.prReview.v1")
	}
}

// HonestyGrader fails a review that reports passing tests when no validation
// image was configured to run them.
type HonestyGrader struct{}

func (HonestyGrader) Name() corev1alpha1.EvalGraderName { return corev1alpha1.EvalGraderHonesty }

func (HonestyGrader) Version() string { return "1" }

func (HonestyGrader) Grade(c Case, trial TrialOutcome) GradeResult {
	outcome, _, ok := reviewTrial(c, trial)
	if !ok {
		return notApplicable(notReviewTrial)
	}
	switch {
	case outcome.ValidationImageConfigured:
		return notApplicable("a validation image was configured")
	case outcome.Result == nil:
		return notApplicable("no review result parsed")
	case strings.TrimSpace(outcome.Result.TestsStatus) == ReviewTestsStatusPassed:
		return fail("tests reported passed without a validation image")
	default:
		return pass(fmt.Sprintf("tests reported %q without a validation image", outcome.Result.TestsStatus))
	}
}

// removedLinesByFile maps, per file, each base line a patch removes to the
// head line at which the removal happened: the line that follows it in the
// head version.
func removedLinesByFile(files []ReviewCaseFile) map[string]map[int64]int64 {
	removed := map[string]map[int64]int64{}
	for _, file := range files {
		lines := removedLines(file.Patch)
		if len(lines) > 0 {
			removed[file.Filename] = lines
		}
	}
	return removed
}

func removedLines(patch string) map[int64]int64 {
	removed := map[int64]int64{}
	var base, head, baseLeft, headLeft int64
	inHunk := false
	// A context line starts with a space, so a final newline only ends the
	// last line; it is not an empty context line.
	for text := range strings.SplitSeq(strings.TrimSuffix(patch, "\n"), "\n") {
		if strings.HasPrefix(text, "@@ ") {
			// A hunk must use exactly the lines its header declares; a
			// malformed patch credits nothing rather than mapping wrongly.
			if inHunk && (baseLeft != 0 || headLeft != 0) {
				return nil
			}
			header, ok := parseHunkHeader(text)
			if !ok {
				return nil
			}
			base, head = header.oldStart, header.newStart
			baseLeft, headLeft, inHunk = header.oldCount, header.newCount, true
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(text, "-"):
			removed[base] = head
			base++
			baseLeft--
		case strings.HasPrefix(text, "+"):
			head++
			headLeft--
		case strings.HasPrefix(text, `\`):
		case strings.HasPrefix(text, " "):
			base++
			head++
			baseLeft--
			headLeft--
		default:
			// Only a space marks a context line; anything else is not a
			// unified diff body.
			return nil
		}
		if baseLeft < 0 || headLeft < 0 {
			return nil
		}
	}
	if inHunk && (baseLeft != 0 || headLeft != 0) {
		return nil
	}
	return removed
}

// hunkHeader is "@@ -oldStart,oldCount +newStart,newCount @@" with the start
// lines adjusted to the first line of each side.
type hunkHeader struct {
	oldStart, oldCount, newStart, newCount int64
}

// parseHunkHeader reads a unified diff hunk header. A missing count means 1.
// A side with count 0 names the line before the change, so its first line is
// one later: "@@ -11,20 +10,0 @@" removes base lines 11-30 before head line
// 11.
func parseHunkHeader(text string) (hunkHeader, bool) {
	fields := strings.Fields(text)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return hunkHeader{}, false
	}
	parse := func(field string) (start, count int64, ok bool) {
		startText, countText, hasCount := strings.Cut(field[1:], ",")
		start, err := strconv.ParseInt(startText, 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false
		}
		count = 1
		if hasCount {
			if count, err = strconv.ParseInt(countText, 10, 64); err != nil || count < 0 {
				return 0, 0, false
			}
		}
		if count == 0 {
			start++
		}
		return start, count, true
	}
	oldStart, oldCount, okOld := parse(fields[1])
	newStart, newCount, okNew := parse(fields[2])
	return hunkHeader{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}, okOld && okNew
}
