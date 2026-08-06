package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	controlstore "github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testControlNamespace = "orka-system"

var testNow = time.Date(2026, time.July, 25, 6, 0, 0, 0, time.UTC)

func TestSamePublicationSpecUsesInstantEqualityForCommitTimestamp(t *testing.T) {
	value := controlstore.Publication{ID: "publication-time", CommitTimestamp: testNow}
	object := &corev1alpha1.Publication{Spec: publicationSpecToAPI(value)}
	object.Spec.CommitTimestamp = metav1.NewTime(testNow.In(time.FixedZone("fixture-local", -7*60*60)))
	if !samePublicationSpec(object, value) {
		t.Fatal("equivalent publication commit timestamp instant was rejected after API round trip")
	}
	object.Spec.CommitTimestamp = metav1.NewTime(testNow.Add(time.Second))
	if samePublicationSpec(object, value) {
		t.Fatal("different publication commit timestamp instant was accepted")
	}
}

func TestControllerEpochLeaseCAS(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient := newTestStore(t)

	first, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		ExpectedVersion: 0,
		ExpectedEpoch:   0,
		NewEpoch:        1,
		HolderID:        "controller-a",
		RequestDigest:   testDigest("epoch-1"),
		UpdatedAt:       testNow,
	})
	if err != nil {
		t.Fatalf("create controller epoch: %v", err)
	}
	if first.Version != 1 || first.Epoch != 1 || first.HolderID != "controller-a" {
		t.Fatalf("first epoch = %#v", first)
	}

	var lease coordinationv1.Lease
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(controlstore.DefaultControllerEpochName)}
	if err := kubeClient.Get(ctx, leaseKey, &lease); err != nil {
		t.Fatalf("get epoch Lease: %v", err)
	}
	if lease.ResourceVersion == "" || lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "controller-a" {
		t.Fatalf("epoch Lease not authoritative: %#v", lease)
	}
	var record corev1alpha1.ControllerEpoch
	recordKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(controlstore.DefaultControllerEpochName)}
	if err := kubeClient.Get(ctx, recordKey, &record); err != nil {
		t.Fatalf("get epoch record: %v", err)
	}
	if record.Status.LeaseResourceVersion != lease.ResourceVersion || record.Status.Epoch != 1 {
		t.Fatalf("epoch status = %#v, lease rv = %q", record.Status, lease.ResourceVersion)
	}

	secondChange := controlstore.ControllerEpochCAS{
		ExpectedVersion: 1,
		ExpectedEpoch:   1,
		NewEpoch:        2,
		HolderID:        "controller-b",
		RequestDigest:   testDigest("epoch-2"),
		UpdatedAt:       testNow.Add(time.Minute),
	}
	second, err := kubeStore.CompareAndSwapControllerEpoch(ctx, secondChange)
	if err != nil {
		t.Fatalf("advance controller epoch: %v", err)
	}
	if second.Version != 2 || second.Epoch != 2 || second.HolderID != "controller-b" {
		t.Fatalf("second epoch = %#v", second)
	}

	if _, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		ExpectedVersion: 1,
		ExpectedEpoch:   1,
		NewEpoch:        3,
		HolderID:        "controller-c",
		RequestDigest:   testDigest("epoch-3"),
		UpdatedAt:       testNow.Add(2 * time.Minute),
	}); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("stale epoch CAS error = %v, want conflict", err)
	}

	retry, err := kubeStore.CompareAndSwapControllerEpoch(ctx, secondChange)
	if err != nil {
		t.Fatalf("idempotent epoch retry: %v", err)
	}
	if retry.Version != 2 || retry.Epoch != 2 {
		t.Fatalf("retry epoch = %#v", retry)
	}
}

func TestControllerEpochMutationReleaseUsesAcquiredLeaseVersion(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	leaseKey := client.ObjectKey{
		Namespace: testControlNamespace,
		Name:      controllerEpochLeaseName(controlstore.DefaultControllerEpochName),
	}
	var staleLease coordinationv1.Lease
	if err := rawClient.Get(ctx, leaseKey, &staleLease); err != nil {
		t.Fatalf("get stale epoch Lease fixture: %v", err)
	}
	serveStaleLease := false
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if key == leaseKey && serveStaleLease {
				lease, ok := object.(*coordinationv1.Lease)
				if !ok {
					t.Fatalf("epoch Lease read target = %T", object)
				}
				staleLease.DeepCopyInto(lease)
				serveStaleLease = false
				return nil
			}
			return delegate.Get(ctx, key, object, options...)
		},
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if lease, ok := object.(*coordinationv1.Lease); ok && lease.Name == leaseKey.Name &&
				lease.Annotations[annotationMutationToken] != "" {
				if err := delegate.Update(ctx, object, options...); err != nil {
					return err
				}
				serveStaleLease = true
				return nil
			}
			return delegate.Update(ctx, object, options...)
		},
	})

	key := controlstore.PromptAttemptKey{
		Namespace: "tenant-a", TaskUID: "task-stale-release", Attempt: 1, PromptID: "prompt-stale-release",
	}
	ensureActiveAgentTask(t, ctx, rawClient, key.Namespace, key.TaskUID, key.TaskUID)
	if _, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{
		Key: key, RequestDigest: testDigest("stale-release"),
	}), fence); err != nil {
		t.Fatalf("create prompt attempt: %v", err)
	}

	var released coordinationv1.Lease
	if err := rawClient.Get(ctx, leaseKey, &released); err != nil {
		t.Fatalf("get released epoch Lease: %v", err)
	}
	if token := released.Annotations[annotationMutationToken]; token != "" {
		t.Fatalf("controller epoch mutation token remained after successful mutation: %q", token)
	}
	if expiresAt := released.Annotations[annotationMutationExpiresAt]; expiresAt != "" {
		t.Fatalf("controller epoch mutation expiry remained after successful mutation: %q", expiresAt)
	}
}

func TestControllerEpochMutationWaitsForShortContention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	_, snapshot, err := kubeStore.requireControllerEpoch(ctx, fence)
	if err != nil {
		t.Fatalf("acquire blocking mutation: %v", err)
	}
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(controlstore.DefaultControllerEpochName)}
	contentionObserved := make(chan struct{})
	kubeStore.reader = interceptor.NewClient(withWatch, interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if err := delegate.Get(ctx, key, object, options...); err != nil {
				return err
			}
			if key == leaseKey {
				if lease, ok := object.(*coordinationv1.Lease); ok && lease.Annotations[annotationMutationToken] == snapshot.MutationToken {
					select {
					case <-contentionObserved:
					default:
						close(contentionObserved)
					}
				}
			}
			return nil
		},
	})
	released := make(chan struct{})
	go func() {
		<-contentionObserved
		kubeStore.releaseControllerEpochMutation(snapshot)
		close(released)
	}()
	key := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-contention", Attempt: 1, PromptID: "prompt-contention"}
	ensureActiveAgentTask(t, ctx, rawClient, key.Namespace, key.TaskUID, key.TaskUID)
	attempt, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: key, RequestDigest: testDigest("contention")}), fence)
	if err != nil {
		t.Fatalf("create prompt attempt after short contention: %v", err)
	}
	<-released
	if attempt.ExecutionState != controlstore.PromptExecutionQueued || attempt.Version != 1 {
		t.Fatalf("attempt after contention = %#v", attempt)
	}
}

func TestControllerEpochMutationReleasePreservesConcurrentLeaseUpdate(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	_, snapshot, err := kubeStore.requireControllerEpoch(ctx, fence)
	if err != nil {
		t.Fatalf("acquire mutation: %v", err)
	}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(controlstore.DefaultControllerEpochName)}
	var concurrent coordinationv1.Lease
	if err := rawClient.Get(ctx, leaseKey, &concurrent); err != nil {
		t.Fatalf("get epoch Lease for concurrent update: %v", err)
	}
	concurrent.Annotations["orka.ai/test-concurrent-marker"] = "preserve-me"
	if err := rawClient.Update(ctx, &concurrent); err != nil {
		t.Fatalf("update epoch Lease concurrently: %v", err)
	}

	kubeStore.releaseControllerEpochMutation(snapshot)

	var released coordinationv1.Lease
	if err := rawClient.Get(ctx, leaseKey, &released); err != nil {
		t.Fatalf("get released epoch Lease: %v", err)
	}
	if got := released.Annotations["orka.ai/test-concurrent-marker"]; got != "preserve-me" {
		t.Fatalf("concurrent epoch Lease annotation = %q, want preserved", got)
	}
	if token := released.Annotations[annotationMutationToken]; token != "" {
		t.Fatalf("controller epoch mutation token remained after retrying release: %q", token)
	}
	if expiresAt := released.Annotations[annotationMutationExpiresAt]; expiresAt != "" {
		t.Fatalf("controller epoch mutation expiry remained after retrying release: %q", expiresAt)
	}
}

func TestGetPromptAttemptUsesConfiguredAPIReader(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	key := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-reader", Attempt: 1, PromptID: "prompt-reader"}
	ensureActiveAgentTask(t, ctx, rawClient, key.Namespace, key.TaskUID, key.TaskUID)
	attempt, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: key, RequestDigest: testDigest("reader")}), fence)
	if err != nil {
		t.Fatalf("create prompt attempt: %v", err)
	}
	objectKey := client.ObjectKey{Namespace: key.Namespace, Name: objectName(promptAttemptNamePrefix, attempt.ID)}
	var stale corev1alpha1.PromptAttempt
	if err := rawClient.Get(ctx, objectKey, &stale); err != nil {
		t.Fatalf("get stale PromptAttempt fixture: %v", err)
	}
	if _, err := kubeStore.TransitionPromptAttemptExecution(ctx, controlstore.PromptAttemptExecutionTransition{
		ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: controlstore.PromptExecutionQueued,
		NewState: controlstore.PromptExecutionReserved, OperationID: "reserve-reader", OperationDigest: testDigest("reserve-reader"),
	}); err != nil {
		t.Fatalf("reserve prompt attempt: %v", err)
	}
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	staleWriter := interceptor.NewClient(withWatch, interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if key == objectKey {
				stale.DeepCopyInto(object.(*corev1alpha1.PromptAttempt))
				return nil
			}
			return delegate.Get(ctx, key, object, options...)
		},
		List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if attempts, ok := list.(*corev1alpha1.PromptAttemptList); ok {
				attempts.Items = []corev1alpha1.PromptAttempt{*stale.DeepCopy()}
				return nil
			}
			return delegate.List(ctx, list, options...)
		},
	})
	readerStore, err := New(staleWriter, testControlNamespace, WithAPIReader(rawClient))
	if err != nil {
		t.Fatalf("construct store with API reader: %v", err)
	}

	current, err := readerStore.GetPromptAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatalf("get prompt attempt through API reader: %v", err)
	}
	if current.ExecutionState != controlstore.PromptExecutionReserved || current.Version != 2 {
		t.Fatalf("PromptAttempt = %#v, want fresh Reserved version 2", current)
	}
}

