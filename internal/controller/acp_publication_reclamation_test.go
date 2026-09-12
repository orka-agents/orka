package controller

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/publisher"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const targetReadCredentialName = "target-read"

func TestReclaimStandaloneTaskBranchClaimAdvancesThenDeletes(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-old", "prompt-old")
	attemptID := createTerminalBranchClaimReclamationAttempt(t, controlStore, fence, task, store.PromptDeliveryVerifiedExact)
	publication, claim := createStandaloneBranchClaimPublication(t, controlStore, fence, task, store.PublicationVerifiedExact)
	dispatcher := &ACPDispatcher{Store: controlStore}

	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, attemptID, fence, publication); err != nil {
		t.Fatalf("reclaimStandaloneTaskBranchClaim: %v", err)
	}
	if _, err := controlStore.GetBranchClaim(ctx, claim.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBranchClaim after reclaim error = %v, want ErrNotFound", err)
	}
	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, attemptID, fence, publication); err != nil {
		t.Fatalf("reclaimStandaloneTaskBranchClaim(idempotent retry): %v", err)
	}

	replacementDigest, err := branchClaimRequestDigest(
		claim.RepositoryID, claim.Ref, store.BranchClaimOwnerTask, claim.OwnerUID,
		publication.VerificationReceipt.ObservedRemote, "pub-replacement-attempt",
	)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := controlStore.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: claim.RepositoryID, Ref: claim.Ref,
		OwnerKind: store.BranchClaimOwnerTask, OwnerUID: claim.OwnerUID, LastVerified: publication.VerificationReceipt.ObservedRemote,
		RequestDigest: replacementDigest, CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(replacement): %v", err)
	}
	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, attemptID, fence, publication); err != nil {
		t.Fatalf("reclaimStandaloneTaskBranchClaim(old retry after replacement): %v", err)
	}
	preserved, err := controlStore.GetBranchClaim(ctx, replacement.ID)
	if err != nil {
		t.Fatalf("GetBranchClaim(replacement): %v", err)
	}
	if preserved.OwnerUID != replacement.OwnerUID || preserved.RequestDigest != replacement.RequestDigest {
		t.Fatalf("replacement claim was changed: %#v", preserved)
	}
}

func TestReclaimStandaloneTaskBranchClaimAcceptsLegacyDigest(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-legacy", "prompt-legacy")
	attemptID := createTerminalBranchClaimReclamationAttempt(t, controlStore, fence, task, store.PromptDeliveryVerifiedExact)
	publication, claim := createStandaloneBranchClaimPublication(t, controlStore, fence, task, store.PublicationVerifiedExact)
	if err := controlStore.ReclaimBranchClaim(ctx, store.ReclaimBranchClaimRequest{
		ID: claim.ID, Fence: fence, ExpectedVersion: claim.Version, ExpectedGeneration: claim.Generation,
		ExpectedRepositoryID: claim.RepositoryID, ExpectedRef: claim.Ref,
		ExpectedOwnerKind: claim.OwnerKind, ExpectedOwnerUID: claim.OwnerUID,
		ExpectedLastVerified: claim.LastVerified, ExpectedAvailability: claim.Availability,
		ExpectedRequestDigest: claim.RequestDigest,
	}); err != nil {
		t.Fatal(err)
	}
	legacyDigest, err := legacyBranchClaimRequestDigest(
		claim.RepositoryID, claim.Ref, claim.OwnerKind, claim.OwnerUID, publication.Baseline,
	)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := controlStore.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: claim.RepositoryID, Ref: claim.Ref, OwnerKind: claim.OwnerKind, OwnerUID: claim.OwnerUID,
		LastVerified: publication.Baseline, RequestDigest: legacyDigest, CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	publication.BranchClaimGeneration = legacy.Generation
	dispatcher := &ACPDispatcher{Store: controlStore}
	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, attemptID, fence, publication); err != nil {
		t.Fatalf("reclaim legacy claim: %v", err)
	}
	if _, err := controlStore.GetBranchClaim(ctx, legacy.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("legacy claim remains: %v", err)
	}
}

