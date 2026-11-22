package sqlite

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var (
	_ storagev1.StateProvider    = (*Store)(nil)
	_ storagev1.ArtifactProvider = (*Store)(nil)
)

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
		Durability: storagev1.DurabilityLocalBuffered,

		ContractRevision: storagev1.ContractRevision,
		State:            s.datasetState(),
		Incarnation:      s.incarnation,
		ObservedAt:       time.Now().UTC(),
		Bootstrap:        storagev1.DatasetBootstrap{Complete: record.complete, Empty: !populated},
		Usage:            datasetUsage(s.dbPath),
		Binding:          binding.public(),
	}, nil
}

const inMemoryPath = ":memory:"

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
