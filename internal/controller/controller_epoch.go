package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	defaultEpochAcquisitionTimeout = 3 * time.Minute
	defaultEpochCASMaxRetryDelay   = time.Second
)

type ControllerEpochManager struct {
	Store    store.ControllerEpochStore
	Name     string
	HolderID string

	WatermarkInterval time.Duration
	HoldPollInterval  time.Duration

	mu      sync.RWMutex
	current *store.ControllerEpoch
	ready   chan struct{}
	once    sync.Once

	admission      sync.RWMutex
	storageMu      sync.Mutex
	storage        map[string]*contractFence
	hold           *store.StorageRecoveryHold
	storageChanged chan struct{}

	newHoldID func() string
	now       func() time.Time
}

type EpochDataset interface {
	storagev1.ProviderDescriber
	storagev1.ControllerEpochAdvancer
	storagev1.WriteWatermarkStore
}

type contractFence struct {
	dataset   EpochDataset
	opened    bool
	datasetID string
	fence     store.ControllerEpochFence
}

const (
	epochStorageCallTimeout   = 30 * time.Second
	epochStorageMaxRetryDelay = 30 * time.Second
)

func NewControllerEpochManager(epochStore store.ControllerEpochStore, holderID string) *ControllerEpochManager {
	return &ControllerEpochManager{
		Store: epochStore, Name: store.DefaultControllerEpochName, HolderID: strings.TrimSpace(holderID), ready: make(chan struct{}),
	}
}

func (m *ControllerEpochManager) WithStorage(contract string, dataset EpochDataset) *ControllerEpochManager {
	if m == nil {
		return m
	}
	m.storageMu.Lock()
	defer m.storageMu.Unlock()
	if m.storage == nil {
		m.storage = map[string]*contractFence{}
	}
	m.storage[contract] = &contractFence{dataset: dataset}
	return m
}

func (m *ControllerEpochManager) NeedLeaderElection() bool { return true }

func (m *ControllerEpochManager) Start(ctx context.Context) error {
	if m == nil || m.Store == nil {
		return fmt.Errorf("controller epoch store is required")
	}
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		m.Name = store.DefaultControllerEpochName
	}
	m.HolderID = strings.TrimSpace(m.HolderID)
	if m.HolderID == "" {
		return fmt.Errorf("controller epoch holder ID is required")
	}
	acquisitionCtx, cancelAcquisition := context.WithTimeout(ctx, defaultEpochAcquisitionTimeout)
	defer cancelAcquisition()
	current, err := readControllerEpoch(acquisitionCtx, m.Store, m.Name)
	if err != nil {
		return err
	}
	pending, err := newControllerEpochCASIntent(m.Name, m.HolderID, current)
	if err != nil {
		return err
	}
	var acquired *store.ControllerEpoch
	for attempt := 0; ; attempt++ {
		if err := acquisitionCtx.Err(); err != nil {
			return fmt.Errorf("advance controller epoch: %w", err)
		}
		candidate, casErr := m.Store.CompareAndSwapControllerEpoch(acquisitionCtx, pending.change)
		err = casErr
		if err == nil {
			if !controllerEpochMatchesCAS(candidate, pending.change) {
				return fmt.Errorf("advance controller epoch: store returned a result that does not match the submitted CAS intent")
			}
			acquired = candidate
			break
		}
		if !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("advance controller epoch: %w", err)
		}
		observed, readErr := readControllerEpoch(acquisitionCtx, m.Store, m.Name)
		if readErr != nil {
			return fmt.Errorf("reconcile controller epoch CAS conflict: %w", readErr)
		}
		switch {
		case controllerEpochMatchesCAS(observed, pending.change):
			acquired = observed
		case controllerEpochsEqual(observed, pending.predecessor):
		case coherentCompetingControllerEpoch(observed, pending):
			pending, err = newControllerEpochCASIntent(m.Name, m.HolderID, observed)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf(
				"reconcile controller epoch CAS conflict: authoritative state is not the exact predecessor, submitted target, or a coherent competing advancement",
			)
		}
		if acquired != nil {
			break
		}
		delay := min(time.Duration(attempt+1)*10*time.Millisecond, defaultEpochCASMaxRetryDelay)
		select {
		case <-acquisitionCtx.Done():
			return fmt.Errorf("controller epoch acquisition did not converge: %w; last CAS conflict: %v", acquisitionCtx.Err(), casErr)
		case <-time.After(delay):
		}
	}
	cancelAcquisition()
	return m.openStorage(ctx, *acquired)
}

