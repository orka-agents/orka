package controller

import (
	"context"
	"errors"
	"sync"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// ACPMCPPromptLeaseRegistry holds the controller-confirmed lease of each live
// approval-enabled prompt. Its zero value is ready to use. Durable attempts and
// epoch fences remain authoritative; a missing local lease denies execution,
// including after controller restart, when accepted prompts are not replayed.
type ACPMCPPromptLeaseRegistry struct {
	mu      sync.Mutex
	prompts map[store.PromptAttemptKey]*acpMCPPromptLease
}

type acpMCPPromptLease struct {
	registry   *ACPMCPPromptLeaseRegistry
	key        store.PromptAttemptKey
	fence      harnessv2.Fence
	ctx        context.Context
	generation uint64
	expiresAt  time.Time
}

var errACPMCPPromptLeaseInactive = errors.New("MCP approval prompt lease is not active")

func mcpPromptLeaseKey(namespace string, metadata harnessv2.MutationMetadata) store.PromptAttemptKey {
	return store.PromptAttemptKey{
		Namespace: namespace, TaskUID: string(metadata.TaskUID),
		Attempt: int64(metadata.TaskAttempt), PromptID: string(metadata.PromptID),
	}
}

func mcpPromptLeaseExpiry(lease harnessv2.PromptLease, authorization harnessv2.PromptMCPAuthorization) time.Time {
	if authorization.ExpiresAt.Before(lease.ExpiresAt) {
		return authorization.ExpiresAt
	}
	return lease.ExpiresAt
}

func (r *ACPMCPPromptLeaseRegistry) register(ctx context.Context, namespace string, request harnessv2.StartPromptRequest) (*acpMCPPromptLease, error) {
	if len(request.MCPAuthorization.ApprovalPolicy.RequiredTools) == 0 {
		return nil, nil
	}
	if r == nil || ctx == nil || ctx.Err() != nil {
		return nil, errACPMCPPromptLeaseInactive
	}
	key := mcpPromptLeaseKey(namespace, request.Metadata)
	if err := key.Validate(); err != nil {
		return nil, err
	}
	if err := request.Metadata.Fence.Validate(true); err != nil {
		return nil, err
	}
	entry := &acpMCPPromptLease{
		registry: r, key: key, fence: request.Metadata.Fence, ctx: ctx,
		generation: request.Lease.Generation, expiresAt: mcpPromptLeaseExpiry(request.Lease, request.MCPAuthorization),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry.generation == 0 || request.MCPAuthorization.LeaseGeneration != entry.generation ||
		!time.Now().UTC().Before(entry.expiresAt) || r.prompts[key] != nil {
		return nil, errACPMCPPromptLeaseInactive
	}
	if r.prompts == nil {
		r.prompts = make(map[store.PromptAttemptKey]*acpMCPPromptLease)
	}
	r.prompts[key] = entry
	return entry, nil
}

// renew consumes only a controller-sent renewal whose exact lease the runtime
// client has confirmed. An ambiguous or late response cannot revive authority.
func (p *acpMCPPromptLease) renew(request harnessv2.RenewPromptLeaseRequest) error {
	if p == nil {
		return nil
	}
	r := p.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	if r.prompts[p.key] != p || p.ctx.Err() != nil || !now.Before(p.expiresAt) ||
		mcpPromptLeaseKey(p.key.Namespace, request.Metadata) != p.key || request.Metadata.Fence != p.fence ||
		request.ExpectedLeaseGeneration != p.generation || request.Lease.Generation != p.generation+1 ||
		request.MCPAuthorization.LeaseGeneration != request.Lease.Generation {
		return errACPMCPPromptLeaseInactive
	}
	expiry := mcpPromptLeaseExpiry(request.Lease, request.MCPAuthorization)
	if !expiry.After(p.expiresAt) {
		return errACPMCPPromptLeaseInactive
	}
	p.generation, p.expiresAt = request.Lease.Generation, expiry
	return nil
}

func (p *acpMCPPromptLease) release() {
	if p == nil {
		return
	}
	p.registry.mu.Lock()
	defer p.registry.mu.Unlock()
	if p.registry.prompts[p.key] == p {
		delete(p.registry.prompts, p.key)
	}
}

func (r *ACPMCPPromptLeaseRegistry) authorize(request harnessv2.MCPBrokerCallRequest) error {
	if r == nil {
		return errACPMCPPromptLeaseInactive
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.prompts[mcpPromptLeaseKey(request.Namespace, request.Metadata)]
	// Check the context directly, without waiting for a cancellation goroutine
	// or remote cleanup, and read the clock after the durable authorizer's I/O.
	if entry == nil || entry.fence != request.Metadata.Fence || entry.ctx.Err() != nil ||
		!time.Now().UTC().Before(entry.expiresAt) {
		return errACPMCPPromptLeaseInactive
	}
	return nil
}
