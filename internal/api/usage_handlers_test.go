package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/usage"
)

func seedUsageAPIWork(t *testing.T, backend *sqlite.Store, namespace string, tokens int64) string {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour)
	work := store.UsageWorkID(namespace, "monitor", "org/repo", "issue", 1)
	require.NoError(t, backend.RegisterUsageWork(t.Context(), store.UsageWorkRequest{ID: work, Namespace: namespace, MonitorName: "monitor", MonitorUID: "monitor", Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: at}))
	require.NoError(t, backend.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: namespace, WorkID: work, TaskUID: "task", TaskName: "task", Phase: "Succeeded", StartedAt: at, PhaseObservedAt: at}))
	zero := int64(0)
	require.NoError(t, backend.RecordUsage(t.Context(), store.UsageObservation{Namespace: namespace, TaskUID: "task", ID: "call", CounterID: "call", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, Model: "model", InputTokens: &tokens, OutputTokens: &zero, Complete: true, Status: store.UsageStatusCompleted, ObservedAt: at}))
	require.NoError(t, backend.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: namespace, WorkID: work, Repository: "org/repo", Number: 2, Origin: store.UsagePRCreated, EvidenceID: "publication", LinkedAt: at}))
	require.NoError(t, backend.RecordUsagePullRequest(t.Context(), store.UsagePullRequest{Namespace: namespace, Repository: "org/repo", Number: 2, URL: "https://github.com/org/repo/pull/2", GitHubID: "PR_2", State: "merged", MergedAt: &at, ObservedAt: at}))
	return work
}

