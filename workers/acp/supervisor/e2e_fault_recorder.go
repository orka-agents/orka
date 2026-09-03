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

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type E2EPromptWriteFaultRecorder interface {
	Consume(context.Context, harnessv2.MutationMetadata) (bool, error)
}

type controllerE2EPromptWriteFaultRecorder struct {
	endpoint         *url.URL
	client           *http.Client
	namespace        string
	controllerBearer string
	capabilitySecret []byte
}

func NewControllerE2EPromptWriteFaultRecorder(
	baseURL, namespace, bearer string,
	capabilitySecret []byte,
) (E2EPromptWriteFaultRecorder, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != providerProxyScheme && parsed.Scheme != providerProxyTLSScheme) || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("E2E prompt write fault recorder URL is invalid")
	}
	parsed.Path = harnessv2.E2EPromptWriteAmbiguityRecordPath
	namespace = strings.TrimSpace(namespace)
	if namespace == "" || len(strings.TrimSpace(bearer)) < 32 || len(capabilitySecret) < harnessv2.MinCapabilitySecretBytes {
		return nil, fmt.Errorf("E2E prompt write fault recorder identity is invalid")
	}
	return &controllerE2EPromptWriteFaultRecorder{
		endpoint: parsed,
		client: &http.Client{
			Timeout:       10 * time.Second,
			Transport:     harnessv2.NewProxylessTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		namespace: namespace, controllerBearer: bearer,
		capabilitySecret: append([]byte(nil), capabilitySecret...),
	}, nil
}

func (r *controllerE2EPromptWriteFaultRecorder) Consume(
	ctx context.Context,
	metadata harnessv2.MutationMetadata,
) (bool, error) {
	promptOperationID := metadata.OperationID
	metadata.OperationID = "record-e2e-prompt-write-ambiguity"
	metadata.ExpiresAt = time.Now().UTC().Add(30 * time.Second)
	metadata.RequestDigest = ""
	request := harnessv2.E2EPromptWriteAmbiguityRecordRequest{
		Namespace: r.namespace, Metadata: metadata, PromptOperationID: promptOperationID,
	}
	digest, err := harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		return false, err
	}
	request.Metadata.RequestDigest = digest
	if err := request.ValidateAt(time.Now().UTC()); err != nil {
		return false, err
	}
	capability, err := harnessv2.SignOperationCapability(r.capabilitySecret, harnessv2.ClaimsForMutation(request.Metadata))
	if err != nil {
		return false, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+r.controllerBearer)
	httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolNamespaceHeader, r.namespace)
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolUIDHeader, string(request.Metadata.Fence.RuntimePoolUID))
	response, err := r.client.Do(httpRequest)
	if err != nil {
		return false, fmt.Errorf("E2E prompt write fault recorder transport failed")
	}
	defer response.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	if err != nil || response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("E2E prompt write fault recorder rejected request")
	}
	var decoded harnessv2.E2EPromptWriteAmbiguityRecordResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return false, fmt.Errorf("E2E prompt write fault recorder response is invalid")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return false, fmt.Errorf("E2E prompt write fault recorder response is invalid")
	}
	return decoded.Inject, nil
}
