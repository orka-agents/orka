package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
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

const (
	testControlNamespace   = "orka-system"
	testControllerFresh    = "controller-fresh"
	testControllerRestored = "controller-restored"
	testControllerFirst    = "controller-first"
	testControllerSecond   = "controller-second"
	testStatusSubresource  = "status"
)

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
	if record.Annotations[annotationControllerEpochLeaseUID] != string(lease.UID) {
		t.Fatalf("epoch mirror Lease UID binding = %q, want %q", record.Annotations[annotationControllerEpochLeaseUID], lease.UID)
	}
	if predecessorDigest := lease.Annotations[annotationControllerEpochPredecessorDigest]; predecessorDigest != "" {
		t.Fatalf("initial epoch Lease predecessor digest = %q, want empty", predecessorDigest)
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
	if err := kubeClient.Get(ctx, leaseKey, &lease); err != nil {
		t.Fatalf("get advanced epoch Lease: %v", err)
	}
	if err := kubeClient.Get(ctx, recordKey, &record); err != nil {
		t.Fatalf("get advanced epoch record: %v", err)
	}
	predecessorDigest, err := controllerEpochPredecessorDigest(&lease)
	if err != nil {
		t.Fatalf("parse advanced epoch predecessor digest: %v", err)
	}
	if predecessorDigest == "" {
		t.Fatal("advanced epoch Lease omitted authenticated predecessor digest")
	}
	if record.Annotations[annotationControllerEpochLeaseUID] != string(lease.UID) || record.Status.Epoch != 2 || record.Status.Version != 2 {
		t.Fatalf("advanced epoch mirror = annotations %#v status %#v Lease UID %q", record.Annotations, record.Status, lease.UID)
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

func TestControllerEpochFenceReadToleratesLeaseMutationResourceVersionChurn(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, want := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	before := &corev1alpha1.ControllerEpoch{}
	if err := kubeClient.Get(ctx, objectKey, before); err != nil {
		t.Fatalf("get controller epoch mirror before fence read: %v", err)
	}
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get controller epoch Lease before mutation churn: %v", err)
	}
	lease.Annotations[annotationMutationToken] = "concurrent-control-store-mutation"
	lease.Annotations[annotationMutationExpiresAt] = formatTime(time.Now().UTC().Add(time.Minute))
	if err := kubeClient.Update(ctx, lease); err != nil {
		t.Fatalf("change controller epoch Lease resourceVersion: %v", err)
	}

	got, err := kubeStore.GetControllerEpochFence(ctx, name)
	if err != nil {
		t.Fatalf("read controller epoch fence during mutation churn: %v", err)
	}
	if got != want {
		t.Fatalf("controller epoch fence = %#v, want %#v", got, want)
	}
	after := &corev1alpha1.ControllerEpoch{}
	if err := kubeClient.Get(ctx, objectKey, after); err != nil {
		t.Fatalf("get controller epoch mirror after fence read: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("read-only fence lookup changed mirror resourceVersion from %q to %q", before.ResourceVersion, after.ResourceVersion)
	}
}

func TestControllerEpochFenceReadRejectsConcurrentAuthorityChange(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, _ := newTestStoreWithEpoch(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	name := controlstore.DefaultControllerEpochName
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	var leaseReads atomic.Int64
	kubeStore.reader = interceptor.NewClient(withWatch, interceptor.Funcs{
		Get: func(ctx context.Context, delegate client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
			if _, isLease := object.(*coordinationv1.Lease); !isLease || key != leaseKey {
				return delegate.Get(ctx, key, object, options...)
			}
			if leaseReads.Add(1) == 2 {
				latest := &coordinationv1.Lease{}
				if err := delegate.Get(ctx, key, latest, options...); err != nil {
					return err
				}
				setControllerEpochLease(latest, controlstore.ControllerEpochCAS{
					Name: name, NewEpoch: 2, HolderID: testControllerSecond,
					RequestDigest: testDigest("concurrent-fence-read"), UpdatedAt: testNow.Add(time.Minute),
				}, 2, "")
				if err := delegate.Update(ctx, latest); err != nil {
					return err
				}
			}
			return delegate.Get(ctx, key, object, options...)
		},
	})

	if _, err := kubeStore.GetControllerEpochFence(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "changed during fence read") {
		t.Fatalf("concurrent controller epoch fence read error = %v, want authority-change conflict", err)
	}
	if got := leaseReads.Load(); got != 2 {
		t.Fatalf("controller epoch fence Lease reads = %d, want 2", got)
	}
}

func TestControllerEpochAuthorityRejectsRecreatedLeaseIncarnation(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	lease := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get original controller epoch Lease: %v", err)
	}
	originalUID := lease.UID
	originalResourceVersion := lease.ResourceVersion
	replacement := lease.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = types.UID("replacement-controller-epoch-lease-uid")
	replacement.CreationTimestamp = metav1.Time{}
	replacement.ManagedFields = nil
	if err := kubeClient.Delete(ctx, lease); err != nil {
		t.Fatalf("delete original controller epoch Lease: %v", err)
	}
	if err := kubeClient.Create(ctx, replacement); err != nil {
		t.Fatalf("recreate controller epoch Lease under a new UID: %v", err)
	}
	if replacement.UID == originalUID {
		t.Fatalf("replacement Lease reused original UID %q", originalUID)
	}
	if replacement.ResourceVersion != originalResourceVersion {
		t.Fatalf("fake replacement Lease resourceVersion = %q, want original %q to prove UID is decisive", replacement.ResourceVersion, originalResourceVersion)
	}

	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	before := &corev1alpha1.ControllerEpoch{}
	if err := kubeClient.Get(ctx, objectKey, before); err != nil {
		t.Fatalf("get bound controller epoch mirror: %v", err)
	}
	if before.Annotations[annotationControllerEpochLeaseUID] != string(originalUID) {
		t.Fatalf("mirror binding = %q, want original UID %q", before.Annotations[annotationControllerEpochLeaseUID], originalUID)
	}
	if _, err := kubeStore.GetControllerEpoch(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "bound to Lease UID") {
		t.Fatalf("recreated Lease read error = %v, want UID conflict", err)
	}
	if _, err := kubeStore.GetControllerEpochFence(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "bound to Lease UID") {
		t.Fatalf("recreated Lease fence read error = %v, want UID conflict", err)
	}
	if _, _, err := kubeStore.requireControllerEpoch(ctx, fence); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "bound to Lease UID") {
		t.Fatalf("recreated Lease mutation-fence error = %v, want UID conflict", err)
	}
	key := controlstore.PromptAttemptKey{
		Namespace: "tenant-a", TaskUID: "task-recreated-epoch-lease", Attempt: 1, PromptID: "prompt-recreated-epoch-lease",
	}
	ensureActiveAgentTask(t, ctx, kubeClient, key.Namespace, key.TaskUID, key.TaskUID)
	if _, err := kubeStore.CreatePromptAttempt(ctx, boundPromptAttemptForKubeTest(&controlstore.PromptAttempt{
		Key: key, RequestDigest: testDigest("recreated-epoch-lease"),
	}), fence); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "bound to Lease UID") {
		t.Fatalf("recreated Lease PromptAttempt mutation error = %v, want UID conflict", err)
	}
	var attempts corev1alpha1.PromptAttemptList
	if err := kubeClient.List(ctx, &attempts, client.InNamespace(key.Namespace)); err != nil {
		t.Fatalf("list PromptAttempts after rejected recreated Lease: %v", err)
	}
	if len(attempts.Items) != 0 {
		t.Fatalf("recreated Lease authorized PromptAttempt writes: %#v", attempts.Items)
	}
	currentLease := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, currentLease); err != nil {
		t.Fatalf("get rejected replacement controller epoch Lease: %v", err)
	}
	if currentLease.ResourceVersion != replacement.ResourceVersion {
		t.Fatalf("rejected replacement Lease resourceVersion changed from %q to %q", replacement.ResourceVersion, currentLease.ResourceVersion)
	}
	if token := currentLease.Annotations[annotationMutationToken]; token != "" {
		t.Fatalf("recreated Lease gained a mutation token: %q", token)
	}
	if expiresAt := currentLease.Annotations[annotationMutationExpiresAt]; expiresAt != "" {
		t.Fatalf("recreated Lease gained a mutation expiry: %q", expiresAt)
	}
	after := &corev1alpha1.ControllerEpoch{}
	if err := kubeClient.Get(ctx, objectKey, after); err != nil {
		t.Fatalf("get mirror after rejected Lease recreation: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion || after.Annotations[annotationControllerEpochLeaseUID] != string(originalUID) {
		t.Fatalf("rejected Lease recreation changed mirror: before=%#v after=%#v", before.ObjectMeta, after.ObjectMeta)
	}
}

