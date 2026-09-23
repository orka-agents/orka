package controller

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func approvalLeaseCall(t *testing.T) harnessv2.MCPBrokerCallRequest {
	t.Helper()
	request, _ := testMCPBrokerRequest(t, harnessv2.MCPToolEffectConsequential)
	request.Authorization.ApprovalPolicy.RequiredTools = []string{request.Call.ToolName}
	var err error
	request.Authorization.ApprovalPolicyDigest, err = harnessv2.CanonicalMCPApprovalPolicyDigest(request.Authorization.ApprovalPolicy)
	require.NoError(t, err)
	return request
}

func registerApprovalLease(t *testing.T, registry *ACPMCPPromptLeaseRegistry, ctx context.Context, request harnessv2.MCPBrokerCallRequest) *acpMCPPromptLease {
	t.Helper()
	entry, err := registry.register(ctx, request.Namespace, harnessv2.StartPromptRequest{
		Metadata: request.Metadata, Lease: request.Lease, MCPAuthorization: request.Authorization,
	})
	require.NoError(t, err)
	t.Cleanup(entry.release)
	return entry
}

func approvalLeaseRenewal(request harnessv2.MCPBrokerCallRequest, expiresAt time.Time) harnessv2.RenewPromptLeaseRequest {
	renewal := harnessv2.RenewPromptLeaseRequest{
		Metadata: request.Metadata, ExpectedLeaseGeneration: request.Lease.Generation,
		Lease: request.Lease, MCPAuthorization: request.Authorization,
	}
	renewal.Lease.Generation++
	renewal.Lease.IssuedAt = time.Now().UTC()
	renewal.Lease.ExpiresAt = expiresAt
	renewal.MCPAuthorization.LeaseGeneration = renewal.Lease.Generation
	renewal.MCPAuthorization.ExpiresAt = expiresAt
	return renewal
}

func TestACPMCPPromptLeaseRegistryRequiresExactIdentity(t *testing.T) {
	request := approvalLeaseCall(t)
	registry := &ACPMCPPromptLeaseRegistry{}
	leaseCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	entry := registerApprovalLease(t, registry, leaseCtx, request)
	require.NoError(t, registry.authorize(request))
	for _, test := range []struct {
		name   string
		change func(*harnessv2.MCPBrokerCallRequest)
	}{
		{"namespace", func(r *harnessv2.MCPBrokerCallRequest) { r.Namespace += "-other" }},
		{"task", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.TaskUID += "-other" }},
		{"attempt", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.TaskAttempt++ }},
		{"prompt", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.PromptID += "-other" }},
		{"runtime", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.RuntimeInstanceID += "-other" }},
		{"boot", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.SupervisorBootID += "-other" }},
		{"epoch", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.ControllerEpoch++ }},
		{"pool", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.RuntimePoolUID += "-other" }},
		{"pool generation", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.RuntimePoolGeneration++ }},
		{"session", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.RuntimeSessionUID += "-other" }},
		{"session generation", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.RuntimeSessionGeneration++ }},
		{"profile", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.RuntimeProfileDigest += "-other" }},
		{"profile schema", func(r *harnessv2.MCPBrokerCallRequest) { r.Metadata.Fence.ProfileDigestSchemaVersion++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := request
			test.change(&changed)
			require.ErrorIs(t, registry.authorize(changed), errACPMCPPromptLeaseInactive)
		})
	}
	// No release callback or cleanup goroutine has run when cancellation lands.
	cancel()
	require.ErrorIs(t, registry.authorize(request), errACPMCPPromptLeaseInactive)
	require.ErrorIs(t, entry.renew(approvalLeaseRenewal(request, time.Now().UTC().Add(3*time.Minute))), errACPMCPPromptLeaseInactive)
}

func TestACPMCPPromptLeaseRegistryExpiryCannotBeRevived(t *testing.T) {
	for _, deadline := range []string{"lease", "authorization"} {
		t.Run(deadline, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				request := approvalLeaseCall(t)
				expiry := time.Now().UTC().Add(time.Second)
				if deadline == "lease" {
					request.Lease.ExpiresAt = expiry
				} else {
					request.Authorization.ExpiresAt = expiry
				}
				registry := &ACPMCPPromptLeaseRegistry{}
				entry := registerApprovalLease(t, registry, t.Context(), request)
				require.NoError(t, registry.authorize(request))
				time.Sleep(time.Second)
				require.ErrorIs(t, registry.authorize(request), errACPMCPPromptLeaseInactive)
				require.ErrorIs(t, entry.renew(approvalLeaseRenewal(request, time.Now().UTC().Add(time.Minute))), errACPMCPPromptLeaseInactive)
				require.ErrorIs(t, registry.authorize(request), errACPMCPPromptLeaseInactive)
			})
		})
	}
}

