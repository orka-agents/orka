/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package conformance probes and validates the orka.gateway.v1 adapter contract.
package conformance

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

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

const (
	defaultTimeout          = 15 * time.Second
	conformanceMessageLimit = 1024
	redactedValue           = "[REDACTED]"
)

// Target describes one adapter endpoint.
type Target struct {
	BaseURL            string
	AuthorizationValue string
	HTTPClient         *http.Client
	Timeout            time.Duration
	DisableProxy       bool
	ReferenceFixtures  bool
}

// ProbeResult is the non-mutating health and capability result used by reconciliation.
type ProbeResult struct {
	Passed       bool
	Message      string
	Capabilities *protocol.CapabilitiesResponse
}

// CheckResult is the full adapter conformance result.
type CheckResult struct {
	Passed       bool
	Message      string
	Capabilities *protocol.CapabilitiesResponse
}

// Probe performs authenticated health and capability checks without sending a delivery.
func Probe(ctx context.Context, target Target) (result ProbeResult) {
	defer func() {
		result = sanitizeProbeResult(result, target.AuthorizationValue)
	}()

	client, baseURL, err := normalizedTarget(target)
	if err != nil {
		return ProbeResult{Message: err.Error()}
	}
	healthBody, status, err := request(ctx, client, http.MethodGet, baseURL+"/v1/health", target.AuthorizationValue, nil)
	if err != nil {
		return ProbeResult{Message: safeError("health probe failed", err)}
	}
	if status != http.StatusOK {
		return ProbeResult{Message: fmt.Sprintf("health probe returned HTTP %d", status)}
	}
	var health protocol.HealthResponse
	if err := json.Unmarshal(healthBody, &health); err != nil || strings.TrimSpace(health.Status) != "ok" {
		return ProbeResult{Message: "health probe returned an invalid response"}
	}

	capsBody, status, err := request(ctx, client, http.MethodGet, baseURL+"/v1/capabilities", target.AuthorizationValue, nil)
	if err != nil {
		return ProbeResult{Message: safeError("capability probe failed", err)}
	}
	if status != http.StatusOK {
		return ProbeResult{Message: fmt.Sprintf("capability probe returned HTTP %d", status)}
	}
	caps, err := protocol.DecodeCapabilities(capsBody)
	if err != nil {
		return ProbeResult{Message: err.Error()}
	}
	if capabilitiesContainCredential(caps, target.AuthorizationValue) {
		return ProbeResult{Message: "capability probe returned sensitive data"}
	}
	if err := verifyProbeAuthentication(ctx, client, baseURL, target.AuthorizationValue); err != nil {
		return ProbeResult{Message: err.Error(), Capabilities: caps}
	}
	return ProbeResult{Passed: true, Message: "adapter health and capabilities are valid", Capabilities: caps}
}

func verifyProbeAuthentication(ctx context.Context, client *http.Client, baseURL, validCredential string) error {
	for _, endpoint := range []string{"/v1/health", "/v1/capabilities"} {
		for _, credential := range []string{"", validCredential + "-invalid"} {
			responseBody, status, err := request(ctx, client, http.MethodGet, baseURL+endpoint, credential, nil)
			if err != nil {
				return fmt.Errorf("adapter authentication probe failed")
			}
			if containsCredential(responseBody, validCredential) {
				return fmt.Errorf("adapter authentication response contained sensitive data")
			}
			if status != http.StatusUnauthorized {
				return fmt.Errorf("adapter endpoint %s accepted missing or invalid authentication", endpoint)
			}
		}
	}
	return nil
}

