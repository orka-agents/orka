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
	var acquired *store.ControllerEpoch
	for attempt := range defaultEpochCASRetries {
		current, err := m.Store.GetControllerEpoch(ctx, m.Name)
		expectedVersion, expectedEpoch := int64(0), int64(0)
		if err == nil {
			expectedVersion, expectedEpoch = current.Version, current.Epoch
		} else if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("read controller epoch: %w", err)
		}
		now := time.Now().UTC()
		digest := controllerEpochDigest(m.Name, m.HolderID, expectedVersion, expectedEpoch, expectedEpoch+1)
		acquired, err = m.Store.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
			Name: m.Name, ExpectedVersion: expectedVersion, ExpectedEpoch: expectedEpoch, NewEpoch: expectedEpoch + 1,
			HolderID: m.HolderID, RequestDigest: digest, UpdatedAt: now,
		})
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("advance controller epoch: %w", err)
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
