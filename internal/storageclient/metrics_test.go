package storageclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
)

// storeMetricNames are the execution-event store metrics recorded below the
// API: appends, store-scope lists, redactions and truncations.
var storeMetricNames = map[string]bool{
	"orka_execution_events_appended_total":         true,
	"orka_execution_event_append_failures_total":   true,
	"orka_execution_event_append_duration_seconds": true,
	"orka_execution_event_list_requests_total":     true,
	"orka_execution_event_list_duration_seconds":   true,
	"orka_execution_event_redactions_total":        true,
	"orka_execution_event_truncations_total":       true,
}

// gatherStoreMetrics reads the store metrics from Orka's registry, the one the
// controller serves on its metrics endpoint. A counter maps to its value and
// a histogram to its sample count.
func gatherStoreMetrics(t *testing.T) map[string]float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	series := map[string]float64{}
	for _, family := range families {
		if !storeMetricNames[family.GetName()] {
			continue
		}
		for _, metric := range family.GetMetric() {
			key := family.GetName() + metricLabels(metric.GetLabel())
			switch {
			case metric.GetCounter() != nil:
				series[key] = metric.GetCounter().GetValue()
			case metric.GetHistogram() != nil:
				series[key] = float64(metric.GetHistogram().GetSampleCount())
			}
		}
	}
	return series
}

func metricLabels(pairs []*dto.LabelPair) string {
	labels := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		labels = append(labels, fmt.Sprintf("%s=%q", pair.GetName(), pair.GetValue()))
	}
	sort.Strings(labels)
	return "{" + strings.Join(labels, ",") + "}"
}

// storeMetricDelta runs fn and returns every store metric series it changed.
func storeMetricDelta(t *testing.T, fn func()) map[string]float64 {
	t.Helper()
	before := gatherStoreMetrics(t)
	fn()
	delta := map[string]float64{}
	for key, value := range gatherStoreMetrics(t) {
		if change := value - before[key]; change != 0 {
			delta[key] = change
		}
	}
	return delta
}

func workloadEvent(streamID, eventType string) *store.ExecutionEvent {
	return &store.ExecutionEvent{Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask, StreamID: streamID,
		TaskName: streamID, SessionName: "session", Type: eventType, ContentText: "hello"}
}

// runStoreMetricWorkload sends appends and lists through every path that
// records a store metric: each append method, deduplicated replays, appends
// and lists inside a Task data transaction, refusals by the client and by the
// provider, argument errors, and successful and failed lists.
func runStoreMetricWorkload(t *testing.T, client *storageclient.Client) {
	t.Helper()
	ctx := t.Context()
	_, err := client.AppendExecutionEvent(ctx, workloadEvent("a", events.ExecutionEventTypeTaskStarted))
	require.NoError(t, err)
	secret := workloadEvent("a", events.ExecutionEventTypeModelMessage)
	secret.ContentText = "api_key=" + protectedSecrets()["OpenAI key"] + " " +
		strings.Repeat("x", events.MaxExecutionEventContentTextChars)
	_, err = client.AppendExecutionEvent(ctx, secret)
	require.NoError(t, err)
	for range 2 {
		_, _, err = client.AppendExecutionEventIfAbsent(ctx, workloadEvent("b", events.ExecutionEventTypeToolCallCompleted), "key")
		require.NoError(t, err)
		_, _, err = client.AppendExecutionEventWithPlanIfAbsent(ctx, workloadEvent("c", events.ExecutionEventTypePlanUpdated), "key",
			&store.PlanState{Namespace: "tenant", TaskName: "c"})
		require.NoError(t, err)
	}
	require.NoError(t, client.WithTaskDataTransaction(ctx, func(tx context.Context) error {
		if _, err := client.AppendExecutionEvent(tx, workloadEvent("d", events.ExecutionEventTypeModelMessage)); err != nil {
			return err
		}
		_, err := client.ListExecutionEvents(tx, store.ExecutionEventFilter{Namespace: "tenant", StreamID: "d"})
		return err
	}))

	// The provider refuses these.
	missingNamespace := workloadEvent("e", events.ExecutionEventTypeModelMessage)
	missingNamespace.Namespace = ""
	unknownType := workloadEvent("e", "NotAnEventType")
	unknownStream := workloadEvent("e", events.ExecutionEventTypeModelMessage)
	unknownStream.StreamType = "other"
	for _, event := range []*store.ExecutionEvent{missingNamespace, unknownType, unknownStream} {
		_, err = client.AppendExecutionEvent(ctx, event)
		require.ErrorIs(t, err, store.ErrValidation)
	}
	_, _, err = client.AppendExecutionEventWithPlanIfAbsent(ctx, workloadEvent("e", events.ExecutionEventTypePlanUpdated), "key",
		&store.PlanState{Namespace: "tenant", TaskName: "other"})
	require.ErrorIs(t, err, store.ErrValidation)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = client.AppendExecutionEvent(canceled, workloadEvent("e", events.ExecutionEventTypeTaskFailed))
	require.ErrorIs(t, err, context.Canceled)

	// The client refuses these before calling the provider.
	_, err = client.AppendExecutionEvent(ctx, nil)
	require.ErrorIs(t, err, store.ErrValidation)
	report, err := json.Marshal(map[string]any{"usage": map[string]any{"id": "bad", "counterID": "c", "status": "unknown"}})
	require.NoError(t, err)
	invalidReport := workloadEvent("e", events.ExecutionEventTypeModelUsageUpdated)
	invalidReport.Content = report
	_, err = client.AppendExecutionEvent(store.WithUsageAttribution(ctx, store.UsageAttribution{TaskUID: "task-uid"}), invalidReport)
	require.ErrorIs(t, err, store.ErrValidation)

	// Argument errors are not append attempts and are not counted.
	_, _, err = client.AppendExecutionEventIfAbsent(ctx, workloadEvent("e", events.ExecutionEventTypeModelMessage), " ")
	require.ErrorIs(t, err, store.ErrValidation)
	_, _, err = client.AppendExecutionEventWithPlanIfAbsent(ctx, workloadEvent("e", events.ExecutionEventTypePlanUpdated), "key", nil)
	require.ErrorIs(t, err, store.ErrValidation)

	listed, err := client.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "tenant", StreamID: "a"})
	require.NoError(t, err)
	require.Len(t, listed, 2)
	_, err = client.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "tenant", EventTypes: []string{"Nope"}})
	require.ErrorIs(t, err, store.ErrValidation)
	_, err = client.ListExecutionEvents(canceled, store.ExecutionEventFilter{Namespace: "tenant"})
	require.Error(t, err)
	sessionEvents, _, err := client.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{Namespace: "tenant", SessionName: "session"})
	require.NoError(t, err)
	require.Len(t, sessionEvents, 5)
	_, _, err = client.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{Namespace: "tenant"})
	require.ErrorIs(t, err, store.ErrValidation)
	_, _, err = client.ListSessionExecutionEvents(canceled, store.SessionExecutionEventFilter{Namespace: "tenant", SessionName: "session"})
	require.Error(t, err)
}

