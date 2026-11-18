package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func observeWatermark[P storagev1.WriteWatermarkStore](t *testing.T, p P) any {
	t.Helper()
	watermark, err := p.GetWriteWatermark(t.Context())
	require.NoError(t, err)
	return watermark
}

func init() {
	stateWriteCases = append(stateWriteCases, writeCase[storagev1.StateProvider]{
		method:  "AdvanceWriteWatermark",
		args:    fixed(),
		observe: observeWatermark[storagev1.StateProvider],
	})
	artifactWriteCases = append(artifactWriteCases, writeCase[storagev1.ArtifactProvider]{
		method:  "AdvanceWriteWatermark",
		args:    fixed(),
		observe: observeWatermark[storagev1.ArtifactProvider],
	})
}
