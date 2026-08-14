/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package referenceadapter

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/orka-agents/orka/pkg/oms/protocol"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite" // pure-Go SQLite driver registration
)

const (
	schemaVersion                          = 2
	maxSearchSnapshotBytes                 = protocol.MaxAdapterResponseBytes
	maxActiveSearchSnapshotsPerAuthority   = 8
	maxActiveSearchSnapshotsGlobal         = 64
	maxRetainedSearchSnapshotsPerAuthority = 8
	maxRetainedSearchSnapshotsGlobal       = 64
)

var (
	errIdentityConflict = errors.New("binding is not the claimed owner")
	errRoutingFenced    = errors.New("routing epoch is below the durable fence")
	errRoutingFuture    = errors.New("routing epoch is above the durable fence")
	errSnapshotInvalid  = errors.New("pagination snapshot is invalid")
	errSnapshotExpired  = errors.New("pagination snapshot expired")
	errSnapshotCapacity = errors.New("pagination snapshot exceeds adapter capacity")
)

type database struct {
	db   *sql.DB
	lock *processLock
}

type processLock struct {
	file *os.File
	info os.FileInfo
}

var processLockRegistry = struct {
	sync.Mutex
	held []os.FileInfo
}{}

type ownershipDecision struct {
	result        string
	claimIdentity string
	maxRouting    uint64
	claimedAt     time.Time
}

type storedRecord struct {
	record protocol.MemoryRecord
}

type searchPage struct {
	actualMode string
	records    []protocol.MemoryRecord
	nextToken  string
	exhausted  bool
	expiresAt  time.Time
}

func openDatabase(ctx context.Context, path string) (*database, error) {
	path = strings.TrimSpace(path)
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("a plain durable SQLite database path is required")
	}
	cleanPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	lock, err := acquireProcessLock(ctx, cleanPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", cleanPath)
	if err != nil {
		_ = lock.close()
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			_ = lock.close()
			return nil, fmt.Errorf("configure SQLite: %w", err)
		}
	}
	result := &database{db: db, lock: lock}
	if err := result.migrate(ctx); err != nil {
		_ = result.close()
		return nil, err
	}
	return result, nil
}

func acquireProcessLock(ctx context.Context, path string) (*processLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	target, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open process lock target: %w", err)
	}
	info, err := target.Stat()
	if err != nil {
		_ = target.Close()
		return nil, fmt.Errorf("stat process lock target: %w", err)
	}
	if !reserveProcessLock(info) {
		_ = target.Close()
		return nil, errors.New("adapter database is already locked by another active process")
	}

	lockFile := target
	if runtime.GOOS != "linux" {
		lockFile, err = openCanonicalProcessLock(info)
		_ = target.Close()
		if err != nil {
			releaseProcessLock(info)
			return nil, err
		}
	}
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		releaseProcessLock(info)
		_ = lockFile.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("adapter database is already locked by another active process: %w", err)
		}
		return nil, fmt.Errorf("acquire process lock: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
		releaseProcessLock(info)
		_ = lockFile.Close()
		return nil, err
	}
	return &processLock{file: lockFile, info: info}, nil
}

func openCanonicalProcessLock(info os.FileInfo) (*os.File, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("resolve process lock inode identity")
	}
	directory := filepath.Join(os.TempDir(), "orka-oms-process-locks")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create canonical process lock directory: %w", err)
	}
	path := filepath.Join(directory, fmt.Sprintf("%x-%x.lock", stat.Dev, stat.Ino))
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open canonical process lock: %w", err)
	}
	return file, nil
}

func reserveProcessLock(info os.FileInfo) bool {
	processLockRegistry.Lock()
	defer processLockRegistry.Unlock()
	for _, held := range processLockRegistry.held {
		if os.SameFile(info, held) {
			return false
		}
	}
	processLockRegistry.held = append(processLockRegistry.held, info)
	return true
}

func releaseProcessLock(info os.FileInfo) {
	processLockRegistry.Lock()
	defer processLockRegistry.Unlock()
	for i, held := range processLockRegistry.held {
		if os.SameFile(info, held) {
			processLockRegistry.held = append(processLockRegistry.held[:i], processLockRegistry.held[i+1:]...)
			return
		}
	}
}

func (l *processLock) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var result error
	if err := unix.Flock(int(l.file.Fd()), unix.LOCK_UN); err != nil {
		result = err
	}
	releaseProcessLock(l.info)
	if err := l.file.Close(); err != nil && result == nil {
		result = err
	}
	l.file = nil
	l.info = nil
	return result
}

