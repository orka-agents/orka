package api

import (
	"context"
	"encoding/json"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	namespaceUID := "namespace-uid"
	if namespace != "default" {
		namespaceUID = namespace + "-namespace-uid"
	}
	work := store.UsageWorkID(namespace, "monitor", "org/repo", "issue", 1)
	require.NoError(t, backend.RegisterUsageWork(t.Context(), store.UsageWorkRequest{ID: work, Namespace: namespace, NamespaceUID: namespaceUID, MonitorName: "monitor", MonitorUID: "monitor", Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: at}))
	require.NoError(t, backend.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: namespace, WorkID: work, TaskUID: "task", TaskName: "task", Phase: "Succeeded", StartedAt: at, PhaseObservedAt: at}))
	zero := int64(0)
	require.NoError(t, backend.RecordUsage(t.Context(), store.UsageObservation{Namespace: namespace, TaskUID: "task", ID: "call", CounterID: "call", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, Model: "model", InputTokens: &tokens, OutputTokens: &zero, Complete: true, Status: store.UsageStatusCompleted, ObservedAt: at}))
	require.NoError(t, backend.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: namespace, WorkID: work, Repository: "org/repo", Number: 2, Origin: store.UsagePRCreated, EvidenceID: "publication", LinkedAt: at}))
	require.NoError(t, backend.RecordUsagePullRequest(t.Context(), store.UsagePullRequest{Namespace: namespace, NamespaceUID: namespaceUID, Repository: "org/repo", Number: 2, URL: "https://github.com/org/repo/pull/2", GitHubID: "PR_2", State: "merged", MergedAt: &at, ObservedAt: at}))
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
	require.Equal(t, &usage.Page{Limit: 25, Total: 1}, report.Page)
	require.NotContains(t, body, `"tasks"`)
	require.NotContains(t, body, `"measurements":[`)
	require.NotContains(t, body, `"pullRequests"`)
	f.allowRoute(t, "GET /api/v1/usage/work/:id")
	status, body = f.request(t, http.MethodGet, "/api/v1/usage/work/"+work+query, "")
	require.Equal(t, http.StatusOK, status, body)
	var detail struct {
		Work usage.Work `json:"work"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Equal(t, report.Summary, detail.Work.Summary)
	require.Len(t, detail.Work.Tasks, 1)
	require.Len(t, detail.Work.Tasks[0].Measurements, 1)
	status, _ = f.request(t, http.MethodGet, "/api/v1/usage?namespace=other", "")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = f.request(t, http.MethodGet, "/api/v1/usage?from=invalid", "")
	require.Equal(t, http.StatusBadRequest, status)
}

func TestUsageAPIRejectsDatesOutsideStoredTimestampRange(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	seedUsageAPIWork(t, f.store, "default", 100)
	f.allowRoute(t, "GET /api/v1/usage")
	for _, query := range []string{
		"from=0001-01-01", "from=1677-09-21T00:12:43.145224191Z",
		"until=2262-04-11T23:47:16.854775808Z", "until=9999-12-31",
		"asOf=1000-01-01", "from=0001-01-01&asOf=1000-01-01",
	} {
		t.Run(query, func(t *testing.T) {
			status, body := f.request(t, http.MethodGet, "/api/v1/usage?"+query, "")
			require.Equal(t, http.StatusBadRequest, status, body)
			require.Contains(t, body, "supported timestamp range")
			require.NotContains(t, body, `"summary"`)
		})
	}
	status, body := f.request(t, http.MethodGet,
		"/api/v1/usage?from=1677-09-21T00:12:43.145224192Z&until=2262-04-11T23:47:16.854775807Z", "")
	require.Equal(t, http.StatusOK, status, body)
	var report usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.EqualValues(t, 100, report.Summary.TotalTokens)
}

func TestUsageAPIPaginatesSummariesAndOtherDetails(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	seedUsageAPIWork(t, f.store, "default", 100)
	at := time.Now().UTC().Add(-time.Minute)
	for i := range 27 {
		id := fmt.Sprintf("chat-%02d", i)
		require.NoError(t, f.store.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", NamespaceUID: "namespace-uid",
			ID: id, CounterID: id, Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, Model: "model",
			InputTokens: new(int64(10)), OutputTokens: new(int64(0)), Complete: true, Status: store.UsageStatusCompleted, ObservedAt: at}))
	}
	f.allowRoute(t, "GET /api/v1/usage")
	status, body := f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01&limit=1&offset=1", "")
	require.Equal(t, http.StatusOK, status, body)
	var report usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Empty(t, report.Works)
	require.EqualValues(t, 100, report.Summary.TotalTokens)
	require.Equal(t, &usage.Page{Limit: 1, Offset: 1, Total: 1}, report.Page)
	require.EqualValues(t, 270, report.OtherWork[2].Totals.TotalTokens)
	require.Equal(t, 27, report.OtherWork[2].TaskCount)
	require.NotContains(t, body, `"tasks"`)
	f.allowRoute(t, "GET /api/v1/usage/other/:category")
	status, body = f.request(t, http.MethodGet, "/api/v1/usage/other/unassociated?from=2000-01-01&limit=25&offset=25&asOf="+report.Selection.AsOf.Format(time.RFC3339Nano), "")
	require.Equal(t, http.StatusOK, status, body)
	var detail struct {
		OtherWork usage.OtherWork `json:"otherWork"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Equal(t, report.OtherWork[2].Totals, detail.OtherWork.Totals)
	require.Equal(t, &usage.Page{Limit: 25, Offset: 25, Total: 27}, detail.OtherWork.Page)
	require.Len(t, detail.OtherWork.Tasks, 2)
	require.Equal(t, "call-chat-25", detail.OtherWork.Tasks[0].TaskUID)
	require.Len(t, detail.OtherWork.Tasks[0].Measurements, 1)
	for _, query := range []string{"limit=0", "offset=-1", "limit=invalid"} {
		status, _ = f.request(t, http.MethodGet, "/api/v1/usage?"+query, "")
		require.Equal(t, http.StatusBadRequest, status)
	}
	status, body = f.request(t, http.MethodGet, "/api/v1/usage?limit=1000", "")
	require.Equal(t, http.StatusOK, status, body)
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Equal(t, 100, report.Page.Limit)
}

