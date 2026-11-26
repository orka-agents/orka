package storagetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// fakeReplicated is a synchronous-replicated watermark provider whose
// failover keeps every committed write. HoldWrites makes new watermark writes
// wait until the failover, which ends them with ErrOutcomeUnknown without
// committing, as stopping the primary would. Its faults change how a held
// write ends, which some combinations do correctly, or break the contract or
// the harness.
type fakeReplicated struct {
	// The failover cases never bind the dataset.
	storagev1.DatasetBindingStore
	fakeReplicatedFaults

	mu          sync.Mutex
	incarnation int
	epoch       *storagev1.ControllerEpochMirrorState
	watermark   int64
	receipts    map[string]storagev1.OperationOutcome
	// values holds the watermark each committed operation returned, for
	// its replays.
	values  map[string]int64
	release chan struct{}
	waiting int
}

// fakeReplicatedFaults change how a fakeReplicated ends a held write, which
// the error and commit settings together do correctly or not, or break the
// contract or the harness. The zero value breaks nothing.
type fakeReplicatedFaults struct {
	// holdless makes HoldWrites hold nothing, as a harness whose lock does
	// not reach the database would.
	holdless bool
	// heldErr is what a held write returns once the failover ends it, in
	// place of ErrOutcomeUnknown; heldSucceeds makes it return its
	// watermark with no error instead.
	heldErr      error
	heldSucceeds bool
	// commitHeld commits a held write, with its receipt, before it returns;
	// advanceHeld advances the watermark for it but keeps no receipt.
	commitHeld, advanceHeld bool
	// emptyResolution makes ResolveOperation settle a missing receipt with
	// an empty outcome and no error.
	emptyResolution bool
	// keepIncarnation makes a failover keep the incarnation.
	keepIncarnation bool
	// commitAborted commits a write whose operation settled as aborted.
	commitAborted bool
	// loseReceipts makes a failover drop every receipt while keeping the
	// writes, and a replay of a write whose receipt it dropped record the
	// receipt again and return the write's value, as an idempotent write
	// does.
	loseReceipts bool
}

func newFakeReplicated(faults fakeReplicatedFaults) func(*testing.T) *fakeReplicated {
	return func(*testing.T) *fakeReplicated {
		return &fakeReplicated{
			fakeReplicatedFaults: faults, incarnation: 1,
			receipts: map[string]storagev1.OperationOutcome{}, values: map[string]int64{},
		}
	}
}

func (p *fakeReplicated) Describe(context.Context) (storagev1.ProviderInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return storagev1.ProviderInfo{
		DatasetID: fakeDataset, State: storagev1.DatasetReady, Durability: storagev1.DurabilitySynchronousReplicated,
		Incarnation: fmt.Sprintf("incarnation-%d", p.incarnation),
	}, nil
}

func (p *fakeReplicated) AdvanceControllerEpoch(
	_ context.Context, request storagev1.AdvanceControllerEpochRequest,
) (storagev1.ControllerEpochMirrorState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.epoch = &storagev1.ControllerEpochMirrorState{
		DatasetID: request.DatasetID, Epoch: request.Epoch, InstallationID: request.InstallationID,
	}
	return *p.epoch, nil
}

func (p *fakeReplicated) GetControllerEpochMirror(context.Context, string) (storagev1.ControllerEpochMirrorState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.epoch == nil {
		return storagev1.ControllerEpochMirrorState{}, storagev1.ErrNotFound
	}
	return *p.epoch, nil
}

func (p *fakeReplicated) ResolveOperation(
	_ context.Context, request storagev1.ResolveOperationRequest,
) (storagev1.OperationOutcome, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if outcome, ok := p.receipts[request.OperationID]; ok {
		return outcome, nil
	}
	if p.emptyResolution {
		return storagev1.OperationOutcome{}, nil
	}
	outcome := storagev1.OperationOutcome{
		OperationID: request.OperationID, Method: request.Method, RequestDigest: request.RequestDigest,
		State: storagev1.OperationAborted, IssuedAt: request.IssuedAt,
	}
	p.receipts[request.OperationID] = outcome
	return outcome, nil
}

