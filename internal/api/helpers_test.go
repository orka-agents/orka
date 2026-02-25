/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import "time"

// DefaultChatConfig returns a ChatConfig with default values for testing.
func DefaultChatConfig() ChatConfig {
	return ChatConfig{
		Enabled:         true,
		MaxIterations:   20,
		MaxDuration:     5 * time.Minute,
		ToolTimeout:     60 * time.Second,
		MaxConcurrent:   10,
		MaxTasksPerTurn: 5,
		MaxSessionSize:  500 * 1024,
	}
}
