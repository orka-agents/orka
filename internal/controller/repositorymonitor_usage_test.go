package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func usageGitHubPR(number int64) map[string]any {
	return map[string]any{"id": fmt.Sprint("PR_", number), "number": number, "url": fmt.Sprintf("https://github.com/org/repo/pull/%d", number),
		"state": "OPEN", "createdAt": "2026-01-01T00:00:00Z", "mergedAt": nil, "closedAt": nil,
		"headRefOid": "head-one", "isDraft": false, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED"}
}

func TestUsageGitHubReadinessAndVerifiedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, key      string
		value          any
		ready, invalid bool
	}{
		{name: "approved", ready: true},
		{name: "no review requirement", key: "reviewDecision", value: nil, ready: true},
		{name: "draft", key: "isDraft", value: true},
		{name: "blocked checks", key: "mergeStateStatus", value: "BLOCKED"},
		{name: "nonpassing status", key: "mergeStateStatus", value: "UNSTABLE"},
		{name: "unknown mergeability", key: "mergeable", value: "UNKNOWN"},
		{name: "review required", key: "reviewDecision", value: "REVIEW_REQUIRED"},
		{name: "changes requested", key: "reviewDecision", value: "CHANGES_REQUESTED"},
		{name: "unknown review", key: "reviewDecision", value: "UNKNOWN"},
		{name: "missing head", key: "headRefOid", value: ""},
		{name: "closed", key: "state", value: "CLOSED"},
		{name: "different number", key: "number", value: 99, invalid: true},
		{name: "canonical repository casing", key: "url", value: "https://github.com/Org/Repo/pull/1", ready: true},
		{name: "different URL", key: "url", value: "https://github.com/other/repo/pull/1", invalid: true},
		{name: "missing identity", key: "id", value: "", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := usageGitHubPR(1)
			if tc.key != "" {
				pr[tc.key] = tc.value
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
					http.NotFound(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "org/repo", "pullRequest": pr}}})
			}))
			t.Cleanup(server.Close)
			r := &RepositoryMonitorReconciler{GitHubAPIBaseURL: server.URL, HTTPClient: server.Client()}
			got, err := r.fetchUsagePullRequest(t.Context(), "org/repo", 1, "fixture")
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.ready, got.Ready)
			require.Equal(t, "PR_1", got.GitHubID)
			require.Equal(t, pr["headRefOid"], got.HeadSHA)
		})
	}
}

func TestUsageCreatedPullRequestValidatesCanonicalRepositoryIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		valid     bool
	}{
		{name: "canonical casing", url: "https://github.com/Org/Repo/pull/7", valid: true},
		{name: "different repository", url: "https://github.com/Other/Repo/pull/7"},
		{name: "different number", url: "https://github.com/Org/Repo/pull/8"},
		{name: "different host", url: "https://example.com/Org/Repo/pull/7"},
		{name: "different path", url: "https://github.com/Org/Repo/issues/7"},
		{name: "query", url: "https://github.com/Org/Repo/pull/7?extra=1"},
		{name: "userinfo", url: "https://user@github.com/Org/Repo/pull/7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			monitor, secret := repositoryMonitorInventoryTestObjects("monitor")
			monitor.UID = "monitor-uid"
			monitor.Spec.RepoURL = "https://github.com/ORG/REPO.git"
			monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: secret.Name}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, "[]")
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "html_url": tc.url})
			}))
			t.Cleanup(server.Close)
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: monitor.Namespace, UID: "namespace-uid"}}
			r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, monitor, secret).Build(),
				Store: backend, GitHubAPIBaseURL: server.URL, HTTPClient: server.Client()}
			prURL, number, err := r.createIssueImplementationPullRequest(t.Context(), monitor,
				&store.MonitorItem{Number: 1, Title: "Implement issue"},
				&corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", UID: "task-uid"}}, "issue-1", "")
			if tc.valid {
				require.NoError(t, err)
				require.Equal(t, tc.url, prURL)
				require.Equal(t, 7, number)
			} else {
				require.Error(t, err)
			}
			data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{monitor.Namespace}, AsOf: time.Now().UTC()})
			require.NoError(t, err)
			if tc.valid {
				require.Len(t, data.Links, 1)
				require.Equal(t, store.UsagePRCreated, data.Links[0].Origin)
				require.Equal(t, "org/repo", data.Links[0].Repository)
			} else {
				require.Empty(t, data.Links)
			}
		})
	}
}

