package storagetest

import (
	"errors"
	"slices"
	"sync"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// DurabilityHarness restarts a provider, and forces each automatic database
// switch it declares, over the same dataset. A provider supplies it through
// WithDurability; the suite then checks that every committed write and its
// operation receipt survive.
type DurabilityHarness struct {
	// Restart stops the provider and starts it again over the same dataset,
	// and returns the provider that now serves it, of the same kind as the
	// one it was given. An embedded provider closes and reopens its dataset;
	// an adapter restarts its own process. Restart releases what it opens
	// with t.Cleanup.
	Restart func(t *testing.T, provider storagev1.ProviderDescriber) storagev1.ProviderDescriber
	// Failovers forces, by name, each automatic database switch the provider
	// declares, such as promoting a synchronous standby, and returns the
	// provider that serves the dataset afterwards. A provider whose
	// durability class is local-fsync or local-buffered declares none; one
	// that declares synchronous-replicated supplies at least one.
	Failovers map[string]func(t *testing.T, provider storagev1.ProviderDescriber) storagev1.ProviderDescriber
}

// WithDurability enables the durability cases: after a restart, and after
// each declared failover, every write in the write catalog is still present
// with its operation receipt, a replay returns its original outcome without
// a second change, and the dataset keeps its identity, controller epoch,
// installation and write watermark. Each failover also changes the reported
// incarnation, and the writes that run while it happens either survive or
// settle as aborted through ResolveOperation.
func WithDurability(harness DurabilityHarness) Option {
	return func(options *suiteOptions) { options.durability = &harness }
}

// failoverWriters is how many watermark writes run while a failover happens.
const failoverWriters = 8

func runDurability[P suiteProvider](t *testing.T, newProvider func(t *testing.T) P, cases []writeCase[P], harness DurabilityHarness) {
	require.NotNil(t, harness.Restart, "a durability harness needs Restart")
	t.Run("declared failovers match the durability class", func(t *testing.T) {
		info, err := newProvider(t).Describe(t.Context())
		require.NoError(t, err)
		switch info.Durability {
		case storagev1.DurabilityLocalFsync, storagev1.DurabilityLocalBuffered:
			require.Empty(t, harness.Failovers, "a %s provider has no automatic database switch", info.Durability)
		case storagev1.DurabilitySynchronousReplicated:
			require.NotEmpty(t, harness.Failovers, "a synchronous-replicated provider must exercise its automatic switch")
		}
	})
	switches := map[string]func(t *testing.T, provider storagev1.ProviderDescriber) storagev1.ProviderDescriber{
		"restart": harness.Restart,
	}
	for name, failover := range harness.Failovers {
		require.NotEqual(t, "restart", name, "a failover cannot be named restart")
		switches[name] = failover
	}
	names := make([]string, 0, len(switches))
	for name := range switches {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		reopen := func(t *testing.T, provider P) P {
			t.Helper()
			reopened, ok := switches[name](t, provider).(P)
			require.Truef(t, ok, "%s returned a provider of another kind", name)
			return reopened
		}
		failover := name != "restart"
		t.Run(name, func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.method, func(t *testing.T) {
					runDurableWrite(t, newProvider(t), c, reopen, failover)
				})
			}
			if failover {
				t.Run("writes during the switch", func(t *testing.T) {
					runWritesDuringFailover(t, newProvider(t), reopen)
				})
			}
		})
	}
}

// durableState is what a dataset holds apart from the records a write
// changes.
type durableState struct {
	datasetID string
	mirror    storagev1.ControllerEpochMirrorState
	watermark storagev1.WriteWatermark
}

func readDurableState[P suiteProvider](t *testing.T, provider P) (durableState, string) {
	t.Helper()
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	mirror, err := provider.GetControllerEpochMirror(t.Context(), controlEpochName)
	require.NoError(t, err)
	watermark, err := provider.GetWriteWatermark(t.Context())
	require.NoError(t, err)
	return durableState{datasetID: info.DatasetID, mirror: mirror, watermark: watermark}, info.Incarnation
}

