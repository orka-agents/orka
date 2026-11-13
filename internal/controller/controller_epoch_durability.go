package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	// DefaultWatermarkInterval is how often the manager advances each
	// dataset's write watermark and checks whether its database restarted or
	// switched.
	DefaultWatermarkInterval = time.Minute
	// DefaultHoldPollInterval is how often a held manager looks for an
	// operator's acknowledgement of the hold.
	DefaultHoldPollInterval = 10 * time.Second
	// storageHoldRequeue is how long a reconciler waits before it tries a side
	// effect again while storage is held for recovery.
	storageHoldRequeue = 30 * time.Second
)

// storageTakeover is the storage side of one controller epoch. It runs
// until its context ends:
//
//  1. Read the storage record Kubernetes keeps for the installation. While
//     it holds a storage recovery hold that no operator acknowledged, keep
//     every fence closed and wait for the acknowledgement. Once the hold is
//     acknowledged, advance every dataset, record what each holds now as
//     acknowledged, and clear the hold in the same update.
//  2. For each contract, in parallel: describe its dataset. A contract served
//     by another dataset than the one that acknowledged it raises a hold.
//     Then, once per dataset, read the epoch and write watermark the dataset
//     holds; a dataset holding a lower epoch or watermark than any contract
//     acknowledged on it raises a hold and is left as found. Otherwise
//     advance it to the acquired epoch, record the contract's
//     acknowledgement and open the contract's fence.
//  3. Alongside step 2, every WatermarkInterval, for each dataset that has
//     passed: describe it, and when its incarnation changed, check that it
//     still holds this controller's epoch and at least its acknowledged
//     watermark, and raise a hold for an async-replicated dataset. Then
//     advance its write watermark with a fenced write and record the new
//     value once the dataset acknowledged it.
//
// A raised hold closes every fence at once and is then recorded in
// Kubernetes, so a restart keeps it; the fences stay closed until an
// operator acknowledges it.
type storageTakeover struct {
	manager        *ControllerEpochManager
	acquired       store.ControllerEpoch
	fence          store.ControllerEpochFence
	installationID string
	records        store.StorageRecordStore
	contracts      map[string]*contractFence

	mu       sync.Mutex
	datasets map[string]*trackedDataset
}

// trackedDataset is one dataset the takeover advanced.
type trackedDataset struct {
	dataset   EpochDataset
	contracts []string
	// opened holds the contracts whose acknowledgement is recorded. A passed
	// switch check reopens only their fences.
	opened map[string]bool
	// incarnation is the last one checked: first the one the takeover's
	// advance described, then each one a switch check passed.
	incarnation string
	// checking is set while a restart or switch the loop saw has not passed
	// its check, so a contract acknowledged meanwhile waits for that check.
	checking bool
	// acknowledged is the highest write watermark recorded as acknowledged.
	acknowledged int64
}

// contractOutcome is how one contract's takeover step ended: with an
// acknowledgement to record, a hold to raise, or an error retrying cannot
// fix.
type contractOutcome struct {
	contract string
	ack      store.StorageAcknowledgement
	hold     *store.StorageRecoveryHold
	err      error
}

func (t *storageTakeover) run(ctx context.Context) error {
	log := logf.FromContext(ctx)
	for {
		record, err := retryEpochStorageStep(ctx, "read the storage record", func(ctx context.Context) (store.StorageRecord, error) {
			return t.records.GetStorageRecord(ctx, t.acquired.Name)
		})
		if err != nil {
			return err
		}
		if hold := record.Hold; hold != nil {
			t.manager.setHold(*hold)
			if record.AcknowledgedHoldID != hold.ID {
				log.Info("Storage is held for recovery; storage contract writes and control-store changes are refused until an operator acknowledges the hold",
					"hold", hold.ID, "reason", hold.Reason, "message", hold.Message)
				fencing, stopFencing := context.WithCancel(ctx)
				t.fenceHeld(fencing, record)
				err := t.waitForAcknowledgement(ctx, hold.ID)
				stopFencing()
				if err != nil {
					return err
				}
				continue
			}
			if err := t.release(ctx, hold.ID); err != nil {
				return err
			}
			log.Info("Released the acknowledged storage recovery hold and recorded what each dataset holds now", "hold", hold.ID)
			continue
		}
		// Each fence opens only once its dataset passes the takeover step,
		// also after a hold that left the record without this manager
		// releasing it, as an edit of the status does.
		t.manager.closeFences()
		t.manager.clearHold()
		hold, err := t.openAndMaintain(ctx, record)
		if err != nil || hold == nil {
			return err
		}
		hold.ID = t.manager.holdID()
		hold.RaisedAt = t.manager.clock()
		// Close every fence before recording the hold, so no write reaches a
		// dataset known to have lost writes while Kubernetes is slow. A hold
		// that cannot be recorded stays in place until the process stops.
		t.manager.setHold(*hold)
		log.Error(errors.New(hold.Message), "Storage lost acknowledged writes or switched unsafely; holding storage contract writes and control-store changes for recovery",
			"hold", hold.ID, "reason", hold.Reason, "contract", hold.Contract, "dataset", hold.DatasetID)
		recorded, err := retryEpochStorageStep(ctx, "record the storage recovery hold", func(ctx context.Context) (store.StorageRecoveryHold, error) {
			return t.records.HoldStorage(ctx, t.fence, *hold)
		})
		if err != nil {
			return err
		}
		t.manager.setHold(recorded)
	}
}