func (d *database) close() error {
	if d == nil {
		return nil
	}
	var result error
	if d.db != nil {
		if err := d.db.Close(); err != nil {
			result = err
		}
	}
	if err := d.lock.close(); err != nil && result == nil {
		result = err
	}
	return result
}

func (d *database) migrate(ctx context.Context) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin OMS migration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	statements := []string{
		`CREATE TABLE IF NOT EXISTS oms_schema (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			version INTEGER NOT NULL
		)`,
		`INSERT INTO oms_schema(id, version) VALUES(1, 2)
			ON CONFLICT(id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS store_resolutions (
			tenant_id TEXT NOT NULL,
			store_name TEXT NOT NULL,
			store_uuid TEXT NOT NULL,
			cluster_id TEXT NOT NULL,
			namespace_uid TEXT NOT NULL,
			created_by_backend_uid TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(tenant_id, store_name),
			UNIQUE(store_uuid)
		)`,
		`CREATE TABLE IF NOT EXISTS ownership_claims (
			claim_scope_digest TEXT PRIMARY KEY,
			authority_digest TEXT NOT NULL,
			cluster_id TEXT NOT NULL,
			namespace_uid TEXT NOT NULL,
			backend_uid TEXT NOT NULL,
			authority_epoch INTEGER NOT NULL,
			tenant_id TEXT NOT NULL,
			store_uuid TEXT NOT NULL,
			max_routing_epoch INTEGER NOT NULL,
			claimed_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS operation_receipts (
			authority_digest TEXT NOT NULL,
			operation_id TEXT NOT NULL,
			mutation_digest TEXT NOT NULL,
			request_json BLOB NOT NULL,
			receipt_json BLOB NOT NULL,
			completed_at TEXT NOT NULL,
			PRIMARY KEY(authority_digest, operation_id)
		)`,
		`CREATE TABLE IF NOT EXISTS memory_records (
			authority_digest TEXT NOT NULL,
			upsert_key TEXT NOT NULL,
			memory_id TEXT NOT NULL,
			state TEXT NOT NULL,
			generation INTEGER NOT NULL,
			backend_version TEXT NOT NULL,
			backend_memory_id TEXT NOT NULL,
			content_digest TEXT NOT NULL,
			content TEXT NOT NULL,
			tags_json BLOB NOT NULL,
			metadata_json BLOB NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(authority_digest, upsert_key)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_records_search
			ON memory_records(authority_digest, state, upsert_key)`,
		`CREATE TABLE IF NOT EXISTS record_versions (
			authority_digest TEXT NOT NULL,
			upsert_key TEXT NOT NULL,
			generation INTEGER NOT NULL,
			operation_id TEXT NOT NULL,
			state TEXT NOT NULL,
			record_json BLOB NOT NULL,
			completed_at TEXT NOT NULL,
			PRIMARY KEY(authority_digest, upsert_key, generation)
		)`,
		`CREATE TABLE IF NOT EXISTS pagination_snapshots (
			snapshot_id TEXT PRIMARY KEY,
			authority_digest TEXT NOT NULL,
			request_fingerprint TEXT NOT NULL,
			requested_mode TEXT NOT NULL,
			actual_mode TEXT NOT NULL,
			page_size INTEGER NOT NULL,
			entry_count INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			terminal INTEGER NOT NULL DEFAULT 0 CHECK (terminal IN (0, 1))
		)`,
		`CREATE TABLE IF NOT EXISTS pagination_entries (
			snapshot_id TEXT NOT NULL,
			position INTEGER NOT NULL,
			record_json BLOB NOT NULL,
			PRIMARY KEY(snapshot_id, position),
			FOREIGN KEY(snapshot_id) REFERENCES pagination_snapshots(snapshot_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_pagination_snapshots_expiry
			ON pagination_snapshots(expires_at)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("run OMS migration: %w", err)
		}
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM oms_schema WHERE id = 1`).Scan(&version); err != nil {
		return fmt.Errorf("read OMS schema version: %w", err)
	}
	if version == 1 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE pagination_snapshots
			ADD COLUMN terminal INTEGER NOT NULL DEFAULT 0 CHECK (terminal IN (0, 1))`); err != nil {
			return fmt.Errorf("upgrade OMS pagination schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oms_schema SET version = ? WHERE id = 1`, schemaVersion); err != nil {
			return fmt.Errorf("record OMS schema upgrade: %w", err)
		}
		version = schemaVersion
	}
	if version != schemaVersion {
		return fmt.Errorf("unsupported OMS schema version %d", version)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit OMS migration: %w", err)
	}
	return nil
}

