package sqlite

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func usageWork(t *testing.T, s *Store, namespace string, number int64, at time.Time) string {
	t.Helper()
	id := store.UsageWorkID(namespace, "monitor-uid", "org/repo", "issue", number)
	require.NoError(t, s.RegisterUsageWork(context.Background(), store.UsageWorkRequest{ID: id, Namespace: namespace,
		MonitorName: "monitor", MonitorUID: "monitor-uid", Repository: "org/repo", Kind: "issue", Number: number, StartedAt: at}))
	return id
}

func usageTask(t *testing.T, s *Store, namespace, work, uid, phase string, at time.Time) {
	t.Helper()
	require.NoError(t, s.RegisterUsageTask(context.Background(), store.UsageTask{Namespace: namespace, TaskUID: uid,
		TaskName: uid, WorkID: work, Phase: phase, Runtime: "agent", StartedAt: at, PhaseObservedAt: at}))
}

func usageSample(t *testing.T, s *Store, namespace, task, id string, tokens int64, at time.Time) {
	t.Helper()
	require.NoError(t, s.RecordUsage(context.Background(), store.UsageObservation{Namespace: namespace, TaskUID: task, TaskName: task,
		ID: id, CounterID: id, Scope: store.UsageScopeCall, Source: store.UsageSourceProvider,
		Provider: "provider", Model: "served-model", InputTokens: new(tokens), OutputTokens: new(int64(0)),
		Status: store.UsageStatusCompleted, Complete: true, ObservedAt: at}))
}

func TestUsageRecordCacheBreakdownsRespectInclusiveInput(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		input, cached, write *int64
		valid                bool
	}{
		{name: "read exceeds zero", input: new(int64(0)), cached: new(int64(100))},
		{name: "write exceeds zero", input: new(int64(0)), write: new(int64(100))},
		{name: "read exceeds input", input: new(int64(100)), cached: new(int64(101))},
		{name: "write exceeds input", input: new(int64(100)), write: new(int64(101))},
		{name: "combined exceeds input", input: new(int64(100)), cached: new(int64(60)), write: new(int64(50))},
		{name: "combined exceeds maximum", input: new(store.MaxUsageTokenCount), cached: new(store.MaxUsageTokenCount), write: new(int64(1))},
		{name: "exact boundary", input: new(int64(100)), cached: new(int64(60)), write: new(int64(40)), valid: true},
		{name: "maximum boundary", input: new(store.MaxUsageTokenCount), cached: new(store.MaxUsageTokenCount - 1), write: new(int64(1)), valid: true},
		{name: "reported zero", input: new(int64(0)), cached: new(int64(0)), write: new(int64(0)), valid: true},
		{name: "unknown input", cached: new(int64(100)), write: new(int64(50)), valid: true},
		{name: "unknown breakdowns", input: new(int64(100)), valid: true},
		{name: "unknown write", input: new(int64(100)), cached: new(int64(100)), valid: true},
		{name: "unknown read", input: new(int64(100)), write: new(int64(100)), valid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			observation := store.UsageObservation{Namespace: "a", ID: "call", CounterID: "call", Scope: store.UsageScopeCall,
				Source: store.UsageSourceProvider, Status: store.UsageStatusCompleted, ObservedAt: time.Now().UTC(),
				InputTokens: tc.input, OutputTokens: new(int64(0)), CachedInputTokens: tc.cached, CacheWriteInputTokens: tc.write}
			err := s.RecordUsage(t.Context(), observation)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, store.ErrValidation)
			}
			data, err := s.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"a"}, AsOf: time.Now().UTC()})
			require.NoError(t, err)
			if tc.valid {
				require.Equal(t, []store.UsageObservation{observation}, data.Observations)
			} else {
				require.Empty(t, data.Observations, "invalid counts must not enter retained usage")
			}
		})
	}
}

func usagePR(t *testing.T, s *Store, namespace, work string, number int64, merged bool, at time.Time) {
	t.Helper()
	require.NoError(t, s.LinkUsagePullRequest(context.Background(), store.UsagePRLink{Namespace: namespace, WorkID: work,
		Repository: "org/repo", Number: number, Origin: store.UsagePRCreated, EvidenceID: "publication", LinkedAt: at}))
	pr := store.UsagePullRequest{Namespace: namespace, Repository: "org/repo", Number: number, GitHubID: fmt.Sprint(number),
		URL: fmt.Sprintf("https://github.com/org/repo/pull/%d", number), State: "open", HeadSHA: "head", ObservedAt: at}
	if merged {
		pr.State = "merged"
		pr.MergedAt = &at
	}
	require.NoError(t, s.RecordUsagePullRequest(context.Background(), pr))
}

func usageReport(t *testing.T, s *Store, teams []string, from, until, asOf time.Time) usage.Report {
	t.Helper()
	filter := store.UsageFilter{Namespaces: teams, From: from, Until: until, AsOf: asOf}
	data, err := s.LoadUsage(context.Background(), filter)
	require.NoError(t, err)
	return buildUsageReport(t, data, filter)
}

func buildUsageReport(t *testing.T, data store.UsageData, filter store.UsageFilter) usage.Report {
	t.Helper()
	report, err := usage.Build(data, filter)
	require.NoError(t, err)
	return report
}