func TestControllerEpochAuthorityRejectsRecreatedLeaseWhenMirrorBindingIsMissing(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, _ := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	lease := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get original controller epoch Lease: %v", err)
	}
	originalResourceVersion := lease.ResourceVersion
	replacement := lease.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = types.UID("replacement-unbound-controller-epoch-lease-uid")
	replacement.CreationTimestamp = metav1.Time{}
	replacement.ManagedFields = nil

	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	object := &corev1alpha1.ControllerEpoch{}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get bound controller epoch mirror: %v", err)
	}
	delete(object.Annotations, annotationControllerEpochLeaseUID)
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("remove controller epoch Lease UID binding: %v", err)
	}
	beforeResourceVersion := object.ResourceVersion

	if err := kubeClient.Delete(ctx, lease); err != nil {
		t.Fatalf("delete original controller epoch Lease: %v", err)
	}
	if err := kubeClient.Create(ctx, replacement); err != nil {
		t.Fatalf("recreate unbound controller epoch Lease under a new UID: %v", err)
	}
	if replacement.ResourceVersion != originalResourceVersion {
		t.Fatalf("fake replacement Lease resourceVersion = %q, want original %q to prove UID is decisive", replacement.ResourceVersion, originalResourceVersion)
	}

	if _, err := kubeStore.GetControllerEpoch(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "has no immutable authoritative Lease UID binding") {
		t.Fatalf("unbound recreated Lease read error = %v, want fail-closed conflict", err)
	}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get mirror after rejected unbound Lease recreation: %v", err)
	}
	if object.ResourceVersion != beforeResourceVersion {
		t.Fatalf("rejected unbound Lease recreation changed mirror resourceVersion from %q to %q", beforeResourceVersion, object.ResourceVersion)
	}
	if _, present := object.Annotations[annotationControllerEpochLeaseUID]; present {
		t.Fatalf("rejected unbound Lease recreation gained a UID binding: %#v", object.Annotations)
	}
}

func TestControllerEpochAuthorityRejectsMissingLeaseUIDBinding(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	leaseBefore := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, leaseBefore); err != nil {
		t.Fatalf("get controller epoch Lease before missing-binding rejection: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get controller epoch mirror: %v", err)
	}
	delete(object.Annotations, annotationControllerEpochLeaseUID)
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("simulate pre-UID-binding controller epoch mirror: %v", err)
	}
	beforeResourceVersion := object.ResourceVersion

	if _, err := kubeStore.GetControllerEpoch(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "has no immutable authoritative Lease UID binding") {
		t.Fatalf("missing Lease UID binding error = %v, want fail-closed conflict", err)
	}
	if _, _, err := kubeStore.requireControllerEpoch(ctx, fence); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "has no immutable authoritative Lease UID binding") {
		t.Fatalf("missing Lease UID mutation-fence error = %v, want fail-closed conflict", err)
	}
	leaseAfter := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, leaseAfter); err != nil {
		t.Fatalf("get controller epoch Lease after missing-binding rejection: %v", err)
	}
	if leaseAfter.ResourceVersion != leaseBefore.ResourceVersion || leaseAfter.Annotations[annotationMutationToken] != "" || leaseAfter.Annotations[annotationMutationExpiresAt] != "" {
		t.Fatalf("missing binding changed controller epoch Lease: before=%#v after=%#v", leaseBefore.ObjectMeta, leaseAfter.ObjectMeta)
	}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get rejected unbound controller epoch mirror: %v", err)
	}
	if object.ResourceVersion != beforeResourceVersion {
		t.Fatalf("rejected unbound mirror changed resourceVersion from %q to %q", beforeResourceVersion, object.ResourceVersion)
	}
	if _, present := object.Annotations[annotationControllerEpochLeaseUID]; present {
		t.Fatalf("rejected unbound mirror gained a Lease UID binding: %#v", object.Annotations)
	}
}

func TestControllerEpochAuthorityRejectsLegacyUIDBackfillDuringActiveMutation(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, _ := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get controller epoch mirror: %v", err)
	}
	delete(object.Annotations, annotationControllerEpochLeaseUID)
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("remove legacy Lease UID binding: %v", err)
	}
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get controller epoch Lease: %v", err)
	}
	lease.Annotations[annotationMutationToken] = "active-upgrade-mutation"
	lease.Annotations[annotationMutationExpiresAt] = formatTime(time.Now().UTC().Add(time.Minute))
	if err := kubeClient.Update(ctx, lease); err != nil {
		t.Fatalf("mark active controller epoch mutation: %v", err)
	}

	if _, err := kubeStore.GetControllerEpoch(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "has no immutable authoritative Lease UID binding") {
		t.Fatalf("active-mutation missing-binding error = %v, want conflict", err)
	}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get mirror after rejected UID backfill: %v", err)
	}
	if _, present := object.Annotations[annotationControllerEpochLeaseUID]; present {
		t.Fatalf("active mutation gained a Lease UID binding: %#v", object.Annotations)
	}
}

func TestControllerEpochAuthorityRejectsWrongLeaseUIDBinding(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	leaseBefore := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, leaseBefore); err != nil {
		t.Fatalf("get controller epoch Lease before wrong-binding rejection: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get controller epoch mirror: %v", err)
	}
	object.Annotations[annotationControllerEpochLeaseUID] = "wrong-controller-epoch-lease-uid"
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("forge controller epoch Lease UID binding: %v", err)
	}
	if _, err := kubeStore.GetControllerEpoch(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "bound to Lease UID") {
		t.Fatalf("wrong Lease UID binding error = %v, want conflict", err)
	}
	if _, _, err := kubeStore.requireControllerEpoch(ctx, fence); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "bound to Lease UID") {
		t.Fatalf("wrong Lease UID mutation-fence error = %v, want conflict", err)
	}
	leaseAfter := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, leaseAfter); err != nil {
		t.Fatalf("get controller epoch Lease after wrong-binding rejection: %v", err)
	}
	if leaseAfter.ResourceVersion != leaseBefore.ResourceVersion || leaseAfter.Annotations[annotationMutationToken] != "" || leaseAfter.Annotations[annotationMutationExpiresAt] != "" {
		t.Fatalf("wrong binding changed controller epoch Lease: before=%#v after=%#v", leaseBefore.ObjectMeta, leaseAfter.ObjectMeta)
	}
}