func TestGetPromptAttemptReadsLegacySpecWithoutBindingDigests(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, _ := newTestStoreWithEpoch(t)
	key := controlstore.PromptAttemptKey{
		Namespace: "tenant-a", TaskUID: "legacy-task-uid", Attempt: 1, PromptID: "legacy-prompt",
	}
	id, err := key.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	legacy := &corev1alpha1.PromptAttempt{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: key.Namespace, Name: objectName(promptAttemptNamePrefix, id), Labels: controlLabels(id),
		},
		Spec: corev1alpha1.PromptAttemptSpec{
			ID: id, TaskUID: key.TaskUID, Attempt: key.Attempt, PromptID: key.PromptID,
			RequestDigest: testDigest("legacy-request"),
		},
	}
	if err := kubeClient.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	got, err := kubeStore.GetPromptAttempt(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.BindingDigest != "" || got.SnapshotDigest != "" || got.RequestDigest != legacy.Spec.RequestDigest {
		t.Fatalf("legacy PromptAttempt = %#v", got)
	}
}

func TestPromptAttemptStatusCASAndIdempotency(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	key := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-uid", Attempt: 1, PromptID: "prompt-1"}
	ensureActiveAgentTask(t, ctx, kubeClient, key.Namespace, key.TaskUID, key.TaskUID)
	attempt, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: key, RequestDigest: testDigest("prompt")}), fence)
	if err != nil {
		t.Fatalf("create prompt attempt: %v", err)
	}
	if attempt.Version != 1 || attempt.ExecutionState != controlstore.PromptExecutionQueued || attempt.DeliveryState != controlstore.PromptDeliveryNotRequested {
		t.Fatalf("created attempt = %#v", attempt)
	}
	if attempt.BindingDigest != testDigest("test-v2-binding") || attempt.SnapshotDigest != testDigest("test-v2-snapshot") {
		t.Fatalf("created PromptAttempt binding = %q/%q", attempt.BindingDigest, attempt.SnapshotDigest)
	}
	idempotent, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: key, RequestDigest: testDigest("prompt")}), fence)
	if err != nil || idempotent.ID != attempt.ID || idempotent.Version != 1 {
		t.Fatalf("idempotent create = %#v, %v", idempotent, err)
	}
	mismatchedBinding := boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: key, RequestDigest: testDigest("prompt")})
	mismatchedBinding.BindingDigest = testDigest("different-binding")
	if _, err := kubeStore.CreatePromptAttempt(ctx, mismatchedBinding, fence); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("binding digest mismatch error = %v, want conflict", err)
	}
	unboundKey := controlstore.PromptAttemptKey{Namespace: key.Namespace, TaskUID: key.TaskUID, Attempt: 2, PromptID: "prompt-unbound"}
	if _, err := kubeStore.CreatePromptAttempt(ctx, &controlstore.PromptAttempt{
		Key: unboundKey, RequestDigest: testDigest("prompt-unbound"),
	}, fence); !errors.Is(err, controlstore.ErrValidation) {
		t.Fatalf("unbound PromptAttempt error = %v, want validation", err)
	}

	operationDigest := testDigest("reserve")
	reserved, err := kubeStore.TransitionPromptAttemptExecution(ctx, controlstore.PromptAttemptExecutionTransition{
		ID:              attempt.ID,
		Fence:           fence,
		ExpectedVersion: 1,
		ExpectedState:   controlstore.PromptExecutionQueued,
		NewState:        controlstore.PromptExecutionReserved,
		OperationID:     "reserve-1",
		OperationDigest: operationDigest,
		UpdatedAt:       testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("reserve prompt attempt: %v", err)
	}
	if reserved.Version != 2 || reserved.ExecutionState != controlstore.PromptExecutionReserved {
		t.Fatalf("reserved attempt = %#v", reserved)
	}
	retry, err := kubeStore.TransitionPromptAttemptExecution(ctx, controlstore.PromptAttemptExecutionTransition{
		ID:              attempt.ID,
		Fence:           fence,
		ExpectedVersion: 1,
		ExpectedState:   controlstore.PromptExecutionQueued,
		NewState:        controlstore.PromptExecutionReserved,
		OperationID:     "reserve-1",
		OperationDigest: operationDigest,
		UpdatedAt:       testNow.Add(time.Minute),
	})
	if err != nil || retry.Version != 2 {
		t.Fatalf("idempotent execution retry = %#v, %v", retry, err)
	}
	if _, err := kubeStore.TransitionPromptAttemptExecution(ctx, controlstore.PromptAttemptExecutionTransition{
		ID:              attempt.ID,
		Fence:           fence,
		ExpectedVersion: 1,
		ExpectedState:   controlstore.PromptExecutionQueued,
		NewState:        controlstore.PromptExecutionFailed,
		OperationID:     "fail-stale",
		OperationDigest: testDigest("fail-stale"),
		TerminalReason:  "stale",
	}); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("stale prompt transition error = %v, want conflict", err)
	}

	var object corev1alpha1.PromptAttempt
	objectKey := client.ObjectKey{Namespace: key.Namespace, Name: objectName(promptAttemptNamePrefix, attempt.ID)}
	if err := kubeClient.Get(ctx, objectKey, &object); err != nil {
		t.Fatalf("get PromptAttempt object: %v", err)
	}
	if object.Status.ControllerEpochLeaseResourceVersion == "" || object.Status.Version != 2 {
		t.Fatalf("PromptAttempt status lacks epoch/resourceVersion fence: %#v", object.Status)
	}
	if object.Spec.BindingDigest != attempt.BindingDigest || object.Spec.SnapshotDigest != attempt.SnapshotDigest {
		t.Fatalf("PromptAttempt spec binding = %q/%q", object.Spec.BindingDigest, object.Spec.SnapshotDigest)
	}
}

func TestStatusUpdateConflictMapsToStoreConflict(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	key := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-conflict", Attempt: 1, PromptID: "prompt"}
	ensureActiveAgentTask(t, ctx, rawClient, key.Namespace, key.TaskUID, key.TaskUID)
	attempt, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: key, RequestDigest: testDigest("prompt-conflict")}), fence)
	if err != nil {
		t.Fatalf("create prompt attempt: %v", err)
	}
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(_ context.Context, _ client.Client, subresource string, obj client.Object, _ ...client.SubResourceUpdateOption) error {
			return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "promptattempts"}, obj.GetName(), errors.New("stale resourceVersion"))
		},
	})
	_, err = kubeStore.TransitionPromptAttemptExecution(ctx, controlstore.PromptAttemptExecutionTransition{
		ID:              attempt.ID,
		Fence:           fence,
		ExpectedVersion: attempt.Version,
		ExpectedState:   controlstore.PromptExecutionQueued,
		NewState:        controlstore.PromptExecutionReserved,
		OperationID:     "reserve-conflict",
		OperationDigest: testDigest("reserve-conflict"),
	})
	if !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("resourceVersion conflict error = %v, want store.ErrConflict", err)
	}
}

