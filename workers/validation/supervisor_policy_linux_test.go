//go:build linux

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidationWorkerMustRecheckItsOwnCgroup(t *testing.T) {
	processChecks, cgroupChecks := 0, 0
	missing := errors.New("finite pids.max missing")
	process := func() error { processChecks++; return nil }
	cgroups := func() error {
		cgroupChecks++
		if cgroupChecks == 2 {
			return missing
		}
		return nil
	}
	require.NoError(t, verifySupervisorPolicies(process, cgroups))
	require.ErrorIs(t, verifySupervisorPolicies(process, cgroups), missing)
	require.Equal(t, 2, processChecks)
	require.Equal(t, 2, cgroupChecks, "a prior preparation or worker qualification cannot replace this worker's check")
}
