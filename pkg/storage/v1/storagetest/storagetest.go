package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

type StateFactory func(t *testing.T) storagev1.StateProvider

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
	{"ahp", runAHP},
	{"auth-sessions", runAuthSessions},
	{"sealed-snapshots", runSealedSnapshots},
	{"sealed-native-sessions", runSealedNativeSessions},
	{"session-turn-records", runSessionTurnRecords},
	{"sealed-connector-custody", runSealedConnectorCustody},
	{"sealed-connector-consents", runSealedConnectorConsents},
	{"sealed-connector-listings", runSealedConnectorListings},
	{"memory-proposal-application", runMemoryProposalApplication},
	{"memory-consolidation", runMemoryConsolidation},
	{"memory-consolidation-runs", runMemoryConsolidationRuns},
	{"task-data-fences", runTaskDataFences},
	{"artifact-fence", runArtifactFence},
	{"result-write-order", runResultWriteOrder},
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
	t.Run("artifact-write-order", func(t *testing.T) {
		runArtifactWriteOrder(t, newProvider)
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