func TestControllerEpochStatusSyncExactMirrorIsWriteFree(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, _ := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := rawClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get authoritative controller epoch Lease: %v", err)
	}
	epoch, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse authoritative controller epoch Lease: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get exact controller epoch mirror: %v", err)
	}
	beforeResourceVersion := object.ResourceVersion

	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	var statusWrites atomic.Int32
	var objectWrites atomic.Int32
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); isEpochObject {
				statusWrites.Add(1)
			}
			return delegate.SubResource(subresource).Update(ctx, object, options...)
		},
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); isEpochObject {
				objectWrites.Add(1)
			}
			return delegate.Update(ctx, object, options...)
		},
	})

	if err := kubeStore.syncControllerEpochStatus(ctx, epoch, lease, ""); err != nil {
		t.Fatalf("sync exact controller epoch mirror: %v", err)
	}
	if got := statusWrites.Load(); got != 0 {
		t.Fatalf("exact controller epoch mirror triggered %d status write(s)", got)
	}
	if got := objectWrites.Load(); got != 0 {
		t.Fatalf("exact controller epoch mirror triggered %d object write(s)", got)
	}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get controller epoch mirror after exact sync: %v", err)
	}
	if object.ResourceVersion != beforeResourceVersion {
		t.Fatalf("exact controller epoch mirror resourceVersion changed from %q to %q", beforeResourceVersion, object.ResourceVersion)
	}
}

func TestControllerEpochStatusSyncRejectsChangedLeaseSnapshotWithoutWriting(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, _ := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := rawClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get authoritative controller epoch Lease: %v", err)
	}
	epoch, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse authoritative controller epoch Lease: %v", err)
	}
	staleLeaseSnapshot := lease.DeepCopy()
	staleLeaseResourceVersion := lease.ResourceVersion
	lease.Annotations["core.orka.ai/test-lease-revision"] = "changed"
	if err := rawClient.Update(ctx, lease); err != nil {
		t.Fatalf("change controller epoch Lease revision: %v", err)
	}
	if lease.ResourceVersion == staleLeaseResourceVersion {
		t.Fatal("controller epoch Lease revision did not change")
	}

	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get controller epoch mirror before stale sync: %v", err)
	}
	beforeResourceVersion := object.ResourceVersion
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	var writes atomic.Int32
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); isEpochObject {
				writes.Add(1)
			}
			return delegate.SubResource(subresource).Update(ctx, object, options...)
		},
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); isEpochObject {
				writes.Add(1)
			}
			return delegate.Update(ctx, object, options...)
		},
	})

	err = kubeStore.syncControllerEpochStatus(ctx, epoch, staleLeaseSnapshot, "")
	if !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "changed from UID/resourceVersion") {
		t.Fatalf("changed Lease snapshot sync error = %v, want resourceVersion conflict", err)
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("changed Lease snapshot triggered %d controller epoch write(s)", got)
	}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get controller epoch mirror after stale sync: %v", err)
	}
	if object.ResourceVersion != beforeResourceVersion || object.Status.Epoch != epoch.Epoch || object.Status.Version != epoch.Version {
		t.Fatalf("changed Lease snapshot changed controller epoch mirror: %#v", object.Status)
	}
}

func TestControllerEpochStatusSyncRejectsNonPredecessorMirrorWithoutWriting(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, _ := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := rawClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get epoch-1 authoritative Lease: %v", err)
	}
	predecessorDigest := testDigest("restored-epoch-3-predecessor")
	setControllerEpochLease(lease, controlstore.ControllerEpochCAS{
		Name: name, NewEpoch: 3, HolderID: "controller-three",
		RequestDigest: testDigest("epoch-3-restore"), UpdatedAt: testNow.Add(2 * time.Minute),
	}, 3, predecessorDigest)
	if err := rawClient.Update(ctx, lease); err != nil {
		t.Fatalf("restore epoch-3 authoritative Lease over epoch-1 mirror: %v", err)
	}
	target, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse restored epoch-3 authoritative Lease: %v", err)
	}

	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get epoch-1 controller epoch mirror: %v", err)
	}
	beforeResourceVersion := object.ResourceVersion
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	var writes atomic.Int32
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); isEpochObject {
				writes.Add(1)
			}
			return delegate.SubResource(subresource).Update(ctx, object, options...)
		},
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); isEpochObject {
				writes.Add(1)
			}
			return delegate.Update(ctx, object, options...)
		},
	})

	err = kubeStore.syncControllerEpochStatus(ctx, target, lease, predecessorDigest)
	if !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "not the exact predecessor") {
		t.Fatalf("non-predecessor mirror sync error = %v, want exact-predecessor conflict", err)
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("non-predecessor mirror triggered %d controller epoch write(s)", got)
	}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get controller epoch mirror after rejected jump: %v", err)
	}
	if object.ResourceVersion != beforeResourceVersion || object.Status.Epoch != 1 || object.Status.Version != 1 {
		t.Fatalf("rejected epoch jump changed controller epoch mirror: %#v", object.Status)
	}
}

func TestControllerEpochDelayedEpochOneStatusRetryCannotRollbackEpochTwo(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient := newTestStore(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	name := controlstore.DefaultControllerEpochName
	firstStatusBlocked := make(chan struct{})
	releaseFirstStatus := make(chan struct{})
	var statusCalls atomic.Int32
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); subresource == testStatusSubresource && isEpochObject && statusCalls.Add(1) == 1 {
				close(firstStatusBlocked)
				select {
				case <-releaseFirstStatus:
				case <-ctx.Done():
					return ctx.Err()
				}
				return apierrors.NewConflict(
					schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "controllerepochs"},
					object.GetName(), errors.New("simulated delayed epoch-1 status conflict"),
				)
			}
			return delegate.SubResource(subresource).Update(ctx, object, options...)
		},
	})

	type casResult struct {
		epoch *controlstore.ControllerEpoch
		err   error
	}
	firstResult := make(chan casResult, 1)
	go func() {
		epoch, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
			Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
			HolderID: testControllerFirst, RequestDigest: testDigest("delayed-epoch-1"), UpdatedAt: testNow,
		})
		firstResult <- casResult{epoch: epoch, err: err}
	}()

	select {
	case <-firstStatusBlocked:
	case <-time.After(2 * time.Second):
		t.Fatal("epoch-1 status sync did not reach deterministic delay")
	}
	advanced, advanceErr := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: testControllerSecond, RequestDigest: testDigest("delayed-epoch-2"), UpdatedAt: testNow.Add(time.Minute),
	})
	close(releaseFirstStatus)

	var delayed casResult
	select {
	case delayed = <-firstResult:
	case <-time.After(2 * time.Second):
		t.Fatal("delayed epoch-1 status sync did not finish")
	}
	if advanceErr != nil {
		t.Fatalf("advance to epoch 2 while epoch-1 status write is delayed: %v", advanceErr)
	}
	if advanced.Epoch != 2 || advanced.Version != 2 || advanced.HolderID != testControllerSecond {
		t.Fatalf("advanced controller epoch = %#v", advanced)
	}
	if !errors.Is(delayed.err, controlstore.ErrConflict) {
		t.Fatalf("delayed epoch-1 retry error = %v, want conflict after epoch 2 committed", delayed.err)
	}
	if delayed.epoch != nil {
		t.Fatalf("delayed epoch-1 retry returned an epoch after conflict: %#v", delayed.epoch)
	}

	lease := &coordinationv1.Lease{}
	if err := rawClient.Get(ctx, client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}, lease); err != nil {
		t.Fatalf("get final authoritative controller epoch Lease: %v", err)
	}
	finalEpoch, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse final authoritative controller epoch Lease: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	if err := rawClient.Get(ctx, client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}, object); err != nil {
		t.Fatalf("get final controller epoch mirror: %v", err)
	}
	if finalEpoch.Epoch != 2 || finalEpoch.Version != 2 || object.Status.Epoch != 2 || object.Status.Version != 2 ||
		object.Status.HolderID != testControllerSecond || object.Status.RequestDigest != finalEpoch.RequestDigest {
		t.Fatalf("delayed epoch-1 retry rolled back epoch 2: Lease=%#v mirror=%#v", finalEpoch, object.Status)
	}
	if controllerEpochInitializationMarkerPresent(object) {
		t.Fatalf("final epoch-2 mirror retained initialization marker: %#v", object.Annotations)
	}
}

