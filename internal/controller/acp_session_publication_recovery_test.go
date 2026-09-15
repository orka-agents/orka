package controller

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskterminal"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRecoveredPublicationSessionFinalizationUsesDurableDelivery(t *testing.T) {
	ctx := context.Background()
	controls, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "recovered-publication.db"))
	defer closeStore()
	continuity := newACPSessionTestContinuity(t, controls, ACPBootstrapLimits{})
	control := ensureACPSessionForTest(t, continuity, fence, "recovered-publication")
	turn, attempt := openACPSessionTurnForTest(t, continuity, controls, fence, control,
		"recovered-publication-uid", "recovered-publication-prompt", "publish one change")
	for _, state := range []store.PromptExecutionState{
		store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned,
		store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning,
		store.PromptExecutionSettling, store.PromptExecutionSucceeded,
	} {
		transition := store.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: state, OperationID: "recovery-" + string(state), OperationDigest: acpSessionTestDigest(string(state)),
			UpdatedAt: attempt.UpdatedAt.Add(time.Second),
		}
		if state == store.PromptExecutionSessionStarting {
			transition.RuntimeInstanceID = "retired-runtime"
		}
		var err error
		attempt, err = controls.TransitionPromptAttemptExecution(ctx, transition)
		if err != nil {
			t.Fatal(err)
		}
	}
	attempt = completeACPAttemptDeliveryForTest(t, controls, fence, attempt, store.PromptDeliveryVerifiedExact)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "recovered-publication", UID: types.UID(attempt.Key.TaskUID)},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "publish one change",
			SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName},
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/example/repo.git",
				PushBranch: "recovered-publication",
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: attempt.Key.PromptID, RequestDigest: attempt.RequestDigest,
				RuntimeInstanceID: "retired-runtime", RuntimeSessionUID: control.SessionUID,
				RuntimeSessionGeneration: turn.Lease.Key.LeaseGeneration, ControllerEpoch: fence.Epoch,
				RuntimeSessionSupervisorBootID: "retired-boot", RuntimeSessionProfileDigest: acpSessionTestDigest("profile"),
			},
			Delivery: &corev1alpha1.TaskDeliveryStatus{
				State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
			},
		},
	}
	cleanupDigest, err := taskScopedRuntimeSessionCleanupDigest(task.UID, 1, task.Status.Execution.RuntimeInstanceID,
		task.Status.Execution.RuntimeSessionUID, task.Status.Execution.RuntimeSessionGeneration)
	if err != nil {
		t.Fatal(err)
	}
	task.Status.Execution.RuntimeSessionCleanupDigest = cleanupDigest
	now := time.Now().UTC()
	claim, err := controls.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: "github.com/example/repo", Ref: "refs/heads/recovered-publication",
		OwnerKind: store.BranchClaimOwnerSession, OwnerUID: control.SessionUID, Generation: 1,
		LastVerified: store.RemoteRefState{Absent: true}, RequestDigest: acpSessionTestDigest("recovery-claim"), CreatedAt: now,
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := controls.CreatePublication(ctx, &store.Publication{
		ID: publicationIDForTask(task), Namespace: task.Namespace, Generation: 1,
		TaskUID: string(task.UID), Attempt: 1, PromptID: attempt.Key.PromptID, SessionUID: control.SessionUID,
		BranchClaimID: claim.ID, BranchClaimGeneration: claim.Generation,
		SourceRepositoryID: claim.RepositoryID, SourceRef: "refs/heads/main", SourceBaselineSHA: acpSessionTestSHA,
		TargetRepositoryID: claim.RepositoryID, TargetRef: claim.Ref, Baseline: claim.LastVerified,
		ArtifactID: "delta-artifact", ArtifactDigest: acpSessionTestDigest("recovery-delta"), ArtifactSizeBytes: 128,
		ArtifactMediaType: "application/vnd.orka.workspace-delta.v1+tar", PublicationCredentialRef: "publisher-reference",
		CommitIdentity: "Orka <orka@example.invalid>", CommitMessage: "test: recover publication", CommitTimestamp: now,
		RequestDigest: acpSessionTestDigest("recovery-publication"), CreatedAt: now,
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	prepared := &store.PreparedPublicationReceipt{
		OperationID: "prepare", RequestDigest: acpSessionTestDigest("recovery-prepare"),
		TreeSHA: acpSessionTestSHA2, CommitSHA: acpSessionTestSHA, ManifestDigest: acpSessionTestDigest("manifest"),
		BundleArtifactID: "bundle-artifact", BundleDigest: acpSessionTestDigest("bundle"), BundleSizeBytes: 256,
		BundleMediaType: store.PreparedBundleMediaType, BundleRef: "refs/orka/publications/" + strings.Repeat("f", 64),
		PreparedAt: now,
	}
	publication = transitionACPPublicationForTest(t, controls, fence, publication, store.PublicationPrepared, "prepare", now, prepared, nil, nil, "")
	publication = transitionACPPublicationForTest(t, controls, fence, publication, store.PublicationPublishing, "publishing", now, nil, nil, nil, "")
	published := &store.PublishOperationReceipt{
		OperationID: "publish", RequestDigest: acpSessionTestDigest("publish"), TargetRepositoryID: claim.RepositoryID,
		TargetRef: claim.Ref, RemoteBefore: claim.LastVerified, ExpectedCommitSHA: prepared.CommitSHA, PublishedAt: now,
	}
	publication = transitionACPPublicationForTest(t, controls, fence, publication, store.PublicationVerifying, "publish", now, nil, published, nil, "")
	verified := &store.PublicationVerificationReceipt{
		OperationID: "verify", RequestDigest: acpSessionTestDigest("verify"), Outcome: store.PublicationVerifiedExact,
		ExpectedCommitSHA: prepared.CommitSHA, ObservedRemote: store.RemoteRefState{SHA: prepared.CommitSHA}, VerifiedAt: now,
	}
	publication = transitionACPPublicationForTest(t, controls, fence, publication, store.PublicationVerifiedExact, "verify", now, nil, nil, verified, "")
	if err := controls.SaveResult(ctx, task.Namespace, task.Name, []byte("published one change")); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	dispatcher := &ACPDispatcher{Client: client, Store: controls, ResultStore: controls, Sessions: continuity}
	baseline := harnessv2.WorkspaceBaseline{RepositoryIdentity: publication.SourceRepositoryID, Revision: publication.SourceBaselineSHA}
	delta := harnessv2.WorkspaceDeltaDescriptor{Artifact: &harnessv2.ArtifactReference{Digest: publication.ArtifactDigest}}
	wantDelivery := publicationTaskDeliveryStatus(task.Spec.Workspace, baseline, delta, publication, task.Spec.Workspace.PushBranch)
	// Recovery writes the new Task status without refreshing this caller's copy.
	if err := dispatcher.patchDeliveryStatus(ctx, task, wantDelivery); err != nil {
		t.Fatal(err)
	}
	if task.Status.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested {
		t.Fatal("fixture did not retain the stale in-memory Task delivery")
	}
	if err := dispatcher.finalizeRecoveredTerminalSession(ctx, task, attempt, fence); err != nil {
		t.Fatal(err)
	}
	finalized, err := controls.GetSessionTurn(ctx, turn.Turn.ID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := controls.GetOutboxProjection(ctx, finalized.ProjectionID)
	if err != nil {
		t.Fatal(err)
	}
	var payload taskTerminalProjection
	if err := json.Unmarshal(projection.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Delivery == nil || payload.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeVerifiedExact ||
		payload.Delivery.PublicationID != publication.ID || payload.Delivery.ExpectedCommitSHA != prepared.CommitSHA ||
		payload.Delivery.VerifiedRemoteSHA != verified.ObservedRemote.SHA {
		t.Fatalf("recovered Session projection persisted stale delivery: %#v", payload.Delivery)
	}
	wantDelivery.LastTransitionTime = payload.Delivery.LastTransitionTime
	if !reflect.DeepEqual(*payload.Delivery, wantDelivery) {
		t.Fatalf("recovered delivery lost publication evidence: got %#v, want %#v", payload.Delivery, wantDelivery)
	}
	current := task.DeepCopy()
	current.Status.Delivery = &wantDelivery
	if _, err := taskterminal.ValidateFinalizedSessionProjection(projection.Payload, current, string(task.UID), attempt, finalized); err != nil {
		t.Fatalf("recovered projection cannot pass Session reclamation: %v", err)
	}
	// A receipt produced by the controller's canonical publication ID must also
	// authorize cleanup of the exact historical stale-delivery shape.
	legacy := payload
	legacy.Delivery = task.Status.Delivery.DeepCopy()
	legacyPayload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyTurn := *finalized
	legacyTurn.ProjectionDigest = store.CanonicalBytesDigest(legacyPayload)
	if _, err := taskterminal.ValidateSessionCleanupProjection(legacyPayload, current, string(task.UID), attempt, &legacyTurn); err != nil {
		t.Fatalf("cleanup rejected the controller-produced publication identity or receipt: %v", err)
	}
}

func TestRecoveredSessionFinalizationRejectsMismatchedDeliveryFallback(t *testing.T) {
	for _, test := range []struct {
		name     string
		terminal store.PromptDeliveryState
		delivery corev1alpha1.TaskDeliveryStatus
	}{
		{name: "missing publication with stale NotRequested", terminal: store.PromptDeliveryVerifiedExact,
			delivery: corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested}},
		{name: "matching state with stale outcome", terminal: store.PromptDeliveryReadValidated,
			delivery: corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateReadValidated, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			controls, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "fallback.db"))
			defer closeStore()
			continuity := newACPSessionTestContinuity(t, controls, ACPBootstrapLimits{})
			control := ensureACPSessionForTest(t, continuity, fence, "fallback")
			turn, attempt := openACPSessionTurnForTest(t, continuity, controls, fence, control,
				"fallback-task-uid", "fallback-prompt", "finish this turn")
			attempt = completeACPAttemptExecutionForTest(t, controls, fence, attempt, false)
			if test.terminal == store.PromptDeliveryVerifiedExact {
				attempt = completeACPAttemptDeliveryForTest(t, controls, fence, attempt, test.terminal)
			} else {
				for _, state := range []store.PromptDeliveryState{store.PromptDeliveryValidating, test.terminal} {
					var err error
					attempt, err = controls.TransitionPromptAttemptDelivery(ctx, store.PromptAttemptDeliveryTransition{
						ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
						NewState: state, OperationID: "fallback-" + string(state), OperationDigest: acpSessionTestDigest(string(state)), UpdatedAt: time.Now().UTC(),
					})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: control.Namespace, Name: "fallback", UID: types.UID(attempt.Key.TaskUID)},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "finish this turn", SessionRef: &corev1alpha1.SessionReference{Name: control.SessionName}},
				Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
					State: corev1alpha1.TaskExecutionStateSucceeded, Attempt: 1, PromptID: attempt.Key.PromptID,
					RuntimeSessionUID: control.SessionUID, RuntimeSessionGeneration: turn.Lease.Key.LeaseGeneration,
					RequestDigest: attempt.RequestDigest,
				}, Delivery: test.delivery.DeepCopy()},
			}
			if err := controls.SaveResult(ctx, task.Namespace, task.Name, []byte("completed")); err != nil {
				t.Fatal(err)
			}
			dispatcher := &ACPDispatcher{Store: controls, ResultStore: controls, Sessions: continuity}
			err := dispatcher.finalizeRecoveredTerminalSession(ctx, task, attempt, fence)
			if !errors.Is(err, store.ErrConflict) || !strings.Contains(err.Error(), "delivery does not match the authoritative terminal attempt") {
				t.Fatalf("mismatched delivery fallback was not rejected: %v", err)
			}
			current, err := controls.GetSessionTurn(ctx, turn.Turn.ID)
			if err != nil || current.State != store.SessionTurnOpen || current.ProjectionID != "" {
				t.Fatalf("rejected finalization changed the immutable turn: %#v, %v", current, err)
			}
		})
	}
}
