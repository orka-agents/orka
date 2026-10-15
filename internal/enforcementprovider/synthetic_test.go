package enforcementprovider

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"
)

type synthetic struct {
	d                Descriptor
	priv             ed25519.PrivateKey
	a                Authorization
	rb               ReadBack
	p                Proof
	releaseFailures  int
	released         map[string]ReleaseRequest
	statusState      string
	cleanupErr       error
	prepareRequests  []PrepareRequest
	activateRequests []ActivateRequest
	readBackIDs      []string
	proofIDs         []string
	releaseRequests  []ReleaseRequest
	cleanupRequests  []CleanupRequest
}

func newSynthetic() (*synthetic, TrustProfile, Authorization) {
	d, trust, a, rb, p := fixture()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	d.PublicKey = pub
	trust.PublicKey = pub
	SignDescriptor(&d, priv)
	p.KeyID = d.KeyID
	p.AuthorizationDigest = authorizationDigest(a)
	SignProof(&p, priv)
	return &synthetic{d: d, priv: priv, a: a, rb: rb, p: p, released: map[string]ReleaseRequest{}}, trust, a
}
func (s *synthetic) DiscoverDescriptor() (Descriptor, error) { return s.d, nil }
func (s *synthetic) Prepare(r PrepareRequest) (string, error) {
	s.prepareRequests = append(s.prepareRequests, r)
	if r.Authorization.ID != s.a.ID {
		return "", errors.New("wrong authorization")
	}
	return "prep", nil
}
func (s *synthetic) Activate(r ActivateRequest) (string, error) {
	s.activateRequests = append(s.activateRequests, r)
	return s.rb.ActivationID, nil
}
func (s *synthetic) ReadBack(id string) (ReadBack, error) {
	s.readBackIDs = append(s.readBackIDs, id)
	return s.rb, nil
}
func (s *synthetic) GetProof(id string) (Proof, error) {
	s.proofIDs = append(s.proofIDs, id)
	return s.p, nil
}
func (s *synthetic) Release(r ReleaseRequest) error {
	s.releaseRequests = append(s.releaseRequests, r)
	if s.releaseFailures > 0 {
		s.releaseFailures--
		return errors.New("transient")
	}
	if old, ok := s.released[r.ReleaseID]; ok && old != r {
		return errors.New("release conflict")
	}
	s.released[r.ReleaseID] = r
	return nil
}
func (s *synthetic) GetStatus(string) (Status, error) {
	if s.statusState != "" {
		return Status{State: s.statusState}, nil
	}
	return Status{State: "active"}, nil
}
func (s *synthetic) Reconcile(ReconcileRequest) (Status, error) { return Status{State: "active"}, nil }
func (s *synthetic) Cleanup(r CleanupRequest) error {
	s.cleanupRequests = append(s.cleanupRequests, r)
	return s.cleanupErr
}
func TestHeldUntilDurableProofConsumption(t *testing.T) {
	p, trust, a := newSynthetic()
	o := Orchestrator{Store: NewMemoryStore(), Provider: p, Trust: trust, Now: time.Now}
	if _, e := o.Authorize(a); e != nil {
		t.Fatal(e)
	}
	if _, e := o.Release(a.Context.AttemptID); e == nil {
		t.Fatal("released held workload")
	}
	x, e := o.Activate(a.Context.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	if x.Phase != PhaseProofIssued {
		t.Fatal(x.Phase)
	}
	x, e = o.ConsumeProof(a.Context.AttemptID, x.ProofID)
	if e != nil {
		t.Fatal(e)
	}
	if x.ReleaseID == "" || x.Phase != PhaseProofConsumed {
		t.Fatal("missing atomic release obligation")
	}
	x, e = o.Release(a.Context.AttemptID)
	if e != nil || x.Phase != PhaseReleased {
		t.Fatalf("%v %s", e, x.Phase)
	}
}
func TestReleaseRecoveryAndReplay(t *testing.T) {
	p, trust, a := newSynthetic()
	p.releaseFailures = 1
	o := Orchestrator{Store: NewMemoryStore(), Provider: p, Trust: trust, Now: time.Now}
	if _, err := o.Authorize(a); err != nil {
		t.Fatal(err)
	}
	x, _ := o.Activate(a.Context.AttemptID)
	x, _ = o.ConsumeProof(a.Context.AttemptID, x.ProofID)
	if _, e := o.Release(a.Context.AttemptID); e == nil {
		t.Fatal("expected transient")
	}
	x, e := o.Release(a.Context.AttemptID)
	if e != nil || x.Phase != PhaseReleased {
		t.Fatal(e)
	}
	if _, e = o.ConsumeProof(a.Context.AttemptID, "other"); e == nil {
		t.Fatal("accepted conflicting replay")
	}
}
func TestCrashRecoveryPreservesObligation(t *testing.T) {
	p, trust, a := newSynthetic()
	st := NewMemoryStore()
	o := Orchestrator{Store: st, Provider: p, Trust: trust, Now: time.Now}
	if _, err := o.Authorize(a); err != nil {
		t.Fatal(err)
	}
	x, _ := o.Activate(a.Context.AttemptID)
	x, _ = o.ConsumeProof(a.Context.AttemptID, x.ProofID)
	restarted := Orchestrator{Store: st, Provider: p, Trust: trust, Now: time.Now}
	x, e := restarted.Release(a.Context.AttemptID)
	if e != nil || x.Phase != PhaseReleased {
		t.Fatal(e)
	}
}

func TestUnsupportedCapabilityRejectedBeforePrepare(t *testing.T) {
	p, trust, a := newSynthetic()
	a.Context.RequiredCapabilities = []string{"missing/v1"}
	o := Orchestrator{Store: NewMemoryStore(), Provider: p, Trust: trust, Now: time.Now}
	if _, err := o.Authorize(a); err == nil {
		t.Fatal("unsupported capability reached authorization")
	}
	if _, err := o.Store.Get(a.Context.AttemptID); err == nil {
		t.Fatal("unsupported capability was persisted")
	}
	if len(p.released) != 0 {
		t.Fatal("provider side effect on failed negotiation")
	}
}

type countingProvider struct {
	*synthetic
	discoverCalls int
	prepareCalls  int
}

func (p *countingProvider) DiscoverDescriptor() (Descriptor, error) {
	p.discoverCalls++
	return p.synthetic.DiscoverDescriptor()
}
func (p *countingProvider) Prepare(request PrepareRequest) (string, error) {
	p.prepareCalls++
	return p.synthetic.Prepare(request)
}
func TestNilAndEmptyCapabilitiesFailBeforePersistenceOrProviderCalls(t *testing.T) {
	for name, required := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			base, trust, authorization := newSynthetic()
			provider := &countingProvider{synthetic: base}
			authorization.Context.RequiredCapabilities = required
			store := NewMemoryStore()
			orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
			if _, err := orchestrator.Authorize(authorization); err == nil {
				t.Fatal("authorization accepted missing mandatory capabilities")
			}
			if provider.discoverCalls != 0 || provider.prepareCalls != 0 {
				t.Fatal("provider called before capability validation")
			}
			if _, err := store.Get(authorization.Context.AttemptID); err == nil {
				t.Fatal("invalid authorization persisted")
			}
		})
	}
}
func TestActivationRevalidatesMandatoryCapabilitiesBeforeProviderCalls(t *testing.T) {
	base, trust, authorization := newSynthetic()
	provider := &countingProvider{synthetic: base}
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	current.Context.RequiredCapabilities = nil
	current.Authorization.Context.RequiredCapabilities = nil
	store.attempts[authorization.Context.AttemptID] = current
	beforeDiscover := provider.discoverCalls
	if _, err = orchestrator.Activate(authorization.Context.AttemptID); err == nil {
		t.Fatal("activation accepted missing mandatory capabilities")
	}
	if provider.discoverCalls != beforeDiscover || provider.prepareCalls != 0 {
		t.Fatal("provider called before activation capability validation")
	}
}

