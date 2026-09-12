package aitools

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRemoteMCPSelectionCeiling(t *testing.T) {
	for _, tc := range []string{"selected", "no Agent", "no reference", "disabled", "Task expansion", "runtime Agent", "wrong Agent", "wrong namespace", "non-native Task", "legacy expansion"} {
		t.Run(tc, func(t *testing.T) {
			tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "team"}, Spec: corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{Remote: &corev1alpha1.RemoteMCPServer{URL: "https://example.com/mcp", ToolName: "read"}}}}
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: "team"}, Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: "operator"}, AI: &corev1alpha1.AISpec{Tools: []string{"remote", "legacy"}}}}
			agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "team"}, Spec: corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: "remote"}}}}
			switch tc {
			case "no Agent":
				agent = nil
			case "no reference":
				task.Spec.AgentRef = nil
			case "disabled":
				disabled := false
				agent.Spec.Tools[0].Enabled = &disabled
			case "Task expansion":
				agent.Spec.Tools = nil
			case "runtime Agent":
				agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{}
			case "wrong Agent":
				agent.Name = "other"
			case "wrong namespace":
				tool.Namespace = "other"
			case "non-native Task":
				task.Spec.Type = corev1alpha1.TaskTypeAgent
			case "legacy expansion":
				tool.Spec.MCP = nil
				agent = nil
				task.Spec.AgentRef = nil
			}
			err := ValidateRemoteMCPSelection(task, agent, tool)
			wantErr := tc != "selected" && tc != "legacy expansion"
			if (err != nil) != wantErr {
				t.Fatalf("selection error %v; want error %t", err, wantErr)
			}
		})
	}
}

func TestRemoteMCPSelectionAgentNamespace(t *testing.T) {
	for _, namespace := range []string{"", "team", "other"} {
		t.Run(namespace, func(t *testing.T) {
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "team"},
				Spec: corev1alpha1.TaskSpec{
					Type: corev1alpha1.TaskTypeAI, AgentRef: &corev1alpha1.AgentReference{Name: "operator", Namespace: namespace},
				},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "operator", Namespace: "team"},
				Spec:       corev1alpha1.AgentSpec{Tools: []corev1alpha1.ToolReference{{Name: "remote"}}},
			}
			if namespace != "" {
				agent.Namespace = namespace
			}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "remote", Namespace: "team"},
				Spec:       corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{Remote: &corev1alpha1.RemoteMCPServer{}}},
			}
			err := ValidateRemoteMCPSelection(task, agent, tool)
			if namespace == "other" {
				if err == nil || err.Error() != "remote MCP requires the Agent and Task to share a namespace" {
					t.Fatal("foreign Agent did not fail with the remote namespace support boundary")
				}
			} else if err != nil {
				t.Fatalf("same-namespace Agent selection failed: %v", err)
			}
		})
	}
}

func TestRemoteMCPParametersNumericBudget(t *testing.T) {
	for _, tc := range []struct {
		name, constraints string
		valid             bool
	}{
		{"boundary", `"maximum":1e-65535`, true},
		{"beyond boundary", `"maximum":1e-65536`, false},
		{"aggregate boundary", `"minimum":1e-32767,"maximum":1e-32767`, true},
		{"aggregate beyond boundary", `"minimum":1e-32767,"maximum":1e-32768`, false},
		{"exponent integer overflow", `"maximum":1e-999999999999999999999999999999999999`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"type":"object","properties":{"value":{` + tc.constraints + `}}}`)
			_, err := ResolveRemoteMCPParameters(&apiextensionsv1.JSON{Raw: raw})
			if (err == nil) != tc.valid {
				t.Fatalf("schema budget acceptance = %t; want %t", err == nil, tc.valid)
			}
			if !tc.valid && (err == nil || !strings.Contains(err.Error(), "remote MCP numeric expansion exceeds limit")) {
				t.Error("schema budget failure lost its safe category")
			}
		})
	}
}

func TestRemoteMCPParametersRejectUnrepresentableNumericConstraints(t *testing.T) {
	for name, raw := range map[string]string{
		"exponent":    `{"type":"object","properties":{"value":{"maximum":1e1000001}}}`,
		"cardinality": `{"type":"object","minProperties":18446744073709551616}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveRemoteMCPParameters(&apiextensionsv1.JSON{Raw: []byte(raw)}); err == nil {
				t.Fatal("unrepresentable numeric constraint was silently accepted")
			}
		})
	}
}

