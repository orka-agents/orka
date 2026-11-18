//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func TestEachPreparationPreflightPreservesItsTypedReason(t *testing.T) {
	for _, test := range []struct {
		sentinel preparationIsolationFailure
		reason   string
	}{
		{preparationIsolationIdentity, "identity"},
		{preparationIsolationStatus, "process-status"},
		{preparationIsolationCapabilities, "capabilities"},
		{preparationIsolationProcessSecurity, "process-security"},
		{preparationIsolationFilesystem, "filesystem"},
		{preparationIsolationCredentials, "credentials"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			preflight := unavailablePreparationPolicy(test.sentinel)
			require.ErrorIs(t, preflight, errValidationPreparation)
			require.ErrorIs(t, preflight, test.sentinel)
			err := pv.PreparationFailure(pv.PreparationIsolation, preflight)
			var stderr, termination bytes.Buffer
			returned := finishValidationPreparation(err, &stderr, func(record []byte) error {
				_, err := termination.Write(record)
				return err
			})
			require.Equal(t, errValidationPreparation, returned)
			require.Equal(t, 1, generalWorkerExitCode(returned))
			require.Equal(t, stderr.Bytes(), termination.Bytes())
			require.LessOrEqual(t, stderr.Len(), pv.MaxPreparationDiagnosticBytes)
			var diagnostic map[string]any
			require.NoError(t, json.Unmarshal(stderr.Bytes(), &diagnostic))
			require.Equal(t, "isolation", diagnostic["stage"])
			require.Equal(t, "isolation", diagnostic["category"])
			require.Equal(t, test.reason, diagnostic["reason"])
		})
	}
	unknownPolicy := unavailablePreparationPolicy(preparationIsolationFailure(255))
	unknown := pv.PreparationFailure(pv.PreparationIsolation, unknownPolicy)
	var diagnostic map[string]any
	require.NoError(t, json.Unmarshal(pv.PreparationFailureDiagnostic(unknown), &diagnostic))
	require.NotContains(t, diagnostic, "reason")
}
