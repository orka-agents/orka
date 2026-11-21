package enforcementprovider

import (
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func waitForExternalRelease(t *testing.T) {
	t.Helper()
	activeFIFO := os.Getenv("ORKA_PROVIDER_TEST_ACTIVE_FIFO")
	if activeFIFO == "" {
		return
	}
	releaseFIFO := os.Getenv("ORKA_PROVIDER_TEST_RELEASE_FIFO")
	if releaseFIFO == "" {
		t.Fatal("release FIFO is required with active FIFO")
	}
	active, err := os.OpenFile(activeFIFO, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = active.WriteString("active\n"); err != nil {
		_ = active.Close()
		t.Fatal(err)
	}
	if err = active.Close(); err != nil {
		t.Fatal(err)
	}
	release, err := os.Open(releaseFIFO)
	if err != nil {
		t.Fatal(err)
	}
	var signal [1]byte
	if _, err = release.Read(signal[:]); err != nil {
		_ = release.Close()
		t.Fatal(err)
	}
	if err = release.Close(); err != nil {
		t.Fatal(err)
	}
}

func restartExternalAttempt(t *testing.T, store *FileStore, storePath string, provider Provider, trust TrustProfile, attempt Attempt, expected Phase) (*FileStore, Attempt) {
	t.Helper()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restartedStore, err := NewFileStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	restarted := Orchestrator{Store: restartedStore, Provider: provider, Trust: trust, Now: time.Now}
	results := restarted.Recover([]Attempt{attempt})
	if err = results[attempt.Context.AttemptID]; err != nil {
		_ = restartedStore.Close()
		t.Fatal(err)
	}
	attempt, err = restartedStore.Get(attempt.Context.AttemptID)
	if err != nil {
		_ = restartedStore.Close()
		t.Fatal(err)
	}
	if attempt.Phase != expected {
		_ = restartedStore.Close()
		t.Fatalf("phase = %s, want %s", attempt.Phase, expected)
	}
	return restartedStore, attempt
}

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
	storePath := secureStorePath(t, "orka-enforcement.json")
	store, err := NewFileStore(storePath)
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
	policy, err := hex.DecodeString(os.Getenv("ORKA_PROVIDER_TEST_POLICY_HEX"))
	if err != nil {
		t.Fatal(err)
	}
	wantPolicyDigest := os.Getenv("ORKA_PROVIDER_TEST_POLICY_DIGEST")
	if got := PolicyDigest(policy); got != wantPolicyDigest {
		t.Fatalf("policy digest mismatch: got %q want %q", got, wantPolicyDigest)
	}
	attemptID := os.Getenv("ORKA_PROVIDER_TEST_ATTEMPT_ID")
	if attemptID == "" {
		attemptID = "attempt"
	}
	workloadID := os.Getenv("ORKA_PROVIDER_TEST_WORKLOAD_ID")
	if workloadID == "" {
		workloadID = "workload"
	}
	ctx := Context{RequestID: "req:" + attemptID, AttemptID: attemptID, WorkloadID: workloadID, ProviderID: descriptor.ProviderID, Audience: "orka", Challenge: "challenge:" + attemptID, IdempotencyKey: "idem:" + attemptID, PolicyBytes: policy, RequiredCapabilities: []string{"filesystem/deny-write/v1"}}
	ctx.PolicyDigest = wantPolicyDigest
	auth := Authorization{ID: "auth:" + attemptID, Context: ctx, IssuedAt: now.Add(-time.Second), NotBefore: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute), Epoch: descriptor.Epoch}
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
	waitForExternalRelease(t)
	restartedStore, a := restartExternalAttempt(t, store, storePath, provider, trust, a, PhaseReleased)
	cleanupStore, _ := restartExternalAttempt(t, restartedStore, storePath, provider, trust, a, PhaseCleaned)
	if err = cleanupStore.Close(); err != nil {
		t.Fatal(err)
	}
}
