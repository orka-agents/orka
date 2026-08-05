package kube

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	controlstore "github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReclaimPromptAttemptsDefersWhileReferencesRemain(t *testing.T) {
	tests := []struct {
		name  string
		block func(t *testing.T, ctx context.Context, kubeStore *Store, kubeClient client.Client, fence controlstore.ControllerEpochFence, attempt *controlstore.PromptAttempt)
	}{
		{
			name: "nonterminal attempt",
			block: func(t *testing.T, ctx context.Context, kubeStore *Store, _ client.Client, fence controlstore.ControllerEpochFence, attempt *controlstore.PromptAttempt) {
				t.Helper()
				if _, err := kubeStore.CreatePromptAttempt(ctx, &controlstore.PromptAttempt{
					Key: controlstore.PromptAttemptKey{
						Namespace: attempt.Key.Namespace, TaskUID: attempt.Key.TaskUID, Attempt: 1, PromptID: "prompt-old",
					},
					RequestDigest: testDigest("prompt-old"),
				}, fence); err != nil {
					t.Fatalf("create nonterminal attempt: %v", err)
				}
			},
		},
		{
			name: "active publication",
			block: func(t *testing.T, ctx context.Context, _ *Store, kubeClient client.Client, _ controlstore.ControllerEpochFence, attempt *controlstore.PromptAttempt) {
				t.Helper()
				publication := &corev1alpha1.Publication{
					ObjectMeta: metav1.ObjectMeta{Namespace: attempt.Key.Namespace, Name: "publication-active"},
					Spec: corev1alpha1.PublicationSpec{
						ID: "publication-active", TaskUID: attempt.Key.TaskUID, Attempt: attempt.Key.Attempt, PromptID: attempt.Key.PromptID,
					},
				}
				if err := kubeClient.Create(ctx, publication); err != nil {
					t.Fatalf("create active publication: %v", err)
				}
				publication.Status.State = corev1alpha1.PublicationControlState(controlstore.PublicationPublishing)
				if err := kubeClient.Status().Update(ctx, publication); err != nil {
					t.Fatalf("mark publication active: %v", err)
				}
			},
		},
		{
			name: "session reconciliation reference",
			block: func(t *testing.T, ctx context.Context, _ *Store, kubeClient client.Client, _ controlstore.ControllerEpochFence, attempt *controlstore.PromptAttempt) {
				t.Helper()
				control := &corev1alpha1.RuntimeSessionControl{
					ObjectMeta: metav1.ObjectMeta{Namespace: attempt.Key.Namespace, Name: "session-reconciliation"},
					Spec: corev1alpha1.RuntimeSessionControlSpec{
						SessionName: "session-reconciliation", SessionUID: "session-reconciliation-uid",
						RequestDigest: testDigest("session-reconciliation"),
						Owner:         corev1alpha1.ControlRecordOwner{Kind: "Session", UID: "session-reconciliation-uid"},
					},
				}
				if err := kubeClient.Create(ctx, control); err != nil {
					t.Fatalf("create Session control: %v", err)
				}
				control.Status.Availability = corev1alpha1.RuntimeSessionControlAvailability(controlstore.SessionReconciliationBlocked)
				control.Status.BlockedReason = "publication outcome requires reconciliation"
				control.Status.RelatedPromptAttemptID = attempt.ID
				if err := kubeClient.Status().Update(ctx, control); err != nil {
					t.Fatalf("record Session reconciliation reference: %v", err)
				}
			},
		},
		{
			name: "undelivered terminal projection",
			block: func(t *testing.T, _ context.Context, _ *Store, _ client.Client, _ controlstore.ControllerEpochFence, _ *controlstore.PromptAttempt) {
				t.Helper()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
			attempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-reclaim-blocked", 2, "prompt-final")
			projectionID := enqueueTaskTerminalProjection(t, ctx, kubeStore, fence, attempt, tt.name != "undelivered terminal projection")
			tt.block(t, ctx, kubeStore, kubeClient, fence, attempt)
			taskName := createDeletingTaskForPromptAttempt(t, ctx, kubeClient, attempt)

			deleted, err := kubeStore.ReclaimPromptAttempts(ctx, controlstore.ReclaimPromptAttemptsRequest{
				Namespace: attempt.Key.Namespace, TaskName: taskName, TaskUID: attempt.Key.TaskUID,
				Mode:                 controlstore.PromptAttemptReclamationProjected,
				FinalPromptAttemptID: attempt.ID, TerminalProjectionID: projectionID, Fence: fence,
			})
			if !errors.Is(err, controlstore.ErrNotReady) {
				t.Fatalf("ReclaimPromptAttempts() error = %v, want ErrNotReady", err)
			}
			if deleted != 0 {
				t.Fatalf("ReclaimPromptAttempts() deleted = %d, want 0", deleted)
			}
			if _, err := kubeStore.GetPromptAttempt(ctx, attempt.ID); err != nil {
				t.Fatalf("final prompt attempt was deleted while still referenced: %v", err)
			}
		})
	}
}

