package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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
		wantUnavailable bool
		blankToken      bool
		permission      string
		writeMonitor    bool
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
		{name: "suspended_monitor", wantQueued: true, wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			suspended := true
			m.Spec.Suspend = &suspended
			m.Spec.Triggers.GitHub.Labels.Enabled = false
		}},
		{name: "suspended_monitor_permission_still_required", wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			suspended := true
			m.Spec.Suspend = &suspended
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.Triggers.GitHub.Labels.RequireActorPermission = githubPermissionAdmin
		}},
		{name: "permission_still_required", wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.Triggers.GitHub.Labels.RequireActorPermission = githubPermissionAdmin
		}},
		{name: "read_credential_without_forge", wantQueued: true, wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = nil
			m.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
		}},
		{name: "legacy_read_credential_without_forge", wantQueued: true, wantPermissions: 1, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = nil
			m.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
		}},
		{name: "missing_permission_credentials", wantUnavailable: true, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = nil
		}},
		{name: "blank_permission_credentials", wantUnavailable: true, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: " "}
			m.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: " "}
			m.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: " "}
		}},
		{name: "missing_read_secret", wantUnavailable: true, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = nil
			m.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: "missing"}
		}},
		{name: "blank_read_secret", wantUnavailable: true, blankToken: true, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = nil
			m.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
		}},
		{name: "invalid_forge_does_not_fall_back", wantUnavailable: true, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: "missing"}
			m.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
		}},
		{name: "read_credential_rejects_sender", wantPermissions: 1, permission: githubPermissionRead, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = nil
			m.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
		}},
		{name: "read_only_monitor_does_not_abort_later_command", wantQueued: true, wantPermissions: 1, writeMonitor: true, configure: func(m *corev1alpha1.RepositoryMonitor, _ *githubLabelWebhookPayload) {
			m.Spec.Triggers.GitHub.Labels.Enabled = false
			m.Spec.ForgeCredentialRef = nil
			m.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
		}},
	} {
		for _, action := range []string{githubWebhookActionLabeled, githubWebhookActionUnlabeled} {
			t.Run(tc.name+"/"+action, func(t *testing.T) {
				var permissionCalls atomic.Int32
				var mutationCalls atomic.Int32
				permissionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if tc.writeMonitor && req.Method == http.MethodDelete && req.URL.Path == "/repos/sozercan/vekil/issues/12/labels/hold" {
						if req.Header.Get("Authorization") != "Bearer write-test-token" {
							t.Error("write command used a read credential for label consumption")
						}
						mutationCalls.Add(1)
						w.WriteHeader(http.StatusNoContent)
						return
					}
					permissionCalls.Add(1)
					if req.Method != http.MethodGet || req.URL.Path != "/repos/sozercan/vekil/collaborators/octocat/permission" {
						t.Errorf("unexpected permission request: %s %s", req.Method, req.URL.Path)
						w.WriteHeader(http.StatusForbidden)
						return
					}
					wantAuthorization := "Bearer test-token"
					if tc.writeMonitor && permissionCalls.Load() == 2 {
						wantAuthorization = "Bearer write-test-token"
					}
					if req.Header.Get("Authorization") != wantAuthorization {
						t.Error("permission lookup used the wrong credential role")
					}
					w.Header().Set("Content-Type", "application/json")
					permission := tc.permission
					if permission == "" {
						permission = githubPermissionWrite
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"permission": permission})
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
				readSecret := githubWebhookGitSecret()
				if tc.blankToken {
					readSecret.Data = map[string][]byte{"token": []byte(" ")}
				}
				fc := newGitHubWebhookFakeClient(t, monitor, readSecret)
				if tc.writeMonitor {
					later := monitor.DeepCopy()
					later.Name = "zz-write-monitor"
					later.ResourceVersion = ""
					later.Spec.Triggers.GitHub.Labels.Enabled = true
					later.Spec.Triggers.GitHub.Labels.ConsumeCommandLabels = true
					later.Spec.Triggers.GitHub.Labels.Issues.Implement = "hold"
					later.Spec.Policy.PauseLabels = []string{"other-pause"}
					forgeSecret := githubWebhookGitSecret()
					forgeSecret.Name = "write-forge"
					forgeSecret.Data = map[string][]byte{"token": []byte("write-test-token")}
					later.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: forgeSecret.Name}
					if err := fc.Create(t.Context(), later); err != nil {
						t.Fatal(err)
					}
					if err := fc.Create(t.Context(), forgeSecret); err != nil {
						t.Fatal(err)
					}
				}
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
				if tc.wantUnavailable {
					wantStatus = http.StatusServiceUnavailable
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
				events, _, err := db.ListMonitorEvents(t.Context(), store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, ItemKind: kind, ItemNumber: 12, EventType: "pause_label_changed", Limit: 10})
				if err != nil || len(events) != wantRuns {
					t.Fatalf("pause audits for %s=%+v, want %d, err=%v", kind, events, wantRuns, err)
				}
				if tc.wantQueued {
					kindName, otherKind, wantSHA := "issue", repositoryMonitorTargetKindPullRequest, ""
					if tc.pullRequest {
						kindName, otherKind, wantSHA = "pull request", repositoryMonitorTargetKindIssue, githubWebhookTestHeadSHA
					}
					wantSummary := fmt.Sprintf("GitHub %s event queued repository monitor run for %s #12", action, kindName)
					if events[0].RunID != runs[0].ID || events[0].ItemSHA != wantSHA || events[0].Summary != wantSummary {
						t.Fatalf("pause audit described the wrong target: %+v", events[0])
					}
					wrongKindEvents, _, err := db.ListMonitorEvents(t.Context(), store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, ItemKind: otherKind, ItemNumber: 12, EventType: "pause_label_changed", Limit: 10})
					if err != nil || len(wrongKindEvents) != 0 {
						t.Fatalf("pause audit appeared under %s: %+v, err=%v", otherKind, wrongKindEvents, err)
					}
				}
				if monitor.Spec.Suspend != nil && *monitor.Spec.Suspend {
					var current corev1alpha1.RepositoryMonitor
					if err := fc.Get(t.Context(), types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}, &current); err != nil {
						t.Fatal(err)
					}
					if current.Spec.Suspend == nil || !*current.Spec.Suspend {
						t.Fatal("pause intake resumed the suspended schedule")
					}
					if tc.wantQueued && current.Annotations[repositoryMonitorRunRequestAnnotation] != runs[0].ID {
						t.Fatal("suspended monitor did not receive its pause inventory signal")
					}
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
				wantPermissions := tc.wantPermissions
				if tc.writeMonitor && action == githubWebhookActionLabeled {
					wantPermissions++
				}
				if got := permissionCalls.Load(); got != wantPermissions {
					t.Fatalf("permission calls=%d, want %d", got, wantPermissions)
				}
				commands, _, err := db.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
				if err != nil || len(commands) != 0 {
					t.Fatalf("pause created commands: %+v, err=%v", commands, err)
				}
				if tc.writeMonitor {
					laterCommands, _, err := db.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: "zz-write-monitor", Limit: 10})
					wantCommands := 0
					if action == githubWebhookActionLabeled {
						wantCommands = 1
					}
					if err != nil || len(laterCommands) != wantCommands || mutationCalls.Load() != int32(wantCommands) {
						t.Fatalf("later write command was skipped: commands=%+v, mutations=%d, err=%v", laterCommands, mutationCalls.Load(), err)
					}
					if wantCommands == 1 && (laterCommands[0].Intent != githubActionImplement || laterCommands[0].Status != githubCommandStatusAccepted) {
						t.Fatalf("later command was not accepted: %+v", laterCommands[0])
					}
				}
				assertNoTasks(t, fc)
			})
		}
	}
}