func TestUsageAPIRejectsOversizedReportsAndLoadsRequestedWork(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	work := seedUsageAPIWork(t, f.store, "default", 100)
	at := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, f.store.WithTaskDataTransaction(t.Context(), func(ctx context.Context) error {
		for i := range usageMaxRecords {
			id := fmt.Sprint("chat-", i)
			if err := f.store.RecordUsage(ctx, store.UsageObservation{Namespace: "default", NamespaceUID: "namespace-uid", ID: id,
				CounterID: id, Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, ObservedAt: at}); err != nil {
				return err
			}
		}
		return nil
	}))
	f.allowRoute(t, "GET /api/v1/usage")
	status, body := f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01&limit=1", "")
	require.Equal(t, http.StatusUnprocessableEntity, status, body)
	require.Contains(t, body, "narrow")
	require.NotContains(t, body, `"summary"`)
	f.allowRoute(t, "GET /api/v1/usage/work/:id")
	status, body = f.request(t, http.MethodGet, "/api/v1/usage/work/"+work, "")
	require.Equal(t, http.StatusOK, status, body)
	var detail struct {
		Work usage.Work `json:"work"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.EqualValues(t, 100, detail.Work.Summary.TotalTokens)
	require.Len(t, detail.Work.Tasks, 1)
}

func TestUsageAPIRejectsOutOfRangeTotals(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	work := seedUsageAPIWork(t, f.store, "default", store.MaxUsageTokenCount)
	f.allowRoute(t, "GET /api/v1/usage")
	status, body := f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01", "")
	require.Equal(t, http.StatusOK, status, body)
	var report usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Equal(t, store.MaxUsageTokenCount, report.Summary.TotalTokens)
	require.Contains(t, body, `"totalTokens":9007199254740991`)
	// Both observations are individually valid, but the selected total is not.
	require.NoError(t, f.store.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", TaskUID: "task", ID: "extra",
		CounterID: "extra", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, InputTokens: new(int64(1)), OutputTokens: new(int64(0)),
		Complete: true, Status: store.UsageStatusCompleted, ObservedAt: time.Now().UTC().Add(-time.Minute)}))
	for _, route := range []struct{ pattern, path string }{
		{"GET /api/v1/usage", "/api/v1/usage?from=2000-01-01"},
		{"GET /api/v1/usage/work/:id", "/api/v1/usage/work/" + work},
	} {
		f.allowRoute(t, route.pattern)
		status, body = f.request(t, http.MethodGet, route.path, "")
		require.Equal(t, http.StatusUnprocessableEntity, status, body)
		require.Contains(t, body, "usage totals exceed the reporting limit")
		require.NotContains(t, body, `"summary"`)
	}
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

func TestUsageAPIHidesHistoryAfterNamespaceRecreation(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	oldWork := seedUsageAPIWork(t, f.store, "default", 7654)
	f.allowRoute(t, "GET /api/v1/usage")
	status, body := f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01", "")
	require.Equal(t, http.StatusOK, status, body)
	var before usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &before))
	require.EqualValues(t, 7654, before.Summary.TotalTokens)
	require.NoError(t, f.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
	require.NoError(t, f.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "replacement-uid"}}))

	// Token-count digits in a timestamp must not be mistaken for leaked usage.
	at := time.Now().UTC().Add(-time.Minute).Truncate(time.Second).Add(9999 * time.Nanosecond)
	work := store.UsageWorkID("default", "replacement-monitor", "org/repo", "issue", 1)
	require.NoError(t, f.store.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "default", NamespaceUID: "replacement-uid",
		MonitorName: "monitor", MonitorUID: "replacement-monitor", Repository: "org/repo", Kind: "issue", Number: 1, StartedAt: at}))
	require.NoError(t, f.store.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "default", WorkID: work, TaskUID: "replacement-task",
		TaskName: "task", Phase: "Succeeded", StartedAt: at, PhaseObservedAt: at}))
	require.NoError(t, f.store.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", TaskUID: "replacement-task",
		ID: "replacement-call", CounterID: "call", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, Model: "model",
		InputTokens: new(int64(100)), OutputTokens: new(int64(0)), Complete: true, Status: store.UsageStatusCompleted, ObservedAt: at}))
	// Reusing the same repository and PR number must not import the prior
	// namespace's verified merge, even when the current link has no observation.
	require.NoError(t, f.store.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: "default", WorkID: work,
		Repository: "org/repo", Number: 2, Origin: store.UsagePRCreated, EvidenceID: "replacement-publication", LinkedAt: at}))
	// Unowned accounting and old native chat records must also remain hidden.
	for _, uid := range []string{"", "namespace-uid"} {
		require.NoError(t, f.store.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", NamespaceUID: uid,
			ID: "chat-" + uid, CounterID: "chat-" + uid, Scope: store.UsageScopeCall, Source: store.UsageSourceProvider,
			InputTokens: new(int64(9999)), OutputTokens: new(int64(0)), Complete: true, Status: store.UsageStatusCompleted, ObservedAt: at}))
	}
	status, body = f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01", "")
	require.Equal(t, http.StatusOK, status, body)
	var report usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.EqualValues(t, 100, report.Summary.TotalTokens)
	require.Zero(t, report.Summary.PRsMerged)
	require.Len(t, report.Works, 1)
	require.Equal(t, work, report.Works[0].ID)
	require.Equal(t, "replacement-uid", report.Works[0].NamespaceUID)
	require.EqualValues(t, 100, report.Works[0].Summary.TotalTokens)
	require.Len(t, report.Teams, 1)
	require.EqualValues(t, 100, report.Teams[0].Summary.TotalTokens)
	for _, other := range report.OtherWork {
		require.Zero(t, other.Totals.TotalTokens, other.Category)
		require.Zero(t, other.TaskCount, other.Category)
	}
	require.NotContains(t, body, oldWork)
	f.allowRoute(t, "GET /api/v1/usage/work/:id")
	status, _ = f.request(t, http.MethodGet, "/api/v1/usage/work/"+oldWork, "")
	require.Equal(t, http.StatusNotFound, status)
	status, body = f.request(t, http.MethodGet, "/api/v1/usage/work/"+work, "")
	require.Equal(t, http.StatusOK, status, body)
	var detail struct {
		Work usage.Work `json:"work"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &detail))
	require.Equal(t, report.Summary, detail.Work.Summary)
}

