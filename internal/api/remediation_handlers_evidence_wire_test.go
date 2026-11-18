package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
)

// Keep this wire regression usable on the pre-contract tree: its assertions use
// raw JSON and existing helpers rather than new evidence fields or constants.
func TestRemediationEvidenceRequirementWireContract(t *testing.T) {
	base := `{"requestID":"scope-request","mode":"generate","report":{"title":"Synthetic check","problem":"Check synthetic behavior."},"repository":"https://github.com/example/project","ref":"v1"}`
	for _, mode := range []string{"generate", "validate"} {
		for _, scope := range []string{"source-harness", "application-runtime"} {
			t.Run(mode+"/"+scope, func(t *testing.T) {
				body := strings.Replace(base, `"mode":"generate"`, `"mode":"`+mode+`"`, 1)
				body = strings.TrimSuffix(body, "}") + `,"requiredEvidenceScope":"` + scope + `"}`
				request, err := decodeRemediationRequest([]byte(body))
				require.NoError(t, err, "known requirements must reach capability admission, not be discarded or rejected as unknown fields")
				encoded, err := json.Marshal(request)
				require.NoError(t, err)
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(encoded, &fields))
				require.JSONEq(t, `"`+scope+`"`, string(fields["requiredEvidenceScope"]))
			})
		}
	}
	for _, raw := range []string{`"runtime"`, `""`, `null`, `false`, `1`, `[]`, `{}`, `" source-harness"`} {
		t.Run("invalid/"+raw, func(t *testing.T) {
			body := strings.TrimSuffix(base, "}") + `,"requiredEvidenceScope":` + raw + `}`
			_, err := decodeRemediationRequest([]byte(body))
			require.Error(t, err)
		})
	}
	for _, extra := range []string{
		`,"requiredEvidenceScope":"source-harness","requiredEvidenceScope":"application-runtime"`,
		`,"requiredEvidenceScope":"source-harness","RequiredEvidenceScope":"application-runtime"`,
		`,"evidenceScope":"application-runtime"`,
	} {
		_, err := decodeRemediationRequest([]byte(strings.TrimSuffix(base, "}") + extra + "}"))
		require.Error(t, err, "callers cannot supply actual scope or ambiguous requirements")
	}
	_, err := decodeRemediationRequest([]byte(base))
	require.NoError(t, err, "omitted requirement must preserve the existing request shape")
}

func TestRemediationEvidenceStatusWireValidation(t *testing.T) {
	base := `{"id":"scope-request","namespace":"default","phase":"Succeeded","stage":"complete","outcome":"Verified","reason":"verified","patchReady":true}`
	for _, tc := range []struct {
		name, extra string
		valid       bool
	}{
		{"legacy", "", true},
		{"source harness", `,"evidenceScope":"source-harness"`, true},
		{"runtime", `,"evidenceScope":"application-runtime"`, true},
		{"unknown", `,"evidenceScope":"future-scope"`, false},
		{"empty", `,"evidenceScope":""`, false},
		{"null", `,"evidenceScope":null`, false},
		{"boolean", `,"evidenceScope":true`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status contract.Status
			err := json.Unmarshal([]byte(strings.TrimSuffix(base, "}")+tc.extra+"}"), &status)
			valid := err == nil && validRemediationStatus(&status)
			require.Equal(t, tc.valid, valid, "unknown or malformed evidence must not become a verified runtime label")
		})
	}
}
