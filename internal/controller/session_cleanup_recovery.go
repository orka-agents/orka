package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const defaultSessionCleanupRecoveryInterval = 30 * time.Second

type sessionCleanupFenceSource interface {
	CurrentFence(context.Context) (store.ControllerEpochFence, error)
}

// SessionCleanupRecoveryManager replays durable cross-store Session deletion
// intents after controller restart. It runs only on the elected controller.
type SessionCleanupRecoveryManager struct {
	cleanup  store.SessionCleanupRecoveryStore
	fences   sessionCleanupFenceSource
	interval time.Duration
}

// NewSessionCleanupRecoveryManager constructs the startup recovery runnable.
func NewSessionCleanupRecoveryManager(cleanup store.SessionCleanupRecoveryStore, epochs *ControllerEpochManager) *SessionCleanupRecoveryManager {
	return &SessionCleanupRecoveryManager{cleanup: cleanup, fences: epochs, interval: defaultSessionCleanupRecoveryInterval}
}

// Start retries pending cleanups until the manager context is cancelled.
func (m *SessionCleanupRecoveryManager) Start(ctx context.Context) error {
	if m == nil || m.cleanup == nil || m.fences == nil {
		return fmt.Errorf("session cleanup recovery requires a cleanup store and controller epoch source")
	}
	interval := m.interval
	if interval <= 0 {
		interval = defaultSessionCleanupRecoveryInterval
	}
	logger := log.FromContext(ctx).WithName("session-cleanup-recovery")
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			fence, err := m.fences.CurrentFence(ctx)
			if err != nil {
				logger.Error(err, "unable to acquire controller epoch for Session cleanup recovery")
			} else if err := m.cleanup.ResumeSessionCleanups(ctx, fence); err != nil {
				logger.Error(err, "one or more pending Session cleanups remain blocked")
			}
			timer.Reset(interval)
		}
	}
}

// NeedLeaderElection keeps recovery single-writer with the controller epoch.
func (*SessionCleanupRecoveryManager) NeedLeaderElection() bool { return true }