func TestUsageAPIFailsClosedWithoutStableNamespaceIdentity(t *testing.T) {
	for _, tc := range []string{"missing namespace", "missing UID", "recreated during report"} {
		t.Run(tc, func(t *testing.T) {
			f := newExternalAuthorizationFixture(t)
			work := seedUsageAPIWork(t, f.store, "default", 7654)
			reads := 0
			f.server.handlers.apiReader = interceptor.NewClient(f.kube.(client.WithWatch), interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if err := c.Get(ctx, key, object, opts...); err != nil {
						return err
					}
					if namespace, ok := object.(*corev1.Namespace); ok {
						reads++
						if tc == "missing UID" {
							namespace.UID = ""
						} else if tc == "recreated during report" && reads%2 == 0 {
							namespace.UID = "replacement-uid"
						}
					}
					return nil
				},
			})
			if tc == "missing namespace" {
				require.NoError(t, f.kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
			}
			for _, route := range []struct{ key, path string }{
				{"GET /api/v1/usage", "/api/v1/usage?from=2000-01-01"},
				{"GET /api/v1/usage/work/:id", "/api/v1/usage/work/" + work},
			} {
				f.allowRoute(t, route.key)
				status, body := f.request(t, http.MethodGet, route.path, "")
				require.Equal(t, http.StatusServiceUnavailable, status, body)
				require.NotContains(t, body, "7654")
			}
		})
	}
}

