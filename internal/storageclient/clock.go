package storageclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const DefaultClockCheckInterval = time.Minute

type clockSkew struct {
	offset      time.Duration
	uncertainty time.Duration
}

func (s clockSkew) String() string {
	return fmt.Sprintf("%s (within %s)", s.offset, s.uncertainty)
}

func estimateClockSkew(sent, received, observedAt time.Time) clockSkew {
	half := received.Sub(sent) / 2
	return clockSkew{offset: observedAt.Sub(sent.Add(half)), uncertainty: half}
}

func (s clockSkew) beyond(limit time.Duration) bool {
	offset := s.offset
	if offset < 0 {
		offset = -offset
	}
	return offset-s.uncertainty > limit
}

func (c *Client) CheckClocks(ctx context.Context) error {
	bindings := []struct {
		contract string
		provider storagev1.ProviderDescriber
	}{
		{storagev1.StateContract, c.state},
		{storagev1.ArtifactsContract, c.artifacts},
	}
	if sameStore(c.state, c.artifacts) {
		bindings = bindings[:1]
	}
	var (
		mu     sync.Mutex
		errs   []error
		claims = &clockClaims{claimed: map[string]bool{}}
		wg     sync.WaitGroup
	)
	for _, binding := range bindings {
		wg.Go(func() {
			if err := c.checkClock(ctx, binding.contract, binding.provider, claims); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

type clockClaims struct {
	mu      sync.Mutex
	claimed map[string]bool
}

func (c *clockClaims) claim(datasetID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.claimed[datasetID] {
		return false
	}
	c.claimed[datasetID] = true
	return true
}

func (c *clockClaims) clearUnclaimed(datasetID string, clear func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.claimed[datasetID] {
		clear()
	}
}

func (c *Client) checkClock(ctx context.Context, contract string, provider storagev1.ProviderDescriber, claims *clockClaims) error {
	sent := c.ops.now()
	info, err := provider.Describe(ctx)
	received := c.ops.now()
	if err != nil {
		return fmt.Errorf("check the %s clock: %w", contract, err)
	}
	if info.ObservedAt.IsZero() {
		claims.clearUnclaimed(info.DatasetID, func() { c.ops.setClockSkew(info.DatasetID, nil) })
		return nil
	}
	if !claims.claim(info.DatasetID) {
		return nil
	}
	skew := estimateClockSkew(sent, received, info.ObservedAt)
	if !skew.beyond(storagev1.MaxClockSkew) {
		c.ops.setClockSkew(info.DatasetID, nil)
		return nil
	}
	c.ops.setClockSkew(info.DatasetID, &skew)
	return fmt.Errorf("%w: the clock of dataset %s is %s away from Orka's, more than %s",
		storagev1.ErrNotReady, info.DatasetID, skew, storagev1.MaxClockSkew)
}

func (o *operations) setClockSkew(datasetID string, skew *clockSkew) {
	o.mu.Lock()
	previous, wasSkewed := o.clockSkews[datasetID]
	if skew == nil {
		delete(o.clockSkews, datasetID)
	} else {
		o.clockSkews[datasetID] = *skew
	}
	o.mu.Unlock()
	log := logf.Log.WithName("storage-client").WithValues("dataset", datasetID)
	switch {
	case skew != nil && !wasSkewed:
		log.Error(storagev1.ErrClockSkew, "The storage clock is too far from the controller clock; writes to this dataset are refused",
			"offset", skew.offset.String(), "uncertainty", skew.uncertainty.String(), "limit", storagev1.MaxClockSkew.String())
	case skew == nil && wasSkewed:
		log.Info("The storage clock is back within bounds; writes to this dataset resume", "previousOffset", previous.offset.String())
	}
}

func (o *operations) requireClockAgreement(datasetID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if skew, ok := o.clockSkews[datasetID]; ok {
		return fmt.Errorf("%w: %w: the clock of dataset %s is %s away from Orka's, more than %s",
			storagev1.ErrNotReady, storagev1.ErrClockSkew, datasetID, skew, storagev1.MaxClockSkew)
	}
	return nil
}

type ClockMonitor struct {
	Client   *Client
	Interval time.Duration
}

func (m *ClockMonitor) NeedLeaderElection() bool { return true }

func (m *ClockMonitor) Start(ctx context.Context) error {
	interval := m.Interval
	if interval <= 0 {
		interval = DefaultClockCheckInterval
	}
	log := logf.FromContext(ctx).WithName("storage-clock")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		checkCtx, cancel := context.WithTimeout(ctx, interval)
		if err := m.Client.CheckClocks(checkCtx); err != nil && ctx.Err() == nil {
			log.V(1).Info("Storage clock check found a problem", "error", err.Error())
		}
		cancel()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
