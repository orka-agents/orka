package workspaceagent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxErrorBodyBytes = 1024
	schemeHTTP        = "http"
	schemeHTTPS       = "https"
)

// ClientConfig configures a direct or provider-routed workspace-agent client.
type ClientConfig struct {
	Endpoint      string
	CAData        []byte
	AllowInsecure bool
	HTTPClient    *http.Client
	HostHeader    string
	ControlAuth   string
	Timeout       time.Duration
}

// Client calls one workspace-agent endpoint. It is safe for concurrent use.
type Client struct {
	baseURL     *url.URL
	httpClient  *http.Client
	hostHeader  string
	controlAuth string
}

// ErrorReason is a bounded transport/protocol failure reason suitable for metrics.
type ErrorReason string

const (
	ErrorReasonEncodeRequest   ErrorReason = "encode_request"
	ErrorReasonInvalidEndpoint ErrorReason = "invalid_endpoint"
	ErrorReasonCreateRequest   ErrorReason = "create_request"
	ErrorReasonRequestFailed   ErrorReason = "request_failed"
	ErrorReasonStatus          ErrorReason = "status"
	ErrorReasonDecodeResponse  ErrorReason = "decode_response"
	ErrorReasonVersion         ErrorReason = "protocol_version"
)

// Error is a sanitized client failure. It never contains bearer or control tokens.
type Error struct {
	Reason     ErrorReason
	Message    string
	StatusCode int
	Retryable  bool
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Cause != nil {
		return e.Message + ": " + e.Cause.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewClient validates transport policy and constructs a workspace-agent client.
func NewClient(config ClientConfig) (*Client, error) {
	rawEndpoint := strings.TrimSpace(config.Endpoint)
	parsed, err := url.Parse(rawEndpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, &Error{Reason: ErrorReasonInvalidEndpoint, Message: "invalid workspace-agent endpoint"}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, &Error{
			Reason:  ErrorReasonInvalidEndpoint,
			Message: "workspace-agent endpoint must not contain userinfo, query, or fragment",
		}
	}
	if parsed.Scheme != schemeHTTPS && parsed.Scheme != schemeHTTP {
		return nil, &Error{Reason: ErrorReasonInvalidEndpoint, Message: "workspace-agent endpoint must use http or https"}
	}
	if parsed.Scheme != schemeHTTPS && !config.AllowInsecure {
		return nil, &Error{Reason: ErrorReasonInvalidEndpoint, Message: "insecure workspace-agent transport is disabled"}
	}

	var httpClient *http.Client
	if config.HTTPClient == nil {
		httpClient = &http.Client{}
	} else {
		clone := *config.HTTPClient
		httpClient = &clone
	}
	transport, err := configureWorkspaceTransport(httpClient.Transport, parsed.Scheme, parsed.Hostname(), config.CAData)
	if err != nil {
		return nil, err
	}
	httpClient.Transport = transport
	if config.Timeout > 0 {
		httpClient.Timeout = config.Timeout
	} else if httpClient.Timeout <= 0 {
		httpClient.Timeout = 30 * time.Second
	}
	httpClient.CheckRedirect = rejectWorkspaceAgentRedirect

	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return &Client{
		baseURL:     parsed,
		httpClient:  httpClient,
		hostHeader:  strings.TrimSpace(config.HostHeader),
		controlAuth: strings.TrimSpace(config.ControlAuth),
	}, nil
}

// Health checks liveness and validates the protocol version.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var out HealthResponse
	if err := c.do(ctx, http.MethodGet, HealthPath, nil, authNone, AttachmentCredentials{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Capabilities returns bounded workspace-agent capabilities.
func (c *Client) Capabilities(ctx context.Context) (*CapabilitiesResponse, error) {
	var out CapabilitiesResponse
	if err := c.do(ctx, http.MethodGet, CapabilitiesPath, nil, authNone, AttachmentCredentials{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ActivateAttachment activates one epoch through the privileged control endpoint.
func (c *Client) ActivateAttachment(
	ctx context.Context,
	request AttachmentControlRequest,
) (*AttachmentControlResponse, error) {
	request.Versioned = NewVersioned()
	var out AttachmentControlResponse
	if err := c.do(
		ctx, http.MethodPut, AttachmentControlPath, request, authControl, AttachmentCredentials{}, &out,
	); err != nil {
		return nil, err
	}
	return &out, nil
}

// RevokeAttachment clears epoch before cleanup or Task terminalization.
func (c *Client) RevokeAttachment(
	ctx context.Context,
	workspaceUID string,
	bindingGeneration string,
	epoch int64,
) (*AttachmentControlResponse, error) {
	request := AttachmentRevocationRequest{
		Versioned:         NewVersioned(),
		WorkspaceUID:      workspaceUID,
		BindingGeneration: bindingGeneration,
	}
	var out AttachmentControlResponse
	path := AttachmentRevocationPath(strconv.FormatInt(epoch, 10))
	if err := c.do(ctx, http.MethodDelete, path, request, authControl, AttachmentCredentials{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Exec starts or returns the existing state of an idempotent operation.
func (c *Client) Exec(
	ctx context.Context,
	credentials AttachmentCredentials,
	request ExecRequest,
) (*ExecResponse, error) {
	request.Versioned = NewVersioned()
	var out ExecResponse
	if err := c.do(ctx, http.MethodPost, ExecPath, request, authAttachment, credentials, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExecStatus returns retained operation state/result.
func (c *Client) ExecStatus(
	ctx context.Context,
	credentials AttachmentCredentials,
	operationID string,
) (*ExecResponse, error) {
	var out ExecResponse
	if err := c.do(ctx, http.MethodGet, ExecStatusPath(operationID), nil, authAttachment, credentials, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel requests cancellation of a running operation.
func (c *Client) Cancel(
	ctx context.Context,
	credentials AttachmentCredentials,
	operationID string,
) (*CancelResponse, error) {
	var out CancelResponse
	if err := c.do(
		ctx, http.MethodPost, ExecCancelPath(operationID), NewVersioned(), authAttachment, credentials, &out,
	); err != nil {
		return nil, err
	}
	return &out, nil
}

// Upload writes files with path+digest idempotency.
func (c *Client) Upload(
	ctx context.Context,
	credentials AttachmentCredentials,
	request UploadRequest,
) (*UploadResponse, error) {
	request.Versioned = NewVersioned()
	var out UploadResponse
	if err := c.do(ctx, http.MethodPut, FilesPath, request, authAttachment, credentials, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Download reads selected workspace files.
func (c *Client) Download(
	ctx context.Context,
	credentials AttachmentCredentials,
	request DownloadRequest,
) (*DownloadResponse, error) {
	request.Versioned = NewVersioned()
	var out DownloadResponse
	if err := c.do(ctx, http.MethodPost, FilesDownloadPath, request, authAttachment, credentials, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Scrub runs privileged idempotent cleanup after attachment revocation.
func (c *Client) Scrub(ctx context.Context, request ScrubRequest) (*ScrubResponse, error) {
	request.Versioned = NewVersioned()
	var out ScrubResponse
	if err := c.do(ctx, http.MethodPost, ScrubPath, request, authControl, AttachmentCredentials{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reset runs a privileged idempotent reset after attachment revocation.
func (c *Client) Reset(ctx context.Context, request ResetRequest) (*ResetResponse, error) {
	request.Versioned = NewVersioned()
	var out ResetResponse
	if err := c.do(ctx, http.MethodPost, ResetPath, request, authControl, AttachmentCredentials{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func configureWorkspaceTransport(
	roundTripper http.RoundTripper,
	scheme string,
	hostname string,
	caData []byte,
) (http.RoundTripper, error) {
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	transport, ok := roundTripper.(*http.Transport)
	if !ok {
		if scheme == schemeHTTPS || len(caData) > 0 {
			return nil, &Error{
				Reason:  ErrorReasonInvalidEndpoint,
				Message: "custom HTTPS workspace-agent transport must use *http.Transport",
			}
		}
		return roundTripper, nil
	}
	clone := transport.Clone()
	hasCustomTLSDialer := clone.DialTLSContext != nil
	hasLegacyTLSDialer := clone.DialTLS != nil //nolint:staticcheck // Reject deprecated override too.
	if scheme == schemeHTTPS && (hasCustomTLSDialer || hasLegacyTLSDialer) {
		return nil, &Error{
			Reason:  ErrorReasonInvalidEndpoint,
			Message: "custom TLS dialers are not allowed for workspace-agent transport",
		}
	}
	if scheme != schemeHTTPS {
		return clone, nil
	}
	tlsConfig := clone.TLSClientConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	} else {
		tlsConfig = tlsConfig.Clone()
	}
	if tlsConfig.InsecureSkipVerify {
		return nil, &Error{
			Reason:  ErrorReasonInvalidEndpoint,
			Message: "TLS verification cannot be disabled for workspace-agent transport",
		}
	}
	if tlsConfig.ServerName != "" && !strings.EqualFold(tlsConfig.ServerName, hostname) {
		return nil, &Error{
			Reason:  ErrorReasonInvalidEndpoint,
			Message: "workspace-agent TLS server name must match the endpoint host",
		}
	}
	tlsConfig.ServerName = hostname
	if tlsConfig.MinVersion < tls.VersionTLS12 {
		tlsConfig.MinVersion = tls.VersionTLS12
	}
	if len(caData) > 0 {
		roots := tlsConfig.RootCAs
		if roots == nil {
			var err error
			roots, err = x509.SystemCertPool()
			if err != nil || roots == nil {
				roots = x509.NewCertPool()
			}
		} else {
			roots = roots.Clone()
		}
		if !roots.AppendCertsFromPEM(caData) {
			return nil, &Error{Reason: ErrorReasonInvalidEndpoint, Message: "invalid workspace-agent CA data"}
		}
		tlsConfig.RootCAs = roots
	}
	clone.TLSClientConfig = tlsConfig
	return clone, nil
}

func rejectWorkspaceAgentRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

type authMode int

const (
	authNone authMode = iota
	authControl
	authAttachment
)

func (c *Client) do(
	ctx context.Context,
	method string,
	path string,
	body any,
	mode authMode,
	credentials AttachmentCredentials,
	out any,
) error {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return &Error{Reason: ErrorReasonEncodeRequest, Message: "failed to encode workspace-agent request", Cause: err}
		}
		payload = bytes.NewReader(data)
	}

	endpoint := *c.baseURL
	endpoint.Path = strings.TrimSuffix(c.baseURL.Path, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), payload)
	if err != nil {
		return &Error{Reason: ErrorReasonCreateRequest, Message: "failed to create workspace-agent request", Cause: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.hostHeader != "" {
		req.Host = c.hostHeader
	}
	switch mode {
	case authControl:
		if c.controlAuth == "" {
			return &Error{Reason: ErrorReasonCreateRequest, Message: "workspace-agent control credential is unavailable"}
		}
		req.Header.Set("Authorization", "Bearer "+c.controlAuth)
	case authAttachment:
		missingToken := strings.TrimSpace(credentials.Bearer) == ""
		missingWorkspace := strings.TrimSpace(credentials.WorkspaceUID) == ""
		if missingToken || missingWorkspace || credentials.Epoch <= 0 {
			return &Error{Reason: ErrorReasonCreateRequest, Message: "workspace-agent attachment credentials are incomplete"}
		}
		req.Header.Set("Authorization", "Bearer "+credentials.Bearer)
		req.Header.Set(WorkspaceUIDHeader, credentials.WorkspaceUID)
		req.Header.Set(AttachmentEpochHeader, strconv.FormatInt(credentials.Epoch, 10))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &Error{
			Reason:    ErrorReasonRequestFailed,
			Message:   "workspace-agent request failed",
			Retryable: true,
			Cause:     sanitizeTransportError(err),
		}
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		message := readErrorMessage(resp.Body)
		message = redactSensitive(message, c.controlAuth, credentials.Bearer)
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return &Error{
			Reason:     ErrorReasonStatus,
			Message:    fmt.Sprintf("workspace-agent returned status %d: %s", resp.StatusCode, message),
			StatusCode: resp.StatusCode,
			Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError,
		}
	}
	if out == nil {
		return nil
	}
	if resp.StatusCode == http.StatusNoContent {
		return &Error{
			Reason:     ErrorReasonDecodeResponse,
			Message:    "workspace-agent response body is required",
			StatusCode: resp.StatusCode,
		}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<20)).Decode(out); err != nil {
		return &Error{
			Reason:    ErrorReasonDecodeResponse,
			Message:   "failed to decode workspace-agent response",
			Retryable: errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF),
			Cause:     err,
		}
	}
	version, ok := protocolVersionOf(out)
	if !ok || version != ProtocolVersion {
		return &Error{Reason: ErrorReasonVersion, Message: "workspace-agent response protocol version is incompatible"}
	}
	return nil
}

func protocolVersionOf(value any) (string, bool) {
	switch typed := value.(type) {
	case *HealthResponse:
		return typed.ProtocolVersion, true
	case *CapabilitiesResponse:
		return typed.ProtocolVersion, true
	case *AttachmentControlResponse:
		return typed.ProtocolVersion, true
	case *ExecResponse:
		return typed.ProtocolVersion, true
	case *CancelResponse:
		return typed.ProtocolVersion, true
	case *UploadResponse:
		return typed.ProtocolVersion, true
	case *DownloadResponse:
		return typed.ProtocolVersion, true
	case *ScrubResponse:
		return typed.ProtocolVersion, true
	case *ResetResponse:
		return typed.ProtocolVersion, true
	default:
		return "", false
	}
}

func redactSensitive(message string, values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			message = strings.ReplaceAll(message, value, "[redacted]")
		}
	}
	return message
}

func readErrorMessage(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return errors.New("transport error")
	}
	return errors.New("transport error")
}
