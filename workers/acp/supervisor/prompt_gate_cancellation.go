package supervisor

import "context"

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
