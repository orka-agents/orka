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
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	defaultOMSRequestTimeout = 15 * time.Second
	omsHTTPMaxConnsPerHost   = 4
)

// AdapterError is a bounded, provider-neutral OMS error. Raw provider bodies,
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
		return "OMS adapter error"
	}
	if e.Message == "" {
		return fmt.Sprintf("OMS adapter request failed (%s)", e.Code)
	}
	return e.Message
}

// OMSAdapter is the strict runtime subset used by governed memory dispatch and hydration.
type OMSAdapter interface {
	Capabilities(context.Context, protocol.CapabilitiesRequest) (*protocol.CapabilitiesResponse, error)
	ClaimOwnership(context.Context, protocol.OwnershipClaimRequest) (*protocol.OwnershipClaimResponse, error)
	AdvanceRoutingFence(context.Context, protocol.RoutingFenceRequest) (*protocol.RoutingFenceResponse, error)
	Mutate(context.Context, protocol.MutationEnvelope) (*protocol.MutationReceipt, error)
	Get(context.Context, protocol.GetRequest) (*protocol.GetResponse, error)
	LookupOperation(context.Context, protocol.OperationLookupRequest) (*protocol.OperationLookupResponse, error)
	Search(context.Context, protocol.SearchRequest) (*protocol.SearchResponse, error)
}

// OMSClient calls one strict authenticated public HTTPS adapter endpoint.
type OMSClient struct {
	baseURL          string
	token            string
	client           *http.Client
	maxRequestBytes  int64
	maxResponseBytes int64
}

// NewOMSClient constructs a no-proxy, no-redirect client pinned to the exact
// public address set that was already validated for the backend status.
func NewOMSClient(
	policy endpointpolicy.PublicHTTPSPolicy,
	base *http.Client,
	resolution endpointpolicy.Resolution,
	expectedCertificateDigest string,
	token string,
	maxRequestBytes int64,
	maxResponseBytes int64,
	timeout time.Duration,
) (*OMSClient, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("OMS bearer token is empty")
	}
	if maxRequestBytes <= 0 {
		return nil, fmt.Errorf("OMS maximum request size is invalid")
	}
	if maxResponseBytes <= 0 {
		return nil, fmt.Errorf("OMS maximum response size is invalid")
	}
	if timeout <= 0 || timeout > time.Minute {
		timeout = defaultOMSRequestTimeout
	}
	expectedCertificateDigest = strings.TrimSpace(expectedCertificateDigest)
	if expectedCertificateDigest == "" {
		return nil, fmt.Errorf("OMS server certificate digest is empty")
	}
	client, err := policy.NewPinnedHTTPClient(base, timeout, resolution)
	if err != nil {
		return nil, err
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		return nil, fmt.Errorf("OMS HTTP transport cannot enforce certificate identity")
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
			return fmt.Errorf("OMS server certificate identity changed")
		}
		return nil
	}
	transport.TLSClientConfig = tlsConfig
	transport.MaxConnsPerHost = omsHTTPMaxConnsPerHost
	client.Transport = transport
	return &OMSClient{
		baseURL:          strings.TrimRight(resolution.Identity, "/"),
		token:            token,
		client:           client,
		maxRequestBytes:  effectiveOMSRequestLimit(maxRequestBytes),
		maxResponseBytes: effectiveOMSResponseLimit(maxResponseBytes),
	}, nil
}