// storeMetricsGolden is what the workload recorded before the store metrics
// moved into the client, when the SQLite provider recorded appends and store
// reads and the client recorded only its own refusals, redactions and
// truncations. Histograms map to their sample counts.
var storeMetricsGolden = map[string]float64{
	`orka_execution_event_append_duration_seconds{event_type="ModelMessage",result="error",stream_type="task"}`:        1,
	`orka_execution_event_append_duration_seconds{event_type="ModelMessage",result="error",stream_type="unknown"}`:     1,
	`orka_execution_event_append_duration_seconds{event_type="ModelMessage",result="success",stream_type="task"}`:      2,
	`orka_execution_event_append_duration_seconds{event_type="ModelUsageUpdated",result="error",stream_type="task"}`:   1,
	`orka_execution_event_append_duration_seconds{event_type="PlanUpdated",result="error",stream_type="task"}`:         1,
	`orka_execution_event_append_duration_seconds{event_type="PlanUpdated",result="success",stream_type="task"}`:       2,
	`orka_execution_event_append_duration_seconds{event_type="TaskFailed",result="error",stream_type="task"}`:          1,
	`orka_execution_event_append_duration_seconds{event_type="TaskStarted",result="success",stream_type="task"}`:       1,
	`orka_execution_event_append_duration_seconds{event_type="ToolCallCompleted",result="success",stream_type="task"}`: 2,
	`orka_execution_event_append_duration_seconds{event_type="unknown",result="error",stream_type="task"}`:             1,
	`orka_execution_event_append_duration_seconds{event_type="unknown",result="error",stream_type="unknown"}`:          1,
	`orka_execution_event_append_failures_total{event_type="ModelMessage",stream_type="task"}`:                         1,
	`orka_execution_event_append_failures_total{event_type="ModelMessage",stream_type="unknown"}`:                      1,
	`orka_execution_event_append_failures_total{event_type="ModelUsageUpdated",stream_type="task"}`:                    1,
	`orka_execution_event_append_failures_total{event_type="PlanUpdated",stream_type="task"}`:                          1,
	`orka_execution_event_append_failures_total{event_type="TaskFailed",stream_type="task"}`:                           1,
	`orka_execution_event_append_failures_total{event_type="unknown",stream_type="task"}`:                              1,
	`orka_execution_event_append_failures_total{event_type="unknown",stream_type="unknown"}`:                           1,
	`orka_execution_event_list_duration_seconds{result="error",scope="session_store"}`:                                 2,
	`orka_execution_event_list_duration_seconds{result="error",scope="task_store"}`:                                    2,
	`orka_execution_event_list_duration_seconds{result="success",scope="session_store"}`:                               1,
	`orka_execution_event_list_duration_seconds{result="success",scope="task_store"}`:                                  2,
	`orka_execution_event_list_requests_total{result="error",scope="session_store"}`:                                   2,
	`orka_execution_event_list_requests_total{result="error",scope="task_store"}`:                                      2,
	`orka_execution_event_list_requests_total{result="success",scope="session_store"}`:                                 1,
	`orka_execution_event_list_requests_total{result="success",scope="task_store"}`:                                    2,
	`orka_execution_event_redactions_total{event_type="ModelMessage",stream_type="task"}`:                              1,
	`orka_execution_event_truncations_total{event_type="ModelMessage",stream_type="task"}`:                             1,
	`orka_execution_events_appended_total{event_type="ModelMessage",stream_type="task"}`:                               2,
	`orka_execution_events_appended_total{event_type="PlanUpdated",stream_type="task"}`:                                2,
	`orka_execution_events_appended_total{event_type="TaskStarted",stream_type="task"}`:                                1,
	`orka_execution_events_appended_total{event_type="ToolCallCompleted",stream_type="task"}`:                          2,
}

