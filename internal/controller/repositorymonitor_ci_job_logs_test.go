package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRepositoryMonitorCIEvidenceActionsJobLogTail(t *testing.T) {
	ctx := context.Background()
	var blobAuthHeader string
	blobServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blobAuthHeader = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("2026-11-25T04:00:00.1234567Z line 1 ok\n2026-11-25T04:00:01.0000000Z password: |\n2026-11-25T04:00:01.0000001Z   super-secret-multiline-value\n2026-11-25T04:00:02.0000000Z line 2 secret=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB\n\n2026-11-25T04:00:03.0000000Z FAIL TestFoo (0.01s)\n2026-11-25T04:00:03.0000001Z     foo_test.go:12: expected 2, got 1\n"))
	}))
	defer blobServer.Close()

	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/jobs/555/logs"):
			http.Redirect(w, r, blobServer.URL+"/signed-log", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer githubServer.Close()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	monitor, secret := repositoryMonitorInventoryTestObjects("ci-job-logs")
	monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: secret.Name}
	monitor.Spec.Repair.CIEvidence.Text = true
	monitor.Spec.Repair.CIEvidence.JobLogs = true

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, secret).Build()
	r := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, GitHubAPIBaseURL: githubServer.URL}

	pr := repositoryMonitorPullRequest{Number: 12, HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	failures := repositoryMonitorCIFailures{
		broke: true,
		introduced: []repositoryMonitorCIFailure{{
			id:         555,
			name:       "unit-tests",
			source:     "check_run:15368",
			appSlug:    "github-actions",
			headSHA:    pr.HeadSHA,
			status:     "completed",
			conclusion: "failure",
		}},
	}

	ev := r.repositoryMonitorCIEvidenceFor(ctx, monitor, "orka-agents/orka", pr, failures)
	if len(ev.Checks) != 1 {
		t.Fatalf("checks = %+v, want 1", ev.Checks)
	}
	if blobAuthHeader != "" {
		t.Fatalf("cross-origin redirect leaked Authorization header: %q", blobAuthHeader)
	}
	logTail := ev.Checks[0].LogTail
	if !strings.Contains(logTail, "FAIL TestFoo") || !strings.Contains(logTail, "foo_test.go:12") {
		t.Fatalf("expected failure lines in logTail, got:\n%s", logTail)
	}
	if strings.Contains(logTail, "ghp_0123456789") || strings.Contains(logTail, "super-secret-multiline-value") {
		t.Fatalf("secret token or multiline credential was not redacted in logTail:\n%s", logTail)
	}

	// Verify jobLogs=false leaves LogTail empty.
	monitor.Spec.Repair.CIEvidence.JobLogs = false
	evOff := r.repositoryMonitorCIEvidenceFor(ctx, monitor, "orka-agents/orka", pr, failures)
	if len(evOff.Checks) != 1 || evOff.Checks[0].LogTail != "" {
		encoded, _ := json.Marshal(evOff)
		t.Fatalf("expected empty LogTail when jobLogs=false: %s", encoded)
	}
}