type durablePhaseProvider struct {
	*synthetic
	store Store
}

func (p *durablePhaseProvider) requirePhase(want Phase, requirePreparation, requireActivation bool) error {
	attempt, err := p.store.Get(p.a.Context.AttemptID)
	if err != nil {
		return err
	}
	if attempt.Phase != want {
		return fmt.Errorf("provider side effect in phase %s, want %s", attempt.Phase, want)
	}
	if requirePreparation && attempt.PreparationID == "" {
		return errors.New("provider activation before preparation was durable")
	}
	if requireActivation && attempt.ActivationID == "" {
		return errors.New("provider verification before activation was durable")
	}
	return nil
}

func (p *durablePhaseProvider) Prepare(request PrepareRequest) (string, error) {
	if err := p.requirePhase(PhaseProvisioning, false, false); err != nil {
		return "", err
	}
	return p.synthetic.Prepare(request)
}

func (p *durablePhaseProvider) Activate(request ActivateRequest) (string, error) {
	if err := p.requirePhase(PhaseProvisioning, true, false); err != nil {
		return "", err
	}
	return p.synthetic.Activate(request)
}

func (p *durablePhaseProvider) ReadBack(id string) (ReadBack, error) {
	if err := p.requirePhase(PhaseActivatedAndReadBack, true, true); err != nil {
		return ReadBack{}, err
	}
	return p.synthetic.ReadBack(id)
}