func (m *ControllerEpochManager) openStorage(ctx context.Context, acquired store.ControllerEpoch) error {
	m.storageMu.Lock()
	contracts := make(map[string]*contractFence, len(m.storage))
	maps.Copy(contracts, m.storage)
	m.storageMu.Unlock()
	if _, ok := contracts[storagev1.StateContract]; !ok {
		m.openCurrent(acquired)
	}
	if len(contracts) == 0 {
		<-ctx.Done()
		return nil
	}
	for contract, fence := range contracts {
		if isNilEpochDataset(fence.dataset) {
			return fmt.Errorf("no dataset serves %s", contract)
		}
	}
	installations, ok := m.Store.(store.ControllerEpochInstallationReader)
	if !ok {
		return fmt.Errorf("controller epoch store cannot identify the installation that owns storage")
	}
	records, ok := m.Store.(store.StorageRecordStore)
	if !ok {
		return fmt.Errorf("controller epoch store cannot record what storage acknowledged")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	installationID, err := retryEpochStorageStep(ctx, "read the controller epoch installation ID",
		func(ctx context.Context) (string, error) {
			return installations.ControllerEpochInstallationID(ctx, acquired.Name)
		})
	if err != nil {
		return ignoreDone(ctx, err)
	}
	takeover := &storageTakeover{
		manager: m, acquired: acquired, installationID: installationID, records: records, contracts: contracts,
		fence: store.ControllerEpochFence{Name: acquired.Name, Epoch: acquired.Epoch, HolderID: acquired.HolderID},
	}
	return ignoreDone(ctx, takeover.run(ctx))
}

func (m *ControllerEpochManager) openContractFence(contract, datasetID string, acquired store.ControllerEpoch) {
	value := store.ControllerEpochFence{Name: acquired.Name, Epoch: acquired.Epoch, HolderID: acquired.HolderID}
	m.storageMu.Lock()
	fence := m.storage[contract]
	if fence != nil && (!fence.opened || fence.datasetID != datasetID || fence.fence != value) {
		fence.datasetID, fence.fence, fence.opened = datasetID, value, true
		m.notifyStorageLocked()
	}
	m.storageMu.Unlock()
	if contract == storagev1.StateContract {
		m.openCurrent(acquired)
	}
}

func (m *ControllerEpochManager) openCurrent(acquired store.ControllerEpoch) {
	m.mu.Lock()
	m.current = &acquired
	m.mu.Unlock()
	m.once.Do(func() { close(m.ready) })
}

type datasetAdvances struct {
	mu       sync.Mutex
	advances map[string]*datasetAdvance
}

type datasetAdvance struct {
	done   chan struct{}
	result datasetAdvanceResult
	err    error
}

type datasetAdvanceResult struct {
	incarnation  string
	storedBefore int64
	watermark    storagev1.WriteWatermark
	hold         *store.StorageRecoveryHold
}

func (a *datasetAdvances) do(
	ctx context.Context, datasetID string, advance func(context.Context) (datasetAdvanceResult, error),
) (datasetAdvanceResult, error) {
	a.mu.Lock()
	existing, ok := a.advances[datasetID]
	if !ok {
		existing = &datasetAdvance{done: make(chan struct{})}
		a.advances[datasetID] = existing
	}
	a.mu.Unlock()
	if !ok {
		existing.result, existing.err = advance(ctx)
		close(existing.done)
		return existing.result, existing.err
	}
	select {
	case <-existing.done:
		return existing.result, existing.err
	case <-ctx.Done():
		return datasetAdvanceResult{}, ctx.Err()
	}
}

func retryEpochStorageStep[T any](ctx context.Context, step string, call func(context.Context) (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, epochStorageCallTimeout)
		value, err := call(callCtx)
		cancel()
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return value, ctx.Err()
		}
		if definiteEpochStorageError(err) {
			return value, fmt.Errorf("%s: %w", step, err)
		}
		logf.FromContext(ctx).Error(err, "Storage step at takeover failed; retrying", "step", step)
		delay := min(time.Duration(1<<min(attempt, 9))*100*time.Millisecond, epochStorageMaxRetryDelay)
		select {
		case <-ctx.Done():
			return value, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func definiteEpochStorageError(err error) bool {
	return errors.Is(err, store.ErrConflict) ||
		errors.Is(err, errEpochBelowAcknowledgement) ||
		errors.Is(err, storagev1.ErrDatasetMismatch) ||
		errors.Is(err, storagev1.ErrIncompatible) ||
		errors.Is(err, store.ErrValidation) ||
		errors.Is(err, store.ErrNotFound)
}

func ignoreDone(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func isNilEpochDataset(dataset EpochDataset) bool {
	if dataset == nil {
		return true
	}
	value := reflect.ValueOf(dataset)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func (m *ControllerEpochManager) WriteFence(ctx context.Context, contract string) (string, store.ControllerEpochFence, error) {
	if m == nil {
		return "", store.ControllerEpochFence{}, fmt.Errorf("controller epoch manager is not initialized")
	}
	for {
		m.storageMu.Lock()
		fence := m.storage[contract]
		if fence == nil {
			m.storageMu.Unlock()
			return "", store.ControllerEpochFence{}, fmt.Errorf("no dataset serves %s", contract)
		}
		if m.hold != nil {
			hold := *m.hold
			m.storageMu.Unlock()
			return "", store.ControllerEpochFence{}, &store.StorageHoldError{Hold: hold}
		}
		if fence.opened {
			datasetID, value := fence.datasetID, fence.fence
			m.storageMu.Unlock()
			return datasetID, value, nil
		}
		changed := m.storageSignalLocked()
		m.storageMu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return "", store.ControllerEpochFence{}, ctx.Err()
		}
	}
}

func (m *ControllerEpochManager) StorageHold() error {
	if m == nil {
		return nil
	}
	m.storageMu.Lock()
	defer m.storageMu.Unlock()
	if m.hold != nil {
		return &store.StorageHoldError{Hold: *m.hold}
	}
	for contract, fence := range m.storage {
		if !fence.opened {
			return store.NotReadyErrorf("the %s dataset has not passed its storage checks", contract)
		}
	}
	return nil
}

func (m *ControllerEpochManager) AdmitWork(ctx context.Context, create func(context.Context) error) error {
	if m == nil {
		return create(ctx)
	}
	m.admission.RLock()
	defer m.admission.RUnlock()
	if err := m.StorageHold(); err != nil {
		return err
	}
	return create(ctx)
}

func (m *ControllerEpochManager) heldError() error {
	m.storageMu.Lock()
	defer m.storageMu.Unlock()
	if m.hold == nil {
		return nil
	}
	return &store.StorageHoldError{Hold: *m.hold}
}

func (m *ControllerEpochManager) stateFenceOpenLocked() bool {
	fence := m.storage[storagev1.StateContract]
	return fence == nil || fence.opened
}

func (m *ControllerEpochManager) storageSignalLocked() chan struct{} {
	if m.storageChanged == nil {
		m.storageChanged = make(chan struct{})
	}
	return m.storageChanged
}

func (m *ControllerEpochManager) notifyStorageLocked() {
	if m.storageChanged != nil {
		close(m.storageChanged)
	}
	m.storageChanged = make(chan struct{})
}

type controllerEpochCASIntent struct {
	change      store.ControllerEpochCAS
	predecessor *store.ControllerEpoch
}

func readControllerEpoch(ctx context.Context, epochStore store.ControllerEpochStore, name string) (*store.ControllerEpoch, error) {
	current, err := epochStore.GetControllerEpoch(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read controller epoch: %w", err)
	}
	if err := validateControllerEpochForAcquisition(current, name); err != nil {
		return nil, err
	}
	copy := *current
	return &copy, nil
}

func newControllerEpochCASIntent(name, holderID string, predecessor *store.ControllerEpoch) (controllerEpochCASIntent, error) {
	expectedVersion, expectedEpoch := int64(0), int64(0)
	var predecessorCopy *store.ControllerEpoch
	if predecessor != nil {
		if err := validateControllerEpochForAcquisition(predecessor, name); err != nil {
			return controllerEpochCASIntent{}, err
		}
		expectedVersion, expectedEpoch = predecessor.Version, predecessor.Epoch
		copy := *predecessor
		predecessorCopy = &copy
	}
	change := store.ControllerEpochCAS{
		Name:            name,
		ExpectedVersion: expectedVersion,
		ExpectedEpoch:   expectedEpoch,
		NewEpoch:        expectedEpoch + 1,
		HolderID:        holderID,
		UpdatedAt:       time.Now().UTC(),
	}
	change.RequestDigest = controllerEpochDigest(
		change.Name,
		change.HolderID,
		change.ExpectedVersion,
		change.ExpectedEpoch,
		change.NewEpoch,
	)
	return controllerEpochCASIntent{change: change, predecessor: predecessorCopy}, nil
}

func validateControllerEpochForAcquisition(epoch *store.ControllerEpoch, name string) error {
	if epoch == nil {
		return fmt.Errorf("read controller epoch: store returned a nil record")
	}
	if epoch.Name != name {
		return fmt.Errorf("read controller epoch: logical name %q does not match %q", epoch.Name, name)
	}
	if epoch.Epoch < 1 || epoch.Version < 1 || epoch.Epoch != epoch.Version {
		return fmt.Errorf("read controller epoch: invalid epoch/version %d/%d", epoch.Epoch, epoch.Version)
	}
	if strings.TrimSpace(epoch.HolderID) != epoch.HolderID {
		return fmt.Errorf("read controller epoch: holder ID is not canonical")
	}
	if err := store.ValidateControlIdentifier("controller epoch holder ID", epoch.HolderID); err != nil {
		return fmt.Errorf("read controller epoch: %w", err)
	}
	if err := store.ValidateCanonicalDigest("controller epoch request digest", epoch.RequestDigest); err != nil {
		return fmt.Errorf("read controller epoch: %w", err)
	}
	if epoch.AcquiredAt.IsZero() || epoch.UpdatedAt.IsZero() || !epoch.AcquiredAt.Equal(epoch.UpdatedAt) {
		return fmt.Errorf("read controller epoch: invalid acquisition timestamps")
	}
	return nil
}

func controllerEpochMatchesCAS(epoch *store.ControllerEpoch, change store.ControllerEpochCAS) bool {
	return epoch != nil &&
		epoch.Name == change.Name &&
		epoch.Version == change.ExpectedVersion+1 &&
		epoch.Epoch == change.NewEpoch &&
		epoch.HolderID == change.HolderID &&
		epoch.RequestDigest == change.RequestDigest &&
		epoch.AcquiredAt.Equal(change.UpdatedAt) &&
		epoch.UpdatedAt.Equal(change.UpdatedAt)
}

func controllerEpochsEqual(left, right *store.ControllerEpoch) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Name == right.Name &&
		left.Version == right.Version &&
		left.Epoch == right.Epoch &&
		left.HolderID == right.HolderID &&
		left.RequestDigest == right.RequestDigest &&
		left.AcquiredAt.Equal(right.AcquiredAt) &&
		left.UpdatedAt.Equal(right.UpdatedAt)
}

func coherentCompetingControllerEpoch(observed *store.ControllerEpoch, pending controllerEpochCASIntent) bool {
	if observed == nil || observed.Name != pending.change.Name {
		return false
	}
	epochDelta := observed.Epoch - pending.change.ExpectedEpoch
	versionDelta := observed.Version - pending.change.ExpectedVersion
	return epochDelta >= 1 && epochDelta == versionDelta &&
		observed.HolderID != pending.change.HolderID &&
		observed.RequestDigest != pending.change.RequestDigest
}

func (m *ControllerEpochManager) CurrentFence(ctx context.Context) (store.ControllerEpochFence, error) {
	if m == nil || m.ready == nil {
		return store.ControllerEpochFence{}, fmt.Errorf("controller epoch manager is not initialized")
	}
	for {
		m.storageMu.Lock()
		if m.hold != nil {
			hold := *m.hold
			m.storageMu.Unlock()
			return store.ControllerEpochFence{}, &store.StorageHoldError{Hold: hold}
		}
		stateOpen := m.stateFenceOpenLocked()
		changed := m.storageSignalLocked()
		m.storageMu.Unlock()
		if !stateOpen {
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return store.ControllerEpochFence{}, ctx.Err()
			}
		}
		select {
		case <-m.ready:
			if current, ok := m.Current(); ok {
				return store.ControllerEpochFence{Name: current.Name, Epoch: current.Epoch, HolderID: current.HolderID}, nil
			}
			if err := m.heldError(); err != nil {
				return store.ControllerEpochFence{}, err
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return store.ControllerEpochFence{}, ctx.Err()
			}
		case <-changed:
		case <-ctx.Done():
			return store.ControllerEpochFence{}, ctx.Err()
		}
	}
}

func (m *ControllerEpochManager) Current() (store.ControllerEpoch, bool) {
	if m == nil {
		return store.ControllerEpoch{}, false
	}
	m.storageMu.Lock()
	usable := m.hold == nil && m.stateFenceOpenLocked()
	m.storageMu.Unlock()
	if !usable {
		return store.ControllerEpoch{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return store.ControllerEpoch{}, false
	}
	return *m.current, true
}

func controllerEpochDigest(name, holder string, expectedVersion, expectedEpoch, newEpoch int64) string {
	payload := fmt.Sprintf("orka-controller-epoch-v1\x00%s\x00%s\x00%d\x00%d\x00%d", name, holder, expectedVersion, expectedEpoch, newEpoch)
	sum := sha256.Sum256([]byte(payload))
	return "sha256:" + hex.EncodeToString(sum[:])
}
