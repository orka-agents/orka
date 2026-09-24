package service

import (
	"context"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/publisher"
)

func TestGitHubPRReconcilerTaskPresentation(t *testing.T) {
	for _, test := range []struct {
		name, title, prompt, body, wantTitle string
	}{
		{name: "explicit title", title: "fix(api): preserve exact  title", prompt: "Ignored prompt", wantTitle: "fix(api): preserve exact  title"},
		{name: "derived title", prompt: "  fix(api): handle empty filters  \nDo not use this line", wantTitle: "fix(api): handle empty filters"},
		{name: "empty prompt", wantTitle: "Orka publication generation 7"},
		{name: "custom body", title: "fix: reject empty filters", body: "## Summary\n\nReject empty filters.\n", wantTitle: "fix: reject empty filters"},
		{name: "maximum escaped body", title: "fix: preserve body", body: strings.Repeat("<", publisher.MaxPullRequestBodyLength), wantTitle: "fix: preserve body"},
		{name: "maximum Unicode body", title: "fix: preserve Unicode", body: strings.Repeat("🙂", publisher.MaxPullRequestBodyLength), wantTitle: "fix: preserve Unicode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			scenario := newGitHubPRTestScenario(t)
			intent := githubTestIntent()
			intent.Title, intent.Body = test.title, test.body
			if intent.Title == "" {
				intent.Title = publisher.DefaultPullRequestTitle(test.prompt, intent.PublicationGeneration)
			}
			intent.TaskName, intent.TaskNamespace = "fix-filters", "automation"
			intent.SessionUID = "task-session"
			key, err := intent.Key()
			if err != nil {
				t.Fatal(err)
			}
			sessionKey, err := intent.SessionKey()
			if err != nil {
				t.Fatal(err)
			}
			scenario.create = githubTestPullRequest(scenario.base, scenario.head, 17, publisher.PullRequestOpen, githubTestSHA, githubPullRequestBody(intent, key, sessionKey))
			reconciler := newGitHubTestReconciler(t, scenario.handler(t))
			if _, err := publisher.ReconcilePullRequest(context.Background(), intent, reconciler); err != nil {
				t.Fatalf("ReconcilePullRequest: %v", err)
			}
			created := scenario.createSnapshot()
			if len(created) != 1 || created[0].Title != test.wantTitle {
				t.Fatal("publisher did not create the requested title")
			}
			body := created[0].Body
			wantPrefix := test.body
			if wantPrefix == "" {
				wantPrefix = "Created by the Orka clean-room workspace publisher.\n\nTask: `automation/fix-filters`"
			}
			want := wantPrefix + "\n\nPublication generation: 7\n\n" + githubIntentMarker(key) + "\n\n" + githubSessionMarkerPrefix + sessionKey + githubIntentMarkerSuffix
			if body != want {
				t.Fatal("publisher changed the body or omitted its Task metadata or reconciliation markers")
			}
			// A retry must adopt the same PR without rewriting its presentation.
			scenario.list = []githubPullRequest{scenario.create}
			intent.Title, intent.Body = "A later Task title", "A later Task body"
			if _, err := publisher.ReconcilePullRequest(context.Background(), intent, reconciler); err != nil {
				t.Fatalf("reconcile existing PR: %v", err)
			}
			if len(scenario.createSnapshot()) != 1 {
				t.Fatal("presentation changes created another pull request")
			}
		})
	}
}

func TestGitHubPRReconcilerRejectsAuthoredOwnershipMarkers(t *testing.T) {
	for _, prefix := range []string{githubIntentMarkerPrefix, githubSessionMarkerPrefix} {
		scenario := newGitHubPRTestScenario(t)
		intent := githubTestIntent()
		intent.SessionUID = "task-session"
		intent.Body = "Copied description\n\n" + prefix + "sha256:" + strings.Repeat("a", 64) + githubIntentMarkerSuffix
		reconciler := newGitHubTestReconciler(t, scenario.handler(t))
		if _, err := publisher.ReconcilePullRequest(context.Background(), intent, reconciler); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("accepted a copied ownership marker: %v", err)
		}
		if len(scenario.requestSnapshot()) != 0 {
			t.Fatal("invalid authored metadata reached the forge")
		}
	}
}
