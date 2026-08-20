package v2

import "testing"

func TestUsageUpdateValidateAllowsZeroSnapshot(t *testing.T) {
	event := UpdateEvent{Kind: UpdateUsage, Usage: &UsageUpdate{}}
	if err := event.Validate(); err != nil {
		t.Fatalf("validate zero usage snapshot: %v", err)
	}
}
