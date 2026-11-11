//go:build linux

package patchverification

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type untrustedIsolationReason struct{ value string }

func (e untrustedIsolationReason) Error() string                      { panic("error text must not be rendered") }
func (e untrustedIsolationReason) PreparationIsolationReason() string { return e.value }

func TestCgroupPreflightHasTypedDiagnosticReasons(t *testing.T) {
	baseline := map[string]string{
		"/sys/fs/cgroup/memory.max":      "536870912",
		"/sys/fs/cgroup/pids.max":        "512",
		"/sys/fs/cgroup/memory.swap.max": "0",
		"/sys/fs/cgroup/cpu.max":         "100000 100000",
	}
	for _, test := range []struct {
		file     string
		reason   string
		sentinel workerCgroupFailure
	}{
		{"memory.max", "cgroup-memory", workerCgroupMemory},
		{"pids.max", "cgroup-pids", workerCgroupPIDs},
		{"memory.swap.max", "cgroup-swap", workerCgroupSwap},
		{"cpu.max", "cgroup-cpu", workerCgroupCPU},
	} {
		for _, missing := range []bool{false, true} {
			t.Run(test.reason, func(t *testing.T) {
				err := checkWorkerCgroupBounds(func(path string) ([]byte, error) {
					if path == "/sys/fs/cgroup/"+test.file {
						if missing {
							return nil, unrenderablePreparationError{}
						}
						return []byte("max"), nil
					}
					return []byte(baseline[path]), nil
				})
				require.ErrorIs(t, err, test.sentinel)
				diagnostic := decodeFailure(t, PreparationFailure(PreparationIsolation, err))
				require.Equal(t, "isolation", diagnostic.Stage)
				require.Equal(t, "isolation", diagnostic.Category)
				require.Equal(t, test.reason, diagnostic.Reason)
			})
		}
	}
}

func TestPreparationIsolationReasonWhitelistAndRedaction(t *testing.T) {
	for _, reason := range []string{"identity", "process-status", "capabilities", "process-security", "filesystem", "credentials",
		"cgroup-memory", "cgroup-pids", "cgroup-swap", "cgroup-cpu"} {
		err := PreparationFailure(PreparationIsolation, untrustedIsolationReason{value: reason})
		require.Equal(t, reason, decodeFailure(t, err).Reason)
		require.LessOrEqual(t, len(PreparationFailureDiagnostic(err)), MaxPreparationDiagnosticBytes)
	}
	canary := strings.Repeat("DO-NOT-EMIT-report-patch-https://private.invalid/path", 64)
	content := PreparationFailureDiagnostic(PreparationFailure(PreparationIsolation, untrustedIsolationReason{value: canary}))
	require.NotContains(t, string(content), "DO-NOT-EMIT")
	require.NotContains(t, string(content), "private.invalid")
	require.NotContains(t, string(content), `"reason"`)
	require.LessOrEqual(t, len(content), MaxPreparationDiagnosticBytes)
	// Text that looks identical to a trusted preflight message is not a type.
	lookalike := PreparationFailure(PreparationIsolation, errors.New(workerCgroupPIDs.Error()))
	require.Empty(t, decodeFailure(t, lookalike).Reason)
	for _, diagnostic := range []preparationDiagnostic{
		{Stage: "git-fetch", Category: "isolation", Reason: "cgroup-pids"},
		{Stage: "isolation", Category: "exit", Reason: "cgroup-pids", ExitCode: 128},
		{Stage: "isolation", Category: "isolation", Reason: canary},
	} {
		record := PreparationFailureDiagnostic(&preparationDiagnosticError{diagnostic: diagnostic})
		var fields map[string]any
		require.NoError(t, json.Unmarshal(record, &fields))
		require.NotContains(t, fields, "reason")
		require.NotContains(t, string(record), "DO-NOT-EMIT")
		require.LessOrEqual(t, len(record), MaxPreparationDiagnosticBytes)
	}
}
