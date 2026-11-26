package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrStorageRecoveryRequired = errors.New("storage recovery required")

type StorageHoldError struct {
	Hold StorageRecoveryHold
}

func (e *StorageHoldError) Error() string {
	return fmt.Sprintf("%v: hold %s (%s): %s", ErrStorageRecoveryRequired, e.Hold.ID, e.Hold.Reason, e.Hold.Message)
}

func (e *StorageHoldError) Is(target error) bool {
	return target == ErrStorageRecoveryRequired || target == ErrNotReady
}

type StorageRecoveryReason string

const (
	StorageRecoveryEpochRegressed     StorageRecoveryReason = "EpochRegressed"
	StorageRecoveryWatermarkRegressed StorageRecoveryReason = "WatermarkRegressed"
	StorageRecoveryDatasetChanged     StorageRecoveryReason = "DatasetChanged"
	StorageRecoveryUnsafeSwitch       StorageRecoveryReason = "UnsafeSwitch"
)

type StorageAcknowledgement struct {
	Contract                string
	DatasetID               string
	Epoch                   int64
	AcknowledgedAt          time.Time
	Watermark               int64
	WatermarkAcknowledgedAt time.Time
}

type StorageRecoveryHold struct {
	ID                    string
	Reason                StorageRecoveryReason
	Message               string
	Contract              string
	DatasetID             string
	AcknowledgedDatasetID string
	AcknowledgedEpoch     int64
	StoredEpoch           int64
	AcknowledgedWatermark int64
	StoredWatermark       int64
	Durability            string
	RaisedAt              time.Time
}

type StorageRecord struct {
	Acknowledgements   []StorageAcknowledgement
	Hold               *StorageRecoveryHold
	AcknowledgedHoldID string
}

func (r StorageRecord) Acknowledgement(contract string) (StorageAcknowledgement, bool) {
	for _, ack := range r.Acknowledgements {
		if ack.Contract == contract {
			return ack, true
		}
	}
	return StorageAcknowledgement{}, false
}

type StorageRecordStore interface {
	GetStorageRecord(ctx context.Context, name string) (StorageRecord, error)
	RecordStorageAcknowledgement(ctx context.Context, fence ControllerEpochFence, ack StorageAcknowledgement) error
	RecordStorageWatermark(ctx context.Context, fence ControllerEpochFence, datasetID string, watermark int64, at time.Time) error
	HoldStorage(ctx context.Context, fence ControllerEpochFence, hold StorageRecoveryHold) (StorageRecoveryHold, error)
	ReleaseStorageHold(ctx context.Context, fence ControllerEpochFence, id string, acks []StorageAcknowledgement) error
}
