package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// MaintenanceHarness puts a provider's dataset into the migrating state and
// back, as a schema migration does. A provider supplies it through
// WithMaintenance when its tests can hold the dataset in that state; the
// suite then checks the dataset-state step of every write.
type MaintenanceHarness struct {
	// Enter puts the dataset that provider serves into the migrating state,
	// as a migration does once in-flight writes have finished.
	Enter func(t *testing.T, provider storagev1.ProviderDescriber)
	// Exit returns the dataset to the ready state.
	Exit func(t *testing.T, provider storagev1.ProviderDescriber)
	// RefusesReadingWrites is set when the provider also refuses, while the
	// dataset is migrating, the catalog writes the suite reads through:
	// GetMessages without markRead, and IsLocked. The cases that observe
	// through them are then compared, while the dataset is migrating, only
	// through their catalog reads.
	RefusesReadingWrites bool
}

// WithMaintenance enables the maintenance cases: while the dataset is
// migrating, every write in the write catalog is refused with ErrNotReady and
// changes nothing, a replay of a committed operation and reads are still
// served, Describe reports DatasetMigrating, and ResolveOperation records
// nothing new; once it is ready again, the refused operation commits. Each
// case's records are compared while the dataset is migrating and again once
// it is ready.
func WithMaintenance(harness MaintenanceHarness) Option {
	return func(options *suiteOptions) { options.maintenance = &harness }
}

func runMaintenance[P suiteProvider](t *testing.T, newProvider func(t *testing.T) P, cases []writeCase[P], harness MaintenanceHarness) {
	require.NotNil(t, harness.Enter, "a maintenance harness needs Enter")
	require.NotNil(t, harness.Exit, "a maintenance harness needs Exit")
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			provider := newProvider(t)
			dataset, baseline := prepareWriteCase(t, provider, c)
			observeMigrating := migratingObservation(c, harness)
			migratingBaseline := observeIfSet(t, provider, observeMigrating)
			ctx := t.Context()
			envelope := envelopeFor(t, dataset, c, suiteFence)
			resolve := storagev1.ResolveOperationRequest{
				DatasetID: dataset, OperationID: envelope.OperationID, Method: c.method,
				RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
			}

			harness.Enter(t, provider)
			requireDatasetState(t, provider, storagev1.DatasetMigrating)
			refused := callWrite(ctx, t, provider, c, envelope, suiteFence)
			require.ErrorIs(t, refused.err, storagev1.ErrNotReady, "a write while the dataset is migrating")
			observeWhileMigrating(t, provider, observeMigrating, migratingBaseline, "a refused write changed state")
			_, err := provider.ResolveOperation(ctx, resolve)
			require.ErrorIs(t, err, storagev1.ErrNotReady, "resolution records nothing while the dataset is migrating")
			requireReadServed(t, provider)
			harness.Exit(t, provider)
			requireDatasetState(t, provider, storagev1.DatasetReady)
			require.Equal(t, baseline, c.observe(t, provider), "a write refused while the dataset was migrating changed state")

			first := callWrite(ctx, t, provider, c, envelope, suiteFence)
			require.NoError(t, first.err, "the refusal recorded nothing, so the operation commits once the dataset is ready")
			if c.afterCommit != nil {
				c.afterCommit(t, provider)
			}
			after := c.observe(t, provider)
			migratingAfter := observeIfSet(t, provider, observeMigrating)

			harness.Enter(t, provider)
			requireSameOutcome(t, first, callWrite(ctx, t, provider, c, envelope, suiteFence))
			observeWhileMigrating(t, provider, observeMigrating, migratingAfter, "a replay while the dataset is migrating changed state")
			resolved, err := provider.ResolveOperation(ctx, resolve)
			require.NoError(t, err, "resolution returns an outcome the dataset already holds")
			require.Equal(t, storagev1.OperationCommitted, resolved.State)
			fresh := envelopeFor(t, dataset, c, suiteFence)
			require.ErrorIs(t, callWrite(ctx, t, provider, c, fresh, suiteFence).err, storagev1.ErrNotReady)
			observeWhileMigrating(t, provider, observeMigrating, migratingAfter, "a refused write changed state")
			requireReadServed(t, provider)
			harness.Exit(t, provider)
			require.Equal(t, after, c.observe(t, provider),
				"a replay or a refused write while the dataset was migrating changed state")
		})
	}
}

// migratingObservation returns how c is observed while the dataset is
// migrating: through observe, unless observe calls a catalog write the
// provider refuses then, in which case through observeReads, which is nil
// when no catalog read serves the records.
func migratingObservation[P suiteProvider](c writeCase[P], harness MaintenanceHarness) func(*testing.T, P) any {
	if c.observeWrites && harness.RefusesReadingWrites {
		return c.observeReads
	}
	return c.observe
}

func observeIfSet[P suiteProvider](t *testing.T, provider P, observe func(*testing.T, P) any) any {
	t.Helper()
	if observe == nil {
		return nil
	}
	return observe(t, provider)
}

// observeWhileMigrating checks, while the dataset is migrating, that the
// records observe reads are served and still equal want.
func observeWhileMigrating[P suiteProvider](t *testing.T, provider P, observe func(*testing.T, P) any, want any, message string) {
	t.Helper()
	if observe == nil {
		return
	}
	require.Equal(t, want, observe(t, provider), message)
}

// requireReadServed checks that a read is served while the dataset is
// migrating, with GetWriteWatermark, a read every provider serves for both
// contracts, including for a case with no read of its own to compare.
func requireReadServed(t *testing.T, provider storagev1.WriteWatermarkStore) {
	t.Helper()
	_, err := provider.GetWriteWatermark(t.Context())
	require.NoError(t, err, "a read while the dataset is migrating")
}

func requireDatasetState(t *testing.T, provider storagev1.ProviderDescriber, want storagev1.DatasetState) {
	t.Helper()
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, info.State)
}
