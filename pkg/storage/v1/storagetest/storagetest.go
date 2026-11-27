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
	{"message-budget", runMessageBudget},
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
	{"grouped-writes", runGroupedWrites},
	{"protocols", runProtocols},
	{"write-watermark", func(t *testing.T, newProvider StateFactory) {
		runWriteWatermark(t, newProvider)
	}},
	{"installations", func(t *testing.T, newProvider StateFactory) {
		runInstallations(t, newProvider)
	}},
	{"export-import", runStateExportImport},
}

func RunState(t *testing.T, newProvider StateFactory, options ...Option) {
	t.Helper()
	settings := collectOptions(options)
	if !settings.providerAttachesEnvelopes {
		settings.run(t, "missing-envelopes", func(t *testing.T) {
			runMissingEnvelopes(t, newProvider, wrapState, stateWriteCases)
		})
	}
	wrapped := stateFactory(newProvider, settings)
	for _, family := range stateFamilies {
		settings.run(t, family.name, func(t *testing.T) {
			family.run(t, wrapped)
		})
	}
	settings.run(t, "write-envelopes", func(t *testing.T) {
		runWriteCatalog(t, wrapped, stateWriteCases, settings)
	})
	runOptionalFamilies(t, wrapped, stateWriteCases, settings)
}

func RunArtifacts(t *testing.T, newProvider ArtifactFactory, options ...Option) {
	t.Helper()
	settings := collectOptions(options)
	if !settings.providerAttachesEnvelopes {
		settings.run(t, "missing-envelopes", func(t *testing.T) {
			runMissingEnvelopes(t, newProvider, wrapArtifacts, artifactWriteCases)
		})
	}
	wrapped := artifactFactory(newProvider, settings)
	settings.run(t, "artifacts", func(t *testing.T) {
		runArtifacts(t, wrapped)
	})
	settings.run(t, "artifact-write-order", func(t *testing.T) {
		runArtifactWriteOrder(t, wrapped)
	})
	settings.run(t, "write-envelopes", func(t *testing.T) {
		runWriteCatalog(t, wrapped, artifactWriteCases, settings)
	})
	settings.run(t, "dataset-binding", func(t *testing.T) {
		runDatasetBinding(t, wrapped, func(t *testing.T, provider storagev1.ArtifactProvider) {
			saveArtifact(t, provider, artifactOwner("tenant-a", "task-1"), "report.txt", "text/plain", []byte("report"))
		})
	})
	settings.run(t, "write-watermark", func(t *testing.T) {
		runWriteWatermark(t, wrapped)
	})
	settings.run(t, "installations", func(t *testing.T) {
		runInstallations(t, wrapped)
	})
	settings.run(t, "export-import", func(t *testing.T) {
		runArtifactsExportImport(t, wrapped)
	})
	runOptionalFamilies(t, wrapped, artifactWriteCases, settings)
}

func runOptionalFamilies[P suiteProvider](t *testing.T, newProvider func(t *testing.T) P, cases []writeCase[P], selected suiteOptions) {
	t.Helper()
	if selected.maintenance != nil {
		selected.run(t, "maintenance", func(t *testing.T) {
			runMaintenance(t, newProvider, cases, *selected.maintenance)
		})
	}
	if selected.durability != nil {
		selected.run(t, "durability", func(t *testing.T) {
			runDurability(t, newProvider, cases, *selected.durability)
		})
	}
}

func (o suiteOptions) run(t *testing.T, name string, family func(t *testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		if o.parallel {
			t.Parallel()
		}
		family(t)
	})
}

func harnessProvider(provider storagev1.ProviderDescriber) storagev1.ProviderDescriber {
	if wrapped, ok := provider.(suiteWrapped); ok {
		return wrapped.unwrapped()
	}
	return provider
}

func rewrapHarnessProvider(replaced, returned storagev1.ProviderDescriber) storagev1.ProviderDescriber {
	if wrapped, ok := replaced.(suiteWrapped); ok {
		return wrapped.rewrap(returned)
	}
	return returned
}

type suiteWrapped interface {
	unwrapped() storagev1.ProviderDescriber
	rewrap(storagev1.ProviderDescriber) storagev1.ProviderDescriber
}

func (e envelopedState) unwrapped() storagev1.ProviderDescriber { return e.StateProvider }

func (e envelopedState) rewrap(provider storagev1.ProviderDescriber) storagev1.ProviderDescriber {
	if state, ok := provider.(storagev1.StateProvider); ok {
		return wrapState(state)
	}
	return provider
}

func (e envelopedArtifacts) unwrapped() storagev1.ProviderDescriber { return e.ArtifactProvider }

func (e stateWithArtifacts) unwrapped() storagev1.ProviderDescriber {
	return harnessProvider(e.StateProvider)
}

func (e stateWithArtifacts) rewrap(provider storagev1.ProviderDescriber) storagev1.ProviderDescriber {
	if state, ok := provider.(storagev1.StateProvider); ok {
		return wrapState(state)
	}
	return provider
}

func (e envelopedArtifacts) rewrap(provider storagev1.ProviderDescriber) storagev1.ProviderDescriber {
	if artifacts, ok := provider.(storagev1.ArtifactProvider); ok {
		return wrapArtifacts(artifacts)
	}
	return provider
}

type suiteProvider interface {
	writeCaseProvider
	storagev1.WriteWatermarkStore
}
