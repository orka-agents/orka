/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/transactiontoken"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/internal/workspace/daemonprotocol"
)

const (
	SubstrateMCPToolTxnTokenHeader = "X-Orka-Tool-Txn-Token"

	SubstrateMCPToolUIDHeader        = "X-Orka-Tool-UID"
	SubstrateMCPToolGenerationHeader = "X-Orka-Tool-Generation"
	SubstrateMCPToolActorHeader      = "X-Orka-Tool-Actor"

	SubstrateMCPDefaultPath = "/mcp"

	substrateMCPMaxPathLength = 1024
)

func substrateMCPHTTPScheme(scheme string) bool {
	return strings.EqualFold(scheme, "https") || strings.EqualFold(scheme, "http")
}

func SubstrateMCPProxyPath(namespace, name string) string {
	query := neturl.Values{}
	query.Set("namespace", namespace)
	return "/api/v1/tools/" + neturl.PathEscape(name) + "/mcp?" + query.Encode()
}

func SubstrateMCPSensitiveHeaderValues(headers map[string]string) []string {
	values := []string{}
	for name, value := range headers {
		if sensitiveToolHeader(name) {
			values = append(values, sensitiveHeaderValues(value)...)
		}
	}
	return values
}

func IsSubstrateMCPActorTool(tool *corev1alpha1.Tool) bool {
	return isMCPSubstrateActorTool(tool)
}

func SubstrateMCPActorTarget(tool *corev1alpha1.Tool) (string, error) {
	if !isMCPSubstrateActorTool(tool) {
		return "", errors.New("tool is not a Substrate MCP actor tool")
	}
	actor := tool.Status.Actor
	if actor == nil || actor.Provider != corev1alpha1.WorkspaceProviderSubstrate {
		return "", errors.New("MCP tool actor endpoint is not ready")
	}
	target, ok := daemonprotocol.TargetActor(strings.TrimSpace(actor.ActorID))
	if !ok {
		return "", errors.New("MCP tool actor status has no qualified Substrate actor ID")
	}
	return target, nil
}

func SubstrateMCPActorPath(tool *corev1alpha1.Tool) (string, error) {
	path := ""
	if tool != nil && tool.Spec.MCP != nil {
		path = strings.TrimSpace(tool.Spec.MCP.Path)
	}
	if path == "" {
		return SubstrateMCPDefaultPath, nil
	}
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || len(path) > substrateMCPMaxPathLength ||
		strings.ContainsAny(path, "?#\\\r\n\t ") {
		return "", errors.New("mcp.path must be an absolute URL path without a query or fragment")
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("mcp.path must not contain dot segments")
		}
	}
	return path, nil
}

type SubstrateMCPRouter struct {
	base   *neturl.URL
	client *http.Client
}

func NewSubstrateMCPRouter(routerURL string, client *http.Client) (*SubstrateMCPRouter, error) {
	parsed, err := neturl.Parse(strings.TrimSpace(routerURL))
	if err != nil || parsed.Host == "" || !substrateMCPHTTPScheme(parsed.Scheme) || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("substrate router URL must be an absolute URL without credentials, query or fragment")
	}
	if client == nil {
		return nil, errors.New("substrate router HTTP client is required")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return &SubstrateMCPRouter{base: parsed, client: client}, nil
}

func (r *SubstrateMCPRouter) HTTPClient() *http.Client {
	if r == nil {
		return nil
	}
	return r.client
}

func (r *SubstrateMCPRouter) NewRequest(ctx context.Context, tool *corev1alpha1.Tool, method string, body io.Reader) (*http.Request, error) {
	if r == nil || r.base == nil || r.client == nil {
		return nil, errors.New("substrate MCP router is not configured")
	}
	target, err := SubstrateMCPActorTarget(tool)
	if err != nil {
		return nil, err
	}
	path, err := SubstrateMCPActorPath(tool)
	if err != nil {
		return nil, err
	}
	endpoint := *r.base
	endpoint.Path = r.base.Path + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(daemonprotocol.TargetActorHeader, target)
	return req, nil
}

func SubstrateMCPReservedHeader(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch normalized {
	case "host", "content-length", "content-type", "accept", "connection", "keep-alive", "transfer-encoding", "te", "trailer",
		"upgrade", "cookie", "forwarded", "via", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto",
		strings.ToLower(daemonprotocol.TargetActorHeader), strings.ToLower(transactiontoken.HeaderName),
		strings.ToLower(SubstrateMCPToolTxnTokenHeader), strings.ToLower(toolIdempotencyKeyHeader),
		strings.ToLower(mcpSessionIDHeader), strings.ToLower(mcpProtocolVersionHeader), "last-event-id":
		return true
	}
	return strings.HasPrefix(normalized, "proxy-") || strings.HasPrefix(normalized, "ate-") ||
		strings.HasPrefix(normalized, "x-orka-")
}