// TestClientRecordsTheStoreMetricsSQLiteRecorded runs the workload through
// the client over SQLite, as production does, and requires exactly the series
// and counts the SQLite provider recorded before the move.
func TestClientRecordsTheStoreMetricsSQLiteRecorded(t *testing.T) {
	client := newEmbeddedClient(t)
	delta := storeMetricDelta(t, func() { runStoreMetricWorkload(t, client) })
	require.Equal(t, storeMetricsGolden, delta)
}

// TestProvidersRecordNoStoreMetrics calls SQLite and the test fake directly:
// neither records a store metric, so the client's series are not counted twice.
func TestProvidersRecordNoStoreMetrics(t *testing.T) {
	ctx := t.Context()
	secret := func(streamID string) *store.ExecutionEvent {
		event := workloadEvent(streamID, events.ExecutionEventTypeModelMessage)
		event.ContentText = "api_key=" + protectedSecrets()["OpenAI key"]
		return event
	}
	providers := map[string]store.ExecutionEventStore{
		"sqlite": openSQLite(t),
		"fake":   storetest.NewFakeExecutionEventStore(),
	}
	for name, provider := range providers {
		t.Run(name, func(t *testing.T) {
			delta := storeMetricDelta(t, func() {
				_, err := provider.AppendExecutionEvent(ctx, secret("a"))
				require.NoError(t, err)
				_, err = provider.AppendExecutionEvent(ctx, workloadEvent("a", "NotAnEventType"))
				require.ErrorIs(t, err, store.ErrValidation)
				deduplicating, ok := provider.(store.DeduplicatingExecutionEventStore)
				require.True(t, ok)
				for range 2 {
					_, _, err = deduplicating.AppendExecutionEventIfAbsent(ctx, secret("b"), "key")
					require.NoError(t, err)
				}
				_, err = provider.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "tenant"})
				require.NoError(t, err)
				_, err = provider.ListExecutionEvents(ctx, store.ExecutionEventFilter{EventTypes: []string{"Nope"}})
				require.ErrorIs(t, err, store.ErrValidation)
				_, _, err = provider.ListSessionExecutionEvents(ctx,
					store.SessionExecutionEventFilter{Namespace: "tenant", SessionName: "session"})
				require.NoError(t, err)
				_, _, err = provider.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{})
				require.ErrorIs(t, err, store.ErrValidation)
			})
			require.Empty(t, delta)
		})
	}
}

// unavailableEventProvider stands in for a provider in another process whose
// execution-event calls fail. It embeds SQLite only to serve the rest of the
// client's provider interfaces.
type unavailableEventProvider struct {
	*sqlite.Store
}

func (unavailableEventProvider) AppendExecutionEvent(context.Context, *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	return nil, store.ErrUnavailable
}

func (unavailableEventProvider) AppendExecutionEventIfAbsent(
	context.Context, *store.ExecutionEvent, string,
) (*store.ExecutionEvent, bool, error) {
	return nil, false, store.ErrUnavailable
}

func (unavailableEventProvider) AppendExecutionEventWithPlanIfAbsent(
	context.Context, *store.ExecutionEvent, string, *store.PlanState,
) (*store.ExecutionEvent, bool, error) {
	return nil, false, store.ErrUnavailable
}