func (p *durablePhaseProvider) GetProof(id string) (Proof, error) {
	if err := p.requirePhase(PhaseActivatedAndReadBack, true, true); err != nil {
		return Proof{}, err
	}
	return p.synthetic.GetProof(id)
}

func TestProviderEffectsFollowDurablePhase(t *testing.T) {
	base, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	provider := &durablePhaseProvider{synthetic: base, store: store}
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.Activate(authorization.Context.AttemptID); err != nil {
		t.Fatal(err)
	}
}

type substitutedActivationProvider struct {
	*synthetic
}

func (p *substitutedActivationProvider) Activate(ActivateRequest) (string, error) {
	return "substituted-activation", nil
}

func TestActivationBindsExactProviderResult(t *testing.T) {
	base, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	orchestrator := Orchestrator{
		Store:    store,
		Provider: &substitutedActivationProvider{synthetic: base},
		Trust:    trust,
		Now:      time.Now,
	}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.Activate(authorization.Context.AttemptID); err == nil {
		t.Fatal("accepted proof and readback for a different activation")
	}
}

func TestRecoveryRejectsReleasedHeldAttempt(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	provider.statusState = "released"
	result := orchestrator.Recover([]Attempt{attempt})
	if result[authorization.Context.AttemptID] == nil {
		t.Fatal("accepted released provider state for durably held attempt")
	}
}

type failOnceStore struct {
	Store
	fail   func(Attempt) bool
	failed bool
}

func (s *failOnceStore) CompareAndSwap(id string, version uint64, mutate func(*Attempt) error) (Attempt, error) {
	current, err := s.Get(id)
	if err != nil {
		return Attempt{}, err
	}
	candidate := current
	if err = mutate(&candidate); err != nil {
		return Attempt{}, err
	}
	if !s.failed && s.fail(candidate) {
		s.failed = true
		return Attempt{}, errors.New("injected persistence failure")
	}
	return s.Store.CompareAndSwap(id, version, mutate)
}

