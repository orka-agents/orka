package taskterminal

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/types"
)

func TestSessionCleanupAcceptsPinnedStalePublicationDelivery(t *testing.T) {
	task, attempt, projection, turn := cleanupPublicationProjectionFixture(t)
	payload := marshalProjection(t, projection)
	original := append([]byte(nil), payload...)
	before, err := json.Marshal([]any{task, attempt, turn})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateFinalizedSessionProjection(payload, task, string(task.UID), attempt, turn); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("general finalized validation accepted stale publication delivery: %v", err)
	}
	if _, err := ValidateRestoredProjection(payload, task, string(task.UID), attempt); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("general restore validation accepted stale publication delivery: %v", err)
	}
	validated, err := ValidateSessionCleanupProjection(payload, task, string(task.UID), attempt, turn)
	if err != nil {
		t.Fatalf("cleanup rejected the exact immutable publication proof: %v", err)
	}
	if validated.Delivery.State != corev1alpha1.TaskDeliveryStateVerifiedExact || validated.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeVerifiedExact {
		t.Fatalf("cleanup did not recover the receipt's terminal classification: %#v", validated.Delivery)
	}
	after, err := json.Marshal([]any{task, attempt, turn})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, original) || !bytes.Equal(before, after) || projection.Delivery.State != corev1alpha1.TaskDeliveryStateNotRequested {
		t.Fatal("cleanup compatibility changed the original payload or authoritative evidence")
	}
}

//nolint:gocyclo // Keep the complete cleanup compatibility rejection matrix together.
func TestSessionCleanupRejectsUnprovenStalePublicationDelivery(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.Task, *store.PromptAttempt, *Projection, *store.SessionTurn)
	}{
		{name: "missing publication receipt", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt = nil
		}},
		{name: "wrong turn publication", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationID = "another-publication"
		}},
		{name: "wrong receipt publication", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.PublicationID = "another-publication"
		}},
		{name: "matching noncanonical publication", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationID, s.PublicationReceipt.PublicationID = "another-publication", "another-publication"
		}},
		{name: "missing publication generation", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Generation = 0
		}},
		{name: "unverified publication", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.State = store.PublicationVerifying
		}},
		{name: "missing prepare receipt", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Prepared = nil
		}},
		{name: "invalid prepared bundle", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Prepared.BundleDigest = "invalid"
		}},
		{name: "missing publish receipt", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Publish = nil
		}},
		{name: "publish commit mismatch", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Publish.ExpectedCommitSHA = strings.Repeat("a", 40)
		}},
		{name: "invalid publish digest", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Publish.RequestDigest = "invalid"
		}},
		{name: "missing verification receipt", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Verification = nil
		}},
		{name: "verification commit mismatch", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Verification.ExpectedCommitSHA = strings.Repeat("a", 40)
		}},
		{name: "verification remote mismatch", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Verification.ObservedRemote.SHA = strings.Repeat("a", 40)
		}},
		{name: "unobserved remote", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Verification.ObservedRemote = store.RemoteRefState{Absent: true}
		}},
		{name: "wrong verification outcome", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.Verification.Outcome = store.PublicationOutcomeUnknown
		}},
		{name: "missing requested PR receipt", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.PullRequest = nil
		}},
		{name: "wrong PR head", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, s *store.SessionTurn) {
			s.PublicationReceipt.PullRequest.HeadSHA = strings.Repeat("a", 40)
		}},
		{name: "non-placeholder delivery", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, p *Projection, _ *store.SessionTurn) {
			p.Delivery.ArtifactDigest = store.CanonicalBytesDigest([]byte("artifact"))
		}},
		{name: "partial delivery outcome", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, p *Projection, _ *store.SessionTurn) {
			p.Delivery.Outcome = ""
		}},
		{name: "different delivery state", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, p *Projection, _ *store.SessionTurn) {
			p.Delivery.State = corev1alpha1.TaskDeliveryStatePreparing
		}},
		{name: "different attempt delivery", mutate: func(_ *corev1alpha1.Task, a *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			a.DeliveryState = store.PromptDeliveryNoChange
		}},
		{name: "different execution outcome", mutate: func(_ *corev1alpha1.Task, a *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			a.ExecutionState = store.PromptExecutionFailed
		}},
		{name: "different prompt", mutate: func(_ *corev1alpha1.Task, a *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			a.Key.PromptID = "another-prompt"
		}},
		{name: "different mutation lease", mutate: func(_ *corev1alpha1.Task, a *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			a.SessionLeaseGeneration++
		}},
		{name: "different attempt runtime", mutate: func(_ *corev1alpha1.Task, a *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			a.RuntimeInstanceID = "another-runtime"
		}},
		{name: "omitted attempt runtime", mutate: func(_ *corev1alpha1.Task, a *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			a.RuntimeInstanceID = ""
		}},
		{name: "different Task runtime", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			task.Status.Execution.RuntimeInstanceID = "another-runtime"
		}},
		{name: "different supervisor boot", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			task.Status.Execution.RuntimeSessionSupervisorBootID = "another-boot"
		}},
		{name: "different runtime generation", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			task.Status.Execution.RuntimeSessionGeneration++
		}},
		{name: "different runtime profile", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			task.Status.Execution.RuntimeSessionProfileDigest = store.CanonicalBytesDigest([]byte("another-profile"))
		}},
		{name: "omitted runtime fence", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, p *Projection, _ *store.SessionTurn) {
			p.Execution.RuntimeSessionSupervisorBootID = ""
		}},
		{name: "omitted runtime epoch", mutate: func(_ *corev1alpha1.Task, _ *store.PromptAttempt, p *Projection, _ *store.SessionTurn) {
			p.Execution.ControllerEpoch = 0
		}},
		{name: "restored Task incarnation", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			task.UID = "restored-task-uid"
		}},
		{name: "non-write workspace", mutate: func(task *corev1alpha1.Task, _ *store.PromptAttempt, _ *Projection, _ *store.SessionTurn) {
			task.Spec.Workspace.Intent = corev1alpha1.WorkspaceIntentRead
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			task, attempt, projection, turn := cleanupPublicationProjectionFixture(t)
			sourceTaskUID := string(task.UID)
			test.mutate(task, attempt, &projection, turn)
			payload := marshalProjection(t, projection)
			turn.ProjectionDigest = store.CanonicalBytesDigest(payload)
			if _, err := ValidateSessionCleanupProjection(payload, task, sourceTaskUID, attempt, turn); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("unproven cleanup compatibility was accepted: %v", err)
			}
		})
	}
	for _, corrupt := range []string{"payload digest", "prompt attempt", "Session UID"} {
		t.Run(corrupt, func(t *testing.T) {
			task, attempt, projection, turn := cleanupPublicationProjectionFixture(t)
			switch corrupt {
			case "payload digest":
				turn.ProjectionDigest = store.CanonicalBytesDigest([]byte("different-payload"))
			case "prompt attempt":
				turn.PromptAttemptID = "another-attempt"
			case "Session UID":
				turn.Key.SessionUID = "another-session"
			}
			if _, err := ValidateSessionCleanupProjection(marshalProjection(t, projection), task, string(task.UID), attempt, turn); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("changed turn fence was accepted: %v", err)
			}
		})
	}
}

