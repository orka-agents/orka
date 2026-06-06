package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/sozercan/orka/internal/store"
)

func TestRepositoryMonitorStoreCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	monitor := &store.RepositoryMonitorRecord{
		Namespace:  "demo",
		Name:       "orka",
		UID:        "uid-1",
		RepoURL:    "https://github.com/sozercan/orka",
		Owner:      "sozercan",
		Repository: "orka",
		Branch:     "main",
		Generation: 1,
	}
	if err := s.UpsertRepositoryMonitor(ctx, monitor); err != nil {
		t.Fatalf("UpsertRepositoryMonitor() error = %v", err)
	}
	monitor.Generation = 2
	monitor.Branch = "develop"
	if err := s.UpsertRepositoryMonitor(ctx, monitor); err != nil {
		t.Fatalf("UpsertRepositoryMonitor(update) error = %v", err)
	}

	got, err := s.GetRepositoryMonitor(ctx, "demo", "orka")
	if err != nil {
		t.Fatalf("GetRepositoryMonitor() error = %v", err)
	}
	if got.Generation != 2 || got.Branch != "develop" {
		t.Fatalf("monitor = %#v, want updated generation and branch", got)
	}

	list, next, err := s.ListRepositoryMonitors(ctx, "demo", 10, "")
	if err != nil {
		t.Fatalf("ListRepositoryMonitors() error = %v", err)
	}
	if next != "" || len(list) != 1 {
		t.Fatalf("list len=%d next=%q, want one item and no cursor", len(list), next)
	}
}

func TestMonitorStoreRunsItemsReviewsRepairsAndEvents(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		TargetKind:       "pull_request",
		TargetNumber:     42,
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	runs, _, err := s.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "run-1" {
		t.Fatalf("runs = %#v, want run-1", runs)
	}

	if err := s.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Kind:             "pull_request",
		Number:           42,
		Title:            "test pr",
		State:            "open",
		HeadSHA:          "abc123",
		LastVerdict:      "needs_changes",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	item, err := s.GetMonitorItem(ctx, "demo", "orka", "pull_request", "42")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.HeadSHA != "abc123" || item.LastVerdict != "needs_changes" {
		t.Fatalf("item = %#v, want stored head SHA and verdict", item)
	}

	if err := s.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "review-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Kind:             "pull_request",
		Number:           42,
		HeadSHA:          "abc123",
		Verdict:          "needs_changes",
	}); err != nil {
		t.Fatalf("CreateReviewRecord() error = %v", err)
	}
	reviews, _, err := s.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: "demo", MonitorName: "orka", Number: 42})
	if err != nil {
		t.Fatalf("ListReviewRecords() error = %v", err)
	}
	if len(reviews) != 1 || reviews[0].ID != "review-1" {
		t.Fatalf("reviews = %#v, want review-1", reviews)
	}

	if err := s.CreateRepairJob(ctx, &store.RepairJob{
		ID:               "repair-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Repo:             "sozercan/orka",
		PRNumber:         42,
		Intent:           "fix_ci",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateRepairJob() error = %v", err)
	}
	repairs, _, err := s.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "demo", MonitorName: "orka", PRNumber: 42})
	if err != nil {
		t.Fatalf("ListRepairJobs() error = %v", err)
	}
	if len(repairs) != 1 || repairs[0].ID != "repair-1" {
		t.Fatalf("repairs = %#v, want repair-1", repairs)
	}

	if err := s.CreateMonitorEvent(ctx, &store.MonitorEvent{
		ID:               "event-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		RunID:            "run-1",
		ItemKind:         "pull_request",
		ItemNumber:       42,
		EventType:        "run_queued",
		Summary:          "manual run queued",
	}); err != nil {
		t.Fatalf("CreateMonitorEvent() error = %v", err)
	}
	events, _, err := s.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "demo", MonitorName: "orka", RunID: "run-1"})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	if len(events) != 1 || events[0].ID != "event-1" {
		t.Fatalf("events = %#v, want event-1", events)
	}
}

