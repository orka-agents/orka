package v1

import (
	"context"
	"time"
)

// DatasetOrigin records how a provider's dataset came to exist.
type DatasetOrigin string

const (
	// DatasetOriginNew marks a dataset the provider created empty.
	DatasetOriginNew DatasetOrigin = "new"
	// DatasetOriginAdopted marks a dataset that predates dataset identity and
	// was given an identity when the provider first opened it.
	DatasetOriginAdopted DatasetOrigin = "adopted"
	// DatasetOriginMigrated marks a dataset imported from another provider.
	DatasetOriginMigrated DatasetOrigin = "migrated"
)

// IsValidDatasetOrigin reports whether origin is a defined dataset origin.
func IsValidDatasetOrigin(origin DatasetOrigin) bool {
	switch origin {
	case DatasetOriginNew, DatasetOriginAdopted, DatasetOriginMigrated:
		return true
	default:
		return false
	}
}

// DurabilityClass states what a successful write survives. Orka uses it to
// decide whether automatic continuation after a provider failover is safe.
type DurabilityClass string

const (
	// DurabilitySynchronousReplicated acknowledges a write only after it is
	// durable on more than one node, so an automatic failover loses no
	// acknowledged write.
	DurabilitySynchronousReplicated DurabilityClass = "synchronous-replicated"
	// DurabilityLocalFsync acknowledges a write after it is synced to local
	// storage. It survives a process or host restart, and there is no
	// automatic failover.
	DurabilityLocalFsync DurabilityClass = "local-fsync"
	// DurabilityLocalBuffered acknowledges a write after it reaches local
	// storage without a sync on every commit. It survives a process restart,
	// but the most recent acknowledged writes can be lost if the host or its
	// storage fails before they are synced. There is no automatic failover.
	DurabilityLocalBuffered DurabilityClass = "local-buffered"
	// DurabilityAsyncReplicated acknowledges a write before every replica has
	// it, so an automatic failover may lose acknowledged writes.
	DurabilityAsyncReplicated DurabilityClass = "async-replicated"
)

// IsValidDurabilityClass reports whether class is a defined durability class.
func IsValidDurabilityClass(class DurabilityClass) bool {
	switch class {
	case DurabilitySynchronousReplicated, DurabilityLocalFsync, DurabilityLocalBuffered, DurabilityAsyncReplicated:
		return true
	default:
		return false
	}
}

// ProviderLimits states a provider's size limits. A provider must accept at
// least the sizes the contract defines, such as [MaxArtifactBytes].
type ProviderLimits struct {
	// MaxArtifactBytes is the largest artifact the provider stores. An
	// artifacts.v1 provider reports at least MaxArtifactBytes; a provider that
	// does not serve artifacts.v1 reports zero.
	MaxArtifactBytes int64
	// MaxValueBytes is the largest single stored value the provider accepts,
	// or zero when the provider imposes no limit of its own.
	MaxValueBytes int64
	// ReceiptRetention is how long the provider keeps an operation receipt
	// after recording it, at least MinReceiptRetention, or zero when the
	// provider keeps no operation receipts yet.
	ReceiptRetention time.Duration
	// MaxReceiptResultBytes is the largest result the provider keeps with an
	// operation receipt; it reads a larger result again from the records the
	// write made. It is zero when the provider keeps no results, or no
	// operation receipts yet.
	MaxReceiptResultBytes int64
}

// DatasetState says whether a dataset accepts writes.
type DatasetState string

const (
	// DatasetReady marks a dataset that accepts writes.
	DatasetReady DatasetState = "ready"
	// DatasetMigrating marks a dataset whose schema is changing. New writes
	// return ErrNotReady and commit nothing until the migration finishes; a
	// replay of a committed operation is still served, and reads keep
	// working.
	DatasetMigrating DatasetState = "migrating"
	// DatasetImporting marks a dataset receiving a migration import. New
	// writes return ErrNotReady and commit nothing until the import finishes.
	DatasetImporting DatasetState = "importing"
)

// IsValidDatasetState reports whether state is a defined dataset state.
func IsValidDatasetState(state DatasetState) bool {
	switch state {
	case DatasetReady, DatasetMigrating, DatasetImporting:
		return true
	default:
		return false
	}
}

// ProviderInfo describes a provider and the dataset it serves. It exists for
// startup validation of a storage binding, installation binding and
// migration; Orka does not read it yet.
type ProviderInfo struct {
	// ContractVersion is the contract the provider implements, ContractVersion
	// for this package.
	ContractVersion string
	// Contracts lists the contract parts the provider serves, such as
	// StateContract and ArtifactsContract.
	Contracts []string
	// DatasetID identifies the dataset independently of its location. It is
	// stable for the dataset's lifetime and survives a move or restore.
	DatasetID string
	// DatasetOrigin records how the dataset came to exist.
	DatasetOrigin DatasetOrigin
	// SchemaVersion is the provider's own schema version for the dataset. It
	// is opaque to Orka beyond equality.
	SchemaVersion string
	// Capabilities lists optional families the provider serves beyond the
	// contract parts in Contracts. Version 1 defines none; families added later
	// are detected here rather than assumed.
	Capabilities []string
	// Limits states the provider's size limits.
	Limits ProviderLimits
	// Durability states what a successful write survives.
	Durability DurabilityClass
	// State says whether the dataset accepts writes. It is empty when the
	// provider does not report it yet.
	State DatasetState
	// Incarnation changes whenever the serving database may have restarted or
	// switched to another node, so Orka can validate the dataset again before
	// trusting it. It is empty when the provider does not report it yet.
	Incarnation string
	// ObservedAt is the provider clock when Describe ran, so Orka can estimate
	// the clock difference between itself and the provider. It is zero when
	// the provider does not report it yet.
	ObservedAt time.Time
	// Binding is the installation the dataset is bound to, as
	// [DatasetBindingStore.GetDatasetBinding] returns it, or nil when the
	// dataset is unbound.
	Binding *DatasetBinding
}

// ProviderDescriber reports what a provider serves and guarantees.
type ProviderDescriber interface {
	Describe(ctx context.Context) (ProviderInfo, error)
}