// Check performs the reusable full contract check, including auth, size bounds,
// redaction safety, and idempotent delivery.
func Check(ctx context.Context, target Target) (result CheckResult) {
	defer func() {
		result = SanitizeCheckResult(result, target.AuthorizationValue)
	}()

	probe := Probe(ctx, target)
	if !probe.Passed {
		return CheckResult{Message: probe.Message, Capabilities: probe.Capabilities}
	}
	client, baseURL, err := normalizedTarget(target)
	if err != nil {
		return CheckResult{Message: err.Error()}
	}

	unauthorizedRequest := protocol.DeliveryRequest{
		ProtocolVersion: protocol.Version,
		DeliveryID:      "conformance-auth", IdempotencyID: "conformance-auth", OriginatingEvent: "conformance-event",
		Kind: protocol.DeliveryKindFinal, AccountID: "conformance", ContextID: "conformance",
		ReplyTarget: "conformance", Text: "conformance authentication probe",
	}
	body, _ := json.Marshal(unauthorizedRequest)
	unauthorizedBody, status, err := request(ctx, client, http.MethodPost, baseURL+"/v1/deliveries", "", body)
	if err != nil {
		return CheckResult{Message: safeError("unauthenticated delivery probe failed", err), Capabilities: probe.Capabilities}
	}
	if containsCredential(unauthorizedBody, target.AuthorizationValue) {
		return CheckResult{Message: "unauthenticated delivery response contained sensitive data", Capabilities: probe.Capabilities}
	}
	if status != http.StatusUnauthorized {
		return CheckResult{Message: fmt.Sprintf("adapter accepted missing outbound auth with HTTP %d", status), Capabilities: probe.Capabilities}
	}
	badAuthBody, status, err := request(
		ctx, client, http.MethodPost, baseURL+"/v1/deliveries", target.AuthorizationValue+"-invalid", body,
	)
	if err != nil {
		return CheckResult{Message: safeError("bad-auth delivery probe failed", err), Capabilities: probe.Capabilities}
	}
	if containsCredential(badAuthBody, target.AuthorizationValue) {
		return CheckResult{Message: "bad-auth delivery response contained sensitive data", Capabilities: probe.Capabilities}
	}
	if status != http.StatusUnauthorized {
		return CheckResult{Message: fmt.Sprintf("adapter accepted bad outbound auth with HTTP %d", status), Capabilities: probe.Capabilities}
	}
	if err := verifyDeliverySizeBounds(ctx, client, baseURL, target.AuthorizationValue, unauthorizedRequest); err != nil {
		return CheckResult{Message: err.Error(), Capabilities: probe.Capabilities}
	}

	delivery := unauthorizedRequest
	delivery.DeliveryID = "conformance-idempotency"
	delivery.IdempotencyID = delivery.DeliveryID
	body, _ = json.Marshal(delivery)
	firstBody, status, err := request(ctx, client, http.MethodPost, baseURL+"/v1/deliveries", target.AuthorizationValue, body)
	if err != nil || status != http.StatusOK {
		return CheckResult{Message: deliveryFailureMessage("delivery probe", status, err), Capabilities: probe.Capabilities}
	}
	if containsCredential(firstBody, target.AuthorizationValue) {
		return CheckResult{Message: "delivery response contained sensitive data", Capabilities: probe.Capabilities}
	}
	first, err := protocol.DecodeDeliveryResponse(firstBody)
	if err != nil || first.Status != protocol.DeliveryStatusDelivered {
		return CheckResult{Message: "delivery probe did not return delivered", Capabilities: probe.Capabilities}
	}
	secondBody, status, err := request(ctx, client, http.MethodPost, baseURL+"/v1/deliveries", target.AuthorizationValue, body)
	if err != nil || status != http.StatusOK {
		return CheckResult{Message: deliveryFailureMessage("duplicate delivery probe", status, err), Capabilities: probe.Capabilities}
	}
	if containsCredential(secondBody, target.AuthorizationValue) {
		return CheckResult{Message: "duplicate delivery response contained sensitive data", Capabilities: probe.Capabilities}
	}
	second, err := protocol.DecodeDeliveryResponse(secondBody)
	if err != nil || second.Status != protocol.DeliveryStatusDelivered || second.ProviderMessageID != first.ProviderMessageID {
		return CheckResult{Message: "adapter delivery idempotency probe failed", Capabilities: probe.Capabilities}
	}
	if target.ReferenceFixtures {
		for fixture, want := range map[string]string{
			"retryable": protocol.DeliveryStatusRetryableError,
			"permanent": protocol.DeliveryStatusNonRetryableError,
		} {
			fixtureDelivery := delivery
			fixtureDelivery.DeliveryID = "conformance-" + fixture
			fixtureDelivery.IdempotencyID = fixtureDelivery.DeliveryID
			fixtureDelivery.Metadata = map[string]string{"fixture": fixture}
			fixtureBody, _ := json.Marshal(fixtureDelivery)
			resultBody, fixtureStatus, fixtureErr := request(
				ctx, client, http.MethodPost, baseURL+"/v1/deliveries", target.AuthorizationValue, fixtureBody,
			)
			if fixtureErr != nil || fixtureStatus != http.StatusOK {
				return CheckResult{
					Message:      deliveryFailureMessage(fixture+" classification probe", fixtureStatus, fixtureErr),
					Capabilities: probe.Capabilities,
				}
			}
			if containsCredential(resultBody, target.AuthorizationValue) {
				return CheckResult{Message: fixture + " delivery response contained sensitive data", Capabilities: probe.Capabilities}
			}
			result, decodeErr := protocol.DecodeDeliveryResponse(resultBody)
			if decodeErr != nil || result.Status != want {
				return CheckResult{Message: fixture + " delivery classification probe failed", Capabilities: probe.Capabilities}
			}
		}
	}
	return CheckResult{Passed: true, Message: "adapter conforms to orka.gateway.v1", Capabilities: probe.Capabilities}
}