func TestUsageAPIGatewayAccessSurvivesTaskCleanup(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	seedUsageAPIWork(t, f.store, "default", 100)
	require.NoError(t, f.kube.Create(t.Context(), &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "default", UID: "gateway-uid"}}))
	at := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, f.store.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "default", NamespaceUID: "namespace-uid", TaskUID: "private-task", TaskName: "private-task", Phase: "Succeeded", StartedAt: at,
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

func TestUsageAPIHidesEveryWorkSharingAnInaccessiblePRTask(t *testing.T) {
	f := newExternalAuthorizationFixture(t)
	first := seedUsageAPIWork(t, f.store, "default", 100)
	at := time.Now().UTC().Add(-30 * time.Minute)
	works := map[int64]string{1: first}
	for _, number := range []int64{3, 4} {
		work := store.UsageWorkID("default", "monitor", "org/repo", "issue", number)
		works[number] = work
		require.NoError(t, f.store.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "default", NamespaceUID: "namespace-uid",
			MonitorName: "monitor", MonitorUID: "monitor", Repository: "org/repo", Kind: "issue", Number: number, StartedAt: at}))
		task := fmt.Sprint("task-", number)
		require.NoError(t, f.store.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "default", TaskUID: task, TaskName: task,
			WorkID: work, Phase: "Succeeded", StartedAt: at}))
		require.NoError(t, f.store.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", TaskUID: task, ID: task, CounterID: task,
			Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, InputTokens: new(int64(100)), OutputTokens: new(int64(0)), Complete: true, ObservedAt: at}))
		prNumber := int64(2)
		if number == 4 {
			prNumber = 4
		}
		require.NoError(t, f.store.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: "default", WorkID: work, Repository: "org/repo",
			Number: prNumber, Origin: store.UsagePRCreated, EvidenceID: "publication", LinkedAt: at}))
	}
	require.NoError(t, f.kube.Create(t.Context(), &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "default", UID: "gateway-uid"}}))
	reviewWork := store.UsageWorkID("default", "monitor", "org/repo", "pull_request", 2)
	require.NoError(t, f.store.RegisterUsageWork(t.Context(), store.UsageWorkRequest{Namespace: "default", NamespaceUID: "namespace-uid",
		MonitorName: "monitor", MonitorUID: "monitor", Repository: "org/repo", Kind: "pull_request", Number: 2, StartedAt: at}))
	require.NoError(t, f.store.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: "default", TaskUID: "private-review", TaskName: "private-review",
		WorkID: reviewWork, Repository: "org/repo", PRNumber: 2, Phase: "Succeeded", StartedAt: at,
		GatewayOwner: &store.UsageGatewayOwner{Namespace: "default", NamespaceUID: "namespace-uid", Name: "private", UID: "gateway-uid"}}))
	require.NoError(t, f.store.RecordUsage(t.Context(), store.UsageObservation{Namespace: "default", TaskUID: "private-review", ID: "private", CounterID: "private",
		Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, InputTokens: new(int64(7654)), OutputTokens: new(int64(0)), Complete: true, ObservedAt: at}))
	f.allowRoute(t, "GET /api/v1/usage")
	status, body := f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01", "")
	require.Equal(t, http.StatusOK, status, body)
	var report usage.Report
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Len(t, report.Works, 1)
	require.Equal(t, works[4], report.Works[0].ID)
	require.EqualValues(t, 100, report.Summary.TotalTokens)
	require.NotContains(t, body, "7654")
	f.allowRoute(t, "GET /api/v1/usage/work/:id")
	for _, work := range []string{first, works[3]} {
		status, body = f.request(t, http.MethodGet, "/api/v1/usage/work/"+work, "")
		require.Equal(t, http.StatusNotFound, status, body)
	}
	f.allowRoute(t, "GET /api/v1/usage", authorizationv1.ResourceAttributes{Namespace: "default", Group: gatewayv1alpha1.GroupVersion.Group,
		Resource: "gateways", Verb: "get", Name: "private"})
	status, body = f.request(t, http.MethodGet, "/api/v1/usage?from=2000-01-01", "")
	require.Equal(t, http.StatusOK, status, body)
	require.NoError(t, json.Unmarshal([]byte(body), &report))
	require.Len(t, report.Works, 3)
	require.EqualValues(t, 7954, report.Summary.TotalTokens, "shared review usage is counted once after authorization")
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
			h.apiReader = testInternalExecutionEventClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}})
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
	task.Spec.Type = corev1alpha1.TaskTypeAI
	pod.Spec.Containers = []corev1.Container{{Name: "worker", Command: []string{"/worker"}, Args: []string{"--mode=ai"}}}
	require.NoError(t, backend.RegisterUsageTask(t.Context(), store.UsageTask{Namespace: task.Namespace, NamespaceUID: "namespace-uid", TaskUID: string(task.UID), TaskName: task.Name, Phase: "Running"}))
	app := setupInternalExecutionEventAppWithClient(backend, testInternalExecutionEventClient(t, task, job, pod), testInternalExecutionEventWorkerUser("owned-task-pod"))
	input, output := int64(100), int64(20)
	before := time.Now().UTC()
	observation := store.UsageObservation{Namespace: "other", NamespaceUID: "other-namespace-uid", TaskUID: "other-task", ID: "call", CounterID: "call", Scope: store.UsageScopeSession, Source: store.UsageSourceEstimate,
		AttemptID: "other-attempt", Status: store.UsageStatusCompleted, InputTokens: &input, OutputTokens: &output, Complete: true, ObservedAt: before.Add(24 * time.Hour)}
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
	require.Equal(t, "namespace-uid", data.Observations[0].NamespaceUID)
	require.Equal(t, string(task.UID), data.Observations[0].TaskUID)
	require.Equal(t, store.UsageScopeCall, data.Observations[0].Scope)
	require.Equal(t, store.UsageSourceProvider, data.Observations[0].Source)
	require.Empty(t, data.Observations[0].AttemptID)
	require.False(t, data.Observations[0].ObservedAt.Before(before))
	require.False(t, data.Observations[0].ObservedAt.After(time.Now().UTC()))
	body["content"] = map[string]any{"harnessV2": map[string]any{"taskUID": "other-task"}}
	response := doJSONRequest(t, app, "/internal/v1/events/default/task/owned-task", body)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
}

