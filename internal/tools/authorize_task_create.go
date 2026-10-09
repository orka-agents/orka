/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func authorizeTaskCreate(ctx context.Context, tc *ToolContext, task *corev1alpha1.Task) (string, bool) {
	if tc == nil || tc.AuthorizeTaskCreate == nil {
		return "", true
	}
	if err := tc.AuthorizeTaskCreate(ctx, task); err != nil {
		result, _ := ChatToolErrorResult(err.Type, err.Message, err.Suggestion)
		return result, false
	}
	return "", true
}

// sealTaskCreate hands a just-created Task to the API's sealer, when one is
// installed, so its server-assigned UID is bound to the stamped requester.
func sealTaskCreate(ctx context.Context, tc *ToolContext, task *corev1alpha1.Task) {
	if tc == nil || tc.SealTaskCreate == nil || tc.Client == nil {
		return
	}
	// An unsealed child is still created; it fails closed for connector
	// tools, so the failure is recorded rather than returned to the agent.
	if err := tc.SealTaskCreate(ctx, tc.Client, task); err != nil {
		log.FromContext(ctx).Info("child task could not be sealed for connector use", "task", task.Name, "error", err.Error())
	}
}
