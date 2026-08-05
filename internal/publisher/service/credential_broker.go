package service

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
)

const (
	CredentialBrokerPath             = "/internal/v2/acp/publisher-credentials"
	maxCredentialBrokerResponseBytes = int64(64 << 10)
)

// CredentialMaterialRequest binds one credential read to an authenticated
// Publisher operation and a non-secret Kubernetes Secret reference.
type CredentialMaterialRequest struct {
	ParentOperation Operation           `json:"parentOperation"`
	Metadata        OperationMetadata   `json:"metadata"`
	Reference       CredentialReference `json:"reference"`
}

func (r CredentialMaterialRequest) Validate() error {
	if r.ParentOperation.Path() == "" {
		return fmt.Errorf("credential parent operation is invalid")
	}
	if err := r.Metadata.validateFor(r.ParentOperation); err != nil {
		return err
	}
	expected := CredentialHTTPExtraHeader
	if r.ParentOperation == OperationPullRequestReconcile {
		expected = CredentialForgeToken
	}
	if err := validateCredentialReference(&r.Reference, expected); err != nil {
		return err
	}
	if r.Reference.Role == "" {
		return nil
	}
	allowed := false
	switch r.ParentOperation {
	case OperationWorkspaceResolve, OperationWorkspacePrepare:
		allowed = r.Reference.Role == CredentialRoleSourceRead || r.Reference.Role == CredentialRoleTargetRead
	case OperationPublicationPrepare:
		allowed = r.Reference.Role == CredentialRoleSourceRead || r.Reference.Role == CredentialRoleTargetRead
	case OperationPublicationPreflight, OperationPublicationVerify:
		allowed = r.Reference.Role == CredentialRoleTargetRead
	case OperationPublicationPublish:
		allowed = r.Reference.Role == CredentialRoleTargetWrite
	case OperationPullRequestReconcile:
		allowed = r.Reference.Role == CredentialRoleForge
	}
	if !allowed {
		return apiError(ErrCredential, "invalid_credential_ref", "credential reference role is invalid for the parent operation", 400, false, nil)
	}
	return nil
}

// CredentialMaterialResponse is never logged or persisted by the Publisher.
// ResourceVersion is non-secret audit metadata for the frozen source Secret.
type CredentialMaterialResponse struct {
	Material        string `json:"material"`
	ResourceVersion string `json:"resourceVersion"`
}

type credentialProvider interface {
	Read(context.Context, CredentialMaterialRequest) ([]byte, string, error)
}

type fileCredentialProvider struct{ root string }

func (p *fileCredentialProvider) Read(_ context.Context, request CredentialMaterialRequest) ([]byte, string, error) {
	if err := request.Validate(); err != nil {
		return nil, "", err
	}
	value, err := readCredentialFile(p.root, request.Reference.Name, 32<<10)
	return value, "local-file", err
}

type brokerCredentialProvider struct {
	endpoint *url.URL
	client   *http.Client
	bearer   string
}

func newBrokerCredentialProvider(rawURL string, client *http.Client, bearer []byte, timeout time.Duration) (*brokerCredentialProvider, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("credential broker URL must be an absolute credential-free HTTP(S) URL")
	}
	if (parsed.Scheme != schemeHTTP && parsed.Scheme != schemeHTTPS) || (parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("credential broker URL must contain only scheme and authority")
	}
	if len(bytes.TrimSpace(bearer)) < 16 {
		return nil, fmt.Errorf("credential broker bearer token is invalid")
	}
	parsed.Path = CredentialBrokerPath
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
	return &brokerCredentialProvider{endpoint: parsed, client: client, bearer: string(bytes.TrimSpace(bearer))}, nil
}

func (p *brokerCredentialProvider) Read(ctx context.Context, request CredentialMaterialRequest) ([]byte, string, error) {
	if err := request.Validate(); err != nil {
		return nil, "", err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, "", err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	httpRequest.Header.Set("Authorization", "Bearer "+p.bearer)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	response, err := p.client.Do(httpRequest)
	if err != nil {
		return nil, "", apiError(ErrCredential, "credential_broker_transport_failed", "operation credential broker transport failed", 503, true, err)
	}
	defer response.Body.Close() //nolint:errcheck
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxCredentialBrokerResponseBytes+1))
	if readErr != nil || int64(len(data)) > maxCredentialBrokerResponseBytes || response.StatusCode != http.StatusOK {
		retryable := response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests
		return nil, "", apiError(ErrCredential, "credential_broker_rejected", "operation credential broker rejected the request", response.StatusCode, retryable, readErr)
	}
	var decoded CredentialMaterialResponse
	if err := decodeStrict(data, &decoded); err != nil || decoded.Material == "" || decoded.ResourceVersion == "" {
		return nil, "", apiError(ErrCredential, "credential_broker_response_invalid", "operation credential broker returned an invalid response", 502, false, err)
	}
	value := []byte(decoded.Material)
	if len(value) > 32<<10 || bytes.ContainsAny(value, "\r\n\x00") {
		return nil, "", apiError(ErrCredential, "credential_broker_response_invalid", "operation credential broker returned invalid material", 502, false, nil)
	}
	return value, decoded.ResourceVersion, nil
}
