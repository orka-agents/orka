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
	DefaultWatermarkInterval = time.Minute
	DefaultHoldPollInterval  = 10 * time.Second
	storageHoldRequeue       = 30 * time.Second
)

type storageTakeover struct {
	manager        *ControllerEpochManager
	acquired       store.ControllerEpoch
	fence          store.ControllerEpochFence
	installationID string
	records        store.StorageRecordStore
	contracts      map[string]*contractFence

	mu       sync.Mutex
	attempt  uint64
	ended    bool
	datasets map[string]*trackedDataset
}

type trackedDataset struct {
	dataset      EpochDataset
	contracts    []string
	opened       map[string]bool
	incarnation  string
	checking     bool
	acknowledged int64
}

type contractOutcome struct {
	contract string
	ack      store.StorageAcknowledgement
	hold     *store.StorageRecoveryHold
	err      error
}

func (t *storageTakeover) run(ctx context.Context) error {
	log := logf.FromContext(ctx)
	var held *store.StorageRecoveryHold
	fencingHold, stopFencing := "", context.CancelFunc(func() {})
	endFencing := func() {
		stopFencing()
		fencingHold, stopFencing = "", func() {}
	}
	defer endFencing()
	fence := func(id string, record store.StorageRecord) {
		if fencingHold == id {
			return
		}
		endFencing()
		fencing, cancel := context.WithCancel(ctx)
		fencingHold, stopFencing = id, cancel
		t.fenceHeld(fencing, record)
	}
	for {
		record, err := retryEpochStorageStep(ctx, "read the storage record", func(ctx context.Context) (store.StorageRecord, error) {
			return t.records.GetStorageRecord(ctx, t.acquired.Name)
		})
		if err != nil {
			return err
		}
		if record.Hold == nil && held != nil && record.AcknowledgedHoldID != held.ID {
			log.Info("The storage recovery hold left the record without being acknowledged; recording it again", "hold", held.ID)
			hold := *held
			fence(hold.ID, record)
			if _, err := retryEpochStorageStep(ctx, "record the storage recovery hold again", func(ctx context.Context) (store.StorageRecoveryHold, error) {
				return t.records.HoldStorage(ctx, t.fence, hold)
			}); err != nil {
				return err
			}
			continue
		}
		if hold := record.Hold; hold != nil {
			recordedHold := *hold
			held = &recordedHold
			t.manager.setHold(*hold)
			if record.AcknowledgedHoldID != hold.ID {
				log.Info("Storage is held for recovery; storage contract writes and control-store changes are refused until an operator acknowledges the hold",
					"hold", hold.ID, "reason", hold.Reason, "message", hold.Message)
				fence(hold.ID, record)
				if err := t.waitForAcknowledgement(ctx, hold.ID); err != nil {
					return err
				}
				continue
			}
			endFencing()
			if err := t.release(ctx, hold.ID); err != nil {
				return err
			}
			held = nil
			log.Info("Released the acknowledged storage recovery hold and recorded what each dataset holds now", "hold", hold.ID)
			continue
		}
		held = nil
		endFencing()
		t.manager.closeFences()
		t.manager.clearHold()
		hold, err := t.openAndMaintain(ctx, record)
		if err != nil || hold == nil {
			return err
		}
		hold.ID = t.manager.holdID()
		hold.RaisedAt = t.manager.clock()
		t.manager.setHold(*hold)
		log.Error(errors.New(hold.Message), "Storage lost acknowledged writes or switched unsafely; holding storage contract writes and control-store changes for recovery",
			"hold", hold.ID, "reason", hold.Reason, "contract", hold.Contract, "dataset", hold.DatasetID)
		recorded, err := retryEpochStorageStep(ctx, "record the storage recovery hold", func(ctx context.Context) (store.StorageRecoveryHold, error) {
			return t.records.HoldStorage(ctx, t.fence, *hold)
		})
		if err != nil {
			return err
		}
		held = &recorded
		t.manager.setHold(recorded)
	}
}

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
				log.Info("A held storage dataset refused the advance and keeps its evidence", "reason", err.Error())
			case errors.Is(err, storagev1.ErrDatasetMismatch):
				log.Info("A held storage contract is served by another dataset now; leaving that dataset alone", "reason", err.Error())
			default:
				log.Error(err, "Fencing a held storage dataset failed")
			}
		}()
	}
}

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

type takeoverResult struct {
	hold *store.StorageRecoveryHold
	err  error
}

func (t *storageTakeover) openAndMaintain(ctx context.Context, record store.StorageRecord) (*store.StorageRecoveryHold, error) {
	attempt := t.startAttempt()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	opened := make(chan takeoverResult, 1)
	go func() {
		hold, err := t.open(ctx, attempt, record)
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
			t.endAttempt()
			cancel()
			if result.hold != nil {
				return result.hold, nil
			}
			<-maintained
			return nil, result.err
		case hold := <-maintained:
			t.endAttempt()
			cancel()
			if hold != nil {
				return hold, nil
			}
			if opened != nil {
				<-opened
			}
			return nil, nil
		}
	}
}