//nolint:gocyclo // The integration test intentionally exercises the complete lease/reconciliation state machine.
func TestSessionMutationLeaseAndReconciliation(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)

	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "session-a", SessionUID: "session-uid-a", RequestDigest: testDigest("session-a"),
	}, fence)
	if err != nil {
		t.Fatalf("create session control: %v", err)
	}
	expires := testNow.Add(10 * time.Minute)
	request := controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		TaskUID: "task-session", Attempt: 1, PromptID: "prompt-session", RequestDigest: testDigest("session-lease"),
		AcquiredAt: testNow, ExpiresAt: &expires, Lineage: testSessionLineageClaim(control),
	}
	leased, err := kubeStore.AcquireSessionMutationLease(ctx, request)
	if err != nil {
		t.Fatalf("acquire session mutation Lease: %v", err)
	}
	if leased.Version != 2 || leased.LeaseGeneration != 1 || leased.Lease == nil || leased.Lease.TaskUID != request.TaskUID {
		t.Fatalf("leased session = %#v", leased)
	}
	retry, err := kubeStore.AcquireSessionMutationLease(ctx, request)
	if err != nil || retry.Version != leased.Version {
		t.Fatalf("idempotent lease retry = %#v, %v", retry, err)
	}
	crossProtocol := request
	crossProtocolLineage := *request.Lineage
	crossProtocolLineage.ContractVersion = "orka.harness.v1"
	crossProtocol.Lineage = &crossProtocolLineage
	if _, err := kubeStore.AcquireSessionMutationLease(ctx, crossProtocol); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("cross-protocol continuation error = %v, want conflict", err)
	}
	competitor := request
	competitor.TaskUID = "task-other"
	competitor.PromptID = "prompt-other"
	competitor.RequestDigest = testDigest("session-lease-other")
	if _, err := kubeStore.AcquireSessionMutationLease(ctx, competitor); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("competing lease error = %v, want conflict", err)
	}
	var sessionLease coordinationv1.Lease
	leaseKey := client.ObjectKey{Namespace: control.Namespace, Name: runtimeSessionLeaseName(control.SessionUID)}
	if err := kubeClient.Get(ctx, leaseKey, &sessionLease); err != nil {
		t.Fatalf("get Session Lease: %v", err)
	}
	if sessionLease.Spec.HolderIdentity == nil || leased.Lease == nil || leased.LeaseGeneration != 1 {
		t.Fatalf("Session Lease/status not authoritative: lease=%#v control=%#v", sessionLease.Spec, leased)
	}

	oldSHA := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	branch, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "repo-a", Ref: "refs/heads/session", OwnerKind: controlstore.BranchClaimOwnerSession,
		OwnerUID: "blocked-session-uid", LastVerified: controlstore.RemoteRefState{SHA: oldSHA},
		Availability: controlstore.BranchClaimReconciliationBlocked, BlockedReason: "unknown publish",
		RelatedPublicationID: "publication-blocked", RequestDigest: testDigest("blocked-branch"),
	}, fence)
	if err != nil {
		t.Fatalf("create blocked branch claim: %v", err)
	}
	blocked, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "blocked-session", SessionUID: "blocked-session-uid",
		RequestDigest: testDigest("blocked-session"), Availability: controlstore.SessionReconciliationBlocked,
		LeaseGeneration: 1, BlockedReason: "unknown publish", RelatedPromptAttemptID: "attempt-blocked",
		RelatedPublicationID: "publication-blocked",
		VerifiedBaseline:     &controlstore.VerifiedBranchBaseline{RepositoryID: "repo-a", Ref: "refs/heads/session", SHA: oldSHA},
	}, fence)
	if err != nil {
		t.Fatalf("create blocked session: %v", err)
	}
	reconcile := controlstore.ReconcileSessionControlRequest{
		Namespace: blocked.Namespace, SessionName: blocked.SessionName, SessionUID: blocked.SessionUID,
		Fence: fence, ExpectedVersion: blocked.Version, ExpectedLeaseGeneration: blocked.LeaseGeneration,
		ExpectedRelatedPublicationID: "publication-blocked", BranchClaimID: branch.ID,
		ExpectedBranchClaimVersion: branch.Version, ExpectedBranchClaimGeneration: branch.Generation,
		ExpectedBranchBaseline: branch.LastVerified,
		VerifiedBaseline:       controlstore.VerifiedBranchBaseline{RepositoryID: branch.RepositoryID, Ref: branch.Ref, SHA: newSHA},
		OperationID:            "reconcile-session", OperationDigest: testDigest("reconcile-session"), ReconciledAt: testNow.Add(time.Hour),
	}
	reconciled, err := kubeStore.ReconcileSessionControl(ctx, reconcile)
	if err != nil {
		t.Fatalf("reconcile session control: %v", err)
	}
	if reconciled.Availability != controlstore.SessionAvailable || reconciled.VerifiedBaseline == nil || reconciled.VerifiedBaseline.SHA != newSHA || reconciled.Version != blocked.Version+1 {
		t.Fatalf("reconciled session = %#v", reconciled)
	}
	reconciledBranch, err := kubeStore.GetBranchClaim(ctx, branch.ID)
	if err != nil {
		t.Fatalf("get reconciled branch: %v", err)
	}
	if reconciledBranch.Availability != controlstore.BranchClaimAvailable || reconciledBranch.LastVerified.SHA != newSHA || reconciledBranch.Version != branch.Version+1 {
		t.Fatalf("reconciled branch = %#v", reconciledBranch)
	}
	var reconciliationLease coordinationv1.Lease
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: blocked.Namespace, Name: runtimeSessionLeaseName(blocked.SessionUID)}, &reconciliationLease); err != nil {
		t.Fatalf("get reconciliation Lease: %v", err)
	}
	if reconciliationLease.Spec.HolderIdentity != nil || reconciliationLease.Annotations[annotationLeaseMode] != leaseModeEmpty {
		t.Fatalf("reconciliation Lease was not released: %#v", reconciliationLease)
	}
	if retry, err := kubeStore.ReconcileSessionControl(ctx, reconcile); err != nil || retry.Version != reconciled.Version {
		t.Fatalf("idempotent reconciliation retry = %#v, %v", retry, err)
	}
}

func TestSessionLineageStatusCASRecoversAfterLeaseWrite(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "lineage-status-retry", SessionUID: "lineage-status-retry-uid",
		RequestDigest: testDigest("lineage-status-retry"),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	request := controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		TaskUID: "task-lineage-retry", Attempt: 1, PromptID: "prompt-lineage-retry",
		RequestDigest: testDigest("lease-lineage-retry"), AcquiredAt: testNow,
		Lineage: testSessionLineageClaim(control),
	}
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	failStatusOnce := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if session, ok := obj.(*corev1alpha1.RuntimeSessionControl); ok && failStatusOnce && session.Status.Lineage != nil {
				failStatusOnce = false
				return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated crash after Lease write"))
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	if _, err := kubeStore.AcquireSessionMutationLease(ctx, request); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("first acquisition error = %v, want conflict", err)
	}

	var partial corev1alpha1.RuntimeSessionControl
	if err := rawClient.Get(ctx, client.ObjectKey{Namespace: control.Namespace, Name: runtimeSessionObjectName(control.SessionName)}, &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Status.Lineage != nil || partial.Status.MutationLease != nil {
		t.Fatalf("status exposed a partial lineage/Lease commit: %+v", partial.Status)
	}
	var held coordinationv1.Lease
	if err := rawClient.Get(ctx, client.ObjectKey{Namespace: control.Namespace, Name: runtimeSessionLeaseName(control.SessionUID)}, &held); err != nil {
		t.Fatal(err)
	}
	if held.Spec.HolderIdentity == nil || *held.Spec.HolderIdentity == "" {
		t.Fatal("coordination Lease was not retained across the simulated crash")
	}
	altered := request
	alteredLineage := *request.Lineage
	alteredLineage.ConfigDigest = testDigest("different-lineage-config")
	altered.Lineage = &alteredLineage
	if _, err := kubeStore.AcquireSessionMutationLease(ctx, altered); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("altered lineage completed pending Lease: %v", err)
	}
	if err := rawClient.Get(ctx, client.ObjectKey{Namespace: control.Namespace, Name: runtimeSessionObjectName(control.SessionName)}, &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Status.Lineage != nil || partial.Status.MutationLease != nil {
		t.Fatalf("altered retry committed pending Session status: %+v", partial.Status)
	}

	kubeStore.client = rawClient
	recovered, err := kubeStore.AcquireSessionMutationLease(ctx, request)
	if err != nil {
		t.Fatalf("retry exact acquisition: %v", err)
	}
	if recovered.Lineage == nil || recovered.Lease == nil || recovered.Version != 2 || recovered.LeaseGeneration != 1 {
		t.Fatalf("retry did not atomically expose lineage and Lease: %+v", recovered)
	}
}

func TestSessionReconciliationResumesAfterBranchOnlyCommit(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	oldSHA := strings.Repeat("1", 40)
	newSHA := strings.Repeat("2", 40)
	branch, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "repo-restart", Ref: "refs/heads/session", OwnerKind: controlstore.BranchClaimOwnerSession,
		OwnerUID: "session-restart-uid", LastVerified: controlstore.RemoteRefState{SHA: oldSHA},
		Availability: controlstore.BranchClaimReconciliationBlocked, BlockedReason: "unknown publish",
		RelatedPublicationID: "publication-restart", RequestDigest: testDigest("branch-restart"),
	}, fence)
	if err != nil {
		t.Fatalf("create blocked branch: %v", err)
	}
	blocked, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "session-restart", SessionUID: "session-restart-uid",
		RequestDigest: testDigest("session-restart"), Availability: controlstore.SessionReconciliationBlocked,
		LeaseGeneration: 1, BlockedReason: "unknown publish", RelatedPublicationID: "publication-restart",
		VerifiedBaseline: &controlstore.VerifiedBranchBaseline{RepositoryID: branch.RepositoryID, Ref: branch.Ref, SHA: oldSHA},
	}, fence)
	if err != nil {
		t.Fatalf("create blocked session: %v", err)
	}
	request := controlstore.ReconcileSessionControlRequest{
		Namespace: blocked.Namespace, SessionName: blocked.SessionName, SessionUID: blocked.SessionUID,
		Fence: fence, ExpectedVersion: blocked.Version, ExpectedLeaseGeneration: blocked.LeaseGeneration,
		ExpectedRelatedPublicationID: "publication-restart", BranchClaimID: branch.ID,
		ExpectedBranchClaimVersion: branch.Version, ExpectedBranchClaimGeneration: branch.Generation,
		ExpectedBranchBaseline: branch.LastVerified,
		VerifiedBaseline:       controlstore.VerifiedBranchBaseline{RepositoryID: branch.RepositoryID, Ref: branch.Ref, SHA: newSHA},
		OperationID:            "reconcile-after-restart", OperationDigest: testDigest("reconcile-after-restart"), ReconciledAt: testNow.Add(time.Hour),
	}

	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	failSessionStatusOnce := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failSessionStatusOnce {
				if _, isSession := obj.(*corev1alpha1.RuntimeSessionControl); isSession {
					failSessionStatusOnce = false
					return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated restart"))
				}
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	if _, err := kubeStore.ReconcileSessionControl(ctx, request); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("first reconciliation error = %v, want simulated conflict", err)
	}
	partiallyReconciledBranch, err := kubeStore.GetBranchClaim(ctx, branch.ID)
	if err != nil {
		t.Fatalf("get branch after partial reconciliation: %v", err)
	}
	if partiallyReconciledBranch.Availability != controlstore.BranchClaimAvailable || partiallyReconciledBranch.LastOperationID != request.OperationID {
		t.Fatalf("branch was not committed before simulated restart: %#v", partiallyReconciledBranch)
	}
	stillBlocked, err := kubeStore.GetSessionControl(ctx, blocked.Namespace, blocked.SessionName)
	if err != nil {
		t.Fatalf("get session after partial reconciliation: %v", err)
	}
	if stillBlocked.Availability != controlstore.SessionReconciliationBlocked {
		t.Fatalf("session unexpectedly committed before retry: %#v", stillBlocked)
	}

	reconciled, err := kubeStore.ReconcileSessionControl(ctx, request)
	if err != nil {
		t.Fatalf("resume reconciliation: %v", err)
	}
	if reconciled.Availability != controlstore.SessionAvailable || reconciled.VerifiedBaseline == nil || reconciled.VerifiedBaseline.SHA != newSHA {
		t.Fatalf("resumed session = %#v", reconciled)
	}
}

