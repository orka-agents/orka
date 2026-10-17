package enforcementprovider

import (
	"crypto/ed25519"
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
	PhaseCleanupPending       Phase = "CleanupPending"
	PhaseCleaned              Phase = "Cleaned"
	PhaseQuarantined          Phase = "Quarantined"
)

// Attempt is durable orchestration state. ReleaseID is written atomically with proof consumption.
type Attempt struct {
	Context                                                                 Context
	Authorization                                                           Authorization
	Descriptor                                                              Descriptor
	Phase                                                                   Phase
	PreparationID, ActivationID, ProofID, ProofDigest, ReleaseID, CleanupID string
	Proof                                                                   Proof
	Version                                                                 uint64
	LastError                                                               string
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
	c = cloneContext(c)
	if old, ok := s.attempts[c.AttemptID]; ok {
		if !equalContext(old.Context, c) {
			return Attempt{}, errors.New("attempt identity conflict")
		}
		return cloneAttempt(old), nil
	}
	a := Attempt{Context: c, Phase: PhaseRequested, Version: 1}
	s.attempts[c.AttemptID] = cloneAttempt(a)
	return cloneAttempt(a), nil
}
func (s *MemoryStore) Get(id string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok {
		return a, ErrAttemptNotFound
	}
	return cloneAttempt(a), nil
}
func (s *MemoryStore) CompareAndSwap(id string, v uint64, fn func(*Attempt) error) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.attempts[id]
	if !ok {
		return a, errors.New("attempt not found")
	}
	if a.Version != v {
		return cloneAttempt(a), errors.New("attempt version conflict")
	}
	candidate := cloneAttempt(a)
	if err := fn(&candidate); err != nil {
		return Attempt{}, err
	}
	candidate = cloneAttempt(candidate)
	candidate.Version++
	prospective := cloneAttempts(s.attempts)
	prospective[id] = candidate
	if err := validateAttempts(prospective); err != nil {
		return Attempt{}, err
	}
	s.attempts[id] = candidate
	return cloneAttempt(candidate), nil
}

type Orchestrator struct {
	Store    Store
	Provider Provider
	Trust    TrustProfile
	Now      func() time.Time
}