func TestReclaimPromptAttemptsDeletesTerminalTaskRecords(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
	oldAttempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-reclaim", 1, "prompt-old")
	finalAttempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-reclaim", 2, "prompt-final")
	otherAttempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-other", 1, "prompt-other")
	taskName := createDeletingTaskForPromptAttempt(t, ctx, kubeClient, finalAttempt)
	projectionID := enqueueTaskTerminalProjection(t, ctx, kubeStore, fence, finalAttempt, true)

	request := controlstore.ReclaimPromptAttemptsRequest{
		Namespace: finalAttempt.Key.Namespace, TaskName: taskName, TaskUID: finalAttempt.Key.TaskUID,
		Mode:                 controlstore.PromptAttemptReclamationProjected,
		FinalPromptAttemptID: finalAttempt.ID, TerminalProjectionID: projectionID, Fence: fence,
	}
	deleted, err := kubeStore.ReclaimPromptAttempts(ctx, request)
	if err != nil {
		t.Fatalf("ReclaimPromptAttempts(): %v", err)
	}
	if deleted != 2 {
		t.Fatalf("ReclaimPromptAttempts() deleted = %d, want 2", deleted)
	}
	for _, id := range []string{oldAttempt.ID, finalAttempt.ID} {
		if _, err := kubeStore.GetPromptAttempt(ctx, id); !errors.Is(err, controlstore.ErrNotFound) {
			t.Fatalf("GetPromptAttempt(%q) error = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := kubeStore.GetPromptAttempt(ctx, otherAttempt.ID); err != nil {
		t.Fatalf("unrelated PromptAttempt was deleted: %v", err)
	}
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, finalAttempt.Key.TaskUID, false)
	assertPromptAttemptReclamationCompleted(t, ctx, kubeClient, request, true)
	if err := kubeStore.PreparePromptAttemptReclamation(ctx, request); err != nil {
		t.Fatalf("idempotent PreparePromptAttemptReclamation() error = %v", err)
	}
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, finalAttempt.Key.TaskUID, false)
	if deleted, err := kubeStore.ReclaimPromptAttempts(ctx, request); err != nil || deleted != 0 {
		t.Fatalf("idempotent ReclaimPromptAttempts() = deleted:%d err:%v, want 0,nil", deleted, err)
	}
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, finalAttempt.Key.TaskUID, false)
}