func verifyDeliverySizeBounds(
	ctx context.Context,
	client *http.Client,
	baseURL, credential string,
	base protocol.DeliveryRequest,
) error {
	oversizedText := base
	oversizedText.DeliveryID = "conformance-size-text"
	oversizedText.IdempotencyID = oversizedText.DeliveryID
	oversizedText.Text = strings.Repeat("x", protocol.MaxTextBytes+1)
	oversizedTextBody, err := json.Marshal(oversizedText)
	if err != nil {
		return fmt.Errorf("text size probe could not be encoded")
	}
	if err := expectRejectedDelivery(ctx, client, baseURL, credential, "oversized text", oversizedTextBody); err != nil {
		return err
	}

	oversizedBodyRequest := base
	oversizedBodyRequest.DeliveryID = "conformance-size-body"
	oversizedBodyRequest.IdempotencyID = oversizedBodyRequest.DeliveryID
	oversizedBody, err := json.Marshal(oversizedBodyRequest)
	if err != nil {
		return fmt.Errorf("request body size probe could not be encoded")
	}
	padding := protocol.MaxHTTPBodyBytes + 1 - len(oversizedBody)
	if padding <= 0 {
		return fmt.Errorf("request body size probe exceeded the limit before padding")
	}
	oversizedBody = append(oversizedBody, bytes.Repeat([]byte(" "), padding)...)
	return expectRejectedDelivery(ctx, client, baseURL, credential, "oversized request body", oversizedBody)
}

func expectRejectedDelivery(
	ctx context.Context,
	client *http.Client,
	baseURL, credential, probeName string,
	body []byte,
) error {
	responseBody, status, err := request(ctx, client, http.MethodPost, baseURL+"/v1/deliveries", credential, body)
	if err != nil {
		return fmt.Errorf("%s probe failed: %w", probeName, err)
	}
	if containsCredential(responseBody, credential) {
		return fmt.Errorf("%s response contained sensitive data", probeName)
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return nil
	}
	if status != http.StatusOK {
		return fmt.Errorf("%s probe returned HTTP %d instead of a bounded rejection", probeName, status)
	}
	response, err := protocol.DecodeDeliveryResponse(responseBody)
	if err != nil || response.Status != protocol.DeliveryStatusNonRetryableError {
		return fmt.Errorf("adapter accepted %s", probeName)
	}
	return nil
}