func TestRemoteMCPParametersReferencesPreserveTypedSchemaLocations(t *testing.T) {
	for _, tc := range []struct {
		name, raw         string
		valid, emptyValid bool
	}{
		{
			name: "referenced extra cardinality overflow",
			raw:  `{"type":"object","$ref":"#/x","x":{"minProperties":18446744073709551616}}`,
		},
		{
			name: "known defs cardinality overflow",
			raw:  `{"type":"object","$ref":"#/$defs/x","$defs":{"x":{"minProperties":18446744073709551616}}}`,
		},
		{
			name:  "known defs enforces cardinality",
			raw:   `{"type":"object","$ref":"#/$defs/x","$defs":{"x":{"minProperties":1}}}`,
			valid: true,
		},
		{
			name:  "unreferenced extra remains metadata",
			raw:   `{"type":"object","x":{"minProperties":18446744073709551616}}`,
			valid: true, emptyValid: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema, err := ResolveRemoteMCPParameters(&apiextensionsv1.JSON{Raw: []byte(tc.raw)})
			if (err == nil) != tc.valid {
				t.Fatalf("referenced schema acceptance = %t; want %t", err == nil, tc.valid)
			}
			if tc.valid {
				if (schema.Validate(map[string]any{}) == nil) != tc.emptyValid {
					t.Error("referenced cardinality constraint was not enforced")
				}
				if schema.Validate(map[string]any{"name": "fixture"}) != nil {
					t.Error("valid instance failed the referenced schema")
				}
			}
		})
	}
}

