package supervisor

import harnessv2 "github.com/orka-agents/orka/internal/harness/v2"

var sessionStateTransitionHook func(from, to harnessv2.RuntimeSessionState)

func setSessionState(state *sessionState, next harnessv2.RuntimeSessionState) {
	if hook := sessionStateTransitionHook; hook != nil {
		hook(state.descriptor.State, next)
	}
	state.descriptor.State = next
	if _, admitted := operatorAccessStates[next]; !admitted {
		revokeSessionOperatorsLocked(state)
	} else {
		state.operatorsClosed = false
	}
}
