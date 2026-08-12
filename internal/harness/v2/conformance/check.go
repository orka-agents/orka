package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	defaultControlTimeout = 60 * time.Second
	maxProbeResponseBytes = int64(harnessv2.MaxCanonicalJSONBytes)
)

// Check probes one external runtime. It is deliberately single-attempt: no
// control request is retried, and the lifecycle probe opens exactly one prompt
// stream that is consumed to its original terminal settlement.
func Check(ctx context.Context, target Target) Result {
	result := Result{}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateTarget(target); err != nil {
		result.Message = boundedMessage(err)
		return result
	}
	timeout := target.ControlTimeout
	if timeout <= 0 {
		timeout = defaultControlTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpClient := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client, err := harnessv2.NewClient(
		target.BaseURL,
		harnessv2.WithHTTPClient(httpClient),
		harnessv2.WithControlTimeout(timeout),
		harnessv2.WithControllerBearerToken(target.ControllerBearerToken),
		harnessv2.WithOperationCapabilitySecret(target.OperationCapabilitySecret),
		harnessv2.WithProtocolLimits(target.Limits),
	)
	if err != nil {
		result.Message = boundedMessage(fmt.Errorf("construct v2 client: %w", err))
		return result
	}

	health, err := client.Health(probeCtx)
	if err != nil {
		result.Message = boundedMessage(fmt.Errorf("unauthenticated health probe: %w", err))
		return result
	}
	if health.Status != harnessv2.HealthStatusOK {
		result.Message = fmt.Sprintf("runtime health status is %q, want %q", health.Status, harnessv2.HealthStatusOK)
		return result
	}

	capabilities, err := getCapabilities(probeCtx, httpClient, target.BaseURL)
	if err != nil {
		result.Message = boundedMessage(fmt.Errorf("unauthenticated capabilities probe: %w", err))
		return result
	}
	result.ObservedCapabilities = capabilities
	if err := validateExactCapabilities(target, capabilities); err != nil {
		result.Message = boundedMessage(err)
		return result
	}

	if err := probeStatusAuthNegatives(probeCtx, httpClient, target); err != nil {
		result.Message = boundedMessage(err)
		return result
	}
	status, err := client.Status(probeCtx)
	if err != nil {
		result.Message = boundedMessage(fmt.Errorf("authenticated status probe: %w", err))
		return result
	}
	result.ObservedStatus = status
	if err := validateExactStatus(target, status); err != nil {
		result.Message = boundedMessage(err)
		return result
	}

	if err := probeMutationAuthNegatives(probeCtx, httpClient, target); err != nil {
		result.Message = boundedMessage(err)
		return result
	}

	if target.ProbeLifecycle {
		result.LifecycleProbeExecuted = true
		if err := probeLifecycle(probeCtx, httpClient, client, target, status); err != nil {
			result.Message = boundedMessage(err)
			return result
		}
	}

	result.Passed = true
	result.Message = "orka.harness.v2 conformance passed"
	return result
}

func validateTarget(target Target) error {
	if strings.TrimSpace(target.BaseURL) == "" {
		return fmt.Errorf("base URL is required")
	}
	if strings.TrimSpace(string(target.ExpectedRuntimeInstanceID)) == "" {
		return fmt.Errorf("expected runtime instance ID is required")
	}
	if err := target.Profile.Validate(); err != nil {
		return fmt.Errorf("expected runtime profile: %w", err)
	}
	if len(target.Profile.AdapterDigests) != 1 {
		return fmt.Errorf("external AgentRuntime profile must pin exactly one adapter digest")
	}
	digest, err := harnessv2.CanonicalProfileDigest(target.Profile)
	if err != nil {
		return fmt.Errorf("expected runtime profile digest: %w", err)
	}
	if err := harnessv2.ValidateProfileDigest(digest); err != nil {
		return fmt.Errorf("expected runtime profile digest: %w", err)
	}
	if err := target.Limits.Validate(); err != nil {
		return fmt.Errorf("expected protocol limits: %w", err)
	}
	if err := target.WorkspaceGovernance.Validate(); err != nil {
		return fmt.Errorf("expected workspace governance: %w", err)
	}
	if len(target.ControllerBearerToken) < 32 {
		return fmt.Errorf("controller bearer token must be at least 32 bytes")
	}
	if len(target.OperationCapabilitySecret) < harnessv2.MinCapabilitySecretBytes {
		return fmt.Errorf("operation capability secret must be at least %d bytes", harnessv2.MinCapabilitySecretBytes)
	}
	if target.ControlTimeout < 0 {
		return fmt.Errorf("control timeout must not be negative")
	}
	return nil
}

func validateExactCapabilities(target Target, observed *CapabilitiesResponse) error {
	if observed == nil {
		return fmt.Errorf("runtime returned no capabilities")
	}
	expectedDigest, err := harnessv2.CanonicalProfileDigest(target.Profile)
	if err != nil {
		return fmt.Errorf("canonicalize expected runtime profile: %w", err)
	}
	base := observed.CapabilitiesResponse
	if base.RuntimeProfileDigest != expectedDigest {
		return fmt.Errorf("runtime profile digest %q does not match expected %q", base.RuntimeProfileDigest, expectedDigest)
	}
	if base.ProfileDigestSchemaVersion != harnessv2.ProfileDigestSchemaVersion {
		return fmt.Errorf("profile digest schema version %d does not match expected %d", base.ProfileDigestSchemaVersion, harnessv2.ProfileDigestSchemaVersion)
	}
	if base.ACPVersion != target.Profile.ACPProfile {
		return fmt.Errorf("ACP profile %q does not match expected %q", base.ACPVersion, target.Profile.ACPProfile)
	}
	if !maps.Equal(base.AdapterDigests, target.Profile.AdapterDigests) {
		return fmt.Errorf("adapter digest set does not exactly match the registered runtime profile")
	}
	if base.Limits != target.Limits {
		return fmt.Errorf("protocol limits do not exactly match the AgentRuntime registration")
	}
	if base.SupportsDrain != target.SupportsDrain {
		return fmt.Errorf("supportsDrain=%t does not match expected %t", base.SupportsDrain, target.SupportsDrain)
	}
	if base.SupportsPublicationFinalization != target.SupportsPublicationFinalization {
		return fmt.Errorf("supportsPublicationFinalization=%t does not match expected %t", base.SupportsPublicationFinalization, target.SupportsPublicationFinalization)
	}
	if observed.WorkspaceGovernance != target.WorkspaceGovernance {
		return fmt.Errorf("workspace governance claims do not exactly match the AgentRuntime registration")
	}
	if !slices.Contains(base.Provider.ProviderKinds, target.Profile.ProviderKind) {
		return fmt.Errorf("provider kind %q is absent from capabilities", target.Profile.ProviderKind)
	}
	if !slices.Contains(base.Provider.Models, target.Profile.Model) {
		return fmt.Errorf("model %q is absent from capabilities", target.Profile.Model)
	}
	if !base.Provider.SupportsCancel {
		return fmt.Errorf("provider capabilities must support cancellation")
	}
	if target.WorkspaceGovernance.Strict() && (!base.Provider.SupportsPermissions || !base.Provider.SupportsTools) {
		return fmt.Errorf("strict-governed runtime must support permissions and prompt-scoped tools")
	}
	return nil
}

func validateExactStatus(target Target, status *harnessv2.StatusResponse) error {
	if status == nil {
		return fmt.Errorf("runtime returned no authenticated status")
	}
	expectedDigest, err := harnessv2.CanonicalProfileDigest(target.Profile)
	if err != nil {
		return fmt.Errorf("canonicalize expected runtime profile: %w", err)
	}
	if status.Fence.RuntimeInstanceID != target.ExpectedRuntimeInstanceID {
		return fmt.Errorf("authenticated status runtime instance ID %q does not match expected %q", status.Fence.RuntimeInstanceID, target.ExpectedRuntimeInstanceID)
	}
	if status.Fence.RuntimeProfileDigest != expectedDigest {
		return fmt.Errorf("authenticated status profile digest %q does not match expected %q", status.Fence.RuntimeProfileDigest, expectedDigest)
	}
	if status.Fence.ProfileDigestSchemaVersion != harnessv2.ProfileDigestSchemaVersion {
		return fmt.Errorf("authenticated status profile digest schema version %d does not match expected %d", status.Fence.ProfileDigestSchemaVersion, harnessv2.ProfileDigestSchemaVersion)
	}
	if status.Lifecycle != harnessv2.SupervisorLifecycleReady {
		return fmt.Errorf("supervisor lifecycle is %q, want %q", status.Lifecycle, harnessv2.SupervisorLifecycleReady)
	}
	if status.Drain.Requested || !status.Drain.AcceptingNewSessions {
		return fmt.Errorf("external runtime is not accepting new sessions")
	}
	if status.Pressure.ResidentSessions > target.Limits.MaxResidentSessions {
		return fmt.Errorf("resident session pressure %d exceeds limit %d", status.Pressure.ResidentSessions, target.Limits.MaxResidentSessions)
	}
	if status.Pressure.ActivePrompts > target.Limits.MaxConcurrentPrompts {
		return fmt.Errorf("active prompt pressure %d exceeds limit %d", status.Pressure.ActivePrompts, target.Limits.MaxConcurrentPrompts)
	}
	if status.Pressure.PendingPermissions > target.Limits.MaxPendingPermissions {
		return fmt.Errorf("pending permission pressure %d exceeds limit %d", status.Pressure.PendingPermissions, target.Limits.MaxPendingPermissions)
	}
	return nil
}

func getCapabilities(ctx context.Context, client *http.Client, baseURL string) (*CapabilitiesResponse, error) {
	endpoint, err := endpointURL(baseURL, harnessv2.CapabilitiesPath)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("capabilities endpoint returned HTTP %d", resp.StatusCode)
	}
	if err := requireJSON(resp.Header.Get("Content-Type")); err != nil {
		return nil, err
	}
	body, err := readBounded(resp.Body, resp.ContentLength, maxProbeResponseBytes)
	if err != nil {
		return nil, err
	}
	if _, err := harnessv2.CanonicalJSON(body); err != nil {
		return nil, fmt.Errorf("capabilities response is not valid bounded JSON: %w", err)
	}
	var response CapabilitiesResponse
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode capabilities response: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, fmt.Errorf("validate capabilities response: %w", err)
	}
	return &response, nil
}

