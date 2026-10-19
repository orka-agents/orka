// Package storageclient is Orka's single entry point to application storage.
//
// Every Orka consumer receives a Client rather than a concrete database. The
// client composes a state.v1 provider and an artifacts.v1 provider from the
// public storage contract (pkg/storage/v1). Families that still carry
// plaintext protected data or Go callbacks into a database transaction are
// served through Embedded, which only the in-process SQLite store implements.
package storageclient

import (
	"context"
	"errors"

	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Embedded is the part of the application store that is not yet expressed in
// the public provider contract. These families either receive plaintext
// protected data (execution snapshots, native sessions, connector custody,
// unredacted memory, turn finalization with native capture) or run Orka
// callbacks inside a database transaction (task data transactions, scan
// ingestion and memory proposal application). Only the in-process SQLite store
// implements Embedded, and it must be the same store that serves both
// contracts so transaction-scoped contexts reach every participating record.
type Embedded interface {
	store.AgentExecutionSnapshotStore
	store.AgentExecutionSnapshotLifecycleStore
	store.NativeSessionImportStore
	store.ConnectorCredentialStore
	store.ConnectorConsentStore
	store.MemoryStore
	store.MemoryProposalStore
	store.SessionTurnPersistenceStore
	harness.RuntimeSessionStore

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
// method set is the union of the state, artifact and embedded families.
type Client struct {
	storagev1.StateProvider
	storagev1.ArtifactStore
	Embedded

	artifactHealth storagev1.HealthChecker
	sameBackend    bool
}

// ErrSplitEmbeddedStore rejects a client whose embedded families would not
// share the transaction scope of the state and artifact records they touch.
var ErrSplitEmbeddedStore = errors.New("embedded storage families require state and artifacts on the same embedded store")

// New composes a storage client. State, Artifacts and Embedded are required.
func New(options Options) (*Client, error) {
	if options.State == nil || options.Artifacts == nil || options.Embedded == nil {
		return nil, store.ValidationErrorf("storage client requires state, artifact and embedded stores")
	}
	if !sameStore(options.State, options.Embedded) || !sameStore(options.Artifacts, options.Embedded) {
		return nil, ErrSplitEmbeddedStore
	}
	return &Client{
		StateProvider:  options.State,
		ArtifactStore:  options.Artifacts,
		Embedded:       options.Embedded,
		artifactHealth: options.Artifacts,
		sameBackend:    sameStore(options.State, options.Artifacts),
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
	if backend == nil {
		return nil, store.ValidationErrorf("embedded storage backend is required")
	}
	return New(Options{State: backend, Artifacts: backend, Embedded: backend})
}

// HealthCheck verifies every selected provider.
func (c *Client) HealthCheck(ctx context.Context) error {
	if err := c.StateProvider.HealthCheck(ctx); err != nil {
		return err
	}
	if c.sameBackend {
		return nil
	}
	return c.artifactHealth.HealthCheck(ctx)
}

func sameStore(a, b any) bool {
	return a == b
}
