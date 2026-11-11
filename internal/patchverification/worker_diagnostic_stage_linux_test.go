//go:build linux

package patchverification

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEveryPreparationStageHasFixedDiagnosticEncoding(t *testing.T) {
	for _, stage := range []PreparationStage{
		PreparationInputOpen, PreparationInputDecode, PreparationIdentity, PreparationIsolation, PreparationWorkspace,
		PreparationGitInit, PreparationGitFetch, PreparationGitImport, PreparationOriginalArchive,
		PreparationPatchValidate, PreparationPatchApply, PreparationPatchedArchive, PreparationDiff,
		PreparationReportValidate, PreparationReportEncode, PreparationReportWrite,
	} {
		got := decodeFailure(t, PreparationFailure(stage, context.DeadlineExceeded))
		require.Equal(t, string(stage), got.Stage)
		require.Equal(t, "timeout", got.Category)
	}
	got := decodeFailure(t, PreparationFailure(PreparationStage("https://private.invalid/DO-NOT-EMIT"), ErrEvidence))
	require.Equal(t, preparationInternal, got.Stage)
	require.Equal(t, "invalid", got.Category)
	require.Nil(t, PreparationFailure(PreparationGitFetch, nil))
}