func probeStatusAuthNegatives(ctx context.Context, client *http.Client, target Target) error {
	if err := expectAuthRejected(ctx, client, target.BaseURL, http.MethodGet, harnessv2.StatusPath, "", "", nil); err != nil {
		return fmt.Errorf("unauthenticated status negative probe: %w", err)
	}
	wrongToken := strings.Repeat("w", 32)
	if wrongToken == target.ControllerBearerToken {
		wrongToken = strings.Repeat("z", 32)
	}
	if err := expectAuthRejected(ctx, client, target.BaseURL, http.MethodGet, harnessv2.StatusPath, wrongToken, "", nil); err != nil {
		return fmt.Errorf("wrong-token status negative probe: %w", err)
	}
	return nil
}

func probeMutationAuthNegatives(ctx context.Context, client *http.Client, target Target) error {
	path := harnessv2.RuntimeSessionsPath + "/conformance-auth-negative"
	body := []byte(`{"protocol":"orka.harness.v2"}`)
	if err := expectAuthRejected(ctx, client, target.BaseURL, http.MethodPut, path, "", "", body); err != nil {
		return fmt.Errorf("unauthenticated mutation negative probe: %w", err)
	}
	wrongToken := strings.Repeat("w", 32)
	if wrongToken == target.ControllerBearerToken {
		wrongToken = strings.Repeat("z", 32)
	}
	if err := expectAuthRejected(ctx, client, target.BaseURL, http.MethodPut, path, wrongToken, "", body); err != nil {
		return fmt.Errorf("wrong-token mutation negative probe: %w", err)
	}
	if err := expectAuthRejected(ctx, client, target.BaseURL, http.MethodPut, path, target.ControllerBearerToken, "", body); err != nil {
		return fmt.Errorf("missing operation-capability negative probe: %w", err)
	}
	if err := expectAuthRejected(ctx, client, target.BaseURL, http.MethodPut, path, target.ControllerBearerToken, "invalid.capability", body); err != nil {
		return fmt.Errorf("invalid operation-capability negative probe: %w", err)
	}
	return nil
}