func TestUsagePaymentsCohortIncludesUnsuccessfulWork(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-60 * 24 * time.Hour)
	for i := int64(1); i <= 20; i++ {
		work := usageWork(t, s, "payments", i, start)
		phase := "Succeeded"
		if i > 15 {
			phase = "Failed"
		}
		if i == 20 {
			phase = "Cancelled"
		}
		task := fmt.Sprintf("task-%d", i)
		usageTask(t, s, "payments", work, task, phase, start)
		// Follow-up usage can be later than the selected request-start month.
		usageSample(t, s, "payments", task, task+"/call", 600000, start.Add(40*24*time.Hour))
		if i <= 15 {
			usagePR(t, s, "payments", work, i+100, i <= 10, start.Add(41*24*time.Hour))
		}
	}
	report := usageReport(t, s, []string{"payments"}, start.Add(-time.Hour), start.Add(24*time.Hour), time.Now().UTC())
	require.Equal(t, 20, report.Summary.WorkRequests)
	require.Equal(t, int64(12000000), report.Summary.TotalTokens)
	require.Equal(t, 15, report.Summary.PRsOpened)
	require.Equal(t, 10, report.Summary.PRsMerged)
	require.Equal(t, 800000.0, *report.Summary.TokensPerPROpened)
	require.Equal(t, 1200000.0, *report.Summary.TokensPerPRMerged)
	require.Equal(t, "Price unavailable", report.Summary.ModelCost)
}

func TestUsageSharedWorkAndCombinedTeamsDeduplicatePRs(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	one := usageWork(t, s, "a", 1, start)
	two := usageWork(t, s, "a", 2, start)
	usageTask(t, s, "a", one, "task-one", "Failed", start)
	usageTask(t, s, "a", two, "task-two", "Succeeded", start)
	usageSample(t, s, "a", "task-one", "call-one", 100, start)
	usageSample(t, s, "a", "task-two", "call-two", 200, start)
	usagePR(t, s, "a", one, 10, true, start)
	usagePR(t, s, "a", two, 10, true, start)
	// A review after publication contributes once to the team and is shared
	// in the work details, rather than copied onto each produced PR.
	require.NoError(t, s.RegisterUsageTask(context.Background(), store.UsageTask{Namespace: "a", TaskUID: "review", TaskName: "review",
		Repository: "org/repo", PRNumber: 10, Phase: "Succeeded", StartedAt: start}))
	usageSample(t, s, "a", "review", "review-call", 300, start)
	three := usageWork(t, s, "b", 3, start)
	usageTask(t, s, "b", three, "task-three", "Succeeded", start)
	usageSample(t, s, "b", "task-three", "third-call", 100, start)
	usagePR(t, s, "b", three, 10, true, start)
	report := usageReport(t, s, []string{"a", "b"}, start.Add(-time.Second), start.Add(time.Second), time.Now().UTC())
	require.Equal(t, int64(700), report.Summary.TotalTokens)
	require.Equal(t, 1, report.Summary.PRsOpened)
	require.Equal(t, 1, report.Summary.PRsMerged)
	require.Equal(t, int64(600), report.Teams[0].Summary.TotalTokens)
	require.Equal(t, int64(100), report.Teams[1].Summary.TotalTokens)
	for _, work := range report.Works {
		if work.Namespace != "a" {
			continue
		}
		for _, task := range work.Tasks {
			if task.TaskUID == "review" {
				require.True(t, task.Shared)
			}
		}
	}
	filter := store.UsageFilter{Namespaces: []string{"a", "b"}, From: start.Add(-time.Second), Until: start.Add(time.Second), AsOf: report.Selection.AsOf}
	data, err := s.LoadUsage(t.Context(), filter)
	require.NoError(t, err)
	seen := map[string]bool{}
	for offset := range 3 {
		page, err := usage.BuildPage(data, filter, usage.Page{Limit: 1, Offset: offset}, "")
		require.NoError(t, err)
		require.Equal(t, report.Summary, page.Summary)
		require.Equal(t, report.Teams, page.Teams)
		require.Equal(t, 3, page.Page.Total)
		require.Len(t, page.Works, 1)
		require.Equal(t, report.Works[offset].Summary, page.Works[0].Summary)
		require.False(t, seen[page.Works[0].ID])
		seen[page.Works[0].ID] = true
		require.Empty(t, page.Works[0].Tasks)
		require.Empty(t, page.Works[0].PullRequests)
	}
}

func TestUsageJournalReplayCleanupAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	db, err := NewDB(path)
	require.NoError(t, err)
	s := NewStore(db, path)
	start := time.Now().UTC().Add(-time.Hour)
	work := usageWork(t, s, "a", 1, start)
	usageTask(t, s, "a", work, "task", "Cancelled", start)
	appendEvent := func(sequence int, input int64, typ, journal string) *store.ExecutionEvent {
		content, err := json.Marshal(map[string]any{
			"harnessV2":   map[string]any{"taskUID": "task", "promptID": "prompt", "taskAttempt": 1, "sequence": sequence},
			"inputTokens": input, "outputTokens": 0, "cachedInputTokens": 100, "model": "model", "journalKind": journal,
		})
		require.NoError(t, err)
		event := &store.ExecutionEvent{Namespace: "a", TaskName: "task", StreamType: "task", StreamID: "task", Type: typ,
			Severity: "info", Content: content, CreatedAt: start.Add(time.Duration(sequence) * time.Second)}
		_, _, err = s.AppendExecutionEventIfAbsent(context.Background(), event, fmt.Sprintf("%d/%s", sequence, journal))
		require.NoError(t, err)
		return event
	}
	appendEvent(1, 0, events.ExecutionEventTypeModelRequestStarted, "prompt_accepted")
	appendEvent(2, 1000, events.ExecutionEventTypeModelUsageUpdated, "")
	appendEvent(3, 1500, events.ExecutionEventTypeModelUsageUpdated, "")
	final := appendEvent(4, 1500, events.ExecutionEventTypeModelUsageUpdated, "terminal_usage")
	appendEvent(4, 0, events.ExecutionEventTypeModelRequestFailed, "prompt_terminal")
	get := func() usage.Report {
		return usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Second), time.Now().UTC())
	}
	require.Equal(t, int64(1500), get().Summary.TotalTokens)
	require.Equal(t, int64(100), get().Summary.CachedInputTokens)
	require.Nil(t, get().Summary.TokensPerPRMerged)
	require.NoError(t, s.DeleteExecutionEvents(context.Background(), "a", "task", "task"))
	require.NoError(t, db.Close())
	db, err = NewDB(path)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	s = NewStore(db, path)
	// The event journal itself was cleaned up. The accounting identity still
	// prevents replay of its final summary from charging the team twice.
	_, _, err = s.AppendExecutionEventIfAbsent(context.Background(), final, "4/terminal_usage")
	require.NoError(t, err)
	require.Equal(t, int64(1500), get().Summary.TotalTokens)
	require.Equal(t, "partial", get().Summary.Completeness)
}

