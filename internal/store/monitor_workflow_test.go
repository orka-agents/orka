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
		" fix ":          repositoryMonitorDesiredActionRepair,
		" approve_plan ": "approve",
		" review ":       "review",
	}
	for input, want := range tests {
		if got := RepositoryMonitorDesiredActionForIntent(input); got != want {
			t.Errorf("RepositoryMonitorDesiredActionForIntent(%q) = %q, want %q", input, got, want)
		}
	}
}
