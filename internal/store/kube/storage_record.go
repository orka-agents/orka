package kube

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

var _ store.StorageRecordStore = (*Store)(nil)

// GetStorageRecord implements store.StorageRecordStore. It reads the
// ControllerEpoch object without the cache, so a hold another process
// recorded, and an operator's acknowledgement, are seen at once.
func (s *Store) GetStorageRecord(ctx context.Context, name string) (store.StorageRecord, error) {
	if err := s.requireClient(); err != nil {
		return store.StorageRecord{}, err
	}
	normalized, err := store.NormalizeControllerEpochName(name)
	if err != nil {
		return store.StorageRecord{}, err
	}
	object, err := s.getControllerEpochObject(ctx, normalized)
	if err != nil {
		return store.StorageRecord{}, err
	}
	return storageRecordFromObject(object), nil
}

func storageRecordFromObject(object *corev1alpha1.ControllerEpoch) store.StorageRecord {
	record := store.StorageRecord{
		AcknowledgedHoldID: strings.TrimSpace(object.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation]),
	}
	for _, entry := range object.Status.StorageEpochs {
		ack := store.StorageAcknowledgement{
			Contract: entry.Contract, DatasetID: entry.DatasetID, Epoch: entry.Epoch,
			AcknowledgedAt: entry.AcknowledgedAt.UTC(), Watermark: entry.WriteWatermark,
		}
		if entry.WatermarkAcknowledgedAt != nil {
			ack.WatermarkAcknowledgedAt = entry.WatermarkAcknowledgedAt.UTC()
		}
		record.Acknowledgements = append(record.Acknowledgements, ack)
	}
	if hold := object.Status.StorageRecovery; hold != nil {
		record.Hold = &store.StorageRecoveryHold{
			ID: hold.ID, Reason: store.StorageRecoveryReason(hold.Reason), Message: hold.Message,
			Contract: hold.Contract, DatasetID: hold.DatasetID, AcknowledgedDatasetID: hold.AcknowledgedDatasetID,
			AcknowledgedEpoch: hold.AcknowledgedEpoch, StoredEpoch: hold.StoredEpoch,
			AcknowledgedWatermark: hold.AcknowledgedWatermark, StoredWatermark: hold.StoredWatermark,
			Durability: hold.Durability, RaisedAt: hold.RaisedAt.UTC(),
		}
	}
	return record
}

// RecordStorageAcknowledgement implements store.StorageRecordStore.
func (s *Store) RecordStorageAcknowledgement(ctx context.Context, fence store.ControllerEpochFence, ack store.StorageAcknowledgement) error {
	if err := validateStorageAcknowledgement(ack); err != nil {
		return err
	}
	return s.updateStorageRecord(ctx, fence, func(status *corev1alpha1.ControllerEpochStatus) (bool, error) {
		if status.StorageRecovery != nil {
			return false, store.ConflictErrorf("storage recovery hold %q is recorded", status.StorageRecovery.ID)
		}
		entry := storageEpochEntry(ack)
		for i, existing := range status.StorageEpochs {
			if existing.Contract != ack.Contract {
				continue
			}
			if existing.DatasetID == ack.DatasetID {
				entry.Epoch = max(entry.Epoch, existing.Epoch)
				if existing.WriteWatermark > entry.WriteWatermark {
					entry.WriteWatermark = existing.WriteWatermark
					entry.WatermarkAcknowledgedAt = existing.WatermarkAcknowledgedAt
				}
				if entry.Epoch == existing.Epoch {
					entry.AcknowledgedAt = existing.AcknowledgedAt
				}
			}
			if storageEpochEntriesEqual(existing, entry) {
				return false, nil
			}
			status.StorageEpochs[i] = entry
			return true, nil
		}
		status.StorageEpochs = append(status.StorageEpochs, entry)
		slices.SortFunc(status.StorageEpochs, func(a, b corev1alpha1.StorageEpochAcknowledgement) int {
			return strings.Compare(a.Contract, b.Contract)
		})
		return true, nil
	})
}

// RecordStorageWatermark implements store.StorageRecordStore.
func (s *Store) RecordStorageWatermark(
	ctx context.Context, fence store.ControllerEpochFence, datasetID string, watermark int64, at time.Time,
) error {
	if err := store.ValidateControlIdentifier("storage dataset ID", datasetID); err != nil {
		return err
	}
	if watermark < 1 || at.IsZero() {
		return store.ValidationErrorf("a recorded storage write watermark must be at least 1 and have a time")
	}
	return s.updateStorageRecord(ctx, fence, func(status *corev1alpha1.ControllerEpochStatus) (bool, error) {
		if status.StorageRecovery != nil {
			return false, store.ConflictErrorf("storage recovery hold %q is recorded", status.StorageRecovery.ID)
		}
		changed, found := false, false
		for i := range status.StorageEpochs {
			entry := &status.StorageEpochs[i]
			if entry.DatasetID != datasetID {
				continue
			}
			found = true
			if watermark > entry.WriteWatermark {
				entry.WriteWatermark = watermark
				entry.WatermarkAcknowledgedAt = metaTime(at)
				changed = true
			}
		}
		if !found {
			return false, fmt.Errorf("%w: no storage contract acknowledged dataset %q", store.ErrNotFound, datasetID)
		}
		return changed, nil
	})
}