func TestEnsureBranchClaimAcceptsLegacyDigestOnlyForPersistedPublication(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-legacy-admission", "prompt-legacy-admission")
	target := publisher.Repository{ID: "github.com/orka/legacy-target"}
	ref := "refs/heads/legacy-admission"
	baseline := store.RemoteRefState{Absent: true}
	legacyDigest, err := legacyBranchClaimRequestDigest(target.ID, ref, store.BranchClaimOwnerTask, string(task.UID), baseline)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := controlStore.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: target.ID, Ref: ref, OwnerKind: store.BranchClaimOwnerTask, OwnerUID: string(task.UID),
		LastVerified: baseline, RequestDigest: legacyDigest, CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	incarnation := publicationIDForTask(task)
	dispatcher := &ACPDispatcher{Store: controlStore}
	if _, _, err := dispatcher.ensureBranchClaim(
		ctx, fence, target, ref, store.BranchClaimOwnerTask, string(task.UID), baseline, incarnation,
	); err == nil {
		t.Fatal("legacy claim without a persisted publication was admitted")
	}
	persisted := &store.Publication{
		ID: incarnation, TaskUID: string(task.UID), BranchClaimID: legacy.ID, BranchClaimGeneration: legacy.Generation,
		TargetRepositoryID: target.ID, TargetRef: ref, Baseline: baseline,
	}
	dispatcher.Store = &recoveredPublicationStore{DurableControlStore: controlStore, publication: persisted}
	got, created, err := dispatcher.ensureBranchClaim(
		ctx, fence, target, ref, store.BranchClaimOwnerTask, string(task.UID), baseline, incarnation,
	)
	if err != nil || created || got.ID != legacy.ID {
		t.Fatalf("persisted legacy admission = claim %#v created %t err %v", got, created, err)
	}
}

func TestReclaimStandaloneTaskBranchClaimWaitsForEffectsAndPreservesSessions(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-effects", "prompt-effects")
	attemptID := createTerminalBranchClaimReclamationAttempt(t, controlStore, fence, task, store.PromptDeliveryConflict)
	publication, claim := createStandaloneBranchClaimPublication(t, controlStore, fence, task, store.PublicationPreparationFailed)
	identity := store.ExternalEffectIdentity{
		Kind: "publisher.prepare", Namespace: task.Namespace, AggregateID: publication.ID,
		OperationID: publicationOperationID("prepare", task),
	}
	effect, err := controlStore.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
		Identity: identity, RequestDigest: testControlDigestForDispatcher("pending-effect"), Fence: fence, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("ReserveExternalEffect: %v", err)
	}
	dispatcher := &ACPDispatcher{Store: controlStore}
	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, attemptID, fence, publication); err == nil || !strings.Contains(err.Error(), "has not settled") {
		t.Fatalf("reclaim with pending effect error = %v, want unsettled error", err)
	}
	if _, err := controlStore.GetBranchClaim(ctx, claim.ID); err != nil {
		t.Fatalf("pending effect allowed claim deletion: %v", err)
	}
	if _, err := controlStore.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: effect.State,
		NewState: store.ExternalEffectFailed, RequestDigest: effect.RequestDigest, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("TransitionExternalEffect(failed): %v", err)
	}
	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, attemptID, fence, publication); err != nil {
		t.Fatalf("reclaim after effect settled: %v", err)
	}
	if _, err := controlStore.GetBranchClaim(ctx, claim.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetBranchClaim after settled reclaim error = %v, want ErrNotFound", err)
	}

	sessionTask := branchClaimReclamationTask("task-session", "prompt-session")
	sessionAttemptID := createTerminalBranchClaimReclamationAttempt(t, controlStore, fence, sessionTask, store.PromptDeliveryVerifiedExact)
	sessionPublication, sessionClaim := createStandaloneBranchClaimPublication(t, controlStore, fence, sessionTask, store.PublicationVerifiedExact)
	sessionPublication.SessionUID = "session-owner"
	if err := controlStore.ReclaimBranchClaim(ctx, store.ReclaimBranchClaimRequest{
		ID: sessionClaim.ID, Fence: fence, ExpectedVersion: sessionClaim.Version, ExpectedGeneration: sessionClaim.Generation,
		ExpectedRepositoryID: sessionClaim.RepositoryID, ExpectedRef: sessionClaim.Ref,
		ExpectedOwnerKind: store.BranchClaimOwnerTask, ExpectedOwnerUID: sessionClaim.OwnerUID,
		ExpectedLastVerified: sessionClaim.LastVerified, ExpectedAvailability: sessionClaim.Availability,
		ExpectedRequestDigest: sessionClaim.RequestDigest,
	}); err != nil {
		t.Fatalf("remove temporary Task claim before Session fixture: %v", err)
	}
	sessionDigest, err := branchClaimRequestDigest(
		sessionPublication.TargetRepositoryID, sessionPublication.TargetRef, store.BranchClaimOwnerSession,
		sessionPublication.SessionUID, sessionPublication.Baseline, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionClaim, err = controlStore.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: sessionPublication.TargetRepositoryID, Ref: sessionPublication.TargetRef,
		OwnerKind: store.BranchClaimOwnerSession, OwnerUID: sessionPublication.SessionUID,
		LastVerified: sessionPublication.Baseline, RequestDigest: sessionDigest, CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim(session): %v", err)
	}
	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, sessionTask, sessionAttemptID, fence, sessionPublication); err != nil {
		t.Fatalf("Session-owned reclamation should be a no-op: %v", err)
	}
	if _, err := controlStore.GetBranchClaim(ctx, sessionClaim.ID); err != nil {
		t.Fatalf("Session-owned claim was removed: %v", err)
	}
}