func TestUsageEqualTimestampsPreserveJournalOrderAcrossTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-order.db")
	db, err := NewDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, path)
	start := time.Now().UTC().Add(-time.Hour)
	first := usageWork(t, s, "a", 1, start)
	second := usageWork(t, s, "a", 2, start.Add(time.Minute))
	usageTask(t, s, "a", first, "z-first", "Succeeded", start)
	usageTask(t, s, "a", second, "a-second", "Succeeded", start.Add(time.Minute))
	at := start.Add(2 * time.Minute)
	var journal []*store.ExecutionEvent
	for _, task := range []struct {
		uid    string
		counts []int64
	}{
		{"z-first", []int64{0, 10, 20, 30, 40, 50, 60, 70, 80, 90, 100}},
		{"a-second", []int64{100, 150}},
	} {
		for i, count := range task.counts {
			// Runtime sequence numbers restart for each prompt. Task IDs and
			// decimal sequence strings do not encode the cross-prompt order.
			content, err := json.Marshal(map[string]any{
				"harnessV2": map[string]any{"taskUID": task.uid, "taskAttempt": 1, "promptID": "prompt-" + task.uid,
					"runtimeSessionUID": "runtime-session", "runtimeSessionGeneration": 1, "sequence": i + 1},
				"usageScope": store.UsageScopeSession, "usageReported": true, "usageComplete": i == len(task.counts)-1,
				"inputTokens": count, "outputTokens": 0,
			})
			require.NoError(t, err)
			event := &store.ExecutionEvent{Namespace: "a", TaskName: task.uid, StreamType: "task", StreamID: task.uid,
				SessionName: "conversation", Type: events.ExecutionEventTypeModelUsageUpdated, Severity: "info", Content: content, CreatedAt: at}
			_, err = s.AppendExecutionEvent(t.Context(), event)
			require.NoError(t, err)
			journal = append(journal, event)
		}
	}
	check := func() {
		t.Helper()
		report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), at.Add(time.Minute))
		require.EqualValues(t, 150, report.Summary.TotalTokens)
		require.Equal(t, "complete", report.Summary.Completeness)
		require.Len(t, report.Works, 2)
		require.EqualValues(t, 100, report.Works[0].Summary.TotalTokens)
		require.EqualValues(t, 50, report.Works[1].Summary.TotalTokens)
		require.Zero(t, report.OtherWork[2].Totals.TotalTokens)
	}
	check()
	require.NoError(t, db.Close())
	db, err = NewDB(path)
	require.NoError(t, err)
	s = NewStore(db, path)
	check()
	for _, task := range []string{"z-first", "a-second"} {
		require.NoError(t, s.DeleteExecutionEvents(t.Context(), "a", "task", task))
	}
	for _, j := range slices.Backward(journal) {
		_, err := s.AppendExecutionEvent(t.Context(), j)
		require.NoError(t, err)
	}
	check()
}

func TestUsageMissingCountsAndReviewOnlyAreVisible(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	work := usageWork(t, s, "a", 1, start)
	usageTask(t, s, "a", work, "missing", "Failed", start)
	require.NoError(t, s.RegisterUsageTask(context.Background(), store.UsageTask{Namespace: "a", TaskUID: "review", TaskName: "review",
		Repository: "org/repo", PRNumber: 50, Phase: "Succeeded", StartedAt: start}))
	usageSample(t, s, "a", "review", "review-call", 10, start)
	usageSample(t, s, "a", "", "chat-call", 20, start)
	report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Second), time.Now().UTC())
	require.Equal(t, 1, report.Summary.MissingMeasurements)
	require.Equal(t, "unavailable", report.Summary.Completeness)
	require.Nil(t, report.Summary.TokensPerPROpened)
	require.Nil(t, report.Works[0].Tasks[0].Measurements[0].InputTokens)
	require.Equal(t, 0, report.Summary.PRsOpened)
	require.Equal(t, int64(10), report.OtherWork[0].Totals.TotalTokens)
	require.Equal(t, int64(20), report.OtherWork[2].Totals.TotalTokens)
}

