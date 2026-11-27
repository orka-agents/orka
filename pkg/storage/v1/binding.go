package v1

import (
	"context"
	"fmt"
	"time"
)

var ErrDatasetDetached = fmt.Errorf("%w: dataset detached from its installation", ErrNotReady)

type DatasetBindingState string

const (
	DatasetBound    DatasetBindingState = "bound"
	DatasetDetached DatasetBindingState = "detached"
)

func IsValidDatasetBindingState(state DatasetBindingState) bool {
	switch state {
	case DatasetBound, DatasetDetached:
		return true
	default:
		return false
	}
}

type DatasetBinding struct {
	InstallationID   string
	RecordUID        string
	SwitchGeneration int64
	KeyFingerprint   string
	State            DatasetBindingState
	BoundAt          time.Time
}

type BindMode string

const (
	BindFresh    BindMode = "fresh"
	BindEmbedded BindMode = "embedded"
	BindMigrated BindMode = "migrated"
	BindAdopt    BindMode = "adopt"
	BindRebind   BindMode = "rebind"
)

func IsValidBindMode(mode BindMode) bool {
	switch mode {
	case BindFresh, BindEmbedded, BindMigrated, BindAdopt, BindRebind:
		return true
	default:
		return false
	}
}

type BindDatasetRequest struct {
	DatasetID        string
	InstallationID   string
	RecordUID        string
	SwitchGeneration int64
	KeyFingerprint   string
	Mode             BindMode
}

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

type DetachDatasetRequest struct {
	DatasetID        string
	InstallationID   string
	RecordUID        string
	SwitchGeneration int64
}

func (r DetachDatasetRequest) Validate() error {
	return validateBindingIdentity("detach dataset", r.DatasetID, r.InstallationID, r.RecordUID, r.SwitchGeneration)
}

type UpdateBindingKeyRequest struct {
	DatasetID              string
	InstallationID         string
	RecordUID              string
	SwitchGeneration       int64
	PreviousKeyFingerprint string
	KeyFingerprint         string
}

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

type BindingConflictError struct {
	DatasetID string
	Binding   *DatasetBinding
	Reason    string
}

func (e BindingConflictError) Error() string {
	return fmt.Sprintf("dataset %q binding conflict: %s", e.DatasetID, e.Reason)
}

func (e BindingConflictError) Is(target error) bool {
	return target == ErrConflict
}

type DatasetBindingStore interface {
	BindDataset(ctx context.Context, request BindDatasetRequest) (DatasetBinding, error)
	GetDatasetBinding(ctx context.Context, datasetID string) (DatasetBinding, error)
	DetachDatasetBinding(ctx context.Context, request DetachDatasetRequest) (DatasetBinding, error)
	UpdateBindingKeyFingerprint(ctx context.Context, request UpdateBindingKeyRequest) (DatasetBinding, error)
	CompleteDatasetBootstrap(ctx context.Context, datasetID string) error
}
