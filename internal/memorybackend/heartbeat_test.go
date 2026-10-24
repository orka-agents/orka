package memorybackend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

type featureHeartbeatCaptureStore struct {
	store.GovernedMemoryStore
	heartbeat store.ControllerFeatureHeartbeat
	cancel    context.CancelFunc
}

func (s *featureHeartbeatCaptureStore) UpsertControllerFeatureHeartbeat(
	_ context.Context,
	heartbeat store.ControllerFeatureHeartbeat,
) error {
	s.heartbeat = heartbeat
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func TestFeatureHeartbeatUsesOneEffectiveIntervalForTickerAndTTL(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		interval     time.Duration
		ttl          time.Duration
		wantInterval time.Duration
		wantTTL      time.Duration
	}{
		{name: "defaults", wantInterval: 10 * time.Second, wantTTL: 30 * time.Second},
		{name: "default interval with short custom TTL", ttl: 5 * time.Second, wantInterval: 10 * time.Second, wantTTL: 30 * time.Second},
		{name: "custom interval expands default TTL", interval: time.Minute, wantInterval: time.Minute, wantTTL: 3 * time.Minute},
		{name: "custom valid TTL", interval: 5 * time.Second, ttl: 6 * time.Second, wantInterval: 5 * time.Second, wantTTL: 6 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			capture := &featureHeartbeatCaptureStore{cancel: cancel}
			heartbeat := &FeatureHeartbeat{
				Store: capture, InstanceID: "replica-a", Interval: test.interval, TTL: test.ttl,
				Now: func() time.Time { return now },
			}
			interval, ttl, err := heartbeat.effectiveTiming()
			if err != nil {
				t.Fatalf("effectiveTiming() error = %v", err)
			}
			if interval != test.wantInterval || ttl != test.wantTTL {
				t.Fatalf("effectiveTiming() = (%s, %s), want (%s, %s)", interval, ttl, test.wantInterval, test.wantTTL)
			}
			if ttl <= interval {
				t.Fatalf("effective TTL %s must be greater than interval %s", ttl, interval)
			}
			if err := heartbeat.Start(ctx); err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			if got := capture.heartbeat.ExpiresAt.Sub(capture.heartbeat.LastHeartbeatAt); got != test.wantTTL {
				t.Fatalf("persisted heartbeat TTL = %s, want %s", got, test.wantTTL)
			}
		})
	}
}

type readinessBindingStore struct {
	store.GovernedMemoryStore
	bindings []store.MemoryBackendBinding
}

func (s *readinessBindingStore) ForEachMemoryBackendBinding(
	_ context.Context,
	filter store.MemoryBackendBindingFilter,
	visit store.MemoryBackendBindingVisitor,
) error {
	for _, binding := range s.bindings {
		if len(filter.Modes) > 0 && binding.Mode != filter.Modes[0] {
			continue
		}
		if err := visit(binding); err != nil {
			return err
		}
	}
	return nil
}

func TestFeatureEpochReadinessRejectsClusterIdentityDrift(t *testing.T) {
	governed := &readinessBindingStore{bindings: []store.MemoryBackendBinding{
		{NamespaceUID: "namespace-a", ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote, MinimumFeatureEpoch: 1},
		{NamespaceUID: "namespace-b", ClusterID: "cluster-b", Mode: store.MemoryBackendModeRemote, MinimumFeatureEpoch: 1},
	}}
	err := CheckFeatureEpochReadiness(context.Background(), governed, ActivationFeatureEpoch, true, "cluster-a")
	if err == nil || !strings.Contains(err.Error(), "different cluster identity") {
		t.Fatalf("CheckFeatureEpochReadiness() error = %v, want cluster identity drift rejection", err)
	}
}

func TestFeatureEpochReadinessAcceptsMatchingClusterIdentity(t *testing.T) {
	governed := &readinessBindingStore{bindings: []store.MemoryBackendBinding{{
		NamespaceUID: "namespace-a", ClusterID: "cluster-a",
		Mode: store.MemoryBackendModeRemote, MinimumFeatureEpoch: ActivationFeatureEpoch,
	}}}
	if err := CheckFeatureEpochReadiness(
		context.Background(), governed, ActivationFeatureEpoch, true, "cluster-a",
	); err != nil {
		t.Fatalf("CheckFeatureEpochReadiness() error = %v", err)
	}
}

func TestFeatureEpochReadinessIgnoresLegacyBindingWhenFoundationDisabled(t *testing.T) {
	governed := &readinessBindingStore{bindings: []store.MemoryBackendBinding{{
		NamespaceUID: "namespace-a", ClusterID: "retired-cluster",
		Mode: store.MemoryBackendModeLegacy, State: store.MemoryBackendBindingLegacy,
	}}}
	if err := CheckFeatureEpochReadiness(context.Background(), governed, ActivationFeatureEpoch, false, ""); err != nil {
		t.Fatalf("CheckFeatureEpochReadiness() legacy error = %v", err)
	}
}
