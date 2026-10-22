package storagetest

import (
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

func observeArtifacts(t *testing.T, p storagev1.ArtifactProvider) any {
	t.Helper()
	owner := artifactOwner("tenant", "task")
	listed, err := p.ListArtifacts(t.Context(), owner)
	if err != nil {
		return observed(t, nil, err)
	}
	data, contentType, getErr := p.GetArtifact(t.Context(), artifactKey("tenant", "task", "report.txt"))
	return []any{listed, observed(t, []any{data, contentType}, getErr)}
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
		},
		writeCase[storagev1.ArtifactProvider]{
			method: "DeleteArtifacts",
			setup: func(t *testing.T, p storagev1.ArtifactProvider) {
				saveArtifact(t, p, artifactOwner("tenant", "task"), "report.txt", "text/plain", []byte("report"))
			},
			args:    fixed(artifactOwner("tenant", "task")),
			observe: observeArtifacts,
		},
	)
}