func TestUsageWorkerRejectsCacheBreakdownsExceedingInput(t *testing.T) {
	for _, counts := range [][3]int64{{0, 100, 0}, {0, 0, 100}, {100, 60, 50}} {
		t.Run(fmt.Sprint(counts), func(t *testing.T) {
			backend := newInternalExecutionEventStore(t)
			task, job, pod := testInternalExecutionEventOwnedWorkerObjects("owned-task")
			task.Spec.Type = corev1alpha1.TaskTypeAI
			pod.Spec.Containers = []corev1.Container{{Name: "worker", Command: []string{"/worker"}, Args: []string{"--mode=ai"}}}
			app := setupInternalExecutionEventAppWithClient(backend, testInternalExecutionEventClient(t, task, job, pod), testInternalExecutionEventWorkerUser(pod.Name))
			observation := store.UsageObservation{ID: "call", CounterID: "call", Status: store.UsageStatusCompleted,
				InputTokens: &counts[0], OutputTokens: new(int64(0)), CachedInputTokens: &counts[1], CacheWriteInputTokens: &counts[2],
				Complete: true, ObservedAt: time.Now().UTC()}
			body := map[string]any{"type": events.ExecutionEventTypeModelUsageUpdated, "content": map[string]any{"usage": observation}}
			response := doJSONRequest(t, app, "/internal/v1/events/default/task/owned-task", body)
			require.Equal(t, http.StatusBadRequest, response.StatusCode)
			require.NoError(t, response.Body.Close())
			data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"default"}, AsOf: time.Now().UTC()})
			require.NoError(t, err)
			require.Empty(t, data.Observations)
			journal, err := backend.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "default", StreamType: "task", StreamID: "owned-task"})
			require.NoError(t, err)
			require.Empty(t, journal, "invalid worker usage must not commit its execution event")
		})
	}
}