func TestUsageAPIReportAndDetailUseSameCohort(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	work := seedUsageAPIWork(t, f.store, "default", 1200)
	seedUsageAPIWork(t, f.store, "other", 9999)
	f.allowRoute(t, "GET /api/v1/usage")
	query := "?from=2000-01-01&repository=org/repo&model=model"
	status, body := f.request(t, http.MethodGet, "/api/v1/usage"+query, "")
	require.Equal(t, http.StatusOK, status, body)
	var report usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.EqualValues(t, 1200, report.Summary.TotalTokens)
	require.Equal(t, 1, report.Summary.PRsMerged)
	require.Equal(t, 1200.0, *report.Summary.TokensPerPRMerged)
	require.Len(t, report.Works, 1)
	require.Equal(t, "default", report.Works[0].Namespace)
	f.allowRoute(t, "GET /api/v1/usage/work/:id")
	status, body = f.request(t, http.MethodGet, "/api/v1/usage/work/"+work+query, "")
	require.Equal(t, http.StatusOK, status, body)
	var detail struct {
		Work usage.Work `json:"work"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Equal(t, report.Summary, detail.Work.Summary)
	status, _ = f.request(t, http.MethodGet, "/api/v1/usage?namespace=other", "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = f.request(t, http.MethodGet, "/api/v1/usage?from=invalid", "")
	require.Equal(t, http.StatusBadRequest, status)
}

func TestUsageAPICombinedTeamsRequireEveryNamespaceGrant(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	f.server.handlers.watchNamespace = ""
	f.server.handlers.enforceNamespaceIsolation = false
	seedUsageAPIWork(t, f.store, "default", 100)
	seedUsageAPIWork(t, f.store, "other", 200)
	f.allowRoute(t, "GET /api/v1/usage")
	path := "/api/v1/usage?teams=default,other&from=2000-01-01"
	status, _ := f.request(t, http.MethodGet, path, "")
	require.Equal(t, http.StatusForbidden, status)
	additional := make([]authorizationv1.ResourceAttributes, 0, 3)
	for _, resource := range []string{externalToolTaskResource, usageMonitorResource, usageSessionResource} {
		additional = append(additional, authorizationv1.ResourceAttributes{Namespace: "other", Group: corev1alpha1.GroupVersion.Group, Resource: resource, Verb: "list"})
	}
	f.allowRoute(t, "GET /api/v1/usage", additional...)
	status, body := f.request(t, http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, status, body)
	var report usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Len(t, report.Teams, 2)
	require.EqualValues(t, 300, report.Summary.TotalTokens)
	require.Equal(t, 1, report.Summary.PRsMerged)
	for _, resource := range []string{externalToolTaskResource, usageMonitorResource, usageSessionResource} {
		require.Contains(t, f.reviews, authorizationv1.SubjectAccessReviewSpec{User: f.user.Username, UID: f.user.UID, Groups: f.user.Groups,
			Extra:              map[string]authorizationv1.ExtraValue{"example.com/claims": {"a", "b"}},
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "other", Group: corev1alpha1.GroupVersion.Group, Resource: resource, Verb: "list"}})
	}
}

func TestUsageAPIGatewayAccessSurvivesTaskCleanup(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	seedUsageAPIWork(t, f.store, "default", 100)
	for _, object := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}},
		&gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "default", UID: "gateway-uid"}},
	} {
		require.NoError(t, f.kube.Create(t.Context(), object))
	}
	at := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, f.store.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "default", TaskUID: "private-task", TaskName: "private-task", Phase: "Succeeded", StartedAt: at,
		GatewayOwner: &store.UsageGatewayOwner{Namespace: "default", NamespaceUID: "namespace-uid", Name: "private", UID: "gateway-uid"}}))
	input, output := int64(7654), int64(0)
	require.NoError(t, f.store.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", TaskUID: "private-task", ID: "private-call", CounterID: "private-call", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, InputTokens: &input, OutputTokens: &output, Complete: true, ObservedAt: at}))
	f.allowRoute(t, "GET /api/v1/usage")
	status, body := f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01", "")
	require.Equal(t, http.StatusOK, status, body)
	require.NotContains(t, body, "private-task")
	require.NotContains(t, body, "7654")
	f.allowRoute(t, "GET /api/v1/usage", authorizationv1.ResourceAttributes{Namespace: "default", Group: gatewayv1alpha1.GroupVersion.Group, Resource: "gateways", Verb: "get", Name: "private"})
	status, body = f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01", "")
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "7654")
}

func TestUsageAPIContextTokensRequireReadScopesAndTeamContext(t *testing.T) {
	for _, tc := range []struct {
		name        string
		scopes      []string
		transaction map[string]any
		status      int
	}{
		{"missing scopes", []string{ContextTokenScopeTaskList}, nil, http.StatusForbidden},
		{"namespace", []string{ContextTokenScopeTaskList, ContextTokenScopeMonitorsRead, ContextTokenScopeSessionsRead}, map[string]any{"namespace": "default"}, http.StatusOK},
		{"other namespace", []string{ContextTokenScopeTaskList, ContextTokenScopeMonitorsRead, ContextTokenScopeSessionsRead}, map[string]any{"namespace": "other"}, http.StatusForbidden},
		{"object constraint", []string{ContextTokenScopeTaskList, ContextTokenScopeMonitorsRead, ContextTokenScopeSessionsRead}, map[string]any{"task": "task"}, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, app := setupTestHandlers()
			h.executionEventStore = newInternalExecutionEventStore(t)
			var err error
			h.contextTokenAuthorization, err = NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
			require.NoError(t, err)
			app.Use(func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeContextToken, Namespace: "default", ContextToken: &ContextToken{Scopes: tc.scopes, TransactionContext: tc.transaction}})
				return c.Next()
			})
			app.Get("/usage", h.GetUsageReport)
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/usage", nil))
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, tc.status, response.StatusCode)
		})
	}
}

func TestUsageWorkerRecordsUseAuthenticatedTaskIdentity(t *testing.T) {
	backend := newInternalExecutionEventStore(t)
	task, job, pod := testInternalExecutionEventOwnedWorkerObjects("owned-task")
	app := setupInternalExecutionEventAppWithClient(backend, testInternalExecutionEventClient(t, task, job, pod), testInternalExecutionEventWorkerUser("owned-task-pod"))
	input, output := int64(100), int64(20)
	observation := store.UsageObservation{Namespace: "other", TaskUID: "other-task", ID: "call", CounterID: "call", Scope: store.UsageScopeSession, Source: store.UsageSourceProvider,
		InputTokens: &input, OutputTokens: &output, Complete: true, ObservedAt: time.Now().UTC()}
	body := map[string]any{"type": events.ExecutionEventTypeModelUsageUpdated, "content": map[string]any{"usage": observation}}
	for range 2 {
		response := doJSONRequest(t, app, "/internal/v1/events/default/task/owned-task", body)
		require.Equal(t, http.StatusCreated, response.StatusCode)
		require.NoError(t, response.Body.Close())
	}
	data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"default"}, AsOf: time.Now().UTC()})
	require.NoError(t, err)
	require.Len(t, data.Observations, 1)
	require.Equal(t, "default", data.Observations[0].Namespace)
	require.Equal(t, string(task.UID), data.Observations[0].TaskUID)
	require.Equal(t, store.UsageScopeCall, data.Observations[0].Scope)
	body["content"] = map[string]any{"harnessV2": map[string]any{"taskUID": "other-task"}}
	response := doJSONRequest(t, app, "/internal/v1/events/default/task/owned-task", body)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestUsageDetachedStreamingContextRetainsRecorder(t *testing.T) {
	backend := newInternalExecutionEventStore(t)
	ctx, cancel := context.WithCancel(usageRequestContext(t.Context(), backend, "default", "session"))
	detached := detachedSpanContext(ctx)
	cancel()
	require.NoError(t, detached.Err())
	// A real provider call exercises the copied recorder after cancellation of
	// the request context, as happens with SendStreamWriter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"response","object":"response","model":"model","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`))
	}))
	t.Cleanup(server.Close)
	provider, err := llm.NewProvider("openai", llm.ProviderConfig{APIKey: "fixture", BaseURL: server.URL})
	require.NoError(t, err)
	_, err = provider.Complete(detached, &llm.CompletionRequest{Model: "model", Messages: []llm.Message{{Role: "user", Content: "fixture"}}})
	require.NoError(t, err)
	data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"default"}, AsOf: time.Now().UTC()})
	require.NoError(t, err)
	require.Len(t, data.Observations, 2)
	require.Equal(t, "session", data.Observations[0].SessionName)
}
