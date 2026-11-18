package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// memoryStorageRecords is an in-memory store.StorageRecordStore for test
// epoch stores that keep no ControllerEpoch object. It applies the same
// merge rules as the Kubernetes store and checks no fence.
type memoryStorageRecords struct {
	mu     sync.Mutex
	record store.StorageRecord
}

func newMemoryStorageRecords() *memoryStorageRecords { return &memoryStorageRecords{} }

func (r *memoryStorageRecords) GetStorageRecord(context.Context, string) (store.StorageRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.record
	record.Acknowledgements = slices.Clone(record.Acknowledgements)
	if record.Hold != nil {
		hold := *record.Hold
		record.Hold = &hold
	}
	return record, nil
}

func (r *memoryStorageRecords) RecordStorageAcknowledgement(_ context.Context, _ store.ControllerEpochFence, ack store.StorageAcknowledgement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record.Hold != nil {
		return store.ConflictErrorf("storage recovery hold %q is recorded", r.record.Hold.ID)
	}
	for i, existing := range r.record.Acknowledgements {
		if existing.Contract != ack.Contract {
			continue
		}
		if existing.DatasetID == ack.DatasetID {
			ack.Epoch = max(ack.Epoch, existing.Epoch)
			if existing.Watermark > ack.Watermark {
				ack.Watermark, ack.WatermarkAcknowledgedAt = existing.Watermark, existing.WatermarkAcknowledgedAt
			}
		}
		r.record.Acknowledgements[i] = ack
		return nil
	}
	r.record.Acknowledgements = append(r.record.Acknowledgements, ack)
	slices.SortFunc(r.record.Acknowledgements, func(a, b store.StorageAcknowledgement) int {
		return strings.Compare(a.Contract, b.Contract)
	})
	return nil
}

func (r *memoryStorageRecords) RecordStorageWatermark(
	_ context.Context, _ store.ControllerEpochFence, datasetID string, watermark int64, at time.Time,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record.Hold != nil {
		return store.ConflictErrorf("storage recovery hold %q is recorded", r.record.Hold.ID)
	}
	found := false
	for i := range r.record.Acknowledgements {
		ack := &r.record.Acknowledgements[i]
		if ack.DatasetID != datasetID {
			continue
		}
		found = true
		if watermark > ack.Watermark {
			ack.Watermark, ack.WatermarkAcknowledgedAt = watermark, at
		}
	}
	if !found {
		return fmt.Errorf("%w: no storage contract acknowledged dataset %q", store.ErrNotFound, datasetID)
	}
	return nil
}

func (r *memoryStorageRecords) HoldStorage(_ context.Context, _ store.ControllerEpochFence, hold store.StorageRecoveryHold) (store.StorageRecoveryHold, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record.Hold != nil {
		return *r.record.Hold, nil
	}
	r.record.Hold = &hold
	return hold, nil
}

func (r *memoryStorageRecords) ReleaseStorageHold(
	_ context.Context, _ store.ControllerEpochFence, id string, acks []store.StorageAcknowledgement,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record.Hold == nil || r.record.Hold.ID != id || r.record.AcknowledgedHoldID != id {
		return store.ConflictErrorf("storage recovery hold %q is not the acknowledged hold", id)
	}
	r.record.Hold = nil
	r.record.Acknowledgements = slices.Clone(acks)
	return nil
}

var _ store.StorageRecordStore = (*memoryStorageRecords)(nil)

// AdvanceWriteWatermark advances the fake dataset's write watermark.
func (d *recordingEpochDataset) AdvanceWriteWatermark(context.Context) (storagev1.WriteWatermark, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.watermark++
	d.watermarkAt = time.Now().UTC()
	return storagev1.WriteWatermark{DatasetID: d.datasetID, Value: d.watermark, AdvancedAt: d.watermarkAt}, nil
}

// GetWriteWatermark returns the fake dataset's write watermark.
func (d *recordingEpochDataset) GetWriteWatermark(context.Context) (storagev1.WriteWatermark, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return storagev1.WriteWatermark{DatasetID: d.datasetID, Value: d.watermark, AdvancedAt: d.watermarkAt}, nil
}