func TestReclaimPromptAttemptsRecoversMarkerCleanupCrash(t *testing.T) {
	tests := []struct {
		name                 string
		persistMarkerDelete  bool
		wantMarkerAfterError bool
	}{
		{name: "before marker delete", wantMarkerAfterError: true},
		{name: "after marker delete before acknowledgement", persistMarkerDelete: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
			attempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-reclaim-cleanup-crash", 1, "prompt-final")
			taskName := createDeletingTaskForPromptAttempt(t, ctx, kubeClient, attempt)
			projectionID := enqueueTaskTerminalProjection(t, ctx, kubeStore, fence, attempt, true)
			request := controlstore.ReclaimPromptAttemptsRequest{
				Namespace: attempt.Key.Namespace, TaskName: taskName, TaskUID: attempt.Key.TaskUID,
				Mode:                 controlstore.PromptAttemptReclamationProjected,
				FinalPromptAttemptID: attempt.ID, TerminalProjectionID: projectionID, Fence: fence,
			}
			if err := kubeStore.PreparePromptAttemptReclamation(ctx, request); err != nil {
				t.Fatalf("PreparePromptAttemptReclamation(): %v", err)
			}
			assertPromptAttemptReclamationMarker(t, ctx, kubeClient, attempt.Key.TaskUID, true)

			withWatch, ok := kubeClient.(client.WithWatch)
			if !ok {
				t.Fatal("fake client does not implement client.WithWatch")
			}
			injectedErr := errors.New("simulated marker cleanup crash")
			markerName := promptAttemptReclamationMarkerName(attempt.Key.TaskUID)
			kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					marker, isMarker := object.(*corev1.ConfigMap)
					if !isMarker || marker.Namespace != testControlNamespace || marker.Name != markerName {
						return c.Delete(ctx, object, options...)
					}
					if tt.persistMarkerDelete {
						if err := c.Delete(ctx, object, options...); err != nil {
							return err
						}
					}
					return injectedErr
				},
			})
			deleted, err := kubeStore.ReclaimPromptAttempts(ctx, request)
			if !errors.Is(err, injectedErr) {
				t.Fatalf("ReclaimPromptAttempts() error = %v, want injected cleanup error", err)
			}
			if deleted != 1 {
				t.Fatalf("ReclaimPromptAttempts() deleted = %d, want 1", deleted)
			}
			if _, err := kubeStore.GetPromptAttempt(ctx, attempt.ID); !errors.Is(err, controlstore.ErrNotFound) {
				t.Fatalf("GetPromptAttempt() after cleanup crash error = %v, want ErrNotFound", err)
			}
			assertPromptAttemptReclamationMarker(t, ctx, kubeClient, attempt.Key.TaskUID, tt.wantMarkerAfterError)
			assertPromptAttemptReclamationCompleted(t, ctx, kubeClient, request, true)

			kubeStore.client = kubeClient
			if err := kubeStore.PreparePromptAttemptReclamation(ctx, request); err != nil {
				t.Fatalf("PreparePromptAttemptReclamation() retry: %v", err)
			}
			if deleted, err := kubeStore.ReclaimPromptAttempts(ctx, request); err != nil || deleted != 0 {
				t.Fatalf("ReclaimPromptAttempts() retry = deleted:%d err:%v, want 0,nil", deleted, err)
			}
			assertPromptAttemptReclamationMarker(t, ctx, kubeClient, attempt.Key.TaskUID, false)
		})
	}
}

func TestReclaimPromptAttemptsRecordsCompletionOnlyAfterDeletionIsObserved(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
	attempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-reclaim-finalized-delete", 1, "prompt-final")
	object := &corev1alpha1.PromptAttempt{}
	key := client.ObjectKey{Namespace: attempt.Key.Namespace, Name: objectName(promptAttemptNamePrefix, attempt.ID)}
	if err := kubeClient.Get(ctx, key, object); err != nil {
		t.Fatalf("get PromptAttempt fixture: %v", err)
	}
	object.Finalizers = []string{"reclamation-test"}
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("add PromptAttempt finalizer: %v", err)
	}
	taskName := createDeletingTaskForPromptAttempt(t, ctx, kubeClient, attempt)
	projectionID := enqueueTaskTerminalProjection(t, ctx, kubeStore, fence, attempt, true)
	request := controlstore.ReclaimPromptAttemptsRequest{
		Namespace: attempt.Key.Namespace, TaskName: taskName, TaskUID: attempt.Key.TaskUID,
		Mode:                 controlstore.PromptAttemptReclamationProjected,
		FinalPromptAttemptID: attempt.ID, TerminalProjectionID: projectionID, Fence: fence,
	}
	if _, err := kubeStore.ReclaimPromptAttempts(ctx, request); !errors.Is(err, controlstore.ErrNotReady) {
		t.Fatalf("ReclaimPromptAttempts(finalizer retained) error = %v, want ErrNotReady", err)
	}
	assertPromptAttemptReclamationCompleted(t, ctx, kubeClient, request, false)
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, attempt.Key.TaskUID, true)

	if err := kubeClient.Get(ctx, key, object); err != nil {
		t.Fatalf("get deleting PromptAttempt: %v", err)
	}
	object.Finalizers = nil
	if err := kubeClient.Update(ctx, object); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("remove PromptAttempt finalizer: %v", err)
	}
	if err := kubeStore.PreparePromptAttemptReclamation(ctx, request); err != nil {
		t.Fatalf("PreparePromptAttemptReclamation(retry): %v", err)
	}
	if _, err := kubeStore.ReclaimPromptAttempts(ctx, request); err != nil {
		t.Fatalf("ReclaimPromptAttempts(retry): %v", err)
	}
	assertPromptAttemptReclamationCompleted(t, ctx, kubeClient, request, true)
}

