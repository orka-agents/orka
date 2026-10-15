package enforcementprovider

import (
	"os"
	"testing"
	"time"
)

// Installed temporarily into Orka's package by the handoff verification script.
// It exercises Orka's actual Unix provider client against a configured server.
func TestExternalProviderEndToEnd(t *testing.T) {
	socket := os.Getenv("ORKA_PROVIDER_TEST_SOCKET")
	if socket == "" {
		t.Skip("external provider socket not configured")
	}
	trustPath := os.Getenv("ORKA_PROVIDER_TEST_TRUST")
	b, err := os.ReadFile(trustPath)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := ParseTrustProfileJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(secureStorePath(t, "orka-enforcement.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider := NewUnixProvider(socket, uint32(os.Getuid()), 3*time.Second)
	descriptor, err := provider.DiscoverDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err = AuthenticateDescriptor(descriptor, trust, time.Now()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	policy := []byte(`{"version":"agentsuite.policy/v1","denyWritePaths":["/workspace/source"],"requiredCapabilities":["filesystem/deny-write/v1"]}`)
	ctx := Context{RequestID: "req", AttemptID: "attempt", WorkloadID: "workload", ProviderID: descriptor.ProviderID, Audience: "orka", Challenge: "challenge", IdempotencyKey: "idem", PolicyBytes: policy, RequiredCapabilities: []string{"filesystem/deny-write/v1"}}
	ctx.PolicyDigest = PolicyDigest(policy)
	auth := Authorization{ID: "auth", Context: ctx, IssuedAt: now.Add(-time.Second), NotBefore: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute), Epoch: descriptor.Epoch}
	o := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err = o.Authorize(auth); err != nil {
		t.Fatal(err)
	}
	a, err := o.Activate(ctx.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	a, err = o.ConsumeProof(ctx.AttemptID, a.ProofID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Phase != PhaseProofConsumed {
		t.Fatal(a.Phase)
	}
	a, err = o.Release(ctx.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Phase != PhaseReleased {
		t.Fatal(a.Phase)
	}
}
