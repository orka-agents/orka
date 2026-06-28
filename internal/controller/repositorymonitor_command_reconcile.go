package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

const repositoryMonitorTriggerLabelCommand = "github_label_command"

func (r *RepositoryMonitorReconciler) enqueueAcceptedRepositoryMonitorCommands(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor) (bool, error) {
	queued := false
	cursor := ""
	for {
		commands, next, err := r.Store.ListCommandEvents(ctx, store.CommandEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Status: "accepted", Limit: 100, Cursor: cursor})
		if err != nil {
			return false, err
		}
		for i := range commands {
			command := commands[i]
			if strings.TrimSpace(command.Kind) == "" || command.Number == 0 {
				continue
			}
			runID := repositoryMonitorCommandRunIDFromCommand(command.ID)
			if handled, resetQueued, err := r.ensureNoExistingCommandRunBlocksQueue(ctx, monitor, command, runID); err != nil {
				return queued, err
			} else if handled {
				queued = queued || resetQueued
				continue
			}
			run := &store.MonitorRun{
				ID:               runID,
				MonitorNamespace: monitor.Namespace,
				MonitorName:      monitor.Name,
				Trigger:          repositoryMonitorTriggerLabelCommand,
				TargetKind:       command.Kind,
				TargetNumber:     command.Number,
				TargetSHA:        command.HeadSHA,
				CommandEventID:   command.ID,
				Phase:            repositoryMonitorRunPhaseQueued,
				StartedAt:        time.Now(),
			}
			if err := r.Store.CreateMonitorRun(ctx, run); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "conflict") {
					continue
				}
				return queued, err
			}
			queued = true
			if err := r.createMonitorEvent(ctx, monitor, run.ID, command.Kind, command.Number, command.HeadSHA, "command_run_queued", fmt.Sprintf("Queued monitor run for accepted command %s", command.ID), map[string]any{"commandEventID": command.ID, "intent": command.Intent}); err != nil {
				return queued, err
			}
		}
		if next == "" {
			return queued, nil
		}
		cursor = next
	}
}

func (r *RepositoryMonitorReconciler) ensureNoExistingCommandRunBlocksQueue(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, command store.CommandEvent, runID string) (bool, bool, error) {
	runs, _, err := r.Store.ListMonitorRuns(ctx, store.MonitorRunFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, Limit: 100})
	if err != nil {
		return false, false, err
	}
	for _, run := range runs {
		if run.CommandEventID != command.ID {
			continue
		}
		if run.Phase == repositoryMonitorRunPhaseSucceeded && command.Intent == repositoryMonitorCommandIntentAutomerge {
			if item, err := r.Store.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, command.Kind, fmt.Sprintf("%d", command.Number)); err == nil && item.AutomergeState == repositoryMonitorAutomergeStatePending {
				now := time.Now()
				run.Phase = repositoryMonitorRunPhaseQueued
				run.StartedAt = now
				run.CompletedAt = nil
				run.Error = ""
				if err := r.Store.UpdateMonitorRun(ctx, &run); err != nil {
					return false, false, err
				}
				return true, true, nil
			}
		}
		if run.Phase != repositoryMonitorRunPhaseFailed {
			return true, false, nil
		}
		if run.ID == runID && strings.Contains(run.Error, "failed to signal repository monitor run") {
			now := time.Now()
			run.Phase = repositoryMonitorRunPhaseQueued
			run.StartedAt = now
			run.CompletedAt = nil
			run.Error = ""
			if err := r.Store.UpdateMonitorRun(ctx, &run); err != nil {
				return false, false, err
			}
			return true, true, nil
		}
		return true, false, nil
	}
	return false, false, nil
}

func repositoryMonitorCommandRunIDFromCommand(commandID string) string {
	sum := sha256.Sum256([]byte(commandID + "|run"))
	return "monrun-" + hex.EncodeToString(sum[:])[:12]
}
