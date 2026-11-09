package store

import "testing"

func TestRepositoryMonitorWorkActionIDIsDeterministicAndUnambiguous(t *testing.T) {
	got := RepositoryMonitorWorkActionID(" cmd-a-b ", " plan ")
	if got != RepositoryMonitorWorkActionID("cmd-a-b", "plan") {
		t.Fatalf("trimmed work action IDs differ: %q", got)
	}
	if got == RepositoryMonitorWorkActionID("cmd-a", "b-plan") {
		t.Fatalf("work action ID components collide: %q", got)
	}
}

func TestRepositoryMonitorDesiredActionForIntentNormalizesBeforeMapping(t *testing.T) {
	tests := map[string]string{
		" fix ": repositoryMonitorDesiredActionRepair,

		" review ": "review",
	}
	for input, want := range tests {
		if got := RepositoryMonitorDesiredActionForIntent(input); got != want {
			t.Errorf("RepositoryMonitorDesiredActionForIntent(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRepositoryMonitorWorkActionStatusClassification(t *testing.T) {
	for status, want := range map[string][2]bool{ // {active, terminal}
		RepositoryMonitorWorkActionStatusQueued:       {true, false},
		RepositoryMonitorWorkActionStatusLeased:       {true, false},
		RepositoryMonitorWorkActionStatusRunning:      {true, false},
		RepositoryMonitorWorkActionStatusRetryPending: {true, false},
		RepositoryMonitorWorkActionStatusSucceeded:    {false, true},
		RepositoryMonitorWorkActionStatusFailed:       {false, true},
		RepositoryMonitorWorkActionStatusBlocked:      {false, true},
		RepositoryMonitorWorkActionStatusCancelled:    {false, true},
		RepositoryMonitorWorkActionStatusCompleted:    {false, true},
		"":          {false, false},
		"processed": {false, false},
	} {
		if got := IsRepositoryMonitorWorkActionActive(status); got != want[0] {
			t.Errorf("IsRepositoryMonitorWorkActionActive(%q) = %v, want %v", status, got, want[0])
		}
		if got := IsRepositoryMonitorWorkActionTerminal(status); got != want[1] {
			t.Errorf("IsRepositoryMonitorWorkActionTerminal(%q) = %v, want %v", status, got, want[1])
		}
	}
	for _, status := range RepositoryMonitorWorkActionActiveStatuses() {
		if !IsRepositoryMonitorWorkActionActive(status) {
			t.Errorf("active status list contains inactive %q", status)
		}
	}
}
