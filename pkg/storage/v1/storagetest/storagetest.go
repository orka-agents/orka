// Package storagetest is the reusable behavioral suite for orka.storage.v1
// providers.
//
// A provider runs RunState and RunArtifacts from its own tests with a factory
// that returns an empty dataset for each case. The suite uses only the public
// contract in pkg/storage/v1, so a separately built provider can run it
// without Orka's internal packages. Cases assert contract behavior: identity
// and namespace isolation, the typed errors providers return, idempotent
// replays, atomic grouped writes, bounded queries, one winner among
// concurrent callers, conditional commands that refuse a changed target or a
// stale fence, exact nanosecond times and the provider description.
// They do not inspect a provider's schema or rely on its clock beyond the
// times passed in requests.
//
// Except for raw-provider missing-envelope rejection cases, the suite attaches
// a write envelope to every write it sends without one, as
// the contract requires of callers: the dataset Describe reports, the epoch
// the dataset holds (a dataset that holds none is given the suite's first
// epoch), a new operation ID, the request digest, the current time and the
// next sequence. Cases that test envelopes send their own. A provider that
// attaches its own envelopes runs the suite with ProviderAttachesEnvelopes.
package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// StateFactory returns a state.v1 provider over a fresh, empty dataset: a new
// provider, or an isolated dataset (such as a schema or key prefix) on a
// shared one. The factory owns that lifecycle and must release it with
// t.Cleanup.
type StateFactory func(t *testing.T) storagev1.StateProvider

// ArtifactFactory returns an artifacts.v1 provider over a fresh, empty
// dataset, in the same way as StateFactory. The factory owns that lifecycle
// and must release it with t.Cleanup.
type ArtifactFactory func(t *testing.T) storagev1.ArtifactProvider

type stateFamily struct {
	name string
	run  func(t *testing.T, newProvider StateFactory)
}

var stateFamilies = []stateFamily{
	{"results", runResults},
	{"plans", runPlans},
	{"messages", runMessages},
	{"sessions", runSessions},
	{"session-locks", runSessionLocks},
	{"chat-turns", runChatTurns},
	{"lease-clocks", runLeaseClocks},
	{"session-soul", runSessionSoul},
	{"session-lineage", runSessionLineage},
	{"session-cleanup-persistence", runSessionCleanupPersistence},
	{"controller-epoch-mirror", runControllerEpochMirror},
	{"outbox-persistence", runOutboxPersistence},
	{"task-job-authority", runTaskJobAuthority},
	{"execution-events", runExecutionEvents},
	{"gateway-events", runGatewayEvents},
	{"gateway-deliveries", runGatewayDeliveries},
	{"gateway-cleanup", runGatewayCleanup},
	{"security", runSecurity},
	{"repository-monitors", runRepositoryMonitors},
	{"usage", runUsage},
	{"memories", runMemories},
	{"sealed-snapshots", runSealedSnapshots},
	{"sealed-native-sessions", runSealedNativeSessions},
	{"session-turn-records", runSessionTurnRecords},
	{"sealed-connector-custody", runSealedConnectorCustody},
	{"sealed-connector-consents", runSealedConnectorConsents},
	{"sealed-connector-listings", runSealedConnectorListings},
	{"memory-proposal-application", runMemoryProposalApplication},
	{"task-data-fences", runTaskDataFences},
	{"artifact-fence", runArtifactFence},
	{"grouped-execution-events", runGroupedExecutionEvents},
	{"scan-ingestion", runScanIngestion},
	{"timestamps", runTimestamps},
	{"concurrency", runConcurrency},
	{"describe", runDescribe},
	{"operation-receipts", runOperationReceipts},
	{"controller-ownership", runControllerOwnership},
	{"dataset-binding", func(t *testing.T, newProvider StateFactory) {
		runDatasetBinding(t, newProvider, func(t *testing.T, provider storagev1.StateProvider) {
			require.NoError(t, provider.SaveResult(t.Context(), "tenant-a", "task-1", []byte("result")))
		})
	}},
	{"lost-responses", runLostResponses},
	{"write-envelopes", func(t *testing.T, newProvider StateFactory) {
		runWriteCatalog(t, newProvider, stateWriteCases)
	}},
}

// RunState runs every state.v1 case against providers from newProvider. Raw
// writes without envelopes must be rejected; ordinary cases get suite
// envelopes. ProviderAttachesEnvelopes delegates envelope attachment to the
// provider and skips the raw-write rejection cases.
func RunState(t *testing.T, newProvider StateFactory, options ...Option) {
	t.Helper()
	settings := collectOptions(options)
	if !settings.providerAttachesEnvelopes {
		t.Run("missing-envelopes", func(t *testing.T) {
			runMissingEnvelopes(t, newProvider, wrapState, stateWriteCases)
		})
	}
	newProvider = stateFactory(newProvider, settings)
	for _, family := range stateFamilies {
		t.Run(family.name, func(t *testing.T) {
			family.run(t, newProvider)
		})
	}
}

// RunArtifacts runs every artifacts.v1 case against providers from
// newProvider, attaching envelopes as RunState does.
func RunArtifacts(t *testing.T, newProvider ArtifactFactory, options ...Option) {
	t.Helper()
	settings := collectOptions(options)
	if !settings.providerAttachesEnvelopes {
		t.Run("missing-envelopes", func(t *testing.T) {
			runMissingEnvelopes(t, newProvider, wrapArtifacts, artifactWriteCases)
		})
	}
	newProvider = artifactFactory(newProvider, settings)
	t.Run("artifacts", func(t *testing.T) {
		runArtifacts(t, newProvider)
	})
	t.Run("write-envelopes", func(t *testing.T) {
		runWriteCatalog(t, newProvider, artifactWriteCases)
	})
	t.Run("dataset-binding", func(t *testing.T) {
		runDatasetBinding(t, newProvider, func(t *testing.T, provider storagev1.ArtifactProvider) {
			saveArtifact(t, provider, artifactOwner("tenant-a", "task-1"), "report.txt", "text/plain", []byte("report"))
		})
	})
}
