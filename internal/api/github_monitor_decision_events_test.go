package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// Every label command's intake decision is recorded as one monitor event,
// with the reason and the policy clause that decided, however often GitHub
// redelivers the webhook.
func TestGitHubWebhook_LabelCommandIntakeRecordsOneDecisionEvent(t *testing.T) {
	tests := []struct {
		name         string
		permission   string
		labels       string
		wantStatus   string
		wantType     string
		wantDecision string
		wantReason   string
		wantPolicy   string
	}{
		{
			name: "accepted", permission: "maintain", labels: `{"name":"orka:implement"}`,
			wantStatus: githubCommandStatusAccepted, wantType: store.RepositoryMonitorEventCommandAccepted,
			wantDecision: store.RepositoryMonitorDecisionAccepted,
		},
		{
			name: "rejected below minimum permission", permission: "triage", labels: `{"name":"orka:implement"}`,
			wantStatus: githubCommandStatusRejected, wantType: store.RepositoryMonitorEventCommandRejected,
			wantDecision: store.RepositoryMonitorDecisionRefused, wantReason: repositoryMonitorCommandReasonPermission,
			wantPolicy: "spec.triggers.github.labels.requireActorPermission=maintain",
		},
		{
			name: "blocked by protected label", permission: "maintain", labels: `{"name":"security-sensitive"},{"name":"orka:implement"}`,
			wantStatus: githubCommandStatusBlocked, wantType: store.RepositoryMonitorEventCommandBlocked,
			wantDecision: store.RepositoryMonitorDecisionBlocked, wantReason: repositoryMonitorCommandReasonGuardLabel,
			wantPolicy: "spec.policy.protectedLabels",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			permissionServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"permission":"` + tt.permission + `"}`))
			}))
			t.Cleanup(permissionServer.Close)
			secret := configureGitHubWebhookTest(t, map[string]string{githubAPIBaseURLEnv: permissionServer.URL})
			pullRequestsEnabled := false
			monitor := githubWebhookRepositoryMonitor("decision-loop", false)
			monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
			monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
			monitor.Spec.Targets.Issues.Enabled = true
			monitor.Spec.Triggers.GitHub.Labels.Enabled = true
			monitor.Spec.Triggers.GitHub.Labels.RequireActorPermission = githubPermissionMaintain
			monitor.Spec.Policy.ProtectedLabels = []string{"security-sensitive"}
			fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
			monitorStore := setupGitHubWebhookMonitorStore(t)
			server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})

			body := []byte(`{
				"action":"labeled",
				"label":{"name":"orka:implement"},
				"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
				"issue":{"number":44,"state":"open","title":"Add health endpoint","body":"Please add /healthz.","html_url":"https://github.com/sozercan/vekil/issues/44","updated_at":"2026-06-01T00:00:00Z","labels":[` + tt.labels + `]},
				"sender":{"login":"octocat"}
			}`)
			for range 2 {
				resp := performSignedGitHubWebhook(t, server, githubEventIssues, "delivery-decision", secret, body)
				if resp.StatusCode >= http.StatusBadRequest {
					t.Fatalf("status = %d; body: %s", resp.StatusCode, readRespBody(t, resp))
				}
			}
			commands, _, err := monitorStore.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: "default", MonitorName: monitor.Name, Limit: 10})
			if err != nil || len(commands) != 1 || commands[0].Status != tt.wantStatus {
				t.Fatalf("commands = %#v err=%v, want one %s command", commands, err, tt.wantStatus)
			}
			events, _, err := monitorStore.ListMonitorEvents(t.Context(), store.MonitorEventFilter{Namespace: "default", MonitorName: monitor.Name, EventType: tt.wantType, Limit: 10})
			if err != nil || len(events) != 1 {
				t.Fatalf("%s events = %#v err=%v, want exactly one across redeliveries", tt.wantType, events, err)
			}
			event := events[0]
			if event.ItemKind != repositoryMonitorTargetKindIssue || event.ItemNumber != 44 || event.ItemSHA != commands[0].IssueSnapshotDigest {
				t.Fatalf("event target = %s #%d %q, want issue #44 at its snapshot", event.ItemKind, event.ItemNumber, event.ItemSHA)
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata[store.RepositoryMonitorEventDecisionField] != tt.wantDecision || metadata["commandEventID"] != commands[0].ID || metadata["sender"] != "octocat" {
				t.Fatalf("metadata = %v, want decision %s linked to %s", metadata, tt.wantDecision, commands[0].ID)
			}
			if reason, _ := metadata[store.RepositoryMonitorEventReasonField].(string); reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if policy, _ := metadata[store.RepositoryMonitorEventPolicyField].(string); policy != tt.wantPolicy {
				t.Fatalf("policy = %q, want %q", policy, tt.wantPolicy)
			}
		})
	}
}

func TestRepositoryMonitorPermissionPolicyNamesTheDecidingClause(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{}
	monitor.Spec.Triggers.GitHub.Labels.RequireActorPermission = githubPermissionMaintain
	monitor.Spec.Policy.AllowedRepositoryPermissions = []string{githubPermissionAdmin}
	tests := []struct {
		permission, intent, want string
	}{
		{permission: githubPermissionWrite, intent: githubActionImplement, want: "spec.triggers.github.labels.requireActorPermission=maintain"},
		{permission: githubPermissionMaintain, intent: githubActionImplement, want: "spec.policy.allowedRepositoryPermissions"},
		{permission: githubPermissionRead, intent: commandIntentPlan, want: repositoryMonitorReadOnlyCommandPermissionPolicy},
		{permission: githubPermissionWrite, intent: "", want: "spec.triggers.github.labels.requireActorPermission=maintain"},
	}
	for _, tt := range tests {
		if got := repositoryMonitorPermissionPolicy(monitor, tt.permission, tt.intent); got != tt.want {
			t.Errorf("repositoryMonitorPermissionPolicy(%q, %q) = %q, want %q", tt.permission, tt.intent, got, tt.want)
		}
	}
	monitor.Spec.Triggers.GitHub.Labels.RequireActorPermission = ""
	if got := repositoryMonitorPermissionPolicy(monitor, githubPermissionTriage, githubActionImplement); got != "spec.triggers.github.labels.requireActorPermission=write" {
		t.Errorf("default minimum policy = %q", got)
	}
}

// A label command that reaches a monitor for its repository but falls
// outside the monitor's target scope records one command_rejected event and
// no command. Monitors the command is not addressed to record nothing.
func TestGitHubWebhook_OutOfScopeLabelCommandRecordsRejectionWithoutCommand(t *testing.T) {
	tests := []struct {
		name       string
		state      string
		labels     string
		configure  func(*corev1alpha1.RepositoryMonitor)
		wantReason string
		wantPolicy string
	}{
		{name: "closed issue", state: "closed", labels: `{"name":"orka:implement"}`, wantReason: repositoryMonitorCommandReasonTargetNotOpen},
		{
			name: "issue targets disabled", state: "open", labels: `{"name":"orka:implement"}`,
			configure:  func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Targets.Issues.Enabled = false },
			wantReason: repositoryMonitorCommandReasonIssueTargetsDisabled, wantPolicy: "spec.targets.issues.enabled=false",
		},
		{
			name: "excluded label", state: "open", labels: `{"name":"wontfix"},{"name":"orka:implement"}`,
			configure:  func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Targets.Issues.ExcludeLabels = []string{"wontfix"} },
			wantReason: repositoryMonitorCommandReasonExcludedLabel, wantPolicy: "spec.targets.issues.excludeLabels",
		},
		{
			name: "missing required label", state: "open", labels: `{"name":"orka:implement"}`,
			configure:  func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Targets.Issues.IncludeLabels = []string{"orka-ok"} },
			wantReason: repositoryMonitorCommandReasonMissingLabel, wantPolicy: "spec.targets.issues.includeLabels",
		},
		{
			name: "monitor for another repository", state: "closed", labels: `{"name":"orka:implement"}`,
			configure: func(m *corev1alpha1.RepositoryMonitor) { m.Spec.RepoURL = "https://github.com/sozercan/other" },
		},
		{
			name: "suspended monitor", state: "closed", labels: `{"name":"orka:implement"}`,
			configure: func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Suspend = new(true) },
		},
		{
			name: "label intake off", state: "closed", labels: `{"name":"orka:implement"}`,
			configure: func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Triggers.GitHub.Labels.Enabled = false },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := configureGitHubWebhookTest(t, nil)
			pullRequestsEnabled := false
			monitor := githubWebhookRepositoryMonitor("scope-loop", false)
			monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: githubWebhookTestGitSecret}
			monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
			monitor.Spec.Targets.Issues.Enabled = true
			monitor.Spec.Triggers.GitHub.Labels.Enabled = true
			if tt.configure != nil {
				tt.configure(monitor)
			}
			fc := newGitHubWebhookFakeClient(t, monitor, githubWebhookGitSecret())
			monitorStore := setupGitHubWebhookMonitorStore(t)
			server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})

			body := []byte(`{
				"action":"labeled",
				"label":{"name":"orka:implement"},
				"repository":{"full_name":"sozercan/vekil","html_url":"https://github.com/sozercan/vekil","clone_url":"https://github.com/sozercan/vekil.git","default_branch":"main"},
				"issue":{"number":45,"state":"` + tt.state + `","title":"Out of scope","body":"x","html_url":"https://github.com/sozercan/vekil/issues/45","updated_at":"2026-06-01T00:00:00Z","labels":[` + tt.labels + `]},
				"sender":{"login":"octocat"}
			}`)
			for range 2 {
				resp := performSignedGitHubWebhook(t, server, githubEventIssues, "delivery-scope", secret, body)
				if resp.StatusCode != http.StatusAccepted {
					t.Fatalf("status = %d, want accepted ignored event; body: %s", resp.StatusCode, readRespBody(t, resp))
				}
			}
			commands, _, err := monitorStore.ListCommandEvents(t.Context(), store.CommandEventFilter{Namespace: "default", MonitorName: monitor.Name, Limit: 10})
			if err != nil || len(commands) != 0 {
				t.Fatalf("commands = %#v err=%v, want none", commands, err)
			}
			events, _, err := monitorStore.ListMonitorEvents(t.Context(), store.MonitorEventFilter{Namespace: "default", MonitorName: monitor.Name, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantReason == "" {
				if len(events) != 0 {
					t.Fatalf("events = %#v, want none for a command not addressed to this monitor", events)
				}
				return
			}
			if len(events) != 1 || events[0].EventType != store.RepositoryMonitorEventCommandRejected || events[0].ItemNumber != 45 {
				t.Fatalf("events = %#v, want one command_rejected across redeliveries", events)
			}
			var metadata map[string]any
			if err := json.Unmarshal([]byte(events[0].MetadataJSON), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata[store.RepositoryMonitorEventDecisionField] != store.RepositoryMonitorDecisionRefused || metadata[store.RepositoryMonitorEventReasonField] != tt.wantReason ||
				metadata["delivery"] != "delivery-scope" || metadata[repositoryMonitorEventSenderField] != "octocat" {
				t.Fatalf("metadata = %v, want a refusal for %s", metadata, tt.wantReason)
			}
			if policy, _ := metadata[store.RepositoryMonitorEventPolicyField].(string); policy != tt.wantPolicy {
				t.Fatalf("policy = %q, want %q", policy, tt.wantPolicy)
			}
		})
	}
}