// HoldStorage implements store.StorageRecordStore.
func (s *Store) HoldStorage(ctx context.Context, fence store.ControllerEpochFence, hold store.StorageRecoveryHold) (store.StorageRecoveryHold, error) {
	if err := store.ValidateControlIdentifier("storage recovery hold ID", hold.ID); err != nil {
		return store.StorageRecoveryHold{}, err
	}
	if hold.Reason == "" || hold.RaisedAt.IsZero() {
		return store.StorageRecoveryHold{}, store.ValidationErrorf("a storage recovery hold needs a reason and a time")
	}
	var recorded store.StorageRecoveryHold
	err := s.updateStorageRecord(ctx, fence, func(status *corev1alpha1.ControllerEpochStatus) (bool, error) {
		if existing := status.StorageRecovery; existing != nil {
			recorded = *storageRecordFromObject(&corev1alpha1.ControllerEpoch{Status: *status}).Hold
			return false, nil
		}
		status.StorageRecovery = &corev1alpha1.StorageRecoveryHold{
			ID: hold.ID, Reason: corev1alpha1.StorageRecoveryReason(hold.Reason), Message: truncateMessage(hold.Message),
			Contract: hold.Contract, DatasetID: hold.DatasetID, AcknowledgedDatasetID: hold.AcknowledgedDatasetID,
			AcknowledgedEpoch: hold.AcknowledgedEpoch, StoredEpoch: hold.StoredEpoch,
			AcknowledgedWatermark: hold.AcknowledgedWatermark, StoredWatermark: hold.StoredWatermark,
			Durability: hold.Durability, RaisedAt: metav1.NewTime(hold.RaisedAt.UTC()),
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionTrue,
			Reason: string(hold.Reason),
			Message: truncateMessage(fmt.Sprintf("storage recovery hold %s: %s; set annotation %s=%s after recovery to release it",
				hold.ID, hold.Message, corev1alpha1.StorageRecoveryAcknowledgedAnnotation, hold.ID)),
		})
		recorded = hold
		return true, nil
	})
	if err != nil {
		return store.StorageRecoveryHold{}, err
	}
	return recorded, nil
}

// ReleaseStorageHold implements store.StorageRecordStore.
func (s *Store) ReleaseStorageHold(
	ctx context.Context, fence store.ControllerEpochFence, id string, acks []store.StorageAcknowledgement,
) error {
	for _, ack := range acks {
		if err := validateStorageAcknowledgement(ack); err != nil {
			return err
		}
	}
	return s.updateStorageRecordObject(ctx, fence, func(object *corev1alpha1.ControllerEpoch) (bool, error) {
		status := &object.Status
		hold := status.StorageRecovery
		if hold == nil || hold.ID != id {
			return false, store.ConflictErrorf("storage recovery hold %q is not the recorded hold", id)
		}
		if strings.TrimSpace(object.Annotations[corev1alpha1.StorageRecoveryAcknowledgedAnnotation]) != id {
			return false, store.ConflictErrorf("storage recovery hold %q is not acknowledged", id)
		}
		status.StorageRecovery = nil
		status.StorageEpochs = nil
		for _, ack := range acks {
			status.StorageEpochs = append(status.StorageEpochs, storageEpochEntry(ack))
		}
		slices.SortFunc(status.StorageEpochs, func(a, b corev1alpha1.StorageEpochAcknowledgement) int {
			return strings.Compare(a.Contract, b.Contract)
		})
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: corev1alpha1.ConditionStorageRecoveryRequired, Status: metav1.ConditionFalse,
			Reason:  corev1alpha1.StorageRecoveryAcknowledgedReason,
			Message: truncateMessage(fmt.Sprintf("storage recovery hold %s was acknowledged; storage acknowledgements were recorded again", id)),
		})
		return true, nil
	})
}

// updateStorageRecord applies change to the ControllerEpoch status while
// fence is the epoch it holds, retrying on write conflicts.
func (s *Store) updateStorageRecord(
	ctx context.Context, fence store.ControllerEpochFence, change func(*corev1alpha1.ControllerEpochStatus) (bool, error),
) error {
	return s.updateStorageRecordObject(ctx, fence, func(object *corev1alpha1.ControllerEpoch) (bool, error) {
		return change(&object.Status)
	})
}

