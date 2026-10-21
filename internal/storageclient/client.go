// Package storageclient is Orka's single entry point to application storage.
//
// Every Orka consumer receives a Client rather than a concrete database. The
// client composes a state.v1 provider and an artifacts.v1 provider from the
// public storage contract (pkg/storage/v1). Families that still run Go
// callbacks inside a database transaction or carry plaintext connector custody
// are served through Embedded, which only the in-process SQLite store
// implements.
//
// The client applies Orka's redaction, payload sanitization and usage
// attribution before any provider call, so providers store only content Orka
// has already made safe to persist. It also owns the snapshot key: it seals
// execution snapshots and native session state before a provider sees them
// and opens them after a provider returns them, so a provider stores only
// sealed bytes and never holds the key.
package storageclient

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Embedded is the part of the application store that is not yet expressed in
// the public provider contract. These families either receive plaintext
// connector custody or run Orka callbacks inside a database transaction (task
// data transactions, scan ingestion and memory proposal application). Only
// the in-process SQLite store implements Embedded, and it must be the same
// store that serves both contracts so transaction-scoped contexts reach every
// participating record. The embedded store must also accept the connector
// custody sealer the client hands over when it activates the snapshot key
// (see ActivateSnapshotKey); that method is deliberately not part of this
// interface, so it never joins the client's method set.
type Embedded interface {
	store.ConnectorCredentialStore
	store.ConnectorConsentStore

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
}

// Client implements every application store interface Orka consumes. Its
// method set is the union of the state, artifact and embedded families, plus
// the plaintext views of the sealed families.
type Client struct {
	storagev1.StateProvider
	storagev1.ArtifactStore
	Embedded

	// custody holds the snapshot key once ActivateSnapshotKey succeeds.
	custody atomic.Pointer[keyCustody]
	// nativeSessionMaxBytes limits new native bundles; zero selects the
	// default.
	nativeSessionMaxBytes atomic.Int64
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
	return &Client{
		StateProvider: options.State,
		ArtifactStore: options.Artifacts,
		Embedded:      options.Embedded,
	}, nil
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
	return c.StateProvider.HealthCheck(ctx)
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