func TestControllerEpochLeaseCASRepairsAuthenticatedPostCASTailOnRestart(t *testing.T) {
	ctx := context.Background()
	rawClient, change := leaveAuthenticatedControllerEpochPostCASTail(t)
	restartedStore, err := New(rawClient, testControlNamespace)
	if err != nil {
		t.Fatalf("create restarted Kubernetes store: %v", err)
	}
	current, err := restartedStore.GetControllerEpoch(ctx, change.Name)
	if err != nil {
		t.Fatalf("repair authenticated post-CAS mirror tail: %v", err)
	}
	if current.Epoch != change.NewEpoch || current.Version != change.NewEpoch || current.HolderID != change.HolderID || current.RequestDigest != change.RequestDigest {
		t.Fatalf("repaired controller epoch = %#v, want target %#v", current, change)
	}
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(change.Name)}
	if err := rawClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get repaired authoritative Lease: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(change.Name)}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get repaired controller epoch mirror: %v", err)
	}
	if object.Status.Epoch != change.NewEpoch || object.Status.Version != change.NewEpoch ||
		object.Status.LeaseResourceVersion != lease.ResourceVersion ||
		object.Annotations[annotationControllerEpochLeaseUID] != string(lease.UID) {
		t.Fatalf("repaired mirror = annotations %#v status %#v Lease=%#v", object.Annotations, object.Status, lease.ObjectMeta)
	}
	retry, err := restartedStore.CompareAndSwapControllerEpoch(ctx, change)
	if err != nil {
		t.Fatalf("retry committed post-CAS target: %v", err)
	}
	if retry.Epoch != change.NewEpoch || retry.Version != change.NewEpoch {
		t.Fatalf("post-CAS target retry = %#v", retry)
	}
}

func TestControllerEpochLeaseCASRejectsConcurrentPredecessorMutation(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, _ := newTestStoreWithEpoch(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	name := controlstore.DefaultControllerEpochName
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	mutated := false
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			lease, isLease := object.(*coordinationv1.Lease)
			if !isLease || lease.Annotations[annotationControllerEpoch] != "2" || mutated {
				return delegate.Update(ctx, object, options...)
			}
			if err := delegate.Update(ctx, object, options...); err != nil {
				return err
			}
			mirror := &corev1alpha1.ControllerEpoch{}
			if err := delegate.Get(ctx, objectKey, mirror); err != nil {
				return err
			}
			mirror.Status.RequestDigest = testDigest("forged-concurrent-predecessor")
			if err := delegate.Status().Update(ctx, mirror); err != nil {
				return err
			}
			mutated = true
			return nil
		},
	})
	change := controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: testControllerSecond, RequestDigest: testDigest("concurrent-predecessor-epoch-2"), UpdatedAt: testNow.Add(time.Minute),
	}
	if _, err := kubeStore.CompareAndSwapControllerEpoch(ctx, change); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "predecessor mirror digest does not match") {
		t.Fatalf("concurrent predecessor mutation CAS error = %v, want digest conflict", err)
	}
	if !mutated {
		t.Fatal("concurrent predecessor mutation hook did not run")
	}
	lease := &coordinationv1.Lease{}
	if err := rawClient.Get(ctx, client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}, lease); err != nil {
		t.Fatalf("get committed epoch-2 Lease after rejected mirror sync: %v", err)
	}
	committed, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse committed epoch-2 Lease: %v", err)
	}
	if committed.Epoch != 2 || committed.Version != 2 {
		t.Fatalf("authoritative Lease did not commit epoch 2: %#v", committed)
	}
	restartedStore, err := New(rawClient, testControlNamespace)
	if err != nil {
		t.Fatalf("create restarted Kubernetes store: %v", err)
	}
	if _, err := restartedStore.GetControllerEpoch(ctx, name); !errors.Is(err, controlstore.ErrConflict) || !strings.Contains(err.Error(), "predecessor mirror digest does not match") {
		t.Fatalf("restart accepted forged predecessor mirror: %v", err)
	}
}

func TestControllerEpochAuthorityRejectsCorruptPredecessorTimestampsWithoutPanic(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1alpha1.ControllerEpochStatus)
	}{
		{name: "missing acquiredAt", mutate: func(status *corev1alpha1.ControllerEpochStatus) { status.AcquiredAt = nil }},
		{name: "missing updatedAt", mutate: func(status *corev1alpha1.ControllerEpochStatus) { status.UpdatedAt = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			rawClient, change := leaveAuthenticatedControllerEpochPostCASTail(t)
			object := &corev1alpha1.ControllerEpoch{}
			objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(change.Name)}
			if err := rawClient.Get(ctx, objectKey, object); err != nil {
				t.Fatalf("get authenticated predecessor mirror: %v", err)
			}
			test.mutate(&object.Status)
			if err := rawClient.Status().Update(ctx, object); err != nil {
				t.Fatalf("corrupt predecessor timestamp: %v", err)
			}
			restartedStore, err := New(rawClient, testControlNamespace)
			if err != nil {
				t.Fatalf("create restarted Kubernetes store: %v", err)
			}
			if _, err := restartedStore.GetControllerEpoch(ctx, change.Name); err == nil || !strings.Contains(err.Error(), "predecessor mirror has incomplete timestamps") {
				t.Fatalf("corrupt predecessor timestamp error = %v, want fail-closed validation", err)
			}
		})
	}
}

