package contract

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireEvidenceScope(t *testing.T) {
	for _, tc := range []struct {
		name                string
		required, supported EvidenceScope
		err                 error
	}{
		{"default source harness", "", SourceHarness, nil},
		{"explicit source harness", SourceHarness, SourceHarness, nil},
		{"strict runtime unsupported", ApplicationRuntime, SourceHarness, ErrEvidenceScopeUnsupported},
		{"runtime capability", ApplicationRuntime, ApplicationRuntime, nil},
		{"no implicit scope substitution", SourceHarness, ApplicationRuntime, ErrEvidenceScopeUnsupported},
		{"unknown requirement", "future-scope", SourceHarness, ErrInvalid},
		{"malformed requirement", " source-harness", SourceHarness, ErrInvalid},
		{"unrecorded capability", "", "", ErrEvidenceScopeUnsupported},
		{"unknown capability", "", "future-scope", ErrEvidenceScopeUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorIs(t, RequireEvidenceScope(tc.required, tc.supported), tc.err)
		})
	}
}

func TestEvidenceScopeJSONIsOptionalButExplicitValuesAreStrict(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"omitted", `{}`, true},
		{"source harness", `{"evidenceScope":"source-harness"}`, true},
		{"application runtime", `{"evidenceScope":"application-runtime"}`, true},
		{"unknown", `{"evidenceScope":"runtime"}`, false},
		{"empty", `{"evidenceScope":""}`, false},
		{"null", `{"evidenceScope":null}`, false},
		{"number", `{"evidenceScope":1}`, false},
		{"boolean", `{"evidenceScope":true}`, false},
		{"array", `{"evidenceScope":["source-harness"]}`, false},
		{"object", `{"evidenceScope":{"scope":"source-harness"}}`, false},
		{"whitespace", `{"evidenceScope":"source-harness "}`, false},
		{"case", `{"evidenceScope":"Source-Harness"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status Status
			err := json.Unmarshal([]byte(tc.raw), &status)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
