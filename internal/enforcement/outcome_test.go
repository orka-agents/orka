package enforcement

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestOutcomeDropsUntrustedNeedsAndPreservesSafeGuidance(t *testing.T) {
	const private = "private-provider-policy-details"
	outcome := Outcome("EnforcementRefused", private, "supported-format", "supported-format", "complete-policy")
	if !slices.Equal(outcome.Needs, []corev1alpha1.EnforcementNeed{"supported-format", "complete-policy"}) {
		t.Fatalf("unexpected safe needs: %v", outcome.Needs)
	}
	if message := OutcomeMessage(outcome); strings.Contains(message, private) || len(message) == 0 {
		t.Fatalf("unsafe or missing guidance: %q", message)
	}
}

func TestOutcomeFallsBackWithoutPromotingUnknownProviderIdentifiers(t *testing.T) {
	for _, reason := range []string{"EnforcementPending", "EnforcementAdmissionPending", "EnforcementRefused", "EnforcementUncertain", "private-provider-reason"} {
		t.Run(reason, func(t *testing.T) {
			outcome := Outcome(reason, "private-provider-need")
			if len(outcome.Needs) == 0 || strings.Contains(OutcomeMessage(outcome), "private-provider") {
				t.Fatal("unknown provider detail escaped or left the operator without guidance")
			}
			if reason == "private-provider-reason" && outcome.Reason != "EnforcementUncertain" {
				t.Fatal("unknown provider reason became an authoritative outcome")
			}
		})
	}
}

func TestUnobservedReleaseRetainsRecoveryGuidance(t *testing.T) {
	outcome := Outcome("EnforcementReleaseUnobserved")
	if outcome.Reason != "EnforcementUncertain" || !slices.Contains(outcome.Needs, "admission-recovery") {
		t.Fatalf("cleanup recovery lost its permitted explanatory outcome: %+v", outcome)
	}
}

func TestSafeAdmissionErrorsRetainLifecycleClassification(t *testing.T) {
	cause := fmt.Errorf("private-provider-details: %w", context.DeadlineExceeded)
	err := fmt.Errorf("admission: %w", &AdmissionError{Cause: cause})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("safe admission error changed the runtime deadline classification")
	}
	if strings.Contains(err.Error(), "private-provider-details") {
		t.Fatal("provider details escaped the safe admission wrapper")
	}
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Cause != cause {
		t.Fatal("safe admission error lost its original cause")
	}
}

func TestRefusalNeedsSurviveValidationWrappingWithoutUnsafeProse(t *testing.T) {
	sentinel := errors.New("validation")
	err := errors.Join(sentinel, &RefusalError{Needs: []string{"supported-format", "private-provider-details"}})
	if !errors.Is(err, sentinel) || !slices.Contains(RefusalNeeds(err), "supported-format") {
		t.Fatal("refusal wrapping lost validation classification or actionable needs")
	}
	if strings.Contains(err.Error(), "private-provider-details") {
		t.Fatal("unknown refusal needs escaped into the error message")
	}
}