func leaveAuthenticatedControllerEpochPostCASTail(t *testing.T) (client.Client, controlstore.ControllerEpochCAS) {
	t.Helper()
	ctx := context.Background()
	kubeStore, rawClient, _ := newTestStoreWithEpoch(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	injectedErr := errors.New("injected post-CAS status sync failure")
	failTargetStatus := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			mirror, isEpochObject := object.(*corev1alpha1.ControllerEpoch)
			if subresource == testStatusSubresource && isEpochObject && mirror.Status.Epoch == 2 && failTargetStatus {
				failTargetStatus = false
				return injectedErr
			}
			return delegate.SubResource(subresource).Update(ctx, object, options...)
		},
	})
	change := controlstore.ControllerEpochCAS{
		Name: controlstore.DefaultControllerEpochName, ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: testControllerSecond, RequestDigest: testDigest("post-CAS-tail-epoch-2"), UpdatedAt: testNow.Add(time.Minute),
	}
	if _, err := kubeStore.CompareAndSwapControllerEpoch(ctx, change); !errors.Is(err, injectedErr) {
		t.Fatalf("post-CAS status sync error = %v, want injected failure", err)
	}
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(change.Name)}
	if err := rawClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get committed post-CAS Lease: %v", err)
	}
	predecessorDigest, err := controllerEpochPredecessorDigest(lease)
	if err != nil {
		t.Fatalf("parse committed predecessor digest: %v", err)
	}
	if predecessorDigest == "" {
		t.Fatal("committed post-CAS Lease omitted predecessor digest")
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(change.Name)}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get post-CAS predecessor mirror: %v", err)
	}
	if object.Status.Epoch != 1 || object.Status.Version != 1 || object.Annotations[annotationControllerEpochLeaseUID] != string(lease.UID) {
		t.Fatalf("post-CAS tail = annotations %#v status %#v Lease UID %q", object.Annotations, object.Status, lease.UID)
	}
	return rawClient, change
}

func TestControllerEpochLeaseCASRejectsMissingLeaseWithBlankObject(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient := newTestStore(t)
	name := controlstore.DefaultControllerEpochName
	object := &corev1alpha1.ControllerEpoch{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testControlNamespace,
			Name:      controllerEpochObjectName(name),
		},
		Spec: corev1alpha1.ControllerEpochSpec{Name: name},
	}
	if err := kubeClient.Create(ctx, object); err != nil {
		t.Fatalf("create blank controller epoch object: %v", err)
	}

	_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: testControllerFresh, RequestDigest: testDigest("fresh-epoch-1"), UpdatedAt: testNow,
	})
	if err == nil || !strings.Contains(err.Error(), "authoritative Lease is missing") {
		t.Fatalf("blank pre-existing object CAS error = %v, want fail-closed authority error", err)
	}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := kubeClient.Get(ctx, leaseKey, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("missing Lease was created from a blank pre-existing object: %v", err)
	}
}

func TestControllerEpochLeaseCASRejectsRestoreWinningFreshObjectCreation(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient := newTestStore(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	name := controlstore.DefaultControllerEpochName
	restoreApplied := false
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
			epochObject, isEpochObject := object.(*corev1alpha1.ControllerEpoch)
			if !isEpochObject || restoreApplied {
				return delegate.Create(ctx, object, options...)
			}
			restored := epochObject.DeepCopy()
			if err := delegate.Create(ctx, restored, options...); err != nil {
				return err
			}
			restoredAt := metav1.NewTime(testNow.Add(7 * time.Hour))
			restored.Status = corev1alpha1.ControllerEpochStatus{
				Epoch: 7, Version: 7, HolderID: testControllerRestored, RequestDigest: testDigest("restored-epoch-7"),
				AcquiredAt: &restoredAt, UpdatedAt: &restoredAt,
				LeaseName: controllerEpochLeaseName(name), LeaseResourceVersion: "restored-lease-rv",
			}
			if err := delegate.Status().Update(ctx, restored); err != nil {
				return err
			}
			restoreApplied = true
			return apierrors.NewAlreadyExists(
				schema.GroupResource{Group: corev1alpha1.GroupVersion.Group, Resource: "controllerepochs"},
				restored.Name,
			)
		},
	})

	_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: testControllerFresh, RequestDigest: testDigest("fresh-epoch-1"), UpdatedAt: testNow,
	})
	if err == nil || !strings.Contains(err.Error(), "authoritative Lease is missing") {
		t.Fatalf("restore-winning CAS error = %v, want fail-closed authority error", err)
	}
	if !restoreApplied {
		t.Fatal("restore race was not exercised")
	}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := rawClient.Get(ctx, leaseKey, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("epoch-1 Lease was created after restore won object creation: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get restore-winning ControllerEpoch object: %v", err)
	}
	if object.Status.Epoch != 7 || object.Status.Version != 7 || object.Status.HolderID != testControllerRestored {
		t.Fatalf("restore-winning status changed: %#v", object.Status)
	}
}

func TestControllerEpochLeaseCASCrashTailAfterFreshObjectCreationFailsClosed(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient := newTestStore(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	name := controlstore.DefaultControllerEpochName
	injectedErr := errors.New("injected Lease create failure")
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if lease, isLease := object.(*coordinationv1.Lease); isLease && lease.Name == controllerEpochLeaseName(name) {
				return injectedErr
			}
			return delegate.Create(ctx, object, options...)
		},
	})
	_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: testControllerFresh, RequestDigest: testDigest("fresh-epoch-1"), UpdatedAt: testNow,
	})
	if !errors.Is(err, injectedErr) {
		t.Fatalf("initial CAS error = %v, want injected Lease create failure", err)
	}

	restartedStore, err := New(rawClient, testControlNamespace)
	if err != nil {
		t.Fatalf("create restarted Kubernetes store: %v", err)
	}
	_, err = restartedStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: "controller-restarted", RequestDigest: testDigest("restart-epoch-1"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err == nil || !strings.Contains(err.Error(), "authoritative Lease is missing") {
		t.Fatalf("crash-tail retry error = %v, want fail-closed authority error", err)
	}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := rawClient.Get(ctx, leaseKey, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("crash-tail retry created epoch-1 Lease: %v", err)
	}
}

func TestControllerEpochLeaseCASRepairsMarkedEpochOneCrashTailBeforeAdvance(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient := newTestStore(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	name := controlstore.DefaultControllerEpochName
	injectedErr := errors.New("injected status mirror failure")
	failStatus := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); subresource == testStatusSubresource && isEpochObject && failStatus {
				failStatus = false
				return injectedErr
			}
			return delegate.SubResource(subresource).Update(ctx, object, options...)
		},
	})
	_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: testControllerFirst, RequestDigest: testDigest("marked-epoch-1"), UpdatedAt: testNow,
	})
	if !errors.Is(err, injectedErr) {
		t.Fatalf("initial CAS error = %v, want injected mirror failure", err)
	}

	restartedStore, err := New(rawClient, testControlNamespace)
	if err != nil {
		t.Fatalf("create restarted Kubernetes store: %v", err)
	}
	current, err := restartedStore.GetControllerEpoch(ctx, name)
	if err != nil {
		t.Fatalf("read marked epoch-1 crash tail: %v", err)
	}
	if current.Epoch != 1 || current.Version != 1 || current.HolderID != testControllerFirst {
		t.Fatalf("marked crash-tail epoch = %#v", current)
	}
	advanced, err := restartedStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: testControllerSecond, RequestDigest: testDigest("marked-epoch-2"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("advance marked crash tail: %v", err)
	}
	if advanced.Epoch != 2 || advanced.Version != 2 || advanced.HolderID != testControllerSecond {
		t.Fatalf("advanced marked crash-tail epoch = %#v", advanced)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := rawClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get repaired ControllerEpoch object: %v", err)
	}
	if object.Status.Epoch != 2 || object.Status.Version != 2 || object.Status.HolderID != testControllerSecond {
		t.Fatalf("repaired ControllerEpoch status = %#v", object.Status)
	}
	if controllerEpochInitializationMarkerPresent(object) {
		t.Fatalf("repaired ControllerEpoch retained reusable initialization marker: %#v", object.Annotations)
	}
}