func TestReclaimPromptAttemptsCleansNoAttemptMarker(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
	const taskUID = "task-reclaim-no-attempt"
	createDeletingAgentTask(t, ctx, kubeClient, "tenant-a", taskUID, taskUID)
	request := controlstore.ReclaimPromptAttemptsRequest{
		Namespace: "tenant-a", TaskName: taskUID, TaskUID: taskUID,
		Mode: controlstore.PromptAttemptReclamationNoAttempt, Fence: fence,
	}
	if err := kubeStore.PreparePromptAttemptReclamation(ctx, request); err != nil {
		t.Fatalf("PreparePromptAttemptReclamation(): %v", err)
	}
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, taskUID, true)
	if deleted, err := kubeStore.ReclaimPromptAttempts(ctx, request); err != nil || deleted != 0 {
		t.Fatalf("ReclaimPromptAttempts() = deleted:%d err:%v, want 0,nil", deleted, err)
	}
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, taskUID, false)
	assertPromptAttemptReclamationCompleted(t, ctx, kubeClient, request, true)
	if deleted, err := kubeStore.ReclaimPromptAttempts(ctx, request); err != nil || deleted != 0 {
		t.Fatalf("ReclaimPromptAttempts() retry = deleted:%d err:%v, want 0,nil", deleted, err)
	}
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, taskUID, false)
}

func TestReclaimPromptAttemptsCompletionAllowsUnboundContinuityProjectionRetry(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
	attempt := createFailedPromptAttempt(t, ctx, kubeStore, fence, "task-reclaim-unbound-continuity", 1, "prompt-final")
	taskName := createDeletingTaskForPromptAttempt(t, ctx, kubeClient, attempt)
	projectionID := enqueueTaskTerminalProjection(t, ctx, kubeStore, fence, attempt, true)
	request := controlstore.ReclaimPromptAttemptsRequest{
		Namespace: attempt.Key.Namespace, TaskName: taskName, TaskUID: attempt.Key.TaskUID,
		Mode:                 controlstore.PromptAttemptReclamationProjected,
		ContinuitySession:    true,
		FinalPromptAttemptID: attempt.ID, TerminalProjectionID: projectionID, Fence: fence,
	}
	if deleted, err := kubeStore.ReclaimPromptAttempts(ctx, request); err != nil || deleted != 1 {
		t.Fatalf("ReclaimPromptAttempts() = deleted:%d err:%v, want 1,nil", deleted, err)
	}
	retryRequest := request
	retryRequest.TerminalProjectionID = ""
	if err := kubeStore.PreparePromptAttemptReclamation(ctx, retryRequest); err != nil {
		t.Fatalf("PreparePromptAttemptReclamation(post-attempt retry): %v", err)
	}
	if deleted, err := kubeStore.ReclaimPromptAttempts(ctx, retryRequest); err != nil || deleted != 0 {
		t.Fatalf("ReclaimPromptAttempts(post-attempt retry) = deleted:%d err:%v, want 0,nil", deleted, err)
	}
}

