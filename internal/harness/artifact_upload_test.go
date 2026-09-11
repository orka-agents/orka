package harness

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/artifactcap"
)

func TestArtifactUploadCapabilityScope(t *testing.T) {
	bearer := strings.Repeat("fixture-auth-", 3)
	now := time.Now().UTC()
	upload := ArtifactUpload{
		Namespace: "tenant", TaskName: "task", TaskUID: "task-uid", TurnID: "turn-1",
		BindingDigest: artifactcap.DigestBytes([]byte("binding")), Filename: "output.txt",
		ContentType: "text/plain", Data: []byte("output"),
	}
	authorization, err := SignArtifactUpload(bearer, upload, now)
	require.NoError(t, err)
	require.NoError(t, VerifyArtifactUpload(bearer, authorization, upload, now))

	changes := map[string]func(*ArtifactUpload){
		"namespace":        func(u *ArtifactUpload) { u.Namespace = "other" },
		"name":             func(u *ArtifactUpload) { u.TaskName = "other" },
		"Task incarnation": func(u *ArtifactUpload) { u.TaskUID = "replacement-uid" },
		"turn":             func(u *ArtifactUpload) { u.TurnID = "turn-2" },
		"binding":          func(u *ArtifactUpload) { u.BindingDigest = artifactcap.DigestBytes([]byte("other")) },
		"filename":         func(u *ArtifactUpload) { u.Filename = "other.txt" },
		"content type":     func(u *ArtifactUpload) { u.ContentType = "application/json" },
		"content":          func(u *ArtifactUpload) { u.Data = []byte("forged") },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := upload
			change(&changed)
			require.Error(t, VerifyArtifactUpload(bearer, authorization, changed, now))
		})
	}
	require.Error(t, VerifyArtifactUpload(strings.Repeat("other-auth-", 3), authorization, upload, now))
	require.ErrorIs(t, VerifyArtifactUpload(bearer, authorization, upload, now.Add(3*time.Minute)), artifactcap.ErrExpired)
	operation, err := upload.operation()
	require.NoError(t, err)
	_, err = artifactcap.Verify([]byte(bearer), authorization.Capability, artifactcap.PresentedRequest{
		Method: operation.Method(), Path: operation.Path(), ObjectDigest: operation.ObjectDigest,
		ContentLength: operation.ContentLength, MediaType: operation.MediaType, RequestDigest: authorization.RequestDigest,
	}, now)
	require.ErrorIs(t, err, artifactcap.ErrUnauthorized, "v1 capabilities must not authorize ACP v2 uploads")
}
