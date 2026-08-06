package memorybackend

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/store"
)

const (
	defaultFeatureHeartbeatInterval = 10 * time.Second
	defaultFeatureHeartbeatTTL      = 30 * time.Second
	maxDuration                     = time.Duration(1<<63 - 1)
)

// FeatureHeartbeat advertises this replica's supported memory feature epoch.
type FeatureHeartbeat struct {
	Store        store.GovernedMemoryStore
	InstanceID   string
	Role         string
	FeatureEpoch int64
	Interval     time.Duration
	TTL          time.Duration
	Now          func() time.Time
}

// Start runs until manager shutdown.
func (h *FeatureHeartbeat) Start(ctx context.Context) error {
	if h == nil || h.Store == nil {
		return nil
	}
	interval, ttl, err := h.effectiveTiming()
	if err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := h.write(ctx, ttl); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (h *FeatureHeartbeat) write(ctx context.Context, ttl time.Duration) error {
	now := h.now()
	return h.Store.UpsertControllerFeatureHeartbeat(ctx, store.ControllerFeatureHeartbeat{
		InstanceID: h.instanceID(), Role: h.role(), FeatureEpoch: h.featureEpoch(),
		LastHeartbeatAt: now, ExpiresAt: now.Add(ttl),
	})
}

func (h *FeatureHeartbeat) effectiveTiming() (time.Duration, time.Duration, error) {
	interval := h.Interval
	if interval <= 0 {
		interval = defaultFeatureHeartbeatInterval
	}
	ttl := h.TTL
	if ttl <= 0 {
		ttl = defaultFeatureHeartbeatTTL
	}
	if ttl <= interval {
		if interval <= maxDuration/3 {
			ttl = 3 * interval
		} else {
			ttl = maxDuration
		}
	}
	if ttl <= interval {
		return 0, 0, fmt.Errorf("memory feature heartbeat TTL must be greater than its interval")
	}
	return interval, ttl, nil
}

func (h *FeatureHeartbeat) instanceID() string {
	if h.InstanceID == "" {
		h.InstanceID = "memory-controller-" + uuid.NewString()
	}
	return h.InstanceID
}

func (h *FeatureHeartbeat) role() string {
	if h.Role == "" {
		return "serving_dispatching"
	}
	return h.Role
}

func (h *FeatureHeartbeat) featureEpoch() int64 {
	if h.FeatureEpoch <= 0 {
		return ActivationFeatureEpoch
	}
	return h.FeatureEpoch
}

func (h *FeatureHeartbeat) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

// CheckFeatureEpochReadiness fails closed when an activated binding requires a
// newer binary feature epoch, remote-memory serving is disabled, or the
// configured cluster identity differs from any durable remote binding.
func CheckFeatureEpochReadiness(
	ctx context.Context,
	governed store.GovernedMemoryStore,
	supported int64,
	enabled bool,
	clusterIdentity string,
) error {
	if governed == nil {
		return nil
	}
	clusterIdentity = strings.TrimSpace(clusterIdentity)
	return governed.ForEachMemoryBackendBinding(ctx, store.MemoryBackendBindingFilter{
		Modes: []store.MemoryBackendMode{store.MemoryBackendModeRemote},
	}, func(binding store.MemoryBackendBinding) error {
		if clusterIdentity == "" || binding.ClusterID != clusterIdentity {
			return fmt.Errorf("memory backend %s belongs to a different cluster identity", binding.NamespaceUID)
		}
		if !enabled {
			return fmt.Errorf("memory backend %s is active while MemoryBackend support is disabled", binding.NamespaceUID)
		}
		if binding.MinimumFeatureEpoch > supported {
			return fmt.Errorf("memory backend requires feature epoch %d; this replica supports %d", binding.MinimumFeatureEpoch, supported)
		}
		return nil
	})
}
