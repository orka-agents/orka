package controller

import (
	"context"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// testRepositoryMonitorRepo is the monitored repository the prompt tests use.
const testRepositoryMonitorRepo = "orka-agents/orka"

// buildRepositoryMonitorRepairPrompt builds a repair prompt the way the repair
// handoff does, looking up fix_ci evidence itself, for tests that only need
// the resulting prompt.
func (r *RepositoryMonitorReconciler) buildRepositoryMonitorRepairPrompt(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, intent string, pr repositoryMonitorPullRequest, item *store.MonitorItem) (string, error) {
	var ciInput *repositoryMonitorRepairCIInput
	if intent == repositoryMonitorCommandIntentFixCI {
		input := r.buildRepositoryMonitorRepairCIInput(ctx, monitor, testRepositoryMonitorRepo, pr)
		ciInput = &input
	}
	return r.buildRepositoryMonitorRepairPromptWithCI(ctx, monitor, intent, testRepositoryMonitorRepo, pr, item, ciInput)
}
