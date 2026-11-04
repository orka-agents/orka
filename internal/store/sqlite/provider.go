package sqlite

import (
	"context"
	"fmt"
	"strconv"

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
		return storagev1.ProviderInfo{}, fmt.Errorf("unsupported SQLite schema version %d", version)
	}
	record, err := readDataset(tx)
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
			ReceiptRetention:      receiptRetention,
			MaxReceiptResultBytes: maxReceiptResultBytes,
		},
		Durability: storagev1.DurabilityLocalBuffered,
		State:      s.datasetState(),
		Binding:    binding.public(),
	}, nil
}
