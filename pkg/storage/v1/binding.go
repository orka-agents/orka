package v1

import (
	"context"
	"fmt"
	"time"
)

// ErrDatasetDetached is returned for a write that carries a [WriteEnvelope],
// or a controller epoch advance, on a dataset its installation has detached
// (see [DatasetBindingStore.DetachDatasetBinding]). It matches [ErrNotReady].
// Nothing was committed, and the dataset accepts writes again only after its
// installation binds it again.
var ErrDatasetDetached = fmt.Errorf("%w: dataset detached from its installation", ErrNotReady)

// DatasetBindingState says whether a bound dataset is in use by its
// installation.
type DatasetBindingState string

const (
	// DatasetBound marks a dataset its installation reads and writes.
	DatasetBound DatasetBindingState = "bound"
	// DatasetDetached marks a dataset its installation has left, for example
	// when an explicit storage switch moved it to another dataset. The dataset
	// still belongs to that installation: reads work, but writes and controller
	// epoch advances return [ErrDatasetDetached].
	DatasetDetached DatasetBindingState = "detached"
)

// IsValidDatasetBindingState reports whether state is a defined binding state.
func IsValidDatasetBindingState(state DatasetBindingState) bool {
	switch state {
	case DatasetBound, DatasetDetached:
		return true
	default:
		return false
	}
}

// DatasetBinding records the one installation that owns a dataset.
type DatasetBinding struct {
	// InstallationID identifies the Orka installation that owns the dataset.
	InstallationID string
	// RecordUID is the UID of the installation record that bound the dataset
	// at SwitchGeneration. It is empty for a binding the controller epoch
	// advance recorded (see [ControllerEpochAdvancer]).
	RecordUID string
	// SwitchGeneration is the installation's storage switch generation at
	// which the dataset reached State, at least 1. It is 0 for a binding the
	// controller epoch advance recorded: the dataset belongs to the
	// installation, which has not bound it itself yet.
	SwitchGeneration int64
	// KeyFingerprint identifies the snapshot key that seals the dataset's
	// records, for a dataset that serves StateContract, or is empty. Orka
	// derives it from the key; it is never key material, and providers compare
	// it only for equality.
	KeyFingerprint string
	// State says whether the installation uses the dataset or has left it.
	State DatasetBindingState
	// BoundAt is the provider clock when the dataset reached State at
	// SwitchGeneration. It is zero when the provider cannot tell.
	BoundAt time.Time
}

// BindMode states which dataset an installation may bind, and so how a
// provider checks a [BindDatasetRequest]. Orka chooses the mode from how the
// installation came to use the dataset.
type BindMode string

const (
	// BindFresh binds a new dataset to a new installation. The dataset's
	// origin must be [DatasetOriginNew], it must hold no record of any kind,
	// including control records such as a controller epoch or an operation
	// receipt, it must not have completed its bootstrap (see
	// [DatasetBindingStore.CompleteDatasetBootstrap]) and it must be
	// unclaimed. A dataset a controller epoch advance has reached holds an
	// epoch, so it is never fresh. A provider that cannot prove the dataset
	// holds no record refuses the mode.
	BindFresh BindMode = "fresh"
	// BindEmbedded binds an embedded dataset that the provider opened under
	// its own process lock, so no other process can use it, in any bootstrap
	// state. The dataset must be unclaimed. Only an embedded provider accepts
	// this mode; any other provider returns ErrValidation.
	BindEmbedded BindMode = "embedded"
	// BindMigrated binds a dataset imported from another provider: its origin
	// is [DatasetOriginMigrated], its import has finished, so its state is
	// [DatasetReady], and it is unclaimed.
	BindMigrated BindMode = "migrated"
	// BindAdopt binds a dataset that already holds data the installation takes
	// over. The dataset must be unclaimed, or detached from the same
	// installation at a lower switch generation.
	BindAdopt BindMode = "adopt"
	// BindRebind moves a dataset the same installation already owns, bound,
	// detached or recorded by the controller epoch advance at generation 0, to
	// a higher switch generation, for example when a storage switch returns to
	// it or when a recovered installation replaces its record.
	BindRebind BindMode = "rebind"
)

// IsValidBindMode reports whether mode is a defined bind mode.
func IsValidBindMode(mode BindMode) bool {
	switch mode {
	case BindFresh, BindEmbedded, BindMigrated, BindAdopt, BindRebind:
		return true
	default:
		return false
	}
}

