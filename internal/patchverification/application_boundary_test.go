package patchverification

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplicationSourceBoundaryRequiresFrozenPublicTLSProof(t *testing.T) {
	input := ApplicationBoundaryInput{
		Version: ApplicationProtocolVersion, ID: "source-probe", TaskUID: "probe-task",
		Target:     ApplicationTarget{ClusterUID: "target-cluster", Namespace: "source-probe", NamespaceUID: "namespace-uid"},
		CanaryHost: "127.0.0.1", CanaryPort: 443, PositiveControl: true,
	}
	legacy, err := ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	encoded, err := json.Marshal(input)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "sourceHost")
	input.SourceHost = "github.com"
	digest, err := ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	require.NotEqual(t, legacy, digest)
	report := ApplicationBoundaryReport{
		Version: ApplicationProtocolVersion, InputDigest: digest, TaskUID: input.TaskUID, PodUID: "probe-pod",
		UID: 65532, GID: 65532, PID: 1, SeccompMode: 2, NoNewPrivileges: true, ReadOnlyRoot: true,
		Capabilities: map[string]string{
			"CapEff": strings.Repeat("0", 16), "CapPrm": strings.Repeat("0", 16), "CapBnd": strings.Repeat("0", 16),
			"CapInh": strings.Repeat("0", 16), "CapAmb": strings.Repeat("0", 16),
		},
		MemoryMax: 512 << 20, PIDsMax: 128, CPUQuota: 100000, CPUPeriod: 100000, CanaryReachable: true,
		FileSizeSoft: 16 << 20, FileSizeHard: 16 << 20,
	}
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "a TCP handshake alone is not public source HTTPS proof")
	report.SourceHTTPSVerified, report.SourceHTTPStatus = true, 200
	require.NoError(t, ValidateApplicationBoundaryReport(input, report))
	report.SourceHTTPStatus = 403
	require.Error(t, ValidateApplicationBoundaryReport(input, report))
	input.PositiveControl = false
	report.InputDigest, err = ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	report.CanaryReachable = false
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "a negative subject probe cannot claim HTTPS exchange")
	report.SourceHTTPSVerified, report.SourceHTTPStatus = false, 0
	require.NoError(t, ValidateApplicationBoundaryReport(input, report))
	for _, host := range []string{"github.com.attacker.invalid", "https://github.com", "github.com:443", "127.0.0.1"} {
		input.SourceHost = host
		_, err := ApplicationBoundaryInputDigest(input)
		require.Error(t, err)
	}
	input.SourceHost, input.CanaryPort = "api.github.com", 8443
	_, err = ApplicationBoundaryInputDigest(input)
	require.Error(t, err)
}

func TestApplicationBoundaryRequiresMeasurementsAndAuthenticatedDenials(t *testing.T) {
	input := ApplicationBoundaryInput{
		Version: ApplicationProtocolVersion, ID: "boundary", TaskUID: "probe-task",
		Target:     ApplicationTarget{ClusterUID: "target-cluster", Namespace: "run-original", NamespaceUID: "namespace-uid"},
		CanaryHost: "10.0.0.9", CanaryPort: 8080, APIAddress: "https://10.0.0.1:6443", ServiceAccountName: "subject",
	}
	digest, err := ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	makeReport := func() ApplicationBoundaryReport {
		return ApplicationBoundaryReport{
			Version: ApplicationProtocolVersion, InputDigest: digest, TaskUID: input.TaskUID, PodUID: "probe-pod",
			UID: 65532, GID: 65532, PID: 1, SeccompMode: 2, NoNewPrivileges: true, ReadOnlyRoot: true,
			Capabilities: map[string]string{
				"CapEff": strings.Repeat("0", 16), "CapPrm": strings.Repeat("0", 16), "CapBnd": strings.Repeat("0", 16),
				"CapInh": strings.Repeat("0", 16), "CapAmb": strings.Repeat("0", 16),
			},
			MemoryMax: 512 << 20, PIDsMax: 128, CPUQuota: 100000, CPUPeriod: 100000,
			FileSizeSoft: 16 << 20, FileSizeHard: 16 << 20,
			APIUserUID: "subject-sa-uid", APIStatuses: map[string]int{
				"self-identity": 201, "foreign-namespace": 403, "secrets": 403, "token-request": 403, "cluster-list": 403,
			},
		}
	}
	require.NoError(t, ValidateApplicationBoundaryReport(input, makeReport()))
	tests := map[string]func(*ApplicationBoundaryReport){
		"wrong input":             func(r *ApplicationBoundaryReport) { r.InputDigest = Digest(nil) },
		"root":                    func(r *ApplicationBoundaryReport) { r.UID = 0 },
		"missing PID namespace":   func(r *ApplicationBoundaryReport) { r.PID = 57 },
		"capability retained":     func(r *ApplicationBoundaryReport) { r.Capabilities["CapEff"] = "0000000000000001" },
		"unbounded memory":        func(r *ApplicationBoundaryReport) { r.MemoryMax = 0 },
		"unbounded pids":          func(r *ApplicationBoundaryReport) { r.PIDsMax = 0 },
		"unbounded CPU":           func(r *ApplicationBoundaryReport) { r.CPUQuota = 0 },
		"swap allowed":            func(r *ApplicationBoundaryReport) { r.SwapMax = 1 },
		"egress escaped":          func(r *ApplicationBoundaryReport) { r.CanaryReachable = true },
		"all-forbidden spoof":     func(r *ApplicationBoundaryReport) { r.APIStatuses["self-identity"] = 403 },
		"unauthenticated":         func(r *ApplicationBoundaryReport) { r.APIStatuses["secrets"] = 401 },
		"wrong identity":          func(r *ApplicationBoundaryReport) { r.APIUserUID = "" },
		"token authority":         func(r *ApplicationBoundaryReport) { r.APIStatuses["token-request"] = 201 },
		"missing forbidden probe": func(r *ApplicationBoundaryReport) { delete(r.APIStatuses, "foreign-namespace") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			report := makeReport()
			mutate(&report)
			require.Error(t, ValidateApplicationBoundaryReport(input, report))
		})
	}
	input.PositiveControl = true
	digest, err = ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	report := makeReport()
	report.InputDigest, report.CanaryReachable = digest, true
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "positive API control is required")
	report.APIStatuses["self-pod"] = 200
	require.NoError(t, ValidateApplicationBoundaryReport(input, report))
	report.CanaryReachable = false
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "dead canary cannot qualify isolation")
}