func TestPreparePromptAttemptReclamationRejectsUnprovenEmptyProjectedState(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
	const taskUID = "task-reclaim-missing-attempt"
	createDeletingAgentTask(t, ctx, kubeClient, "tenant-a", taskUID, taskUID)
	request := controlstore.ReclaimPromptAttemptsRequest{
		Namespace: "tenant-a", TaskName: taskUID, TaskUID: taskUID,
		Mode: controlstore.PromptAttemptReclamationProjected, FinalPromptAttemptID: "missing-attempt",
		TerminalProjectionID: "missing-projection", Fence: fence,
	}
	if err := kubeStore.PreparePromptAttemptReclamation(ctx, request); !errors.Is(err, controlstore.ErrNotReady) {
		t.Fatalf("PreparePromptAttemptReclamation() error = %v, want ErrNotReady", err)
	}
	assertPromptAttemptReclamationMarker(t, ctx, kubeClient, taskUID, false)
	assertPromptAttemptReclamationCompleted(t, ctx, kubeClient, request, false)
}

func TestCreatePromptAttemptRejectsDeletingOrMissingTaskOwner(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newPromptAttemptReclaimStore(t)
	const taskUID = "task-reclaim-late-create"
	createDeletingAgentTask(t, ctx, kubeClient, "tenant-a", taskUID, taskUID)
	attempt := &controlstore.PromptAttempt{
		Key:           controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: taskUID, Attempt: 1, PromptID: "late"},
		RequestDigest: testDigest("late-create"),
	}
	if _, err := kubeStore.CreatePromptAttempt(ctx, attempt, fence); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("CreatePromptAttempt(deleting Task) error = %v, want ErrConflict", err)
	}

	missing := *attempt
	missing.Key.TaskUID = "task-reclaim-missing-owner"
	missing.Key.PromptID = "missing-owner"
	missing.ID = ""
	missing.RequestDigest = testDigest("missing-owner")
	if _, err := kubeStore.CreatePromptAttempt(ctx, &missing, fence); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("CreatePromptAttempt(missing Task) error = %v, want ErrConflict", err)
	}
}

func createDeletingTaskForPromptAttempt(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
	attempt *controlstore.PromptAttempt,
) string {
	t.Helper()
	name := attempt.Key.TaskUID
	createDeletingAgentTask(t, ctx, kubeClient, attempt.Key.Namespace, name, attempt.Key.TaskUID)
	return name
}

func createDeletingAgentTask(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
	namespace, name, uid string,
) {
	t.Helper()
	task := ensureActiveAgentTask(t, ctx, kubeClient, namespace, name, uid)
	if len(task.Finalizers) == 0 {
		task.Finalizers = []string{"reclamation-test"}
		if err := kubeClient.Update(ctx, task); err != nil {
			t.Fatalf("add deleting Task finalizer: %v", err)
		}
	}
	if err := kubeClient.Delete(ctx, task); err != nil {
		t.Fatalf("mark Task deleting: %v", err)
	}
}

func assertPromptAttemptReclamationMarker(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
	taskUID string,
	want bool,
) {
	t.Helper()
	marker := &corev1.ConfigMap{}
	err := kubeClient.Get(ctx, client.ObjectKey{
		Namespace: testControlNamespace,
		Name:      promptAttemptReclamationMarkerName(taskUID),
	}, marker)
	if want && err != nil {
		t.Fatalf("get prompt attempt reclamation marker: %v", err)
	}
	if !want && !apierrors.IsNotFound(err) {
		t.Fatalf("prompt attempt reclamation marker still exists or lookup failed: %v", err)
	}
}

func assertPromptAttemptReclamationCompleted(
	t *testing.T,
	ctx context.Context,
	kubeClient client.Client,
	request controlstore.ReclaimPromptAttemptsRequest,
	want bool,
) {
	t.Helper()
	task := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: request.Namespace, Name: request.TaskName}, task); err != nil {
		t.Fatalf("get Task reclamation completion receipt: %v", err)
	}
	condition := meta.FindStatusCondition(task.Status.Conditions, promptAttemptReclamationCompleteCondition)
	if !want {
		if condition != nil {
			t.Fatalf("unexpected PromptAttempt reclamation completion condition: %#v", condition)
		}
		return
	}
	digest, err := promptAttemptReclamationCompletionDigest(request)
	if err != nil {
		t.Fatalf("promptAttemptReclamationCompletionDigest(): %v", err)
	}
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != promptAttemptReclamationCompleteReason || condition.Message != digest {
		t.Fatalf("PromptAttempt reclamation completion condition = %#v, want digest %q", condition, digest)
	}
}

