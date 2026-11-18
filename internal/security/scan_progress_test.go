/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package security

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func stageTask(name, stage string, phase corev1alpha1.TaskPhase) corev1alpha1.Task {
	return corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{labels.LabelSecurityStage: stage}},
		Status:     corev1alpha1.TaskStatus{Phase: phase},
	}
}

func TestScanStageProgressGroupsByLabelInPipelineOrder(t *testing.T) {
	tasks := []corev1alpha1.Task{
		stageTask("goof-review-slice-3-mislabeled-threat-model", StageReview, corev1alpha1.TaskPhaseRunning),
		stageTask("goof-threat-model", StageThreatModel, corev1alpha1.TaskPhaseSucceeded),
		stageTask("goof-mapper", StageMapper, corev1alpha1.TaskPhaseSucceeded),
		stageTask("goof-review-slice-1", StageReview, corev1alpha1.TaskPhaseSucceeded),
		stageTask("goof-review-slice-2", StageReview, corev1alpha1.TaskPhaseFailed),
		stageTask("goof-review-slice-4", StageReview, corev1alpha1.TaskPhasePending),
		stageTask("goof-review-slice-5", StageReview, ""),
		stageTask("goof-review-slice-6", StageReview, corev1alpha1.TaskPhaseCancelled),
		{ObjectMeta: metav1.ObjectMeta{Name: "unlabeled"}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning}},
	}
	got := ScanStageProgress(tasks)
	if len(got) != len(ScanStageOrder) {
		t.Fatalf("stages = %d, want %d: %+v", len(got), len(ScanStageOrder), got)
	}
	for i, stage := range ScanStageOrder {
		if got[i].Stage != stage || got[i].Label != ScanStageLabel(stage) {
			t.Fatalf("stage %d = %+v, want %s", i, got[i], stage)
		}
	}
	if got[0].Tasks != 1 || got[0].Succeeded != 1 {
		t.Fatalf("threat model = %+v", got[0])
	}
	review := got[2]
	if review.Tasks != 6 || review.Pending != 2 || review.Running != 1 || review.Succeeded != 1 || review.Failed != 1 || review.Cancelled != 1 {
		t.Fatalf("review = %+v", review)
	}
	if len(review.FailedTasks) != 2 || review.FailedTasks[0] != "goof-review-slice-2" || review.FailedTasks[1] != "goof-review-slice-6" {
		t.Fatalf("review failed tasks = %v", review.FailedTasks)
	}
	// Stages the scan has not reached are present with zero counts.
	if got[3].Tasks != 0 || got[4].Tasks != 0 || got[3].Label != "validate findings" {
		t.Fatalf("unreached stages = %+v %+v", got[3], got[4])
	}
}

func TestScanStageProgressKeepsUnknownStagesAfterKnownOnes(t *testing.T) {
	got := ScanStageProgress([]corev1alpha1.Task{stageTask("x", "future-stage", corev1alpha1.TaskPhaseRunning)})
	if len(got) != len(ScanStageOrder)+1 || got[len(got)-1].Stage != "future-stage" || got[len(got)-1].Running != 1 {
		t.Fatalf("unknown stage handling = %+v", got)
	}
}

func TestIsActiveScanRunPhase(t *testing.T) {
	for phase, want := range map[string]bool{"pending": true, "Running": true, "succeeded": false, "failed": false, "": false} {
		if got := IsActiveScanRunPhase(phase); got != want {
			t.Errorf("IsActiveScanRunPhase(%q) = %v, want %v", phase, got, want)
		}
	}
}
