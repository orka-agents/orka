package v1

import (
	"context"
	"time"
)

type DatasetOrigin string

const (
	DatasetOriginNew      DatasetOrigin = "new"
	DatasetOriginAdopted  DatasetOrigin = "adopted"
	DatasetOriginMigrated DatasetOrigin = "migrated"
)

func IsValidDatasetOrigin(origin DatasetOrigin) bool {
	switch origin {
	case DatasetOriginNew, DatasetOriginAdopted, DatasetOriginMigrated:
		return true
	default:
		return false
	}
}

type DurabilityClass string

const (
	DurabilitySynchronousReplicated DurabilityClass = "synchronous-replicated"
	DurabilityLocalFsync            DurabilityClass = "local-fsync"
	DurabilityLocalBuffered         DurabilityClass = "local-buffered"
	DurabilityAsyncReplicated       DurabilityClass = "async-replicated"
)

func IsValidDurabilityClass(class DurabilityClass) bool {
	switch class {
	case DurabilitySynchronousReplicated, DurabilityLocalFsync, DurabilityLocalBuffered, DurabilityAsyncReplicated:
		return true
	default:
		return false
	}
}

type ProviderLimits struct {
	MaxArtifactBytes       int64
	MaxResultBytes         int64
	MaxMessageContentBytes int64
	MaxValueBytes          int64
	ReceiptRetention       time.Duration
	MaxReceiptResultBytes  int64
}

type DatasetState string

const (
	DatasetReady        DatasetState = "ready"
	DatasetMigrating    DatasetState = "migrating"
	DatasetImporting    DatasetState = "importing"
	DatasetIncompatible DatasetState = "incompatible"
)

func IsValidDatasetState(state DatasetState) bool {
	switch state {
	case DatasetReady, DatasetMigrating, DatasetImporting, DatasetIncompatible:
		return true
	default:
		return false
	}
}

type DatasetBootstrap struct {
	Complete bool
	Empty    bool
}

type DatasetUsage struct {
	DatasetBytes int64
}

type ProviderInfo struct {
	ContractVersion  string
	ContractRevision int
	Contracts        []string
	DatasetID        string
	DatasetOrigin    DatasetOrigin
	SchemaVersion    string
	Capabilities     []string
	Limits           ProviderLimits
	Durability       DurabilityClass
	State            DatasetState
	Incarnation      string
	ObservedAt       time.Time
	Bootstrap        DatasetBootstrap
	Usage            *DatasetUsage
	Binding          *DatasetBinding
	Source           *DatasetSource
	MigratedTo       []MigrationMarker
}

type ProviderDescriber interface {
	Describe(ctx context.Context) (ProviderInfo, error)
}
