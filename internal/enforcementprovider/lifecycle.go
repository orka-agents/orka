package enforcementprovider

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

type Phase string

const (
	PhaseRequested            Phase = "Requested"
	PhaseAuthorized           Phase = "Authorized"
	PhaseProvisioning         Phase = "Provisioning"
	PhaseActivatedAndReadBack Phase = "ActivatedAndReadBack"
	PhaseProofIssued          Phase = "ProofIssued"
	PhaseProofConsumed        Phase = "ProofConsumed"
	PhaseReleased             Phase = "Released"
	PhaseQuarantined          Phase = "Quarantined"
)

// Attempt is durable orchestration state. ReleaseID is written atomically with proof consumption.
type Attempt struct {
	Context                                                      Context
	Authorization                                                Authorization
	Phase                                                        Phase
	PreparationID, ActivationID, ProofID, ProofDigest, ReleaseID string
	Version                                                      uint64
	LastError                                                    string
}
type Store interface {
	Create(Context) (Attempt, error)
	Get(string) (Attempt, error)
	CompareAndSwap(string, uint64, func(*Attempt) error) (Attempt, error)
}

// MemoryStore is the synthetic/test store; production adapters provide the same CAS contract.
type MemoryStore struct {
	mu       sync.Mutex
	attempts map[string]Attempt
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{attempts: map[string]Attempt{}} }
func (s *MemoryStore) Create(c Context) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.attempts[c.AttemptID]; ok {
		if !equalContext(old.Context, c) {
			return Attempt{}, errors.New("attempt identity conflict")
		}
		return old, nil
	}
	a := Attempt{Context: c, Phase: PhaseRequested, Version: 1}
	s.attempts[c.AttemptID] = a
	return a, nil
}
func (s *MemoryStore) Get(id string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok {
		return a, errors.New("attempt not found")
	}
	return a, nil
}
func (s *MemoryStore) CompareAndSwap(id string, v uint64, fn func(*Attempt) error) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok {
		return a, errors.New("attempt not found")
	}
	if a.Version != v {
		return a, errors.New("attempt version conflict")
	}
	if err := fn(&a); err != nil {
		return Attempt{}, err
	}
	a.Version++
	s.attempts[id] = a
	return a, nil
}

type Orchestrator struct {
	Store    Store
	Provider Provider
	Trust    TrustProfile
	Now      func() time.Time
}

func (o Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now().UTC()
}
func (o Orchestrator) Authorize(a Authorization) (Attempt, error) {
	if err := ValidateAuthorization(a, o.now()); err != nil {
		return Attempt{}, err
	}
	// Authenticate provider identity and active epoch before persisting an
	// authorization. Activation repeats the check to close descriptor rollover.
	d, err := o.Provider.DiscoverDescriptor()
	if err != nil {
		return Attempt{}, err
	}
	if err = Negotiate(d, o.Trust, a.Context.RequiredCapabilities, o.now()); err != nil {
		return Attempt{}, err
	}
	if err = bindAuthorizationToDescriptor(a, d, o.Trust); err != nil {
		return Attempt{}, err
	}
	cur, err := o.Store.Create(a.Context)
	if err != nil {
		return Attempt{}, err
	}
	if cur.Phase != PhaseRequested {
		if !equalAuthorization(cur.Authorization, a) {
			return Attempt{}, errors.New("authorization identity conflict")
		}
		return cur, nil
	}
	return o.Store.CompareAndSwap(a.Context.AttemptID, cur.Version, func(x *Attempt) error {
		if x.Phase != PhaseRequested {
			return fmt.Errorf("cannot authorize from %s", x.Phase)
		}
		x.Authorization = a
		x.Phase = PhaseAuthorized
		return nil
	})
}
func (o Orchestrator) Activate(attemptID string) (Attempt, error) {
	a, err := o.Store.Get(attemptID)
	if err != nil {
		return a, err
	}
	if a.Phase == PhaseProofIssued || a.Phase == PhaseProofConsumed || a.Phase == PhaseReleased {
		return a, nil
	}
	if a.Phase != PhaseAuthorized {
		return a, fmt.Errorf("cannot activate from %s", a.Phase)
	}
	if err = ValidateAuthorization(a.Authorization, o.now()); err != nil {
		return a, err
	}
	if !equalContext(a.Context, a.Authorization.Context) {
		return a, errors.New("persisted authorization context mismatch")
	}
	// Negotiate and authenticate before any provider operation can create side effects.
	d, err := o.Provider.DiscoverDescriptor()
	if err != nil {
		return a, err
	}
	if err = Negotiate(d, o.Trust, a.Context.RequiredCapabilities, o.now()); err != nil {
		return a, err
	}
	if err = bindAuthorizationToDescriptor(a.Authorization, d, o.Trust); err != nil {
		return a, err
	}
	prep, err := o.Provider.Prepare(PrepareRequest{Authorization: a.Authorization})
	if err != nil {
		return a, err
	}
	activation, err := o.Provider.Activate(ActivateRequest{Authorization: a.Authorization, PreparationID: prep})
	if err != nil {
		return a, err
	}
	rb, err := o.Provider.ReadBack(activation)
	if err != nil {
		return a, err
	}
	proof, err := o.Provider.GetProof(activation)
	if err != nil {
		return a, err
	}
	if err = VerifyProof(proof, a.Authorization, d, o.Trust, rb, o.now()); err != nil {
		return a, err
	}
	return o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
		x.PreparationID = prep
		x.ActivationID = activation
		x.ProofID = proof.ID
		x.ProofDigest = stableID("proof-", string(proof.Signature))
		x.Phase = PhaseProofIssued
		return nil
	})
}