// BindDatasetRequest asks a provider to bind its dataset to an installation.
type BindDatasetRequest struct {
	// DatasetID is the dataset to bind. A different dataset returns
	// ErrDatasetMismatch.
	DatasetID string
	// InstallationID identifies the installation that owns the dataset.
	InstallationID string
	// RecordUID is the UID of the installation record making the binding.
	RecordUID string
	// SwitchGeneration is the installation's storage switch generation, at
	// least 1.
	SwitchGeneration int64
	// KeyFingerprint identifies the snapshot key for a dataset that serves
	// StateContract, or is empty.
	KeyFingerprint string
	// Mode states which dataset the installation may bind.
	Mode BindMode
}

// Validate checks that every field is present and well formed.
func (r BindDatasetRequest) Validate() error {
	if err := validateBindingIdentity("bind dataset", r.DatasetID, r.InstallationID, r.RecordUID, r.SwitchGeneration); err != nil {
		return err
	}
	if r.KeyFingerprint != "" {
		if err := ValidateControlIdentifier("bind dataset key fingerprint", r.KeyFingerprint); err != nil {
			return err
		}
	}
	if !IsValidBindMode(r.Mode) {
		return ValidationErrorf("bind dataset mode %q is not defined", r.Mode)
	}
	return nil
}

// DetachDatasetRequest asks a provider to record that an installation has left
// its dataset.
type DetachDatasetRequest struct {
	// DatasetID is the dataset to detach. A different dataset returns
	// ErrDatasetMismatch.
	DatasetID string
	// InstallationID identifies the installation that owns the dataset.
	InstallationID string
	// RecordUID is the UID of the installation record leaving the dataset.
	RecordUID string
	// SwitchGeneration is the installation's storage switch generation after
	// the switch that leaves the dataset. It is higher than the generation the
	// dataset is bound at.
	SwitchGeneration int64
}

// Validate checks that every field is present and well formed.
func (r DetachDatasetRequest) Validate() error {
	return validateBindingIdentity("detach dataset", r.DatasetID, r.InstallationID, r.RecordUID, r.SwitchGeneration)
}

// UpdateBindingKeyRequest records a new snapshot key fingerprint on a bound
// dataset, after Orka proved that the dataset's sealed records authenticate
// under the new key.
type UpdateBindingKeyRequest struct {
	// DatasetID is the bound dataset. A different dataset returns
	// ErrDatasetMismatch.
	DatasetID string
	// InstallationID, RecordUID and SwitchGeneration must equal the stored
	// binding.
	InstallationID   string
	RecordUID        string
	SwitchGeneration int64
	// PreviousKeyFingerprint is the fingerprint the caller expects the binding
	// to hold, or empty when it expects none.
	PreviousKeyFingerprint string
	// KeyFingerprint is the new fingerprint.
	KeyFingerprint string
}

// Validate checks that every field is present and well formed.
func (r UpdateBindingKeyRequest) Validate() error {
	if err := validateBindingIdentity("update binding key", r.DatasetID, r.InstallationID, r.RecordUID, r.SwitchGeneration); err != nil {
		return err
	}
	if r.PreviousKeyFingerprint != "" {
		if err := ValidateControlIdentifier("update binding key previous fingerprint", r.PreviousKeyFingerprint); err != nil {
			return err
		}
	}
	return ValidateControlIdentifier("update binding key fingerprint", r.KeyFingerprint)
}