func (s *Store) updateStorageRecordObject(
	ctx context.Context, fence store.ControllerEpochFence, change func(*corev1alpha1.ControllerEpoch) (bool, error),
) error {
	if err := s.requireClient(); err != nil {
		return err
	}
	normalized, err := store.NormalizeEpochFence(fence)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		object, err := s.getControllerEpochObject(ctx, normalized.Name)
		if err != nil {
			return err
		}
		if object.Status.Epoch != normalized.Epoch || object.Status.HolderID != normalized.HolderID {
			return store.FencedErrorf("controller epoch %q is at %d held by %q, not %d held by %q",
				normalized.Name, object.Status.Epoch, object.Status.HolderID, normalized.Epoch, normalized.HolderID)
		}
		changed, err := change(object)
		if err != nil || !changed {
			return err
		}
		want := object.Status.DeepCopy()
		if err := s.client.Status().Update(ctx, object); err != nil {
			return err
		}
		// An API server still serving an older ControllerEpoch schema
		// accepts the update but drops the fields it does not know. A record
		// that does not keep what was written would let a hold vanish, so
		// the write fails instead.
		if !storageRecordKept(*want, object.Status) {
			return store.ValidationErrorf(
				"controller epoch object %s/%s did not keep its storage record; apply this release's ControllerEpoch CRD",
				object.Namespace, object.Name)
		}
		return nil
	})
}

// storageRecordKept reports whether the status the API server returned keeps
// the storage record that was written. Times are not compared, because the
// API server stores them to the second.
func storageRecordKept(want, got corev1alpha1.ControllerEpochStatus) bool {
	if len(want.StorageEpochs) != len(got.StorageEpochs) {
		return false
	}
	for i, entry := range want.StorageEpochs {
		kept := got.StorageEpochs[i]
		if entry.Contract != kept.Contract || entry.DatasetID != kept.DatasetID ||
			entry.Epoch != kept.Epoch || entry.WriteWatermark != kept.WriteWatermark {
			return false
		}
	}
	if (want.StorageRecovery == nil) != (got.StorageRecovery == nil) {
		return false
	}
	if want.StorageRecovery != nil && (want.StorageRecovery.ID != got.StorageRecovery.ID ||
		want.StorageRecovery.Reason != got.StorageRecovery.Reason) {
		return false
	}
	return meta.FindStatusCondition(want.Conditions, corev1alpha1.ConditionStorageRecoveryRequired) == nil ||
		meta.FindStatusCondition(got.Conditions, corev1alpha1.ConditionStorageRecoveryRequired) != nil
}

func validateStorageAcknowledgement(ack store.StorageAcknowledgement) error {
	if err := store.ValidateControlIdentifier("storage contract", ack.Contract); err != nil {
		return err
	}
	if err := store.ValidateControlIdentifier("storage dataset ID", ack.DatasetID); err != nil {
		return err
	}
	if ack.Epoch < 1 || ack.AcknowledgedAt.IsZero() || ack.Watermark < 0 ||
		(ack.Watermark > 0) != !ack.WatermarkAcknowledgedAt.IsZero() {
		return store.ValidationErrorf("invalid storage acknowledgement for %s", ack.Contract)
	}
	return nil
}

func storageEpochEntry(ack store.StorageAcknowledgement) corev1alpha1.StorageEpochAcknowledgement {
	entry := corev1alpha1.StorageEpochAcknowledgement{
		Contract: ack.Contract, DatasetID: ack.DatasetID, Epoch: ack.Epoch,
		AcknowledgedAt: metav1.NewTime(ack.AcknowledgedAt.UTC()), WriteWatermark: ack.Watermark,
	}
	if ack.Watermark > 0 {
		entry.WatermarkAcknowledgedAt = metaTime(ack.WatermarkAcknowledgedAt)
	}
	return entry
}

func storageEpochEntriesEqual(a, b corev1alpha1.StorageEpochAcknowledgement) bool {
	sameWatermarkTime := (a.WatermarkAcknowledgedAt == nil) == (b.WatermarkAcknowledgedAt == nil) &&
		(a.WatermarkAcknowledgedAt == nil || a.WatermarkAcknowledgedAt.Equal(b.WatermarkAcknowledgedAt))
	return a.Contract == b.Contract && a.DatasetID == b.DatasetID && a.Epoch == b.Epoch &&
		a.AcknowledgedAt.Equal(&b.AcknowledgedAt) && a.WriteWatermark == b.WriteWatermark && sameWatermarkTime
}

// truncateMessage keeps a status message within the 1024 bytes the
// ControllerEpoch schema allows.
func truncateMessage(message string) string {
	const limit = 1024
	if len(message) <= limit {
		return message
	}
	cut := limit - len("...")
	for cut > 0 && !utf8RuneStart(message[cut]) {
		cut--
	}
	return message[:cut] + "..."
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