func (t *storageTakeover) startAttempt() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.attempt++
	t.ended = false
	t.datasets = map[string]*trackedDataset{}
	return t.attempt
}

func (t *storageTakeover) endAttempt() {
	t.mu.Lock()
	t.ended = true
	t.mu.Unlock()
}

func (t *storageTakeover) inProgress(attempt uint64) bool {
	return attempt == t.attempt && !t.ended
}

func (t *storageTakeover) open(ctx context.Context, attempt uint64, record store.StorageRecord) (*store.StorageRecoveryHold, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	advances := &datasetAdvances{advances: map[string]*datasetAdvance{}}
	outcomes := make(chan contractOutcome, len(t.contracts))
	for contract, fence := range t.contracts {
		go func() {
			outcome := t.check(ctx, attempt, contract, fence.dataset, record, advances)
			if outcome.err == nil && outcome.hold == nil {
				outcome.err = t.openContract(ctx, attempt, contract, outcome.ack)
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

func (t *storageTakeover) openContract(ctx context.Context, attempt uint64, contract string, ack store.StorageAcknowledgement) error {
	if _, err := retryEpochStorageStep(ctx, "record the "+contract+" acknowledgement", func(ctx context.Context) (struct{}, error) {
		return struct{}{}, t.records.RecordStorageAcknowledgement(ctx, t.fence, ack)
	}); err != nil {
		return err
	}
	log := logf.FromContext(ctx).WithValues("contract", contract, "dataset", ack.DatasetID, "epoch", t.acquired.Epoch)
	opened, current := t.acknowledged(attempt, contract, ack.DatasetID)
	switch {
	case !current:
	case !opened:
		log.Info("Recorded the storage acknowledgement; the write fence opens once the restarted or switched dataset passes its check")
	default:
		log.Info("Opened the storage write fence")
	}
	return nil
}

func (t *storageTakeover) acknowledged(attempt uint64, contract, datasetID string) (opened, current bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.inProgress(attempt) {
		return false, false
	}
	tracked := t.datasets[datasetID]
	if tracked == nil {
		return false, true
	}
	if tracked.opened == nil {
		tracked.opened = map[string]bool{}
	}
	tracked.opened[contract] = true
	if tracked.checking {
		return false, true
	}
	t.manager.openContractFence(contract, datasetID, t.acquired)
	return true, true
}

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
	t.manager.closeFences()
	t.manager.clearHold()
	return nil
}

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

func (t *storageTakeover) check(
	ctx context.Context, attempt uint64, contract string, dataset EpochDataset, record store.StorageRecord, advances *datasetAdvances,
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
	described := info
	described.Incarnation = advanced.incarnation
	t.track(attempt, contract, described, dataset, outcome.ack.Watermark)
	return outcome
}

var errEpochBelowAcknowledgement = errors.New("dataset holds a lower controller epoch than it acknowledged")

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

func (t *storageTakeover) track(attempt uint64, contract string, info storagev1.ProviderInfo, dataset EpochDataset, acknowledged int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.inProgress(attempt) {
		return
	}
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
	if contract == storagev1.StateContract {
		tracked.dataset = dataset
	}
}

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
	if info.Incarnation != tracked.incarnation || tracked.checking {
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
	t.update(datasetID, func(tracked *trackedDataset) { tracked.acknowledged = max(tracked.acknowledged, watermark.Value) })
	if err := t.records.RecordStorageWatermark(callCtx, t.fence, datasetID, watermark.Value, t.manager.clock()); err != nil {
		log.Error(err, "Recording a storage dataset's write watermark failed; trying again later")
	}
	return nil
}

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
	if state.Epoch.Epoch != t.acquired.Epoch || state.Epoch.HolderID != t.acquired.HolderID {
		log.Info("A switched storage dataset holds another controller's epoch; keeping its fences closed",
			"storedEpoch", state.Epoch.Epoch, "storedHolder", state.Epoch.HolderID,
			"acquiredEpoch", t.acquired.Epoch, "acquiredHolder", t.acquired.HolderID)
		return nil, false
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

func (s taskSuccess) reportsNew(reported taskSuccess) bool {
	return (s.phase && !reported.phase) || (s.outcome && !reported.outcome) || (s.execution && !reported.execution)
}

func admitTaskStatus(
	ctx context.Context, epochs *ControllerEpochManager, reported taskSuccess,
	task *corev1alpha1.Task, write func(context.Context) error,
) error {
	if taskSuccessOf(task).reportsNew(reported) {
		return epochs.AdmitWork(ctx, write)
	}
	return write(ctx)
}

func (m *ControllerEpochManager) closeFences() {
	m.storageMu.Lock()
	for _, fence := range m.storage {
		fence.opened = false
	}
	m.notifyStorageLocked()
	m.storageMu.Unlock()
	m.drainAdmittedWork()
}

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

func (m *ControllerEpochManager) drainAdmittedWork() {
	m.admission.Lock()
	m.admission.Unlock() //nolint:staticcheck // An empty critical section waits for every reader.
}

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
