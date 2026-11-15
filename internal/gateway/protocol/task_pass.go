/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import "time"

const (
	// TaskPassPath is where the controller requests a per-call Task pass.
	TaskPassPath = "/v1/task-passes"
	// TaskToolPathPrefix prefixes each gateway-held tool's fixed endpoint.
	TaskToolPathPrefix = "/v1/task-tools/"

	// MaxTaskPassResponseBytes bounds a Task pass issuance response.
	MaxTaskPassResponseBytes = 16 << 10
	// MaxTaskPassBytes bounds an issued pass and the controller credential.
	MaxTaskPassBytes = 4096
	// MaxTaskPassTTL bounds how far past controller receipt a pass may expire.
	MaxTaskPassTTL = 5 * time.Minute
)

// TaskToolPath is the only path a gateway task-pass Tool may call.
func TaskToolPath(toolName string) string {
	return TaskToolPathPrefix + toolName
}

// TaskToolURL is the fixed endpoint for toolName at an authenticated gateway origin.
func TaskToolURL(origin, toolName string) string {
	return origin + TaskToolPath(toolName)
}
