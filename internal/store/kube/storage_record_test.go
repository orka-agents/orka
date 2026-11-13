package kube

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	controlstore "github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func storageAck(contract, dataset string, epoch, watermark int64) controlstore.StorageAcknowledgement {
	ack := controlstore.StorageAcknowledgement{
		Contract: contract, DatasetID: dataset, Epoch: epoch, AcknowledgedAt: testNow.Add(time.Duration(epoch) * time.Second),
		Watermark: watermark,
	}
	if watermark > 0 {
		ack.WatermarkAcknowledgedAt = testNow.Add(time.Duration(watermark) * time.Minute)
	}
	return ack
}

func controllerEpochObject(t *testing.T, kubeClient client.Client) *corev1alpha1.ControllerEpoch {
	t.Helper()
	object := &corev1alpha1.ControllerEpoch{}
	key := client.ObjectKey{Namespace: testControlNamespace, Name: controllerEpochObjectName(controlstore.DefaultControllerEpochName)}
	if err := kubeClient.Get(context.Background(), key, object); err != nil {
		t.Fatalf("get controller epoch object: %v", err)
	}
	return object
}

func requireStorageRecord(t *testing.T, kubeStore *Store) controlstore.StorageRecord {
	t.Helper()
	record, err := kubeStore.GetStorageRecord(context.Background(), controlstore.DefaultControllerEpochName)
	if err != nil {
		t.Fatalf("GetStorageRecord: %v", err)
	}
	return record
}

// Acknowledgements are recorded per contract and never move backward for
// the same dataset; a contract served by another dataset is replaced.
func TestStorageAcknowledgementsOnlyMoveForward(t *testing.T) {
	ctx := context.Background()
	kubeStore, _, fence := newTestStoreWithEpoch(t)
	if record := requireStorageRecord(t, kubeStore); len(record.Acknowledgements) != 0 || record.Hold != nil {
		t.Fatalf("new installation storage record = %#v", record)
	}
	for _, ack := range []controlstore.StorageAcknowledgement{
		storageAck("state.v1", "dataset-a", 1, 4),
		storageAck("artifacts.v1", "dataset-a", 1, 4),
		storageAck("state.v1", "dataset-a", 1, 2),
	} {
		if err := kubeStore.RecordStorageAcknowledgement(ctx, fence, ack); err != nil {
			t.Fatalf("RecordStorageAcknowledgement(%s): %v", ack.Contract, err)
		}
	}
	record := requireStorageRecord(t, kubeStore)
	state, ok := record.Acknowledgement("state.v1")
	if !ok || state.Watermark != 4 || state.Epoch != 1 || !state.WatermarkAcknowledgedAt.Equal(testNow.Add(4*time.Minute)) {
		t.Fatalf("state acknowledgement = %#v, want watermark 4 kept", state)
	}
	if len(record.Acknowledgements) != 2 || record.Acknowledgements[0].Contract != "artifacts.v1" {
		t.Fatalf("acknowledgements = %#v, want one per contract in order", record.Acknowledgements)
	}

	if err := kubeStore.RecordStorageWatermark(ctx, fence, "dataset-a", 7, testNow.Add(time.Hour)); err != nil {
		t.Fatalf("RecordStorageWatermark: %v", err)
	}
	if err := kubeStore.RecordStorageWatermark(ctx, fence, "dataset-a", 5, testNow.Add(2*time.Hour)); err != nil {
		t.Fatalf("RecordStorageWatermark lower: %v", err)
	}
	for _, ack := range requireStorageRecord(t, kubeStore).Acknowledgements {
		if ack.Watermark != 7 || !ack.WatermarkAcknowledgedAt.Equal(testNow.Add(time.Hour)) {
			t.Fatalf("%s watermark = %d at %s, want 7, never lowered", ack.Contract, ack.Watermark, ack.WatermarkAcknowledgedAt)
		}
	}
	if err := kubeStore.RecordStorageWatermark(ctx, fence, "dataset-unknown", 9, testNow); !errors.Is(err, controlstore.ErrNotFound) {
		t.Fatalf("watermark for an unacknowledged dataset = %v, want ErrNotFound", err)
	}

	moved := storageAck("artifacts.v1", "dataset-b", 1, 0)
	if err := kubeStore.RecordStorageAcknowledgement(ctx, fence, moved); err != nil {
		t.Fatalf("record a contract on another dataset: %v", err)
	}
	artifacts, _ := requireStorageRecord(t, kubeStore).Acknowledgement("artifacts.v1")
	if artifacts.DatasetID != "dataset-b" || artifacts.Watermark != 0 {
		t.Fatalf("artifacts acknowledgement = %#v, want dataset-b without the old watermark", artifacts)
	}
	for _, invalid := range []controlstore.StorageAcknowledgement{
		{Contract: "state.v1", DatasetID: "dataset-a", AcknowledgedAt: testNow},
		{Contract: "state.v1", DatasetID: "dataset-a", Epoch: 1},
		{Contract: "state.v1", DatasetID: "dataset-a", Epoch: 1, AcknowledgedAt: testNow, Watermark: 3},
		{DatasetID: "dataset-a", Epoch: 1, AcknowledgedAt: testNow},
	} {
		if err := kubeStore.RecordStorageAcknowledgement(ctx, fence, invalid); !errors.Is(err, controlstore.ErrValidation) {
			t.Fatalf("RecordStorageAcknowledgement(%#v) = %v, want ErrValidation", invalid, err)
		}
	}
}

