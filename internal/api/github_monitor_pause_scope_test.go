package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
)

//nolint:gocyclo // Exercise policy intake and rejected scopes through signed webhooks.
func TestGitHubWebhookPausePolicyScope(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pullRequest     bool
		exactEvents     bool
		configure       func(*corev1alpha1.RepositoryMonitor, *githubLabelWebhookPayload)
		wantQueued      bool
		wantPermissions int32
	}{
		{name: "command_labels_disabled", wantQueued: true, wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
		}},
		{name: "missing_include_label", wantQueued: true, wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Targets.Issues.IncludeLabels = []string{"feature"}
		}},
		{name: "excluded_issue", wantQueued: true, wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Targets.Issues.ExcludeLabels = []string{"bug"}
		}},
		{name: "pull_request_command_labels_disabled", pullRequest: true, exactEvents: true, wantQueued: true, wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
		}},
		{name: "other_repository", configure: func(_ *corev1alpha1.RepositoryMonitor, p *githubLabelWebhookPayload) {
			p.Repository.FullName = "other/repository"
		}},
		{name: "issues_disabled", configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Targets.Issues.Enabled = false
		}},
		{name: "pull_requests_disabled", pullRequest: true, exactEvents: true, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			disabled := false
			m.Spec.Targets.PullRequests.Enabled = &disabled
		}},
		{name: "other_branch", pullRequest: true, exactEvents: true, configure: func(_ *corev1alpha1.RepositoryMonitor, p *githubLabelWebhookPayload) {
			p.PullRequest.Base.Ref = "release"
		}},
		{name: "draft_pull_request", pullRequest: true, configure: func(_ *corev1alpha1.RepositoryMonitor, p *githubLabelWebhookPayload) { p.PullRequest.Draft = true }},
		{name: "closed_issue", configure: func(_ *corev1alpha1.RepositoryMonitor, p *githubLabelWebhookPayload) { p.Issue.State = "closed" }},
		{name: "incomplete_pull_request", configure: func(_ *corev1alpha1.RepositoryMonitor, p *githubLabelWebhookPayload) {
			p.Issue.PullRequest = &githubIssuePullRequestID{}
		}},
		{name: "suspended_monitor", configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			suspended := true
			m.Spec.Suspend = &suspended
		}},
		{name: "permission_still_required", wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.Triggers.GitHub.Labels.RequireActorPermission = githubPermissionAdmin
		}},
	} {
		for _, action := range []string{githubWebhookActionLabeled, githubWebhookActionUnlabeled} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				var permissionCalls atomic.Int32
				permissionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					permissionCalls.Add(1)
					if req.Method != http.MethodGet || req.URL.Path != "/repos/sozercan/vekil/collaborators/octocat/permission" {
						t.Errorf("unexpected permission request: %s %s", req.Method, req.URL.Path)
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"permission":"write"}`))
				}))
				t.Cleanup(permissionServer.Close)
				secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: permissionServer.URL})
				monitor := githubWebhookRepositoryMonitor("pause-scope", tc.exactEvents)
				monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
				monitor.Spec.Targets.Issues.Enabled = true
				monitor.Spec.Triggers.GitHub.Labels.Enabled = true
				monitor.Spec.Policy.PauseLabels = []string{"hold"}
				labels := []githubWebhookLabel{{Name: "bug"}}
				if action == githubWebhookActionLabeled {
					labels = append(labels, githubWebhookLabel{Name: "hold"})
				}
				payload := githubLabelWebhookPayload{Action: action, Label: githubWebhookLabel{Name: "hold"}, Repository: githubWebhookRepository{FullName: "sozercan/vekil", CloneURL: githubWebhookTestVekilCloneURL, DefaultBranch: githubWebhookTestDefaultBranch}, Sender: githubWebhookUser{Login: "octocat"}}
				event, kind := githubEventIssues, repositoryMonitorTargetKindIssue
				if tc.pullRequest {
					event, kind = githubEventPullRequest, repositoryMonitorTargetKindPullRequest
					payload.PullRequest = &githubWebhookPullRequest{Number: 12, State: "open", Labels: labels}
					payload.PullRequest.Base.Ref = githubWebhookTestDefaultBranch
					payload.PullRequest.Base.Repo = payload.Repository
					payload.PullRequest.Head.Ref = "topic"
					payload.PullRequest.Head.SHA = githubWebhookTestHeadSHA
					payload.PullRequest.Head.Repo = payload.Repository
				} else {
					payload.Issue = &githubWebhookIssue{Number: 12, State: "open", Labels: labels}
				}
				tc.configure(monitor, &payload)
				fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
				db := setupGitHubWebhookMonitorStore(t)
				server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: db})
				storedLabels := `["bug"]`
				if action == githubWebhookActionUnlabeled {
					storedLabels = `["bug","hold"]`
				}
				if err := db.UpsertMonitorItem(t.Context(), &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: kind, ItemKey: "12", Number: 12, LabelsJSON: storedLabels, WorkflowPhase: "implementing", LastActionTaskName: "active-task", SnapshotDigest: "original"}); err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				resp := performSignedGitHubWebhook(t, server, event, "pause-scope-"+action, secret, body)
				wantStatus, wantRuns := http.StatusAccepted, 0
				if tc.wantQueued {
					wantStatus, wantRuns = http.StatusCreated, 1
				}
				if resp.StatusCode != wantStatus {
					t.Fatalf("status=%d, want %d; body: %s", resp.StatusCode, wantStatus, readRespBody(t, resp))
				}
				runs, _, err := db.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
				if err != nil || len(runs) != wantRuns {
					t.Fatalf("pause runs=%+v, want %d, err=%v", runs, wantRuns, err)
				}
				if tc.wantQueued && (runs[0].Trigger != "pause_label_event" || runs[0].TargetKind != kind || runs[0].TargetNumber != 12 || tc.pullRequest && runs[0].TargetSHA != githubWebhookTestHeadSHA) {
					t.Fatalf("pause queued the wrong target: %+v", runs[0])
				}
				item, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, kind, "12")
				if err != nil {
					t.Fatal(err)
				}
				wantLabels := storedLabels
				if tc.wantQueued && action == githubWebhookActionLabeled {
					wantLabels = `["bug","hold"]`
				}
				if item.LabelsJSON != wantLabels || item.WorkflowPhase != "implementing" || item.LastActionTaskName != "active-task" || item.SnapshotDigest != "original" {
					t.Fatalf("pause changed the wrong stored state: %+v", item)
				}
				if got := permissionCalls.Load(); got != tc.wantPermissions {
					t.Fatalf("permission calls=%d, want %d", got, tc.wantPermissions)
				}
				commands, _, err := db.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
				if err != nil || len(commands) != 0 {
					t.Fatalf("pause created commands: %+v, err=%v", commands, err)
				}
				assertNoTasks(t, fc)
			})
		}
	}
}