func expectAuthRejected(
	ctx context.Context,
	client *http.Client,
	baseURL, method, relative, bearer, capability string,
	body []byte,
) error {
	endpoint, err := endpointURL(baseURL, relative)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if len(body) != 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if capability != "" {
		req.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return fmt.Errorf("endpoint returned HTTP %d, want 401 or 403", resp.StatusCode)
	}
	return nil
}

func endpointURL(baseURL, relative string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse base URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("base URL must be absolute HTTP(S)")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" || strings.Contains(parsed.Path, "\\") {
		return "", fmt.Errorf("base URL contains unsupported components")
	}
	if !strings.HasPrefix(relative, "/v2/") || strings.ContainsAny(relative, "?#\\\r\n") || strings.Contains(relative, "//") {
		return "", fmt.Errorf("endpoint path is not a canonical /v2 path")
	}
	prefix := strings.TrimSuffix(parsed.Path, "/")
	if prefix == "/" {
		prefix = ""
	}
	parsed.Path = prefix + relative
	return parsed.String(), nil
}

func requireJSON(header string) error {
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return fmt.Errorf("parse response Content-Type: %w", err)
	}
	if !strings.EqualFold(mediaType, "application/json") {
		return fmt.Errorf("response Content-Type %q is unsupported", mediaType)
	}
	return nil
}

func readBounded(reader io.Reader, contentLength, limit int64) ([]byte, error) {
	if contentLength > limit {
		return nil, fmt.Errorf("response Content-Length %d exceeds limit %d", contentLength, limit)
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds limit %d", limit)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("response body is empty")
	}
	return body, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("JSON response contains a trailing value")
		}
		return fmt.Errorf("JSON response contains trailing data: %w", err)
	}
	return nil
}

func boundedMessage(err error) string {
	if err == nil {
		return ""
	}
	// Repair invalid sequences and truncate on a rune boundary so bounded
	// messages always remain valid UTF-8.
	message := strings.ToValidUTF8(strings.TrimSpace(err.Error()), "�")
	if len(message) <= 1024 {
		return message
	}
	end := 1024
	for end > 0 && !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end]
}
