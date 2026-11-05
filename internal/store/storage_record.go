package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrStorageRecoveryRequired is returned while the installation is held for
// storage recovery. Errors that carry it also match ErrNotReady, so callers
// treat the hold as a temporary storage outage: they neither repeat an action
// nor report success.
var ErrStorageRecoveryRequired = errors.New("storage recovery required")

// StorageHoldError reports the installation's storage recovery hold.
type StorageHoldError struct {
	Hold StorageRecoveryHold
}

func (e *StorageHoldError) Error() string {
	return fmt.Sprintf("%v: hold %s (%s): %s", ErrStorageRecoveryRequired, e.Hold.ID, e.Hold.Reason, e.Hold.Message)
}

// Is matches ErrStorageRecoveryRequired and ErrNotReady.
func (e *StorageHoldError) Is(target error) bool {
	return target == ErrStorageRecoveryRequired || target == ErrNotReady
}

// StorageRecoveryReason says why Orka held the installation for storage
// recovery.
type StorageRecoveryReason string

const (
	// StorageRecoveryEpochRegressed: a dataset holds a lower controller epoch
	// than it acknowledged.
	StorageRecoveryEpochRegressed StorageRecoveryReason = "EpochRegressed"
	// StorageRecoveryWatermarkRegressed: a dataset holds a lower write
	// watermark than it acknowledged.
	StorageRecoveryWatermarkRegressed StorageRecoveryReason = "WatermarkRegressed"
	// StorageRecoveryDatasetChanged: a storage contract is served by another
	// dataset than the one that acknowledged it.
	StorageRecoveryDatasetChanged StorageRecoveryReason = "DatasetChanged"
	// StorageRecoveryUnsafeSwitch: the database of a dataset whose durability
	// class can lose acknowledged writes in a failover restarted or switched.
	StorageRecoveryUnsafeSwitch StorageRecoveryReason = "UnsafeSwitch"
)

// StorageAcknowledgement is what one storage contract's dataset acknowledged
// to the installation: the controller epoch it stored and the highest write
// watermark it stored, each recorded only after the dataset returned success.
type StorageAcknowledgement struct {
	Contract       string
	DatasetID      string
	Epoch          int64
	AcknowledgedAt time.Time
	// Watermark is zero until Orka records an acknowledged watermark for the
	// dataset.
	Watermark               int64
	WatermarkAcknowledgedAt time.Time
}

// StorageRecoveryHold records why and where Orka stopped work for storage
// recovery. Its fields other than ID, Reason, Message and RaisedAt are
// evidence and may be zero.
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

// StorageRecord is what the installation's controller epoch record holds
// about storage.
type StorageRecord struct {
	// Acknowledgements holds one entry per storage contract.
	Acknowledgements []StorageAcknowledgement
	// Hold is the installation's storage recovery hold, or nil.
	Hold *StorageRecoveryHold
	// AcknowledgedHoldID is the hold ID an operator acknowledged, or empty.
	AcknowledgedHoldID string
}

// Acknowledgement returns the entry for contract.
func (r StorageRecord) Acknowledgement(contract string) (StorageAcknowledgement, bool) {
	for _, ack := range r.Acknowledgements {
		if ack.Contract == contract {
			return ack, true
		}
	}
	return StorageAcknowledgement{}, false
}

// StorageRecordStore keeps, in Kubernetes, what each storage dataset
// acknowledged and the installation's storage recovery hold. Every write is
// fenced: it commits only while fence is the controller epoch the record
// holds, and otherwise returns ErrFenced, which also matches ErrConflict. A
// write that a recorded hold refuses returns ErrConflict.
type StorageRecordStore interface {
	// GetStorageRecord returns the storage record of the controller epoch
	// named name.
	GetStorageRecord(ctx context.Context, name string) (StorageRecord, error)
	// RecordStorageAcknowledgement stores the entry for ack.Contract. It never
	// lowers the epoch or watermark already recorded for the same contract
	// and dataset, and it is refused while a hold is recorded.
	RecordStorageAcknowledgement(ctx context.Context, fence ControllerEpochFence, ack StorageAcknowledgement) error
	// RecordStorageWatermark raises the recorded watermark of every contract
	// that datasetID serves to watermark. It never lowers one, and it is
	// refused while a hold is recorded.
	RecordStorageWatermark(ctx context.Context, fence ControllerEpochFence, datasetID string, watermark int64, at time.Time) error
	// HoldStorage records hold, with the StorageRecoveryRequired condition,
	// unless a hold is already recorded, and returns the recorded one.
	HoldStorage(ctx context.Context, fence ControllerEpochFence, hold StorageRecoveryHold) (StorageRecoveryHold, error)
	// ReleaseStorageHold clears the hold whose ID is id, which an operator
	// has acknowledged, and replaces every acknowledgement with acks, in one
	// update. It returns ErrConflict when that hold is not the recorded one
	// or was not acknowledged.
	ReleaseStorageHold(ctx context.Context, fence ControllerEpochFence, id string, acks []StorageAcknowledgement) error
}