func (p *fakeReplicated) PruneOperationReceipts(context.Context, int) (int, error) { return 0, nil }

func (p *fakeReplicated) AdvanceWriteWatermark(ctx context.Context) (storagev1.WriteWatermark, error) {
	envelope, ok := storagev1.WriteEnvelopeFrom(ctx)
	if !ok {
		return storagev1.WriteWatermark{}, storagev1.ValidationErrorf("AdvanceWriteWatermark requires a write envelope")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if outcome, ok := p.receipts[envelope.OperationID]; ok {
		switch {
		case outcome.State == storagev1.OperationCommitted:
			return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.values[envelope.OperationID]}, nil
		case !p.commitAborted:
			return storagev1.WriteWatermark{}, storagev1.ErrOperationAborted
		}
	}
	if value, ok := p.values[envelope.OperationID]; ok && p.loseReceipts {
		p.receipts[envelope.OperationID] = committedOutcome(envelope)
		return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: value}, nil
	}
	if p.epoch == nil || envelope.Fence.Epoch != p.epoch.Epoch.Epoch || envelope.Fence.HolderID != p.epoch.Epoch.HolderID {
		return storagev1.WriteWatermark{}, storagev1.FencedErrorf("the write's fence is not the dataset's controller epoch")
	}
	if release := p.release; release != nil {
		p.waiting++
		p.mu.Unlock()
		<-release
		p.mu.Lock()
		switch {
		case p.commitHeld:
			p.commit(envelope)
		case p.advanceHeld:
			p.watermark++
		}
		switch {
		case p.heldSucceeds:
			return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.watermark}, nil
		case p.heldErr != nil:
			return storagev1.WriteWatermark{}, p.heldErr
		}
		return storagev1.WriteWatermark{}, fmt.Errorf("%w: the primary stopped", storagev1.ErrOutcomeUnknown)
	}
	p.commit(envelope)
	return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.watermark}, nil
}

// commit advances the watermark for envelope's operation and keeps its
// receipt.
func (p *fakeReplicated) commit(envelope storagev1.WriteEnvelope) {
	p.watermark++
	p.values[envelope.OperationID] = p.watermark
	p.receipts[envelope.OperationID] = committedOutcome(envelope)
}

// committedOutcome is the receipt of envelope's committed watermark write.
func committedOutcome(envelope storagev1.WriteEnvelope) storagev1.OperationOutcome {
	return storagev1.OperationOutcome{
		OperationID: envelope.OperationID, Method: "AdvanceWriteWatermark", RequestDigest: envelope.RequestDigest,
		State: storagev1.OperationCommitted, Fence: envelope.Fence, IssuedAt: envelope.IssuedAt,
	}
}

func (p *fakeReplicated) GetWriteWatermark(context.Context) (storagev1.WriteWatermark, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.watermark}, nil
}

// holdWrites makes the next writes wait until the failover, and returns a
// function that waits until n of them are waiting.
func (p *fakeReplicated) holdWrites(t *testing.T) func(int) {
	if p.holdless {
		return func(int) {}
	}
	p.mu.Lock()
	p.release = make(chan struct{})
	p.mu.Unlock()
	return func(n int) {
		require.Eventually(t, func() bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			return p.waiting >= n
		}, 10*time.Second, time.Millisecond, "%d writes never waited in the database", n)
	}
}

// failover promotes the standby: committed writes stay, the incarnation
// changes, and the held writes end.
func (p *fakeReplicated) failover() *fakeReplicated {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.keepIncarnation {
		p.incarnation++
	}
	if p.loseReceipts {
		p.receipts = map[string]storagev1.OperationOutcome{}
	}
	if p.release != nil {
		close(p.release)
		p.release = nil
	}
	return p
}

