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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/workerenv"
)

const workspaceStatusMaxRetries = 3

type executionWorkspaceStatusUpdate struct {
	Provider      corev1alpha1.WorkspaceProvider           `json:"provider"`
	TemplateRef   *corev1alpha1.WorkspaceTemplateReference `json:"templateRef,omitempty"`
	Phase         corev1alpha1.ExecutionWorkspacePhase     `json:"phase"`
	Reason        corev1alpha1.ExecutionWorkspaceReason    `json:"reason"`
	ReusePolicy   corev1alpha1.WorkspaceReusePolicy        `json:"reusePolicy,omitempty"`
	CleanupPolicy corev1alpha1.WorkspaceCleanupPolicy      `json:"cleanupPolicy,omitempty"`
	Reused        bool                                     `json:"reused,omitempty"`
	Message       string                                   `json:"message,omitempty"`
	ObservedAt    time.Time                                `json:"observedAt"`
}

func submitExecutionWorkspaceStatus(
	env workerenv.ExecutionWorkspaceEnv,
	phase corev1alpha1.ExecutionWorkspacePhase,
	reason corev1alpha1.ExecutionWorkspaceReason,
	reused bool,
	message string,
) {
	endpoint := strings.TrimSpace(env.StatusEndpoint)
	if endpoint == "" {
		return
	}
	body, err := json.Marshal(executionWorkspaceStatusUpdate{
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
	})
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
