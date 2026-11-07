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
		{"put", MCPForwardRequest{Method: "PUT", Path: "/subscriptions/s1/resourcegroups/rg1", Body: json.RawMessage(`{"location":"eastus"}`)}, true},
		{"post array", MCPForwardRequest{Method: "POST", Path: "/subscriptions/s1/providers/x/batch", Body: json.RawMessage(`[{"id":1}]`)}, true},
		{"post without body", MCPForwardRequest{Method: "POST", Path: "/subscriptions/s1/providers/x/listCredential"}, true},
		{"delete", MCPForwardRequest{Method: "DELETE", Path: "/subscriptions/s1/resourcegroups/rg1"}, true},
		{"trace", MCPForwardRequest{Method: "TRACE", Path: "/subscriptions"}, false},
		{"read with body", MCPForwardRequest{Method: "GET", Path: "/subscriptions", Body: json.RawMessage(`{}`)}, false},
		{"write with non-JSON body", MCPForwardRequest{Method: "PUT", Path: "/subscriptions", Body: json.RawMessage("location=eastus")}, false},
		{"write with string body", MCPForwardRequest{Method: "PUT", Path: "/subscriptions", Body: json.RawMessage(`"{\"adminPassword\":\"x\"}"`)}, false},
		{"write with null body", MCPForwardRequest{Method: "PUT", Path: "/subscriptions", Body: json.RawMessage(`null`)}, false},
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
		"https://management.azure.com:443":  "https://management.azure.com",
		"https://management.azure.com:8443": "https://management.azure.com:8443",
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
	if _, err := DecodeMCPForwardRequest(json.RawMessage(`{"method":"GET","path":"/subscriptions","headers":"x"}`)); err == nil {
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

// TestForwardRequestsGoToTheToolForTheirMethod covers the split between a
// forwarding Tool, which carries reads, and its write tool, which carries
// writes: neither accepts the other's methods.
func TestForwardRequestsGoToTheToolForTheirMethod(t *testing.T) {
	read := MCPToolForward{Protocol: MCPForwardProtocolAzureResourceManager, Origin: "https://management.azure.com"}
	write := read
	write.Write = true
	get := MCPForwardRequest{Method: "GET", Path: "/subscriptions"}
	put := MCPForwardRequest{Method: "PUT", Path: "/subscriptions/s1/resourcegroups/rg1", Body: json.RawMessage(`{"location":"eastus"}`)}
	if err := get.ValidateFor(read); err != nil {
		t.Fatalf("a read on the read tool: %v", err)
	}
	if err := put.ValidateFor(write); err != nil {
		t.Fatalf("a write on the write tool: %v", err)
	}
	if err := put.ValidateFor(read); err == nil {
		t.Fatal("a write on the read tool must be refused")
	}
	if err := get.ValidateFor(write); err == nil {
		t.Fatal("a read on the write tool must be refused")
	}
	if MCPForwardWriteToolName("azure-arm") != "azure-arm:write" || MCPForwardToolResource("azure-arm:write") != "azure-arm" ||
		MCPForwardToolResource("azure-arm") != "azure-arm" {
		t.Fatal("the write tool is named after its Tool")
	}
}

// TestForwardingWriteDescriptorMustBeConsequential covers a write tool's
// descriptor: consequential, named after its Tool, with the write schema.
func TestForwardingWriteDescriptorMustBeConsequential(t *testing.T) {
	forward := &MCPToolForward{Protocol: MCPForwardProtocolAzureResourceManager, Origin: "https://management.azure.com", Write: true}
	descriptor := MCPToolDescriptor{
		Name: MCPForwardWriteToolName("azure_arm"), Description: "Azure", InputSchema: forward.InputSchema(),
		Source: MCPToolSourceBrokeredCustom, Effect: MCPToolEffectConsequential, DefinitionDigest: "sha256:" + strings.Repeat("a", 64),
		Forward: forward,
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*MCPToolDescriptor){
		"read only":   func(d *MCPToolDescriptor) { d.Effect = MCPToolEffectReadOnly },
		"read schema": func(d *MCPToolDescriptor) { d.InputSchema = MCPForwardInputSchema },
		"read name":   func(d *MCPToolDescriptor) { d.Name = "azure_arm" },
		"read forward": func(d *MCPToolDescriptor) {
			read := *forward
			read.Write = false
			d.Forward = &read
		},
	} {
		copied := descriptor
		mutate(&copied)
		if copied.Validate() == nil {
			t.Errorf("%s: descriptor was accepted", name)
		}
	}
}

// TestOnlyForwardingWritesSkipRuntimeApprovals covers which approval-required
// tools need a runtime that waits for brokered approvals: every tool but a
// forwarding write tool, whose client forwarder waits instead.
func TestOnlyForwardingWritesSkipRuntimeApprovals(t *testing.T) {
	write := MCPToolDescriptor{Name: "azure_arm:write", Forward: &MCPToolForward{Write: true}}
	read := MCPToolDescriptor{Name: "azure_arm", Forward: &MCPToolForward{}}
	plain := MCPToolDescriptor{Name: "gh_comment"}
	tools := MCPToolPolicy{Tools: []MCPToolDescriptor{read, write, plain}}
	for name, tc := range map[string]struct {
		required []string
		want     bool
	}{
		"none":            {nil, false},
		"forwarded write": {[]string{"azure_arm:write"}, false},
		"forwarded read":  {[]string{"azure_arm"}, true},
		"other tool":      {[]string{"azure_arm:write", "gh_comment"}, true},
		"unknown tool":    {[]string{"other:write"}, true},
	} {
		if got := (MCPApprovalPolicy{RequiredTools: tc.required}).NeedsRuntimeApprovals(tools); got != tc.want {
			t.Errorf("%s: NeedsRuntimeApprovals = %t, want %t", name, got, tc.want)
		}
	}
}
