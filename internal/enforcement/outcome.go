package enforcement

import (
	"errors"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

var needGuidance = map[string]string{
	"complete-policy":               "Supply one complete supported policy; do not omit a required capability",
	"supported-format":              "Use a policy format supported by every mandatory provider",
	"configured-backend":            "Ask the operator to configure the native backend for the requested policy",
	"supported-target":              "Use a supported prepared container and filesystem/network profile",
	"supported-runtime":             "Use the managed native AI worker or a supported managed ACP RuntimePool",
	"mandatory-capability-coverage": "Ask the operator to configure mandatory providers covering every requested capability",
	"provider-recovery":             "Wait for the configured providers and prepared runtime to recover; do not weaken the requirement",
	"prepared-container":            "Wait for the exact managed worker container to become prepared",
	"stopped-previous-container":    "Wait for the previous physical container to stop and its controls to retire",
	"operator-configuration":        "Ask the operator to check provider enrollment, policy and target compatibility",
	"admission-recovery":            "Inspect the Task admission recovery; a missing receipt does not authorize removal of controls",
}

// Outcome accepts only known explanatory identifiers. Provider prose and unknown
// needs never enter public Task status.
func Outcome(reason string, needs ...string) *corev1alpha1.EnforcementOutcome {
	fallback := "provider-recovery"
	switch reason {
	case "EnforcementPending":
		fallback = "prepared-container"
	case "EnforcementRefused":
		fallback = "operator-configuration"
	case "EnforcementAdmissionPending", "EnforcementUncertain":
	case "EnforcementReleaseUnobserved":
		reason, fallback = "EnforcementUncertain", "admission-recovery"
	default:
		reason = "EnforcementUncertain"
	}
	result := &corev1alpha1.EnforcementOutcome{Reason: reason}
	seen := make(map[string]bool)
	for _, need := range needs {
		if _, known := needGuidance[need]; known && !seen[need] {
			result.Needs = append(result.Needs, corev1alpha1.EnforcementNeed(need))
			seen[need] = true
		}
	}
	if len(result.Needs) == 0 {
		result.Needs = []corev1alpha1.EnforcementNeed{corev1alpha1.EnforcementNeed(fallback)}
	}
	return result
}

func OutcomeMessage(outcome *corev1alpha1.EnforcementOutcome) string {
	message := "Required container enforcement admission is not confirmed."
	if outcome.Reason == "EnforcementRefused" {
		message = "Required container enforcement was refused; the workload was not admitted."
	}
	var guidance []string
	for _, need := range outcome.Needs {
		if text, known := needGuidance[string(need)]; known {
			guidance = append(guidance, text)
		}
	}
	if len(guidance) > 0 {
		message += " " + strings.Join(guidance, "; ") + "."
	}
	return message
}

// RefusalError carries only explanatory needs; callers attach their own
// validation sentinel without coupling the provider client to Task storage.
type RefusalError struct {
	Needs []string
}

func (e *RefusalError) Error() string {
	return OutcomeMessage(Outcome("EnforcementRefused", e.Needs...))
}

func RefusalNeeds(err error) []string {
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		return refusal.Needs
	}
	return nil
}

// AdmissionError preserves lifecycle classification while hiding provider/runtime
// prose from execution records and enforcement-scoped diagnostics.
type AdmissionError struct {
	Cause error
}

func (e *AdmissionError) Error() string {
	return OutcomeMessage(Outcome("EnforcementAdmissionPending"))
}

func (e *AdmissionError) Unwrap() error { return e.Cause }
