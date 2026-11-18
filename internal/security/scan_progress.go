/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package security

import (
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

// StageProgress counts one scan run's Tasks for a single pipeline stage.
// Counts come from the orka.ai/security-stage label and the Task phase,
// never from Task names.
type StageProgress struct {
	Stage     string `json:"stage"`
	Label     string `json:"label"`
	Tasks     int    `json:"tasks"`
	Pending   int    `json:"pending"`
	Running   int    `json:"running"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	Cancelled int    `json:"cancelled"`
	// FailedTasks names the failed and cancelled Tasks so a reader can go
	// straight to `orka task status <name>`.
	FailedTasks []string `json:"failedTasks,omitempty"`
}

// ScanStageOrder is the pipeline order stages run in.
var ScanStageOrder = []string{StageThreatModel, StageMapper, StageReview, StageValidation, StagePatch}

// ScanStageLabel returns the plain-language name of a stage for people.
func ScanStageLabel(stage string) string {
	switch stage {
	case StageThreatModel:
		return "threat model"
	case StageMapper:
		return "map the code"
	case StageReview:
		return "review slices"
	case StageValidation:
		return "validate findings"
	case StagePatch:
		return "patch findings"
	default:
		return stage
	}
}

// ScanRunOwnsTask reports whether a Task belongs to this scan run: it must
// carry the run's ID label and be controlled by the RepositoryScan that
// admitted the run. Labels alone are mutable and do not prove membership.
func ScanRunOwnsTask(run *store.ScanRun, scan *corev1alpha1.RepositoryScan, task *corev1alpha1.Task) bool {
	return scanRunOwnsTask(run, scan, task)
}

// IsActiveScanRunPhase reports whether a scan run is still in progress.
func IsActiveScanRunPhase(phase string) bool {
	return activeScanRunPhase(strings.ToLower(strings.TrimSpace(phase)))
}

// ScanStageProgress groups a scan run's Tasks by stage in pipeline order and
// counts each phase. Every known stage is present even with zero Tasks, so
// a scan that has not reached a stage yet still shows it. Tasks whose stage
// label is unknown are appended after the known stages under that label.
func ScanStageProgress(tasks []corev1alpha1.Task) []StageProgress {
	byStage := map[string]*StageProgress{}
	order := make([]string, 0, len(ScanStageOrder))
	for _, stage := range ScanStageOrder {
		byStage[stage] = &StageProgress{Stage: stage, Label: ScanStageLabel(stage)}
		order = append(order, stage)
	}
	for i := range tasks {
		task := &tasks[i]
		stage := strings.TrimSpace(task.Labels[labels.LabelSecurityStage])
		if stage == "" {
			continue
		}
		progress, ok := byStage[stage]
		if !ok {
			progress = &StageProgress{Stage: stage, Label: ScanStageLabel(stage)}
			byStage[stage] = progress
			order = append(order, stage)
		}
		progress.Tasks++
		switch task.Status.Phase {
		case corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing:
			progress.Running++
		case corev1alpha1.TaskPhaseSucceeded:
			progress.Succeeded++
		case corev1alpha1.TaskPhaseFailed:
			progress.Failed++
			progress.FailedTasks = append(progress.FailedTasks, task.Name)
		case corev1alpha1.TaskPhaseCancelled:
			progress.Cancelled++
			progress.FailedTasks = append(progress.FailedTasks, task.Name)
		default:
			// "", Pending, and Scheduled have not started yet.
			progress.Pending++
		}
	}
	out := make([]StageProgress, 0, len(order))
	for _, stage := range order {
		out = append(out, *byStage[stage])
	}
	return out
}
