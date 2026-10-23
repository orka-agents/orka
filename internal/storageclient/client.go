// Package storageclient is Orka's single entry point to application storage.
//
// Every Orka consumer receives a Client rather than a concrete database. The
// client composes a state.v1 provider and an artifacts.v1 provider from the
// public storage contract (pkg/storage/v1). Families that still carry
// plaintext protected data or Go callbacks into a database transaction are
// served through Embedded, which only the in-process SQLite store implements.
//
// The client applies Orka's redaction, payload sanitization and usage
// attribution before any provider call, so providers store only content Orka
// has already made safe to persist.
//
// Below that policy, the client runs every contract write under a write
// envelope (pkg/storage/v1 WriteEnvelope): it waits for the controller epoch
// fence of the dataset the write targets, gives the write an operation ID,
// request digest, issue time and sequence, retries it with the same envelope
// while its outcome is unknown, and settles it with ResolveOperation when
// retrying cannot. Embedded families pass through unchanged; SQLite checks
// the fences they carry when they commit.
package storageclient

import (
	"context"
	"errors"
	"reflect"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Embedded is the part of the application store that is not yet expressed in
// the public provider contract. These families either receive plaintext
// protected data (execution snapshots, native sessions, connector custody,
// turn finalization with native capture) or run Orka callbacks inside a
// database transaction (task data transactions, scan ingestion and memory
// proposal application). Only the in-process SQLite store implements
// Embedded, and it must be the same store that serves both contracts so
// transaction-scoped contexts reach every participating record.
type Embedded interface {
	store.AgentExecutionSnapshotStore
	store.AgentExecutionSnapshotLifecycleStore
	store.NativeSessionImportStore
	store.ConnectorCredentialStore
	store.ConnectorConsentStore
	store.SessionTurnPersistenceStore

	ApplyMemoryProposal(ctx context.Context, apply store.MemoryProposalApply) (*store.Memory, error)
	WithTaskDataTransaction(ctx context.Context, fn func(context.Context) error) error
	WithAuthorizedTaskDataTransaction(
		ctx context.Context,
		namespace, taskName string,
		authorize func(context.Context) error,
		fn func(context.Context) error,
	) error
	ApplyScanTaskIngestion(
		ctx context.Context,
		ingestion *store.ScanTaskIngestion,
		validate func(*store.ScanRun) error,
		apply func(store.SecurityStore, *store.ScanRun) error,
	) (bool, error)
}

// Options selects the providers behind a Client.
type Options struct {
	// State serves state.v1.
	State storagev1.StateProvider
	// Artifacts serves artifacts.v1.
	Artifacts storagev1.ArtifactProvider
	// Embedded serves the families outside the provider contract. It must be
	// the same store as State and Artifacts.
	Embedded Embedded
	// Fences supplies the dataset and controller epoch fence of each write.
	// When it is nil, BindFences must supply it before writes can run;
	// until then a write waits for it, and is refused with ErrNotReady once
	// the fence wait passes.
	Fences FenceSource
}

// Client implements every application store interface Orka consumes. Its
// method set is the union of the state, artifact and embedded families.
type Client struct {
	storagev1.StateProvider
	storagev1.ArtifactStore
	Embedded

	state     storagev1.StateProvider
	artifacts storagev1.ArtifactProvider
	ops       *operations
}

// ErrSplitEmbeddedStore rejects a client whose embedded families would not
// share the transaction scope of the state and artifact records they touch.
var ErrSplitEmbeddedStore = errors.New("embedded storage families require state and artifacts on the same embedded store")

// New composes a storage client. State, Artifacts and Embedded are required,
// and while embedded families exist they must all be one store.
func New(options Options) (*Client, error) {
	if isNilProvider(options.State) || isNilProvider(options.Artifacts) || isNilProvider(options.Embedded) {
		return nil, store.ValidationErrorf("storage client requires state, artifact and embedded stores")
	}
	if !sameStore(options.State, options.Embedded) || !sameStore(options.Artifacts, options.Embedded) {
		return nil, ErrSplitEmbeddedStore
	}
	ops := newOperations(map[string]storagev1.OperationReceiptStore{
		storagev1.StateContract:     options.State,
		storagev1.ArtifactsContract: options.Artifacts,
	})
	if options.Fences != nil {
		if err := ops.bindFences(options.Fences); err != nil {
			return nil, err
		}
	}
	return &Client{
		StateProvider: envelopedState{StateProvider: options.State, ops: ops},
		ArtifactStore: envelopedArtifacts{ArtifactProvider: options.Artifacts, ops: ops},
		Embedded:      options.Embedded,
		state:         options.State,
		artifacts:     options.Artifacts,
		ops:           ops,
	}, nil
}

// BindFences supplies the fence source of a client created without one. The
// controller epoch manager is built after the client, because the Kubernetes
// control store it uses persists through the client. It can be bound once.
func (c *Client) BindFences(source FenceSource) error {
	return c.ops.bindFences(source)
}

// Dataset is a store whose controller epoch Orka advances.
type Dataset interface {
	storagev1.ProviderDescriber
	storagev1.ControllerEpochAdvancer
}

// Datasets returns the provider that serves each contract, keyed by contract
// name, so the controller epoch manager can advance each dataset.
func (c *Client) Datasets() map[string]Dataset {
	return map[string]Dataset{
		storagev1.StateContract:     c.state,
		storagev1.ArtifactsContract: c.artifacts,
	}
}

// UnresolvedOperations returns how many writes have an outcome the client is
// still resolving. While a dataset has any, the client refuses its new
// writes with ErrNotReady.
func (c *Client) UnresolvedOperations() int {
	return c.ops.unresolvedCount()
}

// WithTaskDataTransaction waits for the write fences before opening the
// transaction, and the writes inside it carry those fences, so no write
// waits for a fence, or asks the fence source, while the transaction holds
// the store.
func (c *Client) WithTaskDataTransaction(ctx context.Context, fn func(context.Context) error) error {
	ctx, err := c.ops.withTransactionFences(ctx)
	if err != nil {
		return err
	}
	return c.Embedded.WithTaskDataTransaction(ctx, fn)
}

// WithAuthorizedTaskDataTransaction waits for the write fences before
// authorizing and opening the transaction, as WithTaskDataTransaction does.
func (c *Client) WithAuthorizedTaskDataTransaction(
	ctx context.Context,
	namespace, taskName string,
	authorize func(context.Context) error,
	fn func(context.Context) error,
) error {
	ctx, err := c.ops.withTransactionFences(ctx)
	if err != nil {
		return err
	}
	return c.Embedded.WithAuthorizedTaskDataTransaction(ctx, namespace, taskName, authorize, fn)
}

// EmbeddedStore is the in-process store that serves both contracts and the
// embedded families.
type EmbeddedStore interface {
	storagev1.StateProvider
	storagev1.ArtifactProvider
	Embedded
}

// NewEmbedded composes a client over one in-process store.
func NewEmbedded(backend EmbeddedStore) (*Client, error) {
	if isNilProvider(backend) {
		return nil, store.ValidationErrorf("embedded storage backend is required")
	}
	return New(Options{State: backend, Artifacts: backend, Embedded: backend})
}

// HealthCheck verifies the selected provider. State, artifacts and the
// embedded families share one store, so one check covers every binding.
func (c *Client) HealthCheck(ctx context.Context) error {
	return c.state.HealthCheck(ctx)
}

// isNilProvider reports whether provider is nil, including a typed nil such as
// (*sqlite.Store)(nil) whose methods would panic on first use.
func isNilProvider(provider any) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return value.IsNil()
	default:
		return false
	}
}

// sameStore reports whether two provider values are provably the same store.
// Values that == cannot compare, including comparable types holding an
// uncomparable value in an interface field, are never treated as the same, so
// New rejects them instead of panicking.
func sameStore(a, b any) bool {
	valueA, valueB := reflect.ValueOf(a), reflect.ValueOf(b)
	if !valueA.IsValid() || !valueB.IsValid() || valueA.Type() != valueB.Type() {
		return false
	}
	if !valueA.Comparable() || !valueB.Comparable() {
		return false
	}
	return a == b
}
