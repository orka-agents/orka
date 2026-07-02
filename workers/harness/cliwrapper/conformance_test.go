package cliwrapper

import (
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/harness/harnesstest"
)

func TestHarnessConformance(t *testing.T) {
	harnesstest.RunHarnessConformance(t, func(t *testing.T, behavior harnesstest.FakeBehavior) (string, func()) {
		t.Helper()
		cfg := DefaultConfig()
		cfg.AllowUnauthenticated = true
		cfg.Generic.Command = testEchoCommand
		adapter := NewFakeAdapter(string(behavior))
		server, err := NewServer(cfg, adapter)
		if err != nil {
			t.Fatalf("NewServer: %v", err)
		}
		srv := httptest.NewServer(server.Handler())
		return srv.URL, srv.Close
	})
}
