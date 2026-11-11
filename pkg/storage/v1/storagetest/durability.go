package storagetest

import (
	"errors"
	"slices"
	"sync"
	"sync/atomic"
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
	// one it was given: the provider the factory returned, or one a previous
	// switch returned. An embedded provider closes and reopens its dataset;
	// an adapter restarts its own process. Restart releases what it opens
	// with t.Cleanup.
	Restart func(t *testing.T, provider storagev1.ProviderDescriber) storagev1.ProviderDescriber
	// Failovers forces, by name, each automatic database switch the provider
	// declares, such as promoting a synchronous standby, and returns the
	// provider that serves the dataset afterwards. A provider whose
	// durability class is local-fsync or local-buffered declares none; one
	// that declares synchronous-replicated supplies at least one. An
	// async-replicated provider declares none either: its switch may lose
	// acknowledged writes, which these cases do not allow, and Orka holds the
	// installation after one instead.
	Failovers map[string]func(t *testing.T, provider storagev1.ProviderDescriber) storagev1.ProviderDescriber
	// HoldWrites makes the provider's next writes to the dataset wait inside
	// the database that serves it, as writes wait behind a row lock another
	// session holds, and returns a function that returns once n of them are
	// waiting there. A provider that declares failovers supplies it, so the
	// suite starts each failover while writes are in flight. The failover
	// ends the wait, for example by stopping the database that holds the
	// lock; the writes it holds must have returned by the time the failover
	// returns.
	HoldWrites func(t *testing.T, provider storagev1.ProviderDescriber) (waiting func(n int))
}

// WithDurability enables the durability cases: after a restart, and after
// each declared failover, every write in the write catalog is still present
// with its operation receipt, a replay returns its original outcome without
// a second change, and the dataset keeps its identity, controller epoch,
// installation and write watermark. Each failover also changes the reported
// incarnation, and the writes HoldWrites keeps in flight while it happens
// settle through ResolveOperation as their errors said: one that returned nil
// survives, one refused as unavailable or not ready committed nothing, and
// only one whose outcome is unknown may settle either way.
func WithDurability(harness DurabilityHarness) Option {
	return func(options *suiteOptions) { options.durability = &harness }
}

// failoverWriters is how many watermark writes are in flight while a
// failover happens.
const failoverWriters = 8

func runDurability[P suiteProvider](t *testing.T, newProvider func(t *testing.T) P, cases []writeCase[P], harness DurabilityHarness) {
	require.NotNil(t, harness.Restart, "a durability harness needs Restart")
	require.True(t, len(harness.Failovers) == 0 || harness.HoldWrites != nil,
		"a durability harness that declares failovers needs HoldWrites")
	t.Run("declared failovers match the durability class", func(t *testing.T) {
		info, err := newProvider(t).Describe(t.Context())
		require.NoError(t, err)
		switch info.Durability {
		case storagev1.DurabilityLocalFsync, storagev1.DurabilityLocalBuffered:
			require.Empty(t, harness.Failovers, "a %s provider has no automatic database switch", info.Durability)
		case storagev1.DurabilityAsyncReplicated:
			require.Empty(t, harness.Failovers,
				"an async-replicated switch may lose acknowledged writes, which the failover cases do not allow")
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
			returned := switches[name](t, harnessProvider(provider))
			reopened, ok := rewrapHarnessProvider(provider, returned).(P)
			require.Truef(t, ok, "%s returned a provider of another kind", name)
			return reopened
		}
		holdWrites := func(t *testing.T, provider P) func(int) {
			t.Helper()
			return harness.HoldWrites(t, harnessProvider(provider))
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
					runWritesDuringFailover(t, newProvider(t), reopen, holdWrites)
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

// runWritesDuringFailover holds watermark writes in the database and fails
// the provider over while they are in flight. Every write that returned nil
// survives with its receipt. Every write refused as unavailable or not ready
// settles through ResolveOperation as aborted, because those errors promise
// that nothing committed and a caller may repeat the action as a new
// operation; a write the provider cannot rule out returns ErrOutcomeUnknown
// instead. A write whose outcome is unknown settles as committed and
// present, or as aborted. An aborted write is absent and refused if it
// arrives late.
func runWritesDuringFailover[P suiteProvider](
	t *testing.T, provider P, reopen func(*testing.T, P) P, holdWrites func(*testing.T, P) func(int),
) {
	ctx := t.Context()
	advanceEpoch(t, provider, suiteFence)
	dataset := datasetID(t, provider)
	envelopes := make([]storagev1.WriteEnvelope, failoverWriters)
	for i := range envelopes {
		envelopes[i] = NewWriteEnvelope(t, dataset, suiteFence, "AdvanceWriteWatermark")
	}
	waiting := holdWrites(t, provider)
	errs := make([]error, failoverWriters)
	// late marks a write that began after the failover began, and early one
	// that returned before it; neither was in flight across the switch.
	late := make([]bool, failoverWriters)
	early := make([]bool, failoverWriters)
	var switching atomic.Bool
	var wg sync.WaitGroup
	for i := range failoverWriters {
		wg.Go(func() {
			late[i] = switching.Load()
			_, errs[i] = provider.AdvanceWriteWatermark(storagev1.WithWriteEnvelope(ctx, envelopes[i]))
			early[i] = !switching.Load()
		})
	}
	waiting(failoverWriters)
	switching.Store(true)
	reopened := reopen(t, provider)
	wg.Wait()

	committed := int64(0)
	var aborted []storagev1.WriteEnvelope
	for i, err := range errs {
		require.Falsef(t, late[i] || early[i],
			"write %d was not in flight when the failover began (began after it: %t, returned before it: %t, error: %v); "+
				"HoldWrites must keep it waiting in the database", i, late[i], early[i], err)
		refused := false
		switch {
		case err == nil, errors.Is(err, storagev1.ErrOutcomeUnknown):
		case errors.Is(err, storagev1.ErrUnavailable), errors.Is(err, storagev1.ErrNotReady):
			refused = true
		default:
			require.NoErrorf(t, err, "a write during a failover commits, has an unknown outcome, or is refused as unavailable or not ready")
		}
		outcome, err := reopened.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
			DatasetID: dataset, OperationID: envelopes[i].OperationID, Method: "AdvanceWriteWatermark",
			RequestDigest: envelopes[i].RequestDigest, IssuedAt: envelopes[i].IssuedAt,
		})
		require.NoError(t, err)
		require.Containsf(t, []storagev1.OperationState{storagev1.OperationCommitted, storagev1.OperationAborted}, outcome.State,
			"a write during a failover settles as committed or aborted, not %q", outcome.State)
		if errs[i] == nil {
			require.Equal(t, storagev1.OperationCommitted, outcome.State, "a write that returned nil survives the failover")
		}
		if refused {
			require.Equalf(t, storagev1.OperationAborted, outcome.State,
				"write %d was refused as unavailable or not ready (%v), but it committed: "+
					"a write that may have committed returns ErrOutcomeUnknown", i, errs[i])
		}
		if outcome.State == storagev1.OperationCommitted {
			committed++
		} else {
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