func TestBranchPublicationAndExternalEffectStores(t *testing.T) {
	ctx := context.Background()
	kubeStore, _, fence := newTestStoreWithEpoch(t)
	branch, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "repo-publish", Ref: "refs/heads/feature", OwnerKind: controlstore.BranchClaimOwnerTask,
		OwnerUID: "task-publish", LastVerified: controlstore.RemoteRefState{Absent: true}, RequestDigest: testDigest("branch-publish"),
	}, fence)
	if err != nil {
		t.Fatalf("create publication branch: %v", err)
	}
	blocked, err := kubeStore.CompareAndSwapBranchClaim(ctx, controlstore.BranchClaimCAS{
		ID: branch.ID, Fence: fence, ExpectedVersion: branch.Version, ExpectedGeneration: branch.Generation,
		NewGeneration: branch.Generation, ExpectedLastVerified: branch.LastVerified, NewLastVerified: branch.LastVerified,
		ExpectedAvailability: controlstore.BranchClaimAvailable, NewAvailability: controlstore.BranchClaimReconciliationBlocked,
		BlockedReason: "test block", RelatedPublicationID: "publication-branch-cas",
		OperationID: "block-branch", OperationDigest: testDigest("block-branch"), UpdatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("block branch claim: %v", err)
	}
	if blocked.Availability != controlstore.BranchClaimReconciliationBlocked || blocked.Version != 2 {
		t.Fatalf("blocked branch = %#v", blocked)
	}
	branch, err = kubeStore.CompareAndSwapBranchClaim(ctx, controlstore.BranchClaimCAS{
		ID: blocked.ID, Fence: fence, ExpectedVersion: blocked.Version, ExpectedGeneration: blocked.Generation,
		NewGeneration: blocked.Generation, ExpectedLastVerified: blocked.LastVerified, NewLastVerified: blocked.LastVerified,
		ExpectedAvailability: blocked.Availability, NewAvailability: controlstore.BranchClaimAvailable,
		OperationID: "unblock-branch", OperationDigest: testDigest("unblock-branch"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("unblock branch claim: %v", err)
	}

	commitSHA := strings.Repeat("c", 40)
	publication, err := kubeStore.CreatePublication(ctx, &controlstore.Publication{
		ID: "publication-1", Namespace: "tenant-a", Generation: 1, TaskUID: "task-publish", Attempt: 1, PromptID: "prompt-publish",
		BranchClaimID: branch.ID, BranchClaimGeneration: branch.Generation,
		SourceRepositoryID: "repo-source", SourceRef: "refs/heads/main", SourceBaselineSHA: strings.Repeat("d", 40),
		TargetRepositoryID: branch.RepositoryID, TargetRef: branch.Ref, Baseline: branch.LastVerified,
		ArtifactID: "artifact-1", ArtifactDigest: testDigest("artifact"), ArtifactSizeBytes: 123,
		ArtifactMediaType: "application/vnd.orka.workspace-delta", PublicationCredentialRef: "publish-secret",
		CommitIdentity: "Orka <orka@example.com>", CommitMessage: "Apply change", CommitTimestamp: testNow,
		RequestDigest: testDigest("publication"),
	}, fence)
	if err != nil {
		t.Fatalf("create publication: %v", err)
	}
	prepared, err := kubeStore.TransitionPublication(ctx, controlstore.PublicationTransition{
		ID: publication.ID, Fence: fence, ExpectedVersion: publication.Version, ExpectedGeneration: publication.Generation,
		ExpectedState: controlstore.PublicationPreparing, NewState: controlstore.PublicationPrepared,
		OperationID: "prepare-publication", OperationDigest: testDigest("prepare-publication"),
		PreparedReceipt: &controlstore.PreparedPublicationReceipt{
			OperationID: "prepare-publication", RequestDigest: testDigest("prepare-response"),
			TreeSHA: strings.Repeat("e", 40), CommitSHA: commitSHA, ManifestDigest: testDigest("manifest"),
			BundleArtifactID: "bundle-artifact", BundleDigest: testDigest("bundle"), BundleSizeBytes: 256,
			BundleMediaType: controlstore.PreparedBundleMediaType, BundleRef: "refs/orka/publications/" + strings.Repeat("f", 64),
			PreparedAt: testNow.Add(time.Minute),
		},
		UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("prepare publication: %v", err)
	}
	if prepared.State != controlstore.PublicationPrepared || prepared.PreparedReceipt == nil || prepared.Version != 2 {
		t.Fatalf("prepared publication = %#v", prepared)
	}
	intent := controlstore.PullRequestIntent{BaseRepositoryID: "repo-source", BaseRef: "refs/heads/main", HeadRepositoryID: branch.RepositoryID, HeadRef: branch.Ref, PublicationGeneration: 1, ExpectedHeadSHA: commitSHA}
	withIntent, err := kubeStore.SetPublicationPRIntent(ctx, controlstore.SetPublicationPRIntentRequest{
		ID: prepared.ID, Fence: fence, ExpectedVersion: prepared.Version, ExpectedGeneration: prepared.Generation,
		ExpectedState: controlstore.PublicationPrepared, Intent: intent,
		OperationID: "set-pr-intent", OperationDigest: testDigest("set-pr-intent"), UpdatedAt: testNow.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("set PR intent: %v", err)
	}
	if withIntent.PRIntent == nil || withIntent.PRIntent.ExpectedHeadSHA != commitSHA || withIntent.Version != 3 {
		t.Fatalf("publication with PR intent = %#v", withIntent)
	}

	identity := controlstore.ExternalEffectIdentity{Kind: "PullRequest", Namespace: "tenant-a", AggregateID: publication.ID, OperationID: "forge-op-1"}
	effect, err := kubeStore.ReserveExternalEffect(ctx, controlstore.ReserveExternalEffectRequest{Identity: identity, RequestDigest: testDigest("effect"), Fence: fence, CreatedAt: testNow})
	if err != nil {
		t.Fatalf("reserve external effect: %v", err)
	}
	expires := testNow.Add(5 * time.Minute)
	inFlight, err := kubeStore.TransitionExternalEffect(ctx, controlstore.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: controlstore.ExternalEffectPending,
		NewState: controlstore.ExternalEffectInFlight, RequestDigest: effect.RequestDigest,
		LeaseOwner: "worker-a", LeaseExpiresAt: &expires, UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("claim external effect: %v", err)
	}
	if inFlight.State != controlstore.ExternalEffectInFlight || inFlight.Attempts != 1 || inFlight.Version != 2 {
		t.Fatalf("in-flight effect = %#v", inFlight)
	}
	response := []byte(`{"url":"https://example.invalid/pr/1"}`)
	succeeded, err := kubeStore.TransitionExternalEffect(ctx, controlstore.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: inFlight.Version, ExpectedState: controlstore.ExternalEffectInFlight,
		NewState: controlstore.ExternalEffectSucceeded, RequestDigest: effect.RequestDigest,
		ExpectedLeaseOwner: "worker-a", Response: response, ResponseDigest: testBytesDigest(response), UpdatedAt: testNow.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("complete external effect: %v", err)
	}
	if succeeded.State != controlstore.ExternalEffectSucceeded || !strings.Contains(string(succeeded.Response), "example.invalid") || succeeded.LeaseOwner != "" {
		t.Fatalf("succeeded effect = %#v", succeeded)
	}
	if retry, err := kubeStore.TransitionExternalEffect(ctx, controlstore.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: inFlight.Version, ExpectedState: controlstore.ExternalEffectInFlight,
		NewState: controlstore.ExternalEffectSucceeded, RequestDigest: effect.RequestDigest,
		ExpectedLeaseOwner: "worker-a", Response: response, ResponseDigest: testBytesDigest(response), UpdatedAt: testNow.Add(2 * time.Minute),
	}); err != nil || retry.Version != succeeded.Version {
		t.Fatalf("idempotent external-effect retry = %#v, %v", retry, err)
	}
}

func TestSessionTurnDelegationIsExplicit(t *testing.T) {
	kubeStore, _ := newTestStore(t)
	if _, err := kubeStore.GetSessionTurn(context.Background(), "turn"); !errors.Is(err, ErrSessionTurnStoreNotConfigured) {
		t.Fatalf("GetSessionTurn error = %v", err)
	}
	delegate := &recordingSessionTurnStore{}
	withDelegate, err := New(kubeStore.client, testControlNamespace, WithSessionTurnPersistence(delegate))
	if err != nil {
		t.Fatalf("New with turn delegate: %v", err)
	}
	if _, err := withDelegate.GetSessionTurn(context.Background(), "turn-1"); err != nil || delegate.gotID != "turn-1" {
		t.Fatalf("delegated GetSessionTurn: id=%q err=%v", delegate.gotID, err)
	}
}

//nolint:gocyclo // The restart boundary assertions intentionally stay in one end-to-end scenario.
func TestCrossStoreSessionTurnFinalizationResumesAfterSQLiteCommit(t *testing.T) {
	ctx := context.Background()
	_, rawClient, fence := newTestStoreWithEpoch(t)
	db, err := sqlitestore.NewDB(filepath.Join(t.TempDir(), "turns.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqliteStore := sqlitestore.NewStore(db, "")
	if err := sqliteStore.CreateSession(ctx, &controlstore.SessionRecord{Namespace: "tenant-a", Name: "session-finalize", SessionType: "task"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	kubeStore, err := NewComposite(rawClient, testControlNamespace, sqliteStore)
	if err != nil {
		t.Fatalf("NewComposite: %v", err)
	}
	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "session-finalize", SessionUID: "session-finalize-uid",
		RequestDigest: testDigest("session-finalize"),
	}, fence)
	if err != nil {
		t.Fatalf("CreateSessionControl: %v", err)
	}
	leaseExpires := testNow.Add(15 * time.Minute)
	promptRequestDigest := testDigest("prompt-finalize")
	leaseRequestDigest, err := controlstore.SessionMutationLeaseRequestDigest(
		control.SessionUID, control.LeaseGeneration+1, "task-finalize", 1, "prompt-finalize", promptRequestDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, err = kubeStore.AcquireSessionMutationLease(ctx, controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		TaskUID: "task-finalize", Attempt: 1, PromptID: "prompt-finalize",
		RequestDigest: leaseRequestDigest, AcquiredAt: testNow, ExpiresAt: &leaseExpires,
		Lineage: testSessionLineageClaim(control),
	})
	if err != nil {
		t.Fatalf("AcquireSessionMutationLease: %v", err)
	}
	attemptKey := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-finalize", Attempt: 1, PromptID: "prompt-finalize"}
	ensureActiveAgentTask(t, ctx, rawClient, attemptKey.Namespace, attemptKey.TaskUID, attemptKey.TaskUID)
	attempt, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: attemptKey, RequestDigest: promptRequestDigest}), fence)
	if err != nil {
		t.Fatalf("CreatePromptAttempt: %v", err)
	}
	attempt = advancePromptAttemptToSuccess(t, ctx, kubeStore, fence, attempt, control)
	turnKey := controlstore.SessionTurnKey{SessionUID: control.SessionUID, LeaseGeneration: control.LeaseGeneration, TaskUID: attemptKey.TaskUID, Attempt: attemptKey.Attempt, PromptID: attemptKey.PromptID}
	turn, err := kubeStore.CreateSessionTurn(ctx, controlstore.CreateSessionTurnRequest{
		Turn:  controlstore.SessionTurn{Key: turnKey, PromptAttemptID: attempt.ID, RequestDigest: testDigest("turn-finalize"), UserPrompt: "finish the task"},
		Fence: fence, ExpectedSessionVersion: control.Version,
	})
	if err != nil {
		t.Fatalf("CreateSessionTurn: %v", err)
	}
	projectionPayload := []byte(`{"phase":"Succeeded"}`)
	projection := controlstore.OutboxProjection{
		ID: "projection-finalize", AggregateKind: sessionTurnAggregateKind, AggregateID: turn.ID,
		ProjectionKind: "TaskTerminalStatus", Payload: projectionPayload, PayloadDigest: testBytesDigest(projectionPayload),
	}
	finalize := controlstore.FinalizeSessionTurnRequest{
		Key: turnKey, Fence: fence, ExpectedSessionVersion: control.Version, ExpectedTurnVersion: turn.Version,
		FinalizationDigest: testDigest("finalize-turn"), TerminalKind: controlstore.SessionTurnAssistantResult,
		TerminalContent: "done", Projection: projection, FinalizedAt: testNow.Add(time.Hour),
	}

	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	failSessionStatusOnce := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failSessionStatusOnce {
				if _, isSession := obj.(*corev1alpha1.RuntimeSessionControl); isSession {
					failSessionStatusOnce = false
					return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "runtimesessioncontrols"}, obj.GetName(), errors.New("simulated controller restart"))
				}
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	if _, err := kubeStore.FinalizeSessionTurn(ctx, finalize); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("first FinalizeSessionTurn error = %v, want simulated conflict", err)
	}
	committedTurn, err := sqliteStore.GetSessionTurn(ctx, turn.ID)
	if err != nil {
		t.Fatalf("GetSessionTurn after SQLite commit: %v", err)
	}
	if committedTurn.State != controlstore.SessionTurnFinalized {
		t.Fatalf("turn did not commit before simulated restart: %#v", committedTurn)
	}
	transcript, err := sqliteStore.LoadTranscript(ctx, control.Namespace, control.SessionName, 10)
	if err != nil {
		t.Fatalf("LoadTranscript after SQLite commit: %v", err)
	}
	if len(transcript) != 2 {
		t.Fatalf("transcript entries = %d, want 2", len(transcript))
	}
	deferred, err := sqliteStore.GetOutboxProjection(ctx, projection.ID)
	if err != nil {
		t.Fatalf("GetOutboxProjection after SQLite commit: %v", err)
	}
	if deferred.AvailableAt.Year() != 9999 {
		t.Fatalf("projection was exposed before Kubernetes finalization: %s", deferred.AvailableAt)
	}
	changedProjection := finalize
	changedProjection.Projection.ID = "projection-finalize-changed"
	if _, err := kubeStore.FinalizeSessionTurn(ctx, changedProjection); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("changed projection retry error = %v, want conflict", err)
	}

	failSessionLeaseOnce := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if failSessionLeaseOnce {
				if lease, isLease := obj.(*coordinationv1.Lease); isLease && lease.Annotations[annotationSessionUID] == control.SessionUID {
					failSessionLeaseOnce = false
					return apierrors.NewConflict(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, obj.GetName(), errors.New("simulated lease-release crash"))
				}
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	resumeRequest := controlstore.ResumeSessionTurnFinalizationRequest{
		Key: committedTurn.Key, PromptAttemptID: committedTurn.PromptAttemptID,
		FinalizationDigest: committedTurn.FinalizationDigest, Fence: fence,
	}
	if _, err := kubeStore.ResumeSessionTurnFinalization(ctx, resumeRequest); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("lease-boundary ResumeSessionTurnFinalization error = %v, want simulated conflict", err)
	}
	statusCommitted, err := kubeStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatalf("GetSessionControl after lease-boundary crash: %v", err)
	}
	if statusCommitted.Lease != nil || statusCommitted.LastOperationID != "finalize:"+turn.ID {
		t.Fatalf("SessionControl was not committed before lease-boundary crash: %#v", statusCommitted)
	}
	kubeStore.client = rawClient
	leases := &coordinationv1.LeaseList{}
	if err := rawClient.List(ctx, leases, client.InNamespace(control.Namespace)); err != nil {
		t.Fatal(err)
	}
	var heldLease *coordinationv1.Lease
	for i := range leases.Items {
		if leases.Items[i].Annotations[annotationSessionUID] == control.SessionUID {
			heldLease = leases.Items[i].DeepCopy()
			break
		}
	}
	if heldLease == nil {
		t.Fatal("held finalized Session Lease not found")
	}
	originalRequestDigest := heldLease.Annotations[annotationRequestDigest]
	originalHolder := *heldLease.Spec.HolderIdentity
	heldLease.Annotations[annotationRequestDigest] = testDigest("mismatched-finalized-lease")
	if err := rawClient.Update(ctx, heldLease); err != nil {
		t.Fatal(err)
	}
	if _, err := kubeStore.ResumeSessionTurnFinalization(ctx, resumeRequest); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("mismatched finalized Lease recovery error = %v, want conflict", err)
	}
	if err := rawClient.Get(ctx, client.ObjectKeyFromObject(heldLease), heldLease); err != nil {
		t.Fatal(err)
	}
	if heldLease.Spec.HolderIdentity == nil || *heldLease.Spec.HolderIdentity == "" {
		t.Fatal("mismatched finalized Session Lease was cleared")
	}
	heldLease.Annotations[annotationRequestDigest] = originalRequestDigest
	if err := rawClient.Update(ctx, heldLease); err != nil {
		t.Fatal(err)
	}
	if err := rawClient.Get(ctx, client.ObjectKeyFromObject(heldLease), heldLease); err != nil {
		t.Fatal(err)
	}
	mismatchedHolder := "mutation:mismatched-finalized-holder"
	heldLease.Spec.HolderIdentity = &mismatchedHolder
	if err := rawClient.Update(ctx, heldLease); err != nil {
		t.Fatal(err)
	}
	if _, err := kubeStore.ResumeSessionTurnFinalization(ctx, resumeRequest); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("mismatched finalized Lease holder recovery error = %v, want conflict", err)
	}
	if err := rawClient.Get(ctx, client.ObjectKeyFromObject(heldLease), heldLease); err != nil {
		t.Fatal(err)
	}
	if heldLease.Spec.HolderIdentity == nil || *heldLease.Spec.HolderIdentity != mismatchedHolder {
		t.Fatal("mismatched finalized Session Lease holder was cleared or changed")
	}
	heldLease.Spec.HolderIdentity = &originalHolder
	if err := rawClient.Update(ctx, heldLease); err != nil {
		t.Fatal(err)
	}

	activationErr := errors.New("simulated outbox activation crash")
	failingActivation := &failOnceSessionTurnActivationStore{SessionTurnPersistenceStore: sqliteStore, err: activationErr}
	kubeStore.sessionTurns = failingActivation
	if _, err := kubeStore.ResumeSessionTurnFinalization(ctx, resumeRequest); !errors.Is(err, activationErr) {
		t.Fatalf("activation-boundary ResumeSessionTurnFinalization error = %v, want simulated error", err)
	}
	kubeStore.sessionTurns = sqliteStore
	finalized, err := kubeStore.ResumeSessionTurnFinalization(ctx, controlstore.ResumeSessionTurnFinalizationRequest{
		Key: committedTurn.Key, PromptAttemptID: committedTurn.PromptAttemptID,
		FinalizationDigest: committedTurn.FinalizationDigest, Fence: fence,
	})
	if err != nil {
		t.Fatalf("ResumeSessionTurnFinalization: %v", err)
	}
	if finalized.State != controlstore.SessionTurnFinalized {
		t.Fatalf("finalized turn = %#v", finalized)
	}
	updatedControl, err := kubeStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatalf("GetSessionControl after finalization: %v", err)
	}
	if updatedControl.Availability != controlstore.SessionAvailable || updatedControl.Lease != nil || updatedControl.LastOperationID != "finalize:"+turn.ID {
		t.Fatalf("finalized Kubernetes SessionControl = %#v", updatedControl)
	}
	activated, err := sqliteStore.GetOutboxProjection(ctx, projection.ID)
	if err != nil {
		t.Fatalf("GetOutboxProjection after activation: %v", err)
	}
	if !activated.AvailableAt.Equal(finalize.FinalizedAt) {
		t.Fatalf("activated projection available_at = %s, want %s", activated.AvailableAt, finalize.FinalizedAt)
	}
	claimed, err := kubeStore.ClaimOutboxProjections(ctx, controlstore.ClaimOutboxProjectionsRequest{
		Fence: fence, WorkerID: "outbox-worker", Limit: 10, LeaseDuration: time.Minute,
		Now: finalize.FinalizedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("ClaimOutboxProjections through Kubernetes fence: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != projection.ID {
		t.Fatalf("claimed projections = %#v", claimed)
	}
	if _, err := kubeStore.CompleteOutboxProjection(ctx, controlstore.CompleteOutboxProjectionRequest{
		ID: claimed[0].ID, Fence: fence, ExpectedVersion: claimed[0].Version,
		LeaseOwner: "outbox-worker", OperationID: "deliver-projection",
		OperationDigest: testDigest("deliver-projection"), NewState: controlstore.OutboxProjectionDelivered,
		DeliveryDigest: testDigest("delivered-status"), UpdatedAt: finalize.FinalizedAt.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("CompleteOutboxProjection through Kubernetes fence: %v", err)
	}
	laterExpires := finalize.FinalizedAt.Add(30 * time.Minute)
	advancedControl, err := kubeStore.AcquireSessionMutationLease(ctx, controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: updatedControl.Namespace, SessionName: updatedControl.SessionName, SessionUID: updatedControl.SessionUID,
		Fence: fence, ExpectedVersion: updatedControl.Version, ExpectedLeaseGeneration: updatedControl.LeaseGeneration,
		TaskUID: "task-later", Attempt: 1, PromptID: "prompt-later", RequestDigest: testDigest("lease-later"),
		AcquiredAt: finalize.FinalizedAt.Add(3 * time.Second), ExpiresAt: &laterExpires,
		Lineage: testSessionLineageClaim(updatedControl),
	})
	if err != nil {
		t.Fatalf("AcquireSessionMutationLease after finalization: %v", err)
	}
	if _, err := kubeStore.ResumeSessionTurnFinalization(ctx, resumeRequest); err != nil {
		t.Fatalf("ResumeSessionTurnFinalization after later lease: %v", err)
	}
	preservedAdvancedControl, err := kubeStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatalf("GetSessionControl after older-turn recovery: %v", err)
	}
	if preservedAdvancedControl.Version != advancedControl.Version || preservedAdvancedControl.LeaseGeneration != advancedControl.LeaseGeneration || preservedAdvancedControl.Lease == nil || preservedAdvancedControl.Lease.TaskUID != "task-later" {
		t.Fatalf("older-turn recovery changed the later Session lease: before=%#v after=%#v", advancedControl, preservedAdvancedControl)
	}
	transcript, err = sqliteStore.LoadTranscript(ctx, control.Namespace, control.SessionName, 10)
	if err != nil {
		t.Fatalf("LoadTranscript after retry: %v", err)
	}
	if len(transcript) != 2 {
		t.Fatalf("idempotent retry duplicated transcript: %d entries", len(transcript))
	}
	for _, table := range []string{"controller_epochs", "prompt_attempts", "session_controls", "branch_claims", "publications", "external_effects"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil { //nolint:gosec // fixed test-only table allowlist
			t.Fatalf("count SQLite %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("SQLite %s contains %d dual-authoritative control rows", table, count)
		}
	}
}

func TestCrossStoreSessionTurnFinalizationDerivesPublicationBaseline(t *testing.T) {
	ctx := context.Background()
	_, rawClient, fence := newTestStoreWithEpoch(t)
	db, err := sqlitestore.NewDB(filepath.Join(t.TempDir(), "publication-turn.db"))
	if err != nil {
		t.Fatalf("NewDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqliteStore := sqlitestore.NewStore(db, "")
	if err := sqliteStore.CreateSession(ctx, &controlstore.SessionRecord{Namespace: "tenant-a", Name: "session-publication", SessionType: "task"}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	kubeStore, err := NewComposite(rawClient, testControlNamespace, sqliteStore)
	if err != nil {
		t.Fatalf("NewComposite: %v", err)
	}
	oldSHA := strings.Repeat("4", 40)
	commitSHA := strings.Repeat("5", 40)
	branch, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "repo-publication", Ref: "refs/heads/session-publication", OwnerKind: controlstore.BranchClaimOwnerSession,
		OwnerUID: "session-publication-uid", LastVerified: controlstore.RemoteRefState{SHA: oldSHA}, RequestDigest: testDigest("publication-branch-finalize"),
	}, fence)
	if err != nil {
		t.Fatalf("CreateBranchClaim: %v", err)
	}
	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "session-publication", SessionUID: "session-publication-uid",
		RequestDigest:    testDigest("session-publication"),
		VerifiedBaseline: &controlstore.VerifiedBranchBaseline{RepositoryID: branch.RepositoryID, Ref: branch.Ref, SHA: oldSHA},
	}, fence)
	if err != nil {
		t.Fatalf("CreateSessionControl: %v", err)
	}
	leaseExpires := testNow.Add(15 * time.Minute)
	promptRequestDigest := testDigest("prompt-publication")
	leaseRequestDigest, err := controlstore.SessionMutationLeaseRequestDigest(
		control.SessionUID, control.LeaseGeneration+1, "task-publication", 1, "prompt-publication", promptRequestDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, err = kubeStore.AcquireSessionMutationLease(ctx, controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		TaskUID: "task-publication", Attempt: 1, PromptID: "prompt-publication",
		RequestDigest: leaseRequestDigest, AcquiredAt: testNow, ExpiresAt: &leaseExpires,
		Lineage: testSessionLineageClaim(control),
	})
	if err != nil {
		t.Fatalf("AcquireSessionMutationLease: %v", err)
	}
	attemptKey := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-publication", Attempt: 1, PromptID: "prompt-publication"}
	ensureActiveAgentTask(t, ctx, rawClient, attemptKey.Namespace, attemptKey.TaskUID, attemptKey.TaskUID)
	attempt, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{Key: attemptKey, RequestDigest: promptRequestDigest}), fence)
	if err != nil {
		t.Fatalf("CreatePromptAttempt: %v", err)
	}
	attempt = advancePromptAttemptToSuccess(t, ctx, kubeStore, fence, attempt, control)
	attempt = advancePromptDeliveryToVerified(t, ctx, kubeStore, fence, attempt)
	publication, err := kubeStore.CreatePublication(ctx, &controlstore.Publication{
		ID: "publication-finalize", Namespace: "tenant-a", Generation: 1,
		TaskUID: attemptKey.TaskUID, Attempt: attemptKey.Attempt, PromptID: attemptKey.PromptID, SessionUID: control.SessionUID,
		BranchClaimID: branch.ID, BranchClaimGeneration: branch.Generation,
		SourceRepositoryID: branch.RepositoryID, SourceRef: branch.Ref, SourceBaselineSHA: oldSHA,
		TargetRepositoryID: branch.RepositoryID, TargetRef: branch.Ref, Baseline: branch.LastVerified,
		ArtifactID: "artifact-publication", ArtifactDigest: testDigest("artifact-publication"), ArtifactSizeBytes: 42,
		ArtifactMediaType: "application/vnd.orka.workspace-delta", PublicationCredentialRef: "publish-secret",
		CommitIdentity: "Orka <orka@example.com>", CommitMessage: "Apply session change", CommitTimestamp: testNow,
		RequestDigest: testDigest("publication-finalize"),
	}, fence)
	if err != nil {
		t.Fatalf("CreatePublication: %v", err)
	}
	preparedDigest := testDigest("prepare-publication-finalize")
	publication, err = kubeStore.TransitionPublication(ctx, controlstore.PublicationTransition{
		ID: publication.ID, Fence: fence, ExpectedVersion: publication.Version, ExpectedGeneration: publication.Generation,
		ExpectedState: publication.State, NewState: controlstore.PublicationPrepared,
		OperationID: "prepare-publication-finalize", OperationDigest: preparedDigest,
		PreparedReceipt: &controlstore.PreparedPublicationReceipt{
			OperationID: "prepare-publication-finalize", RequestDigest: preparedDigest,
			TreeSHA: strings.Repeat("6", 40), CommitSHA: commitSHA,
			ManifestDigest: testDigest("manifest-publication-finalize"), BundleArtifactID: "bundle-publication-finalize",
			BundleDigest: testDigest("bundle-publication-finalize"), BundleSizeBytes: 123,
			BundleMediaType: controlstore.PreparedBundleMediaType, BundleRef: "refs/orka/publications/" + strings.Repeat("a", 64),
			PreparedAt: testNow.Add(time.Minute),
		},
		UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("prepare publication: %v", err)
	}
	publication, err = kubeStore.TransitionPublication(ctx, controlstore.PublicationTransition{
		ID: publication.ID, Fence: fence, ExpectedVersion: publication.Version, ExpectedGeneration: publication.Generation,
		ExpectedState: publication.State, NewState: controlstore.PublicationPublishing,
		OperationID: "start-publish", OperationDigest: testDigest("start-publish"), UpdatedAt: testNow.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("start publishing: %v", err)
	}
	publishDigest := testDigest("publish-publication-finalize")
	publication, err = kubeStore.TransitionPublication(ctx, controlstore.PublicationTransition{
		ID: publication.ID, Fence: fence, ExpectedVersion: publication.Version, ExpectedGeneration: publication.Generation,
		ExpectedState: publication.State, NewState: controlstore.PublicationVerifying,
		OperationID: "publish-publication-finalize", OperationDigest: publishDigest,
		PublishReceipt: &controlstore.PublishOperationReceipt{
			OperationID: "publish-publication-finalize", RequestDigest: publishDigest,
			TargetRepositoryID: branch.RepositoryID, TargetRef: branch.Ref, RemoteBefore: branch.LastVerified,
			ExpectedCommitSHA: commitSHA, PublishedAt: testNow.Add(3 * time.Minute),
		},
		UpdatedAt: testNow.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("record publish receipt: %v", err)
	}
	verifyDigest := testDigest("verify-publication-finalize")
	publication, err = kubeStore.TransitionPublication(ctx, controlstore.PublicationTransition{
		ID: publication.ID, Fence: fence, ExpectedVersion: publication.Version, ExpectedGeneration: publication.Generation,
		ExpectedState: publication.State, NewState: controlstore.PublicationVerifiedExact,
		OperationID: "verify-publication-finalize", OperationDigest: verifyDigest,
		VerificationReceipt: &controlstore.PublicationVerificationReceipt{
			OperationID: "verify-publication-finalize", RequestDigest: verifyDigest,
			Outcome: controlstore.PublicationVerifiedExact, ExpectedCommitSHA: commitSHA,
			ObservedRemote: controlstore.RemoteRefState{SHA: commitSHA}, VerifiedAt: testNow.Add(4 * time.Minute),
		},
		UpdatedAt: testNow.Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("verify publication: %v", err)
	}

	turnKey := controlstore.SessionTurnKey{SessionUID: control.SessionUID, LeaseGeneration: control.LeaseGeneration, TaskUID: attemptKey.TaskUID, Attempt: attemptKey.Attempt, PromptID: attemptKey.PromptID}
	turn, err := kubeStore.CreateSessionTurn(ctx, controlstore.CreateSessionTurnRequest{
		Turn:  controlstore.SessionTurn{Key: turnKey, PromptAttemptID: attempt.ID, RequestDigest: testDigest("turn-publication"), UserPrompt: "publish the change"},
		Fence: fence, ExpectedSessionVersion: control.Version,
	})
	if err != nil {
		t.Fatalf("CreateSessionTurn: %v", err)
	}
	payload := []byte(`{"phase":"Succeeded","delivery":"VerifiedExact"}`)
	finalizeRequest := controlstore.FinalizeSessionTurnRequest{
		Key: turnKey, Fence: fence, ExpectedSessionVersion: control.Version, ExpectedTurnVersion: turn.Version,
		FinalizationDigest: testDigest("finalize-publication-turn"), TerminalKind: controlstore.SessionTurnAssistantResult,
		TerminalContent: "published", PublicationID: publication.ID,
		Projection:  controlstore.OutboxProjection{ID: "projection-publication", AggregateKind: sessionTurnAggregateKind, AggregateID: turn.ID, ProjectionKind: "TaskTerminalStatus", Payload: payload, PayloadDigest: testBytesDigest(payload)},
		FinalizedAt: testNow.Add(time.Hour),
	}
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	failBranchStatusOnce := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if failBranchStatusOnce {
				if _, isBranch := obj.(*corev1alpha1.BranchClaim); isBranch {
					failBranchStatusOnce = false
					return apierrors.NewConflict(schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "branchclaims"}, obj.GetName(), errors.New("simulated controller restart"))
				}
			}
			return c.SubResource(subresource).Update(ctx, obj, opts...)
		},
	})
	if _, err := kubeStore.FinalizeSessionTurn(ctx, finalizeRequest); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("first publication FinalizeSessionTurn error = %v, want simulated conflict", err)
	}
	kubeStore.client = rawClient
	committedTurn, err := kubeStore.GetSessionTurn(ctx, turn.ID)
	if err != nil {
		t.Fatalf("GetSessionTurn after branch-boundary crash: %v", err)
	}
	finalized, err := kubeStore.ResumeSessionTurnFinalization(ctx, controlstore.ResumeSessionTurnFinalizationRequest{
		Key: committedTurn.Key, PromptAttemptID: committedTurn.PromptAttemptID,
		FinalizationDigest: committedTurn.FinalizationDigest, Fence: fence,
	})
	if err != nil {
		t.Fatalf("ResumeSessionTurnFinalization after branch-boundary crash: %v", err)
	}
	if finalized.PublicationReceipt == nil || finalized.PublicationReceipt.State != controlstore.PublicationVerifiedExact {
		t.Fatalf("finalized publication receipt = %#v", finalized.PublicationReceipt)
	}
	updatedControl, err := kubeStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatalf("GetSessionControl: %v", err)
	}
	if updatedControl.VerifiedBaseline == nil || updatedControl.VerifiedBaseline.SHA != commitSHA || updatedControl.Lease != nil {
		t.Fatalf("SessionControl did not advance verified baseline: %#v", updatedControl)
	}
	updatedBranch, err := kubeStore.GetBranchClaim(ctx, branch.ID)
	if err != nil {
		t.Fatalf("GetBranchClaim: %v", err)
	}
	if updatedBranch.LastVerified.SHA != commitSHA || updatedBranch.Availability != controlstore.BranchClaimAvailable {
		t.Fatalf("BranchClaim did not advance verified baseline: %#v", updatedBranch)
	}
}