// Every storage record write is fenced by the epoch the object holds, and
// the record survives the status sync of a later epoch.
func TestStorageRecordIsFencedAndSurvivesEpochSync(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	if err := kubeStore.RecordStorageAcknowledgement(ctx, fence, storageAck("state.v1", "dataset-a", 1, 3)); err != nil {
		t.Fatalf("RecordStorageAcknowledgement: %v", err)
	}
	next, err := kubeStore.CompareAndSwapControllerEpoch(ctx, controlstore.ControllerEpochCAS{
		ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2, HolderID: "controller-b",
		RequestDigest: testDigest("epoch-2"), UpdatedAt: testNow.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("advance controller epoch: %v", err)
	}
	object := controllerEpochObject(t, kubeClient)
	if object.Status.Epoch != 2 || len(object.Status.StorageEpochs) != 1 || object.Status.StorageEpochs[0].WriteWatermark != 3 {
		t.Fatalf("status after the epoch sync = %#v, want the storage record carried across", object.Status)
	}

	stale := storageAck("state.v1", "dataset-a", 1, 5)
	if err := kubeStore.RecordStorageAcknowledgement(ctx, fence, stale); !errors.Is(err, controlstore.ErrFenced) {
		t.Fatalf("acknowledgement under a replaced fence = %v, want ErrFenced", err)
	}
	if err := kubeStore.RecordStorageWatermark(ctx, fence, "dataset-a", 9, testNow); !errors.Is(err, controlstore.ErrFenced) {
		t.Fatalf("watermark under a replaced fence = %v, want ErrFenced", err)
	}
	hold := controlstore.StorageRecoveryHold{ID: "hold-stale", Reason: controlstore.StorageRecoveryWatermarkRegressed, RaisedAt: testNow}
	if _, err := kubeStore.HoldStorage(ctx, fence, hold); !errors.Is(err, controlstore.ErrFenced) {
		t.Fatalf("hold under a replaced fence = %v, want ErrFenced", err)
	}
	current := controlstore.ControllerEpochFence{Name: next.Name, Epoch: next.Epoch, HolderID: next.HolderID}
	if err := kubeStore.RecordStorageAcknowledgement(ctx, current, storageAck("state.v1", "dataset-a", 2, 0)); err != nil {
		t.Fatalf("acknowledgement under the current fence: %v", err)
	}
	if ack, _ := requireStorageRecord(t, kubeStore).Acknowledgement("state.v1"); ack.Epoch != 2 || ack.Watermark != 3 {
		t.Fatalf("acknowledgement = %#v, want epoch 2 with watermark 3 kept", ack)
	}
}

// A hold is recorded once, with its condition; it refuses new
// acknowledgements, and only the acknowledged hold can be released, which
// replaces the acknowledgements in the same update.
func TestStorageRecoveryHoldLifecycle(t *testing.T) {
	ctx := context.Background()
	kubeStore, kubeClient, fence := newTestStoreWithEpoch(t)
	if err := kubeStore.RecordStorageAcknowledgement(ctx, fence, storageAck("state.v1", "dataset-a", 1, 8)); err != nil {
		t.Fatalf("RecordStorageAcknowledgement: %v", err)
	}
	hold := controlstore.StorageRecoveryHold{
		ID: "hold-1", Reason: controlstore.StorageRecoveryWatermarkRegressed, Message: "dataset-a holds watermark 2",
		Contract: "state.v1", DatasetID: "dataset-a", AcknowledgedWatermark: 8, StoredWatermark: 2,
		Durability: "local-buffered", RaisedAt: testNow,
	}
	recorded, err := kubeStore.HoldStorage(ctx, fence, hold)
	if err != nil || recorded != hold {
		t.Fatalf("HoldStorage = %#v, %v", recorded, err)
	}
	second := hold
	second.ID, second.Reason = "hold-2", controlstore.StorageRecoveryUnsafeSwitch
	if again, err := kubeStore.HoldStorage(ctx, fence, second); err != nil || again != hold {
		t.Fatalf("second HoldStorage = %#v, %v, want the first hold", again, err)
	}
	object := controllerEpochObject(t, kubeClient)
	condition := meta.FindStatusCondition(object.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != string(controlstore.StorageRecoveryWatermarkRegressed) {
		t.Fatalf("condition = %#v, want StorageRecoveryRequired=True", condition)
	}
	record := requireStorageRecord(t, kubeStore)
	if record.Hold == nil || *record.Hold != hold || record.AcknowledgedHoldID != "" {
		t.Fatalf("record = %#v, want the hold, unacknowledged", record)
	}
	if err := kubeStore.RecordStorageAcknowledgement(ctx, fence, storageAck("state.v1", "dataset-a", 1, 9)); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("acknowledgement while held = %v, want ErrConflict", err)
	}
	if err := kubeStore.RecordStorageWatermark(ctx, fence, "dataset-a", 9, testNow); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("watermark while held = %v, want ErrConflict", err)
	}

	rebaseline := []controlstore.StorageAcknowledgement{storageAck("state.v1", "dataset-a", 1, 2)}
	if err := kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", rebaseline); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("release before acknowledgement = %v, want ErrConflict", err)
	}
	object.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] = "hold-other"
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	if err := kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", rebaseline); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("release acknowledged for another hold = %v, want ErrConflict", err)
	}
	object = controllerEpochObject(t, kubeClient)
	object.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation] = "hold-1"
	if err := kubeClient.Update(ctx, object); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	if got := requireStorageRecord(t, kubeStore).AcknowledgedHoldID; got != "hold-1" {
		t.Fatalf("acknowledged hold ID = %q", got)
	}
	if err := kubeStore.ReleaseStorageHold(ctx, fence, "hold-2", rebaseline); !errors.Is(err, controlstore.ErrConflict) {
		t.Fatalf("release of a hold that is not recorded = %v, want ErrConflict", err)
	}
	if err := kubeStore.ReleaseStorageHold(ctx, fence, "hold-1", rebaseline); err != nil {
		t.Fatalf("ReleaseStorageHold: %v", err)
	}
	record = requireStorageRecord(t, kubeStore)
	if record.Hold != nil || len(record.Acknowledgements) != 1 || record.Acknowledgements[0].Watermark != 2 {
		t.Fatalf("record after release = %#v, want no hold and the new baseline", record)
	}
	object = controllerEpochObject(t, kubeClient)
	condition = meta.FindStatusCondition(object.Status.Conditions, corev1alpha1.ConditionStorageRecoveryRequired)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != corev1alpha1.StorageRecoveryAcknowledgedReason {
		t.Fatalf("condition after release = %#v, want False", condition)
	}
	if err := kubeStore.RecordStorageWatermark(ctx, fence, "dataset-a", 3, testNow); err != nil {
		t.Fatalf("watermark after release: %v", err)
	}
}

