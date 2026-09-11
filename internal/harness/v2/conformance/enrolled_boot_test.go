package conformance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/harness/v2/conformance"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
)

func TestCheckRejectsUnenrolledBootBeforeAnyMutation(t *testing.T) {
	target, config := testTargetAndConfig(t)
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	target.BaseURL, target.ProbeLifecycle = server.URL(), true
	fence := server.Fence()
	fence.SupervisorBootID = "enrolled-prior-boot"
	target.ExpectedFence = &fence
	called := false
	target.BeforeMutation = func(context.Context) error { called = true; return nil }
	result := conformance.Check(t.Context(), target)
	if result.Passed || called || server.Counts().SessionCreates != 0 || !strings.Contains(result.Message, "enrolled supervisor fence") {
		t.Fatalf("unwitnessed boot reached lifecycle probe: passed=%t, mutation=%t, message=%s", result.Passed, called, result.Message)
	}
}

func TestCheckMutationGuardCoversDirectReplayRequests(t *testing.T) {
	target, config := testTargetAndConfig(t)
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	target.BaseURL, target.ProbeLifecycle = server.URL(), true
	fence := server.Fence()
	target.ExpectedFence = &fence
	blocked := false
	target.BeforeMutation = func(context.Context) error {
		if server.Counts().SessionCreates > 0 {
			blocked = true
			return errors.New("enrolled container changed before replay")
		}
		return nil
	}
	result := conformance.Check(t.Context(), target)
	if result.Passed || !blocked || server.Counts().SessionCreates != 1 || server.Counts().ReplayClassifications != 0 {
		t.Fatalf("direct replay bypassed physical guard: passed=%t, blocked=%t, counts=%+v", result.Passed, blocked, server.Counts())
	}
}
