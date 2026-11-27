package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

type MaintenanceHarness struct {
	Enter                func(t *testing.T, provider storagev1.ProviderDescriber)
	Exit                 func(t *testing.T, provider storagev1.ProviderDescriber)
	RefusesReadingWrites bool
}

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

			harness.Enter(t, harnessProvider(provider))
			requireDatasetState(t, provider, storagev1.DatasetMigrating)
			refused := callWrite(ctx, t, provider, c, envelope, suiteFence)
			require.ErrorIs(t, refused.err, storagev1.ErrNotReady, "a write while the dataset is migrating")
			observeWhileMigrating(t, provider, observeMigrating, migratingBaseline, "a refused write changed state")
			_, err := provider.ResolveOperation(ctx, resolve)
			require.ErrorIs(t, err, storagev1.ErrNotReady, "resolution records nothing while the dataset is migrating")
			requireReadServed(t, provider)
			harness.Exit(t, harnessProvider(provider))
			requireDatasetState(t, provider, storagev1.DatasetReady)
			require.Equal(t, baseline, c.observe(t, provider), "a write refused while the dataset was migrating changed state")

			first := callWrite(ctx, t, provider, c, envelope, suiteFence)
			require.NoError(t, first.err, "the refusal recorded nothing, so the operation commits once the dataset is ready")
			if c.afterCommit != nil {
				c.afterCommit(t, provider)
			}
			after := c.observe(t, provider)
			migratingAfter := observeIfSet(t, provider, observeMigrating)

			harness.Enter(t, harnessProvider(provider))
			requireSameOutcome(t, first, callWrite(ctx, t, provider, c, envelope, suiteFence))
			observeWhileMigrating(t, provider, observeMigrating, migratingAfter, "a replay while the dataset is migrating changed state")
			resolved, err := provider.ResolveOperation(ctx, resolve)
			require.NoError(t, err, "resolution returns an outcome the dataset already holds")
			require.Equal(t, storagev1.OperationCommitted, resolved.State)
			fresh := envelopeFor(t, dataset, c, suiteFence)
			require.ErrorIs(t, callWrite(ctx, t, provider, c, fresh, suiteFence).err, storagev1.ErrNotReady)
			observeWhileMigrating(t, provider, observeMigrating, migratingAfter, "a refused write changed state")
			requireReadServed(t, provider)
			harness.Exit(t, harnessProvider(provider))
			require.Equal(t, after, c.observe(t, provider),
				"a replay or a refused write while the dataset was migrating changed state")
		})
	}
}

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

func observeWhileMigrating[P suiteProvider](t *testing.T, provider P, observe func(*testing.T, P) any, want any, message string) {
	t.Helper()
	if observe == nil {
		return
	}
	require.Equal(t, want, observe(t, provider), message)
}

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
