/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// DeleteSessionTool deletes a session and its transcript data.
type DeleteSessionTool struct{}

func (t *DeleteSessionTool) Name() string { return "delete_session" }

func (t *DeleteSessionTool) Description() string {
	return "Delete a session and its transcript data."
}

func (t *DeleteSessionTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sessionId": map[string]any{"type": "string", "description": "Session ID to delete"},
			"namespace": map[string]any{"type": "string", "description": "Namespace"},
		},
		"required": []string{"sessionId"},
	})
}

func (t *DeleteSessionTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult("internal_error", "missing tool context", "")
	}

	var a map[string]any
	if err := json.Unmarshal(args, &a); err != nil {
		return ChatToolErrorResult("invalid_arguments", fmt.Sprintf("failed to parse arguments: %v", err), "Ensure arguments are valid JSON")
	}

	sessionID := chatGetStringArg(a, "sessionId")
	if sessionID == "" {
		return ChatToolErrorResult("invalid_arguments", "sessionId is required", "Provide the session ID")
	}

	namespace := chatGetStringArgDefault(a, "namespace", tc.Namespace)

	if tc.SessionDeleter == nil {
		return ChatToolErrorResult("internal_error", "session manager not configured", "")
	}

	if err := tc.SessionDeleter.DeleteSession(ctx, namespace, sessionID); err != nil {
		return classifyChatK8sErr(err)
	}

	return ChatToolSuccess(map[string]any{
		"sessionId": sessionID,
		"message":   "Session deleted",
	})
}
