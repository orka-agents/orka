package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// Exercise ingestion, durable publication records, Secret reload and real HTTP
// requests through Reconcile. Only the Kubernetes client and GitHub service are
// substituted; no LLM execution is needed to republish a completed review.
func TestRepositoryMonitorReviewPublishRecoversAfterTokenRefresh(t *testing.T) {
	tests := []struct {
		name           string
		failureStage   string
		status         int
		suspended      bool
		headChanged    bool
		acceptedReview bool
		wantPhase      string
		wantReason     string
		wantReviews    int
	}{
		{name: "PR fetch unauthorized", failureStage: "pull", status: http.StatusUnauthorized, wantPhase: repositoryMonitorPublishPhaseSucceeded, wantReviews: 1},
		{name: "review listing unauthorized", failureStage: "list", status: http.StatusUnauthorized, wantPhase: repositoryMonitorPublishPhaseSucceeded, wantReviews: 1},
		{name: "submission unauthorized", failureStage: "post", status: http.StatusUnauthorized, wantPhase: repositoryMonitorPublishPhaseSucceeded, wantReviews: 1},
		{name: "suspended monitor retries publication", failureStage: "post", status: http.StatusUnauthorized, suspended: true, wantPhase: repositoryMonitorPublishPhaseSucceeded, wantReviews: 1},
		{name: "head changes before retry", failureStage: "post", status: http.StatusUnauthorized, headChanged: true, wantPhase: repositoryMonitorPublishPhaseSkipped, wantReason: repositoryMonitorPublishSkipHeadSHAChanged},
		{name: "permission denied stays terminal", failureStage: "post", status: http.StatusForbidden, wantPhase: repositoryMonitorPublishPhaseFailed, wantReason: repositoryMonitorPublishFailureGitHubPermissionDenied},
		{name: "missing resource stays terminal", failureStage: "post", status: http.StatusNotFound, wantPhase: repositoryMonitorPublishPhaseFailed, wantReason: repositoryMonitorPublishFailureGitHubPermissionDenied},
		{name: "ambiguous submission reconciles remote review", failureStage: "post", status: http.StatusInternalServerError, acceptedReview: true, wantPhase: repositoryMonitorPublishPhaseSkipped, wantReason: repositoryMonitorPublishSkipDuplicateSameHead, wantReviews: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			const monitorName = "publish-token-refresh"
			const secretName = "forge-token"
			monitor := repositoryMonitorReviewIngestTestMonitor(monitorName)
			if tt.suspended {
				monitor.Spec.Suspend = &tt.suspended
				monitor.Spec.Schedule = "* * * * *"
				monitor.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
			}
			monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: secretName}
			monitor.Spec.Review.Publish.Enabled = true
			task := repositoryMonitorReviewIngestTestTask("completed-review", monitorName, 1, repositoryMonitorTestHeadSHA)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: monitor.Namespace},
				Data:       map[string][]byte{"token": []byte(repositoryMonitorExpiredTestCredential)},
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, task, secret)...).Build()

			fixture := newRepositoryMonitorPublishRetryFixture(t, tt.failureStage, tt.status, tt.acceptedReview)
			server := fixture.Server
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}}
			reconcile := func() ctrl.Result {
				t.Helper()
				result, err := reconciler.Reconcile(ctx, request)
				if err != nil {
					t.Fatalf("Reconcile: %v", err)
				}
				return result
			}
			seedRepositoryMonitorQueuedReview(t, ctx, monitorStore, monitorName, task.Name,
				repositoryMonitorReviewResultEnvelope(t, 1, repositoryMonitorTestHeadSHA, repositoryMonitorReviewVerdictNeedsChanges))
			reconcile()
			failed, _, err := monitorStore.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
			if err != nil || len(failed) != 1 || failed[0].Phase != repositoryMonitorPublishPhaseFailed {
				t.Fatalf("expected one failed publication, records=%v err=%v", failed, err)
			}
			fixture.mu.Lock()
			initialRequests, initialPosts, initialFailures := fixture.requests, fixture.posts, fixture.failures
			fixture.mu.Unlock()
			if initialFailures != 1 {
				t.Fatalf("fixture failures = %d, want exactly one", initialFailures)
			}

			// Rotate the Secret, then replace the reconciler to exclude in-memory
			// retry state. The stored result and failed publication are retained.
			if err := cl.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, secret); err != nil {
				t.Fatal(err)
			}
			secret.Data["token"] = []byte(repositoryMonitorRefreshedTestCredential)
			if err := cl.Update(ctx, secret); err != nil {
				t.Fatal(err)
			}
			reconciler = &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
			cooldown := reconcile()
			fixture.mu.Lock()
			cooldownRequests := fixture.requests
			if tt.headChanged {
				fixture.currentHead = "different-head"
			}
			fixture.mu.Unlock()
			if cooldownRequests != initialRequests {
				t.Fatal("publication retried during cooldown")
			}
			if tt.status == http.StatusUnauthorized && cooldown.RequeueAfter <= 0 {
				t.Fatal("authentication failure must keep reconciliation scheduled during cooldown")
			}

			// Advance the persisted attempt time instead of sleeping in the test.
			failed[0].UpdatedAt = time.Now().Add(-6 * time.Minute)
			if err := monitorStore.UpdateReviewPublishRecord(ctx, &failed[0]); err != nil {
				t.Fatal(err)
			}
			reconcile()
			settled := reconcile()
			item, err := monitorStore.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "1")
			if err != nil {
				t.Fatal(err)
			}
			if item.LastPublishPhase != tt.wantPhase || item.LastPublishReason != tt.wantReason {
				t.Fatalf("publication = %s/%s, want %s/%s", item.LastPublishPhase, item.LastPublishReason, tt.wantPhase, tt.wantReason)
			}
			if tt.suspended {
				if settled.RequeueAfter != 0 {
					t.Fatal("suspended monitor must stop requeuing after publication completes")
				}
				runs, _, err := monitorStore.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
				if err != nil {
					t.Fatal(err)
				}
				if len(runs) != 0 {
					t.Fatal("publication retry must not queue scheduled monitor runs while suspended")
				}
			}
			fixture.mu.Lock()
			finalPosts, finalReviews, finalFailures := fixture.posts, fixture.reviews, fixture.failures
			fixture.mu.Unlock()
			wantPosts := initialPosts
			if tt.wantPhase == repositoryMonitorPublishPhaseSucceeded {
				wantPosts++
			}
			if finalPosts != wantPosts || finalReviews != tt.wantReviews || finalFailures != 1 {
				t.Fatalf("posts/reviews/failures = %d/%d/%d, want %d/%d/1", finalPosts, finalReviews, finalFailures, wantPosts, tt.wantReviews)
			}
			assertRepositoryMonitorPublicationReusesReview(t, monitor, task, cl, monitorStore, failed[0].ReviewRecordID)
			if tt.wantPhase == repositoryMonitorPublishPhaseSucceeded && !strings.Contains(item.LastPublishURL, "pullrequestreview-123") {
				t.Fatal("successful retry must persist the GitHub review URL")
			}
		})
	}
}