// fenceHeld advances, in the background, every dataset that serves the
// contract the record says it acknowledged, so a replaced controller cannot
// write to it while the installation is held. It runs only once the hold is
// recorded, and each advance supplies the dataset's acknowledged epoch as
// its floor: a dataset that lost that epoch refuses and keeps the evidence,
// while one that kept it, such as one that lost only watermark advances, is
// fenced. A contract now served by another dataset is left alone, because
// advancing that dataset would bind it to this installation before an
// operator decided to switch. Each advance is tried again until it
// succeeds, the dataset refuses it for good, or ctx ends with the hold, so a
// dataset that was unreachable when the hold was recorded is fenced once it
// answers.
func (t *storageTakeover) fenceHeld(ctx context.Context, record store.StorageRecord) {
	for contract, fence := range t.contracts {
		previous, acknowledged := record.Acknowledgement(contract)
		if !acknowledged {
			continue
		}
		_, floor, _, _ := highestAcknowledged(record, previous.DatasetID)
		go func() {
			_, err := retryEpochStorageStep(ctx, "fence the held "+contract+" dataset", func(ctx context.Context) (struct{}, error) {
				return struct{}{}, t.fenceHeldDataset(ctx, fence.dataset, previous.DatasetID, floor)
			})
			log := logf.FromContext(ctx).WithValues("contract", contract, "dataset", previous.DatasetID)
			switch {
			case err == nil || ctx.Err() != nil:
			case errors.Is(err, errEpochBelowAcknowledgement):
				// A dataset that lost its acknowledged epoch refuses the
				// advance and keeps that evidence until the hold is released.
				log.Info("A held storage dataset refused the advance and keeps its evidence", "reason", err.Error())
			case errors.Is(err, storagev1.ErrDatasetMismatch):
				log.Info("A held storage contract is served by another dataset now; leaving that dataset alone", "reason", err.Error())
			default:
				log.Error(err, "Fencing a held storage dataset failed")
			}
		}()
	}
}

// fenceHeldDataset advances, once, the dataset serving a held contract that
// dataset datasetID acknowledged, with floor as the acknowledged epoch. A
// refusal is final only when the dataset holds a lower epoch than floor; one
// that is briefly not ready is tried again.
func (t *storageTakeover) fenceHeldDataset(ctx context.Context, dataset EpochDataset, datasetID string, floor int64) error {
	info, err := dataset.Describe(ctx)
	if err != nil {
		return err
	}
	if info.DatasetID != datasetID {
		return fmt.Errorf("%w: dataset %s serves the contract, but dataset %s acknowledged it", storagev1.ErrDatasetMismatch, info.DatasetID, datasetID)
	}
	_, err = dataset.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
		DatasetID: datasetID, Epoch: t.acquired, InstallationID: t.installationID, AcknowledgedEpoch: floor,
	})
	if errors.Is(err, storagev1.ErrNotReady) {
		if stored, readErr := t.storedEpoch(ctx, datasetID, dataset); readErr == nil && stored < floor {
			return fmt.Errorf("%w: dataset %s holds controller epoch %d, below its acknowledged epoch %d",
				errEpochBelowAcknowledgement, datasetID, stored, floor)
		}
	}
	return err
}

// waitForAcknowledgement returns once the hold named id is acknowledged or
// no longer recorded.
func (t *storageTakeover) waitForAcknowledgement(ctx context.Context, id string) error {
	for {
		record, err := t.records.GetStorageRecord(ctx, t.acquired.Name)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				logf.FromContext(ctx).Error(err, "Reading the storage recovery hold failed; retrying")
			}
		case record.Hold == nil || record.Hold.ID != id || record.AcknowledgedHoldID == id:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(t.manager.holdPollInterval()):
		}
	}
}