func TestCreateMonitorRunRejectsDuplicateActiveRun(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-2",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun(second queued) error = %v", err)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-3",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "running",
	}); err != nil {
		t.Fatalf("CreateMonitorRun(running behind queued) error = %v", err)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-4",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "running",
	}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateMonitorRun(duplicate running) error = %v, want ErrConflict", err)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-5",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "succeeded",
	}); err != nil {
		t.Fatalf("CreateMonitorRun(succeeded) error = %v", err)
	}
}

func TestUpsertRepositoryMonitorBranchChangeClearsDependentState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.UpsertRepositoryMonitor(ctx, &store.RepositoryMonitorRecord{
		Namespace:  "demo",
		Name:       "orka",
		UID:        "uid-1",
		RepoURL:    "https://github.com/sozercan/orka",
		Owner:      "sozercan",
		Repository: "orka",
		Branch:     "main",
	}); err != nil {
		t.Fatalf("UpsertRepositoryMonitor() error = %v", err)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-main",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if err := s.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Kind:             "pull_request",
		Number:           7,
		State:            "open",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}

	if err := s.UpsertRepositoryMonitor(ctx, &store.RepositoryMonitorRecord{
		Namespace:  "demo",
		Name:       "orka",
		UID:        "uid-1",
		RepoURL:    "https://github.com/sozercan/orka",
		Owner:      "sozercan",
		Repository: "orka",
		Branch:     "release",
	}); err != nil {
		t.Fatalf("UpsertRepositoryMonitor(branch change) error = %v", err)
	}

	got, err := s.GetRepositoryMonitor(ctx, "demo", "orka")
	if err != nil {
		t.Fatalf("GetRepositoryMonitor() error = %v", err)
	}
	if got.Branch != "release" {
		t.Fatalf("branch = %q, want release", got.Branch)
	}
	runs, _, err := s.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	items, _, err := s.ListMonitorItems(ctx, store.MonitorItemFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorItems() error = %v", err)
	}
	if len(runs) != 0 || len(items) != 0 {
		t.Fatalf("dependent branch state remains: runs=%d items=%d", len(runs), len(items))
	}
}

func TestDeleteRepositoryMonitorCleansDependentStateWithoutMetadata(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "orphan-run",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if err := s.DeleteRepositoryMonitor(ctx, "demo", "orka"); err != nil {
		t.Fatalf("DeleteRepositoryMonitor() error = %v", err)
	}

	runs, _, err := s.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v, want orphaned run removed", runs)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "new-run",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun(after cleanup) error = %v", err)
	}
}