func advancePromptAttemptToSuccess(t *testing.T, ctx context.Context, kubeStore *Store, fence controlstore.ControllerEpochFence, attempt *controlstore.PromptAttempt, control *controlstore.SessionControl) *controlstore.PromptAttempt {
	t.Helper()
	states := []controlstore.PromptExecutionState{
		controlstore.PromptExecutionReserved,
		controlstore.PromptExecutionSessionStarting,
		controlstore.PromptExecutionPlanned,
		controlstore.PromptExecutionSubmitting,
		controlstore.PromptExecutionAccepted,
		controlstore.PromptExecutionRunning,
		controlstore.PromptExecutionSettling,
		controlstore.PromptExecutionSucceeded,
	}
	for i, next := range states {
		transition := controlstore.PromptAttemptExecutionTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.ExecutionState,
			NewState: next, OperationID: "advance-" + string(next), OperationDigest: testDigest("advance-" + string(next)),
			UpdatedAt: testNow.Add(time.Duration(i+1) * time.Minute),
		}
		if next == controlstore.PromptExecutionSessionStarting {
			transition.SessionUID = control.SessionUID
			transition.SessionLeaseGeneration = control.LeaseGeneration
			transition.RuntimeInstanceID = "runtime-instance"
		}
		updated, err := kubeStore.TransitionPromptAttemptExecution(ctx, transition)
		if err != nil {
			t.Fatalf("TransitionPromptAttemptExecution(%s): %v", next, err)
		}
		attempt = updated
	}
	return attempt
}