func TestUsageInvalidACPCountsDoNotAbortJournal(t *testing.T) {
	for _, field := range []string{"inputTokens", "outputTokens", "cachedInputTokens", "cacheWriteInputTokens"} {
		for _, tc := range []struct {
			name  string
			count any
			valid bool
		}{
			{name: "maximum safe count", count: int64(1<<53 - 1), valid: true},
			{name: "outside safe range", count: int64(1 << 53)},
			{name: "maximum signed count", count: int64(math.MaxInt64)},
			{name: "maximum unsigned count", count: uint64(math.MaxUint64)},
			{name: "negative count", count: int64(-1)},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				s := setupTestStore(t)
				start := time.Now().UTC().Add(-time.Hour)
				work := usageWork(t, s, "a", 1, start)
				usageTask(t, s, "a", work, "task", "Succeeded", start)
				for i, typ := range []string{events.ExecutionEventTypeModelRequestStarted, events.ExecutionEventTypeModelUsageUpdated, events.ExecutionEventTypeModelRequestCompleted} {
					content := map[string]any{"harnessV2": map[string]any{"taskUID": "task", "promptID": "prompt", "taskAttempt": 1, "sequence": i + 1}}
					if typ == events.ExecutionEventTypeModelUsageUpdated {
						content[field], content["usageReported"] = tc.count, true
					}
					encoded, err := json.Marshal(content)
					require.NoError(t, err)
					event := &store.ExecutionEvent{Namespace: "a", TaskName: "task", StreamType: "task", StreamID: "task", Type: typ,
						Severity: "info", Content: encoded, CreatedAt: start.Add(time.Duration(i+1) * time.Second)}
					_, added, err := s.AppendExecutionEventIfAbsent(t.Context(), event, typ)
					require.NoError(t, err)
					require.True(t, added)
					_, added, err = s.AppendExecutionEventIfAbsent(t.Context(), event, typ)
					require.NoError(t, err)
					require.False(t, added)
				}
				journal, err := s.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "a", StreamType: "task", StreamID: "task"})
				require.NoError(t, err)
				require.Len(t, journal, 3)
				data, err := s.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"a"}, AsOf: time.Now().UTC()})
				require.NoError(t, err)
				if tc.valid {
					require.Len(t, data.Observations, 3)
				} else {
					require.Len(t, data.Observations, 2)
					report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
					require.Equal(t, "unavailable", report.Summary.Completeness)
					require.Zero(t, report.Summary.TotalTokens)
				}
			})
		}
	}
}

func TestUsageLaterMergeAndReadinessUseReportDate(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	work := usageWork(t, s, "a", 1, start)
	usageTask(t, s, "a", work, "task", "Succeeded", start)
	usageSample(t, s, "a", "task", "call", 30, start)
	usagePR(t, s, "a", work, 5, false, start)
	ready := store.UsagePullRequest{Namespace: "a", Repository: "org/repo", Number: 5, GitHubID: "5", HeadSHA: "head-a", State: "open", Ready: true, ObservedAt: start.Add(time.Minute)}
	require.NoError(t, s.RecordUsagePullRequest(context.Background(), ready))
	get := func(at time.Time) usage.Report {
		return usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Second), at)
	}
	require.Equal(t, 1, get(start.Add(2*time.Minute)).Summary.PRsReady)
	require.Equal(t, 0, get(start.Add(20*time.Minute)).Summary.PRsReady)
	usagePR(t, s, "a", work, 5, true, start.Add(30*time.Minute))
	usagePR(t, s, "a", work, 5, true, start.Add(30*time.Minute))
	require.Equal(t, 0, get(start.Add(20*time.Minute)).Summary.PRsMerged)
	require.Equal(t, 1, get(time.Now().UTC()).Summary.PRsMerged)
	require.Equal(t, 30.0, *get(time.Now().UTC()).Summary.TokensPerPRMerged)
}

func TestUsagePRRefreshStopsAfterConfirmedMerge(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	work := usageWork(t, s, "a", 1, start)
	usagePR(t, s, "a", work, 1, true, start)
	usagePR(t, s, "a", work, 2, false, start)
	usagePR(t, s, "a", work, 3, false, start)
	// A newer failed observation does not erase the earlier confirmed merge.
	require.NoError(t, s.RecordUsagePullRequest(t.Context(), store.UsagePullRequest{Namespace: "a", Repository: "org/repo",
		Number: 1, State: "unknown", ObservedAt: start.Add(time.Minute)}))
	// Closed PRs can reopen, so they still need refreshes.
	require.NoError(t, s.RecordUsagePullRequest(t.Context(), store.UsagePullRequest{Namespace: "a", Repository: "org/repo",
		Number: 3, GitHubID: "3", State: "closed", ClosedAt: &start, ObservedAt: start.Add(time.Minute)}))
	links, err := s.ListUsagePullRequestLinks(t.Context(), "a", "monitor-uid", start.Add(10*time.Minute), 20)
	require.NoError(t, err)
	numbers := make([]int64, 0, len(links))
	for _, link := range links {
		numbers = append(numbers, link.Number)
	}
	require.ElementsMatch(t, []int64{2, 3}, numbers)
	// Another team's observation does not establish this team's merge evidence.
	other := usageWork(t, s, "b", 1, start)
	usagePR(t, s, "b", other, 1, false, start)
	links, err = s.ListUsagePullRequestLinks(t.Context(), "b", "monitor-uid", start.Add(10*time.Minute), 20)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.EqualValues(t, 1, links[0].Number)
}

func TestUsageRetentionKeepsWholeActiveCohort(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-120 * 24 * time.Hour)
	oldWork := usageWork(t, s, "a", 1, start)
	activeWork := usageWork(t, s, "a", 2, start)
	usageTask(t, s, "a", oldWork, "expired", "Failed", start)
	usageTask(t, s, "a", activeWork, "active", "Succeeded", start)
	usageSample(t, s, "a", "expired", "old-call", 20, start)
	usageSample(t, s, "a", "active", "early-call", 10, start)
	usageSample(t, s, "a", "active", "late-call", 30, time.Now().UTC().Add(-time.Hour))
	require.NoError(t, s.PruneUsage(context.Background(), time.Now().UTC().Add(-90*24*time.Hour)))
	report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Second), time.Now().UTC())
	require.Equal(t, 1, report.Summary.WorkRequests)
	require.Equal(t, int64(40), report.Summary.TotalTokens)
	require.NotNil(t, report.RetainedSince)
}