func TestUpsertRepositoryMonitorIdentityChangeClearsDependentState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.UpsertRepositoryMonitor(ctx, &store.RepositoryMonitorRecord{
		Namespace:  "demo",
		Name:       "orka",
		UID:        "uid-1",
		RepoURL:    "https://github.com/sozercan/orka",
		Owner:      "sozercan",
		Repository: "orka",
		Branch:     "main",
	}); err != nil {
		t.Fatalf("UpsertRepositoryMonitor() error = %v", err)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if err := s.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Kind:             "pull_request",
		Number:           42,
		State:            "open",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	if err := s.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "review-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Kind:             "pull_request",
		Number:           42,
	}); err != nil {
		t.Fatalf("CreateReviewRecord() error = %v", err)
	}
	if err := s.CreateCommandEvent(ctx, &store.CommandEvent{
		ID:               "command-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
	}); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := s.CreateRepairJob(ctx, &store.RepairJob{
		ID:               "repair-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
	}); err != nil {
		t.Fatalf("CreateRepairJob() error = %v", err)
	}
	if err := s.CreateMonitorEvent(ctx, &store.MonitorEvent{
		ID:               "event-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		EventType:        "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorEvent() error = %v", err)
	}

	if err := s.UpsertRepositoryMonitor(ctx, &store.RepositoryMonitorRecord{
		Namespace:  "demo",
		Name:       "orka",
		UID:        "uid-2",
		RepoURL:    "https://github.com/example/other",
		Owner:      "example",
		Repository: "other",
		Branch:     "main",
	}); err != nil {
		t.Fatalf("UpsertRepositoryMonitor(recreate) error = %v", err)
	}

	got, err := s.GetRepositoryMonitor(ctx, "demo", "orka")
	if err != nil {
		t.Fatalf("GetRepositoryMonitor() error = %v", err)
	}
	if got.UID != "uid-2" || got.Owner != "example" || got.Repository != "other" {
		t.Fatalf("monitor = %#v, want replacement identity", got)
	}
	runs, _, err := s.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	items, _, err := s.ListMonitorItems(ctx, store.MonitorItemFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorItems() error = %v", err)
	}
	reviews, _, err := s.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRecords() error = %v", err)
	}
	repairs, _, err := s.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListRepairJobs() error = %v", err)
	}
	events, _, err := s.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	if len(runs)+len(items)+len(reviews)+len(repairs)+len(events) != 0 {
		t.Fatalf("dependent state remains: runs=%d items=%d reviews=%d repairs=%d events=%d", len(runs), len(items), len(reviews), len(repairs), len(events))
	}
	if _, err := s.GetCommandEvent(ctx, "demo", "command-1"); err != store.ErrNotFound {
		t.Fatalf("GetCommandEvent() error = %v, want ErrNotFound", err)
	}
}

func TestDeleteRepositoryMonitorCascadesMonitorState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	if err := s.UpsertRepositoryMonitor(ctx, &store.RepositoryMonitorRecord{
		Namespace: "demo",
		Name:      "orka",
		RepoURL:   "https://github.com/sozercan/orka",
	}); err != nil {
		t.Fatalf("UpsertRepositoryMonitor() error = %v", err)
	}
	if err := s.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Trigger:          "manual",
		Phase:            "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if err := s.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Kind:             "pull_request",
		Number:           42,
		State:            "open",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	if err := s.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "review-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		Kind:             "pull_request",
		Number:           42,
	}); err != nil {
		t.Fatalf("CreateReviewRecord() error = %v", err)
	}
	if err := s.CreateCommandEvent(ctx, &store.CommandEvent{
		ID:               "command-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
	}); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := s.CreateRepairJob(ctx, &store.RepairJob{
		ID:               "repair-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
	}); err != nil {
		t.Fatalf("CreateRepairJob() error = %v", err)
	}
	if err := s.CreateMonitorEvent(ctx, &store.MonitorEvent{
		ID:               "event-1",
		MonitorNamespace: "demo",
		MonitorName:      "orka",
		EventType:        "queued",
	}); err != nil {
		t.Fatalf("CreateMonitorEvent() error = %v", err)
	}

	if err := s.DeleteRepositoryMonitor(ctx, "demo", "orka"); err != nil {
		t.Fatalf("DeleteRepositoryMonitor() error = %v", err)
	}

	if _, err := s.GetRepositoryMonitor(ctx, "demo", "orka"); err != store.ErrNotFound {
		t.Fatalf("GetRepositoryMonitor() error = %v, want ErrNotFound", err)
	}
	runs, _, err := s.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	items, _, err := s.ListMonitorItems(ctx, store.MonitorItemFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorItems() error = %v", err)
	}
	reviews, _, err := s.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRecords() error = %v", err)
	}
	repairs, _, err := s.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListRepairJobs() error = %v", err)
	}
	events, _, err := s.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "demo", MonitorName: "orka", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	if len(runs)+len(items)+len(reviews)+len(repairs)+len(events) != 0 {
		t.Fatalf("dependent state remains: runs=%d items=%d reviews=%d repairs=%d events=%d", len(runs), len(items), len(reviews), len(repairs), len(events))
	}
	if _, err := s.GetCommandEvent(ctx, "demo", "command-1"); err != store.ErrNotFound {
		t.Fatalf("GetCommandEvent() error = %v, want ErrNotFound", err)
	}
}
