package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/artifactcap"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const artifactAuthorizationBrokerPath = "/internal/v2/acp/artifact-authorizations"

type brokerArtifactAuthorizationProvider struct {
	endpoint         *url.URL
	client           *http.Client
	namespace        string
	controllerBearer string
	capabilitySecret []byte
}

type artifactBrokerRequest struct {
	Namespace string                      `json:"namespace"`
	Metadata  harnessv2.MutationMetadata  `json:"metadata"`
	Artifact  harnessv2.ArtifactReference `json:"artifact"`
}

type artifactBrokerResponse struct {
	Capability    string `json:"capability"`
	RequestDigest string `json:"requestDigest"`
}

func NewBrokerArtifactAuthorizationProvider(baseURL, namespace, bearer string, capabilitySecret []byte) (ArtifactAuthorizationProvider, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != providerProxyScheme && parsed.Scheme != providerProxyTLSScheme) || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("artifact broker URL is invalid")
	}
	parsed.Path = artifactAuthorizationBrokerPath
	if strings.TrimSpace(namespace) == "" || len(strings.TrimSpace(bearer)) < 32 || len(capabilitySecret) < harnessv2.MinCapabilitySecretBytes {
		return nil, fmt.Errorf("artifact broker identity is invalid")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &brokerArtifactAuthorizationProvider{endpoint: parsed, client: client, namespace: namespace, controllerBearer: bearer, capabilitySecret: append([]byte(nil), capabilitySecret...)}, nil
}

func (p *brokerArtifactAuthorizationProvider) AuthorizeArtifact(ctx context.Context, request ArtifactAuthorizationRequest) (artifactcap.Authorization, error) {
	if request.Operation != artifactcap.OperationUpload || request.WorkspaceDelta == nil {
		return artifactcap.Authorization{}, fmt.Errorf("artifact broker supports workspace-delta uploads only")
	}
	metadata := request.WorkspaceDelta.Metadata
	metadata.OperationID = harnessv2.OperationID("authorize-" + string(metadata.OperationID))
	metadata.ExpiresAt = time.Now().UTC().Add(30 * time.Second)
	metadata.RequestDigest = ""
	brokerRequest := artifactBrokerRequest{Namespace: p.namespace, Metadata: metadata, Artifact: request.Reference}
	digest, err := harnessv2.CanonicalRequestDigest(brokerRequest)
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	brokerRequest.Metadata.RequestDigest = digest
	capability, err := harnessv2.SignOperationCapability(p.capabilitySecret, harnessv2.ClaimsForMutation(brokerRequest.Metadata))
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	body, err := json.Marshal(brokerRequest)
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return artifactcap.Authorization{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+p.controllerBearer)
	httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	response, err := p.client.Do(httpRequest)
	if err != nil {
		return artifactcap.Authorization{}, fmt.Errorf("artifact authorization broker transport failed")
	}
	defer response.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	if err != nil || response.StatusCode != http.StatusOK {
		return artifactcap.Authorization{}, fmt.Errorf("artifact authorization broker rejected request")
	}
	var decoded artifactBrokerResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil || decoded.Capability == "" || !artifactcap.IsRequestDigest(decoded.RequestDigest) {
		return artifactcap.Authorization{}, fmt.Errorf("artifact authorization broker response is invalid")
	}
	return artifactcap.Authorization{Capability: decoded.Capability, RequestDigest: decoded.RequestDigest}, nil
}
