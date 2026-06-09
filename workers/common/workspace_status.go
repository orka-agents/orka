/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

const workspaceStatusMaxRetries = 3

type executionWorkspaceStatusUpdate struct {
	Provider      corev1alpha1.WorkspaceProvider                  `json:"provider"`
	TemplateRef   *corev1alpha1.WorkspaceTemplateReference        `json:"templateRef,omitempty"`
	Phase         corev1alpha1.ExecutionWorkspacePhase            `json:"phase"`
	Reason        corev1alpha1.ExecutionWorkspaceReason           `json:"reason"`
	ReusePolicy   corev1alpha1.WorkspaceReusePolicy               `json:"reusePolicy,omitempty"`
	CleanupPolicy corev1alpha1.WorkspaceCleanupPolicy             `json:"cleanupPolicy,omitempty"`
	Reused        bool                                            `json:"reused,omitempty"`
	Placement     *corev1alpha1.ExecutionWorkspacePlacementStatus `json:"placement,omitempty"`
	Density       *corev1alpha1.ExecutionWorkspaceDensityStatus   `json:"density,omitempty"`
	ResumeLatency *metav1.Duration                                `json:"resumeLatency,omitempty"`
	Message       string                                          `json:"message,omitempty"`
	ObservedAt    time.Time                                       `json:"observedAt"`
}

type executionWorkspaceStatusOption func(*executionWorkspaceStatusUpdate)

func withExecutionWorkspaceReadyResult(ready *workspace.ReadyResult) executionWorkspaceStatusOption {
	return func(update *executionWorkspaceStatusUpdate) {
		if ready == nil {
			return
		}
		if !ready.Placement.IsZero() {
			update.Placement = &corev1alpha1.ExecutionWorkspacePlacementStatus{
				WorkerNamespace: ready.Placement.WorkerNamespace,
				WorkerPool:      ready.Placement.WorkerPool,
				WorkerPodName:   ready.Placement.WorkerPodName,
			}
		}
		if !ready.Density.IsZero() {
			update.Density = &corev1alpha1.ExecutionWorkspaceDensityStatus{
				WorkerCount:         int32(max(ready.Density.WorkerCount, 0)),
				ActorCount:          int32(max(ready.Density.ActorCount, 0)),
				RunningActorCount:   int32(max(ready.Density.RunningActorCount, 0)),
				SuspendedActorCount: int32(max(ready.Density.SuspendedActorCount, 0)),
				ActorsPerWorker:     ready.Density.ActorsPerWorker,
			}
		}
		if ready.ResumeLatency > 0 {
			update.ResumeLatency = &metav1.Duration{Duration: ready.ResumeLatency}
		}
	}
}

func submitExecutionWorkspaceStatus(
	env workerenv.ExecutionWorkspaceEnv,
	phase corev1alpha1.ExecutionWorkspacePhase,
	reason corev1alpha1.ExecutionWorkspaceReason,
	reused bool,
	message string,
	options ...executionWorkspaceStatusOption,
) {
	endpoint := strings.TrimSpace(env.StatusEndpoint)
	if endpoint == "" {
		return
	}
	update := executionWorkspaceStatusUpdate{
		Provider: corev1alpha1.WorkspaceProvider(env.Provider),
		TemplateRef: &corev1alpha1.WorkspaceTemplateReference{
			Name:      env.TemplateName,
			Namespace: env.TemplateNamespace,
		},
		Phase:         phase,
		Reason:        reason,
		ReusePolicy:   corev1alpha1.WorkspaceReusePolicy(env.ReusePolicy),
		CleanupPolicy: corev1alpha1.WorkspaceCleanupPolicy(env.CleanupPolicy),
		Reused:        reused,
		Message:       message,
		ObservedAt:    time.Now().UTC(),
	}
	for _, option := range options {
		option(&update)
	}
	body, err := json.Marshal(update)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workspace status encode failed: %v\n", err)
		return
	}

	saToken := workerServiceAccountToken()
	var lastErr error
	for attempt := range workspaceStatusMaxRetries {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		lastErr = doPostOnceWithContentType(endpoint, body, saToken, "application/json", 10*time.Second)
		if lastErr == nil {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "workspace status update failed: %v\n", lastErr)
}
