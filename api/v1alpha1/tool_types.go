/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ToolSpec defines the desired state of Tool
// +kubebuilder:validation:XValidation:rule="has(self.http) || has(self.mcp)",message="http or an MCP backend is required"
// +kubebuilder:validation:XValidation:rule="!has(self.http) || has(self.mcp) || (has(self.http.url) && self.http.url.size() > 0)",message="http.url is required unless an MCP backend is set"
// +kubebuilder:validation:XValidation:rule="!has(self.mcp) || !has(self.mcp.remote) || (has(self.http) && has(self.http.authSecretRef) && self.http.authSecretRef.name.size() > 0 && self.http.authSecretRef.key.size() > 0 && has(self.http.outboundAccessPolicyRef))",message="remote MCP requires a named auth Secret key and outbound access policy"
// +kubebuilder:validation:XValidation:rule="!has(self.mcp) || !has(self.mcp.remote) || !has(self.http) || ((!has(self.http.url) || self.http.url.size() == 0) && (!has(self.http.authInject) || self.http.authInject == 'header') && (!has(self.http.authBodyKey) || self.http.authBodyKey.size() == 0) && (!has(self.http.method) || self.http.method == 'POST'))",message="remote MCP forbids http.url, body auth and method overrides"
// +kubebuilder:validation:XValidation:rule="!has(self.mcp) || !has(self.mcp.remote) || !has(self.http) || !has(self.http.headers) || self.http.headers.all(k, !(k.lowerAscii() in ['host','authorization','cookie','txn-token','accept','content-type']))",message="remote MCP forbids authority and credential header overrides"
// +kubebuilder:validation:XValidation:rule="!has(self.mcp) || !has(self.mcp.remote) || !has(self.http) || !has(self.http.headers) || self.http.headers.all(k, !(k.lowerAscii() in ['content-length','connection','transfer-encoding','upgrade','idempotency-key','last-event-id']))",message="remote MCP forbids protocol header overrides"
// +kubebuilder:validation:XValidation:rule="!has(self.mcp) || !has(self.mcp.remote) || !has(self.http) || !has(self.http.headers) || self.http.headers.all(k, !k.lowerAscii().startsWith('mcp-') && !k.lowerAscii().startsWith('proxy-'))",message="remote MCP forbids MCP session and proxy header overrides"
// +kubebuilder:validation:XValidation:rule="!has(self.mcp) || !has(self.mcp.remote) || !has(self.http) || !has(self.http.headers) || self.http.headers.all(k, !k.lowerAscii().matches('token|secret|password|credential|authorization|api[-_]?key|subscription[-_]?key'))",message="remote MCP credentials must use authSecretRef, not custom headers"
// +kubebuilder:validation:XValidation:rule="!has(self.http) || !has(self.http.forward) || (has(self.brokeredToolClass) && self.brokeredToolClass == 'read')",message="a forwarding tool must declare brokeredToolClass read"
// +kubebuilder:validation:XValidation:rule="!has(self.http) || !has(self.http.forward) || has(self.http.outboundAccessPolicyRef)",message="a forwarding tool takes its credential from an outbound access policy"
// +kubebuilder:validation:XValidation:rule="!has(self.http) || !has(self.http.forward) || (!has(self.mcp) && !has(self.http.authSecretRef) && (!has(self.http.headers) || size(self.http.headers) == 0) && (!has(self.http.authInject) || self.http.authInject == 'header') && (!has(self.http.authBodyKey) || self.http.authBodyKey.size() == 0))",message="a forwarding tool sets no MCP backend, auth Secret, or headers"
// +kubebuilder:validation:XValidation:rule="!has(self.http) || !has(self.http.forward) || (has(self.http.url) && self.http.url.matches('^https://[^/?#@{}]+/?$'))",message="a forwarding tool url must be an https origin with no path"
type ToolSpec struct {
	// Description is the tool description shown to the LLM
	// +kubebuilder:validation:Required
	Description string `json:"description"`

	// Parameters is the JSON Schema for tool parameters (OpenAI function calling format).
	// Remote MCP requires a reviewed object schema, checked by the controller and
	// native worker before discovery/execution. It stays schemaless at admission
	// to preserve compatibility with existing Tool parameter schemas.
	// +optional
	Parameters *apiextensionsv1.JSON `json:"parameters,omitempty"`

	// BrokeredToolClass declares the governed class for remote AgentRuntime brokered calls.
	// Tools without this field are not exposed to remote runtimes through brokered governance.
	// +optional
	BrokeredToolClass AgentRuntimeBrokeredToolClass `json:"brokeredToolClass,omitempty"`

	// HTTP defines the HTTP execution configuration. It is required unless MCP
	// is set.
	// +optional
	HTTP *HTTPExecution `json:"http,omitempty"`

	// MCP defines a managed or remote MCP server backend for this tool.
	// +optional
	MCP *MCPToolServer `json:"mcp,omitempty"`
}

