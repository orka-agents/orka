package publisher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

func (p *Publisher) Verify(ctx context.Context, request VerifyRequest) (VerificationReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prepared, err := p.validateVerifyRequest(request)
	if err != nil {
		return VerificationReceipt{}, err
	}
	requestDigest, err := verifyRequestDigest(request)
	if err != nil {
		return VerificationReceipt{}, err
	}
	receiptPath, err := p.operationPath(request.PublicationID, "verify", request.OperationID)
	if err != nil {
		return VerificationReceipt{}, err
	}
	var existing VerificationReceipt
	if err := readCanonical(receiptPath, &existing); err == nil {
		if existing.RequestDigest != requestDigest {
			return VerificationReceipt{}, operationError(ErrIdempotencyConflict, "verify publication", "operation ID already has a different request digest", nil)
		}
		return existing, verificationReceiptError(existing)
	} else if !os.IsNotExist(err) {
		return VerificationReceipt{}, operationError(ErrPreparedArtifactCorrupt, "read verification receipt", "", err)
	}
	var lastError error
	for attempt := 0; attempt < p.verifyAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return VerificationReceipt{}, err
		}
		receipt, verifyErr := p.verifyOnce(ctx, request, prepared, requestDigest)
		if verifyErr == nil {
			if err := writeCanonicalDurable(receiptPath, receipt); err != nil {
				return VerificationReceipt{}, fmt.Errorf("persist verification receipt: %w", err)
			}
			return receipt, verificationReceiptError(receipt)
		}
		lastError = verifyErr
		if attempt+1 < p.verifyAttempts {
			timer := time.NewTimer(p.verifyBackoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return VerificationReceipt{}, ctx.Err()
			case <-timer.C:
			}
		}
	}
	receipt := VerificationReceipt{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration,
		OperationID: request.OperationID, RequestDigest: requestDigest, Outcome: PublicationOutcomeUnknown,
		TargetRepositoryID: request.Target.ID, TargetRef: request.TargetRef,
		ExpectedCommitOID: request.ExpectedCommitOID,
	}
	if err := writeCanonicalDurable(receiptPath, receipt); err != nil {
		return receipt, operationError(ErrVerificationUnknown, "persist unknown verification receipt", "", errors.Join(lastError, err))
	}
	return receipt, operationError(ErrVerificationUnknown, "verify remote publication", "bounded read-only observation failed", lastError)
}

func (p *Publisher) verifyOnce(ctx context.Context, request VerifyRequest, prepared PreparedPublication, requestDigest string) (VerificationReceipt, error) {
	box, err := p.newSandbox("verify")
	if err != nil {
		return VerificationReceipt{}, err
	}
	defer box.Close() //nolint:errcheck
	observed, err := p.observeRef(ctx, box, request.Target, request.TargetRef)
	if err != nil {
		return VerificationReceipt{}, err
	}
	receipt := VerificationReceipt{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration,
		OperationID: request.OperationID, RequestDigest: requestDigest,
		TargetRepositoryID: request.Target.ID, TargetRef: request.TargetRef,
		ExpectedCommitOID: request.ExpectedCommitOID, ObservedRemote: observed,
	}
	if observed.Absent {
		receipt.Outcome = DeliveryConflict
		return receipt, nil
	}
	if observed.OID == request.ExpectedCommitOID {
		receipt.Outcome = VerifiedExact
		return receipt, nil
	}
	repositoryPath, err := p.importBundle(ctx, box, prepared)
	if err != nil {
		return VerificationReceipt{}, err
	}
	if err := p.fetchObserved(ctx, box, repositoryPath, request.Target, request.TargetRef, observed.OID); err != nil {
		return VerificationReceipt{}, err
	}
	descendant, err := p.isAncestor(ctx, box, repositoryPath, request.ExpectedCommitOID, observed.OID)
	if err != nil {
		return VerificationReceipt{}, err
	}
	if !descendant {
		receipt.Outcome = DeliveryConflict
		return receipt, nil
	}
	proof, err := p.descendantProof(ctx, box, repositoryPath, request.ExpectedCommitOID, observed.OID)
	if err != nil {
		return VerificationReceipt{}, err
	}
	receipt.Outcome = DeliveredSuperseded
	receipt.DescendantProofDigest = proof
	return receipt, nil
}