// runDurableWrite commits one write, restarts or fails over the provider, and
// checks that the change, its receipt and the dataset's ownership survive.
func runDurableWrite[P suiteProvider](t *testing.T, provider P, c writeCase[P], reopen func(*testing.T, P) P, failover bool) {
	dataset, _ := prepareWriteCase(t, provider, c)
	ctx := t.Context()
	_, _, err := advanceWatermark(t, provider, suiteFence)
	require.NoError(t, err)
	envelope := envelopeFor(t, dataset, c, suiteFence)
	first := callWrite(ctx, t, provider, c, envelope, suiteFence)
	require.NoError(t, first.err)
	if c.afterCommit != nil {
		c.afterCommit(t, provider)
	}
	after := c.observe(t, provider)
	before, incarnation := readDurableState(t, provider)

	provider = reopen(t, provider)
	survived, reopenedIncarnation := readDurableState(t, provider)
	require.Equal(t, before, survived, "the dataset's identity, epoch, installation and watermark survive")
	require.NotEmpty(t, reopenedIncarnation)
	if failover {
		require.NotEqual(t, incarnation, reopenedIncarnation, "a failover changes the incarnation")
	}
	require.Equal(t, after, c.observe(t, provider), "the committed change survives")

	requireSameOutcome(t, first, callWrite(ctx, t, provider, c, envelope, suiteFence))
	require.Equal(t, after, c.observe(t, provider), "a replay after the switch changed state")
	resolved, err := provider.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: dataset, OperationID: envelope.OperationID, Method: c.method,
		RequestDigest: envelope.RequestDigest, IssuedAt: envelope.IssuedAt,
	})
	require.NoError(t, err)
	require.Equal(t, storagev1.OperationCommitted, resolved.State, "the receipt survives with the change")

	advanceEpoch(t, provider, nextFence)
	stale := envelopeFor(t, dataset, c, suiteFence)
	requireFenced(t, callWrite(ctx, t, provider, c, stale, suiteFence).err)
}

// runWritesDuringFailover runs watermark writes while the provider fails
// over. Every write that returned nil survives with its receipt; every write
// whose outcome is unknown settles through ResolveOperation as committed and
// present, or as aborted, absent and refused if it arrives late.
func runWritesDuringFailover[P suiteProvider](t *testing.T, provider P, reopen func(*testing.T, P) P) {
	ctx := t.Context()
	advanceEpoch(t, provider, suiteFence)
	dataset := datasetID(t, provider)
	envelopes := make([]storagev1.WriteEnvelope, failoverWriters)
	for i := range envelopes {
		envelopes[i] = NewWriteEnvelope(t, dataset, suiteFence, "AdvanceWriteWatermark")
	}
	errs := make([]error, failoverWriters)
	var wg sync.WaitGroup
	for i := range failoverWriters {
		wg.Go(func() {
			_, errs[i] = provider.AdvanceWriteWatermark(storagev1.WithWriteEnvelope(ctx, envelopes[i]))
		})
	}
	reopened := reopen(t, provider)
	wg.Wait()

	committed := int64(0)
	var aborted []storagev1.WriteEnvelope
	for i, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, storagev1.ErrOutcomeUnknown), errors.Is(err, storagev1.ErrUnavailable):
		default:
			require.NoErrorf(t, err, "a write during a failover either commits or has an unknown outcome")
		}
		outcome, err := reopened.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
			DatasetID: dataset, OperationID: envelopes[i].OperationID, Method: "AdvanceWriteWatermark",
			RequestDigest: envelopes[i].RequestDigest, IssuedAt: envelopes[i].IssuedAt,
		})
		require.NoError(t, err)
		if errs[i] == nil {
			require.Equal(t, storagev1.OperationCommitted, outcome.State, "a write that returned nil survives the failover")
		}
		switch outcome.State {
		case storagev1.OperationCommitted:
			committed++
		case storagev1.OperationAborted:
			aborted = append(aborted, envelopes[i])
		}
	}
	watermark, err := reopened.GetWriteWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, committed, watermark.Value, "exactly the committed writes advanced the watermark")
	for _, envelope := range aborted {
		_, err := reopened.AdvanceWriteWatermark(storagev1.WithWriteEnvelope(ctx, envelope))
		require.ErrorIs(t, err, storagev1.ErrOperationAborted, "an aborted write arriving late is refused")
	}
	again, err := reopened.GetWriteWatermark(ctx)
	require.NoError(t, err)
	require.Equal(t, watermark, again)
}