// HTTPExecution defines how to execute the tool via HTTP
type HTTPExecution struct {
	// URL is the endpoint to call when the tool is invoked
	// Required for plain HTTP tools. MCP actor-backed tools may omit it when
	// HTTP is present only for transport auth settings.
	// +optional
	URL string `json:"url,omitempty"`

	// Method is the HTTP method to use (default: POST)
	// +kubebuilder:validation:Enum=GET;POST;PUT;PATCH;DELETE
	// +kubebuilder:default=POST
	// +optional
	Method string `json:"method,omitempty"`

	// Headers are additional headers to include in requests
	// +optional
	Headers map[string]string `json:"headers,omitempty"`

	// Timeout is the request timeout (default: 30s)
	// Consequential (non-read) tools brokered to ACP runtimes execute under a
	// fixed external-effect ledger lease; their timeout must stay at or below
	// the controller's brokered call bound (currently four minutes). Longer
	// timeouts are rejected when the tool is exposed to an ACP prompt, and
	// brokered calls are always clamped to that bound at execution time.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// AuthSecretRef references a Secret containing authentication token
	// Token is injected as Authorization: Bearer <token> (when AuthInject=header)
	// or into the request body (when AuthInject=body)
	// +optional
	AuthSecretRef *SecretKeySelector `json:"authSecretRef,omitempty"`

	// OutboundAccessPolicyRef references an OutboundAccessPolicy in the same namespace.
	// +optional
	OutboundAccessPolicyRef *LocalObjectReference `json:"outboundAccessPolicyRef,omitempty"`

	// AuthInject specifies where to inject the auth token: "header" (default) or "body"
	// When "header", the token is added as Authorization: Bearer <token>
	// When "body", the token is added to the JSON request body using AuthBodyKey
	// +kubebuilder:validation:Enum=header;body
	// +kubebuilder:default=header
	// +optional
	AuthInject string `json:"authInject,omitempty"`

	// AuthBodyKey is the JSON key name to use when AuthInject=body
	// For example, "api_key" would inject {"api_key": "<token>", ...}
	// Only used when AuthInject=body
	// +optional
	AuthBodyKey string `json:"authBodyKey,omitempty"`

	// Forward makes this Tool the route a command-line client in an ACP
	// runtime uses, instead of a function the model calls. The runtime serves
	// the client's protocol on loopback and sends each request through the
	// broker to this Tool's origin, where the outbound access policy supplies
	// the credential. The client never holds it. Read requests (GET and HEAD)
	// are forwarded; writes only when Writes is set. The Tool is not listed to
	// the model.
	// +optional
	Forward *HTTPForward `json:"forward,omitempty"`
}

// HTTPForwardProtocol names a client protocol an ACP runtime can serve for a
// forwarding Tool.
// +kubebuilder:validation:Enum=AzureResourceManager
type HTTPForwardProtocol string