// takeoverResult is how the takeover step of the contracts ended.
type takeoverResult struct {
	hold *store.StorageRecoveryHold
	err  error
}

// openAndMaintain runs the takeover step of every contract and, alongside
// it, the watermark loop over every dataset that has passed, so a dataset
// that opened is watched even while another is still unreachable. It returns
// the first hold either raises, an error retrying cannot fix, or nothing once
// ctx ends.
func (t *storageTakeover) openAndMaintain(ctx context.Context, record store.StorageRecord) (*store.StorageRecoveryHold, error) {
	t.mu.Lock()
	t.datasets = map[string]*trackedDataset{}
	t.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opened := make(chan takeoverResult, 1)
	go func() {
		hold, err := t.open(ctx, record)
		opened <- takeoverResult{hold: hold, err: err}
	}()
	maintained := make(chan *store.StorageRecoveryHold, 1)
	go func() { maintained <- t.maintain(ctx) }()
	for {
		select {
		case result := <-opened:
			if result.err == nil && result.hold == nil {
				opened = nil
				continue
			}
			cancel()
			<-maintained
			return result.hold, result.err
		case hold := <-maintained:
			cancel()
			if opened != nil {
				<-opened
			}
			return hold, nil
		}
	}
}

// open runs the takeover step of every contract and opens each contract's
// fence once its own dataset passed and its acknowledgement is recorded. It
// returns the first hold a contract raises.
func (t *storageTakeover) open(ctx context.Context, record store.StorageRecord) (*store.StorageRecoveryHold, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	advances := &datasetAdvances{advances: map[string]*datasetAdvance{}}
	outcomes := make(chan contractOutcome, len(t.contracts))
	for contract, fence := range t.contracts {
		go func() {
			outcome := t.check(ctx, contract, fence.dataset, record, advances)
			if outcome.err == nil && outcome.hold == nil {
				outcome.err = t.openContract(ctx, contract, outcome.ack)
			}
			outcomes <- outcome
		}()
	}
	for range t.contracts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case outcome := <-outcomes:
			if outcome.err != nil {
				return nil, outcome.err
			}
			if outcome.hold != nil {
				return outcome.hold, nil
			}
		}
	}
	return nil, nil
}

// openContract records a contract's acknowledgement and opens its fence,
// unless a switch check of its dataset is running, which opens it once it
// passes.
func (t *storageTakeover) openContract(ctx context.Context, contract string, ack store.StorageAcknowledgement) error {
	if _, err := retryEpochStorageStep(ctx, "record the "+contract+" acknowledgement", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, t.records.RecordStorageAcknowledgement(ctx, t.fence, ack)
	}); err != nil {
		return err
	}
	log := logf.FromContext(ctx).WithValues("contract", contract, "dataset", ack.DatasetID, "epoch", t.acquired.Epoch)
	if !t.acknowledged(contract, ack.DatasetID) {
		log.Info("Recorded the storage acknowledgement; the write fence opens once the restarted or switched dataset passes its check")
		return nil
	}
	log.Info("Opened the storage write fence")
	return nil
}

// acknowledged marks contract's acknowledgement on datasetID as recorded and
// opens its fence, unless the watermark loop is checking a restart or switch
// of the dataset, whose check then opens it. It reports whether the fence
// opened.
func (t *storageTakeover) acknowledged(contract, datasetID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tracked := t.datasets[datasetID]; tracked != nil {
		if tracked.opened == nil {
			tracked.opened = map[string]bool{}
		}
		tracked.opened[contract] = true
		if tracked.checking {
			return false
		}
	}
	t.manager.openContractFence(contract, datasetID, t.acquired)
	return true
}

// release records what each dataset holds now as acknowledged and clears the
// acknowledged hold in one update. It advances nothing: a dataset that lost
// its acknowledged epoch keeps that evidence until the new acknowledgement is
// recorded, because every advance supplies the recorded acknowledgement as
// its floor. The next takeover step then advances each dataset against the
// new record.
func (t *storageTakeover) release(ctx context.Context, id string) error {
	acks := make([]store.StorageAcknowledgement, 0, len(t.contracts))
	for contract, fence := range t.contracts {
		ack, ok, err := t.accept(ctx, contract, fence.dataset)
		if err != nil {
			return err
		}
		if ok {
			acks = append(acks, ack)
		}
	}
	slices.SortFunc(acks, func(a, b store.StorageAcknowledgement) int { return compareStrings(a.Contract, b.Contract) })
	if _, err := retryEpochStorageStep(ctx, "release the storage recovery hold", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, t.records.ReleaseStorageHold(ctx, t.fence, id, acks)
	}); err != nil {
		return err
	}
	// Each contract's fence opens again only once its dataset passes the
	// takeover step against the new record, which may name another dataset.
	t.manager.closeFences()
	t.manager.clearHold()
	return nil
}