func TestReclaimStandaloneTaskBranchClaimPreservesOutcomeUnknown(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-unknown", "prompt-unknown")
	publication, claim := createStandaloneBranchClaimPublication(t, controlStore, fence, task, store.PublicationOutcomeUnknown)
	dispatcher := &ACPDispatcher{Store: controlStore}

	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, "", fence, publication); err != nil {
		t.Fatalf("reclaimStandaloneTaskBranchClaim: %v", err)
	}
	if _, err := controlStore.GetBranchClaim(ctx, claim.ID); err != nil {
		t.Fatalf("outcome-unknown claim was removed: %v", err)
	}
}

func TestUnpublishedBranchClaimCleanupReclaimsOnlyCreationWonByCaller(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	dispatcher := &ACPDispatcher{Store: controlStore}
	target := publisher.Repository{ID: "github.com/orka/session-target"}
	ref := "refs/heads/orka/session-cleanup"
	baseline := store.RemoteRefState{Absent: true}
	claim, created, err := dispatcher.ensureBranchClaim(
		ctx, fence, target, ref, store.BranchClaimOwnerSession, "session-cleanup", baseline, "pub-session-cleanup",
	)
	if err != nil || !created {
		t.Fatalf("ensureBranchClaim() = claim %#v created %t err %v", claim, created, err)
	}
	cause := errors.New("publication preflight failed")
	if got := dispatcher.withUnpublishedBranchClaimCleanup(ctx, fence, claim, claim.RequestDigest, "pub-session-cleanup", cause); !errors.Is(got, cause) {
		t.Fatalf("cleanup error = %v, want original cause", got)
	}
	if _, err := controlStore.GetBranchClaim(ctx, claim.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("new unpublished Session claim remains: %v", err)
	}

	existing, existingCreated, err := dispatcher.ensureBranchClaim(
		ctx, fence, target, ref, store.BranchClaimOwnerSession, "session-cleanup", baseline, "pub-session-retry",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !existingCreated {
		t.Fatal("recreated Session claim was not reported as a new insertion")
	}
	idempotent, idempotentCreated, err := dispatcher.ensureBranchClaim(
		ctx, fence, target, ref, store.BranchClaimOwnerSession, "session-cleanup", baseline, "pub-session-retry",
	)
	if err != nil {
		t.Fatal(err)
	}
	if idempotentCreated {
		t.Fatal("idempotent Session claim lookup incorrectly reported a new insertion")
	}
	if got := dispatcher.withUnpublishedBranchClaimCleanup(ctx, fence, idempotent, idempotent.RequestDigest, "pub-session-retry", cause); !errors.Is(got, cause) {
		t.Fatalf("existing cleanup error = %v, want original cause", got)
	}
	if _, err := controlStore.GetBranchClaim(ctx, existing.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("same-incarnation Session claim was not reclaimed on retry: %v", err)
	}

	taskClaim, taskCreated, err := dispatcher.ensureBranchClaim(
		ctx, fence, publisher.Repository{ID: "github.com/orka/task-target"}, "refs/heads/orka/task-cleanup",
		store.BranchClaimOwnerTask, "task-cleanup", baseline, "pub-task-cleanup",
	)
	if err != nil || !taskCreated {
		t.Fatalf("ensure task claim = %#v created %t err %v", taskClaim, taskCreated, err)
	}
	if got := dispatcher.withUnpublishedBranchClaimCleanup(ctx, fence, taskClaim, taskClaim.RequestDigest, "pub-task-cleanup", cause); !errors.Is(got, cause) {
		t.Fatalf("Task cleanup error = %v, want original cause", got)
	}
	if _, err := controlStore.GetBranchClaim(ctx, taskClaim.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("new unpublished Task claim remains: %v", err)
	}
}

func TestPersistedPublicationRecoveryFinishesAfterStandaloneClaimReclaimed(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-recovery", "prompt-recovery")
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		Intent:  corev1alpha1.WorkspaceIntentWrite,
		GitRepo: "https://github.com/orka/source.git", PublicationGitRepo: "https://github.com/orka/target.git",
		PublicationCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "publish-secret"},
	}
	attemptID := createTerminalBranchClaimReclamationAttempt(t, controlStore, fence, task, store.PromptDeliveryVerifiedExact)
	publication, _ := createStandaloneBranchClaimPublication(t, controlStore, fence, task, store.PublicationVerifiedExact)
	publication.SourceRepositoryID = "github.com/orka/source"
	publication.SourceRef = strings.Repeat("b", 40)
	publication.SourceBaselineSHA = publication.SourceRef
	publication.ArtifactID = "artifact-recovery"
	publication.ArtifactDigest = testControlDigestForDispatcher("artifact-recovery")
	publication.ArtifactSizeBytes = 64
	publication.ArtifactMediaType = "application/vnd.orka.workspace-delta"
	dispatcher := &ACPDispatcher{Store: controlStore}
	if err := dispatcher.reclaimStandaloneTaskBranchClaim(ctx, task, attemptID, fence, publication); err != nil {
		t.Fatalf("initial claim reclamation: %v", err)
	}

	recoveryStore := &recoveredPublicationStore{DurableControlStore: controlStore, publication: publication}
	dispatcher.Store = recoveryStore
	result, err := dispatcher.reconcilePersistedPublication(ctx, task, attemptID, fence)
	if err != nil {
		t.Fatalf("reconcilePersistedPublication after claim reclamation: %v", err)
	}
	if result.PublicationID != publication.ID || result.Status.Outcome != corev1alpha1.TaskDeliveryOutcomeVerifiedExact {
		t.Fatalf("recovered result = %#v", result)
	}
}

