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
}

// WithMaintenance enables the maintenance cases: while the dataset is
// migrating, every write in the write catalog is refused with ErrNotReady and
// changes nothing, a replay of a committed operation and reads are still
// served, Describe reports DatasetMigrating, and ResolveOperation records
// nothing new; once it is ready again, the refused operation commits.
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
			require.Equal(t, baseline, c.observe(t, provider), "a refused write changed state")
			_, err := provider.ResolveOperation(ctx, resolve)
			require.ErrorIs(t, err, storagev1.ErrNotReady, "resolution records nothing while the dataset is migrating")
			harness.Exit(t, provider)
			requireDatasetState(t, provider, storagev1.DatasetReady)

			first := callWrite(ctx, t, provider, c, envelope, suiteFence)
			require.NoError(t, first.err, "the refusal recorded nothing, so the operation commits once the dataset is ready")
			if c.afterCommit != nil {
				c.afterCommit(t, provider)
			}
			after := c.observe(t, provider)

			harness.Enter(t, provider)
			requireSameOutcome(t, first, callWrite(ctx, t, provider, c, envelope, suiteFence))
			require.Equal(t, after, c.observe(t, provider), "a replay while the dataset is migrating changed state")
			resolved, err := provider.ResolveOperation(ctx, resolve)
			require.NoError(t, err, "resolution returns an outcome the dataset already holds")
			require.Equal(t, storagev1.OperationCommitted, resolved.State)
			fresh := envelopeFor(t, dataset, c, suiteFence)
			require.ErrorIs(t, callWrite(ctx, t, provider, c, fresh, suiteFence).err, storagev1.ErrNotReady)
			require.Equal(t, after, c.observe(t, provider))
			harness.Exit(t, provider)
		})
	}
}

func requireDatasetState(t *testing.T, provider storagev1.ProviderDescriber, want storagev1.DatasetState) {
	t.Helper()
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, info.State)
}
