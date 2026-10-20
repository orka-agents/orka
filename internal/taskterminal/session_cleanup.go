package taskterminal

import (
	"crypto/sha256"
	"fmt"
	"reflect"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// ValidateSessionCleanupProjection normally applies the strict finalized-turn
// contract. Older publication recovery could instead pin the initial empty
// NotRequested delivery after the Publication had become VerifiedExact. Only
// cleanup may recover that classification from the same turn's immutable
// publication receipt. The original payload is never changed or re-enqueued;
// the returned delivery classification is not a replacement Task status.
func ValidateSessionCleanupProjection(
	payload []byte,
	task *corev1alpha1.Task,
	sourceTaskUID string,
	attempt *store.PromptAttempt,
	turn *store.SessionTurn,
) (*Projection, error) {
	validated, strictErr := ValidateFinalizedSessionProjection(payload, task, sourceTaskUID, attempt, turn)
	if strictErr == nil {
		return validated, nil
	}
	projection, err := decodeProjection(payload)
	if err != nil || !emptyNotRequestedDelivery(projection.Delivery) {
		return nil, strictErr
	}
	if task == nil || task.Status.Execution == nil || string(task.UID) != sourceTaskUID ||
		task.Spec.Workspace == nil || task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentWrite ||
		attempt == nil || attempt.ExecutionState != store.PromptExecutionSucceeded ||
		attempt.DeliveryState != store.PromptDeliveryVerifiedExact {
		return nil, strictErr
	}
	if err := validateFinalizedSessionTurn(payload, task, sourceTaskUID, attempt, turn); err != nil {
		return nil, err
	}
	// Do not combine this compatibility case with omitted execution identity
	// or restored Task incarnations. Runtime deletion still needs every fence.
	execution := projection.Execution
	if execution.RuntimeInstanceID == "" || execution.RuntimeInstanceID != attempt.RuntimeInstanceID ||
		execution.ControllerEpoch < 1 || execution.RuntimeSessionUID == "" ||
		execution.RuntimeSessionGeneration < 1 || execution.RuntimeSessionSupervisorBootID == "" ||
		execution.RuntimeSessionProfileDigest == "" {
		return nil, strictErr
	}
	if task.Spec.Workspace.CreatePR && turn.PublicationReceipt != nil && turn.PublicationReceipt.PullRequest == nil {
		return nil, conflict("Session cleanup lacks the requested pull request receipt")
	}
	if err := validateCleanupPublicationReceipt(turn, attempt); err != nil {
		return nil, err
	}
	// The placeholder carries no publication evidence to reconcile with mutable
	// Task status. Validate all remaining payload/attempt/runtime fields exactly,
	// taking only the terminal classification from the immutable receipt above.
	projection.Delivery = projection.Delivery.DeepCopy()
	projection.Delivery.State = corev1alpha1.TaskDeliveryStateVerifiedExact
	projection.Delivery.Outcome = corev1alpha1.TaskDeliveryOutcomeVerifiedExact
	comparison := task.DeepCopy()
	comparison.Status.Delivery = projection.Delivery.DeepCopy()
	return validateProjection(projection, comparison, sourceTaskUID, attempt)
}

func emptyNotRequestedDelivery(delivery *corev1alpha1.TaskDeliveryStatus) bool {
	if delivery == nil || delivery.State != corev1alpha1.TaskDeliveryStateNotRequested ||
		delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeNotRequested {
		return false
	}
	remaining := *delivery.DeepCopy()
	remaining.State, remaining.Outcome, remaining.LastTransitionTime = "", "", nil
	return reflect.DeepEqual(remaining, corev1alpha1.TaskDeliveryStatus{})
}

func validateCleanupPublicationReceipt(turn *store.SessionTurn, attempt *store.PromptAttempt) error {
	receipt := turn.PublicationReceipt
	publicationID, err := cleanupPublicationID(attempt.Key)
	if err != nil {
		return err
	}
	if receipt == nil || turn.PublicationID != publicationID || receipt.PublicationID != publicationID ||
		receipt.Generation < 1 || receipt.State != store.PublicationVerifiedExact ||
		receipt.Prepared == nil || receipt.Publish == nil || receipt.Verification == nil {
		return conflict("Session cleanup lacks the exact verified publication receipt")
	}
	if err := store.ValidatePreparedReceipt(*receipt.Prepared); err != nil {
		return conflict("Session cleanup prepared receipt is invalid: %v", err)
	}
	publication := store.Publication{
		ID: publicationID, Generation: receipt.Generation,
		TargetRepositoryID: receipt.Publish.TargetRepositoryID, TargetRef: receipt.Publish.TargetRef,
		Baseline: receipt.Publish.RemoteBefore, PreparedReceipt: receipt.Prepared, PublishReceipt: receipt.Publish,
	}
	if err := store.ValidateVerifiedBaseline(store.VerifiedBranchBaseline{
		RepositoryID: publication.TargetRepositoryID, Ref: publication.TargetRef, SHA: receipt.Prepared.CommitSHA,
	}); err != nil {
		return conflict("Session cleanup publication target is invalid: %v", err)
	}
	for _, operation := range []struct{ id, digest string }{
		{receipt.Publish.OperationID, receipt.Publish.RequestDigest},
		{receipt.Verification.OperationID, receipt.Verification.RequestDigest},
	} {
		if err := store.ValidateControlIdentifier("publication operation ID", operation.id); err != nil {
			return conflict("Session cleanup publication operation is invalid: %v", err)
		}
		if err := store.ValidateCanonicalDigest("publication operation digest", operation.digest); err != nil {
			return conflict("Session cleanup publication operation is invalid: %v", err)
		}
	}
	if err := store.ValidatePublishReceipt(publication, store.PublicationTransition{
		OperationID: receipt.Publish.OperationID, OperationDigest: receipt.Publish.RequestDigest,
	}, *receipt.Publish); err != nil {
		return conflict("Session cleanup publish receipt is invalid: %v", err)
	}
	if err := store.ValidateVerificationReceipt(publication, store.PublicationTransition{
		OperationID: receipt.Verification.OperationID, OperationDigest: receipt.Verification.RequestDigest,
		NewState: store.PublicationVerifiedExact,
	}, *receipt.Verification); err != nil {
		return conflict("Session cleanup verification receipt is invalid: %v", err)
	}
	if receipt.PullRequest != nil {
		if err := store.ValidatePullRequestReceipt(*receipt.PullRequest); err != nil {
			return conflict("Session cleanup pull request receipt is invalid: %v", err)
		}
		if receipt.PullRequest.HeadSHA != receipt.Prepared.CommitSHA {
			return conflict("Session cleanup pull request receipt does not match the verified commit")
		}
	}
	return nil
}

func cleanupPublicationID(key store.PromptAttemptKey) (string, error) {
	canonical, err := harnessv2.CanonicalValue(map[string]any{
		"taskUID": key.TaskUID, "attempt": key.Attempt, "promptID": key.PromptID,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("orka.acp.publication-id\x00"), canonical...))
	return fmt.Sprintf("pub-%x", digest[:24]), nil
}