func TestActivationRetriesProviderSuccessAfterPersistenceFailure(t *testing.T) {
	tests := []struct {
		name   string
		fail   func(Attempt) bool
		assert func(*testing.T, *synthetic)
	}{
		{
			name: "preparation",
			fail: func(a Attempt) bool {
				return a.Phase == PhaseProvisioning && a.PreparationID != ""
			},
			assert: func(t *testing.T, provider *synthetic) {
				if len(provider.prepareRequests) != 2 || !equalAuthorization(provider.prepareRequests[0].Authorization, provider.prepareRequests[1].Authorization) {
					t.Fatal("prepare retry changed authorization")
				}
			},
		},
		{
			name: "activation",
			fail: func(a Attempt) bool {
				return a.Phase == PhaseActivatedAndReadBack
			},
			assert: func(t *testing.T, provider *synthetic) {
				if len(provider.activateRequests) != 2 || provider.activateRequests[0].PreparationID != provider.activateRequests[1].PreparationID {
					t.Fatal("activation retry changed preparation")
				}
			},
		},
		{
			name: "proof",
			fail: func(a Attempt) bool {
				return a.Phase == PhaseProofIssued
			},
			assert: func(t *testing.T, provider *synthetic) {
				if len(provider.readBackIDs) != 2 || provider.readBackIDs[0] != provider.readBackIDs[1] ||
					len(provider.proofIDs) != 2 || provider.proofIDs[0] != provider.proofIDs[1] {
					t.Fatal("proof retry changed activation")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, trust, authorization := newSynthetic()
			store := &failOnceStore{Store: NewMemoryStore(), fail: test.fail}
			orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
			if _, err := orchestrator.Authorize(authorization); err != nil {
				t.Fatal(err)
			}
			if _, err := orchestrator.Activate(authorization.Context.AttemptID); err == nil {
				t.Fatal("expected injected persistence failure")
			}
			attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			if attempt.Phase != PhaseProofIssued {
				t.Fatalf("phase = %s", attempt.Phase)
			}
			test.assert(t, provider)
		})
	}
}

func TestReleaseRetriesExactDurableObligationAfterPersistenceFailure(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	store := &failOnceStore{
		Store: NewMemoryStore(),
		fail:  func(a Attempt) bool { return a.Phase == PhaseReleased },
	}
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID); err != nil {
		t.Fatal(err)
	}
	if _, err = orchestrator.Release(authorization.Context.AttemptID); err == nil {
		t.Fatal("expected injected persistence failure")
	}
	if _, err = orchestrator.Release(authorization.Context.AttemptID); err != nil {
		t.Fatal(err)
	}
	if len(provider.releaseRequests) != 2 || provider.releaseRequests[0] != provider.releaseRequests[1] {
		t.Fatal("release retry changed durable obligation")
	}
}

func TestRecoveryResumesAdmittedAttemptAfterAuthorizationExpiry(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	admittedAt := time.Now().UTC()
	authorization.IssuedAt = admittedAt.Add(-time.Second)
	authorization.NotBefore = admittedAt.Add(-time.Second)
	authorization.ExpiresAt = admittedAt.Add(time.Second)
	provider.a = authorization
	provider.d.ExpiresAt = admittedAt.Add(time.Hour)
	SignDescriptor(&provider.d, provider.priv)
	provider.p.ExpiresAt = admittedAt.Add(time.Hour)
	provider.p.AuthorizationDigest = authorizationDigest(authorization)
	SignProof(&provider.p, provider.priv)
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: func() time.Time { return admittedAt }}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	restarted := Orchestrator{
		Store:    store,
		Provider: provider,
		Trust:    trust,
		Now:      func() time.Time { return admittedAt.Add(2 * time.Second) },
	}
	result := restarted.Recover([]Attempt{{Context: authorization.Context}})
	if result[authorization.Context.AttemptID] != nil {
		t.Fatal(result)
	}
	attempt, err := store.Get(authorization.Context.AttemptID)
	if err != nil || attempt.Phase != PhaseProofIssued {
		t.Fatalf("attempt = %#v, err = %v", attempt, err)
	}
}

