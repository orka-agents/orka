package memory

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/endpointpolicy"
	memoryv1 "github.com/orka-agents/orka/pkg/memory/v1"
)

const (
	defaultProtocolRequestTimeout = 15 * time.Second
	adapterHTTPMaxConnsPerHost    = 4
)

// AdapterError is a bounded, provider-neutral memory protocol error. Raw provider bodies,
// headers, URLs, and credentials are never retained.
type AdapterError struct {
	StatusCode        int
	Code              string
	Message           string
	Retryable         bool
	RetryAfterSeconds int
}

func (e *AdapterError) Error() string {
	if e == nil {
		return "memory adapter error"
	}
	if e.Message == "" {
		return fmt.Sprintf("memory adapter request failed (%s)", e.Code)
	}
	return e.Message
}

// ProtocolAdapter is the strict runtime subset used by governed memory dispatch and hydration.
type ProtocolAdapter interface {
	Capabilities(context.Context, memoryv1.CapabilitiesRequest) (*memoryv1.CapabilitiesResponse, error)
	ClaimOwnership(context.Context, memoryv1.OwnershipClaimRequest) (*memoryv1.OwnershipClaimResponse, error)
	AdvanceRoutingFence(context.Context, memoryv1.RoutingFenceRequest) (*memoryv1.RoutingFenceResponse, error)
	Mutate(context.Context, memoryv1.MutationEnvelope) (*memoryv1.MutationReceipt, error)
	Get(context.Context, memoryv1.GetRequest) (*memoryv1.GetResponse, error)
	LookupOperation(context.Context, memoryv1.OperationLookupRequest) (*memoryv1.OperationLookupResponse, error)
	Search(context.Context, memoryv1.SearchRequest) (*memoryv1.SearchResponse, error)
}

// ProtocolClient calls one strict authenticated public HTTPS adapter endpoint.
type ProtocolClient struct {
	baseURL          string
	token            string
	client           *http.Client
	maxRequestBytes  int64
	maxResponseBytes int64
}

// NewProtocolClient constructs a no-proxy, no-redirect client pinned to the exact
// public address set that was already validated for the backend status.
func NewProtocolClient(
	policy endpointpolicy.PublicHTTPSPolicy,
	base *http.Client,
	resolution endpointpolicy.Resolution,
	expectedCertificateDigest string,
	token string,
	maxRequestBytes int64,
	maxResponseBytes int64,
	timeout time.Duration,
) (*ProtocolClient, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("memory adapter bearer token is empty")
	}
	if maxRequestBytes <= 0 {
		return nil, fmt.Errorf("memory protocol maximum request size is invalid")
	}
	if maxResponseBytes <= 0 {
		return nil, fmt.Errorf("memory protocol maximum response size is invalid")
	}
	if timeout <= 0 || timeout > time.Minute {
		timeout = defaultProtocolRequestTimeout
	}
	expectedCertificateDigest = strings.TrimSpace(expectedCertificateDigest)
	if expectedCertificateDigest == "" {
		return nil, fmt.Errorf("memory adapter server certificate digest is empty")
	}
	client, err := policy.NewPinnedHTTPClient(base, timeout, resolution)
	if err != nil {
		return nil, err
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		return nil, fmt.Errorf("memory protocol HTTP transport cannot enforce certificate identity")
	}
	transport = transport.Clone()
	tlsConfig := transport.TLSClientConfig.Clone()
	previousVerify := tlsConfig.VerifyConnection
	tlsConfig.VerifyConnection = func(state tls.ConnectionState) error {
		if previousVerify != nil {
			if err := previousVerify(state); err != nil {
				return err
			}
		}
		actual, err := endpointpolicy.CertificateDigest(&state)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(actual), []byte(expectedCertificateDigest)) != 1 {
			return fmt.Errorf("memory adapter server certificate identity changed")
		}
		return nil
	}
	transport.TLSClientConfig = tlsConfig
	transport.MaxConnsPerHost = adapterHTTPMaxConnsPerHost
	client.Transport = transport
	return &ProtocolClient{
		baseURL:          strings.TrimRight(resolution.Identity, "/"),
		token:            token,
		client:           client,
		maxRequestBytes:  effectiveProtocolRequestLimit(maxRequestBytes),
		maxResponseBytes: effectiveProtocolResponseLimit(maxResponseBytes),
	}, nil
}

