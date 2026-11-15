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
	for status, want := range map[string]bool{
		RepositoryMonitorWorkActionStatusQueued:       true,
		RepositoryMonitorWorkActionStatusLeased:       true,
		RepositoryMonitorWorkActionStatusRunning:      true,
		RepositoryMonitorWorkActionStatusRetryPending: true,
		RepositoryMonitorWorkActionStatusSucceeded:    false,
		RepositoryMonitorWorkActionStatusFailed:       false,
		RepositoryMonitorWorkActionStatusBlocked:      false,
		RepositoryMonitorWorkActionStatusCancelled:    false,
		RepositoryMonitorWorkActionStatusCompleted:    false,
		"":          false,
		"processed": false,
	} {
		if got := IsRepositoryMonitorWorkActionActive(status); got != want {
			t.Errorf("IsRepositoryMonitorWorkActionActive(%q) = %v, want %v", status, got, want)
		}
	}
	for _, status := range RepositoryMonitorWorkActionActiveStatuses() {
		if !IsRepositoryMonitorWorkActionActive(status) {
			t.Errorf("active status list contains inactive %q", status)
		}
	}
}