func TestLoadPersistedPublicationRecoveryUsesContinuationTargetSource(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-continuation-recovery", "prompt-continuation-recovery")
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		Intent:                       corev1alpha1.WorkspaceIntentWrite,
		GitRepo:                      "https://github.com/orka/upstream.git",
		ReadCredentialRef:            &corev1alpha1.WorkspaceCredentialReference{Name: "source-read"},
		PublicationGitRepo:           "https://github.com/orka/fork.git",
		PublicationReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: targetReadCredentialName},
		PublicationCredentialRef:     &corev1alpha1.WorkspaceCredentialReference{Name: "target-write"},
		ForgeCredentialRef:           &corev1alpha1.WorkspaceCredentialReference{Name: "forge-only"},
	}
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "continuation", Append: true}
	target, err := workspacePublicationRepository(task.Spec.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/orka/session-continuation"
	baseline := store.RemoteRefState{SHA: strings.Repeat("a", 40)}
	digest, err := branchClaimRequestDigest(target.ID, ref, store.BranchClaimOwnerSession, "session-continuation", baseline, "")
	if err != nil {
		t.Fatal(err)
	}
	claim, err := controlStore.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: target.ID, Ref: ref, OwnerKind: store.BranchClaimOwnerSession, OwnerUID: "session-continuation",
		LastVerified: baseline, RequestDigest: digest, CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	publication := &store.Publication{
		ID: publicationIDForTask(task), Namespace: task.Namespace, Generation: 1,
		TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID,
		SessionUID: "session-continuation", BranchClaimID: claim.ID, BranchClaimGeneration: claim.Generation,
		SourceRepositoryID: target.ID, SourceRef: baseline.SHA, SourceBaselineSHA: baseline.SHA,
		TargetRepositoryID: target.ID, TargetRef: ref, Baseline: baseline,
		PublicationCredentialRef: "target-write", State: store.PublicationPrepared,
		PRIntent: &store.PullRequestIntent{
			BaseRepositoryID: "github.com/orka/upstream", BaseRef: "refs/heads/main",
			HeadRepositoryID: target.ID, HeadRef: ref, PublicationGeneration: 1, ExpectedHeadSHA: baseline.SHA,
		},
	}
	dispatcher := &ACPDispatcher{Store: &recoveredPublicationStore{
		DurableControlStore: controlStore, publication: publication,
		sessionControl: &store.SessionControl{
			Namespace: task.Namespace, SessionName: task.Spec.SessionRef.Name, SessionUID: publication.SessionUID,
			VerifiedBaseline: &store.VerifiedBranchBaseline{RepositoryID: target.ID, Ref: ref, SHA: baseline.SHA},
		},
	}}
	_, recovery, err := dispatcher.loadPersistedPublicationRecovery(ctx, task, "attempt-unused", fence)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.source.ID != target.ID || recovery.pullRequestBase.ID != "github.com/orka/upstream" ||
		recovery.sourceCredential == nil || recovery.sourceCredential.Name != targetReadCredentialName || recovery.sourceCredential.Role != publisherservice.CredentialRoleTargetRead ||
		recovery.targetReadCredential == nil || recovery.targetReadCredential.Role != publisherservice.CredentialRoleTargetRead ||
		recovery.writeCredential == nil || recovery.writeCredential.Role != publisherservice.CredentialRoleTargetWrite ||
		recovery.forgeCredential == nil || recovery.forgeCredential.Name != "forge-only" ||
		recovery.forgeCredential.Kind != publisherservice.CredentialForgeToken || recovery.forgeCredential.Role != publisherservice.CredentialRoleForge {
		t.Fatalf("continuation recovery = source %#v base %#v source credential %#v forge credential %#v", recovery.source, recovery.pullRequestBase, recovery.sourceCredential, recovery.forgeCredential)
	}
}