func normalizedTarget(target Target) (*http.Client, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(target.BaseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, "", fmt.Errorf("adapter base URL is invalid")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, "", fmt.Errorf("adapter base URL contains forbidden components")
	}
	if strings.TrimSpace(target.AuthorizationValue) == "" {
		return nil, "", fmt.Errorf("adapter bearer token is required")
	}
	timeout := target.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	client := &http.Client{Timeout: timeout}
	if target.HTTPClient != nil {
		copy := *target.HTTPClient
		client = &copy
		if client.Timeout <= 0 {
			client.Timeout = timeout
		}
	}
	if target.DisableProxy {
		transport, err := noProxyTransport(client.Transport)
		if err != nil {
			return nil, "", err
		}
		client.Transport = transport
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client, strings.TrimRight(parsed.String(), "/"), nil
}

func noProxyTransport(roundTripper http.RoundTripper) (*http.Transport, error) {
	if roundTripper == nil {
		base, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("default HTTP transport is not configurable")
		}
		transport := base.Clone()
		transport.Proxy = nil
		return transport, nil
	}
	base, ok := roundTripper.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("custom HTTP transport cannot disable proxying")
	}
	transport := base.Clone()
	transport.Proxy = nil
	return transport, nil
}

func request(ctx context.Context, client *http.Client, method, targetURL, token string, body []byte) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	limited := io.LimitReader(resp.Body, protocol.MaxAdapterResponseBytes+1)
	responseBody, err := io.ReadAll(limited)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(responseBody) > protocol.MaxAdapterResponseBytes {
		return nil, resp.StatusCode, fmt.Errorf("adapter response exceeded size limit")
	}
	return responseBody, resp.StatusCode, nil
}

// SanitizeCheckResult returns an output-safe copy of a conformance result. It
// provides a final defense before CLI or test harness serialization, even when
// a caller constructs a CheckResult directly.
func SanitizeCheckResult(result CheckResult, authorizationValue string) CheckResult {
	result.Message = sanitizeOutputText(result.Message, authorizationValue, conformanceMessageLimit)
	result.Capabilities = sanitizeCapabilities(result.Capabilities, authorizationValue)
	return result
}

func sanitizeProbeResult(result ProbeResult, authorizationValue string) ProbeResult {
	result.Message = sanitizeOutputText(result.Message, authorizationValue, conformanceMessageLimit)
	result.Capabilities = sanitizeCapabilities(result.Capabilities, authorizationValue)
	return result
}

func sanitizeCapabilities(
	capabilities *protocol.CapabilitiesResponse,
	authorizationValue string,
) *protocol.CapabilitiesResponse {
	if capabilities == nil {
		return nil
	}
	result := *capabilities
	result.ProtocolVersion = sanitizeOutputText(result.ProtocolVersion, authorizationValue, protocol.MaxIdentityBytes)
	result.AdapterName = sanitizeOutputText(result.AdapterName, authorizationValue, protocol.MaxIdentityBytes)
	result.AdapterVersion = sanitizeOutputText(result.AdapterVersion, authorizationValue, protocol.MaxIdentityBytes)
	return &result
}

func capabilitiesContainCredential(capabilities *protocol.CapabilitiesResponse, authorizationValue string) bool {
	if capabilities == nil {
		return false
	}
	credential := strings.TrimSpace(authorizationValue)
	if credential == "" {
		return false
	}
	return strings.Contains(capabilities.ProtocolVersion, credential) ||
		strings.Contains(capabilities.AdapterName, credential) ||
		strings.Contains(capabilities.AdapterVersion, credential)
}

func containsCredential(body []byte, authorizationValue string) bool {
	credential := strings.TrimSpace(authorizationValue)
	return credential != "" && bytes.Contains(body, []byte(credential))
}

func sanitizeOutputText(value, authorizationValue string, limit int) string {
	credential := strings.TrimSpace(authorizationValue)
	if credential != "" {
		value = strings.ReplaceAll(value, credential, redactedValue)
	}
	return protocol.SanitizeMessage(value, limit)
}

func safeError(prefix string, err error) string {
	if err == nil {
		return prefix
	}
	return protocol.SanitizeMessage(prefix+": "+err.Error(), conformanceMessageLimit)
}

func deliveryFailureMessage(prefix string, status int, err error) string {
	if err != nil {
		return safeError(prefix+" failed", err)
	}
	return fmt.Sprintf("%s returned HTTP %d", prefix, status)
}
