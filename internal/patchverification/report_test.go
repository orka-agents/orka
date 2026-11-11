package patchverification

import "testing"

func TestReportValidationConclusions(test *testing.T) {
	for _, scenario := range []struct {
		name string
		want Conclusion
	}{
		{"reproduced", Reproduced},
		{"not-reproduced", NotReproduced},
		{"partial-reproduction", Reproduced},
		{"missing", UnableToValidate},
		{"blocked", UnableToValidate},
		{"conflict", UnableToValidate},
		{"foreign-patched", UnableToValidate},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			manifest := testManifest()
			manifest.Action = ValidateReport
			manifest.ReportDigest, _ = StableReportDigest(manifest.Problem, manifest.Scope)
			manifest.Sources.Patched = SourceIdentity{}
			manifest.Sources.DiffDigest = ""
			binding, paired := testEvidence(manifest)
			binding.PatchedTaskID = ""
			observations := paired[:len(manifest.Checks)]
			switch scenario.name {
			case "not-reproduced", "partial-reproduction":
				for index := range 2 {
					if scenario.name == "partial-reproduction" && index == 1 {
						break
					}
					observations[index].StdoutDigest = Digest([]byte(manifest.Checks[index].Healthy.Stdout))
					observations[index].StdoutBytes = len(manifest.Checks[index].Healthy.Stdout)
				}
			case "missing":
				observations = observations[:1]
			case "blocked":
				observations[0].SetupError = "required service is unreachable"
			case "conflict":
				other := observations[0]
				other.Skipped = true
				observations = append(observations, other)
			case "foreign-patched":
				observations = append(observations, paired[len(manifest.Checks)])
			}
			assessment := Evaluate(manifest, binding, observations)
			if assessment.Conclusion != scenario.want || assessment.Conclusion == Verified {
				test.Fatalf("report result = %s, want %s", assessment.Conclusion, scenario.want)
			}
			if scenario.name == "missing" && assessment.Checks[0].Outcome != "reproduced" {
				test.Fatal("partial report observations were discarded")
			}
			if len(assessment.Checks) != len(manifest.Checks) {
				test.Fatal("required cases disappeared from the assessment")
			}
			if scenario.name == "conflict" && assessment.Checks[0].Outcome != "untested" {
				test.Fatal("conflicting observations were counted as a reproduction")
			}
		})
	}
}

func TestActionVerificationPreservesCases(test *testing.T) {
	manifest := testManifest()
	manifest.Action = VerifyPatch
	manifest.ReportDigest, _ = StableReportDigest(manifest.Problem, manifest.Scope)
	manifest.DeclaredChanges = []DeclaredChange{{Kind: "configuration", Description: "retain managed setting"}}
	binding, observations := testEvidence(manifest)
	for position, check := range manifest.Checks {
		if position == 1 {
			continue
		}
		observation := &observations[len(manifest.Checks)+position]
		observation.ExitCode = new(check.Failure.ExitCode)
		observation.StdoutDigest = Digest([]byte(check.Failure.Stdout))
		observation.StdoutBytes = len(check.Failure.Stdout)
	}
	result := Evaluate(manifest, binding, observations)
	if result.Conclusion != Regression || result.Checks[0].Outcome != "not-fixed" ||
		result.Checks[1].Outcome != "passed" || result.Checks[2].Outcome != "regression" {
		test.Fatalf("lost simultaneous unfixed and regression cases: %+v", result)
	}
	observations[0].StdoutDigest = Digest([]byte(manifest.Checks[0].Healthy.Stdout))
	observations[0].StdoutBytes = len(manifest.Checks[0].Healthy.Stdout)
	if OriginalReady(manifest, binding, observations[:len(manifest.Checks)]) {
		test.Fatal("a missing required reproduction authorized the patched arm")
	}
	result = Evaluate(manifest, binding, observations[:len(manifest.Checks)])
	if result.Conclusion != UnableToVerify || result.Checks[0].OriginalOutcome != "not-reproduced" ||
		result.Checks[1].OriginalOutcome != "reproduced" || result.Checks[1].PatchedOutcome != "untested" {
		test.Fatalf("baseline results or untested patch slots were lost: %+v", result)
	}
}
