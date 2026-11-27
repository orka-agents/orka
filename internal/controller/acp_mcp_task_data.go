package controller

import (
	"context"
	"errors"
	"fmt"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

type ACPMCPTaskDataGuard func(context.Context, func(context.Context) error) error

type acpMCPTaskDataGuardContextKey struct{}

func ACPMCPTaskDataGuardFromContext(ctx context.Context) (ACPMCPTaskDataGuard, bool) {
	guard, ok := ctx.Value(acpMCPTaskDataGuardContextKey{}).(ACPMCPTaskDataGuard)
	return guard, ok && guard != nil
}

func withACPTaskStatusGuard(ctx context.Context, controls store.DurableControlStore, epochs *ControllerEpochManager, write func(context.Context) error) error {
	guard, ok := controls.(store.ControllerEpochMutationStore)
	if !ok || epochs == nil {
		return fmt.Errorf("ACP Task status requires the authoritative epoch mutation guard")
	}
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	return guard.WithControllerEpochMutation(ctx, fence, write)
}

func (b *ACPMCPBroker) taskDataGuard(request harnessv2.MCPBrokerCallRequest, expected ACPMCPBrokerCredentials) ACPMCPTaskDataGuard {
	return func(ctx context.Context, access func(context.Context) error) error {
		if b.EpochMutations == nil || access == nil {
			return fmt.Errorf("MCP task data requires the authoritative epoch mutation guard")
		}
		entered := false
		err := b.EpochMutations.WithControllerEpochMutation(ctx, expected.ControllerFence, func(guardCtx context.Context) error {
			entered = true
			current, err := b.Credentials.ResolveACPMCPBrokerCredentials(guardCtx, request)
			if err != nil {
				return fmt.Errorf("refresh MCP task data authority: %w", err)
			}
			if current.Task.Name != expected.Task.Name || current.Task.Namespace != expected.Task.Namespace || current.Task.UID != expected.Task.UID ||
				current.ControllerFence != expected.ControllerFence || harnessv2.CompareFence(current.ExpectedFence, request.Metadata.Fence, true) != harnessv2.FenceMatch {
				return fmt.Errorf("MCP task data identity is no longer active")
			}
			if err := request.Authorization.ValidateProfile(current.RuntimeProfile); err != nil {
				return fmt.Errorf("MCP task data policy is stale: %w", err)
			}
			if current.ExpectedMCPConfiguration != nil && !request.Authorization.Configuration().Matches(*current.ExpectedMCPConfiguration) {
				return fmt.Errorf("MCP task data policy is stale")
			}
			if err := b.Prompts.AuthorizeACPMCPPrompt(guardCtx, request); err != nil {
				return fmt.Errorf("MCP task data prompt is no longer active: %w", err)
			}
			return access(guardCtx)
		})
		if err != nil && !entered {
			if errors.Is(err, store.ErrControllerEpochMutationContention) {
				return fmt.Errorf("%w: %w", errACPMCPAuthorityUnavailable, err)
			}
			return acpMCPAuthorityReadError(err)
		}
		return err
	}
}