func TestControllerEpochLeaseCASConsumesInitializationMarkerAfterSuccessfulMirror(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient := newTestStore(t)
	name := controlstore.DefaultControllerEpochName
	initial, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: testControllerFirst, RequestDigest: testDigest("consumed-epoch-1"), UpdatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("create initial epoch: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get committed ControllerEpoch object: %v", err)
	}
	if controllerEpochInitializationMarkerPresent(object) {
		t.Fatalf("committed ControllerEpoch retained reusable initialization marker: %#v", object.Annotations)
	}
	object.Status = corev1alpha1.ControllerEpochStatus{}
	if err := kubeClient.Status().Update(ctx, object); err != nil {
		t.Fatalf("blank committed ControllerEpoch status: %v", err)
	}
	if _, err := kubeStore.GetControllerEpoch(ctx, name); err == nil || !strings.Contains(err.Error(), "blank status without an exact epoch-1 initialization marker") {
		t.Fatalf("lost committed epoch-1 mirror error = %v, want fail-closed marker error", err)
	}
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get preserved authoritative Lease: %v", err)
	}
	parsed, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse preserved authoritative Lease: %v", err)
	}
	if parsed.Epoch != initial.Epoch || parsed.Version != initial.Version || parsed.HolderID != initial.HolderID {
		t.Fatalf("lost mirror changed authoritative Lease: got %#v, want %#v", parsed, initial)
	}
}

func TestControllerEpochLeaseCASRepairsInitializationMarkerCleanupCrashTail(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient := newTestStore(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	name := controlstore.DefaultControllerEpochName
	injectedErr := errors.New("injected initialization marker cleanup failure")
	failCleanup := true
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		Update: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.UpdateOption) error {
			if _, isEpochObject := object.(*corev1alpha1.ControllerEpoch); isEpochObject && failCleanup {
				failCleanup = false
				return injectedErr
			}
			return delegate.Update(ctx, object, options...)
		},
	})
	_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: testControllerFirst, RequestDigest: testDigest("marker-cleanup-epoch-1"), UpdatedAt: testNow,
	})
	if !errors.Is(err, injectedErr) {
		t.Fatalf("initial CAS error = %v, want injected marker cleanup failure", err)
	}
	partial := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := rawClient.Get(ctx, objectKey, partial); err != nil {
		t.Fatalf("get marker cleanup crash tail: %v", err)
	}
	if partial.Status.Epoch != 1 || partial.Status.Version != 1 || !controllerEpochInitializationMarkerPresent(partial) {
		t.Fatalf("marker cleanup crash tail = status %#v annotations %#v", partial.Status, partial.Annotations)
	}
	if _, bound := partial.Annotations[annotationControllerEpochLeaseUID]; bound {
		t.Fatalf("marker cleanup crash tail already bound a Lease UID: %#v", partial.Annotations)
	}

	restartedStore, err := New(rawClient, testControlNamespace)
	if err != nil {
		t.Fatalf("create restarted Kubernetes store: %v", err)
	}
	current, err := restartedStore.GetControllerEpoch(ctx, name)
	if err != nil {
		t.Fatalf("repair marker cleanup crash tail: %v", err)
	}
	if current.Epoch != 1 || current.Version != 1 || current.HolderID != testControllerFirst {
		t.Fatalf("repaired marker cleanup epoch = %#v", current)
	}
	repaired := &corev1alpha1.ControllerEpoch{}
	if err := rawClient.Get(ctx, objectKey, repaired); err != nil {
		t.Fatalf("get repaired marker cleanup object: %v", err)
	}
	if controllerEpochInitializationMarkerPresent(repaired) {
		t.Fatalf("repaired object retained initialization marker: %#v", repaired.Annotations)
	}
	lease := &coordinationv1.Lease{}
	if err := rawClient.Get(ctx, client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}, lease); err != nil {
		t.Fatalf("get authoritative Lease after marker cleanup repair: %v", err)
	}
	if repaired.Annotations[annotationControllerEpochLeaseUID] != string(lease.UID) {
		t.Fatalf("repaired object Lease UID binding = %q, want %q", repaired.Annotations[annotationControllerEpochLeaseUID], lease.UID)
	}
}

func TestControllerEpochAuthorityRejectsBlankMirrorAfterAdvance(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, _ := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	advanced, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: testControllerSecond, RequestDigest: testDigest("epoch-2"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("advance controller epoch: %v", err)
	}
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get advanced ControllerEpoch object: %v", err)
	}
	object.Status = corev1alpha1.ControllerEpochStatus{}
	if err := kubeClient.Status().Update(ctx, object); err != nil {
		t.Fatalf("blank advanced ControllerEpoch status: %v", err)
	}
	if _, err := kubeStore.GetControllerEpoch(ctx, name); err == nil || !strings.Contains(err.Error(), "blank mirror cannot consume") {
		t.Fatalf("blank advanced mirror read error = %v, want fail-closed marker error", err)
	}
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get preserved authoritative Lease: %v", err)
	}
	parsed, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse preserved authoritative Lease: %v", err)
	}
	if parsed.Epoch != advanced.Epoch || parsed.Version != advanced.Version || parsed.HolderID != advanced.HolderID {
		t.Fatalf("blank mirror changed authoritative Lease: got %#v, want %#v", parsed, advanced)
	}
}

func TestControllerEpochAuthorityRejectsForgedPredecessorAfterAdvance(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, _ := newTestStoreWithEpoch(t)
	name := controlstore.DefaultControllerEpochName
	object := &corev1alpha1.ControllerEpoch{}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get epoch-1 ControllerEpoch object: %v", err)
	}
	staleStatus := object.Status.DeepCopy()
	advanced, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2,
		HolderID: testControllerSecond, RequestDigest: testDigest("epoch-2"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("advance controller epoch: %v", err)
	}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get epoch-2 ControllerEpoch object: %v", err)
	}
	object.Status = *staleStatus
	object.Status.HolderID = "controller-forged-predecessor"
	if err := kubeClient.Status().Update(ctx, object); err != nil {
		t.Fatalf("restore forged predecessor ControllerEpoch status: %v", err)
	}
	if _, err := kubeStore.GetControllerEpoch(ctx, name); err == nil || !strings.Contains(err.Error(), "predecessor mirror digest does not match") {
		t.Fatalf("forged predecessor mirror read error = %v, want digest mismatch", err)
	}
	lease := &coordinationv1.Lease{}
	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get preserved authoritative Lease: %v", err)
	}
	parsed, err := controllerEpochFromLease(name, lease)
	if err != nil {
		t.Fatalf("parse preserved authoritative Lease: %v", err)
	}
	if parsed.Epoch != advanced.Epoch || parsed.Version != advanced.Version || parsed.HolderID != advanced.HolderID {
		t.Fatalf("stale mirror changed authoritative Lease: got %#v, want %#v", parsed, advanced)
	}
}

