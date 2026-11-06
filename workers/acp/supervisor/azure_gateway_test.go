package supervisor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const testForwardTool = "azure_arm"

type azureGatewayFixture struct {
	session   *mcpProxySession
	endpoint  string
	env       map[string]string
	home      string
	client    *http.Client
	mu        sync.Mutex
	calls     []harnessv2.MCPBrokerCallRequest
	respond   func(harnessv2.MCPBrokerCallRequest) json.RawMessage
	authorize harnessv2.PromptMCPAuthorization
}

func (f *azureGatewayFixture) brokerCalls() []harnessv2.MCPBrokerCallRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]harnessv2.MCPBrokerCallRequest(nil), f.calls...)
}

func forwardingTestAuthorization(t *testing.T, fence harnessv2.Fence, now time.Time, forwarders int) (harnessv2.PromptMCPAuthorization, harnessv2.PromptLease) {
	t.Helper()
	authorization, lease := buildTestMCPAuthorization(t, fence, now, "lookup", harnessv2.MCPToolEffectReadOnly, false)
	tools := append([]harnessv2.MCPToolDescriptor(nil), authorization.ToolPolicy.Tools...)
	names := append([]string(nil), authorization.ToolPolicy.AllowedToolNames...)
	for index := range forwarders {
		name := testForwardTool
		if index > 0 {
			name += "_second"
		}
		tools = append(tools, harnessv2.MCPToolDescriptor{
			Name: name, Description: "Azure Resource Manager", InputSchema: harnessv2.MCPForwardInputSchema,
			Source: harnessv2.MCPToolSourceBrokeredCustom, Effect: harnessv2.MCPToolEffectReadOnly,
			DefinitionDigest: testDigest("forward-" + name),
			Forward:          &harnessv2.MCPToolForward{Protocol: harnessv2.MCPForwardProtocolAzureResourceManager, Origin: "https://management.azure.com"},
		})
		names = append(names, name)
	}
	slices.SortFunc(tools, func(a, b harnessv2.MCPToolDescriptor) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(names)
	digest, err := harnessv2.CanonicalMCPToolDescriptorDigest(tools)
	if err != nil {
		t.Fatal(err)
	}
	authorization.ToolPolicy.Tools = tools
	authorization.ToolPolicy.AllowedToolNames = names
	authorization.ToolPolicy.DescriptorDigest = digest
	if authorization.ToolPolicyDigest, err = harnessv2.CanonicalRuntimeToolPolicyDigest(names, nil, true); err != nil {
		t.Fatal(err)
	}
	if authorization.MCPConfigurationDigest, err = harnessv2.CanonicalMCPConfigurationDigest(names); err != nil {
		t.Fatal(err)
	}
	return authorization, lease
}

func newAzureGatewayFixture(t *testing.T) *azureGatewayFixture {
	t.Helper()
	fixture := &azureGatewayFixture{}
	broker := MCPBrokerFunc(func(_ context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		fixture.mu.Lock()
		fixture.calls = append(fixture.calls, request)
		respond := fixture.respond
		fixture.mu.Unlock()
		result := json.RawMessage(`{"status":200,"headers":{"Content-Type":"application/json"},"body":"{\"value\":[]}"}`)
		if respond != nil {
			result = respond(request)
		}
		return harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: result}, nil
	})
	proxy, err := newMCPProxy(broker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = proxy.close(ctx)
	})
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot", ControllerEpoch: 2,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3,
		RuntimeProfileDigest:       harnessv2.ProfileDigest(testDigest("profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	now := time.Now().UTC()
	authorization, lease := forwardingTestAuthorization(t, fence, now, 1)
	session, binding, err := proxy.newSession(fence, authorization.Configuration())
	if err != nil {
		t.Fatal(err)
	}
	fixture.session, fixture.endpoint, fixture.authorize = session, binding.URL, authorization
	fixture.home = t.TempDir()
	server := &Server{}
	fixture.env, err = server.attachAzureGateway(session, authorization.Configuration(), fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(fixture.env["REQUESTS_CA_BUNDLE"])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(bundle) {
		t.Fatal("CA bundle holds no certificate")
	}
	fixture.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	if err := session.activate(t.Context(), authorization, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := session.markRunning(authorization.PromptID, now); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *azureGatewayFixture) base() string {
	return strings.TrimSuffix(f.env["IDENTITY_ENDPOINT"], azureIdentityPath)
}

func (f *azureGatewayFixture) do(t *testing.T, method, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	request, err := http.NewRequest(method, f.base()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(response.Body)
	return response, string(body)
}

func (f *azureGatewayFixture) token(t *testing.T) string {
	t.Helper()
	response, body := f.do(t, http.MethodGet, azureIdentityPath+"?api-version=2019-08-01&resource=https%3A%2F%2Fmanagement.core.windows.net%2F",
		http.Header{"X-Identity-Header": {f.env["IDENTITY_HEADER"]}, "Metadata": {"true"}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("identity status = %d: %s", response.StatusCode, body)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresOn   string `json:"expires_on"`
	}
	if err := json.Unmarshal([]byte(body), &token); err != nil || strings.Count(token.AccessToken, ".") != 2 || token.ExpiresOn == "" {
		t.Fatalf("identity token = %q", body)
	}
	return token.AccessToken
}

func TestAzureGatewayForwardsResourceManagerReadsThroughTheBroker(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	fixture.respond = func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		body := `{"value":[{"id":"/subscriptions/s1","tags":{"docs":"https://management.azure.com/keep-me"}}],"nextLink":"https://management.azure.com/subscriptions?api-version=2022-12-01&$skiptoken=abc"}`
		raw, _ := json.Marshal(harnessv2.MCPForwardResponse{
			Status: http.StatusOK, Body: body,
			Headers: map[string]string{
				"Content-Type": "application/json", "Location": "https://management.azure.com/operations/1",
				"Azure-Asyncoperation": "https://elsewhere.example/operations/2",
			},
		})
		return raw
	}
	token := fixture.token(t)
	response, body := fixture.do(t, http.MethodGet, "/subscriptions?api-version=2022-12-01", http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	var page struct {
		Value []struct {
			Tags map[string]string `json:"tags"`
		} `json:"value"`
		NextLink string `json:"nextLink"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if page.NextLink != fixture.base()+"/subscriptions?api-version=2022-12-01&$skiptoken=abc" {
		t.Fatalf("nextLink was not pointed at the gateway: %s", body)
	}
	if len(page.Value) != 1 || page.Value[0].Tags["docs"] != "https://management.azure.com/keep-me" {
		t.Fatalf("resource data was rewritten: %s", body)
	}
	if got := response.Header.Get("Azure-AsyncOperation"); got != "https://elsewhere.example/operations/2" {
		t.Fatalf("a link to another host was rewritten: %q", got)
	}
	if got := response.Header.Get("Location"); got != fixture.base()+"/operations/1" {
		t.Fatalf("Location = %q", got)
	}
	calls := fixture.brokerCalls()
	if len(calls) != 1 || calls[0].Call.ToolName != testForwardTool {
		t.Fatalf("broker calls = %#v", calls)
	}
	forwarded, err := harnessv2.DecodeMCPForwardRequest(calls[0].Call.Arguments)
	if err != nil || forwarded != (harnessv2.MCPForwardRequest{Method: http.MethodGet, Path: "/subscriptions", Query: "api-version=2022-12-01"}) {
		t.Fatalf("forwarded request = %#v, %v", forwarded, err)
	}
	if strings.Contains(string(calls[0].Call.Arguments), token) {
		t.Fatal("the placeholder token reached the broker")
	}
}

func TestAzureGatewayPassesUpstreamErrorsThrough(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	fixture.respond = func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		raw, _ := json.Marshal(harnessv2.MCPForwardResponse{Status: http.StatusForbidden, Body: `{"error":{"code":"AuthorizationFailed"}}`})
		return raw
	}
	token := fixture.token(t)
	response, body := fixture.do(t, http.MethodGet, "/subscriptions/s1/resourcegroups?api-version=1", http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusForbidden || !strings.Contains(body, "AuthorizationFailed") {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
}

func TestAzureGatewayRefusesRequestsBeforeTheBroker(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	token := fixture.token(t)
	bearer := http.Header{"Authorization": {"Bearer " + token}}
	for _, tc := range []struct {
		name, method, path string
		header             http.Header
		status             int
	}{
		{"no token", http.MethodGet, "/subscriptions", nil, http.StatusUnauthorized},
		{"another session's token", http.MethodGet, "/subscriptions", http.Header{"Authorization": {"Bearer a.b.c"}}, http.StatusUnauthorized},
		{"write", http.MethodPut, "/subscriptions/s1/resourcegroups/rg", bearer, http.StatusMethodNotAllowed},
		{"action", http.MethodPost, "/subscriptions/s1/providers/Microsoft.Storage/storageAccounts/a/listKeys", bearer, http.StatusMethodNotAllowed},
		{"encoded separator", http.MethodGet, "/subscriptions/s1%2F..%2Fother", bearer, http.StatusBadRequest},
		{"identity without header", http.MethodGet, azureIdentityPath + "?resource=https%3A%2F%2Fmanagement.azure.com%2F", nil, http.StatusUnauthorized},
		{"identity for another service", http.MethodGet, azureIdentityPath + "?resource=https%3A%2F%2Fgraph.microsoft.com%2F",
			http.Header{"X-Identity-Header": {fixture.env["IDENTITY_HEADER"]}}, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, body := fixture.do(t, tc.method, tc.path, tc.header)
			if response.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.StatusCode, tc.status, body)
			}
		})
	}
	if calls := fixture.brokerCalls(); len(calls) != 0 {
		t.Fatalf("refused requests reached the broker: %d", len(calls))
	}
}

func TestAzureGatewayRefusesOutsideARunningPrompt(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	token := fixture.token(t)
	fixture.session.deactivate(fixture.authorize.PromptID, harnessv2.RuntimeSessionStateIdle)
	response, body := fixture.do(t, http.MethodGet, "/subscriptions", http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusForbidden || !strings.Contains(body, "OrkaNotAuthorized") {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if calls := fixture.brokerCalls(); len(calls) != 0 {
		t.Fatalf("a request outside the prompt reached the broker")
	}
}

func TestAzureGatewayClosesWithTheSession(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	fixture.session.close()
	request, err := http.NewRequest(http.MethodGet, fixture.base()+azureMetadataPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response, err := fixture.client.Do(request); err == nil {
		_ = response.Body.Close()
		t.Fatal("the gateway still answers after its session closed")
	}
}

func TestAzureGatewayWritesTheCloudConfiguration(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	dir := filepath.Join(fixture.home, ".azure")
	if fixture.env["AZURE_CONFIG_DIR"] != dir {
		t.Fatalf("AZURE_CONFIG_DIR = %q", fixture.env["AZURE_CONFIG_DIR"])
	}
	config, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil || !strings.Contains(string(config), "name = "+azureCloudName) {
		t.Fatalf("config = %q, %v", config, err)
	}
	clouds, err := os.ReadFile(filepath.Join(dir, "clouds.config"))
	if err != nil || !strings.Contains(string(clouds), "endpoint_resource_manager = "+fixture.base()+"/") {
		t.Fatalf("clouds.config = %q, %v", clouds, err)
	}
	response, body := fixture.do(t, http.MethodGet, azureMetadataPath+"?api-version=2022-09-01", nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, fixture.base()+"/") {
		t.Fatalf("metadata = %d: %s", response.StatusCode, body)
	}
}

func TestForwardingToolIsHiddenFromTheModel(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	for _, tool := range fixture.session.listTools(time.Now()) {
		if tool[protocolNameField] == testForwardTool {
			t.Fatal("the forwarding Tool was listed to the model")
		}
	}
	session := fixture.session
	session.mu.Lock()
	session.credential = []byte("credential")
	session.mu.Unlock()
	response := doMCPRequest(t, fixture.endpoint, "credential",
		`{"jsonrpc":"2.0","id":"call-1","method":"tools/call","params":{"name":"`+testForwardTool+`","arguments":{"method":"GET","path":"/subscriptions"}}}`)
	decoded := decodeMCPResponse(t, response)
	if decoded.Error == nil || decoded.Error.Code != -32001 {
		t.Fatalf("MCP call to the forwarding Tool = %#v", decoded)
	}
	if calls := fixture.brokerCalls(); len(calls) != 0 {
		t.Fatal("the model reached the forwarding Tool")
	}
}

func TestAzureForwardingToolRefusesTwo(t *testing.T) {
	fence := harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 1}
	authorization, _ := forwardingTestAuthorization(t, fence, time.Now().UTC(), 2)
	if _, _, err := azureForwardingTool(authorization.Configuration()); err == nil {
		t.Fatal("two Azure forwarding Tools were accepted")
	}
}

func TestAzureGatewayHeadReportsTheUpstreamLength(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	fixture.respond = func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		raw, _ := json.Marshal(harnessv2.MCPForwardResponse{Status: http.StatusOK, Headers: map[string]string{"Content-Length": "1234"}})
		return raw
	}
	token := fixture.token(t)
	response, _ := fixture.do(t, http.MethodHead, "/subscriptions/s1/resourcegroups/rg1", http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusOK || response.ContentLength != 1234 {
		t.Fatalf("HEAD status = %d, length = %d", response.StatusCode, response.ContentLength)
	}
}
