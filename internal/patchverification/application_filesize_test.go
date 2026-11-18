package patchverification

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplicationFileSizeCapsAreFixedByTrustedRole(t *testing.T) {
	for _, role := range []string{"prepare", "build", "initialize"} {
		limit, err := ApplicationFileSizeLimit(role)
		require.NoError(t, err)
		require.Equal(t, uint64(512<<20), limit)
	}
	for _, role := range []string{"", "execute"} {
		limit, err := ApplicationFileSizeLimit(role)
		require.NoError(t, err)
		require.Equal(t, uint64(16<<20), limit)
	}
	for _, role := range []string{"unlimited", "1024MiB", "subject:512", "../initialize"} {
		_, err := ApplicationFileSizeLimit(role)
		require.Error(t, err)
	}
	input := ApplicationBoundaryInput{Version: ApplicationProtocolVersion, ID: "file-bound", TaskUID: "probe-task",
		Target:     ApplicationTarget{ClusterUID: "target", Namespace: "run-owned", NamespaceUID: "namespace-uid"},
		CanaryHost: "127.0.0.1", CanaryPort: 8080, FileSizeRole: "execute"}
	digest, err := ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	report := ApplicationBoundaryReport{Version: ApplicationProtocolVersion, InputDigest: digest, TaskUID: input.TaskUID,
		PodUID: "probe-pod", UID: 65532, GID: 65532, PID: 1, SeccompMode: 2, NoNewPrivileges: true, ReadOnlyRoot: true,
		MemoryMax: 512 << 20, PIDsMax: 128, CPUQuota: 100000, CPUPeriod: 100000,
		Capabilities: map[string]string{"CapEff": strings.Repeat("0", 16), "CapPrm": strings.Repeat("0", 16),
			"CapBnd": strings.Repeat("0", 16), "CapInh": strings.Repeat("0", 16), "CapAmb": strings.Repeat("0", 16)}}
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "a declaration without actual readback is not a file bound")
	report.FileSizeSoft, report.FileSizeHard = 16<<20, 16<<20
	require.NoError(t, ValidateApplicationBoundaryReport(input, report))
	report.FileSizeHard = 512 << 20
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "a raiseable soft-only bound must fail")
	input.FileSizeRole = "prepare"
	report.InputDigest, err = ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	require.Error(t, ValidateApplicationBoundaryReport(input, report))
	report.FileSizeSoft = 512 << 20
	require.NoError(t, ValidateApplicationBoundaryReport(input, report))
}