// accept returns the acknowledgement of what the dataset serving contract
// holds now, as an operator accepted it by acknowledging the hold. A dataset
// that holds no controller epoch has nothing to acknowledge.
func (t *storageTakeover) accept(ctx context.Context, contract string, dataset EpochDataset) (store.StorageAcknowledgement, bool, error) {
	info, err := t.describe(ctx, contract, dataset)
	if err != nil {
		return store.StorageAcknowledgement{}, false, err
	}
	epoch, watermark, err := t.held(ctx, info.DatasetID, dataset)
	if err != nil || epoch == 0 {
		return store.StorageAcknowledgement{}, false, err
	}
	now := t.manager.clock()
	ack := store.StorageAcknowledgement{Contract: contract, DatasetID: info.DatasetID, Epoch: epoch, AcknowledgedAt: now}
	if watermark.Value > 0 {
		ack.Watermark, ack.WatermarkAcknowledgedAt = watermark.Value, now
	}
	return ack, true, nil
}

// describe describes the dataset serving contract. An incompatible dataset
// fails it, as it fails the takeover.
func (t *storageTakeover) describe(ctx context.Context, contract string, dataset EpochDataset) (storagev1.ProviderInfo, error) {
	return retryEpochStorageStep(ctx, "describe the "+contract+" dataset",
		func(ctx context.Context) (storagev1.ProviderInfo, error) {
			info, err := dataset.Describe(ctx)
			if err != nil {
				return info, err
			}
			if err := store.ValidateControlIdentifier("dataset ID", info.DatasetID); err != nil {
				return info, err
			}
			if info.State == storagev1.DatasetIncompatible {
				return info, storagev1.ErrIncompatible
			}
			return info, nil
		})
}

// held reads the controller epoch, zero when it holds none, and the write
// watermark a dataset holds.
func (t *storageTakeover) held(ctx context.Context, datasetID string, dataset EpochDataset) (int64, storagev1.WriteWatermark, error) {
	epoch, err := retryEpochStorageStep(ctx, "read the controller epoch of dataset "+datasetID,
		func(ctx context.Context) (int64, error) {
			return t.storedEpoch(ctx, datasetID, dataset)
		})
	if err != nil {
		return 0, storagev1.WriteWatermark{}, err
	}
	watermark, err := retryEpochStorageStep(ctx, "read the write watermark of dataset "+datasetID,
		func(ctx context.Context) (storagev1.WriteWatermark, error) {
			watermark, err := dataset.GetWriteWatermark(ctx)
			if err == nil && watermark.DatasetID != datasetID {
				err = fmt.Errorf("%w: dataset %s reports its write watermark as dataset %s", storagev1.ErrDatasetMismatch, datasetID, watermark.DatasetID)
			}
			return watermark, err
		})
	return epoch, watermark, err
}

// storedEpoch reads the controller epoch a dataset holds, zero when it holds
// none.
func (t *storageTakeover) storedEpoch(ctx context.Context, datasetID string, dataset EpochDataset) (int64, error) {
	state, err := dataset.GetControllerEpochMirror(ctx, t.acquired.Name)
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if state.DatasetID != "" && state.DatasetID != datasetID {
		return 0, fmt.Errorf("%w: dataset %s reports its epoch as dataset %s", storagev1.ErrDatasetMismatch, datasetID, state.DatasetID)
	}
	return state.Epoch.Epoch, nil
}