func reopenFakeReplicated(_ *testing.T, p *fakeReplicated) *fakeReplicated { return p.failover() }

func holdFakeReplicatedWrites(t *testing.T, p *fakeReplicated) func(int) { return p.holdWrites(t) }

// fakeReplicatedHarness restarts a fakeReplicated by keeping it, and fails
// it over by promoting its standby.
var fakeReplicatedHarness = DurabilityHarness{
	Restart: func(_ *testing.T, p storagev1.ProviderDescriber) storagev1.ProviderDescriber { return p },
	Failovers: map[string]func(*testing.T, storagev1.ProviderDescriber) storagev1.ProviderDescriber{
		"promote": func(_ *testing.T, p storagev1.ProviderDescriber) storagev1.ProviderDescriber {
			return p.(*fakeReplicated).failover()
		},
	},
	HoldWrites: func(t *testing.T, p storagev1.ProviderDescriber) func(int) { return p.(*fakeReplicated).holdWrites(t) },
}

// fakeReplicatedWrites is the write catalog of a fakeReplicated.
var fakeReplicatedWrites = []writeCase[*fakeReplicated]{{
	method: "AdvanceWriteWatermark", args: fixed(), observe: observeWatermark[*fakeReplicated],
}}

// A synchronous-replicated provider whose failover keeps every committed
// write passes the durability cases, its failover included.
func TestDurabilityPassesAProviderThatKeepsItsWrites(t *testing.T) {
	runDurability(t, newFakeReplicated(fakeReplicatedFaults{}), fakeReplicatedWrites, fakeReplicatedHarness)
}

// A synchronous-replicated provider passes the writes-during-a-failover case
// when the switch commits its held writes and reports them committed or
// unknown, ends them with an unknown outcome without committing them, or
// refuses them as unavailable or not ready without committing them.
func TestWritesDuringFailoverPassAProviderThatSettlesThem(t *testing.T) {
	for name, faults := range map[string]fakeReplicatedFaults{
		"committed":                 {commitHeld: true, heldSucceeds: true},
		"unknown outcome":           {},
		"unknown outcome committed": {commitHeld: true},
		"refused as not ready":      {heldErr: fmt.Errorf("%w: the standby is being promoted", storagev1.ErrNotReady)},
		"refused as unavailable":    {heldErr: fmt.Errorf("%w: the connection was refused", storagev1.ErrUnavailable)},
	} {
		t.Run(name, func(t *testing.T) {
			runWritesDuringFailover(t, newFakeReplicated(faults)(t), reopenFakeReplicated, holdFakeReplicatedWrites)
		})
	}
}

const failingFailoverEnv = "STORAGETEST_FAILING_FAILOVER"

