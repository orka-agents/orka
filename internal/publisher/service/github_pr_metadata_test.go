package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/publisher"
)

func TestGitHubPRReconcilerTaskPresentation(t *testing.T) {
	for _, test := range []struct {
		name, title, prompt, body, wantTitle string
	}{
		{name: "explicit title", title: "fix(api): preserve exact  title", prompt: "Ignored prompt", wantTitle: "fix(api): preserve exact  title"},
		{name: "explicit title with whitespace", title: " \tfix(api): preserve exact \u2003 title\u00a0", prompt: "Ignored prompt", wantTitle: " \tfix(api): preserve exact \u2003 title\u00a0"},
		{name: "derived title", prompt: "  fix(api): handle empty filters  \nDo not use this line", wantTitle: "fix(api): handle empty filters"},
		{name: "empty prompt", wantTitle: "Orka publication generation 7"},
		{name: "whitespace prompt", prompt: " \t\n\u0085\u00a0\u2003\u3000", wantTitle: "Orka publication generation 7"},
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

func TestGitHubPRReconcilerRejectsInvalidPullRequestText(t *testing.T) {
	fakeToken := "g" + "hp_" + strings.Repeat("NOTAREALSECRET", 3)
	fakeAssignment := "password=" + strings.Repeat("FAKE_TEST_ONLY_", 3)
	for _, test := range []struct {
		name, title, body, wantError string
	}{
		{name: "spaces", title: "   ", wantError: "validate prTitle: must not be whitespace-only"},
		{name: "tabs", title: "\t\t", wantError: "validate prTitle: must not be whitespace-only"},
		{name: "newlines", title: "\n\r\n", wantError: "validate prTitle: must not be whitespace-only"},
		{name: "Unicode whitespace", title: "\u0085\u00a0\u1680\u2003\u2028\u2029\u202f\u205f\u3000", wantError: "validate prTitle: must not be whitespace-only"},
		{name: "overlong title", title: strings.Repeat("界", publisher.MaxPullRequestTitleLength+1), wantError: "validate prTitle: must not exceed 256 characters"},
		{name: "overlong body", body: strings.Repeat("🙂", publisher.MaxPullRequestBodyLength+1), wantError: "validate prBody: must not exceed 32768 characters"},
		{name: "token in title", title: "fix: remove " + fakeToken, wantError: "validate prTitle: must not contain credentials or tokens"},
		{name: "assignment in title", title: fakeAssignment, wantError: "validate prTitle: must not contain credentials or tokens"},
		{name: "token in body", body: "## Details\n\n" + fakeToken, wantError: "validate prBody: must not contain credentials or tokens"},
		{name: "assignment in body", body: "## Details\n\n" + fakeAssignment, wantError: "validate prBody: must not contain credentials or tokens"},
		{name: "token in prompt-derived title", title: publisher.DefaultPullRequestTitle(" \nfix: remove "+fakeToken+"\nMore details", 7), wantError: "validate prTitle: must not contain credentials or tokens"},
		{name: "assignment in prompt-derived title", title: publisher.DefaultPullRequestTitle(fakeAssignment+"\nMore details", 7), wantError: "validate prTitle: must not contain credentials or tokens"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, direct := range []bool{false, true} {
				name := "shared publisher"
				if direct {
					name = "GitHub adapter"
				}
				t.Run(name, func(t *testing.T) {
					scenario := newGitHubPRTestScenario(t)
					intent := githubTestIntent()
					intent.Title, intent.Body = test.title, test.body
					reconciler := newGitHubTestReconciler(t, scenario.handler(t))
					var err error
					if direct {
						_, err = reconciler.Reconcile(t.Context(), intent)
					} else {
						_, err = publisher.ReconcilePullRequest(t.Context(), intent, reconciler)
					}
					if len(scenario.requestSnapshot()) != 0 {
						t.Error("invalid pull request text reached the forge")
					}
					if !errors.Is(err, publisher.ErrInvalidRequest) || err.Error() != test.wantError {
						// Do not print the error: a regression could include the fake value.
						t.Fatal("invalid text did not return the expected non-sensitive validation error")
					}
				})
			}
		})
	}
}
