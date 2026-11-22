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

type SessionCleanupRecoveryManager struct {
	cleanup  store.SessionCleanupRecoveryStore
	fences   sessionCleanupFenceSource
	interval time.Duration
}

func NewSessionCleanupRecoveryManager(cleanup store.SessionCleanupRecoveryStore, epochs *ControllerEpochManager) *SessionCleanupRecoveryManager {
	return &SessionCleanupRecoveryManager{cleanup: cleanup, fences: epochs, interval: defaultSessionCleanupRecoveryInterval}
}

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

func (*SessionCleanupRecoveryManager) NeedLeaderElection() bool { return true }