func TestUsageWorkerAuthorityRejectsUntrustedExecution(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind corev1alpha1.TaskType
		args []string
	}{
		{"managed container", corev1alpha1.TaskTypeContainer, []string{"--mode=ai"}},
		{"ACP runtime", corev1alpha1.TaskTypeAgent, []string{"--mode=ai"}},
		{"changed task type", corev1alpha1.TaskTypeAI, []string{"sh", "-c", "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := newInternalExecutionEventStore(t)
			task, job, pod := testInternalExecutionEventOwnedWorkerObjects("owned-task")
			task.Spec.Type = tc.kind
			pod.Spec.Containers = []corev1.Container{{Name: "worker", Command: []string{"/worker"}, Args: tc.args}}
			app := setupInternalExecutionEventAppWithClient(backend, testInternalExecutionEventClient(t, task, job, pod), testInternalExecutionEventWorkerUser(pod.Name))
			body := map[string]any{"type": events.ExecutionEventTypeModelUsageUpdated, "content": map[string]any{"usage": store.UsageObservation{
				ID: "call", CounterID: "call", Scope: store.UsageScopeCall, Source: store.UsageSourceProvider, Status: store.UsageStatusCompleted,
				InputTokens: new(int64(100)), OutputTokens: new(int64(0)), ObservedAt: time.Now().UTC()}}}
			response := doJSONRequest(t, app, "/internal/v1/events/default/task/owned-task", body)
			require.Equal(t, http.StatusForbidden, response.StatusCode)
			require.NoError(t, response.Body.Close())
			data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"default"}, AsOf: time.Now().UTC()})
			require.NoError(t, err)
			require.Empty(t, data.Observations)
		})
	}
}

func TestUsageDetachedStreamingContextRetainsRecorder(t *testing.T) {
	backend := newInternalExecutionEventStore(t)
	kube := testInternalExecutionEventClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}})
	reader := interceptor.NewClient(kube.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	ctx, cancel := context.WithCancel(usageRequestContext(t.Context(), backend, reader, "default", "session"))
	detached := detachedSpanContext(ctx)
	cancel()
	require.NoError(t, detached.Err())
	// A real provider call exercises the copied recorder after cancellation of
	// the request context, as happens with SendStreamWriter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, kube.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}))
		require.NoError(t, kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "replacement-uid"}}))
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
	for _, observation := range data.Observations {
		require.Equal(t, "namespace-uid", observation.NamespaceUID)
	}
	current, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"default"}, NamespaceUIDs: map[string]string{"default": "replacement-uid"}})
	require.NoError(t, err)
	require.Empty(t, current.Observations)
}
