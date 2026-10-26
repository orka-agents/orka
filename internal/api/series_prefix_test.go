package api

import (
	"encoding/json"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
)

func TestSeriesPrefixRequestPreservesProtection(t *testing.T) {
	original := &corev1alpha1.EnforcementRequirement{
		Format: "example.policy/v1", Policy: []byte("complete policy\n"),
		RequiredCapabilities: []string{"network/egress-allow/v1"}, Mode: "enforce",
	}
	for _, manifest := range []bool{false, true} {
		t.Run(map[bool]string{false: "flat", true: "manifest"}[manifest], func(t *testing.T) {
			body := map[string]any{"type": "ai", "enforcement": original}
			if manifest {
				body = map[string]any{"spec": map[string]any{"type": "ai", "enforcement": original}}
			}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			var request CreateTaskRequest
			require.NoError(t, json.Unmarshal(raw, &request))
			spec := corev1alpha1.TaskSpec{}
			if request.Spec != nil {
				spec = *request.Spec.DeepCopy()
			}
			applyFlatTaskRequest(&spec, request)
			require.Equal(t, original, spec.Enforcement)
			if !manifest {
				request.Enforcement.Policy[0] = 'x'
				require.Equal(t, original.Policy, spec.Enforcement.Policy)
			}
		})
	}
}
