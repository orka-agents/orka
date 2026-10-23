package enforcementprovider

import (
	"errors"
	"fmt"
)

type identityOwners struct{ authorizations, proofs, releases map[string]string }

func validateAttempts(attempts map[string]Attempt) error {
	owners := identityOwners{map[string]string{}, map[string]string{}, map[string]string{}}
	for key, attempt := range attempts {
		if err := validateAttempt(key, attempt); err != nil {
			return err
		}
		if err := owners.add(key, attempt); err != nil {
			return err
		}
	}
	return nil
}
func validateAttempt(key string, attempt Attempt) error {
	if key == "" || attempt.Context.AttemptID != key || attempt.Version == 0 {
		return errors.New("invalid attempt identity or version")
	}
	switch attempt.Phase {
	case PhaseRequested:
	case PhaseAuthorized, PhaseProvisioning, PhaseActivatedAndReadBack, PhaseProofIssued, PhaseProofConsumed, PhaseReleased, PhaseCleanupPending, PhaseCleaned, PhaseQuarantined:
		if attempt.Authorization.ID == "" || !equalContext(attempt.Context, attempt.Authorization.Context) {
			return fmt.Errorf("attempt %q has invalid authorization", key)
		}
	default:
		return fmt.Errorf("attempt %q has unsupported phase %q", key, attempt.Phase)
	}
	if proofPhase(attempt.Phase) && (attempt.ActivationID == "" || attempt.ProofID == "" || attempt.Proof.ID != attempt.ProofID || attempt.ProofDigest == "") {
		return fmt.Errorf("attempt %q has incomplete proof state", key)
	}
	if releasePhase(attempt.Phase) && attempt.ReleaseID == "" {
		return fmt.Errorf("attempt %q has incomplete release obligation", key)
	}
	if (attempt.Phase == PhaseCleanupPending || attempt.Phase == PhaseCleaned) && attempt.CleanupID == "" {
		return fmt.Errorf("attempt %q has incomplete cleanup obligation", key)
	}
	if (attempt.Phase == PhaseCleanupPending || attempt.Phase == PhaseCleaned) &&
		attempt.ReleaseID == "" && attempt.ProofID == "" {
		return fmt.Errorf("attempt %q has incomplete abandonment obligation", key)
	}
	return nil
}
func proofPhase(phase Phase) bool {
	return phase == PhaseProofIssued || phase == PhaseProofConsumed || phase == PhaseReleased || phase == PhaseCleanupPending || phase == PhaseCleaned
}
func releasePhase(phase Phase) bool {
	return phase == PhaseProofConsumed || phase == PhaseReleased
}
func hasReleaseObligation(attempt Attempt) bool {
	return releasePhase(attempt.Phase) ||
		((attempt.Phase == PhaseCleanupPending || attempt.Phase == PhaseCleaned) && attempt.ReleaseID != "")
}
func (owners identityOwners) add(key string, attempt Attempt) error {
	if attempt.Authorization.ID != "" {
		if err := claimIdentity(owners.authorizations, attempt.Authorization.ID, key, "authorization"); err != nil {
			return err
		}
	}
	if proofPhase(attempt.Phase) {
		if err := claimIdentity(owners.proofs, attempt.ProofDigest, key, "proof"); err != nil {
			return err
		}
	}
	if hasReleaseObligation(attempt) {
		if err := claimIdentity(owners.releases, attempt.ReleaseID, key, "release"); err != nil {
			return err
		}
	}
	return nil
}
func claimIdentity(owners map[string]string, id, key, kind string) error {
	if owner, ok := owners[id]; ok && owner != key {
		return fmt.Errorf("%s %q reused by attempts %q and %q", kind, id, owner, key)
	}
	owners[id] = key
	return nil
}
