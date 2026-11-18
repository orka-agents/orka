package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
)

func TestGitHubWebhookPauseRemovalQueuesInventoryWithoutACommand(t *testing.T) {
	permissionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/sozercan/vekil/collaborators/octocat/permission" {
			t.Fatalf("permission path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("Authorization = %q, want bearer auth", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"permission":"write"}`))
	}))
	t.Cleanup(permissionServer.Close)
	secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: permissionServer.URL})
	pullRequestsEnabled := false
	monitor := githubWebhookRepositoryMonitor("issue-loop", false)
	monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
	monitorStore := setupGitHubWebhookMonitorStore(t)
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})

	body := []byte(`{
		"action":"unlabeled",
		"label":{"name":"orka:pause"},
		"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
		"issue":{"number":12,"title":"Add health endpoint","body":"Please add /healthz.","html_url":"https://github.com/sozercan/vekil/issues/12","updated_at":"2026-06-01T00:00:00Z","labels":[{"name":"bug"},{"name":"orka:pause"}]},
		"sender":{"login":"octocat"}
	}`)
	resp := performSignedGitHubWebhook(t, server, githubEventIssues, "delivery-orka-plan", secret, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusCreated, readRespBody(t, resp))
	}
	commands, _, err := monitorStore.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: "default", MonitorName: monitor.Name, Limit: 10})
	if err != nil || len(commands) != 0 {
		t.Fatalf("pause event created a command: %#v %v", commands, err)
	}
	runs, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "default", MonitorName: "issue-loop", TargetKind: repositoryMonitorTargetKindIssue, TargetNumber: 12, Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].Trigger != "pause_label_event" {
		t.Fatalf("runs = %#v, want one label-command issue run", runs)
	}
	assertNoTasks(t, fc)

}

