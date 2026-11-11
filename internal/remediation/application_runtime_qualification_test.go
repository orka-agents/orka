package remediation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func testRuntimeQualification() pv.ApplicationRuntimeQualification {
	return pv.ApplicationRuntimeQualification{
		Version: 1, Profile: "gvisor-bounded-v1",
		QualificationDigest: pv.Digest([]byte("synthetic operator qualification")),
		TargetClusterUID:    "target-cluster-uid", NodeName: "qualified-node", NodeUID: "node-uid",
		RuntimeClassName: "isolated-runtime", RuntimeClassUID: "runtime-class-uid", RuntimeHandler: "synthetic-gvisor-handler",
		RuntimeBinaryDigest:   pv.Digest([]byte("synthetic runtime binary")),
		RuntimeBaseSpecDigest: pv.Digest([]byte("synthetic runtime base specification")),
		RuntimeOptionsDigest:  pv.Digest([]byte("synthetic runtime options")),
		NativeSwapMax:         0, NativeSwapSource: "qualified-native-ancestor",
		AggregatePVCBytes: 6 << 30, NodeMemoryBytes: 16 << 30, AggregateLogBytes: 512 << 20,
		StorageQuotaMode:  "aggregate-only-no-per-pvc-guarantee",
		StorageDurability: "ephemeral-node-loss-fail-closed",
	}
}

func TestRuntimeQualificationIsStrictOperatorMetadata(t *testing.T) {
	expected := testRuntimeQualification()
	actual, err := DecodeApplicationRuntimeQualification(encodeStartupPolicy(t, expected))
	require.NoError(t, err)
	require.Equal(t, &expected, actual)
	for name, mutate := range map[string]func(map[string]any){
		"missing swap":            func(data map[string]any) { delete(data, "nativeSwapMax") },
		"null swap":               func(data map[string]any) { data["nativeSwapMax"] = nil },
		"swap enabled":            func(data map[string]any) { data["nativeSwapMax"] = 1 },
		"fake guest swap":         func(data map[string]any) { data["nativeSwapSource"] = "guest-measured" },
		"runtime name waiver":     func(data map[string]any) { data["profile"] = "runsc" },
		"missing identity":        func(data map[string]any) { delete(data, "nodeUID") },
		"per-volume claim":        func(data map[string]any) { data["storageQuotaMode"] = "per-volume-hard" },
		"unknown skip flag":       func(data map[string]any) { data["skipIsolation"] = true },
		"missing runtime base":    func(data map[string]any) { delete(data, "runtimeBaseSpecDigest") },
		"missing runtime options": func(data map[string]any) { delete(data, "runtimeOptionsDigest") },
		"mislabelled node disk": func(data map[string]any) {
			data["nodeStorageBytes"] = data["nodeMemoryBytes"]
			delete(data, "nodeMemoryBytes")
		},
		"ambiguous runtime configuration": func(data map[string]any) {
			data["runtimeConfigDigest"] = data["runtimeBaseSpecDigest"]
			delete(data, "runtimeBaseSpecDigest")
			delete(data, "runtimeOptionsDigest")
		},
	} {
		t.Run(name, func(t *testing.T) {
			var data map[string]any
			require.NoError(t, json.Unmarshal(encodeStartupPolicy(t, expected), &data))
			mutate(data)
			_, err := DecodeApplicationRuntimeQualification(encodeStartupPolicy(t, data))
			require.Error(t, err)
		})
	}
}