func cleanupPublicationProjectionFixture(t *testing.T) (*corev1alpha1.Task, *store.PromptAttempt, Projection, *store.SessionTurn) {
	t.Helper()
	task, sourceUID, attempt, projection := restoredProjectionFixture()
	task.UID = types.UID(sourceUID)
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "publication-session"}
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/example/repo.git", CreatePR: true}
	projection.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested,
		LastTransitionTime: timePtr(time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)),
	}
	turn := finalizedSessionProjectionTurn(t, marshalProjection(t, projection), attempt)
	publicationID, err := cleanupPublicationID(attempt.Key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 12, 5, 0, 0, time.UTC)
	digest := func(value string) string { return store.CanonicalBytesDigest([]byte(value)) }
	commit := strings.Repeat("4", 40)
	turn.PublicationID = publicationID
	turn.PublicationReceipt = &store.PublicationReceipt{
		PublicationID: publicationID, Generation: 1, State: store.PublicationVerifiedExact,
		Prepared: &store.PreparedPublicationReceipt{
			OperationID: "prepare", RequestDigest: digest("prepare"), TreeSHA: strings.Repeat("3", 40), CommitSHA: commit,
			ManifestDigest: digest("manifest"), BundleArtifactID: "bundle-artifact", BundleDigest: digest("bundle"),
			BundleSizeBytes: 128, BundleMediaType: store.PreparedBundleMediaType,
			BundleRef: "refs/orka/publications/" + strings.Repeat("f", 64), PreparedAt: now,
		},
		Publish: &store.PublishOperationReceipt{
			OperationID: "publish", RequestDigest: digest("publish"), TargetRepositoryID: "github.com/example/repo",
			TargetRef: "refs/heads/release-gate", RemoteBefore: store.RemoteRefState{Absent: true}, ExpectedCommitSHA: commit, PublishedAt: now,
		},
		Verification: &store.PublicationVerificationReceipt{
			OperationID: "verify", RequestDigest: digest("verify"), Outcome: store.PublicationVerifiedExact,
			ExpectedCommitSHA: commit, ObservedRemote: store.RemoteRefState{SHA: commit}, VerifiedAt: now,
		},
		PullRequest: &store.PullRequestOperationReceipt{
			OperationID: "pr", RequestDigest: digest("pr"), IntentKey: digest("pr-intent"), ForgeID: "github:123:1",
			URL: "https://github.com/example/repo/pull/1", State: "Open", HeadSHA: commit, ReconciledAt: now,
		},
	}
	return task, attempt, projection, turn
}
