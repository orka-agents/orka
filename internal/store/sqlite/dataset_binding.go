package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var _ storagev1.DatasetBindingStore = (*Store)(nil)

// Binding states stored in store_dataset.binding_state. An empty state means
// BindDataset has not bound the dataset: it belongs to no installation, or to
// the one a controller epoch advance recorded in installation_id.
const (
	bindingStateNone     = ""
	bindingStateBound    = string(storagev1.DatasetBound)
	bindingStateDetached = string(storagev1.DatasetDetached)
)

// datasetBinding is the installation binding the dataset row holds.
type datasetBinding struct {
	installation   string
	recordUID      string
	generation     int64
	keyFingerprint string
	state          string
	boundAtNanos   int64
}

// readDatasetBinding reads and checks the dataset's installation binding. A
// row whose binding columns contradict one another is refused, never
// repaired.
func readDatasetBinding(ctx context.Context, q controlQueryRower) (datasetBinding, error) {
	var binding datasetBinding
	if err := q.QueryRowContext(ctx,
		`SELECT installation_id, record_uid, switch_generation, key_fingerprint, binding_state, bound_at_ns
		 FROM store_dataset WHERE singleton = 1`,
	).Scan(&binding.installation, &binding.recordUID, &binding.generation, &binding.keyFingerprint,
		&binding.state, &binding.boundAtNanos); err != nil {
		return datasetBinding{}, fmt.Errorf("read SQLite dataset binding: %w", err)
	}
	if err := binding.validate(); err != nil {
		return datasetBinding{}, fmt.Errorf("invalid SQLite dataset binding: %w", err)
	}
	return binding, nil
}

func (b datasetBinding) validate() error {
	for field, value := range map[string]string{
		"installation ID": b.installation, "record UID": b.recordUID, "key fingerprint": b.keyFingerprint,
	} {
		if value != "" {
			if err := storagev1.ValidateControlIdentifier(field, value); err != nil {
				return err
			}
		}
	}
	if b.boundAtNanos < 0 {
		return fmt.Errorf("negative bind time")
	}
	switch b.state {
	case bindingStateNone:
		if b.recordUID != "" || b.generation != 0 || b.keyFingerprint != "" {
			return fmt.Errorf("a dataset that was never bound holds binding details")
		}
		if b.installation == "" && b.boundAtNanos != 0 {
			return fmt.Errorf("an unbound dataset holds a bind time")
		}
	case bindingStateBound, bindingStateDetached:
		if b.installation == "" || b.recordUID == "" || b.generation < 1 {
			return fmt.Errorf("a %s dataset lacks its installation, record or generation", b.state)
		}
	default:
		return fmt.Errorf("unknown binding state %q", b.state)
	}
	return nil
}

// public returns the binding as the contract reports it, or nil when the
// dataset belongs to no installation.
func (b datasetBinding) public() *storagev1.DatasetBinding {
	if b.installation == "" {
		return nil
	}
	binding := &storagev1.DatasetBinding{
		InstallationID:   b.installation,
		RecordUID:        b.recordUID,
		SwitchGeneration: b.generation,
		KeyFingerprint:   b.keyFingerprint,
		State:            storagev1.DatasetBound,
	}
	if b.state == bindingStateDetached {
		binding.State = storagev1.DatasetDetached
	}
	if b.boundAtNanos != 0 {
		binding.BoundAt = time.Unix(0, b.boundAtNanos).UTC()
	}
	return binding
}

// unclaimedFor reports whether installation may bind the dataset as a new
// binding: it belongs to no installation, or only a controller epoch advance
// recorded installation.
func (b datasetBinding) unclaimedFor(installation string) bool {
	return b.installation == "" || (b.installation == installation && b.state == bindingStateNone)
}

// requireNotDetached refuses a write on a detached dataset.
func requireNotDetached(ctx context.Context, q controlQueryRower) error {
	binding, err := readDatasetBinding(ctx, q)
	if err != nil {
		return err
	}
	return binding.refuseDetached()
}

// refuseDetached returns ErrDatasetDetached for a detached dataset.
func (b datasetBinding) refuseDetached() error {
	if b.state != bindingStateDetached {
		return nil
	}
	return fmt.Errorf("%w: installation %q left the dataset at switch generation %d",
		storagev1.ErrDatasetDetached, b.installation, b.generation)
}

