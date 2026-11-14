package worker

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// SetGatewayTaskPass installs controller-only per-call authority. Native workers
// never install it, and refuse before looking up any outbound credentials.
func (e *ToolExecutor) SetGatewayTaskPass(source func(context.Context, *corev1alpha1.Tool) (string, error), httpClient *http.Client) {
	e.gatewayTaskPass = source
	e.client = httpClient
}

// ValidateGatewayTaskPassTool excludes competing credential and routing paths.
func ValidateGatewayTaskPassTool(tool *corev1alpha1.Tool) error {
	h := tool.Spec.HTTP
	if h == nil || !h.GatewayTaskPass {
		return nil
	}
	u, err := url.Parse(h.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != "/v1/task-tools/"+tool.Name ||
		tool.Spec.MCP != nil || h.Forward != nil || h.AuthSecretRef != nil || h.OutboundAccessPolicyRef != nil || len(h.Headers) != 0 ||
		(h.Method != "" && h.Method != http.MethodPost) || (h.AuthInject != "" && h.AuthInject != "header") || h.AuthBodyKey != "" {
		return errors.New("gateway task pass requires a fixed HTTPS POST endpoint and exclusive header authentication")
	}
	return nil
}
