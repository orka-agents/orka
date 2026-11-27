package v1

import (
	"context"
	"time"
)

type ControllerEpochMirror interface {
	SyncControllerEpochMirror(ctx context.Context, epoch ControllerEpoch) error
}

type AdvanceControllerEpochRequest struct {
	DatasetID         string
	Epoch             ControllerEpoch
	InstallationID    string
	AcknowledgedEpoch int64
}

func (r AdvanceControllerEpochRequest) Validate() error {
	if err := ValidateControlIdentifier("advance controller epoch dataset ID", r.DatasetID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("advance controller epoch installation ID", r.InstallationID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("controller epoch name", r.Epoch.Name); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("controller epoch holder ID", r.Epoch.HolderID); err != nil {
		return err
	}
	if err := ValidateCanonicalDigest("controller epoch request digest", r.Epoch.RequestDigest); err != nil {
		return err
	}
	if r.Epoch.Epoch < 1 || r.Epoch.Version < 1 {
		return ValidationErrorf("controller epoch and version must be at least 1")
	}
	if r.AcknowledgedEpoch < 0 || r.AcknowledgedEpoch > r.Epoch.Epoch {
		return ValidationErrorf("acknowledged epoch must be between zero and the requested epoch")
	}
	if r.Epoch.AcquiredAt.IsZero() || r.Epoch.UpdatedAt.IsZero() {
		return ValidationErrorf("controller epoch acquired and updated times are required")
	}
	return nil
}

type ControllerEpochMirrorState struct {
	DatasetID      string
	Epoch          ControllerEpoch
	InstallationID string
	Previous       *ControllerEpochFence
	AdvancedAt     time.Time
}

type ControllerEpochAdvancer interface {
	AdvanceControllerEpoch(ctx context.Context, request AdvanceControllerEpochRequest) (ControllerEpochMirrorState, error)
	GetControllerEpochMirror(ctx context.Context, name string) (ControllerEpochMirrorState, error)
}
