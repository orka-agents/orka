package api

import (
	"context"
	"errors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func (h *Handlers) securityRunBoundToRepositoryScan(
	ctx context.Context,
	scan *corev1alpha1.RepositoryScan,
	runID string,
) (bool, error) {
	if h == nil || h.securityStore == nil || scan == nil || runID == "" {
		return false, nil
	}
	run, err := h.securityStore.GetScanRun(ctx, scan.Namespace, runID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return run.RepositoryScan == scan.Name &&
		run.RepositoryScanUID != "" && run.RepositoryScanGeneration > 0 &&
		run.RepositoryScanUID == string(scan.UID) && run.RepositoryScanGeneration == scan.Generation, nil
}

func threatModelBoundToRepositoryScan(model *store.ThreatModel, scan *corev1alpha1.RepositoryScan) bool {
	if model == nil || scan == nil || model.RepositoryScan != scan.Name {
		return false
	}
	if scan.UID == "" {
		return true
	}
	return model.RepositoryScanUID != "" && model.RepositoryScanGeneration > 0 &&
		model.RepositoryScanUID == string(scan.UID) && model.RepositoryScanGeneration == scan.Generation
}
