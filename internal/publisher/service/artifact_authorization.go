package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	// ArtifactAuthorizationBrokerPath is the controller endpoint that exchanges
	// one authenticated Publisher operation for one exact artifact capability.
	ArtifactAuthorizationBrokerPath = "/internal/v2/acp/publisher-artifact-authorizations"
	maxArtifactBrokerResponseBytes  = int64(32 << 10)
	maxArtifactRemoteAttempts       = 20
)

// ArtifactAuthorizationRequest binds one artifact transfer to the exact
// Publisher operation that is currently executing. Raw capability-signing
// material is never delivered to the Publisher when broker mode is enabled.
type ArtifactAuthorizationRequest struct {
	ParentOperation   Operation                   `json:"parentOperation"`
	Metadata          OperationMetadata           `json:"metadata"`
	ArtifactOperation artifactcap.Operation       `json:"artifactOperation"`
	Artifact          harnessv2.ArtifactReference `json:"artifact"`
	Attempt           int                         `json:"attempt"`
}

// Validate validates the immutable transfer binding independently of any
// controller-side Task or Publication state.
func (r ArtifactAuthorizationRequest) Validate() error {
	if r.ParentOperation.Path() == "" || r.Attempt < 1 || r.Attempt > maxArtifactRemoteAttempts {
		return fmt.Errorf("artifact authorization request is invalid")
	}
	if err := r.Metadata.validateFor(r.ParentOperation); err != nil {
		return err
	}
	if err := validateArtifactTransportReference(r.Artifact); err != nil {
		return err
	}
	switch r.ArtifactOperation {
	case artifactcap.OperationUpload:
		workspaceUpload := r.ParentOperation == OperationWorkspacePrepare && r.Artifact.MediaType == artifactcap.MediaTypeWorkspaceTar
		bundleUpload := r.ParentOperation == OperationPublicationPrepare && r.Artifact.MediaType == artifactcap.MediaTypeGitBundle
		if !workspaceUpload && !bundleUpload {
			return fmt.Errorf("artifact upload is not valid for parent operation")
		}
	case artifactcap.OperationDownload:
		deltaDownload := r.ParentOperation == OperationPublicationPrepare && r.Artifact.MediaType == artifactcap.MediaTypeWorkspaceDelta
		bundleDownload := (r.ParentOperation == OperationPublicationPublish || r.ParentOperation == OperationPublicationVerify) && r.Artifact.MediaType == artifactcap.MediaTypeGitBundle
		if !deltaDownload && !bundleDownload {
			return fmt.Errorf("artifact download is not valid for parent operation")
		}
	default:
		return fmt.Errorf("artifact operation is invalid")
	}
	return nil
}

// ArtifactAuthorizationResponse contains only a one-operation capability and
// its exact request digest. It never exposes the artifact signing key.
type ArtifactAuthorizationResponse struct {
	Capability    string `json:"capability"`
	RequestDigest string `json:"requestDigest"`
}

// ArtifactOperationID returns a deterministic, bounded replay-ledger identity
// for one remote transfer attempt.
func ArtifactOperationID(request ArtifactAuthorizationRequest) string {
	return "publisher-" + hashID(
		request.Metadata.Namespace,
		request.Metadata.OperationID,
		request.Metadata.TaskID,
		request.Metadata.PublicationID,
		string(request.ParentOperation),
		string(request.ArtifactOperation),
		request.Artifact.Digest,
		strconv.Itoa(request.Attempt),
	)
}

type artifactAuthorizer interface {
	Authorize(context.Context, ArtifactAuthorizationRequest) (artifactcap.Authorization, error)
}

type localArtifactAuthorizer struct {
	secret []byte
	ttl    time.Duration
	now    func() time.Time
}

func (a *localArtifactAuthorizer) Authorize(_ context.Context, request ArtifactAuthorizationRequest) (artifactcap.Authorization, error) {
	if err := request.Validate(); err != nil {
		return artifactcap.Authorization{}, err
	}
	binding, err := ArtifactBinding(request)
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	return artifactcap.Issue(a.secret, binding, a.now(), a.ttl)
}

type brokerArtifactAuthorizer struct {
	endpoint *url.URL
	client   *http.Client
	bearer   string
}

func newBrokerArtifactAuthorizer(rawURL string, client *http.Client, bearer []byte, timeout time.Duration) (*brokerArtifactAuthorizer, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("artifact authorization broker URL must be an absolute credential-free HTTP(S) URL")
	}
	if (parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS) || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("artifact authorization broker URL must contain only scheme and authority")
	}
	if len(bytes.TrimSpace(bearer)) < 16 {
		return nil, fmt.Errorf("artifact authorization broker bearer token is invalid")
	}
	parsed.Path = ArtifactAuthorizationBrokerPath
	parsed.RawPath = ""
	if client == nil {
		client = &http.Client{Timeout: timeout}
	} else {
		clone := *client
		client = &clone
		if client.Timeout == 0 {
			client.Timeout = timeout
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &brokerArtifactAuthorizer{endpoint: parsed, client: client, bearer: string(bytes.TrimSpace(bearer))}, nil
}

func (a *brokerArtifactAuthorizer) Authorize(ctx context.Context, request ArtifactAuthorizationRequest) (artifactcap.Authorization, error) {
	if err := request.Validate(); err != nil {
		return artifactcap.Authorization{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+a.bearer)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := a.client.Do(httpRequest)
	if err != nil {
		return artifactcap.Authorization{}, apiError(ErrArtifactTransport, "artifact_authorization_transport_failed", "artifact authorization broker transport failed", 503, true, err)
	}
	defer response.Body.Close() //nolint:errcheck
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxArtifactBrokerResponseBytes+1))
	if readErr != nil || int64(len(data)) > maxArtifactBrokerResponseBytes || response.StatusCode != http.StatusOK {
		retryable := response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests
		return artifactcap.Authorization{}, apiError(ErrArtifactTransport, "artifact_authorization_rejected", "artifact authorization broker rejected the transfer", response.StatusCode, retryable, readErr)
	}
	var decoded ArtifactAuthorizationResponse
	if err := decodeStrict(data, &decoded); err != nil || decoded.Capability == "" || !artifactcap.IsRequestDigest(decoded.RequestDigest) {
		return artifactcap.Authorization{}, apiError(ErrArtifactTransport, "artifact_authorization_response_invalid", "artifact authorization broker returned an invalid response", 502, false, err)
	}
	return artifactcap.Authorization{Capability: decoded.Capability, RequestDigest: decoded.RequestDigest}, nil
}

func ArtifactBinding(request ArtifactAuthorizationRequest) (artifactcap.OperationRequest, error) {
	if err := request.Validate(); err != nil {
		return artifactcap.OperationRequest{}, err
	}
	identity := artifactcap.Identity{Namespace: request.Metadata.Namespace, TaskID: request.Metadata.TaskID, PublicationID: request.Metadata.PublicationID}
	return artifactcap.OperationRequest{
		Operation: request.ArtifactOperation, ObjectDigest: request.Artifact.Digest, Identity: identity,
		ContentLength: request.Artifact.SizeBytes, MediaType: request.Artifact.MediaType,
		OperationID: ArtifactOperationID(request),
	}, nil
}
