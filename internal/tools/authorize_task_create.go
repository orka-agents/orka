/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
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
