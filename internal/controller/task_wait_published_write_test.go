/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/publisher"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// A write turn publishes, parks on a timer, and resumes: the continuation
// created by the wait controller must prepare its workspace and its next
// publication from the Session's verified baseline (the published commit),
// not fail on the parked Task's pre-publication expectedRemoteSHA.
func TestPublishedWriteTurnParksAndItsContinuationStartsFromTheSessionBaseline(t *testing.T) {
	const repositoryID = "github.com/orka-agents/orka"
	prePublicationSHA := strings.Repeat("a", 40)
	// The fake publisher resolves the branch at this commit, as it is after publication.
	publishedSHA := strings.Repeat("1", 40)

	parked := parkedTestTask(corev1alpha1.TaskWaitKindTimer, corev1alpha1.TaskPhaseSucceeded)
	parked.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		GitRepo: "https://github.com/orka-agents/orka", Intent: corev1alpha1.WorkspaceIntentWrite,
		PushBranch: "orka/release", ExpectedRemoteSHA: prePublicationSHA,
	}
	parked.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
		Branch: "orka/release", ExpectedCommitSHA: publishedSHA, VerifiedRemoteSHA: publishedSHA,
	}
	resumeAt := metav1.NewTime(taskWaitTestNow.Add(-time.Second))
	parked.Status.Wait.ResumeAt = &resumeAt

	// The wait controller arms the wait and resumes it into a continuation.
	h := newTaskWaitHarness(t, parked)
	h.settle("parked")
	wait := h.task("parked").Status.Wait
	if wait.State != corev1alpha1.TaskWaitStateResumed {
		t.Fatalf("the parked write turn must resume, got %+v", wait)
	}
	continuation := h.task(wait.ContinuationTaskName)
	continuation.Status.Execution = &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-continuation-1"}

	// The Session recorded the published commit as its verified baseline.
	baseline := &store.VerifiedBranchBaseline{RepositoryID: repositoryID, Ref: "refs/heads/orka/release", SHA: publishedSHA}
	session := &acpTaskSession{Binding: ACPRuntimeSessionBinding{SessionUID: "session-release"}, VerifiedBaseline: baseline, Reused: true}

	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "published-write.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sealedStore(t, sqlite.NewStore(db, "test"))
	epochs := NewControllerEpochManager(controlStore, "controller-published-write")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	defer cancelEpoch()
	go func() { _ = epochs.Start(epochCtx) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	publisherServer := newDispatcherPublisherServer(t, strings.Repeat("2", 40), strings.Repeat("3", 40), "sha256:"+strings.Repeat("4", 64))
	defer publisherServer.Close()
	publisherClient, err := publisherservice.NewClient(publisherservice.ClientConfig{
		BaseURL: publisherServer.URL, BearerToken: []byte(strings.Repeat("b", 32)),
		CapabilitySecret: []byte(strings.Repeat("c", 32)), HTTPClient: publisherServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	d := &ACPDispatcher{Store: controlStore, Publisher: publisherClient, ArtifactCapabilitySecret: []byte(strings.Repeat("d", 32))}

	// Starting the continuation prepares its workspace from the Session baseline.
	prepared, err := d.prepareRuntimeWorkspace(ctx, continuation, fence, session, taskWaitTestNow, true)
	if err != nil {
		t.Fatalf("the continuation of a published write turn must start: %v", err)
	}
	if prepared.spec.Baseline.Revision != publishedSHA {
		t.Fatalf("the continuation must start from the published commit, got %s", prepared.spec.Baseline.Revision)
	}
	// Its next publication is fenced on the same verified baseline.
	expected, err := expectedPublicationRemoteState(continuation.Spec.Workspace, session, publisher.Repository{ID: repositoryID}, baseline.Ref)
	if err != nil || expected.Absent || expected.SHA != publishedSHA {
		t.Fatalf("the continuation's publication must expect the published commit, got %+v, %v", expected, err)
	}
}
