package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// samples counts the observations vec holds for method.
func samples(t *testing.T, vec *prometheus.HistogramVec, method string) uint64 {
	t.Helper()
	var metric dto.Metric
	require.NoError(t, vec.WithLabelValues(method).(prometheus.Histogram).Write(&metric))
	return metric.GetHistogram().GetSampleCount()
}

// A write operation records how long it waited for the connection and how
// long it then held it, under its method, so a pool wait can be told from a
// slow transaction.
func TestWriteOperationRecordsItsWaitAndHold(t *testing.T) {
	s := setupTestStore(t)
	dataset := advanceTestEpoch(t, s, testWriteFence)
	const method = "AppendExecutionEvent"
	waits, holds := samples(t, writeWaitSeconds, method), samples(t, writeHoldSeconds, method)

	event := &store.ExecutionEvent{Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task-a",
		TaskName: "task-a", Type: events.ExecutionEventTypeTaskStarted}
	ctx, _ := testEnvelope(t, t.Context(), dataset, method, time.Now(), event)
	_, err := s.AppendExecutionEvent(ctx, event)
	require.NoError(t, err)

	require.Equal(t, waits+1, samples(t, writeWaitSeconds, method))
	require.Equal(t, holds+1, samples(t, writeHoldSeconds, method))
}

// Receipt pruning runs outside a write transaction and records its wait and
// hold as one does.
func TestReceiptPruningRecordsItsWaitAndHold(t *testing.T) {
	s := setupTestStore(t)
	const method = "PruneOperationReceipts"
	waits, holds := samples(t, writeWaitSeconds, method), samples(t, writeHoldSeconds, method)
	_, err := s.PruneOperationReceipts(t.Context(), receiptPruneBatch)
	require.NoError(t, err)
	require.Equal(t, waits+1, samples(t, writeWaitSeconds, method))
	require.Equal(t, holds+1, samples(t, writeHoldSeconds, method))
}

// The store the controller opens exports its connection pool statistics
// while it is open.
func TestLockedStoreExportsPoolStatsWhileOpen(t *testing.T) {
	poolStatsExported := func() bool {
		families, err := metrics.Registry.Gather()
		require.NoError(t, err)
		for _, family := range families {
			if family.GetName() != "go_sql_wait_duration_seconds_total" {
				continue
			}
			for _, metric := range family.GetMetric() {
				for _, label := range metric.GetLabel() {
					if label.GetName() == "db_name" && label.GetValue() == dbStatsName {
						return true
					}
				}
			}
		}
		return false
	}
	require.False(t, poolStatsExported(), "no store is open")
	s, err := OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	require.True(t, poolStatsExported())
	require.NoError(t, s.Close())
	require.False(t, poolStatsExported(), "a closed store's statistics are unregistered")
}