const (
	repositoryMonitorExpiredTestCredential   = "expired-test-credential"
	repositoryMonitorRefreshedTestCredential = "refreshed-test-credential"
)

type repositoryMonitorPublishRetryFixture struct {
	*httptest.Server
	mu                                 sync.Mutex
	requests, posts, failures, reviews int
	currentHead, remoteBody            string
}

func newRepositoryMonitorPublishRetryFixture(t *testing.T, failureStage string, status int, acceptedReview bool) *repositoryMonitorPublishRetryFixture {
	t.Helper()
	fixture := &repositoryMonitorPublishRetryFixture{currentHead: repositoryMonitorTestHeadSHA}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		fixture.requests++
		stage := ""
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/1":
			stage = "pull"
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/1/reviews":
			stage = "list"
		case r.Method == http.MethodPost && r.URL.Path == "/repos/orka-agents/orka/pulls/1/reviews":
			stage = "post"
			fixture.posts++
		default:
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		authorization := r.Header.Get("Authorization")
		if authorization != "Bearer "+repositoryMonitorExpiredTestCredential && authorization != "Bearer "+repositoryMonitorRefreshedTestCredential {
			t.Error("request did not use the configured forge credential")
			http.Error(w, "invalid credential", http.StatusUnauthorized)
			return
		}
		var payload repositoryMonitorPullRequestReviewRequest
		if stage == "post" {
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode review: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if payload.CommitID != repositoryMonitorTestHeadSHA || payload.Event != repositoryMonitorPublishEventComment {
				t.Error("review must target the original head with COMMENT")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if stage == failureStage && authorization == "Bearer "+repositoryMonitorExpiredTestCredential {
			fixture.failures++
			if acceptedReview {
				fixture.remoteBody = payload.Body
				fixture.reviews++
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"fixture failure"}`))
			return
		}
		switch stage {
		case "pull":
			_, _ = w.Write([]byte(repositoryMonitorPublishPullBody(repositoryMonitorPublishTestServerConfig{
				State: "open", HeadSHA: fixture.currentHead, BaseBranch: repositoryMonitorTestDefaultBranch,
			})))
		case "list":
			result := []repositoryMonitorPullRequestReviewResponse{}
			if fixture.remoteBody != "" {
				result = append(result, repositoryMonitorPullRequestReviewResponse{ID: 123, HTMLURL: "https://github.com/orka-agents/orka/pull/1#pullrequestreview-123", Body: fixture.remoteBody})
			}
			_ = json.NewEncoder(w).Encode(result)
		case "post":
			fixture.remoteBody = payload.Body
			fixture.reviews++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(repositoryMonitorPullRequestReviewResponse{ID: 123, HTMLURL: "https://github.com/orka-agents/orka/pull/1#pullrequestreview-123"})
		}
	}))
	t.Cleanup(fixture.Close)
	return fixture
}

func assertRepositoryMonitorPublicationReusesReview(t *testing.T, monitor *corev1alpha1.RepositoryMonitor, task *corev1alpha1.Task, cl client.Client, monitorStore store.RepositoryMonitorStore, reviewRecordID string) {
	t.Helper()
	ctx := context.Background()
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks); err != nil || len(tasks.Items) != 1 || tasks.Items[0].Name != task.Name {
		t.Fatalf("retry must reuse the completed Task: tasks=%v err=%v", tasks.Items, err)
	}
	records, _, err := monitorStore.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Limit: 10})
	if err != nil || len(records) != 1 || records[0].ID != reviewRecordID {
		t.Fatalf("retry must reuse the review result: records=%v err=%v", records, err)
	}
}
