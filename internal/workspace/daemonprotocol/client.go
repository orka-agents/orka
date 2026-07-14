package daemonprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	maxErrorBodyBytes         = 1024
	maxResponseBodyDrainBytes = 64 << 10
)

type Client interface {
	Health(ctx context.Context, req ActorRequest) error
	Exec(ctx context.Context, req ActorRequest, body ExecRequest) (*ExecResponse, error)
	ExecStatus(ctx context.Context, req ActorRequest, execID string) (*ExecResponse, error)
	Upload(ctx context.Context, req ActorRequest, body UploadRequest) (*UploadResponse, error)
	UploadNoResponse(ctx context.Context, req ActorRequest, body UploadRequest) error
	Download(ctx context.Context, req ActorRequest, body DownloadRequest) (*DownloadResponse, error)
	Scrub(ctx context.Context, req ActorRequest, body ScrubRequest) error
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type HTTPClient struct {
	RouterURL      string
	ActorDNSSuffix string
	HTTPClient     HTTPDoer
}

var _ Client = HTTPClient{}

type ActorRequest struct {
	ActorID   string
	AuthValue string
}

type ErrorReason string

const (
	ErrorReasonEncodeRequest  ErrorReason = "encode_request"
	ErrorReasonInvalidURL     ErrorReason = "invalid_url"
	ErrorReasonCreateRequest  ErrorReason = "create_request"
	ErrorReasonRequestFailed  ErrorReason = "request_failed"
	ErrorReasonStatus         ErrorReason = "status"
	ErrorReasonDecodeResponse ErrorReason = "decode_response"
)

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

func (c HTTPClient) Health(ctx context.Context, req ActorRequest) error {
	return c.do(ctx, req, http.MethodGet, HealthPath, nil, nil)
}

func (c HTTPClient) Exec(ctx context.Context, req ActorRequest, body ExecRequest) (*ExecResponse, error) {
	var out ExecResponse
	if err := c.do(ctx, req, http.MethodPost, ExecPath, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c HTTPClient) ExecStatus(ctx context.Context, req ActorRequest, execID string) (*ExecResponse, error) {
	var out ExecResponse
	if err := c.do(ctx, req, http.MethodGet, ExecStatusPath(execID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c HTTPClient) Upload(ctx context.Context, req ActorRequest, body UploadRequest) (*UploadResponse, error) {
	var out UploadResponse
	if err := c.do(ctx, req, http.MethodPut, FilesPath, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c HTTPClient) UploadNoResponse(ctx context.Context, req ActorRequest, body UploadRequest) error {
	return c.do(ctx, req, http.MethodPut, FilesPath, body, nil)
}

func (c HTTPClient) Download(ctx context.Context, req ActorRequest, body DownloadRequest) (*DownloadResponse, error) {
	var out DownloadResponse
	if err := c.do(ctx, req, http.MethodPost, FilesDownloadPath, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c HTTPClient) Scrub(ctx context.Context, req ActorRequest, body ScrubRequest) error {
	return c.do(ctx, req, http.MethodPost, ScrubPath, body, nil)
}

func (c HTTPClient) do(ctx context.Context, req ActorRequest, method, relPath string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return &Error{Reason: ErrorReasonEncodeRequest, Message: "failed to encode request", Cause: err}
		}
		reader = bytes.NewReader(data)
	}
	endpoint, err := c.resolve(relPath)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return &Error{Reason: ErrorReasonCreateRequest, Message: "failed to create request", Cause: err}
	}
	httpReq.Host = strings.TrimSpace(req.ActorID) + "." + strings.TrimSpace(c.ActorDNSSuffix)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if auth := strings.TrimSpace(req.AuthValue); auth != "" {
		httpReq.Header.Set("Authorization", "Bearer "+auth)
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return &Error{Reason: ErrorReasonRequestFailed, Message: "daemon request failed", Retryable: true, Cause: err}
	}
	defer drainAndCloseResponseBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		message := fmt.Sprintf("daemon returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		return &Error{Reason: ErrorReasonStatus, Message: message, StatusCode: resp.StatusCode, Retryable: resp.StatusCode >= 500}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{Reason: ErrorReasonDecodeResponse, Message: "failed to decode response", Cause: err}
	}
	return nil
}

func drainAndCloseResponseBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.CopyN(io.Discard, body, maxResponseBodyDrainBytes)
	_ = body.Close()
}

func (c HTTPClient) resolve(relPath string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(c.RouterURL))
	if err != nil {
		return "", &Error{Reason: ErrorReasonInvalidURL, Message: "invalid router URL", Cause: err}
	}
	if base.Scheme == "" || base.Host == "" {
		return "", &Error{Reason: ErrorReasonInvalidURL, Message: "invalid router URL"}
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(relPath, "/")
	return base.String(), nil
}
