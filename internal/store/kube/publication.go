package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreatePublication persists immutable publication intent and initializes the
// Preparing state after validating the exact BranchClaim fence.
func (s *Store) CreatePublication(ctx context.Context, publication *store.Publication, fence store.ControllerEpochFence) (*store.Publication, error) {
	if err := s.requireClient(); err != nil {
		return nil, err
	}
	normalized, normalizedFence, err := normalizePublicationForCreate(publication, fence)
	if err != nil {
		return nil, err
	}
	_, snapshot, err := s.requireControllerEpoch(ctx, normalizedFence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	if existing, getErr := s.findPublicationByID(ctx, normalized.ID); getErr == nil {
		return s.completePublicationCreation(ctx, existing, normalized, normalizedFence, snapshot)
	} else if !errors.Is(getErr, store.ErrNotFound) {
		return nil, getErr
	}

	key := client.ObjectKey{Namespace: normalized.Namespace, Name: objectName(publicationNamePrefix, normalized.ID)}
	claimObject, err := s.getBranchClaimObject(ctx, normalized.BranchClaimID)
	if err != nil {
		return nil, err
	}
	claim := branchClaimFromObject(claimObject)
	if claim.RepositoryID != normalized.TargetRepositoryID || claim.Ref != normalized.TargetRef || claim.Generation != normalized.BranchClaimGeneration || !claim.LastVerified.Equal(normalized.Baseline) || claim.Availability != store.BranchClaimAvailable {
		return nil, controlConflict("publication %q branch claim does not match exact target, generation, baseline, or availability", normalized.ID)
	}

	labels := controlLabels(normalized.ID)
	labelIfValid(labels, corev1alpha1.ControlRecordTaskUIDLabel, normalized.TaskUID)
	object := &corev1alpha1.Publication{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: normalized.Namespace,
			Name:      key.Name,
			Labels:    labels,
		},
		Spec: publicationSpecToAPI(normalized),
	}
	if err := s.client.Create(ctx, object); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if getErr := s.readClient().Get(ctx, key, object); getErr != nil {
				return nil, mapKubernetesError("get concurrently created publication", getErr)
			}
			return s.completePublicationCreation(ctx, object, normalized, normalizedFence, snapshot)
		}
		return nil, mapKubernetesError("create publication", err)
	}
	return s.completePublicationCreation(ctx, object, normalized, normalizedFence, snapshot)
}

// GetPublication returns a namespaced Publication by canonical ID.
func (s *Store) GetPublication(ctx context.Context, id string) (*store.Publication, error) {
	if err := s.requireClient(); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if err := store.ValidateControlIdentifier("publication ID", id); err != nil {
		return nil, err
	}
	object, err := s.findPublicationByID(ctx, id)
	if err != nil {
		return nil, err
	}
	result := publicationFromObject(object)
	return &result, nil
}