func (c *ProtocolClient) Capabilities(ctx context.Context, request memoryv1.CapabilitiesRequest) (*memoryv1.CapabilitiesResponse, error) {
	body, err := c.post(ctx, memoryv1.PathCapabilities, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := memoryv1.DecodeCapabilitiesResponse(body)
	if err != nil || !memoryv1.BindingEqual(response.Binding, request.Binding) {
		return nil, invalidProtocolResponse("capabilities response did not match the request")
	}
	if int64(len(body)) > effectiveProtocolResponseLimit(int64(response.Limits.MaxResponseBytes)) {
		return nil, oversizedProtocolResponse(http.StatusOK, "", false)
	}
	return response, nil
}

func (c *ProtocolClient) ClaimOwnership(ctx context.Context, request memoryv1.OwnershipClaimRequest) (*memoryv1.OwnershipClaimResponse, error) {
	body, err := c.post(ctx, memoryv1.PathOwnershipClaim, request, isOwnershipConflictResponse, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := memoryv1.DecodeOwnershipClaimResponse(body)
	if err != nil || !memoryv1.BindingEqual(response.Binding, request.Binding) ||
		response.MaximumRoutingEpoch < request.Binding.RoutingEpoch {
		return nil, invalidProtocolResponse("ownership response did not match the request")
	}
	return response, nil
}

func (c *ProtocolClient) AdvanceRoutingFence(ctx context.Context, request memoryv1.RoutingFenceRequest) (*memoryv1.RoutingFenceResponse, error) {
	body, err := c.post(ctx, memoryv1.PathRoutingFence, request, isRoutingFenceConflictResponse, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := memoryv1.DecodeRoutingFenceResponse(body)
	if err != nil || !memoryv1.BindingEqual(response.Binding, request.Binding) ||
		response.MaximumRoutingEpoch < request.Binding.RoutingEpoch {
		return nil, invalidProtocolResponse("routing fence response did not match the request")
	}
	return response, nil
}

func (c *ProtocolClient) Mutate(ctx context.Context, request memoryv1.MutationEnvelope) (*memoryv1.MutationReceipt, error) {
	semantic := func(body []byte) bool {
		receipt, err := memoryv1.DecodeMutationReceipt(body)
		return err == nil && mutationReceiptCorrelatesRequest(request, receipt)
	}
	body, err := c.post(ctx, memoryv1.PathMutations, request, semantic, &request.Binding, true)
	if err != nil {
		return nil, err
	}
	receipt, err := memoryv1.DecodeMutationReceipt(body)
	if err != nil || !mutationReceiptCorrelatesRequest(request, receipt) {
		return nil, malformedProtocolAdapterError(http.StatusOK, "", memoryv1.ErrorCodeInternal, "memory protocol mutation acknowledgement was invalid", true)
	}
	return receipt, nil
}

func (c *ProtocolClient) Get(ctx context.Context, request memoryv1.GetRequest) (*memoryv1.GetResponse, error) {
	body, err := c.post(ctx, memoryv1.PathRecordsGet, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := memoryv1.DecodeGetResponse(body)
	if err != nil || !memoryv1.BindingEqual(response.Binding, request.Binding) ||
		response.Found && (response.Record == nil || response.Record.UpsertKey != request.UpsertKey) {
		return nil, invalidProtocolResponse("exact-get response did not match the request")
	}
	return response, nil
}

func (c *ProtocolClient) LookupOperation(ctx context.Context, request memoryv1.OperationLookupRequest) (*memoryv1.OperationLookupResponse, error) {
	body, err := c.post(ctx, memoryv1.PathOperationsGet, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := memoryv1.DecodeOperationLookupResponse(body)
	if err != nil || !memoryv1.BindingEqual(response.Binding, request.Binding) ||
		response.Found && (response.Receipt == nil || response.Receipt.OperationID != request.OperationID) {
		return nil, invalidProtocolResponse("operation lookup response did not match the request")
	}
	return response, nil
}

func (c *ProtocolClient) Search(ctx context.Context, request memoryv1.SearchRequest) (*memoryv1.SearchResponse, error) {
	body, err := c.post(ctx, memoryv1.PathSearch, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := memoryv1.DecodeSearchResponse(body)
	if err != nil || !memoryv1.BindingEqual(response.Binding, request.Binding) ||
		response.RequestedMode != request.Mode || len(response.Records) > request.PageSize ||
		(request.Mode != memoryv1.SearchModeAuto && response.ActualMode != request.Mode) ||
		(!response.Exhausted && (response.NextPageToken == "" || response.NextPageToken == request.PageToken)) {
		return nil, invalidProtocolResponse("search response did not match the request")
	}
	return response, nil
}

func invalidProtocolResponse(message string) error {
	return malformedProtocolAdapterError(http.StatusOK, "", memoryv1.ErrorCodeIdentityConflict, message, false)
}

func (c *ProtocolClient) post(
	ctx context.Context,
	endpointPath string,
	value any,
	semanticConflict func([]byte) bool,
	expectedBinding *memoryv1.Binding,
	malformedAmbiguous bool,
) ([]byte, error) {
	if c == nil || c.client == nil || c.baseURL == "" || c.token == "" {
		return nil, fmt.Errorf("memory protocol client is not configured")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode memory protocol request: %w", err)
	}
	maxRequestBytes := effectiveProtocolRequestLimit(c.maxRequestBytes)
	if int64(len(body)) > maxRequestBytes {
		return nil, fmt.Errorf("memory protocol request exceeds the configured %d-byte limit", maxRequestBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpointPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create memory protocol request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	response, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memory protocol request failed: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck

	maxResponseBytes := effectiveProtocolResponseLimit(c.maxResponseBytes)
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read memory protocol response: %w", err)
	}
	if int64(len(responseBody)) > maxResponseBytes {
		return nil, oversizedProtocolResponse(response.StatusCode, response.Header.Get("Retry-After"), malformedAmbiguous)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return nil, malformedProtocolAdapterError(response.StatusCode, response.Header.Get("Retry-After"),
			memoryv1.ErrorCodeInvalidRequest, "memory adapter returned an unsupported content type", malformedAmbiguous)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if semanticConflict != nil && semanticConflict(responseBody) {
			return responseBody, nil
		}
		return nil, decodeProtocolAdapterError(response.StatusCode, response.Header.Get("Retry-After"), responseBody, expectedBinding, malformedAmbiguous)
	}
	return responseBody, nil
}

func effectiveProtocolRequestLimit(advertised int64) int64 {
	hardLimit := int64(memoryv1.MaxHTTPBodyBytes)
	if advertised > 0 && advertised < hardLimit {
		return advertised
	}
	return hardLimit
}

func effectiveProtocolResponseLimit(advertised int64) int64 {
	hardLimit := int64(memoryv1.MaxAdapterResponseBytes)
	if advertised > 0 && advertised < hardLimit {
		return advertised
	}
	return hardLimit
}

func oversizedProtocolResponse(status int, retryAfter string, malformedAmbiguous bool) error {
	return malformedProtocolAdapterError(status, retryAfter,
		memoryv1.ErrorCodeResponseTooLarge, "memory adapter response exceeded the configured limit", malformedAmbiguous)
}

func isOwnershipConflictResponse(body []byte) bool {
	response, err := memoryv1.DecodeOwnershipClaimResponse(body)
	return err == nil && response.Result == memoryv1.ResultIdentityConflict
}

func isRoutingFenceConflictResponse(body []byte) bool {
	response, err := memoryv1.DecodeRoutingFenceResponse(body)
	return err == nil && (response.Result == memoryv1.ResultIdentityConflict || response.Result == memoryv1.ResultPreconditionFailed)
}

func mutationReceiptCorrelatesRequest(request memoryv1.MutationEnvelope, receipt *memoryv1.MutationReceipt) bool {
	return receipt != nil && memoryv1.BindingEqual(receipt.Binding, request.Binding) &&
		receipt.OperationID == request.OperationID && receipt.BindingDigest == memoryv1.BindingDigest(request.Binding) &&
		receipt.MutationDigest == request.MutationDigest
}

func decodeProtocolAdapterError(status int, retryAfter string, body []byte, expectedBinding *memoryv1.Binding, malformedAmbiguous bool) error {
	response, err := memoryv1.DecodeErrorResponse(body)
	if err != nil {
		return malformedProtocolAdapterError(status, retryAfter, memoryv1.ErrorCodeInternal, "memory adapter returned an invalid error response", malformedAmbiguous)
	}
	preAuthenticationUnauthorized := status == http.StatusUnauthorized &&
		response.Code == memoryv1.ErrorCodeUnauthorized && response.Binding == nil
	if expectedBinding != nil && !preAuthenticationUnauthorized &&
		(response.Binding == nil || !memoryv1.BindingEqual(*response.Binding, *expectedBinding)) {
		return malformedProtocolAdapterError(status, retryAfter, memoryv1.ErrorCodeIdentityConflict, "memory protocol error response binding did not match the request", malformedAmbiguous)
	}
	if expectedBinding == nil && response.Binding != nil {
		return malformedProtocolAdapterError(status, retryAfter, memoryv1.ErrorCodeIdentityConflict, "memory protocol pre-binding error unexpectedly included a binding", malformedAmbiguous)
	}
	code, message := localAdapterError(response.Code)
	return &AdapterError{
		StatusCode:        status,
		Code:              code,
		Message:           message,
		Retryable:         response.Retryable,
		RetryAfterSeconds: response.RetryAfterSeconds,
	}
}

func malformedProtocolAdapterError(status int, retryAfter, code, message string, forceRetryable ...bool) *AdapterError {
	retryable := len(forceRetryable) > 0 && forceRetryable[0]
	if !retryable {
		retryable = status >= http.StatusOK && status < http.StatusMultipleChoices ||
			status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	}
	return &AdapterError{
		StatusCode:        status,
		Code:              code,
		Message:           message,
		Retryable:         retryable,
		RetryAfterSeconds: parseProtocolRetryAfterSeconds(retryAfter),
	}
}

func parseProtocolRetryAfterSeconds(raw string) int {
	seconds, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || seconds <= 0 {
		return 0
	}
	if seconds > 3600 {
		return 3600
	}
	return seconds
}

func localAdapterError(code string) (string, string) {
	switch code {
	case memoryv1.ErrorCodeUnauthorized:
		return code, "memory adapter rejected authentication"
	case memoryv1.ErrorCodeInvalidRequest:
		return code, "memory adapter rejected the request"
	case memoryv1.ErrorCodeMethodNotAllowed:
		return code, "memory adapter rejected the request method"
	case memoryv1.ErrorCodeNotFound:
		return code, "memory adapter endpoint was not found"
	case memoryv1.ErrorCodeInternal:
		return code, "memory adapter reported an internal error"
	case memoryv1.ErrorCodeResponseTooLarge:
		return code, "memory adapter response exceeded the configured limit"
	case memoryv1.ErrorCodeSearchModeUnsupported:
		return code, "memory adapter does not support the requested search mode"
	case memoryv1.ErrorCodeIdentityConflict:
		return code, "memory adapter rejected the authority identity"
	case memoryv1.ErrorCodeRoutingFenced:
		return code, "memory adapter rejected a fenced routing epoch"
	case memoryv1.ErrorCodePageTokenInvalid:
		return code, "memory adapter rejected the page token"
	case memoryv1.ErrorCodePageTokenExpired:
		return code, "memory adapter page token expired"
	case memoryv1.ErrorCodeSnapshotCapacity:
		return code, "memory adapter snapshot capacity is unavailable"
	default:
		return memoryv1.ErrorCodeInternal, "memory adapter request failed"
	}
}
