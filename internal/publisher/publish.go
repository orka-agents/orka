package publisher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type publishJournal struct {
	PublicationID string          `json:"publicationId"`
	OperationID   string          `json:"operationId"`
	RequestDigest string          `json:"requestDigest"`
	Started       bool            `json:"started"`
	Receipt       *PublishReceipt `json:"receipt,omitempty"`
}

// Preflight independently checks the current target against the exact durable
// BranchClaim baseline. Any movement fails closed; a descendant is not accepted
// as permission to publish.
func (p *Publisher) Preflight(ctx context.Context, request PreflightRequest) (PreflightResult, error) {
	if err := validateRepository(request.Target); err != nil {
		return PreflightResult{}, err
	}
	if err := validateBranchRef(request.Claim.Ref); err != nil {
		return PreflightResult{}, err
	}
	if err := validateRemoteRef("branch claim baseline", request.Claim.LastVerified); err != nil {
		return PreflightResult{}, err
	}
	if err := validateClaim(request.Claim, request.Target, request.Claim.Ref, request.Claim.LastVerified, request.Claim.Generation); err != nil {
		return PreflightResult{}, err
	}
	box, err := p.newSandbox("preflight")
	if err != nil {
		return PreflightResult{}, err
	}
	defer box.Close() //nolint:errcheck
	observed, err := p.observeRef(ctx, box, request.Target, request.Claim.Ref)
	if err != nil {
		return PreflightResult{}, err
	}
	result := PreflightResult{Expected: request.Claim.LastVerified, Observed: observed, Matches: observed.Equal(request.Claim.LastVerified)}
	if !result.Matches {
		return result, operationError(ErrBranchMoved, "preflight publication branch", fmt.Sprintf("expected %s, observed %s", formatRemoteRef(result.Expected), formatRemoteRef(result.Observed)), nil)
	}
	return result, nil
}

