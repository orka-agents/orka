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
	dataset     EpochDataset
	contracts   []string
	incarnation string
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
				t.fenceHeld(ctx, record)
				if err := t.waitForAcknowledgement(ctx, hold.ID); err != nil {
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
// recorded, so the evidence a dataset held, such as a lower epoch, is kept
// in the hold first. A contract now served by another dataset is left
// alone, because advancing that dataset would bind it to this installation
// before an operator decided to switch.
func (t *storageTakeover) fenceHeld(ctx context.Context, record store.StorageRecord) {
	for contract, fence := range t.contracts {
		previous, acknowledged := record.Acknowledgement(contract)
		if !acknowledged {
			continue
		}
		go func() {
			callCtx, cancel := context.WithTimeout(ctx, epochStorageCallTimeout)
			defer cancel()
			info, err := fence.dataset.Describe(callCtx)
			if err != nil || info.DatasetID != previous.DatasetID {
				return
			}
			if _, err := fence.dataset.AdvanceControllerEpoch(callCtx, storagev1.AdvanceControllerEpochRequest{
				DatasetID: info.DatasetID, Epoch: t.acquired, InstallationID: t.installationID,
			}); err != nil && ctx.Err() == nil {
				logf.FromContext(ctx).Error(err, "Fencing a held storage dataset failed", "contract", contract, "dataset", info.DatasetID)
			}
		}()
	}
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
// fence once its own dataset passed. It returns the first hold a contract
// raises.
func (t *storageTakeover) open(ctx context.Context, record store.StorageRecord) (*store.StorageRecoveryHold, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	advances := &datasetAdvances{advances: map[string]*datasetAdvance{}}
	outcomes := make(chan contractOutcome, len(t.contracts))
	for contract, fence := range t.contracts {
		go func() {
			outcome := t.check(ctx, contract, fence.dataset, record, advances, false)
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

// openContract records a contract's acknowledgement and opens its fence.
func (t *storageTakeover) openContract(ctx context.Context, contract string, ack store.StorageAcknowledgement) error {
	if _, err := retryEpochStorageStep(ctx, "record the "+contract+" acknowledgement", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, t.records.RecordStorageAcknowledgement(ctx, t.fence, ack)
	}); err != nil {
		return err
	}
	t.manager.openContractFence(contract, ack.DatasetID, t.acquired)
	logf.FromContext(ctx).Info("Opened the storage write fence", "contract", contract, "dataset", ack.DatasetID, "epoch", t.acquired.Epoch)
	return nil
}

// release advances every dataset, records what each holds now as
// acknowledged and clears the acknowledged hold in one update.
func (t *storageTakeover) release(ctx context.Context, id string) error {
	advances := &datasetAdvances{advances: map[string]*datasetAdvance{}}
	acks := make([]store.StorageAcknowledgement, 0, len(t.contracts))
	for contract, fence := range t.contracts {
		outcome := t.check(ctx, contract, fence.dataset, store.StorageRecord{}, advances, true)
		if outcome.err != nil {
			return outcome.err
		}
		acks = append(acks, outcome.ack)
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

// check describes one contract's dataset, compares it with what the record
// says it acknowledged and, when it passed, advances it. With accept set, as
// when an operator acknowledged a hold, it compares nothing and acknowledges
// what the dataset holds now.
func (t *storageTakeover) check(
	ctx context.Context, contract string, dataset EpochDataset, record store.StorageRecord,
	advances *datasetAdvances, accept bool,
) contractOutcome {
	outcome := contractOutcome{contract: contract}
	info, err := retryEpochStorageStep(ctx, "describe the "+contract+" dataset",
		func(ctx context.Context) (storagev1.ProviderInfo, error) {
			info, err := dataset.Describe(ctx)
			if err != nil {
				return info, err
			}
			return info, store.ValidateControlIdentifier("dataset ID", info.DatasetID)
		})
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
	if accept {
		record = store.StorageRecord{}
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
	now := t.manager.clock()
	outcome.ack = store.StorageAcknowledgement{
		Contract: contract, DatasetID: info.DatasetID, Epoch: t.acquired.Epoch, AcknowledgedAt: now,
	}
	switch {
	case accept:
		if advanced.watermark.Value > 0 {
			outcome.ack.Watermark, outcome.ack.WatermarkAcknowledgedAt = advanced.watermark.Value, now
		}
	case acknowledged:
		outcome.ack.Watermark, outcome.ack.WatermarkAcknowledgedAt = previous.Watermark, previous.WatermarkAcknowledgedAt
	}
	t.track(contract, info, dataset, outcome.ack.Watermark)
	return outcome
}

// advance reads the epoch and write watermark a dataset holds and compares
// them with the highest the record says any contract acknowledged on it. A
// dataset that holds less is not advanced, so its evidence stays as found
// even if the hold cannot be recorded; otherwise it is advanced to the
// acquired epoch. Every contract the dataset serves shares the result.
func (t *storageTakeover) advance(
	ctx context.Context, info storagev1.ProviderInfo, dataset EpochDataset, record store.StorageRecord,
) (datasetAdvanceResult, error) {
	datasetID := info.DatasetID
	var result datasetAdvanceResult
	before, err := retryEpochStorageStep(ctx, "read the controller epoch of dataset "+datasetID,
		func(ctx context.Context) (int64, error) {
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
		})
	if err != nil {
		return result, err
	}
	result.storedBefore = before
	result.watermark, err = retryEpochStorageStep(ctx, "read the write watermark of dataset "+datasetID,
		func(ctx context.Context) (storagev1.WriteWatermark, error) {
			watermark, err := dataset.GetWriteWatermark(ctx)
			if err == nil && watermark.DatasetID != datasetID {
				err = fmt.Errorf("%w: dataset %s reports its write watermark as dataset %s", storagev1.ErrDatasetMismatch, datasetID, watermark.DatasetID)
			}
			return watermark, err
		})
	if err != nil {
		return result, err
	}
	epochContract, epoch, watermarkContract, watermark := highestAcknowledged(record, datasetID)
	switch {
	case before < epoch:
		result.hold = &store.StorageRecoveryHold{
			Reason: store.StorageRecoveryEpochRegressed, Contract: epochContract, DatasetID: datasetID,
			AcknowledgedEpoch: epoch, StoredEpoch: before, Durability: string(info.Durability),
			Message: fmt.Sprintf("dataset %s held controller epoch %d, but it acknowledged epoch %d: it lost acknowledged writes or was restored from an older copy",
				datasetID, before, epoch),
		}
		return result, nil
	case result.watermark.Value < watermark:
		result.hold = watermarkHold(watermarkContract, info, watermark, result.watermark.Value)
		return result, nil
	}
	state, err := retryEpochStorageStep(ctx, "advance the controller epoch of dataset "+datasetID,
		func(ctx context.Context) (storagev1.ControllerEpochMirrorState, error) {
			return dataset.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
				DatasetID: datasetID, Epoch: t.acquired, InstallationID: t.installationID,
			})
		})
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

// track records a dataset the takeover advanced, for the watermark loop.
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
	return copy, true
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
	if info.Incarnation != tracked.incarnation {
		// Nothing is written to a dataset whose database restarted or
		// switched until it has been checked again.
		t.manager.closeContractFences(tracked.contracts)
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
		t.update(datasetID, func(tracked *trackedDataset) { tracked.incarnation = info.Incarnation })
		for _, contract := range tracked.contracts {
			t.manager.openContractFence(contract, datasetID, t.acquired)
		}
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