func TestUsageOwnershipAndImmutableRecords(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	work := usageWork(t, s, "a", 1, start)
	err := s.RegisterUsageTask(context.Background(), store.UsageTask{Namespace: "b", TaskUID: "task", TaskName: "task", WorkID: work})
	require.Error(t, err)
	usageTask(t, s, "a", work, "task", "Succeeded", start)
	usageSample(t, s, "a", "task", "one", 10, start)
	usageSample(t, s, "a", "task", "one", 10, start)
	require.ErrorIs(t, s.RecordUsage(context.Background(), store.UsageObservation{Namespace: "a", ID: "one", TaskUID: "task",
		CounterID: "one", Scope: "call", Source: "provider", InputTokens: new(int64(50)), Status: "completed", ObservedAt: start}), store.ErrConflict)
	report := usageReport(t, s, []string{"b"}, start.Add(-time.Second), start.Add(time.Second), time.Now().UTC())
	require.Equal(t, 0, report.Summary.WorkRequests)
	require.Equal(t, int64(0), report.Summary.TotalTokens)
}

func TestUsageNamespaceOwnershipCannotBeReassigned(t *testing.T) {
	s := setupTestStore(t)
	work := store.UsageWorkID("team", "monitor", "org/repo", "issue", 1)
	require.NoError(t, s.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "team", NamespaceUID: "previous-namespace",
		MonitorName: "monitor", MonitorUID: "monitor", Repository: "org/repo", Kind: "issue", Number: 1}))
	parent := store.UsageTask{Namespace: "team", TaskUID: "parent", TaskName: "parent", WorkID: work}
	require.NoError(t, s.RegisterUsageTask(t.Context(), parent))
	parent.NamespaceUID = "current-namespace"
	require.ErrorIs(t, s.RegisterUsageTask(t.Context(), parent), store.ErrConflict)
	require.ErrorIs(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "team", NamespaceUID: "current-namespace",
		TaskUID: "child", TaskName: "child", ParentTaskUID: "parent"}), store.ErrConflict)
	require.ErrorIs(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "team", NamespaceUID: "current-namespace",
		TaskUID: "other", TaskName: "other", WorkID: work}), store.ErrConflict)
	require.ErrorIs(t, s.RecordUsage(t.Context(), store.UsageObservation{Namespace: "team", NamespaceUID: "current-namespace",
		TaskUID: "parent", ID: "call", CounterID: "call", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, ObservedAt: time.Now().UTC()}), store.ErrConflict)
}

func TestUsageRetentionSeparatesNamespaceGenerations(t *testing.T) {
	s := setupTestStore(t)
	old := time.Now().UTC().Add(-180 * 24 * time.Hour)
	for _, uid := range []string{"previous-namespace", "current-namespace"} {
		work := store.UsageWorkID("team", "monitor-"+uid, "org/repo", "issue", 1)
		require.NoError(t, s.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "team", NamespaceUID: uid,
			MonitorName: "monitor", MonitorUID: "monitor-" + uid, Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: old}))
		usageTask(t, s, "team", work, uid+"-task", "Succeeded", old)
		usageSample(t, s, "team", uid+"-task", uid+"-call", 100, old)
		require.NoError(t, s.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: "team", WorkID: work,
			Repository: "org/repo", Number: 12, Origin: store.UsagePRCreated, EvidenceID: "publication", LinkedAt: old}))
		pr := store.UsagePullRequest{Namespace: "team", NamespaceUID: uid, Repository: "org/repo", Number: 12, State: "open", ObservedAt: old}
		if uid == "previous-namespace" {
			pr.State, pr.GitHubID, pr.MergedAt = "merged", "PR_12", &old
		}
		require.NoError(t, s.RecordUsagePullRequest(t.Context(), pr))
	}
	// A new namespace's active review and open PR cannot keep old work alive,
	// nor can its PR history inherit the predecessor's terminal merge.
	require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "team", NamespaceUID: "current-namespace",
		TaskUID: "review", TaskName: "review", Repository: "org/repo", PRNumber: 12, Phase: "Running", StartedAt: old}))
	links, err := s.ListUsagePullRequestLinks(t.Context(), "team", "monitor-current-namespace", time.Now().UTC(), 20)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.NoError(t, s.PruneUsage(t.Context(), time.Now().UTC().Add(-90*24*time.Hour)))
	data, err := s.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"team"}})
	require.NoError(t, err)
	require.Len(t, data.Works, 1)
	require.Len(t, data.Tasks, 2)
	require.Len(t, data.Observations, 1)
	require.Len(t, data.PullRequests, 1)
	require.Equal(t, "current-namespace", data.Works[0].NamespaceUID)
	require.Equal(t, "current-namespace", data.PullRequests[0].NamespaceUID)
	require.Equal(t, "open", data.PullRequests[0].State)
}

func TestUsageRetentionExpiresStaleUnknownPRs(t *testing.T) {
	for _, tc := range []struct {
		name, state                         string
		recentLink, recentObservation, keep bool
	}{
		{name: "unobserved expired link"},
		{name: "stale unknown", state: "unknown"},
		{name: "unobserved recent link", recentLink: true, keep: true},
		{name: "unknown recent link", state: "unknown", recentLink: true, keep: true},
		{name: "recent unknown", state: "unknown", recentObservation: true, keep: true},
		{name: "known open", state: "open", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			now := time.Now().UTC()
			start := now.Add(-120 * 24 * time.Hour)
			work := usageWork(t, s, "a", 1, start)
			usageTask(t, s, "a", work, "task", "Failed", start)
			usageSample(t, s, "a", "task", "call", 10, start)
			linkedAt := start
			if tc.recentLink {
				linkedAt = now.Add(-time.Hour)
			}
			require.NoError(t, s.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: "a", WorkID: work,
				Repository: "org/repo", Number: 1, Origin: store.UsagePRCreated, EvidenceID: "publication", LinkedAt: linkedAt}))
			if tc.state != "" {
				observedAt := start
				if tc.recentObservation {
					observedAt = now.Add(-time.Hour)
				}
				require.NoError(t, s.RecordUsagePullRequest(t.Context(), store.UsagePullRequest{Namespace: "a", Repository: "org/repo",
					Number: 1, State: tc.state, ObservedAt: observedAt}))
			}
			require.NoError(t, s.PruneUsage(t.Context(), now.Add(-90*24*time.Hour)))
			data, err := s.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"a"}, AsOf: now})
			require.NoError(t, err)
			if tc.keep {
				require.Len(t, data.Works, 1)
				require.Len(t, data.Tasks, 1)
				require.Len(t, data.Links, 1)
				require.Len(t, data.Observations, 1)
			} else {
				require.Empty(t, data.Works)
				require.Empty(t, data.Tasks)
				require.Empty(t, data.Links)
				require.Empty(t, data.PullRequests)
				require.Empty(t, data.Observations)
			}
		})
	}
}