// check describes one contract's dataset, compares it with what the record
// says it acknowledged and, when it passed, advances it.
func (t *storageTakeover) check(
	ctx context.Context, contract string, dataset EpochDataset, record store.StorageRecord, advances *datasetAdvances,
) contractOutcome {
	outcome := contractOutcome{contract: contract}
	info, err := t.describe(ctx, contract, dataset)
	if err != nil {
		outcome.err = err
		return outcome
	}
	previous, acknowledged := record.Acknowledgement(contract)
	if acknowledged && previous.DatasetID != info.DatasetID {
		outcome.hold = &store.StorageRecoveryHold{
			Reason: store.StorageRecoveryDatasetChanged, Contract: contract,
			DatasetID: info.DatasetID, AcknowledgedDatasetID: previous.DatasetID,
			Message: fmt.Sprintf("%s is served by dataset %s, but dataset %s acknowledged it; moving a contract to another dataset is a storage switch",
				contract, info.DatasetID, previous.DatasetID),
		}
		return outcome
	}
	advanced, err := advances.do(ctx, info.DatasetID, func(ctx context.Context) (datasetAdvanceResult, error) {
		return t.advance(ctx, info, dataset, record)
	})
	if err != nil {
		outcome.err = err
		return outcome
	}
	if advanced.hold != nil {
		hold := *advanced.hold
		outcome.hold = &hold
		return outcome
	}
	outcome.ack = store.StorageAcknowledgement{
		Contract: contract, DatasetID: info.DatasetID, Epoch: t.acquired.Epoch, AcknowledgedAt: t.manager.clock(),
	}
	if acknowledged {
		outcome.ack.Watermark, outcome.ack.WatermarkAcknowledgedAt = previous.Watermark, previous.WatermarkAcknowledgedAt
	}
	// The loop watches the dataset from the incarnation the advance
	// described, which every contract the dataset serves shares, so a switch
	// after it is checked even if this contract described the dataset later.
	described := info
	described.Incarnation = advanced.incarnation
	t.track(contract, described, dataset, outcome.ack.Watermark)
	return outcome
}

// errEpochBelowAcknowledgement reports that a dataset refused the advance
// because it holds a lower controller epoch than the floor the advance
// supplied. Retrying cannot fix it.
var errEpochBelowAcknowledgement = errors.New("dataset holds a lower controller epoch than it acknowledged")

// advance reads the epoch and write watermark a dataset holds and compares
// them with the highest the record says any contract acknowledged on it. A
// dataset that holds less is not advanced, so its evidence stays as found
// even if the hold cannot be recorded; otherwise it is advanced to the
// acquired epoch, with the acknowledged epoch as the advance's floor, so the
// provider itself refuses to move a dataset that lost it in the meantime.
// Every contract the dataset serves shares the result.
func (t *storageTakeover) advance(
	ctx context.Context, info storagev1.ProviderInfo, dataset EpochDataset, record store.StorageRecord,
) (datasetAdvanceResult, error) {
	datasetID := info.DatasetID
	result := datasetAdvanceResult{incarnation: info.Incarnation}
	before, watermark, err := t.held(ctx, datasetID, dataset)
	if err != nil {
		return result, err
	}
	result.storedBefore, result.watermark = before, watermark
	epochContract, epoch, watermarkContract, acknowledgedWatermark := highestAcknowledged(record, datasetID)
	epochHold := func(stored int64) *store.StorageRecoveryHold {
		return &store.StorageRecoveryHold{
			Reason: store.StorageRecoveryEpochRegressed, Contract: epochContract, DatasetID: datasetID,
			AcknowledgedEpoch: epoch, StoredEpoch: stored, Durability: string(info.Durability),
			Message: fmt.Sprintf("dataset %s held controller epoch %d, but it acknowledged epoch %d: it lost acknowledged writes or was restored from an older copy",
				datasetID, stored, epoch),
		}
	}
	switch {
	case before < epoch:
		result.hold = epochHold(before)
		return result, nil
	case result.watermark.Value < acknowledgedWatermark:
		result.hold = watermarkHold(watermarkContract, info, acknowledgedWatermark, result.watermark.Value)
		return result, nil
	}
	refusedAt := int64(-1)
	state, err := retryEpochStorageStep(ctx, "advance the controller epoch of dataset "+datasetID,
		func(ctx context.Context) (storagev1.ControllerEpochMirrorState, error) {
			state, err := dataset.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
				DatasetID: datasetID, Epoch: t.acquired, InstallationID: t.installationID, AcknowledgedEpoch: epoch,
			})
			if errors.Is(err, storagev1.ErrNotReady) && epoch > 0 {
				// The dataset may have lost the epoch since it was read, or
				// be briefly not ready; only a lower stored epoch is final.
				if stored, readErr := t.storedEpoch(ctx, datasetID, dataset); readErr == nil && stored < epoch {
					refusedAt = stored
					return state, errEpochBelowAcknowledgement
				}
			}
			return state, err
		})
	if refusedAt >= 0 {
		result.hold = epochHold(refusedAt)
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if state.Epoch.Epoch != t.acquired.Epoch || state.Epoch.HolderID != t.acquired.HolderID {
		return result, fmt.Errorf("advance the controller epoch of dataset %s: the dataset holds epoch %d of %q, not %d of %q",
			datasetID, state.Epoch.Epoch, state.Epoch.HolderID, t.acquired.Epoch, t.acquired.HolderID)
	}
	return result, nil
}

