package supervisor

import "context"

// promptGateCancellation identifies a local authority revocation for one exact
// prompt. Its wait only holds the resulting downstream error while the adapter
// consumes cancellation; it never delays revocation or supplies outcome proof.
type promptGateCancellation struct {
	wait func(context.Context)
}

func (*promptGateCancellation) Error() string { return "prompt authority cancelled" }

func waitForPromptGateCancellation(downstream, gate context.Context) {
	if gate == nil {
		return
	}
	if cancellation, ok := context.Cause(gate).(*promptGateCancellation); ok && cancellation != nil && cancellation.wait != nil {
		cancellation.wait(downstream)
	}
}