func (unavailableEventProvider) ListExecutionEvents(context.Context, store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	return nil, store.ErrUnavailable
}

func (unavailableEventProvider) ListSessionExecutionEvents(
	context.Context, store.SessionExecutionEventFilter,
) ([]store.SessionExecutionEvent, int64, error) {
	return nil, 0, store.ErrUnavailable
}

// TestClientRecordsStoreMetricsForAnyProvider checks that a provider that
// records nothing in Orka's registry still reports appends and store reads,
// here as failures during an outage.
func TestClientRecordsStoreMetricsForAnyProvider(t *testing.T) {
	client, err := storageclient.NewEmbedded(unavailableEventProvider{Store: openSQLite(t)})
	require.NoError(t, err)
	ctx := t.Context()
	delta := storeMetricDelta(t, func() {
		_, err := client.AppendExecutionEvent(ctx, workloadEvent("a", events.ExecutionEventTypeTaskStarted))
		require.ErrorIs(t, err, store.ErrUnavailable)
		_, _, err = client.AppendExecutionEventIfAbsent(ctx, workloadEvent("a", events.ExecutionEventTypeToolCallCompleted), "key")
		require.ErrorIs(t, err, store.ErrUnavailable)
		_, _, err = client.AppendExecutionEventWithPlanIfAbsent(ctx, workloadEvent("a", events.ExecutionEventTypePlanUpdated), "key",
			&store.PlanState{Namespace: "tenant", TaskName: "a"})
		require.ErrorIs(t, err, store.ErrUnavailable)
		_, err = client.ListExecutionEvents(ctx, store.ExecutionEventFilter{Namespace: "tenant"})
		require.ErrorIs(t, err, store.ErrUnavailable)
		_, _, err = client.ListSessionExecutionEvents(ctx, store.SessionExecutionEventFilter{Namespace: "tenant", SessionName: "session"})
		require.ErrorIs(t, err, store.ErrUnavailable)
	})
	require.Equal(t, map[string]float64{
		`orka_execution_event_append_duration_seconds{event_type="PlanUpdated",result="error",stream_type="task"}`:       1,
		`orka_execution_event_append_duration_seconds{event_type="TaskStarted",result="error",stream_type="task"}`:       1,
		`orka_execution_event_append_duration_seconds{event_type="ToolCallCompleted",result="error",stream_type="task"}`: 1,
		`orka_execution_event_append_failures_total{event_type="PlanUpdated",stream_type="task"}`:                        1,
		`orka_execution_event_append_failures_total{event_type="TaskStarted",stream_type="task"}`:                        1,
		`orka_execution_event_append_failures_total{event_type="ToolCallCompleted",stream_type="task"}`:                  1,
		`orka_execution_event_list_duration_seconds{result="error",scope="session_store"}`:                               1,
		`orka_execution_event_list_duration_seconds{result="error",scope="task_store"}`:                                  1,
		`orka_execution_event_list_requests_total{result="error",scope="session_store"}`:                                 1,
		`orka_execution_event_list_requests_total{result="error",scope="task_store"}`:                                    1,
	}, delta)
}

// TestClientCountsAlreadySanitizedPayloadsOnce covers payloads sanitized
// before they reach the client: a worker that already redacted its payload,
// and Orka code that sanitized an event before appending it. Each persisted
// event counts once; a clean payload counts nothing.
func TestClientCountsAlreadySanitizedPayloadsOnce(t *testing.T) {
	client := newEmbeddedClient(t)
	redactions := `orka_execution_event_redactions_total{event_type="ModelMessage",stream_type="task"}`
	sanitizedTwice := workloadEvent("twice", events.ExecutionEventTypeModelMessage)
	sanitizedTwice.ContentText = "authorization: Bearer " + protectedSecrets()["bearer value"]
	require.NoError(t, store.SanitizeExecutionEventPayloadFields(sanitizedTwice))
	preRedacted := workloadEvent("worker", events.ExecutionEventTypeModelMessage)
	preRedacted.ContentText = "api key " + events.ExecutionEventRedactedValue
	cases := map[string]struct {
		event *store.ExecutionEvent
		want  float64
	}{
		"worker pre-redacted": {event: preRedacted, want: 1},
		"sanitized twice":     {event: sanitizedTwice, want: 1},
		"clean":               {event: workloadEvent("clean", events.ExecutionEventTypeModelMessage), want: 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			delta := storeMetricDelta(t, func() {
				_, err := client.AppendExecutionEvent(t.Context(), tc.event)
				require.NoError(t, err)
			})
			require.InDelta(t, tc.want, delta[redactions], 0)
		})
	}
}
