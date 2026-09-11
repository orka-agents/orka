package harness

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
)

// ArtifactUpload binds the legacy named-artifact endpoint to one immutable
// harness v1 Task/turn and the exact uploaded file. It contains no credentials.
type ArtifactUpload struct {
	Namespace     string
	TaskName      string
	TaskUID       string
	TurnID        string
	BindingDigest string
	Filename      string
	ContentType   string
	Data          []byte
}

func (u ArtifactUpload) operation() (artifactcap.OperationRequest, error) {
	if strings.TrimSpace(u.TaskName) == "" || strings.TrimSpace(u.TurnID) == "" ||
		strings.TrimSpace(u.Filename) == "" || !artifactcap.IsRequestDigest(u.BindingDigest) {
		return artifactcap.OperationRequest{}, artifactcap.ErrInvalidRequest
	}
	identity, err := json.Marshal([]string{u.TaskName, u.TurnID, u.BindingDigest, u.Filename})
	if err != nil {
		return artifactcap.OperationRequest{}, artifactcap.ErrInvalidRequest
	}
	return artifactcap.OperationRequest{
		Operation: artifactcap.OperationUpload, ObjectDigest: artifactcap.DigestBytes(u.Data),
		Identity:      artifactcap.Identity{Namespace: u.Namespace, TaskID: u.TaskUID},
		ContentLength: int64(len(u.Data)), MediaType: u.ContentType,
		OperationID: "harness-v1-upload-" + artifactcap.DigestBytes(identity),
	}, nil
}

// Derive a separate key so a v1 named-artifact capability cannot authorize an
// ACP v2 object operation, even if an operator reused the underlying Secret.
func artifactUploadKey(bearer string) ([]byte, error) {
	if len(bearer) < artifactcap.MinSecretBytes {
		return nil, artifactcap.ErrUnauthorized
	}
	mac := hmac.New(sha256.New, []byte(bearer))
	_, _ = mac.Write([]byte("orka.harness.v1/artifact-upload"))
	return mac.Sum(nil), nil
}

func SignArtifactUpload(bearer string, upload ArtifactUpload, now time.Time) (artifactcap.Authorization, error) {
	key, err := artifactUploadKey(bearer)
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	operation, err := upload.operation()
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	return artifactcap.Issue(key, operation, now, 2*time.Minute)
}

func VerifyArtifactUpload(bearer string, authorization artifactcap.Authorization, upload ArtifactUpload, now time.Time) error {
	key, err := artifactUploadKey(bearer)
	if err != nil {
		return err
	}
	operation, err := upload.operation()
	if err != nil {
		return err
	}
	claims, err := artifactcap.Verify(key, authorization.Capability, artifactcap.PresentedRequest{
		Method: operation.Method(), Path: operation.Path(), ObjectDigest: operation.ObjectDigest,
		ContentLength: operation.ContentLength, MediaType: operation.MediaType, RequestDigest: authorization.RequestDigest,
	}, now)
	if err != nil {
		return err
	}
	// The canonical operation also includes the Task/turn/filename identity,
	// which is derived from the controller's current durable attempt.
	if claims.Request != operation {
		return artifactcap.ErrUnauthorized
	}
	return nil
}
