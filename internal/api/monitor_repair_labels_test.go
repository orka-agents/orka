package api

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRepositoryMonitorRepairLabelsAPI(t *testing.T) {
	for _, field := range []string{"spec.repair.includeLabels", "spec.repair.ciOnlyLabels"} {
		spec := corev1alpha1.RepositoryMonitorSpec{RepoURL: "https://github.com/orka-agents/orka"}
		if field == "spec.repair.includeLabels" {
			spec.Repair.IncludeLabels = []string{"orka:fix"}
		} else {
			spec.Repair.CIOnlyLabels = []string{"orka:fix-ci"}
		}
		if err := validateRepositoryMonitorSupportedTargets(spec); err != nil {
			t.Fatalf("%s without issue implementation: %v", field, err)
		}
		spec.Targets.Issues.Enabled = true
		spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
		if err := validateRepositoryMonitorSupportedTargets(spec); err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("%s with issue implementation error = %v, want rejection", field, err)
		}
	}

	monitor := &corev1alpha1.RepositoryMonitor{Spec: corev1alpha1.RepositoryMonitorSpec{RepoURL: "https://github.com/orka-agents/orka"}}
	monitor.Spec.Review.ExactEventEnabled = true
	monitor.Spec.Targets.PullRequests.IncludeLabels = []string{"orka:review"}
	monitor.Spec.Repair.IncludeLabels = []string{"orka:fix"}
	monitor.Spec.Repair.CIOnlyLabels = []string{"orka:fix-ci"}
	repo := githubWebhookRepository{FullName: "orka-agents/orka"}
	for _, label := range []string{"Orka:Fix", "orka:fix-ci"} {
		if !repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main", Labels: []string{label}}) {
			t.Fatalf("pull request event with repair label %q was rejected", label)
		}
	}
	if repositoryMonitorAcceptsPullRequestEvent(monitor, repo, githubLabelTarget{IsPR: true, BaseBranch: "main", Labels: []string{"bug"}}) {
		t.Fatal("pull request event without a review or repair label was accepted")
	}
}
