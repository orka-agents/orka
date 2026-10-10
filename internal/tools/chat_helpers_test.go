/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"math"
	"testing"
)

func TestChatGetIntArgSaturates(t *testing.T) {
	for value, want := range map[float64]int{9.3e18: math.MaxInt, -9.3e18: math.MinInt, 42: 42, -5: -5} {
		if got := chatGetIntArg(map[string]any{"n": value}, "n", 30); got != want {
			t.Errorf("chatGetIntArg(%v) = %d, want %d", value, got, want)
		}
	}
	if got := min(chatGetIntArg(map[string]any{"timeout": 9223372036854775807.0}, "timeout", 30), 60); got != 60 {
		t.Errorf("a huge wait_for_task timeout clamps to %d, want 60", got)
	}
}