// An API server still serving the older ControllerEpoch schema accepts a
// status update but drops the storage fields. The write then fails instead of
// letting an acknowledgement or a hold vanish.
func TestStorageRecordFailsWhenTheSchemaDropsIt(t *testing.T) {
	ctx := context.Background()
	kubeStore, rawClient, fence := newTestStoreWithEpoch(t)
	withWatch, ok := rawClient.(client.WithWatch)
	if !ok {
		t.Fatal("fake client does not implement client.WithWatch")
	}
	kubeStore.client = interceptor.NewClient(withWatch, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, delegate client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if epoch, ok := object.(*corev1alpha1.ControllerEpoch); ok {
				epoch.Status.StorageEpochs, epoch.Status.StorageRecovery, epoch.Status.Conditions = nil, nil, nil
			}
			return delegate.SubResource(subresource).Update(ctx, object, options...)
		},
	})
	err := kubeStore.RecordStorageAcknowledgement(ctx, fence, storageAck("state.v1", "dataset-a", 1, 0))
	if !errors.Is(err, controlstore.ErrValidation) {
		t.Fatalf("acknowledgement the schema dropped = %v, want ErrValidation", err)
	}
	hold := controlstore.StorageRecoveryHold{ID: "hold-1", Reason: controlstore.StorageRecoveryUnsafeSwitch, RaisedAt: testNow}
	if _, err := kubeStore.HoldStorage(ctx, fence, hold); !errors.Is(err, controlstore.ErrValidation) {
		t.Fatalf("hold the schema dropped = %v, want ErrValidation", err)
	}
}