// BindDataset implements storagev1.DatasetBindingStore. The store has one
// connection and reserves SQLite's writer before reading the binding, so the
// check and the bind are one step with respect to every other call.
func (s *Store) BindDataset(ctx context.Context, request storagev1.BindDatasetRequest) (storagev1.DatasetBinding, error) {
	if err := request.Validate(); err != nil {
		return storagev1.DatasetBinding{}, err
	}
	if request.Mode == storagev1.BindEmbedded && !s.ownsProcessLock {
		return storagev1.DatasetBinding{}, store.ValidationErrorf(
			"an embedded binding requires a SQLite store opened under its process lock")
	}
	return s.changeDatasetBinding(ctx, "BindDataset", request.DatasetID,
		func(ctx context.Context, tx *sql.Tx, stored datasetBinding) (datasetBinding, error) {
			if stored.state == bindingStateBound && stored.installation == request.InstallationID &&
				stored.recordUID == request.RecordUID && stored.generation == request.SwitchGeneration &&
				stored.keyFingerprint == request.KeyFingerprint {
				return stored, nil
			}
			if stored.installation != "" && stored.installation != request.InstallationID {
				return datasetBinding{}, bindingConflict(request.DatasetID, stored,
					"the dataset belongs to installation %q, not %q", stored.installation, request.InstallationID)
			}
			if err := s.checkBindMode(ctx, tx, request, stored); err != nil {
				return datasetBinding{}, err
			}
			return datasetBinding{
				installation:   request.InstallationID,
				recordUID:      request.RecordUID,
				generation:     request.SwitchGeneration,
				keyFingerprint: request.KeyFingerprint,
				state:          bindingStateBound,
				boundAtNanos:   s.now().UnixNano(),
			}, nil
		})
}

// checkBindMode checks the mode's precondition against a dataset that belongs
// to the request's installation or to none.
func (s *Store) checkBindMode(
	ctx context.Context, tx *sql.Tx, request storagev1.BindDatasetRequest, stored datasetBinding,
) error {
	unclaimed := stored.unclaimedFor(request.InstallationID)
	higher := stored.generation < request.SwitchGeneration
	switch request.Mode {
	case storagev1.BindFresh:
		if !unclaimed {
			return bindingConflict(request.DatasetID, stored, "a fresh binding needs an unclaimed dataset")
		}
		record, err := readDataset(tx)
		if err != nil {
			return err
		}
		if record.origin != datasetOriginNew {
			return bindingConflict(request.DatasetID, stored, "the dataset was adopted, not created new")
		}
		if record.complete {
			return bindingConflict(request.DatasetID, stored, "the dataset has completed its bootstrap")
		}
		populated, err := datasetHasRecords(ctx, tx)
		if err != nil {
			return err
		}
		if populated {
			return bindingConflict(request.DatasetID, stored, "the dataset holds records")
		}
	case storagev1.BindEmbedded:
		if !unclaimed {
			return bindingConflict(request.DatasetID, stored, "an embedded binding needs an unclaimed dataset")
		}
	case storagev1.BindMigrated:
		// SQLite creates or adopts its dataset; it never imports one.
		return bindingConflict(request.DatasetID, stored, "the dataset was not imported from another provider")
	case storagev1.BindAdopt:
		if !unclaimed && (stored.state != bindingStateDetached || !higher) {
			return bindingConflict(request.DatasetID, stored,
				"adopting needs an unclaimed dataset, or one detached from this installation at a lower generation")
		}
	case storagev1.BindRebind:
		if stored.installation == "" || !higher {
			return bindingConflict(request.DatasetID, stored,
				"rebinding needs a dataset of this installation at a lower generation")
		}
	default:
		return store.ValidationErrorf("bind dataset mode %q is not defined", request.Mode)
	}
	return nil
}

// GetDatasetBinding implements storagev1.DatasetBindingStore.
func (s *Store) GetDatasetBinding(ctx context.Context, datasetID string) (storagev1.DatasetBinding, error) {
	if err := storagev1.ValidateControlIdentifier("dataset binding dataset ID", datasetID); err != nil {
		return storagev1.DatasetBinding{}, err
	}
	var binding *storagev1.DatasetBinding
	err := s.readSnapshot(ctx, func(q taskDataExecutor) error {
		if err := requireDataset(q, datasetID); err != nil {
			return err
		}
		stored, err := readDatasetBinding(ctx, q)
		if err != nil {
			return err
		}
		binding = stored.public()
		return nil
	})
	if err != nil {
		return storagev1.DatasetBinding{}, err
	}
	if binding == nil {
		return storagev1.DatasetBinding{}, fmt.Errorf("%w: dataset %q is not bound", storagev1.ErrNotFound, datasetID)
	}
	return *binding, nil
}

// DetachDatasetBinding implements storagev1.DatasetBindingStore.
func (s *Store) DetachDatasetBinding(ctx context.Context, request storagev1.DetachDatasetRequest) (storagev1.DatasetBinding, error) {
	if err := request.Validate(); err != nil {
		return storagev1.DatasetBinding{}, err
	}
	return s.changeDatasetBinding(ctx, "DetachDatasetBinding", request.DatasetID,
		func(_ context.Context, _ *sql.Tx, stored datasetBinding) (datasetBinding, error) {
			if stored.installation != request.InstallationID {
				return datasetBinding{}, bindingConflict(request.DatasetID, stored,
					"the dataset does not belong to installation %q", request.InstallationID)
			}
			if stored.state == bindingStateDetached {
				if stored.recordUID == request.RecordUID && stored.generation == request.SwitchGeneration {
					return stored, nil
				}
				return datasetBinding{}, bindingConflict(request.DatasetID, stored,
					"the dataset was detached under another record or generation")
			}
			if stored.generation >= request.SwitchGeneration {
				return datasetBinding{}, bindingConflict(request.DatasetID, stored,
					"the dataset is bound at generation %d, not below %d", stored.generation, request.SwitchGeneration)
			}
			detached := stored
			detached.recordUID = request.RecordUID
			detached.generation = request.SwitchGeneration
			detached.state = bindingStateDetached
			detached.boundAtNanos = s.now().UnixNano()
			return detached, nil
		})
}

