/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// rejectedPromptRuntimeSessionGone reports whether the exact RuntimeSession a
// rate-limited, unaccepted prompt was sent to can no longer take it: the
// runtime no longer lists it at this generation, or is deleting it, as after
// the runtime retired it for idling. Waiting for prompt capacity and resending
// to it could then never succeed, so the attempt must requeue, which recreates
// the session. A status that cannot be read keeps the in-place wait.
func rejectedPromptRuntimeSessionGone(
	ctx context.Context,
	runtimeClient *harnessv2.Client,
	sessionID harnessv2.RuntimeSessionID,
	runtimeFence harnessv2.Fence,
) bool {
	_, observed, err := runtimeSessionStatusForUID(ctx, runtimeClient, runtimeFence.RuntimeSessionUID)
	if err != nil {
		return false
	}
	return observed == nil || observed.RuntimeSessionID != sessionID ||
		observed.Generation != runtimeFence.RuntimeSessionGeneration ||
		observed.State == harnessv2.RuntimeSessionStateDeleting || observed.State == harnessv2.RuntimeSessionStateDeleted
}
