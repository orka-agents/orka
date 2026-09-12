package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

type MCPBroker interface {
	Call(context.Context, harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error)
}

type MCPBrokerFunc func(context.Context, harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error)

func (f MCPBrokerFunc) Call(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
	return f(ctx, request)
}

type controllerMCPBrokerClient struct {
	endpoint         *url.URL
	client           *http.Client
	namespace        string
	controllerBearer string
	capabilitySecret []byte
}

func NewControllerMCPBrokerClient(baseURL, namespace, bearer string, capabilitySecret []byte) (MCPBroker, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("MCP broker URL is invalid")
	}
	parsed.Path = harnessv2.MCPBrokerCallPath
	namespace = strings.TrimSpace(namespace)
	if namespace == "" || len(strings.TrimSpace(bearer)) < 32 || len(capabilitySecret) < harnessv2.MinCapabilitySecretBytes {
		return nil, fmt.Errorf("MCP broker identity is invalid")
	}
	transport := &http.Transport{
		// Broker requests target the in-cluster controller endpoint with the
		// controller bearer and operation capability in headers; environment
		// proxies must never carry them.
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           100,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &controllerMCPBrokerClient{
		endpoint: parsed,
		client: &http.Client{
			Transport: transport,
			// The broker does not write response headers until the authorized tool
			// finishes. A client-wide timeout would therefore override longer tool
			// deadlines such as wait_for_tasks' 10-minute default. Request-context
			// cancellation and its deadline remain authoritative for the complete
			// call; the transport independently bounds connection setup.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		namespace: namespace, controllerBearer: bearer,
		capabilitySecret: append([]byte(nil), capabilitySecret...),
	}, nil
}

func (c *controllerMCPBrokerClient) Call(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
	request.Namespace = c.namespace
	digest, err := harnessv2.CanonicalRequestDigest(request)
	if err != nil {
		return harnessv2.MCPBrokerCallResponse{}, err
	}
	if request.Metadata.RequestDigest != "" && request.Metadata.RequestDigest != digest {
		return harnessv2.MCPBrokerCallResponse{}, fmt.Errorf("MCP broker request digest is already sealed differently")
	}
	request.Metadata.RequestDigest = digest
	if _, err := request.ValidateAt(time.Now().UTC()); err != nil {
		return harnessv2.MCPBrokerCallResponse{}, err
	}
	capability, err := harnessv2.SignOperationCapability(c.capabilitySecret, harnessv2.ClaimsForMutation(request.Metadata))
	if err != nil {
		return harnessv2.MCPBrokerCallResponse{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return harnessv2.MCPBrokerCallResponse{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return harnessv2.MCPBrokerCallResponse{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", "Bearer "+c.controllerBearer)
	httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	// Carry the non-secret pool identity so the broker can verify the bearer
	// before reading the body.
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolNamespaceHeader, request.Namespace)
	httpRequest.Header.Set(harnessv2.MCPBrokerPoolUIDHeader, string(request.Metadata.Fence.RuntimePoolUID))
	response, err := c.client.Do(httpRequest)
	if err != nil {
		return harnessv2.MCPBrokerCallResponse{}, fmt.Errorf("MCP broker transport failed")
	}
	defer response.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(harnessv2.MaxMCPResultBytes+(64<<10))))
	if err != nil || response.StatusCode != http.StatusOK {
		return harnessv2.MCPBrokerCallResponse{}, fmt.Errorf("MCP broker rejected the tool call")
	}
	var decoded harnessv2.MCPBrokerCallResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil || decoded.CallID != request.Call.CallID {
		return harnessv2.MCPBrokerCallResponse{}, fmt.Errorf("MCP broker response is invalid")
	}
	if err := decoded.Validate(); err != nil {
		return harnessv2.MCPBrokerCallResponse{}, fmt.Errorf("MCP broker response is invalid")
	}
	return decoded, nil
}