func TestLoadPersistedPublicationRecoveryUsesTargetReadForSameRepositoryContinuation(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-same-repo-continuation", "prompt-same-repo-continuation")
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "same-repo", Append: true}
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/orka/repo.git",
		ReadCredentialRef:            &corev1alpha1.WorkspaceCredentialReference{Name: "source-read"},
		PublicationGitRepo:           "https://github.com/orka/repo.git",
		PublicationReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: targetReadCredentialName},
		PublicationCredentialRef:     &corev1alpha1.WorkspaceCredentialReference{Name: "target-write"},
	}
	repository, err := workspacePublicationRepository(task.Spec.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/orka/same-repo"
	baseline := store.RemoteRefState{SHA: strings.Repeat("a", 40)}
	digest, err := branchClaimRequestDigest(repository.ID, ref, store.BranchClaimOwnerSession, "session-same-repo", baseline, publicationIDForTask(task))
	if err != nil {
		t.Fatal(err)
	}
	claim, err := controlStore.CreateBranchClaim(ctx, &store.BranchClaim{
		RepositoryID: repository.ID, Ref: ref, OwnerKind: store.BranchClaimOwnerSession, OwnerUID: "session-same-repo",
		LastVerified: baseline, RequestDigest: digest, CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	publication := &store.Publication{
		ID: publicationIDForTask(task), Namespace: task.Namespace, Generation: 1,
		TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID, SessionUID: "session-same-repo",
		BranchClaimID: claim.ID, BranchClaimGeneration: claim.Generation,
		SourceRepositoryID: repository.ID, SourceRef: baseline.SHA, SourceBaselineSHA: baseline.SHA,
		TargetRepositoryID: repository.ID, TargetRef: ref, Baseline: baseline,
		PublicationCredentialRef: "target-write", State: store.PublicationPrepared,
	}
	dispatcher := &ACPDispatcher{Store: &recoveredPublicationStore{
		DurableControlStore: controlStore, publication: publication,
		sessionControl: &store.SessionControl{
			Namespace: task.Namespace, SessionName: task.Spec.SessionRef.Name, SessionUID: publication.SessionUID,
			VerifiedBaseline: &store.VerifiedBranchBaseline{RepositoryID: repository.ID, Ref: ref, SHA: baseline.SHA},
		},
	}}
	_, recovery, err := dispatcher.loadPersistedPublicationRecovery(ctx, task, "attempt-unused", fence)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.sourceCredential == nil || recovery.sourceCredential.Name != targetReadCredentialName ||
		recovery.sourceCredential.Role != publisherservice.CredentialRoleTargetRead {
		t.Fatalf("same-repository continuation source credential = %#v", recovery.sourceCredential)
	}
}

func TestLoadPersistedPublicationRecoveryRejectsMissingOutcomeUnknownClaim(t *testing.T) {
	ctx := context.Background()
	controlStore, fence := newBranchClaimReclamationStore(t)
	task := branchClaimReclamationTask("task-missing-unknown", "prompt-missing-unknown")
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/orka/source.git",
		PublicationGitRepo:       "https://github.com/orka/target.git",
		PublicationCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "target-write"},
	}
	publication := &store.Publication{
		ID: publicationIDForTask(task), Namespace: task.Namespace, Generation: 1,
		TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID,
		BranchClaimID: "missing-claim", BranchClaimGeneration: 1,
		SourceRepositoryID: "github.com/orka/source", TargetRepositoryID: "github.com/orka/target",
		State: store.PublicationOutcomeUnknown,
	}
	dispatcher := &ACPDispatcher{Store: &recoveredPublicationStore{DurableControlStore: controlStore, publication: publication}}
	if _, _, err := dispatcher.loadPersistedPublicationRecovery(ctx, task, "attempt-unused", fence); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing outcome-unknown claim error = %v, want ErrNotFound", err)
	}
}

