package v1

import (
	"context"
	"time"
)

// ControllerEpochMirror receives the exact epoch acquired from the
// Kubernetes-authoritative ControllerEpochStore. Mirrors are subordinate
// fencing records for SQLite payload transactions; they never select or
// advance controller ownership themselves.
type ControllerEpochMirror interface {
	SyncControllerEpochMirror(ctx context.Context, epoch ControllerEpoch) error
}

// AdvanceControllerEpochRequest moves one dataset's controller epoch forward
// to the epoch Kubernetes granted.
type AdvanceControllerEpochRequest struct {
	// DatasetID is the dataset to advance. A different dataset returns
	// ErrDatasetMismatch.
	DatasetID string
	// Epoch is the exact record the Kubernetes compare-and-swap produced.
	Epoch ControllerEpoch
	// InstallationID identifies the Orka installation that owns the dataset.
	// Orka passes the UID of the installation's ControllerEpoch object.
	InstallationID string
}

// Validate checks that the dataset, the installation and every field of the
// epoch record are present and well formed.
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
	if r.Epoch.AcquiredAt.IsZero() || r.Epoch.UpdatedAt.IsZero() {
		return ValidationErrorf("controller epoch acquired and updated times are required")
	}
	return nil
}

// ControllerEpochMirrorState is the controller epoch one dataset holds.
type ControllerEpochMirrorState struct {
	// DatasetID is the dataset that holds this epoch. Each dataset holds its
	// own.
	DatasetID string
	// Epoch is the stored epoch record.
	Epoch ControllerEpoch
	// InstallationID is the installation the dataset is bound to.
	InstallationID string
	// Previous is the epoch this one replaced, or nil when the dataset held
	// none before it.
	Previous *ControllerEpochFence
	// AdvancedAt is the provider clock when the epoch was stored.
	AdvancedAt time.Time
}

// ControllerEpochAdvancer is the forward-only, installation-bound form of the
// controller epoch mirror. Both providers implement it, and Orka uses it in
// place of [ControllerEpochMirror.SyncControllerEpochMirror]: once Orka
// advances a dataset with AdvanceControllerEpoch, it no longer calls
// SyncControllerEpochMirror on it, and a provider that implements both
// refuses SyncControllerEpochMirror on a dataset bound to an installation
// with a [FencedError], as rule 3 refuses an advance from another
// installation. A dataset holds one epoch for each epoch name, and Orka uses
// only [DefaultControllerEpochName]; the epoch rules below apply per name, and
// the installation binding belongs to the dataset as a whole.
//
// AdvanceControllerEpoch follows these rules. A refused advance commits
// nothing.
//
//  1. A DatasetID other than the provider's dataset returns
//     [ErrDatasetMismatch].
//  2. A dataset with no epoch under the name stores the request's epoch, and
//     binds its InstallationID when the dataset has no installation yet; a
//     dataset bound to another installation is refused by rule 3 first.
//  3. An InstallationID other than the bound one returns a [FencedError]: the
//     dataset belongs to another installation. Moving a dataset to another
//     installation is an explicit storage switch, never this method.
//  4. An epoch lower than the stored one returns a [FencedError]: the caller
//     is a stale controller.
//  5. An equal epoch with the same holder and request digest succeeds without
//     changing the stored epoch. Anything else at an equal epoch returns
//     [ErrConflict].
//  6. A higher epoch is stored, and Previous reports the epoch it replaced.
//     Epochs may jump forward, because Kubernetes can advance several times
//     without a store advance when a controller stops right after acquiring.
//  7. A dataset that holds an epoch but no installation, because it was
//     written before installations were recorded, binds the InstallationID of
//     the first advance that succeeds under rule 5 or 6, in the same commit.
//  8. The advance is linearized with fenced writes: it waits for writes in
//     progress under the stored fence, and once it returns, no write carrying
//     an older fence commits on this dataset.
//
// The advance carries no [WriteEnvelope] and leaves no operation receipt: it
// establishes the fence, and it is idempotent by epoch and request digest.
// At takeover, Orka advances every distinct dataset it is bound to and opens
// each dataset's fence for writes once that dataset's own advance succeeds.
type ControllerEpochAdvancer interface {
	// AdvanceControllerEpoch stores a newer epoch for the dataset under the
	// rules above and returns the state the dataset now holds.
	AdvanceControllerEpoch(ctx context.Context, request AdvanceControllerEpochRequest) (ControllerEpochMirrorState, error)
	// GetControllerEpochMirror returns the epoch the dataset holds under name,
	// or ErrNotFound when it holds none.
	GetControllerEpochMirror(ctx context.Context, name string) (ControllerEpochMirrorState, error)
}
