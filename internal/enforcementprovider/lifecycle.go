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
	Descriptor                                                   Descriptor
	Phase                                                        Phase
	PreparationID, ActivationID, ProofID, ProofDigest, ReleaseID string
	Version                                                      uint64
	LastError                                                    string
}

var ErrAttemptNotFound = errors.New("attempt not found")

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
		return a, ErrAttemptNotFound
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
	if err := validateAuthorizationContract(a); err != nil {
		return Attempt{}, err
	}
	if current, err := o.Store.Get(a.Context.AttemptID); err == nil {
		if current.Phase != PhaseRequested {
			if !equalAuthorization(current.Authorization, a) {
				return Attempt{}, errors.New("authorization identity conflict")
			}
			return current, nil
		}
	} else if !errors.Is(err, ErrAttemptNotFound) {
		return Attempt{}, err
	}
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
		x.Descriptor = d
		x.Phase = PhaseAuthorized
		return nil
	})
}

func (o Orchestrator) authenticateAttempt(a Attempt) (Descriptor, error) {
	if !equalContext(a.Context, a.Authorization.Context) {
		return Descriptor{}, errors.New("persisted authorization context mismatch")
	}
	if err := validateAuthorizationContract(a.Authorization); err != nil {
		return Descriptor{}, err
	}
	if err := negotiateAdmittedDescriptor(a.Descriptor, o.Trust, a.Context.RequiredCapabilities); err != nil {
		return Descriptor{}, err
	}
	if err := bindAuthorizationToDescriptor(a.Authorization, a.Descriptor, o.Trust); err != nil {
		return Descriptor{}, err
	}
	return a.Descriptor, nil
}

func (o Orchestrator) Activate(attemptID string) (Attempt, error) {
	for {
		a, err := o.Store.Get(attemptID)
		if err != nil {
			return a, err
		}
		switch a.Phase {
		case PhaseProofIssued:
			if _, err = o.authenticateAttempt(a); err != nil {
				return a, err
			}
			return a, nil
		case PhaseProofConsumed, PhaseReleased:
			return a, nil
		case PhaseAuthorized:
			if _, err = o.authenticateAttempt(a); err != nil {
				return a, err
			}
			if _, err = o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
				if x.Phase != PhaseAuthorized {
					return fmt.Errorf("cannot start provisioning from %s", x.Phase)
				}
				x.Phase = PhaseProvisioning
				return nil
			}); err != nil {
				return a, err
			}
		case PhaseProvisioning:
			if _, err = o.authenticateAttempt(a); err != nil {
				return a, err
			}
			if a.PreparationID == "" {
				preparationID, prepareErr := o.Provider.Prepare(PrepareRequest{Authorization: a.Authorization})
				if prepareErr != nil {
					return a, prepareErr
				}
				if preparationID == "" {
					return a, errors.New("provider returned empty preparation ID")
				}
				if _, err = o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
					if x.Phase != PhaseProvisioning || x.PreparationID != "" {
						return errors.New("provisioning preparation state changed")
					}
					x.PreparationID = preparationID
					return nil
				}); err != nil {
					return a, err
				}
				continue
			}
			activationID, activateErr := o.Provider.Activate(ActivateRequest{Authorization: a.Authorization, PreparationID: a.PreparationID})
			if activateErr != nil {
				return a, activateErr
			}
			if activationID == "" {
				return a, errors.New("provider returned empty activation ID")
			}
			if _, err = o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
				if x.Phase != PhaseProvisioning || x.PreparationID != a.PreparationID || x.ActivationID != "" {
					return errors.New("provisioning activation state changed")
				}
				x.ActivationID = activationID
				x.Phase = PhaseActivatedAndReadBack
				return nil
			}); err != nil {
				return a, err
			}
		case PhaseActivatedAndReadBack:
			descriptor, authErr := o.authenticateAttempt(a)
			if authErr != nil {
				return a, authErr
			}
			readback, readErr := o.Provider.ReadBack(a.ActivationID)
			if readErr != nil {
				return a, readErr
			}
			proof, proofErr := o.Provider.GetProof(a.ActivationID)
			if proofErr != nil {
				return a, proofErr
			}
			if err = VerifyProof(proof, a.Authorization, descriptor, o.Trust, a.ActivationID, readback, o.now()); err != nil {
				return a, err
			}
			return o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
				if x.Phase != PhaseActivatedAndReadBack || x.ActivationID != a.ActivationID {
					return errors.New("activation verification state changed")
				}
				x.ProofID = proof.ID
				x.ProofDigest = stableID("proof-", string(proof.Signature))
				x.Phase = PhaseProofIssued
				return nil
			})
		default:
			return a, fmt.Errorf("cannot activate from %s", a.Phase)
		}
	}
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
	if _, err = o.authenticateAttempt(a); err != nil {
		return a, err
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
		case PhaseAuthorized, PhaseProvisioning, PhaseActivatedAndReadBack:
			_, err = o.Activate(id)
		case PhaseProofConsumed:
			_, err = o.Release(id)
		case PhaseProofIssued:
			if _, err = o.authenticateAttempt(current); err != nil {
				break
			}
			var status Status
			status, err = o.Provider.GetStatus(current.ActivationID)
			if err == nil && status.State != "active" {
				err = fmt.Errorf("provider state %q for held attempt", status.State)
			}
		case PhaseReleased:
			var status Status
			status, err = o.Provider.GetStatus(current.ActivationID)
			if err == nil && status.State != "released" {
				err = fmt.Errorf("provider lost released activation: %q", status.State)
			}
		}
		results[id] = err
	}
	return results
}
