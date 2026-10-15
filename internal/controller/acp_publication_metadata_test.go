package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/publisher"
)

func TestPublicationPullRequestUsesFrozenTaskMetadata(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		workspace := &corev1alpha1.WorkspaceConfig{}
		wantTitle := "fix: use the frozen prompt"
		if explicit {
			workspace.PRTitle = "fix(api): use the exact  authored title"
			workspace.PRBody = "Authored summary.\n"
			wantTitle = workspace.PRTitle
		}
		live := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "fix-task", Namespace: "automation"},
			Spec:       corev1alpha1.TaskSpec{Prompt: "Changed prompt", Workspace: &corev1alpha1.WorkspaceConfig{PRTitle: "Changed title", PRBody: "Changed body"}},
		}
		frozen := frozenTaskFromAgentExecutionSnapshot(live, &corev1alpha1.AgentExecutionBinding{}, agentExecutionSnapshotBody{
			Prompt: "  fix: use the frozen prompt  \nAdditional instructions", Workspace: workspace,
		})
		intent := withTaskPullRequestMetadata(publisher.PullRequestIntent{PublicationGeneration: 3}, frozen)
		if intent.Title != wantTitle || intent.Body != workspace.PRBody || intent.TaskName != "fix-task" || intent.TaskNamespace != "automation" {
			t.Fatalf("PR presentation did not use frozen Task input: %#v", intent)
		}
	}
}

func TestValidateACPWorkspacePreflightPullRequestText(t *testing.T) {
	for _, test := range []struct{ name, title, body string }{
		{name: "prTitle", title: strings.Repeat("界", publisher.MaxPullRequestTitleLength+1)},
		{name: "prBody", body: strings.Repeat("界", publisher.MaxPullRequestBodyLength+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{
				Type:      corev1alpha1.TaskTypeAgent,
				Workspace: &corev1alpha1.WorkspaceConfig{PRTitle: test.title, PRBody: test.body},
			}}
			if err := validateACPWorkspacePreflight(task); err == nil || !strings.Contains(err.Error(), test.name) {
				t.Fatalf("preflight accepted overlong %s: %v", test.name, err)
			}
		})
	}
}
