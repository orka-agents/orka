package usage_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func reportFixture() (store.UsageData, store.UsageFilter) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	return store.UsageData{
		Works: []store.UsageWorkRequest{{ID: "work", Namespace: "team", Kind: "issue", StartedAt: start}},
		Tasks: []store.UsageTask{{Namespace: "team", TaskUID: "task", TaskName: "task", WorkID: "work", Phase: "Succeeded", StartedAt: start}},
	}, store.UsageFilter{Namespaces: []string{"team"}, From: start, Until: start.Add(time.Hour), AsOf: start.Add(24 * time.Hour)}
}

func TestUsageTotalsStayWithinExactIntegerRange(t *testing.T) {
	const maximum = store.MaxUsageTokenCount
	for _, tc := range []struct {
		name                           string
		count                          int
		input, output, cached, written int64
		estimate, overflow             bool
	}{
		{name: "maximum is exact", count: 1, input: maximum},
		{name: "input sum", count: 2, input: maximum, overflow: true},
		{name: "output sum", count: 2, output: maximum, overflow: true},
		{name: "input plus output", count: 1, input: maximum, output: 1, overflow: true},
		{name: "cache reads", count: 2, cached: maximum, overflow: true},
		{name: "cache writes", count: 2, written: maximum, overflow: true},
		{name: "int64 input overflow", count: 1025, input: maximum, overflow: true},
		{name: "int64 combined overflow", count: 513, input: maximum, output: maximum, overflow: true},
		{name: "estimated sum", count: 2, input: maximum, estimate: true, overflow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, filter := reportFixture()
			for i := range tc.count {
				id := fmt.Sprint("call-", i)
				source := store.UsageSourceProvider
				if tc.estimate {
					source = store.UsageSourceEstimate
				}
				data.Observations = append(data.Observations, store.UsageObservation{Namespace: "team", TaskUID: "task", ID: id, CounterID: id,
					Scope: store.UsageScopeCall, Source: source, InputTokens: new(tc.input), OutputTokens: new(tc.output),
					CachedInputTokens: new(tc.cached), CacheWriteInputTokens: new(tc.written), Complete: true, ObservedAt: filter.From})
			}
			report, err := usage.Build(data, filter)
			if tc.overflow {
				require.ErrorIs(t, err, usage.ErrTotalsOutOfRange)
				require.Empty(t, report)
				_, err = usage.BuildPage(data, filter, usage.Page{Limit: 1}, "")
				require.ErrorIs(t, err, usage.ErrTotalsOutOfRange)
				data.Works = nil
				data.Tasks[0].WorkID = ""
				_, err = usage.BuildPage(data, filter, usage.Page{Limit: 1}, "unassociated")
				require.ErrorIs(t, err, usage.ErrTotalsOutOfRange)
				return
			}
			require.NoError(t, err)
			require.Equal(t, maximum, report.Summary.TotalTokens)
			require.Equal(t, maximum, report.Works[0].Tasks[0].Totals.TotalTokens)
		})
	}
}

func TestUsageSharedWorkAtExactLimitCountsOnce(t *testing.T) {
	data, filter := reportFixture()
	data.Works = append(data.Works, store.UsageWorkRequest{ID: "second", Namespace: "team", Kind: "issue", StartedAt: filter.From})
	data.Tasks[0].PRNumber, data.Tasks[0].Repository = 1, "org/repo"
	for _, work := range data.Works {
		data.Links = append(data.Links, store.UsagePRLink{Namespace: "team", WorkID: work.ID, Repository: "org/repo", Number: 1,
			Origin: store.UsagePRCreated, LinkedAt: filter.From})
	}
	data.Observations = []store.UsageObservation{{Namespace: "team", TaskUID: "task", ID: "call", CounterID: "call",
		Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, InputTokens: new(store.MaxUsageTokenCount),
		OutputTokens: new(int64(0)), Complete: true, ObservedAt: filter.From}}
	report, err := usage.Build(data, filter)
	require.NoError(t, err)
	require.Equal(t, store.MaxUsageTokenCount, report.Summary.TotalTokens)
	require.Len(t, report.Works, 2)
	require.Equal(t, 1, report.Summary.PRsOpened)
}

