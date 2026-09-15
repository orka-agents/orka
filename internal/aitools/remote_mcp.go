package aitools

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"

	googlejsonschema "github.com/google/jsonschema-go/jsonschema"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/santhosh-tekuri/jsonschema/v6"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// IsRemoteMCP reports whether the Tool selects an independently operated server.
func IsRemoteMCP(tool *corev1alpha1.Tool) bool {
	return tool != nil && tool.Spec.MCP != nil && tool.Spec.MCP.Remote != nil
}

var remoteMCPName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// ValidateRemoteMCPAgentReference checks the remote-only Task support boundary
// before consumers read the Agent. It is independent of namespace isolation:
// the initial backend does not grant workers cross-namespace Agent access.
func ValidateRemoteMCPAgentReference(task *corev1alpha1.Task) error {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAI || task.Spec.AgentRef == nil {
		return errors.New("remote MCP requires a referenced native Agent")
	}
	if namespace := task.Spec.AgentRef.Namespace; namespace != "" && namespace != task.Namespace {
		return errors.New("remote MCP requires the Agent and Task to share a namespace")
	}
	return nil
}

// ValidateRemoteMCPSelection enforces the native Agent's remote-only ceiling.
// Resolve deliberately retains its historical additive behavior for other Tools.
func ValidateRemoteMCPSelection(task *corev1alpha1.Task, agent *corev1alpha1.Agent, tool *corev1alpha1.Tool) error {
	if !IsRemoteMCP(tool) {
		return nil
	}
	if err := ValidateRemoteMCPAgentReference(task); err != nil {
		return err
	}
	if agent == nil || agent.Spec.Runtime != nil {
		return errors.New("remote MCP requires a referenced native Agent")
	}
	if agent.Name != task.Spec.AgentRef.Name || agent.Namespace != task.Namespace || tool.Namespace != task.Namespace || !agent.DeletionTimestamp.IsZero() {
		return errors.New("remote MCP Agent or Tool identity does not match the Task")
	}
	for _, ref := range agent.Spec.Tools {
		if strings.TrimSpace(ref.Name) == tool.Name && (ref.Enabled == nil || *ref.Enabled) {
			return nil
		}
	}
	return errors.New("remote MCP Tool is not enabled by the referenced Agent")
}

// ValidateRemoteMCPConfiguration applies the remote-only contract at consumers,
// including callers using clients that do not perform Kubernetes admission.
func ValidateRemoteMCPConfiguration(tool *corev1alpha1.Tool) error {
	if !IsRemoteMCP(tool) {
		return errors.New("remote MCP configuration is required")
	}
	mcp := tool.Spec.MCP
	if mcp.SubstrateActor != nil || mcp.Workspace != nil || mcp.Path != "" {
		return errors.New("remote MCP cannot use managed hosting or mcp.path")
	}
	if !remoteMCPName.MatchString(mcp.Remote.ToolName) {
		return errors.New("remote MCP toolName is invalid")
	}
	if err := validateRemoteMCPEndpoint(mcp.Remote.URL); err != nil {
		return err
	}
	if _, err := ResolveRemoteMCPParameters(tool.Spec.Parameters); err != nil {
		return err
	}
	return validateRemoteMCPHTTP(tool.Spec.HTTP)
}

func validateRemoteMCPEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || strings.ContainsAny(raw, "{}#") || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("remote MCP requires a fixed HTTP URL without credentials or interpolation")
	}
	// Endpoint queries may select a route, but must never carry credentials.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("remote MCP URL query is invalid")
	}
	for key := range query {
		if remoteMCPURLCredentialName(key) {
			return errors.New("remote MCP URL must not contain credential query parameters")
		}
	}
	return nil
}

// remoteMCPURLCredentialName retains the stricter remote token checks and the
// controller's sensitiveURLParameter exact signed-query denylist. Signed-query
// names stay URL-specific so this does not change custom-header admission.
func remoteMCPURLCredentialName(name string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	switch normalized {
	case "awsaccesskeyid", "googleaccessid", "key-pair-id", "sig", "signature",
		"x-amz-credential", "x-amz-security-token", "x-amz-signature",
		"x-goog-credential", "x-goog-signature", "x-ms-signature":
		return true
	}
	return remoteMCPCredentialName(name)
}

// remoteMCPCredentialName conservatively rejects common credential names, not
// arbitrary secret values. Keep the remote-only header admission rules in sync.
func remoteMCPCredentialName(name string) bool {
	name = strings.ToLower(name)
	for _, sensitive := range []string{"token", "secret", "password", "credential", "authorization", "api_key", "api-key", "apikey"} {
		if strings.Contains(name, sensitive) {
			return true
		}
	}
	return false
}

