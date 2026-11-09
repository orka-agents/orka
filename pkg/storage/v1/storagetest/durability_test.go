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
// committing, as stopping the primary would.
type fakeReplicated struct {
	// The failover cases never bind the dataset.
	storagev1.DatasetBindingStore

	// holdless makes HoldWrites hold nothing, as a harness whose lock does
	// not reach the database would. refuse makes the failover refuse the
	// held writes with ErrNotReady instead. emptyResolution makes
	// ResolveOperation settle a missing receipt with an empty outcome and no
	// error.
	holdless, refuse, emptyResolution bool

	mu          sync.Mutex
	incarnation int
	epoch       *storagev1.ControllerEpochMirrorState
	watermark   int64
	receipts    map[string]storagev1.OperationOutcome
	release     chan struct{}
	waiting     int
}

func newFakeReplicated(holdless, refuse, emptyResolution bool) func(*testing.T) *fakeReplicated {
	return func(*testing.T) *fakeReplicated {
		return &fakeReplicated{
			holdless: holdless, refuse: refuse, emptyResolution: emptyResolution,
			incarnation: 1, receipts: map[string]storagev1.OperationOutcome{},
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
		if outcome.State != storagev1.OperationCommitted {
			return storagev1.WriteWatermark{}, storagev1.ErrOperationAborted
		}
		return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.watermark}, nil
	}
	if release := p.release; release != nil {
		p.waiting++
		p.mu.Unlock()
		<-release
		p.mu.Lock()
		if p.refuse {
			return storagev1.WriteWatermark{}, fmt.Errorf("%w: the standby is being promoted", storagev1.ErrNotReady)
		}
		return storagev1.WriteWatermark{}, fmt.Errorf("%w: the primary stopped", storagev1.ErrOutcomeUnknown)
	}
	p.watermark++
	p.receipts[envelope.OperationID] = storagev1.OperationOutcome{
		OperationID: envelope.OperationID, Method: "AdvanceWriteWatermark", RequestDigest: envelope.RequestDigest,
		State: storagev1.OperationCommitted, Fence: envelope.Fence, IssuedAt: envelope.IssuedAt,
	}
	return storagev1.WriteWatermark{DatasetID: fakeDataset, Value: p.watermark}, nil
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
	p.incarnation++
	if p.release != nil {
		close(p.release)
		p.release = nil
	}
	return p
}

func reopenFakeReplicated(_ *testing.T, p *fakeReplicated) *fakeReplicated { return p.failover() }

func holdFakeReplicatedWrites(t *testing.T, p *fakeReplicated) func(int) { return p.holdWrites(t) }

// A synchronous-replicated provider passes the writes-during-a-failover case
// when the switch ends its held writes with an unknown outcome and when it
// refuses them as not ready: neither committed, and both settle as aborted.
func TestWritesDuringFailoverPassAProviderThatSettlesThem(t *testing.T) {
	for name, refuse := range map[string]bool{"unknown outcome": false, "refused as not ready": true} {
		t.Run(name, func(t *testing.T) {
			runWritesDuringFailover(t, newFakeReplicated(false, refuse, false)(t), reopenFakeReplicated, holdFakeReplicatedWrites)
		})
	}
}

const failingFailoverEnv = "STORAGETEST_FAILING_FAILOVER"

// The durability cases fail a harness whose writes finish before the
// failover begins, a provider that settles a write with an outcome that is
// neither committed nor aborted, a harness that declares failovers without
// HoldWrites, and an async-replicated provider that declares a failover.
func TestDurabilityFailsAFailoverItCannotCheck(t *testing.T) {
	failover := func(_ *testing.T, p storagev1.ProviderDescriber) storagev1.ProviderDescriber {
		return p.(*fakeReplicated).failover()
	}
	restart := func(_ *testing.T, p storagev1.ProviderDescriber) storagev1.ProviderDescriber { return p }
	variants := map[string]struct {
		run  func(t *testing.T)
		want string
	}{
		"writes not in flight": {
			run: func(t *testing.T) {
				runWritesDuringFailover(t, newFakeReplicated(true, false, false)(t), reopenFakeReplicated, holdFakeReplicatedWrites)
			},
			want: "was not in flight when the failover began",
		},
		"empty resolution": {
			run: func(t *testing.T) {
				runWritesDuringFailover(t, newFakeReplicated(false, false, true)(t), reopenFakeReplicated, holdFakeReplicatedWrites)
			},
			want: "settles as committed or aborted",
		},
		"failovers without HoldWrites": {
			run: func(t *testing.T) {
				runDurability(t, newFakeReplicated(false, false, false), nil, DurabilityHarness{
					Restart: restart, Failovers: map[string]func(*testing.T, storagev1.ProviderDescriber) storagev1.ProviderDescriber{"promote": failover},
				})
			},
			want: "needs HoldWrites",
		},
		"async-replicated failover": {
			run: func(t *testing.T) {
				runDurability(t, func(t *testing.T) *fakeAsync {
					return &fakeAsync{fakeReplicated: newFakeReplicated(false, false, false)(t)}
				}, nil, DurabilityHarness{
					Restart: restart,
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
