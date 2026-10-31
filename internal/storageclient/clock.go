package storageclient

import (
	"context"
	"errors"
	"fmt"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// DefaultClockCheckInterval is how often a ClockMonitor checks the clock of
// each dataset.
const DefaultClockCheckInterval = time.Minute

// clockSkew is the estimated offset of one dataset's clock from Orka's.
type clockSkew struct {
	offset      time.Duration
	uncertainty time.Duration
}

func (s clockSkew) String() string {
	return fmt.Sprintf("%s (within %s)", s.offset, s.uncertainty)
}

// estimateClockSkew estimates how far a provider clock that read observedAt is
// ahead of Orka's clock, from a Describe call sent at sent and answered at
// received. The provider read its clock somewhere in between, so the estimate
// takes the midpoint and is uncertain by half the round trip.
func estimateClockSkew(sent, received, observedAt time.Time) clockSkew {
	half := received.Sub(sent) / 2
	return clockSkew{offset: observedAt.Sub(sent.Add(half)), uncertainty: half}
}

// beyond reports whether the offset exceeds limit even at the edge of its
// uncertainty.
func (s clockSkew) beyond(limit time.Duration) bool {
	offset := s.offset
	if offset < 0 {
		offset = -offset
	}
	return offset-s.uncertainty > limit
}

// CheckClocks estimates how far the clock of each dataset the client writes
// to is from Orka's clock, from the time its provider reports in Describe
// (ProviderInfo.ObservedAt). While a dataset's clock is more than
// storagev1.MaxClockSkew away, even allowing for the round trip, the client
// refuses new writes to that dataset with ErrNotReady, and a later check that
// finds the clocks back within bounds lifts the refusal. Leases are decided
// on Orka's clock, so this bounds only the provider clock's own uses:
// operation age, receipt retention and the times it fills in.
//
// A provider that does not report ObservedAt is not checked. A Describe
// failure leaves that dataset's state as it was. The error names each
// dataset that is out of bounds or could not be checked.
func (c *Client) CheckClocks(ctx context.Context) error {
	var errs []error
	checked := map[string]bool{}
	for _, binding := range []struct {
		contract string
		provider storagev1.ProviderDescriber
	}{
		{storagev1.StateContract, c.state},
		{storagev1.ArtifactsContract, c.artifacts},
	} {
		sent := c.ops.now()
		info, err := binding.provider.Describe(ctx)
		received := c.ops.now()
		if err != nil {
			errs = append(errs, fmt.Errorf("check the %s clock: %w", binding.contract, err))
			continue
		}
		if checked[info.DatasetID] {
			continue
		}
		checked[info.DatasetID] = true
		if info.ObservedAt.IsZero() {
			c.ops.setClockSkew(info.DatasetID, nil)
			continue
		}
		skew := estimateClockSkew(sent, received, info.ObservedAt)
		if !skew.beyond(storagev1.MaxClockSkew) {
			c.ops.setClockSkew(info.DatasetID, nil)
			continue
		}
		c.ops.setClockSkew(info.DatasetID, &skew)
		errs = append(errs, fmt.Errorf("%w: the clock of dataset %s is %s away from Orka's, more than %s",
			storagev1.ErrNotReady, info.DatasetID, skew, storagev1.MaxClockSkew))
	}
	return errors.Join(errs...)
}

// setClockSkew records that a dataset's clock is out of bounds by skew, or
// back within bounds when skew is nil, and logs a change.
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

// requireClockAgreement refuses a new write to a dataset whose clock the last
// check found out of bounds.
func (o *operations) requireClockAgreement(datasetID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if skew, ok := o.clockSkews[datasetID]; ok {
		return fmt.Errorf("%w: %w: the clock of dataset %s is %s away from Orka's, more than %s",
			storagev1.ErrNotReady, storagev1.ErrClockSkew, datasetID, skew, storagev1.MaxClockSkew)
	}
	return nil
}

// ClockMonitor checks the storage clocks (see Client.CheckClocks) when it
// starts and then every Interval, or DefaultClockCheckInterval when Interval
// is zero. It runs on the leader, the only controller that writes.
type ClockMonitor struct {
	Client   *Client
	Interval time.Duration
}

// NeedLeaderElection implements the controller-runtime LeaderElectionRunnable.
func (m *ClockMonitor) NeedLeaderElection() bool { return true }

// Start checks the clocks until ctx ends. A failed check is logged and retried
// at the next interval; it never stops the controller.
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
