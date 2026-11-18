package patchverification

import (
	"reflect"
	"strings"
)

func evaluateReport(manifest Manifest, binding Binding, observations []Observation) Assessment {
	return EvaluateOriginal(manifest, binding, observations)
}

type observationIndex struct {
	bySlot  map[string]Observation
	invalid map[string]bool
	reason  string
}

func indexObservations(manifest Manifest, binding Binding, observations []Observation) observationIndex {
	index := observationIndex{bySlot: make(map[string]Observation), invalid: make(map[string]bool)}
	if err := ValidateManifest(manifest); err != nil {
		index.reason = err.Error()
		return index
	}
	if err := validateApplicationObservationSet(manifest, observations); err != nil {
		index.reason = err.Error()
		return index
	}
	for _, observation := range observations {
		key := observation.Side + ":" + observation.CheckID
		if err := ValidateObservation(manifest, binding, observation); err != nil {
			index.reason, index.invalid[key] = err.Error(), true
			continue
		}
		if previous, found := index.bySlot[key]; found && !reflect.DeepEqual(previous, observation) {
			index.reason, index.invalid[key] = "conflicting observations for the same check", true
			continue
		}
		index.bySlot[key] = observation
	}
	return index
}

func EvaluateOriginal(manifest Manifest, binding Binding, observations []Observation) Assessment {
	return assessOriginal(manifest, indexObservations(manifest, binding, observations))
}

func assessOriginal(manifest Manifest, index observationIndex) Assessment {
	assessment := Assessment{Conclusion: UnavailableAction(manifest.Action)}
	missing := MissingRequirements(manifest.Environment)
	reproduced, incomplete := 0, index.reason != "" || len(missing) != 0
	for _, check := range manifest.Checks {
		key := Original + ":" + check.ID
		observation, found := index.bySlot[key]
		result := CheckResult{CheckID: check.ID, Outcome: OutcomeUntested}
		switch {
		case len(missing) != 0:
			result.Reason = "required environment is unavailable; see the overall reason"
		case index.invalid[key]:
			result.Reason = "required check has conflicting or incorrectly bound evidence"
		case !found || !usableCheck(check, observation):
			result.Reason = "required check is missing, skipped, or could not run under a suitable setup"
		case check.Kind == Reproduction && matches(observation, check.Failure):
			result.Outcome = OutcomeReproduced
			reproduced++
		case check.Kind == Reproduction && matches(observation, check.Healthy):
			result.Outcome = OutcomeNotReproduced
		case check.Kind == Normal && matches(observation, check.Healthy):
			result.Outcome = OutcomePassed
		default:
			result.Reason = "observations do not establish the expected behavior under suitable conditions"
		}
		incomplete = incomplete || result.Outcome == OutcomeUntested
		assessment.Checks = append(assessment.Checks, result)
	}
	switch {
	case incomplete:
		assessment.Reason = "required report evidence is missing, unsuitable, or conflicting"
		if index.reason != "" {
			assessment.Reason = index.reason
		}
		if len(missing) != 0 {
			assessment.Reason = strings.Join(missing, "; ")
		}
	case reproduced > 0:
		assessment.Conclusion, assessment.Reason = Reproduced, "reported behavior was reproduced in the listed cases"
	default:
		assessment.Conclusion, assessment.Reason = NotReproduced, "required checks did not reproduce the report under these conditions; this does not establish that the report is wrong"
	}
	return assessment
}

func OriginalReady(manifest Manifest, binding Binding, observations []Observation) bool {
	return originalReady(EvaluateOriginal(manifest, binding, observations))
}

func originalReady(assessment Assessment) bool {
	if assessment.Conclusion != Reproduced {
		return false
	}
	for _, result := range assessment.Checks {
		if result.Outcome != OutcomeReproduced && result.Outcome != OutcomePassed {
			return false
		}
	}
	return true
}

func evaluateVerification(manifest Manifest, binding Binding, observations []Observation) Assessment {
	index := indexObservations(manifest, binding, observations)
	original := assessOriginal(manifest, index)
	assessment := Assessment{Conclusion: UnableToVerify}
	fixed, unfixed, regressions := 0, 0, 0
	incomplete := !originalReady(original)
	for position, check := range manifest.Checks {
		result := compareCheck(check, original.Checks[position], index)
		switch result.Outcome {
		case OutcomePassed:
			if check.Kind == Reproduction {
				fixed++
			}
		case OutcomeNotFixed:
			unfixed++
		case OutcomeRegression:
			regressions++
		default:
			incomplete = true
		}
		assessment.Checks = append(assessment.Checks, result)
	}
	switch {
	case !originalReady(original):
		assessment.Reason = "all required original problems and normal behavior must be established before comparison"
		if original.Conclusion != Reproduced && original.Conclusion != NotReproduced {
			assessment.Reason = original.Reason
		}
	case incomplete:
		assessment.Reason = "one or more required comparisons could not be verified"
	case regressions > 0:
		assessment.Conclusion, assessment.Reason = Regression, "required normal behavior fails on the patch"
	case fixed > 0 && unfixed > 0:
		assessment.Conclusion, assessment.Reason = PartiallyFixed, "some demonstrated cases remain unfixed"
	case unfixed > 0:
		assessment.Conclusion, assessment.Reason = NotFixed, "all demonstrated cases remain unfixed"
	default:
		assessment.Conclusion, assessment.Reason = Verified,
			"all stated problems reproduced on the original; patched checks and required normal behavior passed"
	}
	return assessment
}

func compareCheck(check Check, original CheckResult, index observationIndex) CheckResult {
	result := CheckResult{CheckID: check.ID, Outcome: OutcomeUntested, OriginalOutcome: original.Outcome,
		PatchedOutcome: OutcomeUntested}
	key := Patched + ":" + check.ID
	patched, found := index.bySlot[key]
	switch {
	case original.Outcome != OutcomeReproduced && original.Outcome != OutcomePassed:
		result.Reason = "required original behavior was not established"
	case index.invalid[key]:
		result.Reason = "patched evidence is conflicting or incorrectly bound"
	case !found || !usableCheck(check, patched):
		result.Reason = "patched check is missing, skipped, or could not run under a suitable setup"
	case matches(patched, check.Healthy):
		result.Outcome, result.PatchedOutcome = OutcomePassed, OutcomePassed
	case matches(patched, check.Failure):
		result.Outcome, result.PatchedOutcome = OutcomeNotFixed, OutcomeNotFixed
		if check.Kind == Normal {
			result.Outcome, result.PatchedOutcome = OutcomeRegression, OutcomeRegression
		}
	default:
		result.Reason = "observed output and exit code match neither frozen expectation"
	}
	return result
}