func newPromptAttemptReclaimStore(t *testing.T) (*Store, client.Client, controlstore.ControllerEpochFence) {
	t.Helper()
	_, kubeClient, fence := newTestStoreWithEpoch(t)
	db, err := sqlitestore.NewDB(filepath.Join(t.TempDir(), "prompt-attempt-reclaim.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	kubeStore, err := NewComposite(kubeClient, testControlNamespace, sqlitestore.NewStore(db, ""))
	if err != nil {
		t.Fatalf("NewComposite: %v", err)
	}
	return kubeStore, kubeClient, fence
}

func createFailedPromptAttempt(
	t *testing.T,
	ctx context.Context,
	kubeStore *Store,
	fence controlstore.ControllerEpochFence,
	taskUID string,
	attemptNumber int64,
	promptID string,
) *controlstore.PromptAttempt {
	t.Helper()
	ensureActiveAgentTask(t, ctx, kubeStore.client, "tenant-a", taskUID, taskUID)
	attempt, err := kubeStore.CreatePromptAttempt(ctx, &controlstore.PromptAttempt{
		Key: controlstore.PromptAttemptKey{
			Namespace: "tenant-a", TaskUID: taskUID, Attempt: attemptNumber, PromptID: promptID,
		},
		RequestDigest: testDigest(taskUID + ":" + promptID),
	}, fence)
	if err != nil {
		t.Fatalf("CreatePromptAttempt: %v", err)
	}
	attempt, err = kubeStore.TransitionPromptAttemptExecution(ctx, controlstore.PromptAttemptExecutionTransition{
		ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
		NewState: controlstore.PromptExecutionFailed, OperationID: "fail-" + promptID,
		OperationDigest: testDigest("fail-" + taskUID + ":" + promptID), TerminalReason: "test terminal failure",
		UpdatedAt: testNow.Add(time.Duration(attemptNumber) * time.Minute),
	})
	if err != nil {
		t.Fatalf("TransitionPromptAttemptExecution(Failed): %v", err)
	}
	return attempt
}

func enqueueTaskTerminalProjection(
	t *testing.T,
	ctx context.Context,
	kubeStore *Store,
	fence controlstore.ControllerEpochFence,
	attempt *controlstore.PromptAttempt,
	delivered bool,
) string {
	t.Helper()
	projectionID := controlstore.CanonicalControlID(
		"task-terminal-projection", attempt.Key.Namespace, attempt.Key.TaskUID, fmt.Sprint(attempt.Key.Attempt),
	)
	payload := []byte(`{"phase":"Failed"}`)
	projection, err := kubeStore.EnqueueOutboxProjection(ctx, &controlstore.OutboxProjection{
		ID: projectionID, AggregateKind: "Task", AggregateID: attempt.Key.TaskUID,
		ProjectionKind: "TaskTerminalStatus", PayloadDigest: testBytesDigest(payload), Payload: payload,
		AvailableAt: testNow, CreatedAt: testNow,
	}, fence)
	if err != nil {
		t.Fatalf("EnqueueOutboxProjection: %v", err)
	}
	if !delivered {
		return projectionID
	}
	claims, err := kubeStore.ClaimOutboxProjections(ctx, controlstore.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "reclaim-test", Limit: 1, LeaseDuration: time.Minute, Now: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ClaimOutboxProjections: %v", err)
	}
	if len(claims) != 1 || claims[0].ID != projection.ID {
		t.Fatalf("claimed projections = %#v, want %q", claims, projection.ID)
	}
	claimed := claims[0]
	if _, err := kubeStore.CompleteOutboxProjection(ctx, controlstore.CompleteOutboxProjectionRequest{
		ID: claimed.ID, Fence: fence, ExpectedVersion: claimed.Version, LeaseOwner: claimed.LeaseOwner,
		OperationID: "deliver-" + attempt.Key.PromptID, OperationDigest: testDigest("deliver-" + attempt.Key.PromptID),
		NewState: controlstore.OutboxProjectionDelivered, DeliveryDigest: testDigest("delivered-" + attempt.Key.PromptID),
		UpdatedAt: testNow.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("CompleteOutboxProjection(Delivered): %v", err)
	}
	return projectionID
}
