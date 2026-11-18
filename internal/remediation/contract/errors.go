package contract

import "errors"

var (
	ErrDisabled                 = errors.New("remediation is disabled")
	ErrInvalid                  = errors.New("invalid remediation request")
	ErrNotFound                 = errors.New("remediation run not found")
	ErrConflict                 = errors.New("remediation request conflicts with retained state")
	ErrUnavailable              = errors.New("remediation is temporarily unavailable")
	ErrForbidden                = errors.New("remediation action is not authorized")
	ErrEvidenceScopeUnsupported = errors.New("required evidence scope is not supported by the configured remediation backend")
)

const (
	PhasePending    = "Pending"
	PhaseRunning    = "Running"
	PhaseSucceeded  = "Succeeded"
	PhaseFailed     = "Failed"
	PhaseCancelled  = "Cancelled"
	PhaseTimedOut   = "TimedOut"
	PhaseNeedsInput = "NeedsInput"

	StageReproduce = "reproduce"
	StageGenerate  = "generate"
	StageVerify    = "verify"
	StageComplete  = "complete"

	OutcomeReproduced       = "Reproduced"
	OutcomeNotReproduced    = "NotReproduced"
	OutcomeUnableToValidate = "UnableToValidate"
	OutcomeUnableToVerify   = "UnableToVerify"
	OutcomeUnverified       = "Unverified"
	OutcomeVerified         = "Verified"
	OutcomeCancelled        = "Cancelled"
	OutcomeTimedOut         = "TimedOut"

	ReasonEvidenceScopeUnsupported = "evidence-scope-unsupported"
)

func TerminalPhase(phase string) bool {
	switch phase {
	case PhaseSucceeded, PhaseFailed, PhaseCancelled, PhaseTimedOut, PhaseNeedsInput:
		return true
	default:
		return false
	}
}
