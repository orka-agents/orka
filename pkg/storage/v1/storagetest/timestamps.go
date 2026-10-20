package storagetest

import (
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// timestampNanos has a non-zero sub-microsecond part, so a provider that
// stores microseconds or milliseconds changes it, and a non-UTC offset, so a
// provider that keeps the caller's zone instead of returning UTC fails.
var timestampNanos = time.Date(2026, time.March, 4, 5, 6, 7, 123456789, time.FixedZone("AEST", 10*60*60))

// requireSameInstant checks that a stored time round-trips exactly, to the
// nanosecond, and comes back in UTC.
func requireSameInstant(t *testing.T, want, got time.Time, field string) {
	t.Helper()
	require.Truef(t, want.Equal(got), "%s = %s, want %s", field, got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	require.Equalf(t, time.UTC, got.Location(), "%s is returned in UTC", field)
}

func runTimestamps(t *testing.T, newProvider StateFactory) {
	t.Run("Sessions and transcripts", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		createSessionFixture(t, provider, "tenant-a", "precise", sessionsTypeTask, timestampNanos)
		got, err := provider.GetSession(ctx, "tenant-a", "precise")
		require.NoError(t, err)
		requireSameInstant(t, timestampNanos, got.CreatedAt, "SessionRecord.CreatedAt")
		requireSameInstant(t, timestampNanos, got.UpdatedAt, "SessionRecord.UpdatedAt")

		messageAt := timestampNanos.Add(time.Nanosecond)
		require.NoError(t, provider.AppendMessages(ctx, "tenant-a", "precise", []storagev1.SessionMessage{
			{ID: "m1", Role: "user", Content: "hello", Timestamp: messageAt},
		}))
		transcript, err := provider.LoadTranscript(ctx, "tenant-a", "precise", 0)
		require.NoError(t, err)
		require.Len(t, transcript, 1)
		requireSameInstant(t, messageAt, transcript[0].Timestamp, "SessionMessage.Timestamp")
	})

	t.Run("gateway events", func(t *testing.T) {
		provider := newProvider(t)
		event := gatewayEvent(timestampNanos, "tenant-a", "precise")
		admitGateway(t, provider, event)
		got, err := provider.GetGatewayEvent(t.Context(), "tenant-a", event.ID)
		require.NoError(t, err)
		requireSameInstant(t, event.ReceivedAt, got.ReceivedAt, "GatewayEvent.ReceivedAt")
		requireSameInstant(t, event.ExpiresAt, got.ExpiresAt, "GatewayEvent.ExpiresAt")
	})

	t.Run("outbox projections", func(t *testing.T) {
		provider := newProvider(t)
		fence := syncEpoch(t, provider, 1, "owner-a")
		projection := outboxProjection("precise", `{"n":1}`, timestampNanos)
		projection.CreatedAt = timestampNanos
		_, err := provider.EnqueueOutboxProjectionRecord(t.Context(), projection, fence)
		require.NoError(t, err)
		got, err := provider.GetOutboxProjection(t.Context(), "precise")
		require.NoError(t, err)
		requireSameInstant(t, timestampNanos, got.AvailableAt, "OutboxProjection.AvailableAt")
		requireSameInstant(t, timestampNanos, got.CreatedAt, "OutboxProjection.CreatedAt")
	})

	t.Run("repository monitor and security runs", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		require.NoError(t, provider.CreateMonitorRun(ctx, &storagev1.MonitorRun{
			ID: "run-precise", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, StartedAt: timestampNanos,
		}))
		monitorRun, err := provider.GetMonitorRun(ctx, workflowNamespace, "run-precise")
		require.NoError(t, err)
		requireSameInstant(t, timestampNanos, monitorRun.StartedAt, "MonitorRun.StartedAt")

		scan := workflowScanRun("scan-precise", "completed")
		scan.StartedAt = timestampNanos
		require.NoError(t, provider.CreateScanRun(ctx, scan))
		scanRun, err := provider.GetScanRun(ctx, workflowNamespace, "scan-precise")
		require.NoError(t, err)
		requireSameInstant(t, timestampNanos, scanRun.StartedAt, "ScanRun.StartedAt")
	})
}
