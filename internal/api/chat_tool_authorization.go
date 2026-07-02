/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"

	"github.com/orka-agents/orka/internal/tools"
)

func chatToolAuthorizationError[T any](authorize func(context.Context, *T) error, ctx context.Context, obj *T, suggestion string) *tools.ChatToolError {
	if authorize == nil {
		return nil
	}
	if err := authorize(ctx, obj); err != nil {
		return &tools.ChatToolError{
			Type:       "authorization_failed",
			Message:    err.Error(),
			Suggestion: suggestion,
		}
	}
	return nil
}