func advancePromptDeliveryToVerified(t *testing.T, ctx context.Context, kubeStore *Store, fence controlstore.ControllerEpochFence, attempt *controlstore.PromptAttempt) *controlstore.PromptAttempt {
	t.Helper()
	states := []controlstore.PromptDeliveryState{
		controlstore.PromptDeliveryValidating,
		controlstore.PromptDeliveryPreparing,
		controlstore.PromptDeliveryPrepared,
		controlstore.PromptDeliveryPublishing,
		controlstore.PromptDeliveryVerifying,
		controlstore.PromptDeliveryVerifiedExact,
	}
	for i, next := range states {
		updated, err := kubeStore.TransitionPromptAttemptDelivery(ctx, controlstore.PromptAttemptDeliveryTransition{
			ID: attempt.ID, Fence: fence, ExpectedVersion: attempt.Version, ExpectedState: attempt.DeliveryState,
			NewState: next, OperationID: "delivery-" + string(next), OperationDigest: testDigest("delivery-" + string(next)),
			UpdatedAt: testNow.Add(time.Duration(i+20) * time.Minute),
		})
		if err != nil {
			t.Fatalf("TransitionPromptAttemptDelivery(%s): %v", next, err)
		}
		attempt = updated
	}
	return attempt
}

type recordingSessionTurnStore struct{ gotID string }

