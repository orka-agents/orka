package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var _ storagev1.ControllerEpochAdvancer = (*Store)(nil)

// AdvanceControllerEpoch implements storagev1.ControllerEpochAdvancer. The
// store has one connection, so the advance is serialized with every write
// operation: a write that checked the previous fence has committed or rolled
// back before the advance runs, and every later write checks the new one.
func (s *Store) AdvanceControllerEpoch(
	ctx context.Context, request storagev1.AdvanceControllerEpochRequest,
) (storagev1.ControllerEpochMirrorState, error) {
	if err := request.Validate(); err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	if err := s.requireOwnTransaction(ctx, "AdvanceControllerEpoch"); err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	name, err := store.NormalizeControllerEpochName(request.Epoch.Name)
	if err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	epoch := request.Epoch
	epoch.Name = name
	epoch.AcquiredAt = epoch.AcquiredAt.UTC()
	epoch.UpdatedAt = epoch.UpdatedAt.UTC()
	var state storagev1.ControllerEpochMirrorState
	err = s.inWriteTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := requireDataset(tx, request.DatasetID); err != nil {
			return err
		}
		binding, err := readDatasetBinding(ctx, tx)
		if err != nil {
			return err
		}
		if binding.installation != "" && binding.installation != request.InstallationID {
			return store.FencedErrorf("dataset belongs to installation %q, not %q", binding.installation, request.InstallationID)
		}
		if err := binding.refuseDetached(); err != nil {
			return err
		}
		now := s.now()
		if err := advanceControllerEpochTx(ctx, tx, epoch, now); err != nil {
			return err
		}
		if binding.installation == "" {
			// The dataset now belongs to this installation, which has not
			// bound it itself: no record and switch generation 0.
			if _, err := tx.ExecContext(ctx,
				`UPDATE store_dataset SET installation_id = ?, bound_at_ns = ? WHERE singleton = 1`,
				request.InstallationID, now.UnixNano(),
			); err != nil {
				return fmt.Errorf("bind dataset installation: %w", err)
			}
		}
		state, err = getControllerEpochMirrorState(ctx, tx, name)
		return err
	})
	if err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	return state, nil
}

// advanceControllerEpochTx stores epoch under the forward-only rules: a
// first epoch is stored, a lower one is fenced, an identical one changes
// nothing, any other record at the same epoch conflicts, and a higher one
// replaces the stored epoch and records it as previous.
func advanceControllerEpochTx(ctx context.Context, tx *sql.Tx, epoch storagev1.ControllerEpoch, now time.Time) error {
	current, err := getControllerEpoch(ctx, tx, epoch.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO controller_epochs(name, epoch, holder_id, request_digest, version, acquired_at, updated_at, advanced_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			epoch.Name, epoch.Epoch, epoch.HolderID, epoch.RequestDigest, epoch.Version,
			epoch.AcquiredAt, epoch.UpdatedAt, now,
		); err != nil {
			return fmt.Errorf("store controller epoch: %w", err)
		}
		return nil
	case err != nil:
		return err
	case epoch.Epoch < current.Epoch:
		return store.FencedErrorf("controller epoch %q is at %d, the advance is to %d", epoch.Name, current.Epoch, epoch.Epoch)
	case epoch.Epoch == current.Epoch:
		if epoch.HolderID != current.HolderID || epoch.RequestDigest != current.RequestDigest {
			return store.ConflictErrorf("controller epoch %q already holds epoch %d with a different holder or digest", epoch.Name, epoch.Epoch)
		}
		return nil
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE controller_epochs
		 SET epoch = ?, holder_id = ?, request_digest = ?, version = ?, acquired_at = ?, updated_at = ?,
		     previous_epoch = ?, previous_holder_id = ?, advanced_at = ?
		 WHERE name = ? AND epoch = ? AND holder_id = ?`,
		epoch.Epoch, epoch.HolderID, epoch.RequestDigest, epoch.Version, epoch.AcquiredAt, epoch.UpdatedAt,
		current.Epoch, current.HolderID, now,
		epoch.Name, current.Epoch, current.HolderID,
	)
	if err != nil {
		return fmt.Errorf("advance controller epoch: %w", err)
	}
	return rowsAffectedExactlyOne(result, "controller epoch")
}

// GetControllerEpochMirror implements storagev1.ControllerEpochAdvancer.
func (s *Store) GetControllerEpochMirror(ctx context.Context, name string) (storagev1.ControllerEpochMirrorState, error) {
	name, err := store.NormalizeControllerEpochName(name)
	if err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	var state storagev1.ControllerEpochMirrorState
	err = s.readSnapshot(ctx, func(q taskDataExecutor) error {
		var err error
		state, err = getControllerEpochMirrorState(ctx, q, name)
		return err
	})
	if err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	return state, nil
}

// getControllerEpochMirrorState reads the epoch the dataset holds under name,
// with the dataset and installation it belongs to.
func getControllerEpochMirrorState(ctx context.Context, q taskDataExecutor, name string) (storagev1.ControllerEpochMirrorState, error) {
	epoch, err := getControllerEpoch(ctx, q, name)
	if err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	record, err := readDataset(q)
	if err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	state := storagev1.ControllerEpochMirrorState{DatasetID: record.id, Epoch: epoch}
	if state.InstallationID, err = readDatasetInstallation(ctx, q); err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	var previousEpoch sql.NullInt64
	var previousHolder sql.NullString
	var advancedAt sql.NullTime
	if err := q.QueryRowContext(ctx,
		`SELECT previous_epoch, previous_holder_id, advanced_at FROM controller_epochs WHERE name = ?`, name,
	).Scan(&previousEpoch, &previousHolder, &advancedAt); err != nil {
		return storagev1.ControllerEpochMirrorState{}, fmt.Errorf("read controller epoch advance: %w", err)
	}
	if previousEpoch.Valid {
		state.Previous = &storagev1.ControllerEpochFence{Name: name, Epoch: previousEpoch.Int64, HolderID: previousHolder.String}
	}
	if advancedAt.Valid {
		state.AdvancedAt = advancedAt.Time.UTC()
	}
	return state, nil
}

// readDatasetInstallation returns the installation the dataset is bound to,
// or "" when it is bound to none.
func readDatasetInstallation(ctx context.Context, q controlQueryRower) (string, error) {
	var installation string
	if err := q.QueryRowContext(ctx, `SELECT installation_id FROM store_dataset WHERE singleton = 1`).Scan(&installation); err != nil {
		return "", fmt.Errorf("read dataset installation: %w", err)
	}
	return installation, nil
}