func TestRecoveryUsesAdmittedDescriptorAcrossProviderRollover(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	attempt, err := orchestrator.Authorize(authorization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CompareAndSwap(authorization.Context.AttemptID, attempt.Version, func(a *Attempt) error {
		a.Phase = PhaseProvisioning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	provider.d.Epoch++
	SignDescriptor(&provider.d, provider.priv)
	result := orchestrator.Recover([]Attempt{{Context: authorization.Context}})
	if result[authorization.Context.AttemptID] != nil {
		t.Fatal(result)
	}
	recovered, err := store.Get(authorization.Context.AttemptID)
	if err != nil || recovered.Phase != PhaseProofIssued || recovered.Descriptor.Epoch != authorization.Epoch {
		t.Fatalf("attempt = %#v, err = %v", recovered, err)
	}
}

func TestRecoveryRejectsRevokedAdmittedDescriptor(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	attempt, err := orchestrator.Authorize(authorization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.CompareAndSwap(authorization.Context.AttemptID, attempt.Version, func(a *Attempt) error {
		a.Phase = PhaseProvisioning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	orchestrator.Trust.MinimumEpoch = authorization.Epoch + 1
	result := orchestrator.Recover([]Attempt{{Context: authorization.Context}})
	if result[authorization.Context.AttemptID] == nil {
		t.Fatal("resumed descriptor revoked by operator minimum epoch")
	}
}

func TestProofIssuedAttemptHonorsOperatorRevocation(t *testing.T) {
	tests := map[string]func(*TrustProfile){
		"minimum epoch": func(trust *TrustProfile) {
			trust.MinimumEpoch++
		},
		"replacement key": func(trust *TrustProfile) {
			public, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			trust.PublicKey = public
		},
	}
	for name, revoke := range tests {
		t.Run(name, func(t *testing.T) {
			provider, trust, authorization := newSynthetic()
			store := NewMemoryStore()
			orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
			if _, err := orchestrator.Authorize(authorization); err != nil {
				t.Fatal(err)
			}
			attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			revoke(&orchestrator.Trust)
			if _, err = orchestrator.Activate(authorization.Context.AttemptID); err == nil {
				t.Fatal("Hold retry accepted revoked admitted descriptor")
			}
			result := orchestrator.Recover([]Attempt{attempt})
			if result[authorization.Context.AttemptID] == nil {
				t.Fatal("startup recovery accepted revoked admitted descriptor")
			}
		})
	}
}

func TestProofConsumptionHonorsOperatorRevocation(t *testing.T) {
	tests := map[string]func(*TrustProfile){
		"minimum epoch": func(trust *TrustProfile) {
			trust.MinimumEpoch++
		},
		"replacement key": func(trust *TrustProfile) {
			public, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			trust.PublicKey = public
		},
	}
	for name, revoke := range tests {
		t.Run(name, func(t *testing.T) {
			provider, trust, authorization := newSynthetic()
			store := NewMemoryStore()
			orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
			if _, err := orchestrator.Authorize(authorization); err != nil {
				t.Fatal(err)
			}
			attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			revoke(&orchestrator.Trust)
			if _, err = orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID); err == nil {
				t.Fatal("consumed proof after admitted descriptor revocation")
			}
		})
	}
}

func TestRecoveryCleansReleasedAttemptAndRetainsTombstone(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	store := NewMemoryStore()
	orchestrator := Orchestrator{Store: store, Provider: provider, Trust: trust, Now: time.Now}
	if _, err := orchestrator.Authorize(authorization); err != nil {
		t.Fatal(err)
	}
	attempt, err := orchestrator.Activate(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = orchestrator.ConsumeProof(authorization.Context.AttemptID, attempt.ProofID)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err = orchestrator.Release(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	result := orchestrator.Recover([]Attempt{attempt})
	if result[authorization.Context.AttemptID] != nil {
		t.Fatal(result)
	}
	cleaned, err := store.Get(authorization.Context.AttemptID)
	if err != nil || cleaned.Phase != PhaseCleaned || cleaned.CleanupID == "" || cleaned.ProofDigest == "" {
		t.Fatalf("cleanup lost replay tombstone: %#v, %v", cleaned, err)
	}
	result = orchestrator.Recover([]Attempt{cleaned})
	if result[authorization.Context.AttemptID] != nil {
		t.Fatal(result)
	}
}
