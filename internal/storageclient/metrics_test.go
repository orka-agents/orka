package storageclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
)

var storeMetricNames = map[string]bool{
	"orka_execution_events_appended_total":         true,
	"orka_execution_event_append_failures_total":   true,
	"orka_execution_event_append_duration_seconds": true,
	"orka_execution_event_list_requests_total":     true,
	"orka_execution_event_list_duration_seconds":   true,
	"orka_execution_event_redactions_total":        true,
	"orka_execution_event_truncations_total":       true,
}

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
	require.NoError(t, client.WithTaskDataAccess(ctx, "tenant", "d", func(tx context.Context) error {
		if _, err := client.AppendExecutionEvent(tx, workloadEvent("d", events.ExecutionEventTypeModelMessage)); err != nil {
			return err
		}
		_, err := client.ListExecutionEvents(tx, store.ExecutionEventFilter{Namespace: "tenant", StreamID: "d"})
		return err
	}))

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

	_, err = client.AppendExecutionEvent(ctx, nil)
	require.ErrorIs(t, err, store.ErrValidation)
	report, err := json.Marshal(map[string]any{"usage": map[string]any{"id": "bad", "counterID": "c", "status": "unknown"}})
	require.NoError(t, err)
	invalidReport := workloadEvent("e", events.ExecutionEventTypeModelUsageUpdated)
	invalidReport.Content = report
	_, err = client.AppendExecutionEvent(store.WithUsageAttribution(ctx, store.UsageAttribution{TaskUID: "task-uid"}), invalidReport)
	require.ErrorIs(t, err, store.ErrValidation)

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

func TestClientRecordsTheStoreMetricsSQLiteRecorded(t *testing.T) {
	client := newEmbeddedClient(t)
	delta := storeMetricDelta(t, func() { runStoreMetricWorkload(t, client) })
	require.Equal(t, storeMetricsGolden, delta)
}

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

func TestClientRecordsStoreMetricsForAnyProvider(t *testing.T) {
	client := storageclienttest.NewEmbedded(t, unavailableEventProvider{Store: openSQLite(t)})
	delta := storeMetricDelta(t, func() {
		for _, write := range []func(context.Context) error{
			func(ctx context.Context) error {
				_, err := client.AppendExecutionEvent(ctx, workloadEvent("a", events.ExecutionEventTypeTaskStarted))
				return err
			},
			func(ctx context.Context) error {
				_, _, err := client.AppendExecutionEventIfAbsent(ctx, workloadEvent("a", events.ExecutionEventTypeToolCallCompleted), "key")
				return err
			},
			func(ctx context.Context) error {
				_, _, err := client.AppendExecutionEventWithPlanIfAbsent(ctx, workloadEvent("a", events.ExecutionEventTypePlanUpdated), "key",
					&store.PlanState{Namespace: "tenant", TaskName: "a"})
				return err
			},
		} {
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			err := write(ctx)
			cancel()
			require.ErrorIs(t, err, store.ErrUnavailable)
		}
		_, err := client.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "tenant"})
		require.ErrorIs(t, err, store.ErrUnavailable)
		_, _, err = client.ListSessionExecutionEvents(t.Context(), store.SessionExecutionEventFilter{Namespace: "tenant", SessionName: "session"})
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

func TestClientRecordsOneAppendAttemptPerGroupedEvent(t *testing.T) {
	client := newEmbeddedClient(t)
	ctx := t.Context()
	group := func(entries ...*store.ExecutionEvent) []store.ExecutionEventAppend {
		appends := make([]store.ExecutionEventAppend, len(entries))
		for i, event := range entries {
			appends[i] = store.ExecutionEventAppend{Event: event, DedupeKey: fmt.Sprintf("group-%d", i)}
		}
		return appends
	}

	appended := storeMetricDelta(t, func() {
		_, err := client.AppendExecutionEventsIfAbsent(ctx, group(
			workloadEvent("g", events.ExecutionEventTypeModelMessage),
			workloadEvent("g", events.ExecutionEventTypeToolCallCompleted),
		))
		require.NoError(t, err)
	})
	require.Equal(t, map[string]float64{
		`orka_execution_event_append_duration_seconds{event_type="ModelMessage",result="success",stream_type="task"}`:      1,
		`orka_execution_event_append_duration_seconds{event_type="ToolCallCompleted",result="success",stream_type="task"}`: 1,
		`orka_execution_events_appended_total{event_type="ModelMessage",stream_type="task"}`:                               1,
		`orka_execution_events_appended_total{event_type="ToolCallCompleted",stream_type="task"}`:                          1,
	}, appended)

	refusedByClient := storeMetricDelta(t, func() {
		_, err := client.AppendExecutionEventsIfAbsent(ctx, group(workloadEvent("h", events.ExecutionEventTypeModelMessage), nil))
		require.ErrorIs(t, err, store.ErrValidation)
	})
	require.Equal(t, map[string]float64{
		`orka_execution_event_append_duration_seconds{event_type="ModelMessage",result="error",stream_type="task"}`: 1,
		`orka_execution_event_append_duration_seconds{event_type="unknown",result="error",stream_type="unknown"}`:   1,
		`orka_execution_event_append_failures_total{event_type="ModelMessage",stream_type="task"}`:                  1,
		`orka_execution_event_append_failures_total{event_type="unknown",stream_type="unknown"}`:                    1,
	}, refusedByClient)

	refusedByProvider := storeMetricDelta(t, func() {
		_, err := client.AppendExecutionEventsIfAbsent(ctx, group(
			workloadEvent("i", events.ExecutionEventTypeModelMessage),
			workloadEvent("i", "NotAnEventType"),
		))
		require.ErrorIs(t, err, store.ErrValidation)
	})
	require.Equal(t, map[string]float64{
		`orka_execution_event_append_duration_seconds{event_type="ModelMessage",result="error",stream_type="task"}`: 1,
		`orka_execution_event_append_duration_seconds{event_type="unknown",result="error",stream_type="task"}`:      1,
		`orka_execution_event_append_failures_total{event_type="ModelMessage",stream_type="task"}`:                  1,
		`orka_execution_event_append_failures_total{event_type="unknown",stream_type="task"}`:                       1,
	}, refusedByProvider)

	replayed := storeMetricDelta(t, func() {
		results, err := client.AppendExecutionEventsIfAbsent(ctx, group(
			workloadEvent("g", events.ExecutionEventTypeModelMessage),
			workloadEvent("g", events.ExecutionEventTypeToolCallCompleted),
		))
		require.NoError(t, err)
		require.False(t, results[0].Appended)
		require.False(t, results[1].Appended)
	})
	require.Equal(t, appended, replayed, "a deduplicated replay counts as an appended event")

	invalidKey := storeMetricDelta(t, func() {
		_, err := client.AppendExecutionEventsIfAbsent(ctx, []store.ExecutionEventAppend{
			{Event: workloadEvent("j", events.ExecutionEventTypeModelMessage), DedupeKey: "key"},
			{Event: workloadEvent("j", events.ExecutionEventTypeToolCallCompleted), DedupeKey: " "},
		})
		require.ErrorIs(t, err, store.ErrValidation)
	})
	require.Empty(t, invalidKey, "an invalid deduplication key is not an append attempt")
}
