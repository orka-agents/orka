package supervisor

import harnessv2 "github.com/orka-agents/orka/internal/harness/v2"

// sessionStateTransitionHook observes every RuntimeSession lifecycle write.
// It is nil in production; tests set it to check each transition against
// harnessv2.RuntimeSessionTransitionAllowed so the supervisor cannot drift
// from the published lifecycle table unnoticed.
var sessionStateTransitionHook func(from, to harnessv2.RuntimeSessionState)

// setSessionState records a RuntimeSession lifecycle transition. It does not
// validate or reject the move; callers keep their own guards. The caller must
// hold s.mu or otherwise own state exclusively.
func setSessionState(state *sessionState, next harnessv2.RuntimeSessionState) {
	if hook := sessionStateTransitionHook; hook != nil {
		hook(state.descriptor.State, next)
	}
	state.descriptor.State = next
	if _, admitted := operatorAccessStates[next]; !admitted {
		revokeSessionOperatorsLocked(state)
	}
}
