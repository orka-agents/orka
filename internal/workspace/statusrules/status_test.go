package statusrules

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func TestUpdateStatusSanitizesInboundFields(t *testing.T) {
	observed := metav1.NewTime(time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC))
	status := Update{
		Provider: corev1alpha1.WorkspaceProviderSubstrate,
		Phase:    corev1alpha1.ExecutionWorkspacePhaseReady,
		Reason:   corev1alpha1.ExecutionWorkspaceReasonReady,
		Placement: &corev1alpha1.ExecutionWorkspacePlacementStatus{
			WorkerNamespace: "  substrate-system  ",
			WorkerPool:      strings.Repeat("p", 300),
			WorkerPodName:   " actor-pod ",
		},
		Density: &corev1alpha1.ExecutionWorkspaceDensityStatus{
			WorkerCount:         -1,
			ActorCount:          2,
			RunningActorCount:   -3,
			SuspendedActorCount: 4,
			ActorsPerWorker:     "  2.00  ",
		},
		Message:    "  " + strings.Repeat("m", 1100) + "  ",
		ObservedAt: &observed,
	}.Status()

	if status.LastUpdateTime == nil || !status.LastUpdateTime.Equal(&observed) {
		t.Fatalf("LastUpdateTime = %#v, want observed", status.LastUpdateTime)
	}
	if status.Placement == nil || status.Placement.WorkerNamespace != "substrate-system" || status.Placement.WorkerPodName != "actor-pod" || len(status.Placement.WorkerPool) != 256 {
		t.Fatalf("sanitized placement = %#v", status.Placement)
	}
	if status.Density == nil || status.Density.WorkerCount != 0 || status.Density.ActorCount != 2 || status.Density.RunningActorCount != 0 || status.Density.SuspendedActorCount != 4 || status.Density.ActorsPerWorker != "2.00" {
		t.Fatalf("sanitized density = %#v", status.Density)
	}
	if !strings.HasSuffix(status.Message, "...<truncated>") || len(status.Message) != len("...<truncated>")+1024 {
		t.Fatalf("sanitized message len=%d suffix=%q", len(status.Message), status.Message[len(status.Message)-15:])
	}
}

func TestUpdateStatusDropsEmptyPlacementAndDensity(t *testing.T) {
	status := Update{
		Provider:  corev1alpha1.WorkspaceProviderAgentSandbox,
		Phase:     corev1alpha1.ExecutionWorkspacePhasePending,
		Reason:    corev1alpha1.ExecutionWorkspaceReasonClaimed,
		Placement: &corev1alpha1.ExecutionWorkspacePlacementStatus{WorkerNamespace: " ", WorkerPool: " ", WorkerPodName: " "},
		Density:   &corev1alpha1.ExecutionWorkspaceDensityStatus{WorkerCount: -1, ActorCount: -1, RunningActorCount: -1, SuspendedActorCount: -1, ActorsPerWorker: " "},
	}.Status()
	if status.Placement != nil || status.Density != nil {
		t.Fatalf("status placement=%#v density=%#v, want nil/nil", status.Placement, status.Density)
	}
}

func TestValidInboundStatusAllowlistsValues(t *testing.T) {
	valid := &corev1alpha1.ExecutionWorkspaceStatus{
		Provider:      corev1alpha1.WorkspaceProviderSubstrate,
		Phase:         corev1alpha1.ExecutionWorkspacePhaseReady,
		Reason:        corev1alpha1.ExecutionWorkspaceReasonReady,
		ReusePolicy:   corev1alpha1.WorkspaceReusePolicySession,
		CleanupPolicy: corev1alpha1.WorkspaceCleanupPolicyDelete,
	}
	if !HasRequiredInboundFields(valid) || !ValidInboundStatus(valid) {
		t.Fatalf("valid status rejected")
	}
	invalid := *valid
	invalid.Provider = "provider-native"
	if ValidInboundStatus(&invalid) {
		t.Fatalf("unsupported provider accepted")
	}
	invalid = *valid
	invalid.CleanupPolicy = "snapshot"
	if ValidInboundStatus(&invalid) {
		t.Fatalf("unsupported cleanup policy accepted")
	}
}

func TestPreserveReadyTelemetry(t *testing.T) {
	previous := &corev1alpha1.ExecutionWorkspaceStatus{
		Phase: corev1alpha1.ExecutionWorkspacePhaseReady,
		Placement: &corev1alpha1.ExecutionWorkspacePlacementStatus{
			WorkerNamespace: "substrate-system",
		},
		Density:       &corev1alpha1.ExecutionWorkspaceDensityStatus{ActorCount: 3},
		ResumeLatency: &metav1.Duration{Duration: 2 * time.Second},
	}
	status := &corev1alpha1.ExecutionWorkspaceStatus{Reason: corev1alpha1.ExecutionWorkspaceReasonCommandFailed}
	PreserveReadyTelemetry(status, previous)
	if status.Placement == nil || status.Density == nil || status.ResumeLatency == nil {
		t.Fatalf("ready telemetry was not preserved: %#v", status)
	}
	status = &corev1alpha1.ExecutionWorkspaceStatus{Reason: corev1alpha1.ExecutionWorkspaceReasonReadinessFailed}
	PreserveReadyTelemetry(status, previous)
	if status.Placement != nil || status.Density != nil || status.ResumeLatency != nil {
		t.Fatalf("unexpected telemetry preservation for readiness failure: %#v", status)
	}
}