func TestUsageSessionDeltasSurviveRetentionOfEarlierTasks(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-120 * 24 * time.Hour)
	oldWork := usageWork(t, s, "a", 1, start)
	activeWork := usageWork(t, s, "a", 2, start)
	usageTask(t, s, "a", oldWork, "old", "Succeeded", start)
	usageTask(t, s, "a", activeWork, "active", "Cancelled", start)
	usagePR(t, s, "a", activeWork, 10, false, start)
	for i, sample := range []struct {
		task, attempt, status string
		input                 int64
		complete              bool
	}{
		{"old", "a1", store.UsageStatusStarted, 0, false},
		{"old", "a1", store.UsageStatusCompleted, 1000, true},
		{"active", "a2", store.UsageStatusStarted, 1000, false},
		{"active", "a2", store.UsageStatusCancelled, 1500, false},
	} {
		require.NoError(t, s.RecordUsage(t.Context(), store.UsageObservation{Namespace: "a", TaskUID: sample.task, TaskName: sample.task, SessionName: "conversation", AttemptID: sample.attempt,
			ID: fmt.Sprint(i), CounterID: "conversation", Scope: store.UsageScopeSession, Source: store.UsageSourceAgent, InputTokens: new(sample.input), OutputTokens: new(int64(0)),
			Status: sample.status, Complete: sample.complete, ObservedAt: start.Add(time.Duration(i) * time.Second)}))
	}
	// Empty ACP lifecycle measurements must not double the number of attempts.
	require.NoError(t, s.RecordUsage(t.Context(), store.UsageObservation{Namespace: "a", TaskUID: "active", ID: "lifecycle", CounterID: "prompt", AttemptID: "a2", Scope: store.UsageScopeAttempt,
		Source: store.UsageSourceAgent, Status: store.UsageStatusCancelled, ObservedAt: start.Add(5 * time.Second)}))
	get := func() usage.Report {
		return usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
	}
	require.EqualValues(t, 1500, get().Summary.TotalTokens)
	require.Equal(t, 2, get().Summary.Attempts)
	require.Equal(t, 1, get().Summary.PartialMeasurements)
	require.NoError(t, s.PruneUsage(t.Context(), time.Now().UTC().Add(-90*24*time.Hour)))
	report := get()
	require.Equal(t, 1, report.Summary.WorkRequests)
	require.EqualValues(t, 500, report.Summary.TotalTokens)
	require.Equal(t, 1, report.Summary.Attempts)
	require.Equal(t, store.UsageStatusCancelled, report.Works[0].Tasks[0].Measurements[0].Status)
}

func TestUsageRetentionKeepsIssueCohortWithRecentSharedReview(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-120 * 24 * time.Hour)
	work := usageWork(t, s, "a", 1, start)
	usageTask(t, s, "a", work, "implementation", "Succeeded", start)
	usageSample(t, s, "a", "implementation", "implementation-call", 1000, start)
	usagePR(t, s, "a", work, 10, true, start)
	recent := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "a", TaskUID: "review", TaskName: "review", Repository: "org/repo", PRNumber: 10, Phase: "Succeeded", StartedAt: recent, PhaseObservedAt: recent}))
	usageSample(t, s, "a", "review", "review-call", 500, recent)
	require.NoError(t, s.PruneUsage(t.Context(), time.Now().UTC().Add(-90*24*time.Hour)))
	report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
	require.Equal(t, 1, report.Summary.WorkRequests)
	require.EqualValues(t, 1500, report.Summary.TotalTokens)
}

func TestUsageUnknownSessionBaselineRemainsVisibleSeparately(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	work := usageWork(t, s, "a", 1, start)
	usageTask(t, s, "a", work, "task", "Succeeded", start)
	for i, input := range []int64{1000, 1500, 1500} {
		require.NoError(t, s.RecordUsage(t.Context(), store.UsageObservation{Namespace: "a", TaskUID: "task", ID: fmt.Sprint(i), CounterID: "session", Scope: store.UsageScopeSession,
			Source: store.UsageSourceAgent, InputTokens: new(input), OutputTokens: new(int64(0)), Complete: true, Status: store.UsageStatusCompleted, ObservedAt: start.Add(time.Duration(i) * time.Second)}))
	}
	report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
	require.EqualValues(t, 500, report.Summary.TotalTokens)
	require.EqualValues(t, 1000, report.OtherWork[2].Totals.TotalTokens)
	require.Equal(t, "partial", report.Summary.Completeness)
	require.Contains(t, report.Works[0].Tasks[0].Measurements[0].Gap, "baseline")
}