type authorizationAdmissionValidator interface {
	validateAuthorizationAdmission(Authorization) error
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
	if validator, ok := o.Provider.(authorizationAdmissionValidator); ok {
		if err := validator.validateAuthorizationAdmission(a); err != nil {
			return Attempt{}, err
		}
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

// verifyPersistedObligations is the single fail-closed gate for persisted
// state that can authorize provider release, status, or cleanup side effects.
// Expiry is intentionally not re-applied after proof consumption: the durable
// obligation remains executable, but every signature, binding, digest, and
// deterministic identifier must still derive from authenticated source fields.
func (o Orchestrator) verifyPersistedObligations(a Attempt) error {
	descriptor, err := o.authenticateAttempt(a)
	if err != nil {
		return fmt.Errorf("authenticate persisted attempt: %w", err)
	}
	if !proofPhase(a.Phase) {
		return nil
	}
	proof := a.Proof
	context := a.Authorization.Context
	if proof.ID == "" || proof.ID != a.ProofID ||
		proof.AuthorizationID != a.Authorization.ID ||
		proof.AuthorizationDigest != authorizationDigest(a.Authorization) ||
		proof.ActivationID == "" || proof.ActivationID != a.ActivationID ||
		proof.WorkloadID != context.WorkloadID || proof.ProviderID != context.ProviderID ||
		proof.Audience != context.Audience || proof.Challenge != context.Challenge ||
		proof.PolicyDigest != context.PolicyDigest || proof.KeyID != descriptor.KeyID ||
		proof.Epoch != descriptor.Epoch || !proof.ExpiresAt.After(proof.IssuedAt) {
		return errors.New("persisted proof binding mismatch")
	}
	expectedProofDigest := stableID("proof-", string(proofMessage(proof)), string(proof.Signature))
	if a.ProofDigest != expectedProofDigest {
		return errors.New("persisted proof digest mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(descriptor.PublicKey), proofMessage(proof), proof.Signature) {
		return errors.New("invalid persisted proof signature")
	}
	if hasReleaseObligation(a) {
		expectedReleaseID := stableID("release-", a.Context.AttemptID, a.Authorization.ID, a.ProofID)
		if a.ReleaseID != expectedReleaseID {
			return errors.New("persisted release obligation mismatch")
		}
	}
	if a.Phase == PhaseCleanupPending || a.Phase == PhaseCleaned {
		expectedCleanupID := cleanupID(a)
		if a.CleanupID != expectedCleanupID {
			return errors.New("persisted cleanup obligation mismatch")
		}
	}
	return nil
}

func (o Orchestrator) Activate(attemptID string) (Attempt, error) {
	for {
		attempt, err := o.Store.Get(attemptID)
		if err != nil {
			return attempt, err
		}
		done, result, err := o.activateStep(attempt)
		if err != nil || done {
			return result, err
		}
	}
}

func (o Orchestrator) activateStep(attempt Attempt) (bool, Attempt, error) {
	switch attempt.Phase {
	case PhaseProofIssued:
		_, err := o.authenticateAttempt(attempt)
		return true, attempt, err
	case PhaseProofConsumed, PhaseReleased, PhaseCleanupPending, PhaseCleaned:
		return true, attempt, nil
	case PhaseAuthorized:
		if _, err := o.authenticateAttempt(attempt); err != nil {
			return true, attempt, err
		}
		_, err := o.Store.CompareAndSwap(attempt.Context.AttemptID, attempt.Version, func(candidate *Attempt) error {
			if candidate.Phase != PhaseAuthorized {
				return fmt.Errorf("cannot start provisioning from %s", candidate.Phase)
			}
			candidate.Phase = PhaseProvisioning
			return nil
		})
		return false, attempt, err
	case PhaseProvisioning:
		return o.activateProvisioning(attempt)
	case PhaseActivatedAndReadBack:
		return o.activateReadBack(attempt)
	default:
		return true, attempt, fmt.Errorf("cannot activate from %s", attempt.Phase)
	}
}

func (o Orchestrator) activateProvisioning(attempt Attempt) (bool, Attempt, error) {
	if _, err := o.authenticateAttempt(attempt); err != nil {
		return true, attempt, err
	}
	if attempt.PreparationID == "" {
		preparationID, err := o.Provider.Prepare(PrepareRequest{Authorization: attempt.Authorization})
		if err != nil {
			return true, attempt, err
		}
		if preparationID == "" {
			return true, attempt, errors.New("provider returned empty preparation ID")
		}
		_, err = o.Store.CompareAndSwap(attempt.Context.AttemptID, attempt.Version, func(candidate *Attempt) error {
			if candidate.Phase != PhaseProvisioning || candidate.PreparationID != "" {
				return errors.New("provisioning preparation state changed")
			}
			candidate.PreparationID = preparationID
			return nil
		})
		return false, attempt, err
	}
	activationID, err := o.Provider.Activate(ActivateRequest{Authorization: attempt.Authorization, PreparationID: attempt.PreparationID})
	if err != nil {
		return true, attempt, err
	}
	if activationID == "" {
		return true, attempt, errors.New("provider returned empty activation ID")
	}
	_, err = o.Store.CompareAndSwap(attempt.Context.AttemptID, attempt.Version, func(candidate *Attempt) error {
		if candidate.Phase != PhaseProvisioning || candidate.PreparationID != attempt.PreparationID || candidate.ActivationID != "" {
			return errors.New("provisioning activation state changed")
		}
		candidate.ActivationID = activationID
		candidate.Phase = PhaseActivatedAndReadBack
		return nil
	})
	return false, attempt, err
}

func (o Orchestrator) activateReadBack(attempt Attempt) (bool, Attempt, error) {
	descriptor, err := o.authenticateAttempt(attempt)
	if err != nil {
		return true, attempt, err
	}
	readback, err := o.Provider.ReadBack(attempt.ActivationID)
	if err != nil {
		return true, attempt, err
	}
	proof, err := o.Provider.GetProof(attempt.ActivationID)
	if err != nil {
		return true, attempt, err
	}
	if err = VerifyProof(proof, attempt.Authorization, descriptor, o.Trust, attempt.ActivationID, readback, o.now()); err != nil {
		return true, attempt, err
	}
	result, err := o.Store.CompareAndSwap(attempt.Context.AttemptID, attempt.Version, func(candidate *Attempt) error {
		if candidate.Phase != PhaseActivatedAndReadBack || candidate.ActivationID != attempt.ActivationID {
			return errors.New("activation verification state changed")
		}
		candidate.ProofID = proof.ID
		candidate.Proof = cloneProof(proof)
		candidate.ProofDigest = stableID("proof-", string(proofMessage(proof)), string(proof.Signature))
		candidate.Phase = PhaseProofIssued
		return nil
	})
	return true, result, err
}

// ConsumeProof atomically records both consumption and the immutable release obligation.
func (o Orchestrator) ConsumeProof(attemptID, proofID string) (Attempt, error) {
	a, err := o.Store.Get(attemptID)
	if err != nil {
		return a, err
	}
	if a.Phase == PhaseProofConsumed || a.Phase == PhaseReleased ||
		((a.Phase == PhaseCleanupPending || a.Phase == PhaseCleaned) && a.ReleaseID != "") {
		if a.ProofID == proofID {
			return a, nil
		}
		return a, errors.New("proof replay conflict")
	}
	if (a.Phase == PhaseCleanupPending || a.Phase == PhaseCleaned) && a.ReleaseID == "" {
		return a, errors.New("proof expired and activation was abandoned")
	}
	if a.Phase != PhaseProofIssued || a.ProofID != proofID {
		return a, errors.New("proof is not releasable")
	}
	if _, err = o.authenticateAttempt(a); err != nil {
		return a, err
	}
	return o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
		return o.consumeProofCandidate(x, proofID)
	})
}

func (o Orchestrator) consumeProofCandidate(x *Attempt, proofID string) error {
	descriptor, err := o.authenticateAttempt(*x)
	if err != nil {
		return err
	}
	if x.Phase != PhaseProofIssued || x.ProofID != proofID {
		return errors.New("proof consumption state changed")
	}
	proof := x.Proof
	context := x.Authorization.Context
	now := o.now()
	if proof.ID != proofID || proof.AuthorizationID != x.Authorization.ID || proof.AuthorizationDigest != authorizationDigest(x.Authorization) ||
		proof.ActivationID != x.ActivationID || proof.WorkloadID != context.WorkloadID || proof.ProviderID != context.ProviderID ||
		proof.Audience != context.Audience || proof.Challenge != context.Challenge || proof.PolicyDigest != context.PolicyDigest ||
		proof.KeyID != descriptor.KeyID || proof.Epoch != descriptor.Epoch || now.Before(proof.IssuedAt) || !now.Before(proof.ExpiresAt) {
		return errors.New("proof is stale or no longer bound to authorization")
	}
	if x.ProofDigest != stableID("proof-", string(proofMessage(proof)), string(proof.Signature)) {
		return errors.New("persisted proof digest mismatch")
	}
	if !ed25519.Verify(ed25519.PublicKey(descriptor.PublicKey), proofMessage(proof), proof.Signature) {
		return errors.New("invalid persisted proof signature")
	}
	x.ReleaseID = stableID("release-", x.Context.AttemptID, x.Authorization.ID, x.ProofID)
	x.Phase = PhaseProofConsumed
	return nil
}

func (o Orchestrator) Release(attemptID string) (Attempt, error) {
	a, err := o.Store.Get(attemptID)
	if err != nil {
		return a, err
	}
	if a.Phase == PhaseReleased ||
		((a.Phase == PhaseCleanupPending || a.Phase == PhaseCleaned) && a.ReleaseID != "") {
		return a, nil
	}
	if (a.Phase == PhaseCleanupPending || a.Phase == PhaseCleaned) && a.ReleaseID == "" {
		return a, errors.New("activation was abandoned without release")
	}
	if a.Phase != PhaseProofConsumed {
		return a, errors.New("workload remains held: proof is not durably consumed")
	}
	if err = o.verifyPersistedObligations(a); err != nil {
		return a, err
	}
	if err = o.Provider.Release(ReleaseRequest{ActivationID: a.ActivationID, AuthorizationID: a.Authorization.ID, ProofID: a.ProofID, ReleaseID: a.ReleaseID}); err != nil {
		return a, err
	}
	return o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error { x.Phase = PhaseReleased; return nil })
}

