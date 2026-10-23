package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// laterArtifactOwner is the artifact owner of a later Task created with the
// same name as artifactOwner("tenant", "task").
var laterArtifactOwner = storagev1.ArtifactOwner{Namespace: "tenant", TaskName: "task", TaskUID: "task-later-uid"}

func observeArtifacts(t *testing.T, p storagev1.ArtifactProvider) any {
	t.Helper()
	seen := make([]any, 0, 4)
	for _, owner := range []storagev1.ArtifactOwner{artifactOwner("tenant", "task"), laterArtifactOwner} {
		listed, err := p.ListArtifacts(t.Context(), owner)
		require.NoError(t, err)
		data, contentType, getErr := p.GetArtifact(t.Context(), storagev1.ArtifactKey{Owner: owner, Filename: "report.txt"})
		seen = append(seen, listed, observed(t, []any{data, contentType}, getErr))
	}
	return seen
}

func init() {
	artifactWriteCases = append(artifactWriteCases,
		writeCase[storagev1.ArtifactProvider]{
			method: "SaveArtifact",
			setup: func(t *testing.T, p storagev1.ArtifactProvider) {
				saveArtifact(t, p, artifactOwner("tenant", "task"), "report.txt", "text/plain", []byte("old"))
			},
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{storagev1.SaveArtifactRequest{
					Owner: artifactOwner("tenant", "task"), Filename: "report.txt", ContentType: "text/plain", Data: []byte("new report"),
				}}
			},
			observe: observeArtifacts,
			afterCommit: func(t *testing.T, p storagev1.ArtifactProvider) {
				// Issued after the case's write, so it replaces the file, and
				// a replay of the older write must not restore it.
				request := storagev1.SaveArtifactRequest{
					Owner: artifactOwner("tenant", "task"), Filename: "report.txt", ContentType: "text/plain", Data: []byte("newer report"),
				}
				require.NoError(t, p.SaveArtifact(EnvelopeContext(t.Context(), t, p, suiteFence, "SaveArtifact", request), request))
			},
		},
		writeCase[storagev1.ArtifactProvider]{
			method: "DeleteArtifacts",
			setup: func(t *testing.T, p storagev1.ArtifactProvider) {
				saveArtifact(t, p, artifactOwner("tenant", "task"), "report.txt", "text/plain", []byte("report"))
			},
			args:    fixed(artifactOwner("tenant", "task")),
			observe: observeArtifacts,
			afterCommit: func(t *testing.T, p storagev1.ArtifactProvider) {
				// The deleted Task's UID is refused from now on, so a later
				// Task with the same name writes again.
				saveArtifact(t, p, laterArtifactOwner, "report.txt", "text/plain", []byte("written again"))
			},
		},
	)
}