func TestUsageGitHubRefreshIsBoundedAndContinuesAfterTasks(t *testing.T) {
	backend := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	monitor, secret := repositoryMonitorInventoryTestObjects("monitor")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var query struct {
			Variables struct {
				Number int64 `json:"number"`
			} `json:"variables"`
		}
		_ = json.NewDecoder(r.Body).Decode(&query)
		pr := usageGitHubPR(query.Variables.Number)
		pr["state"], pr["mergedAt"] = "MERGED", "2026-01-02T00:00:00Z"
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{"nameWithOwner": "org/repo", "pullRequest": pr}}})
	}))
	t.Cleanup(server.Close)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: monitor.Namespace, UID: "namespace-uid"}}
	r := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, monitor, secret).Build(), Store: backend, GitHubAPIBaseURL: server.URL, HTTPClient: server.Client()}
	work, err := r.prepareMonitorUsageWork(t.Context(), monitor, "org/repo", "issue", 1)
	require.NoError(t, err)
	// No Task needs to survive for the retained publication links to refresh.
	for number := int64(1); number <= 25; number++ {
		require.NoError(t, backend.LinkUsagePullRequest(t.Context(), store.UsagePRLink{Namespace: monitor.Namespace, WorkID: work, Repository: "org/repo", Number: number, Origin: store.UsagePRCreated, EvidenceID: "publication"}))
	}
	require.NoError(t, r.refreshMonitorUsageOutcomes(t.Context(), monitor))
	require.EqualValues(t, 20, requests.Load())
	require.NoError(t, r.refreshMonitorUsageOutcomes(t.Context(), monitor))
	require.EqualValues(t, 25, requests.Load())
	require.NoError(t, r.refreshMonitorUsageOutcomes(t.Context(), monitor))
	require.EqualValues(t, 25, requests.Load())
	data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{monitor.Namespace}, AsOf: time.Now().UTC()})
	require.NoError(t, err)
	require.Len(t, data.PullRequests, 25)
	for _, pr := range data.PullRequests {
		require.Equal(t, "namespace-uid", pr.NamespaceUID)
		require.Equal(t, "merged", pr.State)
		require.NotNil(t, pr.MergedAt)
		require.False(t, pr.Ready)
	}
}

func TestUsageDelegationInheritsVerifiedWorkAndSession(t *testing.T) {
	backend := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "monitor", Namespace: "team", UID: "monitor-uid"}}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "namespace-uid"}}).Build()
	r := &RepositoryMonitorReconciler{Client: kube, Store: backend}
	work, err := r.prepareMonitorUsageWork(t.Context(), monitor, "org/repo", "issue", 1)
	require.NoError(t, err)
	again, err := r.prepareMonitorUsageWork(t.Context(), monitor, "ORG/REPO", "issue", 1)
	require.NoError(t, err)
	require.Equal(t, work, again)
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "team", UID: "parent-uid", CreationTimestamp: metav1.Now()},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, SessionRef: &corev1alpha1.SessionReference{Name: "conversation"}}}
	require.NoError(t, kube.Create(t.Context(), parent))
	require.NoError(t, r.retainMonitorUsageTask(t.Context(), parent, work, "planning", 0))
	tasks := &TaskReconciler{Client: kube, ExecutionEventStore: backend}
	require.NoError(t, tasks.retainUsageTask(t.Context(), parent))
	controller := true
	child := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "team", UID: "child-uid", CreationTimestamp: metav1.Now(),
		OwnerReferences: []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parent.UID, Controller: &controller}}},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, SessionRef: &corev1alpha1.SessionReference{Name: "conversation"}}}
	require.NoError(t, kube.Create(t.Context(), child))
	require.NoError(t, tasks.retainUsageTask(t.Context(), child))
	data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"team"}, AsOf: time.Now().UTC()})
	require.NoError(t, err)
	require.Len(t, data.Works, 1)
	require.Len(t, data.Tasks, 2)
	for _, task := range data.Tasks {
		require.Equal(t, "namespace-uid", task.NamespaceUID)
		require.Equal(t, work, task.WorkID)
		require.Equal(t, "conversation", task.SessionName)
	}
}

func TestUsageRetentionRejectsStaleObjectsAfterNamespaceRecreation(t *testing.T) {
	backend := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "old-namespace"}}
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "monitor", Namespace: "team", UID: "old-monitor"}}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "team", UID: "old-task"}}
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, monitor, task).Build()
	currentNamespace, currentMonitor, currentTask := namespace.DeepCopy(), monitor.DeepCopy(), task.DeepCopy()
	currentNamespace.UID, currentMonitor.UID, currentTask.UID = "new-namespace", "new-monitor", "new-task"
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentNamespace, currentMonitor, currentTask).Build()
	monitors := &RepositoryMonitorReconciler{Client: cached, APIReader: reader, Store: backend}
	tasks := &TaskReconciler{Client: cached, APIReader: reader, ExecutionEventStore: backend}
	_, err := monitors.prepareMonitorUsageWork(t.Context(), monitor, "org/repo", "issue", 1)
	require.ErrorContains(t, err, "usage source identity changed")
	require.ErrorContains(t, tasks.retainUsageTask(t.Context(), task), "usage source identity changed")
	data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"team"}})
	require.NoError(t, err)
	require.Empty(t, data.Works)
	require.Empty(t, data.Tasks)

	_, err = monitors.prepareMonitorUsageWork(t.Context(), currentMonitor, "org/repo", "issue", 1)
	require.NoError(t, err)
	require.NoError(t, tasks.retainUsageTask(t.Context(), currentTask))
	data, err = backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{"team"}, NamespaceUIDs: map[string]string{"team": "new-namespace"}})
	require.NoError(t, err)
	require.Len(t, data.Works, 1)
	require.Len(t, data.Tasks, 1)
	require.Equal(t, "new-namespace", data.Works[0].NamespaceUID)
	require.Equal(t, "new-namespace", data.Tasks[0].NamespaceUID)
}
