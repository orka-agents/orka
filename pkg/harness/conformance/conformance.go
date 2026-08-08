package conformance

import (
	"context"

	internal "github.com/orka-agents/orka/internal/harness/conformance"
)

type (
	Target = internal.Target
	Result = internal.Result
)

func CheckReadiness(ctx context.Context, target Target) Result {
	return internal.CheckReadiness(ctx, target)
}

func Check(ctx context.Context, target Target) Result {
	return internal.Check(ctx, target)
}