func TestUsageTotalsRejectOverflowAcrossWorksAndTeams(t *testing.T) {
	for _, namespace := range []string{"team", "other-team"} {
		t.Run(namespace, func(t *testing.T) {
			data, filter := reportFixture()
			if namespace != "team" {
				filter.Namespaces = append(filter.Namespaces, namespace)
			}
			data.Works = append(data.Works, store.UsageWorkRequest{ID: "second", Namespace: namespace, Kind: "issue", StartedAt: filter.From})
			data.Tasks = append(data.Tasks, store.UsageTask{Namespace: namespace, TaskUID: "second", WorkID: "second", Phase: "Succeeded", StartedAt: filter.From})
			for _, task := range data.Tasks {
				data.Observations = append(data.Observations, store.UsageObservation{Namespace: task.Namespace, TaskUID: task.TaskUID, ID: task.TaskUID,
					CounterID: task.TaskUID, Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, InputTokens: new(store.MaxUsageTokenCount),
					OutputTokens: new(int64(0)), Complete: true, ObservedAt: filter.From})
			}
			// Paging must check the complete selection, even when each work is valid.
			_, err := usage.BuildPage(data, filter, usage.Page{Limit: 1}, "")
			require.ErrorIs(t, err, usage.ErrTotalsOutOfRange)
		})
	}
}

func TestUsageLargeCounterHistoryDoesNotRejectSmallSelectedDeltas(t *testing.T) {
	data, filter := reportFixture()
	old := filter.From.Add(-24 * time.Hour)
	data.Works = append(data.Works, store.UsageWorkRequest{ID: "old", Namespace: "team", Kind: "issue", StartedAt: old})
	data.Tasks = append(data.Tasks, store.UsageTask{Namespace: "team", TaskUID: "old", WorkID: "old", Phase: "Succeeded", StartedAt: old})
	for i := range 2 {
		counter := fmt.Sprint("session-", i)
		for j, sample := range []struct {
			task  string
			count int64
			at    time.Time
		}{
			{"old", 0, old},
			{"old", store.MaxUsageTokenCount - 1, old.Add(time.Minute)},
			{"task", store.MaxUsageTokenCount, filter.From.Add(time.Minute)},
		} {
			data.Observations = append(data.Observations, store.UsageObservation{Namespace: "team", TaskUID: sample.task, ID: fmt.Sprintf("%s-%d", counter, j),
				CounterID: counter, Scope: store.UsageScopeSession, Source: store.UsageSourceAgent, InputTokens: new(sample.count),
				OutputTokens: new(int64(0)), Complete: true, ObservedAt: sample.at})
		}
	}
	report, err := usage.Build(data, filter)
	require.NoError(t, err)
	require.EqualValues(t, 2, report.Summary.TotalTokens)
	require.Len(t, report.Works, 1)
}

func TestUsageAggregateCacheAvailabilityRequiresEveryMeasurement(t *testing.T) {
	zero, one, two := int64(0), int64(1), int64(2)
	for _, tc := range []struct {
		name                          string
		first, second                 [2]*int64
		readReported, writtenReported bool
		read, written                 int64
	}{
		{name: "none"},
		{name: "reported zero", first: [2]*int64{&zero, &zero}, second: [2]*int64{&zero, &zero}, readReported: true, writtenReported: true},
		{name: "all reported", first: [2]*int64{&one, &two}, second: [2]*int64{&two, &one}, readReported: true, writtenReported: true, read: 3, written: 3},
		{name: "partial reads", first: [2]*int64{&one, &two}, second: [2]*int64{nil, &one}, writtenReported: true, read: 1, written: 3},
		{name: "partial writes", first: [2]*int64{&one, nil}, second: [2]*int64{&two, &one}, readReported: true, read: 3, written: 1},
		{name: "only later report", second: [2]*int64{&one, &two}, read: 1, written: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, filter := reportFixture()
			data.Tasks = append(data.Tasks, store.UsageTask{Namespace: "team", TaskUID: "second", WorkID: "work", Phase: "Succeeded", StartedAt: filter.From})
			for i, counts := range [][2]*int64{tc.first, tc.second} {
				id := fmt.Sprint("call-", i)
				data.Observations = append(data.Observations, store.UsageObservation{Namespace: "team", TaskUID: data.Tasks[i].TaskUID,
					ID: id, CounterID: id, Scope: store.UsageScopeCall, Source: store.UsageSourceProvider,
					InputTokens: new(int64(100)), OutputTokens: new(int64(0)), CachedInputTokens: counts[0], CacheWriteInputTokens: counts[1],
					Complete: true, ObservedAt: filter.From})
			}
			report, err := usage.Build(data, filter)
			require.NoError(t, err)
			require.Equal(t, "complete", report.Summary.Completeness, "input/output coverage is independent of optional cache breakdowns")
			require.Equal(t, tc.readReported, report.Summary.CachedUsageReported)
			require.Equal(t, tc.writtenReported, report.Summary.CacheWriteUsageReported)
			require.Equal(t, tc.read, report.Summary.CachedInputTokens, "retain the known subtotal even when aggregate coverage is unavailable")
			require.Equal(t, tc.written, report.Summary.CacheWriteInputTokens)
		})
	}
}
