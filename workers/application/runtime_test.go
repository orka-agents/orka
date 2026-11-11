//go:build linux

package main

import (
	"context"
	"os"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func TestRuntimeQualificationIsExplicitNotANameInference(t *testing.T) {
	t.Setenv("ORKA_APPLICATION_RUNTIME_PROFILE", "")
	t.Setenv("ORKA_APPLICATION_NODE_NAME", "qualified-node")
	t.Setenv("RUNTIME_CLASS_NAME", "gvisor-bounded")
	config, err := runtimeConfig()
	require.NoError(t, err)
	require.Nil(t, config)
	require.Error(t, verifyRuntimeManifest(pv.Manifest{Environment: pv.Environment{Dependencies: map[string]string{
		pv.ApplicationRuntimeQualificationKey: pv.Digest([]byte("not-an-activation")),
	}}}))
	t.Setenv("ORKA_APPLICATION_RUNTIME_PROFILE", "runsc")
	_, err = runtimeConfig()
	require.Error(t, err)
	t.Setenv("ORKA_APPLICATION_RUNTIME_PROFILE", "gvisor")
	_, err = runtimeConfig()
	require.Error(t, err)
}

func TestQualifiedGuestLimitsCannotComeFromAnOrdinaryFile(t *testing.T) {
	_, err := readGuestCgroupLimit("runtime_test.go")
	require.Error(t, err, "ordinary workspace files cannot impersonate cgroup readback")
}

func TestReplicaProcessRejectsMissingOrExpiredDeadlineBeforeExecution(t *testing.T) {
	require.Error(t, runReplicaProcess(context.Background(), []string{"/this-path-must-not-run"}, nil))
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	require.Error(t, runReplicaProcess(ctx, []string{"/this-path-must-not-run"}, nil))
}

func TestReplicaFixtureDeadlineChild(_ *testing.T) {
	if os.Getenv("ORKA_TEST_REPLICA_DEADLINE_CHILD") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestReplicaProcessTerminatesFixedChildAtAbsoluteDeadline(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = runReplicaProcess(ctx, []string{executable, "-test.run=^TestReplicaFixtureDeadlineChild$"},
		[]string{"ORKA_TEST_REPLICA_DEADLINE_CHILD=1"})
	require.Error(t, err)
	require.Less(t, time.Since(start), 5*time.Second)
	require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
}
