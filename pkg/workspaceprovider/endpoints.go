package workspaceprovider

import (
	"fmt"
	"net/url"
	"strings"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

// ValidateEndpoints rejects endpoint metadata that could disclose credentials or
// produce ambiguous routing. Adapters should call this before patching status;
// the CRD schema independently enforces the same credential-free invariant.
func ValidateEndpoints(endpoints []workspacev1alpha1.ExecutionWorkspaceEndpoint) error {
	for _, endpoint := range endpoints {
		parsed, err := url.Parse(strings.TrimSpace(endpoint.URL))
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("workspace endpoint %q is invalid", endpoint.Name)
		}
		switch parsed.Scheme {
		case "http", "https", "tcp":
		default:
			return fmt.Errorf("workspace endpoint %q uses unsupported scheme", endpoint.Name)
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.ContainsAny(parsed.Host, "@?#") {
			return fmt.Errorf("workspace endpoint %q contains credentials or ambiguous URL components", endpoint.Name)
		}
	}
	return nil
}