// The durability cases fail a harness whose writes finish before the
// failover begins, a provider that settles a write with an outcome that is
// neither committed nor aborted, a harness that declares failovers without
// HoldWrites, and an async-replicated provider that declares a failover.
// They also fail a provider that breaks what a failover must keep: one whose
// failover keeps the incarnation, loses a write that returned nil, loses a
// receipt that a replay would record again, advances the watermark for a
// write it kept no receipt for, or commits a write it refused as unavailable
// or not ready, or one that settled as aborted.
func TestDurabilityFailsAFailoverItCannotCheck(t *testing.T) {
	duringFailover := func(faults fakeReplicatedFaults) func(t *testing.T) {
		return func(t *testing.T) {
			runWritesDuringFailover(t, newFakeReplicated(faults)(t), reopenFakeReplicated, holdFakeReplicatedWrites)
		}
	}
	variants := map[string]struct {
		run  func(t *testing.T)
		want string
	}{
		"writes not in flight": {
			run:  duringFailover(fakeReplicatedFaults{holdless: true}),
			want: "was not in flight when the failover began",
		},
		"empty resolution": {
			run:  duringFailover(fakeReplicatedFaults{emptyResolution: true}),
			want: "settles as committed or aborted",
		},
		"failovers without HoldWrites": {
			run: func(t *testing.T) {
				runDurability(t, newFakeReplicated(fakeReplicatedFaults{}), nil, DurabilityHarness{
					Restart: fakeReplicatedHarness.Restart, Failovers: fakeReplicatedHarness.Failovers,
				})
			},
			want: "needs HoldWrites",
		},
		"async-replicated failover": {
			run: func(t *testing.T) {
				runDurability(t, func(t *testing.T) *fakeAsync {
					return &fakeAsync{fakeReplicated: newFakeReplicated(fakeReplicatedFaults{})(t)}
				}, nil, DurabilityHarness{
					Restart: fakeReplicatedHarness.Restart,
					Failovers: map[string]func(*testing.T, storagev1.ProviderDescriber) storagev1.ProviderDescriber{
						"promote": func(_ *testing.T, p storagev1.ProviderDescriber) storagev1.ProviderDescriber {
							p.(*fakeAsync).failover()
							return p
						},
					},
					HoldWrites: func(t *testing.T, p storagev1.ProviderDescriber) func(int) {
						return p.(*fakeAsync).holdWrites(t)
					},
				})
			},
			want: "an async-replicated switch may lose acknowledged writes",
		},
		"failover keeps the incarnation": {
			run: func(t *testing.T) {
				runDurability(t, newFakeReplicated(fakeReplicatedFaults{keepIncarnation: true}), fakeReplicatedWrites, fakeReplicatedHarness)
			},
			want: "a failover changes the incarnation",
		},
		"receipt lost and recorded again by the replay": {
			run: func(t *testing.T) {
				runDurability(t, newFakeReplicated(fakeReplicatedFaults{loseReceipts: true}), fakeReplicatedWrites, fakeReplicatedHarness)
			},
			want: "the receipt survives with the change",
		},
		"write that returned nil lost": {
			run:  duringFailover(fakeReplicatedFaults{heldSucceeds: true}),
			want: "a write that returned nil survives the failover",
		},
		"watermark advanced without a receipt": {
			run:  duringFailover(fakeReplicatedFaults{advanceHeld: true}),
			want: "exactly the committed writes advanced the watermark",
		},
		"aborted write committed late": {
			run:  duringFailover(fakeReplicatedFaults{commitAborted: true}),
			want: "an aborted write arriving late is refused",
		},
		"refused as not ready but committed": {
			run: duringFailover(fakeReplicatedFaults{
				commitHeld: true, heldErr: fmt.Errorf("%w: the standby is being promoted", storagev1.ErrNotReady),
			}),
			want: "was refused as unavailable or not ready",
		},
		"refused as unavailable but committed": {
			run: duringFailover(fakeReplicatedFaults{
				commitHeld: true, heldErr: fmt.Errorf("%w: the connection was refused", storagev1.ErrUnavailable),
			}),
			want: "was refused as unavailable or not ready",
		},
	}
	if variant := os.Getenv(failingFailoverEnv); variant != "" {
		variants[variant].run(t)
		return
	}
	for name, variant := range variants {
		t.Run(name, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestDurabilityFailsAFailoverItCannotCheck$", "-test.count=1")
			command.Env = append(os.Environ(), failingFailoverEnv+"="+name)
			output, err := command.CombinedOutput()
			require.Error(t, err, "the durability cases passed %s:\n%s", name, output)
			require.Contains(t, string(output), variant.want)
		})
	}
}

// fakeAsync is fakeReplicated declaring the async-replicated class.
type fakeAsync struct{ *fakeReplicated }

func (p *fakeAsync) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	info, err := p.fakeReplicated.Describe(ctx)
	info.Durability = storagev1.DurabilityAsyncReplicated
	return info, err
}
