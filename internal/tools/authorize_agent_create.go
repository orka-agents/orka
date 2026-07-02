/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func authorizeAgentCreate(ctx context.Context, tc *ToolContext, agent *corev1alpha1.Agent) (string, bool) {
	if tc == nil || tc.AuthorizeAgentCreate == nil {
		return "", true
	}
	if err := tc.AuthorizeAgentCreate(ctx, agent); err != nil {
		result, _ := ChatToolErrorResult(err.Type, err.Message, err.Suggestion)
		return result, false
	}
	return "", true
}
