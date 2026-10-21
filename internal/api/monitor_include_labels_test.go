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
	if repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main"}) {
		t.Fatal("pull request event without an include label was accepted")
	}
	if !repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main", Labels: []string{"Orka:Review"}}) {
		t.Fatal("pull request event with an include label was rejected")
	}
}