func (c *OMSClient) Capabilities(ctx context.Context, request protocol.CapabilitiesRequest) (*protocol.CapabilitiesResponse, error) {
	body, err := c.post(ctx, protocol.PathCapabilities, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := protocol.DecodeCapabilitiesResponse(body)
	if err != nil || !protocol.BindingEqual(response.Binding, request.Binding) {
		return nil, invalidOMSResponse("capabilities response did not match the request")
	}
	if int64(len(body)) > effectiveOMSResponseLimit(int64(response.Limits.MaxResponseBytes)) {
		return nil, oversizedOMSResponse(http.StatusOK, "", false)
	}
	return response, nil
}

func (c *OMSClient) ClaimOwnership(ctx context.Context, request protocol.OwnershipClaimRequest) (*protocol.OwnershipClaimResponse, error) {
	body, err := c.post(ctx, protocol.PathOwnershipClaim, request, isOwnershipConflictResponse, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := protocol.DecodeOwnershipClaimResponse(body)
	if err != nil || !protocol.BindingEqual(response.Binding, request.Binding) ||
		response.MaximumRoutingEpoch < request.Binding.RoutingEpoch {
		return nil, invalidOMSResponse("ownership response did not match the request")
	}
	return response, nil
}

func (c *OMSClient) AdvanceRoutingFence(ctx context.Context, request protocol.RoutingFenceRequest) (*protocol.RoutingFenceResponse, error) {
	body, err := c.post(ctx, protocol.PathRoutingFence, request, isRoutingFenceConflictResponse, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := protocol.DecodeRoutingFenceResponse(body)
	if err != nil || !protocol.BindingEqual(response.Binding, request.Binding) ||
		response.MaximumRoutingEpoch < request.Binding.RoutingEpoch {
		return nil, invalidOMSResponse("routing fence response did not match the request")
	}
	return response, nil
}

func (c *OMSClient) Mutate(ctx context.Context, request protocol.MutationEnvelope) (*protocol.MutationReceipt, error) {
	semantic := func(body []byte) bool {
		receipt, err := protocol.DecodeMutationReceipt(body)
		return err == nil && mutationReceiptCorrelatesRequest(request, receipt)
	}
	body, err := c.post(ctx, protocol.PathMutations, request, semantic, &request.Binding, true)
	if err != nil {
		return nil, err
	}
	receipt, err := protocol.DecodeMutationReceipt(body)
	if err != nil || !mutationReceiptCorrelatesRequest(request, receipt) {
		return nil, malformedOMSAdapterError(http.StatusOK, "", protocol.ErrorCodeInternal, "OMS mutation acknowledgement was invalid", true)
	}
	return receipt, nil
}

func (c *OMSClient) Get(ctx context.Context, request protocol.GetRequest) (*protocol.GetResponse, error) {
	body, err := c.post(ctx, protocol.PathRecordsGet, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := protocol.DecodeGetResponse(body)
	if err != nil || !protocol.BindingEqual(response.Binding, request.Binding) ||
		response.Found && (response.Record == nil || response.Record.UpsertKey != request.UpsertKey) {
		return nil, invalidOMSResponse("exact-get response did not match the request")
	}
	return response, nil
}

func (c *OMSClient) LookupOperation(ctx context.Context, request protocol.OperationLookupRequest) (*protocol.OperationLookupResponse, error) {
	body, err := c.post(ctx, protocol.PathOperationsGet, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := protocol.DecodeOperationLookupResponse(body)
	if err != nil || !protocol.BindingEqual(response.Binding, request.Binding) ||
		response.Found && (response.Receipt == nil || response.Receipt.OperationID != request.OperationID) {
		return nil, invalidOMSResponse("operation lookup response did not match the request")
	}
	return response, nil
}

func (c *OMSClient) Search(ctx context.Context, request protocol.SearchRequest) (*protocol.SearchResponse, error) {
	body, err := c.post(ctx, protocol.PathSearch, request, nil, &request.Binding, false)
	if err != nil {
		return nil, err
	}
	response, err := protocol.DecodeSearchResponse(body)
	if err != nil || !protocol.BindingEqual(response.Binding, request.Binding) ||
		response.RequestedMode != request.Mode || len(response.Records) > request.PageSize ||
		(request.Mode != protocol.SearchModeAuto && response.ActualMode != request.Mode) ||
		(!response.Exhausted && (response.NextPageToken == "" || response.NextPageToken == request.PageToken)) {
		return nil, invalidOMSResponse("search response did not match the request")
	}
	return response, nil
}

func invalidOMSResponse(message string) error {
	return malformedOMSAdapterError(http.StatusOK, "", protocol.ErrorCodeIdentityConflict, message, false)
}

func (c *OMSClient) post(
	ctx context.Context,
	endpointPath string,
	value any,
	semanticConflict func([]byte) bool,
	expectedBinding *protocol.Binding,
	malformedAmbiguous bool,
) ([]byte, error) {
	if c == nil || c.client == nil || c.baseURL == "" || c.token == "" {
		return nil, fmt.Errorf("OMS client is not configured")
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode OMS request: %w", err)
	}
	maxRequestBytes := effectiveOMSRequestLimit(c.maxRequestBytes)
	if int64(len(body)) > maxRequestBytes {
		return nil, fmt.Errorf("OMS request exceeds the configured %d-byte limit", maxRequestBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+endpointPath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create OMS request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	response, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OMS request failed: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck

	maxResponseBytes := effectiveOMSResponseLimit(c.maxResponseBytes)
	limited := io.LimitReader(response.Body, maxResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read OMS response: %w", err)
	}
	if int64(len(responseBody)) > maxResponseBytes {
		return nil, oversizedOMSResponse(response.StatusCode, response.Header.Get("Retry-After"), malformedAmbiguous)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return nil, malformedOMSAdapterError(response.StatusCode, response.Header.Get("Retry-After"),
			protocol.ErrorCodeInvalidRequest, "OMS adapter returned an unsupported content type", malformedAmbiguous)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if semanticConflict != nil && semanticConflict(responseBody) {
			return responseBody, nil
		}
		return nil, decodeOMSAdapterError(response.StatusCode, response.Header.Get("Retry-After"), responseBody, expectedBinding, malformedAmbiguous)
	}
	return responseBody, nil
}

func effectiveOMSRequestLimit(advertised int64) int64 {
	hardLimit := int64(protocol.MaxHTTPBodyBytes)
	if advertised > 0 && advertised < hardLimit {
		return advertised
	}
	return hardLimit
}

func effectiveOMSResponseLimit(advertised int64) int64 {
	hardLimit := int64(protocol.MaxAdapterResponseBytes)
	if advertised > 0 && advertised < hardLimit {
		return advertised
	}
	return hardLimit
}

func oversizedOMSResponse(status int, retryAfter string, malformedAmbiguous bool) error {
	return malformedOMSAdapterError(status, retryAfter,
		protocol.ErrorCodeResponseTooLarge, "OMS response exceeded the configured limit", malformedAmbiguous)
}

func isOwnershipConflictResponse(body []byte) bool {
	response, err := protocol.DecodeOwnershipClaimResponse(body)
	return err == nil && response.Result == protocol.ResultIdentityConflict
}

func isRoutingFenceConflictResponse(body []byte) bool {
	response, err := protocol.DecodeRoutingFenceResponse(body)
	return err == nil && (response.Result == protocol.ResultIdentityConflict || response.Result == protocol.ResultPreconditionFailed)
}

func mutationReceiptCorrelatesRequest(request protocol.MutationEnvelope, receipt *protocol.MutationReceipt) bool {
	return receipt != nil && protocol.BindingEqual(receipt.Binding, request.Binding) &&
		receipt.OperationID == request.OperationID && receipt.BindingDigest == protocol.BindingDigest(request.Binding) &&
		receipt.MutationDigest == request.MutationDigest
}

func decodeOMSAdapterError(status int, retryAfter string, body []byte, expectedBinding *protocol.Binding, malformedAmbiguous bool) error {
	response, err := protocol.DecodeErrorResponse(body)
	if err != nil {
		return malformedOMSAdapterError(status, retryAfter, protocol.ErrorCodeInternal, "OMS adapter returned an invalid error response", malformedAmbiguous)
	}
	preAuthenticationUnauthorized := status == http.StatusUnauthorized &&
		response.Code == protocol.ErrorCodeUnauthorized && response.Binding == nil
	if expectedBinding != nil && !preAuthenticationUnauthorized &&
		(response.Binding == nil || !protocol.BindingEqual(*response.Binding, *expectedBinding)) {
		return malformedOMSAdapterError(status, retryAfter, protocol.ErrorCodeIdentityConflict, "OMS error response binding did not match the request", malformedAmbiguous)
	}
	if expectedBinding == nil && response.Binding != nil {
		return malformedOMSAdapterError(status, retryAfter, protocol.ErrorCodeIdentityConflict, "OMS pre-binding error unexpectedly included a binding", malformedAmbiguous)
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

func malformedOMSAdapterError(status int, retryAfter, code, message string, forceRetryable ...bool) *AdapterError {
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
		RetryAfterSeconds: parseOMSRetryAfterSeconds(retryAfter),
	}
}

func parseOMSRetryAfterSeconds(raw string) int {
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
	case protocol.ErrorCodeUnauthorized:
		return code, "OMS adapter rejected authentication"
	case protocol.ErrorCodeInvalidRequest:
		return code, "OMS adapter rejected the request"
	case protocol.ErrorCodeMethodNotAllowed:
		return code, "OMS adapter rejected the request method"
	case protocol.ErrorCodeNotFound:
		return code, "OMS adapter endpoint was not found"
	case protocol.ErrorCodeInternal:
		return code, "OMS adapter reported an internal error"
	case protocol.ErrorCodeResponseTooLarge:
		return code, "OMS adapter response exceeded the configured limit"
	case protocol.ErrorCodeSearchModeUnsupported:
		return code, "OMS adapter does not support the requested search mode"
	case protocol.ErrorCodeIdentityConflict:
		return code, "OMS adapter rejected the authority identity"
	case protocol.ErrorCodeRoutingFenced:
		return code, "OMS adapter rejected a fenced routing epoch"
	case protocol.ErrorCodePageTokenInvalid:
		return code, "OMS adapter rejected the page token"
	case protocol.ErrorCodePageTokenExpired:
		return code, "OMS adapter page token expired"
	case protocol.ErrorCodeSnapshotCapacity:
		return code, "OMS adapter snapshot capacity is unavailable"
	default:
		return protocol.ErrorCodeInternal, "OMS adapter request failed"
	}
}