func TestUsageAsOfPhaseHistoryAndWorkTypeFilter(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-2 * time.Hour)
	work := usageWork(t, s, "a", 1, start)
	usageTask(t, s, "a", work, "task", "Running", start)
	earlyAsOf := time.Now().UTC()
	require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "a", TaskUID: "task", TaskName: "task", Phase: "Succeeded", PhaseObservedAt: start.Add(time.Hour)}))
	early := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), earlyAsOf)
	require.Equal(t, 1, early.Summary.UnfinishedWork)
	require.Equal(t, "Running", early.Works[0].Tasks[0].Phase)
	late := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
	require.Zero(t, late.Summary.UnfinishedWork)
	require.Equal(t, "Succeeded", late.Works[0].Tasks[0].Phase)
	require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "a", TaskUID: "review", TaskName: "review", Repository: "org/repo", PRNumber: 10, Phase: "Succeeded", StartedAt: start}))
	usageSample(t, s, "a", "review", "review-call", 200, start)
	usageSample(t, s, "a", "", "chat-call", 300, start)
	filter := store.UsageFilter{Namespaces: []string{"a"}, From: start.Add(-time.Second), Until: start.Add(time.Hour), AsOf: time.Now().UTC(), Kind: "pull_request"}
	data, err := s.LoadUsage(t.Context(), filter)
	require.NoError(t, err)
	report := buildUsageReport(t, data, filter)
	require.Zero(t, report.Summary.WorkRequests)
	require.EqualValues(t, 200, report.OtherWork[0].Totals.TotalTokens)
	require.Empty(t, report.OtherWork[1].Tasks)
	require.Empty(t, report.OtherWork[2].Tasks)
}

func TestUsageTaskSnapshotsFollowObservationTime(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			s := setupTestStore(t)
			start := time.Now().UTC().Add(-120 * 24 * time.Hour)
			work := usageWork(t, s, "a", 1, start)
			phases := []string{"Pending", "Running", "Succeeded"}
			unobservedAt := time.Now().UTC()
			checks := make([]struct {
				at    time.Time
				phase string
			}, 0, len(order))
			latest := 0
			for _, i := range order {
				snapshot := store.UsageTask{
					Namespace: "a", TaskUID: "task", TaskName: "task",
					Phase: phases[i], StartedAt: start, PhaseObservedAt: start.Add(time.Duration(i) * time.Minute),
				}
				if i == 0 {
					// The monitor's post-create snapshot supplies the work link.
					snapshot.WorkID = work
				}
				require.NoError(t, s.RegisterUsageTask(t.Context(), snapshot))
				latest = max(latest, i)
				checks = append(checks, struct {
					at    time.Time
					phase string
				}{time.Now().UTC(), phases[latest]})
			}
			unobserved := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), unobservedAt)
			require.Equal(t, "Unknown", unobserved.Works[0].Tasks[0].Phase)
			for _, check := range checks {
				report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), check.at)
				require.Len(t, report.Works, 1)
				require.Len(t, report.Works[0].Tasks, 1)
				require.Equal(t, check.phase, report.Works[0].Tasks[0].Phase)
				if check.phase == "Succeeded" {
					require.Zero(t, report.Summary.UnfinishedWork)
				} else {
					require.Equal(t, 1, report.Summary.UnfinishedWork)
				}
			}
			// A stale nonterminal snapshot must not pin the completed cohort.
			require.NoError(t, s.PruneUsage(t.Context(), time.Now().UTC().Add(-90*24*time.Hour)))
			report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
			require.Zero(t, report.Summary.WorkRequests)
		})
	}
}

func TestUsageLateSnapshotBetweenRepeatedPhaseObservations(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour)
	work := usageWork(t, s, "a", 1, start)
	reports := make([]usage.Report, 0, 4)
	for _, state := range []struct {
		phase  string
		minute int
	}{{"Pending", 0}, {"Running", 1}, {"Running", 3}, {"Finalizing", 2}} {
		require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{
			Namespace: "a", WorkID: work, TaskUID: "task", TaskName: "task", Phase: state.phase,
			StartedAt: start, PhaseObservedAt: start.Add(time.Duration(state.minute) * time.Minute),
		}))
		reports = append(reports, usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC()))
	}
	for i, phase := range []string{"Pending", "Running", "Running", "Running"} {
		report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), reports[i].Selection.AsOf)
		require.Equal(t, phase, report.Works[0].Tasks[0].Phase)
		require.Equal(t, reports[i], report)
	}
}

func TestUsageTaskPhaseTimestampTiesRetainTerminalState(t *testing.T) {
	for _, terminal := range []string{"Succeeded", "Failed", "Cancelled"} {
		for _, phases := range [][]string{{"Pending", "Running", "Finalizing", terminal}, {terminal, "Finalizing", "Running", "Pending"}} {
			for _, fractional := range []time.Duration{0, 800 * time.Millisecond} {
				t.Run(fmt.Sprint(phases)+"/"+fractional.String(), func(t *testing.T) {
					s := setupTestStore(t)
					start := time.Now().UTC().Add(-120 * 24 * time.Hour).Truncate(time.Second)
					work := usageWork(t, s, "a", 1, start)
					for _, phase := range phases {
						observedAt := start
						if phase != terminal {
							observedAt = observedAt.Add(fractional)
						}
						require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{
							Namespace: "a", WorkID: work, TaskUID: "task", TaskName: "task", Phase: phase,
							StartedAt: start, PhaseObservedAt: observedAt,
						}))
					}
					report := usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
					require.Equal(t, terminal, report.Works[0].Tasks[0].Phase)
					require.Zero(t, report.Summary.UnfinishedWork)
					require.NoError(t, s.PruneUsage(t.Context(), time.Now().UTC().Add(-90*24*time.Hour)))
					report = usageReport(t, s, []string{"a"}, start.Add(-time.Second), start.Add(time.Hour), time.Now().UTC())
					require.Zero(t, report.Summary.WorkRequests)
				})
			}
		}
	}
}