func validateBindingIdentity(operation, datasetID, installationID, recordUID string, generation int64) error {
	if err := ValidateControlIdentifier(operation+" dataset ID", datasetID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier(operation+" installation ID", installationID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier(operation+" record UID", recordUID); err != nil {
		return err
	}
	if generation < 1 {
		return ValidationErrorf("%s switch generation must be at least 1", operation)
	}
	return nil
}

// BindingConflictError reports a binding request that the dataset's stored
// binding or bootstrap state does not admit: the dataset belongs to another
// installation, it is bound under a different record, generation or key, or
// the request's mode does not fit the dataset. It matches [ErrConflict].
// Nothing was committed.
type BindingConflictError struct {
	// DatasetID is the dataset the request named.
	DatasetID string
	// Binding is the dataset's stored binding, or nil when the dataset is
	// unbound.
	Binding *DatasetBinding
	// Reason says why the request was refused.
	Reason string
}

func (e BindingConflictError) Error() string {
	return fmt.Sprintf("dataset %q binding conflict: %s", e.DatasetID, e.Reason)
}

// Is reports whether target is ErrConflict.
func (e BindingConflictError) Is(target error) bool {
	return target == ErrConflict
}

// DatasetBindingStore records which installation owns a dataset, written once,
// so that a second installation can never claim a dataset another one bound,
// and a replaced or rebuilt dataset is noticed. Both providers implement it.
// Orka does not call it yet.
//
// A dataset is unbound, bound or detached, and belongs to at most one
// installation. A dataset is "unclaimed" for an installation when it is
// unbound, or when the controller epoch advance recorded that installation
// without a binding (SwitchGeneration 0; see [ControllerEpochAdvancer]).
//
// Every method is administrative ([MethodAdmin]): it carries no write envelope
// and is not checked against the controller fence, because it establishes the
// dataset's identity before any fence exists. Each method is atomic and
// linearizable with every other call on the dataset, a refusal commits
// nothing, and a request for another dataset returns [ErrDatasetMismatch].
// Every change is durable when the method returns.
//
// BindDataset follows these rules, in order:
//
//  1. A dataset bound, not detached, to the request's installation, record
//     UID, switch generation and key fingerprint returns the stored binding
//     unchanged, whatever the mode, so a retry after a lost response
//     succeeds.
//  2. A dataset that belongs to another installation, bound, detached or
//     recorded by the advance, returns a [BindingConflictError]. Moving a
//     dataset to another installation is never this method.
//  3. The mode's precondition must hold (see [BindMode]), or the request
//     returns a [BindingConflictError]. A dataset bound to the same
//     installation under another record UID, generation or key fingerprint
//     is never unclaimed: only [BindRebind] at a higher generation moves it,
//     and [DatasetBindingStore.UpdateBindingKeyFingerprint] changes its key
//     fingerprint.
//  4. The dataset is bound to the request's installation, record UID,
//     generation and key fingerprint, and the binding is returned.
//
// Binding a dataset records its installation as the controller epoch advance
// does: from then on the advance accepts only that installation, and a
// provider that also serves [ControllerEpochMirror.SyncControllerEpochMirror]
// refuses it.
//
// While a dataset is detached, every write that carries a [WriteEnvelope]
// returns [ErrDatasetDetached] at the dataset-state check (see
// [WriteEnvelope]), and so does [ControllerEpochAdvancer.AdvanceControllerEpoch].
// Reads, the replay of a committed operation and the
// [OperationReceiptStore] methods still work, because none of them can make
// a new change. Only BindDataset with [BindAdopt] or [BindRebind] at a higher
// generation binds it again.
type DatasetBindingStore interface {
	// BindDataset binds the dataset to an installation under the rules above
	// and returns the stored binding.
	BindDataset(ctx context.Context, request BindDatasetRequest) (DatasetBinding, error)
	// GetDatasetBinding returns the dataset's binding, including one the
	// controller epoch advance recorded, or ErrNotFound when the dataset is
	// unbound.
	GetDatasetBinding(ctx context.Context, datasetID string) (DatasetBinding, error)
	// DetachDatasetBinding records that the installation has left the dataset
	// at the request's switch generation, keeping the installation and key
	// fingerprint, and returns the stored binding. The dataset must belong to
	// the request's installation at a lower generation, bound or recorded by
	// the advance; a dataset already detached with the request's record UID
	// and generation returns its binding unchanged. Anything else returns a
	// [BindingConflictError].
	DetachDatasetBinding(ctx context.Context, request DetachDatasetRequest) (DatasetBinding, error)
	// UpdateBindingKeyFingerprint replaces the key fingerprint of a dataset
	// bound, not detached, to the request's installation, record UID and
	// generation, and returns the stored binding. It is a compare-and-swap:
	// the stored fingerprint must equal PreviousKeyFingerprint, or already
	// equal KeyFingerprint, which returns the binding unchanged. Anything else
	// returns a [BindingConflictError]. BoundAt does not change.
	UpdateBindingKeyFingerprint(ctx context.Context, request UpdateBindingKeyRequest) (DatasetBinding, error)
	// CompleteDatasetBootstrap records that the dataset finished its first
	// start: Orka activated the snapshot key for it. It never becomes
	// incomplete again, so the dataset can no longer be bound with
	// [BindFresh] or receive a newly minted key. Repeating it succeeds. It does
	// not depend on the binding.
	CompleteDatasetBootstrap(ctx context.Context, datasetID string) error
}
