package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The embedded SQLite store is the in-process provider for both storage
// contracts.
var (
	_ storagev1.StateProvider    = (*Store)(nil)
	_ storagev1.ArtifactProvider = (*Store)(nil)
)

// Describe reports the dataset identity and the guarantees of the embedded
// store. The store runs SQLite in WAL mode. Under synchronous=NORMAL, the
// default, a commit survives a process restart but not necessarily a host or
// storage failure before the WAL is synced (local-buffered); under
// synchronous=FULL every commit is synced first (local-fsync). Describe reads
// the mode the connection actually uses. The incarnation is new each time the
// store is opened, and the dataset is always ready, because the schema is
// upgraded while the store opens, under its process lock.
func (s *Store) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	defer func() { _ = tx.Rollback() }()
	version, err := readSchemaVersion(tx)
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	if version != SchemaVersion {
		return storagev1.ProviderInfo{}, fmt.Errorf("%w: unsupported SQLite schema version %d", storagev1.ErrIncompatible, version)
	}
	record, err := readDataset(tx)
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	populated, err := datasetHasRecords(ctx, tx)
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	binding, err := readDatasetBinding(ctx, tx)
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	durability, err := sqliteDurability(ctx, tx)
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	return storagev1.ProviderInfo{
		ContractVersion: storagev1.ContractVersion,
		Contracts:       []string{storagev1.StateContract, storagev1.ArtifactsContract},
		DatasetID:       record.id,
		DatasetOrigin:   storagev1.DatasetOrigin(record.origin),
		SchemaVersion:   "sqlite/" + strconv.Itoa(version),
		Limits: storagev1.ProviderLimits{
			MaxArtifactBytes:      storagev1.MaxArtifactBytes,
			MaxResultBytes:        storagev1.MaxResultBytes,
			ReceiptRetention:      receiptRetention,
			MaxReceiptResultBytes: maxReceiptResultBytes,
		},
		Durability: durability,

		ContractRevision: storagev1.ContractRevision,
		// The schema is upgraded when the store opens, under the process lock,
		// so an open store serves a ready dataset unless a test holds it in
		// another state.
		State:       s.datasetState(),
		Incarnation: s.incarnation,
		ObservedAt:  time.Now().UTC(),
		Bootstrap:   storagev1.DatasetBootstrap{Complete: record.complete, Empty: !populated},
		Usage:       datasetUsage(s.dbPath),
		Binding:     binding.public(),
	}, nil
}

// inMemoryPath is the path of a database that SQLite keeps in memory.
const inMemoryPath = ":memory:"

// datasetUsage returns the size of the database file and its write-ahead log,
// or nil for an in-memory database or a file it cannot read.
func datasetUsage(path string) *storagev1.DatasetUsage {
	if path == "" || path == inMemoryPath {
		return nil
	}
	file, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := file.Size()
	if wal, err := os.Stat(path + "-wal"); err == nil {
		size += wal.Size()
	}
	return &storagev1.DatasetUsage{DatasetBytes: size}
}

// sqliteDurability derives the durability class from the synchronous mode of
// the connection q runs on: FULL (2) and EXTRA (3) sync the write-ahead log on
// every commit; lower modes do not.
func sqliteDurability(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (storagev1.DurabilityClass, error) {
	var mode int
	if err := q.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&mode); err != nil {
		return "", fmt.Errorf("read SQLite synchronous mode: %w", err)
	}
	if mode >= 2 {
		return storagev1.DurabilityLocalFsync, nil
	}
	return storagev1.DurabilityLocalBuffered, nil
}