func TestUsageRetriesWithinTheSameSecond(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states []store.UsageTaskPhase
		phase  string
	}{
		{"first retry pending", []store.UsageTaskPhase{{Phase: "Running", Attempt: 1}, {Phase: "Pending", Attempt: 1}, {Phase: "Running", Attempt: 1}}, "Pending"},
		{"retry started", []store.UsageTaskPhase{{Phase: "Pending", Attempt: 1}, {Phase: "Running", Attempt: 2}, {Phase: "Pending", Attempt: 1}}, "Running"},
		{"second retry pending", []store.UsageTaskPhase{{Phase: "Pending", Attempt: 2}, {Phase: "Running", Attempt: 2}, {Phase: "Pending", Attempt: 1}}, "Pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			work := usageWork(t, s, "a", 1, start)
			for _, state := range tc.states {
				require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{
					Namespace: "a", WorkID: work, TaskUID: "task", TaskName: "task", Phase: state.Phase,
					StartedAt: start, PhaseObservedAt: start.Add(time.Minute), PhaseAttempt: state.Attempt,
				}))
			}
			report := usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), time.Now().UTC())
			require.Equal(t, tc.phase, report.Works[0].Tasks[0].Phase)
		})
	}
}

func TestUsageLatePhaseDoesNotChangeEarlierReport(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after store.UsageTaskPhase
	}{
		{"initial running", store.UsageTaskPhase{Phase: "Pending"}, store.UsageTaskPhase{Phase: "Running", Attempt: 1}},
		{"retry pending", store.UsageTaskPhase{Phase: "Running", Attempt: 1}, store.UsageTaskPhase{Phase: "Pending", Attempt: 1}},
		{"retry running", store.UsageTaskPhase{Phase: "Pending", Attempt: 1}, store.UsageTaskPhase{Phase: "Running", Attempt: 2}},
		{"finalizing", store.UsageTaskPhase{Phase: "Running", Attempt: 2}, store.UsageTaskPhase{Phase: "Finalizing", Attempt: 2}},
		{"succeeded", store.UsageTaskPhase{Phase: "Finalizing", Attempt: 2}, store.UsageTaskPhase{Phase: "Succeeded", Attempt: 2}},
		{"failed", store.UsageTaskPhase{Phase: "Running", Attempt: 2}, store.UsageTaskPhase{Phase: "Failed", Attempt: 2}},
		{"cancelled", store.UsageTaskPhase{Phase: "Running", Attempt: 2}, store.UsageTaskPhase{Phase: "Cancelled", Attempt: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupTestStore(t)
			start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			work := usageWork(t, s, "a", 1, start)
			register := func(state store.UsageTaskPhase) {
				t.Helper()
				require.NoError(t, s.RegisterUsageTask(t.Context(), store.UsageTask{
					Namespace: "a", WorkID: work, TaskUID: "task", TaskName: "task", Phase: state.Phase,
					StartedAt: start, PhaseObservedAt: start.Add(time.Minute), PhaseAttempt: state.Attempt,
				}))
			}
			register(tc.before)
			asOf := time.Now().UTC()
			original := usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), asOf)
			require.Equal(t, tc.before.Phase, original.Works[0].Tasks[0].Phase)
			// The next watch snapshot carries a rounded Kubernetes timestamp
			// before asOf, although it reaches the store after this report.
			register(tc.after)
			replay := usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), asOf)
			require.Equal(t, original, replay)
			data, err := s.LoadUsage(t.Context(), original.Selection)
			require.NoError(t, err)
			require.Len(t, data.Tasks[0].PhaseHistory, 2)
			recordedAt := data.Tasks[0].PhaseHistory[1].RecordedAt
			require.True(t, recordedAt.After(asOf))
			require.Equal(t, tc.before.Phase, usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), recordedAt.Add(-time.Nanosecond)).Works[0].Tasks[0].Phase)
			require.Equal(t, tc.after.Phase, usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), recordedAt).Works[0].Tasks[0].Phase)
			currentAt := time.Now().UTC()
			current := usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), currentAt)
			require.Equal(t, tc.after.Phase, current.Works[0].Tasks[0].Phase)
			// Repeated observations preserve the first recorded time, and a
			// late stale snapshot cannot regress either report.
			register(tc.after)
			register(tc.before)
			require.Equal(t, original, usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), asOf))
			require.Equal(t, current, usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), currentAt))
			require.Equal(t, tc.after.Phase, usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), time.Now().UTC()).Works[0].Tasks[0].Phase)
		})
	}
}

func TestUsageLegacyPhaseHistoryKeepsItsOriginalVisibility(t *testing.T) {
	s := setupTestStore(t)
	start := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	work := usageWork(t, s, "a", 1, start)
	legacy := store.UsageTask{Namespace: "a", WorkID: work, Repository: "org/repo", TaskUID: "task", TaskName: "task", Phase: "Running", StartedAt: start,
		PhaseHistory: []store.UsageTaskPhase{{Phase: "Running", ObservedAt: start, Attempt: 1}}}
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "recordedAt")
	_, err = s.db.ExecContext(t.Context(), `INSERT INTO usage_tasks(namespace, task_uid, task_name, started_at, data) VALUES (?, ?, ?, ?, ?)`,
		legacy.Namespace, legacy.TaskUID, legacy.TaskName, start.UnixNano(), string(encoded))
	require.NoError(t, err)
	asOf := start.Add(time.Minute)
	original := usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), asOf)
	require.Equal(t, "Running", original.Works[0].Tasks[0].Phase)
	// Refreshing a row created before RecordedAt was introduced must not
	// move its existing phase's visibility forward to the upgrade time.
	legacy.PhaseObservedAt, legacy.PhaseAttempt = start, 1
	require.NoError(t, s.RegisterUsageTask(t.Context(), legacy))
	require.Equal(t, original, usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), asOf))
	// New transitions on the same legacy Task follow the new visibility rule.
	legacy.Phase, legacy.PhaseObservedAt = "Succeeded", start.Add(30*time.Second)
	require.NoError(t, s.RegisterUsageTask(t.Context(), legacy))
	require.Equal(t, original, usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), asOf))
	require.Equal(t, "Succeeded", usageReport(t, s, []string{"a"}, start, start.Add(time.Hour), time.Now().UTC()).Works[0].Tasks[0].Phase)
}