func (p *Publisher) validateVerifyRequest(request VerifyRequest) (PreparedPublication, error) {
	if err := validateIdentifier("publication ID", request.PublicationID); err != nil {
		return PreparedPublication{}, err
	}
	if request.PublicationGeneration < 1 {
		return PreparedPublication{}, invalid("publication generation", "must be at least 1")
	}
	if err := validateIdentifier("operation ID", request.OperationID); err != nil {
		return PreparedPublication{}, err
	}
	if err := validateRepository(request.Target); err != nil {
		return PreparedPublication{}, err
	}
	if err := validateBranchRef(request.TargetRef); err != nil {
		return PreparedPublication{}, err
	}
	if err := validateObjectID("expected commit", request.ExpectedCommitOID); err != nil {
		return PreparedPublication{}, err
	}
	if err := validateDigest("bundle digest", request.BundleDigest); err != nil {
		return PreparedPublication{}, err
	}
	prepared, err := p.loadPrepared(request.PublicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if prepared.PublicationGeneration != request.PublicationGeneration || prepared.Target != request.Target ||
		prepared.TargetRef != request.TargetRef || prepared.CommitOID != request.ExpectedCommitOID || prepared.BundleDigest != request.BundleDigest {
		return PreparedPublication{}, operationError(ErrIdempotencyConflict, "validate verify request", "request differs from durable prepared artifact", nil)
	}
	if err := validateClaim(request.Claim, request.Target, request.TargetRef, prepared.RemoteBefore, prepared.BranchClaimGeneration); err != nil {
		return PreparedPublication{}, err
	}
	return prepared, nil
}

func verifyRequestDigest(request VerifyRequest) (string, error) {
	return digestCanonical(struct {
		Domain                string      `json:"domain"`
		PublicationID         string      `json:"publicationId"`
		PublicationGeneration int64       `json:"publicationGeneration"`
		Target                Repository  `json:"target"`
		TargetRef             string      `json:"targetRef"`
		Claim                 BranchClaim `json:"claim"`
		ExpectedCommitOID     string      `json:"expectedCommitOid"`
		BundleDigest          string      `json:"bundleDigest"`
	}{
		Domain: "orka.publisher.verify.v1", PublicationID: request.PublicationID,
		PublicationGeneration: request.PublicationGeneration, Target: request.Target, TargetRef: request.TargetRef,
		Claim: request.Claim, ExpectedCommitOID: request.ExpectedCommitOID, BundleDigest: request.BundleDigest,
	})
}

func (p *Publisher) descendantProof(ctx context.Context, box *sandbox, repositoryPath, expected, observed string) (string, error) {
	result, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "rev-list", "--topo-order", "--reverse", expected+".."+observed)
	if err != nil {
		return "", err
	}
	commits := strings.Fields(result.stdout)
	if len(commits) == 0 || commits[len(commits)-1] != observed {
		return "", fmt.Errorf("descendant proof does not terminate at observed commit")
	}
	return digestCanonical(struct {
		Domain   string   `json:"domain"`
		Expected string   `json:"expected"`
		Observed string   `json:"observed"`
		Commits  []string `json:"commits"`
	}{Domain: "orka.publisher.descendant-proof.v1", Expected: expected, Observed: observed, Commits: commits})
}

func verificationReceiptError(receipt VerificationReceipt) error {
	if receipt.Outcome == PublicationOutcomeUnknown {
		return ErrVerificationUnknown
	}
	return nil
}
