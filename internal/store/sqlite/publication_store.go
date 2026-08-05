package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

var _ store.PublicationStore = (*Store)(nil)

// CreatePublication persists immutable clean-room publication intent in Preparing.
func (s *Store) CreatePublication(ctx context.Context, publication *store.Publication, fence store.ControllerEpochFence) (*store.Publication, error) {
	normalized, fence, err := normalizePublicationForCreate(publication, fence)
	if err != nil {
		return nil, err
	}
	prIntentJSON, err := marshalOptionalControlJSON(normalized.PRIntent)
	if err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireControllerEpoch(ctx, tx, fence); err != nil {
		return nil, err
	}
	if existing, getErr := getPublication(ctx, tx, normalized.ID); getErr == nil {
		if samePublicationCreation(existing, normalized) {
			return &existing, nil
		}
		return nil, controlConflict("publication %q was reused with different immutable intent or request digest", normalized.ID)
	} else if !errors.Is(getErr, store.ErrNotFound) {
		return nil, getErr
	}
	claim, err := getBranchClaim(ctx, tx, normalized.BranchClaimID)
	if err != nil {
		return nil, fmt.Errorf("publication branch claim: %w", err)
	}
	if claim.RepositoryID != normalized.TargetRepositoryID || claim.Ref != normalized.TargetRef ||
		claim.Generation != normalized.BranchClaimGeneration || !claim.LastVerified.Equal(normalized.Baseline) ||
		claim.Availability != store.BranchClaimAvailable {
		return nil, controlConflict("publication %q branch claim does not match exact target, generation, baseline, or availability", normalized.ID)
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO publications(
			id, namespace, generation, task_uid, attempt, prompt_id, session_uid, branch_claim_id,
			branch_claim_generation, source_repository_id, source_ref, source_baseline_sha, target_repository_id, target_ref,
			baseline_absent, baseline_sha, artifact_id, artifact_digest, artifact_size_bytes, artifact_media_type,
			publication_credential_ref, commit_identity,
			commit_message, commit_timestamp, pr_intent, request_digest, state, prepared_receipt,
			publish_receipt, verification_receipt, pr_receipt, terminal_reason, controller_epoch_name,
			controller_epoch, last_operation_id, last_operation_digest, version, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		normalized.ID, normalized.Namespace, normalized.Generation, normalized.TaskUID, normalized.Attempt,
		normalized.PromptID, normalized.SessionUID, normalized.BranchClaimID, normalized.BranchClaimGeneration,
		normalized.SourceRepositoryID, normalized.SourceRef, normalized.SourceBaselineSHA, normalized.TargetRepositoryID, normalized.TargetRef,
		normalized.Baseline.Absent, normalized.Baseline.SHA, normalized.ArtifactID, normalized.ArtifactDigest,
		normalized.ArtifactSizeBytes, normalized.ArtifactMediaType,
		normalized.PublicationCredentialRef, normalized.CommitIdentity, normalized.CommitMessage,
		normalized.CommitTimestamp, prIntentJSON, normalized.RequestDigest, string(normalized.State), nil, nil, nil, nil,
		normalized.TerminalReason, normalized.ControllerEpochName, normalized.ControllerEpoch,
		normalized.LastOperationID, normalized.LastOperationDigest, normalized.Version,
		normalized.CreatedAt, normalized.UpdatedAt,
	)
	if err != nil {
		if !isSQLiteConstraintError(err) {
			return nil, err
		}
		existing, getErr := getPublication(ctx, tx, normalized.ID)
		if getErr == nil && samePublicationCreation(existing, normalized) {
			return &existing, nil
		}
		return nil, controlConflict("publication %q was reused with different immutable intent or request digest", normalized.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &normalized, nil
}

// GetPublication returns a publication and its exact receipts.
func (s *Store) GetPublication(ctx context.Context, id string) (*store.Publication, error) {
	id = strings.TrimSpace(id)
	if err := store.ValidateControlIdentifier("publication ID", id); err != nil {
		return nil, err
	}
	publication, err := getPublication(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	return &publication, nil
}

// SetPublicationPRIntent persists the exact PR tuple after independent remote
// verification has fixed the current head and before the first forge call.
func (s *Store) SetPublicationPRIntent(ctx context.Context, request store.SetPublicationPRIntentRequest) (*store.Publication, error) {
	request.ID = strings.TrimSpace(request.ID)
	if err := store.ValidateControlIdentifier("publication ID", request.ID); err != nil {
		return nil, err
	}
	fence, err := normalizeEpochFence(request.Fence)
	if err != nil {
		return nil, err
	}
	request.Fence = fence
	if request.ExpectedVersion < 1 || request.ExpectedGeneration < 1 || request.ExpectedState != store.PublicationPrepared && request.ExpectedState != store.PublicationVerifying {
		return nil, store.ValidationErrorf("PR intent requires an exact Prepared or Verifying publication version/generation")
	}
	if err := validatePullRequestIntent(request.Intent, request.ExpectedGeneration); err != nil {
		return nil, err
	}
	request.OperationID = strings.TrimSpace(request.OperationID)
	if err := store.ValidateControlIdentifier("PR intent operation ID", request.OperationID); err != nil {
		return nil, err
	}
	if err := store.ValidateCanonicalDigest("PR intent operation digest", request.OperationDigest); err != nil {
		return nil, err
	}
	request.UpdatedAt = normalizeControlTime(request.UpdatedAt)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireControllerEpoch(ctx, tx, request.Fence); err != nil {
		return nil, err
	}
	publication, err := getPublication(ctx, tx, request.ID)
	if err != nil {
		return nil, err
	}
	if publication.PreparedReceipt == nil {
		return nil, store.ValidationErrorf("PR intent requires a durable prepared receipt")
	}
	if request.ExpectedState == store.PublicationPrepared && publication.PreparedReceipt.CommitSHA != request.Intent.ExpectedHeadSHA {
		return nil, store.ValidationErrorf("Prepared PR intent expected head must equal the durable prepared commit")
	}
	if publication.LastOperationID == request.OperationID {
		if publication.LastOperationDigest == request.OperationDigest && publication.PRIntent != nil && reflect.DeepEqual(*publication.PRIntent, request.Intent) {
			return &publication, nil
		}
		return nil, controlConflict("PR intent operation %q was reused with different content", request.OperationID)
	}
	if publication.Version != request.ExpectedVersion || publication.Generation != request.ExpectedGeneration || publication.State != request.ExpectedState || publication.PRIntent != nil {
		return nil, controlConflict("publication %q no longer matches PR intent version/generation/state", publication.ID)
	}
	intentJSON, err := marshalOptionalControlJSON(&request.Intent)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE publications SET pr_intent = ?, controller_epoch_name = ?, controller_epoch = ?,
		 last_operation_id = ?, last_operation_digest = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ? AND generation = ? AND state = ? AND pr_intent IS NULL`,
		intentJSON, request.Fence.Name, request.Fence.Epoch, request.OperationID, request.OperationDigest,
		request.UpdatedAt, request.ID, request.ExpectedVersion, request.ExpectedGeneration, string(request.ExpectedState),
	)
	if err != nil {
		return nil, err
	}
	if err := rowsAffectedExactlyOne(result, "publication PR intent"); err != nil {
		return nil, err
	}
	intent := request.Intent
	publication.PRIntent = &intent
	publication.ControllerEpochName = request.Fence.Name
	publication.ControllerEpoch = request.Fence.Epoch
	publication.LastOperationID = request.OperationID
	publication.LastOperationDigest = request.OperationDigest
	publication.Version++
	publication.UpdatedAt = request.UpdatedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &publication, nil
}

// SetPublicationPRReceipt commits the exact forge result before the
// Publication leaves Verifying.
func (s *Store) SetPublicationPRReceipt(ctx context.Context, request store.SetPublicationPRReceiptRequest) (*store.Publication, error) {
	request.ID = strings.TrimSpace(request.ID)
	if err := store.ValidateControlIdentifier("publication ID", request.ID); err != nil {
		return nil, err
	}
	fence, err := normalizeEpochFence(request.Fence)
	if err != nil {
		return nil, err
	}
	request.Fence = fence
	if request.ExpectedVersion < 1 || request.ExpectedGeneration < 1 || request.ExpectedState != store.PublicationVerifying {
		return nil, store.ValidationErrorf("PR receipt requires an exact Verifying publication version/generation")
	}
	request.OperationID = strings.TrimSpace(request.OperationID)
	if err := store.ValidateControlIdentifier("PR receipt operation ID", request.OperationID); err != nil {
		return nil, err
	}
	if err := store.ValidateCanonicalDigest("PR receipt operation digest", request.OperationDigest); err != nil {
		return nil, err
	}
	request.UpdatedAt = normalizeControlTime(request.UpdatedAt)
	if err := validatePullRequestReceipt(request.Receipt); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireControllerEpoch(ctx, tx, request.Fence); err != nil {
		return nil, err
	}
	publication, err := getPublication(ctx, tx, request.ID)
	if err != nil {
		return nil, err
	}
	if publication.PRIntent == nil || publication.PRIntent.ExpectedHeadSHA != request.Receipt.HeadSHA {
		return nil, store.ValidationErrorf("PR receipt does not match the persisted intent head")
	}
	if publication.LastOperationID == request.OperationID {
		if publication.LastOperationDigest == request.OperationDigest && publication.PullRequestReceipt != nil && reflect.DeepEqual(*publication.PullRequestReceipt, request.Receipt) {
			return &publication, nil
		}
		return nil, controlConflict("PR receipt operation %q was reused with different content", request.OperationID)
	}
	if publication.Version != request.ExpectedVersion || publication.Generation != request.ExpectedGeneration || publication.State != request.ExpectedState || publication.PullRequestReceipt != nil {
		return nil, controlConflict("publication %q no longer matches PR receipt version/generation/state", publication.ID)
	}
	receiptJSON, err := marshalOptionalControlJSON(&request.Receipt)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE publications SET pr_receipt = ?, controller_epoch_name = ?, controller_epoch = ?,
		 last_operation_id = ?, last_operation_digest = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ? AND generation = ? AND state = 'Verifying' AND pr_receipt IS NULL`,
		receiptJSON, request.Fence.Name, request.Fence.Epoch, request.OperationID, request.OperationDigest,
		request.UpdatedAt, request.ID, request.ExpectedVersion, request.ExpectedGeneration,
	)
	if err != nil {
		return nil, err
	}
	if err := rowsAffectedExactlyOne(result, "publication PR receipt"); err != nil {
		return nil, err
	}
	receipt := request.Receipt
	publication.PullRequestReceipt = &receipt
	publication.ControllerEpochName = request.Fence.Name
	publication.ControllerEpoch = request.Fence.Epoch
	publication.LastOperationID = request.OperationID
	publication.LastOperationDigest = request.OperationDigest
	publication.Version++
	publication.UpdatedAt = request.UpdatedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &publication, nil
}

func validatePullRequestReceipt(receipt store.PullRequestOperationReceipt) error {
	for field, value := range map[string]string{
		"PR receipt operation ID": receipt.OperationID, "PR receipt intent key": receipt.IntentKey,
		"PR receipt forge ID": receipt.ForgeID, "PR receipt URL": receipt.URL, "PR receipt state": receipt.State,
	} {
		if err := store.ValidateControlIdentifier(field, strings.TrimSpace(value)); err != nil {
			return err
		}
	}
	if err := store.ValidateCanonicalDigest("PR receipt request digest", receipt.RequestDigest); err != nil {
		return err
	}
	if err := store.ValidateGitObjectID("PR receipt head SHA", receipt.HeadSHA); err != nil {
		return err
	}
	if receipt.ReconciledAt.IsZero() {
		return store.ValidationErrorf("PR receipt reconciliation timestamp is required")
	}
	return nil
}

// TransitionPublication applies a fenced generation/version/state CAS and
// validates that the persisted receipt exactly matches immutable intent.
//
//nolint:gocyclo // Receipt-specific validation and the publication CAS intentionally share one boundary.
func (s *Store) TransitionPublication(ctx context.Context, transition store.PublicationTransition) (*store.Publication, error) {
	transition.ID = strings.TrimSpace(transition.ID)
	if err := store.ValidateControlIdentifier("publication ID", transition.ID); err != nil {
		return nil, err
	}
	fence, err := normalizeEpochFence(transition.Fence)
	if err != nil {
		return nil, err
	}
	transition.Fence = fence
	if transition.ExpectedVersion < 1 || transition.ExpectedGeneration < 1 {
		return nil, store.ValidationErrorf("publication expected version and generation must be at least 1")
	}
	if err := store.ValidatePublicationTransition(transition.ExpectedState, transition.NewState); err != nil {
		return nil, err
	}
	transition.OperationID = strings.TrimSpace(transition.OperationID)
	if err := store.ValidateControlIdentifier("publication operation ID", transition.OperationID); err != nil {
		return nil, err
	}
	if err := store.ValidateCanonicalDigest("publication operation digest", transition.OperationDigest); err != nil {
		return nil, err
	}
	transition.TerminalReason = strings.TrimSpace(transition.TerminalReason)
	if err := store.ValidateControlReason("publication terminal reason", transition.TerminalReason); err != nil {
		return nil, err
	}
	transition.UpdatedAt = normalizeControlTime(transition.UpdatedAt)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireControllerEpoch(ctx, tx, transition.Fence); err != nil {
		return nil, err
	}
	publication, err := getPublication(ctx, tx, transition.ID)
	if err != nil {
		return nil, err
	}
	if err := validatePublicationTransitionReceipts(publication, transition); err != nil {
		return nil, err
	}
	if publication.LastOperationID == transition.OperationID {
		if publication.LastOperationDigest != transition.OperationDigest {
			return nil, controlConflict("publication operation %q was reused with a different digest", transition.OperationID)
		}
		if publication.State == transition.NewState && publicationTransitionReceiptsMatch(publication, transition) {
			return &publication, nil
		}
		return nil, controlConflict("publication operation %q was already applied with different target state or receipt", transition.OperationID)
	}
	if publication.Version != transition.ExpectedVersion || publication.Generation != transition.ExpectedGeneration || publication.State != transition.ExpectedState {
		return nil, controlConflict("publication %q is version %d generation %d state %s, expected version %d generation %d state %s", publication.ID, publication.Version, publication.Generation, publication.State, transition.ExpectedVersion, transition.ExpectedGeneration, transition.ExpectedState)
	}

	prepared := publication.PreparedReceipt
	if transition.PreparedReceipt != nil {
		copyValue := *transition.PreparedReceipt
		prepared = &copyValue
	}
	publish := publication.PublishReceipt
	if transition.PublishReceipt != nil {
		copyValue := *transition.PublishReceipt
		publish = &copyValue
	}
	verification := publication.VerificationReceipt
	if transition.VerificationReceipt != nil {
		copyValue := *transition.VerificationReceipt
		verification = &copyValue
	}
	preparedJSON, err := marshalOptionalControlJSON(prepared)
	if err != nil {
		return nil, err
	}
	publishJSON, err := marshalOptionalControlJSON(publish)
	if err != nil {
		return nil, err
	}
	verificationJSON, err := marshalOptionalControlJSON(verification)
	if err != nil {
		return nil, err
	}
	terminalReason := publication.TerminalReason
	if transition.TerminalReason != "" || store.IsTerminalPublicationState(transition.NewState) {
		terminalReason = transition.TerminalReason
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE publications
		 SET state = ?, prepared_receipt = ?, publish_receipt = ?, verification_receipt = ?,
		     terminal_reason = ?, controller_epoch_name = ?, controller_epoch = ?, last_operation_id = ?,
		     last_operation_digest = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ? AND generation = ? AND state = ?`,
		string(transition.NewState), preparedJSON, publishJSON, verificationJSON, terminalReason,
		transition.Fence.Name, transition.Fence.Epoch, transition.OperationID, transition.OperationDigest,
		transition.UpdatedAt, transition.ID, transition.ExpectedVersion, transition.ExpectedGeneration,
		string(transition.ExpectedState),
	)
	if err != nil {
		return nil, err
	}
	if err := rowsAffectedExactlyOne(result, "publication"); err != nil {
		return nil, err
	}
	publication.State = transition.NewState
	publication.PreparedReceipt = prepared
	publication.PublishReceipt = publish
	publication.VerificationReceipt = verification
	publication.TerminalReason = terminalReason
	publication.ControllerEpochName = transition.Fence.Name
	publication.ControllerEpoch = transition.Fence.Epoch
	publication.LastOperationID = transition.OperationID
	publication.LastOperationDigest = transition.OperationDigest
	publication.Version++
	publication.UpdatedAt = transition.UpdatedAt
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &publication, nil
}

//nolint:gocyclo // The explicit state-machine branches are easier to audit together.
func normalizePublicationForCreate(publication *store.Publication, fence store.ControllerEpochFence) (store.Publication, store.ControllerEpochFence, error) {
	if publication == nil {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("publication is required")
	}
	normalized := *publication
	normalized.ID = strings.TrimSpace(normalized.ID)
	normalized.Namespace = strings.TrimSpace(normalized.Namespace)
	normalized.TaskUID = strings.TrimSpace(normalized.TaskUID)
	normalized.PromptID = strings.TrimSpace(normalized.PromptID)
	normalized.SessionUID = strings.TrimSpace(normalized.SessionUID)
	normalized.BranchClaimID = strings.TrimSpace(normalized.BranchClaimID)
	normalized.SourceRepositoryID = strings.TrimSpace(normalized.SourceRepositoryID)
	normalized.SourceRef = strings.TrimSpace(normalized.SourceRef)
	normalized.TargetRepositoryID = strings.TrimSpace(normalized.TargetRepositoryID)
	normalized.TargetRef = strings.TrimSpace(normalized.TargetRef)
	normalized.PublicationCredentialRef = strings.TrimSpace(normalized.PublicationCredentialRef)
	normalized.CommitIdentity = strings.TrimSpace(normalized.CommitIdentity)
	for field, value := range map[string]string{
		"publication ID": normalized.ID, "publication namespace": normalized.Namespace,
		"publication task UID": normalized.TaskUID, "publication prompt ID": normalized.PromptID,
		"branch claim ID": normalized.BranchClaimID, "source repository ID": normalized.SourceRepositoryID,
		"source ref": normalized.SourceRef, "target repository ID": normalized.TargetRepositoryID,
		"publication credential reference": normalized.PublicationCredentialRef,
		"commit identity":                  normalized.CommitIdentity,
	} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return store.Publication{}, store.ControllerEpochFence{}, err
		}
	}
	if normalized.SessionUID != "" {
		if err := store.ValidateControlIdentifier("publication session UID", normalized.SessionUID); err != nil {
			return store.Publication{}, store.ControllerEpochFence{}, err
		}
	}
	if normalized.Generation < 1 || normalized.BranchClaimGeneration < 1 || normalized.Attempt < 1 {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("publication generation, branch claim generation, and attempt must be at least 1")
	}
	if err := store.ValidateFullBranchRef(normalized.TargetRef); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	if err := normalized.Baseline.Validate("publication baseline"); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	if err := store.ValidateCanonicalDigest("publication artifact digest", normalized.ArtifactDigest); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	if err := store.ValidateGitObjectID("publication source baseline SHA", strings.TrimSpace(normalized.SourceBaselineSHA)); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	normalized.SourceBaselineSHA = strings.TrimSpace(normalized.SourceBaselineSHA)
	normalized.ArtifactID = strings.TrimSpace(normalized.ArtifactID)
	normalized.ArtifactMediaType = strings.TrimSpace(normalized.ArtifactMediaType)
	if err := store.ValidateControlIdentifier("publication artifact ID", normalized.ArtifactID); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	if normalized.ArtifactSizeBytes < 1 {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("publication artifact size must be positive")
	}
	if normalized.ArtifactMediaType == "" {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("publication artifact media type is required")
	}
	if err := store.ValidateCanonicalDigest("publication request digest", normalized.RequestDigest); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	if strings.TrimSpace(normalized.CommitMessage) == "" {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("publication commit message is required")
	}
	if err := store.ValidateControlReason("publication commit message", normalized.CommitMessage); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	if normalized.CommitTimestamp.IsZero() {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("publication commit timestamp is required")
	}
	normalized.CommitTimestamp = normalized.CommitTimestamp.UTC()
	if normalized.PRIntent != nil {
		copyIntent := *normalized.PRIntent
		copyIntent.BaseRepositoryID = strings.TrimSpace(copyIntent.BaseRepositoryID)
		copyIntent.BaseRef = strings.TrimSpace(copyIntent.BaseRef)
		copyIntent.HeadRepositoryID = strings.TrimSpace(copyIntent.HeadRepositoryID)
		copyIntent.HeadRef = strings.TrimSpace(copyIntent.HeadRef)
		copyIntent.ExpectedHeadSHA = strings.TrimSpace(copyIntent.ExpectedHeadSHA)
		normalized.PRIntent = &copyIntent
		if err := validatePullRequestIntent(copyIntent, normalized.Generation); err != nil {
			return store.Publication{}, store.ControllerEpochFence{}, err
		}
	}
	if normalized.State == "" {
		normalized.State = store.PublicationPreparing
	}
	if normalized.State != store.PublicationPreparing {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("new publication must start in %s", store.PublicationPreparing)
	}
	if normalized.PreparedReceipt != nil || normalized.PublishReceipt != nil || normalized.VerificationReceipt != nil || normalized.PullRequestReceipt != nil {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("new publication must not include operation receipts")
	}
	fence, err := normalizeEpochFence(fence)
	if err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
	if normalized.Version != 0 && normalized.Version != 1 {
		return store.Publication{}, store.ControllerEpochFence{}, store.ValidationErrorf("new publication version must be zero or one")
	}
	now := normalizeControlTime(normalized.CreatedAt)
	normalized.CreatedAt = now
	if normalized.UpdatedAt.IsZero() {
		normalized.UpdatedAt = now
	} else {
		normalized.UpdatedAt = normalized.UpdatedAt.UTC()
	}
	normalized.ControllerEpochName = fence.Name
	normalized.ControllerEpoch = fence.Epoch
	normalized.Version = 1
	normalized.LastOperationID = ""
	normalized.LastOperationDigest = ""
	normalized.TerminalReason = ""
	return normalized, fence, nil
}

func validatePullRequestIntent(intent store.PullRequestIntent, generation int64) error {
	for field, value := range map[string]string{
		"PR base repository ID": intent.BaseRepositoryID,
		"PR head repository ID": intent.HeadRepositoryID,
	} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return err
		}
	}
	if err := store.ValidateFullBranchRef(intent.BaseRef); err != nil {
		return err
	}
	if err := store.ValidateFullBranchRef(intent.HeadRef); err != nil {
		return err
	}
	if intent.PublicationGeneration != generation {
		return store.ValidationErrorf("PR intent publication generation %d does not match publication generation %d", intent.PublicationGeneration, generation)
	}
	return store.ValidateGitObjectID("PR expected head SHA", intent.ExpectedHeadSHA)
}

//nolint:gocyclo // Each destination state has intentionally distinct exact-receipt invariants.
func validatePublicationTransitionReceipts(publication store.Publication, transition store.PublicationTransition) error {
	var receiptErr error
	switch transition.NewState {
	case store.PublicationPrepared:
		if transition.PreparedReceipt == nil || transition.PublishReceipt != nil || transition.VerificationReceipt != nil {
			return store.ValidationErrorf("Prepared transition requires only a prepared receipt")
		}
		receiptErr = validatePreparedReceipt(*transition.PreparedReceipt)
	case store.PublicationPublishing, store.PublicationCancelledBeforePublish:
		if transition.PreparedReceipt != nil || transition.PublishReceipt != nil || transition.VerificationReceipt != nil {
			return store.ValidationErrorf("%s transition must not replace receipts", transition.NewState)
		}
	case store.PublicationVerifying:
		if transition.PublishReceipt == nil || transition.PreparedReceipt != nil || transition.VerificationReceipt != nil {
			return store.ValidationErrorf("Verifying transition requires only a publish receipt")
		}
		receiptErr = validatePublishReceipt(publication, transition, *transition.PublishReceipt)
	case store.PublicationVerifiedExact, store.PublicationDeliveredSuperseded, store.PublicationOutcomeUnknown:
		if transition.VerificationReceipt == nil || transition.PreparedReceipt != nil || transition.PublishReceipt != nil {
			return store.ValidationErrorf("%s transition requires only a verification receipt", transition.NewState)
		}
		receiptErr = validateVerificationReceipt(publication, transition, *transition.VerificationReceipt)
	case store.PublicationDeliveryConflict:
		if transition.ExpectedState == store.PublicationVerifying {
			if transition.VerificationReceipt == nil || transition.PreparedReceipt != nil || transition.PublishReceipt != nil {
				return store.ValidationErrorf("verified DeliveryConflict transition requires only a verification receipt")
			}
			receiptErr = validateVerificationReceipt(publication, transition, *transition.VerificationReceipt)
		} else if transition.PreparedReceipt != nil || transition.PublishReceipt != nil || transition.VerificationReceipt != nil {
			return store.ValidationErrorf("preparation DeliveryConflict transition must not include receipts")
		}
	default:
		if transition.PreparedReceipt != nil || transition.PublishReceipt != nil || transition.VerificationReceipt != nil {
			return store.ValidationErrorf("%s transition must not include receipts", transition.NewState)
		}
	}
	if receiptErr != nil {
		return receiptErr
	}
	switch transition.NewState {
	case store.PublicationDeliveryConflict, store.PublicationCredentialBlocked,
		store.PublicationPreparationFailed, store.PublicationOutcomeUnknown:
		if strings.TrimSpace(transition.TerminalReason) == "" {
			return store.ValidationErrorf("terminal publication state %s requires a reason", transition.NewState)
		}
	}
	return nil
}

func validatePreparedReceipt(receipt store.PreparedPublicationReceipt) error {
	if err := store.ValidateControlIdentifier("prepared operation ID", receipt.OperationID); err != nil {
		return err
	}
	if err := store.ValidateCanonicalDigest("prepared request digest", receipt.RequestDigest); err != nil {
		return err
	}
	if err := store.ValidateGitObjectID("prepared tree SHA", receipt.TreeSHA); err != nil {
		return err
	}
	if err := store.ValidateGitObjectID("prepared commit SHA", receipt.CommitSHA); err != nil {
		return err
	}
	if err := store.ValidateCanonicalDigest("prepared manifest digest", receipt.ManifestDigest); err != nil {
		return err
	}
	if err := store.ValidateWorkspaceRelativeRoot(receipt.RelativeRoot); err != nil {
		return err
	}
	if err := store.ValidateControlIdentifier("prepared bundle artifact ID", receipt.BundleArtifactID); err != nil {
		return err
	}
	if err := store.ValidateCanonicalDigest("prepared bundle digest", receipt.BundleDigest); err != nil {
		return err
	}
	if receipt.BundleSizeBytes < 1 || receipt.BundleMediaType != store.PreparedBundleMediaType ||
		!strings.HasPrefix(receipt.BundleRef, "refs/orka/publications/") || len(strings.TrimPrefix(receipt.BundleRef, "refs/orka/publications/")) != 64 {
		return store.ValidationErrorf("prepared bundle artifact metadata is invalid")
	}
	if receipt.PreparedAt.IsZero() {
		return store.ValidationErrorf("prepared receipt timestamp is required")
	}
	return nil
}

func validatePublishReceipt(publication store.Publication, transition store.PublicationTransition, receipt store.PublishOperationReceipt) error {
	if publication.PreparedReceipt == nil {
		return store.ValidationErrorf("publication must have a durable prepared receipt before publishing")
	}
	if receipt.OperationID != transition.OperationID || receipt.RequestDigest != transition.OperationDigest {
		return store.ValidationErrorf("publish receipt operation identity and digest must match transition")
	}
	if receipt.TargetRepositoryID != publication.TargetRepositoryID || receipt.TargetRef != publication.TargetRef || !receipt.RemoteBefore.Equal(publication.Baseline) || receipt.ExpectedCommitSHA != publication.PreparedReceipt.CommitSHA {
		return store.ValidationErrorf("publish receipt does not exactly match persisted publication target, baseline, or commit")
	}
	if err := receipt.RemoteBefore.Validate("publish remote-before"); err != nil {
		return err
	}
	if err := store.ValidateGitObjectID("publish expected commit SHA", receipt.ExpectedCommitSHA); err != nil {
		return err
	}
	if receipt.PublishedAt.IsZero() {
		return store.ValidationErrorf("publish receipt timestamp is required")
	}
	return nil
}

func validateVerificationReceipt(publication store.Publication, transition store.PublicationTransition, receipt store.PublicationVerificationReceipt) error {
	if publication.PreparedReceipt == nil || publication.PublishReceipt == nil {
		return store.ValidationErrorf("publication must have prepare and publish receipts before verification")
	}
	if receipt.OperationID != transition.OperationID || receipt.RequestDigest != transition.OperationDigest || receipt.Outcome != transition.NewState {
		return store.ValidationErrorf("verification receipt operation identity, digest, and outcome must match transition")
	}
	if receipt.ExpectedCommitSHA != publication.PreparedReceipt.CommitSHA {
		return store.ValidationErrorf("verification expected commit does not match prepared commit")
	}
	if receipt.VerifiedAt.IsZero() {
		return store.ValidationErrorf("verification receipt timestamp is required")
	}
	switch receipt.Outcome {
	case store.PublicationVerifiedExact:
		if err := receipt.ObservedRemote.Validate("verified remote"); err != nil {
			return err
		}
		if receipt.ObservedRemote.Absent || receipt.ObservedRemote.SHA != receipt.ExpectedCommitSHA {
			return store.ValidationErrorf("VerifiedExact requires observed remote to equal expected commit")
		}
		if receipt.DescendantProofDigest != "" {
			return store.ValidationErrorf("VerifiedExact must not include a descendant proof")
		}
	case store.PublicationDeliveredSuperseded:
		if err := receipt.ObservedRemote.Validate("superseding remote"); err != nil {
			return err
		}
		if receipt.ObservedRemote.Absent || receipt.ObservedRemote.SHA == receipt.ExpectedCommitSHA {
			return store.ValidationErrorf("DeliveredSuperseded requires a different observed descendant SHA")
		}
		if err := store.ValidateCanonicalDigest("descendant proof digest", receipt.DescendantProofDigest); err != nil {
			return err
		}
	case store.PublicationDeliveryConflict:
		if err := receipt.ObservedRemote.Validate("conflicting remote"); err != nil {
			return err
		}
	case store.PublicationOutcomeUnknown:
		if receipt.ObservedRemote.Absent || receipt.ObservedRemote.SHA != "" || receipt.DescendantProofDigest != "" {
			return store.ValidationErrorf("PublicationOutcomeUnknown must not invent a remote observation or descendant proof")
		}
	default:
		return store.ValidationErrorf("unsupported verification outcome %q", receipt.Outcome)
	}
	return nil
}

func publicationTransitionReceiptsMatch(publication store.Publication, transition store.PublicationTransition) bool {
	if transition.PreparedReceipt != nil && !equalPreparedReceipt(publication.PreparedReceipt, transition.PreparedReceipt) {
		return false
	}
	if transition.PublishReceipt != nil && !equalPublishReceipt(publication.PublishReceipt, transition.PublishReceipt) {
		return false
	}
	if transition.VerificationReceipt != nil && !equalVerificationReceipt(publication.VerificationReceipt, transition.VerificationReceipt) {
		return false
	}
	return true
}

func equalPreparedReceipt(a, b *store.PreparedPublicationReceipt) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.OperationID == b.OperationID && a.RequestDigest == b.RequestDigest &&
		a.TreeSHA == b.TreeSHA && a.CommitSHA == b.CommitSHA && a.ManifestDigest == b.ManifestDigest && a.RelativeRoot == b.RelativeRoot &&
		a.BundleArtifactID == b.BundleArtifactID && a.BundleDigest == b.BundleDigest && a.BundleSizeBytes == b.BundleSizeBytes &&
		a.BundleMediaType == b.BundleMediaType && a.BundleRef == b.BundleRef && a.PreparedAt.Equal(b.PreparedAt)
}

func equalPublishReceipt(a, b *store.PublishOperationReceipt) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.OperationID == b.OperationID && a.RequestDigest == b.RequestDigest &&
		a.TargetRepositoryID == b.TargetRepositoryID && a.TargetRef == b.TargetRef &&
		a.RemoteBefore.Equal(b.RemoteBefore) && a.ExpectedCommitSHA == b.ExpectedCommitSHA &&
		a.AcknowledgementUnknown == b.AcknowledgementUnknown && a.PublishedAt.Equal(b.PublishedAt)
}

func equalVerificationReceipt(a, b *store.PublicationVerificationReceipt) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.OperationID == b.OperationID && a.RequestDigest == b.RequestDigest && a.Outcome == b.Outcome &&
		a.ExpectedCommitSHA == b.ExpectedCommitSHA && a.ObservedRemote.Equal(b.ObservedRemote) &&
		a.DescendantProofDigest == b.DescendantProofDigest && a.VerifiedAt.Equal(b.VerifiedAt)
}

func samePublicationCreation(a, b store.Publication) bool {
	prIntentMatches := b.PRIntent == nil || reflect.DeepEqual(a.PRIntent, b.PRIntent)
	return a.ID == b.ID && a.Namespace == b.Namespace && a.Generation == b.Generation &&
		a.TaskUID == b.TaskUID && a.Attempt == b.Attempt && a.PromptID == b.PromptID &&
		a.SessionUID == b.SessionUID && a.BranchClaimID == b.BranchClaimID &&
		a.BranchClaimGeneration == b.BranchClaimGeneration && a.SourceRepositoryID == b.SourceRepositoryID &&
		a.SourceRef == b.SourceRef && a.SourceBaselineSHA == b.SourceBaselineSHA && a.TargetRepositoryID == b.TargetRepositoryID && a.TargetRef == b.TargetRef &&
		a.Baseline.Equal(b.Baseline) && a.ArtifactID == b.ArtifactID && a.ArtifactDigest == b.ArtifactDigest &&
		a.ArtifactSizeBytes == b.ArtifactSizeBytes && a.ArtifactMediaType == b.ArtifactMediaType &&
		a.PublicationCredentialRef == b.PublicationCredentialRef && a.CommitIdentity == b.CommitIdentity &&
		a.CommitMessage == b.CommitMessage && a.CommitTimestamp.Equal(b.CommitTimestamp) &&
		prIntentMatches && a.RequestDigest == b.RequestDigest
}

func publicationReceipt(publication store.Publication) store.PublicationReceipt {
	return store.PublicationReceipt{
		PublicationID: publication.ID,
		Generation:    publication.Generation,
		State:         publication.State,
		Prepared:      publication.PreparedReceipt,
		Publish:       publication.PublishReceipt,
		Verification:  publication.VerificationReceipt,
		PullRequest:   publication.PullRequestReceipt,
	}
}

func getPublication(ctx context.Context, q controlQueryRower, id string) (store.Publication, error) {
	var publication store.Publication
	var prIntentJSON, preparedJSON, publishJSON, verificationJSON, pullRequestJSON []byte
	err := q.QueryRowContext(ctx,
		`SELECT id, namespace, generation, task_uid, attempt, prompt_id, session_uid, branch_claim_id,
		        branch_claim_generation, source_repository_id, source_ref, source_baseline_sha, target_repository_id, target_ref,
		        baseline_absent, baseline_sha, artifact_id, artifact_digest, artifact_size_bytes, artifact_media_type,
		        publication_credential_ref, commit_identity,
		        commit_message, commit_timestamp, pr_intent, request_digest, state, prepared_receipt,
		        publish_receipt, verification_receipt, pr_receipt, terminal_reason, controller_epoch_name,
		        controller_epoch, last_operation_id, last_operation_digest, version, created_at, updated_at
		 FROM publications WHERE id = ?`, id,
	).Scan(
		&publication.ID, &publication.Namespace, &publication.Generation, &publication.TaskUID,
		&publication.Attempt, &publication.PromptID, &publication.SessionUID, &publication.BranchClaimID,
		&publication.BranchClaimGeneration, &publication.SourceRepositoryID, &publication.SourceRef, &publication.SourceBaselineSHA,
		&publication.TargetRepositoryID, &publication.TargetRef, &publication.Baseline.Absent,
		&publication.Baseline.SHA, &publication.ArtifactID, &publication.ArtifactDigest,
		&publication.ArtifactSizeBytes, &publication.ArtifactMediaType, &publication.PublicationCredentialRef,
		&publication.CommitIdentity, &publication.CommitMessage, &publication.CommitTimestamp,
		&prIntentJSON, &publication.RequestDigest, &publication.State, &preparedJSON, &publishJSON,
		&verificationJSON, &pullRequestJSON, &publication.TerminalReason, &publication.ControllerEpochName,
		&publication.ControllerEpoch, &publication.LastOperationID, &publication.LastOperationDigest,
		&publication.Version, &publication.CreatedAt, &publication.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Publication{}, store.ErrNotFound
	}
	if err != nil {
		return store.Publication{}, fmt.Errorf("get publication: %w", err)
	}
	if controlJSONPresent(prIntentJSON) {
		publication.PRIntent = &store.PullRequestIntent{}
		if err := unmarshalOptionalControlJSON(prIntentJSON, publication.PRIntent); err != nil {
			return store.Publication{}, fmt.Errorf("decode publication PR intent: %w", err)
		}
	}
	if controlJSONPresent(preparedJSON) {
		publication.PreparedReceipt = &store.PreparedPublicationReceipt{}
		if err := unmarshalOptionalControlJSON(preparedJSON, publication.PreparedReceipt); err != nil {
			return store.Publication{}, fmt.Errorf("decode prepared receipt: %w", err)
		}
	}
	if controlJSONPresent(publishJSON) {
		publication.PublishReceipt = &store.PublishOperationReceipt{}
		if err := unmarshalOptionalControlJSON(publishJSON, publication.PublishReceipt); err != nil {
			return store.Publication{}, fmt.Errorf("decode publish receipt: %w", err)
		}
	}
	if controlJSONPresent(verificationJSON) {
		publication.VerificationReceipt = &store.PublicationVerificationReceipt{}
		if err := unmarshalOptionalControlJSON(verificationJSON, publication.VerificationReceipt); err != nil {
			return store.Publication{}, fmt.Errorf("decode verification receipt: %w", err)
		}
	}
	if controlJSONPresent(pullRequestJSON) {
		publication.PullRequestReceipt = &store.PullRequestOperationReceipt{}
		if err := unmarshalOptionalControlJSON(pullRequestJSON, publication.PullRequestReceipt); err != nil {
			return store.Publication{}, fmt.Errorf("decode pull request receipt: %w", err)
		}
	}
	return publication, nil
}
