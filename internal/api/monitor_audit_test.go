package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

type repositoryMonitorAuditTestPage struct {
	Items    []RepositoryMonitorAuditRecord `json:"items"`
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
}

func repositoryMonitorAuditTestMonitor(name string) *corev1alpha1.RepositoryMonitor {
	return &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: monitorTestRepoURL,
			Agents:  corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}},
		},
	}
}

// seedRepositoryMonitorAudit records, for monitor "audit-monitor", decisions
// inside and outside a 24h window, a lifecycle event between them, and refs
// to a command and mutation of its own and a command of another monitor.
func seedRepositoryMonitorAudit(t *testing.T, handlers *Handlers, now time.Time) {
	t.Helper()
	ctx := t.Context()
	s := handlers.repositoryMonitorStore
	require.NoError(t, s.CreateCommandEvent(ctx, &store.CommandEvent{ID: "cmd-own", MonitorNamespace: "demo", MonitorName: "audit-monitor",
		Kind: "issue", Number: 7, Source: "github_label", Author: "octocat", Intent: "implement", Status: "accepted", CreatedAt: now}))
	require.NoError(t, s.CreateCommandEvent(ctx, &store.CommandEvent{ID: "cmd-other", MonitorNamespace: "demo", MonitorName: "other-monitor",
		Kind: "issue", Number: 8, Source: "github_label", Author: "someone", Intent: "implement", Status: "accepted", CreatedAt: now}))
	require.NoError(t, s.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: "ghmut-push", MonitorNamespace: "demo", MonitorName: "audit-monitor",
		Operation: "push", Status: "succeeded", GitHubURL: "feature-branch", CreatedAt: now}))
	event := func(id, eventType string, at time.Time, metadata map[string]any, policy string) {
		t.Helper()
		e, err := store.NewRepositoryMonitorEvent(store.RepositoryMonitorEventInput{
			ID: id, MonitorNamespace: "demo", MonitorName: "audit-monitor", ItemKind: "pull_request", ItemNumber: 7, ItemSHA: "head7",
			EventType: eventType, Actor: "controller", Summary: id, Metadata: metadata, Policy: policy,
		})
		require.NoError(t, err)
		e.CreatedAt = at
		require.NoError(t, s.CreateMonitorEvent(ctx, e))
	}
	event("mevt-old-refusal", "repair_refused", now.Add(-48*time.Hour), map[string]any{"reason": "repair_pr_budget_exhausted"}, "spec.repair.maxRepairsPerPR=5")
	event("mevt-accepted", "command_accepted", now.Add(-2*time.Hour), map[string]any{"commandEventID": "cmd-own"}, "")
	event("mevt-run-started", "run_started", now.Add(-time.Hour), nil, "")
	event("mevt-repaired", "repair_succeeded", now.Add(-30*time.Minute), map[string]any{"commandEventID": "cmd-own", "taskName": "repair-task", "mutationID": "ghmut-push"}, "")
	event("mevt-rejected", "command_rejected", now.Add(-10*time.Minute), map[string]any{"commandEventID": "cmd-other", "reason": "actor_permission_not_allowed"}, "spec.triggers.github.labels.requireActorPermission=write")
}

func getRepositoryMonitorAudit(t *testing.T, app *fiber.App, query string, wantStatus int) repositoryMonitorAuditTestPage {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/monitors/repositories/audit-monitor/audit?namespace=demo"+query, nil))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck
	require.Equal(t, wantStatus, resp.StatusCode)
	var page repositoryMonitorAuditTestPage
	if wantStatus == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&page))
	}
	return page
}

