package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	if h.repositoryMonitorStore == nil || (payload.Action != githubWebhookActionLabeled && payload.Action != githubWebhookActionUnlabeled) {
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
		matches := repositoryMonitorWebhookMatchingLabel(repositoryMonitorAPIPauseLabels(monitor), []string{payload.Label.Name}) != ""
		if !matches || !repositoryMonitorAcceptsLabelCommand(monitor, payload.Repository, target, commandIntentResume) {
			continue
		}
		result.Matched++
		permission, err := h.repositoryMonitorCommandActorPermission(c.Context(), monitor, payload.Repository, payload.Sender.Login)
		if err != nil {
			return result, fiber.NewError(fiber.StatusServiceUnavailable, "cannot verify pause-label sender permission")
		}
		if !repositoryMonitorPermissionAllowed(monitor, permission) {
			if err := h.recordRepositoryMonitorPauseRejection(c, monitor, payload, target, delivery, permission); err != nil {
				return result, err
			}
			continue
		}
		id := githubRepositoryMonitorExactRunID(monitor, delivery+"|pause")
		run := &store.MonitorRun{ID: id, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Trigger: "pause_label_event", TargetKind: target.Kind, TargetNumber: int64(target.Number), Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now()}
		if target.IsPR {
			run.TargetSHA = target.HeadSHA
		}
		existing, lookupErr := h.repositoryMonitorStore.GetMonitorRun(c.Context(), monitor.Namespace, id)
		if lookupErr == nil && existing.Phase != repositoryMonitorRunPhaseFailed {
			result.Duplicate++
			continue
		}
		if lookupErr != nil && !errors.Is(lookupErr, store.ErrNotFound) {
			return result, lookupErr
		}
		// Reconciliation ingests completed Tasks before queued inventory runs.
		// Persist this guard before exposing a run so its handoff sees the pause.
		if payload.Action == githubWebhookActionLabeled {
			if err := h.persistRepositoryMonitorPauseLabel(c, monitor, payload, target); err != nil {
				return result, err
			}
		}
		// Removal deliveries can arrive before or after a newer label addition.
		// Only fresh inventory may clear the durable pause guard.
		if lookupErr == nil {
			requeued, err := h.requeueFailedRepositoryMonitorEventRun(c, run)
			if err != nil {
				return result, err
			}
			if !requeued {
				result.Duplicate++
				continue
			}
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

func (h *Handlers) recordRepositoryMonitorPauseRejection(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, payload githubLabelWebhookPayload, target githubLabelTarget, delivery, permission string) error {
	metadata, err := json.Marshal(map[string]string{
		apiFieldAction: payload.Action, apiFieldLabel: payload.Label.Name,
		"delivery": delivery, "repository": payload.Repository.FullName,
		"sender": payload.Sender.Login, "permission": permission,
	})
	if err != nil {
		return err
	}
	event := &store.MonitorEvent{
		ID:               "mevt-" + githubRepositoryMonitorExactRunID(monitor, delivery+"|pause-rejected"),
		MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
		ItemKind: target.Kind, ItemNumber: int64(target.Number), ItemSHA: target.HeadSHA,
		EventType: "pause_label_rejected", Actor: "github-webhook",
		Summary:      "Pause-label reconciliation trigger rejected: sender permission is not allowed for this monitor",
		MetadataJSON: string(metadata),
	}
	if err := h.repositoryMonitorStore.CreateMonitorEvent(c.Context(), event); err != nil {
		// Delivery retries retain one rejection receipt, like accepted intake.
		existing, _, lookupErr := h.repositoryMonitorStore.ListMonitorEvents(c.Context(), store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, ID: event.ID, Limit: 1})
		if lookupErr == nil && len(existing) == 1 {
			return nil
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to record pause-label rejection: %v", err))
	}
	return nil
}

func (h *Handlers) persistRepositoryMonitorPauseLabel(c fiber.Ctx, monitor *corev1alpha1.RepositoryMonitor, payload githubLabelWebhookPayload, target githubLabelTarget) error {
	item, err := h.repositoryMonitorStore.GetMonitorItem(c.Context(), monitor.Namespace, monitor.Name, target.Kind, strconv.Itoa(target.Number))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var labels []string
	if item.LabelsJSON != "" {
		if err := json.Unmarshal([]byte(item.LabelsJSON), &labels); err != nil {
			return fmt.Errorf("invalid stored monitor item labels: %w", err)
		}
	}
	updated := make([]string, 0, len(labels)+1)
	for _, label := range labels {
		if !strings.EqualFold(strings.TrimSpace(label), strings.TrimSpace(payload.Label.Name)) {
			updated = append(updated, label)
		}
	}
	updated = append(updated, payload.Label.Name)
	data, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	item.LabelsJSON = string(data)
	return h.repositoryMonitorStore.UpsertMonitorItem(c.Context(), item)
}
