package kube

import (
	"context"
	"errors"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ store.BranchClaimCreationStore = (*Store)(nil)

// CreateBranchClaim creates the cluster-scoped canonical repository/ref claim.
func (s *Store) CreateBranchClaim(ctx context.Context, claim *store.BranchClaim, fence store.ControllerEpochFence) (*store.BranchClaim, error) {
	created, _, err := s.CreateBranchClaimWithResult(ctx, claim, fence)
	return created, err
}

// CreateBranchClaimWithResult reports true only when this call created the
// immutable Kubernetes object rather than observing an existing equivalent.
func (s *Store) CreateBranchClaimWithResult(ctx context.Context, claim *store.BranchClaim, fence store.ControllerEpochFence) (*store.BranchClaim, bool, error) {
	if err := s.requireClient(); err != nil {
		return nil, false, err
	}
	normalized, normalizedFence, err := normalizeBranchClaimForCreate(claim, fence)
	if err != nil {
		return nil, false, err
	}
	_, snapshot, err := s.requireControllerEpoch(ctx, normalizedFence)
	if err != nil {
		return nil, false, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	if normalized.OwnerKind == store.BranchClaimOwnerSession {
		fenced, fenceErr := s.sessionCleanupFencedForUID(ctx, normalized.OwnerUID)
		if fenceErr != nil {
			return nil, false, fenceErr
		}
		if fenced {
			return nil, false, controlConflict("Session UID %q is being deleted or was already deleted", normalized.OwnerUID)
		}
	}

	key := client.ObjectKey{Name: objectName(branchClaimNamePrefix, normalized.ID)}
	object := &corev1alpha1.BranchClaim{}
	err = s.readClient().Get(ctx, key, object)
	if err == nil {
		result, completeErr := s.completeBranchClaimCreation(ctx, object, normalized, normalizedFence, snapshot)
		return result, false, completeErr
	}
	if !apierrors.IsNotFound(err) {
		return nil, false, mapKubernetesError("get branch claim", err)
	}

	object = &corev1alpha1.BranchClaim{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Labels: controlLabels(normalized.ID)},
		Spec: corev1alpha1.BranchClaimSpec{
			ID:            normalized.ID,
			RepositoryID:  normalized.RepositoryID,
			Ref:           normalized.Ref,
			OwnerKind:     corev1alpha1.BranchClaimOwnerKind(normalized.OwnerKind),
			OwnerUID:      normalized.OwnerUID,
			RequestDigest: normalized.RequestDigest,
		},
	}
	if createErr := s.client.Create(ctx, object); createErr != nil {
		fresh := &corev1alpha1.BranchClaim{}
		if getErr := s.readClient().Get(ctx, key, fresh); getErr == nil {
			result, completeErr := s.completeBranchClaimCreation(ctx, fresh, normalized, normalizedFence, snapshot)
			return result, false, completeErr
		} else if apierrors.IsAlreadyExists(createErr) {
			return nil, false, mapKubernetesError("get concurrently created branch claim", getErr)
		}
		return nil, false, mapKubernetesError("create branch claim", createErr)
	}
	result, completeErr := s.completeBranchClaimCreation(ctx, object, normalized, normalizedFence, snapshot)
	return result, completeErr == nil, completeErr
}

// GetBranchClaim returns a cluster-scoped claim by canonical ID.
func (s *Store) GetBranchClaim(ctx context.Context, id string) (*store.BranchClaim, error) {
	if err := s.requireClient(); err != nil {
		return nil, err
	}
	id = strings.TrimSpace(id)
	if err := store.ValidateControlIdentifier("branch claim ID", id); err != nil {
		return nil, err
	}
	object, err := s.getBranchClaimObject(ctx, id)
	if err != nil {
		return nil, err
	}
	result := branchClaimFromObject(object)
	return &result, nil
}

// ReclaimBranchClaim deletes only the exact available owner-fenced object. UID
// and resourceVersion preconditions close the read/delete race. Absence or a
// different immutable owner/request identity means the original claim was
// already reclaimed and possibly replaced, so the retry is a safe no-op.
func (s *Store) ReclaimBranchClaim(ctx context.Context, request store.ReclaimBranchClaimRequest) error {
	if err := s.requireClient(); err != nil {
		return err
	}
	normalized, err := normalizeBranchClaimReclamationRequest(request)
	if err != nil {
		return err
	}
	fence, snapshot, err := s.requireControllerEpoch(ctx, normalized.Fence)
	if err != nil {
		return err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	normalized.Fence = fence

	object, err := s.getBranchClaimObject(ctx, normalized.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	claim := branchClaimFromObject(object)
	if branchClaimReclamationIdentityReplaced(claim, normalized) {
		return nil
	}
	if !branchClaimMatchesReclamation(claim, normalized) {
		return controlConflict("branch claim %q no longer matches the exact Task-owner reclamation fence", claim.ID)
	}
	uid := object.UID
	resourceVersion := object.ResourceVersion
	deleteErr := s.client.Delete(ctx, object, &client.DeleteOptions{Preconditions: &metav1.Preconditions{
		UID: &uid, ResourceVersion: &resourceVersion,
	}})
	if deleteErr == nil || apierrors.IsNotFound(deleteErr) {
		return nil
	}
	fresh, getErr := s.getBranchClaimObject(ctx, normalized.ID)
	if errors.Is(getErr, store.ErrNotFound) {
		return nil
	}
	if getErr == nil && branchClaimReclamationIdentityReplaced(branchClaimFromObject(fresh), normalized) {
		return nil
	}
	if getErr != nil {
		return getErr
	}
	return mapKubernetesError("delete reclaimed branch claim", deleteErr)
}

// CompareAndSwapBranchClaim applies exact version, generation, baseline,
// availability, resourceVersion, and controller-epoch fences.
func (s *Store) CompareAndSwapBranchClaim(ctx context.Context, change store.BranchClaimCAS) (*store.BranchClaim, error) {
	change.ID = strings.TrimSpace(change.ID)
	if err := store.ValidateControlIdentifier("branch claim ID", change.ID); err != nil {
		return nil, err
	}
	fence, snapshot, err := s.requireControllerEpoch(ctx, change.Fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	change.Fence = fence
	if err := validateBranchClaimCAS(&change); err != nil {
		return nil, err
	}

	object, err := s.getBranchClaimObject(ctx, change.ID)
	if err != nil {
		return nil, err
	}
	claim := branchClaimFromObject(object)
	if claim.LastOperationID == change.OperationID {
		if claim.LastOperationDigest != change.OperationDigest {
			return nil, controlConflict("branch claim operation %q was reused with a different digest", change.OperationID)
		}
		if claim.Generation == change.NewGeneration && claim.LastVerified.Equal(change.NewLastVerified) && claim.Availability == change.NewAvailability && claim.BlockedReason == change.BlockedReason && claim.RelatedPublicationID == change.RelatedPublicationID {
			return &claim, nil
		}
		return nil, controlConflict("branch claim operation %q was already applied with different target values", change.OperationID)
	}
	if claim.Version != change.ExpectedVersion || claim.Generation != change.ExpectedGeneration || !claim.LastVerified.Equal(change.ExpectedLastVerified) || claim.Availability != change.ExpectedAvailability {
		return nil, controlConflict("branch claim %q no longer matches expected version, generation, baseline, or availability", claim.ID)
	}

	updated := object.DeepCopy()
	updated.Status.Generation = change.NewGeneration
	remote := remoteRefToAPI(change.NewLastVerified)
	updated.Status.LastVerified = &remote
	updated.Status.Availability = corev1alpha1.BranchClaimAvailability(change.NewAvailability)
	updated.Status.BlockedReason = change.BlockedReason
	updated.Status.RelatedPublicationID = change.RelatedPublicationID
	setMutationStatus(&updated.Status.ControlRecordMutationStatus, fence, snapshot, claim.Version+1, change.OperationID, change.OperationDigest, claim.CreatedAt, change.UpdatedAt)
	if err := s.client.Status().Update(ctx, updated); err != nil {
		return nil, mapKubernetesError("compare-and-swap branch claim", err)
	}
	result := branchClaimFromObject(updated)
	return &result, nil
}

func (s *Store) completeBranchClaimCreation(ctx context.Context, object *corev1alpha1.BranchClaim, normalized store.BranchClaim, fence store.ControllerEpochFence, snapshot epochSnapshot) (*store.BranchClaim, error) {
	if !sameBranchClaimSpec(object, normalized) {
		return nil, controlConflict("branch claim %q was reused with a different owner or request digest", normalized.ID)
	}
	if object.Status.Version > 0 {
		existing := branchClaimFromObject(object)
		return &existing, nil
	}
	updated := object.DeepCopy()
	updated.Status.Generation = normalized.Generation
	remote := remoteRefToAPI(normalized.LastVerified)
	updated.Status.LastVerified = &remote
	updated.Status.Availability = corev1alpha1.BranchClaimAvailability(normalized.Availability)
	updated.Status.BlockedReason = normalized.BlockedReason
	updated.Status.RelatedPublicationID = normalized.RelatedPublicationID
	setMutationStatus(&updated.Status.ControlRecordMutationStatus, fence, snapshot, 1, "", "", normalized.CreatedAt, normalized.UpdatedAt)
	if err := s.client.Status().Update(ctx, updated); err != nil {
		if apierrors.IsConflict(err) {
			fresh, getErr := s.getBranchClaimObject(ctx, normalized.ID)
			if getErr == nil && sameBranchClaimSpec(fresh, normalized) && fresh.Status.Version > 0 {
				result := branchClaimFromObject(fresh)
				if result.LastVerified.Equal(normalized.LastVerified) {
					return &result, nil
				}
			}
		}
		return nil, mapKubernetesError("initialize branch claim status", err)
	}
	result := branchClaimFromObject(updated)
	return &result, nil
}

func (s *Store) getBranchClaimObject(ctx context.Context, id string) (*corev1alpha1.BranchClaim, error) {
	object := &corev1alpha1.BranchClaim{}
	key := client.ObjectKey{Name: objectName(branchClaimNamePrefix, id)}
	if err := s.readClient().Get(ctx, key, object); err != nil {
		return nil, mapKubernetesError("get branch claim", err)
	}
	if object.Spec.ID != id {
		return nil, controlConflict("branch claim object %q has a different canonical ID", object.Name)
	}
	return object, nil
}

func normalizeBranchClaimForCreate(claim *store.BranchClaim, fence store.ControllerEpochFence) (store.BranchClaim, store.ControllerEpochFence, error) {
	if claim == nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("branch claim is required")
	}
	normalized := *claim
	normalized.RepositoryID = strings.TrimSpace(normalized.RepositoryID)
	normalized.Ref = strings.TrimSpace(normalized.Ref)
	normalized.OwnerUID = strings.TrimSpace(normalized.OwnerUID)
	if err := store.ValidateControlIdentifier("publication repository ID", normalized.RepositoryID); err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	if err := store.ValidateFullBranchRef(normalized.Ref); err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	canonicalID, err := store.CanonicalBranchClaimID(normalized.RepositoryID, normalized.Ref)
	if err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	normalized.ID = strings.TrimSpace(normalized.ID)
	if normalized.ID == "" {
		normalized.ID = canonicalID
	}
	if normalized.ID != canonicalID {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("branch claim ID must equal canonical ID %q", canonicalID)
	}
	if normalized.OwnerKind != store.BranchClaimOwnerTask && normalized.OwnerKind != store.BranchClaimOwnerSession {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("unsupported branch claim owner kind %q", normalized.OwnerKind)
	}
	if err := store.ValidateControlIdentifier("branch claim owner UID", normalized.OwnerUID); err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	if normalized.Generation == 0 {
		normalized.Generation = 1
	}
	if normalized.Generation != 1 {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("new branch claim generation must be one")
	}
	if err := normalized.LastVerified.Validate("branch baseline"); err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	if normalized.Availability == "" {
		normalized.Availability = store.BranchClaimAvailable
	}
	if !isKnownBranchClaimAvailability(normalized.Availability) {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("unsupported branch claim availability %q", normalized.Availability)
	}
	normalized.BlockedReason = strings.TrimSpace(normalized.BlockedReason)
	normalized.RelatedPublicationID = strings.TrimSpace(normalized.RelatedPublicationID)
	if normalized.Availability == store.BranchClaimAvailable && (normalized.BlockedReason != "" || normalized.RelatedPublicationID != "") {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("available branch claim must not have block metadata")
	}
	if normalized.Availability == store.BranchClaimReconciliationBlocked && normalized.BlockedReason == "" {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("reconciliation-blocked branch claim requires a reason")
	}
	if err := store.ValidateControlReason("branch claim blocked reason", normalized.BlockedReason); err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	if err := store.ValidateCanonicalDigest("branch claim request digest", normalized.RequestDigest); err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	normalizedFence, err := normalizeEpochFence(fence)
	if err != nil {
		return store.BranchClaim{}, store.ControllerEpochFence{}, err
	}
	if normalized.Version != 0 && normalized.Version != 1 {
		return store.BranchClaim{}, store.ControllerEpochFence{}, store.ValidationErrorf("new branch claim version must be zero or one")
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
	return normalized, normalizedFence, nil
}

func normalizeBranchClaimReclamationRequest(request store.ReclaimBranchClaimRequest) (store.ReclaimBranchClaimRequest, error) {
	request.ID = strings.TrimSpace(request.ID)
	request.ExpectedRepositoryID = strings.TrimSpace(request.ExpectedRepositoryID)
	request.ExpectedRef = strings.TrimSpace(request.ExpectedRef)
	request.ExpectedOwnerUID = strings.TrimSpace(request.ExpectedOwnerUID)
	request.ExpectedRequestDigest = strings.TrimSpace(request.ExpectedRequestDigest)
	if err := store.ValidateControlIdentifier("branch claim ID", request.ID); err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	if err := store.ValidateControlIdentifier("publication repository ID", request.ExpectedRepositoryID); err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	if err := store.ValidateFullBranchRef(request.ExpectedRef); err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	canonicalID, err := store.CanonicalBranchClaimID(request.ExpectedRepositoryID, request.ExpectedRef)
	if err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	if request.ID != canonicalID {
		return store.ReclaimBranchClaimRequest{}, store.ValidationErrorf("branch claim ID must equal canonical ID %q", canonicalID)
	}
	if request.ExpectedOwnerKind != store.BranchClaimOwnerTask && request.ExpectedOwnerKind != store.BranchClaimOwnerSession {
		return store.ReclaimBranchClaimRequest{}, store.ValidationErrorf("branch claim owner kind is not reclaimable")
	}
	if err := store.ValidateControlIdentifier("branch claim owner UID", request.ExpectedOwnerUID); err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	if request.ExpectedVersion < 1 || request.ExpectedGeneration < 1 {
		return store.ReclaimBranchClaimRequest{}, store.ValidationErrorf("branch claim expected version and generation must be at least 1")
	}
	if err := request.ExpectedLastVerified.Validate("expected branch baseline"); err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	if request.ExpectedAvailability != store.BranchClaimAvailable {
		return store.ReclaimBranchClaimRequest{}, store.ValidationErrorf("only available branch claims may be reclaimed")
	}
	if err := store.ValidateCanonicalDigest("branch claim request digest", request.ExpectedRequestDigest); err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	normalizedFence, err := normalizeEpochFence(request.Fence)
	if err != nil {
		return store.ReclaimBranchClaimRequest{}, err
	}
	request.Fence = normalizedFence
	return request, nil
}

func branchClaimReclamationIdentityReplaced(claim store.BranchClaim, request store.ReclaimBranchClaimRequest) bool {
	return claim.OwnerKind != request.ExpectedOwnerKind || claim.OwnerUID != request.ExpectedOwnerUID || claim.RequestDigest != request.ExpectedRequestDigest
}

func branchClaimMatchesReclamation(claim store.BranchClaim, request store.ReclaimBranchClaimRequest) bool {
	return claim.ID == request.ID && claim.RepositoryID == request.ExpectedRepositoryID && claim.Ref == request.ExpectedRef &&
		claim.OwnerKind == request.ExpectedOwnerKind && claim.OwnerUID == request.ExpectedOwnerUID &&
		claim.Generation == request.ExpectedGeneration && claim.LastVerified.Equal(request.ExpectedLastVerified) &&
		claim.Availability == request.ExpectedAvailability && claim.BlockedReason == "" && claim.RelatedPublicationID == "" &&
		claim.RequestDigest == request.ExpectedRequestDigest && claim.Version == request.ExpectedVersion
}

func validateBranchClaimCAS(change *store.BranchClaimCAS) error {
	if change.ExpectedVersion < 1 || change.ExpectedGeneration < 1 {
		return store.ValidationErrorf("branch claim expected version and generation must be at least 1")
	}
	if change.NewGeneration != change.ExpectedGeneration && change.NewGeneration != change.ExpectedGeneration+1 {
		return store.ValidationErrorf("branch claim generation may be preserved or incremented exactly by one")
	}
	if err := change.ExpectedLastVerified.Validate("expected branch baseline"); err != nil {
		return err
	}
	if err := change.NewLastVerified.Validate("new branch baseline"); err != nil {
		return err
	}
	if !isKnownBranchClaimAvailability(change.ExpectedAvailability) || !isKnownBranchClaimAvailability(change.NewAvailability) {
		return store.ValidationErrorf("unsupported branch claim availability transition %q -> %q", change.ExpectedAvailability, change.NewAvailability)
	}
	change.BlockedReason = strings.TrimSpace(change.BlockedReason)
	change.RelatedPublicationID = strings.TrimSpace(change.RelatedPublicationID)
	if err := store.ValidateControlReason("branch claim blocked reason", change.BlockedReason); err != nil {
		return err
	}
	if change.NewAvailability == store.BranchClaimAvailable && (change.BlockedReason != "" || change.RelatedPublicationID != "") {
		return store.ValidationErrorf("available branch claim must clear blocked reason and related publication")
	}
	if change.NewAvailability == store.BranchClaimReconciliationBlocked && change.BlockedReason == "" {
		return store.ValidationErrorf("reconciliation-blocked branch claim requires a reason")
	}
	change.OperationID = strings.TrimSpace(change.OperationID)
	if err := store.ValidateControlIdentifier("branch claim operation ID", change.OperationID); err != nil {
		return err
	}
	if err := store.ValidateCanonicalDigest("branch claim operation digest", change.OperationDigest); err != nil {
		return err
	}
	change.UpdatedAt = normalizeControlTime(change.UpdatedAt)
	return nil
}

func sameBranchClaimSpec(object *corev1alpha1.BranchClaim, claim store.BranchClaim) bool {
	return object.Spec.ID == claim.ID && object.Spec.RepositoryID == claim.RepositoryID && object.Spec.Ref == claim.Ref &&
		store.BranchClaimOwnerKind(object.Spec.OwnerKind) == claim.OwnerKind && object.Spec.OwnerUID == claim.OwnerUID && object.Spec.RequestDigest == claim.RequestDigest
}

func branchClaimFromObject(object *corev1alpha1.BranchClaim) store.BranchClaim {
	result := store.BranchClaim{
		ID:                   object.Spec.ID,
		RepositoryID:         object.Spec.RepositoryID,
		Ref:                  object.Spec.Ref,
		OwnerKind:            store.BranchClaimOwnerKind(object.Spec.OwnerKind),
		OwnerUID:             object.Spec.OwnerUID,
		Generation:           object.Status.Generation,
		Availability:         store.BranchClaimAvailability(object.Status.Availability),
		BlockedReason:        object.Status.BlockedReason,
		RelatedPublicationID: object.Status.RelatedPublicationID,
		RequestDigest:        object.Spec.RequestDigest,
		ControllerEpochName:  object.Status.ControllerEpochName,
		ControllerEpoch:      object.Status.ControllerEpoch,
		LastOperationID:      object.Status.LastOperationID,
		LastOperationDigest:  object.Status.LastOperationDigest,
		Version:              object.Status.Version,
		CreatedAt:            timeValue(object.Status.CreatedAt),
		UpdatedAt:            timeValue(object.Status.UpdatedAt),
	}
	if object.Status.LastVerified != nil {
		result.LastVerified = remoteRefFromAPI(*object.Status.LastVerified)
	}
	return result
}

func remoteRefToAPI(value store.RemoteRefState) corev1alpha1.ControlRemoteRefState {
	return corev1alpha1.ControlRemoteRefState{Absent: value.Absent, SHA: value.SHA}
}

func remoteRefFromAPI(value corev1alpha1.ControlRemoteRefState) store.RemoteRefState {
	return store.RemoteRefState{Absent: value.Absent, SHA: value.SHA}
}

func isKnownBranchClaimAvailability(value store.BranchClaimAvailability) bool {
	return value == store.BranchClaimAvailable || value == store.BranchClaimReconciliationBlocked
}
