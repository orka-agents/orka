// Package storagetest is the reusable behavioral suite for orka.storage.v1
// providers.
//
// A provider runs RunState and RunArtifacts from its own tests with a factory
// that returns a fresh, empty provider for each case. The suite uses only the
// public contract in pkg/storage/v1, so a separately built provider can run it
// without Orka's internal packages. Cases assert contract behavior: identity
// and namespace isolation, not-found and conflict errors, idempotent replays,
// atomic grouped writes and bounded queries. They do not inspect a provider's
// schema or rely on its clock beyond the times passed in requests.
package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// StateFactory returns a fresh, empty state.v1 provider. The factory owns the
// provider's lifecycle and must release it with t.Cleanup.
type StateFactory func(t *testing.T) storagev1.StateProvider

// ArtifactFactory returns a fresh, empty artifacts.v1 provider. The factory
// owns the provider's lifecycle and must release it with t.Cleanup.
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
	{"harness-v1-attempts", runHarnessV1Attempts},
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
}

// RunState runs every state.v1 case against providers from newProvider.
func RunState(t *testing.T, newProvider StateFactory) {
	t.Helper()
	for _, family := range stateFamilies {
		t.Run(family.name, func(t *testing.T) {
			family.run(t, newProvider)
		})
	}
}

// RunArtifacts runs every artifacts.v1 case against providers from newProvider.
func RunArtifacts(t *testing.T, newProvider ArtifactFactory) {
	t.Helper()
	t.Run("artifacts", func(t *testing.T) {
		runArtifacts(t, newProvider)
	})
}
