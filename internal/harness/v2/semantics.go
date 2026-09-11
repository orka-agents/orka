package v2

import "fmt"

type ACPStopReason string

const (
	ACPStopReasonEndTurn         ACPStopReason = "end_turn"
	ACPStopReasonMaxTokens       ACPStopReason = "max_tokens"
	ACPStopReasonMaxTurnRequests ACPStopReason = "max_turn_requests"
	ACPStopReasonRefusal         ACPStopReason = "refusal"
	ACPStopReasonCancelled       ACPStopReason = "cancelled"
)

type PromptOutcome string

const (
	PromptOutcomeSucceeded PromptOutcome = "succeeded"
	PromptOutcomeFailed    PromptOutcome = "failed"
	PromptOutcomeCancelled PromptOutcome = "cancelled"
	PromptOutcomeUnknown   PromptOutcome = "outcome_unknown"
)

func (o PromptOutcome) Validate() error {
	switch o {
	case PromptOutcomeSucceeded, PromptOutcomeFailed, PromptOutcomeCancelled, PromptOutcomeUnknown:
		return nil
	default:
		return fmt.Errorf("unsupported prompt outcome %q", o)
	}
}

func (o PromptOutcome) RequiresExplicitNewTask() bool {
	return o == PromptOutcomeUnknown
}

type StopReasonMapping struct {
	StopReason           ACPStopReason `json:"stopReason,omitempty"`
	Known                bool          `json:"known"`
	EventType            EventType     `json:"eventType"`
	Outcome              PromptOutcome `json:"outcome"`
	CanValidateWorkspace bool          `json:"canValidateWorkspace"`
	PoisonSession        bool          `json:"poisonSession"`
	FailureCode          string        `json:"failureCode,omitempty"`
}

// MapACPStopReason converts the ACP session/prompt completion barrier into Orka
// semantics. An unproven settlement always becomes terminal outcome_unknown;
// no caller may infer success or automatically replay from transport loss.
func MapACPStopReason(reason ACPStopReason, settlementProven bool) StopReasonMapping {
	if !settlementProven {
		return StopReasonMapping{
			StopReason:    reason,
			Known:         isKnownACPStopReason(reason),
			EventType:     EventOutcomeUnknown,
			Outcome:       PromptOutcomeUnknown,
			PoisonSession: true,
			FailureCode:   "settlement_unproven",
		}
	}
	switch reason {
	case ACPStopReasonEndTurn:
		return StopReasonMapping{
			StopReason:           reason,
			Known:                true,
			EventType:            EventCompleted,
			Outcome:              PromptOutcomeSucceeded,
			CanValidateWorkspace: true,
		}
	case ACPStopReasonCancelled:
		return StopReasonMapping{
			StopReason:    reason,
			Known:         true,
			EventType:     EventCancelled,
			Outcome:       PromptOutcomeCancelled,
			PoisonSession: true,
			FailureCode:   "prompt_cancelled",
		}
	case ACPStopReasonMaxTokens:
		return failedStopReason(reason, true, "token_limit")
	case ACPStopReasonMaxTurnRequests:
		return failedStopReason(reason, true, "turn_limit")
	case ACPStopReasonRefusal:
		return failedStopReason(reason, true, "refusal")
	default:
		return failedStopReason(reason, false, "unknown_stop_reason")
	}
}

func failedStopReason(reason ACPStopReason, known bool, code string) StopReasonMapping {
	return StopReasonMapping{
		StopReason:    reason,
		Known:         known,
		EventType:     EventFailed,
		Outcome:       PromptOutcomeFailed,
		PoisonSession: true,
		FailureCode:   code,
	}
}

func isKnownACPStopReason(reason ACPStopReason) bool {
	switch reason {
	case ACPStopReasonEndTurn, ACPStopReasonMaxTokens, ACPStopReasonMaxTurnRequests,
		ACPStopReasonRefusal, ACPStopReasonCancelled:
		return true
	default:
		return false
	}
}
