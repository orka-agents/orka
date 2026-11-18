package main

import (
	"net/url"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"github.com/orka-agents/orka/internal/workspace"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestNativeConformanceTemplatesUsePinnedProtocol(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("native conformance template rendering requires %s", tool)
		}
	}
	script, err := filepath.Abs("../../agent-substrate-e2e.sh")
	require.NoError(t, err)
	for _, name := range []string{"orka-direct", "orka-mcp", "orka-acp-infra"} {
		t.Run(name, func(t *testing.T) {
			image := "registry.example.test/orka/conformance@sha256:" + strings.Repeat("a", 64)
			command := exec.CommandContext(t.Context(), "bash", "-c",
				`source "$1"; native_template_manifest "$2" "$3" "$4"`,
				"native-template-test", script, name, image, "public-test-key")
			data, err := command.CombinedOutput()
			require.NoError(t, err, "%s", data)
			template := &ateapipb.ActorTemplate{}
			require.NoError(t, protojson.Unmarshal(data, template), "conformance must emit strict upstream protojson")
			require.Equal(t, name, template.GetMetadata().GetName())
			require.Equal(t, image, template.GetContainers()[0].GetImage())
			// Data-only: snapshots never capture process memory and a fresh
			// Actor cold-boots instead of restoring a golden snapshot.
			require.Equal(t, ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES,
				template.GetSnapshotConfig().GetPreferredFidelity())
			require.Equal(t, ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED,
				template.GetSnapshotConfig().GetGoldenSnapshotConfig().GetMode())
			require.Positive(t, template.GetResources().GetLimits().GetCpuMillicores())
			require.Positive(t, template.GetResources().GetLimits().GetMemoryBytes())
			require.Equal(t, int32(80), template.GetContainers()[0].GetWakeupProbe().GetHttpGet().GetPort())
		})
	}
}

// Substrate v1 denies an Actor without an egress policy everything. The policy
// the proof installs must compile, and must allow exactly the request the proof
// then makes from inside the Actor.
func TestDirectConformanceEgressAllowsOnlyTheProviderProxyProbe(t *testing.T) {
	policy, err := workspace.CompileSubstrateActorEgressPolicy(conformanceAtespace, providerProxyEgress())
	require.NoError(t, err)
	require.Len(t, policy.GetRules(), 1)
	rule := policy.GetRules()[0].GetHttp()
	require.NotNil(t, rule, "the probe uses plain HTTP")
	probe, err := url.Parse(providerProxyHealthURL())
	require.NoError(t, err)
	require.Equal(t, "http", probe.Scheme)
	require.Equal(t, []string{probe.Hostname()}, rule.GetHostnames())
	port, err := strconv.ParseInt(probe.Port(), 10, 32)
	require.NoError(t, err)
	require.Equal(t, []int32{int32(port)}, rule.GetPorts().GetNumbers())
}
