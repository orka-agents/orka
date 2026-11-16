package api

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRepositoryMonitorPullRequestIncludeLabelsAPI(t *testing.T) {
	spec := corev1alpha1.RepositoryMonitorSpec{RepoURL: "https://github.com/orka-agents/orka"}
	spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	if err := validateRepositoryMonitorSupportedTargets(spec); err != nil {
		t.Fatalf("includeLabels without issue implementation: %v", err)
	}
	spec.Targets.Issues.Enabled = true
	spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	if err := validateRepositoryMonitorSupportedTargets(spec); err == nil || !strings.Contains(err.Error(), "includeLabels") {
		t.Fatalf("includeLabels with issue implementation error = %v, want rejection", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{Spec: corev1alpha1.RepositoryMonitorSpec{RepoURL: "https://github.com/orka-agents/orka"}}
	monitor.Spec.Review.ExactEventEnabled = true
	monitor.Spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	repo := githubWebhookRepository{FullName: "orka-agents/orka"}
	if repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main"}, "") {
		t.Fatal("pull request event without an include label was accepted")
	}
	if !repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main", Labels: []string{"Orka:Review"}}, "") {
		t.Fatal("pull request event with an include label was rejected")
	}
	// Removing the include label must reach the controller so it retires the PR.
	if !repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main"}, "orka:review") {
		t.Fatal("removal of the include label was rejected")
	}
	if repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main"}, "unrelated") {
		t.Fatal("removal of an unrelated label from an excluded pull request was accepted")
	}

	monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	excluded := githubLabelTarget{IsPR: true, BaseBranch: "main", State: "open"}
	if repositoryMonitorAcceptsLabelCommand(monitor, repo, excluded, commandIntentFixCI) {
		t.Fatal("label command on an excluded pull request was accepted before the label could be consumed")
	}
	if !repositoryMonitorAcceptsLabelCommand(monitor, repo, excluded, commandIntentStop) {
		t.Fatal("stop on an excluded pull request was rejected")
	}
	excluded.Labels = []string{"orka:review"}
	if !repositoryMonitorAcceptsLabelCommand(monitor, repo, excluded, commandIntentFixCI) {
		t.Fatal("label command on an included pull request was rejected")
	}
}
