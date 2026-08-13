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

type ClientConfig struct {
	BaseURL          string
	HTTPClient       *http.Client
	BearerToken      []byte
	CapabilitySecret []byte
	CapabilityTTL    time.Duration
	MaxResponseBytes int64
	Now              func() time.Time
}

type Client struct {
	baseURL          *url.URL
	httpClient       *http.Client
	bearerToken      []byte
	capabilitySecret []byte
	capabilityTTL    time.Duration
	maxResponseBytes int64
	now              func() time.Time
}

type ClientError struct {
	StatusCode int
	Response   ErrorResponse
}

func (e *ClientError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("workspace publisher request failed with status %d and code %q", e.StatusCode, e.Response.Code)
}

func NewClient(config ClientConfig) (*Client, error) {
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("publisher base URL must be an absolute credential-free HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Path != "" && parsed.Path != "/" {
		return nil, fmt.Errorf("publisher base URL must contain only scheme and authority")
	}
	parsed.Path = ""
	if len(config.BearerToken) < 16 || len(config.CapabilitySecret) < MinSecretBytes {
		return nil, fmt.Errorf("publisher client authentication is invalid")
	}
	if config.CapabilityTTL == 0 {
		config.CapabilityTTL = time.Minute
	}
	if config.CapabilityTTL <= 0 || config.CapabilityTTL > MaxCapabilityTTL {
		return nil, fmt.Errorf("publisher capability TTL is invalid")
	}
	if config.MaxResponseBytes == 0 {
		config.MaxResponseBytes = defaultMaxResponseBytes
	}
	if config.MaxResponseBytes < 1 {
		return nil, fmt.Errorf("publisher response limit is invalid")
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	client := config.HTTPClient
	if client == nil {
		// No client-level timeout: operation deadlines come from the caller's
		// request context so configured operation timeouts (for example
		// ORKA_PUBLISHER_PUBLISH_TIMEOUT above the former three-minute client
		// ceiling) are honored. Requests without a caller deadline fall back to
		// defaultRequestTimeout in requestContext.
		client = &http.Client{}
	} else {
		clone := *client
		client = &clone
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		baseURL: parsed, httpClient: client, bearerToken: append([]byte(nil), config.BearerToken...),
		capabilitySecret: append([]byte(nil), config.CapabilitySecret...), capabilityTTL: config.CapabilityTTL,
		maxResponseBytes: config.MaxResponseBytes, now: config.Now,
	}, nil
}

func (c *Client) Health(ctx context.Context) (HealthResponse, error) {
	var response HealthResponse
	err := c.get(ctx, HealthPath, &response)
	return response, err
}

func (c *Client) Capabilities(ctx context.Context) (CapabilitiesResponse, error) {
	var response CapabilitiesResponse
	err := c.get(ctx, CapabilitiesPath, &response)
	return response, err
}

func (c *Client) ResolveWorkspace(ctx context.Context, request WorkspaceResolveRequest) (WorkspaceResolveResponse, error) {
	var response WorkspaceResolveResponse
	err := c.post(ctx, OperationWorkspaceResolve, request.Metadata, request, &response)
	return response, err
}

func (c *Client) PrepareWorkspace(ctx context.Context, request WorkspacePrepareRequest) (WorkspacePrepareResponse, error) {
	var response WorkspacePrepareResponse
	err := c.post(ctx, OperationWorkspacePrepare, request.Metadata, request, &response)
	return response, err
}

func (c *Client) PreflightPublication(ctx context.Context, request PublicationPreflightRequest) (PublicationPreflightResponse, error) {
	var response PublicationPreflightResponse
	err := c.post(ctx, OperationPublicationPreflight, request.Metadata, request, &response)
	return response, err
}

func (c *Client) PreparePublication(ctx context.Context, request PublicationPrepareRequest) (PublicationPrepareResponse, error) {
	var response PublicationPrepareResponse
	err := c.post(ctx, OperationPublicationPrepare, request.Metadata, request, &response)
	return response, err
}

func (c *Client) Publish(ctx context.Context, request PublicationPublishRequest) (PublicationPublishResponse, error) {
	var response PublicationPublishResponse
	err := c.post(ctx, OperationPublicationPublish, request.Metadata, request, &response)
	return response, err
}

func (c *Client) Verify(ctx context.Context, request PublicationVerifyRequest) (PublicationVerifyResponse, error) {
	var response PublicationVerifyResponse
	err := c.post(ctx, OperationPublicationVerify, request.Metadata, request, &response)
	return response, err
}

func (c *Client) ReclaimPublication(ctx context.Context, request PublicationReclaimRequest) (PublicationReclaimResponse, error) {
	var response PublicationReclaimResponse
	err := c.post(ctx, OperationPublicationReclaim, request.Metadata, request, &response)
	return response, err
}

func (c *Client) ReconcilePullRequest(ctx context.Context, request PullRequestReconcileRequest) (PullRequestReconcileResponse, error) {
	var response PullRequestReconcileResponse
	err := c.post(ctx, OperationPullRequestReconcile, request.Metadata, request, &response)
	return response, err
}

// defaultRequestTimeout bounds publisher requests whose caller context carries
// no deadline. Callers with explicit deadlines (for example settlement contexts
// sized to the publisher's configured operation timeouts) are never clamped.
const defaultRequestTimeout = 3 * time.Minute

func requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultRequestTimeout)
}