func newBranchClaimReclamationStore(t *testing.T) (*sqlite.Store, store.ControllerEpochFence) {
	t.Helper()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	controlStore := sqlite.NewStore(db, "test")
	epoch, err := controlStore.CompareAndSwapControllerEpoch(context.Background(), store.ControllerEpochCAS{
		ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1, HolderID: "controller-test",
		RequestDigest: testControlDigestForDispatcher("branch-reclaim-epoch"), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return controlStore, store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
}

func branchClaimReclamationTask(uid, promptID string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: uid, UID: types.UID(uid)},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			Attempt: 1, PromptID: promptID, RequestDigest: testControlDigestForDispatcher("prompt-" + uid),
		}},
	}
}

func createTerminalBranchClaimReclamationAttempt(
	t *testing.T,
	controlStore *sqlite.Store,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	terminalDelivery store.PromptDeliveryState,
) string {
	t.Helper()
	key := store.PromptAttemptKey{
		Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: int64(task.Status.Execution.Attempt), PromptID: task.Status.Execution.PromptID,
	}
	attemptID, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := controlStore.CreatePromptAttempt(context.Background(), boundPromptAttemptForTest(&store.PromptAttempt{
		ID: attemptID, Key: key, RequestDigest: task.Status.Execution.RequestDigest,
	}), fence)
	if err != nil {
		t.Fatal(err)
	}
	attempt = completeACPAttemptExecutionForTest(t, controlStore, fence, attempt, false)
	path := []store.PromptDeliveryState{store.PromptDeliveryValidating}
	switch terminalDelivery {
	case store.PromptDeliveryVerifiedExact:
		path = append(path, store.PromptDeliveryPreparing, store.PromptDeliveryPrepared, store.PromptDeliveryPublishing, store.PromptDeliveryVerifying, terminalDelivery)
	case store.PromptDeliveryConflict:
		path = append(path, store.PromptDeliveryPreparing, terminalDelivery)
	default:
		t.Fatalf("unsupported terminal delivery fixture %s", terminalDelivery)
	}
	for _, next := range path {
		updated, transitionErr := controlStore.TransitionPromptAttemptDelivery(context.Background(), store.PromptAttemptDeliveryTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState, NewState: next,
			OperationID:     "delivery-" + strings.ToLower(string(next)) + "-" + string(task.UID),
			OperationDigest: testControlDigestForDispatcher("delivery-" + string(next) + "-" + string(task.UID)), UpdatedAt: time.Now().UTC(),
		})
		if transitionErr != nil {
			t.Fatalf("transition delivery to %s: %v", next, transitionErr)
		}
		attempt = updated
	}
	return attemptID
}