func TestACPMCPPromptLeaseRegistryUsesConfirmedRenewalForHeldCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		request := approvalLeaseCall(t)
		request.Lease.ExpiresAt = time.Now().UTC().Add(2 * time.Second)
		request.Authorization.ExpiresAt = request.Lease.ExpiresAt
		registry := &ACPMCPPromptLeaseRegistry{}
		entry := registerApprovalLease(t, registry, t.Context(), request)
		time.Sleep(time.Second)
		renewal := approvalLeaseRenewal(request, time.Now().UTC().Add(5*time.Second))
		renewal.MCPAuthorization.ExpiresAt = time.Now().UTC().Add(3 * time.Second)
		require.NoError(t, entry.renew(renewal))
		time.Sleep(2 * time.Second)
		require.False(t, time.Now().UTC().Before(request.Lease.ExpiresAt))
		require.NoError(t, registry.authorize(request), "an existing approval wait follows the confirmed renewal")
		time.Sleep(time.Second)
		require.ErrorIs(t, registry.authorize(request), errACPMCPPromptLeaseInactive)
	})
}

func TestACPMCPPromptLeaseRegistryFencesRenewalAndRelease(t *testing.T) {
	request := approvalLeaseCall(t)
	registry := &ACPMCPPromptLeaseRegistry{}
	old := registerApprovalLease(t, registry, t.Context(), request)
	renewal := approvalLeaseRenewal(request, time.Now().UTC().Add(3*time.Minute))
	wrongFence := renewal
	wrongFence.Metadata.Fence.SupervisorBootID += "-other"
	require.ErrorIs(t, old.renew(wrongFence), errACPMCPPromptLeaseInactive)
	wrongGeneration := renewal
	wrongGeneration.ExpectedLeaseGeneration++
	require.ErrorIs(t, old.renew(wrongGeneration), errACPMCPPromptLeaseInactive)
	old.release()
	current := registerApprovalLease(t, registry, t.Context(), request)
	require.ErrorIs(t, old.renew(renewal), errACPMCPPromptLeaseInactive)
	old.release()
	require.NoError(t, registry.authorize(request), "old cleanup cannot remove a new registration")
	require.NoError(t, current.renew(renewal))
}

func TestACPMCPPromptLeaseRegistryMissingAuthorityFailsClosed(t *testing.T) {
	request := approvalLeaseCall(t)
	var missing *ACPMCPPromptLeaseRegistry
	_, err := missing.register(t.Context(), request.Namespace, harnessv2.StartPromptRequest{MCPAuthorization: request.Authorization})
	require.ErrorIs(t, err, errACPMCPPromptLeaseInactive)
	require.ErrorIs(t, missing.authorize(request), errACPMCPPromptLeaseInactive)
	require.ErrorIs(t, (&ACPMCPPromptLeaseRegistry{}).authorize(request), errACPMCPPromptLeaseInactive)
	_, err = NewProductionACPMCPBroker(ACPMCPBrokerDependencies{})
	require.ErrorContains(t, err, "prompt lease registry")
	request.Authorization.ApprovalPolicy = harnessv2.MCPApprovalPolicy{}
	entry, err := missing.register(t.Context(), request.Namespace, harnessv2.StartPromptRequest{MCPAuthorization: request.Authorization})
	require.NoError(t, err)
	require.Nil(t, entry, "automatic tools do not require local approval lease tracking")
}

type delayedMCPLeaseAttemptStore struct {
	staticPromptAttemptStore
	delay time.Duration
}

func (s delayedMCPLeaseAttemptStore) GetPromptAttempt(ctx context.Context, id string) (*store.PromptAttempt, error) {
	time.Sleep(s.delay)
	return s.staticPromptAttemptStore.GetPromptAttempt(ctx, id)
}

func TestDurableACPMCPPromptAuthorizerChecksLeaseAfterDurableRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		request := approvalLeaseCall(t)
		request.Authorization.ExpiresAt = time.Now().UTC().Add(time.Second)
		registry := &ACPMCPPromptLeaseRegistry{}
		registerApprovalLease(t, registry, t.Context(), request)
		attempt := &store.PromptAttempt{
			Key: mcpPromptLeaseKey(request.Namespace, request.Metadata), ExecutionState: store.PromptExecutionRunning,
			SessionUID: string(request.Authorization.RuntimeSessionUID), RuntimeInstanceID: string(request.Metadata.Fence.RuntimeInstanceID),
			ControllerEpoch: int64(request.Metadata.Fence.ControllerEpoch),
		}
		attempt.ID, _ = attempt.Key.CanonicalID()
		authorizer := DurableACPMCPPromptAuthorizer{
			Attempts: delayedMCPLeaseAttemptStore{staticPromptAttemptStore{attempt: attempt}, time.Second}, PromptLeases: registry,
		}
		require.ErrorIs(t, authorizer.AuthorizeACPMCPPrompt(t.Context(), request), errACPMCPPromptLeaseInactive)
	})
}