func TestControllerEpochAuthorityRejectsCorruptBlankMirrorInitializationMarker(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "missing holder", mutate: func(annotations map[string]string) {
			delete(annotations, annotationControllerEpochInitializationHolder)
		}},
		{name: "digest mismatch", mutate: func(annotations map[string]string) {
			annotations[annotationRequestDigest] = testDigest("other-initialization")
		}},
		{name: "Lease name mismatch", mutate: func(annotations map[string]string) {
			annotations[annotationControllerEpochInitializationLeaseName] = "other-lease"
		}},
		{name: "epoch mismatch", mutate: func(annotations map[string]string) { annotations[annotationControllerEpoch] = "2" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			kubeStore, rawClient := newTestStore(t)
			withWatch, ok := rawClient.(client.WithWatch)
			if !ok {
				t.Fatal("fake client does not implement client.WithWatch")
			}
			injectedErr := errors.New("injected status mirror failure")
			kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
				SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
					return injectedErr
				},
			})
			_, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
				Name: controlstore.DefaultControllerEpochName, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
				HolderID: testControllerFirst, RequestDigest: testDigest("marked-epoch-1"), UpdatedAt: testNow,
			})
			if !errors.Is(err, injectedErr) {
				t.Fatalf("initial CAS error = %v, want injected mirror failure", err)
			}
			object := &corev1alpha1.ControllerEpoch{}
			objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(controlstore.DefaultControllerEpochName)}
			if err := rawClient.Get(ctx, objectKey, object); err != nil {
				t.Fatalf("get marked ControllerEpoch object: %v", err)
			}
			test.mutate(object.Annotations)
			if err := rawClient.Update(ctx, object); err != nil {
				t.Fatalf("corrupt initialization marker: %v", err)
			}
			restartedStore, err := New(rawClient, testControlNamespace)
			if err != nil {
				t.Fatalf("create restarted Kubernetes store: %v", err)
			}
			if _, err := restartedStore.GetControllerEpoch(ctx, controlstore.DefaultControllerEpochName); err == nil {
				t.Fatal("corrupt blank-mirror initialization marker was accepted")
			}
		})
	}
}

func TestControllerEpochLeaseCASRejectsMissingLeaseWithRestoredStatus(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient := newTestStore(t)
	name := controlstore.DefaultControllerEpochName
	initial, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: "controller-initial", RequestDigest: testDigest("initial-epoch-1"), UpdatedAt: testNow,
	})
	if err != nil {
		t.Fatalf("create initial epoch: %v", err)
	}
	if initial.Epoch != 1 {
		t.Fatalf("initial epoch = %#v", initial)
	}

	leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
	lease := &coordinationv1.Lease{}
	if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
		t.Fatalf("get initial controller epoch Lease: %v", err)
	}
	objectKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}
	object := &corev1alpha1.ControllerEpoch{}
	if err := kubeClient.Get(ctx, objectKey, object); err != nil {
		t.Fatalf("get initial controller epoch object: %v", err)
	}
	restoredAt := metav1.NewTime(testNow.Add(7 * time.Hour))
	object.Status = corev1alpha1.ControllerEpochStatus{
		Epoch: 7, Version: 7, HolderID: testControllerRestored, RequestDigest: testDigest("restored-epoch-7"),
		AcquiredAt: &restoredAt, UpdatedAt: &restoredAt,
		LeaseName: lease.Name, LeaseResourceVersion: lease.ResourceVersion,
	}
	if err := kubeClient.Status().Update(ctx, object); err != nil {
		t.Fatalf("restore controller epoch status: %v", err)
	}
	if err := kubeClient.Delete(ctx, lease); err != nil {
		t.Fatalf("remove authoritative Lease to simulate incomplete restore: %v", err)
	}

	_, err = kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		Name: name, ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1,
		HolderID: "controller-after-restore", RequestDigest: testDigest("invalid-reset-to-1"), UpdatedAt: testNow.Add(8 * time.Hour),
	})
	if err == nil || !strings.Contains(err.Error(), "authoritative Lease is missing") {
		t.Fatalf("missing restored Lease CAS error = %v, want fail-closed authority error", err)
	}
	if err := kubeClient.Get(ctx, leaseKey, &coordinationv1.Lease{}); !apierrors.IsNotFound(err) {
		t.Fatalf("missing restored Lease was recreated: %v", err)
	}
	preserved := &corev1alpha1.ControllerEpoch{}
	if err := kubeClient.Get(ctx, objectKey, preserved); err != nil {
		t.Fatalf("get preserved controller epoch object: %v", err)
	}
	if preserved.Status.Epoch != 7 || preserved.Status.Version != 7 || preserved.Status.HolderID != testControllerRestored {
		t.Fatalf("restored status was changed after rejected reset: %#v", preserved.Status)
	}
}

