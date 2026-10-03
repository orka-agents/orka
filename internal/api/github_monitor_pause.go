package api

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func repositoryMonitorAPIPauseLabels(monitor *corev1alpha1.RepositoryMonitor) []string {
	if monitor.Spec.Policy.PauseLabels == nil {
		return []string{"orka:pause"}
	}
	return monitor.Spec.Policy.PauseLabels
}

func (h *Handlers) handleRepositoryMonitorPauseEvent(c fiber.Ctx, body []byte, payload githubLabelWebhookPayload) (githubRepositoryMonitorEventResult, error) {
	var result githubRepositoryMonitorEventResult
	if h.repositoryMonitorStore == nil || (payload.Action != githubWebhookActionLabeled && payload.Action != "unlabeled") {
		return result, nil
	}
	target, ok := payload.target()
	if !ok || target.IncompletePR {
		return result, nil
	}
	monitors := &corev1alpha1.RepositoryMonitorList{}
	if err := h.client.List(c.Context(), monitors, h.githubWebhookMonitorListOptions()...); err != nil {
		return result, err
	}
	delivery := strings.TrimSpace(c.Get(githubDeliveryHeader))
	if delivery == "" {
		delivery = githubWebhookReplayKey(body)
	}
	for i := range monitors.Items {
		monitor := &monitors.Items[i]
		matches := false
		for _, label := range repositoryMonitorAPIPauseLabels(monitor) {
			if strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(payload.Label.Name)) {
				matches = true
				break
			}
		}
		if !matches || !repositoryMonitorAcceptsLabelCommand(monitor, payload.Repository, target, commandIntentResume) {
			continue
		}
		result.Matched++
		permission, err := h.repositoryMonitorCommandActorPermission(c.Context(), monitor, payload.Repository, payload.Sender.Login)
		if err != nil {
			return result, fiber.NewError(fiber.StatusServiceUnavailable, "cannot verify pause-label sender permission")
		}
		if !repositoryMonitorPermissionAllowed(monitor, permission) {
			return result, fiber.NewError(fiber.StatusForbidden, "pause-label sender is not authorized to operate this monitor")
		}
		id := githubRepositoryMonitorExactRunID(monitor, delivery+"|pause")
		run := &store.MonitorRun{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "pause_label_event", TargetKind: target.Kind, TargetNumber: int64(target.Number), Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now()}
		if target.IsPR {
			run.TargetSHA = target.HeadSHA
		}
		existing, lookupErr := h.repositoryMonitorStore.GetMonitorRun(c.Context(), monitor.Namespace, id)
		if lookupErr == nil {
			if existing.Phase != repositoryMonitorRunPhaseFailed {
				result.Duplicate++
				continue
			}
			requeued, err := h.requeueFailedRepositoryMonitorEventRun(c, run)
			if err != nil {
				return result, err
			}
			if !requeued {
				result.Duplicate++
				continue
			}
		} else if !errors.Is(lookupErr, store.ErrNotFound) {
			return result, lookupErr
		} else if err := h.repositoryMonitorStore.CreateMonitorRun(c.Context(), run); err != nil {
			if errors.Is(err, store.ErrConflict) {
				result.Duplicate++
				continue
			}
			return result, err
		}

		if err := h.annotateRepositoryMonitorRunRequest(c, monitor, run); err != nil {
			if markErr := h.markRepositoryMonitorRunSignalFailed(c, run, err); markErr != nil {
				return result, fmt.Errorf("%w: %v", err, markErr)
			}
			return result, err
		}
		if err := h.createRepositoryMonitorEventRunAudit(c, monitor, run, payload, target, delivery, "pause_label_changed"); err != nil {
			return result, err
		}
		result.Queued++
		result.RunIDs = append(result.RunIDs, id)
	}
	return result, nil
}
