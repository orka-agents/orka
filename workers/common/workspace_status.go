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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/internal/workspace"
	"github.com/orka-agents/orka/internal/workspace/statusrules"
)

const workspaceStatusMaxRetries = 3

type executionWorkspaceStatusUpdate = statusrules.Update

type executionWorkspaceStatusOption func(*executionWorkspaceStatusUpdate)

func withExecutionWorkspaceReadyResult(ready *workspace.ReadyResult) executionWorkspaceStatusOption {
	return func(update *executionWorkspaceStatusUpdate) {
		statusrules.ApplyReadyResult(update, ready)
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
	}
	observedAt := metav1.NewTime(time.Now().UTC())
	update.ObservedAt = &observedAt
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