// SetPublicationPRIntent persists the exact forge tuple after preparation.
func (s *Store) SetPublicationPRIntent(ctx context.Context, request store.SetPublicationPRIntentRequest) (*store.Publication, error) {
	request.ID = strings.TrimSpace(request.ID)
	if err := store.ValidateControlIdentifier("publication ID", request.ID); err != nil {
		return nil, err
	}
	fence, snapshot, err := s.requireControllerEpoch(ctx, request.Fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
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

	object, err := s.findPublicationByID(ctx, request.ID)
	if err != nil {
		return nil, err
	}
	publication := publicationFromObject(object)
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

	updated := object.DeepCopy()
	updated.Status.PRIntent = pullRequestIntentToAPI(&request.Intent)
	setMutationStatus(&updated.Status.ControlRecordMutationStatus, fence, snapshot, publication.Version+1, request.OperationID, request.OperationDigest, publication.CreatedAt, request.UpdatedAt)
	if err := s.client.Status().Update(ctx, updated); err != nil {
		return nil, mapKubernetesError("set publication PR intent", err)
	}
	result := publicationFromObject(updated)
	return &result, nil
}

// SetPublicationPRReceipt commits the reconciled forge result before the
// Publication leaves Verifying.
func (s *Store) SetPublicationPRReceipt(ctx context.Context, request store.SetPublicationPRReceiptRequest) (*store.Publication, error) {
	request.ID = strings.TrimSpace(request.ID)
	if err := store.ValidateControlIdentifier("publication ID", request.ID); err != nil {
		return nil, err
	}
	fence, snapshot, err := s.requireControllerEpoch(ctx, request.Fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
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

	object, err := s.findPublicationByID(ctx, request.ID)
	if err != nil {
		return nil, err
	}
	publication := publicationFromObject(object)
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

	updated := object.DeepCopy()
	updated.Status.PullRequestReceipt = pullRequestReceiptToAPI(&request.Receipt)
	setMutationStatus(&updated.Status.ControlRecordMutationStatus, fence, snapshot, publication.Version+1, request.OperationID, request.OperationDigest, publication.CreatedAt, request.UpdatedAt)
	if err := s.client.Status().Update(ctx, updated); err != nil {
		return nil, mapKubernetesError("set publication PR receipt", err)
	}
	result := publicationFromObject(updated)
	return &result, nil
}

// TransitionPublication applies exact generation/version/state,
// resourceVersion, receipt, and controller-epoch fences.
func (s *Store) TransitionPublication(ctx context.Context, transition store.PublicationTransition) (*store.Publication, error) {
	transition.ID = strings.TrimSpace(transition.ID)
	if err := store.ValidateControlIdentifier("publication ID", transition.ID); err != nil {
		return nil, err
	}
	fence, snapshot, err := s.requireControllerEpoch(ctx, transition.Fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
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

	object, err := s.findPublicationByID(ctx, transition.ID)
	if err != nil {
		return nil, err
	}
	publication := publicationFromObject(object)
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

	updated := object.DeepCopy()
	updated.Status.State = corev1alpha1.PublicationControlState(transition.NewState)
	if transition.PreparedReceipt != nil {
		updated.Status.PreparedReceipt = preparedReceiptToAPI(transition.PreparedReceipt)
	}
	if transition.PublishReceipt != nil {
		updated.Status.PublishReceipt = publishReceiptToAPI(transition.PublishReceipt)
	}
	if transition.VerificationReceipt != nil {
		updated.Status.VerificationReceipt = verificationReceiptToAPI(transition.VerificationReceipt)
	}
	if transition.TerminalReason != "" || store.IsTerminalPublicationState(transition.NewState) {
		updated.Status.TerminalReason = transition.TerminalReason
	}
	setMutationStatus(&updated.Status.ControlRecordMutationStatus, fence, snapshot, publication.Version+1, transition.OperationID, transition.OperationDigest, publication.CreatedAt, transition.UpdatedAt)
	if err := s.client.Status().Update(ctx, updated); err != nil {
		return nil, mapKubernetesError("transition publication", err)
	}
	result := publicationFromObject(updated)
	return &result, nil
}

func (s *Store) completePublicationCreation(ctx context.Context, object *corev1alpha1.Publication, normalized store.Publication, fence store.ControllerEpochFence, snapshot epochSnapshot) (*store.Publication, error) {
	if !samePublicationSpec(object, normalized) {
		return nil, controlConflict("publication %q was reused with different immutable intent or request digest", normalized.ID)
	}
	if object.Status.Version > 0 {
		existing := publicationFromObject(object)
		if !samePublicationCreation(existing, normalized) {
			return nil, controlConflict("publication %q was reused with different immutable intent or request digest", normalized.ID)
		}
		return &existing, nil
	}
	updated := object.DeepCopy()
	updated.Status.State = corev1alpha1.PublicationControlState(normalized.State)
	updated.Status.PRIntent = pullRequestIntentToAPI(normalized.PRIntent)
	updated.Status.TerminalReason = normalized.TerminalReason
	setMutationStatus(&updated.Status.ControlRecordMutationStatus, fence, snapshot, 1, "", "", normalized.CreatedAt, normalized.UpdatedAt)
	if err := s.client.Status().Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			fresh, getErr := s.findPublicationByID(ctx, normalized.ID)
			if getErr == nil && samePublicationSpec(fresh, normalized) && fresh.Status.Version > 0 {
				result := publicationFromObject(fresh)
				if samePublicationCreation(result, normalized) {
					return &result, nil
				}
			}
		}
		return nil, mapKubernetesError("initialize publication status", err)
	}
	result := publicationFromObject(updated)
	return &result, nil
}

func (s *Store) findPublicationByID(ctx context.Context, id string) (*corev1alpha1.Publication, error) {
	list := &corev1alpha1.PublicationList{}
	if err := s.readClient().List(ctx, list, client.MatchingLabels{corev1alpha1.ControlRecordIDHashLabel: dnsDigest(id)}); err != nil {
		return nil, mapKubernetesError("list publications", err)
	}
	var match *corev1alpha1.Publication
	for i := range list.Items {
		if list.Items[i].Spec.ID != id {
			continue
		}
		if match != nil {
			return nil, controlConflict("multiple publications exist for canonical ID %q", id)
		}
		match = list.Items[i].DeepCopy()
	}
	if match == nil {
		return nil, store.ErrNotFound
	}
	return match, nil
}

//nolint:gocyclo // Publication immutable-input validation is intentionally centralized.
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
		"publication ID":                   normalized.ID,
		"publication namespace":            normalized.Namespace,
		"publication task UID":             normalized.TaskUID,
		"publication prompt ID":            normalized.PromptID,
		"branch claim ID":                  normalized.BranchClaimID,
		"source repository ID":             normalized.SourceRepositoryID,
		"source ref":                       normalized.SourceRef,
		"target repository ID":             normalized.TargetRepositoryID,
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
	normalized.SourceBaselineSHA = strings.TrimSpace(normalized.SourceBaselineSHA)
	if err := store.ValidateGitObjectID("publication source baseline SHA", normalized.SourceBaselineSHA); err != nil {
		return store.Publication{}, store.ControllerEpochFence{}, err
	}
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
		normalizePullRequestIntent(&copyIntent)
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
	normalizedFence, err := normalizeEpochFence(fence)
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
	normalized.ControllerEpochName = normalizedFence.Name
	normalized.ControllerEpoch = normalizedFence.Epoch
	normalized.Version = 1
	normalized.LastOperationID = ""
	normalized.LastOperationDigest = ""
	normalized.TerminalReason = ""
	return normalized, normalizedFence, nil
}

