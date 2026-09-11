package supervisor

import (
	"context"
	"fmt"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type remoteArtifactUploader struct {
	client *ArtifactClient
}

func NewRemoteArtifactUploader(client *ArtifactClient) (ArtifactUploader, error) {
	if client == nil {
		return nil, fmt.Errorf("artifact client is required")
	}
	return &remoteArtifactUploader{client: client}, nil
}

func (u *remoteArtifactUploader) UploadWorkspaceDelta(
	ctx context.Context,
	request harnessv2.CreateWorkspaceDeltaRequest,
	artifact []byte,
	digest string,
) (harnessv2.ArtifactReference, error) {
	if err := request.Limits.Validate(); err != nil {
		return harnessv2.ArtifactReference{}, fmt.Errorf("workspace delta limits: %w", err)
	}
	if int64(len(artifact)) > request.Limits.MaxBytes {
		return harnessv2.ArtifactReference{}, fmt.Errorf("workspace delta artifact exceeds request limit")
	}
	if actual := artifactcap.DigestBytes(artifact); actual != digest {
		return harnessv2.ArtifactReference{}, fmt.Errorf("workspace delta artifact digest mismatch")
	}
	artifactID, err := artifactcap.ArtifactIDForDigest(digest)
	if err != nil {
		return harnessv2.ArtifactReference{}, err
	}
	reference := harnessv2.ArtifactReference{
		ArtifactID: harnessv2.ArtifactID(artifactID),
		Digest:     digest,
		SizeBytes:  int64(len(artifact)),
		MediaType:  artifactcap.MediaTypeWorkspaceDelta,
	}
	if request.ArtifactUploadAuthorization == nil {
		// The controller cannot bind an upload capability until the frozen
		// workspace delta has been produced and its exact digest and size are
		// known. Ask the authenticated controller-side broker for that one-shot
		// capability only after constructing the content-addressed reference.
		return u.client.Upload(ctx, reference, artifact, &request)
	}
	authorization := artifactcap.Authorization{Capability: request.ArtifactUploadAuthorization.Capability, RequestDigest: request.ArtifactUploadAuthorization.RequestDigest}
	return u.client.UploadAuthorized(ctx, reference, authorization, artifact)
}
