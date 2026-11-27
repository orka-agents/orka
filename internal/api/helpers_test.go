/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import "time"

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