func TestRemoteMCPParametersNeverLoadExternalResources(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	fileURL := (&url.URL{Scheme: "file", Path: path}).String()
	for name, raw := range map[string]string{
		"HTTP ref":             `{"type":"object","$ref":"` + server.URL + `/schema"}`,
		"file ref":             `{"type":"object","$ref":"` + fileURL + `"}`,
		"HTTP metaschema":      `{"type":"object","$schema":"` + server.URL + `/schema"}`,
		"unresolved local ref": `{"type":"object","$ref":"#/$defs/missing"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveRemoteMCPParameters(&apiextensionsv1.JSON{Raw: []byte(raw)}); err == nil {
				t.Fatal("external or unresolved schema reference was accepted")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("schema compilation fetched an external resource")
	}
}

func TestRemoteMCPParametersRetainLocalReferenceDialects(t *testing.T) {
	for name, raw := range map[string]string{
		"draft7":    `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","definitions":{"name":{"type":"string"}},"properties":{"name":{"$ref":"#/definitions/name"}}}`,
		"draft2020": `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","$defs":{"name":{"$anchor":"name","type":"string"}},"properties":{"name":{"$ref":"#name"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			schema, err := ResolveRemoteMCPParameters(&apiextensionsv1.JSON{Raw: []byte(raw)})
			if err != nil {
				t.Fatal("local schema references did not compile")
			}
			if schema.Validate(map[string]any{"name": "fixture"}) != nil || schema.Validate(map[string]any{"name": false}) == nil {
				t.Fatal("local reference did not enforce its schema")
			}
		})
	}
}

func TestRemoteMCPURLCredentialQueries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"awsaccesskeyid", false}, {"googleaccessid", false}, {"key-pair-id", false},
		{"sig", false}, {"signature", false}, {"x-amz-credential", false},
		{"x-amz-security-token", false}, {"x-amz-signature", false},
		{"x-goog-credential", false}, {"x-goog-signature", false}, {"x-ms-signature", false},
		{"next_token", false}, {"customTokenHint", false}, {"api_key", false},
		{"route", true}, {"tenant", true}, {"signature-version", true},
	} {
		for _, name := range []string{tc.name, strings.ToUpper(tc.name), " " + strings.ReplaceAll(strings.ToUpper(tc.name), "-", "_") + " "} {
			t.Run(name, func(t *testing.T) {
				tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{
					Parameters: &apiextensionsv1.JSON{Raw: []byte(`{"type":"object"}`)},
					MCP: &corev1alpha1.MCPToolServer{Remote: &corev1alpha1.RemoteMCPServer{
						URL: "https://example.com/mcp?route=health&" + url.QueryEscape(name) + "=fixture-value", ToolName: "read",
					}},
					HTTP: &corev1alpha1.HTTPExecution{
						AuthSecretRef:           &corev1alpha1.SecretKeySelector{Name: "auth", Key: "token"},
						OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "egress"},
					},
				}}
				err := ValidateRemoteMCPConfiguration(tool)
				if (err == nil) != tc.valid {
					t.Fatalf("query acceptance = %t; want %t", err == nil, tc.valid)
				}
				if err != nil && err.Error() != "remote MCP URL must not contain credential query parameters" {
					t.Fatal("query rejection lost its safe category")
				}
			})
		}
	}
}

func TestRemoteMCPHeadersAllowMixedCaseWithoutAuthorityOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{"X-Request-ID", true}, {"x-request-id", true}, {"X-ReQuEsT-ID", true}, {"X-Tenant", true}, {"x-TeNaNt", true},
		{"Signature-Version", true}, {"Sig", true}, {"X-Amz-Signature", true}, {"Key-Pair-Id", true},
		{"X-Api-Key", false}, {"api_key", false}, {"API-KEY", false}, {"ApiKey", false}, {"X_API_KEY", false},
		{"X-Auth-Token", false}, {"X-Access-Token", false}, {"X_AUTH_TOKEN", false},
		{"X-Client-Secret", false}, {"X-Password", false}, {"X-Credential", false}, {"X-Authorization", false},
		{" X-Request-ID", false}, {"X-Request-ID ", false},
		{"AuThOrIzAtIoN", false}, {"TxN-ToKeN", false}, {"McP-Session-ID", false}, {"PrOxY-Authorization", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &corev1alpha1.HTTPExecution{
				AuthSecretRef:           &corev1alpha1.SecretKeySelector{Name: "auth", Key: "token"},
				OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "egress"},
				Headers:                 map[string]string{tc.name: "fixture-value"},
			}
			if err := validateRemoteMCPHTTP(h); (err == nil) != tc.valid {
				t.Fatalf("header acceptance = %t; want %t", err == nil, tc.valid)
			}
		})
	}
}

func TestRemoteMCPRequiresReviewedObjectSchemaAtConsumers(t *testing.T) {
	for _, raw := range []string{"", `null`, `true`, `[]`, `{}`, `{"type":"string"}`, `{"type":"object","properties":[]}`} {
		t.Run(raw, func(t *testing.T) {
			tool := &corev1alpha1.Tool{Spec: corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{Remote: &corev1alpha1.RemoteMCPServer{URL: "https://example.com/mcp", ToolName: "read"}}, HTTP: &corev1alpha1.HTTPExecution{AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "auth", Key: "token"}, OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: "egress"}}}}
			if raw != "" {
				tool.Spec.Parameters = &apiextensionsv1.JSON{Raw: []byte(raw)}
			}
			if err := ValidateRemoteMCPConfiguration(tool); err == nil {
				t.Fatal("accepted missing or malformed reviewed schema")
			}
		})
	}
}
