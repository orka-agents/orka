package patchverification

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func applicationTestRuntimeQualification() ApplicationRuntimeQualification {
	return ApplicationRuntimeQualification{
		Version: ApplicationProtocolVersion, Profile: ApplicationRuntimeGVisorBoundedV1,
		QualificationDigest: Digest([]byte("operator-runtime-evidence")), TargetClusterUID: "target-cluster",
		NodeName: "qualified-node", NodeUID: "qualified-node-uid",
		RuntimeClassName: "qualified-runtime", RuntimeClassUID: "qualified-runtime-uid", RuntimeHandler: "qualified-handler",
		RuntimeBinaryDigest: Digest([]byte("installed-runtime")), RuntimeBaseSpecDigest: Digest([]byte("installed-base-spec")),
		RuntimeOptionsDigest: Digest([]byte("installed-runtime-options")),
		NativeSwapMax:        0, NativeSwapSource: "qualified-native-ancestor",
		AggregatePVCBytes: 6 << 30, NodeMemoryBytes: 16 << 30, AggregateLogBytes: 512 << 20,
		StorageQuotaMode: "aggregate-only-no-per-pvc-guarantee", StorageDurability: "ephemeral-node-loss-fail-closed",
	}
}

func TestApplicationRuntimeQualificationRequiresBoundIdentitiesAndTruthfulOrigins(t *testing.T) {
	require.NoError(t, ValidateApplicationRuntimeQualification(applicationTestRuntimeQualification()))
	for _, mutate := range []func(*ApplicationRuntimeQualification){
		func(q *ApplicationRuntimeQualification) { q.Profile = "gvisor" },
		func(q *ApplicationRuntimeQualification) { q.NodeUID = "" },
		func(q *ApplicationRuntimeQualification) { q.RuntimeClassUID = "" },
		func(q *ApplicationRuntimeQualification) { q.RuntimeBinaryDigest = "" },
		func(q *ApplicationRuntimeQualification) { q.QualificationDigest = "" },
		func(q *ApplicationRuntimeQualification) { q.NativeSwapSource = "guest-display-zero" },
		func(q *ApplicationRuntimeQualification) { q.NativeSwapMax = 1 },
		func(q *ApplicationRuntimeQualification) { q.StorageQuotaMode = "per-pvc-hard" },
		func(q *ApplicationRuntimeQualification) { q.StorageDurability = "durable-across-node-loss" },
	} {
		value := applicationTestRuntimeQualification()
		mutate(&value)
		require.Error(t, ValidateApplicationRuntimeQualification(value))
	}
}

func TestQualifiedBoundaryPreservesExactLowerRoleBudgets(t *testing.T) {
	qualification := applicationTestRuntimeQualification()
	limits := ApplicationRuntimeLimits{MemoryMax: 256 << 20, CPUQuota: 25000, CPUPeriod: 100000, PIDsMax: 512, FileSizeMax: 16 << 20}
	input := ApplicationBoundaryInput{Version: ApplicationProtocolVersion, ID: "boundary", TaskUID: "boundary-task",
		Target:     ApplicationTarget{ClusterUID: "target-cluster", Namespace: "run-original", NamespaceUID: "namespace-uid"},
		CanaryHost: "127.0.0.1", CanaryPort: 8080, RuntimeQualification: &qualification, RuntimeLimits: &limits}
	inputDigest, err := ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	qualificationDigest, err := ApplicationRuntimeQualificationDigest(qualification)
	require.NoError(t, err)
	report := ApplicationBoundaryReport{Version: ApplicationProtocolVersion, InputDigest: inputDigest,
		TaskUID: input.TaskUID, PodUID: "probe-pod", UID: 65532, GID: 65532, PID: 1,
		NoNewPrivileges: true, SeccompMode: 2, ReadOnlyRoot: true, MemoryMax: limits.MemoryMax,
		CPUQuota: limits.CPUQuota, CPUPeriod: limits.CPUPeriod, PIDsMax: limits.PIDsMax,
		Capabilities: map[string]string{"CapEff": strings.Repeat("0", 16), "CapPrm": strings.Repeat("0", 16),
			"CapBnd": strings.Repeat("0", 16), "CapInh": strings.Repeat("0", 16), "CapAmb": strings.Repeat("0", 16)},
		RuntimeProfile: qualification.Profile, RuntimeQualificationDigest: qualificationDigest,
		PIDLimitSource: "guest-rlimit-nproc", SwapLimitSource: "qualified-native-ancestor", GuestPIDSoft: 512, GuestPIDHard: 512,
		FileSizeSoft: 16 << 20, FileSizeHard: 16 << 20,
	}
	require.NoError(t, ValidateApplicationBoundaryReport(input, report))
	report.MemoryMax = 2 << 30
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "a qualified maximum must not enlarge the role's budget")
	report.MemoryMax, report.GuestPIDHard = limits.MemoryMax, 513
	require.Error(t, ValidateApplicationBoundaryReport(input, report))
	report.GuestPIDHard, report.SeccompMode = 512, 0
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "qualified runtime is not a seccomp waiver")
	report.SeccompMode = 2
	input.RuntimeQualification, input.RuntimeLimits = nil, nil
	report.InputDigest, err = ApplicationBoundaryInputDigest(input)
	require.NoError(t, err)
	require.Error(t, ValidateApplicationBoundaryReport(input, report), "native mode cannot silently adopt qualified evidence")
}

func TestRuntimeConfigStrictlyDecodesExactRoleLimits(t *testing.T) {
	config := ApplicationRuntimeConfig{Version: ApplicationProtocolVersion, Qualification: applicationTestRuntimeQualification(),
		Limits: ApplicationRuntimeLimits{MemoryMax: 512 << 20, CPUQuota: 100000, CPUPeriod: 100000, PIDsMax: 512, FileSizeMax: 16 << 20}}
	content, err := json.Marshal(config)
	require.NoError(t, err)
	_, err = DecodeApplicationRuntimeConfig(strings.NewReader(string(content)))
	require.NoError(t, err)
	config.Limits.MemoryMax = 3 << 30
	content, err = json.Marshal(config)
	require.NoError(t, err)
	_, err = DecodeApplicationRuntimeConfig(strings.NewReader(string(content)))
	require.Error(t, err)
}
