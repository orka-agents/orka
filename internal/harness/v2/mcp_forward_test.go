package v2

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMCPForwardRequestStaysOnTheOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request MCPForwardRequest
		valid   bool
	}{
		{"list", MCPForwardRequest{Method: "GET", Path: "/subscriptions", Query: "api-version=2022-12-01"}, true},
		{"head", MCPForwardRequest{Method: "HEAD", Path: "/subscriptions/s1/resourcegroups/rg1"}, true},
		{"encoded space", MCPForwardRequest{Method: "GET", Path: "/subscriptions/s1/resourceGroups/my%20rg"}, true},
		{"skiptoken", MCPForwardRequest{Method: "GET", Path: "/subscriptions", Query: "api-version=1&$skiptoken=a%3D%3D"}, true},
		{"put", MCPForwardRequest{Method: "PUT", Path: "/subscriptions"}, false},
		{"post", MCPForwardRequest{Method: "POST", Path: "/subscriptions"}, false},
		{"relative", MCPForwardRequest{Method: "GET", Path: "subscriptions"}, false},
		{"scheme relative", MCPForwardRequest{Method: "GET", Path: "//evil.example/x"}, false},
		{"dot dot", MCPForwardRequest{Method: "GET", Path: "/subscriptions/../tenants"}, false},
		{"dot", MCPForwardRequest{Method: "GET", Path: "/subscriptions/./x"}, false},
		{"encoded slash", MCPForwardRequest{Method: "GET", Path: "/subscriptions%2F..%2Fx"}, false},
		{"encoded dot", MCPForwardRequest{Method: "GET", Path: "/subscriptions/%2e%2e/x"}, false},
		{"backslash", MCPForwardRequest{Method: "GET", Path: "/subscriptions\\x"}, false},
		{"empty segment", MCPForwardRequest{Method: "GET", Path: "/subscriptions//x"}, false},
		{"fragment", MCPForwardRequest{Method: "GET", Path: "/subscriptions#x"}, false},
		{"query in path", MCPForwardRequest{Method: "GET", Path: "/subscriptions?x=1"}, false},
		{"control", MCPForwardRequest{Method: "GET", Path: "/subscriptions/%0a"}, false},
		{"bad escape", MCPForwardRequest{Method: "GET", Path: "/subscriptions/%zz"}, false},
		{"query fragment", MCPForwardRequest{Method: "GET", Path: "/subscriptions", Query: "a=1#b"}, false},
		{"query space", MCPForwardRequest{Method: "GET", Path: "/subscriptions", Query: "a=1 b"}, false},
		{"long path", MCPForwardRequest{Method: "GET", Path: "/" + strings.Repeat("a", 5000)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target, err := tc.request.Target("https://management.azure.com")
			if (err == nil) != tc.valid {
				t.Fatalf("Target() error = %v, valid = %v", err, tc.valid)
			}
			if err == nil && target.Host != "management.azure.com" {
				t.Fatalf("Target() host = %q", target.Host)
			}
		})
	}
}

func TestForwardOriginAcceptsOnlyHTTPSOrigins(t *testing.T) {
	for raw, want := range map[string]string{
		"https://management.azure.com":      "https://management.azure.com",
		"https://Management.Azure.com/":     "https://management.azure.com",
		"http://management.azure.com":       "",
		"https://management.azure.com/x":    "",
		"https://user@management.azure.com": "",
		"https://management.azure.com?x=1":  "",
		"https://{{host}}":                  "",
	} {
		got, err := ForwardOrigin(raw)
		if (want == "") != (err != nil) || got != want {
			t.Errorf("ForwardOrigin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestDecodeMCPForwardRequestIsStrict(t *testing.T) {
	if _, err := DecodeMCPForwardRequest(json.RawMessage(`{"method":"GET","path":"/subscriptions","body":"x"}`)); err == nil {
		t.Fatal("an unknown argument was accepted")
	}
	if _, err := DecodeMCPForwardRequest(json.RawMessage(`{"method":"GET","path":"/subscriptions"} {}`)); err == nil {
		t.Fatal("trailing data was accepted")
	}
}

func TestMCPForwardResponseAllowsOnlyListedHeaders(t *testing.T) {
	if err := (MCPForwardResponse{Status: 200, Headers: map[string]string{"Location": "https://x"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, response := range []MCPForwardResponse{
		{Status: 200, Headers: map[string]string{"Set-Cookie": "a"}},
		{Status: 200, Headers: map[string]string{"Location": "a\r\nb"}},
		{Status: 99},
		{Status: 200, Body: string([]byte{0xff})},
	} {
		if response.Validate() == nil {
			t.Errorf("response %#v was accepted", response)
		}
	}
}

func TestForwardingDescriptorMustBeAReadOnlyCustomTool(t *testing.T) {
	descriptor := MCPToolDescriptor{
		Name: "azure_arm", Description: "Azure", InputSchema: MCPForwardInputSchema,
		Source: MCPToolSourceBrokeredCustom, Effect: MCPToolEffectReadOnly, DefinitionDigest: "sha256:" + strings.Repeat("a", 64),
		Forward: &MCPToolForward{Protocol: MCPForwardProtocolAzureResourceManager, Origin: "https://management.azure.com"},
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*MCPToolDescriptor){
		"consequential": func(d *MCPToolDescriptor) { d.Effect = MCPToolEffectConsequential },
		"builtin":       func(d *MCPToolDescriptor) { d.Source = MCPToolSourceBrokeredBuiltin; d.DefinitionDigest = "" },
		"schema":        func(d *MCPToolDescriptor) { d.InputSchema = json.RawMessage(`{"type":"object"}`) },
		"protocol": func(d *MCPToolDescriptor) {
			d.Forward = &MCPToolForward{Protocol: "Other", Origin: "https://x.example"}
		},
		"origin": func(d *MCPToolDescriptor) {
			d.Forward = &MCPToolForward{Protocol: MCPForwardProtocolAzureResourceManager, Origin: "https://x.example/p"}
		},
	} {
		copied := descriptor
		mutate(&copied)
		if copied.Validate() == nil {
			t.Errorf("%s: descriptor was accepted", name)
		}
	}
}
