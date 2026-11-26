package api

import (
	"net/http"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestGitHubWebhook_PullRequestReviewAndCommentTriggerExternalCommentMonitors(t *testing.T) {
	monitorStore := setupGitHubWebhookMonitorStore(t)
	monitor := githubWebhookRepositoryMonitor("ext-review-webhook", true)
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Repair.ExternalComments.Enabled = true

	// A second monitor without externalComments enabled must not match review events.
	other := githubWebhookRepositoryMonitor("no-ext-review-webhook", true)
	other.Spec.Repair.Enabled = true
	other.Spec.Repair.ExternalComments.Enabled = false

	secret := configureGitHubWebhookTest(t, nil)
	fc := newGitHubWebhookFakeClient(t, monitor, other, githubWebhookGitSecret())
	server := NewServer(fc, nil, ServerConfig{RepositoryMonitorStore: monitorStore})

	reviewBody := []byte(`{
		"action": "submitted",
		"repository": {"full_name": "sozercan/vekil", "html_url": "https://github.com/sozercan/vekil", "clone_url": "https://github.com/sozercan/vekil.git", "default_branch": "main"},
		"pull_request": {
			"number": 77, "state": "open", "title": "Fix bug", "html_url": "https://github.com/sozercan/vekil/pull/77",
			"base": {"ref": "main", "sha": "1111111111111111111111111111111111111111"},
			"head": {"ref": "feat/fix", "sha": "2222222222222222222222222222222222222222"}
		},
		"sender": {"login": "coderabbitai[bot]"}
	}`)

	resp := performSignedGitHubWebhook(t, server, githubEventPullRequestReview, "delivery-review-1", secret, reviewBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("pull_request_review status = %d, want 201; body: %s", resp.StatusCode, readRespBody(t, resp))
	}
	runs, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name})
	if err != nil || len(runs) != 1 || runs[0].TargetNumber != 77 || runs[0].TargetSHA != "2222222222222222222222222222222222222222" {
		t.Fatalf("monitor runs = %+v err=%v", runs, err)
	}
	otherRuns, _, err := monitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: other.Namespace, MonitorName: other.Name})
	if err != nil || len(otherRuns) != 0 {
		t.Fatalf("other monitor should not queue on review event: %+v err=%v", otherRuns, err)
	}

	// pull_request_review_comment created on a different PR also queues a run.
	commentBody := []byte(`{
		"action": "created",
		"repository": {"full_name": "sozercan/vekil", "html_url": "https://github.com/sozercan/vekil", "clone_url": "https://github.com/sozercan/vekil.git", "default_branch": "main"},
		"pull_request": {
			"number": 78, "state": "open", "title": "Another PR", "html_url": "https://github.com/sozercan/vekil/pull/78",
			"base": {"ref": "main", "sha": "1111111111111111111111111111111111111111"},
			"head": {"ref": "feat/two", "sha": "3333333333333333333333333333333333333333"}
		},
		"sender": {"login": "reviewer"}
	}`)
	resp2 := performSignedGitHubWebhook(t, server, githubEventPullRequestReviewComment, "delivery-comment-1", secret, commentBody)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("pull_request_review_comment status = %d, want 201; body: %s", resp2.StatusCode, readRespBody(t, resp2))
	}

	// Ignored action (edited) does not queue.
	editedBody := []byte(`{
		"action": "edited",
		"repository": {"full_name": "sozercan/vekil", "html_url": "https://github.com/sozercan/vekil", "clone_url": "https://github.com/sozercan/vekil.git", "default_branch": "main"},
		"pull_request": {
			"number": 79, "state": "open", "title": "Edited", "html_url": "https://github.com/sozercan/vekil/pull/79",
			"base": {"ref": "main", "sha": "1111111111111111111111111111111111111111"},
			"head": {"ref": "feat/three", "sha": "4444444444444444444444444444444444444444"}
		},
		"sender": {"login": "reviewer"}
	}`)
	resp3 := performSignedGitHubWebhook(t, server, githubEventPullRequestReviewComment, "delivery-comment-edited", secret, editedBody)
	if resp3.StatusCode != http.StatusAccepted {
		t.Fatalf("edited comment status = %d, want 202 ignored; body: %s", resp3.StatusCode, readRespBody(t, resp3))
	}
}