type substrateMCPProxyConfig struct {
	baseURL     *neturl.URL
	tokenSource func() (string, error)
}

func (e *ToolExecutor) SetSubstrateMCPRouter(router *SubstrateMCPRouter) {
	if e == nil {
		return
	}
	e.substrateMCPRouter = router
}

func (e *ToolExecutor) SetSubstrateMCPProxy(baseURL string, tokenSource func() (string, error)) error {
	if e == nil {
		return nil
	}
	parsed, err := neturl.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Host == "" || !substrateMCPHTTPScheme(parsed.Scheme) || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("orka API URL for the MCP proxy must be an absolute URL without credentials, query or fragment")
	}
	if tokenSource == nil {
		return errors.New("orka API MCP proxy requires a workload token source")
	}
	e.substrateMCPProxy = &substrateMCPProxyConfig{baseURL: parsed, tokenSource: tokenSource}
	return nil
}

func substrateMCPProxyFromEnvironment() *substrateMCPProxyConfig {
	base := strings.TrimSpace(os.Getenv(workerenv.ControllerURL))
	if base == "" {
		return nil
	}
	executor := &ToolExecutor{}
	if err := executor.SetSubstrateMCPProxy(base, WorkloadServiceAccountToken); err != nil {
		return nil
	}
	return executor.substrateMCPProxy
}

func WorkloadServiceAccountToken() (string, error) {
	paths := []string{}
	if path := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountTokenPath)); path != "" {
		paths = append(paths, path)
	}
	paths = append(paths, workerenv.ServiceAccountTokenFile)
	for _, path := range paths {
		if data, err := os.ReadFile(path); err == nil { // #nosec G304 -- controller-supplied projected token path.
			if token := strings.TrimSpace(string(data)); token != "" {
				return token, nil
			}
		}
	}
	if token := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken)); token != "" {
		return token, nil
	}
	return "", errors.New("workload ServiceAccount token is unavailable for the Orka MCP proxy")
}

func (e *ToolExecutor) validateSubstrateMCPEgressCredentialAuthority(tool *corev1alpha1.Tool) error {
	secretNames := aitools.SubstrateMCPEgressCredentialSecretNames(tool)
	if slices.Contains(secretNames, "") {
		return errors.New("MCP tool egress credential Secret reference is unresolved")
	}
	return outboundaccess.ValidateCredentialAuthority(
		e.credentialAuthorityEnforced, e.credentialScopeAllowed, e.credentialSecret, secretNames, false,
	)
}

func substrateMCPRouterClient(shared *http.Client) *http.Client {
	client := *shared
	if client.Timeout <= 0 {
		client.Timeout = defaultToolHTTPTimeout
	}
	return &client
}