func repositoryMonitorAuditIDs(records []RepositoryMonitorAuditRecord) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func TestRepositoryMonitorAuditJoinsDecisionsInWindow(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff,
		repositoryMonitorAuditTestMonitor("audit-monitor"), repositoryMonitorAuditTestMonitor("other-monitor"))
	seedRepositoryMonitorAudit(t, handlers, time.Now().UTC())

	page := getRepositoryMonitorAudit(t, app, "&since=24h", http.StatusOK)
	require.Equal(t, []string{"mevt-rejected", "mevt-repaired", "mevt-accepted"}, repositoryMonitorAuditIDs(page.Items),
		"decisions in the window, newest first, without lifecycle events")
	require.Empty(t, page.Metadata.Continue)

	rejected, repaired, accepted := page.Items[0], page.Items[1], page.Items[2]
	require.Equal(t, store.RepositoryMonitorDecisionRefused, rejected.Decision)
	require.Equal(t, "actor_permission_not_allowed", rejected.Reason)
	require.Equal(t, "spec.triggers.github.labels.requireActorPermission=write", rejected.Policy)
	require.Equal(t, &RepositoryMonitorAuditCommandRef{ID: "cmd-other"}, rejected.Command, "another monitor's command shows its ID only")

	require.Equal(t, store.RepositoryMonitorDecisionActed, repaired.Decision)
	require.Equal(t, &RepositoryMonitorAuditTaskRef{Name: "repair-task"}, repaired.Task)
	require.Equal(t, []RepositoryMonitorAuditMutationRef{{ID: "ghmut-push", Operation: "push", Status: "succeeded", GitHubURL: "feature-branch"}}, repaired.Mutations)
	require.Equal(t, &RepositoryMonitorAuditCommandRef{ID: "cmd-own", Intent: "implement", Source: "github_label", Author: "octocat", Status: "accepted"}, repaired.Command)

	require.Equal(t, store.RepositoryMonitorDecisionAccepted, accepted.Decision)
	require.Nil(t, accepted.Task)
	require.Empty(t, accepted.Mutations)

	refusals := getRepositoryMonitorAudit(t, app, "&since=72h&decision=refused", http.StatusOK)
	require.Equal(t, []string{"mevt-rejected", "mevt-old-refusal"}, repositoryMonitorAuditIDs(refusals.Items))

	earlier := getRepositoryMonitorAudit(t, app, "&since=24h&until=20m", http.StatusOK)
	require.Equal(t, []string{"mevt-repaired", "mevt-accepted"}, repositoryMonitorAuditIDs(earlier.Items), "until is exclusive")

	since := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339)
	absolute := getRepositoryMonitorAudit(t, app, "&since="+since, http.StatusOK)
	require.Len(t, absolute.Items, 3)
}

// A page holds at most limit decisions; lifecycle events between them never
// make a page skip or repeat a decision.
func TestRepositoryMonitorAuditPagesAcrossLifecycleEvents(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff,
		repositoryMonitorAuditTestMonitor("audit-monitor"), repositoryMonitorAuditTestMonitor("other-monitor"))
	seedRepositoryMonitorAudit(t, handlers, time.Now().UTC())

	var ids []string
	cont := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10, "paging terminates")
		page := getRepositoryMonitorAudit(t, app, "&since=72h&limit=1&continue="+cont, http.StatusOK)
		require.LessOrEqual(t, len(page.Items), 1)
		ids = append(ids, repositoryMonitorAuditIDs(page.Items)...)
		if page.Metadata.Continue == "" {
			break
		}
		cont = page.Metadata.Continue
	}
	require.Equal(t, []string{"mevt-rejected", "mevt-repaired", "mevt-accepted", "mevt-old-refusal"}, ids)
}

func TestRepositoryMonitorAuditRejectsInvalidQueries(t *testing.T) {
	app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff, repositoryMonitorAuditTestMonitor("audit-monitor"))
	for _, query := range []string{
		"&since=yesterday",
		"&until=-5m",
		"&since=1h&until=2h",
		"&decision=approved",
		"&limit=0",
		"&limit=501",
	} {
		t.Run(query, func(t *testing.T) {
			getRepositoryMonitorAudit(t, app, query, http.StatusBadRequest)
		})
	}
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/monitors/repositories/missing/audit?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