type failOnceSessionTurnActivationStore struct {
	controlstore.SessionTurnPersistenceStore
	err    error
	failed bool
}

func (s *failOnceSessionTurnActivationStore) ActivateSessionTurnProjection(ctx context.Context, request controlstore.ActivateSessionTurnProjectionRequest) (*controlstore.OutboxProjection, error) {
	if !s.failed {
		s.failed = true
		return nil, s.err
	}
	return s.SessionTurnPersistenceStore.ActivateSessionTurnProjection(ctx, request)
}

func (*recordingSessionTurnStore) CreateSessionTurnRecord(context.Context, controlstore.CreateSessionTurnRecordRequest) (*controlstore.SessionTurn, error) {
	return &controlstore.SessionTurn{}, nil
}
func (r *recordingSessionTurnStore) GetSessionTurn(_ context.Context, id string) (*controlstore.SessionTurn, error) {
	r.gotID = id
	return &controlstore.SessionTurn{ID: id}, nil
}
func (*recordingSessionTurnStore) CommitSessionTurnFinalization(context.Context, controlstore.CommitSessionTurnFinalizationRequest) (*controlstore.SessionTurn, error) {
	return &controlstore.SessionTurn{}, nil
}
func (*recordingSessionTurnStore) ActivateSessionTurnProjection(context.Context, controlstore.ActivateSessionTurnProjectionRequest) (*controlstore.OutboxProjection, error) {
	return &controlstore.OutboxProjection{}, nil
}

func newTestStore(t *testing.T) (*Store, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add Orka scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coordination scheme: %v", err)
	}
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(
			&corev1alpha1.Task{},
			&corev1alpha1.ControllerEpoch{},
			&corev1alpha1.PromptAttempt{},
			&corev1alpha1.RuntimeSessionControl{},
			&corev1alpha1.BranchClaim{},
			&corev1alpha1.Publication{},
			&corev1alpha1.ExternalEffect{},
		).
		Build()
	kubeStore, err := New(kubeClient, testControlNamespace)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return kubeStore, kubeClient
}

func newTestStoreWithEpoch(t *testing.T) (*Store, client.Client, controlstore.ControllerEpochFence) {
	t.Helper()
	kubeStore, kubeClient := newTestStore(t)
	epoch, err := kubeStore.CompareAndSwapControllerEpoch(context.Background(), controlstore.ControllerEpochCAS{
		ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1, HolderID: "controller-a",
		RequestDigest: testDigest("epoch"), UpdatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("create epoch: %v", err)
	}
	return kubeStore, kubeClient, controlstore.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
}

func ensureActiveAgentTask(t *testing.T, ctx context.Context, kubeClient client.Client, namespace, name, uid string) *corev1alpha1.Task {
	t.Helper()
	key := client.ObjectKey{Namespace: namespace, Name: name}
	existing := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, key, existing); err == nil {
		if string(existing.UID) != uid || existing.Spec.Type != corev1alpha1.TaskTypeAgent || !existing.DeletionTimestamp.IsZero() {
			t.Fatalf("existing PromptAttempt owner Task = %#v", existing)
		}
		return existing
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("get PromptAttempt owner Task: %v", err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(uid)},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	if err := kubeClient.Create(ctx, task); err != nil {
		t.Fatalf("create PromptAttempt owner Task: %v", err)
	}
	return task
}

func testDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func boundPromptAttemptForKubeTest(attempt *controlstore.PromptAttempt) *controlstore.PromptAttempt {
	if attempt == nil {
		return nil
	}
	if attempt.BindingDigest == "" {
		attempt.BindingDigest = testDigest("test-v2-binding")
	}
	if attempt.SnapshotDigest == "" {
		attempt.SnapshotDigest = testDigest("test-v2-snapshot")
	}
	return attempt
}

func testSessionLineageClaim(control *controlstore.SessionControl) *controlstore.ClaimSessionLineageRequest {
	return &controlstore.ClaimSessionLineageRequest{
		Namespace: control.Namespace, SessionName: control.SessionName,
		NamespaceUID: "namespace-uid-" + control.Namespace, SessionUID: control.SessionUID,
		ContractVersion: "orka.harness.v2", LineageGeneration: 1, RuntimeIdentity: "codex",
		ConfigDigest: testDigest("lineage-config-" + control.SessionUID),
		Provenance:   controlstore.SessionLineageFirstUse, EstablishIfAbsent: true,
	}
}

func testBytesDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPromptAttemptFreezesRoleSeparatedCredentialBindings(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	key := controlstore.PromptAttemptKey{Namespace: "tenant-a", TaskUID: "task-credential", Attempt: 1, PromptID: "prompt-credential"}
	ensureActiveAgentTask(t, ctx, kubeClient, key.Namespace, key.TaskUID, key.TaskUID)
	binding := controlstore.PromptCredentialBinding{
		Role: controlstore.PromptCredentialTargetWrite, Namespace: key.Namespace,
		SecretName: "publish-token", SecretKey: "token", SecretUID: "secret-uid", ResourceVersion: "17",
	}
	created, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{
		Key: key, RequestDigest: testDigest("prompt-credential"), CredentialBindings: []controlstore.PromptCredentialBinding{binding},
	}), fence)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.CredentialBindings) != 1 || created.CredentialBindings[0] != binding {
		t.Fatalf("credential bindings = %#v", created.CredentialBindings)
	}
	changed := binding
	changed.ResourceVersion = "18"
	if _, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{
		Key: key, RequestDigest: testDigest("prompt-credential"), CredentialBindings: []controlstore.PromptCredentialBinding{changed},
	}), fence); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("changed credential binding error = %v, want conflict", err)
	}
}