// highestAcknowledged returns the highest controller epoch and write
// watermark the record says any contract acknowledged on datasetID, with the
// contract that acknowledged each.
func highestAcknowledged(record store.StorageRecord, datasetID string) (epochContract string, epoch int64, watermarkContract string, watermark int64) {
	for _, ack := range record.Acknowledgements {
		if ack.DatasetID != datasetID {
			continue
		}
		if ack.Epoch > epoch {
			epochContract, epoch = ack.Contract, ack.Epoch
		}
		if ack.Watermark > watermark {
			watermarkContract, watermark = ack.Contract, ack.Watermark
		}
	}
	return epochContract, epoch, watermarkContract, watermark
}

// track records a dataset the takeover advanced, for the watermark loop, and
// a contract it serves whose acknowledgement is still to be recorded.
func (t *storageTakeover) track(contract string, info storagev1.ProviderInfo, dataset EpochDataset, acknowledged int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.datasets == nil {
		t.datasets = map[string]*trackedDataset{}
	}
	tracked := t.datasets[info.DatasetID]
	if tracked == nil {
		tracked = &trackedDataset{dataset: dataset, incarnation: info.Incarnation}
		t.datasets[info.DatasetID] = tracked
	}
	tracked.contracts = append(tracked.contracts, contract)
	slices.Sort(tracked.contracts)
	tracked.acknowledged = max(tracked.acknowledged, acknowledged)
	// The state contract's dataset writes the watermark, so it waits for the
	// fence CurrentFence also waits for.
	if contract == storagev1.StateContract {
		tracked.dataset = dataset
	}
}

// maintain runs the watermark loop until ctx ends, and returns the first hold
// it raises.
func (t *storageTakeover) maintain(ctx context.Context) *store.StorageRecoveryHold {
	for {
		if hold := t.tick(ctx); hold != nil {
			return hold
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(t.manager.watermarkInterval()):
		}
	}
}

// tick checks and advances every tracked dataset once.
func (t *storageTakeover) tick(ctx context.Context) *store.StorageRecoveryHold {
	t.mu.Lock()
	ids := make([]string, 0, len(t.datasets))
	for id := range t.datasets {
		ids = append(ids, id)
	}
	t.mu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil
		}
		if hold := t.tickDataset(ctx, id); hold != nil {
			return hold
		}
	}
	return nil
}

// snapshot returns a copy of the tracked dataset datasetID.
func (t *storageTakeover) snapshot(datasetID string) (trackedDataset, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked, ok := t.datasets[datasetID]
	if !ok {
		return trackedDataset{}, false
	}
	copy := *tracked
	copy.contracts = slices.Clone(tracked.contracts)
	copy.opened = nil
	return copy, true
}

// startCheck marks datasetID as being checked after a restart or switch and
// returns every contract it serves, whose fences the caller closes. A
// contract acknowledged from now on waits for the check.
func (t *storageTakeover) startCheck(datasetID string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked, ok := t.datasets[datasetID]
	if !ok {
		return nil
	}
	tracked.checking = true
	return slices.Clone(tracked.contracts)
}

// passCheck records that datasetID passed its check at incarnation and
// reopens the fences of the contracts whose acknowledgement is recorded.
func (t *storageTakeover) passCheck(datasetID, incarnation string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tracked, ok := t.datasets[datasetID]
	if !ok {
		return
	}
	tracked.incarnation, tracked.checking = incarnation, false
	for _, contract := range tracked.contracts {
		if tracked.opened[contract] {
			t.manager.openContractFence(contract, datasetID, t.acquired)
		}
	}
}

// update changes the tracked dataset datasetID under the lock.
func (t *storageTakeover) update(datasetID string, change func(*trackedDataset)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if tracked, ok := t.datasets[datasetID]; ok {
		change(tracked)
	}
}