// Publish crosses the cancellation boundary only after the exact preflight,
// fast-forward proof, and durable Started journal are complete. Once crossed,
// caller cancellation does not kill an in-flight push; the bounded transport
// either settles or returns PublicationOutcomeUnknown for Verify to reconcile.
func (p *Publisher) Publish(ctx context.Context, request PublishRequest) (PublishReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prepared, err := p.validatePublishRequest(request)
	if err != nil {
		return PublishReceipt{}, err
	}
	requestDigest, err := publishRequestDigest(request)
	if err != nil {
		return PublishReceipt{}, err
	}
	journalPath, err := p.operationPath(request.PublicationID, "publish", request.OperationID)
	if err != nil {
		return PublishReceipt{}, err
	}
	journal, exists, err := loadPublishJournal(journalPath)
	if err != nil {
		return PublishReceipt{}, err
	}
	if exists {
		if journal.RequestDigest != requestDigest {
			return PublishReceipt{}, operationError(ErrIdempotencyConflict, "publish publication", "operation ID already has a different request digest", nil)
		}
		if journal.Receipt != nil {
			return *journal.Receipt, publishReceiptError(*journal.Receipt)
		}
	} else {
		journal = publishJournal{PublicationID: request.PublicationID, OperationID: request.OperationID, RequestDigest: requestDigest}
		if err := writeCanonicalDurable(journalPath, journal); err != nil {
			return PublishReceipt{}, fmt.Errorf("persist publish intent: %w", err)
		}
	}
	box, err := p.newSandbox("publish")
	if err != nil {
		return PublishReceipt{}, err
	}
	defer box.Close() //nolint:errcheck
	repositoryPath, err := p.importBundle(ctx, box, prepared)
	if err != nil {
		return PublishReceipt{}, err
	}
	if !request.RemoteBefore.Absent {
		ancestor, ancestryErr := p.isAncestor(ctx, box, repositoryPath, request.RemoteBefore.OID, request.ExpectedCommitOID)
		if ancestryErr != nil || !ancestor {
			return PublishReceipt{}, operationError(ErrCASRejected, "prove fast-forward publication", "expected commit is not a verified descendant of remote-before", ancestryErr)
		}
	}
	observed, err := p.observeRef(ctx, box, request.Target, request.TargetRef)
	if err != nil {
		return PublishReceipt{}, err
	}
	if observed.OID == request.ExpectedCommitOID {
		receipt := p.newPublishReceipt(request, requestDigest, PublishAlreadyExact, observed)
		return p.persistPublishReceipt(journalPath, journal, receipt)
	}
	if !observed.Equal(request.RemoteBefore) {
		receipt := p.newPublishReceipt(request, requestDigest, PublishCASRejected, observed)
		persisted, persistErr := p.persistPublishReceipt(journalPath, journal, receipt)
		if persistErr != nil {
			return persisted, persistErr
		}
		return persisted, operationError(ErrBranchMoved, "publish branch preflight", fmt.Sprintf("expected %s, observed %s", formatRemoteRef(request.RemoteBefore), formatRemoteRef(observed)), nil)
	}
	if err := ctx.Err(); err != nil {
		return PublishReceipt{}, err
	}
	journal.Started = true
	if err := writeCanonicalDurable(journalPath, journal); err != nil {
		return PublishReceipt{}, fmt.Errorf("persist publication cancellation boundary: %w", err)
	}
	if p.beforeCAS != nil {
		if err := p.beforeCAS(ctx, request); err != nil {
			return PublishReceipt{}, err
		}
	}
	mutationContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.publishTimeout)
	defer cancel()
	lease := "--force-with-lease=" + request.TargetRef + ":"
	if !request.RemoteBefore.Absent {
		lease += request.RemoteBefore.OID
	}
	refspec := "refs/orka/prepared:" + request.TargetRef
	pushResult, pushErr := p.runGit(mutationContext, box, box.root, nil, nil,
		"--git-dir="+repositoryPath, "push", "--porcelain", "--no-verify", "--no-recurse-submodules",
		lease, "--", request.Target.URL, refspec)
	if pushErr == nil && p.afterPush != nil {
		pushErr = p.afterPush(mutationContext, request)
	}
	if pushErr == nil {
		receipt := p.newPublishReceipt(request, requestDigest, PublishAcknowledged, RemoteRef{OID: request.ExpectedCommitOID})
		persisted, persistErr := p.persistPublishReceipt(journalPath, journal, receipt)
		if persistErr != nil {
			return persisted, operationError(ErrPublicationUnknown, "persist publish acknowledgement", "remote mutation may have succeeded", persistErr)
		}
		return persisted, nil
	}
	observedAfter, observeErr := p.observeAfterMutation(request)
	outcome := PublishOutcomeUnknown
	classification := ErrPublicationUnknown
	if pushDefinitivelyRejected(pushResult) {
		outcome = PublishCASRejected
		classification = ErrCASRejected
	}
	receipt := p.newPublishReceipt(request, requestDigest, outcome, observedAfter)
	persisted, persistErr := p.persistPublishReceipt(journalPath, journal, receipt)
	if persistErr != nil {
		return persisted, operationError(ErrPublicationUnknown, "persist failed publish receipt", "remote outcome requires verification", errors.Join(pushErr, observeErr, persistErr))
	}
	return persisted, operationError(classification, "publish exact branch CAS", "remote outcome requires independent verification", errors.Join(pushErr, observeErr))
}