const (
	// HTTPForwardProtocolAzureResourceManager serves the Azure CLI (az): the
	// runtime presents itself as an Azure cloud whose Resource Manager endpoint
	// and managed identity are local, and forwards Resource Manager requests.
	HTTPForwardProtocolAzureResourceManager HTTPForwardProtocol = "AzureResourceManager"
)

// HTTPForward configures a forwarding Tool or connector tool.
type HTTPForward struct {
	// Protocol is the client protocol the runtime serves.
	Protocol HTTPForwardProtocol `json:"protocol"`

	// Writes lets the client send write requests (PUT, PATCH, DELETE, and
	// POST) through this Tool, each recorded as an external effect. Without
	// it only reads are forwarded. On a ConnectorProvider tool it grants
	// writes to the matching Tool through readWrite links; the Tool's own
	// approval setting governs them.
	// +optional
	Writes *HTTPForwardWrites `json:"writes,omitempty"`
}

// HTTPForwardWriteApproval decides whether a forwarded write waits for a
// person's approval.
// +kubebuilder:validation:Enum=Required;None
type HTTPForwardWriteApproval string

const (
	// HTTPForwardWriteApprovalRequired holds each write until a person
	// approves it.
	HTTPForwardWriteApprovalRequired HTTPForwardWriteApproval = "Required"
	// HTTPForwardWriteApprovalNone sends writes without approval; the
	// credential's own permissions are the only limit.
	HTTPForwardWriteApprovalNone HTTPForwardWriteApproval = "None"
)

// HTTPForwardWrites configures forwarded writes.
type HTTPForwardWrites struct {
	// Approval decides whether each write waits for a person's approval.
	// +kubebuilder:default=Required
	// +optional
	Approval HTTPForwardWriteApproval `json:"approval,omitempty"`
}

// WriteApprovalRequired reports whether forwarded writes wait for approval.
// An unset approval is Required.
func (w *HTTPForwardWrites) WriteApprovalRequired() bool {
	return w == nil || w.Approval != HTTPForwardWriteApprovalNone
}

