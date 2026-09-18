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
		name, repository, kind, model, workID string
	}{
		{name: "date"},
		{name: "repository", repository: "ORG/REPO"},
		{name: "issue", kind: "issue"},
		{name: "review", kind: "pull_request"},
		{name: "session model", model: "session-model"},
		{name: "delegated model", model: "served-model"},
		{name: "absent model", model: "missing-model"},
		{name: "absent repository", repository: "other/repo"},
		{name: "requested work", workID: work},
		{name: "missing work", workID: "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := filter
			selected.Repository, selected.Kind, selected.Model = tc.repository, tc.kind, tc.model
			selected.WorkID = tc.workID
			if selected.WorkID != "" {
				// Detail requests default to all retained history. Earlier work
				// metadata is needed for baselines, but is not itself selected.
				selected.From = time.Unix(0, 0).UTC()
				selected.MaxRecords = 32
			}
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

func TestUsageLoadRejectsOversizedSelectionAcrossNamespaces(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	for _, team := range []string{"a", "b"} {
		work := usageWork(t, s, team, 1, start)
		usageTask(t, s, team, work, "task", "Succeeded", start)
		usageSample(t, s, team, "task", "call", 100, start)
	}
	filter := store.UsageFilter{Namespaces: []string{"a", "b"}, From: start, Until: start.Add(time.Second), AsOf: time.Now().UTC(), MaxRecords: 6}
	data, err := s.LoadUsage(t.Context(), filter)
	require.NoError(t, err)
	require.EqualValues(t, 200, usage.Build(data, filter).Summary.TotalTokens)
	filter.MaxRecords = 5
	data, err = s.LoadUsage(t.Context(), filter)
	require.ErrorIs(t, err, store.ErrUsageSelectionTooLarge)
	require.Empty(t, data, "an oversized selection must not return partial data")
}

func TestUsageNamespaceFilterPreservesOnlyOwnedCounterHistory(t *testing.T) {
	s := setupTestStore(t)
	old := time.Now().UTC().Add(-60 * 24 * time.Hour)
	start := old.Add(30 * 24 * time.Hour)
	for _, uid := range []string{"previous-namespace", "current-namespace", ""} {
		for number := int64(1); number <= 2; number++ {
			at := old
			if number == 2 {
				at = start
			}
			work := store.UsageWorkID("team", "monitor-"+uid, "org/repo", "issue", number)
			require.NoError(t, s.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "team", NamespaceUID: uid,
				MonitorName: "monitor", MonitorUID: "monitor-" + uid, Repository: "org/repo", Kind: "issue", Number: number, StartedAt: at}))
			task := fmt.Sprintf("%s-task-%d", uid, number)
			usageTask(t, s, "team", work, task, "Succeeded", at)
			// The same runtime counter name exists in each namespace generation.
			for i, count := range []int64{100 * (number - 1), 100 * number} {
				status := store.UsageStatusStarted
				if i == 1 {
					status = store.UsageStatusCompleted
				}
				require.NoError(t, s.RecordUsage(t.Context(), store.UsageObservation{Namespace: "team", TaskUID: task,
					ID: fmt.Sprintf("%s-%d", task, i), CounterID: "conversation", Scope: store.UsageScopeSession, Source: store.UsageSourceAgent,
					InputTokens: new(count), OutputTokens: new(int64(0)), Model: "session-model", Status: status,
					Complete: i == 1, ObservedAt: at.Add(time.Duration(i) * time.Minute)}))
			}
			if number == 2 {
				require.NoError(t, s.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: "team", WorkID: work, Repository: "org/repo",
					Number: 12, Origin: store.UsagePRCreated, EvidenceID: "publication", LinkedAt: start}))
				require.NoError(t, s.RecordUsagePullRequest(t.Context(), store.UsagePullRequest{Namespace: "team", NamespaceUID: uid,
					Repository: "org/repo", Number: 12, State: "open", ObservedAt: start}))
			}
		}
		require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "team", NamespaceUID: uid,
			TaskUID: uid + "-review", TaskName: "review", Repository: "org/repo", PRNumber: 12, Phase: "Succeeded", StartedAt: start.Add(4 * time.Hour)}))
		usageSample(t, s, "team", uid+"-review", uid+"-review-call", 20, start.Add(5*time.Hour))
	}
	filter := store.UsageFilter{Namespaces: []string{"team"}, NamespaceUIDs: map[string]string{"team": "current-namespace"},
		From: start, Until: start.Add(time.Hour), AsOf: start.Add(24 * time.Hour), Model: "session-model"}
	data, err := s.LoadUsage(t.Context(), filter)
	require.NoError(t, err)
	// The earlier Task is retained as a baseline; the shared review remains in
	// the cohort even though it uses another model and starts outside the range.
	require.Len(t, data.Works, 2)
	require.Len(t, data.Tasks, 3)
	require.Len(t, data.Observations, 5)
	require.Len(t, data.Links, 1)
	require.Len(t, data.PullRequests, 1)
	for _, observation := range data.Observations {
		require.Equal(t, "current-namespace", observation.NamespaceUID)
	}
	report := usage.Build(data, filter)
	require.EqualValues(t, 120, report.Summary.TotalTokens)
	filter.NamespaceUIDs = map[string]string{}
	_, err = s.LoadUsage(t.Context(), filter)
	require.ErrorIs(t, err, store.ErrValidation)
}
