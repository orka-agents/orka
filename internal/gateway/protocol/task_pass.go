/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package protocol

import "time"

const (
	TaskPassPath       = "/v1/task-passes"
	TaskToolPathPrefix = "/v1/task-tools/"

	MaxTaskPassResponseBytes = 16 << 10
	MaxTaskPassBytes         = 4096
	MaxTaskPassTTL           = 5 * time.Minute
)

func TaskToolPath(toolName string) string {
	return TaskToolPathPrefix + toolName
}

func TaskToolURL(origin, toolName string) string {
	return origin + TaskToolPath(toolName)
}
