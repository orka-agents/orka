package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var _ storagev1.OperationReceiptStore = (*Store)(nil)

const (
	// receiptRetention is how long SQLite keeps an operation receipt or an
	// aborted record after recording it.
	receiptRetention = storagev1.MinReceiptRetention
	// maxReceiptResultBytes is the largest result SQLite keeps with a
	// receipt: SQLite's own value limit. SQLite keeps every result whole, so
	// a replay never reads records the write changed again.
	maxReceiptResultBytes = 1_000_000_000
	// receiptPruneBatch bounds one pruning statement of the maintenance loop.
	receiptPruneBatch = 1000
)

const (
	operationCommitted = string(storagev1.OperationCommitted)
	operationAborted   = string(storagev1.OperationAborted)
)

// operationReceiptSchemaStatements defines the operation receipt table. Its
// layout is frozen in migrations/168-to-169.sql.
func operationReceiptSchemaStatements() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS operation_receipts (
 operation_id TEXT PRIMARY KEY,
 method TEXT NOT NULL,
 request_digest TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('committed', 'aborted')),
 fence_name TEXT NOT NULL,
 fence_epoch INTEGER NOT NULL,
 fence_holder TEXT NOT NULL,
 issued_at_ns INTEGER NOT NULL,
 recorded_at_ns INTEGER NOT NULL,
 result BLOB
)`,
		`CREATE INDEX IF NOT EXISTS idx_operation_receipts_recorded_at
 ON operation_receipts(recorded_at_ns)`,
	}
}

// RequireWriteEnvelopes makes the store refuse every contract write that
// carries no write envelope, with ErrValidation, as the contract requires of
// a provider. Production calls it once the store is open; package tests
// leave it off so they can write without an envelope. Embedded families,
// which are outside the method catalog, are not affected.
func (s *Store) RequireWriteEnvelopes() {
	s.requireWriteEnvelopes.Store(true)
}

// writeOperationKey marks a context inside a write operation's body, so a
// store method the body calls runs as part of that operation.
type writeOperationKey struct{}

// noResult is the result of a write that returns only an error.
type noResult struct{}

// resultPair is the result of a write that returns two values.
type resultPair[A, B any] struct {
	First  A
	Second B
}

// writeOperation runs body as one call of the contract write method.
//
// Without a write envelope in ctx, a store that requires envelopes, as
// production does, refuses the write with ErrValidation, and any other store
// runs body unchanged, as package tests do. With one, writeOperation runs the
// contract's commit-time checks in the transaction that commits the write, in
// this order: the dataset, the operation receipt, the operation's age and the
// clock, the dataset state and the controller epoch fence. Body then runs in
// the same transaction, which every store method it reaches joins, and its
// result is recorded with the operation receipt before the transaction
// commits. A replay of a committed operation returns the recorded result
// without running body. A refusal, or an error from body, commits nothing and
// records no receipt.
//
// explicitFence is the ControllerEpochFence a method takes as a parameter, or
// nil. With an envelope it must equal the envelope's fence.
//
// Inside an enclosing task-data transaction, the operation runs under a
// savepoint of that transaction, so a failed operation leaves nothing behind
// even if the caller goes on; the enclosing transaction commits it.
func writeOperation[R any](
	ctx context.Context,
	s *Store,
	method string,
	explicitFence *storagev1.ControllerEpochFence,
	body func(context.Context) (R, error),
) (R, error) {
	var zero R
	if ctx.Value(writeOperationKey{}) != nil {
		return body(ctx)
	}
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		if s.requireWriteEnvelopes.Load() {
			return zero, store.ValidationErrorf("%s requires a write envelope", method)
		}
		return body(ctx)
	}
	if info, known := storagev1.LookupMethod(method); !known || info.Kind != storagev1.MethodWrite {
		return zero, fmt.Errorf("SQLite write operation %q is not a contract write method", method)
	}
	if err := envelope.Validate(); err != nil {
		return zero, err
	}
	if explicitFence != nil {
		if err := requireEnvelopeFence(*explicitFence, envelope.Fence); err != nil {
			return zero, err
		}
	}
	var result R
	err := s.inWriteTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		stored, replay, err := s.checkWrite(ctx, tx, method, envelope)
		if err != nil {
			return err
		}
		if replay {
			result, err = decodeReceiptResult[R](method, stored)
			return err
		}
		result, err = body(context.WithValue(ctx, writeOperationKey{}, method))
		if err != nil {
			return err
		}
		encoded, err := encodeReceiptResult(method, result)
		if err != nil {
			return err
		}
		return insertOperationReceipt(ctx, tx, operationReceipt{
			operationID: envelope.OperationID,
			method:      method,
			digest:      envelope.RequestDigest,
			state:       operationCommitted,
			fence:       envelope.Fence,
			issuedAt:    envelope.IssuedAt,
			recordedAt:  s.now(),
			result:      encoded,
		})
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}

// writeOperationErr is writeOperation for a method that returns only an
// error and takes no explicit fence.
func writeOperationErr(
	ctx context.Context,
	s *Store,
	method string,
	body func(context.Context) error,
) error {
	_, err := writeOperation(ctx, s, method, nil, func(ctx context.Context) (noResult, error) {
		return noResult{}, body(ctx)
	})
	return err
}

// writeOperationInto is writeOperation for a method that also writes into the
// record its caller passed, such as an assigned ID or a filled time: the
// record as the write left it is kept with the result, and a replay writes it
// back. A nil record is left to body to refuse.
func writeOperationInto[T, R any](
	ctx context.Context,
	s *Store,
	method string,
	explicitFence *storagev1.ControllerEpochFence,
	record *T,
	body func(context.Context) (R, error),
) (R, error) {
	if record == nil {
		return writeOperation(ctx, s, method, explicitFence, body)
	}
	result, err := writeOperation(ctx, s, method, explicitFence, func(ctx context.Context) (resultPair[R, T], error) {
		value, err := body(ctx)
		return resultPair[R, T]{First: value, Second: *record}, err
	})
	if err != nil {
		return result.First, err
	}
	*record = result.Second
	return result.First, nil
}

// requireOwnTransaction refuses an administrative method called inside
// another transaction. Its outcome is durable when it returns, which an
// enclosing transaction that may still roll back cannot promise.
func (s *Store) requireOwnTransaction(ctx context.Context, method string) error {
	if s.taskDataTx(ctx) != nil || ctx.Value(writeOperationKey{}) != nil {
		return store.ValidationErrorf("%s must not run inside another transaction", method)
	}
	return nil
}

// requireEnvelopeFence checks that a method's explicit fence equals the
// envelope's fence.
func requireEnvelopeFence(explicit, envelope storagev1.ControllerEpochFence) error {
	normalized, err := storagev1.NormalizeEpochFence(explicit)
	if err != nil {
		return err
	}
	if normalized != envelope {
		return store.ValidationErrorf("controller epoch fence %s/%d/%s does not match the write envelope fence %s/%d/%s",
			normalized.Name, normalized.Epoch, normalized.HolderID, envelope.Name, envelope.Epoch, envelope.HolderID)
	}
	return nil
}

// inWriteTransaction runs run in the write transaction ctx carries, under a
// savepoint, or in a new transaction that it commits. A new transaction
// reserves SQLite's writer before run reads anything, so an operation's
// receipt check and its commit see the same state even across connections.
func (s *Store) inWriteTransaction(ctx context.Context, run func(context.Context, *sql.Tx) error) error {
	if tx := s.taskDataTx(ctx); tx != nil {
		if _, err := tx.ExecContext(ctx, `SAVEPOINT orka_write_operation`); err != nil {
			return err
		}
		if err := run(ctx, tx); err != nil {
			_, rollbackErr := tx.ExecContext(context.WithoutCancel(ctx), `ROLLBACK TO orka_write_operation`)
			_, releaseErr := tx.ExecContext(context.WithoutCancel(ctx), `RELEASE orka_write_operation`)
			return errors.Join(err, rollbackErr, releaseErr)
		}
		_, err := tx.ExecContext(context.WithoutCancel(ctx), `RELEASE orka_write_operation`)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// BeginTx is deferred in SQLite. A write statement reserves the writer even
	// when its predicate matches no rows.
	if _, err := tx.ExecContext(ctx, `UPDATE operation_receipts SET method = method WHERE 0`); err != nil {
		return err
	}
	ctx = context.WithValue(ctx, taskDataTransactionKey{}, taskDataTransaction{db: s.db, tx: tx})
	if err := run(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// checkWrite runs the commit-time checks before a write's own
// preconditions. It returns the stored result and true for a replay of a
// committed operation.
func (s *Store) checkWrite(
	ctx context.Context, tx *sql.Tx, method string, envelope storagev1.WriteEnvelope,
) ([]byte, bool, error) {
	if err := requireDataset(tx, envelope.DatasetID); err != nil {
		return nil, false, err
	}
	receipt, found, err := readOperationReceipt(ctx, tx, envelope.OperationID)
	if err != nil {
		return nil, false, err
	}
	if found {
		if err := receipt.matches(method, envelope.RequestDigest); err != nil {
			return nil, false, err
		}
		if receipt.state == operationAborted {
			return nil, false, fmt.Errorf("%w: operation %q", storagev1.ErrOperationAborted, envelope.OperationID)
		}
		return receipt.result, true, nil
	}
	now := s.now()
	if now.Sub(envelope.IssuedAt) > storagev1.MaxOperationAge {
		return nil, false, fmt.Errorf("%w: operation %q was issued at %s, more than %s ago",
			storagev1.ErrOperationExpired, envelope.OperationID, envelope.IssuedAt.UTC().Format(time.RFC3339Nano), storagev1.MaxOperationAge)
	}
	if envelope.IssuedAt.Sub(now) > storagev1.MaxClockSkew {
		return nil, false, fmt.Errorf("%w: operation %q was issued at %s, more than %s ahead of the store clock",
			storagev1.ErrClockSkew, envelope.OperationID, envelope.IssuedAt.UTC().Format(time.RFC3339Nano), storagev1.MaxClockSkew)
	}
	if state := s.datasetState(); state != storagev1.DatasetReady {
		return nil, false, store.NotReadyErrorf("SQLite dataset is %s", state)
	}
	if err := requireControllerEpoch(ctx, tx, envelope.Fence); err != nil {
		return nil, false, err
	}
	return nil, false, nil
}

// requireDataset checks that datasetID names this store's dataset.
func requireDataset(q datasetReader, datasetID string) error {
	record, err := readDataset(q)
	if err != nil {
		return err
	}
	if record.id != datasetID {
		return fmt.Errorf("%w: the request names dataset %q, this store holds %q", storagev1.ErrDatasetMismatch, datasetID, record.id)
	}
	return nil
}

// now is the provider clock.
func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

// datasetState is whether the dataset accepts writes. An open SQLite store
// always does: it migrates its schema while opening, under the process lock.
func (s *Store) datasetState() storagev1.DatasetState {
	if s.writeState != nil {
		return s.writeState()
	}
	return storagev1.DatasetReady
}

type operationReceipt struct {
	operationID string
	method      string
	digest      string
	state       string
	fence       storagev1.ControllerEpochFence
	issuedAt    time.Time
	recordedAt  time.Time
	result      []byte
}

func (r operationReceipt) matches(method, digest string) error {
	if r.method != method || r.digest != digest {
		return fmt.Errorf("%w: operation %q was recorded for a different %s request",
			storagev1.ErrDuplicateMismatch, r.operationID, r.method)
	}
	return nil
}

func (r operationReceipt) outcome() storagev1.OperationOutcome {
	return storagev1.OperationOutcome{
		OperationID:   r.operationID,
		Method:        r.method,
		RequestDigest: r.digest,
		State:         storagev1.OperationState(r.state),
		Fence:         r.fence,
		IssuedAt:      r.issuedAt,
		RecordedAt:    r.recordedAt,
	}
}

func readOperationReceipt(ctx context.Context, q controlQueryRower, operationID string) (operationReceipt, bool, error) {
	receipt := operationReceipt{operationID: operationID}
	var issuedAt, recordedAt int64
	err := q.QueryRowContext(ctx,
		`SELECT method, request_digest, state, fence_name, fence_epoch, fence_holder, issued_at_ns, recorded_at_ns, result
		 FROM operation_receipts WHERE operation_id = ?`,
		operationID,
	).Scan(&receipt.method, &receipt.digest, &receipt.state, &receipt.fence.Name, &receipt.fence.Epoch,
		&receipt.fence.HolderID, &issuedAt, &recordedAt, &receipt.result)
	if errors.Is(err, sql.ErrNoRows) {
		return operationReceipt{}, false, nil
	}
	if err != nil {
		return operationReceipt{}, false, fmt.Errorf("read operation receipt: %w", err)
	}
	receipt.issuedAt = time.Unix(0, issuedAt).UTC()
	receipt.recordedAt = time.Unix(0, recordedAt).UTC()
	return receipt, true, nil
}

func insertOperationReceipt(ctx context.Context, tx *sql.Tx, receipt operationReceipt) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO operation_receipts
		 (operation_id, method, request_digest, state, fence_name, fence_epoch, fence_holder, issued_at_ns, recorded_at_ns, result)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		receipt.operationID, receipt.method, receipt.digest, receipt.state,
		receipt.fence.Name, receipt.fence.Epoch, receipt.fence.HolderID,
		receipt.issuedAt.UnixNano(), receipt.recordedAt.UnixNano(), receipt.result,
	)
	if err != nil {
		return fmt.Errorf("record operation receipt: %w", err)
	}
	return nil
}

// encodeReceiptResult encodes a write's result for its receipt with the
// receipt codec, so a replay returns a value equal to the original.
func encodeReceiptResult[R any](method string, result R) ([]byte, error) {
	if _, ok := any(result).(noResult); ok {
		return nil, nil
	}
	encoded, err := marshalReceiptValue(result)
	if err != nil {
		return nil, fmt.Errorf("encode %s result for its operation receipt: %w", method, err)
	}
	if len(encoded) > maxReceiptResultBytes {
		return nil, fmt.Errorf("%w: %s result is %d bytes, more than an operation receipt keeps", storagev1.ErrTooLarge, method, len(encoded))
	}
	return encoded, nil
}

func decodeReceiptResult[R any](method string, data []byte) (R, error) {
	var value R
	if _, ok := any(value).(noResult); ok {
		return value, nil
	}
	if err := unmarshalReceiptValue(data, &value); err != nil {
		return value, fmt.Errorf("decode %s result from its operation receipt: %w", method, err)
	}
	return value, nil
}

// ResolveOperation implements storagev1.OperationReceiptStore.
func (s *Store) ResolveOperation(ctx context.Context, request storagev1.ResolveOperationRequest) (storagev1.OperationOutcome, error) {
	if err := request.Validate(); err != nil {
		return storagev1.OperationOutcome{}, err
	}
	if err := s.requireOwnTransaction(ctx, "ResolveOperation"); err != nil {
		return storagev1.OperationOutcome{}, err
	}
	var outcome storagev1.OperationOutcome
	err := s.inWriteTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := requireDataset(tx, request.DatasetID); err != nil {
			return err
		}
		receipt, found, err := readOperationReceipt(ctx, tx, request.OperationID)
		if err != nil {
			return err
		}
		if found {
			if err := receipt.matches(request.Method, request.RequestDigest); err != nil {
				return err
			}
			outcome = receipt.outcome()
			return nil
		}
		if state := s.datasetState(); state != storagev1.DatasetReady {
			return store.NotReadyErrorf("SQLite dataset is %s", state)
		}
		now := s.now()
		if request.IssuedAt.Sub(now) > storagev1.MaxClockSkew {
			return fmt.Errorf("%w: operation %q was issued more than %s ahead of the store clock",
				storagev1.ErrClockSkew, request.OperationID, storagev1.MaxClockSkew)
		}
		// A receipt the operation left is kept at least MinReceiptRetention
		// after it was recorded, and was recorded no earlier than IssuedAt
		// minus MaxClockSkew. Past that bound a missing receipt proves nothing.
		if now.Sub(request.IssuedAt) > storagev1.MinReceiptRetention-storagev1.MaxClockSkew {
			return fmt.Errorf("%w: operation %q is too old to establish whether it committed",
				storagev1.ErrOperationExpired, request.OperationID)
		}
		aborted := operationReceipt{
			operationID: request.OperationID,
			method:      request.Method,
			digest:      request.RequestDigest,
			state:       operationAborted,
			issuedAt:    request.IssuedAt.UTC(),
			recordedAt:  now,
		}
		if err := insertOperationReceipt(ctx, tx, aborted); err != nil {
			return err
		}
		outcome = aborted.outcome()
		return nil
	})
	if err != nil {
		return storagev1.OperationOutcome{}, err
	}
	return outcome, nil
}

// PruneOperationReceipts implements storagev1.OperationReceiptStore.
func (s *Store) PruneOperationReceipts(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, store.ValidationErrorf("operation receipt prune limit must be at least 1")
	}
	cutoff := s.now().Add(-receiptRetention).UnixNano()
	result, err := s.taskDataExecutor(ctx).ExecContext(ctx,
		`DELETE FROM operation_receipts WHERE rowid IN (
		   SELECT rowid FROM operation_receipts WHERE recorded_at_ns < ? ORDER BY recorded_at_ns LIMIT ?
		 )`,
		cutoff, limit,
	)
	if err != nil {
		return 0, fmt.Errorf("prune operation receipts: %w", err)
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(removed), nil
}

// pruneExpiredOperationReceipts removes every receipt past its retention in
// bounded batches.
func (s *Store) pruneExpiredOperationReceipts(ctx context.Context) error {
	for {
		removed, err := s.PruneOperationReceipts(ctx, receiptPruneBatch)
		if err != nil || removed < receiptPruneBatch {
			return err
		}
	}
}

// writeTx is the transaction a write method runs in. Inside a write
// operation or a task-data transaction it is a savepoint of that transaction:
// Commit releases the savepoint and Rollback undoes the method's writes, so
// the method behaves as it does on its own, and the owner commits the result
// together with its operation receipt.
type writeTx struct {
	*sql.Tx
	savepoint string
	done      bool
}

// writeSavepoints names the savepoints of joined write transactions, so
// nested ones never share a name.
var writeSavepoints atomic.Uint64

// Commit commits a transaction writeTx began, or releases its savepoint.
func (t *writeTx) Commit() error {
	if t.savepoint == "" {
		return t.Tx.Commit()
	}
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	_, err := t.ExecContext(context.Background(), `RELEASE `+t.savepoint)
	return err
}

// Rollback rolls back a transaction writeTx began, or undoes the writes made
// since its savepoint. After Commit it does nothing.
func (t *writeTx) Rollback() error {
	if t.savepoint == "" {
		return t.Tx.Rollback()
	}
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	_, rollbackErr := t.ExecContext(context.Background(), `ROLLBACK TO `+t.savepoint)
	_, releaseErr := t.ExecContext(context.Background(), `RELEASE `+t.savepoint)
	return errors.Join(rollbackErr, releaseErr)
}

// securityWriteStore returns the store a security write runs on. Security
// queries use the store's security transaction, so inside a write operation
// they run on a store whose security transaction is the operation's.
func (s *Store) securityWriteStore(ctx context.Context) *Store {
	if s.securityTx != nil {
		return s
	}
	if tx := s.taskDataTx(ctx); tx != nil {
		return &Store{db: s.db, securityTx: tx}
	}
	return s
}

// beginTx begins a transaction for a write method, or joins the write
// operation or task-data transaction ctx carries. SQLite has one connection,
// so a write method never opens a second transaction while ctx carries one.
func (s *Store) beginTx(ctx context.Context) (*writeTx, error) {
	if tx := s.taskDataTx(ctx); tx != nil {
		name := fmt.Sprintf("orka_write_%d", writeSavepoints.Add(1))
		if _, err := tx.ExecContext(ctx, `SAVEPOINT `+name); err != nil {
			return nil, err
		}
		return &writeTx{Tx: tx, savepoint: name}, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &writeTx{Tx: tx}, nil
}