func normalizePullRequestIntent(intent *store.PullRequestIntent) {
	intent.BaseRepositoryID = strings.TrimSpace(intent.BaseRepositoryID)
	intent.BaseRef = strings.TrimSpace(intent.BaseRef)
	intent.HeadRepositoryID = strings.TrimSpace(intent.HeadRepositoryID)
	intent.HeadRef = strings.TrimSpace(intent.HeadRef)
	intent.ExpectedHeadSHA = strings.TrimSpace(intent.ExpectedHeadSHA)
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

func validatePullRequestReceipt(receipt store.PullRequestOperationReceipt) error {
	for field, value := range map[string]string{
		"PR receipt operation ID": receipt.OperationID,
		"PR receipt intent key":   receipt.IntentKey,
		"PR receipt forge ID":     receipt.ForgeID,
		"PR receipt URL":          receipt.URL,
		"PR receipt state":        receipt.State,
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

//nolint:gocyclo // Destination states have distinct exact-receipt invariants.
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
	case store.PublicationDeliveryConflict, store.PublicationCredentialBlocked, store.PublicationPreparationFailed, store.PublicationOutcomeUnknown:
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
	return a.OperationID == b.OperationID && a.RequestDigest == b.RequestDigest && a.TargetRepositoryID == b.TargetRepositoryID && a.TargetRef == b.TargetRef && a.RemoteBefore.Equal(b.RemoteBefore) && a.ExpectedCommitSHA == b.ExpectedCommitSHA && a.AcknowledgementUnknown == b.AcknowledgementUnknown && a.PublishedAt.Equal(b.PublishedAt)
}

func equalVerificationReceipt(a, b *store.PublicationVerificationReceipt) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.OperationID == b.OperationID && a.RequestDigest == b.RequestDigest && a.Outcome == b.Outcome && a.ExpectedCommitSHA == b.ExpectedCommitSHA && a.ObservedRemote.Equal(b.ObservedRemote) && a.DescendantProofDigest == b.DescendantProofDigest && a.VerifiedAt.Equal(b.VerifiedAt)
}

func samePublicationCreation(a, b store.Publication) bool {
	prIntentMatches := b.PRIntent == nil || reflect.DeepEqual(a.PRIntent, b.PRIntent)
	return a.ID == b.ID && a.Namespace == b.Namespace && a.Generation == b.Generation && a.TaskUID == b.TaskUID && a.Attempt == b.Attempt && a.PromptID == b.PromptID && a.SessionUID == b.SessionUID && a.BranchClaimID == b.BranchClaimID && a.BranchClaimGeneration == b.BranchClaimGeneration && a.SourceRepositoryID == b.SourceRepositoryID && a.SourceRef == b.SourceRef && a.SourceBaselineSHA == b.SourceBaselineSHA && a.TargetRepositoryID == b.TargetRepositoryID && a.TargetRef == b.TargetRef && a.Baseline.Equal(b.Baseline) && a.ArtifactID == b.ArtifactID && a.ArtifactDigest == b.ArtifactDigest && a.ArtifactSizeBytes == b.ArtifactSizeBytes && a.ArtifactMediaType == b.ArtifactMediaType && a.PublicationCredentialRef == b.PublicationCredentialRef && a.CommitIdentity == b.CommitIdentity && a.CommitMessage == b.CommitMessage && a.CommitTimestamp.Equal(b.CommitTimestamp) && prIntentMatches && a.RequestDigest == b.RequestDigest
}

func samePublicationSpec(object *corev1alpha1.Publication, value store.Publication) bool {
	if object == nil || object.Namespace != value.Namespace {
		return false
	}
	actual := object.Spec
	expected := publicationSpecToAPI(value)
	if !actual.CommitTimestamp.Time.Equal(expected.CommitTimestamp.Time) {
		return false
	}
	actual.CommitTimestamp = metav1.Time{}
	expected.CommitTimestamp = metav1.Time{}
	return reflect.DeepEqual(actual, expected)
}

func publicationSpecToAPI(value store.Publication) corev1alpha1.PublicationSpec {
	return corev1alpha1.PublicationSpec{
		ID:                       value.ID,
		Generation:               value.Generation,
		TaskUID:                  value.TaskUID,
		Attempt:                  value.Attempt,
		PromptID:                 value.PromptID,
		SessionUID:               value.SessionUID,
		BranchClaimID:            value.BranchClaimID,
		BranchClaimGeneration:    value.BranchClaimGeneration,
		SourceRepositoryID:       value.SourceRepositoryID,
		SourceRef:                value.SourceRef,
		SourceBaselineSHA:        value.SourceBaselineSHA,
		TargetRepositoryID:       value.TargetRepositoryID,
		TargetRef:                value.TargetRef,
		Baseline:                 remoteRefToAPI(value.Baseline),
		ArtifactID:               value.ArtifactID,
		ArtifactDigest:           value.ArtifactDigest,
		ArtifactSizeBytes:        value.ArtifactSizeBytes,
		ArtifactMediaType:        value.ArtifactMediaType,
		PublicationCredentialRef: value.PublicationCredentialRef,
		CommitIdentity:           value.CommitIdentity,
		CommitMessage:            value.CommitMessage,
		CommitTimestamp:          metav1.NewTime(value.CommitTimestamp),
		RequestDigest:            value.RequestDigest,
	}
}

func publicationFromObject(object *corev1alpha1.Publication) store.Publication {
	return store.Publication{
		ID:                       object.Spec.ID,
		Namespace:                object.Namespace,
		Generation:               object.Spec.Generation,
		TaskUID:                  object.Spec.TaskUID,
		Attempt:                  object.Spec.Attempt,
		PromptID:                 object.Spec.PromptID,
		SessionUID:               object.Spec.SessionUID,
		BranchClaimID:            object.Spec.BranchClaimID,
		BranchClaimGeneration:    object.Spec.BranchClaimGeneration,
		SourceRepositoryID:       object.Spec.SourceRepositoryID,
		SourceRef:                object.Spec.SourceRef,
		SourceBaselineSHA:        object.Spec.SourceBaselineSHA,
		TargetRepositoryID:       object.Spec.TargetRepositoryID,
		TargetRef:                object.Spec.TargetRef,
		Baseline:                 remoteRefFromAPI(object.Spec.Baseline),
		ArtifactID:               object.Spec.ArtifactID,
		ArtifactDigest:           object.Spec.ArtifactDigest,
		ArtifactSizeBytes:        object.Spec.ArtifactSizeBytes,
		ArtifactMediaType:        object.Spec.ArtifactMediaType,
		PublicationCredentialRef: object.Spec.PublicationCredentialRef,
		CommitIdentity:           object.Spec.CommitIdentity,
		CommitMessage:            object.Spec.CommitMessage,
		CommitTimestamp:          object.Spec.CommitTimestamp.UTC(),
		PRIntent:                 pullRequestIntentFromAPI(object.Status.PRIntent),
		RequestDigest:            object.Spec.RequestDigest,
		State:                    store.PublicationState(object.Status.State),
		PreparedReceipt:          preparedReceiptFromAPI(object.Status.PreparedReceipt),
		PublishReceipt:           publishReceiptFromAPI(object.Status.PublishReceipt),
		VerificationReceipt:      verificationReceiptFromAPI(object.Status.VerificationReceipt),
		PullRequestReceipt:       pullRequestReceiptFromAPI(object.Status.PullRequestReceipt),
		TerminalReason:           object.Status.TerminalReason,
		ControllerEpochName:      object.Status.ControllerEpochName,
		ControllerEpoch:          object.Status.ControllerEpoch,
		LastOperationID:          object.Status.LastOperationID,
		LastOperationDigest:      object.Status.LastOperationDigest,
		Version:                  object.Status.Version,
		CreatedAt:                timeValue(object.Status.CreatedAt),
		UpdatedAt:                timeValue(object.Status.UpdatedAt),
	}
}

func pullRequestIntentToAPI(value *store.PullRequestIntent) *corev1alpha1.PublicationPullRequestIntent {
	if value == nil {
		return nil
	}
	return &corev1alpha1.PublicationPullRequestIntent{BaseRepositoryID: value.BaseRepositoryID, BaseRef: value.BaseRef, HeadRepositoryID: value.HeadRepositoryID, HeadRef: value.HeadRef, PublicationGeneration: value.PublicationGeneration, ExpectedHeadSHA: value.ExpectedHeadSHA}
}

func pullRequestIntentFromAPI(value *corev1alpha1.PublicationPullRequestIntent) *store.PullRequestIntent {
	if value == nil {
		return nil
	}
	return &store.PullRequestIntent{BaseRepositoryID: value.BaseRepositoryID, BaseRef: value.BaseRef, HeadRepositoryID: value.HeadRepositoryID, HeadRef: value.HeadRef, PublicationGeneration: value.PublicationGeneration, ExpectedHeadSHA: value.ExpectedHeadSHA}
}

func preparedReceiptToAPI(value *store.PreparedPublicationReceipt) *corev1alpha1.PreparedPublicationControlReceipt {
	if value == nil {
		return nil
	}
	return &corev1alpha1.PreparedPublicationControlReceipt{
		OperationID: value.OperationID, RequestDigest: value.RequestDigest,
		TreeSHA: value.TreeSHA, CommitSHA: value.CommitSHA, ManifestDigest: value.ManifestDigest, RelativeRoot: value.RelativeRoot,
		BundleArtifactID: value.BundleArtifactID, BundleDigest: value.BundleDigest,
		BundleSizeBytes: value.BundleSizeBytes, BundleMediaType: value.BundleMediaType, BundleRef: value.BundleRef,
		PreparedAt: metav1.NewTime(value.PreparedAt),
	}
}

func preparedReceiptFromAPI(value *corev1alpha1.PreparedPublicationControlReceipt) *store.PreparedPublicationReceipt {
	if value == nil {
		return nil
	}
	return &store.PreparedPublicationReceipt{
		OperationID: value.OperationID, RequestDigest: value.RequestDigest,
		TreeSHA: value.TreeSHA, CommitSHA: value.CommitSHA, ManifestDigest: value.ManifestDigest, RelativeRoot: value.RelativeRoot,
		BundleArtifactID: value.BundleArtifactID, BundleDigest: value.BundleDigest,
		BundleSizeBytes: value.BundleSizeBytes, BundleMediaType: value.BundleMediaType, BundleRef: value.BundleRef,
		PreparedAt: value.PreparedAt.UTC(),
	}
}

func publishReceiptToAPI(value *store.PublishOperationReceipt) *corev1alpha1.PublishOperationControlReceipt {
	if value == nil {
		return nil
	}
	return &corev1alpha1.PublishOperationControlReceipt{OperationID: value.OperationID, RequestDigest: value.RequestDigest, TargetRepositoryID: value.TargetRepositoryID, TargetRef: value.TargetRef, RemoteBefore: remoteRefToAPI(value.RemoteBefore), ExpectedCommitSHA: value.ExpectedCommitSHA, AcknowledgementUnknown: value.AcknowledgementUnknown, PublishedAt: metav1.NewTime(value.PublishedAt)}
}

func publishReceiptFromAPI(value *corev1alpha1.PublishOperationControlReceipt) *store.PublishOperationReceipt {
	if value == nil {
		return nil
	}
	return &store.PublishOperationReceipt{OperationID: value.OperationID, RequestDigest: value.RequestDigest, TargetRepositoryID: value.TargetRepositoryID, TargetRef: value.TargetRef, RemoteBefore: remoteRefFromAPI(value.RemoteBefore), ExpectedCommitSHA: value.ExpectedCommitSHA, AcknowledgementUnknown: value.AcknowledgementUnknown, PublishedAt: value.PublishedAt.UTC()}
}

func verificationReceiptToAPI(value *store.PublicationVerificationReceipt) *corev1alpha1.PublicationVerificationControlReceipt {
	if value == nil {
		return nil
	}
	return &corev1alpha1.PublicationVerificationControlReceipt{OperationID: value.OperationID, RequestDigest: value.RequestDigest, Outcome: corev1alpha1.PublicationControlState(value.Outcome), ExpectedCommitSHA: value.ExpectedCommitSHA, ObservedRemote: remoteRefToAPI(value.ObservedRemote), DescendantProofDigest: value.DescendantProofDigest, VerifiedAt: metav1.NewTime(value.VerifiedAt)}
}

func verificationReceiptFromAPI(value *corev1alpha1.PublicationVerificationControlReceipt) *store.PublicationVerificationReceipt {
	if value == nil {
		return nil
	}
	return &store.PublicationVerificationReceipt{OperationID: value.OperationID, RequestDigest: value.RequestDigest, Outcome: store.PublicationState(value.Outcome), ExpectedCommitSHA: value.ExpectedCommitSHA, ObservedRemote: remoteRefFromAPI(value.ObservedRemote), DescendantProofDigest: value.DescendantProofDigest, VerifiedAt: value.VerifiedAt.UTC()}
}

func pullRequestReceiptToAPI(value *store.PullRequestOperationReceipt) *corev1alpha1.PullRequestOperationControlReceipt {
	if value == nil {
		return nil
	}
	return &corev1alpha1.PullRequestOperationControlReceipt{OperationID: value.OperationID, RequestDigest: value.RequestDigest, IntentKey: value.IntentKey, ForgeID: value.ForgeID, URL: value.URL, State: value.State, HeadSHA: value.HeadSHA, ReconciledAt: metav1.NewTime(value.ReconciledAt)}
}

func pullRequestReceiptFromAPI(value *corev1alpha1.PullRequestOperationControlReceipt) *store.PullRequestOperationReceipt {
	if value == nil {
		return nil
	}
	return &store.PullRequestOperationReceipt{OperationID: value.OperationID, RequestDigest: value.RequestDigest, IntentKey: value.IntentKey, ForgeID: value.ForgeID, URL: value.URL, State: value.State, HeadSHA: value.HeadSHA, ReconciledAt: value.ReconciledAt.UTC()}
}
