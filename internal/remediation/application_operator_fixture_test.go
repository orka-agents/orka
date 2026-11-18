package remediation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestApplicationProposalCannotDefineWorkloadOrStartupCredentials(t *testing.T) {
	for _, field := range []string{
		"podSpec", "podTemplate", "subjectTLS", "subjectTLSBundles", "tlsBundles", "secretVolumes", "hostVolumes", "runtimeQualification",
	} {
		t.Run(field, func(t *testing.T) {
			var data map[string]any
			require.NoError(t, json.Unmarshal(encodeStartupPolicy(t, applicationProposal(t)), &data))
			data["application"].(map[string]any)[field] = map[string]any{"id": "startup"}
			_, err := DecodeApplicationCheckProposal(string(encodeStartupPolicy(t, data)), nil)
			require.Error(t, err, "operator workload/TLS configuration is not a model-owned field")
		})
	}
}

func TestModelFixtureCannotOverrideOperatorTemplate(t *testing.T) {
	for _, field := range []string{
		"podSpec", "template", "image", "command", "args", "env", "envFrom", "volumes",
		"serviceAccountName", "securityContext", "hostNetwork", "nodeSelector", "secretFiles", "replicaTarget",
	} {
		t.Run(field, func(t *testing.T) {
			var data map[string]any
			require.NoError(t, json.Unmarshal(encodeStartupPolicy(t, applicationProposal(t)), &data))
			data["application"].(map[string]any)["fixtures"] = []any{map[string]any{
				"id": "replica-target", "kind": "replica", "profile": "approved-target",
				field: map[string]any{"unapproved": true},
			}}
			_, err := DecodeApplicationCheckProposal(string(encodeStartupPolicy(t, data)), nil)
			require.Error(t, err)
		})
	}
}
