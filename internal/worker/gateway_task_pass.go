package worker

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway/protocol"
)

// SetGatewayTaskPass installs controller-only per-call authority. Native workers
// never install it, and refuse before looking up any outbound credentials.
func (e *ToolExecutor) SetGatewayTaskPass(source func(context.Context, *corev1alpha1.Tool) (string, error), httpClient *http.Client) {
	e.gatewayTaskPass = source
	e.client = httpClient
}

var errGatewayTaskPassTool = errors.New("gateway task pass requires a fixed HTTPS POST endpoint and exclusive header authentication")

// ValidateGatewayTaskPassTool excludes competing credential and routing paths.
func ValidateGatewayTaskPassTool(tool *corev1alpha1.Tool) error {
	h := tool.Spec.HTTP
	if h == nil || !h.GatewayTaskPass {
		return nil
	}
	u, err := url.Parse(h.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Path != protocol.TaskToolPath(tool.Name) ||
		(h.Method != "" && h.Method != http.MethodPost) || (h.AuthInject != "" && h.AuthInject != "header") ||
		!onlyGatewayTaskPassFields(tool.Spec) {
		return errGatewayTaskPassTool
	}
	return nil
}

// onlyGatewayTaskPassFields allows only reviewed fields, so a field added to
// ToolSpec or HTTPExecution later is refused until it is explicitly permitted.
func onlyGatewayTaskPassFields(spec corev1alpha1.ToolSpec) bool {
	h := *spec.HTTP
	if len(h.Headers) == 0 {
		h.Headers = nil
	}
	permittedHTTP := corev1alpha1.HTTPExecution{
		GatewayTaskPass: true, URL: h.URL, Method: h.Method, Timeout: h.Timeout, AuthInject: h.AuthInject,
	}
	permittedSpec := corev1alpha1.ToolSpec{
		Description: spec.Description, Parameters: spec.Parameters, BrokeredToolClass: spec.BrokeredToolClass, HTTP: spec.HTTP,
	}
	return reflect.DeepEqual(h, permittedHTTP) && reflect.DeepEqual(spec, permittedSpec)
}
