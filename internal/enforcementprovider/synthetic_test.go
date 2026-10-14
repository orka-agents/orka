package enforcementprovider

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

type synthetic struct {
	d               Descriptor
	priv            ed25519.PrivateKey
	a               Authorization
	rb              ReadBack
	p               Proof
	releaseFailures int
	released        map[string]ReleaseRequest
}

func newSynthetic(t *testing.T) (*synthetic, TrustProfile, Authorization) {
	d, trust, a, rb, p := fixture(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	d.PublicKey = pub
	trust.PublicKey = pub
	SignDescriptor(&d, priv)
	p.KeyID = d.KeyID
	SignProof(&p, priv)
	return &synthetic{d: d, priv: priv, a: a, rb: rb, p: p, released: map[string]ReleaseRequest{}}, trust, a
}
func (s *synthetic) DiscoverDescriptor() (Descriptor, error) { return s.d, nil }
func (s *synthetic) Prepare(r PrepareRequest) (string, error) {
	if r.Authorization.ID != s.a.ID {
		return "", errors.New("wrong authorization")
	}
	return "prep", nil
}
func (s *synthetic) Activate(r ActivateRequest) (string, error) { return s.rb.ActivationID, nil }
func (s *synthetic) ReadBack(string) (ReadBack, error)          { return s.rb, nil }
func (s *synthetic) GetProof(string) (Proof, error)             { return s.p, nil }
func (s *synthetic) Release(r ReleaseRequest) error {
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
func (s *synthetic) GetStatus(string) (Status, error)           { return Status{State: "active"}, nil }
func (s *synthetic) Reconcile(ReconcileRequest) (Status, error) { return Status{State: "active"}, nil }
func (s *synthetic) Cleanup(CleanupRequest) error               { return nil }
func TestHeldUntilDurableProofConsumption(t *testing.T) {
	p, trust, a := newSynthetic(t)
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
	p, trust, a := newSynthetic(t)
	p.releaseFailures = 1
	o := Orchestrator{Store: NewMemoryStore(), Provider: p, Trust: trust, Now: time.Now}
	o.Authorize(a)
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
	p, trust, a := newSynthetic(t)
	st := NewMemoryStore()
	o := Orchestrator{Store: st, Provider: p, Trust: trust, Now: time.Now}
	o.Authorize(a)
	x, _ := o.Activate(a.Context.AttemptID)
	x, _ = o.ConsumeProof(a.Context.AttemptID, x.ProofID)
	restarted := Orchestrator{Store: st, Provider: p, Trust: trust, Now: time.Now}
	x, e := restarted.Release(a.Context.AttemptID)
	if e != nil || x.Phase != PhaseReleased {
		t.Fatal(e)
	}
}

func TestUnsupportedCapabilityRejectedBeforePrepare(t *testing.T) {
	p, trust, a := newSynthetic(t)
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
			base, trust, authorization := newSynthetic(t)
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
	base, trust, authorization := newSynthetic(t)
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
