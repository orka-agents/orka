package storagetest

import (
	"testing"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The repository monitor write cases. Each write that fills IDs, keys or
// times into the record it is passed gets a fresh record for every attempt,
// so a replay is checked against what the original call wrote back.

func catalogMonitor(generation int64) *storagev1.RepositoryMonitorRecord {
	return &storagev1.RepositoryMonitorRecord{Namespace: workflowNamespace, Name: workflowMonitor, UID: "monitor-uid",
		RepoURL: "https://github.com/org/repo", Owner: "org", Repository: "repo", Branch: "main", Generation: generation}
}

func catalogMonitorRun(phase string) *storagev1.MonitorRun {
	return &storagev1.MonitorRun{ID: "run-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		Trigger: "scheduled", Phase: phase, StartedAt: workflowAt(0)}
}

func catalogWorkAction(id, status string, number int64) *storagev1.WorkAction {
	return &storagev1.WorkAction{ID: id, MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		TargetKind: "pull_request", TargetNumber: number, DesiredAction: "review", Status: status}
}

func catalogActionRecord(summary string) *storagev1.ActionRecord {
	return &storagev1.ActionRecord{ID: "action-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		Kind: "pull_request", Number: 12, ActionKind: "triage", Summary: summary, PayloadJSON: `{"step":"triage"}`}
}

func catalogReviewPublish(phase string) *storagev1.ReviewPublishRecord {
	return &storagev1.ReviewPublishRecord{ID: "publish-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		ItemKind: "pull_request", ItemNumber: 12, HeadSHA: "head-1", ReviewRecordID: "review-1", Phase: phase}
}

func catalogCommandEvent(status string) *storagev1.CommandEvent {
	return &storagev1.CommandEvent{ID: "command-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		Kind: "pull_request", Number: 12, DedupeKey: "comment-1", Command: "/orka review", Intent: "review", Status: status}
}

func catalogImplementationJob(phase string) *storagev1.ImplementationJob {
	return &storagev1.ImplementationJob{ID: "job-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		Repo: "org/repo", IssueNumber: 5, Phase: phase}
}

func catalogMutation(status string) *storagev1.GitHubMutationRecord {
	return &storagev1.GitHubMutationRecord{ID: "mutation-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		Operation: "comment", Status: status}
}

func catalogRepairJob(phase string) *storagev1.RepairJob {
	return &storagev1.RepairJob{ID: "repair-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
		Repo: "org/repo", PRNumber: 12, Intent: "fix-ci", Phase: phase}
}

func catalogMonitorItem(verdict string) *storagev1.MonitorItem {
	return &storagev1.MonitorItem{MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor, Kind: "pull_request",
		Number: 12, Title: "Fix bug", State: "open", HeadSHA: "head-1", LastVerdict: verdict}
}

func observeRepositoryMonitor(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	monitor, err := p.GetRepositoryMonitor(t.Context(), workflowNamespace, workflowMonitor)
	run, runErr := p.GetMonitorRun(t.Context(), workflowNamespace, "run-1")
	return []any{observed(t, monitor, err), observed(t, run, runErr)}
}

func observeMonitorRun(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	run, err := p.GetMonitorRun(t.Context(), workflowNamespace, "run-1")
	return observed(t, run, err)
}

func observeMonitorItem(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	item, err := p.GetMonitorItem(t.Context(), workflowNamespace, workflowMonitor, "pull_request", "12")
	return observed(t, item, err)
}

func observeWorkActions(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	actions, _, err := p.ListWorkActions(t.Context(), storagev1.WorkActionFilter{Namespace: workflowNamespace})
	require.NoError(t, err)
	return actions
}

func observeActionRecord(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	record, err := p.GetActionRecord(t.Context(), workflowNamespace, "action-1")
	return observed(t, record, err)
}

func observeReviewRecord(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	record, err := p.GetReviewRecord(t.Context(), workflowNamespace, "review-1")
	return observed(t, record, err)
}

func observeReviewPublish(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	record, err := p.GetReviewPublishRecord(t.Context(), workflowNamespace, "publish-1")
	return observed(t, record, err)
}

func observeCommandEvent(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	event, err := p.GetCommandEvent(t.Context(), workflowNamespace, "command-1")
	return observed(t, event, err)
}

func observeImplementationJob(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	job, err := p.GetImplementationJob(t.Context(), workflowNamespace, "job-1")
	return observed(t, job, err)
}

func observeMutation(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	record, err := p.GetGitHubMutationRecord(t.Context(), workflowNamespace, "mutation-1")
	return observed(t, record, err)
}

func observeRepairJob(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	job, err := p.GetRepairJob(t.Context(), workflowNamespace, "repair-1")
	return observed(t, job, err)
}

func observeMonitorEvents(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	events, _, err := p.ListMonitorEvents(t.Context(), storagev1.MonitorEventFilter{Namespace: workflowNamespace})
	require.NoError(t, err)
	return events
}

// rebuiltArgs returns args built anew for every attempt.
func rebuiltArgs(build func() []any) func(storagev1.ControllerEpochFence) []any {
	return func(storagev1.ControllerEpochFence) []any { return build() }
}

func init() {
	type monitorCase = writeCase[storagev1.StateProvider]
	stateWriteCases = append(stateWriteCases,
		monitorCase{
			method: "UpsertRepositoryMonitor",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.UpsertRepositoryMonitor(t.Context(), catalogMonitor(1)))
			},
			args:    rebuiltArgs(func() []any { return []any{catalogMonitor(2)} }),
			observe: observeRepositoryMonitor,
		},
		monitorCase{
			method: "DeleteRepositoryMonitor",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.UpsertRepositoryMonitor(t.Context(), catalogMonitor(1)))
				require.NoError(t, p.CreateMonitorRun(t.Context(), catalogMonitorRun("succeeded")))
			},
			args:    fixed(workflowNamespace, workflowMonitor),
			observe: observeRepositoryMonitor,
		},
		monitorCase{
			method:  "CreateMonitorRun",
			args:    rebuiltArgs(func() []any { return []any{catalogMonitorRun("running")} }),
			observe: observeMonitorRun,
		},
		monitorCase{
			method: "UpdateMonitorRun",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateMonitorRun(t.Context(), catalogMonitorRun("running")))
			},
			args: rebuiltArgs(func() []any {
				run := catalogMonitorRun("succeeded")
				completed := workflowAt(5)
				run.CompletedAt, run.CreatedTaskCount = &completed, 2
				return []any{run}
			}),
			observe: observeMonitorRun,
		},
		monitorCase{
			method: "UpsertMonitorItem",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.UpsertMonitorItem(t.Context(), catalogMonitorItem("")))
			},
			args:    rebuiltArgs(func() []any { return []any{catalogMonitorItem("approve")} }),
			observe: observeMonitorItem,
		},
		monitorCase{
			method:  "CreateWorkAction",
			args:    rebuiltArgs(func() []any { return []any{catalogWorkAction("action-1", "queued", 12)} }),
			observe: observeWorkActions,
		},
		monitorCase{
			method: "UpdateWorkAction",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateWorkAction(t.Context(), catalogWorkAction("action-1", "queued", 12)))
			},
			args: rebuiltArgs(func() []any {
				leased := catalogWorkAction("action-1", "leased", 12)
				until := workflowAt(10)
				leased.LeaseOwner, leased.LeaseExpiresAt, leased.Attempt = "worker-1", &until, 1
				return []any{leased}
			}),
			observe: observeWorkActions,
		},
		monitorCase{
			method: "CancelWorkActions",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				for _, action := range []*storagev1.WorkAction{
					catalogWorkAction("queued", "queued", 12), catalogWorkAction("control", "running", 12),
					catalogWorkAction("other-target", "queued", 13),
				} {
					require.NoError(t, p.CreateWorkAction(t.Context(), action))
				}
			},
			args:    fixed(workflowNamespace, workflowMonitor, "pull_request", int64(12), "superseded", "control"),
			observe: observeWorkActions,
		},
		monitorCase{
			method:  "CreateActionRecord",
			args:    rebuiltArgs(func() []any { return []any{catalogActionRecord("raw summary")} }),
			observe: observeActionRecord,
		},
		monitorCase{
			method: "UpdateActionRecord",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateActionRecord(t.Context(), catalogActionRecord("raw summary")))
			},
			args:    rebuiltArgs(func() []any { return []any{catalogActionRecord("redacted summary")} }),
			observe: observeActionRecord,
		},
		monitorCase{
			method: "CreateReviewRecord",
			args: rebuiltArgs(func() []any {
				return []any{&storagev1.ReviewRecord{ID: "review-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
					Kind: "pull_request", Number: 12, HeadSHA: "head-1", Verdict: "approve", FindingsJSON: `[]`}}
			}),
			observe: observeReviewRecord,
		},
		monitorCase{
			method:  "CreateReviewPublishRecord",
			args:    rebuiltArgs(func() []any { return []any{catalogReviewPublish("started")} }),
			observe: observeReviewPublish,
		},
		monitorCase{
			method: "UpdateReviewPublishRecord",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateReviewPublishRecord(t.Context(), catalogReviewPublish("started")))
			},
			args: rebuiltArgs(func() []any {
				record := catalogReviewPublish("succeeded")
				record.GitHubReviewID = "gh-review-1"
				return []any{record}
			}),
			observe: observeReviewPublish,
		},
		monitorCase{
			method:  "CreateCommandEvent",
			args:    rebuiltArgs(func() []any { return []any{catalogCommandEvent("received")} }),
			observe: observeCommandEvent,
		},
		monitorCase{
			method: "UpdateCommandEvent",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateCommandEvent(t.Context(), catalogCommandEvent("received")))
			},
			args: rebuiltArgs(func() []any {
				event := catalogCommandEvent("processed")
				processed := workflowAt(2)
				event.ProcessedAt = &processed
				return []any{event}
			}),
			observe: observeCommandEvent,
		},
		monitorCase{
			method:  "CreateImplementationJob",
			args:    rebuiltArgs(func() []any { return []any{catalogImplementationJob("coding")} }),
			observe: observeImplementationJob,
		},
		monitorCase{
			method: "UpdateImplementationJob",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateImplementationJob(t.Context(), catalogImplementationJob("coding")))
			},
			args: rebuiltArgs(func() []any {
				job := catalogImplementationJob("pr_opened")
				job.PRNumber = 99
				return []any{job}
			}),
			observe: observeImplementationJob,
		},
		monitorCase{
			method:  "CreateGitHubMutationRecord",
			args:    rebuiltArgs(func() []any { return []any{catalogMutation("pending")} }),
			observe: observeMutation,
		},
		monitorCase{
			method: "UpdateGitHubMutationRecord",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateGitHubMutationRecord(t.Context(), catalogMutation("pending")))
			},
			args:    rebuiltArgs(func() []any { return []any{catalogMutation("succeeded")} }),
			observe: observeMutation,
		},
		monitorCase{
			method:  "CreateRepairJob",
			args:    rebuiltArgs(func() []any { return []any{catalogRepairJob("queued")} }),
			observe: observeRepairJob,
		},
		monitorCase{
			method: "UpdateRepairJob",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.CreateRepairJob(t.Context(), catalogRepairJob("queued")))
			},
			args: rebuiltArgs(func() []any {
				job := catalogRepairJob("running")
				job.RepairCountPR = 1
				return []any{job}
			}),
			observe: observeRepairJob,
		},
		monitorCase{
			method: "CreateMonitorEvent",
			args: rebuiltArgs(func() []any {
				return []any{&storagev1.MonitorEvent{ID: "event-1", MonitorNamespace: workflowNamespace, MonitorName: workflowMonitor,
					EventType: "observed"}}
			}),
			observe: observeMonitorEvents,
		},
	)
}