// ResolveRemoteMCPParameters compiles the reviewed schema without loading any
// external resources. Both schema constraints and instances retain exact JSON
// numbers; a binary64 schema representation would round the reviewed bounds.
func ResolveRemoteMCPParameters(parameters *apiextensionsv1.JSON) (*jsonschema.Schema, error) {
	if parameters == nil {
		return nil, errors.New("remote MCP requires reviewed parameters")
	}
	if len(parameters.Raw) > 1<<20 {
		return nil, errors.New("remote MCP parameters exceed size limit")
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(parameters.Raw))
	if err != nil {
		return nil, errors.New("remote MCP parameters must be an object JSON schema")
	}
	schema, ok := document.(map[string]any)
	if !ok || schema["type"] != "object" {
		return nil, errors.New("remote MCP parameters must be an object JSON schema")
	}
	if err := ValidateRemoteMCPNumericBudget(document); err != nil {
		return nil, err
	}
	// Retain the original bounded representation and local-reference checks.
	// References into untyped Extra data must not bypass cardinality bounds.
	// Never use this representation's binary64 values for instance validation.
	var representation googlejsonschema.Schema
	if json.Unmarshal(parameters.Raw, &representation) != nil {
		return nil, errors.New("remote MCP parameters are not a valid bounded JSON schema")
	}
	if _, err := representation.Resolve(nil); err != nil {
		return nil, errors.New("remote MCP parameters contain invalid or unresolved schema references")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(remoteMCPSchemaLoader{})
	const location = "https://orka.invalid/remote-mcp-parameters"
	if err := compiler.AddResource(location, document); err != nil {
		return nil, errors.New("remote MCP parameters cannot be compiled")
	}
	compiled, err := compiler.Compile(location)
	if err != nil {
		return nil, errors.New("remote MCP parameters contain invalid or unresolved schema references")
	}
	return compiled, nil
}

// ValidateRemoteMCPNumericBudget bounds rational expansion before schema or
// argument validation. Values must come from JSON decoding with UseNumber.
// This is a resource guard, not a schema evaluator: it never changes values.
func ValidateRemoteMCPNumericBudget(value any) error {
	remaining := 65_536
	if !remoteMCPNumbersWithinBudget(value, &remaining) {
		return errors.New("remote MCP numeric expansion exceeds limit")
	}
	return nil
}

func remoteMCPNumbersWithinBudget(value any, remaining *int) bool {
	switch value := value.(type) {
	case json.Number:
		coefficient, exponent := string(value), ""
		if index := strings.IndexAny(coefficient, "eE"); index >= 0 {
			coefficient, exponent = coefficient[:index], coefficient[index+1:]
		}
		for _, digit := range coefficient {
			if digit >= '0' && digit <= '9' {
				*remaining--
				if *remaining < 0 {
					return false
				}
			}
		}
		// JSON syntax has already been checked. Parse only the magnitude, stopping
		// at the remaining budget before multiplication could overflow an int.
		exponent = strings.TrimLeft(exponent, "+-")
		magnitude := 0
		for _, digit := range exponent {
			magnitude = magnitude*10 + int(digit-'0')
			if magnitude > *remaining {
				return false
			}
		}
		*remaining -= magnitude
	case []any:
		for _, item := range value {
			if !remoteMCPNumbersWithinBudget(item, remaining) {
				return false
			}
		}
	case map[string]any:
		for _, item := range value {
			if !remoteMCPNumbersWithinBudget(item, remaining) {
				return false
			}
		}
	}
	return true
}

type remoteMCPSchemaLoader struct{}

func (remoteMCPSchemaLoader) Load(string) (any, error) {
	return nil, errors.New("external remote MCP schema resources are forbidden")
}

func validateRemoteMCPHTTP(h *corev1alpha1.HTTPExecution) error {
	if h == nil || h.AuthSecretRef == nil || strings.TrimSpace(h.AuthSecretRef.Name) == "" || strings.TrimSpace(h.AuthSecretRef.Key) == "" || h.OutboundAccessPolicyRef == nil || strings.TrimSpace(h.OutboundAccessPolicyRef.Name) == "" {
		return errors.New("remote MCP requires an exact auth Secret key and outbound access policy")
	}
	if h.URL != "" || (h.Method != "" && h.Method != "POST") || (h.AuthInject != "" && h.AuthInject != "header") || h.AuthBodyKey != "" {
		return errors.New("remote MCP forbids URL, method and body auth overrides")
	}
	if h.Timeout != nil && h.Timeout.Duration <= 0 {
		return errors.New("remote MCP timeout must be positive")
	}
	for key := range h.Headers {
		if remoteMCPCredentialName(key) {
			return errors.New("remote MCP credentials must use authSecretRef, not custom headers")
		}
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized != strings.ToLower(key) || strings.HasPrefix(normalized, "mcp-") || strings.HasPrefix(normalized, "proxy-") {
			return errors.New("remote MCP forbids protocol and authority header overrides")
		}
		switch normalized {
		case "host", "authorization", "cookie", "txn-token", "accept", "content-type", "content-length", "connection", "transfer-encoding", "upgrade", "idempotency-key", "last-event-id":
			return errors.New("remote MCP forbids protocol and authority header overrides")
		}
	}
	return nil
}
