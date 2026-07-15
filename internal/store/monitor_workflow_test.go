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