func TestControllerEpochAuthorityRejectsCorruptRestore(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, context.Context, client.Client, *coordinationv1.Lease, *corev1alpha1.ControllerEpoch)
	}{
		{
			name: "missing Lease holder",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, lease *coordinationv1.Lease, _ *corev1alpha1.ControllerEpoch) {
				t.Helper()
				lease.Spec.HolderIdentity = nil
				if err := kubeClient.Update(ctx, lease); err != nil {
					t.Fatalf("clear Lease holder: %v", err)
				}
			},
		},
		{
			name: "invalid Lease digest",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, lease *coordinationv1.Lease, _ *corev1alpha1.ControllerEpoch) {
				t.Helper()
				lease.Annotations[annotationRequestDigest] = "sha256:invalid"
				if err := kubeClient.Update(ctx, lease); err != nil {
					t.Fatalf("corrupt Lease digest: %v", err)
				}
			},
		},
		{
			name: "missing Lease epoch annotation",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, lease *coordinationv1.Lease, _ *corev1alpha1.ControllerEpoch) {
				t.Helper()
				delete(lease.Annotations, annotationControllerEpoch)
				if err := kubeClient.Update(ctx, lease); err != nil {
					t.Fatalf("remove Lease epoch annotation: %v", err)
				}
			},
		},
		{
			name: "Lease version epoch mismatch",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, lease *coordinationv1.Lease, _ *corev1alpha1.ControllerEpoch) {
				t.Helper()
				lease.Annotations[annotationDomainVersion] = "2"
				if err := kubeClient.Update(ctx, lease); err != nil {
					t.Fatalf("corrupt Lease version: %v", err)
				}
			},
		},
		{
			name: "status holder mismatch",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, _ *coordinationv1.Lease, object *corev1alpha1.ControllerEpoch) {
				t.Helper()
				object.Status.HolderID = "controller-other"
				if err := kubeClient.Status().Update(ctx, object); err != nil {
					t.Fatalf("mismatch status holder: %v", err)
				}
			},
		},
		{
			name: "status digest mismatch",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, _ *coordinationv1.Lease, object *corev1alpha1.ControllerEpoch) {
				t.Helper()
				object.Status.RequestDigest = testDigest("mismatched-status-digest")
				if err := kubeClient.Status().Update(ctx, object); err != nil {
					t.Fatalf("mismatch status digest: %v", err)
				}
			},
		},
		{
			name: "status ahead of Lease",
			mutate: func(t *testing.T, ctx context.Context, kubeClient client.Client, _ *coordinationv1.Lease, object *corev1alpha1.ControllerEpoch) {
				t.Helper()
				object.Status.Epoch = 2
				object.Status.Version = 2
				if err := kubeClient.Status().Update(ctx, object); err != nil {
					t.Fatalf("advance status beyond Lease: %v", err)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
			name := controlstore.DefaultControllerEpochName
			leaseKey := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochLeaseName(name)}
			lease := &coordinationv1.Lease{}
			if err := kubeClient.Get(ctx, leaseKey, lease); err != nil {
				t.Fatalf("get controller epoch Lease: %v", err)
			}
			object := &corev1alpha1.ControllerEpoch{}
			if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(name)}, object); err != nil {
				t.Fatalf("get controller epoch object: %v", err)
			}
			test.mutate(t, ctx, kubeClient, lease, object)
			leaseBefore := &coordinationv1.Lease{}
			if err := kubeClient.Get(ctx, leaseKey, leaseBefore); err != nil {
				t.Fatalf("get corrupted controller epoch Lease: %v", err)
			}
			if _, err := kubeStore.GetControllerEpoch(ctx, name); err == nil {
				t.Fatal("corrupt restored epoch authority was accepted")
			}
			if _, _, err := kubeStore.requireControllerEpoch(ctx, fence); err == nil {
				t.Fatal("corrupt restored epoch authority granted a mutation fence")
			}
			leaseAfter := &coordinationv1.Lease{}
			if err := kubeClient.Get(ctx, leaseKey, leaseAfter); err != nil {
				t.Fatalf("get controller epoch Lease after rejected mutation fence: %v", err)
			}
			if leaseAfter.ResourceVersion != leaseBefore.ResourceVersion ||
				leaseAfter.Annotations[annotationMutationToken] != leaseBefore.Annotations[annotationMutationToken] ||
				leaseAfter.Annotations[annotationMutationExpiresAt] != leaseBefore.Annotations[annotationMutationExpiresAt] {
				t.Fatalf("corrupt authority mutation check changed Lease: before=%#v after=%#v", leaseBefore.ObjectMeta, leaseAfter.ObjectMeta)
			}
		})
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
	released := make(chan struct{})
	go func() {
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
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

func TestExternalEffectLifecyclesQueueThirtyConcurrentControllerEpochMutations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subResourceName string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" {
				if _, ok := object.(*corev1alpha1.ExternalEffect); ok {
					timer := time.NewTimer(30 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case <-timer.C:
					}
				}
			}
			return delegate.SubResource(subResourceName).Update(ctx, object, options...)
		},
	})

	const lifecycleCount = 30
	start := make(chan struct{})
	results := make(chan error, lifecycleCount)
	for index := range lifecycleCount {
		go func() {
			<-start
			label := fmt.Sprintf("security-review-%02d", index)
			identity := controlstore.ExternalEffectIdentity{
				Kind: "security.review", Namespace: "tenant-a", AggregateID: label, OperationID: "review",
			}
			now := testNow.Add(time.Duration(index) * time.Second)
			effect, err := kubeStore.ReserveExternalEffect(ctx, controlstore.ReserveExternalEffectRequest{
				Identity: identity, RequestDigest: testDigest(label), Fence: fence, CreatedAt: now,
			})
			if err != nil {
				results <- fmt.Errorf("reserve %s: %w", label, err)
				return
			}
			leaseExpiry := now.Add(5 * time.Minute)
			inFlight, err := kubeStore.TransitionExternalEffect(ctx, controlstore.ExternalEffectTransition{
				ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: controlstore.ExternalEffectPending,
				NewState: controlstore.ExternalEffectInFlight, RequestDigest: effect.RequestDigest,
				LeaseOwner: "owner-" + label, LeaseExpiresAt: &leaseExpiry, UpdatedAt: now.Add(time.Millisecond),
			})
			if err != nil {
				results <- fmt.Errorf("claim %s: %w", label, err)
				return
			}
			response := []byte(`{"reviewed":true}`)
			completed, err := kubeStore.TransitionExternalEffect(ctx, controlstore.ExternalEffectTransition{
				ID: effect.ID, Fence: fence, ExpectedVersion: inFlight.Version, ExpectedState: controlstore.ExternalEffectInFlight,
				NewState: controlstore.ExternalEffectSucceeded, RequestDigest: effect.RequestDigest,
				ResponseDigest: testBytesDigest(response), Response: response,
				ExpectedLeaseOwner: inFlight.LeaseOwner, UpdatedAt: now.Add(2 * time.Millisecond),
			})
			if err != nil {
				results <- fmt.Errorf("settle %s: %w", label, err)
				return
			}
			if completed.State != controlstore.ExternalEffectSucceeded {
				results <- fmt.Errorf("settle %s state = %s", label, completed.State)
				return
			}
			results <- nil
		}()
	}
	close(start)
	for range lifecycleCount {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestControllerEpochMutationQueueRespectsCallerDeadline(t *testing.T) {
	kubeStore, _, fence := newTestStoreWithEpoch(t)
	_, snapshot, err := kubeStore.requireControllerEpoch(context.Background(), fence)
	if err != nil {
		t.Fatalf("acquire blocking mutation: %v", err)
	}
	defer kubeStore.releaseControllerEpochMutation(snapshot)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, err := kubeStore.requireControllerEpoch(ctx, fence); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued mutation error = %v, want context deadline exceeded", err)
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

func TestNamespacedControlRecordLookupsUseWatchNamespace(t *testing.T) {
	ctx := context.Background()
	_, rawClient := newTestStore(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	const watchNamespace = "tenant-a"
	reader := interceptor.NewClient(withWatch, interceptor.Funcs{
		List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			applied := (&client.ListOptions{}).ApplyOptions(options)
			if applied.Namespace != watchNamespace {
				t.Fatalf("%T list namespace = %q, want %q", list, applied.Namespace, watchNamespace)
			}
			return delegate.List(ctx, list, options...)
		},
	})
	scopedStore, err := New(
		rawClient,
		testControlNamespace,
		WithAPIReader(reader),
		WithWatchNamespace(watchNamespace),
	)
	if err != nil {
		t.Fatalf("construct namespaced control store: %v", err)
	}

	tests := []struct {
		name   string
		lookup func() error
	}{
		{
			name: "PromptAttempt",
			lookup: func() error {
				_, err := scopedStore.findPromptAttemptByID(ctx, "missing-prompt-attempt")
				return err
			},
		},
		{
			name: "Publication",
			lookup: func() error {
				_, err := scopedStore.findPublicationByID(ctx, "missing-publication")
				return err
			},
		},
		{
			name: "ExternalEffect",
			lookup: func() error {
				_, err := scopedStore.findExternalEffectByID(ctx, "missing-external-effect")
				return err
			},
		},
		{
			name: "RuntimeSessionControl",
			lookup: func() error {
				_, err := scopedStore.findSessionControlByUID(ctx, "missing-session")
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.lookup(); !errors.Is(err, controlstore.ErrNotFound) {
				t.Fatalf("lookup error = %v, want ErrNotFound", err)
			}
		})
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
	baseClient := fake.NewClientBuilder().
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
	var leaseUIDSequence atomic.Int64
	kubeClient := interceptor.NewClient(baseClient, interceptor.Funcs{
		Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if lease, ok := object.(*coordinationv1.Lease); ok && lease.UID == "" {
				lease.UID = types.UID(fmt.Sprintf("test-lease-uid-%d", leaseUIDSequence.Add(1)))
			}
			return delegate.Create(ctx, object, options...)
		},
	})
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
		ConfigDigest:      testDigest("lineage-config-" + control.SessionUID),
		EstablishIfAbsent: true,
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
