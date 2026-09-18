package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func TestUsageLoadSelectsCohortAndPreservesCounterHistory(t *testing.T) {
	s := setupTestStore(t)
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	start := old.Add(30 * 24 * time.Hour)
	previous := usageWork(t, s, "a", 1, old)
	work := usageWork(t, s, "a", 2, start)
	usageTask(t, s, "a", previous, "previous", "Succeeded", old)
	usageTask(t, s, "a", work, "selected", "Succeeded", start.Add(time.Minute))
	for i, sample := range []struct {
		task   string
		count  int64
		at     time.Time
		status string
	}{
		{"previous", 0, old, store.UsageStatusStarted},
		{"previous", 100, old.Add(time.Minute), store.UsageStatusCompleted},
		{"selected", 100, start.Add(time.Minute), store.UsageStatusStarted},
		{"selected", 175, start.Add(2 * time.Minute), store.UsageStatusCompleted},
	} {
		require.NoError(t, s.RecordUsage(t.Context(), store.UsageObservation{Namespace: "a", TaskUID: sample.task,
			ID: fmt.Sprint("session-", i), CounterID: "conversation", Scope: store.UsageScopeSession, Source: store.UsageSourceAgent,
			InputTokens: new(sample.count), OutputTokens: new(int64(0)), Model: "session-model", Status: sample.status,
			Complete: sample.status == store.UsageStatusCompleted, ObservedAt: sample.at}))
	}
	// A model used by a delegated Task selects every member of the work.
	require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "a", TaskUID: "delegated", TaskName: "delegated",
		ParentTaskUID: "selected", Phase: "Succeeded", StartedAt: start.Add(3 * time.Minute)}))
	usageSample(t, s, "a", "delegated", "delegated-call", 20, start.Add(4*time.Minute))
	usagePR(t, s, "a", work, 12, true, start.Add(24*time.Hour))
	// A shared review starts outside the selected activity period, but belongs
	// to the original issue cohort through its verified publication link.
	require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "a", TaskUID: "shared", TaskName: "shared",
		Repository: "org/repo", PRNumber: 12, Phase: "Succeeded", StartedAt: start.Add(4 * time.Hour)}))
	usageSample(t, s, "a", "shared", "shared-call", 35, start.Add(5*time.Hour))
	require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "a", TaskUID: "review", TaskName: "review",
		Repository: "org/repo", PRNumber: 99, Phase: "Succeeded", StartedAt: start.Add(15 * time.Minute)}))
	usageSample(t, s, "a", "review", "review-call", 25, start.Add(16*time.Minute))
	usageSample(t, s, "a", "", "chat-call", 15, start.Add(10*time.Minute))
	otherTeam := usageWork(t, s, "b", 2, start)
	usageTask(t, s, "b", otherTeam, "selected", "Succeeded", start)
	usageSample(t, s, "b", "selected", "other-team", 1000, start)
	// Unrelated history must not be decoded for a small date/model selection.
	require.NoError(t, s.WithTaskDataTransaction(t.Context(), func(ctx context.Context) error {
		for i := range 2000 {
			id := fmt.Sprint("unrelated-", i)
			if err := s.RecordUsage(ctx, store.UsageObservation{Namespace: "a", TaskUID: "previous", ID: id, CounterID: id,
				Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, Model: "old-model", InputTokens: new(int64(10)),
				OutputTokens: new(int64(0)), Status: store.UsageStatusCompleted, Complete: true, ObservedAt: old}); err != nil {
				return err
			}
		}
		return nil
	}))
	filter := store.UsageFilter{Namespaces: []string{"a"}, From: start, Until: start.Add(time.Hour), AsOf: start.Add(2 * 24 * time.Hour)}
	all, err := s.LoadUsage(t.Context(), store.UsageFilter{Namespaces: filter.Namespaces, AsOf: filter.AsOf})
	require.NoError(t, err)
	require.Len(t, all.Observations, 2008)
	for _, tc := range []struct {
		name, repository, kind, model string
	}{
		{name: "date"},
		{name: "repository", repository: "ORG/REPO"},
		{name: "issue", kind: "issue"},
		{name: "review", kind: "pull_request"},
		{name: "session model", model: "session-model"},
		{name: "delegated model", model: "served-model"},
		{name: "absent model", model: "missing-model"},
		{name: "absent repository", repository: "other/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := filter
			selected.Repository, selected.Kind, selected.Model = tc.repository, tc.kind, tc.model
			data, err := s.LoadUsage(t.Context(), selected)
			require.NoError(t, err)
			require.LessOrEqual(t, len(data.Observations), 8)
			require.Equal(t, usage.Build(all, selected), usage.Build(data, selected))
			if tc.name == "date" {
				require.Len(t, data.Observations, 8)
				report := usage.Build(data, selected)
				require.EqualValues(t, 130, report.Summary.TotalTokens)
				require.Equal(t, 1, report.Summary.PRsMerged)
				require.EqualValues(t, 25, report.OtherWork[0].Totals.TotalTokens)
				require.EqualValues(t, 15, report.OtherWork[2].Totals.TotalTokens)
			}
		})
	}
}
