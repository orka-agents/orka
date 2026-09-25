package conformance

import (
	"context"
	"fmt"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// The workspace probe must finish one original prompt even when provisioning
// takes longer than its initial lease. This loop alone owns the confirmed lease;
// renewal is never concurrent with cancellation, finalization, or cleanup.
func (s *lifecycleProbeState) consumeWorkspacePrompt(
	ctx context.Context,
	probeID string,
	metadata harnessv2.MutationMetadata,
	stream *harnessv2.PromptStream,
) promptStreamResult {
	streamDone := make(chan promptStreamResult, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		streamDone <- consumePromptStream(stream)
	}()
	defer func() {
		_ = stream.Close()
		<-joined
	}()
	timer := time.NewTimer(time.Until(s.lease.ExpiresAt) / 2)
	defer timer.Stop()
	renewAt := timer.C
	for {
		select {
		case result := <-streamDone:
			if err := ctx.Err(); err != nil {
				return promptStreamResult{err: err}
			}
			return result
		case <-ctx.Done():
			return promptStreamResult{err: ctx.Err()}
		case <-renewAt:
			observation := stream.Summary()
			if observation.Terminal != nil {
				// Still require clean EOF, but a terminal prompt needs no lease.
				renewAt = nil
				continue
			}
			if !observation.Accepted {
				return promptStreamResult{err: fmt.Errorf("workspace prompt acceptance was not observed before lease renewal")}
			}
			renewed, err := s.renewWorkspacePrompt(ctx, probeID, metadata)
			if err != nil {
				return promptStreamResult{err: err}
			}
			if !renewed {
				renewAt = nil
				continue
			}
			timer.Reset(time.Until(s.lease.ExpiresAt) / 2)
		}
	}
}

func (s *lifecycleProbeState) renewWorkspacePrompt(
	ctx context.Context,
	probeID string,
	original harnessv2.MutationMetadata,
) (bool, error) {
	now := time.Now().UTC()
	duration := s.lease.ExpiresAt.Sub(s.lease.IssuedAt)
	expiresAt := now.Add(duration)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(expiresAt) {
		expiresAt = deadline
	}
	minimum := time.Duration(s.target.Limits.MinPromptLeaseMillis) * time.Millisecond
	maximum := time.Duration(s.target.Limits.MaxPromptLeaseMillis) * time.Millisecond
	if !expiresAt.After(s.lease.ExpiresAt) || expiresAt.Sub(now) < minimum {
		// The remaining probe window cannot admit a valid extension. Keep the
		// original lease and wait for its stream or the bounded caller context.
		return false, nil
	}
	lease := harnessv2.PromptLease{Generation: s.lease.Generation + 1, IssuedAt: now, ExpiresAt: expiresAt}
	if err := lease.ValidateAt(now, minimum, maximum); err != nil {
		return false, fmt.Errorf("build workspace prompt lease renewal: %w", err)
	}
	metadata := newMetadata(s.sessionFence, original.TaskUID, original.TaskAttempt, original.PromptID,
		harnessv2.OperationID(fmt.Sprintf("renew-workspace-%s-%d", probeID, lease.Generation)), s.lease.ExpiresAt)
	authorization, err := s.promptAuthorization(metadata, lease)
	if err != nil {
		return false, err
	}
	request := harnessv2.RenewPromptLeaseRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: metadata,
		ExpectedLeaseGeneration: s.lease.Generation, Lease: lease, MCPAuthorization: authorization,
	}
	if err := setRequestDigest(&request, &request.Metadata); err != nil {
		return false, fmt.Errorf("build workspace prompt lease renewal: %w", err)
	}
	// An unacknowledged renewal cannot authorize work beyond the last confirmed
	// lease. Bound this single attempt by that expiry and never retry it.
	renewCtx, cancel := context.WithDeadline(ctx, s.lease.ExpiresAt)
	defer cancel()
	response, err := s.client.RenewPromptLease(renewCtx, s.sessionID, request)
	if err != nil {
		return false, fmt.Errorf("renew workspace prompt lease without replay: %w", err)
	}
	if err := renewCtx.Err(); err != nil {
		return false, fmt.Errorf("renew workspace prompt lease before confirmed expiry: %w", err)
	}
	if response.Classification.Class != harnessv2.RequestClassificationFresh {
		return false, fmt.Errorf("renew workspace prompt lease was not classified fresh")
	}
	// The v2 client validates the complete returned lease against the request.
	s.lease = response.Lease
	return true, nil
}
