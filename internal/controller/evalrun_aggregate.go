package controller

import (
	"slices"
	"sort"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/store"
)

// evalRunMaxResults matches the EvalRun status.results limit.
const evalRunMaxResults = 256

type evalRunAggregate struct {
	Counts       corev1alpha1.EvalRunCounts
	Results      []corev1alpha1.EvalGraderResult
	SuspectCases []string
}

// aggregateEvalRun reduces trial records to integer counts. Infra errors
// count neither way, and a grader that does not apply to a trial does not
// count that trial for that grader.
func aggregateEvalRun(cases []evals.Case, trials int32, graders []corev1alpha1.EvalGraderName, records []store.EvalTrialRecord) evalRunAggregate {
	aggregate := evalRunAggregate{Counts: corev1alpha1.EvalRunCounts{
		Planned:   int32(len(cases)) * trials,
		Completed: int32(len(records)),
	}}
	byCase := map[string][]store.EvalTrialRecord{}
	for _, record := range records {
		byCase[record.CaseID] = append(byCase[record.CaseID], record)
		switch record.Outcome {
		case store.EvalTrialOutcomePass:
			aggregate.Counts.Passed++
		case store.EvalTrialOutcomeFail:
			aggregate.Counts.Failed++
		case store.EvalTrialOutcomeInfraError:
			aggregate.Counts.InfraError++
		}
	}

	tagSet := map[string]struct{}{}
	for _, c := range cases {
		for _, tag := range c.Tags {
			tagSet[tag] = struct{}{}
		}
	}
	tags := make([]string, 0, len(tagSet)+1)
	tags = append(tags, "")
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags[1:])

	sortedGraders := append([]corev1alpha1.EvalGraderName(nil), graders...)
	slices.Sort(sortedGraders)
	// Untagged aggregates come first so truncation never drops them.
	for _, tag := range tags {
		for _, grader := range sortedGraders {
			if len(aggregate.Results) == evalRunMaxResults {
				break
			}
			aggregate.Results = append(aggregate.Results, aggregateEvalGrader(cases, byCase, grader, tag))
		}
	}

	for _, c := range cases {
		caseRecords := byCase[c.ID]
		if int32(len(caseRecords)) < trials {
			continue
		}
		graded, passed := 0, 0
		for _, record := range caseRecords {
			switch record.Outcome {
			case store.EvalTrialOutcomePass:
				graded++
				passed++
			case store.EvalTrialOutcomeFail:
				graded++
			}
		}
		if graded > 0 && passed == 0 {
			aggregate.SuspectCases = append(aggregate.SuspectCases, c.ID)
		}
	}
	sort.Strings(aggregate.SuspectCases)
	return aggregate
}

func aggregateEvalGrader(cases []evals.Case, byCase map[string][]store.EvalTrialRecord, grader corev1alpha1.EvalGraderName, tag string) corev1alpha1.EvalGraderResult {
	result := corev1alpha1.EvalGraderResult{Grader: grader, Tag: tag}
	for _, c := range cases {
		if tag != "" && !evalCaseHasTag(c, tag) {
			continue
		}
		graded, passed := int32(0), int32(0)
		for _, record := range byCase[c.ID] {
			if record.Outcome == store.EvalTrialOutcomeInfraError {
				continue
			}
			for _, grade := range record.Grades {
				if grade.Grader != string(grader) {
					continue
				}
				switch evals.GradeStatus(grade.Status) {
				case evals.GradePass:
					graded++
					passed++
				case evals.GradeFail:
					graded++
				}
			}
		}
		if graded == 0 {
			continue
		}
		result.Trials += graded
		result.PassedTrials += passed
		result.Cases++
		if passed > 0 {
			result.CasesAnyPassed++
		}
		if passed == graded {
			result.CasesAllPassed++
		}
	}
	return result
}

func evalCaseHasTag(c evals.Case, tag string) bool {
	return slices.Contains(c.Tags, tag)
}