// UpdateBindingKeyFingerprint implements storagev1.DatasetBindingStore.
func (s *Store) UpdateBindingKeyFingerprint(
	ctx context.Context, request storagev1.UpdateBindingKeyRequest,
) (storagev1.DatasetBinding, error) {
	if err := request.Validate(); err != nil {
		return storagev1.DatasetBinding{}, err
	}
	return s.changeDatasetBinding(ctx, "UpdateBindingKeyFingerprint", request.DatasetID,
		func(_ context.Context, _ *sql.Tx, stored datasetBinding) (datasetBinding, error) {
			if stored.state != bindingStateBound || stored.installation != request.InstallationID ||
				stored.recordUID != request.RecordUID || stored.generation != request.SwitchGeneration {
				return datasetBinding{}, bindingConflict(request.DatasetID, stored,
					"the dataset is not bound to installation %q under record %q at generation %d",
					request.InstallationID, request.RecordUID, request.SwitchGeneration)
			}
			switch stored.keyFingerprint {
			case request.KeyFingerprint:
				return stored, nil
			case request.PreviousKeyFingerprint:
				updated := stored
				updated.keyFingerprint = request.KeyFingerprint
				return updated, nil
			default:
				return datasetBinding{}, bindingConflict(request.DatasetID, stored,
					"the binding holds another key fingerprint than the one expected")
			}
		})
}

// CompleteDatasetBootstrap implements storagev1.DatasetBindingStore. Like the
// completion the snapshot key activation records, it synchronizes the WAL
// before returning, so the flag survives a host failure.
func (s *Store) CompleteDatasetBootstrap(ctx context.Context, datasetID string) error {
	if err := storagev1.ValidateControlIdentifier("complete dataset bootstrap dataset ID", datasetID); err != nil {
		return err
	}
	if err := s.requireOwnTransaction(ctx, "CompleteDatasetBootstrap"); err != nil {
		return err
	}
	err := s.inWriteTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := requireDataset(tx, datasetID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE store_dataset SET bootstrap_complete = 1 WHERE singleton = 1 AND bootstrap_complete = 0`)
		return err
	})
	if err != nil {
		return err
	}
	return synchronizeDatasetMetadata(s.db)
}

// changeDatasetBinding runs change against the stored binding in one write
// transaction and stores what it returns. It refuses to run inside another
// transaction, and synchronizes the WAL after every successful call, including
// one that changed nothing, so a retry after a failed synchronization still
// makes the binding durable before it returns.
func (s *Store) changeDatasetBinding(
	ctx context.Context,
	method string,
	datasetID string,
	change func(context.Context, *sql.Tx, datasetBinding) (datasetBinding, error),
) (storagev1.DatasetBinding, error) {
	if err := s.requireOwnTransaction(ctx, method); err != nil {
		return storagev1.DatasetBinding{}, err
	}
	var result datasetBinding
	err := s.inWriteTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := requireDataset(tx, datasetID); err != nil {
			return err
		}
		stored, err := readDatasetBinding(ctx, tx)
		if err != nil {
			return err
		}
		result, err = change(ctx, tx, stored)
		if err != nil {
			return err
		}
		if result == stored {
			return nil
		}
		if err := result.validate(); err != nil {
			return fmt.Errorf("%s produced an invalid binding: %w", method, err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE store_dataset
			 SET installation_id = ?, record_uid = ?, switch_generation = ?, key_fingerprint = ?, binding_state = ?, bound_at_ns = ?
			 WHERE singleton = 1`,
			result.installation, result.recordUID, result.generation, result.keyFingerprint, result.state, result.boundAtNanos,
		)
		if err != nil {
			return fmt.Errorf("store dataset binding: %w", err)
		}
		return nil
	})
	if err != nil {
		return storagev1.DatasetBinding{}, err
	}
	if err := synchronizeDatasetMetadata(s.db); err != nil {
		return storagev1.DatasetBinding{}, err
	}
	return *result.public(), nil
}

func bindingConflict(datasetID string, stored datasetBinding, format string, args ...any) error {
	return storagev1.BindingConflictError{
		DatasetID: datasetID,
		Binding:   stored.public(),
		Reason:    fmt.Sprintf(format, args...),
	}
}