// Cleanup durably records an idempotent cleanup obligation and retains a replay-safe tombstone.
func (o Orchestrator) Cleanup(attemptID string) (Attempt, error) {
	for {
		a, err := o.Store.Get(attemptID)
		if err != nil {
			return a, err
		}
		switch a.Phase {
		case PhaseCleaned:
			return a, nil
		case PhaseProofIssued:
			if err = o.verifyPersistedObligations(a); err != nil {
				return a, err
			}
			a, err = o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
				if x.Phase != PhaseProofIssued || x.ProofID != a.ProofID {
					return errors.New("cleanup state changed")
				}
				if o.now().Before(x.Proof.ExpiresAt) {
					return errors.New("unconsumed proof has not expired")
				}
				x.CleanupID = cleanupID(*x)
				x.Phase = PhaseCleanupPending
				return nil
			})
			if err != nil {
				return a, err
			}
		case PhaseReleased:
			if err = o.verifyPersistedObligations(a); err != nil {
				return a, err
			}
			a, err = o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
				if x.Phase != PhaseReleased {
					return errors.New("cleanup state changed")
				}
				x.CleanupID = cleanupID(*x)
				x.Phase = PhaseCleanupPending
				return nil
			})
			if err != nil {
				return a, err
			}
		case PhaseCleanupPending:
			if err = o.verifyPersistedObligations(a); err != nil {
				return a, err
			}
			err = o.Provider.Cleanup(CleanupRequest{AttemptID: a.Context.AttemptID, ActivationID: a.ActivationID, CleanupID: a.CleanupID})
			if err != nil && !errors.Is(err, ErrProviderNotFound) {
				return a, err
			}
			return o.Store.CompareAndSwap(attemptID, a.Version, func(x *Attempt) error {
				if x.Phase != PhaseCleanupPending || x.CleanupID != a.CleanupID {
					return errors.New("cleanup obligation changed")
				}
				x.Phase = PhaseCleaned
				return nil
			})
		default:
			return a, errors.New("attempt is not released for cleanup")
		}
	}
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
			current, err = o.Activate(id)
			if err == nil && current.Phase == PhaseProofIssued {
				err = o.verifyHeldAttemptActive(current)
			}
		case PhaseProofConsumed:
			_, err = o.Release(id)
		case PhaseProofIssued:
			if !o.now().Before(current.Proof.ExpiresAt) {
				_, err = o.Cleanup(id)
			} else {
				err = o.verifyHeldAttemptActive(current)
			}
		case PhaseReleased, PhaseCleanupPending:
			_, err = o.Cleanup(id)
		case PhaseCleaned:
			// Replay-safe terminal tombstone; no provider query is required.
		case PhaseRequested:
			// No provider side effect exists yet; authorization may be retried.
		case PhaseQuarantined:
			err = errors.New("quarantined attempt requires operator reconciliation")
		default:
			err = fmt.Errorf("unsupported persisted phase %q", current.Phase)
		}
		results[id] = err
	}
	return results
}

func cleanupID(attempt Attempt) string {
	return stableID("cleanup-", attempt.Context.AttemptID, attempt.ActivationID, attempt.ProofID, attempt.ReleaseID)
}

func (o Orchestrator) verifyHeldAttemptActive(attempt Attempt) error {
	if err := o.verifyPersistedObligations(attempt); err != nil {
		return err
	}
	status, err := o.Provider.GetStatus(attempt.ActivationID)
	if err != nil {
		return err
	}
	if status.State != "active" {
		return fmt.Errorf("provider state %q for held attempt", status.State)
	}
	return nil
}
