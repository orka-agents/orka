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
}

// Option changes how RunState and RunArtifacts run the suite.
type Option func(*options)

type options struct {
	parallel bool
}

// Parallel runs the contract families in parallel with each other. Cases
// within a family still run in order. The factory must then be safe for
// concurrent use, and each dataset it returns must stay isolated from the
// others, as a schema per case on a shared database is.
func Parallel() Option {
	return func(o *options) { o.parallel = true }
}

func runOptions(opts []Option) options {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// RunState runs every state.v1 case against providers from newProvider.
func RunState(t *testing.T, newProvider StateFactory, opts ...Option) {
	t.Helper()
	o := runOptions(opts)
	for _, family := range stateFamilies {
		t.Run(family.name, func(t *testing.T) {
			if o.parallel {
				t.Parallel()
			}
			family.run(t, newProvider)
		})
	}
}

// RunArtifacts runs every artifacts.v1 case against providers from newProvider.
func RunArtifacts(t *testing.T, newProvider ArtifactFactory, opts ...Option) {
	t.Helper()
	o := runOptions(opts)
	t.Run("artifacts", func(t *testing.T) {
		if o.parallel {
			t.Parallel()
		}
		runArtifacts(t, newProvider)
	})
}