func (p *Publisher) validatePublishRequest(request PublishRequest) (PreparedPublication, error) {
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
	if err := validateRemoteRef("remote before", request.RemoteBefore); err != nil {
		return PreparedPublication{}, err
	}
	if err := validateObjectID("expected commit", request.ExpectedCommitOID); err != nil {
		return PreparedPublication{}, err
	}
	if request.RemoteBefore.OID != "" && len(request.RemoteBefore.OID) != len(request.ExpectedCommitOID) {
		return PreparedPublication{}, invalid("remote before", "object format differs from expected commit")
	}
	if err := validateDigest("bundle digest", request.BundleDigest); err != nil {
		return PreparedPublication{}, err
	}
	prepared, err := p.loadPrepared(request.PublicationID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if prepared.PublicationGeneration != request.PublicationGeneration || prepared.Target != request.Target ||
		prepared.TargetRef != request.TargetRef || prepared.RemoteBefore != request.RemoteBefore ||
		prepared.CommitOID != request.ExpectedCommitOID || prepared.BundleDigest != request.BundleDigest {
		return PreparedPublication{}, operationError(ErrIdempotencyConflict, "validate publish request", "request differs from durable prepared artifact", nil)
	}
	if err := validateClaim(request.Claim, request.Target, request.TargetRef, request.RemoteBefore, prepared.BranchClaimGeneration); err != nil {
		return PreparedPublication{}, err
	}
	return prepared, nil
}

func publishRequestDigest(request PublishRequest) (string, error) {
	return digestCanonical(struct {
		Domain                string      `json:"domain"`
		PublicationID         string      `json:"publicationId"`
		PublicationGeneration int64       `json:"publicationGeneration"`
		Target                Repository  `json:"target"`
		TargetRef             string      `json:"targetRef"`
		Claim                 BranchClaim `json:"claim"`
		RemoteBefore          RemoteRef   `json:"remoteBefore"`
		ExpectedCommitOID     string      `json:"expectedCommitOid"`
		BundleDigest          string      `json:"bundleDigest"`
	}{
		Domain: "orka.publisher.publish.v1", PublicationID: request.PublicationID,
		PublicationGeneration: request.PublicationGeneration, Target: request.Target, TargetRef: request.TargetRef,
		Claim: request.Claim, RemoteBefore: request.RemoteBefore, ExpectedCommitOID: request.ExpectedCommitOID,
		BundleDigest: request.BundleDigest,
	})
}

func loadPublishJournal(path string) (publishJournal, bool, error) {
	var journal publishJournal
	if err := readCanonical(path, &journal); err != nil {
		if os.IsNotExist(err) {
			return publishJournal{}, false, nil
		}
		return publishJournal{}, false, operationError(ErrPreparedArtifactCorrupt, "read publish journal", "", err)
	}
	return journal, true, nil
}

func (p *Publisher) newPublishReceipt(request PublishRequest, digest string, outcome PublishOutcome, observed RemoteRef) PublishReceipt {
	return PublishReceipt{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration,
		OperationID: request.OperationID, RequestDigest: digest, Outcome: outcome,
		TargetRepositoryID: request.Target.ID, TargetRef: request.TargetRef,
		RemoteBefore: request.RemoteBefore, ExpectedCommitOID: request.ExpectedCommitOID, ObservedRemote: observed,
	}
}

func (p *Publisher) persistPublishReceipt(path string, journal publishJournal, receipt PublishReceipt) (PublishReceipt, error) {
	journal.Receipt = &receipt
	if err := writeCanonicalDurable(path, journal); err != nil {
		return receipt, err
	}
	return receipt, publishReceiptError(receipt)
}

func publishReceiptError(receipt PublishReceipt) error {
	switch receipt.Outcome {
	case PublishAcknowledged, PublishAlreadyExact:
		return nil
	case PublishCASRejected:
		return ErrCASRejected
	case PublishOutcomeUnknown:
		return ErrPublicationUnknown
	default:
		return ErrPreparedArtifactCorrupt
	}
}

func (p *Publisher) observeAfterMutation(request PublishRequest) (RemoteRef, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.publishTimeout)
	defer cancel()
	box, err := p.newSandbox("publish-observe")
	if err != nil {
		return RemoteRef{}, err
	}
	defer box.Close() //nolint:errcheck
	return p.observeRef(ctx, box, request.Target, request.TargetRef)
}

func pushDefinitivelyRejected(result commandResult) bool {
	output := result.stdout + "\n" + result.stderr
	return strings.Contains(output, "[rejected] (stale info)") ||
		strings.Contains(output, "[rejected] (non-fast-forward)")
}

func (p *Publisher) fetchObserved(ctx context.Context, box *sandbox, repositoryPath string, target Repository, ref, expectedOID string) error {
	if _, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "fetch", "--no-tags", "--no-recurse-submodules", "--", target.URL, ref+":refs/orka/observed"); err != nil {
		return err
	}
	resolved, err := p.revParse(ctx, box, repositoryPath, "refs/orka/observed^{commit}")
	if err != nil {
		return err
	}
	if resolved != expectedOID {
		return fmt.Errorf("remote changed while fetching: expected %s, fetched %s", expectedOID, resolved)
	}
	return nil
}