func (t *storageTakeover) tickDataset(ctx context.Context, datasetID string) *store.StorageRecoveryHold {
	tracked, ok := t.snapshot(datasetID)
	if !ok || len(tracked.contracts) == 0 {
		return nil
	}
	log := logf.FromContext(ctx).WithValues("dataset", datasetID)
	contract := tracked.contracts[0]
	callCtx, cancel := context.WithTimeout(ctx, epochStorageCallTimeout)
	defer cancel()
	info, err := tracked.dataset.Describe(callCtx)
	if err != nil {
		log.Error(err, "Describing a storage dataset failed; checking again later")
		return nil
	}
	if info.DatasetID != datasetID {
		return &store.StorageRecoveryHold{
			Reason: store.StorageRecoveryDatasetChanged, Contract: contract,
			DatasetID: info.DatasetID, AcknowledgedDatasetID: datasetID,
			Message: fmt.Sprintf("%s is now served by dataset %s instead of dataset %s", contract, info.DatasetID, datasetID),
		}
	}
	// A check that has not finished runs again even when the dataset reports
	// the incarnation checked last, as after a switchover that was seen and
	// then rolled back.
	if info.Incarnation != tracked.incarnation || tracked.checking {
		// Nothing is written to a dataset whose database restarted or
		// switched until it has been checked again.
		t.manager.closeContractFences(t.startCheck(datasetID))
		if info.State != storagev1.DatasetReady {
			log.Info("A storage dataset restarted or switched and is not ready yet; checking again later", "state", info.State)
			return nil
		}
		hold, checked := t.checkSwitch(callCtx, contract, info, tracked)
		if hold != nil {
			return hold
		}
		if !checked || ctx.Err() != nil {
			return nil
		}
		log.Info("A storage dataset restarted or switched and still holds every write it acknowledged",
			"incarnation", info.Incarnation, "durability", info.Durability)
		t.passCheck(datasetID, info.Incarnation)
	}
	watermark, err := tracked.dataset.AdvanceWriteWatermark(callCtx)
	if err != nil {
		log.Error(err, "Advancing a storage dataset's write watermark failed; trying again later")
		return nil
	}
	if watermark.DatasetID != datasetID || watermark.Value <= tracked.acknowledged {
		return watermarkHold(contract, info, tracked.acknowledged+1, watermark.Value)
	}
	if err := t.records.RecordStorageWatermark(callCtx, t.fence, datasetID, watermark.Value, t.manager.clock()); err != nil {
		log.Error(err, "Recording a storage dataset's write watermark failed; trying again later")
		return nil
	}
	t.update(datasetID, func(tracked *trackedDataset) { tracked.acknowledged = max(tracked.acknowledged, watermark.Value) })
	return nil
}

// checkSwitch checks a dataset whose database restarted or switched. It
// reports whether the check finished: a read that failed leaves it
// unfinished, so the caller neither trusts nor writes to the dataset until a
// later check finishes. Any switch of an async-replicated dataset holds,
// whatever the dataset holds.
func (t *storageTakeover) checkSwitch(
	ctx context.Context, contract string, info storagev1.ProviderInfo, tracked trackedDataset,
) (*store.StorageRecoveryHold, bool) {
	if info.Durability == storagev1.DurabilityAsyncReplicated {
		return &store.StorageRecoveryHold{
			Reason: store.StorageRecoveryUnsafeSwitch, Contract: contract, DatasetID: info.DatasetID,
			Durability: string(info.Durability),
			Message: fmt.Sprintf("the database of dataset %s restarted or switched, and its durability class %s can lose acknowledged writes in a failover",
				info.DatasetID, info.Durability),
		}, true
	}
	log := logf.FromContext(ctx).WithValues("dataset", info.DatasetID)
	state, err := tracked.dataset.GetControllerEpochMirror(ctx, t.acquired.Name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		state = storagev1.ControllerEpochMirrorState{}
	case err != nil:
		log.Error(err, "Reading the controller epoch of a switched storage dataset failed; checking again later")
		return nil, false
	}
	if state.Epoch.Epoch < t.acquired.Epoch {
		return &store.StorageRecoveryHold{
			Reason: store.StorageRecoveryEpochRegressed, Contract: contract, DatasetID: info.DatasetID,
			AcknowledgedEpoch: t.acquired.Epoch, StoredEpoch: state.Epoch.Epoch, Durability: string(info.Durability),
			Message: fmt.Sprintf("after its database restarted or switched, dataset %s holds controller epoch %d, but it acknowledged epoch %d",
				info.DatasetID, state.Epoch.Epoch, t.acquired.Epoch),
		}, true
	}
	watermark, err := tracked.dataset.GetWriteWatermark(ctx)
	if err != nil {
		log.Error(err, "Reading the write watermark of a switched storage dataset failed; checking again later")
		return nil, false
	}
	if watermark.Value < tracked.acknowledged {
		return watermarkHold(contract, info, tracked.acknowledged, watermark.Value), true
	}
	return nil, true
}

func watermarkHold(contract string, info storagev1.ProviderInfo, acknowledged, stored int64) *store.StorageRecoveryHold {
	return &store.StorageRecoveryHold{
		Reason: store.StorageRecoveryWatermarkRegressed, Contract: contract, DatasetID: info.DatasetID,
		AcknowledgedWatermark: acknowledged, StoredWatermark: stored, Durability: string(info.Durability),
		Message: fmt.Sprintf("dataset %s holds write watermark %d, but it acknowledged %d: it lost acknowledged writes or was restored from an older copy",
			info.DatasetID, stored, acknowledged),
	}
}

