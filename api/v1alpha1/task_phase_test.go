/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import "testing"

func TestTaskPhaseIsTerminal(t *testing.T) {
	for phase, want := range map[TaskPhase]bool{
		TaskPhasePending:    false,
		TaskPhaseScheduled:  false,
		TaskPhaseRunning:    false,
		TaskPhaseFinalizing: false,
		TaskPhaseSucceeded:  true,
		TaskPhaseFailed:     true,
		TaskPhaseCancelled:  true,
		"":                  false,
		"Unknown":           false,
		"succeeded":         false,
	} {
		if got := phase.IsTerminal(); got != want {
			t.Errorf("TaskPhase(%q).IsTerminal() = %v, want %v", phase, got, want)
		}
	}
}
