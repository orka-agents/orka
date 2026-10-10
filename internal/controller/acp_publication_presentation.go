package controller

import (
	"context"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/publisher"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/security"
)

// Validate the selected text before reserving SCM effects. Inspect the full
// prompt line before truncation so a truncated credential cannot escape detection.
func validateTaskPullRequestText(task *corev1alpha1.Task) error {
	if task == nil || task.Spec.Workspace == nil {
		return nil
	}
	workspace := task.Spec.Workspace
	title := workspace.PRTitle
	if workspace.CreatePR && title == "" {
		line, _, _ := strings.Cut(strings.TrimSpace(task.Spec.Prompt), "\n")
		if security.LooksLikeSecret(line) {
			return fmt.Errorf("PR title derived from the prompt must not contain credentials or tokens")
		}
		title = publisher.DefaultPullRequestTitle(task.Spec.Prompt, acpPublicationGeneration)
	}
	return publisher.ValidatePullRequestText(title, workspace.PRBody)
}

// New fields require a peer that advertises them. During rollout, ordinary
// Tasks can keep using the legacy wire shape; explicit overrides wait before
// prompt admission and are checked again before any branch publication.
func (d *ACPDispatcher) pullRequestPresentationCapability(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if err := validateTaskPullRequestText(task); err != nil {
		return false, err
	}
	if task == nil || task.Spec.Workspace == nil || !task.Spec.Workspace.CreatePR {
		return false, nil
	}
	if d.Publisher == nil {
		return false, fmt.Errorf("clean-room Workspace/Publisher is required")
	}
	capabilities, err := d.Publisher.Capabilities(ctx)
	if err != nil {
		return false, err
	}
	if capabilities.Protocol != publisherservice.ProtocolVersion || !capabilities.PullRequestReconciliation {
		return false, fmt.Errorf("workspace publisher does not support pull-request reconciliation")
	}
	if !capabilities.PullRequestPresentation && (task.Spec.Workspace.PRTitle != "" || task.Spec.Workspace.PRBody != "") {
		return false, fmt.Errorf("workspace publisher does not support Task-authored pull-request presentation")
	}
	return capabilities.PullRequestPresentation, nil
}

func withoutTaskPullRequestMetadata(intent publisher.PullRequestIntent) publisher.PullRequestIntent {
	intent.Title, intent.Body, intent.TaskName, intent.TaskNamespace = "", "", "", ""
	return intent
}
