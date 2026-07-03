/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func authorizeTaskDelete(ctx context.Context, tc *ToolContext, task *corev1alpha1.Task) (string, bool) {
	if tc == nil || tc.AuthorizeTaskDelete == nil {
		return "", true
	}
	if err := tc.AuthorizeTaskDelete(ctx, task); err != nil {
		result, _ := ChatToolErrorResult(err.Type, err.Message, err.Suggestion)
		return result, false
	}
	return "", true
}

func authorizeAgentUpdate(ctx context.Context, tc *ToolContext, agent *corev1alpha1.Agent) (string, bool) {
	if tc == nil || tc.AuthorizeAgentUpdate == nil {
		return "", true
	}
	if err := tc.AuthorizeAgentUpdate(ctx, agent); err != nil {
		result, _ := ChatToolErrorResult(err.Type, err.Message, err.Suggestion)
		return result, false
	}
	return "", true
}

func authorizeAgentDelete(ctx context.Context, tc *ToolContext, agent *corev1alpha1.Agent) (string, bool) {
	if tc == nil || tc.AuthorizeAgentDelete == nil {
		return "", true
	}
	if err := tc.AuthorizeAgentDelete(ctx, agent); err != nil {
		result, _ := ChatToolErrorResult(err.Type, err.Message, err.Suggestion)
		return result, false
	}
	return "", true
}
