package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRepositoryMonitorMarksFixedFindingThreads(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var patched []string
	var resolved []string
	currentHead := "moved99"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(req.Body)
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/pulls/1":
			_, _ = w.Write([]byte(`{"number":1,"state":"open","draft":false,"base":{"ref":"main","sha":"base"},"head":{"ref":"feature","sha":"` + currentHead + `","repo":{"full_name":"orka-agents/orka"}},"labels":[]}`))
		case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/pulls/1/reviews/123/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 501, "body": "**P1: Bug**\n\n<!-- orka:repo-monitor-inline review=r1 finding=f-aaaa -->"},
				{"id": 502, "body": "**P1: Other**\n\n<!-- orka:repo-monitor-inline review=r1 finding=f-bbbb -->"},
				{"id": 503, "body": "A human reply"},
			})
		case req.Method == http.MethodPost && req.URL.Path == "/graphql" && strings.Contains(string(body), "reviewThreads"):
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[
				{"id":"T501","isResolved":false,"comments":{"nodes":[{"databaseId":501}]}},
				{"id":"T502","isResolved":false,"comments":{"nodes":[{"databaseId":502}]}}
			],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}}`))
		case req.Method == http.MethodPost && req.URL.Path == "/graphql" && strings.Contains(string(body), "resolveReviewThread"):
			var request struct {
				Variables map[string]string `json:"variables"`
			}
			_ = json.Unmarshal(body, &request)
			resolved = append(resolved, request.Variables["thread"])
			_, _ = w.Write([]byte(`{"data":{"resolveReviewThread":{"thread":{"id":"x","isResolved":true}}}}`))
		case req.Method == http.MethodPatch && strings.HasPrefix(req.URL.Path, "/repos/orka-agents/orka/pulls/comments/"):
			var update map[string]string
			_ = json.Unmarshal(body, &update)
			patched = append(patched, strings.TrimPrefix(req.URL.Path, "/repos/orka-agents/orka/pulls/comments/")+":"+update["body"])
			_, _ = w.Write([]byte(`{}`))
		default:
			// Reviews Orka did not publish, such as one carrying a copied
			// marker, are never read.
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("threads")
	monitor.Spec.Review.Publish.Enabled = true
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, GitHubAPIBaseURL: server.URL}
	if err := db.CreateReviewPublishRecord(ctx, &store.ReviewPublishRecord{ID: "pub-old", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, ItemKind: repositoryMonitorPullRequestKind, ItemNumber: 1, HeadSHA: "old", Phase: repositoryMonitorPublishPhaseSucceeded, GitHubReviewID: "123"}); err != nil {
		t.Fatal(err)
	}
	findingsJSON, err := repositoryMonitorReviewFindingsJSON([]repositoryMonitorReviewFinding{
		{ID: "f-aaaa", Title: "Bug", Status: repositoryMonitorFindingStatusFixed, ResolvedHead: "newhead1"},
		{ID: "f-bbbb", Title: "Other", Status: repositoryMonitorFindingStatusOpen},
	})
	if err != nil {
		t.Fatal(err)
	}
	record := &store.ReviewRecord{ID: "review-new", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 1, HeadSHA: "newhead1", FindingsJSON: findingsJSON}
	item := &store.MonitorItem{Number: 1}
	// The head moved after the review, so the fix may no longer hold.
	if err := r.markRepositoryMonitorFixedFindingThreads(ctx, monitor, item, record); err != nil {
		t.Fatal(err)
	}
	if len(patched) != 0 || len(resolved) != 0 {
		t.Fatalf("marked threads for a head that moved: patched=%q resolved=%q", patched, resolved)
	}
	currentHead = "newhead1"
	for range 2 {
		if err := r.markRepositoryMonitorFixedFindingThreads(ctx, monitor, item, record); err != nil {
			t.Fatal(err)
		}
	}
	if len(patched) != 1 || !strings.HasPrefix(patched[0], "501:✅ Addressed in `newhead1`.\n\n**P1: Bug**") {
		t.Fatalf("patched comments = %q, want only the fixed finding's comment, once", patched)
	}
	if len(resolved) != 1 || resolved[0] != "T501" {
		t.Fatalf("resolved threads = %q, want T501 once", resolved)
	}

	// A security-sensitive review, or a fix of a restricted finding, never
	// links the finding to its fix.
	restrictedJSON, err := repositoryMonitorReviewFindingsJSON([]repositoryMonitorReviewFinding{
		{ID: "f-aaaa", Title: "Bug", Status: repositoryMonitorFindingStatusFixed, ResolvedHead: "newhead1", SecuritySensitive: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, restricted := range []*store.ReviewRecord{
		{ID: "review-sensitive", HeadSHA: "newhead1", SecurityStatus: repositoryMonitorReviewVerdictSecuritySensitive, FindingsJSON: findingsJSON},
		{ID: "review-restricted", HeadSHA: "newhead1", Verdict: repositoryMonitorReviewVerdictPassed, FindingsJSON: restrictedJSON},
	} {
		if err := r.markRepositoryMonitorFixedFindingThreads(ctx, monitor, item, restricted); err != nil {
			t.Fatal(err)
		}
	}
	if len(patched) != 1 || len(resolved) != 1 {
		t.Fatalf("marked a restricted finding: patched=%q resolved=%q", patched, resolved)
	}
}