// ConsumeProof atomically records both consumption and the immutable release obligation.
func (o Orchestrator) ConsumeProof(attemptID, proofID string) (Attempt, error) {
	a, err := o.Store.Get(attemptID)
	if err != nil {
		return a, err
	}
	if a.Phase == PhaseProofConsumed || a.Phase == PhaseReleased {
		if a.ProofID == proofID {
			return a, nil
		}
		return a, errors.New("proof replay conflict")
	}
	if a.Phase != PhaseProofIssued || a.ProofID != proofID {
		return a, errors.New("proof is not releasable")
	}
	return o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
		x.ReleaseID = stableID("release-", x.Context.AttemptID, x.Authorization.ID, x.ProofID)
		x.Phase = PhaseProofConsumed
		return nil
	})
}
func (o Orchestrator) Release(attemptID string) (Attempt, error) {
	a, err := o.Store.Get(attemptID)
	if err != nil {
		return a, err
	}
	if a.Phase == PhaseReleased {
		return a, nil
	}
	if a.Phase != PhaseProofConsumed {
		return a, errors.New("workload remains held: proof is not durably consumed")
	}
	if err = o.Provider.Release(ReleaseRequest{ActivationID: a.ActivationID, AuthorizationID: a.Authorization.ID, ProofID: a.ProofID, ReleaseID: a.ReleaseID}); err != nil {
		return a, err
	}
	return o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error { x.Phase = PhaseReleased; return nil })
}

// Recover resumes durable release obligations and verifies active held attempts.
// It never releases an attempt that has not durably consumed its proof.
func (o Orchestrator) Recover(attempts []Attempt) map[string]error {
	results := make(map[string]error, len(attempts))
	for _, snapshot := range attempts {
		id := snapshot.Context.AttemptID
		current, err := o.Store.Get(id)
		if err != nil {
			results[id] = err
			continue
		}
		switch current.Phase {
		case PhaseProofConsumed:
			_, err = o.Release(id)
		case PhaseProofIssued:
			var status Status
			status, err = o.Provider.GetStatus(current.ActivationID)
			if err == nil && status.State != "active" && status.State != "released" {
				err = fmt.Errorf("provider state %q for held attempt", status.State)
			}
		case PhaseReleased:
			var status Status
			status, err = o.Provider.GetStatus(current.ActivationID)
			if err == nil && status.State != "released" && status.State != "active" {
				err = fmt.Errorf("provider lost released activation: %q", status.State)
			}
		}
		results[id] = err
	}
	return results
}