// LocalObjectReference selects an object in the same namespace.
type LocalObjectReference struct {
	// Name is the referenced object name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretKeySelector selects a key from a Secret
type SecretKeySelector struct {
	// Name is the name of the Secret
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Key is the key within the Secret
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// MCPToolServer configures a Model Context Protocol server backend.
// +kubebuilder:validation:XValidation:rule="(has(self.substrateActor) ? 1 : 0) + (has(self.workspace) ? 1 : 0) + (has(self.remote) ? 1 : 0) == 1",message="exactly one of substrateActor, workspace or remote is required"
// +kubebuilder:validation:XValidation:rule="!has(self.remote) || !has(self.path) || self.path.size() == 0",message="remote MCP uses remote.url, not mcp.path"
type MCPToolServer struct {
	// Remote selects one reviewed callable on an independently operated server.
	// Only native AI Tasks with an explicitly selecting Agent are supported.
	// +optional
	Remote *RemoteMCPServer `json:"remote,omitempty"`

	// Path is the HTTP path exposed by the MCP server inside the actor.
	// Defaults to /mcp.
	// +optional
	Path string `json:"path,omitempty"`

	// Workspace configures a provider-neutral Service-mode ExecutionWorkspace.
	// +optional
	Workspace *MCPWorkspace `json:"workspace,omitempty"`

	// SubstrateActor configures the legacy durable Substrate actor backend.
	// +optional
	SubstrateActor *SubstrateMCPActor `json:"substrateActor,omitempty"`
}

// RemoteMCPServer selects a Streamable HTTP endpoint and exact remote tool name.
type RemoteMCPServer struct {
	// URL is the fixed endpoint. Credentials belong in http.authSecretRef.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://[^/@?#{}]+([/?][^{}#]*)?$`
	// Keep credential-name checks in sync with internal/aitools/remote_mcp.go.
	// Scalar, percent-aware matching decodes query names once without unbounded CEL map iteration.
	// +kubebuilder:validation:XValidation:rule="isURL(self) && url(self).getHostname() != ''",message="remote MCP requires a fixed HTTP URL with a hostname"
	// +kubebuilder:validation:XValidation:rule="self.matches('^[^?]*([?]([^%;]|%[0-9A-Fa-f]{2})*)?$')",message="remote MCP URL query must use valid percent escapes and no raw semicolons"
	// +kubebuilder:validation:XValidation:rule="!self.lowerAscii().matches('^[^?]*[?]([^&]*&)*([^%=&]|%[0-9a-f]{2})*((t|%[57]4)(o|%[46]f)(k|%[46]b|K|%e2%84%aa)(e|%[46]5)(n|%[46]e)|(s|%[57]3)(e|%[46]5)(c|%[46]3)(r|%[57]2)(e|%[46]5)(t|%[57]4)|(p|%[57]0)(a|%[46]1)(s|%[57]3)(s|%[57]3)(w|%[57]7)(o|%[46]f)(r|%[57]2)(d|%[46]4)|(c|%[46]3)(r|%[57]2)(e|%[46]5)(d|%[46]4)(e|%[46]5)(n|%[46]e)(t|%[57]4)(i|%[46]9|İ|%c4%b0)(a|%[46]1)(l|%[46]c)|(a|%[46]1)(u|%[57]5)(t|%[57]4)(h|%[46]8)(o|%[46]f)(r|%[57]2)(i|%[46]9|İ|%c4%b0)(z|%[57]a)(a|%[46]1)(t|%[57]4)(i|%[46]9|İ|%c4%b0)(o|%[46]f)(n|%[46]e)|(a|%[46]1)(p|%[57]0)(i|%[46]9|İ|%c4%b0)(-|_|%2d|%5f)?(k|%[46]b|K|%e2%84%aa)(e|%[46]5)(y|%[57]9)|(s|%[57]3)(u|%[57]5)(b|%[46]2)(s|%[57]3)(c|%[46]3)(r|%[57]2)(i|%[46]9|İ|%c4%b0)(p|%[57]0)(t|%[57]4)(i|%[46]9|İ|%c4%b0)(o|%[46]f)(n|%[46]e)(-|_|%2d|%5f)?(k|%[46]b|K|%e2%84%aa)(e|%[46]5)(y|%[57]9))([^%=&]|%[0-9a-f]{2})*(=|&|$)')",message="remote MCP URL must not contain credential query parameters"
	// +kubebuilder:validation:XValidation:rule="!self.lowerAscii().matches('^[^?]*[?]([^&]*&)*([+[:space:]\\\\x{0085}\\\\x{00a0}\\\\x{1680}\\\\x{2000}-\\\\x{200a}\\\\x{2028}\\\\x{2029}\\\\x{202f}\\\\x{205f}\\\\x{3000}]|%(09|0a|0b|0c|0d|20|c2%(85|a0)|e1%9a%80|e2%80%(8[0-9a]|a8|a9|af)|e2%81%9f|e3%80%80))*((a|%[46]1)(w|%[57]7)(s|%[57]3)(a|%[46]1)(c|%[46]3)(c|%[46]3)(e|%[46]5)(s|%[57]3)(s|%[57]3)(k|%[46]b|K|%e2%84%aa)(e|%[46]5)(y|%[57]9)(i|%[46]9|İ|%c4%b0)(d|%[46]4)|(a|%[46]1)(w|%[57]7)(s|%[57]3)(-|_|%2d|%5f)(a|%[46]1)(c|%[46]3)(c|%[46]3)(e|%[46]5)(s|%[57]3)(s|%[57]3)(-|_|%2d|%5f)(k|%[46]b|K|%e2%84%aa)(e|%[46]5)(y|%[57]9)(-|_|%2d|%5f)(i|%[46]9|İ|%c4%b0)(d|%[46]4)|(g|%[46]7)(o|%[46]f)(o|%[46]f)(g|%[46]7)(l|%[46]c)(e|%[46]5)(a|%[46]1)(c|%[46]3)(c|%[46]3)(e|%[46]5)(s|%[57]3)(s|%[57]3)(i|%[46]9|İ|%c4%b0)(d|%[46]4)|(g|%[46]7)(o|%[46]f)(o|%[46]f)(g|%[46]7)(l|%[46]c)(e|%[46]5)(-|_|%2d|%5f)(a|%[46]1)(c|%[46]3)(c|%[46]3)(e|%[46]5)(s|%[57]3)(s|%[57]3)(-|_|%2d|%5f)(i|%[46]9|İ|%c4%b0)(d|%[46]4)|(k|%[46]b|K|%e2%84%aa)(e|%[46]5)(y|%[57]9)(-|_|%2d|%5f)(p|%[57]0)(a|%[46]1)(i|%[46]9|İ|%c4%b0)(r|%[57]2)(-|_|%2d|%5f)(i|%[46]9|İ|%c4%b0)(d|%[46]4)|(s|%[57]3)(i|%[46]9|İ|%c4%b0)(g|%[46]7)|(s|%[57]3)(i|%[46]9|İ|%c4%b0)(g|%[46]7)(n|%[46]e)(a|%[46]1)(t|%[57]4)(u|%[57]5)(r|%[57]2)(e|%[46]5)|(x|%[57]8)(-|_|%2d|%5f)(a|%[46]1)(m|%[46]d)(z|%[57]a)(-|_|%2d|%5f)(c|%[46]3)(r|%[57]2)(e|%[46]5)(d|%[46]4)(e|%[46]5)(n|%[46]e)(t|%[57]4)(i|%[46]9|İ|%c4%b0)(a|%[46]1)(l|%[46]c)|(x|%[57]8)(-|_|%2d|%5f)(a|%[46]1)(m|%[46]d)(z|%[57]a)(-|_|%2d|%5f)(s|%[57]3)(e|%[46]5)(c|%[46]3)(u|%[57]5)(r|%[57]2)(i|%[46]9|İ|%c4%b0)(t|%[57]4)(y|%[57]9)(-|_|%2d|%5f)(t|%[57]4)(o|%[46]f)(k|%[46]b|K|%e2%84%aa)(e|%[46]5)(n|%[46]e)|(x|%[57]8)(-|_|%2d|%5f)(a|%[46]1)(m|%[46]d)(z|%[57]a)(-|_|%2d|%5f)(s|%[57]3)(i|%[46]9|İ|%c4%b0)(g|%[46]7)(n|%[46]e)(a|%[46]1)(t|%[57]4)(u|%[57]5)(r|%[57]2)(e|%[46]5)|(x|%[57]8)(-|_|%2d|%5f)(g|%[46]7)(o|%[46]f)(o|%[46]f)(g|%[46]7)(-|_|%2d|%5f)(c|%[46]3)(r|%[57]2)(e|%[46]5)(d|%[46]4)(e|%[46]5)(n|%[46]e)(t|%[57]4)(i|%[46]9|İ|%c4%b0)(a|%[46]1)(l|%[46]c)|(x|%[57]8)(-|_|%2d|%5f)(g|%[46]7)(o|%[46]f)(o|%[46]f)(g|%[46]7)(-|_|%2d|%5f)(s|%[57]3)(i|%[46]9|İ|%c4%b0)(g|%[46]7)(n|%[46]e)(a|%[46]1)(t|%[57]4)(u|%[57]5)(r|%[57]2)(e|%[46]5)|(x|%[57]8)(-|_|%2d|%5f)(m|%[46]d)(s|%[57]3)(-|_|%2d|%5f)(s|%[57]3)(i|%[46]9|İ|%c4%b0)(g|%[46]7)(n|%[46]e)(a|%[46]1)(t|%[57]4)(u|%[57]5)(r|%[57]2)(e|%[46]5))([+[:space:]\\\\x{0085}\\\\x{00a0}\\\\x{1680}\\\\x{2000}-\\\\x{200a}\\\\x{2028}\\\\x{2029}\\\\x{202f}\\\\x{205f}\\\\x{3000}]|%(09|0a|0b|0c|0d|20|c2%(85|a0)|e1%9a%80|e2%80%(8[0-9a]|a8|a9|af)|e2%81%9f|e3%80%80))*(=|&|$)')",message="remote MCP URL must not contain signed credential query parameters"
	URL string `json:"url"`

	// ToolName is the exact tools/list name, independent of the Kubernetes alias.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+$`
	ToolName string `json:"toolName"`
}

// MCPWorkspace selects a Service-mode ExecutionWorkspaceClass for hosting an MCP server.
type MCPWorkspace struct {
	// ClassRef references a class in the Tool namespace.
	ClassRef WorkspaceClassReference `json:"classRef"`

	// Port is the service port exposed by the MCP server.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// SubstrateMCPActor selects the durable actor that hosts an MCP server.
type SubstrateMCPActor struct {
	// TemplateRef references the ActorTemplate that runs the MCP server.
	// +kubebuilder:validation:Required
	TemplateRef WorkspaceTemplateReference `json:"templateRef"`

	// PoolRef optionally assigns this tool actor to an Orka SubstrateActorPool.
	// +optional
	PoolRef *SubstrateActorPoolReference `json:"poolRef,omitempty"`

	// Boot asks Substrate to boot this actor from scratch on first resume.
	// +optional
	Boot bool `json:"boot,omitempty"`

	// Egress lists the only destinations the actor may reach through the
	// Substrate egress gateway. The controller compiles it into the actor's
	// native egress policy before every resume. Empty denies all egress,
	// including DNS resolution of external names. HTTPS entries are
	// intercepted by the gateway, so the operator-owned ActorTemplate must
	// trust the Substrate egress CA bundle; use scheme tls to pass TLS through
	// without interception.
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +optional
	Egress []SubstrateActorEgressRule `json:"egress,omitempty"`
}

// SubstrateActorEgressScheme selects how the egress gateway authorizes traffic.
// +kubebuilder:validation:Enum=http;https;tls
type SubstrateActorEgressScheme string

const (
	// SubstrateActorEgressSchemeHTTP allows cleartext HTTP to the host.
	SubstrateActorEgressSchemeHTTP SubstrateActorEgressScheme = "http"
	// SubstrateActorEgressSchemeHTTPS allows HTTPS intercepted by the gateway.
	SubstrateActorEgressSchemeHTTPS SubstrateActorEgressScheme = "https"
	// SubstrateActorEgressSchemeTLS allows TLS passed through without decryption.
	SubstrateActorEgressSchemeTLS SubstrateActorEgressScheme = "tls"
)

// SubstrateActorEgressRule allows one host name on explicit ports.
// +kubebuilder:validation:XValidation:rule="self.scheme != 'tls' || (has(self.ports) && size(self.ports) > 0)",message="tls egress requires explicit ports"
type SubstrateActorEgressRule struct {
	// Scheme is http, https (gateway-intercepted) or tls (passthrough).
	// +kubebuilder:validation:Required
	Scheme SubstrateActorEgressScheme `json:"scheme"`

	// Host is a lowercase DNS name, or a DNS name whose leftmost label is "*"
	// (for example "*.example.com"). Bare "*", IP addresses, CIDRs, ports,
	// paths and wildcards over a top-level domain are rejected.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)+$`
	// +kubebuilder:validation:XValidation:rule="!self.matches('^[0-9.]+$')",message="egress host must be a DNS name, not an IP address"
	Host string `json:"host"`

	// Ports are destination ports. Defaults to 80 for http and 443 for https;
	// required for tls.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=65535
	// +listType=set
	// +optional
	Ports []int32 `json:"ports,omitempty"`
}

// ToolStatus defines the observed state of Tool
type ToolStatus struct {
	// Available indicates whether the tool endpoint is reachable
	Available bool `json:"available"`

	// LastCheck is the timestamp of the last health check
	// +optional
	LastCheck *metav1.Time `json:"lastCheck,omitempty"`

	// Error is the error message if the tool is unavailable
	// +optional
	Error string `json:"error,omitempty"`

	// Endpoint is the resolved non-secret endpoint used by workers. For
	// Substrate MCP actor Tools it is the Orka API MCP proxy path, relative to
	// the Orka API base URL; the Substrate router is never exposed.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Workspace reports the provider-neutral Service workspace backing this Tool.
	// +optional
	Workspace *ToolWorkspaceStatus `json:"workspace,omitempty"`

	// Actor reports durable actor metadata when this tool is MCP actor-backed.
	// +optional
	Actor *ToolActorStatus `json:"actor,omitempty"`

	// Conditions represent the current state of the Tool
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ToolWorkspaceStatus reports safe generic workspace metadata for a Tool.
type ToolWorkspaceStatus struct {
	// ClassRef is the selected Service workspace class.
	ClassRef WorkspaceClassReference `json:"classRef"`

	// WorkspaceRef identifies the owned concrete workspace.
	// +optional
	WorkspaceRef *WorkspaceObjectReference `json:"workspaceRef,omitempty"`

	// State is the provider-neutral workspace state.
	// +optional
	State string `json:"state,omitempty"`

	// Conditions project generic readiness and cleanup state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ToolActorStatus reports safe durable actor metadata for a Tool.
type ToolActorStatus struct {
	// Provider is the actor provider.
	// +optional
	Provider WorkspaceProvider `json:"provider,omitempty"`

	// ActorID is the stable, Atespace-qualified provider actor id
	// ("name.atespace"). Orka derives the router target only from this field.
	// +optional
	ActorID string `json:"actorID,omitempty"`

	// TemplateRef is the resolved actor template.
	// +optional
	TemplateRef *WorkspaceTemplateReference `json:"templateRef,omitempty"`

	// PoolRef is the resolved Orka actor pool.
	// +optional
	PoolRef *SubstrateActorPoolReference `json:"poolRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.http.url`
// +kubebuilder:printcolumn:name="Available",type=boolean,JSONPath=`.status.available`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Tool is the Schema for the tools API
// A forwarding tool is offered under its own name, which must not be one a
// coding agent or the Orka MCP broker already serves: the broker would
// resolve that name to its own tool and never create the forwarding gateway.
// TestForwardingToolNamesCoverTheBrokeredBuiltIns keeps the list complete.
// +kubebuilder:validation:XValidation:rule="!has(self.spec.http) || !has(self.spec.http.forward) || !(self.metadata.name in ['bash','edit','glob','grep','read','webfetch','websearch','write','check_messages','check_pull_request_ci','comment_on_issue','create_pull_request','delegate_task','get_issue','list_connections','list_issues','list_pull_requests','post_review_comment','propose_memory','recall_memory','remember','reply_in_conversation','review_pull_request','run_validation','search_transcript','send_message','wait_for_tasks','web_fetch','web_search'])",message="a forwarding tool must not share a name with a built-in tool"
// A write tool is named after its Tool with the ":write" suffix, and tool
// names are limited to 253 bytes.
// +kubebuilder:validation:XValidation:rule="!has(self.spec.http) || !has(self.spec.http.forward) || !has(self.spec.http.forward.writes) || size(self.metadata.name) <= 247",message="a forwarding tool that allows writes must have a name of at most 247 characters"
type Tool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ToolSpec   `json:"spec,omitempty"`
	Status ToolStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ToolList contains a list of Tool
type ToolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Tool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Tool{}, &ToolList{})
}
