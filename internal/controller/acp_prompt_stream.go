package controller

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const acpPromptCancellationTimeout = 25 * time.Second

type acpPromptCancellationResult struct {
	reason   harnessv2.CancelReason
	response *harnessv2.CancelPromptResponse
	err      error
}

func requestACPPromptCancellation(
	ctx context.Context,
	runtimeClient *harnessv2.Client,
	sessionID harnessv2.RuntimeSessionID,
	task *corev1alpha1.Task,
	fence harnessv2.Fence,
	reason harnessv2.CancelReason,
) *acpPromptCancellationResult {
	now := time.Now().UTC()
	request := harnessv2.CancelPromptRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: mutationMetadata(fence, task, "cancel-prompt", true, now.Add(30*time.Second)),
		Reason:   reason, SettlementDeadline: now.Add(20 * time.Second),
	}
	result := &acpPromptCancellationResult{reason: reason}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		result.err = fmt.Errorf("seal ACP prompt cancellation request: %w", err)
		return result
	}
	result.response, result.err = cancelPromptWithUnsentRetry(ctx, runtimeClient, sessionID, request)
	return result
}

// streamACPPromptWithCancellation keeps the original accepted stream readable
// while cancellation settles. A completed settlement alone cannot recover its
// result text: only this stream, including validated clean EOF, can prove it.
// Lease renewal and permission decisions still use the execution context.
func streamACPPromptWithCancellation(
	runtimeCtx context.Context,
	runtimeClient *harnessv2.Client,
	sessionID harnessv2.RuntimeSessionID,
	request harnessv2.StartPromptRequest,
	task *corev1alpha1.Task,
	fence harnessv2.Fence,
	cancellationTimeout time.Duration,
	emit func(harnessv2.Event) error,
) (harnessv2.PromptStreamSummary, *acpPromptCancellationResult, error) {
	transportCtx, cancelTransport := context.WithCancel(context.WithoutCancel(runtimeCtx))
	defer cancelTransport()
	// The callback may update Task status while cancellation is in flight.
	cancelTask := task.DeepCopy()
	var accepted atomic.Bool
	streamDone := make(chan struct{})
	cancelDone := make(chan struct{})
	var cancellation *acpPromptCancellationResult
	stopWatching := context.AfterFunc(runtimeCtx, func() {
		defer close(cancelDone)
		if !accepted.Load() {
			cancelTransport()
			return
		}
		drainCtx, cancelDrain := context.WithTimeout(context.WithoutCancel(runtimeCtx), cancellationTimeout)
		defer cancelDrain()
		stopDrain := context.AfterFunc(drainCtx, cancelTransport)
		defer stopDrain()
		reason := harnessv2.CancelReasonControllerShutdown
		if errors.Is(runtimeContextError(runtimeCtx), context.DeadlineExceeded) {
			reason = harnessv2.CancelReasonTaskTimeout
		}
		cancellation = requestACPPromptCancellation(drainCtx, runtimeClient, sessionID, cancelTask, fence, reason)
		if cancellation.err != nil || !cancellation.response.SettlementProven ||
			cancellation.response.Settlement.TerminalEvent != harnessv2.EventCompleted {
			// Only a proven completion needs result text from the stream.
			// Abort promptly when revocation is unproven or no result remains.
			cancelTransport()
			return
		}
		select {
		case <-streamDone:
		case <-drainCtx.Done():
			cancelTransport()
		}
	})
	// AfterFunc is asynchronous even when the context is already cancelled.
	// Prevent a new prompt submission in that case.
	if runtimeCtx.Err() != nil {
		cancelTransport()
	}
	summary, err := runtimeClient.StreamPrompt(transportCtx, sessionID, request, func(event harnessv2.Event) error {
		if event.Type == harnessv2.EventAccepted {
			accepted.Store(true)
		}
		return emit(event)
	})
	close(streamDone)
	if !stopWatching() {
		// Reuse this exact operation's response in stream-error handling, and
		// join the watcher before the RuntimeSession can be cleaned up.
		<-cancelDone
	}
	return summary, cancellation, err
}
