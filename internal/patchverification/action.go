package patchverification

import (
	"encoding/json"
	"fmt"
	"reflect"
)

const (
	OutcomeReproduced    = "reproduced"
	OutcomeNotReproduced = "not-reproduced"
	OutcomeUntested      = "untested"
	OutcomePassed        = "passed"
	OutcomeNotFixed      = "not-fixed"
	OutcomeRegression    = "regression"
)

func ActionOrDefault(action Action) Action {
	if action == "" {
		return VerifyPatch
	}
	return action
}

func UnavailableAction(action Action) Conclusion {
	if action == ValidateReport {
		return UnableToValidate
	}
	return UnableToVerify
}

func ActionSides(action Action) []string {
	if action == ValidateReport {
		return []string{Original}
	}
	return []string{Original, Patched}
}

func StableReportDigest(problem string, scope []string) (string, error) {
	report := struct {
		Problem string   `json:"problem"`
		Scope   []string `json:"scope"`
	}{Problem: problem, Scope: scope}
	if !validManifestUTF8(reflect.ValueOf(report)) {
		return "", fmt.Errorf("report strings must be valid UTF-8")
	}
	content, err := json.Marshal(report)
	if err != nil {
		return "", fmt.Errorf("encode report identity: %w", err)
	}
	return Digest(content), nil
}

func ValidateBinding(action Action, binding Binding) error {
	if !identifierPattern.MatchString(binding.RunID) || !identifierPattern.MatchString(binding.AttemptID) ||
		binding.OriginalTaskID == "" || !sha256Pattern.MatchString(binding.ManifestDigest) {
		return fmt.Errorf("invalid run or attempt binding")
	}
	if action == ValidateReport {
		if binding.PatchedTaskID != "" {
			return fmt.Errorf("report validation cannot bind a patched task")
		}
		return nil
	}
	if binding.PatchedTaskID == "" || binding.OriginalTaskID == binding.PatchedTaskID {
		return fmt.Errorf("patch verification requires distinct original and patched tasks")
	}
	return nil
}
