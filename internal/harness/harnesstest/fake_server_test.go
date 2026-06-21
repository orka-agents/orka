package harnesstest

import "testing"

func TestFakeHarnessPassesConformance(t *testing.T) {
	RunHarnessConformance(t, func(t *testing.T, behavior FakeBehavior) (string, func()) {
		t.Helper()
		server := NewFakeHarnessServer(FakeHarnessConfig{Behavior: behavior})
		return server.URL(), server.Close
	})
}
