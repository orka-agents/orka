package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

const defaultEpochCASRetries = 16

// ControllerEpochManager acquires one durable epoch after controller-runtime
// leader election. Every ACP control-store mutation must use CurrentFence so a
// stale leader cannot commit authoritative state after takeover.
type ControllerEpochManager struct {
	Store    store.ControllerEpochStore
	Name     string
	HolderID string
	Mirrors  []store.ControllerEpochMirror

	mu      sync.RWMutex
	current *store.ControllerEpoch
	ready   chan struct{}
	once    sync.Once
}

func NewControllerEpochManager(epochStore store.ControllerEpochStore, holderID string) *ControllerEpochManager {
	return &ControllerEpochManager{
		Store: epochStore, Name: store.DefaultControllerEpochName, HolderID: strings.TrimSpace(holderID), ready: make(chan struct{}),
	}
}

// WithMirror registers a subordinate epoch mirror that must synchronize before
// CurrentFence becomes available. Call this only during startup wiring.
func (m *ControllerEpochManager) WithMirror(mirror store.ControllerEpochMirror) *ControllerEpochManager {
	if m != nil {
		m.Mirrors = append(m.Mirrors, mirror)
	}
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
	current, err := readControllerEpoch(ctx, m.Store, m.Name)
	if err != nil {
		return err
	}
	pending, err := newControllerEpochCASIntent(m.Name, m.HolderID, current)
	if err != nil {
		return err
	}
	var acquired *store.ControllerEpoch
	for attempt := range defaultEpochCASRetries {
		candidate, casErr := m.Store.CompareAndSwapControllerEpoch(ctx, pending.change)
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
		observed, readErr := readControllerEpoch(ctx, m.Store, m.Name)
		if readErr != nil {
			return fmt.Errorf("reconcile controller epoch CAS conflict: %w", readErr)
		}
		switch {
		case controllerEpochMatchesCAS(observed, pending.change):
			acquired = observed
		case controllerEpochsEqual(observed, pending.predecessor):
			// The CAS did not commit. Retain the exact payload, including its
			// digest and timestamp, so a retry remains idempotent.
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 10 * time.Millisecond):
		}
	}
	if acquired == nil {
		return fmt.Errorf("controller epoch CAS did not converge after %d attempts", defaultEpochCASRetries)
	}
	for i, mirror := range m.Mirrors {
		if mirror == nil {
			return fmt.Errorf("controller epoch mirror %d is nil", i)
		}
		if err := mirror.SyncControllerEpochMirror(ctx, *acquired); err != nil {
			return fmt.Errorf("synchronize controller epoch mirror %d: %w", i, err)
		}
	}
	m.mu.Lock()
	m.current = acquired
	m.mu.Unlock()
	m.once.Do(func() { close(m.ready) })
	<-ctx.Done()
	return nil
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
	select {
	case <-m.ready:
	case <-ctx.Done():
		return store.ControllerEpochFence{}, ctx.Err()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current == nil {
		return store.ControllerEpochFence{}, fmt.Errorf("controller epoch is unavailable")
	}
	return store.ControllerEpochFence{Name: m.current.Name, Epoch: m.current.Epoch, HolderID: m.current.HolderID}, nil
}

func (m *ControllerEpochManager) Current() (store.ControllerEpoch, bool) {
	if m == nil {
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
