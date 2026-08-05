package v2

import "fmt"

type RuntimeSessionState string

const (
	RuntimeSessionStateCreating             RuntimeSessionState = "creating"
	RuntimeSessionStateIdle                 RuntimeSessionState = "idle"
	RuntimeSessionStatePromptRunning        RuntimeSessionState = "prompt_running"
	RuntimeSessionStateValidating           RuntimeSessionState = "validating"
	RuntimeSessionStatePreparingPublication RuntimeSessionState = "preparing_publication"
	RuntimeSessionStatePublicationPrepared  RuntimeSessionState = "publication_prepared"
	RuntimeSessionStatePublishing           RuntimeSessionState = "publishing"
	RuntimeSessionStateVerifying            RuntimeSessionState = "verifying"
	RuntimeSessionStateFinalizing           RuntimeSessionState = "finalizing"
	RuntimeSessionStateCancelling           RuntimeSessionState = "cancelling"
	RuntimeSessionStatePoisoned             RuntimeSessionState = "poisoned"
	RuntimeSessionStateDeleting             RuntimeSessionState = "deleting"
	RuntimeSessionStateDeleted              RuntimeSessionState = "deleted"
)

var runtimeSessionTransitions = map[RuntimeSessionState]map[RuntimeSessionState]struct{}{
	RuntimeSessionStateCreating: {
		RuntimeSessionStateIdle:       {},
		RuntimeSessionStateCancelling: {},
		RuntimeSessionStatePoisoned:   {},
	},
	RuntimeSessionStateIdle: {
		RuntimeSessionStatePromptRunning: {},
		RuntimeSessionStatePoisoned:      {},
		RuntimeSessionStateDeleting:      {},
	},
	RuntimeSessionStatePromptRunning: {
		RuntimeSessionStateValidating: {},
		RuntimeSessionStateCancelling: {},
	},
	RuntimeSessionStateValidating: {
		RuntimeSessionStateIdle:                 {},
		RuntimeSessionStatePreparingPublication: {},
		RuntimeSessionStateCancelling:           {},
		RuntimeSessionStatePoisoned:             {},
	},
	RuntimeSessionStatePreparingPublication: {
		RuntimeSessionStatePublicationPrepared: {},
		RuntimeSessionStateCancelling:          {},
		RuntimeSessionStatePoisoned:            {},
	},
	RuntimeSessionStatePublicationPrepared: {
		RuntimeSessionStatePublishing: {},
		RuntimeSessionStateFinalizing: {},
		RuntimeSessionStateCancelling: {},
		RuntimeSessionStatePoisoned:   {},
	},
	RuntimeSessionStatePublishing: {
		RuntimeSessionStateVerifying: {},
		RuntimeSessionStatePoisoned:  {},
	},
	RuntimeSessionStateVerifying: {
		RuntimeSessionStatePoisoned: {},
	},
	RuntimeSessionStateFinalizing: {
		RuntimeSessionStatePoisoned: {},
		RuntimeSessionStateDeleting: {},
	},
	RuntimeSessionStateCancelling: {
		RuntimeSessionStatePoisoned: {},
	},
	RuntimeSessionStatePoisoned: {
		RuntimeSessionStateDeleting: {},
	},
	RuntimeSessionStateDeleting: {
		RuntimeSessionStateDeleted: {},
	},
	RuntimeSessionStateDeleted: {},
}

func RuntimeSessionStates() []RuntimeSessionState {
	return []RuntimeSessionState{
		RuntimeSessionStateCreating,
		RuntimeSessionStateIdle,
		RuntimeSessionStatePromptRunning,
		RuntimeSessionStateValidating,
		RuntimeSessionStatePreparingPublication,
		RuntimeSessionStatePublicationPrepared,
		RuntimeSessionStatePublishing,
		RuntimeSessionStateVerifying,
		RuntimeSessionStateFinalizing,
		RuntimeSessionStateCancelling,
		RuntimeSessionStatePoisoned,
		RuntimeSessionStateDeleting,
		RuntimeSessionStateDeleted,
	}
}

func IsKnownRuntimeSessionState(state RuntimeSessionState) bool {
	_, ok := runtimeSessionTransitions[state]
	return ok
}

func RuntimeSessionTransitionAllowed(from, to RuntimeSessionState) bool {
	if from == to && IsKnownRuntimeSessionState(from) {
		return true
	}
	allowed, ok := runtimeSessionTransitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

func ValidateRuntimeSessionTransition(from, to RuntimeSessionState) error {
	if !IsKnownRuntimeSessionState(from) {
		return fmt.Errorf("unsupported runtime session state %q", from)
	}
	if !IsKnownRuntimeSessionState(to) {
		return fmt.Errorf("unsupported runtime session state %q", to)
	}
	if !RuntimeSessionTransitionAllowed(from, to) {
		return fmt.Errorf("runtime session transition %s -> %s is not allowed", from, to)
	}
	return nil
}

func (s RuntimeSessionState) CanAdmitPrompt() bool {
	return s == RuntimeSessionStateIdle
}

func (s RuntimeSessionState) CanEvict() bool {
	return s == RuntimeSessionStateIdle
}

func (s RuntimeSessionState) AllowsPromptScopedMCP() bool {
	return s == RuntimeSessionStatePromptRunning
}

func (s RuntimeSessionState) IsTerminal() bool {
	return s == RuntimeSessionStateDeleted
}

func (s RuntimeSessionState) PublicationReconciliationInProgress() bool {
	switch s {
	case RuntimeSessionStatePublishing, RuntimeSessionStateVerifying, RuntimeSessionStateFinalizing:
		return true
	default:
		return false
	}
}

// DeletionTransition returns the first safe state transition for a deletion
// request. Publication reconciliation and finalization are never interrupted;
// callers must finish them before asking again.
func DeletionTransition(state RuntimeSessionState) (RuntimeSessionState, error) {
	if !IsKnownRuntimeSessionState(state) {
		return "", fmt.Errorf("unsupported runtime session state %q", state)
	}
	switch state {
	case RuntimeSessionStateIdle, RuntimeSessionStateFinalizing, RuntimeSessionStatePoisoned:
		return RuntimeSessionStateDeleting, nil
	case RuntimeSessionStateCreating, RuntimeSessionStatePromptRunning:
		return RuntimeSessionStateCancelling, nil
	case RuntimeSessionStateCancelling:
		return RuntimeSessionStateCancelling, nil
	case RuntimeSessionStateValidating, RuntimeSessionStatePreparingPublication,
		RuntimeSessionStatePublicationPrepared, RuntimeSessionStatePublishing, RuntimeSessionStateVerifying:
		return state, fmt.Errorf("runtime session state %s is reserved and must finish validation, publication reconciliation, or finalization before deletion", state)
	case RuntimeSessionStateDeleting, RuntimeSessionStateDeleted:
		return state, nil
	default:
		return "", fmt.Errorf("runtime session state %s has no deletion transition", state)
	}
}
