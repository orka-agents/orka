package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
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
			require.Equal(t, ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA,
				template.GetSnapshotsConfig().GetOnPause())
			require.Equal(t, ateapipb.ResumeSource_RESUME_SOURCE_COLD_BOOT,
				template.GetSnapshotsConfig().GetOnResume().GetFromData())
		})
	}
}
