package controller

import (
	"context"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const testRepositoryMonitorRepo = "orka-agents/orka"

func (r *RepositoryMonitorReconciler) buildRepositoryMonitorRepairPrompt(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, intent string, pr repositoryMonitorPullRequest, item *store.MonitorItem) (string, error) {
	var ciInput *repositoryMonitorRepairCIInput
	if intent == repositoryMonitorCommandIntentFixCI {
		input := r.buildRepositoryMonitorRepairCIInput(ctx, monitor, testRepositoryMonitorRepo, pr)
		ciInput = &input
	}
	return r.buildRepositoryMonitorRepairPromptWithCI(ctx, monitor, intent, testRepositoryMonitorRepo, pr, item, ciInput)
}
