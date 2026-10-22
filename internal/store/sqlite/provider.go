package sqlite

import (
	"context"
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
// store. The store runs SQLite in WAL mode with synchronous=NORMAL, so a
// commit survives a process restart but not necessarily a host or storage
// failure before the WAL is synced.
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
	return storagev1.ProviderInfo{
		ContractVersion: storagev1.ContractVersion,
		Contracts:       []string{storagev1.StateContract, storagev1.ArtifactsContract},
		DatasetID:       record.id,
		DatasetOrigin:   storagev1.DatasetOrigin(record.origin),
		SchemaVersion:   "sqlite/" + strconv.Itoa(version),
		Limits:          storagev1.ProviderLimits{MaxArtifactBytes: storagev1.MaxArtifactBytes},
		Durability:      storagev1.DurabilityLocalBuffered,

		ContractRevision: storagev1.ContractRevision,
		// The schema is upgraded when the store opens, under the process lock,
		// so an open store always serves a ready dataset.
		State:       storagev1.DatasetReady,
		Incarnation: s.incarnation,
		ObservedAt:  time.Now().UTC(),
		Bootstrap:   storagev1.DatasetBootstrap{Complete: record.complete, Empty: !populated},
		Usage:       datasetUsage(s.dbPath),
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