func (c *Client) get(ctx context.Context, path string, target any) error {
	ctx, cancel := requestContext(ctx)
	defer cancel()
	endpoint := *c.baseURL
	endpoint.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("create workspace publisher request")
	}
	request.Header.Set("Accept", "application/json")
	return c.do(request, target)
}

func (c *Client) post(ctx context.Context, operation Operation, metadata OperationMetadata, value, target any) error {
	ctx, cancel := requestContext(ctx)
	defer cancel()
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode workspace publisher request")
	}
	body, err = canonicalRequestBody(body)
	if err != nil {
		return err
	}
	requestDigest, err := RequestDigest(http.MethodPost, operation.Path(), body)
	if err != nil {
		return err
	}
	claims := NewClaims(operation, metadata, requestDigest, c.now(), c.capabilityTTL)
	capability, err := SignCapability(c.capabilitySecret, claims)
	if err != nil {
		return fmt.Errorf("authorize workspace publisher request")
	}
	endpoint := *c.baseURL
	endpoint.Path = operation.Path()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create workspace publisher request")
	}
	request.Header.Set("Authorization", "Bearer "+string(c.bearerToken))
	request.Header.Set(OperationCapabilityHeader, capability)
	request.Header.Set(OperationRequestDigestHeader, requestDigest)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.ContentLength = int64(len(body))
	return c.do(request, target)
}

func (c *Client) do(request *http.Request, target any) error {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("workspace publisher transport failed")
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode >= 300 && response.StatusCode < 400 && strings.TrimSpace(response.Header.Get("Location")) != "" {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.maxResponseBytes))
		return fmt.Errorf("workspace publisher redirects are forbidden")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil || int64(len(data)) > c.maxResponseBytes {
		return fmt.Errorf("workspace publisher response is invalid")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope ErrorResponse
		if decodeErr := decodeStrict(data, &envelope); decodeErr != nil || envelope.Code == "" || envelope.Message == "" {
			return &ClientError{StatusCode: response.StatusCode, Response: ErrorResponse{Code: "invalid_error_response", Message: "publisher returned an invalid error", Retryable: response.StatusCode >= 500}}
		}
		return &ClientError{StatusCode: response.StatusCode, Response: envelope}
	}
	if err := decodeStrict(data, target); err != nil {
		return fmt.Errorf("workspace publisher response is invalid")
	}
	return nil
}
