package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRepositoryMonitorReviewPublishUpdatesSameHeadVerdictChange(t *testing.T) {
	for _, policy := range []string{repositoryMonitorPublishSameHeadPolicySkip, repositoryMonitorPublishSameHeadPolicyUpdate} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			publishServer := newRepositoryMonitorPublishTestServer(t, repositoryMonitorPublishTestServerConfig{HeadSHA: repositoryMonitorTestHeadSHA})
			t.Cleanup(publishServer.Close)
			postPassed := true
			monitor := repositoryMonitorReviewIngestTestMonitor("publish-update")
			suspend := true
			monitor.Spec.Suspend = &suspend
			monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: "github-token"}
			monitor.Spec.Review.Publish = corev1alpha1.RepositoryMonitorReviewPublishSpec{
				Enabled: true, Mode: repositoryMonitorPublishModeSummaryOnly, Event: repositoryMonitorPublishEventComment,
				PostPassed: &postPassed, SameHeadPolicy: policy,
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-token")}}
			cl := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: publishServer.URL}
			review := func(taskName, verdict string) {
				t.Helper()
				if err := cl.Create(ctx, repositoryMonitorReviewIngestTestTask(taskName, monitor.Name, 1, repositoryMonitorTestHeadSHA)); err != nil {
					t.Fatal(err)
				}
				seedRepositoryMonitorQueuedReview(t, ctx, monitorStore, monitor.Name, taskName, repositoryMonitorReviewResultEnvelopeWith(t, 1, repositoryMonitorTestHeadSHA, verdict, func(payload map[string]any) {
					payload["summary"] = "Review of the head ended with " + verdict + "."
					if verdict == repositoryMonitorReviewVerdictPassed {
						payload["findings"] = []map[string]any{}
					}
				}))
				if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}

			review("review-first", repositoryMonitorReviewVerdictNeedsChanges)
			if publishServer.PostCount != 1 || publishServer.PutCount != 0 {
				t.Fatalf("first review: posts=%d updates=%d, want one post", publishServer.PostCount, publishServer.PutCount)
			}
			// A later review of the same head reaches a different verdict.
			review("review-second", repositoryMonitorReviewVerdictPassed)
			wantUpdates := 0
			if policy == repositoryMonitorPublishSameHeadPolicyUpdate {
				wantUpdates = 1
			}
			if publishServer.PostCount != 1 || publishServer.PutCount != wantUpdates {
				t.Fatalf("changed verdict: posts=%d updates=%d, want 1 post and %d updates", publishServer.PostCount, publishServer.PutCount, wantUpdates)
			}
			records, _, err := monitorStore.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: "default", MonitorName: monitor.Name, Phase: repositoryMonitorPublishPhaseSucceeded, Limit: 10})
			if err != nil || len(records) != 1 {
				t.Fatalf("succeeded publish records = %+v err=%v, want one per head", records, err)
			}
			if policy == repositoryMonitorPublishSameHeadPolicySkip {
				return
			}
			if publishServer.PutPath != "/repos/orka-agents/orka/pulls/1/reviews/123" ||
				!strings.HasPrefix(publishServer.PutBody, repositoryMonitorUpdatedReviewNote) ||
				!strings.Contains(publishServer.PutBody, "ended with passed") ||
				!strings.Contains(publishServer.PutBody, "publish="+records[0].ID) {
				t.Fatalf("update %s body = %q, want the passed summary under the original publish marker", publishServer.PutPath, publishServer.PutBody)
			}
			updates, _, err := monitorStore.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: "default", MonitorName: monitor.Name, Phase: repositoryMonitorPublishPhaseSkipped, Limit: 10})
			if err != nil || len(updates) != 1 || updates[0].SkipReason != repositoryMonitorPublishSkipUpdatedPublishedReview || !strings.Contains(updates[0].ReviewRecordID, "review-second") || updates[0].GitHubReviewID != "123" {
				t.Fatalf("update records = %+v err=%v, want one for the second review", updates, err)
			}
			// The same verdict again leaves the published review alone.
			review("review-third", repositoryMonitorReviewVerdictPassed)
			if publishServer.PutCount != 1 {
				t.Fatalf("unchanged verdict updated the review again: updates=%d", publishServer.PutCount)
			}

			// GitHub accepted the original review but Orka never recorded it:
			// marker recovery restores it, and a changed verdict still updates it.
			original := records[0]
			original.Phase = repositoryMonitorPublishPhaseFailed
			if err := monitorStore.UpdateReviewPublishRecord(ctx, &original); err != nil {
				t.Fatal(err)
			}
			reviews, err := json.Marshal([]map[string]any{{"id": 123, "commit_id": repositoryMonitorTestHeadSHA, "body": publishServer.PostedReview.Body, "html_url": "https://github.com/orka-agents/orka/pull/1#pullrequestreview-123", "user": map[string]any{"login": "orka-bot"}}})
			if err != nil {
				t.Fatal(err)
			}
			recovered := newRepositoryMonitorPublishTestServer(t, repositoryMonitorPublishTestServerConfig{HeadSHA: repositoryMonitorTestHeadSHA, ReviewsBody: string(reviews)})
			t.Cleanup(recovered.Close)
			reconciler.GitHubAPIBaseURL = recovered.URL
			review("review-fourth", repositoryMonitorReviewVerdictNeedsChanges)
			if recovered.PostCount != 0 || recovered.PutCount != 1 || !strings.Contains(recovered.PutBody, "ended with needs_changes") {
				t.Fatalf("after marker recovery: posts=%d updates=%d body=%q, want the recovered review updated", recovered.PostCount, recovered.PutCount, recovered.PutBody)
			}
		})
	}
}

func TestRepositoryMonitorShownReviewReadsEveryPage(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("shown-review-pages")
	start := time.Now().Add(-time.Hour)
	record := func(id, reviewID, reason string, at time.Time) *store.ReviewPublishRecord {
		return &store.ReviewPublishRecord{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, ItemKind: repositoryMonitorPullRequestKind, ItemNumber: 9, HeadSHA: "head9", ReviewRecordID: reviewID, Phase: repositoryMonitorPublishPhaseSkipped, SkipReason: reason, CreatedAt: at, UpdatedAt: at}
	}
	// The update happens first, then more than a page of same-verdict skips.
	records := make([]*store.ReviewPublishRecord, 0, 151)
	records = append(records, record("pub-update", "review-update", repositoryMonitorPublishSkipUpdatedPublishedReview, start.Add(time.Second)))
	for i := range 150 {
		records = append(records, record(fmt.Sprintf("pub-dup-%03d", i), fmt.Sprintf("review-dup-%03d", i), repositoryMonitorPublishSkipDuplicateSameHead, start.Add(time.Duration(i+2)*time.Second)))
	}
	for _, r := range records {
		if err := db.CreateReviewPublishRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	published := &store.ReviewPublishRecord{ReviewRecordID: "review-original", HeadSHA: "head9", CreatedAt: start}
	shown, err := (&RepositoryMonitorReconciler{Store: db}).repositoryMonitorShownReviewRecordID(ctx, monitor, &store.MonitorItem{Number: 9}, published)
	if err != nil || shown != "review-update" {
		t.Fatalf("shown review = %q err=%v, want the update past the first page", shown, err)
	}
}