func (e *ToolExecutor) prepareSubstrateMCPRequest(
	ctx context.Context,
	tool *corev1alpha1.Tool,
	params map[string]any,
) (preparedToolRequest, error) {
	if tool.Spec.HTTP != nil && tool.Spec.HTTP.OutboundAccessPolicyRef != nil {
		return preparedToolRequest{}, errors.New("substrate MCP actor tools do not support outboundAccessPolicyRef")
	}
	if strings.TrimSpace(tool.Status.Endpoint) == "" {
		return preparedToolRequest{}, errors.New("MCP tool actor endpoint is not ready")
	}
	if _, err := SubstrateMCPActorTarget(tool); err != nil {
		return preparedToolRequest{}, err
	}
	if !SubstrateMCPToolReadyForCurrentGeneration(tool) {
		return preparedToolRequest{}, errors.New("MCP tool actor is not ready for the current Tool specification")
	}
	var httpConfig corev1alpha1.HTTPExecution
	if tool.Spec.HTTP != nil {
		httpConfig = *tool.Spec.HTTP
	}
	httpConfig.Method = http.MethodPost
	if toolAuthInject(httpConfig) == "body" && httpConfig.AuthSecretRef != nil {
		return preparedToolRequest{}, errors.New("MCP tools do not support authInject=body")
	}
	for name := range httpConfig.Headers {
		if SubstrateMCPReservedHeader(name) || (strings.EqualFold(strings.TrimSpace(name), "Authorization") && httpConfig.AuthSecretRef != nil) {
			return preparedToolRequest{}, fmt.Errorf("tool configured reserved header %q", name)
		}
	}
	if err := e.validateSubstrateMCPEgressCredentialAuthority(tool); err != nil {
		return preparedToolRequest{}, err
	}
	body, err := marshalMCPToolCallRequest(tool, params)
	if err != nil {
		return preparedToolRequest{}, err
	}

	var (
		req        *http.Request
		baseClient *http.Client
		secrets    []string
		txnHeader  = transactiontoken.HeaderName
	)
	switch {
	case e.substrateMCPRouter != nil:
		req, err = e.substrateMCPRouter.NewRequest(ctx, tool, http.MethodPost, strings.NewReader(string(body)))
		if err != nil {
			return preparedToolRequest{}, fmt.Errorf("failed to create MCP actor request: %w", err)
		}
		baseClient = substrateMCPRouterClient(e.substrateMCPRouter.HTTPClient())
		for name, value := range httpConfig.Headers {
			req.Header.Set(name, value)
			if sensitiveToolHeader(name) {
				secrets = append(secrets, sensitiveHeaderValues(value)...)
			}
		}
		if httpConfig.AuthSecretRef != nil {
			token, err := e.getSecretKey(ctx, httpConfig.AuthSecretRef.Name, httpConfig.AuthSecretRef.Key)
			if err != nil {
				return preparedToolRequest{}, fmt.Errorf("failed to get auth secret: %w", err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			secrets = append(secrets, token)
		}
	case e.substrateMCPProxy != nil:
		if httpConfig.AuthSecretRef != nil {
			if err := outboundaccess.ValidateCredentialAuthority(
				e.credentialAuthorityEnforced, e.credentialScopeAllowed, e.credentialSecret,
				[]string{httpConfig.AuthSecretRef.Name}, false,
			); err != nil {
				return preparedToolRequest{}, err
			}
		}
		namespace := strings.TrimSpace(tool.Namespace)
		if namespace == "" {
			namespace = e.namespace
		}
		endpoint, parseErr := neturl.Parse(e.substrateMCPProxy.baseURL.String() + SubstrateMCPProxyPath(namespace, tool.Name))
		if parseErr != nil {
			return preparedToolRequest{}, fmt.Errorf("failed to build MCP proxy URL: %w", parseErr)
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
		if err != nil {
			return preparedToolRequest{}, fmt.Errorf("failed to create MCP proxy request: %w", err)
		}
		token, tokenErr := e.substrateMCPProxy.tokenSource()
		if tokenErr != nil {
			return preparedToolRequest{}, tokenErr
		}
		req.Header.Set("Authorization", "Bearer "+token)
		secrets = append(secrets, token)
		txnHeader = SubstrateMCPToolTxnTokenHeader
		SetSubstrateMCPToolBinding(req.Header, tool)
	default:
		return preparedToolRequest{}, errors.New("substrate MCP actor tools require the Orka API MCP proxy; set " + workerenv.ControllerURL)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcpProtocolVersionHeader, mcpProtocolVersion)

	transactionToken, err := e.outboundTransactionToken(ctx, tool)
	if err != nil {
		return preparedToolRequest{}, err
	}
	if transactionToken != "" {
		req.Header.Set(txnHeader, transactionToken)
	}
	return preparedToolRequest{
		httpConfig:       httpConfig,
		request:          req,
		transactionToken: transactionToken,
		redactionSecrets: compactToolSecrets(append(secrets, transactionToken)...),
		mcp:              true,
		substrateMCP:     true,
		baseClient:       baseClient,
	}, nil
}

func SetSubstrateMCPToolBinding(header http.Header, tool *corev1alpha1.Tool) {
	header.Set(SubstrateMCPToolUIDHeader, string(tool.UID))
	header.Set(SubstrateMCPToolGenerationHeader, strconv.FormatInt(tool.Generation, 10))
	actorID := ""
	if tool.Status.Actor != nil {
		actorID = strings.TrimSpace(tool.Status.Actor.ActorID)
	}
	header.Set(SubstrateMCPToolActorHeader, actorID)
}

func SubstrateMCPToolBindingMatches(tool *corev1alpha1.Tool, uid, generation, actorID string) bool {
	if tool == nil || tool.UID == "" || tool.Status.Actor == nil {
		return false
	}
	current := strings.TrimSpace(tool.Status.Actor.ActorID)
	return strings.TrimSpace(uid) == string(tool.UID) &&
		strings.TrimSpace(generation) == strconv.FormatInt(tool.Generation, 10) &&
		current != "" && strings.TrimSpace(actorID) == current
}

func SubstrateMCPToolReadyForCurrentGeneration(tool *corev1alpha1.Tool) bool {
	if tool == nil || !tool.Status.Available {
		return false
	}
	for _, condition := range tool.Status.Conditions {
		if condition.Type == "Available" {
			return condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == tool.Generation
		}
	}
	return false
}