func TestGitHubWebhookPausePersistsUntilFreshInventory(t *testing.T) {
	permissionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"permission":"write"}`))
	}))
	t.Cleanup(permissionServer.Close)
	secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: permissionServer.URL})
	monitor := githubWebhookRepositoryMonitor("pause-intake", false)
	monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
	db := setupGitHubWebhookMonitorStore(t)
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: db})
	if err := db.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: "issue", ItemKey: "12", Number: 12,
		LabelsJSON: `["bug"]`, SnapshotDigest: "sha256:current", WorkflowPhase: "implementing", LastActionTaskName: "active-task",
	}); err != nil {
		t.Fatal(err)
	}
	// A stale removal delivery must not clear a later addition. Inventory
	// confirms the live label state before a paused workflow can resume.
	for _, action := range []string{"labeled", "unlabeled"} {
		body := fmt.Appendf(nil, `{"action":%q,"label":{"name":"orka:pause"},"repository":{"full_name":"sozercan/vekil"},"issue":{"number":12,"state":"open","labels":[]},"sender":{"login":"octocat"}}`, action)
		resp := performSignedGitHubWebhook(t, server, githubEventIssues, "pause-"+action, secret, body)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s status = %d; body: %s", action, resp.StatusCode, readRespBody(t, resp))
		}
		item, err := db.GetMonitorItem(t.Context(), monitor.Namespace, monitor.Name, "issue", "12")
		if err != nil {
			t.Fatal(err)
		}
		var labels []string
		if err := json.Unmarshal([]byte(item.LabelsJSON), &labels); err != nil {
			t.Fatal(err)
		}
		paused := repositoryMonitorWebhookMatchingLabel(repositoryMonitorAPIPauseLabels(monitor), labels) != ""
		if !paused || repositoryMonitorWebhookMatchingLabel([]string{"bug"}, labels) == "" {
			t.Fatalf("%s labels = %s", action, item.LabelsJSON)
		}
		if item.WorkflowPhase != "implementing" || item.LastActionTaskName != "active-task" || item.SnapshotDigest != "sha256:current" {
			t.Fatalf("pause intake changed active workflow: %+v", item)
		}
	}
}

//nolint:gocyclo // Keep mixed-monitor authorization and replay assertions in one fixture.
func TestGitHubWebhookPauseDoesNotSuppressAnotherMonitorCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		minimum  string
		allowed  []string
		wantRuns int
	}{
		{name: "authorized_pause", minimum: githubPermissionWrite, wantRuns: 1},
		{name: "stricter_minimum", minimum: githubPermissionAdmin},
		{name: "stricter_policy", minimum: githubPermissionWrite, allowed: []string{githubPermissionAdmin}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			permissionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"permission":"write"}`))
			}))
			t.Cleanup(permissionServer.Close)
			secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: permissionServer.URL})
			pauseMonitor := githubWebhookRepositoryMonitor("pause-loop", false)
			pauseMonitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
			pauseMonitor.Spec.Targets.Issues.Enabled = true
			pauseMonitor.Spec.Triggers.GitHub.Labels.Enabled = true
			pauseMonitor.Spec.Triggers.GitHub.Labels.RequireActorPermission = tc.minimum
			pauseMonitor.Spec.Policy.AllowedRepositoryPermissions = tc.allowed
			pauseMonitor.Spec.Policy.PauseLabels = []string{"hold"}
			implementMonitor := pauseMonitor.DeepCopy()
			implementMonitor.Name = "implement-loop"
			implementMonitor.Spec.Policy.PauseLabels = []string{"other-pause"}
			implementMonitor.Spec.Policy.AllowedRepositoryPermissions = nil
			implementMonitor.Spec.Triggers.GitHub.Labels.RequireActorPermission = githubPermissionWrite
			implementMonitor.Spec.Triggers.GitHub.Labels.Issues.Implement = "hold"
			fc := newGitHubWebhookFakeClient(t, pauseMonitor, implementMonitor, githubWebhookGitSecret())
			db := setupGitHubWebhookMonitorStore(t)
			server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: db})
			if err := db.UpsertMonitorItem(t.Context(), &store.MonitorItem{MonitorNamespace: pauseMonitor.Namespace, MonitorName: pauseMonitor.Name, Kind: "issue", ItemKey: "12", Number: 12, LabelsJSON: `[]`}); err != nil {
				t.Fatal(err)
			}
			body := []byte(`{"action":"labeled","label":{"name":"hold"},"repository":{"full_name":"sozercan/vekil"},"issue":{"number":12,"state":"open","labels":[{"name":"hold"}]},"sender":{"login":"octocat"}}`)
			for attempt := range 2 {
				resp := performSignedGitHubWebhook(t, server, githubEventIssues, "shared-label", secret, body)
				wantStatus := http.StatusCreated
				if attempt > 0 {
					wantStatus = http.StatusAccepted
				}
				if resp.StatusCode != wantStatus {
					t.Fatalf("status = %d; body: %s", resp.StatusCode, readRespBody(t, resp))
				}
			}
			commands, _, err := db.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: implementMonitor.Namespace, MonitorName: implementMonitor.Name})
			if err != nil || len(commands) != 1 || commands[0].Intent != githubActionImplement || commands[0].Status != githubCommandStatusAccepted {
				t.Fatalf("other monitor command = %+v, err = %v", commands, err)
			}
			runs, _, err := db.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: pauseMonitor.Namespace, MonitorName: pauseMonitor.Name})
			if err != nil || len(runs) != tc.wantRuns {
				t.Fatalf("pause runs = %+v, err = %v, want %d", runs, err, tc.wantRuns)
			}
			item, err := db.GetMonitorItem(t.Context(), pauseMonitor.Namespace, pauseMonitor.Name, "issue", "12")
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRuns == 0 && item.LabelsJSON != `[]` {
				t.Fatalf("rejected sender changed stored pause labels: %s", item.LabelsJSON)
			}
			events, _, err := db.ListMonitorEvents(t.Context(), store.MonitorEventFilter{Namespace: pauseMonitor.Namespace, MonitorName: pauseMonitor.Name, EventType: "pause_label_rejected"})
			if err != nil || len(events) != 1-tc.wantRuns {
				t.Fatalf("rejection receipts = %+v, err = %v", events, err)
			}
		})
	}
}
