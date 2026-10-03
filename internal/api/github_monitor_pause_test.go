package api

import (
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
