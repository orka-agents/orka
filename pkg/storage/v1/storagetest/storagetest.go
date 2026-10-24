// Package storagetest is the reusable behavioral suite for orka.storage.v1
// providers.
//
// A provider runs RunState and RunArtifacts from its own tests with a factory
// that returns an empty dataset for each case. The suite uses only the public
// contract in pkg/storage/v1, so a separately built provider can run it
// without Orka's internal packages. Cases assert contract behavior: identity
// and namespace isolation, the typed errors providers return, idempotent
// replays, atomic grouped writes, bounded queries, one winner among
// concurrent callers, exact nanosecond times and the provider description.
// They do not inspect a provider's schema or rely on its clock beyond the
// times passed in requests.
package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
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
	{"timestamps", runTimestamps},
	{"concurrency", runConcurrency},
	{"describe", runDescribe},
	{"operation-receipts", runOperationReceipts},
	{"controller-ownership", runControllerOwnership},
	{"lost-responses", runLostResponses},
	{"write-envelopes", func(t *testing.T, newProvider StateFactory) {
		runWriteCatalog(t, newProvider, stateWriteCases)
	}},
	{"write-watermark", func(t *testing.T, newProvider StateFactory) {
		runWriteWatermark(t, newProvider)
	}},
	{"installations", func(t *testing.T, newProvider StateFactory) {
		runInstallations(t, newProvider)
	}},
}

// RunState runs every state.v1 case against providers from newProvider, and
// the optional cases that options enable.
func RunState(t *testing.T, newProvider StateFactory, options ...Option) {
	t.Helper()
	selected := collectOptions(options)
	for _, family := range stateFamilies {
		t.Run(family.name, func(t *testing.T) {
			family.run(t, newProvider)
		})
	}
	runOptionalFamilies(t, newProvider, stateWriteCases, selected)
}

// RunArtifacts runs every artifacts.v1 case against providers from
// newProvider, and the optional cases that options enable.
func RunArtifacts(t *testing.T, newProvider ArtifactFactory, options ...Option) {
	t.Helper()
	selected := collectOptions(options)
	t.Run("artifacts", func(t *testing.T) {
		runArtifacts(t, newProvider)
	})
	t.Run("write-envelopes", func(t *testing.T) {
		runWriteCatalog(t, newProvider, artifactWriteCases)
	})
	t.Run("write-watermark", func(t *testing.T) {
		runWriteWatermark(t, newProvider)
	})
	t.Run("installations", func(t *testing.T) {
		runInstallations(t, newProvider)
	})
	runOptionalFamilies(t, newProvider, artifactWriteCases, selected)
}

// Option enables optional cases of RunState and RunArtifacts that need a
// harness only the provider's own tests can supply.
type Option func(*suiteOptions)

type suiteOptions struct {
	maintenance *MaintenanceHarness
	durability  *DurabilityHarness
}

func collectOptions(options []Option) suiteOptions {
	var selected suiteOptions
	for _, option := range options {
		option(&selected)
	}
	return selected
}

// runOptionalFamilies runs the cases the options enable over every write
// case of a contract.
func runOptionalFamilies[P suiteProvider](t *testing.T, newProvider func(t *testing.T) P, cases []writeCase[P], selected suiteOptions) {
	t.Helper()
	if selected.maintenance != nil {
		t.Run("maintenance", func(t *testing.T) {
			runMaintenance(t, newProvider, cases, *selected.maintenance)
		})
	}
	if selected.durability != nil {
		t.Run("durability", func(t *testing.T) {
			runDurability(t, newProvider, cases, *selected.durability)
		})
	}
}

// suiteProvider is what the cases that every contract shares need from a
// provider.
type suiteProvider interface {
	writeCaseProvider
	storagev1.WriteWatermarkStore
}