// setHold closes every fence for hold at once, then returns once work
// admitted before it has finished.
func (m *ControllerEpochManager) setHold(hold store.StorageRecoveryHold) {
	m.storageMu.Lock()
	if m.hold == nil || m.hold.ID != hold.ID {
		m.hold = &hold
		m.notifyStorageLocked()
	}
	m.storageMu.Unlock()
	metrics.StorageRecoveryHold.Reset()
	metrics.StorageRecoveryHold.WithLabelValues(string(hold.Reason)).Set(1)
	m.drainAdmittedWork()
}

// taskSuccess says whether each surface on which a Task reports success
// does: its phase, the workload execution outcome it recorded, and the state
// or outcome of its ACP execution. Each is written on its own: the outcome
// before the phase, an ACP execution before its delivery settles, and an ACP
// Task whose delivery failed or was cancelled keeps its successful execution
// under a failed or cancelled phase.
type taskSuccess struct{ phase, outcome, execution bool }

func taskSuccessOf(task *corev1alpha1.Task) taskSuccess {
	execution := task.Status.Execution
	return taskSuccess{
		phase:   task.Status.Phase == corev1alpha1.TaskPhaseSucceeded,
		outcome: task.Status.ExecutionOutcome != nil && task.Status.ExecutionOutcome.Phase == corev1alpha1.TaskPhaseSucceeded,
		execution: execution != nil && (execution.State == corev1alpha1.TaskExecutionStateSucceeded ||
			execution.Outcome == corev1alpha1.TaskExecutionOutcomeSucceeded),
	}
}

// reportsNew says whether s reports success on a surface that reported does
// not.
func (s taskSuccess) reportsNew(reported taskSuccess) bool {
	return (s.phase && !reported.phase) || (s.outcome && !reported.outcome) || (s.execution && !reported.execution)
}

// admitTaskStatus runs write, which publishes task's status, through
// epochs.AdmitWork when that status reports success on any surface that
// reported did not. No Task then reports success while storage is held for
// recovery, also when the hold is raised after the storage writes that came
// before the status write. Other status writes run as they are.
func admitTaskStatus(
	ctx context.Context, epochs *ControllerEpochManager, reported taskSuccess,
	task *corev1alpha1.Task, write func(context.Context) error,
) error {
	if taskSuccessOf(task).reportsNew(reported) {
		return epochs.AdmitWork(ctx, write)
	}
	return write(ctx)
}

// closeFences closes every contract's write fence until it opens again, then
// returns once work admitted before it has finished.
func (m *ControllerEpochManager) closeFences() {
	m.storageMu.Lock()
	for _, fence := range m.storage {
		fence.opened = false
	}
	m.notifyStorageLocked()
	m.storageMu.Unlock()
	m.drainAdmittedWork()
}

// closeContractFences closes the write fences of contracts until they open
// again, then returns once work admitted before it has finished.
func (m *ControllerEpochManager) closeContractFences(contracts []string) {
	m.storageMu.Lock()
	for _, contract := range contracts {
		if fence := m.storage[contract]; fence != nil {
			fence.opened = false
		}
	}
	m.notifyStorageLocked()
	m.storageMu.Unlock()
	m.drainAdmittedWork()
}

// drainAdmittedWork waits for work AdmitWork admitted before the caller
// published a hold or closed a fence. Work admitted later sees the change
// and is refused, because AdmitWork checks StorageHold under the read lock.
func (m *ControllerEpochManager) drainAdmittedWork() {
	m.admission.Lock()
	m.admission.Unlock() //nolint:staticcheck // An empty critical section waits for every reader.
}

// clearHold lets every opened fence through again.
func (m *ControllerEpochManager) clearHold() {
	m.storageMu.Lock()
	held := m.hold != nil
	m.hold = nil
	if held {
		m.notifyStorageLocked()
	}
	m.storageMu.Unlock()
	if held {
		metrics.StorageRecoveryHold.Reset()
	}
}

func (m *ControllerEpochManager) watermarkInterval() time.Duration {
	if m.WatermarkInterval > 0 {
		return m.WatermarkInterval
	}
	return DefaultWatermarkInterval
}

func (m *ControllerEpochManager) holdPollInterval() time.Duration {
	if m.HoldPollInterval > 0 {
		return m.HoldPollInterval
	}
	return DefaultHoldPollInterval
}

func (m *ControllerEpochManager) holdID() string {
	if m.newHoldID != nil {
		return m.newHoldID()
	}
	return uuid.NewString()
}

func (m *ControllerEpochManager) clock() time.Time {
	if m.now != nil {
		return m.now().UTC()
	}
	return time.Now().UTC()
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