func (d *database) resolveStore(ctx context.Context, binding protocol.StoreResolutionBinding, storeName string, now time.Time) (string, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck
	var storeUUID string
	err = tx.QueryRowContext(ctx, `SELECT store_uuid FROM store_resolutions
		WHERE tenant_id = ? AND store_name = ?`, binding.TenantID, storeName).Scan(&storeUUID)
	if errors.Is(err, sql.ErrNoRows) {
		storeUUID = uuid.NewString()
		_, err = tx.ExecContext(ctx, `INSERT INTO store_resolutions(
			tenant_id, store_name, store_uuid, cluster_id, namespace_uid, created_by_backend_uid, created_at
		) VALUES(?, ?, ?, ?, ?, ?, ?)`, binding.TenantID, storeName, storeUUID,
			binding.ClusterID, binding.NamespaceUID, binding.BackendUID, formatTime(now))
		if err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return storeUUID, nil
}

func (d *database) claimOwnership(ctx context.Context, binding protocol.Binding, now time.Time) (ownershipDecision, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ownershipDecision{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	scope := protocol.ClaimScopeDigest(binding)
	authority := protocol.AuthorityDigest(binding)
	var resolvedStore int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM store_resolutions WHERE tenant_id = ? AND store_uuid = ?`,
		binding.TenantID, binding.StoreUUID).Scan(&resolvedStore); errors.Is(err, sql.ErrNoRows) {
		return ownershipDecision{result: protocol.ResultIdentityConflict, maxRouting: 0, claimedAt: now}, nil
	} else if err != nil {
		return ownershipDecision{}, err
	}
	var existingAuthority, claimedAtRaw string
	var maxRouting int64
	err = tx.QueryRowContext(ctx, `SELECT authority_digest, max_routing_epoch, claimed_at
		FROM ownership_claims WHERE claim_scope_digest = ?`, scope).Scan(&existingAuthority, &maxRouting, &claimedAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		claimedAtRaw = formatTime(now)
		_, err = tx.ExecContext(ctx, `INSERT INTO ownership_claims(
			claim_scope_digest, authority_digest, cluster_id, namespace_uid, backend_uid, authority_epoch,
			tenant_id, store_uuid, max_routing_epoch, claimed_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, scope, authority, binding.ClusterID,
			binding.NamespaceUID, binding.BackendUID, binding.AuthorityEpoch, binding.TenantID,
			binding.StoreUUID, binding.RoutingEpoch, claimedAtRaw, claimedAtRaw)
		if err != nil {
			return ownershipDecision{}, err
		}
		maxRouting = int64(binding.RoutingEpoch)
	} else if err != nil {
		return ownershipDecision{}, err
	} else if existingAuthority != authority {
		claimedAt, parseErr := parseTime(claimedAtRaw)
		if parseErr != nil {
			return ownershipDecision{}, parseErr
		}
		return ownershipDecision{result: protocol.ResultIdentityConflict, claimIdentity: existingAuthority, maxRouting: uint64(maxRouting), claimedAt: claimedAt}, nil
	}
	claimedAt, err := parseTime(claimedAtRaw)
	if err != nil {
		return ownershipDecision{}, err
	}
	if err := tx.Commit(); err != nil {
		return ownershipDecision{}, err
	}
	return ownershipDecision{result: protocol.ResultApplied, claimIdentity: authority, maxRouting: uint64(maxRouting), claimedAt: claimedAt}, nil
}

func (d *database) advanceRoutingFence(ctx context.Context, binding protocol.Binding, now time.Time) (ownershipDecision, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return ownershipDecision{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	owner, maxRouting, claimedAt, err := ownershipInTx(ctx, tx, binding)
	if err != nil {
		return ownershipDecision{}, err
	}
	decision := ownershipDecision{result: protocol.ResultApplied, claimIdentity: protocol.AuthorityDigest(binding), maxRouting: maxRouting, claimedAt: claimedAt}
	if !owner {
		decision.result = protocol.ResultIdentityConflict
		return decision, nil
	}
	if binding.RoutingEpoch < maxRouting {
		decision.result = protocol.ResultPreconditionFailed
		return decision, nil
	}
	if binding.RoutingEpoch > maxRouting {
		_, err = tx.ExecContext(ctx, `UPDATE ownership_claims
			SET max_routing_epoch = ?, updated_at = ? WHERE claim_scope_digest = ?`,
			binding.RoutingEpoch, formatTime(now), protocol.ClaimScopeDigest(binding))
		if err != nil {
			return ownershipDecision{}, err
		}
		decision.maxRouting = binding.RoutingEpoch
	}
	if err := tx.Commit(); err != nil {
		return ownershipDecision{}, err
	}
	return decision, nil
}

func ownershipInTx(ctx context.Context, tx *sql.Tx, binding protocol.Binding) (bool, uint64, time.Time, error) {
	var authority, claimedAtRaw string
	var maxRouting int64
	err := tx.QueryRowContext(ctx, `SELECT authority_digest, max_routing_epoch, claimed_at
		FROM ownership_claims WHERE claim_scope_digest = ?`, protocol.ClaimScopeDigest(binding)).
		Scan(&authority, &maxRouting, &claimedAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, time.Time{}, nil
	}
	if err != nil {
		return false, 0, time.Time{}, err
	}
	claimedAt, err := parseTime(claimedAtRaw)
	if err != nil {
		return false, 0, time.Time{}, err
	}
	return authority == protocol.AuthorityDigest(binding), uint64(maxRouting), claimedAt, nil
}

func ensureOwnedInTx(ctx context.Context, tx *sql.Tx, binding protocol.Binding) error {
	owner, maxRouting, _, err := ownershipInTx(ctx, tx, binding)
	if err != nil {
		return err
	}
	if !owner {
		return errIdentityConflict
	}
	if binding.RoutingEpoch < maxRouting {
		return errRoutingFenced
	}
	return nil
}

//nolint:gocyclo // Mutation semantics are kept together to preserve transactional fencing guarantees.
func (d *database) applyMutation(ctx context.Context, request *protocol.MutationEnvelope, now time.Time) (protocol.MutationReceipt, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return protocol.MutationReceipt{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	authority := protocol.AuthorityDigest(request.Binding)
	if existing, digest, found, lookupErr := lookupReceiptInTx(ctx, tx, authority, request.OperationID); lookupErr != nil {
		return protocol.MutationReceipt{}, lookupErr
	} else if found {
		if digest == request.MutationDigest {
			return existing, nil
		}
		return conflictReceipt(request, protocol.ResultIdempotencyConflict, now), nil
	}
	owner, maxRouting, _, err := ownershipInTx(ctx, tx, request.Binding)
	if err != nil {
		return protocol.MutationReceipt{}, err
	}
	if !owner {
		receipt := conflictReceipt(request, protocol.ResultIdentityConflict, now)
		if err := persistReceiptInTx(ctx, tx, request, receipt); err != nil {
			return protocol.MutationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return protocol.MutationReceipt{}, err
		}
		return receipt, nil
	}
	if request.Binding.RoutingEpoch != maxRouting {
		if request.Binding.RoutingEpoch > maxRouting {
			return protocol.MutationReceipt{}, errRoutingFuture
		}
		receipt := conflictReceipt(request, protocol.ResultIdentityConflict, now)
		if err := persistReceiptInTx(ctx, tx, request, receipt); err != nil {
			return protocol.MutationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return protocol.MutationReceipt{}, err
		}
		return receipt, nil
	}

	record, found, err := lookupRecordInTx(ctx, tx, authority, request.UpsertKey)
	if err != nil {
		return protocol.MutationReceipt{}, err
	}
	result := protocol.ResultApplied
	currentGeneration := uint64(0)
	currentVersion := ""
	live := found && record.record.State == protocol.RecordStateLive
	if found {
		currentGeneration = record.record.Generation
		currentVersion = record.record.BackendVersion
	}

	preconditionFailed := func() (protocol.MutationReceipt, error) {
		receipt := conflictReceipt(request, protocol.ResultPreconditionFailed, now)
		if err := persistReceiptInTx(ctx, tx, request, receipt); err != nil {
			return protocol.MutationReceipt{}, err
		}
		if err := tx.Commit(); err != nil {
			return protocol.MutationReceipt{}, err
		}
		return receipt, nil
	}

	switch request.Kind {
	case protocol.MutationKindCreate:
		if live || request.ExpectedGeneration != currentGeneration || request.Generation <= currentGeneration {
			return preconditionFailed()
		}
	case protocol.MutationKindReplace:
		if !live || request.ExpectedGeneration != currentGeneration || request.Generation <= currentGeneration ||
			(request.ExpectedBackendVersion != "" && request.ExpectedBackendVersion != currentVersion) {
			return preconditionFailed()
		}
	case protocol.MutationKindDelete:
		if request.ExpectedGeneration != currentGeneration || request.Generation <= currentGeneration ||
			(request.ExpectedBackendVersion != "" && request.ExpectedBackendVersion != currentVersion) {
			return preconditionFailed()
		}
		if !live {
			result = protocol.ResultNotFound
		}
	}

	backendVersion := "ref-v" + strconv.FormatUint(request.Generation, 10)
	backendMemoryID := backendMemoryID(request.UpsertKey)
	recordValue := protocol.MemoryRecord{
		MemoryID: request.MemoryID, UpsertKey: request.UpsertKey,
		Generation: request.Generation, BackendVersion: backendVersion,
		BackendMemoryID: backendMemoryID, ContentDigest: request.ContentDigest, UpdatedAt: now,
		Tags: []string{}, Metadata: map[string]string{},
	}
	if request.Kind == protocol.MutationKindDelete {
		recordValue.State = protocol.RecordStateTombstone
	} else {
		recordValue.State = protocol.RecordStateLive
		recordValue.Content = request.State.Content
		recordValue.Tags = append([]string(nil), request.State.Tags...)
		recordValue.Metadata = cloneMap(request.State.Metadata)
	}
	if err := upsertRecordInTx(ctx, tx, authority, recordValue); err != nil {
		return protocol.MutationReceipt{}, err
	}
	receipt := protocol.MutationReceipt{
		ProtocolVersion: protocol.Version, Binding: request.Binding, Result: result,
		OperationID: request.OperationID, BindingDigest: protocol.BindingDigest(request.Binding),
		AppliedGeneration: request.Generation, BackendVersion: backendVersion,
		BackendMemoryID: backendMemoryID, ContentDigest: request.ContentDigest,
		MutationDigest: request.MutationDigest, CompletedAt: now,
	}
	if err := persistVersionInTx(ctx, tx, authority, request.OperationID, recordValue); err != nil {
		return protocol.MutationReceipt{}, err
	}
	if err := persistReceiptInTx(ctx, tx, request, receipt); err != nil {
		return protocol.MutationReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.MutationReceipt{}, err
	}
	return receipt, nil
}

func conflictReceipt(request *protocol.MutationEnvelope, result string, now time.Time) protocol.MutationReceipt {
	return protocol.MutationReceipt{
		ProtocolVersion: protocol.Version, Binding: request.Binding, Result: result,
		OperationID: request.OperationID, BindingDigest: protocol.BindingDigest(request.Binding),
		ContentDigest: request.ContentDigest, MutationDigest: request.MutationDigest, CompletedAt: now,
	}
}

func lookupReceiptInTx(ctx context.Context, tx *sql.Tx, authority, operationID string) (protocol.MutationReceipt, string, bool, error) {
	var digest string
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT mutation_digest, receipt_json FROM operation_receipts
		WHERE authority_digest = ? AND operation_id = ?`, authority, operationID).Scan(&digest, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.MutationReceipt{}, "", false, nil
	}
	if err != nil {
		return protocol.MutationReceipt{}, "", false, err
	}
	receipt, err := protocol.DecodeMutationReceipt(data)
	if err != nil {
		return protocol.MutationReceipt{}, "", false, fmt.Errorf("stored mutation receipt is invalid: %w", err)
	}
	return *receipt, digest, true, nil
}

func persistReceiptInTx(ctx context.Context, tx *sql.Tx, request *protocol.MutationEnvelope, receipt protocol.MutationReceipt) error {
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return err
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operation_receipts(
		authority_digest, operation_id, mutation_digest, request_json, receipt_json, completed_at
	) VALUES(?, ?, ?, ?, ?, ?)`, protocol.AuthorityDigest(request.Binding), request.OperationID,
		request.MutationDigest, requestJSON, receiptJSON, formatTime(receipt.CompletedAt))
	return err
}

func lookupRecordInTx(ctx context.Context, tx *sql.Tx, authority, upsertKey string) (storedRecord, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT memory_id, upsert_key, state, generation, backend_version,
		backend_memory_id, content_digest, content, tags_json, metadata_json, updated_at
		FROM memory_records WHERE authority_digest = ? AND upsert_key = ?`, authority, upsertKey)
	record, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRecord{}, false, nil
	}
	if err != nil {
		return storedRecord{}, false, err
	}
	return storedRecord{record: record}, true, nil
}

func (d *database) lookupRecord(ctx context.Context, binding protocol.Binding, upsertKey string) (protocol.MemoryRecord, bool, error) {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return protocol.MemoryRecord{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := ensureOwnedInTx(ctx, tx, binding); err != nil {
		return protocol.MemoryRecord{}, false, err
	}
	row := tx.QueryRowContext(ctx, `SELECT memory_id, upsert_key, state, generation, backend_version,
		backend_memory_id, content_digest, content, tags_json, metadata_json, updated_at
		FROM memory_records WHERE authority_digest = ? AND upsert_key = ?`, protocol.AuthorityDigest(binding), upsertKey)
	record, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return protocol.MemoryRecord{}, false, err
		}
		return protocol.MemoryRecord{}, false, nil
	}
	if err != nil {
		return protocol.MemoryRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return protocol.MemoryRecord{}, false, err
	}
	return record, true, nil
}

func (d *database) lookupOperation(ctx context.Context, binding protocol.Binding, operationID string) (protocol.MutationReceipt, bool, error) {
	tx, err := d.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return protocol.MutationReceipt{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := ensureOwnedInTx(ctx, tx, binding); err != nil {
		return protocol.MutationReceipt{}, false, err
	}
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT receipt_json FROM operation_receipts
		WHERE authority_digest = ? AND operation_id = ?`, protocol.AuthorityDigest(binding), operationID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return protocol.MutationReceipt{}, false, err
		}
		return protocol.MutationReceipt{}, false, nil
	}
	if err != nil {
		return protocol.MutationReceipt{}, false, err
	}
	receipt, err := protocol.DecodeMutationReceipt(data)
	if err != nil {
		return protocol.MutationReceipt{}, false, fmt.Errorf("stored mutation receipt is invalid: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return protocol.MutationReceipt{}, false, err
	}
	return *receipt, true, nil
}

func upsertRecordInTx(ctx context.Context, tx *sql.Tx, authority string, record protocol.MemoryRecord) error {
	tags, err := json.Marshal(record.Tags)
	if err != nil {
		return err
	}
	metadata, err := json.Marshal(record.Metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_records(
		authority_digest, upsert_key, memory_id, state, generation, backend_version, backend_memory_id,
		content_digest, content, tags_json, metadata_json, updated_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(authority_digest, upsert_key) DO UPDATE SET
		memory_id=excluded.memory_id, state=excluded.state, generation=excluded.generation,
		backend_version=excluded.backend_version, backend_memory_id=excluded.backend_memory_id,
		content_digest=excluded.content_digest, content=excluded.content, tags_json=excluded.tags_json,
		metadata_json=excluded.metadata_json, updated_at=excluded.updated_at`,
		authority, record.UpsertKey, record.MemoryID, record.State, record.Generation,
		record.BackendVersion, record.BackendMemoryID, record.ContentDigest, record.Content,
		tags, metadata, formatTime(record.UpdatedAt))
	return err
}

func persistVersionInTx(ctx context.Context, tx *sql.Tx, authority, operationID string, record protocol.MemoryRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO record_versions(
		authority_digest, upsert_key, generation, operation_id, state, record_json, completed_at
	) VALUES(?, ?, ?, ?, ?, ?, ?)`, authority, record.UpsertKey, record.Generation,
		operationID, record.State, data, formatTime(record.UpdatedAt))
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row rowScanner) (protocol.MemoryRecord, error) {
	var record protocol.MemoryRecord
	var generation int64
	var tagsJSON, metadataJSON []byte
	var updatedAtRaw string
	if err := row.Scan(&record.MemoryID, &record.UpsertKey, &record.State, &generation,
		&record.BackendVersion, &record.BackendMemoryID, &record.ContentDigest, &record.Content,
		&tagsJSON, &metadataJSON, &updatedAtRaw); err != nil {
		return protocol.MemoryRecord{}, err
	}
	record.Generation = uint64(generation)
	if err := json.Unmarshal(tagsJSON, &record.Tags); err != nil {
		return protocol.MemoryRecord{}, err
	}
	if err := json.Unmarshal(metadataJSON, &record.Metadata); err != nil {
		return protocol.MemoryRecord{}, err
	}
	updatedAt, err := parseTime(updatedAtRaw)
	if err != nil {
		return protocol.MemoryRecord{}, err
	}
	record.UpdatedAt = updatedAt
	return record, nil
}

func (d *database) search(ctx context.Context, request *protocol.SearchRequest, now time.Time, ttl time.Duration, maxRecords int) (searchPage, error) {
	if request.PageToken == "" {
		return d.createSnapshot(ctx, request, now, ttl, maxRecords)
	}
	return d.readSnapshotPage(ctx, request, now)
}

func (d *database) createSnapshot(ctx context.Context, request *protocol.SearchRequest, now time.Time, ttl time.Duration, maxRecords int) (searchPage, error) {
	actualMode := request.Mode
	if actualMode == protocol.SearchModeAuto {
		actualMode = protocol.SearchModeKeyword
	}
	fingerprint := searchFingerprint(request)
	expiresAt := now.Add(ttl)
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return searchPage{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := ensureOwnedInTx(ctx, tx, request.Binding); err != nil {
		return searchPage{}, err
	}
	if err := deleteExpiredSearchSnapshotsInTx(ctx, tx, now); err != nil {
		return searchPage{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT memory_id, upsert_key, state, generation, backend_version,
		backend_memory_id, content_digest, content, tags_json, metadata_json, updated_at
		FROM memory_records WHERE authority_digest = ? AND state = ? ORDER BY upsert_key ASC`,
		protocol.AuthorityDigest(request.Binding), protocol.RecordStateLive)
	if err != nil {
		return searchPage{}, err
	}
	defer rows.Close() //nolint:errcheck
	query := strings.ToLower(strings.TrimSpace(request.Query))
	records := make([]protocol.MemoryRecord, 0)
	encodedRecords := make([][]byte, 0)
	snapshotBytes := 0
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return searchPage{}, err
		}
		if query != "" && !recordMatchesKeyword(record, query) {
			continue
		}
		if len(records) >= maxRecords {
			return searchPage{}, errSnapshotCapacity
		}
		data, err := json.Marshal(record)
		if err != nil {
			return searchPage{}, err
		}
		if len(data) > maxSearchSnapshotBytes-snapshotBytes {
			return searchPage{}, errSnapshotCapacity
		}
		snapshotBytes += len(data)
		records = append(records, record)
		encodedRecords = append(encodedRecords, data)
	}
	if err := rows.Err(); err != nil {
		return searchPage{}, err
	}
	if err := rows.Close(); err != nil {
		return searchPage{}, err
	}
	snapshotID, err := randomSnapshotID()
	if err != nil {
		return searchPage{}, err
	}
	firstEnd := min(request.PageSize, len(records))
	firstPageRecords := append([]protocol.MemoryRecord(nil), records[:firstEnd]...)
	firstPage := pageFromRecords(snapshotID, 0, len(records), actualMode, expiresAt, firstPageRecords)
	if firstPage.exhausted {
		if err := tx.Commit(); err != nil {
			return searchPage{}, err
		}
		return firstPage, nil
	}
	if err := ensureSearchSnapshotCountCapacityInTx(ctx, tx, protocol.AuthorityDigest(request.Binding)); err != nil {
		return searchPage{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pagination_snapshots(
		snapshot_id, authority_digest, request_fingerprint, requested_mode, actual_mode, page_size,
		entry_count, created_at, expires_at
	) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`, snapshotID, protocol.AuthorityDigest(request.Binding),
		fingerprint, request.Mode, actualMode, request.PageSize, len(records), formatTime(now), formatTime(expiresAt))
	if err != nil {
		return searchPage{}, err
	}
	for position, data := range encodedRecords {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pagination_entries(snapshot_id, position, record_json)
			VALUES(?, ?, ?)`, snapshotID, position, data); err != nil {
			return searchPage{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return searchPage{}, err
	}
	return firstPage, nil
}

func deleteExpiredSearchSnapshotsInTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM pagination_snapshots WHERE julianday(expires_at) <= julianday(?)`,
		formatTime(now),
	)
	return err
}

func ensureSearchSnapshotCountCapacityInTx(ctx context.Context, tx *sql.Tx, authority string) error {
	var activeGlobal, activeAuthority, replayGlobal, replayAuthority int
	err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN terminal = 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN terminal = 0 AND authority_digest = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN terminal = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN terminal = 1 AND authority_digest = ? THEN 1 ELSE 0 END), 0)
		FROM pagination_snapshots`, authority, authority).Scan(
		&activeGlobal, &activeAuthority, &replayGlobal, &replayAuthority,
	)
	if err != nil {
		return err
	}
	if activeGlobal >= maxActiveSearchSnapshotsGlobal || activeAuthority >= maxActiveSearchSnapshotsPerAuthority ||
		activeGlobal+replayGlobal >= maxRetainedSearchSnapshotsGlobal ||
		activeAuthority+replayAuthority >= maxRetainedSearchSnapshotsPerAuthority {
		return errSnapshotCapacity
	}
	return nil
}

func (d *database) readSnapshotPage(ctx context.Context, request *protocol.SearchRequest, now time.Time) (searchPage, error) {
	snapshotID, offset, err := parsePageToken(request.PageToken)
	if err != nil {
		return searchPage{}, errSnapshotInvalid
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return searchPage{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := ensureOwnedInTx(ctx, tx, request.Binding); err != nil {
		return searchPage{}, err
	}
	var authority, fingerprint, actualMode, expiresAtRaw string
	var pageSize, entryCount int
	err = tx.QueryRowContext(ctx, `SELECT authority_digest, request_fingerprint, actual_mode, page_size,
		entry_count, expires_at FROM pagination_snapshots WHERE snapshot_id = ?`, snapshotID).
		Scan(&authority, &fingerprint, &actualMode, &pageSize, &entryCount, &expiresAtRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return searchPage{}, errSnapshotInvalid
	}
	if err != nil {
		return searchPage{}, err
	}
	expiresAt, err := parseTime(expiresAtRaw)
	if err != nil {
		return searchPage{}, err
	}
	if !expiresAt.After(now) {
		return searchPage{}, errSnapshotExpired
	}
	if authority != protocol.AuthorityDigest(request.Binding) || fingerprint != searchFingerprint(request) || pageSize != request.PageSize || offset < 0 || offset > entryCount {
		return searchPage{}, errSnapshotInvalid
	}
	rows, err := tx.QueryContext(ctx, `SELECT record_json FROM pagination_entries
		WHERE snapshot_id = ? AND position >= ? AND position < ? ORDER BY position ASC`,
		snapshotID, offset, offset+pageSize)
	if err != nil {
		return searchPage{}, err
	}
	defer rows.Close() //nolint:errcheck
	records := make([]protocol.MemoryRecord, 0, pageSize)
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return searchPage{}, err
		}
		var record protocol.MemoryRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return searchPage{}, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return searchPage{}, err
	}
	if err := rows.Close(); err != nil {
		return searchPage{}, err
	}
	page := pageFromRecords(snapshotID, offset, entryCount, actualMode, expiresAt, records)
	if page.exhausted {
		if _, err := tx.ExecContext(ctx, `UPDATE pagination_snapshots SET terminal = 1 WHERE snapshot_id = ?`, snapshotID); err != nil {
			return searchPage{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return searchPage{}, err
	}
	return page, nil
}

func pageFromRecords(snapshotID string, offset, total int, actualMode string, expiresAt time.Time, records []protocol.MemoryRecord) searchPage {
	records = boundPageRecords(records)
	nextOffset := offset + len(records)
	exhausted := nextOffset >= total
	next := ""
	if !exhausted {
		next = formatPageToken(snapshotID, nextOffset)
	}
	if records == nil {
		records = []protocol.MemoryRecord{}
	}
	return searchPage{actualMode: actualMode, records: records, nextToken: next, exhausted: exhausted, expiresAt: expiresAt}
}

func boundPageRecords(records []protocol.MemoryRecord) []protocol.MemoryRecord {
	const responseEnvelopeReserve = 64 << 10
	budget := protocol.MaxAdapterResponseBytes - responseEnvelopeReserve
	used := 0
	count := 0
	for index := range records {
		data, err := json.Marshal(records[index])
		if err != nil {
			break
		}
		if count > 0 && used+len(data) > budget {
			break
		}
		used += len(data)
		count++
	}
	return records[:count]
}

func recordMatchesKeyword(record protocol.MemoryRecord, query string) bool {
	if strings.Contains(strings.ToLower(record.Content), query) {
		return true
	}
	for _, tag := range record.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	for key, value := range record.Metadata {
		if strings.Contains(strings.ToLower(key), query) || strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func searchFingerprint(request *protocol.SearchRequest) string {
	data, _ := json.Marshal(struct {
		Authority string `json:"authority"`
		Mode      string `json:"mode"`
		Query     string `json:"query"`
		PageSize  int    `json:"pageSize"`
	}{Authority: protocol.AuthorityDigest(request.Binding), Mode: request.Mode, Query: request.Query, PageSize: request.PageSize})
	return protocol.ContentDigest(string(data))
}

func randomSnapshotID() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func formatPageToken(snapshotID string, offset int) string {
	return "oms-page-v1." + snapshotID + "." + strconv.Itoa(offset)
}

func parsePageToken(token string) (string, int, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "oms-page-v1" || len(parts[1]) != 32 {
		return "", 0, errSnapshotInvalid
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", 0, errSnapshotInvalid
	}
	offset, err := strconv.Atoi(parts[2])
	if err != nil || offset < 0 {
		return "", 0, errSnapshotInvalid
	}
	return parts[1], offset, nil
}

func backendMemoryID(upsertKey string) string {
	digest := protocol.ContentDigest(upsertKey)
	return "ref-" + strings.TrimPrefix(digest, "sha256:")[:32]
}

func cloneMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input))
	maps.Copy(result, input)
	return result
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	result, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse persisted time: %w", err)
	}
	return result.UTC(), nil
}