func TestSessionReconciliationRetryClearsLeaseAfterStatusCommit(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	oldSHA, newSHA := strings.Repeat("8", 40), strings.Repeat("9", 40)
	branch, err := kubeStore.CreateBranchClaim(ctx, &controlstore.BranchClaim{
		RepositoryID: "repo-status-tail", Ref: "refs/heads/session", OwnerKind: controlstore.BranchClaimOwnerSession,
		OwnerUID: "session-status-tail-uid", LastVerified: controlstore.RemoteRefState{SHA: oldSHA},
		Availability: controlstore.BranchClaimReconciliationBlocked, BlockedReason: "unknown",
		RelatedPublicationID: "publication-status-tail", RequestDigest: testDigest("branch-status-tail"),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "session-status-tail", SessionUID: "session-status-tail-uid",
		RequestDigest: testDigest("session-status-tail"), Availability: controlstore.SessionReconciliationBlocked,
		LeaseGeneration: 1, BlockedReason: "unknown", RelatedPublicationID: "publication-status-tail",
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	request := controlstore.ReconcileSessionControlRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		ExpectedRelatedPublicationID: "publication-status-tail", BranchClaimID: branch.ID,
		ExpectedBranchClaimVersion: branch.Version, ExpectedBranchClaimGeneration: branch.Generation,
		ExpectedBranchBaseline: branch.LastVerified,
		VerifiedBaseline:       controlstore.VerifiedBranchBaseline{RepositoryID: branch.RepositoryID, Ref: branch.Ref, SHA: newSHA},
		OperationID:            "reconcile-status-tail", OperationDigest: testDigest("reconcile-status-tail"), ReconciledAt: testNow.Add(time.Hour),
	}
	withWatch := rawClient.(client.WithWatch)
	failClearOnce := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if lease, ok := obj.(*coordinationv1.Lease); ok && failClearOnce && (lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "") && lease.Annotations[annotationSessionUID] == control.SessionUID {
				failClearOnce = false
				return apierrors.NewConflict(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, lease.Name, errors.New("simulated clear crash"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	if _, err := kubeStore.ReconcileSessionControl(ctx, request); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("first reconcile = %v", err)
	}
	committed, err := kubeStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Availability != controlstore.SessionAvailable || committed.LastOperationID != request.OperationID {
		t.Fatalf("status not committed: %#v", committed)
	}
	if _, err := kubeStore.ReconcileSessionControl(ctx, request); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
}

//nolint:gocyclo // The crash-tail fencing assertions intentionally stay in one end-to-end scenario.
func TestPrePromptLeaseReleaseResumesAfterStatusCommit(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "abort-tail", SessionUID: "abort-tail-uid", RequestDigest: testDigest("abort-tail"),
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	control, err = kubeStore.AcquireSessionMutationLease(ctx, controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID,
		Fence: fence, ExpectedVersion: control.Version, ExpectedLeaseGeneration: control.LeaseGeneration,
		TaskUID: "task-abort", Attempt: 1, PromptID: "prompt-abort", RequestDigest: testDigest("lease-abort"), AcquiredAt: testNow,
		Lineage: testSessionLineageClaim(control),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := controlstore.SessionTurnKey{SessionUID: control.SessionUID, LeaseGeneration: control.LeaseGeneration, TaskUID: "task-abort", Attempt: 1, PromptID: "prompt-abort"}
	turnID, _ := key.CanonicalID()
	releaseDigest, err := controlstore.SessionLeaseReleaseOperationDigest(turnID, control.Lease.RequestDigest)
	if err != nil {
		t.Fatal(err)
	}
	release := controlstore.ReleaseSessionMutationLeaseRequest{
		Namespace: control.Namespace, SessionName: control.SessionName, SessionUID: control.SessionUID, Key: key,
		Fence: fence, ExpectedSessionVersion: control.Version, LeaseRequestDigest: control.Lease.RequestDigest,
		OperationID: prePromptLeaseReleasePrefix + turnID, OperationDigest: releaseDigest, ReleasedAt: testNow.Add(time.Minute),
	}
	withWatch := rawClient.(client.WithWatch)
	failClearOnce := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if lease, ok := obj.(*coordinationv1.Lease); ok && failClearOnce && (lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "") && lease.Annotations[annotationSessionUID] == control.SessionUID {
				failClearOnce = false
				return apierrors.NewConflict(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, lease.Name, errors.New("simulated release crash"))
			}
			return c.Update(ctx, obj, opts...)
		},
	})
	if _, err := kubeStore.ReleaseSessionMutationLease(ctx, release); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("first release = %v", err)
	}
	committed, err := kubeStore.GetSessionControl(ctx, control.Namespace, control.SessionName)
	if err != nil {
		t.Fatal(err)
	}
	if committed.Lease != nil || committed.LastOperationID != release.OperationID {
		t.Fatalf("release status not committed: %#v", committed)
	}
	leases := &coordinationv1.LeaseList{}
	if err := rawClient.List(ctx, leases, client.InNamespace(control.Namespace)); err != nil {
		t.Fatal(err)
	}
	var sessionLease *coordinationv1.Lease
	for i := range leases.Items {
		if leases.Items[i].Annotations[annotationSessionUID] == control.SessionUID {
			sessionLease = leases.Items[i].DeepCopy()
			break
		}
	}
	if sessionLease == nil {
		t.Fatal("session Lease not found")
	}
	originalRequestDigest := sessionLease.Annotations[annotationRequestDigest]
	originalHolder := *sessionLease.Spec.HolderIdentity
	sessionLease.Annotations[annotationRequestDigest] = testDigest("mismatched-held-lease")
	if err := rawClient.Update(ctx, sessionLease); err != nil {
		t.Fatal(err)
	}
	acquireNext := controlstore.AcquireSessionMutationLeaseRequest{
		Namespace: committed.Namespace, SessionName: committed.SessionName, SessionUID: committed.SessionUID,
		Fence: fence, ExpectedVersion: committed.Version, ExpectedLeaseGeneration: committed.LeaseGeneration,
		TaskUID: "task-next", Attempt: 1, PromptID: "prompt-next", RequestDigest: testDigest("lease-next"), AcquiredAt: testNow.Add(2 * time.Minute),
		Lineage: testSessionLineageClaim(committed),
	}
	if _, err := kubeStore.AcquireSessionMutationLease(ctx, acquireNext); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("acquire with mismatched held Lease digest = %v, want conflict", err)
	}
	if err := rawClient.Get(ctx, client.ObjectKeyFromObject(sessionLease), sessionLease); err != nil {
		t.Fatal(err)
	}
	if sessionLease.Spec.HolderIdentity == nil || *sessionLease.Spec.HolderIdentity == "" {
		t.Fatal("mismatched held Lease was cleared")
	}
	sessionLease.Annotations[annotationRequestDigest] = originalRequestDigest
	if err := rawClient.Update(ctx, sessionLease); err != nil {
		t.Fatal(err)
	}
	if err := rawClient.Get(ctx, client.ObjectKeyFromObject(sessionLease), sessionLease); err != nil {
		t.Fatal(err)
	}
	mismatchedHolder := "mutation:mismatched-pre-prompt-holder"
	sessionLease.Spec.HolderIdentity = &mismatchedHolder
	if err := rawClient.Update(ctx, sessionLease); err != nil {
		t.Fatal(err)
	}
	if _, err := kubeStore.AcquireSessionMutationLease(ctx, acquireNext); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("acquire with mismatched held Lease holder = %v, want conflict", err)
	}
	if err := rawClient.Get(ctx, client.ObjectKeyFromObject(sessionLease), sessionLease); err != nil {
		t.Fatal(err)
	}
	if sessionLease.Spec.HolderIdentity == nil || *sessionLease.Spec.HolderIdentity != mismatchedHolder {
		t.Fatal("mismatched pre-prompt Session Lease holder was cleared or changed")
	}
	sessionLease.Spec.HolderIdentity = &originalHolder
	if err := rawClient.Update(ctx, sessionLease); err != nil {
		t.Fatal(err)
	}
	if _, err := kubeStore.AcquireSessionMutationLease(ctx, acquireNext); err != nil {
		t.Fatalf("acquire after release crash: %v", err)
	}
}

func TestControlRecordsDoNotInstallUnmanagedFinalizers(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	control, err := kubeStore.CreateSessionControl(ctx, &controlstore.SessionControl{
		Namespace: "tenant-a", SessionName: "deletable", SessionUID: "deletable-uid",
		RequestDigest: testDigest("deletable-session"), Availability: controlstore.SessionAvailable,
	}, fence)
	if err != nil {
		t.Fatal(err)
	}
	var sessions corev1alpha1.RuntimeSessionControlList
	if err := kubeClient.List(ctx, &sessions, client.InNamespace(control.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(sessions.Items) != 1 || len(sessions.Items[0].Finalizers) != 0 {
		t.Fatalf("SessionControl finalizers = %#v", sessions.Items)
	}
	var epochs corev1alpha1.ControllerEpochList
	if err := kubeClient.List(ctx, &epochs, client.InNamespace(testControlNamespace)); err != nil {
		t.Fatal(err)
	}
	if len(epochs.Items) != 1 || len(epochs.Items[0].Finalizers) != 0 {
		t.Fatalf("ControllerEpoch finalizers = %#v", epochs.Items)
	}
}