func createStandaloneBranchClaimPublication(
	t *testing.T,
	controlStore store.BranchClaimStore,
	fence store.ControllerEpochFence,
	task *corev1alpha1.Task,
	state store.PublicationState,
) (*store.Publication, *store.BranchClaim) {
	t.Helper()
	targetRepositoryID := "github.com/orka/target"
	targetRef := "refs/heads/shared-" + string(task.UID)
	baseline := store.RemoteRefState{Absent: true}
	publicationID := publicationIDForTask(task)
	requestDigest, err := branchClaimRequestDigest(
		targetRepositoryID, targetRef, store.BranchClaimOwnerTask, string(task.UID), baseline, publicationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := controlStore.CreateBranchClaim(context.Background(), &store.BranchClaim{
		RepositoryID: targetRepositoryID, Ref: targetRef, OwnerKind: store.BranchClaimOwnerTask, OwnerUID: string(task.UID),
		LastVerified: baseline, RequestDigest: requestDigest, CreatedAt: time.Now().UTC(),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	publication := &store.Publication{
		ID: publicationID, Namespace: task.Namespace, Generation: 1,
		TaskUID: string(task.UID), Attempt: int64(task.Status.Execution.Attempt), PromptID: task.Status.Execution.PromptID,
		BranchClaimID: claim.ID, BranchClaimGeneration: claim.Generation,
		TargetRepositoryID: targetRepositoryID, TargetRef: targetRef, Baseline: baseline, State: state,
	}
	if state == store.PublicationVerifiedExact {
		publication.VerificationReceipt = &store.PublicationVerificationReceipt{
			Outcome: state, ObservedRemote: store.RemoteRefState{SHA: strings.Repeat("a", 40)},
		}
	}
	return publication, claim
}

type persistPublicationThenErrorStore struct {
	store.DurableControlStore
	failed bool
}

func (s *persistPublicationThenErrorStore) CreatePublication(
	ctx context.Context,
	publication *store.Publication,
	fence store.ControllerEpochFence,
) (*store.Publication, error) {
	created, err := s.DurableControlStore.CreatePublication(ctx, publication, fence)
	if err != nil || s.failed {
		return created, err
	}
	s.failed = true
	return nil, errors.New("simulated lost Publication create acknowledgement")
}
