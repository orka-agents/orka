package cliwrapper

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestUploadTurnArtifactsSendsCurrentTurnCapability(t *testing.T) {
	t.Setenv(EnvChildUID, "")
	t.Setenv(EnvChildGID, "")
	bearer := strings.Repeat("artifact-fixture-", 3)
	request := validWrapperStartTurnRequest()
	upload := harness.ArtifactUpload{
		Namespace: request.Namespace, TaskName: request.TaskName, TaskUID: request.Metadata[harness.MetadataTaskUID],
		TurnID: string(request.TurnID), BindingDigest: request.Metadata[harness.MetadataBindingDigest],
		Filename: "report + résumé %.txt", ContentType: "text/plain", Data: []byte("artifact body"),
	}
	verified := make(chan error, 1)
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err == nil {
			actual := upload
			actual.Filename = path.Base(r.URL.EscapedPath())
			actual.ContentType, actual.Data = r.Header.Get("Content-Type"), body
			err = harness.VerifyArtifactUpload(bearer, artifactcap.Authorization{
				Capability:    r.Header.Get(artifactcap.CapabilityHeader),
				RequestDigest: r.Header.Get(artifactcap.RequestDigestHeader),
			}, actual, time.Now().UTC())
		}
		verified <- err
		if err != nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(controller.Close)
	t.Setenv(workerenv.ControllerURL, controller.URL)
	artifactDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(artifactDir, upload.Filename), upload.Data, 0o600))
	server := &Server{config: Config{AuthValue: bearer}}
	require.NoError(t, server.uploadTurnArtifacts(TurnContext{
		Namespace: request.Namespace, TaskName: request.TaskName, TurnID: string(request.TurnID), Metadata: request.Metadata,
	}, artifactDir))
	select {
	case err := <-verified:
		require.NoError(t, err)
	default:
		t.Fatal("wrapper did not send its artifact to the controller")
	}
}
